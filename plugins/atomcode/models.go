package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Plan tiers. `models-v2` takes the tier as a query parameter and computes
// `plan_available` relative to it (`crates/atomcode-codingplan/src/types.rs:53-70`),
// so the tier must be the account's real one — asking for Max as a Lite account
// marks models available that answer 403 on every request.
const (
	planTypeMax  = "Max"
	planTypePro  = "Pro"
	planTypeLite = "Lite"
)

// planCascadeOrder is the reference's claim order, highest tier first
// (`crates/atomcode-codingplan/src/types.rs:37-39`).
var planCascadeOrder = []string{planTypeMax, planTypePro, planTypeLite}

// minContextWindow is the floor the reference applies to every CodingPlan model
// (`crates/atomcode-codingplan/src/setup.rs:74`).
//
// The gateway reports a conservative 64k for several models; the floor exists so
// the client does not compact or refuse long turns the gateway can actually
// serve. A larger server value (deepseek-flash reports 1M) is kept as-is.
const minContextWindow = 128_000

// defaultMaxOutputTokens is the output ceiling declared for models whose
// catalogue entry carries no per-model output budget. `models-v2` publishes no
// `max_output_tokens` field at all, so this is the only source.
const defaultMaxOutputTokens = 64_000

// modelEntry is one element of the `GET /coding-plan/models-v2` array
// (`crates/atomcode-codingplan/src/types.rs:91-176`).
type modelEntry struct {
	ID   int64 `json:"id"`
	IsIn bool  `json:"-"`
	// DisplayModelName is the id sent on the wire and shown as the model name.
	DisplayModelName string `json:"display_model_name"`
	// BaseURL is the gateway the server prefers for this model.
	//
	// ⚠️ Deliberately ignored for routing: the server advertises
	// `https://llm-api.atomgit.com/v1`, which requires the closed-source
	// signature. It is kept only so the management page can show which host the
	// server asked for. See DefaultGatewayBase.
	BaseURL string `json:"base_url"`
	// Type is the provider dialect; the gateway is OpenAI-compatible
	// (`setup.rs:67` pins `PROVIDER_TYPE = "openai"`).
	Type string `json:"type"`
	// ContextWindow is the server-declared window, floored at minContextWindow.
	ContextWindow *int `json:"context_window"`
	// SupportsVision is an authoritative opt-out when present.
	SupportsVision *bool `json:"supports_vision"`
	// PlanAvailable is the server's own entitlement decision for the queried tier.
	PlanAvailable bool `json:"plan_available"`
	// ReasoningEffortLevels is the ordered effort list this model accepts.
	// A non-empty list is authoritative (`setup.rs:1724-1730`); an empty one means
	// the model offers no effort switching.
	ReasoningEffortLevels []string `json:"reasoning_effort_levels"`
	// IsInfinity and IsAtomcodeExclusive are server metadata the reference keeps
	// but does not act on (`types.rs:120-130`).
	IsInfinity          int `json:"is_infinity"`
	IsAtomcodeExclusive int `json:"is_atomcode_exclusive"`
	// FromConfig marks an entry this adapter added from the `extra_models`
	// setting rather than one the server advertised. It is local bookkeeping:
	// the pages label such a model so nobody mistakes it for an entitlement.
	FromConfig bool `json:"-"`
}

// effectiveContextWindow applies the reference's 128k floor.
func (m modelEntry) effectiveContextWindow() int {
	if m.ContextWindow == nil || *m.ContextWindow < minContextWindow {
		return minContextWindow
	}
	return *m.ContextWindow
}

// acceptsImages reports the model's image capability. An absent field is a
// conservative "no": the reference falls back to a model-name heuristic for older
// payloads (`setup.rs:1496`), but guessing from a name is how a text-only model
// ends up being handed a base64 image.
func (m modelEntry) acceptsImages() bool {
	return m.SupportsVision != nil && *m.SupportsVision
}

// catalogue is a fetched model list plus the tier it was fetched for.
type catalogue struct {
	models   []modelEntry
	planType string
	fetched  time.Time
}

