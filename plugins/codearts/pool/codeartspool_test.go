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

// fakePool installs a scripted host transport. The pool reaches the host
// through two seams — abiboot's host caller (auth.list/get/save, log,
// reset_cooldown) and the transportFor variable (host.http.do to the codearts
// status page) — so one fixture covers the whole sweep offline.
type fakePool struct {
	mu sync.Mutex
	// files is what host.auth.list reports; json is the file content
	// host.auth.get returns, keyed by auth_index.
	files []pluginapi.HostAuthFileEntry
	json  map[string]json.RawMessage
	// statusBody is served for the codearts status page.
	statusBody string
	statusCode int
	// afterClaimBody is served for the per-account status re-read the sweep
	// issues after a claim (`?format=json&auth_index=…`).
	afterClaimBody string
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
	return &fakePool{files: files, json: map[string]json.RawMessage{}, statusCode: http.StatusOK}
}

func (f *fakePool) install(t *testing.T) {
	t.Helper()
	previousTransport := transportFor
	transportFor = func(*abiboot.Host) doer {
		return func(_, rawURL string, headers http.Header, _ []byte) (*pluginapi.HTTPResponse, error) {
			f.mu.Lock()
			f.httpCalls = append(f.httpCalls, rawURL)
			f.mu.Unlock()
			if strings.Contains(rawURL, "action=checkin") {
				// The claim lives on the HTML representation: any request that
				// does not accept text/html must never reach it.
				if !strings.Contains(headers.Get("Accept"), "text/html") {
					return nil, fmt.Errorf("checkin request without text/html Accept: %s", rawURL)
				}
				return &pluginapi.HTTPResponse{
					StatusCode: http.StatusOK,
					Headers:    map[string][]string{"Content-Type": {"text/html"}},
					Body:       []byte(`<div class="notice success">签到成功，积分 +500</div>`),
				}, nil
			}
			if strings.Contains(rawURL, "/v0/resource/plugins/codearts/status") {
				body := f.statusBody
				if strings.Contains(rawURL, "auth_index=") && f.afterClaimBody != "" {
					body = f.afterClaimBody
				}
				return &pluginapi.HTTPResponse{
					StatusCode: f.statusCode,
					Headers:    map[string][]string{"Content-Type": {"application/json"}},
					Body:       []byte(body),
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
		Provider: "codearts", Type: "codearts",
		AuthIndex: index, Name: name, Disabled: disabled,
	}
}

// statusDoc renders a codearts status document for the given balances. A nil
// entry emits an account with no remaining field at all, which is how the
// provider reports an unreadable balance.
func statusDoc(entries map[string]*float64) string {
	accounts := make([]map[string]any, 0, len(entries))
	for name, remaining := range entries {
		account := map[string]any{"name": name, "status": "active"}
		if remaining != nil {
			account["remaining"] = *remaining
		}
		accounts = append(accounts, account)
	}
	raw, _ := json.Marshal(map[string]any{"accounts": accounts, "account_count": len(accounts)})
	return string(raw)
}

// statusDocFull renders one account with every figure the provider publishes,
// the shape a healthy account entry takes on the real status page.
func statusDocFull() string {
	raw, _ := json.Marshal(map[string]any{
		"account_count": 1,
		"accounts": []map[string]any{{
			"name":           "codearts-a.json",
			"auth_index":     "idx-a",
			"label":          "账号A",
			"status":         "正常",
			"remaining":      904.51,
			"used":           95.49,
			"total":          1000,
			"credit_package": true,
			"daily_checkin": map[string]any{
				"campaign_id": "c1", "claimable": true, "status": "CLAIMABLE",
			},
		}},
	})
	return string(raw)
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

func TestScanDisablesExhaustedAccount(t *testing.T) {
	fake := newFakePool(
		poolEntry("idx-a", "codearts-a.json", false),
		poolEntry("idx-b", "codearts-b.json", false),
	)
	fake.statusBody = statusDoc(map[string]*float64{
		"codearts-a.json": f64(0),
		"codearts-b.json": f64(100),
	})
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "manual")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	if len(fake.saves) != 1 {
		t.Fatalf("saves = %d, want 1 (%+v)", len(fake.saves), fake.saves)
	}
	save := fake.saves[0]
	if save.Name != "codearts-a.json" || save.JSON["disabled"] != true || save.JSON["disabled_by"] != disabledByCredit {
		t.Errorf("save = %+v, want disabled:true + disabled_by %s on codearts-a.json", save, disabledByCredit)
	}
	if report, ok := reportFor(outcome, "codearts-a.json"); !ok || report.Action != actionDisabled {
		t.Errorf("account a report = %+v, want action=disabled", report)
	}
	if report, ok := reportFor(outcome, "codearts-b.json"); !ok || report.Action != actionNone {
		t.Errorf("account b report = %+v, want action=none", report)
	}
	if outcome.DisabledNow != 1 {
		t.Errorf("DisabledNow = %d, want 1", outcome.DisabledNow)
	}
}

func TestScanResurrectsOnlyPoolDisabled(t *testing.T) {
	fake := newFakePool(
		poolEntry("idx-pool", "codearts-pool.json", true),
		poolEntry("idx-manual", "codearts-manual.json", true),
	)
	fake.json["idx-pool"] = json.RawMessage(`{"type":"codearts","disabled":true,"disabled_by":"codeartspool:credit"}`)
	fake.json["idx-manual"] = json.RawMessage(`{"type":"codearts","disabled":true}`)
	fake.statusBody = statusDoc(map[string]*float64{
		"codearts-pool.json":   f64(50),
		"codearts-manual.json": f64(50),
	})
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	if len(fake.saves) != 1 || fake.saves[0].Name != "codearts-pool.json" {
		t.Fatalf("saves = %+v, want exactly one enable write on codearts-pool.json", fake.saves)
	}
	if fake.saves[0].JSON["disabled"] != false {
		t.Errorf("save JSON disabled = %v, want false", fake.saves[0].JSON["disabled"])
	}
	if len(fake.resets) != 1 || fake.resets[0] != "idx-pool" {
		t.Errorf("resets = %v, want [idx-pool]", fake.resets)
	}
	if report, ok := reportFor(outcome, "codearts-manual.json"); !ok || report.Action != actionSkipped {
		t.Errorf("manual-disabled report = %+v, want skipped (marker absent)", report)
	}
}

func TestScanSkipsUnknownBalance(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "codearts-a.json", false))
	fake.statusBody = statusDoc(map[string]*float64{"codearts-a.json": nil})
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	if len(fake.saves) != 0 {
		t.Fatalf("saves = %+v, want none — unknown balance must never be treated as zero", fake.saves)
	}
	if report, ok := reportFor(outcome, "codearts-a.json"); !ok || report.Action != actionSkipped {
		t.Errorf("report = %+v, want skipped", report)
	}
}

func TestScanHonoursWriteGuard(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "codearts-a.json", false))
	fake.statusBody = statusDoc(map[string]*float64{"codearts-a.json": f64(0)})
	fake.install(t)

	cfg := DefaultConfig()
	cfg.AutoDisable = false
	outcome := runScan(testHost(), cfg, "auto")
	if len(fake.saves) != 0 {
		t.Fatalf("saves = %+v, want none when auto_disable=false", fake.saves)
	}
	if report, ok := reportFor(outcome, "codearts-a.json"); !ok || report.Action != actionWriteGuard {
		t.Errorf("report = %+v, want write_guard", report)
	}
}

