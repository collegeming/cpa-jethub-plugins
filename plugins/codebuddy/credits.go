package main

// 本文件是 Jet-Hub `src/credits.ts` 的 Go 移植：每日签到、积分余额，以及把它们
// 暴露给 CPA 管理端的 quota.* 方法。
//
// 端点（credits.ts:33-51）：
//   POST /v2/billing/meter/checkin-activity-status   （权威状态源）
//   POST /v2/billing/meter/daily-checkin             （领取）
//   POST /v2/billing/meter/get-user-resource         （余额，两个产品通用）
//
// 两个必须保留的实测结论：
//   - 幂等判据是**响应体业务码**（10001 / 1001），不是 HTTP 状态码：重复领取同样
//     返回 HTTP 400 + code 10001（credits.ts:19-21, 345-353）。
//   - 余额取 `CycleCapacityRemain`（本计费周期口径），不是 `CapacityRemain`
//     （终身口径）；后者会让数字虚高（credits.ts:106-118, 395-427）。

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// CheckinActivityStatusPath is the authoritative check-in status endpoint.
	// credits.ts:33.
	CheckinActivityStatusPath = "/v2/billing/meter/checkin-activity-status"
	// DailyCheckinPath claims the daily credits. credits.ts:35.
	DailyCheckinPath = "/v2/billing/meter/daily-checkin"
	// UserResourcePath reports the credit balance. credits.ts:51.
	UserResourcePath = "/v2/billing/meter/get-user-resource"

	// CodeAlreadyClaimed is the "already checked in today" business code.
	// credits.ts:57.
	CodeAlreadyClaimed = 10001
	// CodeAlreadyClaimedAlt is the alternative documented code. credits.ts:59.
	CodeAlreadyClaimedAlt = 1001
	// CodeNoQualification means the account is not eligible. credits.ts:60.
	CodeNoQualification = 1002
	// CodeActivityEnded means the campaign is over. credits.ts:61.
	CodeActivityEnded = 1003
	// PackageStatusExpired is the server's "this package expired" status.
	// credits.ts:393.
	PackageStatusExpired = 3
)

// supportsCheckin reports whether the selected product has the check-in
// endpoints. Only CodeBuddy 国内版 does (product.ts:366-367, credits.ts:42-46).
func supportsCheckin(product productConfig) bool {
	return product.ConfigValue == ProductCodeBuddy
}

// checkinStatus mirrors `CheckinStatus` (credits.ts:64-87).
//
// The JSON names are the upstream field names, which is also what a consumer of
// the status document reads (`checkin.today_checked_in`, `checkin.streak_days`);
// without the tags the struct would serialise as `TodayCheckedIn` and the hub's
// documented reader would silently find nothing.
type checkinStatus struct {
	Active         bool     `json:"active"`
	TodayCheckedIn bool     `json:"today_checked_in"`
	StreakDays     float64  `json:"streak_days"`
	DailyCredit    float64  `json:"daily_credit"`
	TodayCredit    float64  `json:"today_credit"`
	IsStreakDay    bool     `json:"is_streak_day"`
	TotalCredits   float64  `json:"total_credits"`
	CheckinDates   []string `json:"checkin_dates"`
	ActivityName   string   `json:"activity_name"`
	ThemeName      string   `json:"theme_name"`
	EndTime        string   `json:"end_time"`
}

// creditPackage mirrors `CreditPackage` (credits.ts:120-145).
type creditPackage struct {
	Name           string
	Unit           string
	Remaining      float64
	Total          float64
	Used           float64
	Active         bool
	CycleStartTime string
	CycleEndTime   string
	ExpiredTime    string
}

// creditBalance mirrors `CreditBalance` (credits.ts:148-167).
type creditBalance struct {
	Total        float64
	Packages     []creditPackage
	ExpiredTotal float64
}

// claimOutcome mirrors `ClaimOutcome` (credits.ts:90-94).
type claimOutcome struct {
	Kind        string // claimed | already-claimed | inactive | failed
	Message     string
	Credit      float64
	StreakDays  float64
	IsStreakDay bool
	Code        int
}

