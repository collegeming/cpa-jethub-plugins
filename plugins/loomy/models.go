package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Model catalogue, ported from `src/loomy.ts:111-191` and
// `src/loomy-adapter.ts:58-112`.
//
// The multiplier a model costs is embedded in its NAME and nowhere else: a
// search for `credit`/`multiplier`/`price`/`factor`/`rate` in the `/models`
// response returns zero hits (`README.md:1440-1441`). The name may carry the
// multiplier in three spellings — `MiniMax M3 （x4.0）`, `Qwen 3.8 Max (x12.0)`,
// `GLM 5.3 Flash(x0.8)` — plus the normalised `· x` form this plugin renders.
//
// The multiplier is presented in the model NAME, never in the description,
// because the composer's model switcher renders only `name`
// (`loomy.ts:151-156`).

// defaultMaxOutputTokens is the per-response cap advertised to the host. Loomy
// publishes no output limit, so a conservative value is advertised rather than a
// fabricated per-model one.
const defaultMaxOutputTokens = 65536

// Rate suffix patterns (`loomy.ts:125-148`). Both are anchored at BOTH ends, so
// only a TRAILING multiplier is recognised: a bracket in the middle belongs to
// the model name (`loomy.ts:121`).
var (
	// bracketRatePattern matches `name （x4.0）`, `name (x12.0)` and
	// `name(x0.8)` — full-width or half-width brackets, optional spaces.
	bracketRatePattern = regexp.MustCompile(`(?i)^(.*?)\s*[（(]\s*(x\s*[0-9.]+)\s*[)）]\s*$`)
	// normalisedRatePattern matches the `name · x4.0` form this plugin produces.
	normalisedRatePattern = regexp.MustCompile(`(?i)^(.*?)\s*·\s*(x\s*[0-9.]+)\s*$`)
	// spaceRuns collapses whitespace inside a multiplier, the Go equivalent of
	// `String(rate).replace(/\s+/g,'')`.
	spaceRuns = regexp.MustCompile(`\s+`)
)

// splitLoomyRate splits a model name into its bare name and its multiplier.
//
// An empty rate means "no multiplier" — image models have none
// (`loomy.ts:123`). When the bracket form captures an empty body the WHOLE
// original string is kept and the rate is dropped, matching `loomy.ts:134-136`.
//
// The function is idempotent, which the source states explicitly
// (`loomy.ts:118-119`): `splitLoomyRate(loomyDisplayName(x)) == splitLoomyRate(x)`.
// A naive port that strips the `· x` suffix only would make `resolveModel` return
// `Spark X2.5 · x0.1` instead of `Spark X2.5` (trap #15).
func splitLoomyRate(name string) (string, string) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ""
	}
	for _, pattern := range []*regexp.Regexp{bracketRatePattern, normalisedRatePattern} {
		matches := pattern.FindStringSubmatch(trimmed)
		if matches == nil {
			continue
		}
		body := strings.TrimSpace(matches[1])
		if body == "" {
			// `（x4.0）` alone carries no model name: keep the original text and
			// report no multiplier rather than an empty model.
			return trimmed, ""
		}
		return body, normalizeRate(matches[2])
	}
	return trimmed, ""
}

// normalizeRate is `String(rate).replace(/\s+/g,”).toLowerCase()`:
// `x 4.0` becomes `x4.0` (`loomy.ts:133,143`).
func normalizeRate(rate string) string {
	return strings.ToLower(spaceRuns.ReplaceAllString(strings.TrimSpace(rate), ""))
}

// loomyDisplayName renders the name a user sees (`loomy.ts:157-160`). With no
// multiplier the bare name is returned — never a dangling separator.
func loomyDisplayName(name string) string {
	bare, rate := splitLoomyRate(name)
	if rate == "" {
		return bare
	}
	return bare + " · " + rate
}

