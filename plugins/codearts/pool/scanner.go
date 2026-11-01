package pool

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file is the pool reconciler. Every scan answers the same question for
// every CodeArts credential: does its own status document still report credit?
//
// The balance is NOT re-implemented here: the codearts plugin already serves a
// per-account quota document at `/v0/resource/plugins/codearts/status?format=json`
// over the unauthenticated loopback mount, so the scanner is a reader of the
// provider's own truth and a writer of the host's auth files — the same seam
// the hub plugin drives for check-ins.
//
// Two rules keep the write path honest:
//
//   - the write is a minimal overlay (`disabled` + `disabled_by`), never a
//     whole-file rewrite: host.auth.save merges it over the file the host is
//     holding, so a token refresh racing the scan cannot be reverted;
//   - an account is only resurrected when the marker says the pool pulled it.
//     A credential disabled any other way stays disabled no matter how much
//     credit it reports.

// statusAccount is one entry of the codearts status document. Every figure is
// a pointer for the same reason the provider renders unknown as absent: a
// failed read must never be mistaken for a zero balance.
type statusAccount struct {
	Name          string        `json:"name"`
	AuthIndex     string        `json:"auth_index"`
	Label         string        `json:"label"`
	Status        string        `json:"status"`
	Remaining     *float64      `json:"remaining"`
	Used          *float64      `json:"used"`
	Total         *float64      `json:"total"`
	CreditPackage *bool         `json:"credit_package"`
	Refreshed     bool          `json:"refreshed"`
	Error         string        `json:"error"`
	CreditError   string        `json:"credit_error"`
	RefreshError  string        `json:"refresh_error"`
	ActivityError string        `json:"activity_error"`
	DailyCheckin  *dailyCheckin `json:"daily_checkin"`
}

// dailyCheckin is the provider's USER_LOGIN campaign state for one account.
type dailyCheckin struct {
	CampaignID string `json:"campaign_id"`
	Claimable  bool   `json:"claimable"`
	Status     string `json:"status"`
}

// statusDocument is the machine-readable shape of the codearts status page.
type statusDocument struct {
	Accounts []statusAccount `json:"accounts"`
}

// accountReport is one credential's outcome of a scan. It doubles as the
// account card the page renders, so it also carries the figures the provider
// published (quota triple, check-in state) and the host-side status text.
type accountReport struct {
	Name          string   `json:"name"`
	AuthIndex     string   `json:"auth_index"`
	Label         string   `json:"label,omitempty"`
	Status        string   `json:"status"`
	Disabled      bool     `json:"disabled"`
	Remaining     *float64 `json:"remaining,omitempty"`
	Used          *float64 `json:"used,omitempty"`
	Total         *float64 `json:"total,omitempty"`
	Checkin       string   `json:"checkin,omitempty"`
	CheckinResult string   `json:"checkin_result,omitempty"`
	Action        string   `json:"action"`
	Detail        string   `json:"detail,omitempty"`
}

// Action values reported per account.
const (
	actionNone       = "none"        // already in the state the pool wants
	actionDisabled   = "disabled"    // pulled from rotation this scan
	actionEnabled    = "enabled"     // returned to rotation this scan
	actionSkipped    = "skipped"     // balance unknown or foreign-disabled
	actionFailed     = "failed"      // a write was needed and failed
	actionWriteGuard = "write_guard" // a write was needed but config forbids it
)

// scanOutcome is the remembered result of one sweep, shown on the page and in
// the JSON document.
type scanOutcome struct {
	Reason      string          `json:"reason"` // auto | manual | api
	StartedAt   time.Time       `json:"started_at"`
	DurationMS  int64           `json:"duration_ms"`
	Accounts    []accountReport `json:"accounts"`
	DisabledNow int             `json:"disabled_now"`
	EnabledNow  int             `json:"enabled_now"`
	Error       string          `json:"error,omitempty"`
}

