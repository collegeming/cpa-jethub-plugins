package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Points: balance, the ONE-OFF desktop login reward, and the quota provider.
//
// ⚠️ There is NO daily check-in for this provider. The daily 300 points are
// granted by the server on its own — there is no endpoint for them
// (`raccoon-credits.ts:12-15`, trap #26). The only claimable item is the
// one-off desktop login reward, which is idempotent server-side through the
// `granted` flag and is exposed here as an EXPLICIT action, never as part of a
// "claim everything" path.

// balanceSnapshot is the `GET /points/v1/balance` answer.
//
// `available_points` is REQUIRED: when it is absent the read is reported as
// unknown rather than as 0, because 0 reads like "the points are gone"
// (`raccoon-credits.ts:143-155`, trap #11).
type balanceSnapshot struct {
	Available float64
	// Reward is 奖励积分; nil when the server did not send the pool.
	Reward *float64
	// Daily is 每日积分 (server-granted, refreshed daily).
	Daily *float64
	// Monthly is 会员积分; the ONLY pool with a positivity filter.
	Monthly *float64
	// Topup is 充值积分.
	Topup *float64
}

// pointPool is one named amount for display.
type pointPool struct {
	Name   string
	Amount float64
}

// pools renders the separate pools the reference keeps apart so a user can see
// that registration, daily and top-up credit are distinct sources with different
// expiry rules (`raccoon-credits.ts:106-125`).
func (s *balanceSnapshot) pools() []pointPool {
	if s == nil {
		return nil
	}
	out := []pointPool{{Name: "可用积分", Amount: s.Available}}
	if s.Reward != nil {
		out = append(out, pointPool{Name: "奖励积分", Amount: *s.Reward})
	}
	if s.Daily != nil {
		out = append(out, pointPool{Name: "每日积分", Amount: *s.Daily})
	}
	if s.Monthly != nil && *s.Monthly > 0 {
		out = append(out, pointPool{Name: "会员积分", Amount: *s.Monthly})
	}
	if s.Topup != nil {
		out = append(out, pointPool{Name: "充值积分", Amount: *s.Topup})
	}
	return out
}

// fetchBalance reads the point pools.
//
// A failed read returns an error, never a zero balance; a body without
// `available_points` returns `(nil, nil)`, which callers render as "unknown".
func fetchBalance(h *abiboot.Host, credential *Credential, cfg Config) (*balanceSnapshot, error) {
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.requestTimeout())*time.Millisecond,
		http.MethodGet, APIBase+PointsBalancePath, creditsHeaders(credential), nil)
	if errDo != nil {
		return nil, transportError("balance_transport", "读取 Raccoon 积分失败：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, credentialError("auth_expired", "Raccoon 凭据已失效（HTTP %d），请重新登录%s", response.StatusCode, credentialAdvice())
	}
	parsed, errEnvelope := parseEnvelope(response.Body, response.StatusCode)
	if errEnvelope != nil {
		return nil, errEnvelope
	}
	if !parsed.success() {
		return nil, envelopeFailure("读取 Raccoon 积分", parsed, true)
	}
	data := parsed.dataObject()
	available, okAvailable := numericField(data, "available_points")
	if !okAvailable {
		return nil, nil
	}
	snapshot := &balanceSnapshot{Available: available}
	snapshot.Reward = optionalNumber(data, "reward_points")
	snapshot.Daily = optionalNumber(data, "daily_points")
	snapshot.Monthly = optionalNumber(data, "monthly_points")
	snapshot.Topup = optionalNumber(data, "topup_points")
	return snapshot, nil
}

// optionalNumber reads a numeric field, returning nil when it is absent.
func optionalNumber(source map[string]any, key string) *float64 {
	value, ok := numericField(source, key)
	if !ok {
		return nil
	}
	return &value
}

// rewardStatus is the one-off reward's state, derived from the bill history.
type rewardStatus struct {
	// Claimed reports whether the desktop login reward is already granted.
	Claimed bool
	// Points is the amount to report; it defaults to LoginRewardPoints.
	Points int
	// Note explains a degraded read.
	Note string
}

