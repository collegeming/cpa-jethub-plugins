package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credits"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Sign-in (daily check-in) and credit balance.
//
//	GET  {APIHost}/minimax-cloud/api/v1/signin/status?timezone_id=<IANA>
//	POST {APIHost}/minimax-cloud/api/v1/signin/claim?timezone_id=<IANA>   body {}
//	GET  {APIHost}/minimax-cloud/api/v1/credit/details
//
// ## ⚠️ The five facts that are easy to get wrong
//
//  1. **`timezone_id` is a QUERY PARAMETER and is required.** Measured four
//     ways: in the query it works; in a header it does not; omitting it answers
//     `invalid timezone_id`; an unknown zone name does too. Putting it in a
//     header returns business code `1406010011` — WITH HTTP 200, so a
//     status-code-only reader calls that a success.
//  2. **The business code is `base_resp.status_code`, NOT a top-level `code`.**
//     And again, a business failure arrives with HTTP 200.
//  3. **Claim idempotency is `claim_result`**: 1 means "granted just now", 2
//     means "already claimed". Both answer HTTP 200, so the HTTP status can
//     never distinguish them.
//  4. **`dailyCredit` is `points` alone.** `bonus_points` is a SUBSET of it
//     (`points: 800` with `bonus_points: 400` on a measured day), so adding them
//     reports double the real reward.
//  5. **The balance is the SUM of `details[].remaining_amount`, parsed as
//     STRINGS** (`"800.00"`), and `total_count` is the number of ledger RECORDS,
//     not an amount.
//
// ⚠️ The `total_count` mistake shipped once. It survived review because an
// account with no credits has NEITHER a `details` array NOR a non-zero
// `total_count` — "record count 0" and "balance 0" coincide exactly, so the
// unit test that asserted `total_count: 0 → total: 0` was a tautology. The two
// only diverge once credits exist: one record, 800 points.
//
// ⚠️ And the two endpoint families do NOT share a response envelope:
// `signin/status` and `signin/claim` wrap their payload in `data`, while
// `credit/details` is FLAT (`details` / `total_count` sit beside `base_resp`).

// Sign-in panel statuses (`MINIMAX_SIGNIN_STATUS`, `minimax-credits.ts`).
const (
	signinStatusUpcoming  = 1
	signinStatusClaimable = 2
	signinStatusClaimed   = 3
	signinStatusDisabled  = 4
	// signinPanelDays is the panel's fixed length; anything else is malformed.
	signinPanelDays = 7
)

// Claim results (`MINIMAX_CLAIM_RESULT`, `minimax-credits.ts`).
const (
	claimResultClaimed        = 1
	claimResultAlreadyClaimed = 2
)

// signinDay is one day of the 7-day panel.
type signinDay struct {
	DayNo       int
	Points      float64
	BonusPoints float64
	Status      int
	IsToday     bool
}

// signinPanel is the normalised panel.
type signinPanel struct {
	Scene int
	Days  []signinDay
}

// creditBalance is the parsed credit ledger.
type creditBalance struct {
	// Total is the sum of every ledger row's remaining amount.
	Total float64
	// Rows is how many ledger rows the response carried, for diagnostics. It is
	// NOT the balance — that is the whole point of the note above.
	Rows int
}

// signinStatus is the host-facing check-in state.
type signinStatus struct {
	// Active is true whenever a panel could be read at all. It is NOT derived
	// from "is anything claimable": doing that reports "the campaign is closed"
	// for an account that has simply already claimed today.
	Active          bool
	TodayCheckedIn  bool
	StreakDays      int
	DailyCredit     float64
	TodayCredit     float64
	IsStreakDay     bool
	ClaimableExists bool
}

