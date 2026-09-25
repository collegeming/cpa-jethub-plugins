package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Model catalog, ported from jethub-src/src/lobsterai-adapter.ts:104-303 and
// jethub-src/src/lobsterai-product.ts:529-549.

// thinkingOption is one entry of the remote `thinkingConfig.options[]`. The two
// fields have different meanings and must never be conflated
// (lobsterai-adapter.ts:92-110):
//
//   - level is the PRODUCT-SIDE name (off/minimal/low/medium/high/xhigh/max),
//     used for display and referenced by defaultLevel;
//   - openclawLevel is the WIRE value sent to the server as reasoning_effort
//     (off/minimal/low/medium/high/xhigh — there is no "max"). The server maps
//     level "max" to openclawLevel "xhigh", and sending reasoning_effort:"max"
//     behaves exactly like sending nothing.
type thinkingOption struct {
	Level         string
	OpenclawLevel string
}

// thinkingConfig is the remote thinking配置: the selectable options plus the
// product-side default level.
type thinkingConfig struct {
	Options      []thinkingOption
	DefaultLevel string
}

// validThinkingLevels are the product-side level names.
var validThinkingLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// validOpenclawLevels are the wire-side level names (no "max").
var validOpenclawLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
}

// effortDisplayNames maps a level (either flavour) to its display name.
// Registered for both `max` and `xhigh` because the product side shows "Max"
// while the wire value is "xhigh" (lobsterai-adapter.ts:63-71).
var effortDisplayNames = map[string]string{
	"off": "Off", "minimal": "Minimal", "low": "Low", "medium": "Medium",
	"high": "High", "xhigh": "XHigh", "max": "Max",
}

// parseThinkingConfig parses a remote thinkingConfig. Structure that does not
// match is discarded whole rather than half-parsed (lobsterai-adapter.ts:181-207):
// options must be a non-empty array, every entry needs a valid level and
// openclawLevel, the two "off" flags must agree, duplicates are rejected, and
// defaultLevel must reference an option. A config with only the "off" level
// means "no selectable levels" and is discarded.
func parseThinkingConfig(value any) *thinkingConfig {
	record := asRecord(value)
	if record == nil {
		return nil
	}
	rawOptions, isArray := record["options"].([]any)
	if !isArray || len(rawOptions) == 0 {
		return nil
	}

	options := make([]thinkingOption, 0, len(rawOptions))
	seenLevels := map[string]bool{}
	seenWire := map[string]bool{}
	for _, raw := range rawOptions {
		entry := asRecord(raw)
		if entry == nil {
			return nil
		}
		level, isLevel := entry["level"].(string)
		wire, isWire := entry["openclawLevel"].(string)
		if !isLevel || !isWire || !validThinkingLevels[level] || !validOpenclawLevels[wire] {
			return nil
		}
		if seenLevels[level] || seenWire[wire] {
			return nil
		}
		if (level == "off") != (wire == "off") {
			return nil
		}
		seenLevels[level] = true
		seenWire[wire] = true
		options = append(options, thinkingOption{Level: level, OpenclawLevel: wire})
	}
	if len(options) == 1 && options[0].Level == "off" {
		return nil
	}

	defaultLevel, _ := record["defaultLevel"].(string)
	if !seenLevels[defaultLevel] {
		return nil
	}
	return &thinkingConfig{Options: options, DefaultLevel: defaultLevel}
}

// remoteModel is one entry of GET /api/models/available. Optional fields stay
// nil when the server did not declare them — "the server says this model has no
// image support" and "the server did not say" are different states, and
// inventing false/0 would silently misclassify future models
// (lobsterai-adapter.ts:119-162).
type remoteModel struct {
	ID            string
	Name          string
	ContextWindow *int64
	MaxTokens     *int64
	SupportsImage *bool
	// CostMultiplier is a BARE number (0.05), unlike the Tencent family's
	// string "x0.05". nil means the field was absent; 0 means free — the
	// pricing rule says 0 is "free", not "no multiplier".
	CostMultiplier *float64
	Thinking       *thinkingConfig
	// RequestCapabilities is what the model declares it accepts (for example
	// lobsterai-options-v1).
	RequestCapabilities []string
	Description         string
	SupportsThinking    *bool
}