var (
	catalogueMu    sync.Mutex
	catalogueCache = map[string]catalogue{}
)

// catalogueKey scopes the cache to the account and the tier.
func catalogueKey(credential *Credential, planType string) string {
	return credential.AccountID() + "|" + planType
}

// cachedCatalogue returns a live cache entry, if one exists.
func cachedCatalogue(credential *Credential, planType string, ttl time.Duration) (catalogue, bool) {
	if ttl <= 0 {
		return catalogue{}, false
	}
	catalogueMu.Lock()
	defer catalogueMu.Unlock()
	entry, ok := catalogueCache[catalogueKey(credential, planType)]
	if !ok || time.Since(entry.fetched) > ttl {
		return catalogue{}, false
	}
	return entry, true
}

// storeCatalogue records a fetched catalogue.
func storeCatalogue(credential *Credential, entry catalogue) {
	catalogueMu.Lock()
	defer catalogueMu.Unlock()
	catalogueCache[catalogueKey(credential, entry.planType)] = entry
}

// invalidateCatalogue drops the cached catalogues of one account, so a claim that
// changes the tier is visible on the next listing.
func invalidateCatalogue(credential *Credential) {
	account := credential.AccountID()
	catalogueMu.Lock()
	defer catalogueMu.Unlock()
	for key := range catalogueCache {
		if strings.HasPrefix(key, account+"|") {
			delete(catalogueCache, key)
		}
	}
}

// resolvePlanType decides which tier to query `models-v2` with.
//
// `auto` reads the account's real tier from `status-v2` and falls back to Max
// when the plan name is unrecognised — the same choice the reference makes
// (`crates/atomcode-codingplan/src/setup.rs:677-700`).
func resolvePlanType(h *abiboot.Host, cfg Config, credential *Credential) string {
	configured := cfg.PlanType
	if configured != PlanTypeAuto && configured != "" {
		return configured
	}
	status, errStatus := fetchStatus(h, cfg, credential)
	if errStatus != nil || status == nil {
		return planTypeMax
	}
	if tier := status.planTier(); tier != "" {
		return tier
	}
	return planTypeMax
}

// fetchModels calls `GET /coding-plan/models-v2?plan_type=<tier>`
// (`crates/atomcode-codingplan/src/client.rs:252-283`).
func fetchModels(h *abiboot.Host, cfg Config, credential *Credential, planType string) ([]modelEntry, error) {
	endpoint := cfg.codingPlanURL(codingPlanModelsPath) + "?" + url.Values{"plan_type": {planType}}.Encode()
	response, errDo := hostDo(h, http.MethodGet, endpoint, authHeaders(credential), nil)
	if errDo != nil {
		return nil, abiboot.RetryableError("transport", "拉取 AtomCode 模型目录失败：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, abiboot.HTTPError("AUTH", http.StatusUnauthorized, "AtomCode 登录态失效，请重新登录")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.HTTPError(httpErrorCode(response.StatusCode), response.StatusCode,
			"%s", classifyCodingPlanError("models-v2", response.StatusCode, string(response.Body)))
	}
	var decoded []modelEntry
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		return nil, abiboot.Errorf("catalogue_protocol", "解析 models-v2 响应失败: %v", errUnmarshal)
	}
	// An empty list is a legitimate answer when the entitlement has not been
	// provisioned yet (`client.rs:246-251`), and the caller falls back.
	return decoded, nil
}

// catalogueFor returns the account's catalogue, from cache when fresh.
func catalogueFor(h *abiboot.Host, cfg Config, credential *Credential) (catalogue, error) {
	planType := resolvePlanType(h, cfg, credential)
	ttl := time.Duration(cfg.ModelCacheTTLMS) * time.Millisecond
	if cached, ok := cachedCatalogue(credential, planType, ttl); ok {
		return cached, nil
	}
	models, errFetch := fetchModels(h, cfg, credential, planType)
	if errFetch != nil {
		return catalogue{}, errFetch
	}
	// Only entitled models are usable: a `plan_available:false` entry answers
	// 403 on every request, so registering it would offer the user a model that
	// can never answer (`crates/atomcode-codingplan/src/types.rs:160-166`).
	entitled := make([]modelEntry, 0, len(models))
	for _, model := range models {
		if !model.PlanAvailable {
			continue
		}
		if strings.TrimSpace(model.DisplayModelName) == "" {
			continue
		}
		entitled = append(entitled, model)
	}
	entry := catalogue{models: entitled, planType: planType, fetched: time.Now()}
	storeCatalogue(credential, entry)
	return entry, nil
}