// runScan performs one sweep. It is shared by the background ticker, the page
// button and the POST route, so the reason is the only thing that differs.
func runScan(h *abiboot.Host, cfg Config, reason string) scanOutcome {
	started := time.Now()
	outcome := scanOutcome{Reason: reason, StartedAt: started}

	status, errStatus := fetchPoolStatus(h, cfg)
	if errStatus != nil {
		outcome.Error = errStatus.Error()
		outcome.DurationMS = time.Since(started).Milliseconds()
		rememberScan(outcome)
		return outcome
	}

	entries, errList := h.ListAuth()
	if errList != nil {
		outcome.Error = fmt.Sprintf("host.auth.list: %v", errList)
		outcome.DurationMS = time.Since(started).Milliseconds()
		rememberScan(outcome)
		return outcome
	}

	for _, entry := range entries {
		if entry.Type != "codearts" && entry.Provider != "codearts" {
			continue
		}
		report := reconcileAccount(h, cfg, entry, status)
		outcome.Accounts = append(outcome.Accounts, report)
		switch report.Action {
		case actionDisabled:
			outcome.DisabledNow++
		case actionEnabled:
			outcome.EnabledNow++
		}
	}
	outcome.DurationMS = time.Since(started).Milliseconds()
	rememberScan(outcome)
	return outcome
}

// reconcileAccount decides and applies one credential's rotation state.
//
// A fresh Host is used per account: Host.Incoming is sticky for the whole
// value, and SaveAuth merges it into the write — sharing one handle across
// accounts would merge account A's file into account B's.
func reconcileAccount(h *abiboot.Host, cfg Config, entry pluginapi.HostAuthFileEntry, status *statusDocument) accountReport {
	report := accountReport{
		Name:      entry.Name,
		AuthIndex: entry.AuthIndex,
		Label:     firstNonEmpty(entry.Label, entry.Email, entry.Account),
		Status:    statusOf(entry),
		Disabled:  entry.Disabled,
		Action:    actionSkipped,
	}
	name := entry.Name
	if strings.TrimSpace(name) == "" {
		report.Detail = "凭据没有 backing 文件，无法停用"
		return report
	}
	account, found := statusByName(status, entry)
	if found {
		report.Remaining = account.Remaining
		report.Used = account.Used
		report.Total = account.Total
		report.Checkin = checkinText(account)
		if report.Label == "" {
			report.Label = account.Label
		}
	}

	// Claim before deciding: a grant that lands credit belongs to THIS sweep's
	// own remaining figure, so an exhausted credential can be revived in one
	// pass instead of waiting for the next tick. Claiming also runs for
	// disabled accounts — parking in CPA never reaches upstream, and the daily
	// grant is exactly what brings a spent credential back.
	if found && cfg.Enabled && cfg.AutoCheckin &&
		account.DailyCheckin != nil && account.DailyCheckin.Claimable {
		outcome := claimCheckin(h, cfg, entry)
		report.CheckinResult = outcome.Message
		if outcome.Remaining != nil {
			account.Remaining = outcome.Remaining
			report.Remaining = outcome.Remaining
		}
		if outcome.CheckinState != "" {
			report.Checkin = outcome.CheckinState
		}
	}
	if !found || account.Remaining == nil {
		report.Detail = detailOr("余额未知（状态页未报告），不动它", account)
		return report
	}
	remaining := *account.Remaining

	if remaining <= cfg.MinRemaining {
		if entry.Disabled {
			report.Action = actionNone
			report.Detail = "积分耗尽，已处于停用状态"
			return report
		}
		if !cfg.Enabled || !cfg.AutoDisable {
			report.Action = actionWriteGuard
			report.Detail = fmt.Sprintf("积分耗尽（remaining=%g），但写开关已关闭", remaining)
			return report
		}
		if err := writeDisabled(h, name, true, disabledByCredit); err != nil {
			report.Action = actionFailed
			report.Detail = err.Error()
			return report
		}
		h.Log("warn", fmt.Sprintf("CodeArts 账号 %s 积分耗尽（remaining=%g），已自动停用", name, remaining), map[string]any{
			"auth":      name,
			"remaining": remaining,
		})
		report.Action = actionDisabled
		report.Disabled = true
		report.Detail = "积分耗尽，已写 disabled:true 并标记 codeartspool:credit"
		return report
	}

	// remaining > MinRemaining: the only resurrection candidate is a credential
	// the pool itself pulled.
	if !entry.Disabled {
		report.Action = actionNone
		return report
	}
	disabledBy, errRead := readDisabledBy(h, entry)
	if errRead != nil {
		report.Detail = "读取凭据失败：" + errRead.Error()
		return report
	}
	if disabledBy != disabledByCredit {
		report.Detail = "非本插件停用（disabled_by=" + printable(disabledBy) + "），保持停用"
		return report
	}
	if !cfg.Enabled || !cfg.AutoEnable {
		report.Action = actionWriteGuard
		report.Detail = fmt.Sprintf("已恢复余额（remaining=%g），但写开关已关闭", remaining)
		return report
	}
	if err := enableAccount(h, entry); err != nil {
		report.Action = actionFailed
		report.Detail = err.Error()
		return report
	}
	h.Log("info", fmt.Sprintf("CodeArts 账号 %s 恢复余额（remaining=%g），已重新启用", name, remaining), map[string]any{
		"auth":      name,
		"remaining": remaining,
	})
	report.Action = actionEnabled
	report.Disabled = false
	report.Detail = "余额恢复，已写 disabled:false 并重置冷却"
	return report
}

