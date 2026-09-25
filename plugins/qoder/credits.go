package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Qoder credit balance and daily check-in.
//
//	GET  {OpenAPIBase}/sash/api/v2/me/usage
//	GET  {OpenAPIBase}/sash/api/v1/me/campaigns
//	POST {OpenAPIBase}/sash/api/v1/me/campaigns/{campaignId}/claim   (empty body)
//
// All three need only `Bearer` + `Cosy-ClientType` — NOT the WASM signature.
// Only the model-listing endpoint is signed (`qoder-credits.ts:1-8`, `:82-96`);
// the balance endpoint was wrongly assumed to need it early on and the credit
// capability was disabled as a result.
const (
	// UsagePath is mounted on OpenAPIBase (`qoder-credits.ts:75`).
	UsagePath = "/sash/api/v2/me/usage"
	// CampaignsPath is mounted on OpenAPIBase (`qoder-credits.ts:77`).
	CampaignsPath = "/sash/api/v1/me/campaigns"
)

// creditPackage is one quota bucket (`CreditPackage`, `qoder-credits.ts:125-150`).
type creditPackage struct {
	Name        string
	Unit        string
	Remaining   float64
	Total       float64
	Used        float64
	Active      bool
	ExpiredTime string
}

// creditBalance aggregates the buckets (`CreditBalance`).
type creditBalance struct {
	Total    float64
	Packages []creditPackage
}

// campaign is one entry of the campaigns list (`QoderCampaign`,
// `qoder-credits.ts:236-248`).
type campaign struct {
	CampaignID  string
	CampaignKey string
	// ActionType is `CLAIM_BENEFIT` for a claimable credit campaign and
	// `VIEW_DETAILS` for a link-only one, which must not be claimed
	// (`qoder-credits.ts:239-243`).
	ActionType string
	// ClaimStatus is the authoritative pre-claim state (`CLAIMABLE`, `CLAIMED`).
	ClaimStatus string
	// Amount is `benefit.amount`; -1 means absent.
	Amount float64
}

// campaigns is the parsed campaigns response (`QoderCampaigns`,
// `qoder-credits.ts:257-261`).
type campaigns struct {
	ShowCampaign bool
	Claimable    bool
	Campaigns    []campaign
}

// claimOutcome is one claim attempt result (`ClaimOutcome`).
type claimOutcome struct {
	// Status is one of: claimed, already-claimed, inactive, failed.
	Status  string
	Message string
	Amount  float64
	Balance *creditBalance
}

// creditsHeaders is `creditsHeaders` (`qoder-credits.ts:88-96`).
func creditsHeaders(credential *Credential, _ *product) http.Header {
	return canonicalHeader(
		"Accept", "application/json",
		"Authorization", "Bearer "+credential.bearerToken(),
		// Fixed header from upstream `Bx()`; the server uses it to tell client
		// shapes apart (`qoder-credits.ts:88-96`).
		"Cosy-ClientType", sharedClientMetadata.ClientType,
		"User-Agent", "Qoder",
	)
}

