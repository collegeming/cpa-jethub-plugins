package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The ZCode model catalogue.
//
// ## Only the two models that were measured to answer are exposed
//
// The upstream pool lists four (`GLM-5-Turbo`, `GLM-5.2`, `GLM-5.3`,
// `GLM-5.3-Flash`), but the first two return EMPTY responses under the Start Plan
// entitlement: the reference measured 0/3 correct for them against 3/3 for
// `GLM-5.3`. Listing a model that cannot answer is worse than not listing it —
// the user picks it and gets silence.
//
// ## Context window, output cap and thinking levels come from upstream
//
// `GET /api/v1/client/configs` is authoritative. A real defect in the reference
// came from guessing instead: the fallback table carried `200_000` / `32_768`
// while upstream publishes `1_000_000` / `128_000`, and the thinking levels were
// missing entirely, so the effort selector never appeared even though the IDE had
// one. Capability fields are copied from upstream, never inferred from "the same
// family should be the same" — `capabilities.vision` is present on Flash and
// absent on `GLM-5.3`, and guessing `true` for both was wrong.
//
// ## ⚠ Naming collision — expected, and deliberately not worked around
//
// `GLM-5.3` and `GLM-5.3-Flash` are already published by other channels in this
// deployment. CPA merges same-named models across credentials into one entry
// served by several credentials, so ZCode adds CAPACITY to the existing model ids
// rather than creating new ones. Renaming or prefixing them to avoid the
// collision would split that capacity apart and break the ids users already have
// selected.

// fallbackModel is one entry of the built-in catalogue.
type fallbackModel struct {
	ID              string
	Name            string
	ContextWindow   int
	MaxOutputTokens int
	SupportsImage   bool
	// ReasoningLevels are the accepted `output_config.effort` values, in the
	// display order the IDE uses (`low → high → max`).
	ReasoningLevels []string
	// DefaultReasoningLevel is upstream's `reasoning.defaultLevel`. The host type
	// has NO default-effort field, so this is carried for the management page and
	// the request path, not for `ModelInfo`.
	DefaultReasoningLevel string
}

// fallbackCatalogue is the built-in table, whose values are the upstream ones
// (they were read off `client/configs`; they are a FALLBACK, not a guess).
var fallbackCatalogue = []fallbackModel{
	{
		ID:              "GLM-5.3-Flash",
		Name:            "GLM-5.3-Flash",
		ContextWindow:   1_000_000,
		MaxOutputTokens: 128_000,
		// upstream `capabilities.vision === true`.
		SupportsImage:         true,
		ReasoningLevels:       []string{"low", "high", "max"},
		DefaultReasoningLevel: "max",
	},
	{
		ID:              "GLM-5.3",
		Name:            "GLM-5.3",
		ContextWindow:   1_000_000,
		MaxOutputTokens: 128_000,
		// ⚠ upstream `capabilities` is an EMPTY OBJECT for this model, so it has
		// no vision. Marking it `true` because "the family should be the same"
		// was a real defect in the reference.
		SupportsImage:         false,
		ReasoningLevels:       []string{"low", "high", "max"},
		DefaultReasoningLevel: "max",
	},
}

// catalogueMu guards the discovered catalogue.
var (
	catalogueMu       sync.RWMutex
	discoveredCatalog []fallbackModel
)

// resetDiscoveredModels drops the cached remote catalogue.
func resetDiscoveredModels() {
	catalogueMu.Lock()
	discoveredCatalog = nil
	catalogueMu.Unlock()
}

// storeDiscoveredModels caches a remote catalogue.
func storeDiscoveredModels(models []fallbackModel) {
	catalogueMu.Lock()
	discoveredCatalog = models
	catalogueMu.Unlock()
}

// currentCatalogue returns the catalogue to publish: the discovered one when it
// was fetched successfully, the built-in table otherwise.
func currentCatalogue() []fallbackModel {
	catalogueMu.RLock()
	discovered := discoveredCatalog
	catalogueMu.RUnlock()
	if len(discovered) > 0 {
		return discovered
	}
	return fallbackCatalogue
}

// catalogueModel looks an id up in a catalogue.
func catalogueModel(catalogue []fallbackModel, id string) (fallbackModel, bool) {
	for _, model := range catalogue {
		if model.ID == id {
			return model, true
		}
	}
	return fallbackModel{}, false
}

// remoteModelsEnabled reports whether the catalogue for a credential may include
// the models the entitlement check accepted. De-duplication of auto-discovered
// models is the only filtering here; the reference's browser-derived models are
// deliberately absent.
func restrictToVerified(models []fallbackModel) []fallbackModel {
	verified := make([]fallbackModel, 0, len(fallbackCatalogue))
	for _, model := range models {
		if _, ok := catalogueModel(fallbackCatalogue, model.ID); ok {
			verified = append(verified, model)
		}
	}
	if len(verified) == 0 {
		return fallbackCatalogue
	}
	return verified
}

