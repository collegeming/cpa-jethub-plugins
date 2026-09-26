package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// allAccounts is the credential list the orchestration tests start from: one
// account per check-in-capable provider, two for qoder, one for the unsupported
// cline (which the hub must show but never call).
func allAccounts() []pluginapi.HostAuthFileEntry {
	return []pluginapi.HostAuthFileEntry{
		account("codearts", "ca-1", "codearts-1.json"),
		account("codebuddy", "cb-1", "codebuddy-1.json"),
		account("qoder", "qd-1", "qoder-1.json"),
		account("qoder", "qd-2", "qoder-2.json"),
		account("trae", "tr-1", "trae-1.json"),
		account("lobsterai", "lb-1", "lobster-1.json"),
		// Loomy is matched through Type instead of Provider on purpose: every
		// provider plugin accepts either field and the hub must too.
		{Type: "loomy", AuthIndex: "lo-1", Name: "loomy-1.json"},
		account("cline", "cl-1", "cline-1.json"),
	}
}

// codeartsStatusBody is the account document codearts emits with
// `?format=json`. The claim itself has no JSON representation.
func codeartsStatusBody(status, claimable string, remaining float64) string {
	return `{"auth_index":"ca-1","name":"codearts-1.json","remaining":` +
		trimFloat(remaining) + `,"daily_checkin":{"campaign_id":"c-1","claimable":` + claimable +
		`,"status":"` + status + `"}}`
}

// codeartsStatusRoute scripts that document for one URL fragment.
func codeartsStatusRoute(match, status, claimable string, remaining float64) route {
	return jsonRoute(match, codeartsStatusBody(status, claimable, remaining))
}

// withCodeartsLifecycle wraps a scripted transport so the three status reads of
// one codearts claim answer in order: enumerate, pre-claim read, post-claim
// read. The claim URL itself (no format=json) still falls through.
func withCodeartsLifecycle(fake *fakeHost) {
	inner := fake.do
	stage := 0
	fake.do = func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(rawURL, "/codearts/status?") && strings.Contains(rawURL, "format=json") {
			stage++
			if stage <= 2 {
				return jsonResponse(codeartsStatusBody("NOT_CLAIMED", "true", 10)), nil
			}
			return jsonResponse(codeartsStatusBody("CLAIMED", "false", 15)), nil
		}
		return inner(method, rawURL, headers, body)
	}
}

// providerStatusRoutes scripts every provider's `?format=json` status page in
// the exact field shape that provider's handler emits (verified against
// plugins/*/management.go and plugins/*/pluginui.go).
func providerStatusRoutes() []route {
	return []route{
		jsonRoute("/codebuddy/status?", `{"product":"codebuddy","account_count":1,"accounts":[{"auth_index":"cb-1","name":"codebuddy-1.json"}]}`),
		jsonRoute("/qoder/status?", `{"provider":"qoder","accounts":2,"account":{"auth_index":"qd-1","name":"qoder-1.json"}}`),
		jsonRoute("/trae/status?", `{"account_count":1,"accounts":[{"auth_index":"tr-1","name":"trae-1.json"}]}`),
		jsonRoute("/lobsterai/status?", `{"accounts":[{"auth_index":"lb-1","name":"lobster-1.json","label":"有道账号"}]}`),
		jsonRoute("/loomy/status?", `{"provider":"loomy","accounts":1,"account":{"auth_index":"lo-1","name":"loomy-1.json"}}`),
		codeartsStatusRoute("/codearts/status?", "NOT_CLAIMED", "true", 10),
	}
}