// timezoneQuery builds a URL with the mandatory `timezone_id` query parameter.
//
// ⚠️ A header would be silently ignored by the server (measured), which is why
// this builds a URL rather than touching a header map.
func timezoneQuery(base, path, timezoneID string) (string, error) {
	parsed, errParse := url.Parse(base + path)
	if errParse != nil {
		return "", statusError(false, "bad_url", http.StatusInternalServerError,
			"minimax: 拼接 URL 失败：%v", errParse)
	}
	query := parsed.Query()
	query.Set("timezone_id", timezoneID)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// envelopeResult is one decoded business response.
type envelopeResult struct {
	// OK reports whether the business code was 0 (or absent).
	OK bool
	// Code is the business status code when it was non-zero.
	Code int
	// Message is the business status message.
	Message string
	// Payload is the decoded response object.
	Payload map[string]any
}

// requestEnvelope performs one request and unwraps the business envelope.
//
// ⚠️ It never returns a transport error as a business failure: a network
// problem and a rejected request are different answers, and the caller renders
// them differently. The business code is read from `base_resp.status_code`.
func requestEnvelope(h *abiboot.Host, method, rawURL string, credential *Credential, body []byte, cfg Config) (envelopeResult, error) {
	headers := businessHeaders(credential)
	if body != nil {
		headers.Set("Content-Type", "application/json")
	}
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.requestTimeout())*time.Millisecond,
		method, rawURL, headers, body)
	if errDo != nil {
		return envelopeResult{}, transportError("request_transport", "minimax: 请求失败：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return envelopeResult{}, credentialError("auth_expired",
			"MiniMax 凭据已失效（HTTP %d），请重新登录%s", response.StatusCode, credentialAdvice(response.StatusCode))
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return envelopeResult{}, upstreamStatusError("request_failed", response.StatusCode,
				"服务端返回了非 JSON 响应（HTTP %d）：%s", response.StatusCode, truncate(string(response.Body), 300))
		}
		return envelopeResult{}, transportError("bad_response",
			"服务端返回了非 JSON 响应（HTTP %d）：%s", response.StatusCode, truncate(string(response.Body), 300))
	}
	result := envelopeResult{OK: true, Payload: payload}
	if baseResp, okBase := payload["base_resp"].(map[string]any); okBase {
		if code, okCode := numericValue(baseResp["status_code"]); okCode && code != 0 {
			result.OK = false
			result.Code = int(code)
			result.Message = strings.TrimSpace(stringField(baseResp, "status_msg"))
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// ⚠️ A non-2xx HTTP status is a failure even when `base_resp` looked
		// clean; the two signals are checked independently because the server
		// uses the HTTP status for transport faults and `base_resp` for business
		// ones, and only one of them is populated on either path.
		return envelopeResult{}, upstreamStatusError("request_failed", response.StatusCode,
			"MiniMax 返回 HTTP %d：%s", response.StatusCode, truncate(string(response.Body), 300))
	}
	return result, nil
}

// unwrapEnvelopeData returns the business payload.
//
// ⚠️ The two endpoint families disagree about their shape and both are handled:
// `signin/status` and `signin/claim` wrap their fields in `data`, while
// `credit/details` is flat. Reading only `data` would judge a perfectly valid
// balance response as "missing the data field", turning "the balance is zero"
// into "the query failed".
func unwrapEnvelopeData(payload map[string]any) map[string]any {
	if nested, okNested := payload["data"].(map[string]any); okNested {
		return nested
	}
	return payload
}

// parseSigninPanel validates a 7-day panel.
//
// The rules are the vendor client's own (`validateSigninPanel`,
// `minimax-credits.ts`) and they are strict on purpose: a malformed panel is
// reported as unreadable rather than half-rendered, because a panel with the
// wrong day count or two "today" markers would show the user a streak that does
// not exist.
func parseSigninPanel(data map[string]any) *signinPanel {
	scene, okScene := numericValue(data["scene"])
	if !okScene {
		return nil
	}
	rawDays, okDays := data["days"].([]any)
	if !okDays || len(rawDays) != signinPanelDays {
		return nil
	}
	panel := &signinPanel{Scene: int(scene)}
	seen := make(map[int]bool, signinPanelDays)
	claimable := 0
	today := 0
	for _, raw := range rawDays {
		day, okDay := raw.(map[string]any)
		if !okDay {
			return nil
		}
		dayNo, okNo := numericValue(day["day_no"])
		points, okPoints := numericValue(day["points"])
		status, okStatus := numericValue(day["status"])
		isToday, okToday := day["is_today"].(bool)
		bonus := 0.0
		if rawBonus, present := day["bonus_points"]; present && rawBonus != nil {
			parsed, okBonus := numericValue(rawBonus)
			if !okBonus {
				return nil
			}
			bonus = parsed
		}
		switch {
		case !okNo || dayNo < 1 || dayNo > signinPanelDays || dayNo != float64(int(dayNo)):
			return nil
		case seen[int(dayNo)]:
			return nil
		case !okPoints || points < 0:
			return nil
		case bonus < 0:
			return nil
		case !okToday:
			return nil
		case !okStatus || status < signinStatusUpcoming || status > signinStatusDisabled:
			return nil
		}
		seen[int(dayNo)] = true
		if int(status) == signinStatusClaimable {
			claimable++
		}
		if isToday {
			today++
		}
		panel.Days = append(panel.Days, signinDay{
			DayNo:       int(dayNo),
			Points:      points,
			BonusPoints: bonus,
			Status:      int(status),
			IsToday:     isToday,
		})
	}
	// At most one claimable day and at most one "today" (the vendor client's
	// own hard constraint).
	if claimable > 1 || today > 1 {
		return nil
	}
	return panel
}

