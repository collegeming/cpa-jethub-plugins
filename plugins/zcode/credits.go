package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Balance, activity reporting and the daily claim.
//
// ## Why the activation events are not optional
//
// The server does NOT push entitlements. `billing/preview` answers
// `{"code":0,"data":{"plans":[]}}` until the client has reported activity; after
// `POST /api/v1/event/report` with `app_launch` and `app_daily_active` the very
// same call lists a plan:
//
//	before: {"code":0,"data":{"plans":[]}}
//	after:  {"code":0,"data":{"plans":[{"plan_id":"zcode-v3-start-plan-trust-…"}]}}
//
// ⇒ "daily random distribution" is not a push at all: the server decides whether
// to grant anything from the activity signal. Querying the preview without
// reporting first reports "nothing to claim" every single day, and the user
// concludes the check-in is broken.
//
// ## Why the balance is rendered in TOKENS
//
// The balance buckets publish `unit_type` (measured `"token"`) and `meter`
// (measured `"model_usage"`) alongside `total_units` / `used_units` /
// `remaining_units`. A real defect in the reference came from rendering
// `remaining_units` as a generic "credits" number: the UI showed `94539275`,
// which reads like 94 million credits rather than 94.54M tokens. Both fields are
// therefore carried through and the unit is rendered explicitly.

// balanceBucket is one entry of `data.balances`.
type balanceBucket struct {
	PlanID   string
	ShowName string
	// UnitType is upstream's `unit_type` (measured `"token"`).
	UnitType string
	// Meter is upstream's `meter` (measured `"model_usage"`).
	Meter     string
	Total     int64
	Used      int64
	Remaining int64
	Available int64
	ExpiresAt int64
}

// balanceResult is the parsed balance.
type balanceResult struct {
	// Enterprise marks the `displayMode: "enterprise"` shape, which publishes no
	// numbers at all. Rendering that as 0 would read as "used up" instead of
	// "not reported".
	Enterprise bool
	Buckets    []balanceBucket
	Remaining  int64
	Total      int64
	// ExpiresAt is the earliest bucket expiry (Unix seconds).
	ExpiresAt int64
	// PlanName is the first bucket's display name.
	PlanName string
}

// claimablePlan is one entry of `data.plans`.
type claimablePlan struct {
	PlanID   string
	Priority int
	Name     string
}

// claimOutcome is one claim attempt.
type claimOutcome struct {
	PlanID string
	// Code is the business code (0 success, 1003 already claimed).
	Code int
	OK   bool
	// AlreadyClaimed marks the idempotent success.
	AlreadyClaimed bool
	HTTPStatus     int
	Message        string
}