// checkinRoutes scripts every provider's check-in answer, using each provider's
// own vocabulary.
func checkinRoutes() []route {
	return []route{
		// The second qoder account is scripted before the general route: routes
		// are matched in order.
		jsonRoute("auth_index=qd-2", `{"status":"already-claimed","message":"今天已领取"}`),
		jsonRoute("/qoder/checkin?", `{"status":"claimed","message":"领取成功","amount":5}`),
		jsonRoute("/trae/checkin?", `{"claim":{"status":"claimed","message":"签到成功","credit":10,"streak_days":3}}`),
		jsonRoute("/lobsterai/checkin?", `{"status":"already-claimed","message":"今天已签到","credit_granted":false}`),
		jsonRoute("/loomy/checkin?", `{"status":"claimed","message":"初始化每日额度成功","credit":100,"balance":100}`),
		jsonRoute("/codebuddy/checkin?", `{"supported":true,"product":"codebuddy","outcome":"claimed","message":"签到成功","credit":2}`),
		htmlRoute("/codearts/status?action=checkin", `<div class="notice success">签到成功，获得 5.00 额度</div>`),
	}
}

// scriptedRoutes is the full scripted surface. Check-in routes come first
// because codearts' claim shares its status path, then the status documents,
// then the host's 404 for every route nobody registered.
func scriptedRoutes() []route {
	return append(append(checkinRoutes(), providerStatusRoutes()...), catchAll())
}

// TestBareStatusLoadNeverClaims is the safety property the panel depends on:
// embedding the overview in an iframe must be unable to claim anything.
//
// The page DOES read every provider's status document — that is where the rows
// come from — so the assertion is not "no request" but "no request that could
// write": the probe is a plain `status?format=json`, and no URL a bare load
// issues carries the `action` parameter that performs a claim.
func TestBareStatusLoadNeverClaims(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(allAccounts()...)
	fake.install(t)
	fake.serve(scriptedRoutes()...)
	withSettings(t, testConfig())

	// HTML page load.
	response := dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status", nil, "text/html"))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	page := string(response.Body)
	for _, want := range []string{"渠道总览", "一键签到", "Qoder", "Cline", "不支持签到", "data:image/"} {
		if !strings.Contains(page, want) {
			t.Fatalf("status page is missing %q", want)
		}
	}

	// JSON representation of the same route.
	response = dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status",
		url.Values{"format": {"json"}}, "application/json"))
	document := map[string]any{}
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("status document is not JSON: %v", errUnmarshal)
	}
	if document["claiming"] != false {
		t.Fatalf("claiming = %v, want false", document["claiming"])
	}
	if document["last_run"] != nil {
		t.Fatalf("last_run = %v, want null before the first run", document["last_run"])
	}
	channels, okChannels := document["channels"].([]any)
	if !okChannels || len(channels) != len(targetCatalogue()) {
		t.Fatalf("channels = %#v, want one row per catalogue provider (%d)", document["channels"], len(targetCatalogue()))
	}

	// The script-facing route without the explicit action is not a write either.
	dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/checkin", nil, "text/html"))
	dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/checkin",
		url.Values{"format": {"json"}}, "application/json"))

	requests := fake.requestURLs()
	if len(requests) == 0 {
		t.Fatal("the overview did not probe any provider status document")
	}
	for _, rawURL := range requests {
		if !strings.Contains(rawURL, "/status?") || !strings.Contains(rawURL, "format=json") {
			t.Fatalf("a bare page load issued something other than a status read: %s", rawURL)
		}
	}
	assertNoClaimRequest(t, fake)
}

// assertNoClaimRequest fails when any recorded request could write. A claim is
// either the provider's /checkin route or the `action` parameter its status page
// reads; a read-only probe may carry neither.
func assertNoClaimRequest(t *testing.T, fake *fakeHost) {
	t.Helper()
	for _, rawURL := range fake.requestURLs() {
		if strings.Contains(rawURL, "/checkin") || strings.Contains(rawURL, "action=") {
			t.Fatalf("a read-only request could claim: %s", rawURL)
		}
	}
}

// countClaimRequests counts the recorded requests that carry the claim parameter.
func countClaimRequests(fake *fakeHost) int {
	count := 0
	for _, rawURL := range fake.requestURLs() {
		if strings.Contains(rawURL, "action=") {
			count++
		}
	}
	return count
}

