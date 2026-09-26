package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Daily check-in and credit balance, ported from
// jethub-src/src/lobsterai-credits.ts:172-533.
//
// Three-step protocol:
//
//	1. GET  /api/client-activities/slot?placement=…&clientVersion=…&containerApiVersion=2&platform=win32
//	2. GET  /api/client-activities/{activityCode}/context?configRevision=…
//	3. POST /api/client-activities/{activityCode}/actions/check_in
//
// Idempotency is client-side: the claim carries an idempotencyKey (UUID v4) and
// two pre-checks run first (state.claimedToday and the actions list containing
// check_in). Both checks are needed — testing only claimedToday misses "the
// activity exists but is not claimable today".

// activitySlot is the `data` of the slot endpoint.
type activitySlot struct {
	SlotState      string
	ActivityCode   string
	ConfigRevision float64
}

// activityContext is the `data` of the context endpoint.
type activityContext struct {
	ClaimedToday bool
	Actions      []string
}

// claimOutcome is the result of one check-in attempt. The Kind values match the
// shared credits vocabulary so management UI summaries stay interchangeable.
type claimOutcome struct {
	Kind    string // claimed | already-claimed | inactive | failed
	Code    int
	Message string
	Credit  float64
	// DelayedMessage is the server-provided message, if any.
	DelayedMessage string
	CreditGranted  bool
}

// creditPackage is one entry of creditItems[].
type creditPackage struct {
	Name        string
	Unit        string
	Remaining   float64
	Total       float64
	Used        float64
	Active      bool
	ExpiredTime string
}

// creditBalance is the normalised profile summary.
type creditBalance struct {
	Total        float64
	Packages     []creditPackage
	ExpiredTotal float64
}

// accountQuota is ONE LobsterAI account's own credit and check-in state.
//
// A quota field is nil when the provider did not publish it, and the matching
// error says why. A failed read must render as 未知, never as 0 — 0 would read
// like "the credits are spent".
type accountQuota struct {
	// Entry is the host credential this quota belongs to.
	Entry pluginapi.HostAuthFileEntry
	// Credential is nil when the credential itself could not be read;
	// CredentialErr then explains why and no upstream call was made.
	Credential    *Credential
	CredentialErr error
	// Refresh is what the freshness check did to this account's credential: it
	// says whether the credential was renewed (so the figures below come from a
	// fresh token) and carries a renewal that failed.
	Refresh authrefresh.Result
	// Balance is the account's own credit aggregate, or nil with BalanceErr set.
	Balance    *creditBalance
	BalanceErr error
	// Activity is the account's own daily-activity slot. CheckinDisabled marks
	// the configured case where check-in is turned off, which is a fact and not
	// a failure; ActivityErr carries a failed read.
	Activity        *activitySlot
	ActivityErr     error
	CheckinDisabled bool
}

