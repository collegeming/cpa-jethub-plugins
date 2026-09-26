package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Model catalogue, ported from `src/cline-models.ts`.
//
// Two endpoints are fetched INDEPENDENTLY and merged; either may fail without
// invalidating the other (`cline-models.ts:246-255`):
//
//	GET {APIBase}/api/v1/ai/cline/recommended-models   anonymous, curated
//	GET {APIBase}/api/v1/models                        needs the credential
//
// Only the curated endpoint publishes the FREE set, and free status is
// server-side marketing state that is re-fetched every time
// (`README.md:1269-1270`) — nothing here hardcodes a free id.

// maxCataloguedModelIDs bounds how many ids of the authenticated listing are
// published. The endpoint returns ~460 ids (`cline-product.ts:127-129`); they
// are real models, so they are all published, and the constant only guards
// against a pathological answer.
const maxCataloguedModelIDs = 4096

// catalogueEntry is one `{id, name?, description?}` row of the curated endpoint.
type catalogueEntry struct {
	ID          string
	Name        string
	Description string
}

// catalogue is the parsed `recommended-models` answer.
type catalogue struct {
	// Free is the authoritative free list (`cline-models.ts:85-94`).
	Free []catalogueEntry
	// Recommended is the curated non-free list.
	Recommended []catalogueEntry
	// ClinePass is a SUBSCRIPTION namespace, explicitly not free
	// (`cline-models.ts:151-153`).
	ClinePass []catalogueEntry
}

// modelCache memoises a fetched catalogue for a bounded time. The free list is
// volatile marketing state, so the TTL is short by default and 0 disables the
// cache entirely.
type modelCache struct {
	mu        sync.Mutex
	models    []pluginapi.ModelInfo
	fetchedAt time.Time
}

var discoveredModels modelCache

func (c *modelCache) get(ttl time.Duration) []pluginapi.ModelInfo {
	if ttl <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.models) == 0 || time.Since(c.fetchedAt) > ttl {
		return nil
	}
	return c.models
}

func (c *modelCache) put(models []pluginapi.ModelInfo) {
	if len(models) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = models
	c.fetchedAt = time.Now()
}

// peek returns the cached listing and its fetch time regardless of the TTL, so
// the management page can report what was actually fetched.
func (c *modelCache) peek() ([]pluginapi.ModelInfo, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models, c.fetchedAt
}

func (c *modelCache) reset() {
	c.mu.Lock()
	c.models = nil
	c.fetchedAt = time.Time{}
	c.mu.Unlock()
}

// catalogueFetch is the combined outcome of the two listing calls. A failure is
// recorded as a note, because a broken catalogue must degrade the listing rather
// than fail model registration.
type catalogueFetch struct {
	catalogue *catalogue
	remoteIDs []string
	notes     []string
}

// parseCatalogue reads the curated endpoint (`cline-models.ts:131-164`). Entries
// without a non-empty string `id` are discarded.
func parseCatalogue(body []byte) *catalogue {
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		return &catalogue{}
	}
	return &catalogue{
		Free:        parseCatalogueList(decoded["free"]),
		Recommended: parseCatalogueList(decoded["recommended"]),
		ClinePass:   parseCatalogueList(decoded["clinePass"]),
	}
}

// parseCatalogueList reads one array of the curated answer.
func parseCatalogueList(value any) []catalogueEntry {
	items, okItems := value.([]any)
	if !okItems {
		return nil
	}
	out := make([]catalogueEntry, 0, len(items))
	for _, item := range items {
		record, okRecord := item.(map[string]any)
		if !okRecord {
			continue
		}
		id := readStringField(record, "id")
		if id == "" {
			continue
		}
		out = append(out, catalogueEntry{
			ID:          id,
			Name:        readStringField(record, "name"),
			Description: readStringField(record, "description"),
		})
	}
	return out
}

// parseRemoteModelIDs reads `data[].id` from the authenticated listing
// (`cline-models.ts:167-178`). The entries carry `{id, object, created,
// owned_by}`; nothing else is read, so nothing else is claimed.
func parseRemoteModelIDs(body []byte) []string {
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		return nil
	}
	out := make([]string, 0, len(decoded.Data))
	for _, entry := range decoded.Data {
		if id := strings.TrimSpace(entry.ID); id != "" {
			out = append(out, id)
		}
		if len(out) >= maxCataloguedModelIDs {
			break
		}
	}
	return out
}

// catalogueHeaders is the anonymous request header set: JSON accept plus the
// four client headers, and no Authorization (`cline-models.ts:263-279`).
func catalogueHeaders() http.Header {
	header := clientHeaders()
	header.Set("Accept", "application/json")
	return header
}