// readNumber reads a JSON number that may also arrive as a numeric string.
func readNumber(source map[string]any, key string) (float64, bool) {
	switch typed := source[key].(type) {
	case float64:
		return typed, true
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed, true
		}
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// readText reads a non-empty JSON string.
func readText(source map[string]any, key string) string {
	if text, ok := source[key].(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// toPackage converts one quota object (`toPackage`, `qoder-credits.ts:125-150`).
//
// Negative values are clamped to 0: the server can send negative remainders
// after an over-charge or a metering rollback, and passing that through shows the
// user "-12.5 credits".
func toPackage(name string, quota any, expiredTime string) (creditPackage, bool) {
	source, ok := quota.(map[string]any)
	if !ok {
		return creditPackage{}, false
	}
	total, hasTotal := readNumber(source, "total")
	used, hasUsed := readNumber(source, "used")
	remaining, hasRemaining := readNumber(source, "remaining")
	if !hasTotal && !hasUsed && !hasRemaining {
		return creditPackage{}, false
	}
	totalValue := maxFloat(total, 0)
	usedValue := maxFloat(used, 0)
	remainingValue := maxFloat(remaining, 0)
	if !hasRemaining {
		remainingValue = maxFloat(totalValue-usedValue, 0)
	}
	unit := readText(source, "unit")
	if unit == "" {
		unit = "credits"
	}
	return creditPackage{
		Name:        name,
		Unit:        unit,
		Remaining:   remainingValue,
		Total:       totalValue,
		Used:        usedValue,
		Active:      true,
		ExpiredTime: expiredTime,
	}, true
}

func maxFloat(value, floor float64) float64 {
	if value < floor {
		return floor
	}
	return value
}

// fetchCreditBalance reads `/sash/api/v2/me/usage`.
//
// A nil balance with a nil error means "not applicable": enterprise accounts
// expose no numbers at all, only an external link, and reporting 0 would suggest
// the quota ran out (`qoder-credits.ts:158-160`, `:193-194`).
func fetchCreditBalance(h *abiboot.Host, credential *Credential, cfg Config) (*creditBalance, error) {
	p := credential.product(cfg.Region)
	response, errDo := hostRequest(h, http.MethodGet, p.OpenAPIBase+UsagePath, creditsHeaders(credential, p), nil, cfg)
	if errDo != nil {
		return nil, abiboot.Errorf("credits_transport", "查询积分失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.Errorf("credits_status", "积分接口返回 HTTP %d：%s",
			response.StatusCode, truncate(string(response.Body), 300))
	}
	var root map[string]any
	if errDecode := json.Unmarshal(response.Body, &root); errDecode != nil {
		return nil, abiboot.Errorf("credits_decode", "解析积分响应失败：%v", errDecode)
	}
	if readText(root, "displayMode") == "enterprise" {
		return nil, nil
	}
	usage, ok := root["qoderUsage"].(map[string]any)
	if !ok {
		return nil, abiboot.Errorf("credits_shape", "积分响应缺少 qoderUsage")
	}

	packages := make([]creditPackage, 0, 3)
	// Order is display order: plan quota, then bonus/add-on, then dedicated packs
	// (`qoder-credits.ts:199-216`).
	if pkg, okPackage := toPackage("套餐额度", usage["userQuota"], ""); okPackage {
		packages = append(packages, pkg)
	}
	if pkg, okPackage := toPackage("资源包", usage["addOnQuota"], ""); okPackage {
		packages = append(packages, pkg)
	}
	if dedicated, okList := usage["dedicatedResourcePackages"].([]any); okList {
		for _, item := range dedicated {
			entry, okEntry := item.(map[string]any)
			if !okEntry {
				continue
			}
			name := readText(entry, "name")
			if name == "" {
				name = readText(entry, "id")
			}
			if name == "" {
				name = "专用资源包"
			}
			expired := readText(entry, "expiresAt")
			if expired == "" {
				expired = readText(entry, "expires_at")
			}
			if pkg, okPackage := toPackage(name, entry, expired); okPackage {
				packages = append(packages, pkg)
			}
		}
	}
	if len(packages) == 0 {
		// A response whose shape does not match is "query failed", not "zero
		// balance" (`qoder-credits.ts:218-220`).
		return nil, abiboot.Errorf("credits_shape", "积分响应没有可解析的额度包")
	}
	total := 0.0
	for _, pkg := range packages {
		total += pkg.Remaining
	}
	return &creditBalance{Total: total, Packages: packages}, nil
}

// loadCampaigns reads the campaigns list (`loadCampaigns`, `qoder-credits.ts:301-320`).
func loadCampaigns(h *abiboot.Host, credential *Credential, cfg Config) (*campaigns, error) {
	p := credential.product(cfg.Region)
	response, errDo := hostRequest(h, http.MethodGet, p.OpenAPIBase+CampaignsPath, creditsHeaders(credential, p), nil, cfg)
	if errDo != nil {
		return nil, abiboot.Errorf("campaigns_transport", "查询活动列表失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.Errorf("campaigns_status", "活动列表返回 HTTP %d：%s",
			response.StatusCode, truncate(string(response.Body), 300))
	}
	return parseCampaigns(response.Body)
}

// parseCampaigns is `parseQoderCampaigns` (`qoder-credits.ts:264-287`).
func parseCampaigns(body []byte) (*campaigns, error) {
	var root map[string]any
	if errDecode := json.Unmarshal(body, &root); errDecode != nil {
		return nil, abiboot.Errorf("campaigns_decode", "解析活动列表失败：%v", errDecode)
	}
	parsed := &campaigns{
		ShowCampaign: root["showCampaign"] == true,
		Claimable:    root["claimable"] == true,
	}
	items, _ := root["campaigns"].([]any)
	for _, item := range items {
		entry, okEntry := item.(map[string]any)
		if !okEntry {
			continue
		}
		id := readText(entry, "campaignId")
		if id == "" {
			continue
		}
		row := campaign{
			CampaignID:  id,
			CampaignKey: readText(entry, "campaignKey"),
			ActionType:  readText(entry, "actionType"),
			ClaimStatus: readText(entry, "claimStatus"),
			Amount:      -1,
		}
		if benefit, okBenefit := entry["benefit"].(map[string]any); okBenefit {
			if amount, okAmount := readNumber(benefit, "amount"); okAmount {
				row.Amount = amount
			}
		}
		parsed.Campaigns = append(parsed.Campaigns, row)
	}
	return parsed, nil
}

// claimableCampaigns keeps only what may be claimed right now
// (`claimableCampaigns`, `qoder-credits.ts:365-369`).
func claimableCampaigns(parsed *campaigns) []campaign {
	out := make([]campaign, 0, len(parsed.Campaigns))
	for _, item := range parsed.Campaigns {
		if item.ActionType == "CLAIM_BENEFIT" && item.ClaimStatus == "CLAIMABLE" {
			out = append(out, item)
		}
	}
	return out
}

// claimCampaign claims one campaign (`claimQoderCampaign`, `qoder-credits.ts:398-435`).
//
// ⚠️ Idempotency is a BODY field, not an HTTP status: a repeat claim also returns
// 200 but carries `replayed:true`, no `benefit`, and the OLD `claimedAt`. Reading
// only the status would report "claimed +100" for a day the user already took
// (`qoder-credits.ts:389-396`).
//
// ⚠️ The request body must be empty (the capture showed `content-length: 0`).
func claimCampaign(h *abiboot.Host, credential *Credential, cfg Config, campaignID string) claimOutcome {
	p := credential.product(cfg.Region)
	headers := creditsHeaders(credential, p)
	headers.Set("Content-Type", "application/json")
	url := p.OpenAPIBase + CampaignsPath + "/" + urlPathEscape(campaignID) + "/claim"
	response, errDo := hostRequest(h, http.MethodPost, url, headers, nil, cfg)
	if errDo != nil {
		return claimOutcome{Status: "failed", Message: errDo.Error()}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return claimOutcome{Status: "failed", Message: describeNonJSON(response.StatusCode, string(response.Body))}
	}

	var root map[string]any
	if errDecode := json.Unmarshal(response.Body, &root); errDecode != nil {
		return claimOutcome{Status: "failed", Message: describeNonJSON(response.StatusCode, string(response.Body))}
	}
	if replayed, okReplayed := root["replayed"].(bool); okReplayed && replayed {
		return claimOutcome{Status: "already-claimed", Message: "今天已领取"}
	}
	if status := readText(root, "status"); status != "" && status != "CLAIMED" {
		return claimOutcome{Status: "failed", Message: "领取未成功（status=" + status + "）"}
	}
	amount := 0.0
	if benefit, okBenefit := root["benefit"].(map[string]any); okBenefit {
		if parsed, okAmount := readNumber(benefit, "amount"); okAmount {
			amount = parsed
		}
	}
	return claimOutcome{Status: "claimed", Message: "领取成功", Amount: amount}
}

// claimDailyCheckin claims every currently claimable campaign
// (`claimQoderDailyCheckin`, `qoder-credits.ts:456-481`).
func claimDailyCheckin(h *abiboot.Host, credential *Credential, cfg Config) (claimOutcome, error) {
	parsed, errLoad := loadCampaigns(h, credential, cfg)
	if errLoad != nil {
		return claimOutcome{Status: "failed", Message: "活动列表查询失败：" + errLoad.Error()}, nil
	}
	targets := claimableCampaigns(parsed)
	if len(targets) == 0 {
		// `showCampaign:false, claimable:false, campaigns:[]` is what the server
		// sends once today's benefit has been taken; "already claimed" and "no
		// campaign" are indistinguishable there, and the activity refreshes daily
		// at 10:00 UTC+8, so the conservative reading is "already claimed"
		// (`qoder-credits.ts:250-256`, `:330-338`).
		return claimOutcome{Status: "already-claimed", Message: "今天已领取"}, nil
	}

	total := 0.0
	firstError := ""
	for _, target := range targets {
		outcome := claimCampaign(h, credential, cfg, target.CampaignID)
		if outcome.Status == "claimed" {
			total += outcome.Amount
			continue
		}
		if outcome.Status == "failed" && firstError == "" {
			firstError = outcome.Message
		}
	}
	if total > 0 {
		return claimOutcome{Status: "claimed", Message: "领取成功", Amount: total}, nil
	}
	if firstError != "" {
		return claimOutcome{Status: "failed", Message: firstError}, nil
	}
	return claimOutcome{Status: "already-claimed", Message: "今天已领取"}, nil
}

// describeNonJSON explains a non-JSON answer; the gateway returns HTML when the
// credential is dead (`qoder-credits.ts:438-442`).
func describeNonJSON(status int, text string) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "凭据已失效（HTTP " + itoaInt(status) + "），请重新登录该账号"
	}
	snippet := strings.Join(strings.Fields(truncate(text, 80)), " ")
	return "服务端返回了非 JSON 响应（HTTP " + itoaInt(status) + "）：" + snippet
}

// urlPathEscape escapes one path segment.
func urlPathEscape(value string) string {
	replacer := strings.NewReplacer("/", "%2F", "?", "%3F", "#", "%23", " ", "%20")
	return replacer.Replace(value)
}

// ---------------------------------------------------------------------------
// quota provider
// ---------------------------------------------------------------------------

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the provider and the reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        "Qoder 积分与每日领取",
		SupportsReset:      false,
	}, nil
}

