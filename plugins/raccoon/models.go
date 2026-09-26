package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Model catalogue, ported from `raccoon-auth.ts:118-164,380-410,532-567` and
// `raccoon-adapter.ts:114-205`.
//
// ⚠️ The wire field `name` IS the model id (`raccoon-auth.ts:120-121`); the
// human label is `description`. Swapping them sends a model that does not exist
// (trap #6). The id is used verbatim — no lowercasing, no alias table, no
// provider prefix — because the fallback index is a plain map keyed by the same
// string (`raccoon-adapter.ts:186`).
//
// ⚠️ The price multiplier goes into the model's NAME (and the display name),
// never into the description: the composer's model switcher renders only the
// name, and omitting `x1` makes a 1x model indistinguishable from a model whose
// multiplier failed to load (trap #8).

// numberFormatDecimals is the reference's `value.toFixed(4)`
// (`raccoon.ts:219-223`).
const numberFormatDecimals = 4

// catalogueEntry is one model, from the bundled table or a live response.
type catalogueEntry struct {
	// ID is the model id sent upstream, taken from the wire `name`.
	ID string
	// Description is the human label (`description`, falling back to the id).
	Description string
	// ContextWindow is `params.context_window`; 0 means unknown and is never
	// replaced by a guess.
	ContextWindow int64
	// MaxTokens is `params.max_tokens`; 0 means unknown, and a non-positive
	// value must NEVER be advertised — DSH aborts the whole turn with
	// INVALID_MODEL_MAX_TOKENS (trap #9).
	MaxTokens int64
	// SupportsImage is true when `tags` names a vision capability.
	SupportsImage bool
	// EffectiveMultiplier is `billing_effective_multiplier`; nil when absent.
	//
	// A POINTER, not a float: the zero value of a float is a legitimate
	// multiplier (0 = free), so a plain float cannot distinguish "costs nothing"
	// from "the server did not say" — and the difference is exactly what the
	// `· 免费` suffix and a missing suffix mean (`raccoon.ts:219-262`).
	EffectiveMultiplier *float64
	// BaseMultiplier is `billing_multiplier`; nil when absent.
	BaseMultiplier *float64
	// Status is `billing_status`, restricted to `discount`/`limited_free`.
	Status string
	// StatusNote is `billing_status_note`.
	StatusNote string
	// Remote marks an entry that came from the live endpoint.
	Remote bool
}

// displayName renders the name shown in model pickers: the label plus its price
// (`raccoonDisplayName`, `raccoon.ts:219-262`).
func (m catalogueEntry) displayName() string {
	base := strings.TrimSpace(m.Description)
	if base == "" {
		base = m.ID
	}
	if m.EffectiveMultiplier == nil {
		// No suffix at all, rather than a dangling separator.
		return base
	}
	effective := *m.EffectiveMultiplier
	if math.IsNaN(effective) || math.IsInf(effective, 0) || effective < 0 {
		return base
	}
	if effective == 0 {
		return base + " · 免费"
	}
	shown := formatMultiplier(effective)
	if m.BaseMultiplier != nil && !math.IsNaN(*m.BaseMultiplier) && !math.IsInf(*m.BaseMultiplier, 0) &&
		*m.BaseMultiplier > 0 && *m.BaseMultiplier > effective {
		return base + " · x" + formatMultiplier(*m.BaseMultiplier) + "→x" + shown
	}
	// `x1` is shown on purpose: it is the difference between "this really costs
	// 1x" and "we failed to read the multiplier".
	return base + " · x" + shown
}

