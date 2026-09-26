package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file drives the run: enumerate the credentials of every target, ask each
// provider's own resource route to check in, and normalise the verdicts into one
// table. It never invents an outcome:
//
//   - a provider that reports `status`/`outcome` in its JSON has that field
//     mapped onto the shared vocabulary (claimed / already-claimed / inactive /
//     failed) while the raw value is kept in the row for display;
//   - codearts has no JSON representation of its claim at all, so its verdict is
//     derived from the provider's own post-claim `?format=json` state (and, when
//     that is unreadable, from the notice the provider itself rendered);
//   - anything that cannot be established is reported as unknown with the reason,
//     never as success.

// ── Result model ──

// rowKind is the normalised verdict vocabulary, shared by every provider so the
// aggregated table is comparable. It is NOT a translation of the provider's
// message: the raw provider value always travels next to it in ProviderStatus.
type rowKind string

const (
	kindClaimed        rowKind = "claimed"
	kindAlreadyClaimed rowKind = "already-claimed"
	kindInactive       rowKind = "inactive"
	kindUnsupported    rowKind = "unsupported"
	kindUnavailable    rowKind = "unavailable"
	kindFailed         rowKind = "failed"
	kindUnknown        rowKind = "unknown"
)

// row is one line of the aggregated table.
type row struct {
	Provider       string  `json:"provider"`
	ProviderLabel  string  `json:"provider_label"`
	AuthIndex      string  `json:"auth_index,omitempty"`
	Account        string  `json:"account,omitempty"`
	Result         rowKind `json:"result"`
	ResultText     string  `json:"result_text"`
	ProviderStatus string  `json:"provider_status,omitempty"`
	Message        string  `json:"message,omitempty"`
	Request        string  `json:"request,omitempty"`
	HTTPStatus     int     `json:"http_status,omitempty"`
}

// verdictText renders the normalised kind for the table.
func verdictText(kind rowKind) string {
	switch kind {
	case kindClaimed:
		return "已领取"
	case kindAlreadyClaimed:
		return "今日已签到"
	case kindInactive:
		return "不可领取"
	case kindUnsupported:
		return "不支持"
	case kindUnavailable:
		return "未启用"
	case kindFailed:
		return "失败"
	default:
		return "未知"
	}
}

// verdictTone maps the kind onto a plugui notice/badge tone.
func verdictTone(kind rowKind) string {
	switch kind {
	case kindClaimed:
		return "success"
	case kindAlreadyClaimed:
		return ""
	case kindInactive, kindUnavailable:
		return "warning"
	case kindUnsupported:
		return ""
	case kindFailed:
		return "danger"
	default:
		return "warning"
	}
}

// accountRef names one credential the host tracks.
type accountRef struct {
	AuthIndex string
	Name      string
	Label     string
	Disabled  bool
	Status    string
}

// display is the label the table shows for the account.
func (a accountRef) display() string {
	switch {
	case strings.TrimSpace(a.Label) != "":
		return strings.TrimSpace(a.Label)
	case strings.TrimSpace(a.Name) != "":
		return strings.TrimSpace(a.Name)
	case strings.TrimSpace(a.AuthIndex) != "":
		return strings.TrimSpace(a.AuthIndex)
	default:
		return "（未命名凭据）"
	}
}

// providerRun is the per-provider section of a run.
type providerRun struct {
	Target target `json:"-"`
	// Accounts is how many credentials the hub enumerated for this provider.
	Accounts int `json:"accounts"`
	// Reported is the account count the provider's own status JSON reported, or
	// -1 when its document does not carry one. It exists so a disagreement
	// between the host's credential list and the provider's own view is visible
	// instead of hidden.
	Reported int   `json:"reported_accounts"`
	Rows     []row `json:"rows"`
	// Error carries a provider-level failure (enumeration or transport) or a
	// disagreement between the provider's own account count and the host's
	// credential list. Either way it is shown instead of being swallowed.
	Error string `json:"error,omitempty"`
}

// runResult is one full one-click execution.
type runResult struct {
	BaseURL    string        `json:"base_url"`
	StartedAt  time.Time     `json:"-"`
	FinishedAt time.Time     `json:"-"`
	Providers  []providerRun `json:"providers"`
}