func TestScanCarriesAccountFigures(t *testing.T) {
	entry := poolEntry("idx-a", "codearts-a.json", false)
	entry.Label = "账号A"
	fake := newFakePool(entry)
	fake.statusBody = statusDocFull()
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	report, ok := reportFor(outcome, "codearts-a.json")
	if !ok {
		t.Fatal("no report for codearts-a.json")
	}
	if report.Remaining == nil || *report.Remaining != 904.51 ||
		report.Used == nil || *report.Used != 95.49 ||
		report.Total == nil || *report.Total != 1000 {
		t.Errorf("quota triple = %v/%v/%v, want 904.51/95.49/1000",
			report.Remaining, report.Used, report.Total)
	}
	if report.Label != "账号A" {
		t.Errorf("Label = %q, want 账号A", report.Label)
	}
	if report.Checkin != "可签到" {
		t.Errorf("Checkin = %q, want 可签到", report.Checkin)
	}
	if report.Status != "正常" {
		t.Errorf("Status = %q, want 正常", report.Status)
	}
}

func TestScanClaimsAndRevivesSameSweep(t *testing.T) {
	fake := newFakePool(poolEntry("idx-x", "codearts-x.json", true))
	fake.json["idx-x"] = json.RawMessage(`{"type":"codearts","disabled":true,"disabled_by":"codeartspool:credit"}`)
	// Pre-claim state: spent but claimable. Post-claim re-read reports +500.
	raw, _ := json.Marshal(map[string]any{"account_count": 1, "accounts": []map[string]any{{
		"name": "codearts-x.json", "auth_index": "idx-x", "status": "已停用",
		"remaining": 0, "used": 4500, "total": 4500,
		"daily_checkin": map[string]any{"campaign_id": "c1", "claimable": true, "status": "CLAIMABLE"},
	}}})
	fake.statusBody = string(raw)
	fake.afterClaimBody = `{"remaining":500,"daily_checkin":{"claimable":false,"status":"CLAIMED"}}`
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	if outcome.Error != "" {
		t.Fatalf("scan error: %s", outcome.Error)
	}
	var claimCalled bool
	for _, rawURL := range fake.httpCalls {
		if strings.Contains(rawURL, "action=checkin") && strings.Contains(rawURL, "auth_index=idx-x") {
			claimCalled = true
		}
	}
	if !claimCalled {
		t.Fatalf("checkin was never requested; httpCalls = %v", fake.httpCalls)
	}
	if len(fake.saves) != 1 || fake.saves[0].JSON["disabled"] != false {
		t.Fatalf("saves = %+v, want one enable write", fake.saves)
	}
	report, ok := reportFor(outcome, "codearts-x.json")
	if !ok {
		t.Fatal("no report for codearts-x.json")
	}
	if report.Action != actionEnabled {
		t.Errorf("Action = %q, want enabled — the +500 grant must drive THIS sweep's decision", report.Action)
	}
	if report.Remaining == nil || *report.Remaining != 500 {
		t.Errorf("Remaining = %v, want 500 (post-claim figure)", report.Remaining)
	}
	if report.CheckinResult == "" {
		t.Error("CheckinResult empty, want the provider's notice text")
	}
}