// fetchRewardStatus derives the one-off reward state from `GET /points/v1/bills`.
//
// ⚠️ There is NO dedicated reward-status endpoint, and two traps are encoded in
// the scan (`raccoon-credits.ts:218-256`):
//
//   - the BALANCE cannot answer this: it aggregates registration, daily and
//     top-up credit;
//   - matching `biz_type === 'reward_grant'` ALONE is wrong, because the new-user
//     registration gift shares that type — the event name is what separates them.
//
// On any failure the answer is conservatively "not claimed": better to let the
// user press the button again (the server is idempotent) than to report claimed
// and make them miss the reward.
func fetchRewardStatus(h *abiboot.Host, credential *Credential, cfg Config) rewardStatus {
	fallback := rewardStatus{Claimed: false, Points: LoginRewardPoints}
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.requestTimeout())*time.Millisecond,
		http.MethodGet, APIBase+PointsBillsPath+PointsBillsQuery, creditsHeaders(credential), nil)
	if errDo != nil {
		fallback.Note = "账单读取失败：" + errDo.Error()
		return fallback
	}
	parsed, errEnvelope := parseEnvelope(response.Body, response.StatusCode)
	if errEnvelope != nil {
		fallback.Note = "账单读取失败：" + errEnvelope.Error()
		return fallback
	}
	if !parsed.success() {
		fallback.Note = "账单读取失败：" + parsed.text("")
		return fallback
	}
	data := parsed.dataObject()
	items, okItems := data["items"].([]any)
	if !okItems {
		fallback.Note = "账单响应没有 items 数组"
		return fallback
	}
	for _, candidate := range items {
		entry, okEntry := candidate.(map[string]any)
		if !okEntry {
			continue
		}
		if stringField(entry, "biz_type") != LoginRewardBizType {
			continue
		}
		if stringField(entry, "event_name") != LoginRewardEventName {
			continue
		}
		status := rewardStatus{Claimed: true, Points: LoginRewardPoints}
		if points, okPoints := numericField(entry, "points"); okPoints && points > 0 {
			status.Points = int(points)
		}
		return status
	}
	return fallback
}

// claimOutcome mirrors the reference's `ClaimOutcome` union
// (`credits.ts:118-122`).
type claimOutcome struct {
	// Kind is "claimed", "already-claimed", "inactive" or "failed".
	Kind string
	// Code is the upstream business code for a failure.
	Code int64
	// Message explains the outcome.
	Message string
	// Credit is the amount granted when Kind is "claimed".
	Credit int
}

// grantLoginReward performs the ONE-OFF `POST /desktop/v1/login/points/grant`.
//
// ⚠️ `X-Client-Platform` is REQUIRED: the endpoint identifies the caller as a
// desktop client and rejects the request without it
// (`raccoon-credits.ts:171-173`).
//
// ⚠️ The idempotency key is the server's `granted` flag: a repeat call answers
// HTTP 200 with `granted: false`, which must map to `already-claimed` and NOT to
// `claimed`, or the user is told points were added again
// (`raccoon-credits.ts:197-205`).
//
// The function never returns an error, so one account's failure cannot interrupt
// a sweep (`raccoon-credits.ts:174-175`).
func grantLoginReward(h *abiboot.Host, credential *Credential, cfg Config) claimOutcome {
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.requestTimeout())*time.Millisecond,
		http.MethodPost, APIBase+LoginPointsGrantPath, creditsHeaders(credential), nil)
	if errDo != nil {
		return claimOutcome{Kind: "failed", Message: "领取登录奖励失败：" + errDo.Error()}
	}
	parsed, errEnvelope := parseEnvelope(response.Body, response.StatusCode)
	if errEnvelope != nil {
		return claimOutcome{Kind: "failed", Message: "领取登录奖励失败：" + errEnvelope.Error()}
	}
	if !parsed.success() {
		return claimOutcome{Kind: "failed", Code: parsed.code(), Message: parsed.text("领取登录奖励失败")}
	}
	data := parsed.dataObject()
	granted, _ := data["granted"].(bool)
	if !granted {
		return claimOutcome{Kind: "already-claimed", Message: "该账号已领取过桌面端登录奖励（每号一次）"}
	}
	credit := LoginRewardPoints
	if popup, okPopup := data["popup"].(map[string]any); okPopup {
		if points, okPoints := numericField(popup, "points"); okPoints && points > 0 {
			credit = int(points)
		}
	}
	return claimOutcome{Kind: "claimed", Credit: credit, Message: "已领取桌面端登录奖励"}
}

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the supported provider and reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        "Raccoon 积分（可用 / 奖励 / 每日 / 充值）",
		// Nothing here can be reset: the daily points are server-granted and the
		// login reward is a one-off the server remembers.
		SupportsReset: false,
	}, nil
}