// fallbackCatalogue is the bundled table used when discovery is off or fails.
//
// It is a 2026-10-01 snapshot of `models-v2?plan_type=Max` for a
// `CodingPlan Lite-体验版` account — every model the free tier can actually call.
// Unlike the sibling plugins this is NOT a substitute for the remote catalogue:
// the reference keeps no bundled table at all (`setup.rs:924-960` registers
// providers straight from the payload), so this exists only so an offline host
// still lists something.
var fallbackCatalogue = []modelEntry{
	{DisplayModelName: "qwen3.8-27b", ContextWindow: intRef(262_144), SupportsVision: boolRef(true),
		PlanAvailable: true, ReasoningEffortLevels: []string{"low", "medium", "xhigh"}, BaseURL: DefaultGatewayBase, Type: "openai"},
	{DisplayModelName: "glm5.3-flash", ContextWindow: intRef(512_000), SupportsVision: boolRef(true),
		PlanAvailable: true, ReasoningEffortLevels: []string{"low", "high"}, BaseURL: DefaultGatewayBase, Type: "openai"},
	{DisplayModelName: "deepseek-flash", ContextWindow: intRef(1_000_000), SupportsVision: boolRef(true),
		PlanAvailable: true, ReasoningEffortLevels: []string{"high", "max"}, BaseURL: DefaultGatewayBase, Type: "openai"},
}

// intRef and boolRef build pointers for the optional catalogue fields.
func intRef(value int) *int    { return &value }
func boolRef(value bool) *bool { return &value }

// canonicalModelNames maps the gateway's own ids onto the names this channel
// publishes.
//
// The gateway names its models `glm5.3-flash` / `qwen3.8-27b` /
// `deepseek-flash`; every other channel in this deployment publishes
// `GLM-5.3-Flash` / `Qwen3.8-27B` / `DeepSeek-V4.1-Flash`, and users select
// models by that vocabulary. Doing the mapping HERE rather than in the host's
// `oauth-model-alias` table is deliberate:
//
//   - the alias table cannot reliably hand an ALREADY-TAKEN name to a second
//     provider. Measured 2026-10-01: with atomcode, cline and lobsterai all
//     mapping onto `DeepSeek-V4.1-Flash` / `Qwen3.8-27B`, the winners varied
//     between reloads, and atomcode's `qwen3.8-27b` was left unrenamed beside
//     cline's `Qwen3.8-27B` — two entries for one model. (The table is still
//     the right tool when a mapping introduces a NEW name.)
//   - a name published by the plugin merges with the same name from other
//     channels exactly like `plugins/zcode/models.go` documents, adding
//     capacity to the id users already have selected.
//
// The mapping is applied in both directions, so the gateway still receives the
// id it knows (`upstreamModelName`).
//
// `glm5.3-flash` is the same model as z.ai's GLM-5.3-Flash — the upstream
// client says so itself ("glm-5.3-flash at z.ai", measured 2026-09-25,
// `crates/atomcode-coding/src/next_prompt_suggestion.rs:47`). The other two
// follow the naming the rest of this deployment already uses for the same
// families.
var canonicalModelNames = map[string]string{
	"glm5.3-flash":   "GLM-5.3-Flash",
	"qwen3.8-27b":    "Qwen3.8-27B",
	"deepseek-flash": "DeepSeek-V4.1-Flash",
}

// canonicalModelName returns the name this channel publishes for an upstream id.
func canonicalModelName(upstream string) string {
	if name, ok := canonicalModelNames[upstream]; ok {
		return name
	}
	return upstream
}