// ── 请求 ──

// checkinHeaders builds the check-in/balance request headers.
// credits.ts:180-197.
func checkinHeaders(credential *Credential, product productConfig) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+credential.AccessToken)
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set(HeaderDomain, credentialDomain(credential, product))
	headers.Set(HeaderProduct, DeploymentType)
	headers.Set(HeaderProductCode, product.ProductCode)
	if credential.UserID != "" {
		headers.Set("X-User-Id", credential.UserID)
	}
	if credential.EnterpriseID != "" {
		headers.Set(HeaderEnterpriseID, credential.EnterpriseID)
		headers.Set(HeaderTenantID, credential.EnterpriseID)
	}
	headers.Set("User-Agent", product.UserAgent)
	return headers
}

// postJSON performs one billing POST and decodes the body.
//
// The response is read as text first and only then parsed: an expired
// credential makes the Tencent gateway answer with an HTML error page, and
// `response.json()` would surface "Unexpected token '<'" — a message that hides
// the real cause (credits.ts:239-244).
func postJSON(h *abiboot.Host, path string, credential *Credential, product productConfig) (map[string]any, string) {
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodPost,
		URL:     product.urlFor(path),
		Headers: checkinHeaders(credential, product),
		Body:    []byte("{}"),
	})
	if errDo != nil {
		return nil, errDo.Error()
	}
	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &parsed); errUnmarshal != nil {
		return nil, describeNonJSONResponse(response.StatusCode, string(response.Body))
	}
	if parsed == nil {
		return nil, "请求失败或响应无法解析"
	}
	return parsed, ""
}

// describeNonJSONResponse turns "the body is not JSON" into a readable reason.
// credits.ts:284-291.
func describeNonJSONResponse(status int, text string) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "凭据已失效（HTTP " + itoa(status) + "），请重新登录该账号"
	}
	snippet := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	return "服务端返回了非 JSON 响应（HTTP " + itoa(status) + "）：" + truncate(snippet, 80)
}

// ── JSON 读取辅助（credits.ts:199-220）──

func readBool(source map[string]any, key string) bool {
	value, _ := source[key].(bool)
	return value
}

func readNumber(source map[string]any, key string) float64 {
	if value, ok := source[key].(float64); ok && value == value {
		return value
	}
	return 0
}

func readString(source map[string]any, key string) string {
	value, _ := source[key].(string)
	return value
}

func readStringArray(source map[string]any, key string) []string {
	values, ok := source[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, okText := value.(string); okText {
			out = append(out, text)
		}
	}
	return out
}

// truthy accepts a JSON boolean or the string "true".
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	case float64:
		return typed != 0
	default:
		return false
	}
}

// ── 签到 ──

// fetchCheckinStatus performs a single status query. A nil status means "not
// available" and must be distinguished from "no check-in today".
// credits.ts:298-323.
func fetchCheckinStatus(h *abiboot.Host, credential *Credential, product productConfig) *checkinStatus {
	status, _ := fetchCheckinStatusReason(h, credential, product)
	return status
}

// fetchCheckinStatusReason is fetchCheckinStatus with the reason a status could
// not be read. The per-account status document must report WHY an account has no
// check-in state instead of showing a bare "查询失败".
func fetchCheckinStatusReason(h *abiboot.Host, credential *Credential, product productConfig) (*checkinStatus, string) {
	body, errMessage := postJSON(h, CheckinActivityStatusPath, credential, product)
	if errMessage != "" {
		return nil, errMessage
	}
	if code := readNumber(body, "code"); code != 0 {
		message := readString(body, "msg")
		if message == "" {
			message = fmt.Sprintf("上游返回 code=%v", code)
		}
		return nil, message
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		return nil, "签到状态响应缺少 data 字段"
	}
	return &checkinStatus{
		Active:         readBool(data, "active"),
		TodayCheckedIn: readBool(data, "today_checked_in"),
		StreakDays:     readNumber(data, "streak_days"),
		DailyCredit:    readNumber(data, "daily_credit"),
		TodayCredit:    readNumber(data, "today_credit"),
		IsStreakDay:    readBool(data, "is_streak_day"),
		TotalCredits:   readNumber(data, "total_credits"),
		CheckinDates:   readStringArray(data, "checkin_dates"),
		ActivityName:   readString(data, "activity_name"),
		ThemeName:      readString(data, "theme_name"),
		EndTime:        readString(data, "end_time"),
	}, ""
}

