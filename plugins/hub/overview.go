package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// This file turns the hub's single menu route into a CHANNEL OVERVIEW: one row
// per provider CPA has a plugin for, carrying that provider's own mark, its
// account count and one line read out of its `status?format=json` document.
//
// Two properties are load bearing:
//
//   - the overview NEVER claims. The only request it makes is the provider's
//     read-only status document; no check-in parameter (`action=checkin` /
//     `action=claim`) is ever added, which is what makes it safe for a page that
//     an iframe mounts on every visit;
//   - a provider that is not installed is a ROW, not an error page. The host
//     answers 404 for a resource route nobody registered, and that is rendered
//     as 未安装/未启用 — the same answer CPA's own plugin list gives.
//
// The status documents are not uniform, so `channelStatusLine` reads only keys
// the corresponding provider's handler actually writes; every fact below names
// its provider and field. A key that is absent contributes nothing rather than a
// placeholder, and a document with no readable field says exactly that.

// channelState classifies what one probe learned about a provider.
type channelState string

const (
	// channelReady: the provider answered with a status document.
	channelReady channelState = "ready"
	// channelMissing: nothing is registered on this provider's resource path, so
	// the plugin is not installed (or not enabled) in this CPA.
	channelMissing channelState = "missing"
	// channelUnreadable: the route exists but no document could be read — a
	// failing provider endpoint or an unreachable host. The provider is still
	// listed, with the reason, instead of disappearing.
	channelUnreadable channelState = "unreadable"
)

// channelReport is one overview row.
type channelReport struct {
	Target target
	State  channelState
	// Accounts is how many credentials the host tracks under this provider key.
	// It is the same ledger the provider plugin itself reads, and the only
	// source that names every account.
	Accounts int
	// Reported is the account count the provider's own document carried, or -1
	// when its document does not carry one. A disagreement between the two
	// numbers is shown rather than hidden.
	Reported int
	// Status is the one-line summary of the provider's own document, or the
	// reason there is none: every row carries text, so a failed probe can never
	// render as a blank cell. Error holds the detail behind that reason.
	Status string
	// Error explains a state other than channelReady.
	Error string
}

// URL is the full page of this channel.
//
// It is an absolute path on purpose: the overview renders inside an iframe
// whose own URL is the hub's route, so a relative link would resolve against
// /v0/resource/plugins/hub/ and land on the hub again.
func (c channelReport) URL() string { return resourcePrefix + c.Target.ID + "/status" }

// channelReports probes every selected provider, in catalogue order.
//
// The probes run concurrently for one reason: the page is embedded in an iframe,
// and with the per-request deadline at its default a sequential sweep of ten
// providers would take ten deadlines to render whenever one of them is stuck.
// Concurrency bounds the wait to a single deadline. The reference panel behaves
// the same way — it queries every provider's state when it mounts.
func channelReports(h *abiboot.Host, cfg Config) []channelReport {
	entries := selectTargets(cfg)
	reports := make([]channelReport, len(entries))

	// The host's credential list is host-wide, so it is read once and shared by
	// every row instead of once per provider.
	ledger := hostLedger(h)

	runner := newRunner(h, cfg)
	var wait sync.WaitGroup
	for index, entry := range entries {
		wait.Add(1)
		go func(index int, entry target) {
			defer wait.Done()
			reports[index] = probeChannel(runner, entry, ledger[entry.ID])
		}(index, entry)
	}
	wait.Wait()
	return reports
}

// hostLedger groups the host's credentials by provider key.
//
// The filter is the same one every provider plugin uses to select its own
// accounts (`entry.Provider == key || entry.Type == key`), so an entry that
// matches under both fields is counted once and the overview cannot disagree
// with the provider's page about how many accounts exist.
func hostLedger(h *abiboot.Host) map[string][]accountRef {
	out := map[string][]accountRef{}
	if h == nil {
		return out
	}
	entries, errList := h.ListAuth()
	if errList != nil {
		return out
	}
	for _, entry := range entries {
		ref := accountRef{
			AuthIndex: entry.AuthIndex,
			Name:      entry.Name,
			Label:     entry.Label,
			Disabled:  entry.Disabled,
			Status:    entry.Status,
		}
		keys := map[string]bool{}
		if key := strings.TrimSpace(entry.Provider); key != "" {
			keys[key] = true
		}
		if key := strings.TrimSpace(entry.Type); key != "" {
			keys[key] = true
		}
		for key := range keys {
			out[key] = append(out[key], ref)
		}
	}
	return out
}