// collectAccountQuotas reads the credits and activity slot of every account, in
// host order, one account at a time.
//
// Sequential on purpose: each account costs two upstream requests, and the
// reference panel queries balances one account at a time for the same reason
// (parallel balance reads are what the provider's risk control reacts to). One
// account's failure never stops the sweep and never hides another's figures.
func collectAccountQuotas(h *abiboot.Host, accounts []pluginapi.HostAuthFileEntry, cfg Config, clientVersion string) []accountQuota {
	out := make([]accountQuota, 0, len(accounts))
	for _, entry := range accounts {
		quota := accountQuota{Entry: entry}
		credential, freshness, errCredential := credentialOf(h, entry)
		if errCredential != nil {
			quota.CredentialErr = errCredential
			out = append(out, quota)
			continue
		}
		quota.Credential = credential
		quota.Refresh = freshness
		if balance, errBalance := fetchCreditBalance(h, credential); errBalance != nil {
			quota.BalanceErr = errBalance
			out = append(out, quota)
			continue
		} else {
			quota.Balance = balance
		}
		if !cfg.DailyCheckin {
			quota.CheckinDisabled = true
		} else if slot, errSlot := fetchActivitySlot(h, credential, clientVersion); errSlot != nil {
			quota.ActivityErr = errSlot
		} else {
			quota.Activity = slot
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
// The keys are exactly the ones the selected account already publishes under
// `selected` (credit / credit_error / activity / activity_error), lifted to the
// entry level, so a consumer that reads the flat document can read one entry of
// `accounts` with the same code. A failed read emits the reason and NO number.
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
	item["refreshable"] = quota.Credential.Refreshable()
	if quota.Refresh.Refreshed {
		item["refreshed"] = true
	}
	if quota.Refresh.Err != nil {
		item["refresh_error"] = quota.Refresh.Err.Error()
	}
	if expiry, ok := quota.Credential.Expiry(); ok {
		item["expires_at"] = expiry.UTC().Format(time.RFC3339)
		item["expired"] = quota.Credential.Expired(0)
	}
	switch {
	case quota.BalanceErr != nil:
		item["credit_error"] = quota.BalanceErr.Error()
	case quota.Balance != nil:
		item["credit"] = map[string]any{
			"total":         quota.Balance.Total,
			"expired_total": quota.Balance.ExpiredTotal,
			"packages":      quota.Balance.Packages,
		}
	}
	switch {
	case quota.CheckinDisabled:
		item["checkin_state"] = "disabled"
	case quota.ActivityErr != nil:
		item["activity_error"] = quota.ActivityErr.Error()
	case quota.Activity != nil:
		item["activity"] = map[string]any{
			"slot_state":    quota.Activity.SlotState,
			"activity_code": quota.Activity.ActivityCode,
		}
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

// requestJSON performs an authenticated request and parses the JSON body. A
// non-JSON body is reported with its status and a short snippet instead of a
// JSON parse error, which is what the reference does and what an operator can
// actually act on (lobsterai-credits.ts:263-305).
func requestJSON(h *abiboot.Host, method, rawURL string, credential *Credential, body []byte) (map[string]any, error) {
	if h == nil {
		return nil, abiboot.Errorf("transport_unavailable", "host transport 不可用")
	}
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  method,
		URL:     rawURL,
		Headers: authHeaders(credential, "application/json"),
		Body:    body,
	})
	if errDo != nil {
		return nil, abiboot.Errorf("request_failed", "%v", errDo)
	}
	var decoded any
	if errUnmarshal := decodeJSON(response.Body, &decoded); errUnmarshal != nil {
		return nil, abiboot.Errorf("non_json_response", "%s", describeNonJSONResponse(response.StatusCode, string(response.Body)))
	}
	record := asRecord(decoded)
	if record == nil {
		return nil, abiboot.Errorf("unparsable_response", "请求失败或响应无法解析")
	}
	return record, nil
}

// describeNonJSONResponse renders a readable reason for a non-JSON body.
func describeNonJSONResponse(status int, text string) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Sprintf("凭据已失效（HTTP %d），请重新登录该账号", status)
	}
	snippet := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if len(snippet) > 80 {
		snippet = snippet[:80]
	}
	return fmt.Sprintf("服务端返回了非 JSON 响应（HTTP %d）：%s", status, snippet)
}

// slotQuery renders the three fixed slot parameters plus the client version.
func slotQuery(clientVersion string) string {
	query := url.Values{
		"placement":           {SlotPlacement},
		"clientVersion":       {clientVersion},
		"containerApiVersion": {SlotContainerAPIVersion},
		"platform":            {SlotPlatform},
	}
	return query.Encode()
}

