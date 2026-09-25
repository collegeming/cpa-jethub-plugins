package main

// 本文件是 Jet-Hub `src/buddy.ts:381-910`（模型目录与展示名）与
// `src/buddy-oauth.ts:348-499`（远端模型拉取）的 Go 移植，外加 `src/product.ts`
// 的两张兜底模型表。
//
// 必须保留的 TS 语义（逐条来自源码/AGENTS.md，`file:line` 见注释）：
//   - 远端 `data.models[].maxOutputTokens` 是**权威的单次输出上限**，必须被消费
//     到请求体的 `max_tokens`（见 executor.go），并作为 defaultMaxTokens 播报；
//     0 / 负数 / NaN 必须过滤（buddy.ts:889-894, buddy-adapter.ts:1467-1469）。
//   - 兜底表是**白名单式重建**，但被 agent 引用的模型即使不在表里也要保留
//     （buddy-adapter.ts:651-698）。
//   - 促销折扣必须按 schedule 本地推算此刻是否生效，不能只看 enabled
//     （buddy.ts:557-587）。

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	// tzdata embeds the IANA database so the promotion schedule can be resolved
	// by name (`Asia/Shanghai`, buddy.ts:537) even in a minimal container image.
	_ "time/tzdata"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ── 展示名兜底表（buddy.ts:384-405）──

var modelDisplayNames = map[string]string{
	"deepseek-v4-flash": "DeepSeek V4 Flash",
	"deepseek-v4-pro":   "DeepSeek V4 Pro",
	"hy4-preview":       "Hy4 Preview",
	"hy4-preview-x":     "Hy4 Preview X",
	"hy3":               "Hy3",
	"hy3-x":             "Hy3 X",
	"glm-5.3":           "GLM-5.3",
	"glm-5.3-flash":     "GLM-5.3 Flash",
	"glm-5.2":           "GLM-5.2",
	"glm-5.1":           "GLM-5.1",
	"glm-5v-turbo":      "GLM-5V Turbo",
	"kimi-k3-1":         "Kimi K3-1",
	"kimi-k2.7":         "Kimi K2.7",
	"kimi-k2.6":         "Kimi K2.6",
	"minimax-m3":        "MiniMax M3",
}

// displayNameForModel falls back to the id. buddy.ts:403-405.
func displayNameForModel(id string) string {
	if name, ok := modelDisplayNames[id]; ok {
		return name
	}
	return id
}

// ── 兜底模型表（product.ts）──

// fallbackModel mirrors `BuddyFallbackModel` (product.ts:33-51).
type fallbackModel struct {
	ID                     string
	Name                   string
	ContextWindow          int64
	MaxOutputTokens        int64
	SupportsImages         bool
	ReasoningEfforts       []string
	DefaultReasoningEffort string
}