// rows flattens every provider section into the aggregated table order.
func (r *runResult) rows() []row {
	if r == nil {
		return nil
	}
	out := make([]row, 0, len(r.Providers))
	for _, entry := range r.Providers {
		out = append(out, entry.Rows...)
	}
	return out
}

// summary counts the normalised verdicts. "total" is the number of rows.
func (r *runResult) summary() map[string]int {
	out := map[string]int{
		string(kindClaimed):        0,
		string(kindAlreadyClaimed): 0,
		string(kindInactive):       0,
		string(kindUnsupported):    0,
		string(kindUnavailable):    0,
		string(kindFailed):         0,
		string(kindUnknown):        0,
		"total":                    0,
	}
	for _, item := range r.rows() {
		out[string(item.Result)]++
		out["total"]++
	}
	return out
}

// accountCounts returns the last known account count per provider id.
func (r *runResult) accountCounts() map[string]int {
	out := map[string]int{}
	if r == nil {
		return out
	}
	for _, entry := range r.Providers {
		out[entry.Target.ID] = entry.Accounts
	}
	return out
}

// duration is the wall-clock time the run took.
func (r *runResult) duration() time.Duration {
	if r == nil || r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// ── Last-run cache ──

var lastRunSlot atomic.Value // holds *runResult

// rememberLastRun stores the snapshot the status page reports as "last known".
func rememberLastRun(result *runResult) { lastRunSlot.Store(result) }

// lastRun returns the previous successful run, or nil when none happened yet.
//
// The status page only ever shows THIS: a bare page load must not perform a
// claim and must not claim anything it has not observed, so it does not read
// live state either. `?action=checkin` refreshes it.
func lastRun() *runResult {
	if value, ok := lastRunSlot.Load().(*runResult); ok {
		return value
	}
	return nil
}

// forgetLastRun drops the snapshot (plugin shutdown).
func forgetLastRun() { lastRunSlot.Store((*runResult)(nil)) }

// ── Transport ──

// runner executes one run against the host.
type runner struct {
	h       *abiboot.Host
	cfg     Config
	do      doer
	timeout time.Duration
}

// newRunner builds a runner from the live settings.
func newRunner(h *abiboot.Host, cfg Config) *runner {
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	if timeout < 0 {
		timeout = 0
	}
	return &runner{h: h, cfg: cfg, do: transportFor(h), timeout: timeout}
}

// boundedCall enforces the configured per-request deadline.
//
// The host's `host.http.do` callback takes no timeout and cannot be cancelled,
// so the deadline is enforced here by abandoning the call: the goroutine that
// owns it finishes in the background and its response is discarded. That is
// enough to keep a single stuck loopback request from blocking the whole run.
func boundedCall(timeout time.Duration, call func() (*pluginapi.HTTPResponse, error)) (*pluginapi.HTTPResponse, error) {
	if timeout <= 0 {
		return call()
	}
	type outcome struct {
		response *pluginapi.HTTPResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, errCall := call()
		done <- outcome{response: response, err: errCall}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.response, result.err
	case <-timer.C:
		return nil, abiboot.Errorf("request_timeout", "请求超时（%s）", timeout)
	}
}

// get performs one GET through the host transport.
func (r *runner) get(rawURL, accept string) (*pluginapi.HTTPResponse, error) {
	headers := http.Header{}
	if strings.TrimSpace(accept) != "" {
		headers.Set("Accept", accept)
	}
	response, errCall := boundedCall(r.timeout, func() (*pluginapi.HTTPResponse, error) {
		call, errBuild := r.do(http.MethodGet, rawURL, headers, nil)
		if errBuild != nil {
			return nil, errBuild
		}
		if call == nil {
			return nil, abiboot.Errorf("empty_response", "宿主返回空响应：%s", rawURL)
		}
		return call, nil
	})
	return response, errCall
}

// fetchStatusJSON reads one provider's status document.
//
// The response is returned even for a non-2xx status so the caller can tell
// "plugin not installed" (the host answers 404 for an unregistered resource
// route) apart from "provider said no".
func (r *runner) fetchStatusJSON(t target, authIndex string) (*pluginapi.HTTPResponse, map[string]any, error) {
	query := url.Values{"format": {"json"}}
	if strings.TrimSpace(authIndex) != "" {
		query.Set("auth_index", authIndex)
	}
	rawURL := resourceURL(r.cfg.HostBaseURL, t.ID, "/status", query)
	response, errCall := r.get(rawURL, "application/json")
	if errCall != nil {
		return nil, nil, errCall
	}
	document, errParse := parseJSONObject(response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, document, abiboot.Errorf("status_http",
			"状态页返回 HTTP %d：%s", response.StatusCode, snippet(response.Body, 160))
	}
	if errParse != nil {
		return response, nil, errParse
	}
	return response, document, nil
}

// ── Account enumeration ──

// accounts resolves the credentials of one provider.
//
// The host's credential list is the ledger, because it is the same list every
// provider plugin itself reads (`host.auth.list`, filtered by provider key), and
// it is the only source that names EVERY account: qoder, loomy, codearts and
// trae expose just the selected account plus a count in their status JSON, so
// their document cannot enumerate anything. The provider's own document is
// parsed as well and its account list is used when the host list comes back
// empty, so a provider-key mismatch degrades to a reported discrepancy instead
// of a silent "no accounts".
func (r *runner) accounts(t target, statusDocument map[string]any) ([]accountRef, int) {
	enumerated := r.hostAccounts(t.ID)
	reported, reportedRefs, hasReported := reportedAccounts(statusDocument)

	if len(enumerated) == 0 && len(reportedRefs) > 0 {
		enumerated = reportedRefs
	}
	count := -1
	if hasReported {
		count = reported
	}
	return enumerated, count
}

// hostAccounts returns the host's credentials for one provider key, in host
// order. Every provider plugin selects its accounts with exactly this filter
// (`entry.Provider == key || entry.Type == key`).
func (r *runner) hostAccounts(providerKey string) []accountRef {
	if r.h == nil {
		return nil
	}
	entries, errList := r.h.ListAuth()
	if errList != nil {
		return nil
	}
	out := make([]accountRef, 0, len(entries))
	for _, entry := range entries {
		if entry.Provider != providerKey && entry.Type != providerKey {
			continue
		}
		out = append(out, accountRef{
			AuthIndex: entry.AuthIndex,
			Name:      entry.Name,
			Label:     entry.Label,
			Disabled:  entry.Disabled,
			Status:    entry.Status,
		})
	}
	return out
}

// reportedAccounts extracts the account information a provider's own status
// document carries. The documents are not uniform, which is why this walks
// several shapes:
//
//	qoder / loomy      {"accounts": <count>, "account": {auth_index, name}}
//	codearts           {"auth_index","name",...}          (flat, only when non-empty)
//	trae               {"account_count": n, "accounts": [ {auth_index, name} ]}
//	codebuddy          {"account_count": n, "accounts": [ {auth_index, name, label} ]}
//	lobsterai          {"accounts": [ {auth_index, name, label} ]}
//
// It returns the reported count, the accounts when the document lists them, and
// whether a count could be established at all.
func reportedAccounts(document map[string]any) (int, []accountRef, bool) {
	if document == nil {
		return 0, nil, false
	}
	var (
		count   = -1
		refs    []accountRef
		present bool
	)
	if items, ok := arrayField(document, "accounts"); ok {
		present = true
		count = len(items)
		refs = make([]accountRef, 0, len(items))
		for _, item := range items {
			object, okObject := item.(map[string]any)
			if !okObject {
				continue
			}
			ref := accountRef{
				AuthIndex: stringField(object, "auth_index"),
				Name:      stringField(object, "name"),
				Label:     stringField(object, "label"),
			}
			if disabled, okDisabled := boolField(object, "disabled"); okDisabled {
				ref.Disabled = disabled
			}
			if ref.AuthIndex == "" && ref.Name == "" {
				continue
			}
			refs = append(refs, ref)
		}
	}
	if value, okNumber := numberField(document, "accounts"); okNumber && !present {
		present = true
		count = int(value)
	}
	if value, okNumber := numberField(document, "account_count"); okNumber && !present {
		present = true
		count = int(value)
	}
	if account, okAccount := objectField(document, "account"); okAccount {
		ref := accountRef{
			AuthIndex: stringField(account, "auth_index"),
			Name:      stringField(account, "name"),
			Label:     stringField(account, "label"),
		}
		if ref.AuthIndex != "" {
			present = true
			if count < 0 {
				count = 1
			}
			refs = appendUniqueAccount(refs, ref)
		}
	}
	// codearts answers with the account fields at the top level (its empty case
	// is the only one that nests them under "account").
	if ref := (accountRef{
		AuthIndex: stringField(document, "auth_index"),
		Name:      stringField(document, "name"),
		Label:     stringField(document, "label"),
	}); ref.AuthIndex != "" {
		present = true
		if count < 1 {
			count = 1
		}
		refs = appendUniqueAccount(refs, ref)
	}
	return count, refs, present
}

func appendUniqueAccount(refs []accountRef, ref accountRef) []accountRef {
	for _, existing := range refs {
		if existing.AuthIndex == ref.AuthIndex {
			return refs
		}
	}
	return append(refs, ref)
}

// ── The run ──

// run performs the one-click check-in for every selected target, one request at
// a time (a slow provider therefore cannot exhaust the host's connections).
func (r *runner) run() *runResult {
	result := &runResult{BaseURL: r.cfg.HostBaseURL, StartedAt: time.Now()}
	for _, entry := range selectTargets(r.cfg) {
		result.Providers = append(result.Providers, r.runTarget(entry))
	}
	result.FinishedAt = time.Now()
	return result
}

// runTarget drives every account of one provider.
func (r *runner) runTarget(t target) providerRun {
	section := providerRun{Target: t, Reported: -1}

	if !t.supportsCheckin() {
		section.Rows = append(section.Rows, row{
			Provider: t.ID, ProviderLabel: t.Label,
			Result: kindUnsupported, ResultText: verdictText(kindUnsupported),
			Message: t.Note,
		})
		return section
	}

	response, document, errStatus := r.fetchStatusJSON(t, "")
	if errStatus != nil {
		// A 404 here means the resource mount has no such route: the provider
		// plugin is not installed, not enabled, or too old to register one.
		section.Error = errStatus.Error()
		kind := kindFailed
		if response != nil && response.StatusCode == http.StatusNotFound {
			kind = kindUnavailable
		}
		section.Rows = append(section.Rows, row{
			Provider: t.ID, ProviderLabel: t.Label,
			Result: kind, ResultText: verdictText(kind),
			Message: errStatus.Error(),
			Request: resourceURL(r.cfg.HostBaseURL, t.ID, "/status", url.Values{"format": {"json"}}),
		})
		return section
	}

	accounts, reported := r.accounts(t, document)
	section.Accounts = len(accounts)
	section.Reported = reported
	if reported >= 0 && reported != len(accounts) {
		section.Error = fmt.Sprintf("provider 状态页报告 %d 个账号，宿主凭据列表给出 %d 个", reported, len(accounts))
	}

	if len(accounts) == 0 {
		section.Rows = append(section.Rows, row{
			Provider: t.ID, ProviderLabel: t.Label,
			Result: kindUnavailable, ResultText: "无账号",
			Message: "该 provider 下没有可用账号",
		})
		return section
	}

	for _, account := range accounts {
		if strings.TrimSpace(account.AuthIndex) == "" {
			// Without an index the provider's selectAccount() falls back to its
			// FIRST account, so driving this entry would claim somebody else's
			// account once per listing. Refuse instead.
			section.Rows = append(section.Rows, row{
				Provider: t.ID, ProviderLabel: t.Label,
				Account: account.display(),
				Result:  kindUnavailable, ResultText: "缺少索引",
				Message: "该凭据没有 auth_index（宿主运行时索引不可用），无法精确指定账号，已跳过以免误签到其它账号",
			})
			continue
		}
		if account.Disabled {
			section.Rows = append(section.Rows, row{
				Provider: t.ID, ProviderLabel: t.Label,
				AuthIndex: account.AuthIndex, Account: account.display(),
				Result: kindUnavailable, ResultText: "已停用",
				Message: "账号在宿主中被停用，跳过",
			})
			continue
		}
		if t.Support == supportStatusHTML {
			section.Rows = append(section.Rows, r.runStatusPageCheckin(t, account))
			continue
		}
		section.Rows = append(section.Rows, r.runJSONCheckin(t, account))
	}
	return section
}

// checkinURL builds the exact request the driver sends for one account: the
// provider route, the parameters that provider needs to actually write, the
// credential selector and — for the JSON providers — `format=json`.
//
// The status-page driver deliberately gets no `format=json`: on codearts that
// parameter selects a representation which skips the claim.
func (r *runner) checkinURL(t target, account accountRef) string {
	query := url.Values{}
	for key, values := range t.CheckinQuery {
		for _, value := range values {
			query.Add(key, value)
		}
	}
	query.Set("auth_index", account.AuthIndex)
	if t.Support != supportStatusHTML {
		query.Set("format", "json")
	}
	return resourceURL(r.cfg.HostBaseURL, t.ID, t.CheckinPath, query)
}

// runJSONCheckin drives a provider whose `/checkin` resource route answers JSON.
func (r *runner) runJSONCheckin(t target, account accountRef) row {
	line := row{
		Provider: t.ID, ProviderLabel: t.Label,
		AuthIndex: account.AuthIndex, Account: account.display(),
	}
	rawURL := r.checkinURL(t, account)
	line.Request = rawURL

	response, errCall := r.get(rawURL, "application/json")
	if errCall != nil {
		line.Result, line.ResultText, line.Message = kindFailed, verdictText(kindFailed), errCall.Error()
		return line
	}
	line.HTTPStatus = response.StatusCode

	if response.StatusCode == http.StatusNotFound {
		// The provider registered no such resource route: either the product has
		// no check-in endpoint at all (codebuddy builds without one) or the
		// installed plugin is too old to expose it. Say exactly that.
		line.Result = kindUnsupported
		line.ResultText = verdictText(kindUnsupported)
		line.Message = "该 provider 未注册此资源路由（HTTP 404）：产品或版本不支持签到"
		return line
	}

	document, errParse := parseJSONObject(response.Body)
	if errParse != nil {
		line.Result = kindUnknown
		line.ResultText = verdictText(kindUnknown)
		line.Message = fmt.Sprintf("响应不是 JSON（HTTP %d）：%s", response.StatusCode, snippet(response.Body, 160))
		return line
	}

	verdict := interpretCheckinJSON(document)
	line.ProviderStatus = verdict.ProviderStatus
	line.Message = verdict.Message
	if len(verdict.Facts) > 0 {
		line.Message = strings.TrimSpace(line.Message + "；" + strings.Join(verdict.Facts, "，"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The status code decides severity, the body still supplies the words:
		// every provider in this repository answers 200 for a repeat claim, so a
		// non-2xx really is an error.
		line.Result = kindFailed
		if verdict.Kind == kindUnsupported {
			line.Result = kindUnsupported
		}
		line.ResultText = verdictText(line.Result)
		if line.Message == "" {
			line.Message = snippet(response.Body, 160)
		}
		return line
	}
	line.Result = verdict.Kind
	line.ResultText = verdictText(verdict.Kind)
	return line
}

// runStatusPageCheckin drives codearts, whose claim exists only as
// `GET /status?action=checkin` in the HTML representation.
//
// The provider's `?format=json` status document does NOT run the claim (its
// statusJSON branch is taken before the action is looked at), so passing
// format=json here would silently do nothing. The driver therefore:
//
//  1. reads the pre-claim JSON state,
//  2. calls the status page with `action=checkin` and asks for HTML,
//  3. re-reads the JSON state and reports the provider's own post-claim values,
//  4. falls back to the notice the provider rendered when the JSON state is
//     unavailable, and never upgrades an unreadable result to "claimed".
func (r *runner) runStatusPageCheckin(t target, account accountRef) row {
	line := row{
		Provider: t.ID, ProviderLabel: t.Label,
		AuthIndex: account.AuthIndex, Account: account.display(),
	}
	_, before, errBefore := r.fetchStatusJSON(t, account.AuthIndex)

	rawURL := r.checkinURL(t, account)
	line.Request = rawURL

	// Accept text/html is load-bearing: wantsJSON() in the provider treats any
	// Accept that does not mention text/html as a JSON request, and the JSON
	// branch skips the claim entirely.
	response, errCall := r.get(rawURL, "text/html")
	if errCall != nil {
		line.Result, line.ResultText, line.Message = kindFailed, verdictText(kindFailed), errCall.Error()
		return line
	}
	line.HTTPStatus = response.StatusCode
	noticeTone, noticeText := noticeFromHTML(string(response.Body))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		line.Result, line.ResultText = kindFailed, verdictText(kindFailed)
		line.Message = fmt.Sprintf("状态页返回 HTTP %d：%s", response.StatusCode, snippet(response.Body, 160))
		return line
	}

	_, after, errAfter := r.fetchStatusJSON(t, account.AuthIndex)
	if errAfter != nil && errBefore != nil {
		// Without either machine-readable state the provider's own notice is the
		// only evidence; report it as such.
		line.Result = noticeKind(noticeTone)
		line.ResultText = verdictText(line.Result)
		line.Message = noticeText
		if line.Message == "" {
			line.Message = "签到页未渲染结果，且状态 JSON 不可读：" + errAfter.Error()
		}
		return line
	}

	beforeState := dailyActivityState(before)
	afterState := dailyActivityState(after)

	kind := kindUnknown
	message := ""

	switch {
	case afterState.present && afterState.claimed():
		// The provider says the benefit is taken. Whether it was taken by THIS
		// call or earlier is decided by the pre-claim state, and by the notice
		// tone when the pre-state was unreadable.
		line.ProviderStatus = afterState.status
		switch {
		case beforeState.present:
			if beforeState.claimed() && !beforeState.claimable {
				kind = kindAlreadyClaimed
			} else {
				kind = kindClaimed
			}
		default:
			kind = noticeKind(noticeTone)
			if kind != kindClaimed && kind != kindAlreadyClaimed {
				kind = kindAlreadyClaimed
			}
		}
		message = firstNonEmpty(noticeText, "签到成功")
		if gain := balanceGain(before, after); gain != "" {
			message = message + "；" + gain
		} else if afterState.remainingKnown {
			message = fmt.Sprintf("%s；剩余 %s", message, trimFloat(afterState.remaining))
		}
	case afterState.present:
		// The post-claim state was readable, but it does not say the benefit was
		// taken. The provider's own notice is the direct answer to THIS claim, so
		// it decides when it disagrees — and the disagreement is shown rather
		// than silently picking one side.
		line.ProviderStatus = afterState.status
		switch noticeKind(noticeTone) {
		case kindClaimed:
			kind = kindClaimed
			message = firstNonEmpty(noticeText, "签到页显示成功") +
				"；provider 返回的 daily_checkin.status=" + orPlaceholder(afterState.status) + "（状态读取可能滞后）"
		case kindFailed:
			kind = kindFailed
			message = firstNonEmpty(noticeText,
				"签到页显示失败（provider daily_checkin.status="+orPlaceholder(afterState.status)+"）")
		default:
			kind = kindInactive
			message = firstNonEmpty(noticeText,
				"活动当前不可领取（provider daily_checkin.status="+orPlaceholder(afterState.status)+"）")
		}
	case beforeState.present && beforeState.claimed() && !beforeState.claimable:
		// The post-state is unreadable, but the pre-state already said today's
		// benefit was taken. Nothing was gained by this call, and reporting
		// "claimed" would be a lie.
		line.ProviderStatus = beforeState.status
		kind = kindAlreadyClaimed
		message = firstNonEmpty(noticeText, "签到前状态即显示今日已签到")
	default:
		kind = noticeKind(noticeTone)
		switch {
		case kind == kindUnknown && errAfter != nil:
			message = firstNonEmpty(noticeText, "状态 JSON 不可读："+errAfter.Error())
		case kind == kindUnknown:
			message = firstNonEmpty(noticeText, "provider 状态页未返回 daily_checkin 字段，无法判定")
		default:
			message = firstNonEmpty(noticeText, "已按签到页渲染的提示判定")
		}
	}

	line.Result = kind
	line.ResultText = verdictText(kind)
	line.Message = message
	return line
}

// dailyState is the check-in state the codearts status document exposes.
type dailyState struct {
	present        bool
	claimable      bool
	status         string
	remaining      float64
	remainingKnown bool
}

// claimed reports the provider's own "already taken" states
// (`plugins/codearts/credits.go:17-21`).
func (s dailyState) claimed() bool { return codeartsClaimedStatuses[strings.ToUpper(s.status)] }

// codeartsClaimedStatuses is the activity status set codearts itself treats as
// "the daily benefit has already been taken today".
var codeartsClaimedStatuses = map[string]bool{
	"CLAIMED":   true,
	"CONFIRMED": true,
	"CONSUMED":  true,
}

// dailyActivityState reads the `daily_checkin` block of a codearts status
// document.
func dailyActivityState(document map[string]any) dailyState {
	state := dailyState{}
	if document == nil {
		return state
	}
	if remaining, okRemaining := numberField(document, "remaining"); okRemaining {
		state.remaining = remaining
		state.remainingKnown = true
	}
	block, okBlock := objectField(document, "daily_checkin")
	if !okBlock {
		return state
	}
	state.present = true
	state.status = stringField(block, "status")
	if claimable, okClaimable := boolField(block, "claimable"); okClaimable {
		state.claimable = claimable
	}
	return state
}

// balanceGain renders how much credit the claim added, when both reads carried
// a balance.
func balanceGain(before, after map[string]any) string {
	beforeState := dailyActivityState(before)
	afterState := dailyActivityState(after)
	if !beforeState.remainingKnown || !afterState.remainingKnown {
		return ""
	}
	delta := afterState.remaining - beforeState.remaining
	if delta <= 0 {
		return ""
	}
	return "本次 +" + trimFloat(delta)
}

// checkinVerdict is what one provider's check-in response actually said.
type checkinVerdict struct {
	Kind           rowKind
	ProviderStatus string
	Message        string
	Facts          []string
}

// interpretCheckinJSON reads the verdict out of a provider's check-in document.
//
// Field names differ per provider and are taken from each provider's own
// handler:
//
//	qoder       {"status","message","amount"}
//	lobsterai   {"status","message","credit","credit_granted","server_message"}
//	loomy       {"status","message","credit","balance"}
//	trae        {"checked_in",...,"claim":{"status","message","credit","streak_days"}}
//	codebuddy   {"supported","outcome","message","credit"}   (`status` is an object)
//
// so the kind is looked up in `outcome` first, then in a STRING `status`, then in
// `claim.status`, and the raw value is preserved verbatim.
func interpretCheckinJSON(document map[string]any) checkinVerdict {
	verdict := checkinVerdict{Kind: kindUnknown}
	if document == nil {
		return verdict
	}
	if supported, okSupported := boolField(document, "supported"); okSupported && !supported {
		verdict.Kind = kindUnsupported
		verdict.Message = stringField(document, "message")
		return verdict
	}

	claim, _ := objectField(document, "claim")
	verdict.ProviderStatus = firstNonEmpty(
		stringField(document, "outcome"),
		stringField(document, "status"),
		stringField(claim, "status"),
	)
	verdict.Kind = normalizeProviderStatus(verdict.ProviderStatus)
	verdict.Message = firstNonEmpty(
		stringField(document, "message"),
		stringField(claim, "message"),
		stringField(document, "server_message"),
	)
	verdict.Facts = checkinFacts(document, claim)
	return verdict
}

// checkinFacts collects the numbers the provider itself returned, in a stable
// order and without inventing any: a field that is absent contributes nothing.
func checkinFacts(document, claim map[string]any) []string {
	facts := make([]string, 0, 3)
	if amount, okAmount := numberField(document, "amount"); okAmount && amount != 0 {
		facts = append(facts, "本次 +"+trimFloat(amount))
	}
	credit, granted := numberField(claim, "credit")
	if !granted {
		credit, granted = numberField(document, "credit")
		if granted {
			if flag, okFlag := boolField(document, "credit_granted"); okFlag && !flag {
				granted = false
			}
		}
	}
	if granted && credit != 0 {
		facts = append(facts, "本次 +"+trimFloat(credit))
	}
	if credits, okCredits := numberField(document, "credits"); okCredits && credits != 0 {
		facts = append(facts, "签到奖励 "+trimFloat(credits))
	}
	streak, okStreak := numberField(claim, "streak_days")
	if !okStreak {
		streak, okStreak = numberField(document, "streak_days")
	}
	if okStreak && streak > 0 {
		facts = append(facts, fmt.Sprintf("连续签到 %d 天", int(streak)))
	}
	remaining, okRemaining := numberField(document, "remaining")
	if !okRemaining {
		remaining, okRemaining = numberField(document, "credit_remaining")
	}
	if okRemaining {
		facts = append(facts, "剩余 "+trimFloat(remaining))
	}
	return dedupeFacts(facts, 3)
}

// normalizeProviderStatus maps a provider's own verdict word onto the shared
// vocabulary. Unknown words stay unknown, and the caller still shows the raw
// value, so a new provider vocabulary can never be mistaken for success.
func normalizeProviderStatus(raw string) rowKind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return kindUnknown
	case "claimed", "claim", "success", "ok", "granted", "checked-in", "checked_in":
		return kindClaimed
	case "already-claimed", "already_claimed", "alreadyclaimed", "already", "replayed",
		"duplicate", "already-checked-in", "already_checked_in":
		return kindAlreadyClaimed
	case "inactive", "not-claimable", "not_claimable", "unavailable", "disabled":
		return kindInactive
	case "unsupported", "not-supported", "not_supported":
		return kindUnsupported
	case "failed", "error":
		return kindFailed
	default:
		return kindUnknown
	}
}

