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

// Remote model catalog parsing and the model-facing handlers. Everything here
// ports trae.ts:522-1086 (parsing + metadata) and trae-auth.ts:456-540 (the
// batch discovery call).

// reasoningConfig is `reasoning_effort_config` (trae.ts:686-693).
//
// `options` values are both the display name and the wire value sent upstream;
// unlike LobsterAI there is no level/openclawLevel pair (trae.ts:147-151).
type reasoningConfig struct {
	DefaultLevel    string
	Options         []string
	SupportThinking *bool
}

// remoteModel is one `config_info_list` entry with the metadata the adapter
// consumes (trae.ts:525-681). Pointer flags keep "absent" distinct from "false".
type remoteModel struct {
	ID      string
	Name    string
	Channel string
	Usage   string

	IsCustomModel *bool
	IsHidden      *bool
	IsEnabled     *bool
	MaxMode       *bool
	Multimodal    *bool
	// ToolResponseMultimodal is carried for diagnostics only: it describes
	// whether *tool results* may embed images and must not be used to veto user
	// images (trae.ts:648-660).
	ToolResponseMultimodal *bool

	ContextWindow       int64
	MaxContextWindow    int64
	MaxOutputTokens     int64
	MaxModeOutputTokens int64

	Reasoning *reasoningConfig

	CreditsRate         *float64
	OriginalCreditsRate *float64
	DiscountEndsAtSec   int64
}

// ── entry readers (trae.ts:732-936) ──

// readContextWindowField reads `context_window_tokens.dev` and falls back to
// `max` (trae.ts:732-742). The regular window is `dev`; announcing `max` on a
// non-Max session makes the client send more context than upstream accepts.
func readContextWindowField(entry map[string]any) int64 {
	raw, ok := asMap(entry["context_window_tokens"])
	if !ok {
		raw, ok = asMap(entry["ContextWindowTokens"])
	}
	if !ok {
		return 0
	}
	for _, key := range []string{"dev", "Dev", "max", "Max"} {
		if value, ok := readNumberField(raw, key); ok && value > 0 {
			return int64(value)
		}
	}
	return 0
}

// readMaxContextWindowField reads `context_window_tokens.max`, the Max-mode
// window (trae.ts:751-757).
func readMaxContextWindowField(entry map[string]any) int64 {
	raw, ok := asMap(entry["context_window_tokens"])
	if !ok {
		raw, ok = asMap(entry["ContextWindowTokens"])
	}
	if !ok {
		return 0
	}
	for _, key := range []string{"max", "Max"} {
		if value, ok := readNumberField(raw, key); ok && value > 0 {
			return int64(value)
		}
	}
	return 0
}

// readDetailMaxTokens reads `model_detail_list[].max_tokens`, preferring the
// entry whose model_name ends with the suffix (trae.ts:768-782). The observed
// suffixes are `__dev` (regular) and `__max` (Max mode).
func readDetailMaxTokens(entry map[string]any, preferredSuffix string) int64 {
	raw, ok := asSlice(entry["model_detail_list"])
	if !ok {
		raw, ok = asSlice(entry["ModelDetailList"])
	}
	if !ok || len(raw) == 0 {
		return 0
	}
	details := []map[string]any{}
	for _, item := range raw {
		if detail, ok := asMap(item); ok {
			details = append(details, detail)
		}
	}
	if len(details) == 0 {
		return 0
	}
	chosen := details[0]
	for _, detail := range details {
		if strings.HasSuffix(readStringField(detail, "model_name"), preferredSuffix) {
			chosen = detail
			break
		}
	}
	for _, key := range []string{"max_tokens", "MaxTokens"} {
		if value, ok := readNumberField(chosen, key); ok && value > 0 {
			return int64(value)
		}
	}
	return 0
}