// codeBuddyFallbackModels is `CODEBUDDY_FALLBACK_MODELS` (product.ts:168-254),
// which is also the WorkBuddy 国内版 catalog (product.ts:450).
var codeBuddyFallbackModels = []fallbackModel{
	{ID: "hy4-preview", Name: "Hy4 preview", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"high"}, DefaultReasoningEffort: "high", MaxOutputTokens: 64_000},
	{ID: "hy3", Name: "Hy3", ContextWindow: 192_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high", MaxOutputTokens: 64_000},
	{ID: "hy3-x", Name: "Hy3", ContextWindow: 192_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high", MaxOutputTokens: 64_000},
	{ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 128_000},
	{ID: "deepseek-v4-pro", Name: "Deepseek-V4-Pro", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high", "xhigh"}, DefaultReasoningEffort: "high", MaxOutputTokens: 128_000},
	{ID: "deepseek-v4-flash", Name: "Deepseek-V4-Flash", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 50_000},
	{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 64_000},
	{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 32_000},
	{ID: "glm-5.2", Name: "GLM-5.2", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"high", "xhigh"}, DefaultReasoningEffort: "high", MaxOutputTokens: 64_000},
	{ID: "glm-5.1", Name: "GLM-5.1", ContextWindow: 200_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 48_000},
	{ID: "glm-5v-turbo", Name: "GLM-5V-Turbo", ContextWindow: 200_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 64_000},
	{ID: "kimi-k3-1", Name: "Kimi-K3-1", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 32_000},
	{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 64_000},
	{ID: "kimi-k2.7", Name: "Kimi-K2.7", ContextWindow: 256_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 32_000},
	{ID: "kimi-k2.6", Name: "Kimi-K2.6", ContextWindow: 256_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 32_000},
	{ID: "minimax-m3", Name: "MiniMax-M3", ContextWindow: 512_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 64_000},
}

// workBuddyFallbackModels is `WORKBUDDY_FALLBACK_MODELS` (product.ts:283-355),
// which is also the CodeBuddy 国际版 catalog (product.ts:421).
var workBuddyFallbackModels = []fallbackModel{
	{ID: "default-model", Name: "Auto", ContextWindow: 176_000, SupportsImages: true, MaxOutputTokens: 24_000},
	{ID: "fast-model", Name: "Fast", ContextWindow: 200_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 32_000},
	{ID: "balanced-model", Name: "Balanced", ContextWindow: 256_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 32_000},
	{ID: "primary-model", Name: "Primary", ContextWindow: 272_000, SupportsImages: true, ReasoningEfforts: []string{"high"}, MaxOutputTokens: 72_000},
	{ID: "deep-model", Name: "Deep", ContextWindow: 176_000, SupportsImages: true, MaxOutputTokens: 24_000},
	{ID: "hy4-preview-f", Name: "Hy4 preview", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"high"}, DefaultReasoningEffort: "high", MaxOutputTokens: 64_000},
	{ID: "hy4-preview", Name: "Hy4 preview", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"high"}, DefaultReasoningEffort: "high", MaxOutputTokens: 64_000},
	{ID: "hy3", Name: "Hy3", ContextWindow: 192_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high", MaxOutputTokens: 64_000},
	{ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"high"}, DefaultReasoningEffort: "high", MaxOutputTokens: 128_000},
	{ID: "deepseek-v4.1-flash-sg", Name: "Deepseek-V4.1-Flash", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"high"}, DefaultReasoningEffort: "high", MaxOutputTokens: 128_000},
	{ID: "gpt-6-astra", Name: "GPT-6-Astra", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 128_000},
	{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 128_000},
	{ID: "gpt-5.6-terra", Name: "GPT-5.6-Terra", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 128_000},
	{ID: "gpt-5.6-luna", Name: "GPT-5.6-Luna", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 128_000},
	{ID: "gpt-5.5", Name: "GPT-5.5", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh"}, DefaultReasoningEffort: "high", MaxOutputTokens: 128_000},
	{ID: "gpt-5.4", Name: "GPT-5.4", ContextWindow: 272_000, SupportsImages: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh"}, DefaultReasoningEffort: "high", MaxOutputTokens: 72_000},
	// gpt-5.3-codex：远端未下发 maxOutputTokens，故不填（product.ts:336-338）。
	{ID: "gpt-5.3-codex", Name: "GPT-5.3-Codex", ContextWindow: 272_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}},
	{ID: "gemini-3.5-flash", Name: "Gemini-3.5-Flash", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 65_536},
	{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 48_000},
	{ID: "glm-5.2", Name: "GLM-5.2", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"high", "xhigh"}, DefaultReasoningEffort: "high", MaxOutputTokens: 48_000},
	{ID: "kimi-k3", Name: "Kimi-K3", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 32_000},
	{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", ContextWindow: 1_000_000, SupportsImages: true, ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high", MaxOutputTokens: 32_000},
	{ID: "kimi-k2.6", Name: "Kimi-K2.6", ContextWindow: 256_000, SupportsImages: true, ReasoningEfforts: []string{"medium"}, MaxOutputTokens: 32_000},
}

// ── 远端模型（buddy.ts:407-462）──

// remoteModel mirrors `BuddyRemoteModel` (buddy.ts:408-462). Zero values mean
// "the remote did not declare the field"; positiveMaxTokens and the *bool
// tri-state keep that distinction for the fields where it matters.
type remoteModel struct {
	ID                     string
	Name                   string
	ContextWindow          int64
	MaxOutputTokens        int64
	SupportsImages         *bool
	CreditsRate            string
	DiscountedCreditsRate  string
	ReasoningEfforts       []string
	DefaultReasoningEffort string
	AgentReferenced        bool
}

// catalog is a resolved model list ready to be rendered.
type catalog struct {
	Models []remoteModel
}

// ── 倍率归一化（buddy.ts:475-506, 680-686）──