func TestScanSkipsCheckinWhenNotClaimable(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "codearts-a.json", false))
	raw, _ := json.Marshal(map[string]any{"account_count": 1, "accounts": []map[string]any{{
		"name": "codearts-a.json", "remaining": 900,
		"daily_checkin": map[string]any{"campaign_id": "c1", "claimable": false, "status": "CONSUMED"},
	}}})
	fake.statusBody = string(raw)
	fake.install(t)

	outcome := runScan(testHost(), DefaultConfig(), "auto")
	for _, rawURL := range fake.httpCalls {
		if strings.Contains(rawURL, "action=checkin") {
			t.Fatalf("checkin was requested for a non-claimable account: %s", rawURL)
		}
	}
	if report, ok := reportFor(outcome, "codearts-a.json"); !ok || report.CheckinResult != "" {
		t.Errorf("report = %+v, want no CheckinResult", report)
	}
}

func TestScanStatusFailureDoesNothing(t *testing.T) {
	fake := newFakePool(poolEntry("idx-a", "codearts-a.json", false))
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

// The page used to render only the remembered sweep: after a restart wiped
// the in-memory outcome — and for any account added since the last tick — the
// roster showed nothing at all. liveAccountRows exists so a plain page load
// still lists every credential from the auth files alone.
func TestLiveAccountRowsListsRosterWithoutSweep(t *testing.T) {
	fake := newFakePool(
		poolEntry("idx-a", "codearts-a.json", false),
		poolEntry("idx-b", "codearts-b.json", true),
		// A foreign provider's credential must never land in the pool table.
		pluginapi.HostAuthFileEntry{Provider: "kimi", Type: "kimi", AuthIndex: "idx-k", Name: "kimi.json"},
	)
	fake.install(t)

	rows, err := liveAccountRows(testHost(), nil)
	if err != nil {
		t.Fatalf("liveAccountRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the 2 codearts credentials only: %+v", len(rows), rows)
	}
	a, b := rows[0], rows[1]
	if a.Name != "codearts-a.json" || a.Status != "正常" || a.Disabled {
		t.Errorf("row a = %+v, want live enabled/normal status", a)
	}
	if a.Remaining != nil || a.Action != "" {
		t.Errorf("row a = %+v, want no measured data before any sweep", a)
	}
	if !b.Disabled || b.Status != "已停用" {
		t.Errorf("row b = %+v, want live disabled flag and 已停用", b)
	}
}

// Post-action clicks and post-scan additions both change the live roster, so
// the live fields must win over whatever the stale remembered sweep recorded.
func TestLiveAccountRowsOverlaysSweepButLiveStateWins(t *testing.T) {
	fake := newFakePool(
		poolEntry("idx-a", "codearts-a.json", true), // disabled since the sweep ran
		poolEntry("idx-b", "codearts-b.json", false),
	)
	fake.install(t)

	outcome := &scanOutcome{
		Accounts: []accountReport{{
			Name: "codearts-a.json", Disabled: false, Status: "正常",
			Remaining: f64(12.5), Used: f64(7.5), Total: f64(20),
			Checkin: "可签到", Action: actionNone,
		}},
	}
	rows, err := liveAccountRows(testHost(), outcome)
	if err != nil {
		t.Fatalf("liveAccountRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	a := rows[0]
	if !a.Disabled || a.Status != "已停用" {
		t.Errorf("row a = %+v, want the LIVE disabled flag, not the sweep's stale false", a)
	}
	if a.Remaining == nil || *a.Remaining != 12.5 || a.Checkin != "可签到" {
		t.Errorf("row a = %+v, want the sweep's measured columns overlaid", a)
	}
	b := rows[1]
	if b.Detail != "未在上次巡检中" {
		t.Errorf("row b = %+v, want 未在上次巡检中 for an account added after the sweep", b)
	}
	if b.Remaining != nil {
		t.Errorf("row b = %+v, want no measured columns", b)
	}
}

func forgetScan() {
	lastScan = atomic.Value{}
}

// testHost returns a Host carrying a callback identity.
func testHost() *abiboot.Host {
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"test-callback"}`))
}