// formatMultiplier is the reference's `String(Number(value.toFixed(4)))`: at most
// four decimals, trailing zeros removed.
func formatMultiplier(value float64) string {
	text := strconv.FormatFloat(value, 'f', numberFormatDecimals, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimSuffix(text, ".")
	if text == "" || text == "-" {
		return "0"
	}
	return text
}

// numberRef wraps a float for the multiplier fields.
func numberRef(value float64) *float64 { return &value }

// info converts the entry into host-facing model metadata.
func (m catalogueEntry) info(now time.Time) pluginapi.ModelInfo {
	modalities := []string{"text"}
	if m.SupportsImage {
		modalities = append(modalities, "image")
	}
	maxTokens := m.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxOutputTokens
	}
	display := m.displayName()
	description := strings.TrimSpace(m.Description)
	if description == "" {
		description = m.ID
	}
	return pluginapi.ModelInfo{
		ID:          m.ID,
		Object:      "model",
		Created:     now.Unix(),
		OwnedBy:     ProviderKey,
		Type:        "chat",
		DisplayName: display,
		// The provider-native name carries the price too: the reference states
		// the switcher renders `name` and that a missing multiplier is read as
		// "no billing information" (`raccoon.ts:228-241`).
		Name:                       display,
		Description:                description,
		ContextLength:              m.ContextWindow,
		InputTokenLimit:            m.ContextWindow,
		MaxCompletionTokens:        maxTokens,
		OutputTokenLimit:           maxTokens,
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   modalities,
		SupportedOutputModalities:  []string{"text"},
	}
}

// fallbackCatalogue is the bundled table (`raccoon-product.ts:99-144`), a
// 2026-09-26 snapshot of `GET /model_catalog` with `visible:true`.
//
// Order is the remote order and is deliberately NOT re-sorted
// (`raccoon-product.ts:81`). The multipliers are the measured ones
// (`raccoon-product.ts:86-93`), which is why two models render as `· 免费`
// (0.5 → 0) and one as `x0.2→x0.1`.
var fallbackCatalogue = []catalogueEntry{
	{ID: "sn-sensenova-6-8-flash", Description: "SenseNova-6.8-Flash", ContextWindow: 256_000, MaxTokens: 63_999, SupportsImage: true, EffectiveMultiplier: numberRef(0), BaseMultiplier: numberRef(0.5)},
	{ID: "sn-sensenova-6-8-flash-lite", Description: "SenseNova-6.8-Flash-Lite", ContextWindow: 256_000, MaxTokens: 63_999, SupportsImage: true, EffectiveMultiplier: numberRef(0), BaseMultiplier: numberRef(0.5)},
	{ID: "sn-glm-5-3", Description: "GLM-5-3", ContextWindow: 1_000_000, MaxTokens: 100_000, SupportsImage: true, EffectiveMultiplier: numberRef(0.75), BaseMultiplier: numberRef(0.75)},
	{ID: "sn-kimi-k3", Description: "Kimi-K3", ContextWindow: 1_000_000, MaxTokens: 100_000, SupportsImage: true, EffectiveMultiplier: numberRef(1), BaseMultiplier: numberRef(1)},
	{ID: "sn-glm-5-3-flash", Description: "GLM-5-3-Flash", ContextWindow: 1_000_000, MaxTokens: 100_000, EffectiveMultiplier: numberRef(0.1), BaseMultiplier: numberRef(0.2)},
	{ID: "sn-deepseek-v4-1-flash", Description: "DeepSeek-V4.1-Flash", ContextWindow: 1_000_000, MaxTokens: 100_000, EffectiveMultiplier: numberRef(0.25), BaseMultiplier: numberRef(0.25)},
}

// fallbackModels renders the bundled table.
func fallbackModels() []catalogueEntry {
	out := make([]catalogueEntry, len(fallbackCatalogue))
	copy(out, fallbackCatalogue)
	return out
}

// staticModelInfos renders the bundled catalogue. It never touches the network.
func staticModelInfos() []pluginapi.ModelInfo {
	now := time.Now()
	entries := fallbackModels()
	infos := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, entry.info(now))
	}
	return infos
}