// readReasoningEffortConfig reads `reasoning_effort_config` (trae.ts:794-829).
// An object with none of the three fields yields nil so the caller declares no
// reasoning control at all.
func readReasoningEffortConfig(entry map[string]any) *reasoningConfig {
	raw, ok := asMap(entry["reasoning_effort_config"])
	if !ok {
		raw, ok = asMap(entry["ReasoningEffortConfig"])
	}
	if !ok {
		return nil
	}

	options := []string{}
	if rawOptions, ok := asSlice(raw["options"]); ok {
		for _, item := range rawOptions {
			if text, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(text); trimmed != "" {
					options = append(options, trimmed)
				}
				continue
			}
			// Defensive: upstream could switch to `{level, openclawLevel}`.
			if object, ok := asMap(item); ok {
				wire := readStringField(object, "openclawLevel")
				if wire == "" {
					wire = readStringField(object, "level", "Level")
				}
				if trimmed := strings.TrimSpace(wire); trimmed != "" {
					options = append(options, trimmed)
				}
			}
		}
	} else if rawOptions, ok := asSlice(raw["Options"]); ok {
		for _, item := range rawOptions {
			if text, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(text); trimmed != "" {
					options = append(options, trimmed)
				}
			}
		}
	}

	defaultLevel := readStringField(raw, "default_level", "DefaultLevel")
	supportThinking, hasSupport := readBooleanField(raw, "support_thinking", "SupportThinking")
	if len(options) == 0 && defaultLevel == "" && !hasSupport {
		return nil
	}
	config := &reasoningConfig{DefaultLevel: defaultLevel, Options: options}
	if hasSupport {
		value := supportThinking
		config.SupportThinking = &value
	}
	return config
}

// parseJSONObjectString parses a JSON object held in a string (trae.ts:928-936).
func parseJSONObjectString(raw string) (map[string]any, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, false
	}
	object, ok := asMap(parsed)
	return object, ok
}

// readConsumptionRate reads the credit multiplier out of `display_contact_config`
// (trae.ts:851-864). That field is a JSON *string*, so it has to be parsed a
// second time. `rate: 0` is a legitimate value (free) and must not be filtered
// out with a `> 0` test (docs/agents/trae.md:411-419).
func readConsumptionRate(entry map[string]any) *float64 {
	raw := readStringField(entry, "display_contact_config", "DisplayContactConfig")
	config, ok := parseJSONObjectString(raw)
	if !ok {
		return nil
	}
	rate, ok := asMap(config["consumption_rate"])
	if !ok {
		rate, ok = asMap(config["ConsumptionRate"])
	}
	if !ok {
		return nil
	}
	if enabled, has := readBooleanField(rate, "enable"); has && !enabled {
		return nil
	}
	data, ok := asMap(rate["data"])
	if !ok {
		data, ok = asMap(rate["Data"])
	}
	if !ok {
		return nil
	}
	value, ok := readNumberField(data, "rate")
	if !ok || value < 0 {
		return nil
	}
	return &value
}

// readActivityDiscount reads `activity_discount` and only reports it when the
// discount is actually in effect right now (trae.ts:885-925):
// `discount_type != "none"`, `before > after > 0` and, when a deadline exists,
// it has not passed.
func readActivityDiscount(entry map[string]any, nowSec int64) (float64, int64, bool) {
	raw := readStringField(entry, "display_contact_config", "DisplayContactConfig")
	config, ok := parseJSONObjectString(raw)
	if !ok {
		return 0, 0, false
	}
	discount, ok := asMap(config["activity_discount"])
	if !ok {
		discount, ok = asMap(config["ActivityDiscount"])
	}
	if !ok {
		return 0, 0, false
	}
	if enabled, has := readBooleanField(discount, "enable"); has && !enabled {
		return 0, 0, false
	}
	data, ok := asMap(discount["data"])
	if !ok {
		data, ok = asMap(discount["Data"])
	}
	if !ok {
		return 0, 0, false
	}
	current, ok := asMap(data["current"])
	if !ok {
		current, ok = asMap(data["Current"])
	}
	if !ok {
		return 0, 0, false
	}
	discountType := strings.ToLower(strings.TrimSpace(readStringField(current, "discount_type", "discountType")))
	if discountType == "" || discountType == "none" {
		return 0, 0, false
	}
	before, hasBefore := readNumberField(current, "before_consumption_rate", "beforeConsumptionRate")
	if !hasBefore || before <= 0 {
		return 0, 0, false
	}
	if after, hasAfter := readNumberField(current, "consumption_rate", "consumptionRate"); hasAfter && before <= after {
		return 0, 0, false
	}
	var endsAt int64
	for _, value := range data {
		nested, ok := asMap(value)
		if !ok {
			continue
		}
		if end, ok := readNumberField(nested, "end_at", "endAt"); ok && end > 0 {
			endsAt = int64(end)
			break
		}
	}
	if endsAt > 0 && endsAt <= nowSec {
		return 0, 0, false
	}
	return before, endsAt, true
}