// modelDescriptor is one catalogue entry, from the bundled table or from a live
// `/models` response.
type modelDescriptor struct {
	// ID is the `model` value sent upstream (`loomy-adapter.ts:81`).
	ID string
	// Name is the raw wire name, multiplier included.
	Name string
	// ContextLength is `context_length`; 0 means unknown and must NOT be
	// replaced by a guess (`loomy-adapter.ts:86,93`).
	ContextLength int64
	// SupportsImage comes from `capabilities.input_modalities` containing
	// `image`. It says the model ACCEPTS images — never filter the catalogue on
	// it, because 5 of the 8 chat models declare it and it has nothing to do with
	// image generation (`loomy.ts:165-167`, trap #16).
	SupportsImage bool
	// SupportsThinking is `capabilities.reasoning === true`
	// (`loomy-adapter.ts:95`). It is parsed for display only: the provider
	// declares no thinking LEVELS, because neither `listModels` nor
	// `resolveModel` emits any reasoning control in the source
	// (`loomy-adapter.ts:209-215,224-235`, §7.6).
	SupportsThinking bool
	// Remote marks an entry that came from the live endpoint.
	Remote bool
}

// displayName is the name rendered by management clients.
func (m modelDescriptor) displayName() string { return loomyDisplayName(m.Name) }

// bareName is the name sent upstream: `resolveModel` strips the multiplier
// (`loomy-adapter.ts:218-236`).
func (m modelDescriptor) bareName() string {
	bare, _ := splitLoomyRate(m.Name)
	return bare
}

// info converts the descriptor into the host-facing model metadata.
func (m modelDescriptor) info(now time.Time) pluginapi.ModelInfo {
	modalities := []string{"text"}
	if m.SupportsImage {
		modalities = append(modalities, "image")
	}
	description := "Loomy（讯飞办公助手）模型"
	if m.Remote {
		description = "来自 GET /models 的实时目录"
	}
	info := pluginapi.ModelInfo{
		ID:                         m.ID,
		Object:                     "model",
		Created:                    now.Unix(),
		OwnedBy:                    ProviderKey,
		Type:                       "chat",
		DisplayName:                m.displayName(),
		Name:                       m.bareName(),
		Description:                description,
		ContextLength:              m.ContextLength,
		InputTokenLimit:            m.ContextLength,
		MaxCompletionTokens:        defaultMaxOutputTokens,
		OutputTokenLimit:           defaultMaxOutputTokens,
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   modalities,
		SupportedOutputModalities:  []string{"text"},
	}
	// No Thinking block: §7.6 — the source never surfaces reasoning controls.
	return info
}

// fallbackCatalogue is the bundled table (`loomy-product.ts:74-83`).
//
// Order is the remote order and is deliberately NOT re-sorted
// (`loomy-product.ts:65-67`). Entries hard-code `supportsImage: false` — "rather
// under-report than advertise a modality the server may reject" — and
// `supportsThinking: true` (`loomy-adapter.ts:101-112`).
//
// ⚠️ `spark-x`: the remote declares 1048576 while the Loomy client forces 262144
// through a local override. This port follows the remote value and keeps
// 1_048_576; switch it to 262144 if long-context requests are rejected in
// practice (`loomy-product.ts:69-72`, `README.md:1455-1457`).
var fallbackCatalogue = []modelDescriptor{
	{ID: "deepseek-v4-flash-0731", Name: "DeepSeek V4 Flash 0731 · x3.0", ContextLength: 1_048_576, SupportsThinking: true},
	{ID: "MiniMax-M3", Name: "MiniMax M3 （x4.0）", ContextLength: 1_048_576, SupportsThinking: true},
	{ID: "Kimi-k2.6", Name: "Kimi k2.6 · x6.5", ContextLength: 262_144, SupportsThinking: true},
	{ID: "qwen-3.8-max", Name: "Qwen 3.8 Max (x12.0)", ContextLength: 1_000_000, SupportsThinking: true},
	{ID: "GLM-5.3-Flash", Name: "GLM 5.3 Flash(x0.8)", ContextLength: 1_048_576, SupportsThinking: true},
	{ID: "qwen3.8-flash", Name: "qwen 3.8 flash · x0.8", ContextLength: 1_000_000, SupportsThinking: true},
	{ID: "spark-x", Name: "Spark X2.5 · x0.1", ContextLength: 1_048_576, SupportsThinking: true},
	{ID: "mimo-v2.5", Name: "MiMo V2.5 · x3.3", ContextLength: 1_048_576, SupportsThinking: true},
}