// parseCatalogue parses a `GET /model_catalog` body
// (`parseRaccoonModelCatalog`, `raccoon-auth.ts:532-567`).
//
// Every failure — a non-object root, a non-zero code, a missing `data`, no
// `chat` category — yields an EMPTY slice, and the caller falls back to the
// bundled table. A catalogue failure must never become an error (trap #10).
func parseCatalogue(body []byte) []catalogueEntry {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &root); errUnmarshal != nil {
		return nil
	}
	if code, present := numericField(root, "code"); !present || code != codeOK {
		return nil
	}
	data, okData := root["data"].(map[string]any)
	if !okData {
		return nil
	}
	categories, okCategories := data["categories"].([]any)
	if !okCategories {
		return nil
	}
	// The FIRST `chat` category wins; there may be several
	// (`raccoon-auth.ts:547-552`).
	var chat map[string]any
	for _, candidate := range categories {
		object, okObject := candidate.(map[string]any)
		if !okObject {
			continue
		}
		if strings.EqualFold(stringField(object, "type"), "chat") {
			chat = object
			break
		}
	}
	if chat == nil {
		return nil
	}
	models, okModels := chat["models"].([]any)
	if !okModels {
		return nil
	}

	entries := make([]catalogueEntry, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, candidate := range models {
		object, okObject := candidate.(map[string]any)
		if !okObject {
			continue
		}
		entry, ok := normaliseCatalogueEntry(object)
		if !ok {
			continue
		}
		// Dedupe by id, FIRST occurrence wins (`raccoon-auth.ts:558-565`).
		if _, duplicate := seen[entry.ID]; duplicate {
			continue
		}
		seen[entry.ID] = struct{}{}
		entries = append(entries, entry)
	}
	return entries
}

// normaliseCatalogueEntry applies the per-model field mapping
// (`raccoon-auth.ts:118-164`).
func normaliseCatalogueEntry(object map[string]any) (catalogueEntry, bool) {
	// ⚠️ `name` is the MODEL ID, not a display name.
	id := strings.TrimSpace(stringField(object, "name"))
	if id == "" {
		return catalogueEntry{}, false
	}
	// `visible === false` drops the entry; ABSENT means visible
	// (`raccoon-auth.ts:122-123`).
	if visible, present := object["visible"]; present {
		if flag, isBool := visible.(bool); isBool && !flag {
			return catalogueEntry{}, false
		}
	}
	entry := catalogueEntry{ID: id, Remote: true}
	entry.Description = stringField(object, "description")
	if entry.Description == "" {
		entry.Description = id
	}

	params, _ := object["params"].(map[string]any)
	// `params.context_window` wins, then a top-level `context_window`.
	if value, ok := intField(params, "context_window"); ok {
		entry.ContextWindow = value
	} else if value, ok := intField(object, "context_window"); ok {
		entry.ContextWindow = value
	}
	// ⚠️ A non-positive or fractional `max_tokens` is DROPPED, not passed
	// through (trap #9).
	if value, ok := intField(params, "max_tokens"); ok {
		entry.MaxTokens = value
	}
	if value, ok := intField(object, "max_tokens"); ok && entry.MaxTokens == 0 {
		entry.MaxTokens = value
	}

	for _, tag := range stringSlice(object["tags"]) {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "vision", "image", "image-understanding":
			entry.SupportsImage = true
		}
	}

	if value, ok := numericField(object, "billing_effective_multiplier"); ok {
		entry.EffectiveMultiplier = numberRef(value)
	}
	if value, ok := numericField(object, "billing_multiplier"); ok {
		entry.BaseMultiplier = numberRef(value)
	}
	entry.Status = "normal"
	switch strings.ToLower(stringField(object, "billing_status")) {
	case "discount", "limited_free":
		entry.Status = strings.ToLower(stringField(object, "billing_status"))
	}
	entry.StatusNote = stringField(object, "billing_status_note")
	return entry, true
}

// stringSlice reads a JSON array of strings, skipping non-strings.
func stringSlice(value any) []string {
	items, okItems := value.([]any)
	if !okItems {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, okText := item.(string); okText {
			out = append(out, text)
		}
	}
	return out
}