// parseTraeConfigEntry parses one catalog entry (trae.ts:939-1005). Every flag
// that upstream did not send stays nil: the filters only drop explicit matches.
func parseTraeConfigEntry(entry map[string]any, channel string, nowSec int64) (remoteModel, bool) {
	id := readStringField(entry, "config_name", "ConfigName")
	if id == "" {
		return remoteModel{}, false
	}
	display, hasDisplay := asMap(entry["display_config"])
	if !hasDisplay {
		display, hasDisplay = asMap(entry["DisplayConfig"])
	}
	name := id
	if hasDisplay {
		if displayName := readStringField(display, "display_name"); displayName != "" {
			name = displayName
		}
	}

	model := remoteModel{ID: id, Name: name, Channel: channel}
	model.Usage = readStringField(entry, "usage", "Usage")
	model.ContextWindow = readContextWindowField(entry)
	model.MaxContextWindow = readMaxContextWindowField(entry)
	model.MaxOutputTokens = readDetailMaxTokens(entry, "__dev")
	model.MaxModeOutputTokens = readDetailMaxTokens(entry, "__max")
	model.Reasoning = readReasoningEffortConfig(entry)
	model.CreditsRate = readConsumptionRate(entry)
	if original, endsAt, ok := readActivityDiscount(entry, nowSec); ok {
		model.OriginalCreditsRate = &original
		model.DiscountEndsAtSec = endsAt
	}

	if hasDisplay {
		if value, ok := readBooleanField(display, "is_custom_model", "IsCustomModel"); ok {
			model.IsCustomModel = &value
		}
		if value, ok := readBooleanField(display, "max_mode", "MaxMode"); ok {
			model.MaxMode = &value
		}
		if value, ok := readBooleanField(display, "multimodal", "Multimodal"); ok {
			model.Multimodal = &value
		}
		if value, ok := readBooleanField(display, "tool_response_multimodal", "ToolResponseMultimodal"); ok {
			model.ToolResponseMultimodal = &value
		}
	}
	if value, ok := readBooleanField(entry, "is_invisible_to_user", "IsInvisibleToUser"); ok {
		model.IsHidden = &value
	}
	if value, ok := readBooleanField(entry, "config_switch", "ConfigSwitch"); ok {
		model.IsEnabled = &value
	}
	return model, true
}