// fallbackModels renders the bundled table.
func fallbackModels() []modelDescriptor {
	out := make([]modelDescriptor, len(fallbackCatalogue))
	copy(out, fallbackCatalogue)
	return out
}

// staticModelInfos renders the catalogue for `model.register` / `model.static`.
// It never touches the network.
func staticModelInfos() []pluginapi.ModelInfo {
	now := time.Now()
	entries := fallbackModels()
	infos := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, entry.info(now))
	}
	return infos
}

// remoteModelsPayload is the live `/models` response. It is either a bare array
// or an object carrying a `data` array (`loomy-adapter.ts:74-79`).
type remoteModelsPayload struct {
	Data []remoteModel `json:"data"`
}

// remoteModel is one wire entry.
type remoteModel struct {
	ID            flexText `json:"id"`
	Type          flexText `json:"type"`
	Name          flexText `json:"name"`
	ContextLength *float64 `json:"context_length"`
	Capabilities  struct {
		InputModalities []string `json:"input_modalities"`
		Reasoning       *bool    `json:"reasoning"`
	} `json:"capabilities"`
}

// parseLoomyRemoteModels parses a `/models` body (`loomy-adapter.ts:73-99`).
//
// Anything that is neither a bare array nor an object with `data` yields an empty
// slice. Only `type == "chat"` entries with a non-empty id are kept — that filter
// is the ONLY one, and in particular the catalogue must not be filtered on
// `input_modalities` (trap #16).
func parseLoomyRemoteModels(body []byte) []modelDescriptor {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil
	}
	var entries []remoteModel
	if strings.HasPrefix(trimmed, "[") {
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &entries); errUnmarshal != nil {
			return nil
		}
	} else {
		var payload remoteModelsPayload
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &payload); errUnmarshal != nil {
			return nil
		}
		entries = payload.Data
	}

	out := make([]modelDescriptor, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID.String())
		if id == "" || !strings.EqualFold(strings.TrimSpace(entry.Type.String()), "chat") {
			continue
		}
		name := strings.TrimSpace(entry.Name.String())
		if name == "" {
			// The display name falls back to the id when `name` is empty
			// (`loomy-adapter.ts:85,92`).
			name = id
		}
		descriptor := modelDescriptor{ID: id, Name: name, Remote: true}
		if entry.ContextLength != nil && *entry.ContextLength > 0 && *entry.ContextLength < 1e12 {
			descriptor.ContextLength = int64(*entry.ContextLength)
		}
		for _, modality := range entry.Capabilities.InputModalities {
			if strings.EqualFold(strings.TrimSpace(modality), "image") {
				descriptor.SupportsImage = true
			}
		}
		if entry.Capabilities.Reasoning != nil {
			descriptor.SupportsThinking = *entry.Capabilities.Reasoning
		}
		out = append(out, descriptor)
	}
	return out
}

// discoveredCatalogue caches the live `/models` result.
var discoveredCatalogue struct {
	mu        sync.Mutex
	models    []modelDescriptor
	fetchedAt time.Time
}

// cachedModels returns the cached catalogue when it is still fresh.
func cachedModels(ttl time.Duration) []modelDescriptor {
	discoveredCatalogue.mu.Lock()
	defer discoveredCatalogue.mu.Unlock()
	if len(discoveredCatalogue.models) == 0 || ttl <= 0 {
		return nil
	}
	if time.Since(discoveredCatalogue.fetchedAt) > ttl {
		return nil
	}
	out := make([]modelDescriptor, len(discoveredCatalogue.models))
	copy(out, discoveredCatalogue.models)
	return out
}