// checkinOutcome is what one automatic claim did, in the account's own words.
type checkinOutcome struct {
	Message      string
	Remaining    *float64
	CheckinState string
}

// claimCheckin drives the codearts daily-login grant for one account.
//
// The claim exists only as `GET /status?action=checkin` on the HTML
// representation — the provider's JSON branch is selected before it ever looks
// at the action, so this request MUST carry an Accept header mentioning
// text/html (the same constraint the hub documents in
// plugins/hub/orchestrator.go runStatusPageCheckin). After the claim the
// account's own status document is re-read so the sweep works with the
// post-grant balance, and the rendered notice supplies the message verbatim.
func claimCheckin(h *abiboot.Host, cfg Config, entry pluginapi.HostAuthFileEntry) checkinOutcome {
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return checkinOutcome{Message: "缺少 auth_index，无法指定账号，跳过签到"}
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.HostBaseURL), "/")
	claimURL := base + "/v0/resource/plugins/codearts/status?action=checkin&auth_index=" +
		url.QueryEscape(entry.AuthIndex)
	response, errDo := transportFor(h)(http.MethodGet, claimURL,
		http.Header{"Accept": {"text/html"}}, nil)
	if errDo != nil {
		return checkinOutcome{Message: "签到请求失败：" + errDo.Error()}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return checkinOutcome{Message: fmt.Sprintf("签到页返回 HTTP %d", response.StatusCode)}
	}
	_, noticeText := noticeFromHTML(string(response.Body))

	outcome := checkinOutcome{Message: firstNonEmpty(noticeText, "已签到")}
	after := fetchAccountCheckinState(h, cfg, entry.AuthIndex)
	if after != nil {
		outcome.Remaining = after.Remaining
		outcome.CheckinState = checkinText(statusAccount{DailyCheckin: after.DailyCheckin})
		if outcome.CheckinState == "" && after.DailyCheckin != nil {
			outcome.CheckinState = after.DailyCheckin.Status
		}
	}
	return outcome
}