// fallbackModels is the static catalog used when discovery is disabled or the
// remote listing fails. The order is copied verbatim from the reference table
// (lobsterai-product.ts:529-549) so comparisons with upstream stay meaningful.
// The contextWindow values are bridge-layer estimates (131072), not per-model
// measurements.
var fallbackModels = []remoteModel{
	{ID: "deepseek-v4-flash", Name: "deepseek-v4-flash", ContextWindow: int64Ptr(131072)},
	{ID: "deepseek-v4-pro", Name: "deepseek-v4-pro", ContextWindow: int64Ptr(131072)},
	{ID: "MiniMax-M3", Name: "MiniMax-M3", ContextWindow: int64Ptr(131072)},
	{ID: "MiniMax-M2.7", Name: "MiniMax-M2.7", ContextWindow: int64Ptr(131072)},
	{ID: "qwen3.7-max", Name: "qwen3.7-max", ContextWindow: int64Ptr(131072)},
	{ID: "qwen3.7-plus", Name: "qwen3.7-plus", ContextWindow: int64Ptr(131072)},
	{ID: "qwen3.6-plus", Name: "qwen3.6-plus", ContextWindow: int64Ptr(131072)},
	{ID: "qwen3.5-plus-2026-04-20", Name: "qwen3.5-plus-2026-04-20", ContextWindow: int64Ptr(131072)},
	{ID: "kimi-k2.7-code", Name: "kimi-k2.7-code", ContextWindow: int64Ptr(131072)},
	{ID: "kimi-k2.7-code-highspeed", Name: "kimi-k2.7-code-highspeed", ContextWindow: int64Ptr(131072)},
	{ID: "kimi-k2.6", Name: "kimi-k2.6", ContextWindow: int64Ptr(131072)},
	{ID: "kimi-k2.5", Name: "kimi-k2.5", ContextWindow: int64Ptr(131072)},
	{ID: "doubao-seed-2-1-pro-260628", Name: "doubao-seed-2-1-pro-260628", ContextWindow: int64Ptr(131072)},
	{ID: "doubao-seed-2-1-turbo-260628", Name: "doubao-seed-2-1-turbo-260628", ContextWindow: int64Ptr(131072)},
	{ID: "doubao-seed-2-0-code-preview-260215", Name: "doubao-seed-2-0-code-preview-260215", ContextWindow: int64Ptr(131072)},
	{ID: "glm-5.2", Name: "glm-5.2", ContextWindow: int64Ptr(131072)},
	{ID: "glm-5.1", Name: "glm-5.1", ContextWindow: int64Ptr(131072)},
	{ID: "glm-5v-turbo", Name: "glm-5v-turbo", ContextWindow: int64Ptr(131072)},
	{ID: "glm-5", Name: "glm-5", ContextWindow: int64Ptr(131072)},
}

func int64Ptr(value int64) *int64       { return &value }
func float64Ptr(value float64) *float64 { return &value }

// modelCache memoises a discovered catalog for a bounded time.
type modelCache struct {
	mu        sync.Mutex
	models    []remoteModel
	index     map[string]remoteModel
	fetchedAt time.Time
}

var discoveredModels modelCache

func (c *modelCache) get(ttl time.Duration, now time.Time) ([]remoteModel, map[string]remoteModel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.models) == 0 || now.Sub(c.fetchedAt) > ttl {
		return nil, nil
	}
	return c.models, c.index
}

func (c *modelCache) put(models []remoteModel, now time.Time) {
	if len(models) == 0 {
		return
	}
	index := make(map[string]remoteModel, len(models))
	for _, model := range models {
		index[model.ID] = model
	}
	c.mu.Lock()
	c.models = models
	c.index = index
	c.fetchedAt = now
	c.mu.Unlock()
}

// readModelArray extracts the model array from the response, accepting both
// shapes that appear in the wild (lobsterai-adapter.ts:209-238):
//
//	single layer: {code:0, message:"success", data:[...]}
//	double layer: {code:0, msg:"OK", data:{data:[...]}}
//
// It deliberately does NOT reuse parseEnvelope: that parser requires `data` to
// be an object (so an expired token reported as data:null is a failure), while
// this endpoint's real data is an array. Reusing it would make the whole model
// list silently empty.
func readModelArray(body any) []any {
	record := asRecord(body)
	if record == nil {
		return nil
	}
	code, found := readNumberField(record, "code")
	if !found || int(code) != 0 {
		return nil
	}
	if array, isArray := record["data"].([]any); isArray {
		return array
	}
	if nested := asRecord(record["data"]); nested != nil {
		if array, isArray := nested["data"].([]any); isArray {
			return array
		}
	}
	return nil
}