// TestClaimIsOptInOnly asserts the write gate on both routes: no action, no
// check-in request; action=checkin, requests happen and are reported as
// claiming.
func TestClaimIsOptInOnly(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(account("qoder", "qd-1", "qoder-1.json"))
	fake.install(t)
	fake.serve(scriptedRoutes()...)
	withSettings(t, testConfig("qoder"))

	for _, probe := range []pluginapi.ManagementRequest{
		managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status", nil, "text/html"),
		managementRequest(http.MethodGet, "/v0/resource/plugins/hub/checkin", nil, "text/html"),
		managementRequest(http.MethodGet, "/v0/resource/plugins/hub/checkin", url.Values{"format": {"json"}}, "application/json"),
	} {
		dispatchManagement(t, probe)
	}
	assertNoClaimRequest(t, fake)

	response := dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status",
		url.Values{"action": {"checkin"}, "format": {"json"}}, "application/json"))
	document := map[string]any{}
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("run document is not JSON: %v", errUnmarshal)
	}
	if document["claiming"] != true {
		t.Fatalf("claiming = %v, want true with action=checkin", document["claiming"])
	}
	found := fake.requestFor("/qoder/checkin")
	if !strings.Contains(found, "action=checkin") {
		t.Fatalf("check-in request was not sent: %v", fake.requestURLs())
	}
}

// TestExactCheckinRequestsPerProvider pins the query parameters of every
// provider, because they are NOT uniform: trae and loomy only claim with
// `action=claim`, codearts only claims from its status page, and codebuddy only
// claims over GET when `action=checkin` is present.
func TestExactCheckinRequestsPerProvider(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(allAccounts()...)
	fake.install(t)
	fake.serve(scriptedRoutes()...)

	withSettings(t, testConfig())
	result := newRunner(testHost(), settings()).run()
	if len(result.Providers) != len(targetCatalogue()) {
		t.Fatalf("ran %d providers, want %d", len(result.Providers), len(targetCatalogue()))
	}

	checks := []struct {
		provider string
		want     []string
		avoid    []string
	}{
		{provider: "qoder", want: []string{"/v0/resource/plugins/qoder/checkin?", "action=checkin", "auth_index=qd-1", "format=json"}},
		{provider: "trae", want: []string{"/v0/resource/plugins/trae/checkin?", "action=claim", "auth_index=tr-1", "format=json"}},
		{provider: "lobsterai", want: []string{"/v0/resource/plugins/lobsterai/checkin?", "action=checkin", "auth_index=lb-1", "format=json"}},
		{provider: "loomy", want: []string{"/v0/resource/plugins/loomy/checkin?", "action=claim", "auth_index=lo-1", "format=json"}},
		{provider: "codebuddy", want: []string{"/v0/resource/plugins/codebuddy/checkin?", "action=checkin", "auth_index=cb-1", "format=json"}},
		// codearts is the exception: the claim is a query string on the STATUS
		// page and must NOT ask for JSON, which would skip the write.
		{provider: "codearts", want: []string{"/v0/resource/plugins/codearts/status?", "action=checkin", "auth_index=ca-1"}, avoid: []string{"format=json"}},
	}
	for _, check := range checks {
		found := ""
		for _, rawURL := range fake.requestURLs() {
			if strings.Contains(rawURL, "/"+check.provider+"/") && strings.Contains(rawURL, "action=") {
				found = rawURL
				break
			}
		}
		if found == "" {
			t.Fatalf("%s: no check-in request was sent: %v", check.provider, fake.requestURLs())
		}
		for _, want := range check.want {
			if !strings.Contains(found, want) {
				t.Fatalf("%s: request %s is missing %q", check.provider, found, want)
			}
		}
		for _, avoid := range check.avoid {
			if strings.Contains(found, avoid) {
				t.Fatalf("%s: request %s must not contain %q", check.provider, found, avoid)
			}
		}
	}

	for _, rawURL := range fake.requestURLs() {
		// Cline has no check-in endpoint, so the run must not touch it at all.
		if strings.Contains(rawURL, "/cline/") {
			t.Fatalf("cline was called although it has no check-in endpoint: %s", rawURL)
		}
		// Loomy's one-off onboarding tasks are NOT part of the daily grant.
		if strings.Contains(rawURL, "onboarding") {
			t.Fatalf("onboarding was driven although it is a one-off task: %s", rawURL)
		}
	}

	// codearts: the claim must be requested as HTML, because passing
	// format=json makes its status route skip the write entirely.
	claimIndex := -1
	for index, rawURL := range fake.requestURLs() {
		if strings.Contains(rawURL, "/codearts/status?") && strings.Contains(rawURL, "action=checkin") {
			claimIndex = index
		}
	}
	if claimIndex < 0 {
		t.Fatalf("codearts claim request missing: %v", fake.requestURLs())
	}
	if accept := fake.requestAccepts()[claimIndex]; !strings.Contains(accept, "text/html") {
		t.Fatalf("codearts claim Accept = %q, want text/html (otherwise the provider skips the claim)", accept)
	}
}

