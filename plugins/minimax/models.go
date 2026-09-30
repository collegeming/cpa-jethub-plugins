package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The model catalogue: a remote snapshot plus a bundled fallback.
//
// The remote catalogue is authoritative because it is the live source of model
// metadata, including the remote-only M3.1 preview that the vendor's own client
// uses. This provider's fallback deliberately mirrors all four measured entries
// so a temporary catalogue outage does not make the current model disappear.
//
// ⚠️ A catalogue failure NEVER becomes an error. The reference states the rule
// directly (`minimax-auth.ts:fetchModelsWith`): the catalogue is display
// information, and hiding every model because a listing endpoint hiccuped would
// take the whole provider down with it. It falls back, and it logs.

// discoveredCatalogue caches the remote snapshot so a page load and an
// inference request arriving together cost the vendor one call.
var discoveredCatalogue struct {
	mu        sync.Mutex
	entries   []ModelCatalogEntry
	fetchedAt time.Time
	// lastError records the most recent discovery failure for the status page.
	lastError string
}

// resetDiscoveredModels drops the cache; used by tests and by reconfiguration.
func resetDiscoveredModels() {
	discoveredCatalogue.mu.Lock()
	discoveredCatalogue.entries = nil
	discoveredCatalogue.fetchedAt = time.Time{}
	discoveredCatalogue.lastError = ""
	discoveredCatalogue.mu.Unlock()
}

// cachedModels returns the cached snapshot when it is still inside its TTL.
func cachedModels(ttl time.Duration) []ModelCatalogEntry {
	discoveredCatalogue.mu.Lock()
	defer discoveredCatalogue.mu.Unlock()
	if len(discoveredCatalogue.entries) == 0 {
		return nil
	}
	if ttl > 0 && nowTime().Sub(discoveredCatalogue.fetchedAt) > ttl {
		return nil
	}
	out := make([]ModelCatalogEntry, len(discoveredCatalogue.entries))
	copy(out, discoveredCatalogue.entries)
	return out
}

// putCachedModels stores a discovered snapshot.
func putCachedModels(entries []ModelCatalogEntry) {
	discoveredCatalogue.mu.Lock()
	discoveredCatalogue.entries = entries
	discoveredCatalogue.fetchedAt = nowTime()
	discoveredCatalogue.lastError = ""
	discoveredCatalogue.mu.Unlock()
}

// noteCatalogueError records a discovery failure for the status page.
func noteCatalogueError(message string) {
	discoveredCatalogue.mu.Lock()
	discoveredCatalogue.lastError = message
	discoveredCatalogue.mu.Unlock()
}

// catalogueLastError reports the most recent discovery failure.
func catalogueLastError() string {
	discoveredCatalogue.mu.Lock()
	defer discoveredCatalogue.mu.Unlock()
	return discoveredCatalogue.lastError
}

// discoverModels performs the live catalogue call.
//
// The URL carries `?region=cn&buildEnv=prod`; both are required by the endpoint
// and neither has a default. Every failure yields nil so the caller falls back
// to the bundled table.
func discoverModels(h *abiboot.Host, credential *Credential, cfg Config) []ModelCatalogEntry {
	if credential == nil || h == nil {
		return nil
	}
	rawURL := APIHost + ModelsPath + "?region=" + Region + "&buildEnv=" + BuildEnv
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.catalogueTimeout())*time.Millisecond,
		http.MethodGet, rawURL, businessHeaders(credential), nil)
	if errDo != nil {
		noteCatalogueError("远端模型目录请求失败：" + errDo.Error())
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		noteCatalogueError("远端模型目录返回 HTTP " + itoaInt(response.StatusCode))
		return nil
	}
	entries := parseModelsPayload(response.Body)
	if len(entries) == 0 {
		// A 2xx that parses to nothing is still a failure worth recording: it
		// is indistinguishable from a broken catalogue at the UI, and without
		// the note the operator would only ever see "the model list is wrong".
		noteCatalogueError("远端模型目录解析出 0 条，已回退兜底表")
		return nil
	}
	return entries
}

// catalogueForAuth resolves the catalogue for one credential, cached.
func catalogueForAuth(h *abiboot.Host, credential *Credential, cfg Config) []ModelCatalogEntry {
	if !cfg.DiscoverModels {
		return fallbackModels()
	}
	if cached := cachedModels(time.Duration(cfg.modelCacheTTL()) * time.Millisecond); len(cached) > 0 {
		return cached
	}
	if discovered := discoverModels(h, credential, cfg); len(discovered) > 0 {
		putCachedModels(discovered)
		return discovered
	}
	return fallbackModels()
}