// fetchCatalogue runs both listing calls through the host transport.
//
// A nil credential skips the authenticated call entirely — the source records
// that the request is not even issued in that case (`cline-models.ts:282-284`).
func fetchCatalogue(d doer, credential *Credential) catalogueFetch {
	fetched := catalogueFetch{}
	response, errRecommended := d(http.MethodGet, APIBase+RecommendedModelsPath, catalogueHeaders(), nil)
	switch {
	case errRecommended != nil:
		fetched.notes = append(fetched.notes, "推荐目录拉取失败："+errRecommended.Error())
	case response.StatusCode < 200 || response.StatusCode >= 300:
		fetched.notes = append(fetched.notes,
			"推荐目录返回 HTTP "+itoaInt(response.StatusCode)+"："+truncate(string(response.Body), 200))
	default:
		fetched.catalogue = parseCatalogue(response.Body)
	}

	if credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return fetched
	}
	response, errModels := d(http.MethodGet, APIBase+ModelsPath, clineHeaders(credential), nil)
	switch {
	case errModels != nil:
		fetched.notes = append(fetched.notes, "模型清单拉取失败："+errModels.Error())
	case response.StatusCode < 200 || response.StatusCode >= 300:
		fetched.notes = append(fetched.notes,
			"模型清单返回 HTTP "+itoaInt(response.StatusCode)+"："+truncate(string(response.Body), 200))
	default:
		fetched.remoteIDs = parseRemoteModelIDs(response.Body)
	}
	return fetched
}

// isFreeModel is the union rule of `cline-models.ts:85-94`.
//
// Free and paid models are DIFFERENT ids (`cline-free/deepseek-v4.1-flash` is
// not `deepseek/deepseek-v4.1-flash`), so no fuzzy or name-based matching is
// allowed (`cline-product.ts:120-125`).
func isFreeModel(id string, remoteFree map[string]struct{}) bool {
	if _, okFree := remoteFree[id]; okFree {
		return true
	}
	if strings.HasSuffix(id, ":free") {
		return true
	}
	if strings.HasPrefix(id, "cline-free/") {
		return true
	}
	fallback, okFallback := fallbackModelFor(id)
	return okFallback && fallback.IsFree
}

// mergeCatalogue applies the four-step merge order and metadata precedence of
// `cline-models.ts:192-235`.
//
//  1. remote free ids (order preserved)
//  2. static fallback table
//  3. recommended + clinePass entries
//  4. every remaining /api/v1/models id, deliberately last (hundreds)
//
// A duplicate id keeps the position of its FIRST insertion while gaining the
// metadata any later step knows about, which is what makes
// `cline-free/deepseek-v4.1-flash` come out as DeepSeek V4.1 Flash rather than as
// a derived name.
func mergeCatalogue(fetched *catalogue, remoteIDs []string) []pluginapi.ModelInfo {
	if fetched == nil {
		fetched = &catalogue{}
	}
	remoteFree := make(map[string]struct{}, len(fetched.Free))
	for _, entry := range fetched.Free {
		remoteFree[entry.ID] = struct{}{}
	}

	type record struct {
		entry        catalogueEntry
		subscription bool
	}
	order := make([]string, 0, len(fetched.Free)+len(clineFallbackModels)+len(remoteIDs))
	records := map[string]*record{}
	add := func(entry catalogueEntry, subscription bool) {
		if strings.TrimSpace(entry.ID) == "" {
			return
		}
		existing, okExisting := records[entry.ID]
		if !okExisting {
			records[entry.ID] = &record{entry: entry, subscription: subscription}
			order = append(order, entry.ID)
			return
		}
		// A later step only fills in what the first insertion did not know: the
		// FIRST insertion keeps the position, and no step overwrites a value an
		// earlier one already provided.
		if existing.entry.Name == "" {
			existing.entry.Name = entry.Name
		}
		if existing.entry.Description == "" {
			existing.entry.Description = entry.Description
		}
		existing.subscription = existing.subscription || subscription
	}

	for _, entry := range fetched.Free {
		add(entry, false)
	}
	for _, model := range clineFallbackModels {
		add(catalogueEntry{ID: model.ID, Name: model.Name}, false)
	}
	for _, entry := range fetched.Recommended {
		add(entry, false)
	}
	for _, entry := range fetched.ClinePass {
		add(entry, true)
	}
	for _, id := range remoteIDs {
		add(catalogueEntry{ID: id}, false)
	}

	now := time.Now()
	out := make([]pluginapi.ModelInfo, 0, len(order))
	for _, id := range order {
		row := records[id]
		out = append(out, modelInfoFor(id, row.entry, isFreeModel(id, remoteFree), row.subscription, now))
	}
	return out
}