// TestAggregatedVerdicts runs the whole catalogue against scripted answers and
// checks the aggregated table, including the honest distinction between a fresh
// claim and a repeat one, and the reporting of providers that are absent or
// unsupported.
func TestAggregatedVerdicts(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(allAccounts()...)
	fake.install(t)
	fake.serve(scriptedRoutes()...)
	withCodeartsLifecycle(fake)
	withSettings(t, testConfig())

	result := newRunner(testHost(), settings()).run()

	want := map[string]rowKind{
		"codearts":       kindClaimed,
		"codebuddy":      kindClaimed,
		"codebuddy-intl": kindUnavailable,
		"workbuddy-cn":   kindUnavailable,
		"workbuddy":      kindUnavailable,
		"lobsterai":      kindAlreadyClaimed,
		"loomy":          kindClaimed,
		"trae":           kindClaimed,
		"cline":          kindUnsupported,
	}
	for provider, kind := range want {
		rows := rowsFor(t, result, provider)
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows, want 1", provider, len(rows))
		}
		if rows[0].Result != kind {
			t.Fatalf("%s: result = %s (%s), want %s", provider, rows[0].Result, rows[0].Message, kind)
		}
	}

	qoderRows := rowsFor(t, result, "qoder")
	if len(qoderRows) != 2 {
		t.Fatalf("qoder: %d rows, want 2", len(qoderRows))
	}
	if qoderRows[0].Result != kindClaimed || !strings.Contains(qoderRows[0].Message, "本次 +5") {
		t.Fatalf("qoder row 1 = %+v, want claimed with the provider's own amount", qoderRows[0])
	}
	if qoderRows[1].Result != kindAlreadyClaimed {
		t.Fatalf("qoder row 2 = %+v, want already-claimed", qoderRows[1])
	}
	if qoderRows[1].Message != "今天已领取" {
		t.Fatalf("qoder row 2 message = %q, want the provider's own words", qoderRows[1].Message)
	}

	codeartsRow := rowsFor(t, result, "codearts")[0]
	if !strings.Contains(codeartsRow.Message, "签到成功") {
		t.Fatalf("codearts message = %q, want the notice the provider rendered", codeartsRow.Message)
	}
	if !strings.Contains(codeartsRow.Message, "本次 +5") {
		t.Fatalf("codearts message = %q, want the balance delta from the provider's JSON", codeartsRow.Message)
	}
	if codeartsRow.ProviderStatus != "CLAIMED" {
		t.Fatalf("codearts provider status = %q, want CLAIMED", codeartsRow.ProviderStatus)
	}

	summary := result.summary()
	if summary["total"] != 11 {
		t.Fatalf("summary total = %d, want 11: %v", summary["total"], summary)
	}
	for _, pair := range []struct {
		kind rowKind
		want int
	}{
		{kindClaimed, 5},
		{kindAlreadyClaimed, 2},
		{kindUnavailable, 3},
		{kindUnsupported, 1},
		{kindFailed, 0},
	} {
		if summary[string(pair.kind)] != pair.want {
			t.Fatalf("summary[%s] = %d, want %d (%v)", pair.kind, summary[string(pair.kind)], pair.want, summary)
		}
	}
}