// handleQuotaFetch reports the current credit balance and campaign state.
func handleQuotaFetch(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.QuotaFetchRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		return nil, errCredential
	}
	cfg := settings()
	summary := []pluginapi.QuotaMetric{}
	balance, errBalance := fetchCreditBalance(h, credential, cfg)
	if errBalance != nil {
		return nil, errBalance
	}
	if balance != nil {
		for index, pkg := range balance.Packages {
			summary = append(summary, pluginapi.QuotaMetric{
				Key:   "package_" + itoaInt(index),
				Label: pkg.Name,
				Value: pkg.Remaining,
				Unit:  pkg.Unit,
				// "number" keeps fractional credit balances readable.
				Format: "number",
			})
		}
		if len(balance.Packages) > 1 {
			summary = append(summary, pluginapi.QuotaMetric{
				Key: "credit_remaining", Label: "剩余合计", Value: balance.Total, Unit: "credit", Format: "number",
			})
		}
	}
	if parsed, errCampaigns := loadCampaigns(h, credential, cfg); errCampaigns == nil {
		claimable := 0.0
		if len(claimableCampaigns(parsed)) > 0 {
			claimable = 1
		}
		summary = append(summary, pluginapi.QuotaMetric{
			Key: "daily_checkin", Label: "今日可领取", Value: claimable, Format: "boolean",
		})
	}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "Qoder 不支持重置额度"}, nil
}