// putCachedModels stores a discovered catalogue.
func putCachedModels(models []modelDescriptor) {
	discoveredCatalogue.mu.Lock()
	discoveredCatalogue.models = append([]modelDescriptor(nil), models...)
	discoveredCatalogue.fetchedAt = time.Now()
	discoveredCatalogue.mu.Unlock()
}

// resetDiscoveredModels drops the cache; used by tests and by reconfiguration.
func resetDiscoveredModels() {
	discoveredCatalogue.mu.Lock()
	discoveredCatalogue.models = nil
	discoveredCatalogue.fetchedAt = time.Time{}
	discoveredCatalogue.mu.Unlock()
}

// discoverModels performs the live `GET /models` call.
//
// ⚠️ The header is the lowercase `token`, NEVER `Authorization: Bearer`: Bearer
// yields HTTP 200 with `100002 缺少 token` and the catalogue then silently stays
// on the fallback table (`index.ts:459-481`). Non-2xx yields no models
// (`index.ts:479`).
func discoverModels(h *abiboot.Host, credential *Credential, cfg Config) []modelDescriptor {
	if credential == nil {
		return nil
	}
	response, errDo := hostRequest(h, http.MethodGet, APIBase+ModelsPath, businessHeaders(credential, false), nil, cfg)
	if errDo != nil {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	parsed, errEnvelope := parseEnvelope(response.Body)
	if errEnvelope != nil {
		// Some deployments answer with a bare array; the spec's parser tolerates
		// both shapes, so try the raw body before giving up.
		return parseLoomyRemoteModels(response.Body)
	}
	if !parsed.success() {
		return nil
	}
	if models := parseLoomyRemoteModels(parsed.Data); len(models) > 0 {
		return models
	}
	return parseLoomyRemoteModels(response.Body)
}

// catalogueForAuth resolves the catalogue for one credential.
func catalogueForAuth(h *abiboot.Host, credential *Credential, cfg Config) []modelDescriptor {
	ttl := time.Duration(cfg.modelCacheTTL()) * time.Millisecond
	if cached := cachedModels(ttl); len(cached) > 0 {
		return cached
	}
	if discovered := discoverModels(h, credential, cfg); len(discovered) > 0 {
		putCachedModels(discovered)
		return discovered
	}
	// A remote failure silently falls back to the bundled table
	// (`loomy-adapter.ts:174-191`).
	return fallbackModels()
}

// activeCatalogue resolves the catalogue the provider will actually offer: the
// bundled table, or the live one when discovery is enabled and a credential is
// available. It is the single place pages and JSON views agree on.
func activeCatalogue(h *abiboot.Host, credential *Credential, cfg Config) []modelDescriptor {
	if !cfg.DiscoverModels || credential == nil {
		return fallbackModels()
	}
	return catalogueForAuth(h, credential, cfg)
}

// handleModelRegister reports the static fallback catalog. It is used when no
// account is bound yet, so it must not touch the network.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelRegistrationResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// handleModelStatic is the model.static variant of the same catalog.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// handleModelForAuth reports the catalog for one bound account, using the live
// listing when it is enabled and available.
//
// A credential that cannot be parsed still yields the bundled table rather than
// an error, and an empty remote answer falls back too: hiding the provider would
// break routing, while an empty list is exactly what the source returns when no
// account is logged in (`loomy-adapter.ts:197-201`, trap #18 — the "no account"
// case is `model.static`, which is the same bundled table).
func handleModelForAuth(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := settings()
	credential, errParse := ParseCredential(request.StorageJSON)
	if errParse != nil || !cfg.DiscoverModels {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
	}
	entries := catalogueForAuth(h, credential, cfg)
	now := time.Now()
	infos := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, entry.info(now))
	}
	if len(infos) == 0 {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
	}
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: infos}, nil
}
