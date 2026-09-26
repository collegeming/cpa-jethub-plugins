package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Daily check-in, credit balance and the quota provider routes. Ported from
// trae-credits.ts:40-380.
//
// Three properties are load-bearing and are encoded below:
//
//   - the check-in request must carry the **full** client header set, not the
//     short Ug set (docs/agents/trae.md:422-448);
//   - the device identity is derived from the uid, so a 9074 "too many users"
//     answer is reported as-is instead of rotating a device id;
//   - the claim response carries **no** credit amount — it is
//     `{"code":0,"message":"success"}` — so the amount is read back from the
//     status endpoint, and a claim is only considered taken when status says
//     `checked_in` (docs/agents/trae.md:452-465).

// trafficBusinessCode is the "too many users, try later" business code.
const trafficBusinessCode = 9074

// traeCheckinStatus is the check-in status payload.
type traeCheckinStatus struct {
	Active      bool
	CheckedIn   bool
	Credits     int64
	StreakDays  int64
	TotalCredit int64
}

// traeClaimOutcome is the result of one check-in attempt.
type traeClaimOutcome struct {
	Status      string // claimed | already-claimed | failed | inactive
	Message     string
	Credit      int64
	StreakDays  int64
	ErrorType   string
	CooldownSec int
}

// traeCreditPackage is one entitlement package.
type traeCreditPackage struct {
	Name      string
	Remaining float64
	Total     float64
	Used      float64
}

// traeCreditBalance is the summed credit balance.
type traeCreditBalance struct {
	Total    float64
	Packages []traeCreditPackage
}

// accountQuota is ONE TRAE account's own credit and check-in state.
//
// A field is nil when the provider did not return it, and the matching error
// says why. A failed read must render as 未知, never as 0 — 0 would read like
// "the credits are spent".
type accountQuota struct {
	// Entry is the host credential this quota belongs to.
	Entry pluginapi.HostAuthFileEntry
	// Credential is nil when the credential itself could not be read;
	// CredentialErr then explains why and no upstream call was made.
	Credential    *Credential
	CredentialErr error
	// Balance is the account's own credit aggregate, or nil with BalanceErr set.
	Balance    *traeCreditBalance
	BalanceErr error
	// Checkin is the account's own daily check-in state, or nil with CheckinErr.
	Checkin    *traeCheckinStatus
	CheckinErr error
}

// collectAccountQuotas reads the credits and check-in state of every account, in
// host order, one account at a time.
//
// Sequential on purpose: each account costs two upstream requests, and the
// reference panel queries balances one account at a time for the same reason
// (parallel balance reads are what the provider's risk control reacts to). One
// account's failure never stops the sweep and never hides another's figures.
//
// The catalog is deliberately NOT part of this sweep: it is a property of the
// credential and costs one more request per account, so it stays on the selected
// account's own entry (see statusJSON).
func collectAccountQuotas(h *abiboot.Host, accounts []pluginapi.HostAuthFileEntry, cfg Config) []accountQuota {
	out := make([]accountQuota, 0, len(accounts))
	for _, entry := range accounts {
		quota := accountQuota{Entry: entry}
		credential, errCredential := credentialOf(h, entry)
		if errCredential != nil {
			quota.CredentialErr = errCredential
			out = append(out, quota)
			continue
		}
		quota.Credential = credential
		if balance, errBalance := fetchCreditBalance(h, credential, cfg); errBalance != nil {
			quota.BalanceErr = errBalance
		} else {
			quota.Balance = balance
		}
		if status, errStatus := fetchCheckinStatus(h, credential, cfg); errStatus != nil {
			quota.CheckinErr = errStatus
		} else {
			quota.Checkin = status
		}
		out = append(out, quota)
	}
	return out
}

// quotaOf picks one account's quota out of a collected list.
func quotaOf(quotas []accountQuota, entry pluginapi.HostAuthFileEntry) (accountQuota, bool) {
	for _, quota := range quotas {
		if quota.Entry.AuthIndex == entry.AuthIndex && quota.Entry.Name == entry.Name {
			return quota, true
		}
	}
	return accountQuota{}, false
}

