package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// CodingPlan plan state, claim and usage. The three REST endpoints live on
// `api.gitcode.com/api/v5` (`crates/atomcode-codingplan/src/client.rs:213,287,318`).

// planInfo is the `codingplan_free` summary (`crates/atomcode-codingplan/src/types.rs:222-260`).
//
// `status` and the dates are the authoritative "is my plan active" signals:
// before the first claim the server reports `status:-1`, `claimed_at:null`,
// `remaining_days:null`; after a successful claim `status:1`, a real
// `claimed_at`/`expires_at` pair and a positive `remaining_days`
// (measured 2026-10-01).
type planInfo struct {
	PlanName  string `json:"plan_name"`
	PlanType  string `json:"plan_type"`
	Status    int    `json:"status"`
	ClaimedAt string `json:"claimed_at"`
	ExpiresAt string `json:"expires_at"`
	Remaining *int   `json:"remaining_days"`
	TotalDays *int   `json:"total_days"`
	ApplyID   *int64 `json:"apply_id"`
	// Active reports the derived verdict, set after decoding.
	Active bool `json:"-"`
}

// rateLimitWindow is one rolling quota window (`types.rs:186-220`).
//
// The plan's real quota is a CALL COUNT per window, not a token budget: the
// free tier measured 2026-10-01 allows 200 calls per 5 hours.
type rateLimitWindow struct {
	RuleIndex         int     `json:"rule_index"`
	ShowEnable        int     `json:"show_enable"`
	WindowHours       int     `json:"window_hours"`
	CallLimit         int64   `json:"call_limit"`
	CallsUsed         int64   `json:"calls_used"`
	UsagePercent      float64 `json:"usage_percent"`
	QuotaExhausted    bool    `json:"quota_exhausted"`
	ResetAt           string  `json:"reset_at"`
	ResetAtDisplay    string  `json:"reset_at_display"`
	SecondsUntilReset int64   `json:"seconds_until_reset"`
	ResetLabel        string  `json:"reset_label"`
	UsageStatusDesc   string  `json:"usage_status_desc"`
}

// usageInfo is the `current_usage` summary (`types.rs:262+`).
type usageInfo struct {
	Placeholder       bool    `json:"placeholder"`
	WindowTokenLimit  *int64  `json:"window_token_limit"`
	WindowTokensUsed  int64   `json:"window_tokens_used"`
	UsagePercent      float64 `json:"usage_percent"`
	WindowHours       *int    `json:"window_hours"`
	ResetAt           string  `json:"reset_at"`
	ResetAtDisplay    string  `json:"reset_at_display"`
	SecondsUntilReset int64   `json:"seconds_until_reset"`
	ResetLabel        string  `json:"reset_label"`
	UsageStatusDesc   string  `json:"usage_status_desc"`
}

// statusResponse is the `GET /coding-plan/status-v2` envelope.
type statusResponse struct {
	CodingPlanFree     *planInfo         `json:"codingplan_free"`
	CurrentUsage       *usageInfo        `json:"current_usage"`
	RateLimitWindows   []rateLimitWindow `json:"rate_limit_windows"`
	AuditStatus        int               `json:"audit_status"`
	PlanType           string            `json:"plan_type"`
	ExpiresAt          string            `json:"expires_at"`
	WindowQuotaExhaust bool              `json:"window_quota_exhausted"`
	WindowQuotaHint    *string           `json:"window_quota_hint"`
}

// planTier maps the server's plan name onto a `models-v2` tier.
//
// The mapping exists because `models-v2` computes `plan_available` relative to
// the tier it is asked for, and `plan_available` is what decides whether a model
// is registered at all. The server names are `"CodingPlan Lite-体验版"` /
// `"CodingPlan Pro"` / `"CodingPlan Max"`, so the match is a substring test from
// most specific to least (`crates/atomcode-codingplan/src/types.rs:53-70`).
func (s *statusResponse) planTier() string {
	if s == nil {
		return ""
	}
	name := ""
	if s.CodingPlanFree != nil {
		name = s.CodingPlanFree.PlanName
	}
	if strings.TrimSpace(name) == "" {
		name = s.PlanType
	}
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "max"):
		return planTypeMax
	case strings.Contains(lower, "pro"):
		return planTypePro
	case strings.Contains(lower, "lite"):
		return planTypeLite
	default:
		return ""
	}
}

// active reports whether the account currently holds a usable plan.
//
// The reference's own rule is "`codingplan_free` is present AND its `expires_at`
// is non-empty" — with an empty `expires_at` the plan is rendered as *pending
// activation*. This adapter follows that rather than the `status` enum, for the
// reason the upstream spec records: `PlanInfo.status` and `audit_status` are
// parsed by the reference and consumed nowhere, so they carry no meaning anyone
// has established. `remaining_days` is kept as a second signal because it is
// plainly derived from the same expiry.
func (s *statusResponse) active() bool {
	if s == nil || s.CodingPlanFree == nil {
		return false
	}
	plan := s.CodingPlanFree
	if strings.TrimSpace(plan.ExpiresAt) != "" {
		return true
	}
	return plan.Remaining != nil && *plan.Remaining > 0
}