// ParseBatchModelList parses a `batch_get_detail_param` response
// (trae.ts:1050-1086). Later channels overwrite earlier ones for the same
// config_name because they tend to carry the fuller configuration, and the three
// hard filters are applied while merging:
//
//  1. `usage` must be `chat_completion` (the batch table also lists summary /
//     multimodal / custom entries);
//  2. `config_switch !== false`;
//  3. `is_invisible_to_user !== true` — the catalog mirrors the official
//     Auto Mode picker (docs/agents/trae.md:103-119).
func ParseBatchModelList(body []byte, nowSec int64) []remoteModel {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	groups, ok := asSlice(root["function_configs"])
	if !ok {
		groups, ok = asSlice(root["FunctionConfigs"])
	}
	if !ok {
		return nil
	}

	order := []string{}
	byID := map[string]remoteModel{}
	for _, rawGroup := range groups {
		group, ok := asMap(rawGroup)
		if !ok {
			continue
		}
		channel := readStringField(group, "function", "Function")
		list, ok := asSlice(group["config_info_list"])
		if !ok {
			list, ok = asSlice(group["ConfigInfoList"])
		}
		if !ok {
			continue
		}
		for _, rawItem := range list {
			item, ok := asMap(rawItem)
			if !ok {
				continue
			}
			model, ok := parseTraeConfigEntry(item, channel, nowSec)
			if !ok {
				continue
			}
			if model.Usage != "" && model.Usage != "chat_completion" {
				continue
			}
			if model.IsEnabled != nil && !*model.IsEnabled {
				continue
			}
			if model.IsHidden != nil && *model.IsHidden {
				continue
			}
			if _, exists := byID[model.ID]; !exists {
				order = append(order, model.ID)
			}
			byID[model.ID] = model
		}
	}

	out := make([]remoteModel, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// ParseModelList parses the single-channel `get_detail_param` response
// (trae.ts:1012-1025). It is not used for catalogs — the batch endpoint is
// authoritative (docs/agents/trae.md:89-101) — but the parser exists so the
// single-channel shape can be recognised if it is ever fed in.
func ParseModelList(body []byte, nowSec int64) []remoteModel {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	list, ok := asSlice(root["config_info_list"])
	if !ok {
		list, ok = asSlice(root["ConfigInfoList"])
	}
	if !ok {
		list, ok = asSlice(root["data"])
	}
	if !ok {
		return nil
	}
	out := []remoteModel{}
	for _, rawItem := range list {
		item, ok := asMap(rawItem)
		if !ok {
			continue
		}
		if model, ok := parseTraeConfigEntry(item, "", nowSec); ok {
			out = append(out, model)
		}
	}
	return out
}

// ── catalog filtering and presentation (trae.ts:695-730, trae-adapter.ts:450-744) ──

// isModelCallable applies the two "necessarily unreachable" filters
// (trae.ts:711-713). `is_invisible_to_user` is deliberately absent: it is already
// a hard filter at parse time.
func isModelCallable(model remoteModel) bool {
	if model.IsCustomModel != nil && *model.IsCustomModel {
		return false
	}
	if model.IsEnabled != nil && !*model.IsEnabled {
		return false
	}
	return true
}

// maxModeFor reports whether this request may use Max mode (1M context). All
// three conditions must hold (trae-adapter.ts:588-597):
//
//  1. the product-level switch is on (default on);
//  2. the *remote* config marks `display_config.max_mode === true` — Max fields
//     must never be invented for an unmarked model;
//  3. the optional whitelist matches.
func maxModeFor(model remoteModel, cfg Config) bool {
	if !cfg.MaxMode {
		return false
	}
	if model.MaxMode == nil || !*model.MaxMode {
		return false
	}
	if len(cfg.MaxModeModels) == 0 {
		return true
	}
	for _, allowed := range cfg.MaxModeModels {
		if allowed == "*" || allowed == model.ID {
			return true
		}
	}
	return false
}

// modelSupportsImage reports the per-model image capability
// (trae-adapter.ts:545-547). Absent means unsupported: capability is never
// invented (docs/agents/trae.md:204-247).
func modelSupportsImage(model remoteModel) bool {
	return model.Multimodal != nil && *model.Multimodal
}

// displayName renders the model name with its credit multiplier
// (trae-adapter.ts:469-480). `name` is purely presentational on the client side,
// and the composer only renders `name`, which is why the rate has to live there.
func displayName(model remoteModel) string {
	if model.CreditsRate == nil {
		return model.Name
	}
	current := "免费"
	if *model.CreditsRate != 0 {
		current = "x" + trimFloat(*model.CreditsRate)
	}
	if model.OriginalCreditsRate != nil && *model.OriginalCreditsRate > *model.CreditsRate {
		return model.Name + " · x" + trimFloat(*model.OriginalCreditsRate) + "→" + current
	}
	return model.Name + " · " + current
}

// modelInfoForRemote renders the host-facing descriptor for one remote model.
func modelInfoForRemote(model remoteModel, cfg Config) pluginapi.ModelInfo {
	contextWindow := model.ContextWindow
	maxOutput := model.MaxOutputTokens
	if maxModeFor(model, cfg) {
		// Never mix the two windows: announcing 1M without the Max fields makes
		// the client send context upstream will reject
		// (trae-adapter.ts:569-575).
		if model.MaxContextWindow > 0 {
			contextWindow = model.MaxContextWindow
		} else {
			contextWindow = MaxContextTokens
		}
		if model.MaxModeOutputTokens > 0 {
			maxOutput = model.MaxModeOutputTokens
		}
	}
	if maxOutput <= 0 {
		maxOutput = productFor(cfg.Region).FallbackMaxOutputTokens
	}
	modalities := []string{"text"}
	if modelSupportsImage(model) {
		modalities = []string{"text", "image"}
	}
	info := pluginapi.ModelInfo{
		ID:                         model.ID,
		Object:                     "model",
		Created:                    time.Now().Unix(),
		OwnedBy:                    ProviderKey,
		Type:                       "chat",
		DisplayName:                displayName(model),
		Name:                       model.ID,
		Description:                "TRAE " + model.Name,
		ContextLength:              contextWindow,
		InputTokenLimit:            contextWindow,
		MaxCompletionTokens:        maxOutput,
		OutputTokenLimit:           maxOutput,
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   modalities,
		SupportedOutputModalities:  []string{"text"},
	}
	if model.Reasoning != nil && len(model.Reasoning.Options) > 0 {
		// `support_thinking === false` means "do not offer a level that does
		// nothing" (trae-adapter.ts:606-611). The reference also picks the
		// strongest level as the default; CPA's ThinkingSupport has no default
		// field, so only the level list is declared here.
		if model.Reasoning.SupportThinking == nil || *model.Reasoning.SupportThinking {
			info.Thinking = &pluginapi.ThinkingSupport{Levels: model.Reasoning.Options}
		}
	}
	return info
}

// staticModelInfos renders the product-level fallback catalog. Hidden entries are
// dropped here as well (trae-adapter.ts:656-660).
func staticModelInfos(cfg Config) []pluginapi.ModelInfo {
	fallback := productFor(cfg.Region).Fallback
	out := make([]pluginapi.ModelInfo, 0, len(fallback))
	for _, entry := range fallback {
		if entry.Hidden {
			continue
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         entry.ID,
			Object:                     "model",
			Created:                    time.Now().Unix(),
			OwnedBy:                    ProviderKey,
			Type:                       "chat",
			DisplayName:                entry.Name,
			Name:                       entry.ID,
			Description:                "TRAE " + entry.Name,
			ContextLength:              entry.ContextWindow,
			InputTokenLimit:            entry.ContextWindow,
			MaxCompletionTokens:        productFor(cfg.Region).FallbackMaxOutputTokens,
			OutputTokenLimit:           productFor(cfg.Region).FallbackMaxOutputTokens,
			SupportedGenerationMethods: []string{"chat.completions"},
			SupportedInputModalities:   []string{"text"},
			SupportedOutputModalities:  []string{"text"},
		})
	}
	return out
}

// catalog functions the discovery endpoint must list. The real CN IDE sends all
// 22 (trae-auth.ts:506-514); asking for every function keeps the response layout
// aligned with what the IDE sees, and `channels` adds experimental extras.
func catalogFunctions(cfg Config) []string {
	functions := []string{
		"ui_builder_v2", "solo_coder", "chat_v3", "solo_builder",
		"builder_v3", "builder", "chat", "inline_chat", "git_ai",
		"custom_agent_generation", "utils", "code_reviewer",
		"code_review_summary", "solo_agent", "solo_agent_remote",
		"solo_work_remote", "solo_agent_lite", "solo_work_lite",
		"solo_design_lite", "solo_design_remote", "multimodal",
		"system_diagnosis",
	}
	seen := map[string]bool{}
	for _, name := range functions {
		seen[name] = true
	}
	for _, channel := range cfg.Channels {
		if !seen[channel] {
			functions = append(functions, channel)
			seen[channel] = true
		}
	}
	return functions
}

// ── discovery + cache (trae-auth.ts:456-540, trae-adapter.ts:514-524) ──

// catalog is one cached discovery result.
type catalog struct {
	models    []remoteModel
	byID      map[string]remoteModel
	fetchedAt time.Time
}

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]catalog{}
)