// fetchAccountCheckinState re-reads one account's own figures from the status
// document after a claim: top-level `remaining` and `daily_checkin` belong to
// the account the auth_index selects.
func fetchAccountCheckinState(h *abiboot.Host, cfg Config, authIndex string) *statusAccount {
	base := strings.TrimRight(strings.TrimSpace(cfg.HostBaseURL), "/")
	rawURL := base + "/v0/resource/plugins/codearts/status?format=json&auth_index=" +
		url.QueryEscape(authIndex)
	response, errDo := transportFor(h)(http.MethodGet, rawURL,
		http.Header{"Accept": {"application/json"}}, nil)
	if errDo != nil || response.StatusCode != http.StatusOK {
		return nil
	}
	var document struct {
		Remaining    *float64      `json:"remaining"`
		DailyCheckin *dailyCheckin `json:"daily_checkin"`
	}
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		return nil
	}
	return &statusAccount{Remaining: document.Remaining, DailyCheckin: document.DailyCheckin}
}

// noticeFromHTML extracts the first themed notice a provider page rendered:
// its tone class and its text. The codearts claim has no JSON representation,
// so the rendered notice is the provider's own words about it (same extractor
// as plugins/hub/orchestrator.go).
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

// statusByName finds the account one host entry names in the status document.
// The file name is the stable join key; auth_index is the fallback for entries
// whose name is a runtime identifier.
func statusByName(status *statusDocument, entry pluginapi.HostAuthFileEntry) (statusAccount, bool) {
	if status == nil {
		return statusAccount{}, false
	}
	for _, account := range status.Accounts {
		if account.Name != "" && account.Name == entry.Name {
			return account, true
		}
	}
	for _, account := range status.Accounts {
		if account.AuthIndex != "" && account.AuthIndex == entry.AuthIndex {
			return account, true
		}
	}
	return statusAccount{}, false
}

// fetchPoolStatus reads the codearts plugin's own status document over the
// unauthenticated resource mount.
func fetchPoolStatus(h *abiboot.Host, cfg Config) (*statusDocument, error) {
	rawURL := strings.TrimRight(strings.TrimSpace(cfg.HostBaseURL), "/") +
		"/v0/resource/plugins/codearts/status?format=json"
	response, errDo := transportFor(h)(http.MethodGet, rawURL,
		http.Header{"Accept": {"application/json"}}, nil)
	if errDo != nil {
		return nil, fmt.Errorf("读取 codearts 状态页失败：%v", errDo)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codearts 状态页返回 HTTP %d（插件未安装或未启用？）", response.StatusCode)
	}
	document := &statusDocument{}
	if errDecode := json.Unmarshal(response.Body, document); errDecode != nil {
		return nil, fmt.Errorf("解析 codearts 状态页失败：%v", errDecode)
	}
	return document, nil
}

// writeDisabled flips the disabled flag on a credential file through a minimal
// overlay: `disabled` + `disabled_by` are the only members written, so the
// merge inside host.auth.save cannot revert a concurrent token refresh.
func writeDisabled(h *abiboot.Host, name string, disabled bool, disabledBy string) error {
	overlay, errMarshal := json.Marshal(map[string]any{
		"disabled":    disabled,
		"disabled_by": disabledBy,
	})
	if errMarshal != nil {
		return errMarshal
	}
	fresh := &abiboot.Host{}
	_, errSave := fresh.SaveAuth(name, overlay)
	if errSave != nil {
		return fmt.Errorf("host.auth.save %s: %v", name, errSave)
	}
	return nil
}

// readDisabledBy returns the `disabled_by` marker stored in the credential
// file, or "" when the file carries none.
func readDisabledBy(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) (string, error) {
	fresh := &abiboot.Host{}
	response, errGet := fresh.GetAuth(entry.AuthIndex)
	if errGet != nil {
		return "", errGet
	}
	var probe struct {
		DisabledBy string `json:"disabled_by"`
	}
	if errDecode := json.Unmarshal(response.JSON, &probe); errDecode != nil {
		return "", errDecode
	}
	return strings.TrimSpace(probe.DisabledBy), nil
}

// enableAccount clears the pool-set disable and resets the routing cooldown so
// the credential re-enters rotation immediately instead of waiting out a 402
// cooldown it accrued before being pulled.
func enableAccount(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) error {
	if err := writeDisabled(h, entry.Name, false, ""); err != nil {
		return err
	}
	resetCooldown(h, entry.AuthIndex)
	return nil
}