// quotaWindows returns the windows the server wants shown (`show_enable == 1`).
func (s *statusResponse) quotaWindows() []rateLimitWindow {
	if s == nil {
		return nil
	}
	shown := make([]rateLimitWindow, 0, len(s.RateLimitWindows))
	for _, window := range s.RateLimitWindows {
		if window.ShowEnable == 1 {
			shown = append(shown, window)
		}
	}
	return shown
}

// claimResponse is the `POST /coding-plan/claim-v2` reply
// (`crates/atomcode-codingplan/src/types.rs:75-89`).
type claimResponse struct {
	Success   bool   `json:"success"`
	Duplicate bool   `json:"duplicate"`
	Message   string `json:"message"`
	PlanType  string `json:"plan_type"`
	PlanName  string `json:"plan_name"`
}

// tierAttempt records one tier's outcome inside the cascade.
type tierAttempt struct {
	Tier     string `json:"tier"`
	Outcome  string `json:"outcome"`
	Message  string `json:"message,omitempty"`
	PlanName string `json:"plan_name,omitempty"`
}

// claimOutcome is the cascade result.
type claimOutcome struct {
	// Status is claimed / already-claimed / refused / failed.
	Status string `json:"status"`
	// Message is the server's own message where one exists.
	Message string `json:"message"`
	// PlanName is the tier name the server reported.
	PlanName string `json:"plan_name,omitempty"`
	// Attempts is the per-tier trail, in cascade order.
	Attempts []tierAttempt `json:"attempts"`
}

// Claim statuses.
const (
	claimClaimed      = "claimed"
	claimAlreadyHeld  = "already-claimed"
	claimRefused      = "refused"
	claimFailed       = "failed"
	claimAuthRequired = "auth-required"
)

// fetchStatus calls `GET /coding-plan/status-v2`.
func fetchStatus(h *abiboot.Host, cfg Config, credential *Credential) (*statusResponse, error) {
	response, errDo := hostDo(h, http.MethodGet, cfg.codingPlanURL(codingPlanStatusPath), authHeaders(credential), nil)
	if errDo != nil {
		return nil, abiboot.RetryableError("transport", "查询 AtomCode 套餐状态失败：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, abiboot.HTTPError("AUTH", http.StatusUnauthorized, "AtomCode 登录态失效，请重新登录")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.HTTPError(httpErrorCode(response.StatusCode), response.StatusCode,
			"%s", classifyCodingPlanError("status-v2", response.StatusCode, string(response.Body)))
	}
	var decoded statusResponse
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		return nil, abiboot.Errorf("codingplan_protocol", "解析 status-v2 响应失败: %v", errUnmarshal)
	}
	return &decoded, nil
}

// usageRow is one day of the 60-day usage report (`crates/atomcode-codingplan/src/usage.rs:9-38`).
type usageRow struct {
	Date        string           `json:"date"`
	ModelCounts map[string]int64 `json:"model_counts"`
	ModelTokens map[string]int64 `json:"model_tokens"`
	TotalCounts int64            `json:"total_counts"`
	TotalTokens int64            `json:"total_tokens"`
}

// usageResponse is the `GET /coding-plan/usage` envelope.
type usageResponse struct {
	Days        int              `json:"days"`
	StartDate   string           `json:"start_date"`
	EndDate     string           `json:"end_date"`
	Models      []string         `json:"models"`
	Rows        []usageRow       `json:"rows"`
	ModelTokens map[string]int64 `json:"model_tokens"`
	ModelCounts map[string]int64 `json:"model_counts"`
	TotalTokens int64            `json:"total_tokens"`
	TotalCounts int64            `json:"total_counts"`
}

// fetchUsage calls `GET /coding-plan/usage`.
func fetchUsage(h *abiboot.Host, cfg Config, credential *Credential) (*usageResponse, error) {
	response, errDo := hostDo(h, http.MethodGet, cfg.codingPlanURL(codingPlanUsagePath), authHeaders(credential), nil)
	if errDo != nil {
		return nil, abiboot.RetryableError("transport", "查询 AtomCode 用量失败：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, abiboot.HTTPError("AUTH", http.StatusUnauthorized, "AtomCode 登录态失效，请重新登录")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.HTTPError(httpErrorCode(response.StatusCode), response.StatusCode,
			"%s", classifyCodingPlanError("usage", response.StatusCode, string(response.Body)))
	}
	var decoded usageResponse
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		return nil, abiboot.Errorf("codingplan_protocol", "解析 usage 响应失败: %v", errUnmarshal)
	}
	return &decoded, nil
}