// catalogKey scopes the cache to a region and account: two accounts can see
// different catalogs for the same model (channel membership is per account).
func catalogKey(cfg Config, credential *Credential) string {
	return cfg.Region + "|" + credential.UID
}

// cachedCatalog returns a non-expired catalog.
func cachedCatalog(cfg Config, credential *Credential) (catalog, bool) {
	ttl := time.Duration(cfg.ModelCacheTTLMS) * time.Millisecond
	if ttl <= 0 {
		ttl = DefaultModelCacheTTLMS * time.Millisecond
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	entry, ok := catalogCache[catalogKey(cfg, credential)]
	if !ok || time.Since(entry.fetchedAt) > ttl {
		return catalog{}, false
	}
	return entry, true
}

// storeCatalog caches one discovery result.
func storeCatalog(cfg Config, credential *Credential, models []remoteModel) catalog {
	entry := catalog{models: models, byID: map[string]remoteModel{}, fetchedAt: time.Now()}
	for _, model := range models {
		entry.byID[model.ID] = model
	}
	catalogMu.Lock()
	catalogCache[catalogKey(cfg, credential)] = entry
	catalogMu.Unlock()
	return entry
}

// invalidateCatalog drops the cached catalog for one account.
func invalidateCatalog(cfg Config, credential *Credential) {
	catalogMu.Lock()
	delete(catalogCache, catalogKey(cfg, credential))
	catalogMu.Unlock()
}

// fetchCatalog calls `batch_get_detail_param` and parses the response
// (trae-auth.ts:484-540). A non-2xx answer or an unparsable body is an error;
// callers fall back to the static catalog.
func fetchCatalog(h *abiboot.Host, credential *Credential, cfg Config) ([]remoteModel, error) {
	body, errMarshal := json.Marshal(map[string]any{
		"functions":                 catalogFunctions(cfg),
		"agent_type":                "",
		"current_config_info":       map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type":                 0,
		"access_type":               0,
		"ab_force_vids":             "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":         true,
	})
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_catalog", "构造模型目录请求失败: %v", errMarshal)
	}
	url := productFor(cfg.Region).AgentHost + BatchModelsPath
	response, errDo := hostDo(h, http.MethodPost, url,
		soloHeaders(credential, productFor(cfg.Region), false, currentMachineGeneration()),
		body, cfg.RequestTimeoutMS)
	if errDo != nil {
		return nil, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.Errorf("catalog_status", "模型目录返回 HTTP %d: %s",
			response.StatusCode, truncate(string(response.Body), 300))
	}
	models := ParseBatchModelList(response.Body, time.Now().Unix())
	if len(models) == 0 {
		return nil, abiboot.Errorf("catalog_empty", "模型目录响应中没有可调用条目")
	}
	return models, nil
}