// handleQuotaFetch reports every pool separately.
//
// A failed read is an error and NEVER a zero balance (trap #11); a read that
// returns no `available_points` is reported as unavailable rather than as 0.
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
	snapshot, errBalance := fetchBalance(h, credential, settings())
	if errBalance != nil {
		return nil, errBalance
	}
	if snapshot == nil {
		return nil, transportError("balance_unavailable", "Raccoon 未返回 available_points 字段，无法给出积分数字")
	}
	summary := make([]pluginapi.QuotaMetric, 0, 5)
	for _, pool := range snapshot.pools() {
		summary = append(summary, pluginapi.QuotaMetric{
			Key:   quotaMetricKey(pool.Name),
			Label: pool.Name,
			Value: pool.Amount,
			Unit:  "积分",
			// Format keeps a whole number whole in the management UI.
			Format: "number",
		})
	}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// quotaMetricKey maps a pool name to a stable machine key.
func quotaMetricKey(name string) string {
	switch name {
	case "可用积分":
		return "available_points"
	case "奖励积分":
		return "reward_points"
	case "每日积分":
		return "daily_points"
	case "会员积分":
		return "monthly_points"
	case "充值积分":
		return "topup_points"
	default:
		return "points"
	}
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{
		Success: false,
		Message: "Raccoon 不支持重置积分：每日积分为服务端自动发放，登录奖励每号仅一次",
	}, nil
}

// trimAmount renders a point amount without a trailing `.0`.
func trimAmount(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// ── Per-account points ──

// accountQuota is ONE account's own point state.
//
// Snapshot is nil when the server published no balance (PointsNone marks that
// answer, which is a fact) and PointsErr carries a failed read. A failed read
// must render as 未知, never as 0.
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
	// Snapshot is the account's own point pools.
	Snapshot   *balanceSnapshot
	PointsNone bool
	PointsErr  error
}

// collectAccountQuotas reads the points of every account, in host order, one
// account at a time.
//
// Sequential on purpose: each account costs an upstream request, and one
// account's failure never stops the sweep nor hides another's figures.
func collectAccountQuotas(h *abiboot.Host, accounts []pluginapi.HostAuthFileEntry, cfg Config) []accountQuota {
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
		snapshot, errBalance := fetchBalance(h, credential, cfg)
		switch {
		case errBalance != nil:
			quota.PointsErr = errBalance
		case snapshot == nil:
			quota.PointsNone = true
		default:
			quota.Snapshot = snapshot
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
// The keys are exactly the ones the selected account publishes at the top level
// (points / points_error / points_note), so a consumer can read one entry of
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
	item["phone"] = quota.Credential.maskedPhone()
	item["userid"] = quota.Credential.UserID
	item["expired"] = quota.Credential.Expired(nowTime())
	item["needs_refresh"] = quota.Credential.NeedsRefresh(nowTime(), refreshWindow(settings()))
	if quota.Refresh.Refreshed {
		item["refreshed"] = true
	}
	if quota.Refresh.Err != nil {
		item["refresh_error"] = quota.Refresh.Err.Error()
	}
	if expiry := quota.Credential.Expiry(); !expiry.IsZero() {
		item["expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	switch {
	case quota.PointsErr != nil:
		item["points_error"] = quota.PointsErr.Error()
	case quota.PointsNone:
		item["points_note"] = "服务端未返回 available_points 字段"
	default:
		item["points"] = pointsJSON(quota.Snapshot)
	}
	return item
}

// pointsJSON renders the point pools the document publishes.
func pointsJSON(snapshot *balanceSnapshot) map[string]any {
	points := map[string]any{"available": snapshot.Available}
	if snapshot.Reward != nil {
		points["reward"] = *snapshot.Reward
	}
	if snapshot.Daily != nil {
		points["daily"] = *snapshot.Daily
	}
	if snapshot.Monthly != nil {
		points["monthly"] = *snapshot.Monthly
	}
	if snapshot.Topup != nil {
		points["topup"] = *snapshot.Topup
	}
	return points
}

// quotaListJSON renders every account's figures, in host order.
func quotaListJSON(quotas []accountQuota) []map[string]any {
	out := make([]map[string]any, 0, len(quotas))
	for _, quota := range quotas {
		out = append(out, quotaJSON(quota))
	}
	return out
}