// claimDailyCheckin claims the daily credits. Idempotence is a body field, not
// an HTTP status. credits.ts:332-367.
func claimDailyCheckin(h *abiboot.Host, credential *Credential, product productConfig) claimOutcome {
	body, errMessage := postJSON(h, DailyCheckinPath, credential, product)
	if errMessage != "" {
		return claimOutcome{Kind: "failed", Code: -1, Message: errMessage}
	}
	code := int(readNumber(body, "code"))
	if _, present := body["code"]; !present {
		code = -1
	}
	message := readString(body, "msg")

	if code == CodeAlreadyClaimed || code == CodeAlreadyClaimedAlt {
		if message == "" {
			message = "今天已签到"
		}
		return claimOutcome{Kind: "already-claimed", Message: message, Code: code}
	}
	if code == CodeNoQualification || code == CodeActivityEnded {
		if message == "" {
			message = "当前无领取资格"
		}
		return claimOutcome{Kind: "inactive", Message: message, Code: code}
	}
	if code != 0 {
		if message == "" {
			message = "领取失败"
		}
		return claimOutcome{Kind: "failed", Message: message, Code: code}
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		return claimOutcome{Kind: "failed", Code: code, Message: "领取响应缺少 data 字段"}
	}
	outcome := claimOutcome{
		Kind:        "claimed",
		Credit:      readNumber(data, "credit"),
		StreakDays:  readNumber(data, "streak_days"),
		IsStreakDay: readBool(data, "is_streak_day"),
		Message:     readString(data, "message"),
	}
	if outcome.Message == "" {
		outcome.Message = "签到成功"
	}
	return outcome
}

// ── 余额 ──

// readPreciseNumber prefers the `…Precise` string form. credits.ts:376-384.
func readPreciseNumber(source map[string]any, baseKey string) float64 {
	switch precise := source[baseKey+"Precise"].(type) {
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(precise), 64); err == nil {
			return parsed
		}
	case float64:
		if precise == precise {
			return precise
		}
	}
	return readNumber(source, baseKey)
}

// parseCreditPackage parses one `Accounts[]` entry. credits.ts:405-427.
func parseCreditPackage(entry map[string]any) creditPackage {
	name := readString(entry, "PackageName")
	if name == "" {
		name = readString(entry, "SubProductName")
	}
	if name == "" {
		name = readString(entry, "PackageCode")
	}
	unit := readString(entry, "CapacityUnit")
	if unit == "" {
		unit = readString(entry, "OriginUnit")
	}
	expiredTime := readString(entry, "ExpiredTime")
	active := true
	if status, ok := entry["Status"].(float64); ok && int(status) == PackageStatusExpired {
		active = false
	}
	if expiredTime != "" {
		// 服务端用 "2006-01-02 15:04:05" 形式的本地时间。
		if parsed, okParse := parseTimeMs(strings.Replace(expiredTime, " ", "T", 1)); okParse {
			if time.Now().UnixMilli() >= parsed {
				active = false
			}
		}
	}
	return creditPackage{
		Name:           name,
		Unit:           unit,
		Remaining:      readPreciseNumber(entry, "CycleCapacityRemain"),
		Total:          readPreciseNumber(entry, "CycleCapacitySize"),
		Used:           readPreciseNumber(entry, "CycleCapacityUsed"),
		Active:         active,
		CycleStartTime: readString(entry, "CycleStartTime"),
		CycleEndTime:   readString(entry, "CycleEndTime"),
		ExpiredTime:    expiredTime,
	}
}