// panelToSigninStatus maps a panel onto the host-facing state.
//
// ⚠️ `dailyCredit` is `points` ALONE — `bonus_points` is already part of it and
// adding them doubles the figure the user is shown.
//
// ⚠️ `active` is unconditionally true once a panel was read. Deriving it from
// "is something claimable" reports the campaign as closed for everyone who has
// already claimed, which is precisely the state a daily user is in.
func panelToSigninStatus(panel *signinPanel) signinStatus {
	if panel == nil {
		return signinStatus{}
	}
	status := signinStatus{Active: true}
	var today *signinDay
	for index := range panel.Days {
		day := panel.Days[index]
		if day.IsToday {
			today = &panel.Days[index]
		}
		if day.Status == signinStatusClaimable {
			status.ClaimableExists = true
		}
	}
	if today != nil && today.Status == signinStatusClaimed {
		status.TodayCheckedIn = true
		status.TodayCredit = today.Points
	}
	// The streak counts consecutive claimed days ENDING AT today. When today is
	// still claimable it counts backwards from yesterday, which is what the
	// vendor's own `getCurrentSigninStreak()` does.
	ordered := make([]signinDay, len(panel.Days))
	copy(ordered, panel.Days)
	sortDaysByNo(ordered)
	start := -1
	if today != nil {
		switch today.Status {
		case signinStatusClaimed:
			for index := range ordered {
				if ordered[index].DayNo == today.DayNo {
					start = index
					break
				}
			}
		case signinStatusClaimable:
			for index := range ordered {
				if ordered[index].DayNo == today.DayNo-1 {
					start = index
					break
				}
			}
		}
	}
	for index := start; index >= 0; index-- {
		if ordered[index].Status != signinStatusClaimed {
			break
		}
		status.StreakDays++
	}
	// ⚠️ The reported amount is today's when it has one, otherwise the next
	// claimable day's, so the button always names a real number.
	switch {
	case today != nil:
		status.DailyCredit = today.Points
	case status.ClaimableExists:
		for _, day := range panel.Days {
			if day.Status == signinStatusClaimable {
				status.DailyCredit = day.Points
				break
			}
		}
	}
	// ⚠️ `isStreakDay` is NOT derivable from `bonus_points`. Measured over a
	// full 7-day panel, `bonus_points` is populated on EVERY day (400 on the
	// 800-point days, 1000 on the 2000-point day), so `bonus_points > 0` is
	// always true and carries no information. The reference shipped that
	// expression with a long note explaining it, and it remains wrong there.
	// This port therefore reports false and says so, rather than repeating a
	// judgement the data does not support.
	status.IsStreakDay = false
	return status
}

// sortDaysByNo orders the panel by day number.
func sortDaysByNo(days []signinDay) {
	for outer := 1; outer < len(days); outer++ {
		for inner := outer; inner > 0 && days[inner-1].DayNo > days[inner].DayNo; inner-- {
			days[inner-1], days[inner] = days[inner], days[inner-1]
		}
	}
}