// quotaJSON renders one account's own figures.
//
// The keys are exactly the ones the selected account already publishes at the
// top level (credits / daily_checkin), so a consumer that reads the flat
// document can read one entry of `accounts` with the same code. A failed read
// emits the reason and NO number at all.
func quotaJSON(quota accountQuota) map[string]any {
	item := map[string]any{
		"auth_index": quota.Entry.AuthIndex,
		"name":       quota.Entry.Name,
		"status":     statusText(quota.Entry),
	}
	if label := strings.TrimSpace(quota.Entry.Label); label != "" {
		item["label"] = label
	}
	if quota.CredentialErr != nil {
		item["error"] = quota.CredentialErr.Error()
		return item
	}
	item["uid"] = quota.Credential.UID
	item["nickname"] = quota.Credential.Nickname
	item["region"] = quota.Credential.Region
	item["refreshable"] = quota.Credential.Refreshable()
	item["expired"] = quota.Credential.Expired()
	if expiresAt, ok := quota.Credential.ExpiresAtMS(); ok {
		item["expires_at_ms"] = expiresAt
	}
	if quota.Balance != nil {
		item["credits"] = quota.Balance.Total
	}
	if quota.BalanceErr != nil {
		item["credit_error"] = quota.BalanceErr.Error()
	}
	if quota.Checkin != nil {
		item["daily_checkin"] = map[string]any{
			"checked_in":  quota.Checkin.CheckedIn,
			"credits":     quota.Checkin.Credits,
			"streak_days": quota.Checkin.StreakDays,
		}
	}
	if quota.CheckinErr != nil {
		item["checkin_error"] = quota.CheckinErr.Error()
	}
	return item
}

// quotaListJSON renders every account's figures, in host order.
func quotaListJSON(quotas []accountQuota) []map[string]any {
	out := make([]map[string]any, 0, len(quotas))
	for _, quota := range quotas {
		out = append(out, quotaJSON(quota))
	}
	return out
}

// decodeResponseBody undoes the transfer encoding of a response. The check-in
// headers ask for `gzip, deflate` verbatim from the reference client, and an
// explicit Accept-Encoding stops the transport from decompressing for us.
func decodeResponseBody(response *pluginapi.HTTPResponse) []byte {
	if response == nil || len(response.Body) == 0 {
		return nil
	}
	encoding := strings.ToLower(strings.TrimSpace(response.Headers.Get("Content-Encoding")))
	switch {
	case strings.Contains(encoding, "gzip"):
		reader, errGzip := gzip.NewReader(bytes.NewReader(response.Body))
		if errGzip != nil {
			return response.Body
		}
		defer reader.Close()
		if decoded, errRead := io.ReadAll(reader); errRead == nil {
			return decoded
		}
	case strings.Contains(encoding, "deflate"):
		reader := flate.NewReader(bytes.NewReader(response.Body))
		defer reader.Close()
		if decoded, errRead := io.ReadAll(reader); errRead == nil {
			return decoded
		}
	}
	return response.Body
}

// postCheckinJSON performs one check-in/balance call with the full client header
// set. Transport failures are distinguished from business failures: the returned
// httpStatus is 0 only for the former, and only those are worth retrying.
func postCheckinJSON(h *abiboot.Host, credential *Credential, cfg Config, path, body string) (map[string]any, int, error) {
	uid := strings.TrimSpace(credential.UID)
	if uid == "" {
		return nil, 0, abiboot.Errorf("missing_uid", "缺少 uid，无法构造签到设备身份")
	}
	product := productFor(cfg.Region)
	headers, errHeaders := checkinHeaders(credential, product, uid)
	if errHeaders != nil {
		return nil, 0, abiboot.Errorf("random_source", "构造签到请求头失败: %v", errHeaders)
	}
	response, errDo := hostDo(h, http.MethodPost, product.UGHost+path, headers, []byte(body), cfg.RequestTimeoutMS)
	if errDo != nil {
		return nil, 0, errDo
	}
	status := response.StatusCode
	payload := decodeResponseBody(response)
	if status < 200 || status >= 300 {
		return nil, status, abiboot.Errorf("checkin_http", "HTTP %d: %s", status, truncate(collapseWhitespace(string(payload)), 120))
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(payload, &decoded); errUnmarshal != nil {
		return nil, status, abiboot.Errorf("checkin_decode", "非 JSON 响应: %s", truncate(collapseWhitespace(string(payload)), 120))
	}
	return decoded, status, nil
}

// collapseWhitespace flattens a response body for a one-line error message.
func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// readClaimCode reads the business code, accepting the numeric and the string
// form. A string `"9074"` must not be mistaken for success
// (trae-credits.ts:127-132). A missing code means 0; anything unrecognisable
// means -1.
func readClaimCode(body map[string]any) int {
	value, ok := body["code"]
	if !ok || value == nil {
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return parsed
		}
	}
	return -1
}