// modelInfoFor builds the host-facing descriptor for one catalogue row.
//
// Context window, output limit and image support come ONLY from the static
// table; an unknown model gets none of them rather than an invented value
// (`cline-models.ts:203-217`, spec risk 11).
func modelInfoFor(id string, entry catalogueEntry, free, subscription bool, now time.Time) pluginapi.ModelInfo {
	fallback, hasFallback := fallbackModelFor(id)

	name := strings.TrimSpace(entry.Name)
	if hasFallback && strings.TrimSpace(fallback.Name) != "" {
		name = fallback.Name
	}
	if name == "" {
		name = derivedModelName(id)
	}

	description := strings.TrimSpace(entry.Description)
	if description == "" && subscription {
		description = "Cline Pass 订阅模型（订阅权益，不属于免费名单）"
	}
	if description == "" && hasFallback {
		description = "Cline 静态目录表条目（上下文与输出上限来自静态表，接口不提供这些字段）"
	}

	modalities := []string{"text"}
	if hasFallback && fallback.SupportsImage {
		modalities = append(modalities, "image")
	}

	info := pluginapi.ModelInfo{
		ID:          id,
		Object:      "model",
		Created:     now.Unix(),
		OwnedBy:     ProviderKey,
		Type:        "chat",
		DisplayName: displayModelName(name, free),
		Name:        name,
		Description: description,
		// Levels are declared for EVERY model because no endpoint publishes
		// them (`cline-adapter.ts:341-350`).
		Thinking: &pluginapi.ThinkingSupport{
			Levels: append([]string(nil), reasoningLevels...),
			// `none` is a declared level: it was measured at 0 reasoning
			// characters, so disabling reasoning is supported.
			ZeroAllowed: true,
		},
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   modalities,
		SupportedOutputModalities:  []string{"text"},
	}
	if hasFallback {
		info.ContextLength = fallback.ContextWindow
		info.InputTokenLimit = fallback.ContextWindow
		info.MaxCompletionTokens = fallback.MaxTokens
		info.OutputTokenLimit = fallback.MaxTokens
	}
	return info
}

// displayModelName writes the free marker into the model NAME, not the
// description: the model picker only renders `name` (`cline-models.ts:96-109`).
func displayModelName(name string, free bool) string {
	if free {
		return name + " · 免费"
	}
	return name
}

// derivedModelName is `deriveModelName` (`cline-models.ts:112-118`): strip the
// `owner/` namespace, strip a `:free` suffix, turn '-' into a space and
// title-case every word.
//
// Only '-' (and whitespace) separates words: a version like `v2.6` must stay one
// word, so '.' and '_' are left inside it.
func derivedModelName(id string) string {
	trimmed := strings.TrimSpace(id)
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	trimmed = strings.TrimSuffix(trimmed, ":free")
	words := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == '-' || r == ' ' || r == '\t'
	})
	for index, word := range words {
		words[index] = titleCaseWord(word)
	}
	name := strings.Join(words, " ")
	if name == "" {
		return id
	}
	return name
}

// titleCaseWord upper-cases the first rune of one word, leaving the rest alone
// (the source uses `charAt(0).toUpperCase() + slice(1)`).
func titleCaseWord(word string) string {
	if word == "" {
		return word
	}
	runes := []rune(word)
	return strings.ToUpper(string(runes[0])) + string(runes[1:])
}

// staticModelInfos renders the static fallback table only. It never touches the
// network, which is what `model.register` requires.
func staticModelInfos() []pluginapi.ModelInfo {
	now := time.Now()
	out := make([]pluginapi.ModelInfo, 0, len(clineFallbackModels))
	for _, model := range clineFallbackModels {
		out = append(out, modelInfoFor(model.ID, catalogueEntry{ID: model.ID, Name: model.Name}, model.IsFree, false, now))
	}
	return out
}

// executeModelCatalog resolves the catalogue for one bound account, using the
// cache, the remote catalogue and finally the static table.
//
// Only a fetch that actually reached a remote source is cached: caching the
// static fallback after a transient failure would pin a 5-entry catalogue for
// the whole TTL.
func executeModelCatalog(h *abiboot.Host, credential *Credential, cfg Config) []pluginapi.ModelInfo {
	if !cfg.ModelDiscovery {
		return staticModelInfos()
	}
	ttl := time.Duration(cfg.ModelCacheTTLMS) * time.Millisecond
	if cached := discoveredModels.get(ttl); len(cached) > 0 {
		return cached
	}
	fetched := fetchCatalogue(transportFor(h), credential)
	for _, note := range fetched.notes {
		// A degraded catalogue must be visible: with discovery on but both
		// endpoints failing, the plugin silently publishes five models, and the
		// reason is otherwise only reachable through this log line.
		if h != nil {
			h.Log("warn", "cline model catalogue degraded: "+note, map[string]any{"provider": ProviderKey})
		}
	}
	if !fetched.hasRemoteData() {
		return staticModelInfos()
	}
	models := mergeCatalogue(fetched.catalogue, fetched.remoteIDs)
	if len(models) == 0 {
		return staticModelInfos()
	}
	discoveredModels.put(models)
	return models
}

// hasRemoteData reports whether at least one listing endpoint answered.
func (f catalogueFetch) hasRemoteData() bool {
	if len(f.remoteIDs) > 0 {
		return true
	}
	if f.catalogue == nil {
		return false
	}
	return len(f.catalogue.Free)+len(f.catalogue.Recommended)+len(f.catalogue.ClinePass) > 0
}

// handleModelRegister reports the static fallback catalogue. It must not touch
// the network: the host calls it before any credential exists.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelRegistrationResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// handleModelStatic is the model.static variant of the same catalogue.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// handleModelForAuth reports the catalogue for one bound account.
//
// A credential that cannot be parsed still yields the static table rather than
// an error: the listing is advisory, and hiding it would break routing for an
// account whose stored body is being repaired.
func handleModelForAuth(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := settings()
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
	}
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: executeModelCatalog(h, credential, cfg)}, nil
}
