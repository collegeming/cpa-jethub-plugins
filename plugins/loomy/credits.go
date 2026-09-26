package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Points: two pools, one read endpoint and one idempotent write endpoint.
//
//	GET  /points/records?pageNo=1&pageSize=1&recordType=all   read-only balance
//	POST /points/first-login   body {}                        daily grant, WRITE
//
// Semantics (`loomy-credits.ts:6-28`):
//
//   - permanent pool `balance` = registration bonus + onboarding rewards;
//   - daily pool `dailyBalance` = dailyQuota - dailyConsumed, granted once per
//     business day and NOT replenished after consumption;
//   - `availableBalance` is the sum of both.
//
// ⚠️ `first-login` is a WRITE endpoint: calling it from a read path such as
// opening the panel silently triggers check-in (`loomy-credits.ts:117-119`,
// trap #12). The read path is `records`, always.
//
// ⚠️ `dailyQuota` exists ONLY in the first-login response
// (`loomy-credits.ts:27-28`, trap #13). It must never be hard-coded to 5000, and
// a failed read must never be rendered as a 0 balance (trap #11).

// dailyQuotaDescription is the mandated wording: the daily benefit is a RESET of
// the daily pool, not a "+5000 points" award (`loomy-credits.ts:50,198-200`).
const dailyQuotaDescription = "每日赠送额度（每日刷新，不叠加）"

// pointsSnapshot is the read-only balance view.
type pointsSnapshot struct {
	// Balance is the permanent pool. HasBalance distinguishes "0 points" from
	// "the server did not answer", which the page must show differently.
	Balance    float64
	HasBalance bool
	// DailyBalance is the remaining daily grant (defaults to 0 when absent).
	DailyBalance float64
	// Available is `availableBalance`, falling back to balance+dailyBalance.
	Available float64
	// DailyQuota/DailyConsumed only exist after a check-in.
	DailyQuota    *float64
	DailyConsumed *float64
	// DailyCycleDate is the business day of the daily quota, `YYYY-MM-DD`.
	DailyCycleDate string
}

// pointsRecordsQuery is the fixed query of the read endpoint
// (`loomy-credits.ts:128-132`).
func pointsRecordsQuery() string {
	values := url.Values{}
	values.Set("pageNo", "1")
	values.Set("pageSize", "1")
	values.Set("recordType", "all")
	return values.Encode()
}