// fetchActivitySlot queries the current activity slot. A parse or envelope
// failure is an error (distinct from "no available activity", which is a
// normal business state).
func fetchActivitySlot(h *abiboot.Host, credential *Credential, clientVersion string) (*activitySlot, error) {
	rawURL := APIBase + ActivitySlotPath + "?" + slotQuery(clientVersion)
	record, errRequest := requestJSON(h, http.MethodGet, rawURL, credential, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	envelope := parseEnvelopeValue(record)
	if !envelope.OK {
		return nil, abiboot.Errorf("slot_rejected", "活动槽位查询失败：%s", envelope.Message)
	}
	activity := asRecord(envelope.Data["activity"])
	return &activitySlot{
		SlotState:      readStringField(envelope.Data, "slotState"),
		ActivityCode:   readStringField(activity, "activityCode"),
		ConfigRevision: numberOrZero(activity, "configRevision"),
	}, nil
}

// fetchActivityContext queries the activity context.
func fetchActivityContext(h *abiboot.Host, credential *Credential, slot *activitySlot) (*activityContext, error) {
	query := url.Values{"configRevision": {formatNumber(slot.ConfigRevision)}}
	rawURL := APIBase + ActivityContextPath + "/" + url.PathEscape(slot.ActivityCode) + "/context?" + query.Encode()
	record, errRequest := requestJSON(h, http.MethodGet, rawURL, credential, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	envelope := parseEnvelopeValue(record)
	if !envelope.OK {
		return nil, abiboot.Errorf("context_rejected", "活动上下文查询失败：%s", envelope.Message)
	}
	state := asRecord(envelope.Data["state"])
	return &activityContext{
		ClaimedToday: readBoolField(state, "claimedToday"),
		Actions:      readStringArray(envelope.Data, "actions"),
	}, nil
}

// claimDailyCheckin runs the full three-step check-in. The precondition order
// keeps "normal business state" strictly separate from "real failure".
func claimDailyCheckin(h *abiboot.Host, credential *Credential, clientVersion string, now time.Time) claimOutcome {
	slot, errSlot := fetchActivitySlot(h, credential, clientVersion)
	if errSlot != nil {
		return claimOutcome{Kind: "failed", Code: -1, Message: "活动槽位查询失败：" + errSlot.Error()}
	}
	if slot.SlotState != "available" || slot.ActivityCode == "" {
		return claimOutcome{Kind: "inactive", Message: fmt.Sprintf("无可用活动（slotState=%s）", slot.SlotState)}
	}

	context, errContext := fetchActivityContext(h, credential, slot)
	if errContext != nil {
		return claimOutcome{Kind: "failed", Code: -1, Message: "活动上下文查询失败：" + errContext.Error()}
	}
	if context.ClaimedToday {
		return claimOutcome{Kind: "already-claimed", Message: "今天已签到"}
	}
	if !containsString(context.Actions, "check_in") {
		return claimOutcome{Kind: "inactive", Message: "当前不可签到"}
	}

	idempotencyKey, errKey := randomUUID()
	if errKey != nil {
		return claimOutcome{Kind: "failed", Code: -1, Message: errKey.Error()}
	}
	body, errMarshal := json.Marshal(map[string]any{
		"configRevision": slot.ConfigRevision,
		"idempotencyKey": idempotencyKey,
		"payload":        map[string]any{},
	})
	if errMarshal != nil {
		return claimOutcome{Kind: "failed", Code: -1, Message: "编码签到请求失败"}
	}
	rawURL := APIBase + ActivityContextPath + "/" + url.PathEscape(slot.ActivityCode) + "/actions/check_in"
	record, errRequest := requestJSON(h, http.MethodPost, rawURL, credential, body)
	if errRequest != nil {
		return claimOutcome{Kind: "failed", Code: -1, Message: errRequest.Error()}
	}
	envelope := parseEnvelopeValue(record)
	if !envelope.OK {
		return claimOutcome{Kind: "failed", Code: envelope.Code, Message: envelope.Message}
	}

	result := asRecord(envelope.Data["result"])
	// Credit field fallback chain: different activities/versions name it
	// creditsGranted, rewardCredits or credits.
	credit := 0.0
	granted := false
	for _, key := range []string{"creditsGranted", "rewardCredits", "credits"} {
		if value, ok := readNumberField(result, key); ok {
			credit = value
			granted = true
			break
		}
	}
	return claimOutcome{
		Kind:           "claimed",
		Message:        "签到成功",
		Credit:         credit,
		CreditGranted:  granted,
		DelayedMessage: readStringField(result, "message"),
	}
}

// fetchCreditBalance reads GET /api/user/profile-summary. It deliberately uses
// profile-summary rather than /api/user/quota, which only reports the free
// credit total and omits activity credits.
//
// nil means "could not be determined", which is strictly different from a zero
// balance: the management page must show the reason rather than 0.
func fetchCreditBalance(h *abiboot.Host, credential *Credential) (*creditBalance, error) {
	record, errRequest := requestJSON(h, http.MethodGet, APIBase+ProfileSummaryPath, credential, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	envelope := parseEnvelopeValue(record)
	if !envelope.OK {
		return nil, abiboot.Errorf("balance_rejected", "积分查询失败：%s", envelope.Message)
	}

	packages := make([]creditPackage, 0, 4)
	if items, ok := envelope.Data["creditItems"].([]any); ok {
		for _, item := range items {
			entry := asRecord(item)
			if entry == nil {
				continue
			}
			name := readStringField(entry, "type")
			if name == "" {
				name = "积分包"
			}
			expiresAt := readStringField(entry, "expiresAt")
			packages = append(packages, creditPackage{
				Name:      name,
				Unit:      "credit",
				Remaining: numberOrZero(entry, "creditsRemaining"),
				// LobsterAI only reports the remaining amount; it has no
				// "cycle total / used" concept. total stays 0 so the UI shows
				// "?" rather than a misleading 1:1 ratio.
				Total:       0,
				Used:        0,
				Active:      !isExpiredTimestamp(expiresAt, time.Now()),
				ExpiredTime: expiresAt,
			})
		}
	}

	// Negative values are clamped: metering rollbacks can make the server report
	// a negative remaining amount, and showing "-12.5 credits" is meaningless.
	total := roundCredits(math.Max(0, numberOrZero(envelope.Data, "totalCreditsRemaining")))
	if total == 0 && len(packages) == 0 {
		return nil, abiboot.Errorf("balance_unavailable", "积分接口未返回 totalCreditsRemaining 或明细")
	}
	expiredTotal := 0.0
	for _, pkg := range packages {
		if !pkg.Active {
			expiredTotal += math.Max(0, pkg.Remaining)
		}
	}
	return &creditBalance{Total: total, Packages: packages, ExpiredTotal: roundCredits(expiredTotal)}, nil
}

// isExpiredTimestamp reports whether an ISO-ish timestamp lies in the past. A
// missing or unparseable timestamp is treated as "not expired".
func isExpiredTimestamp(raw string, now time.Time) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, errParse := time.Parse(layout, trimmed); errParse == nil {
			return !now.Before(parsed)
		}
	}
	return false
}

// roundCredits keeps two decimal places; server values carry float noise such
// as 55.67000031 that becomes visible once several packages are summed.
func roundCredits(value float64) float64 {
	return math.Round(value*100) / 100
}

// numberOrZero reads a numeric field, defaulting to 0.
func numberOrZero(source map[string]any, key string) float64 {
	value, _ := readNumberField(source, key)
	return value
}

// formatNumber renders a float without a trailing .0 for URL parameters.
func formatNumber(value float64) string {
	if value == math.Trunc(value) {
		return fmt.Sprintf("%d", int64(value))
	}
	return fmt.Sprintf("%g", value)
}

// containsString reports whether the slice holds the value.
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// randomUUID renders a RFC 4122 version 4 identifier, matching the reference
// crypto.randomUUID() idempotency key.
func randomUUID() (string, error) {
	buf := make([]byte, 16)
	if _, errRead := cryptoRead(buf); errRead != nil {
		return "", abiboot.Errorf("random_generate", "generate idempotency key: %v", errRead)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the supported provider and reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        "LobsterAI 积分",
		SupportsReset:      false,
	}, nil
}

// handleQuotaFetch reports the current credit balance and check-in state.
func handleQuotaFetch(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.QuotaFetchRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	// The host asks for a quota read before the page is ever opened, so the
	// credential is renewed here as well.
	fresh, errFresh := ensureCredentialFresh(h, authrefresh.Request{
		Name:        request.AuthID,
		StorageJSON: request.StorageJSON,
		Attributes:  request.Attributes,
	})
	if errFresh != nil {
		return nil, errFresh
	}
	credential, errCredential := ParseCredential(fresh.Storage)
	if errCredential != nil {
		return nil, errCredential
	}
	cfg := settings()
	clientVersion := resolveClientVersion(h, cfg)
	summary := make([]pluginapi.QuotaMetric, 0, 4)

	if balance, errBalance := fetchCreditBalance(h, credential); errBalance == nil {
		summary = append(summary, pluginapi.QuotaMetric{
			Key: "credit_remaining", Label: "剩余积分", Value: balance.Total, Unit: "credit", Format: "number",
		})
		if balance.ExpiredTotal > 0 {
			summary = append(summary, pluginapi.QuotaMetric{
				Key: "credit_expired", Label: "已失效积分", Value: balance.ExpiredTotal, Unit: "credit", Format: "number",
			})
		}
	}
	if slot, errSlot := fetchActivitySlot(h, credential, clientVersion); errSlot == nil {
		claimable := 0.0
		if slot.SlotState == "available" {
			claimable = 1
		}
		summary = append(summary, pluginapi.QuotaMetric{
			Key: "daily_checkin", Label: "今日可签到", Value: claimable, Format: "boolean",
		})
	}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "LobsterAI 不支持重置积分"}, nil
}