// fetchBalance reads `GET /zcode-plan/billing/balance`.
//
// ⚠ `Authorization` is REQUIRED here (401 without it) and so is `X-Device-Mid`
// (400 code 3001 without it). This is the one read-only endpoint that exercises
// both, which is why it doubles as the credential probe.
func fetchBalance(h *abiboot.Host, credential *Credential, cfg Config) (*balanceResult, error) {
	headers := buildHeaders(credential, cfg, headerOptions{Authorization: true, Accept: "application/json"})
	response, errDo := hostRequest(h, http.MethodGet, Origin+BalancePath, headers, nil)
	if errDo != nil {
		return nil, transportError("balance_transport", "查询额度失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyUpstreamError(response.StatusCode, response.Body)
	}
	envelope := parseUpstreamEnvelope(response.Body, response.StatusCode)
	if !envelope.success() {
		return nil, classifyUpstreamError(response.StatusCode, response.Body)
	}
	data, errData := envelopeData(envelope, response.Body)
	if errData != nil {
		return nil, errData
	}
	return parseBalance(data), nil
}

// parseBalance reads the balance body.
func parseBalance(data map[string]any) *balanceResult {
	result := &balanceResult{}
	if strings.EqualFold(stringField(data, "displayMode"), "enterprise") {
		return &balanceResult{Enterprise: true}
	}
	rawBuckets, _ := data["balances"].([]any)
	for _, item := range rawBuckets {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bucket := balanceBucket{
			PlanID:   stringField(record, "plan_id"),
			ShowName: stringField(record, "show_name"),
			// ⚠ the unit fields MUST be carried: upstream says `unit_type: "token"`
			// and the page renders the M-magnitude from it.
			UnitType: stringField(record, "unit_type"),
			Meter:    stringField(record, "meter"),
		}
		bucket.Total = nonNegativeInt64(record["total_units"])
		bucket.Used = nonNegativeInt64(record["used_units"])
		bucket.Remaining = nonNegativeInt64(record["remaining_units"])
		bucket.Available = nonNegativeInt64(record["available_units"])
		bucket.ExpiresAt = nonNegativeInt64(record["expires_at"])
		result.Buckets = append(result.Buckets, bucket)
	}
	for _, bucket := range result.Buckets {
		// Prefer `available_units` when the server publishes it, else `remaining_units`.
		if bucket.Available > 0 {
			result.Remaining += bucket.Available
		} else {
			result.Remaining += bucket.Remaining
		}
		result.Total += bucket.Total
		if bucket.ExpiresAt > 0 && (result.ExpiresAt == 0 || bucket.ExpiresAt < result.ExpiresAt) {
			result.ExpiresAt = bucket.ExpiresAt
		}
	}
	if len(result.Buckets) > 0 {
		result.PlanName = result.Buckets[0].ShowName
	}
	return result
}

// reportActivation posts the two activity events.
//
// Idempotent by construction — the server de-duplicates on `device_mid` plus the
// calendar day — so it is safe to call before every preview. A failure is
// swallowed: it must not block the query that follows, and the next call retries.
func reportActivation(h *abiboot.Host, credential *Credential, cfg Config) {
	appVersion := credential.appVersionOrDefault(cfg)
	for _, event := range []string{"app_launch", "app_daily_active"} {
		body, errEncode := reencodeBody(map[string]any{
			"event":       event,
			"device_mid":  credential.DeviceMid,
			"platform":    cfg.Platform,
			"app_version": appVersion,
		})
		if errEncode != nil {
			continue
		}
		headers := buildHeaders(credential, cfg, headerOptions{JSON: true, Accept: "application/json"})
		// Neither `Authorization` nor a response check: the reference posts these
		// and ignores the answer entirely.
		_, _ = hostRequest(h, http.MethodPost, Origin+EventReportPath, headers, body)
	}
}

// fetchClaimablePlans reads `GET /zcode-plan/billing/preview`.
//
// ⚠ This endpoint does NOT need `Authorization`; it needs `X-Device-Mid`.
func fetchClaimablePlans(h *abiboot.Host, credential *Credential, cfg Config) ([]claimablePlan, error) {
	appVersion := credential.appVersionOrDefault(cfg)
	rawURL := Origin + PreviewPath +
		"?app_version=" + urlQueryEscape(appVersion) + "&platform=" + urlQueryEscape(cfg.Platform)
	headers := buildHeaders(credential, cfg, headerOptions{Accept: "application/json"})
	response, errDo := hostRequest(h, http.MethodGet, rawURL, headers, nil)
	if errDo != nil {
		return nil, transportError("preview_transport", "查询可领额度失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyUpstreamError(response.StatusCode, response.Body)
	}
	envelope := parseUpstreamEnvelope(response.Body, response.StatusCode)
	if !envelope.success() {
		return nil, classifyUpstreamError(response.StatusCode, response.Body)
	}
	data, errData := envelopeData(envelope, response.Body)
	if errData != nil {
		return nil, errData
	}
	return parseClaimablePlans(data), nil
}

// parseClaimablePlans reads the plan list and sorts it by descending priority,
// which is the order both the official client and the reference claim in.
func parseClaimablePlans(data map[string]any) []claimablePlan {
	rawPlans, _ := data["plans"].([]any)
	plans := make([]claimablePlan, 0, len(rawPlans))
	for _, item := range rawPlans {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		planID := stringField(record, "plan_id")
		if planID == "" {
			continue
		}
		priority, _ := nonNegativeInt(record["priority"])
		plans = append(plans, claimablePlan{
			PlanID:   planID,
			Priority: priority,
			Name:     stringField(record, "name"),
		})
	}
	// Insertion sort: the list is one or two entries long, so this is both the
	// simplest and the fastest option, and it keeps the sort stable.
	for i := 1; i < len(plans); i++ {
		for j := i; j > 0 && plans[j].Priority > plans[j-1].Priority; j-- {
			plans[j], plans[j-1] = plans[j-1], plans[j]
		}
	}
	return plans
}

// claimPlan posts one `POST /zcode-plan/billing/claim`.
//
// ## ⚠ The captcha header is deliberately NOT sent
//
// The claim endpoint is the ONE place the official client's bundle still attaches
// an Aliyun captcha. This plugin does not mint captchas — inference was measured
// to need none, and the browser machinery that produced them is obsolete — so the
// request goes out without the header and a server that demands one produces a
// clear `3007` message instead of a silent failure. If that ever happens, the
// fix is a captcha provider, not a retry loop.
//
// ## Business codes
//
//	0     claimed
//	1003  already claimed — IDEMPOTENT SUCCESS, not an error
//	1001  plan does not exist
//	1002  the campaign ended
//	1004  not eligible
//	1005  all slots taken
//	3007  captcha rejected
//	401   not logged in
func claimPlan(h *abiboot.Host, credential *Credential, cfg Config, planID string) claimOutcome {
	outcome := claimOutcome{PlanID: planID}
	body, errEncode := reencodeBody(map[string]any{"plan_id": planID})
	if errEncode != nil {
		outcome.Message = errEncode.Error()
		return outcome
	}
	headers := buildHeaders(credential, cfg, headerOptions{JSON: true, Authorization: true, Accept: "application/json"})
	response, errDo := hostRequest(h, http.MethodPost, Origin+ClaimPath, headers, body)
	if errDo != nil {
		outcome.Message = "领取请求失败：" + errDo.Error()
		return outcome
	}
	outcome.HTTPStatus = response.StatusCode
	envelope := parseUpstreamEnvelope(response.Body, response.StatusCode)
	if envelope.Code.present {
		outcome.Code = envelope.Code.value
	}
	outcome.Message = envelope.message("")
	// 1003 = already claimed. Treating it as a failure makes a scheduled check-in
	// report a false error on every subsequent run.
	outcome.AlreadyClaimed = outcome.Code == codeAlreadyClaimed
	outcome.OK = response.StatusCode >= 200 && response.StatusCode < 300 &&
		(outcome.Code == codeClaimSuccess || outcome.AlreadyClaimed)
	if !outcome.OK && outcome.Message == "" {
		outcome.Message = "HTTP " + strconv.Itoa(response.StatusCode)
	}
	return outcome
}

// claimFailureText turns a business code into an actionable sentence.
func claimFailureText(code int, fallback string) string {
	switch code {
	case codePlanGone:
		return "活动不存在（1001）"
	case codePlanEnded:
		return "活动已结束（1002）"
	case codeNotEligible:
		return "账号不符合领取条件（1004）"
	case codePlanSoldOut:
		return "名额已用完（1005）"
	case codeCaptchaFailed:
		return "服务端要求人机验证（3007）：ZCode 的领取端点需要阿里云验证码令牌，而本插件不产出验证码，" +
			"因此一键签到无法完成——重试不会有不同结果。推理通道不受影响（实测无需验证码）。" +
			"请在 ZCode 官方客户端或网页里领取当日额度。"
	default:
		if strings.TrimSpace(fallback) != "" {
			return fallback
		}
		return "领取失败（code " + strconv.Itoa(code) + "）"
	}
}

// checkinStatus is the provider-neutral check-in summary.
type checkinStatus struct {
	// Active is always true once a credential exists. It must NOT be derived from
	// "is the claim list non-empty": the empty-list branch would then report
	// "the campaign is not running" where the truth is "nothing left today".
	Active bool
	// TodayCheckedIn is inferred from "is there anything claimable", because ZCode
	// has no separate "already checked in today" endpoint.
	TodayCheckedIn bool
	ActivityName   string
	Claimable      []claimablePlan
	// Note explains an unusual state instead of leaving the page guessing.
	Note string
}

// fetchCheckinStatus reports what the daily claim would do right now.
//
// The activation report runs FIRST: without it the preview is empty every day,
// which is the measured false negative this ordering exists to avoid.
func fetchCheckinStatus(h *abiboot.Host, credential *Credential, cfg Config) (checkinStatus, error) {
	reportActivation(h, credential, cfg)
	plans, errPlans := fetchClaimablePlans(h, credential, cfg)
	if errPlans != nil {
		return checkinStatus{Active: true}, errPlans
	}
	status := checkinStatus{
		Active:       true,
		ActivityName: "ZCode Start Plan 每日额度",
		Claimable:    plans,
		// No plan left can mean "already claimed" or "the server granted
		// nothing today"; the two are indistinguishable from here, and the user's
		// action in both cases is "come back tomorrow". The note says so rather
		// than asserting one of them.
		TodayCheckedIn: len(plans) == 0,
	}
	if len(plans) == 0 {
		status.Note = "当前没有可领额度：可能今天已经领过，也可能服务端今天没有派发（ZCode 按自然日结算，" +
			"且只有补过 app_launch / app_daily_active 活跃上报后 preview 才会下发 plan —— 本插件已自动补报）"
	}
	return status, nil
}

// claimDaily runs the whole check-in: report, preview, then claim every plan.
//
// The captcha argument the reference threads through here is deliberately absent:
// this plugin has no captcha provider, and the claim endpoint is called without
// the header on purpose.
func claimDaily(h *abiboot.Host, credential *Credential, cfg Config) ([]claimOutcome, error) {
	reportActivation(h, credential, cfg)
	plans, errPlans := fetchClaimablePlans(h, credential, cfg)
	if errPlans != nil {
		return nil, errPlans
	}
	if len(plans) == 0 {
		return []claimOutcome{{
			OK:             true,
			AlreadyClaimed: true,
			Message:        "今日暂无可领额度（服务端按自然日刷新）",
		}}, nil
	}
	outcomes := make([]claimOutcome, 0, len(plans))
	for _, plan := range plans {
		outcome := claimPlan(h, credential, cfg, plan.PlanID)
		if !outcome.OK {
			outcome.Message = claimFailureText(outcome.Code, outcome.Message)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the provider and the reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        DisplayName,
		// The daily allowance resets on the server's own schedule; there is no
		// reset endpoint to call.
		SupportsReset: false,
	}, nil
}

// handleQuotaFetch reports the current balance as normalized quota.
func handleQuotaFetch(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.QuotaFetchRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := credentialForRequest(h, request.AuthIndex, request.StorageJSON)
	if errCredential != nil {
		return nil, errCredential
	}
	cfg := settings()
	balance, errBalance := fetchBalance(h, credential, cfg)
	if errBalance != nil {
		return nil, errBalance
	}

	response := pluginapi.QuotaFetchResponse{}
	if balance.Enterprise {
		response.Summary = []pluginapi.QuotaMetric{{
			Key:   "balance",
			Label: "额度",
			// Enterprise accounts publish no numbers; saying 0 would read as
			// "used up", so the metric reports the shape instead.
			Value:  0,
			Unit:   "企业版",
			Format: "text",
		}}
		return response, nil
	}

	// ⚠ The unit is rendered explicitly. Upstream's `unit_type` is `token`, and
	// the M-magnitude is what makes the number readable: `94.54M tokens` rather
	// than a bare `94539275` that looks like 94 million credits.
	unit := "tokens"
	if len(balance.Buckets) > 0 && balance.Buckets[0].UnitType != "" {
		unit = balance.Buckets[0].UnitType + "s"
	}
	response.Summary = append(response.Summary,
		pluginapi.QuotaMetric{
			Key:    "remaining",
			Label:  "剩余额度",
			Value:  float64(balance.Remaining),
			Unit:   formatTokenMagnitude(balance.Remaining) + " " + unit,
			Format: "number",
		},
		pluginapi.QuotaMetric{
			Key:    "total",
			Label:  "总额度",
			Value:  float64(balance.Total),
			Unit:   formatTokenMagnitude(balance.Total) + " " + unit,
			Format: "number",
		},
	)

	if balance.PlanName != "" || balance.ExpiresAt > 0 {
		response.Subscription = &pluginapi.QuotaSubscription{Plan: balance.PlanName}
	}
	// One bucket per model, so the management UI can show where the quota went.
	for _, bucket := range balance.Buckets {
		label := bucket.ShowName
		if label == "" {
			label = bucket.PlanID
		}
		fraction := 0.0
		switch {
		case bucket.Total > 0:
			remaining := bucket.Remaining
			if bucket.Available > 0 {
				remaining = bucket.Available
			}
			fraction = float64(remaining) / float64(bucket.Total)
		case bucket.Remaining > 0:
			fraction = 1
		}
		bucketLabel := label
		if bucket.UnitType != "" {
			bucketLabel += "（" + bucket.UnitType + "）"
		}
		response.Groups = append(response.Groups, pluginapi.QuotaGroup{
			DisplayName: "ZCode 额度",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            bucketLabel,
				RemainingFraction: clampFraction(fraction),
				ResetTime:         unixSecondsToRFC3339(bucket.ExpiresAt),
				Description: "已用 " + formatTokenMagnitude(bucket.Used) + " / " +
					formatTokenMagnitude(bucket.Total) + " " + unit,
			}},
		})
	}
	return response, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{
		Success: false,
		Message: "ZCode 的每日额度由服务端按自然日重置，没有重置接口",
	}, nil
}

// credentialForRequest resolves the credential a quota call acts on.
//
// The host may hand the storage inline or only an auth index; both spellings are
// accepted so the same handler serves the management page and the host.
func credentialForRequest(h *abiboot.Host, authIndex string, storage []byte) (*Credential, error) {
	if len(strings.TrimSpace(string(storage))) > 0 {
		return ParseCredential(storage)
	}
	if h == nil || strings.TrimSpace(authIndex) == "" {
		return nil, credentialError("missing_credential", "没有可用的 ZCode 凭据：请先登录一个账号")
	}
	auth, errGet := h.GetAuth(authIndex)
	if errGet != nil {
		return nil, errGet
	}
	return ParseCredential(auth.JSON)
}

// clampFraction keeps a fraction inside [0,1]; a negative remaining balance
// happens after a metering rollback and would otherwise render as a negative bar.
func clampFraction(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

// formatTokenMagnitude renders a token count in the M/B magnitude the reference
// settled on (`94.54M`), which is what makes a nine-digit number readable.
func formatTokenMagnitude(value int64) string {
	switch {
	case value >= 1_000_000_000:
		return trimTrailingZeros(fmt.Sprintf("%.2f", float64(value)/1_000_000_000)) + "B"
	case value >= 1_000_000:
		return trimTrailingZeros(fmt.Sprintf("%.2f", float64(value)/1_000_000)) + "M"
	case value >= 1_000:
		return trimTrailingZeros(fmt.Sprintf("%.1f", float64(value)/1_000)) + "K"
	default:
		return strconv.FormatInt(value, 10)
	}
}

// trimTrailingZeros removes a trailing `.00` / `.50` tail so `94.50M` reads as
// `94.5M` and `100.00M` as `100M`.
func trimTrailingZeros(value string) string {
	if !strings.Contains(value, ".") {
		return value
	}
	value = strings.TrimRight(value, "0")
	return strings.TrimRight(value, ".")
}