// catalogFor returns the model catalog for one credential, using the cache when
// it is fresh (trae-adapter.ts:514-524).
func catalogFor(h *abiboot.Host, credential *Credential, cfg Config) (catalog, error) {
	if cached, ok := cachedCatalog(cfg, credential); ok {
		return cached, nil
	}
	models, err := fetchCatalog(h, credential, cfg)
	if err != nil {
		return catalog{}, err
	}
	return storeCatalog(cfg, credential, models), nil
}

// remoteModelFor looks one model up in the account's catalog. The bool reports
// whether the catalog answered at all.
func remoteModelFor(h *abiboot.Host, credential *Credential, cfg Config, modelID string) (remoteModel, bool, error) {
	entry, err := catalogFor(h, credential, cfg)
	if err != nil {
		return remoteModel{}, false, err
	}
	model, ok := entry.byID[modelID]
	return model, ok, nil
}

// channelFor returns the SOLO channel the model was listed under
// (trae-adapter.ts:557-560). Sending the wrong channel fails *inside* the stream
// with `4001`, which is why the model's own channel wins over the default.
func channelFor(model remoteModel, ok bool, cfg Config) string {
	if ok && strings.TrimSpace(model.Channel) != "" {
		return model.Channel
	}
	if strings.TrimSpace(cfg.DefaultChannel) != "" {
		return cfg.DefaultChannel
	}
	return DefaultFunction
}

// configNameFor strips the internal `__dev` / `__max` suffix the chat endpoint
// rejects (trae.ts:1360-1365).
func configNameFor(model string) string {
	if idx := strings.Index(model, "__"); idx >= 0 {
		return model[:idx]
	}
	return model
}

// ── model handlers ──

// handleModelRegister reports the static fallback catalog. It must not touch the
// network: it also runs when no account exists yet.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelRegistrationResponse{Provider: ProviderKey, Models: staticModelInfos(settings())}, nil
}

// handleModelStatic is the model.static variant of the same catalog.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos(settings())}, nil
}

// handleModelForAuth reports the catalog for one bound account.
//
// Without a usable credential the reply is an empty list and never an error
// (docs/PORTING.md:52): an error would be reported as a catalog failure, an empty
// list simply hides the provider group. When discovery is disabled or fails the
// static fallback list is used so the user still sees a usable catalog.
func handleModelForAuth(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := settings()
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil || credential.AccessToken == "" {
		// The contract is to answer with an empty list rather than an error, but a
		// credential that cannot be parsed is a real fault worth surfacing: it
		// silently hides every model of the provider.
		if h != nil && errCredential != nil {
			h.Log("warn", "TRAE 凭据解析失败，无法列出模型",
				map[string]any{"error": errCredential.Error(), "auth_id": request.AuthID})
		}
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: []pluginapi.ModelInfo{}}, nil
	}
	if !cfg.DiscoverModels {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos(cfg)}, nil
	}
	entry, errCatalog := catalogFor(h, credential, cfg)
	if errCatalog != nil || len(entry.models) == 0 {
		if h != nil && errCatalog != nil {
			h.Log("warn", "TRAE 模型目录获取失败，回退内置列表", map[string]any{"error": errCatalog.Error()})
		}
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos(cfg)}, nil
	}
	out := make([]pluginapi.ModelInfo, 0, len(entry.models))
	for _, model := range entry.models {
		if !isModelCallable(model) {
			continue
		}
		out = append(out, modelInfoForRemote(model, cfg))
	}
	if len(out) == 0 {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos(cfg)}, nil
	}
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: out}, nil
}

// truncate shortens an upstream body for inclusion in an error message.
func truncate(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