// numericField reads an optional number from a decoded JSON object, reporting
// whether a number was actually present.
func numericField(source map[string]any, key string) (float64, bool) {
	value, present := source[key]
	if !present {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case json.Number:
		parsed, errParse := typed.Float64()
		if errParse != nil {
			return 0, false
		}
		return parsed, true
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if errParse != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// fetchPoints reads the balance.
//
// A nil snapshot with a nil error means "the server answered without a usable
// `balance`": that is NOT zero and must not be displayed as zero
// (`loomy-credits.ts:135-151`). A read failure returns an error carrying either
// the transport failure or `HTTP_<status>`.
func fetchPoints(h *abiboot.Host, credential *Credential, cfg Config) (*pointsSnapshot, error) {
	rawURL := APIBase + PointsRecordsPath + "?" + pointsRecordsQuery()
	response, errDo := hostRequest(h, http.MethodGet, rawURL, businessHeaders(credential, false), nil, cfg)
	if errDo != nil {
		return nil, transportError("points_transport", "查询 Loomy 积分失败（NETWORK）：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, transportError("points_status", "查询 Loomy 积分失败（HTTP_%d）：%s",
			response.StatusCode, truncate(string(response.Body), 200))
	}
	parsed, errEnvelope := parseEnvelope(response.Body)
	if errEnvelope != nil {
		return nil, errEnvelope
	}
	if !parsed.success() {
		return nil, envelopeFailure("查询 Loomy 积分", parsed)
	}
	var data map[string]any
	if errDecode := parsed.decodeData(&data); errDecode != nil {
		return nil, errDecode
	}
	balance, hasBalance := numericField(data, "balance")
	if !hasBalance {
		// No fabricated 0: the caller must render "unknown".
		return nil, nil
	}
	snapshot := &pointsSnapshot{Balance: balance, HasBalance: true}
	if daily, ok := numericField(data, "dailyBalance"); ok {
		snapshot.DailyBalance = daily
	}
	if available, ok := numericField(data, "availableBalance"); ok {
		snapshot.Available = available
	} else {
		snapshot.Available = snapshot.Balance + snapshot.DailyBalance
	}
	if quota, ok := numericField(data, "dailyQuota"); ok {
		snapshot.DailyQuota = &quota
	}
	if consumed, ok := numericField(data, "dailyConsumed"); ok {
		snapshot.DailyConsumed = &consumed
	}
	if date, ok := data["dailyCycleDate"].(string); ok {
		snapshot.DailyCycleDate = strings.TrimSpace(date)
	}
	return snapshot, nil
}

// grantOutcome is the result of one daily-grant initialisation attempt.
type grantOutcome struct {
	// Status is `claimed`, `already-claimed` or `failed`.
	Status  string
	Message string
	Credit  float64
	// Snapshot carries whatever the server returned, when it returned numbers.
	Snapshot *pointsSnapshot
}

// claimDailyGrant POSTs `/points/first-login`.
//
// The idempotency key is the RESPONSE's `alreadyProcessed`, never the HTTP status
// and never `code`: a repeat still answers HTTP 200
// (`loomy-credits.ts:202-204`, trap in §8.3). A repeat is deliberately reported
// as `already-claimed`, not `claimed`.
//
// This function never returns an error: one bad account must not interrupt a
// batch (`loomy-credits.ts:206,217-221`).
func claimDailyGrant(h *abiboot.Host, credential *Credential, cfg Config) grantOutcome {
	body := []byte("{}")
	response, errDo := hostRequest(h, http.MethodPost, APIBase+PointsFirstLoginPath, businessHeaders(credential, true), body, cfg)
	if errDo != nil {
		return grantOutcome{Status: "failed", Message: "初始化每日额度失败：" + errDo.Error()}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return grantOutcome{Status: "failed", Message: fmt.Sprintf("初始化每日额度失败（HTTP %d）：%s",
			response.StatusCode, truncate(string(response.Body), 200))}
	}
	parsed, errEnvelope := parseEnvelope(response.Body)
	if errEnvelope != nil {
		return grantOutcome{Status: "failed", Message: "初始化每日额度失败：" + errEnvelope.Error()}
	}
	if !parsed.success() {
		return grantOutcome{Status: "failed", Message: "初始化每日额度失败：" + parsed.text()}
	}
	var data map[string]any
	if errDecode := parsed.decodeData(&data); errDecode != nil {
		return grantOutcome{Status: "failed", Message: "初始化每日额度失败：" + errDecode.Error()}
	}

	snapshot := &pointsSnapshot{}
	if permanent, ok := numericField(data, "permanentBalance"); ok {
		snapshot.Balance = permanent
		snapshot.HasBalance = true
	} else if current, ok := numericField(data, "currentBalance"); ok {
		snapshot.Balance = current
		snapshot.HasBalance = true
	}
	if daily, ok := numericField(data, "dailyBalance"); ok {
		snapshot.DailyBalance = daily
	}
	if quota, ok := numericField(data, "dailyQuota"); ok {
		snapshot.DailyQuota = &quota
	}
	if consumed, ok := numericField(data, "dailyConsumed"); ok {
		snapshot.DailyConsumed = &consumed
	}
	if date, ok := data["dailyCycleDate"].(string); ok {
		snapshot.DailyCycleDate = strings.TrimSpace(date)
	}
	snapshot.Available = snapshot.Balance + snapshot.DailyBalance

	alreadyProcessed, _ := data["alreadyProcessed"].(bool)
	if alreadyProcessed {
		message := "今日额度已初始化"
		if snapshot.DailyQuota != nil {
			message = fmt.Sprintf("今日额度已初始化（每日 %s/%s）",
				trimAmount(snapshot.DailyBalance), trimAmount(*snapshot.DailyQuota))
		}
		return grantOutcome{Status: "already-claimed", Message: message, Snapshot: snapshot}
	}
	credit := 0.0
	if snapshot.DailyQuota != nil && snapshot.DailyConsumed != nil {
		credit = math.Max(0, *snapshot.DailyQuota-*snapshot.DailyConsumed)
	} else if snapshot.DailyQuota != nil {
		credit = *snapshot.DailyQuota
	}
	return grantOutcome{
		Status:   "claimed",
		Message:  "每日赠送额度已刷新",
		Credit:   credit,
		Snapshot: snapshot,
	}
}

// ensureDailyGrant runs the best-effort initialisation the official client makes
// immediately after every login path (`loomy-credits.ts:17-21`,
// `loomy-auth.ts:241-249`). A failure is logged and never fails the login.
func ensureDailyGrant(h *abiboot.Host, credential *Credential, cfg Config) {
	if h == nil || credential == nil {
		return
	}
	outcome := claimDailyGrant(h, credential, cfg)
	if outcome.Status == "failed" {
		h.Log("warn", "Loomy 每日额度初始化失败", map[string]any{
			"provider": ProviderKey,
			"account":  credential.displayLabel(),
			"message":  outcome.Message,
		})
	}
}

// probeCredential checks whether a session is still alive.
//
// `refresh` in this plugin is a PROBE: there is no refresh endpoint and no
// refresh token (`loomy.ts:195-205`). Classification follows
// `loomy-auth.ts:350-369` exactly:
//
//   - envelope ok                          -> credential valid;
//   - envelope `100002`                    -> dead credential (terminal);
//   - any other envelope failure           -> plain retryable error;
//   - TRANSPORT failure                    -> plain retryable error, explicitly
//     NOT expiry, so network jitter never forces a re-login (trap #10);
//   - non-JSON / non-2xx response          -> plain retryable error carrying the
//     HTTP status.
func probeCredential(h *abiboot.Host, credential *Credential, cfg Config) error {
	rawURL := APIBase + PointsRecordsPath + "?" + pointsRecordsQuery()
	response, errDo := hostRequest(h, http.MethodGet, rawURL, businessHeaders(credential, false), nil, cfg)
	if errDo != nil {
		return transportError("probe_transport", "探测 Loomy 凭据失败（网络问题，不代表凭据失效）：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return transportError("probe_status", "探测 Loomy 凭据失败（HTTP_%d）：%s",
			response.StatusCode, truncate(string(response.Body), 200))
	}
	parsed, errEnvelope := parseEnvelope(response.Body)
	if errEnvelope != nil {
		return errEnvelope
	}
	if parsed.success() {
		return nil
	}
	if parsed.isAuthExpired() {
		return credentialError("credential_expired", "Loomy 凭证已失效，请重新登录%s", credentialAdvice())
	}
	return transportError("probe_business_error", "探测 Loomy 凭据失败（code %s）：%s", parsed.code(), parsed.text())
}

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the supported provider and reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        "Loomy 积分（永久 + 每日赠送）",
		// The daily grant is initialised by the server; nothing can be reset.
		SupportsReset: false,
	}, nil
}