// fetchCheckinStatus queries the check-in status endpoint (trae-credits.ts:186-209).
func fetchCheckinStatus(h *abiboot.Host, credential *Credential, cfg Config) (*traeCheckinStatus, error) {
	body, _, errDo := postCheckinJSON(h, credential, cfg, CheckinStatusPath, "{}")
	if errDo != nil {
		return nil, errDo
	}
	if readClaimCode(body) != 0 {
		return nil, abiboot.Errorf("checkin_status", "签到状态查询被拒绝（code=%d）", readClaimCode(body))
	}
	credits, _ := readNumberField(body, "credits")
	streak, _ := readNumberField(body, "streak_days")
	total, _ := readNumberField(body, "total_credits")
	active, _ := readBooleanField(body, "enable")
	checkedIn, _ := readBooleanField(body, "checked_in")
	return &traeCheckinStatus{
		Active:      active,
		CheckedIn:   checkedIn,
		Credits:     int64(credits),
		StreakDays:  int64(streak),
		TotalCredit: int64(total),
	}, nil
}

// fetchCreditBalance sums the entitlement packages (trae-credits.ts:316-368).
// The balance body must carry `require_usage: true`, otherwise the response has
// no `usage` block and the remaining amount silently equals the limit.
func fetchCreditBalance(h *abiboot.Host, credential *Credential, cfg Config) (*traeCreditBalance, error) {
	requestBody, _ := json.Marshal(map[string]any{"require_usage": true, "req_source": 2})
	body, _, errDo := postCheckinJSON(h, credential, cfg, EntUsagePath, string(requestBody))
	if errDo != nil {
		return nil, errDo
	}
	packList, ok := asSlice(body["user_entitlement_pack_list"])
	if !ok || len(packList) == 0 {
		return nil, abiboot.Errorf("credit_balance", "响应中没有额度包")
	}
	balance := &traeCreditBalance{Packages: []traeCreditPackage{}}
	for _, raw := range packList {
		entry, ok := asMap(raw)
		if !ok {
			continue
		}
		base, ok := asMap(entry["entitlement_base_info"])
		if !ok {
			continue
		}
		quota, ok := asMap(base["quota"])
		if !ok {
			continue
		}
		limit, _ := readNumberField(quota, "credits_limit")
		if limit <= 0 {
			continue
		}
		used := 0.0
		if usage, ok := asMap(entry["usage"]); ok {
			used, _ = readNumberField(usage, "credits_amount")
		}
		name := readStringField(base, "name")
		if name == "" {
			name = "资源包"
		}
		pack := traeCreditPackage{Name: name, Remaining: limit - used, Total: limit, Used: used}
		balance.Packages = append(balance.Packages, pack)
		balance.Total += pack.Remaining
	}
	return balance, nil
}

// classifyCheckinError maps a check-in failure onto a type and a cooldown, per
// trae-mate's `cooldown.rs` (trae-credits.ts:74-100).
func classifyCheckinError(httpStatus, code int) (string, int) {
	switch {
	case httpStatus == http.StatusOK && code == 1005:
		return "PlanLimit", 43200
	case httpStatus == http.StatusTooManyRequests:
		return "SoftRate", 60
	case httpStatus == http.StatusUnauthorized:
		return "SessionDead", -1
	case httpStatus == http.StatusNotFound:
		return "NotFound", 60
	case httpStatus >= 500 && httpStatus < 600:
		return "Server", 600
	case httpStatus >= 400 && httpStatus < 500:
		return "Client", 600
	case code != 0:
		return "BusinessError", 300
	default:
		return "Unknown", 0
	}
}