// discoveredCatalogue caches the live catalogue result.
var discoveredCatalogue struct {
	mu        sync.Mutex
	models    []catalogueEntry
	fetchedAt time.Time
}

// cachedModels returns the cached catalogue when it is still fresh.
func cachedModels(ttl time.Duration) []catalogueEntry {
	discoveredCatalogue.mu.Lock()
	defer discoveredCatalogue.mu.Unlock()
	if len(discoveredCatalogue.models) == 0 || ttl <= 0 {
		return nil
	}
	if time.Since(discoveredCatalogue.fetchedAt) > ttl {
		return nil
	}
	out := make([]catalogueEntry, len(discoveredCatalogue.models))
	copy(out, discoveredCatalogue.models)
	return out
}

// putCachedModels stores a discovered catalogue.
func putCachedModels(models []catalogueEntry) {
	discoveredCatalogue.mu.Lock()
	discoveredCatalogue.models = append([]catalogueEntry(nil), models...)
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

// discoverModels performs the live catalogue call.
//
// ⚠️ The timeout is 20 s here, not 60 (trap #31), and every failure returns an
// EMPTY slice instead of an error: the adapter then silently uses the bundled
// table (`raccoon-auth.ts:380-410`).
func discoverModels(h *abiboot.Host, credential *Credential, cfg Config) []catalogueEntry {
	if credential == nil {
		return nil
	}
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.catalogueTimeout())*time.Millisecond,
		http.MethodGet, APIBase+ModelCatalogPath, catalogueHeaders(credential), nil)
	if errDo != nil {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	return parseCatalogue(response.Body)
}

// catalogueForAuth resolves the catalogue for one credential, cached.
func catalogueForAuth(h *abiboot.Host, credential *Credential, cfg Config) []catalogueEntry {
	if cached := cachedModels(time.Duration(cfg.modelCacheTTL()) * time.Millisecond); len(cached) > 0 {
		return cached
	}
	if discovered := discoverModels(h, credential, cfg); len(discovered) > 0 {
		putCachedModels(discovered)
		return discovered
	}
	return fallbackModels()
}

// activeCatalogue resolves the catalogue the provider offers: the bundled table,
// or the live one when discovery is enabled and a credential is available. It is
// the single place pages and JSON views agree on.
func activeCatalogue(h *abiboot.Host, credential *Credential, cfg Config) []catalogueEntry {
	if !cfg.DiscoverModels || credential == nil {
		return fallbackModels()
	}
	return catalogueForAuth(h, credential, cfg)
}

// handleModelRegister reports the static catalogue. It is used when no account is
// bound yet, so it must not touch the network.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelRegistrationResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// handleModelStatic reports the catalogue only while an account exists.
//
// ⚠️ This mirrors `listModels()`: with no credential logged in it returns an
// EMPTY list, which hides the provider group instead of advertising six models
// that every request would reject (`raccoon-adapter.ts:163-182`). It must not
// throw — a throw adds a provider failure entry to the catalogue UI.
//
// `model.register` keeps publishing the bundled table: that is the
// development-time metadata used for aliases, and it is not what a client lists.
func handleModelStatic(h *abiboot.Host, _ json.RawMessage) (any, error) {
	if !hasAccount(h) {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: []pluginapi.ModelInfo{}}, nil
	}
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// hasAccount reports whether this provider has at least one credential.
func hasAccount(h *abiboot.Host) bool {
	if h == nil {
		return false
	}
	entries, errList := h.ListAuth()
	if errList != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Provider == ProviderKey || entry.Type == ProviderKey {
			return true
		}
	}
	return false
}

// handleModelForAuth reports the catalogue for one bound account.
//
// A credential that cannot be parsed, a failed fetch and an empty answer all
// yield the bundled table rather than an error (trap #10): a throw would surface
// as a provider failure in the catalogue UI instead of a silent fallback, and
// hiding the provider would break routing.
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
	if len(entries) == 0 {
		entries = fallbackModels()
	}
	now := time.Now()
	infos := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, entry.info(now))
	}
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: infos}, nil
}