// handleQuotaFetch reports the two point pools separately, which is an explicit
// product requirement (`README.md:1471-1474`).
//
// A failed read is an error, never a zero balance (trap #11).
func handleQuotaFetch(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.QuotaFetchRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		return nil, errCredential
	}
	snapshot, errPoints := fetchPoints(h, credential, settings())
	if errPoints != nil {
		return nil, errPoints
	}
	if snapshot == nil {
		return nil, transportError("points_unavailable", "Loomy 未返回 balance 字段，无法给出积分数字")
	}
	summary := []pluginapi.QuotaMetric{
		{Key: "permanent_points", Label: "永久积分", Value: snapshot.Balance, Unit: "积分", Format: "number"},
		{Key: "daily_points", Label: "每日赠送", Value: snapshot.DailyBalance, Unit: "积分", Format: "number"},
		{Key: "available_points", Label: "可用合计", Value: snapshot.Available, Unit: "积分", Format: "number"},
	}
	if snapshot.DailyQuota != nil {
		summary = append(summary, pluginapi.QuotaMetric{
			Key: "daily_quota", Label: dailyQuotaDescription, Value: *snapshot.DailyQuota, Unit: "积分", Format: "number",
		})
	}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{
		Success: false,
		Message: "Loomy 不支持重置积分；每日赠送额度由服务端在首次登录时初始化",
	}, nil
}

// trimAmount renders a point amount without a trailing `.0`.
func trimAmount(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
