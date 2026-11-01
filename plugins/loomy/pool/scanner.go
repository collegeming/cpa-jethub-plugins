package pool

import (
	"encoding/json"
	"fmt"
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
// every Loomy credential: does its own status document still report points?
//
// The balance is NOT re-implemented here: the loomy plugin already serves a
// per-account points document at
// `/v0/resource/plugins/loomy-provider/status?format=json` over the
// unauthenticated loopback mount, so the scanner is a reader of the
// provider's own truth and a writer of the host's auth files.
//
// Two rules keep the write path honest:
//
//   - the write is a minimal overlay (`disabled` + `disabled_by`), never a
//     whole-file rewrite: host.auth.save merges it over the file the host is
//     holding, so a concurrent credential update cannot be reverted;
//   - an account is only resurrected when the marker says the pool pulled it.
//     A credential disabled any other way stays disabled no matter how many
//     points it reports.
//
// Loomy adds a second disable reason on top of the codearts pool's credit
// rule: a dead session (upstream 100002). It fails every relay request, and
// unlike CodeArts there is no refresh path — the account stays parked until a
// re-login rewrites the credential and the next scan reads points again.

// statusAccount is one entry of the loomy status document. `points` is absent
// when the provider could not read the balance — unknown is never treated as
// zero (the repo's trap #11).
type statusAccount struct {
	Name        string `json:"name"`
	AuthIndex   string `json:"auth_index"`
	Label       string `json:"label"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	PointsError string `json:"points_error"`
	Points      *struct {
		Available      *float64 `json:"available"`
		Balance        *float64 `json:"balance"`
		DailyBalance   *float64 `json:"daily_balance"`
		DailyQuota     *float64 `json:"daily_quota"`
		DailyConsumed  *float64 `json:"daily_consumed"`
		DailyCycleDate string   `json:"daily_cycle_date"`
	} `json:"points"`
}

// statusDocument is the machine-readable shape of the loomy status page.
type statusDocument struct {
	Accounts []statusAccount `json:"accounts"`
}

// accountReport is one credential's outcome of a scan. It doubles as the
// account card the page renders.
type accountReport struct {
	Name          string   `json:"name"`
	AuthIndex     string   `json:"auth_index"`
	Label         string   `json:"label,omitempty"`
	Status        string   `json:"status"`
	Disabled      bool     `json:"disabled"`
	Available     *float64 `json:"available,omitempty"`
	Balance       *float64 `json:"balance,omitempty"`
	DailyBalance  *float64 `json:"daily_balance,omitempty"`
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
	actionSkipped    = "skipped"     // points unknown or foreign-disabled
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
		if !isLoomyEntry(entry) {
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

// isLoomyEntry reports whether a host auth entry belongs to this provider.
// The legacy plugin-id type is accepted so files stamped `loomy-provider`
// before the rename are still managed.
func isLoomyEntry(entry pluginapi.HostAuthFileEntry) bool {
	for _, key := range []string{"loomy", "loomy-provider"} {
		if entry.Type == key || entry.Provider == key {
			return true
		}
	}
	return false
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
		report.Available = availableOf(account)
		report.Balance = balanceOf(account)
		report.DailyBalance = dailyBalanceOf(account)
		report.Checkin = checkinText(account)
		if report.Label == "" {
			report.Label = account.Label
		}
	}

	// Claim before deciding: a grant that lands points belongs to THIS sweep's
	// own available figure, so an exhausted credential can be revived in one
	// pass instead of waiting for the next tick. Claiming also runs for
	// disabled accounts — parking in CPA never reaches upstream, and the daily
	// grant is exactly what brings a spent credential back. An account whose
	// points are unreadable still gets the claim attempt: the claim is the
	// only other probe the provider exposes, and its failure carries the same
	// session-death signal.
	if found && cfg.Enabled && cfg.AutoCheckin && checkinDue(account) {
		outcome := claimCheckin(h, cfg, entry)
		report.CheckinResult = outcome.Message
		if outcome.CheckinText != "" {
			report.Checkin = outcome.CheckinText
		}
		if outcome.Available != nil {
			report.Available = outcome.Available
			account = mergeClaimedPoints(account, outcome)
		}
		if outcome.SessionDead {
			account.PointsError = firstNonEmpty(account.PointsError, "会话已失效（100002）")
			account.Points = nil
		}
	}

	switch {
	case !found:
		report.Detail = "不在状态页账号列表中，不动它"
		return report
	case sessionDeadError(account.PointsError), sessionDeadError(account.Error):
		return disableFor(h, cfg, entry, report, disabledBySession, "会话失效（100002）")
	case account.Points == nil || availableOf(account) == nil:
		report.Detail = detailOr("积分未知（状态页未报告），不动它", account)
		return report
	}

	available := *availableOf(account)

	if available <= cfg.MinRemaining {
		return disableFor(h, cfg, entry, report, disabledByCredit, fmt.Sprintf("积分耗尽（available=%g）", available))
	}

	// available > MinRemaining: the only resurrection candidate is a credential
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
	if disabledBy != disabledByCredit && disabledBy != disabledBySession {
		report.Detail = "非本插件停用（disabled_by=" + printable(disabledBy) + "），保持停用"
		return report
	}
	if !cfg.Enabled || !cfg.AutoEnable {
		report.Action = actionWriteGuard
		report.Detail = fmt.Sprintf("已恢复积分（available=%g），但写开关已关闭", available)
		return report
	}
	if err := enableAccount(h, entry); err != nil {
		report.Action = actionFailed
		report.Detail = err.Error()
		return report
	}
	h.Log("info", fmt.Sprintf("Loomy 账号 %s 恢复积分（available=%g），已重新启用", name, available), map[string]any{
		"auth":      name,
		"available": available,
	})
	report.Action = actionEnabled
	report.Disabled = false
	report.Detail = "积分恢复，已写 disabled:false 并重置冷却"
	return report
}

// disableFor applies the shared disable branch: write the marker overlay when
// the account is not already parked and config permits.
func disableFor(h *abiboot.Host, cfg Config, entry pluginapi.HostAuthFileEntry, report accountReport, marker, why string) accountReport {
	if entry.Disabled {
		report.Action = actionNone
		report.Detail = why + "，已处于停用状态"
		return report
	}
	if !cfg.Enabled || !cfg.AutoDisable {
		report.Action = actionWriteGuard
		report.Detail = why + "，但写开关已关闭"
		return report
	}
	if err := writeDisabled(h, entry.Name, true, marker); err != nil {
		report.Action = actionFailed
		report.Detail = err.Error()
		return report
	}
	h.Log("warn", fmt.Sprintf("Loomy 账号 %s %s，已自动停用", entry.Name, why), map[string]any{
		"auth":        entry.Name,
		"disabled_by": marker,
	})
	report.Action = actionDisabled
	report.Disabled = true
	report.Detail = why + "，已写 disabled:true 并标记 " + marker
	return report
}

// checkinOutcome is what one automatic claim did, in the account's own words.
type checkinOutcome struct {
	Message     string
	Available   *float64
	CheckinText string
	SessionDead bool
}

// claimCheckin drives the Loomy daily grant for one account through the
// provider's own checkin route — `/checkin?action=claim` answers JSON, so no
// HTML scraping is needed. A `failed` outcome whose message carries the
// session-expired code doubles as the dead-session probe.
func claimCheckin(h *abiboot.Host, cfg Config, entry pluginapi.HostAuthFileEntry) checkinOutcome {
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return checkinOutcome{Message: "缺少 auth_index，无法指定账号，跳过签到"}
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.HostBaseURL), "/")
	claimURL := base + "/v0/resource/plugins/" + pluginMount + "/checkin?format=json&action=claim&auth_index=" +
		url.QueryEscape(entry.AuthIndex)
	response, errDo := transportFor(h)(http.MethodGet, claimURL,
		http.Header{"Accept": {"application/json"}}, nil)
	if errDo != nil {
		return checkinOutcome{Message: "签到请求失败：" + errDo.Error()}
	}
	var document struct {
		Status       string   `json:"status"`
		Message      string   `json:"message"`
		Error        string   `json:"error"`
		Balance      *float64 `json:"balance"`
		DailyBalance *float64 `json:"daily_balance"`
	}
	if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusBadRequest ||
		response.StatusCode == http.StatusUnauthorized {
		if errDecode := json.Unmarshal(response.Body, &document); errDecode == nil {
			outcome := checkinOutcome{Message: firstNonEmpty(document.Message, document.Error, "已签到")}
			if sessionDeadError(document.Message) || sessionDeadError(document.Error) {
				outcome.SessionDead = true
				return outcome
			}
			if document.Balance != nil || document.DailyBalance != nil {
				sum := 0.0
				if document.Balance != nil {
					sum += *document.Balance
				}
				if document.DailyBalance != nil {
					sum += *document.DailyBalance
				}
				outcome.Available = &sum
				outcome.CheckinText = "已签到"
			}
			return outcome
		}
	}
	return checkinOutcome{Message: fmt.Sprintf("签到页返回 HTTP %d", response.StatusCode)}
}

// mergeClaimedPoints folds a successful claim's figures into the status
// account so the disable/enable decision sees post-grant numbers.
func mergeClaimedPoints(account statusAccount, outcome checkinOutcome) statusAccount {
	if outcome.Available == nil {
		return account
	}
	if account.Points == nil {
		account.Points = &struct {
			Available      *float64 `json:"available"`
			Balance        *float64 `json:"balance"`
			DailyBalance   *float64 `json:"daily_balance"`
			DailyQuota     *float64 `json:"daily_quota"`
			DailyConsumed  *float64 `json:"daily_consumed"`
			DailyCycleDate string   `json:"daily_cycle_date"`
		}{}
	}
	account.Points.Available = outcome.Available
	return account
}

// checkinDue reports whether the account's daily grant has not been claimed
// for the current business day — or whether the state is unreadable, in which
// case attempting the claim is how the pool learns.
func checkinDue(account statusAccount) bool {
	if account.Points == nil {
		return true
	}
	cycle := strings.TrimSpace(account.Points.DailyCycleDate)
	return cycle == "" || cycle < businessDay()
}

// businessDay is Loomy's business date: the service is Chinese, so the cycle
// boundary follows UTC+8 rather than the host's local zone.
func businessDay() string {
	return time.Now().In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02")
}

// sessionDeadError reports whether a provider error string is the session
// dead code (upstream 100002), the only failure that justifies parking.
func sessionDeadError(text string) bool {
	return strings.Contains(text, "100002") || strings.Contains(text, "会话已失效")
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

// fetchPoolStatus reads the loomy plugin's own status document over the
// unauthenticated resource mount.
func fetchPoolStatus(h *abiboot.Host, cfg Config) (*statusDocument, error) {
	rawURL := strings.TrimRight(strings.TrimSpace(cfg.HostBaseURL), "/") +
		"/v0/resource/plugins/" + pluginMount + "/status?format=json"
	response, errDo := transportFor(h)(http.MethodGet, rawURL,
		http.Header{"Accept": {"application/json"}}, nil)
	if errDo != nil {
		return nil, fmt.Errorf("读取 loomy 状态页失败：%v", errDo)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("loomy 状态页返回 HTTP %d（插件未安装或未启用？）", response.StatusCode)
	}
	document := &statusDocument{}
	if errDecode := json.Unmarshal(response.Body, document); errDecode != nil {
		return nil, fmt.Errorf("解析 loomy 状态页失败：%v", errDecode)
	}
	return document, nil
}

// writeDisabled flips the disabled flag on a credential file through a minimal
// overlay: `disabled` + `disabled_by` are the only members written, so the
// merge inside host.auth.save cannot revert a concurrent credential update.
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
// the credential re-enters rotation immediately.
func enableAccount(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) error {
	if err := writeDisabled(h, entry.Name, false, ""); err != nil {
		return err
	}
	resetCooldown(h, entry.AuthIndex)
	return nil
}

// resetCooldown asks the host to drop quota and cooldown state for one
// credential. Best effort: a host that predates host.routing.reset_cooldown
// answers with an error the caller can afford to ignore.
func resetCooldown(h *abiboot.Host, authIndex string) {
	if strings.TrimSpace(authIndex) == "" {
		return
	}
	payload := map[string]any{"auth_index": authIndex}
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
// own status page does.
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

// checkinText renders the account's daily-grant state for the table.
func checkinText(account statusAccount) string {
	if account.Points == nil {
		return ""
	}
	cycle := strings.TrimSpace(account.Points.DailyCycleDate)
	if cycle == "" {
		return ""
	}
	if cycle < businessDay() {
		return "可签到（cycle " + cycle + "）"
	}
	return "今日已签到（" + cycle + "）"
}

// detailOr appends the provider-side error text to a fallback detail line so a
// skipped account still explains why its balance could not be read.
func detailOr(fallback string, account statusAccount) string {
	for _, text := range []string{account.Error, account.PointsError} {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return fallback + "（" + trimmed + "）"
		}
	}
	return fallback
}

// availableOf/balanceOf/dailyBalanceOf unwrap the nested points document.
func availableOf(account statusAccount) *float64 {
	if account.Points == nil {
		return nil
	}
	return account.Points.Available
}
func balanceOf(account statusAccount) *float64 {
	if account.Points == nil {
		return nil
	}
	return account.Points.Balance
}
func dailyBalanceOf(account statusAccount) *float64 {
	if account.Points == nil {
		return nil
	}
	return account.Points.DailyBalance
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
// Mirrors the codearts pool's loop: a dedicated ticker rather than a shared
// scheduler so pool sweeps report in pool vocabulary.

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

// lastScanOutcome returns the remembered sweep, or nil before the first one.
func lastScanOutcome() *scanOutcome {
	if value, ok := lastScan.Load().(scanOutcome); ok {
		return &value
	}
	return nil
}