// fetchSigninStatus reads the check-in panel.
func fetchSigninStatus(h *abiboot.Host, credential *Credential, cfg Config) (signinStatus, error) {
	rawURL, errURL := timezoneQuery(APIHost, SigninStatusPath, cfg.timezoneID())
	if errURL != nil {
		return signinStatus{}, errURL
	}
	result, errRequest := requestEnvelope(h, http.MethodGet, rawURL, credential, nil, cfg)
	if errRequest != nil {
		return signinStatus{}, errRequest
	}
	if !result.OK {
		return signinStatus{}, businessError("读取 MiniMax 签到状态", result)
	}
	panel := parseSigninPanel(unwrapEnvelopeData(result.Payload))
	if panel == nil {
		return signinStatus{}, transportError("signin_shape",
			"MiniMax 签到响应不是合法的 7 天面板（days 必须恰好 7 条且字段齐全）")
	}
	return panelToSigninStatus(panel), nil
}

// claimDailyCheckin performs the daily claim.
//
// ⚠️ The outcome is decided by `claim_result`, not by the HTTP status: a repeat
// claim answers HTTP 200 just like a successful one. Reading the status code
// would tell every user their claim succeeded, every time.
func claimDailyCheckin(h *abiboot.Host, credential *Credential, cfg Config) (credits.Outcome, error) {
	rawURL, errURL := timezoneQuery(APIHost, SigninClaimPath, cfg.timezoneID())
	if errURL != nil {
		return credits.Outcome{}, errURL
	}
	// The body is `{}` — the endpoint takes no parameters of its own.
	result, errRequest := requestEnvelope(h, http.MethodPost, rawURL, credential, []byte("{}"), cfg)
	if errRequest != nil {
		if statusOf(errRequest, 0) == http.StatusUnauthorized {
			// A dead credential is the credential's problem, not the claim's;
			// the caller surfaces it as a 401 so the host can rotate.
			return credits.Outcome{}, errRequest
		}
		// Every other failure is an outcome, not an exception: a batch claim
		// must not stop at the first account that fails.
		return credits.Outcome{
			Status:  credits.StatusFailed,
			Message: errRequest.Error(),
		}, nil
	}
	if !result.OK {
		return credits.Outcome{
			Status:  credits.StatusFailed,
			Message: businessMessage(result),
		}, nil
	}
	data := unwrapEnvelopeData(result.Payload)
	claimResult, okClaim := numericValue(data["claim_result"])
	if !okClaim {
		return credits.Outcome{
			Status:  credits.StatusFailed,
			Message: "MiniMax 签到响应缺少有效的 claim_result",
		}, nil
	}
	// The claim answer also carries the refreshed panel, but the outcome does
	// not depend on it: `claim_result` alone decides what happened. Reading the
	// panel here only guards against a shape change going unnoticed, so a
	// missing or malformed one is ignored rather than failing an otherwise
	// successful claim.
	if rawPanel, okPanel := data["panel"].(map[string]any); okPanel {
		_ = parseSigninPanel(rawPanel)
	}
	switch int(claimResult) {
	case claimResultAlreadyClaimed:
		return credits.Outcome{
			Status:  credits.StatusAlreadyClaimed,
			Message: "今日已签到",
		}, nil
	case claimResultClaimed:
		// ⚠️ `points` is the total and already includes `bonus_points`.
		points, _ := numericValue(data["points"])
		return credits.Outcome{
			Status:  credits.StatusClaimed,
			Amount:  points,
			Message: "签到成功，领取 " + trimAmount(points) + " 积分",
		}, nil
	default:
		return credits.Outcome{
			Status:  credits.StatusFailed,
			Message: "MiniMax 签到返回了未知的 claim_result=" + trimAmount(claimResult),
		}, nil
	}
}