// upstreamModelName reverses it, so a request that used the published name
// reaches the gateway under the id it actually serves.
func upstreamModelName(published string) string {
	for upstream, name := range canonicalModelNames {
		if name == published {
			return upstream
		}
	}
	return published
}

// measuredExtraModels carries SERVER-PUBLISHED metadata for ids this adapter has
// seen in a real `models-v2` payload but which the server no longer lists.
//
// Only ids with captured server metadata belong here; anything else configured
// through `extra_models` is advertised as an id with no claims attached, rather
// than with a guessed context window or a guessed vision capability.
//
// `deepseek-flash` was captured 2026-10-01 from
// `GET /coding-plan/models-v2?plan_type=Max` before the platform dropped it:
//
//	{"display_model_name":"deepseek-flash","context_window":1000000,
//	 "supports_vision":true,"reasoning_effort_levels":["high","max"], ...}
//
// The id keeps answering HTTP 200 with real content, which is why it is worth
// offering at all.
var measuredExtraModels = map[string]modelEntry{
	"deepseek-flash": {
		DisplayModelName:      "deepseek-flash",
		ContextWindow:         intRef(1_000_000),
		SupportsVision:        boolRef(true),
		PlanAvailable:         true,
		ReasoningEffortLevels: []string{"high", "max"},
		BaseURL:               DefaultGatewayBase,
		Type:                  "openai",
	},
}

// extraEntries turns the configured ids into catalogue entries, dropping any the
// catalogue already carries.
func extraEntries(configured []string, existing []modelEntry) []modelEntry {
	if len(configured) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(existing))
	for _, entry := range existing {
		seen[entry.DisplayModelName] = true
	}
	out := make([]modelEntry, 0, len(configured))
	for _, id := range configured {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		if known, ok := measuredExtraModels[trimmed]; ok {
			known.FromConfig = true
			out = append(out, known)
			continue
		}
		// Unknown id: advertise it with NO claims. A guessed context window or
		// vision flag is worse than an absent one, because the client acts on
		// it (`plugins/codearts/models.go` makes the same choice).
		out = append(out, modelEntry{
			DisplayModelName: trimmed,
			PlanAvailable:    true,
			FromConfig:       true,
			BaseURL:          DefaultGatewayBase,
			Type:             "openai",
		})
	}
	return out
}

// staticModelEntries returns the serving catalogue: the live one when discovery
// is enabled and succeeds, the bundled one otherwise.
func staticModelEntries(h *abiboot.Host, cfg Config, credential *Credential) []modelEntry {
	if credential == nil {
		return fallbackCatalogue
	}
	if !cfg.DiscoverModels {
		return append(append([]modelEntry(nil), fallbackCatalogue...), extraEntries(cfg.ExtraModels, fallbackCatalogue)...)
	}
	entry, errCatalogue := catalogueFor(h, cfg, credential)
	if errCatalogue != nil || len(entry.models) == 0 {
		if h != nil && errCatalogue != nil {
			h.Log("warn", "AtomCode 模型目录获取失败，回退内置列表", map[string]any{"error": errCatalogue.Error()})
		}
		return append(append([]modelEntry(nil), fallbackCatalogue...), extraEntries(cfg.ExtraModels, fallbackCatalogue)...)
	}
	// Operator-configured extras ride ON TOP of the discovered catalogue: the
	// server list stays authoritative, and the extras are visibly additions.
	return append(append([]modelEntry(nil), entry.models...), extraEntries(cfg.ExtraModels, entry.models)...)
}

// modelInfos projects catalogue entries onto the host's model records.
func modelInfos(entries []modelEntry, cfg Config, credential *Credential) []pluginapi.ModelInfo {
	prefix := ""
	if cfg.ModelPrefix && credential != nil {
		prefix = modelPrefixFor(credential)
	}
	out := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		out = append(out, modelInfoFor(entry, prefix))
	}
	return out
}