// parseModelsPayload decodes the remote catalogue response.
//
// The shape, measured 2026-09-28:
//
//	{"providers":[
//	   {"providerId":"minimax",
//	    "config":{
//	      "models":{"MiniMax-M3.1-Flash-Preview":{...,"name":"M3.1-Flash-Preview"},
//	                "MiniMax-M3":{...}},
//	      "model_order":["MiniMax-M3.1-Flash-Preview","MiniMax-M3", …]}}]}
//
// Three properties matter and each is a place a naive reader goes wrong:
//
//   - `config.models` is an OBJECT KEYED BY THE LONG MODEL ID, and each value
//     carries the SHORT name. Reading only the values loses the id that the
//     inference endpoint expects; reading only the keys loses the display name
//     the official IDE shows. Both are kept.
//   - `model_order` is the vendor's own display order and is applied when
//     present; without it the object's insertion order would be used, which is
//     not stable across JSON decoders.
//   - the context window is the MAXIMUM of `context_window_options`, not
//     `limit.context`. For `MiniMax-M3.1-Flash-Preview` the two disagree
//     (`limit.context` is 512000, the options are [512000, 1000000]) and the
//     official client offers the max, so 512K would make the host compact far
//     earlier than the vendor's own client does.
func parseModelsPayload(payload []byte) []ModelCatalogEntry {
	var root map[string]any
	if errUnmarshal := json.Unmarshal(payload, &root); errUnmarshal != nil {
		return nil
	}
	providers, okProviders := root["providers"].([]any)
	if !okProviders {
		return nil
	}
	var config map[string]any
	for _, candidate := range providers {
		provider, okProvider := candidate.(map[string]any)
		if !okProvider {
			continue
		}
		if stringField(provider, "providerId") != ProviderKey {
			continue
		}
		if nested, okNested := provider["config"].(map[string]any); okNested {
			config = nested
		}
		break
	}
	if config == nil {
		return nil
	}
	models, okModels := config["models"].(map[string]any)
	if !okModels || len(models) == 0 {
		return nil
	}

	entries := make([]ModelCatalogEntry, 0, len(models))
	for id, raw := range models {
		entry, okEntry := normaliseModel(raw)
		if !okEntry {
			continue
		}
		// ⚠️ The object KEY is the long model id; the entry's own `name` is the
		// short display name. They are different fields and both are kept.
		entry.ID = strings.TrimSpace(id)
		if entry.ID == "" {
			continue
		}
		if entry.Name == "" {
			entry.Name = entry.ID
		}
		entries = append(entries, entry)
	}
	sortByModelOrder(entries, config["model_order"])
	return entries
}

// sortByModelOrder applies the vendor's `model_order` when it is present.
//
// A model the order does not mention sorts last, keeping the vendor's list
// intact ahead of anything newly added. The sort is stable so two unlisted
// models keep their previous relative order.
func sortByModelOrder(entries []ModelCatalogEntry, rawOrder any) {
	order, okOrder := rawOrder.([]any)
	if !okOrder || len(order) == 0 {
		return
	}
	rank := make(map[string]int, len(order))
	for index, candidate := range order {
		if id, okID := candidate.(string); okID {
			rank[strings.TrimSpace(id)] = index
		}
	}
	sort.SliceStable(entries, func(left, right int) bool {
		leftRank, okLeft := rank[entries[left].ID]
		rightRank, okRight := rank[entries[right].ID]
		switch {
		case okLeft && okRight:
			return leftRank < rightRank
		case okLeft:
			return true
		case okRight:
			return false
		default:
			return false
		}
	})
}

// normaliseModel maps one remote model object onto ModelCatalogEntry.
//
// Only the snake_case field names the endpoint actually sends are read
// (`normalizeMinimaxModel`, `minimax.ts`). The reference records that the
// vendor's own client ALSO accepts camelCase aliases and enforces a 2^31-1
// ceiling; neither is implemented here, and the consequence is stated plainly:
// if the endpoint ever starts sending camelCase, the window degrades to the
// `limit.context` fallback and the effort list disappears. Nothing is invented
// to paper over that.
func normaliseModel(raw any) (ModelCatalogEntry, bool) {
	record, okRecord := raw.(map[string]any)
	if !okRecord {
		return ModelCatalogEntry{}, false
	}
	entry := ModelCatalogEntry{
		Name: strings.TrimSpace(stringField(record, "name")),
		// The window: the largest published tier wins. `context_window_options`
		// is the tier table; `limit.context` is only the fallback.
		ContextWindow: maxPositiveInt(record["context_window_options"]),
	}
	if entry.ContextWindow == 0 {
		if limit, okLimit := record["limit"].(map[string]any); okLimit {
			entry.ContextWindow = int64(positiveInt(limit["context"]))
		}
	}
	if limit, okLimit := record["limit"].(map[string]any); okLimit {
		entry.MaxTokens = int64(positiveInt(limit["output"]))
	}
	if modalities, okModalities := record["modalities"].(map[string]any); okModalities {
		entry.SupportsImage = containsString(stringSlice(modalities["input"]), blockTypeImage)
	}
	entry.EffortOptions = stringSlice(record["effort_options"])
	entry.DefaultEffort = strings.TrimSpace(stringField(record, "default_effort"))
	if entry.DefaultEffort != "" && !containsString(entry.EffortOptions, entry.DefaultEffort) {
		// A default outside the published ladder would send a level the model
		// does not accept; the reference drops it for the same reason.
		entry.DefaultEffort = ""
	}
	if thinking, okThinking := record["thinking_config"].(map[string]any); okThinking {
		entry.ThinkingMode = strings.TrimSpace(stringField(thinking, "mode"))
	}
	return entry, true
}