// probeChannel reads one provider's status document.
//
// The request is built by fetchStatusJSON, which asks for `format=json` on
// `/status` and nothing else — no action parameter, so no provider can read this
// as a claim.
func probeChannel(r *runner, entry target, ledger []accountRef) channelReport {
	report := channelReport{Target: entry, Accounts: len(ledger), Reported: -1}

	response, document, errStatus := r.fetchStatusJSON(entry, "")
	switch {
	case response != nil && response.StatusCode == http.StatusNotFound:
		report.State = channelMissing
		report.Status = "未安装或未启用"
		return report
	case errStatus != nil:
		report.State = channelUnreadable
		report.Error = errStatus.Error()
		report.Status = "状态不可读"
		return report
	}

	report.State = channelReady
	if reported, _, ok := reportedAccounts(document); ok {
		report.Reported = reported
	}
	report.Status = channelStatusLine(document)
	return report
}

// ── The one-line summary ──

// channelStatusLine renders what ONE provider's status document says, as a
// single line.
//
// Facts are collected in a fixed order — upstream error first (it explains the
// rest), then the empty-account note, check-in state, account state, credit,
// expiry and finally the model catalogue — so two providers can be read side by
// side. The line stays a line: at most five facts, with the least specific ones
// dropped. Each builder below documents the provider whose handler writes the
// fields it reads.
func channelStatusLine(document map[string]any) string {
	if document == nil {
		return "provider 未返回 JSON 文档"
	}
	facts := make([]string, 0, 5)
	facts = append(facts, upstreamErrorFacts(document)...)
	facts = append(facts, emptyAccountFacts(document)...)
	facts = append(facts, checkinStateFacts(document)...)
	facts = append(facts, accountFacts(document)...)
	facts = append(facts, creditFacts(document)...)
	facts = append(facts, expiryFacts(document)...)
	facts = append(facts, modelCountFacts(document)...)
	facts = dedupeFacts(facts, 5)
	if len(facts) == 0 {
		return "provider 只返回了本页不展示的配置字段"
	}
	return strings.Join(facts, " · ")
}

// emptyAccountFacts reports a provider document that carries a zero account
// count and nothing else to say.
//
// It exists because that is the normal state of a freshly installed provider:
// qoder, loomy, trae, cline and codebuddy all answer with their configuration
// fields, `accounts: 0` and no account block at all, so the row would otherwise
// show two empty cells. The zero is read with the same shape detection the run
// uses (reportedAccounts), so this cannot disagree with the check-in table.
func emptyAccountFacts(document map[string]any) []string {
	count, _, present := reportedAccounts(document)
	if !present || count != 0 {
		return nil
	}
	return []string{"尚无账号"}
}

// modelCountFacts reports how many models the provider's own document says it
// exposes.
//
//	codebuddy / qoder / loomy   model_count
//	cline                       cached_model_count (after discovery) or
//	                            static_model_count (the built-in catalogue)
func modelCountFacts(document map[string]any) []string {
	if count, ok := numberAt(document, "model_count"); ok {
		return []string{fmt.Sprintf("模型 %d", int(count))}
	}
	if count, ok := numberAt(document, "cached_model_count"); ok {
		return []string{fmt.Sprintf("模型 %d（已发现）", int(count))}
	}
	if count, ok := numberAt(document, "static_model_count"); ok {
		return []string{fmt.Sprintf("模型 %d（静态）", int(count))}
	}
	return nil
}

// checkinStateFacts reports the daily check-in state each provider exposes.
//
//	trae        daily_checkin.{checked_in,streak_days}
//	codearts    daily_checkin.{claimable,status}
//	qoder       daily_checkin.claimable
//	codebuddy   checkin.{today_checked_in,streak_days}
//	lobsterai   selected.activity.slot_state
func checkinStateFacts(document map[string]any) []string {
	facts := make([]string, 0, 2)
	if checked, ok := flagAt(document, "daily_checkin.checked_in"); ok {
		facts = append(facts, claimStateText(checked))
	}
	if claimable, ok := flagAt(document, "daily_checkin.claimable"); ok {
		facts = append(facts, claimableText(claimable))
	} else if status := stringAt(document, "daily_checkin.status"); status != "" {
		facts = append(facts, "签到状态 "+status)
	}
	if checked, ok := flagAt(document, "checkin.today_checked_in"); ok {
		facts = append(facts, claimStateText(checked))
	}
	if streak, ok := numberAt(document, "daily_checkin.streak_days"); ok && streak > 0 {
		facts = append(facts, fmt.Sprintf("连续签到 %d 天", int(streak)))
	}
	if streak, ok := numberAt(document, "checkin.streak_days"); ok && streak > 0 {
		facts = append(facts, fmt.Sprintf("连续签到 %d 天", int(streak)))
	}
	if slot := stringAt(document, "selected.activity.slot_state"); slot != "" {
		facts = append(facts, "签到时段 "+slot)
	}
	return facts
}

// claimStateText renders a provider's own "already taken today" boolean. Both
// spellings come from the providers: trae's `checked_in` and codebuddy's
// `today_checked_in` mean the same thing.
func claimStateText(checked bool) string {
	if checked {
		return "今日已签到"
	}
	return "今日未签到"
}