// modelInfoFor builds the host-facing descriptor for one catalogue entry.
func modelInfoFor(model fallbackModel, prefix string, now time.Time) pluginapi.ModelInfo {
	modalities := []string{"text"}
	if model.SupportsImage {
		modalities = append(modalities, "image")
	}
	id := model.ID
	displayName := model.Name
	if prefix != "" {
		id = prefix + "/" + model.ID
		displayName = prefix + "/" + model.Name
	}
	info := pluginapi.ModelInfo{
		ID:                         id,
		Object:                     "model",
		Created:                    now.Unix(),
		OwnedBy:                    ProviderKey,
		Type:                       "chat",
		DisplayName:                displayName,
		Name:                       model.ID,
		Description:                "ZCode 免费额度通道（Anthropic Messages）：" + model.Name,
		ContextLength:              int64(model.ContextWindow),
		InputTokenLimit:            int64(model.ContextWindow),
		MaxCompletionTokens:        int64(model.MaxOutputTokens),
		OutputTokenLimit:           int64(model.MaxOutputTokens),
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   modalities,
		SupportedOutputModalities:  []string{"text"},
	}
	// Levels come straight from upstream's `reasoning.levels` keys, in display
	// order. Min/Max are not published, so they stay unset rather than guessed.
	//
	// ⚠ CPA's `ThinkingSupport` has no default-effort field, so upstream's
	// `reasoning.defaultLevel` (`max`) cannot be declared here — only the level
	// list is. The same limitation is recorded by the sibling `trae` plugin
	// (`plugins/trae/models.go`, around its `ThinkingSupport` usage).
	if len(model.ReasoningLevels) > 0 {
		info.Thinking = &pluginapi.ThinkingSupport{Levels: append([]string(nil), model.ReasoningLevels...)}
	}
	return info
}

// modelInfos renders a catalogue for the host.
func modelInfos(catalogue []fallbackModel, prefix string) []pluginapi.ModelInfo {
	now := time.Now()
	infos := make([]pluginapi.ModelInfo, 0, len(catalogue))
	for _, model := range catalogue {
		infos = append(infos, modelInfoFor(model, prefix, now))
	}
	return infos
}

// staticPrefix is the model prefix a static (unbound) catalogue carries.
//
// A static catalogue has no account behind it, so it carries no prefix even when
// `model_prefix` is on: inventing one would publish ids nothing can serve.
func staticPrefix() string { return "" }

// handleModelRegister reports the static catalog. It must not touch the network.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelRegistrationResponse{
		Provider: ProviderKey,
		Models:   modelInfos(currentCatalogue(), staticPrefix()),
	}, nil
}

// handleModelStatic is the model.static variant of the same catalog.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelResponse{
		Provider: ProviderKey,
		Models:   modelInfos(currentCatalogue(), staticPrefix()),
	}, nil
}

// handleModelForAuth reports the catalog for one bound account.
//
// The prefix follows the credential's own identity so two accounts produce two
// disjoint id sets; a credential that cannot be parsed still yields the
// unprefixed catalogue rather than an error, because the listing is advisory and
// hiding it would break routing.
func handleModelForAuth(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	prefix := ""
	if credential, errParse := ParseCredential(request.StorageJSON); errParse == nil {
		prefix = modelPrefixFor(credential)
	}
	return pluginapi.ModelResponse{
		Provider: ProviderKey,
		Models:   modelInfos(currentCatalogue(), prefix),
	}, nil
}

// modelPrefixFor renders the per-account model prefix.
func modelPrefixFor(credential *Credential) string {
	if !settings().ModelPrefix || credential == nil {
		return ""
	}
	prefix := credential.UserID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return prefix
}

// refreshRemoteCatalogue fetches `client/configs` and caches the authoritative
// window, output cap and reasoning levels.
//
// A failure is silent by design: the catalogue is display information, and a
// provider that fails to enumerate its models must not become unusable. The
// built-in table, whose values match upstream, stays in place instead.
func refreshRemoteCatalogue(h *abiboot.Host, credential *Credential, cfg Config) error {
	if !cfg.DiscoverModels || credential == nil {
		return nil
	}
	models, errFetch := fetchRemoteModels(h, credential, cfg)
	if errFetch != nil || len(models) == 0 {
		return errFetch
	}
	storeDiscoveredModels(models)
	return nil
}