// TestCodeartsRepeatClaimIsReportedHonestly checks the branch that decides
// between "claimed now" and "already claimed today" when the codearts pre-state
// already shows the benefit was taken.
func TestCodeartsRepeatClaimIsReportedHonestly(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(account("codearts", "ca-1", "codearts-1.json"))
	fake.install(t)
	// Enumeration, pre-claim read and post-claim read all report CLAIMED.
	fake.serve(
		htmlRoute("/codearts/status?action=checkin", `<div class="notice">今日已签到</div>`),
		codeartsStatusRoute("/codearts/status?", "CLAIMED", "false", 15),
	)

	withSettings(t, testConfig("codearts"))
	result := newRunner(testHost(), settings()).run()
	rows := rowsFor(t, result, "codearts")
	if len(rows) != 1 {
		t.Fatalf("codearts: %d rows, want 1", len(rows))
	}
	if rows[0].Result != kindAlreadyClaimed {
		t.Fatalf("result = %s, want already-claimed (the pre-claim state already said CLAIMED)", rows[0].Result)
	}
	if !strings.HasPrefix(rows[0].Message, "今日已签到") {
		t.Fatalf("message = %q, want the provider's own notice", rows[0].Message)
	}
	if !strings.Contains(rows[0].Message, "剩余 15") {
		t.Fatalf("message = %q, want the post-claim balance the provider reported", rows[0].Message)
	}
}

// TestCodeartsUnreadableStateIsNotUpgradedToClaimed: when the state cannot be
// read at all the rendered notice decides, and a failure notice must not become
// a success.
func TestCodeartsUnreadableStateIsNotUpgradedToClaimed(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(account("codearts", "ca-1", "codearts-1.json"))
	fake.install(t)
	fake.serve(
		htmlRoute("/codearts/status?action=checkin", `<div class="notice danger">签到失败：上游 500</div>`),
		jsonRoute("/codearts/status?", `{"auth_index":"ca-1","name":"codearts-1.json"}`),
	)

	withSettings(t, testConfig("codearts"))
	result := newRunner(testHost(), settings()).run()
	rows := rowsFor(t, result, "codearts")
	if rows[0].Result != kindFailed {
		t.Fatalf("result = %s, want failed", rows[0].Result)
	}
	if !strings.Contains(rows[0].Message, "签到失败") {
		t.Fatalf("message = %q, want the provider's failure notice", rows[0].Message)
	}
}

// TestCodeartsNoticeWinsWhenTheStateReadLags: the claim page is the direct
// answer to this call, so when the follow-up state read still shows the benefit
// as available the rendered notice decides — and the disagreement is reported
// instead of being hidden.
func TestCodeartsNoticeWinsWhenTheStateReadLags(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(account("codearts", "ca-1", "codearts-1.json"))
	fake.install(t)
	fake.serve(
		htmlRoute("/codearts/status?action=checkin", `<div class="notice success">签到成功，获得 5.00 额度</div>`),
		codeartsStatusRoute("/codearts/status?", "NOT_CLAIMED", "true", 10),
	)

	withSettings(t, testConfig("codearts"))
	result := newRunner(testHost(), settings()).run()
	rows := rowsFor(t, result, "codearts")
	if rows[0].Result != kindClaimed {
		t.Fatalf("result = %s (%s), want claimed: the claim page itself reported success", rows[0].Result, rows[0].Message)
	}
	if !strings.Contains(rows[0].Message, "状态读取可能滞后") {
		t.Fatalf("message = %q, want the disagreement to be visible", rows[0].Message)
	}
}