// roundCredits rounds to two decimals; adding several packages exposes float
// noise otherwise. credits.ts:488-490.
func roundCredits(value float64) float64 {
	return math.Round(value*100) / 100
}

// fetchCreditBalance reads the credit balance. The payload nests as
// `data.Response.Data.Accounts[]` — two levels deeper than the check-in
// endpoint, which is the easiest thing to get wrong here (credits.ts:436-438).
func fetchCreditBalance(h *abiboot.Host, credential *Credential, product productConfig) *creditBalance {
	balance, _ := fetchCreditBalanceReason(h, credential, product)
	return balance
}

// fetchCreditBalanceReason is fetchCreditBalance with the reason a balance could
// not be read: a nil balance must never be rendered as 0 credits, and the reason
// is what the per-account document and page show instead.
func fetchCreditBalanceReason(h *abiboot.Host, credential *Credential, product productConfig) (*creditBalance, string) {
	body, errMessage := postJSON(h, UserResourcePath, credential, product)
	if errMessage != "" {
		return nil, errMessage
	}
	if code := readNumber(body, "code"); code != 0 {
		message := readString(body, "msg")
		if message == "" {
			message = fmt.Sprintf("上游返回 code=%v", code)
		}
		return nil, message
	}
	outer, okOuter := body["data"].(map[string]any)
	if !okOuter {
		return nil, "积分响应缺少 data 字段"
	}
	response, okResponse := outer["Response"].(map[string]any)
	if !okResponse {
		return nil, "积分响应缺少 data.Response 字段"
	}
	inner, okInner := response["Data"].(map[string]any)
	if !okInner {
		return nil, "积分响应缺少 data.Response.Data 字段"
	}
	accounts, okAccounts := inner["Accounts"].([]any)
	if !okAccounts {
		return nil, "积分响应缺少 Accounts 列表"
	}
	packages := make([]creditPackage, 0, len(accounts))
	for _, item := range accounts {
		entry, okEntry := item.(map[string]any)
		if !okEntry {
			continue
		}
		packages = append(packages, parseCreditPackage(entry))
	}
	total := 0.0
	expiredTotal := 0.0
	for _, pkg := range packages {
		if pkg.Active {
			total += pkg.Remaining
			continue
		}
		expiredTotal += pkg.Remaining
	}
	return &creditBalance{Total: roundCredits(total), Packages: packages, ExpiredTotal: roundCredits(expiredTotal)}, ""
}

// ── Per-account quota ──

// accountQuota is ONE CodeBuddy/WorkBuddy account's own credit and check-in
// state.
//
// A quota field is nil when the provider did not publish it, and the matching
// reason says why. A failed read must render as 未知, never as 0 — 0 would read
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
	// Product is the product THIS account was created against; it decides the
	// endpoint and whether a check-in endpoint exists at all.
	Product productConfig
	// Balance is the account's own credit aggregate, or nil with BalanceErr set.
	Balance    *creditBalance
	BalanceErr string
	// Checkin is the account's own daily check-in state. CheckinSupported is
	// false for the products whose backend has no check-in endpoint (国际版),
	// which is a fact about the product rather than a failure; CheckinErr then
	// carries a failed read.
	Checkin          *checkinStatus
	CheckinErr       string
	CheckinSupported bool
}