// claimDailyCheckin performs the daily check-in.
//
// The status endpoint is consulted first: a repeated claim is idempotent
// upstream and answers exactly like a real success, so the response body alone
// cannot tell the two apart — only `checked_in` can
// (docs/agents/trae.md:459-465).
func claimDailyCheckin(h *abiboot.Host, credential *Credential, cfg Config) (*traeClaimOutcome, error) {
	if status, errStatus := fetchCheckinStatus(h, credential, cfg); errStatus == nil && status != nil {
		if status.CheckedIn {
			return &traeClaimOutcome{
				Status:     "already-claimed",
				Message:    "今日已签到",
				Credit:     status.Credits,
				StreakDays: status.StreakDays,
			}, nil
		}
		if !status.Active {
			return &traeClaimOutcome{Status: "inactive", Message: "该账号当前没有可用的签到活动"}, nil
		}
	}

	// Only transport failures are retried; a business answer is final.
	var (
		body   map[string]any
		status int
		last   error
	)
	for attempt := 0; attempt <= 3; attempt++ {
		decoded, httpStatus, errDo := postCheckinJSON(h, credential, cfg, CheckinClaimPath, "{}")
		if errDo == nil {
			body, status = decoded, httpStatus
			last = nil
			break
		}
		last = errDo
		if httpStatus > 0 {
			// Business failure: report the classification, do not retry.
			code := 0
			errorType, cooldown := classifyCheckinError(httpStatus, code)
			return &traeClaimOutcome{
				Status:      "failed",
				Message:     errDo.Error(),
				ErrorType:   errorType,
				CooldownSec: cooldown,
			}, nil
		}
		if attempt < 3 {
			time.Sleep(time.Second)
		}
	}
	if last != nil || body == nil {
		message := "签到请求失败"
		if last != nil {
			message = last.Error()
		}
		return &traeClaimOutcome{Status: "failed", Message: message, ErrorType: "Unknown"}, nil
	}

	code := readClaimCode(body)
	message := readStringField(body, "message", "msg")

	if code == trafficBusinessCode {
		if message == "" {
			message = "签到人数过多，请稍后再试"
		}
		return &traeClaimOutcome{Status: "failed", Message: message, ErrorType: "BusinessError", CooldownSec: 300}, nil
	}
	if code == 0 {
		// The claim body carries no amount; read the awarded credits back from
		// the status endpoint.
		outcome := &traeClaimOutcome{Status: "claimed", Message: "签到成功"}
		if statusAfter, errStatus := fetchCheckinStatus(h, credential, cfg); errStatus == nil && statusAfter != nil {
			outcome.Credit = statusAfter.Credits
			outcome.StreakDays = statusAfter.StreakDays
		}
		return outcome, nil
	}

	errorType, cooldown := classifyCheckinError(status, code)
	if message == "" {
		message = "签到失败（code=" + strconv.Itoa(code) + "）"
	}
	return &traeClaimOutcome{Status: "failed", Message: message, ErrorType: errorType, CooldownSec: cooldown}, nil
}

// ── quota provider routes ──

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the supported provider and that reset is not
// available.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        "TRAE 每日签到",
		SupportsReset:      false,
	}, nil
}

// handleQuotaFetch reports the credit balance and the check-in state.
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
	if balance, errBalance := fetchCreditBalance(h, credential, cfg); errBalance == nil {
		summary = append(summary,
			pluginapi.QuotaMetric{Key: "credit_remaining", Label: "剩余积分", Value: balance.Total, Unit: "credits", Format: "number"},
		)
	}
	if status, errStatus := fetchCheckinStatus(h, credential, cfg); errStatus == nil {
		checkedIn := 0.0
		if status.CheckedIn {
			checkedIn = 1
		}
		summary = append(summary,
			pluginapi.QuotaMetric{Key: "checked_in", Label: "今日已签到", Value: checkedIn, Format: "boolean"},
			pluginapi.QuotaMetric{Key: "checkin_credit", Label: "签到奖励", Value: float64(status.Credits), Unit: "credits", Format: "number"},
			pluginapi.QuotaMetric{Key: "streak_days", Label: "连续签到", Value: float64(status.StreakDays), Unit: "天", Format: "number"},
		)
	}
	if len(summary) == 0 {
		return nil, abiboot.Errorf("quota_unavailable", "无法读取 TRAE 额度与签到状态")
	}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "TRAE 不支持重置额度"}, nil
}