// parseModels parses the remote catalog, consuming every model parameter the
// server declares.
func parseModels(body any) []remoteModel {
	raw := readModelArray(body)
	models := make([]remoteModel, 0, len(raw))
	for _, item := range raw {
		record := asRecord(item)
		if record == nil {
			continue
		}
		id := readStringField(record, "modelId")
		if id == "" {
			continue
		}
		name := readStringField(record, "modelName")
		if name == "" {
			name = id
		}
		model := remoteModel{ID: id, Name: name}

		if contextWindow, ok := readNumberField(record, "contextWindow"); ok && contextWindow > 0 {
			value := int64(contextWindow)
			model.ContextWindow = &value
		}
		if maxTokens, ok := readNumberField(record, "maxTokens"); ok && maxTokens > 0 {
			value := int64(maxTokens)
			model.MaxTokens = &value
		}
		if supportsImage, ok := record["supportsImage"].(bool); ok {
			value := supportsImage
			model.SupportsImage = &value
		}
		if supportsThinking, ok := record["supportsThinking"].(bool); ok {
			value := supportsThinking
			model.SupportsThinking = &value
		}
		if thinking := parseThinkingConfig(record["thinkingConfig"]); thinking != nil {
			model.Thinking = thinking
		}
		if capabilities := readStringArray(record, "requestCapabilities"); len(capabilities) > 0 {
			model.RequestCapabilities = capabilities
		}
		if description := readStringField(record, "description"); description != "" {
			model.Description = description
		}
		// costMultiplier is a bare number. It is kept when present — including
		// 0, which means "free" (per the pricing rules) and must not be dropped
		// as if the field were absent.
		if multiplier, ok := readNumberField(record, "costMultiplier"); ok {
			value := multiplier
			model.CostMultiplier = &value
		}
		models = append(models, model)
	}
	return models
}

// displayNameFor renders the model selector name. The multiplier goes into the
// display name (not the description) because the model switcher only renders
// names. 0 renders as 免费 rather than x0.
func displayNameFor(model remoteModel) string {
	if model.CostMultiplier == nil {
		return model.Name
	}
	if *model.CostMultiplier == 0 {
		return model.Name + " · 免费"
	}
	return model.Name + " · x" + strconv.FormatFloat(*model.CostMultiplier, 'f', -1, 64)
}

// thinkingLevelsFor returns the wire levels advertised for a model and whether
// disabling reasoning is allowed.
func thinkingLevelsFor(model remoteModel) ([]string, bool) {
	if model.Thinking == nil {
		return nil, false
	}
	levels := make([]string, 0, len(model.Thinking.Options))
	zeroAllowed := false
	for _, option := range model.Thinking.Options {
		levels = append(levels, option.OpenclawLevel)
		if option.OpenclawLevel == "off" {
			zeroAllowed = true
		}
	}
	return levels, zeroAllowed
}

// thinkingDisplayNames renders the product-side level names for the description
// (display name MUST use `level`, not the wire value).
func thinkingDisplayNames(model remoteModel) string {
	if model.Thinking == nil {
		return ""
	}
	names := make([]string, 0, len(model.Thinking.Options))
	for _, option := range model.Thinking.Options {
		name := effortDisplayNames[option.Level]
		if name == "" {
			name = option.Level
		}
		if option.Level == model.Thinking.DefaultLevel {
			name += "(默认)"
		}
		names = append(names, name)
	}
	return strings.Join(names, "/")
}

// mapReasoningEffort converts a caller-supplied reasoning_effort into the wire
// value. Accepting the product-side name is required because the product side
// calls the top level "Max" while the wire value is "xhigh"; an already-wire
// value passes through untouched, and an unknown value is returned unchanged
// only when the model declares no thinking config at all.
func mapReasoningEffort(model remoteModel, requested string) string {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		return ""
	}
	if model.Thinking != nil {
		for _, option := range model.Thinking.Options {
			if strings.EqualFold(option.Level, trimmed) {
				return option.OpenclawLevel
			}
		}
		// An unknown value on a model that declares its levels is not a valid
		// wire value; leave it to the upstream to reject rather than invent one.
		return trimmed
	}
	return trimmed
}

// modelInfoFor renders one catalog entry for the host.
func modelInfoFor(model remoteModel) pluginapi.ModelInfo {
	info := pluginapi.ModelInfo{
		ID:                         model.ID,
		Object:                     "model",
		Created:                    time.Now().Unix(),
		OwnedBy:                    ProviderKey,
		Type:                       "chat",
		DisplayName:                displayNameFor(model),
		Name:                       model.ID,
		Description:                descriptionFor(model),
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedParameters:        []string{"stream", "temperature", "max_tokens", "stop", "tools", "reasoning_effort"},
		SupportedOutputModalities:  []string{"text"},
		SupportedInputModalities:   []string{"text"},
		MaxCompletionTokens:        -1,
	}
	if model.ContextWindow != nil {
		info.ContextLength = *model.ContextWindow
		info.InputTokenLimit = *model.ContextWindow
	}
	if model.MaxTokens != nil {
		info.MaxCompletionTokens = *model.MaxTokens
		info.OutputTokenLimit = *model.MaxTokens
	} else {
		info.MaxCompletionTokens = 0
	}
	// Image support is authoritative from the server; an undeclared capability
	// stays conservatively text-only rather than advertising a modality the
	// request would be rejected for.
	if model.SupportsImage != nil && *model.SupportsImage {
		info.SupportedInputModalities = []string{"text", "image"}
	}
	if levels, zeroAllowed := thinkingLevelsFor(model); len(levels) > 0 {
		info.Thinking = &pluginapi.ThinkingSupport{Levels: levels, ZeroAllowed: zeroAllowed, DynamicAllowed: false}
	}
	return info
}