var (
	// ratePrefixRe matches the normal form `x0.29` (x first). buddy.ts:476.
	ratePrefixRe = regexp.MustCompile(`(?i)^x(\d+(?:\.\d+)?)$`)
	// rateSuffixRe matches the promotion form `0.50x` (x last). buddy.ts:490.
	rateSuffixRe = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)x$`)
)

// regexpMatch returns the first capture group, or "" when there is no match.
func regexpMatch(expression *regexp.Regexp, value string) string {
	match := expression.FindStringSubmatch(value)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

// normalizeRate tries the prefixed form then the suffixed form and always
// returns `x<number>`. buddy.ts:499-506.
func normalizeRate(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	// Drop a possible unit suffix such as "x0.03 credits".
	head := strings.Fields(strings.TrimSpace(text))
	if len(head) == 0 {
		return ""
	}
	candidate := head[0]
	if match := regexpMatch(ratePrefixRe, candidate); match != "" {
		return "x" + match
	}
	if match := regexpMatch(rateSuffixRe, candidate); match != "" {
		return "x" + match
	}
	return ""
}

// normalizeCreditsRate normalises `data.models[].credits`. buddy.ts:475-477.
func normalizeCreditsRate(value any) string { return normalizeRate(value) }

// normalizeDiscountedRate normalises `modelPromotions[].discount.discountedCredits`.
// buddy.ts:489-491.
func normalizeDiscountedRate(value any) string { return normalizeRate(value) }

// formatCreditsRate renders `rate→discounted` when a promotion applies.
// buddy.ts:680-686.
func formatCreditsRate(rate, discounted string) string {
	if rate == "" {
		return discounted
	}
	if discounted != "" {
		return rate + "→" + discounted
	}
	return rate
}

// ── 促销解析（buddy.ts:508-669）──

// parseHHMM parses `HH:MM` (tolerating a missing leading zero) into minutes.
// buddy.ts:521-528.
func parseHHMM(value any) (int, bool) {
	text, ok := value.(string)
	if !ok {
		return 0, false
	}
	trimmed := strings.TrimSpace(text)
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 || len(parts[1]) != 2 {
		return 0, false
	}
	hour := 0
	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			return 0, false
		}
		hour = hour*10 + int(r-'0')
	}
	if parts[0] == "" || len(parts[0]) > 2 {
		return 0, false
	}
	minute := 0
	for _, r := range parts[1] {
		if r < '0' || r > '9' {
			return 0, false
		}
		minute = minute*10 + int(r-'0')
	}
	if hour >= 24 || minute >= 60 {
		return 0, false
	}
	return hour*60 + minute, true
}

// zonedMinutes returns the wall-clock minute of day in the promotion timezone.
// buddy.ts:536-549.
func zonedMinutes(now time.Time, timeZone any) (int, bool) {
	zone := "Asia/Shanghai"
	if name, ok := timeZone.(string); ok && name != "" {
		zone = name
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return 0, false
	}
	local := now.In(location)
	return local.Hour()*60 + local.Minute(), true
}

// hasTimeWindow reports whether a schedule carries any window.
// buddy.ts:552-555.
func hasTimeWindow(schedule map[string]any) bool {
	if daily, ok := schedule["daily"].([]any); ok && len(daily) > 0 {
		return true
	}
	if _, ok := schedule["validFrom"]; ok {
		return true
	}
	if _, ok := schedule["validUntil"]; ok {
		return true
	}
	return false
}

// promotionActiveNow decides whether a promotion window contains `now`.
// buddy.ts:565-587.
func promotionActiveNow(item map[string]any, now time.Time) bool {
	raw, ok := item["schedule"]
	if !ok {
		return true
	}
	schedule, ok := raw.(map[string]any)
	if !ok {
		return true
	}
	if from, ok := schedule["validFrom"].(string); ok {
		if parsed, okParse := parseTimeMs(from); okParse && now.UnixMilli() < parsed {
			return false
		}
	}
	if until, ok := schedule["validUntil"].(string); ok {
		if parsed, okParse := parseTimeMs(until); okParse && now.UnixMilli() >= parsed {
			return false
		}
	}
	daily, ok := schedule["daily"].([]any)
	if !ok || len(daily) == 0 {
		return true
	}
	minutes, okZone := zonedMinutes(now, schedule["timezone"])
	if !okZone {
		// 时区不可解析时不误杀（buddy.ts:577-578）。
		return true
	}
	for _, slot := range daily {
		entry, okSlot := slot.(map[string]any)
		if !okSlot {
			continue
		}
		start, okStart := parseHHMM(entry["start"])
		end, okEnd := parseHHMM(entry["end"])
		if !okStart || !okEnd {
			continue
		}
		if start <= end {
			if minutes >= start && minutes < end {
				return true
			}
		} else if minutes >= start || minutes < end {
			return true
		}
	}
	return false
}

// priorityOf reads a promotion priority; missing/invalid is 0.
// buddy.ts:665-669.
func priorityOf(item any) float64 {
	record, ok := item.(map[string]any)
	if !ok {
		return 0
	}
	priority, ok := record["priority"].(float64)
	if !ok {
		return 0
	}
	return priority
}

// parsePromotions maps model id → the promotion rate in effect at `now`.
// buddy.ts:614-662.
func parsePromotions(record map[string]any, now time.Time) map[string]string {
	result := map[string]string{}
	chosen := map[string]float64{}
	promotions, ok := record["modelPromotions"].([]any)
	if !ok {
		return result
	}
	// 升序写入，使高 priority 覆盖低 priority。
	sorted := make([]any, len(promotions))
	copy(sorted, promotions)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && priorityOf(sorted[j-1]) > priorityOf(sorted[j]); j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	for _, item := range sorted {
		promotion, okPromotion := item.(map[string]any)
		if !okPromotion {
			continue
		}
		if enabled, isBool := promotion["enabled"].(bool); isBool && !enabled {
			continue
		}
		if !promotionActiveNow(promotion, now) {
			continue
		}
		discount, okDiscount := promotion["discount"].(map[string]any)
		if !okDiscount {
			continue
		}
		factor, hasFactor := discount["factor"].(float64)
		windowed := false
		if rawSchedule, okSchedule := promotion["schedule"].(map[string]any); okSchedule {
			windowed = hasTimeWindow(rawSchedule)
		}

		rate := ""
		if hasFactor && factor == 0 {
			// factor 0 = 免费，但免费额度必须限时；无窗口的 `0x` 视为已结束占位。
			// buddy.ts:639-642, 607-612。
			if !windowed {
				continue
			}
			rate = "免费"
		} else {
			rate = normalizeDiscountedRate(discount["discountedCredits"])
			if rate == "x0" {
				continue
			}
		}
		if rate == "" {
			continue
		}

		modelIDs, okIDs := promotion["modelIds"].([]any)
		if !okIDs {
			continue
		}
		priority := priorityOf(promotion)
		for _, id := range modelIDs {
			modelID, okID := id.(string)
			if !okID || modelID == "" {
				continue
			}
			if previous, seen := chosen[modelID]; seen && previous > priority {
				continue
			}
			chosen[modelID] = priority
			result[modelID] = rate
		}
	}
	return result
}

// ── /v3/config 解析（buddy.ts:709-909）──

// isAutoSelectAlias filters `auto` / `default`. buddy.ts:865-867.
func isAutoSelectAlias(id string) bool { return id == "auto" || id == "default" }

// isChatModel filters completion/NES-only and image-generation models.
// buddy.ts:869-876.
func isChatModel(id string, meta map[string]any) bool {
	if strings.HasPrefix(id, "nes-") || strings.HasPrefix(id, "completion-") || strings.HasPrefix(id, "codewise-") {
		return false
	}
	if supportsExtra, ok := meta["supportsExtra"].(bool); ok && supportsExtra {
		return false
	}
	if maxOutput, ok := meta["maxOutputTokens"].(float64); ok && maxOutput > 0 && maxOutput <= 256 {
		return false
	}
	if tags, ok := meta["tags"].([]any); ok {
		for _, tag := range tags {
			if tag == "text-to-image" {
				return false
			}
		}
	}
	return true
}

// parseModelMeta extracts the context window and capabilities of one model.
// buddy.ts:884-909.
func parseModelMeta(record map[string]any) remoteModel {
	var out remoteModel
	if record == nil {
		return out
	}
	if limit, ok := record["maxInputTokens"].(float64); ok && limit == limit && limit > 0 {
		out.ContextWindow = int64(limit)
	}
	// 单次输出上限：只保留正数，缺失即 0（= 未声明）。buddy.ts:889-894。
	if maxOutput, ok := record["maxOutputTokens"].(float64); ok && maxOutput == maxOutput && maxOutput > 0 {
		out.MaxOutputTokens = int64(maxOutput)
	}
	if supportsImages, ok := record["supportsImages"].(bool); ok {
		out.SupportsImages = &supportsImages
	}
	if reasoning, ok := record["reasoning"].(map[string]any); ok {
		if efforts, okEfforts := reasoning["supportedEfforts"].([]any); okEfforts {
			filtered := make([]string, 0, len(efforts))
			for _, effort := range efforts {
				if text, okText := effort.(string); okText && text != "" {
					filtered = append(filtered, text)
				}
			}
			if len(filtered) > 0 {
				out.ReasoningEfforts = filtered
			}
		}
		if effort, okEffort := reasoning["defaultEffort"].(string); okEffort && effort != "" {
			out.DefaultReasoningEffort = effort
		}
	}
	return out
}

// preferredAgentNames carries the "selectable chat models" whitelist.
// buddy.ts:821.
var preferredAgentNames = []string{"cli", "craft"}

// trialModelIDs reads productFeaturesConfig.ModelTrialBanner targetModelId.
// buddy.ts:824-838.
func trialModelIDs(data map[string]any) []string {
	features, ok := data["productFeaturesConfig"].(map[string]any)
	if !ok {
		return nil
	}
	banner, ok := features["ModelTrialBanner"].(map[string]any)
	if !ok {
		return nil
	}
	banners, ok := banner["banners"].([]any)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(banners))
	for _, item := range banners {
		record, okRecord := item.(map[string]any)
		if !okRecord {
			continue
		}
		if target, okTarget := record["targetModelId"].(string); okTarget && target != "" {
			ids = append(ids, target)
		}
	}
	return ids
}

// parseModelsFromConfig parses /v3/config (or the scoped enterprise endpoint,
// which shares the shape). buddy.ts:709-813.
func parseModelsFromConfig(body any) []remoteModel {
	root, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	data, ok := root["data"].(map[string]any)
	if !ok {
		return nil
	}

	metaByID := map[string]map[string]any{}
	if models, okModels := data["models"].([]any); okModels {
		for _, model := range models {
			entry, okEntry := model.(map[string]any)
			if !okEntry {
				continue
			}
			if id, okID := entry["id"].(string); okID {
				metaByID[id] = entry
			}
		}
	}

	promotions := parsePromotions(data, time.Now())

	agentReferenced := map[string]bool{}
	if agents, okAgents := data["agents"].([]any); okAgents {
		for _, agent := range agents {
			record, okRecord := agent.(map[string]any)
			if !okRecord {
				continue
			}
			models, okModels := record["models"].([]any)
			if !okModels {
				continue
			}
			for _, model := range models {
				if id, okID := model.(string); okID {
					agentReferenced[id] = true
				}
			}
		}
	}

	parsed := make([]remoteModel, 0, len(metaByID))
	seen := map[string]bool{}
	push := func(id string) {
		if isAutoSelectAlias(id) || seen[id] || !isChatModel(id, metaByID[id]) {
			return
		}
		seen[id] = true
		meta := metaByID[id]
		entry := parseModelMeta(meta)
		entry.ID = id
		// 展示名优先用服务端下发的 name（buddy.ts:750-758）。
		if meta != nil {
			if name, okName := meta["name"].(string); okName && name != "" {
				entry.Name = name
			}
		}
		if entry.Name == "" {
			entry.Name = displayNameForModel(id)
		}
		entry.CreditsRate = normalizeCreditsRate(metaValue(meta, "credits"))
		if discounted, okDiscounted := promotions[id]; okDiscounted {
			entry.DiscountedCreditsRate = discounted
		}
		if agentReferenced[id] {
			entry.AgentReferenced = true
		}
		parsed = append(parsed, entry)
	}

	// 1. 主对话 agent 引用的模型优先（cli → craft，先命中即止）。
	for _, agentName := range preferredAgentNames {
		found := false
		if agents, okAgents := data["agents"].([]any); okAgents {
			for _, agent := range agents {
				record, okRecord := agent.(map[string]any)
				if !okRecord {
					continue
				}
				if name, _ := record["name"].(string); name != agentName {
					continue
				}
				if models, okModels := record["models"].([]any); okModels {
					for _, model := range models {
						if id, okID := model.(string); okID {
							push(id)
						}
					}
				}
				found = true
				break
			}
		}
		if found {
			break
		}
	}

	// 2. 补齐 data.models 里其余可对话模型。
	for id := range metaByID {
		push(id)
	}

	// 3. 追加试用模型。
	for _, id := range trialModelIDs(data) {
		if isAutoSelectAlias(id) || seen[id] {
			continue
		}
		seen[id] = true
		meta := metaByID[id]
		entry := parseModelMeta(meta)
		entry.ID = id
		entry.Name = displayNameForModel(id)
		entry.CreditsRate = normalizeCreditsRate(metaValue(meta, "credits"))
		if discounted, okDiscounted := promotions[id]; okDiscounted {
			entry.DiscountedCreditsRate = discounted
		}
		entry.AgentReferenced = true
		parsed = append(parsed, entry)
	}

	return parsed
}

func metaValue(meta map[string]any, key string) any {
	if meta == nil {
		return nil
	}
	return meta[key]
}

// ── 兜底校正（buddy-adapter.ts:651-698）──

// reconcileWithFallback rebuilds the catalog from the product fallback table
// (whitelist semantics) while keeping models the server marked agent-referenced.
func reconcileWithFallback(models []remoteModel, product productConfig) []remoteModel {
	fallback := product.FallbackModels
	if len(fallback) == 0 {
		return append([]remoteModel(nil), models...)
	}
	remoteByID := map[string]remoteModel{}
	for _, model := range models {
		remoteByID[model.ID] = model
	}
	reconciled := make([]remoteModel, 0, len(fallback)+len(models))
	for _, entry := range fallback {
		remote, hasRemote := remoteByID[entry.ID]
		merged := remoteModel{ID: entry.ID, Name: entry.Name}
		if hasRemote && remote.Name != "" {
			merged.Name = remote.Name
		}
		// 远端优先、兜底补位（buddy-adapter.ts:658-676）。
		if remote.ContextWindow > 0 {
			merged.ContextWindow = remote.ContextWindow
		} else {
			merged.ContextWindow = entry.ContextWindow
		}
		if remote.MaxOutputTokens > 0 {
			merged.MaxOutputTokens = remote.MaxOutputTokens
		} else {
			merged.MaxOutputTokens = entry.MaxOutputTokens
		}
		if remote.SupportsImages != nil {
			merged.SupportsImages = remote.SupportsImages
		} else {
			supportsImages := entry.SupportsImages
			merged.SupportsImages = &supportsImages
		}
		if remote.ReasoningEfforts != nil {
			merged.ReasoningEfforts = append([]string(nil), remote.ReasoningEfforts...)
		} else if entry.ReasoningEfforts != nil {
			merged.ReasoningEfforts = append([]string(nil), entry.ReasoningEfforts...)
		}
		if remote.DefaultReasoningEffort != "" {
			merged.DefaultReasoningEffort = remote.DefaultReasoningEffort
		} else {
			merged.DefaultReasoningEffort = entry.DefaultReasoningEffort
		}
		// 计费倍率只可能来自远端。
		merged.CreditsRate = remote.CreditsRate
		merged.DiscountedCreditsRate = remote.DiscountedCreditsRate
		reconciled = append(reconciled, merged)
	}
	// 追加「被 agent 引用但不在兜底表」的模型（buddy-adapter.ts:687-696）。
	known := map[string]bool{}
	for _, model := range reconciled {
		known[model.ID] = true
	}
	for _, model := range models {
		if !model.AgentReferenced || known[model.ID] {
			continue
		}
		known[model.ID] = true
		reconciled = append(reconciled, model)
	}
	return reconciled
}

// staticCatalog returns the product's built-in catalog (no network).
func staticCatalog(product productConfig) []remoteModel {
	fallback := product.FallbackModels
	out := make([]remoteModel, 0, len(fallback))
	for _, entry := range fallback {
		supportsImages := entry.SupportsImages
		out = append(out, remoteModel{
			ID:                     entry.ID,
			Name:                   entry.Name,
			ContextWindow:          entry.ContextWindow,
			MaxOutputTokens:        entry.MaxOutputTokens,
			SupportsImages:         &supportsImages,
			ReasoningEfforts:       append([]string(nil), entry.ReasoningEfforts...),
			DefaultReasoningEffort: entry.DefaultReasoningEffort,
		})
	}
	return out
}

// ── 展示名（buddy-adapter.ts:1409-1456）──

// displayNameForModelWithSuffix builds `Name · rate variant`. The rate and the
// variant label both go into `name`, never `description`
// (buddy-adapter.ts:1396-1412).
func displayNameForModelWithSuffix(model remoteModel, all []remoteModel) string {
	suffix := displaySuffix(model, all)
	if suffix == "" {
		return model.Name
	}
	return model.Name + " · " + suffix
}

// displaySuffix assembles the rate and same-name variant parts.
// buddy-adapter.ts:1415-1424.
func displaySuffix(model remoteModel, all []remoteModel) string {
	parts := []string{}
	if rate := formatCreditsRate(model.CreditsRate, model.DiscountedCreditsRate); rate != "" {
		parts = append(parts, rate)
	}
	if variant := variantLabelFor(model, all); variant != "" {
		parts = append(parts, variant)
	}
	return strings.Join(parts, " ")
}

// variantLabelFor disambiguates models that share a display name by appending
// the part of the id that follows the group's common prefix.
// buddy-adapter.ts:1437-1443.
func variantLabelFor(model remoteModel, all []remoteModel) string {
	group := make([]string, 0, len(all))
	for _, candidate := range all {
		if candidate.Name == model.Name {
			group = append(group, candidate.ID)
		}
	}
	if len(group) <= 1 {
		return ""
	}
	prefix := commonPrefix(group)
	variant := strings.TrimLeft(strings.TrimPrefix(model.ID, prefix), "-")
	return strings.ToUpper(variant)
}

// commonPrefix returns the longest shared prefix of a set of strings.
// buddy-adapter.ts:1446-1456.
func commonPrefix(values []string) string {
	if len(values) == 0 {
		return ""
	}
	prefix := values[0]
	for _, value := range values[1:] {
		limit := len(prefix)
		if len(value) < limit {
			limit = len(value)
		}
		index := 0
		for index < limit && prefix[index] == value[index] {
			index++
		}
		prefix = prefix[:index]
		if prefix == "" {
			break
		}
	}
	return prefix
}

// ── 模型元数据投影 ──

// positiveMaxTokens keeps only a safe positive integer, so the host never
// receives an illegal defaultMaxTokens (buddy-adapter.ts:1467-1469).
func positiveMaxTokens(value int64) (int, bool) {
	if value <= 0 {
		return 0, false
	}
	const maxInt = int64(^uint(0) >> 1)
	if value > maxInt {
		return 0, false
	}
	return int(value), true
}

// inputModalitiesFor reports whether the model accepts images.
// buddy-adapter.ts:727-733, with the glm-5.1 override from :718-721.
func inputModalitiesFor(model remoteModel) []string {
	supportsImages := false
	if imageCapabilityOverrides[model.ID] {
		supportsImages = true
	} else if model.SupportsImages != nil {
		supportsImages = *model.SupportsImages
	}
	if supportsImages {
		return []string{"text", "image"}
	}
	return []string{"text"}
}

// imageCapabilityOverrides lists models whose remote `supportsImages` was
// disproved by a real request (buddy-adapter.ts:718-721).
var imageCapabilityOverrides = map[string]bool{
	// scoped 端点误报 false，实测能看图。
	"glm-5.1": true,
}

// effortNames maps an effort id to its label (buddy-adapter.ts:159-165).
var effortNames = map[string]string{
	"low":    "Low",
	"medium": "Medium",
	"high":   "High",
	"xhigh":  "XHigh",
	"max":    "Max",
}

// modelInfoFor projects one catalog entry into the host-facing descriptor.
func modelInfoFor(product productConfig, model remoteModel, all []remoteModel) pluginapi.ModelInfo {
	name := displayNameForModelWithSuffix(model, all)
	info := pluginapi.ModelInfo{
		ID:                         model.ID,
		Object:                     "model",
		Created:                    time.Now().Unix(),
		OwnedBy:                    ProviderKey,
		Type:                       "chat",
		DisplayName:                name,
		Name:                       model.ID,
		Description:                product.DisplayName + " " + name,
		ContextLength:              model.ContextWindow,
		InputTokenLimit:            model.ContextWindow,
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   inputModalitiesFor(model),
		SupportedOutputModalities:  []string{"text"},
		UserDefined:                false,
	}
	if maxTokens, ok := positiveMaxTokens(model.MaxOutputTokens); ok {
		info.MaxCompletionTokens = int64(maxTokens)
		info.OutputTokenLimit = int64(maxTokens)
	}
	if len(model.ReasoningEfforts) > 0 {
		levels := make([]string, 0, len(model.ReasoningEfforts))
		for _, effort := range model.ReasoningEfforts {
			if label, ok := effortNames[effort]; ok {
				levels = append(levels, label)
				continue
			}
			levels = append(levels, effort)
		}
		info.Thinking = &pluginapi.ThinkingSupport{Levels: levels}
	}
	return info
}

// modelInfosFromCatalog projects a whole catalog.
func modelInfosFromCatalog(product productConfig, models []remoteModel) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		out = append(out, modelInfoFor(product, model, models))
	}
	return out
}

// ── 远端模型拉取（buddy-oauth.ts:348-499）──

// enterpriseModelsScope is the {scope} segment the personal account uses.
// buddy-oauth.ts:473.
const enterpriseModelsScope = "personal"

// modelRequestHeaders builds the identity headers shared by both model
// endpoints (buddy-oauth.ts:355-364).
func modelRequestHeaders(credential *Credential, product productConfig) map[string]string {
	headers := credentialAuthHeaders(credential)
	headers[HeaderDomain] = product.APIDomain
	headers["User-Agent"] = product.UserAgent
	headers[HeaderProduct] = DeploymentType
	headers[HeaderProductCode] = product.ProductCode
	return headers
}

// configSnapshot is the /v3/config result: models plus the promotion table.
type configSnapshot struct {
	Models     []remoteModel
	Promotions map[string]string
}

// requestConfigModel reads /v3/config and its promotion table.
// buddy-oauth.ts:438-459.
func requestConfigModel(h *abiboot.Host, headers map[string]string, product productConfig) configSnapshot {
	empty := configSnapshot{Promotions: map[string]string{}}
	status, body, errDo := doJSON(h, http.MethodGet, product.urlFor(ConfigPath), headers, nil)
	if errDo != nil || status != http.StatusOK {
		return empty
	}
	root, okRoot := body.(map[string]any)
	if !okRoot {
		return empty
	}
	data, okData := root["data"].(map[string]any)
	if !okData {
		return empty
	}
	return configSnapshot{Models: parseModelsFromConfig(body), Promotions: parsePromotions(data, time.Now())}
}

// requestScopedModels reads the enterprise model endpoint.
// buddy-oauth.ts:479-499.
func requestScopedModels(h *abiboot.Host, headers map[string]string, product productConfig) []remoteModel {
	rawURL := strings.TrimRight(product.Endpoint, "/") + "/console/enterprises/" + enterpriseModelsScope + "/models"
	status, body, errDo := doJSON(h, http.MethodGet, rawURL, headers, nil)
	if errDo != nil || status != http.StatusOK {
		return nil
	}
	models := parseModelsFromConfig(body)
	if len(models) == 0 {
		return nil
	}
	return models
}

// mergeRemoteModels merges two catalogs, keeping primary's metadata and order.
// buddy-oauth.ts:418-424.
func mergeRemoteModels(primary, extra []remoteModel) []remoteModel {
	known := map[string]bool{}
	for _, model := range primary {
		known[model.ID] = true
	}
	merged := append([]remoteModel(nil), primary...)
	for _, model := range extra {
		if known[model.ID] {
			continue
		}
		known[model.ID] = true
		merged = append(merged, model)
	}
	return merged
}

// applyPromotions fills in the discounted rate from the promotion table.
// buddy-oauth.ts:462-470.
func applyPromotions(models []remoteModel, promotions map[string]string) []remoteModel {
	out := make([]remoteModel, 0, len(models))
	for _, model := range models {
		if discounted, ok := promotions[model.ID]; ok {
			model.DiscountedCreditsRate = discounted
		}
		out = append(out, model)
	}
	return out
}

// fetchRemoteModels implements the three-tier fallback documented at
// buddy-oauth.ts:366-408: scoped endpoint (+/v3/config merge) → /v3/config.
func fetchRemoteModels(h *abiboot.Host, credential *Credential, product productConfig) []remoteModel {
	if credential == nil || credential.AccessToken == "" {
		return nil
	}
	headers := modelRequestHeaders(credential, product)
	if scoped := requestScopedModels(h, headers, product); len(scoped) > 0 {
		// ⚠️ 两个端点下发的模型 id 集合不同，必须取并集（buddy-oauth.ts:378-394）。
		config := requestConfigModel(h, headers, product)
		merged := mergeRemoteModels(scoped, config.Models)
		if len(config.Promotions) == 0 {
			return merged
		}
		return applyPromotions(merged, config.Promotions)
	}
	status, body, errDo := doJSON(h, http.MethodGet, product.urlFor(ConfigPath), headers, nil)
	if errDo != nil || status != http.StatusOK {
		return nil
	}
	return parseModelsFromConfig(body)
}

// ── 模型缓存 ──

// modelCache memoises one discovered catalog per product for a bounded time.
type modelCache struct {
	mu      sync.Mutex
	entries map[string]cachedCatalog
}

type cachedCatalog struct {
	models    []remoteModel
	fetchedAt time.Time
}

var discoveredModels = modelCache{entries: map[string]cachedCatalog{}}

func (c *modelCache) get(key string, ttl time.Duration) ([]remoteModel, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || len(entry.models) == 0 || time.Since(entry.fetchedAt) > ttl {
		return nil, false
	}
	return entry.models, true
}

func (c *modelCache) put(key string, models []remoteModel) {
	if len(models) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cachedCatalog{models: models, fetchedAt: time.Now()}
}

// reset drops every cached catalog; used when the settings change.
func (c *modelCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]cachedCatalog{}
}

// cachedCatalogKey scopes the cache to one product so switching products never
// serves the wrong model pool.
func cachedCatalogKey(product productConfig) string { return product.ConfigValue }

// ── 模型方法 ──

// productForCredential resolves the product an account belongs to. The value
// stored in the auth file wins; imported Jet-Hub files without it fall back to
// the configured product.
func productForCredential(credential *Credential) productConfig {
	if credential != nil && credential.Product != "" {
		if product, ok := productByID(credential.Product); ok {
			return product
		}
	}
	product, _ := productByConfigValue(settings().Product)
	return product
}

// handleModelRegister reports the static fallback catalog.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	product, _ := productByConfigValue(settings().Product)
	models := staticCatalog(product)
	return pluginapi.ModelRegistrationResponse{
		Provider: ProviderKey,
		Models:   modelInfosFromCatalog(product, models),
	}, nil
}

// handleModelStatic is the model.static variant of the same catalog.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	product, _ := productByConfigValue(settings().Product)
	models := staticCatalog(product)
	return pluginapi.ModelResponse{
		Provider: ProviderKey,
		Models:   modelInfosFromCatalog(product, models),
	}, nil
}

// handleModelForAuth reports the catalog for one bound account.
//
// Per the catalog-gating rules (AGENTS.md), a missing/unparseable credential
// yields the static catalog rather than an error.
func handleModelForAuth(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := settings()
	product, _ := productByConfigValue(cfg.Product)
	models := staticCatalog(product)

	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential == nil {
		if credential.Product == "" {
			credential.Product = product.ConfigValue
		}
		product = productForCredential(credential)
		if cfg.DiscoverModels {
			models = discoveredCatalogFor(h, credential, product, cfg)
		}
	}
	return pluginapi.ModelResponse{
		Provider: ProviderKey,
		Models:   modelInfosFromCatalog(product, models),
	}, nil
}

// discoveredCatalogFor returns the cached or freshly fetched catalog for an
// account, always reconciled against the product fallback table.
func discoveredCatalogFor(h *abiboot.Host, credential *Credential, product productConfig, cfg Config) []remoteModel {
	ttl := time.Duration(cfg.ModelCacheTTLMS) * time.Millisecond
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	key := cachedCatalogKey(product)
	if cached, ok := discoveredModels.get(key, ttl); ok {
		return cached
	}
	remote := fetchRemoteModels(h, credential, product)
	if len(remote) == 0 {
		return staticCatalog(product)
	}
	reconciled := reconcileWithFallback(remote, product)
	discoveredModels.put(key, reconciled)
	return reconciled
}