// TestUnavailableAndDisabledProviders: an unregistered resource route (plugin
// missing) is reported as 未启用, and a disabled credential is skipped instead
// of being claimed.
func TestUnavailableAndDisabledProviders(t *testing.T) {
	resetLastRun(t)
	disabled := account("qoder", "qd-2", "qoder-2.json")
	disabled.Disabled = true
	fake := newFakeHost(account("qoder", "qd-1", "qoder-1.json"), disabled)
	fake.install(t)
	fake.serve(
		jsonRoute("/qoder/status?", `{"provider":"qoder","accounts":2,"account":{"auth_index":"qd-1","name":"qoder-1.json"}}`),
		jsonRoute("auth_index=qd-1", `{"status":"claimed","message":"领取成功"}`),
		catchAll(),
	)
	withSettings(t, testConfig("qoder", "trae"))

	result := newRunner(testHost(), settings()).run()
	qoderRows := rowsFor(t, result, "qoder")
	if len(qoderRows) != 2 {
		t.Fatalf("qoder rows = %d, want 2", len(qoderRows))
	}
	if qoderRows[1].ResultText != "已停用" {
		t.Fatalf("disabled row = %+v, want a skipped/disabled result", qoderRows[1])
	}
	for _, rawURL := range fake.requestURLs() {
		if strings.Contains(rawURL, "auth_index=qd-2") {
			t.Fatalf("a disabled credential was driven: %s", rawURL)
		}
	}
	traeRows := rowsFor(t, result, "trae")
	if len(traeRows) != 1 || traeRows[0].Result != kindUnavailable {
		t.Fatalf("trae rows = %+v, want one 未启用 row (its resource route is not registered)", traeRows)
	}
}

// TestAccountWithoutIndexIsNotDriven: a credential the host lists without a
// runtime index would make the provider fall back to its first account, so the
// entry must be skipped instead of silently checking in the wrong account.
func TestAccountWithoutIndexIsNotDriven(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(
		pluginapi.HostAuthFileEntry{Provider: "qoder", Name: "qoder-nodindex.json"},
		account("qoder", "qd-1", "qoder-1.json"),
	)
	fake.install(t)
	fake.serve(
		jsonRoute("/qoder/status?", `{"provider":"qoder","accounts":2,"account":{"auth_index":"qd-1","name":"qoder-1.json"}}`),
		jsonRoute("/qoder/checkin?", `{"status":"claimed","message":"领取成功"}`),
	)
	withSettings(t, testConfig("qoder"))

	result := newRunner(testHost(), settings()).run()
	rows := rowsFor(t, result, "qoder")
	if len(rows) != 2 {
		t.Fatalf("qoder rows = %d, want 2", len(rows))
	}
	if rows[0].ResultText != "缺少索引" {
		t.Fatalf("first row = %+v, want the index-less credential skipped", rows[0])
	}
	checkins := 0
	for _, rawURL := range fake.requestURLs() {
		if strings.Contains(rawURL, "/qoder/checkin?") {
			checkins++
			if strings.Contains(rawURL, "auth_index=&") || strings.HasSuffix(rawURL, "auth_index=") {
				t.Fatalf("a check-in was sent without an auth_index: %s", rawURL)
			}
		}
	}
	if checkins != 1 {
		t.Fatalf("check-in requests = %d, want exactly 1 (only the indexed credential)", checkins)
	}
}

// TestCheckinRefusedWhenDisabled: enabled=false must stop the writes and say so.
func TestCheckinRefusedWhenDisabled(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(account("qoder", "qd-1", "qoder-1.json"))
	fake.install(t)
	cfg := testConfig()
	cfg.Enabled = false
	withSettings(t, cfg)

	response := dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status",
		url.Values{"action": {"checkin"}, "format": {"json"}}, "application/json"))
	document := map[string]any{}
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("document is not JSON: %v", errUnmarshal)
	}
	if document["enabled"] != false {
		t.Fatalf("document = %v, want enabled=false", document)
	}
	if requests := fake.requestURLs(); len(requests) != 0 {
		t.Fatalf("disabled plugin performed %d requests, want 0: %v", len(requests), requests)
	}
}

// TestRequestTimeoutIsEnforced covers the plugin-side deadline: the host's
// http.do callback takes no timeout, so an unresponsive route must still be
// abandoned instead of hanging the run.
func TestRequestTimeoutIsEnforced(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(account("qoder", "qd-1", "qoder-1.json"))
	fake.install(t)
	fake.do = func(_, _ string, _ http.Header, _ []byte) (*pluginapi.HTTPResponse, error) {
		time.Sleep(300 * time.Millisecond)
		return jsonResponse(`{"provider":"qoder"}`), nil
	}
	cfg := testConfig("qoder")
	cfg.TimeoutMS = 20
	withSettings(t, cfg)

	started := time.Now()
	result := newRunner(testHost(), settings()).run()
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("run took %s, want it abandoned after the 20ms deadline", elapsed)
	}
	rows := rowsFor(t, result, "qoder")
	if len(rows) != 1 || rows[0].Result != kindFailed {
		t.Fatalf("rows = %+v, want one failed row", rows)
	}
	if !strings.Contains(rows[0].Message, "超时") {
		t.Fatalf("message = %q, want a timeout explanation", rows[0].Message)
	}
}