// noticeKind maps the tone of the notice a provider page rendered onto the
// shared vocabulary. codearts' own renderer uses success for claimed, the plain
// tone for already-claimed, warning for inactive and danger for failure
// (`plugins/codearts/pluginui.go`, checkinNotice).
func noticeKind(tone string) rowKind {
	switch strings.ToLower(strings.TrimSpace(tone)) {
	case "success":
		return kindClaimed
	case "":
		return kindUnknown
	case "warning":
		return kindInactive
	case "danger":
		return kindFailed
	default:
		return kindUnknown
	}
}

// noticeFromHTML extracts the first themed notice a provider page rendered:
// its tone class and its text. It exists because codearts' claim has no JSON
// representation, so the rendered notice is the provider's own words about it.
func noticeFromHTML(markup string) (string, string) {
	const marker = `class="notice`
	index := strings.Index(markup, marker)
	if index < 0 {
		return "", ""
	}
	value := markup[index+len(marker):]
	end := strings.Index(value, `"`)
	if end < 0 {
		return "", ""
	}
	tone := strings.TrimSpace(value[:end])
	value = value[end:]
	gt := strings.Index(value, ">")
	if gt < 0 {
		return tone, ""
	}
	value = value[gt+1:]
	if close := strings.Index(value, "</div>"); close >= 0 {
		value = value[:close]
	}
	if last := strings.LastIndex(value, ">"); last >= 0 {
		value = value[last+1:]
	}
	return tone, strings.TrimSpace(html.UnescapeString(value))
}