// fetchCreditBalance reads the credit ledger.
//
// ⚠️ The balance is the SUM of `details[].remaining_amount`, each of which
// arrives as a STRING (`"800.00"`), and `total_count` is the number of RECORDS.
// A response with no `details` array is a REAL ZERO balance, not a failed read;
// only a missing/garbled response shape is an error.
func fetchCreditBalance(h *abiboot.Host, credential *Credential, cfg Config) (*creditBalance, error) {
	result, errRequest := requestEnvelope(h, http.MethodGet, APIHost+CreditDetailsPath, credential, nil, cfg)
	if errRequest != nil {
		return nil, errRequest
	}
	if !result.OK {
		return nil, businessError("读取 MiniMax 积分", result)
	}
	// The ledger response is flat (no `data`); unwrapEnvelopeData passes it
	// through unchanged.
	data := unwrapEnvelopeData(result.Payload)
	rawDetails, hasDetails := data["details"]
	_, hasCount := numericValue(data["total_count"])
	if !hasDetails && !hasCount {
		// Neither a ledger nor a count: the shape is not one this endpoint
		// produces, so no number is invented.
		return nil, transportError("balance_shape",
			"MiniMax 积分响应既没有 details 也没有 total_count，无法给出余额")
	}
	balance := &creditBalance{}
	if details, okDetails := rawDetails.([]any); okDetails {
		balance.Rows = len(details)
		for _, raw := range details {
			entry, okEntry := raw.(map[string]any)
			if !okEntry {
				continue
			}
			// ⚠️ Strings, not numbers: the endpoint sends `"800.00"`.
			if remaining, okRemaining := looseAmount(entry["remaining_amount"]); okRemaining {
				balance.Total += remaining
			}
		}
	}
	return balance, nil
}

// looseAmount parses an amount that may arrive as a JSON number OR a string.
//
// ⚠️ An empty string is rejected explicitly: `Number("")` is 0 in JavaScript
// and `ParseFloat("")` errors in Go, and reading an empty field as "0 credits"
// is the same class of mistake as reading `total_count` as a balance.
func looseAmount(value any) (float64, bool) {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, errParse := strconv.ParseFloat(trimmed, 64)
		if errParse != nil {
			return 0, false
		}
		return parsed, true
	default:
		return numericValue(value)
	}
}

// numericValue reads a finite JSON number.
func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed != typed {
			return 0, false
		}
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, errParse := strconv.ParseFloat(string(typed), 64)
		if errParse != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// businessError renders a non-zero business code as a classified failure.
func businessError(action string, result envelopeResult) error {
	message := businessMessage(result)
	// ⚠️ An invalid `timezone_id` is reported as business code 1406010011 WITH
	// HTTP 200. It is the operator's configuration, so it is a 500 rather than
	// a bad request: the client did nothing wrong and retrying will not help
	// until the setting changes.
	if result.Code == timezoneRejectedCode {
		return statusError(false, "invalid_timezone", http.StatusInternalServerError,
			"%s 失败：%s（timezone_id 必须是合法的 IANA 时区名，且只能作为 query 参数下发）",
			action, message)
	}
	return transportError("business_error", "%s 失败：%s", action, message)
}

// timezoneRejectedCode is the business code the server returns for a bad or
// misplaced `timezone_id` (measured: the answer is HTTP 200).
const timezoneRejectedCode = 1406010011

// businessMessage renders a business failure's text.
func businessMessage(result envelopeResult) string {
	if strings.TrimSpace(result.Message) != "" {
		return result.Message + "（业务码 " + itoaInt(result.Code) + "）"
	}
	return "业务码 " + itoaInt(result.Code)
}

// ── quota provider ──

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the supported provider and reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        "MiniMax Code 积分（余额 / 每日签到）",
		// Nothing here can be reset: the daily reward is server-side, and a
		// claim is idempotent through `claim_result`.
		SupportsReset: false,
	}, nil
}

// handleQuotaFetch reports the credit balance.
//
// A failed read is an error and NEVER a zero balance: reporting "0 credits" for
// an unreachable endpoint tells the user their credits are gone.
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
	balance, errBalance := fetchCreditBalance(h, credential, cfg)
	if errBalance != nil {
		return nil, errBalance
	}
	summary := []pluginapi.QuotaMetric{{
		Key:    "credits_total",
		Label:  "积分余额",
		Value:  balance.Total,
		Unit:   "积分",
		Format: "number",
	}}
	if status, errStatus := fetchSigninStatus(h, credential, cfg); errStatus == nil {
		summary = append(summary, pluginapi.QuotaMetric{
			Key:    "daily_credit",
			Label:  "今日签到额度",
			Value:  status.DailyCredit,
			Unit:   "积分",
			Format: "number",
		})
	}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{
		Success: false,
		Message: "MiniMax 不支持重置积分：每日额度由服务端按签到发放，重复领取由 claim_result 幂等",
	}, nil
}

// trimAmount renders an amount without a trailing `.0`.
func trimAmount(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