// maxPositiveInt returns the largest positive integer in a JSON array, or 0.
func maxPositiveInt(value any) int64 {
	items, okItems := value.([]any)
	if !okItems {
		return 0
	}
	var best int64
	for _, item := range items {
		if candidate := int64(positiveInt(item)); candidate > best {
			best = candidate
		}
	}
	return best
}

// stringSlice reads an array of non-empty strings, de-duplicated in order.
func stringSlice(value any) []string {
	items, okItems := value.([]any)
	if !okItems {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, okText := item.(string)
		if !okText {
			continue
		}
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || containsString(out, trimmed) {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// modelInfoFor builds the host-facing descriptor for one catalogue entry.
//
// ⚠️ Two host limitations shape this, both shared with the sibling plugins:
//
//   - the host has ONE `ContextLength`, so the maximum context tier is what
//     gets published (see `parseModelsPayload` for why that is the honest
//     number);
//   - `ThinkingSupport` has no default-effort field, so `DefaultEffort` cannot
//     be forwarded. Only the level list and the "off is allowed" flag travel.
func modelInfoFor(entry ModelCatalogEntry, now time.Time) pluginapi.ModelInfo {
	modalities := []string{"text"}
	if entry.SupportsImage {
		modalities = append(modalities, blockTypeImage)
	}
	maxTokens := entry.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxOutputTokens
	}
	info := pluginapi.ModelInfo{
		ID:                         entry.ID,
		Object:                     "model",
		Created:                    now.Unix(),
		OwnedBy:                    ProviderKey,
		Type:                       "chat",
		DisplayName:                entry.Name,
		Name:                       entry.Name,
		Description:                "MiniMax Code " + entry.Name,
		ContextLength:              entry.ContextWindow,
		InputTokenLimit:            entry.ContextWindow,
		MaxCompletionTokens:        maxTokens,
		OutputTokenLimit:           maxTokens,
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   modalities,
		SupportedOutputModalities:  []string{"text"},
	}
	// The thinking controls come straight off the catalogue. A model that
	// publishes neither tiers nor a switchable mode gets NO block at all, which
	// is what the host renders as "no reasoning levels for this model" — the
	// honest answer for the two `forced_on` M2.7 entries.
	if efforts := reasoningEffortsFor(entry); len(efforts) > 0 {
		info.Thinking = &pluginapi.ThinkingSupport{
			Levels: efforts,
			// ⚠️ `ZeroAllowed` is set ONLY for a switchable model. Setting it
			// for M3.1 would advertise a control the server hard-rejects, and
			// for M2.7 one it silently ignores — the user would believe
			// thinking was off while it was not.
			ZeroAllowed: entry.ThinkingMode == thinkingSwitchable,
		}
	}
	return info
}

// defaultMaxOutputTokens is the per-response cap used when the catalogue
// publishes no usable `limit.output`. The observed value for all four models is
// 128000, so the fallback is a conservative fraction of it rather than a
// fabricated per-model number.
const defaultMaxOutputTokens = 65_536

// catalogueEntries resolves the catalogue to publish, preferring the remote
// snapshot and falling back without ever erroring.
func catalogueEntries(h *abiboot.Host, credential *Credential, cfg Config) []ModelCatalogEntry {
	if credential == nil {
		return fallbackModels()
	}
	return catalogueForAuth(h, credential, cfg)
}

// staticCatalogueEntries renders the catalogue without touching the network,
// using the cache when a live snapshot is already available.
func staticCatalogueEntries(cfg Config) []ModelCatalogEntry {
	if !cfg.DiscoverModels {
		return fallbackModels()
	}
	if cached := cachedModels(time.Duration(cfg.modelCacheTTL()) * time.Millisecond); len(cached) > 0 {
		return cached
	}
	return fallbackModels()
}

// modelInfos renders catalogue entries as host descriptors.
func modelInfos(entries []ModelCatalogEntry) []pluginapi.ModelInfo {
	now := nowTime()
	infos := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, modelInfoFor(entry, now))
	}
	return infos
}

// handleModelRegister reports the static catalogue. It must not touch the
// network, so it serves the cache or the fallback table.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	entries := staticCatalogueEntries(settings())
	return pluginapi.ModelRegistrationResponse{
		Provider: ProviderKey,
		Models:   modelInfos(entries),
	}, nil
}

// handleModelStatic is the model.static variant of the same catalogue.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	entries := staticCatalogueEntries(settings())
	return pluginapi.ModelResponse{
		Provider: ProviderKey,
		Models:   modelInfos(entries),
	}, nil
}

// handleModelForAuth reports the catalogue for one bound account.
//
// This is the one place the remote catalogue can be pulled, because it is the
// only model method that arrives with a credential. A credential that cannot be
// parsed still yields the fallback catalogue rather than an error: the listing
// is advisory and hiding it would break routing.
func handleModelForAuth(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := settings()
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: modelInfos(fallbackModels())}, nil
	}
	return pluginapi.ModelResponse{
		Provider: ProviderKey,
		Models:   modelInfos(catalogueEntries(h, credential, cfg)),
	}, nil
}