// ── Small JSON helpers ──

func parseJSONObject(body []byte) (map[string]any, error) {
	var document map[string]any
	if errUnmarshal := json.Unmarshal(body, &document); errUnmarshal != nil {
		return nil, abiboot.Errorf("invalid_json", "解析 JSON 失败：%v", errUnmarshal)
	}
	return document, nil
}

func objectField(document map[string]any, key string) (map[string]any, bool) {
	if document == nil {
		return nil, false
	}
	value, ok := document[key]
	if !ok {
		return nil, false
	}
	object, okObject := value.(map[string]any)
	return object, okObject
}

func arrayField(document map[string]any, key string) ([]any, bool) {
	if document == nil {
		return nil, false
	}
	value, ok := document[key]
	if !ok {
		return nil, false
	}
	items, okArray := value.([]any)
	return items, okArray
}

func stringField(document map[string]any, key string) string {
	if document == nil {
		return ""
	}
	if text, ok := document[key].(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func numberField(document map[string]any, key string) (float64, bool) {
	if document == nil {
		return 0, false
	}
	value, ok := document[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, errParse := typed.Float64()
		return parsed, errParse == nil
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, errParse == nil
	default:
		return 0, false
	}
}

func boolField(document map[string]any, key string) (bool, bool) {
	if document == nil {
		return false, false
	}
	value, ok := document[key]
	if !ok {
		return false, false
	}
	typed, okBool := value.(bool)
	return typed, okBool
}

// ── Formatting helpers ──

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func dedupeFacts(values []string, limit int) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func trimFloat(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

func orPlaceholder(value string) string {
	if strings.TrimSpace(value) == "" {
		return "未返回"
	}
	return strings.TrimSpace(value)
}

// snippet shortens an upstream body for display.
func snippet(body []byte, limit int) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