// resetCooldown asks the host to drop quota and cooldown state for one
// credential. Best effort: a host that predates host.routing.reset_cooldown
// answers with an error the caller can afford to ignore — a re-registered
// credential rebuilds its runtime state anyway.
func resetCooldown(h *abiboot.Host, authIndex string) {
	if strings.TrimSpace(authIndex) == "" {
		return
	}
	payload := map[string]any{"auth_index": authIndex}
	// MethodHostRoutingResetCooldown predates this plugin's SDK pin, so the
	// literal method string is used instead of the pluginabi constant.
	_, _ = abiboot.HostCall("host.routing.reset_cooldown", payload)
}

// printable renders a marker for display, quoting the empty string.
func printable(text string) string {
	if text == "" {
		return "<无标记>"
	}
	return text
}

// statusOf renders the host-side credential status the same way the provider's
// own status page does (plugins/codearts/pluginui.go statusText): a disabled
// or unavailable flag outranks the free-form status string.
func statusOf(entry pluginapi.HostAuthFileEntry) string {
	switch {
	case entry.Disabled:
		return "已停用"
	case entry.Unavailable:
		return "不可用"
	case strings.TrimSpace(entry.Status) != "":
		if entry.StatusMessage != "" {
			return entry.Status + "（" + entry.StatusMessage + "）"
		}
		return entry.Status
	default:
		return "正常"
	}
}

// checkinText renders the account's daily-login campaign state for the table.
func checkinText(account statusAccount) string {
	if account.DailyCheckin == nil {
		return ""
	}
	if account.DailyCheckin.Claimable {
		return "可签到"
	}
	return account.DailyCheckin.Status
}

// detailOr appends the provider-side error text to a fallback detail line so a
// skipped account still explains why its balance could not be read.
func detailOr(fallback string, account statusAccount) string {
	for _, text := range []string{account.Error, account.CreditError, account.RefreshError} {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return fallback + "（" + trimmed + "）"
		}
	}
	return fallback
}

// firstNonEmpty returns the first non-blank argument.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ── background scheduler ──
//
// A dedicated ticker rather than catalog.Scheduler: that one reports a
// catalogue Outcome and logs in catalogue vocabulary, which would misreport a
// pool sweep. The shape (mutex + cancel chan + counters) mirrors it on
// purpose so the two loops read alike.

type poolScanner struct {
	mu       sync.Mutex
	interval time.Duration
	cancel   chan struct{}
}

var scanner = &poolScanner{}

// startScanner (re)configures the background sweep from settings.
func startScanner(cfg Config) {
	scanner.mu.Lock()
	scanner.interval = time.Duration(cfg.ScanIntervalMS) * time.Millisecond
	if scanner.cancel != nil || scanner.interval <= 0 {
		scanner.mu.Unlock()
		return
	}
	cancel := make(chan struct{})
	scanner.cancel = cancel
	interval := scanner.interval
	scanner.mu.Unlock()

	go scannerLoop(interval, cancel)
}

// stopScanner ends the background loop at shutdown.
func stopScanner() {
	scanner.mu.Lock()
	cancel := scanner.cancel
	scanner.cancel = nil
	scanner.mu.Unlock()
	if cancel != nil {
		close(cancel)
	}
}

func scannerLoop(interval time.Duration, cancel chan struct{}) {
	host := &abiboot.Host{}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-cancel:
			return
		case <-ticker.C:
			runScan(host, settings(), "auto")
		}
	}
}

// lastScan is the remembered outcome the page reports.
var lastScan atomic.Value // holds scanOutcome

// rememberScan stores the latest outcome for the status page.
func rememberScan(outcome scanOutcome) { lastScan.Store(outcome) }

// lastScanOutcome returns the most recent sweep, or nil before the first.
func lastScanOutcome() *scanOutcome {
	if value := lastScan.Load(); value != nil {
		if outcome, ok := value.(scanOutcome); ok {
			return &outcome
		}
	}
	return nil
}