// claimTier calls `POST /coding-plan/claim-v2` for one tier.
func claimTier(h *abiboot.Host, cfg Config, credential *Credential, tier string) (*claimResponse, error) {
	body, errMarshal := json.Marshal(map[string]string{"plan_type": tier})
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_request", "序列化领取请求失败: %v", errMarshal)
	}
	headers := authHeaders(credential)
	headers.Set("Content-Type", "application/json")
	response, errDo := hostDo(h, http.MethodPost, cfg.codingPlanURL(codingPlanClaimPath), headers, body)
	if errDo != nil {
		// The request never reached the server, so re-sending cannot double-claim
		// (`crates/atomcode-codingplan/src/client.rs:221-224`).
		return nil, abiboot.RetryableError("transport", "领取 AtomCode 免费套餐失败：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, abiboot.HTTPError("AUTH", http.StatusUnauthorized, "AtomCode 登录态失效，请重新登录")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.HTTPError(httpErrorCode(response.StatusCode), response.StatusCode,
			"%s", classifyCodingPlanError("claim-v2", response.StatusCode, string(response.Body)))
	}
	var decoded claimResponse
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		return nil, abiboot.Errorf("codingplan_protocol", "解析 claim-v2 响应失败: %v", errUnmarshal)
	}
	return &decoded, nil
}

// claimCascade walks Max -> Pro -> Lite and stops at the first tier that is held
// or granted (`crates/atomcode-codingplan/src/setup.rs:825-918`).
//
// The stopping rules are the reference's, and each one matters:
//
//   - `duplicate:true` means the account already holds that tier or a higher
//     one. The cascade must STOP rather than continue to a lower tier, which
//     would report duplicate again or needlessly downgrade.
//   - `success:true` is a grant; stop.
//   - `success:false` is a per-tier refusal (quota, not eligible, not yet
//     open): remember the message and try the next tier down.
//   - a transport/5xx/parse failure aborts the whole cascade: retrying a lower
//     tier cannot fix a broken connection and would only add load.
func claimCascade(h *abiboot.Host, cfg Config, credential *Credential) claimOutcome {
	outcome := claimOutcome{Status: claimFailed, Attempts: []tierAttempt{}}
	lastMessage := ""
	for _, tier := range planCascadeOrder {
		response, errClaim := claimTier(h, cfg, credential, tier)
		if errClaim != nil {
			if code, ok := errorCodeOf(errClaim); ok && code == "AUTH" {
				outcome.Status = claimAuthRequired
				outcome.Message = errClaim.Error()
				outcome.Attempts = append(outcome.Attempts, tierAttempt{Tier: tier, Outcome: "auth-required"})
				return outcome
			}
			outcome.Attempts = append(outcome.Attempts, tierAttempt{Tier: tier, Outcome: "errored", Message: errClaim.Error()})
			outcome.Message = errClaim.Error()
			return outcome
		}
		switch {
		case response.Duplicate:
			outcome.Attempts = append(outcome.Attempts, tierAttempt{
				Tier: tier, Outcome: claimAlreadyHeld, Message: response.Message, PlanName: response.PlanName,
			})
			outcome.Status = claimAlreadyHeld
			outcome.Message = response.Message
			outcome.PlanName = response.PlanName
			return outcome
		case response.Success:
			outcome.Attempts = append(outcome.Attempts, tierAttempt{
				Tier: tier, Outcome: claimClaimed, Message: response.Message, PlanName: response.PlanName,
			})
			outcome.Status = claimClaimed
			outcome.Message = response.Message
			outcome.PlanName = response.PlanName
			return outcome
		default:
			outcome.Attempts = append(outcome.Attempts, tierAttempt{
				Tier: tier, Outcome: claimRefused, Message: response.Message, PlanName: response.PlanName,
			})
			lastMessage = response.Message
		}
	}
	outcome.Status = claimRefused
	outcome.Message = lastMessage
	return outcome
}

// quotaSummary renders the primary quota window for a status page.
//
// The layout is `used/limit calls per Nh, resets at HH:MM` because the free
// tier's quota is a call count per rolling window, not a token budget
// (measured 2026-10-01: `call_limit:200, window_hours:5`).
func quotaSummary(window rateLimitWindow) string {
	parts := []string{itoa64(window.CallsUsed) + "/" + itoa64(window.CallLimit) + " 次调用"}
	if window.WindowHours > 0 {
		parts = append(parts, "每 "+itoa(window.WindowHours)+" 小时")
	}
	if window.ResetAtDisplay != "" {
		parts = append(parts, "重置于 "+window.ResetAtDisplay)
	}
	if window.QuotaExhausted {
		parts = append(parts, "（已用尽）")
	}
	return strings.Join(parts, " · ")
}

// usageTotals sums the 60-day report into a display string.
func usageTotals(usage *usageResponse) string {
	if usage == nil {
		return ""
	}
	activeDays := 0
	for _, row := range usage.Rows {
		if row.TotalCounts > 0 {
			activeDays++
		}
	}
	return itoa64(usage.TotalCounts) + " 次调用 / " + itoa64(usage.TotalTokens) + " tokens / " +
		itoa(activeDays) + " 天有调用（" + usage.StartDate + " ~ " + usage.EndDate + "）"
}

// refreshLeadFor turns the configured window into a duration.
func refreshLeadFor(cfg Config) time.Duration {
	seconds := cfg.RefreshWindowSeconds
	if seconds <= 0 {
		seconds = RefreshWindowSeconds
	}
	return time.Duration(seconds) * time.Second
}
