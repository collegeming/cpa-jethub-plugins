package pool

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakePool installs a scripted host. The scanner reaches the host through two
// seams — abiboot's host caller (auth.list/get/save, log, reset_cooldown) and
// the transportFor variable (host.http.do to the loomy status + checkin
// pages) — so one fixture covers the whole sweep offline.
type fakePool struct {
	mu sync.Mutex
	// files is what host.auth.list reports; json is the file content
	// host.auth.get returns, keyed by auth_index.
	files []pluginapi.HostAuthFileEntry
	json  map[string]json.RawMessage
	// statusBody is served for the loomy status page.
	statusBody string
	statusCode int
	// checkinBody/checkinCode are served for the checkin claim call.
	checkinBody string
	checkinCode int
	// httpCalls records every URL the transport was asked for.
	httpCalls []string
	// saves records every host.auth.save (name + decoded JSON).
	saves []saveCall
	// resets records every host.routing.reset_cooldown auth_index.
	resets []string
}

type saveCall struct {
	Name string
	JSON map[string]any
}

func newFakePool(files ...pluginapi.HostAuthFileEntry) *fakePool {
	return &fakePool{
		files:       files,
		json:        map[string]json.RawMessage{},
		statusCode:  http.StatusOK,
		checkinCode: http.StatusOK,
	}
}

func (f *fakePool) install(t *testing.T) {
	t.Helper()
	previousTransport := transportFor
	transportFor = func(*abiboot.Host) doer {
		return func(_, rawURL string, _ http.Header, _ []byte) (*pluginapi.HTTPResponse, error) {
			f.mu.Lock()
			f.httpCalls = append(f.httpCalls, rawURL)
			f.mu.Unlock()
			switch {
			case strings.Contains(rawURL, "/checkin") && strings.Contains(rawURL, "action=claim"):
				return &pluginapi.HTTPResponse{
					StatusCode: f.checkinCode,
					Headers:    map[string][]string{"Content-Type": {"application/json"}},
					Body:       []byte(f.checkinBody),
				}, nil
			case strings.Contains(rawURL, "/v0/resource/plugins/loomy-provider/status"):
				return &pluginapi.HTTPResponse{
					StatusCode: f.statusCode,
					Headers:    map[string][]string{"Content-Type": {"application/json"}},
					Body:       []byte(f.statusBody),
				}, nil
			}
			return nil, fmt.Errorf("unexpected host.http.do: %s", rawURL)
		}
	}
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return envelopeOK(map[string]any{"files": f.files})
		case pluginabi.MethodHostAuthGet:
			var req pluginapi.HostAuthGetRequest
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
			raw, ok := f.json[req.AuthIndex]
			if !ok {
				return nil, fmt.Errorf("no file for auth_index %s", req.AuthIndex)
			}
			return envelopeOK(pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex, JSON: raw})
		case pluginabi.MethodHostAuthSave:
			var req pluginapi.HostAuthSaveRequest
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
			var decoded map[string]any
			if err := json.Unmarshal(req.JSON, &decoded); err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.saves = append(f.saves, saveCall{Name: req.Name, JSON: decoded})
			f.mu.Unlock()
			return envelopeOK(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auths/" + req.Name})
		case "host.routing.reset_cooldown":
			var req struct {
				AuthIndex string `json:"auth_index"`
			}
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.resets = append(f.resets, req.AuthIndex)
			f.mu.Unlock()
			return envelopeOK(map[string]any{"auth_index": req.AuthIndex})
		case pluginabi.MethodHostLog:
			return envelopeOK(map[string]any{})
		default:
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
	})
	t.Cleanup(func() {
		transportFor = previousTransport
		abiboot.ClearHostCaller()
		forgetScan()
	})
}

// envelopeOK wraps a host result in the ABI envelope.
func envelopeOK(result any) ([]byte, error) {
	raw, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(abiboot.Envelope{OK: true, Result: raw})
}

func poolEntry(index, name string, disabled bool) pluginapi.HostAuthFileEntry {
	return pluginapi.HostAuthFileEntry{
		Provider: "loomy", Type: "loomy",
		AuthIndex: index, Name: name, Disabled: disabled,
	}
}