// modelInfoFor renders one model record.
func modelInfoFor(entry modelEntry, prefix string) pluginapi.ModelInfo {
	contextWindow := entry.effectiveContextWindow()
	modalities := []string{"text"}
	if entry.acceptsImages() {
		// Measured 2026-10-01: the gateway accepts OpenAI `image_url` content
		// parts on every model that declares `supports_vision`.
		modalities = append(modalities, "image")
	}
	display := canonicalModelName(entry.DisplayModelName)
	info := pluginapi.ModelInfo{
		ID:                         prefix + display,
		Object:                     "model",
		Created:                    time.Now().Unix(),
		OwnedBy:                    ProviderKey,
		Type:                       "chat",
		DisplayName:                display,
		Name:                       prefix + display,
		Description:                modelDescription(display, entry.FromConfig),
		ContextLength:              int64(contextWindow),
		InputTokenLimit:            int64(contextWindow),
		MaxCompletionTokens:        defaultMaxOutputTokens,
		OutputTokenLimit:           defaultMaxOutputTokens,
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   modalities,
		SupportedOutputModalities:  []string{"text"},
	}
	if len(entry.ReasoningEffortLevels) > 0 {
		// Declaring the levels is what makes the thinking selector appear at all.
		// `ThinkingSupport` has no default-effort field, so only the list can be
		// published — the same limitation the sibling TRAE and Loomy plugins
		// document. The gateway has no documented literal for "no thinking", so
		// `ZeroAllowed` stays false and `off` is not offered
		// (`crates/atomcode-codingplan/src/types.rs:168-176`).
		info.Thinking = &pluginapi.ThinkingSupport{
			Levels: append([]string(nil), entry.ReasoningEffortLevels...),
		}
	}
	return info
}

// modelDescription labels a model, naming the ones this adapter added by hand.
//
// The label is the only place a user can tell an entitlement from an addition,
// so it says so rather than leaving two identical-looking rows.
func modelDescription(display string, fromConfig bool) string {
	if fromConfig {
		return "AtomCode " + display + "（补充模型：服务端目录未下发，由 extra_models 添加）"
	}
	return "AtomCode " + display
}

// modelPrefixFor is the account token exposed ahead of a model id.
func modelPrefixFor(credential *Credential) string {
	identity := credential.AccountID()
	if identity == "" {
		return ""
	}
	if len(identity) > 8 {
		identity = identity[:8]
	}
	return identity + "/"
}

// stripModelPrefix removes the account prefix from a requested model id.
func stripModelPrefix(model string, credential *Credential) string {
	prefix := modelPrefixFor(credential)
	if prefix != "" && strings.HasPrefix(model, prefix) {
		return strings.TrimPrefix(model, prefix)
	}
	return model
}

// findModel locates a model in the serving catalogue.
func findModel(entries []modelEntry, model string) (modelEntry, bool) {
	for _, entry := range entries {
		if entry.DisplayModelName == model {
			return entry, true
		}
	}
	return modelEntry{}, false
}

// handleModelRegister declares the provider's model list at registration time.
//
// No credential is available yet, so this is the bundled snapshot; the live
// catalogue arrives through `model.for_auth` once an account is bound.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelRegistrationResponse{Provider: ProviderKey, Models: modelInfos(fallbackCatalogue, settings(), nil)}, nil
}

// handleModelStatic is the model.static variant of the same list.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: modelInfos(fallbackCatalogue, settings(), nil)}, nil
}

// handleModelForAuth reports the catalogue for one bound account.
//
// Without a usable credential the reply is an empty list and never an error: an
// error would be reported as a catalogue failure, while an empty list simply
// hides the provider group. When discovery is disabled or the catalogue call
// fails, the bundled snapshot is used so the user still sees a usable list.
func handleModelForAuth(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil || credential.AccessToken == "" {
		// A credential that cannot be parsed is a real fault worth surfacing: it
		// silently hides every model of the provider.
		if h != nil && errCredential != nil {
			h.Log("warn", "AtomCode 凭据解析失败，无法列出模型",
				map[string]any{"error": errCredential.Error(), "auth_id": request.AuthID})
		}
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: []pluginapi.ModelInfo{}}, nil
	}
	cfg := settings()
	entries := staticModelEntries(h, cfg, credential)
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: modelInfos(entries, cfg, credential)}, nil
}