// claimableText renders codearts' and qoder's `claimable` flag.
func claimableText(claimable bool) string {
	if claimable {
		return "今日可领取"
	}
	return "今日不可领取"
}

// creditFacts reports the credit or balance field each provider returns.
//
//	codebuddy    credits.total
//	qoder        credits.total
//	lobsterai    selected.credit.total
//	trae         credits                      (a bare number, not an object)
//	codearts     remaining / total
//	loomy        points.balance / points.daily_quota
func creditFacts(document map[string]any) []string {
	facts := make([]string, 0, 2)
	if total, ok := numberAt(document, "credits.total"); ok {
		facts = append(facts, "积分 "+trimFloat(total))
	} else if total, ok := numberAt(document, "selected.credit.total"); ok {
		facts = append(facts, "积分 "+trimFloat(total))
	} else if total, ok := numberAt(document, "credits"); ok {
		facts = append(facts, "积分 "+trimFloat(total))
	}
	if remaining, ok := numberAt(document, "remaining"); ok {
		if total, okTotal := numberAt(document, "total"); okTotal {
			facts = append(facts, fmt.Sprintf("剩余 %s / %s", trimFloat(remaining), trimFloat(total)))
		} else {
			facts = append(facts, "剩余 "+trimFloat(remaining))
		}
	}
	if balance, ok := numberAt(document, "points.balance"); ok {
		facts = append(facts, "积分余额 "+trimFloat(balance))
	}
	if quota, ok := numberAt(document, "points.daily_quota"); ok {
		facts = append(facts, "每日额度 "+trimFloat(quota))
	}
	return facts
}

// expiryFacts reports the credential expiry the document carries, if any.
//
//	codebuddy / qoder / cline / loomy   expires_at          (RFC3339)
//	lobsterai                           selected.expires_at
//	trae                                accounts.0.expires_at_ms (milliseconds)
//	trae / lobsterai / loomy            expired             (a boolean)
func expiryFacts(document map[string]any) []string {
	facts := make([]string, 0, 1)
	for _, path := range []string{"expires_at", "account.expires_at", "selected.expires_at"} {
		if raw := stringAt(document, path); raw != "" {
			facts = append(facts, "有效期至 "+readableTime(raw))
			break
		}
	}
	for _, path := range []string{"expires_at_ms", "account.expires_at_ms", "accounts.0.expires_at_ms"} {
		if millis, ok := numberAt(document, path); ok && millis > 0 {
			facts = append(facts, "有效期至 "+time.UnixMilli(int64(millis)).Local().Format("2006-01-02 15:04"))
			break
		}
	}
	for _, path := range []string{"expired", "account.expired", "selected.expired", "accounts.0.expired"} {
		if expired, ok := flagAt(document, path); ok && expired {
			facts = append(facts, "凭据已过期")
			break
		}
	}
	return facts
}

// accountFacts reports the account state the document carries.
//
//	codebuddy                      status         (the selected account's state)
//	qoder / cline / loomy          account.status
//	lobsterai                      selected.status
func accountFacts(document map[string]any) []string {
	for _, path := range []string{"status", "account.status", "selected.status"} {
		if status := stringAt(document, path); status != "" {
			return []string{"账号状态 " + status}
		}
	}
	return nil
}

// upstreamErrorFacts surfaces the errors and notes a provider puts in its own
// document instead of failing the request.
//
//	cline        account.error
//	lobsterai    selected.error, selected.credit_error, selected.activity_error
//	qoder        credit_error, credit_note
//	loomy        points_error, points_note
//	codebuddy    error
//
// The note fields are the provider's own words for "there is nothing to show
// here" (qoder: 企业版账号不下发额度数字), so they are passed through verbatim.
func upstreamErrorFacts(document map[string]any) []string {
	facts := make([]string, 0, 2)
	for _, field := range []struct {
		path  string
		label string
	}{
		{"error", "错误"},
		{"account.error", "账号不可读"},
		{"selected.error", "账号不可读"},
		{"credit_error", "积分读取失败"},
		{"selected.credit_error", "积分读取失败"},
		{"activity_error", "签到活动读取失败"},
		{"points_error", "积分读取失败"},
	} {
		if message := stringAt(document, field.path); message != "" {
			facts = append(facts, field.label+"："+message)
		}
	}
	for _, path := range []string{"credit_note", "points_note"} {
		if note := stringAt(document, path); note != "" {
			facts = append(facts, note)
		}
	}
	return facts
}

// readableTime renders an RFC3339 timestamp a provider returned in local time,
// and falls back to the provider's own string when it cannot be parsed: the
// value shown is always the provider's, never a guess.
func readableTime(raw string) string {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05"} {
		if parsed, errParse := time.Parse(layout, raw); errParse == nil {
			return parsed.Local().Format("2006-01-02 15:04")
		}
	}
	return raw
}