// statusDoc renders a loomy status document from per-account point figures.
// A nil available emits an account with no points member at all — how the
// provider reports an unreadable balance.
func statusDoc(accounts ...map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"accounts": accounts, "account_count": len(accounts)})
	return string(raw)
}

// accountDoc is one status account. points=nil means "provider could not read
// the balance"; a map emits the full points member.
func accountDoc(name, index string, points map[string]any) map[string]any {
	account := map[string]any{"name": name, "auth_index": index, "status": "正常"}
	if points != nil {
		account["points"] = points
	}
	return account
}

func pointsDoc(available float64, cycle string) map[string]any {
	return map[string]any{
		"available": available, "balance": available, "daily_balance": 0,
		"daily_cycle_date": cycle,
	}
}

func f64(v float64) *float64 { return &v }

func reportFor(outcome scanOutcome, name string) (accountReport, bool) {
	for _, report := range outcome.Accounts {
		if report.Name == name {
			return report, true
		}
	}
	return accountReport{}, false
}

func forgetScan() { lastScan = atomic.Value{} }

// testHost returns a Host carrying a callback identity.
func testHost() *abiboot.Host {
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"test-callback"}`))
}

// ── config ──

func TestConfigFromMapReadsPrefixedKeys(t *testing.T) {
	cfg := ConfigFromMap(map[string]any{
		"pool_enabled":          true,
		"pool_host_base_url":    "http://127.0.0.1:9999/",
		"pool_scan_interval_ms": 60_000,
		"pool_min_remaining":    5,
		"pool_auto_disable":     false,
		"pool_auto_enable":      true,
		"pool_auto_checkin":     true,
		// Unprefixed and other-plugin keys must not leak in.
		"enabled":          false,
		"reg_enabled":      true,
		"scan_interval_ms": 999,
	}, "pool_")
	if !cfg.Enabled || cfg.HostBaseURL != "http://127.0.0.1:9999/" ||
		cfg.ScanIntervalMS != 60_000 || cfg.MinRemaining != 5 ||
		cfg.AutoDisable != false || !cfg.AutoEnable || !cfg.AutoCheckin {
		t.Fatalf("cfg = %+v, want every pool_* key decoded", cfg)
	}
}

func TestDefaultConfigKeepsWritesOn(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.AutoDisable || !cfg.AutoEnable || cfg.ScanIntervalMS <= 0 {
		t.Fatalf("defaults = %+v, want scanning + writes on", cfg)
	}
}

// ── entry filter ──

func TestIsLoomyEntryAcceptsLegacyType(t *testing.T) {
	for _, entry := range []pluginapi.HostAuthFileEntry{
		{Type: "loomy"}, {Provider: "loomy"},
		{Type: "loomy-provider"}, {Provider: "loomy-provider"},
	} {
		if !isLoomyEntry(entry) {
			t.Errorf("entry %+v should be a loomy credential", entry)
		}
	}
	if isLoomyEntry(pluginapi.HostAuthFileEntry{Type: "kimi", Provider: "kimi"}) {
		t.Error("kimi entry must not be managed by the loomy pool")
	}
}

// ── scanner ──

func TestScanDisablesExhaustedAccount(t *testing.T) {
	fake := newFakePool(
		poolEntry("idx-a", "loomy-a.json", false),
		poolEntry("idx-b", "loomy-b.json", false),
	)
	far := "2999-01-01"
	fake.statusBody = statusDoc(
		accountDoc("loomy-a.json", "idx-a", pointsDoc(0, far)),
		accountDoc("loomy-b.json", "idx-b", pointsDoc(100, far)),
	)
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "manual")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	if len(fake.saves) != 1 {
		t.Fatalf("saves = %d, want 1 (%+v)", len(fake.saves), fake.saves)
	}
	save := fake.saves[0]
	if save.Name != "loomy-a.json" || save.JSON["disabled"] != true || save.JSON["disabled_by"] != disabledByCredit {
		t.Errorf("save = %+v, want disabled:true + disabled_by %s on loomy-a.json", save, disabledByCredit)
	}
	if report, ok := reportFor(outcome, "loomy-a.json"); !ok || report.Action != actionDisabled {
		t.Errorf("account a report = %+v, want action=disabled", report)
	}
	if report, ok := reportFor(outcome, "loomy-b.json"); !ok || report.Action != actionNone {
		t.Errorf("account b report = %+v, want action=none", report)
	}
	if outcome.DisabledNow != 1 {
		t.Errorf("DisabledNow = %d, want 1", outcome.DisabledNow)
	}
}

func TestScanDisablesSessionDeadAccount(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "loomy-a.json", false))
	fake.statusBody = statusDoc(map[string]any{
		"name": "loomy-a.json", "auth_index": "idx-a", "status": "正常",
		"points_error": "Loomy 返回错误 100002：会话已失效",
	})
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if len(fake.saves) != 1 || fake.saves[0].JSON["disabled_by"] != disabledBySession {
		t.Fatalf("saves = %+v, want one disable write marked %s", fake.saves, disabledBySession)
	}
	if report, ok := reportFor(outcome, "loomy-a.json"); !ok || report.Action != actionDisabled {
		t.Errorf("report = %+v, want disabled", report)
	}
}

func TestScanResurrectsOnlyPoolDisabled(t *testing.T) {
	fake := newFakePool(
		poolEntry("idx-pool", "loomy-pool.json", true),
		poolEntry("idx-manual", "loomy-manual.json", true),
		poolEntry("idx-other", "loomy-other.json", true),
	)
	fake.json["idx-pool"] = json.RawMessage(`{"type":"loomy","disabled":true,"disabled_by":"loomypool:credit"}`)
	fake.json["idx-manual"] = json.RawMessage(`{"type":"loomy","disabled":true,"disabled_by":"loomypool:manual"}`)
	fake.json["idx-other"] = json.RawMessage(`{"type":"loomy","disabled":true}`)
	far := "2999-01-01"
	fake.statusBody = statusDoc(
		accountDoc("loomy-pool.json", "idx-pool", pointsDoc(50, far)),
		accountDoc("loomy-manual.json", "idx-manual", pointsDoc(50, far)),
		accountDoc("loomy-other.json", "idx-other", pointsDoc(50, far)),
	)
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	if len(fake.saves) != 1 || fake.saves[0].Name != "loomy-pool.json" {
		t.Fatalf("saves = %+v, want exactly one enable write on loomy-pool.json", fake.saves)
	}
	if fake.saves[0].JSON["disabled"] != false {
		t.Errorf("save JSON disabled = %v, want false", fake.saves[0].JSON["disabled"])
	}
	if len(fake.resets) != 1 || fake.resets[0] != "idx-pool" {
		t.Errorf("resets = %v, want [idx-pool]", fake.resets)
	}
	for _, name := range []string{"loomy-manual.json", "loomy-other.json"} {
		if report, ok := reportFor(outcome, name); !ok || report.Action != actionSkipped {
			t.Errorf("%s report = %+v, want skipped — only pool-pulled credentials resurrect", name, report)
		}
	}
}

func TestScanSkipsUnknownPoints(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "loomy-a.json", false))
	// points member absent entirely — the provider could not read the balance.
	fake.statusBody = statusDoc(accountDoc("loomy-a.json", "idx-a", nil))
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	if len(fake.saves) != 0 {
		t.Fatalf("saves = %+v, want none — unknown balance must never be treated as zero", fake.saves)
	}
	if report, ok := reportFor(outcome, "loomy-a.json"); !ok || report.Action != actionSkipped {
		t.Errorf("report = %+v, want skipped", report)
	}
}

func TestScanHonoursWriteGuard(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "loomy-a.json", false))
	fake.statusBody = statusDoc(accountDoc("loomy-a.json", "idx-a", pointsDoc(0, "2999-01-01")))
	fake.install(t)

	cfg := DefaultConfig()
	cfg.AutoDisable = false
	outcome := runScan(testHost(), cfg, "auto")
	if len(fake.saves) != 0 {
		t.Fatalf("saves = %+v, want none when auto_disable=false", fake.saves)
	}
	if report, ok := reportFor(outcome, "loomy-a.json"); !ok || report.Action != actionWriteGuard {
		t.Errorf("report = %+v, want write_guard", report)
	}
}

// The claim lands before the disable decision, so a grant that posts points
// revives an exhausted credential inside one sweep.
func TestScanClaimsAndRevivesSameSweep(t *testing.T) {
	fake := newFakePool(poolEntry("idx-x", "loomy-x.json", true))
	fake.json["idx-x"] = json.RawMessage(`{"type":"loomy","disabled":true,"disabled_by":"loomypool:credit"}`)
	// Stale cycle means the daily grant is due.
	fake.statusBody = statusDoc(accountDoc("loomy-x.json", "idx-x", pointsDoc(0, "2000-01-01")))
	fake.checkinBody = `{"status":"ok","message":"已签到","balance":50,"daily_balance":1000}`
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	var claimCalled bool
	for _, rawURL := range fake.httpCalls {
		if strings.Contains(rawURL, "action=claim") && strings.Contains(rawURL, "auth_index=idx-x") {
			claimCalled = true
		}
	}
	if !claimCalled {
		t.Fatalf("checkin was never requested; httpCalls = %v", fake.httpCalls)
	}
	if len(fake.saves) != 1 || fake.saves[0].JSON["disabled"] != false {
		t.Fatalf("saves = %+v, want one enable write", fake.saves)
	}
	report, ok := reportFor(outcome, "loomy-x.json")
	if !ok {
		t.Fatal("no report for loomy-x.json")
	}
	if report.Action != actionEnabled {
		t.Errorf("Action = %q, want enabled — the grant must drive THIS sweep's decision", report.Action)
	}
	if report.Available == nil || *report.Available != 1050 {
		t.Errorf("Available = %v, want 1050 (post-claim figure)", report.Available)
	}
}

// A dead session inside the checkin answer is the other probe: parking a
// credential whose session is gone keeps relay from burning requests on it.
func TestScanClaimSessionDeadDisables(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "loomy-a.json", false))
	fake.statusBody = statusDoc(accountDoc("loomy-a.json", "idx-a", pointsDoc(0, "2000-01-01")))
	fake.checkinCode = http.StatusUnauthorized
	fake.checkinBody = `{"status":"error","error":"Loomy 返回错误 100002：会话已失效"}`
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if len(fake.saves) != 1 || fake.saves[0].JSON["disabled_by"] != disabledBySession {
		t.Fatalf("saves = %+v, want one disable write marked %s", fake.saves, disabledBySession)
	}
	if report, ok := reportFor(outcome, "loomy-a.json"); !ok || report.Action != actionDisabled {
		t.Errorf("report = %+v, want disabled", report)
	}
}

func TestScanSkipsCheckinWhenNotDue(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "loomy-a.json", false))
	fake.statusBody = statusDoc(accountDoc("loomy-a.json", "idx-a", pointsDoc(900, "2999-01-01")))
	fake.install(t)

	runScan(testHost(), DefaultConfig(), "auto")
	for _, rawURL := range fake.httpCalls {
		if strings.Contains(rawURL, "action=claim") {
			t.Fatalf("checkin was requested for an already-claimed account: %s", rawURL)
		}
	}
}

// One account's failure must not abort the rest of the sweep.
func TestScanContinuesPastOneFailure(t *testing.T) {
	fake := newFakePool(
		poolEntry("idx-a", "loomy-a.json", false),
		poolEntry("idx-b", "loomy-b.json", false),
	)
	far := "2999-01-01"
	fake.statusBody = statusDoc(
		// Not in status doc → a is skipped with no write.
		accountDoc("loomy-b.json", "idx-b", pointsDoc(0, far)),
	)
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	if len(fake.saves) != 1 || fake.saves[0].Name != "loomy-b.json" {
		t.Fatalf("saves = %+v, want exactly the b disable — a's skip must not abort the sweep", fake.saves)
	}
	if report, ok := reportFor(outcome, "loomy-a.json"); !ok || report.Action != actionSkipped {
		t.Errorf("a report = %+v, want skipped", report)
	}
}

func TestScanStatusFailureDoesNothing(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "loomy-a.json", false))
	fake.statusCode = http.StatusNotFound
	fake.statusBody = "404 page not found"
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if outcome.Error == "" {
		t.Fatal("expected a scan error when the status page is unreachable")
	}
	if len(fake.saves) != 0 {
		t.Fatalf("saves = %+v, want none on status failure", fake.saves)
	}
}

// ── live rows / page ──

func TestLiveAccountRowsListsRosterWithoutSweep(t *testing.T) {
	fake := newFakePool(
		poolEntry("idx-a", "loomy-a.json", false),
		poolEntry("idx-b", "loomy-b.json", true),
		pluginapi.HostAuthFileEntry{Provider: "kimi", Type: "kimi", AuthIndex: "idx-k", Name: "kimi.json"},
	)
	fake.install(t)

	rows := liveAccountRows(testHost(), nil)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the 2 loomy credentials only: %+v", len(rows), rows)
	}
	a, b := rows[0], rows[1]
	if a.Name != "loomy-a.json" || a.Status != "正常" || a.Disabled {
		t.Errorf("row a = %+v, want live enabled/normal status", a)
	}
	if !b.Disabled || b.Status != "已停用" {
		t.Errorf("row b = %+v, want live disabled flag and 已停用", b)
	}
}

func TestManagementRegisterAdvertisesScanOnly(t *testing.T) {
	routes := ManagementRegister()
	var menu, scan bool
	for _, route := range routes {
		if route.Menu != "" {
			menu = true
		}
		if route.Path == "/loomy/scan" && route.Method == http.MethodPost {
			scan = true
		}
	}
	if menu || !scan {
		t.Fatalf("routes = %+v, want POST /loomy/scan only (no Menu; /pool is a main-plugin resource)", routes)
	}
}

// ── pure helpers ──

func TestCheckinDueBoundaries(t *testing.T) {
	due := accountDoc("a", "i", pointsDoc(10, "2000-01-01"))
	if !checkinDue(statusAccount{Points: mustPoints(due)}) {
		t.Error("stale cycle should be due")
	}
	if !checkinDue(statusAccount{}) {
		t.Error("missing points should be due (claim is the only probe)")
	}
	fresh := accountDoc("a", "i", pointsDoc(10, "2999-01-01"))
	if checkinDue(statusAccount{Points: mustPoints(fresh)}) {
		t.Error("future cycle should not be due")
	}
}

func mustPoints(account map[string]any) *struct {
	Available      *float64 `json:"available"`
	Balance        *float64 `json:"balance"`
	DailyBalance   *float64 `json:"daily_balance"`
	DailyQuota     *float64 `json:"daily_quota"`
	DailyConsumed  *float64 `json:"daily_consumed"`
	DailyCycleDate string   `json:"daily_cycle_date"`
} {
	raw, _ := json.Marshal(account["points"])
	var points struct {
		Available      *float64 `json:"available"`
		Balance        *float64 `json:"balance"`
		DailyBalance   *float64 `json:"daily_balance"`
		DailyQuota     *float64 `json:"daily_quota"`
		DailyConsumed  *float64 `json:"daily_consumed"`
		DailyCycleDate string   `json:"daily_cycle_date"`
	}
	_ = json.Unmarshal(raw, &points)
	return &points
}

func TestSessionDeadErrorMatchesCodeOnly(t *testing.T) {
	for _, text := range []string{
		"Loomy 返回错误 100002：会话已失效",
		"会话已失效，请重新登录",
	} {
		if !sessionDeadError(text) {
			t.Errorf("%q should be a dead-session error", text)
		}
	}
	for _, text := range []string{"网络超时", "Loomy 返回错误 100001：参数错误"} {
		if sessionDeadError(text) {
			t.Errorf("%q is a retryable error, not session death", text)
		}
	}
}