// fetchRemoteModels reads the model pool out of `GET /api/v1/client/configs`.
//
// ⚠ `builtinModels` is an OBJECT keyed by an index string, not an array —
// measured as `{"0": {...}, "1": {...}}`. Testing it with `Array.isArray` yields
// the false negative "zero models", which is exactly the bug that made the
// reference fall back to its guessed table.
func fetchRemoteModels(h *abiboot.Host, credential *Credential, cfg Config) ([]fallbackModel, error) {
	appVersion := credential.appVersionOrDefault(cfg)
	rawURL := Origin + ClientConfigsPath +
		"?app_version=" + urlQueryEscape(appVersion) + "&platform=unknown"
	headers := buildHeaders(credential, cfg, headerOptions{Authorization: true, Accept: "application/json"})
	response, errDo := hostRequest(h, http.MethodGet, rawURL, headers, nil)
	if errDo != nil {
		return nil, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, transportError("configs_failed", "读取 client/configs 失败（HTTP %d）", response.StatusCode)
	}
	envelope := parseUpstreamEnvelope(response.Body, response.StatusCode)
	if !envelope.success() {
		return nil, transportError("configs_rejected", "client/configs 返回业务错误：%s",
			envelope.message("code "+strconv.Itoa(envelope.Code.value)))
	}
	data, errData := envelopeData(envelope, response.Body)
	if errData != nil {
		return nil, errData
	}
	models, errParse := parseBuiltinModels(data["builtinModels"])
	if errParse != nil {
		return nil, errParse
	}
	verified := restrictToVerified(models)
	if len(verified) == 0 {
		return nil, transportError("configs_empty", "client/configs 没有返回任何已验证可用的模型")
	}
	return verified, nil
}

// parseBuiltinModels reads the `builtinModels` object (or array).
//
// Both spellings are accepted: the measured shape is an object, and an array
// would carry the same entries.
func parseBuiltinModels(raw any) ([]fallbackModel, error) {
	entries := make([]map[string]any, 0, 4)
	switch typed := raw.(type) {
	case map[string]any:
		for _, value := range typed {
			if entry, ok := value.(map[string]any); ok {
				entries = append(entries, entry)
			}
		}
	case []any:
		for _, value := range typed {
			if entry, ok := value.(map[string]any); ok {
				entries = append(entries, entry)
			}
		}
	default:
		return nil, transportError("configs_shape", "client/configs 的 builtinModels 既不是对象也不是数组")
	}

	models := make([]fallbackModel, 0, len(entries))
	for _, entry := range entries {
		id := firstNonEmpty(stringField(entry, "modelId"), stringField(entry, "id"))
		if id == "" {
			continue
		}
		model := fallbackModel{
			ID:   id,
			Name: firstNonEmpty(stringField(entry, "name"), id),
		}
		// Conservative values when the field is absent: 0 would make the host
		// believe the model has no window at all.
		if window, ok := nonNegativeInt(entry["contextWindow"]); ok && window > 0 {
			model.ContextWindow = window
		} else {
			model.ContextWindow = 200_000
		}
		maxOutput := entry["maxCompletionTokens"]
		if maxOutput == nil {
			maxOutput = entry["maxTokens"]
		}
		if tokens, ok := nonNegativeInt(maxOutput); ok && tokens > 0 {
			model.MaxOutputTokens = tokens
		} else {
			model.MaxOutputTokens = 32_768
		}
		if capabilities, ok := entry["capabilities"].(map[string]any); ok {
			model.SupportsImage = boolField(capabilities, "vision")
		}
		if reasoning, ok := entry["reasoning"].(map[string]any); ok {
			if levels, ok := reasoning["levels"].(map[string]any); ok && len(levels) > 0 {
				keys := make([]string, 0, len(levels))
				for key := range levels {
					keys = append(keys, key)
				}
				model.ReasoningLevels = orderReasoningLevels(keys)
			}
			model.DefaultReasoningLevel = stringField(reasoning, "defaultLevel")
		}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, transportError("configs_empty", "client/configs 的 builtinModels 里没有可用条目")
	}
	return models, nil
}

// reasoningLevelOrder is the IDE's display order.
//
// ⚠ The object's KEY ORDER cannot be used directly: upstream's JSON inserts them
// as `low, max, high` while the IDE renders `low, high, max` (user screenshot as
// evidence). Known levels are sorted into the published order and unknown ones
// are appended in the order they arrived, so a new level upstream is not dropped.
var reasoningLevelOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// orderReasoningLevels sorts level keys into the display order.
func orderReasoningLevels(keys []string) []string {
	known := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, level := range reasoningLevelOrder {
		for _, key := range keys {
			if key == level && !seen[key] {
				known = append(known, key)
				seen[key] = true
			}
		}
	}
	for _, key := range keys {
		if !seen[key] {
			known = append(known, key)
			seen[key] = true
		}
	}
	return known
}

// reasoningLevelSupported reports whether a model accepts a level.
func reasoningLevelSupported(catalogue []fallbackModel, modelID, level string) bool {
	model, ok := catalogueModel(catalogue, modelID)
	if !ok {
		return false
	}
	for _, candidate := range model.ReasoningLevels {
		if candidate == level {
			return true
		}
	}
	return false
}

// urlQueryEscape escapes one query value the way a URL needs it.
func urlQueryEscape(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~':
			builder.WriteRune(r)
		default:
			for _, b := range []byte(string(r)) {
				builder.WriteString("%")
				builder.WriteString(hexDigit(b >> 4))
				builder.WriteString(hexDigit(b & 0x0f))
			}
		}
	}
	return builder.String()
}

// hexDigit renders one nibble as an upper-case hex digit.
func hexDigit(value byte) string {
	const digits = "0123456789ABCDEF"
	return string(digits[value&0x0f])
}