// TestRunDocumentShape pins the machine-readable aggregated result.
func TestRunDocumentShape(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(account("qoder", "qd-1", "qoder-1.json"))
	fake.install(t)
	fake.serve(
		jsonRoute("/qoder/status?", `{"provider":"qoder","accounts":1,"account":{"auth_index":"qd-1","name":"qoder-1.json"}}`),
		jsonRoute("/qoder/checkin?", `{"status":"claimed","message":"领取成功","amount":5}`),
	)
	withSettings(t, testConfig("qoder"))
	result := newRunner(testHost(), settings()).run()
	document := runDocument(result)

	rows, okRows := document["rows"].([]map[string]any)
	if !okRows || len(rows) != 1 {
		t.Fatalf("rows = %#v, want exactly one", document["rows"])
	}
	for _, key := range []string{"provider", "provider_label", "auth_index", "account", "result", "result_text", "message", "request", "http_status"} {
		if _, ok := rows[0][key]; !ok {
			t.Fatalf("row is missing %q: %#v", key, rows[0])
		}
	}
	if rows[0]["result"] != string(kindClaimed) {
		t.Fatalf("result = %v, want claimed", rows[0]["result"])
	}
	summary, okSummary := document["summary"].(map[string]int)
	if !okSummary || summary["total"] != 1 || summary[string(kindClaimed)] != 1 {
		t.Fatalf("summary = %#v", document["summary"])
	}
	providers, okProviders := document["providers"].([]map[string]any)
	if !okProviders || len(providers) != 1 {
		t.Fatalf("providers = %#v", document["providers"])
	}
	if providers[0]["accounts"] != 1 {
		t.Fatalf("provider accounts = %v, want 1", providers[0]["accounts"])
	}
}

// TestReportedAccountDisagreementIsVisible: when the provider's own document
// reports a different account count than the host list, the run says so instead
// of hiding it.
func TestReportedAccountDisagreementIsVisible(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(account("qoder", "qd-1", "qoder-1.json"))
	fake.install(t)
	fake.serve(
		jsonRoute("/qoder/status?", `{"provider":"qoder","accounts":4,"account":{"auth_index":"qd-1","name":"qoder-1.json"}}`),
		jsonRoute("/qoder/checkin?", `{"status":"claimed","message":"领取成功"}`),
	)
	withSettings(t, testConfig("qoder"))
	result := newRunner(testHost(), settings()).run()
	if len(result.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(result.Providers))
	}
	if result.Providers[0].Reported != 4 || result.Providers[0].Accounts != 1 {
		t.Fatalf("provider section = %+v, want reported 4 / enumerated 1", result.Providers[0])
	}
	if !strings.Contains(result.Providers[0].Error, "4") {
		t.Fatalf("disagreement was not surfaced: %+v", result.Providers[0])
	}
}