// collectAccountQuotas reads the credits and check-in state of every account, in
// host order, one account at a time.
//
// Sequential on purpose: each account costs up to two upstream requests, and the
// reference panel queries balances one account at a time for the same reason
// (parallel balance reads are what the provider's risk control reacts to). One
// account's failure never stops the sweep and never hides another's figures.
func collectAccountQuotas(h *abiboot.Host, accounts []pluginapi.HostAuthFileEntry) []accountQuota {
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
		product := productForCredential(credential)
		quota.Product = product
		quota.CheckinSupported = supportsCheckin(product)
		balance, balanceErr := fetchCreditBalanceReason(h, credential, product)
		quota.Balance, quota.BalanceErr = balance, balanceErr
		if quota.CheckinSupported {
			status, statusErr := fetchCheckinStatusReason(h, credential, product)
			quota.Checkin, quota.CheckinErr = status, statusErr
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
// top level (credits / checkin / status / expires_at / refreshable), so a
// consumer that reads the flat document can read one entry of `accounts` with
// the same code. A failed read emits the reason under credit_error /
// checkin_error and NO number at all.
func quotaJSON(quota accountQuota) map[string]any {
	item := map[string]any{
		"auth_index": quota.Entry.AuthIndex,
		"name":       quota.Entry.Name,
		"status":     statusText(quota.Entry),
		"disabled":   quota.Entry.Disabled,
	}
	if label := strings.TrimSpace(quota.Entry.Label); label != "" {
		item["label"] = label
	}
	if quota.CredentialErr != nil {
		item["error"] = quota.CredentialErr.Error()
		return item
	}
	item["account_product"] = quota.Product.ConfigValue
	if expiry := quota.Credential.Expiry(); !expiry.IsZero() {
		item["expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	item["refreshable"] = quota.Credential.Refreshable()
	if quota.Refresh.Refreshed {
		item["refreshed"] = true
	}
	if quota.Refresh.Err != nil {
		item["refresh_error"] = quota.Refresh.Err.Error()
	}
	if quota.Balance != nil {
		item["credits"] = map[string]any{
			"total":         quota.Balance.Total,
			"expired_total": quota.Balance.ExpiredTotal,
			"packages":      quota.Balance.Packages,
		}
	}
	if quota.BalanceErr != "" {
		item["credit_error"] = quota.BalanceErr
	}
	switch {
	case !quota.CheckinSupported:
		// The product's backend has no check-in endpoint: say so instead of
		// leaving the field to look like a failure (or inventing an inactive
		// activity).
		item["checkin_supported"] = false
	case quota.Checkin != nil:
		item["checkin"] = quota.Checkin
		item["checkin_supported"] = true
	case quota.CheckinErr != "":
		item["checkin_error"] = quota.CheckinErr
		item["checkin_supported"] = true
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

// ── quota.* 方法 ──

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the supported provider and reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	product, _ := productByConfigValue(settings().Product)
	display := "CodeBuddy 积分"
	if supportsCheckin(product) {
		display = "CodeBuddy 积分（含每日签到）"
	}
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        display,
		SupportsReset:      false,
	}, nil
}

// handleQuotaFetch reports the credit balance and check-in state.
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
	product := productForCredential(credential)

	summary := []pluginapi.QuotaMetric{}
	balance := fetchCreditBalance(h, credential, product)
	if balance != nil {
		summary = append(summary,
			pluginapi.QuotaMetric{Key: "credit_remaining", Label: "本周期剩余积分", Value: balance.Total, Unit: "credit", Format: "number"},
		)
		if balance.ExpiredTotal > 0 {
			summary = append(summary, pluginapi.QuotaMetric{
				Key: "credit_expired", Label: "已失效积分", Value: balance.ExpiredTotal, Unit: "credit", Format: "number",
			})
		}
	}
	if supportsCheckin(product) {
		if status := fetchCheckinStatus(h, credential, product); status != nil {
			claimable := 0.0
			if status.Active && !status.TodayCheckedIn {
				claimable = 1
			}
			summary = append(summary,
				pluginapi.QuotaMetric{Key: "daily_credit", Label: "今日可领积分", Value: status.DailyCredit, Unit: "credit", Format: "number"},
				pluginapi.QuotaMetric{Key: "daily_checkin_claimable", Label: "今日可签到", Value: claimable, Format: "boolean"},
				pluginapi.QuotaMetric{Key: "checkin_streak_days", Label: "连续签到天数", Value: status.StreakDays, Format: "number"},
			)
		}
	}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "CodeBuddy 不支持重置额度"}, nil
}