// descriptionFor composes the description: the remote text plus the
// product-side thinking level names, so the display naming ("Max") is visible
// even though the wire levels are the openclaw values.
func descriptionFor(model remoteModel) string {
	parts := make([]string, 0, 2)
	if model.Description != "" {
		parts = append(parts, model.Description)
	}
	if names := thinkingDisplayNames(model); names != "" {
		parts = append(parts, "思考档位: "+names)
	}
	if model.CostMultiplier != nil && *model.CostMultiplier == 0 {
		parts = append(parts, "免费")
	}
	if len(parts) == 0 {
		return "Youdao LobsterAI " + model.Name
	}
	return strings.Join(parts, " · ")
}

// staticModelInfos renders the fallback catalog without touching the network.
func staticModelInfos() []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(fallbackModels))
	for _, model := range fallbackModels {
		out = append(out, modelInfoFor(model))
	}
	return out
}

// fallbackByID indexes the fallback catalog.
func fallbackByID(id string) (remoteModel, bool) {
	for _, model := range fallbackModels {
		if model.ID == id {
			return model, true
		}
	}
	return remoteModel{}, false
}

// currentCatalog returns the catalog to advertise: the discovery cache when
// fresh, otherwise the static fallback.
func currentCatalog(now time.Time) []remoteModel {
	cfg := settings()
	cached, _ := discoveredModels.get(modelCacheTTL(cfg), now)
	if len(cached) > 0 {
		return cached
	}
	return fallbackModels
}

// modelCacheTTL resolves the configured cache lifetime.
func modelCacheTTL(cfg Config) time.Duration {
	ttl := time.Duration(cfg.ModelCacheTTLMS) * time.Millisecond
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return ttl
}

// modelByID looks a model up in the discovery cache and then the fallback
// catalog. The boolean reports whether the entry came from the remote listing.
func modelByID(id string, now time.Time) (remoteModel, bool, bool) {
	cfg := settings()
	if _, index := discoveredModels.get(modelCacheTTL(cfg), now); index != nil {
		if model, ok := index[id]; ok {
			return model, true, true
		}
	}
	model, ok := fallbackByID(id)
	return model, ok, false
}

// buildModelsQuery renders the keyfrom identity payload as a query string. It
// deliberately carries no refreshToken: the endpoint only takes the keyfrom
// fields, and a token in a query string lands in server access logs
// (lobsterai-adapter.ts:285-303).
func buildModelsQuery(credential *Credential, clientVersion string) string {
	body := keyfromBody(credential, clientVersion)
	params := url.Values{}
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := stringifyValue(body[key])
		if value != "" {
			params.Set(key, value)
		}
	}
	return params.Encode()
}

// buildModelsURL assembles the full model-listing URL.
func buildModelsURL(credential *Credential, clientVersion string) string {
	query := buildModelsQuery(credential, clientVersion)
	if query == "" {
		return APIBase + ModelsPath
	}
	return APIBase + ModelsPath + "?" + query
}

// discoverModels fetches the remote catalog. Any failure returns nil so the
// caller can fall back to the static list (lobsterai-auth.ts:568-601).
func discoverModels(h *abiboot.Host, credential *Credential, cfg Config) []remoteModel {
	if h == nil || credential == nil {
		return nil
	}
	clientVersion := resolveClientVersion(h, cfg)
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodGet,
		URL:     buildModelsURL(credential, clientVersion),
		Headers: modelsHeaders(credential, clientVersion),
	})
	if errDo != nil {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	var decoded any
	if errDecode := decodeJSON(response.Body, &decoded); errDecode != nil {
		return nil
	}
	return parseModels(decoded)
}

// handleModelRegister reports the static fallback catalog. It runs when no
// account is bound yet, so it must not touch the network.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelRegistrationResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// handleModelStatic is the model.static variant of the same catalog.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// handleModelForAuth reports the catalog for one bound account, using the
// discovered listing when enabled and available. A failure falls back to the
// static list instead of failing the listing.
func handleModelForAuth(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := settings()
	if !cfg.DiscoverModels {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
	}
	now := time.Now()
	if cached, _ := discoveredModels.get(modelCacheTTL(cfg), now); len(cached) > 0 {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: modelInfos(cached)}, nil
	}

	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
	}
	models := discoverModels(h, credential, cfg)
	if len(models) == 0 {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
	}
	discoveredModels.put(models, now)
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: modelInfos(models)}, nil
}

// modelInfos renders a catalog slice for the host.
func modelInfos(models []remoteModel) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		out = append(out, modelInfoFor(model))
	}
	return out
}