// TestStatusPageAfterRunShowsLastKnownCounts: the cached snapshot is what the
// menu page reports, and re-rendering it must not claim anything again — the
// overview probes before it renders, so the assertion is about CLAIM requests.
func TestStatusPageAfterRunShowsLastKnownCounts(t *testing.T) {
	resetLastRun(t)
	fake := newFakeHost(account("qoder", "qd-1", "qoder-1.json"))
	fake.install(t)
	fake.serve(
		jsonRoute("/qoder/status?", `{"provider":"qoder","accounts":1,"account":{"auth_index":"qd-1","name":"qoder-1.json"}}`),
		jsonRoute("/qoder/checkin?", `{"status":"claimed","message":"领取成功"}`),
	)
	withSettings(t, testConfig("qoder"))

	dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status",
		url.Values{"action": {"checkin"}, "format": {"json"}}, "application/json"))
	before := countClaimRequests(fake)

	response := dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status",
		url.Values{"format": {"json"}}, "application/json"))
	if after := countClaimRequests(fake); after != before {
		t.Fatalf("re-rendering the overview issued %d claim requests, want 0", after-before)
	}
	document := map[string]any{}
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("document is not JSON: %v", errUnmarshal)
	}
	if document["claiming"] != false {
		t.Fatalf("claiming = %v, want false", document["claiming"])
	}
	if document["last_run"] == nil {
		t.Fatal("last_run is null after a completed run")
	}
	// `channels` is the overview list; `targets` is the same list under the name
	// the first release used, so both must carry the last run's numbers.
	for _, key := range []string{"channels", "targets"} {
		list, okList := document[key].([]any)
		if !okList || len(list) != 1 {
			t.Fatalf("%s = %#v, want one row", key, document[key])
		}
		row, _ := list[0].(map[string]any)
		if row["last_account_count"] != float64(1) {
			t.Fatalf("%s[0].last_account_count = %v, want 1", key, row["last_account_count"])
		}
		if row["state"] != string(channelReady) {
			t.Fatalf("%s[0].state = %v, want %s", key, row["state"], channelReady)
		}
	}

	page := dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status", nil, "text/html"))
	if !strings.Contains(string(page.Body), "上次运行结果") {
		t.Fatal("the menu page does not show the previous run")
	}
}

// TestInterpretCheckinJSON covers the verdict vocabulary, including the shapes
// where `status` is an object (codebuddy) or a nested claim block (trae).
func TestInterpretCheckinJSON(t *testing.T) {
	cases := []struct {
		body       string
		wantKind   rowKind
		wantStatus string
	}{
		{`{"status":"claimed","message":"领取成功","amount":5}`, kindClaimed, "claimed"},
		{`{"status":"already-claimed","message":"今天已领取"}`, kindAlreadyClaimed, "already-claimed"},
		{`{"status":"inactive","message":"不适用"}`, kindInactive, "inactive"},
		{`{"status":"failed","message":"boom"}`, kindFailed, "failed"},
		{`{"supported":false,"product":"workbuddy","message":"该产品没有每日签到接口"}`, kindUnsupported, ""},
		{`{"supported":true,"outcome":"claimed","status":{"checked_in":true},"message":"签到成功"}`, kindClaimed, "claimed"},
		{`{"claim":{"status":"claimed","message":"签到成功","credit":10}}`, kindClaimed, "claimed"},
		{`{"checked_in":true}`, kindUnknown, ""},
	}
	for _, item := range cases {
		document, errParse := parseJSONObject([]byte(item.body))
		if errParse != nil {
			t.Fatalf("parse %s: %v", item.body, errParse)
		}
		verdict := interpretCheckinJSON(document)
		if verdict.Kind != item.wantKind {
			t.Fatalf("%s: kind = %s, want %s", item.body, verdict.Kind, item.wantKind)
		}
		if verdict.ProviderStatus != item.wantStatus {
			t.Fatalf("%s: raw status = %q, want %q", item.body, verdict.ProviderStatus, item.wantStatus)
		}
	}
}

// TestNoticeFromHTML covers the fallback evidence used for codearts.
func TestNoticeFromHTML(t *testing.T) {
	tone, text := noticeFromHTML(`<section class="card"><div class="notice success">签到成功，获得 5.00 额度</div></section>`)
	if tone != "success" || text != "签到成功，获得 5.00 额度" {
		t.Fatalf("tone=%q text=%q", tone, text)
	}
	tone, text = noticeFromHTML(`<div class="notice">今日已签到</div>`)
	if tone != "" || text != "今日已签到" {
		t.Fatalf("plain notice: tone=%q text=%q", tone, text)
	}
	if tone, text = noticeFromHTML("<html></html>"); tone != "" || text != "" {
		t.Fatalf("missing notice should be empty, got %q %q", tone, text)
	}
}

// jsonResponse builds a canned JSON host response.
func jsonResponse(body string) *pluginapi.HTTPResponse {
	return &pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(body),
	}
}
