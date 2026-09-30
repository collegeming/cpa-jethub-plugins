package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Every provider in this repository must expose the same three ways into its
// login flow, and a missing one is invisible until a user cannot add a second
// account or replace a dead credential. These tests assert them for AtomCode the
// way the sibling plugins do.

// browseHost installs a host with one account and working CodingPlan endpoints.
func browseHost(t *testing.T) *fakeHost {
	t.Helper()
	host := newFakeHost()
	credential := sampleCredential(7 * 24 * 3600)
	storage := mustJSON(t, credential)
	host.auths["idx-1"] = storage
	host.files = []pluginapi.HostAuthFileEntry{
		{ID: "atomcode-qq_23240873.json", AuthIndex: "idx-1", Name: "atomcode-qq_23240873.json",
			Provider: ProviderKey, Type: ProviderKey, Label: "黎明文铮"},
	}
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, codingPlanStatusPath):
			return httpResponse(200, `{"codingplan_free":{"plan_name":"CodingPlan Lite-体验版","plan_type":"Lite-beta",`+
				`"status":1,"claimed_at":"2026-10-01","expires_at":"2026-10-08","remaining_days":7},`+
				`"rate_limit_windows":[{"rule_index":0,"show_enable":1,"window_hours":5,"call_limit":200,"calls_used":3}]}`), nil
		case strings.Contains(request.URL, codingPlanModelsPath):
			return httpResponse(200, `[{"display_model_name":"glm5.3-flash","context_window":512000,`+
				`"supports_vision":true,"plan_available":true,"reasoning_effort_levels":["low","high"]}]`), nil
		case strings.Contains(request.URL, codingPlanUsagePath):
			return httpResponse(200, `{"days":60,"start_date":"2026-08-03","end_date":"2026-10-01",`+
				`"models":["glm5.3-flash"],"rows":[],"model_counts":{"glm5.3-flash":3},"model_tokens":{"glm5.3-flash":120},`+
				`"total_counts":3,"total_tokens":120}`), nil
		}
		return httpResponse(404, `{"message":"unexpected"}`), nil
	}
	host.install(t)
	return host
}

// bodyOf renders a management response body as text.
func bodyOf(response pluginapi.ManagementResponse) string { return string(response.Body) }

// TestStatusPageOffersEveryLoginEntry asserts the three affordances the panel
// contract requires: add an account, re-login, and reach the login page at all.
func TestStatusPageOffersEveryLoginEntry(t *testing.T) {
	browseHost(t)
	page := bodyOf(renderStatusPage(testHost(), pluginapi.ManagementRequest{
		Path:    "/v0/resource/plugins/atomcode/status",
		Query:   url.Values{},
		Headers: map[string][]string{"Accept": {"text/html"}},
	}))
	text := string(page)

	for _, want := range []string{plugui.AddAccountQuery, "新建账号", "重新登录", "领取 / 用量", "全部账号"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status page is missing %q\n%s", want, text)
		}
	}
	// 新建账号 must target the login page with the add intent, not the plain
	// login page: the query is what tells the user their existing accounts are
	// untouched.
	if !strings.Contains(text, "login?"+plugui.AddAccountQuery) {
		t.Fatalf("the 新建账号 action does not carry %q", plugui.AddAccountQuery)
	}
	// Re-login must name the account, so it replaces the right credential file
	// rather than creating another one. BOTH the per-account row in 全部账号 and
	// the 当前账号 card offer it, so the count — not mere presence — is what
	// proves neither was dropped.
	if got := strings.Count(text, "login?auth_index=idx-1"); got < 2 {
		t.Fatalf("re-login links carrying the auth_index = %d, want one in the account list and one on the current-account card", got)
	}
}

// TestStatusPageWithoutAccountsOffersLogin covers the empty state, which is the
// first thing a new user sees.
func TestStatusPageWithoutAccountsOffersLogin(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatal("the empty state must not call upstream")
		return nil, nil
	}
	host.install(t)

	text := bodyOf(renderStatusPage(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/atomcode/status", Query: url.Values{},
	}))
	if !strings.Contains(text, "去登录") {
		t.Fatalf("the empty status page offers no way to log in:\n%s", text)
	}
}

// TestLoginPageHonoursTheAddAccountIntent covers the notice that explains the
// difference between adding an account and replacing one.
func TestLoginPageHonoursTheAddAccountIntent(t *testing.T) {
	browseHost(t)
	text := bodyOf(renderLoginPage(testHost(), pluginapi.ManagementRequest{
		Path:  "/v0/resource/plugins/atomcode/login",
		Query: url.Values{"add": {"1"}},
	}))
	if !strings.Contains(text, plugui.AddAccountNotice) {
		t.Fatalf("the add-account notice is missing:\n%s", text)
	}
	if !strings.Contains(text, "开始登录") {
		t.Fatalf("the login page offers no start action:\n%s", text)
	}

	plain := bodyOf(renderLoginPage(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/atomcode/login", Query: url.Values{},
	}))
	if strings.Contains(plain, plugui.AddAccountNotice) {
		t.Fatal("the add-account notice appeared without the add intent")
	}
}

// TestLoginStartPageShowsTheBrokerURL covers the two-step flow the broker
// imposes: the plugin hands out a URL and the browser gesture happens outside it.
func TestLoginStartPageShowsTheBrokerURL(t *testing.T) {
	host := browseHost(t)
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, brokerLoginPath) {
			return httpResponse(200, `{"login_url":"https://acs.atomgit.com/s/39585c7f","state":"atomcode_1_abc"}`), nil
		}
		t.Fatalf("unexpected URL %s", request.URL)
		return nil, nil
	}

	text := bodyOf(renderLoginPage(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/atomcode/login", Query: url.Values{"action": {"start"}},
	}))
	if !strings.Contains(text, "https://acs.atomgit.com/s/39585c7f") {
		t.Fatalf("the authorisation URL is missing:\n%s", text)
	}
	if !strings.Contains(text, "action=poll") {
		t.Fatalf("the check-result action is missing:\n%s", text)
	}
	// The broker's URL must never be presented as the official client's: that
	// identity is what arms the gateway's signature gate.
	if strings.Contains(text, OfficialUserAgent) {
		t.Fatal("the page published the official client identity")
	}
}

// TestCheckinPageClaimsAndReports covers the claim action and the usage section.
func TestCheckinPageClaimsAndReports(t *testing.T) {
	host := browseHost(t)
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, codingPlanClaimPath):
			return httpResponse(200, `{"success":true,"duplicate":false,"message":"领取成功","plan_name":"CodingPlan Lite-体验版"}`), nil
		case strings.Contains(request.URL, codingPlanStatusPath):
			return httpResponse(200, `{"codingplan_free":{"plan_name":"CodingPlan Lite-体验版","expires_at":"2026-10-08","remaining_days":7}}`), nil
		case strings.Contains(request.URL, codingPlanUsagePath):
			return httpResponse(200, `{"models":["glm5.3-flash"],"rows":[],"model_counts":{"glm5.3-flash":3},`+
				`"model_tokens":{"glm5.3-flash":120},"total_counts":3,"total_tokens":120,"start_date":"2026-08-03","end_date":"2026-10-01"}`), nil
		}
		return httpResponse(404, `{"message":"unexpected"}`), nil
	}

	text := bodyOf(renderCheckinPage(testHost(), pluginapi.ManagementRequest{
		Path:  "/v0/resource/plugins/atomcode/checkin",
		Query: url.Values{"action": {"claim"}, "auth_index": {"idx-1"}},
	}))
	// The cascade stops at the first granted tier, so only Max appears in the
	// trail. The full three-tier walk is covered by TestClaimCascadeWalksDownOnRefusal.
	for _, want := range []string{"领取成功", "Max", "生效中"} {
		if !strings.Contains(text, want) {
			t.Fatalf("claim result is missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "3 次调用") {
		t.Fatalf("usage totals are missing:\n%s", text)
	}
}

// TestModelCardDeclaresThinkingLevels covers the selector: without a
// ThinkingSupport block the client offers no level choice at all.
func TestModelCardDeclaresThinkingLevels(t *testing.T) {
	browseHost(t)
	text := bodyOf(renderStatusPage(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/atomcode/status", Query: url.Values{},
	}))
	if !strings.Contains(text, "思考级别 low/high") {
		t.Fatalf("the model card does not publish the server's effort levels:\n%s", text)
	}
	if !strings.Contains(text, "512000 ctx") {
		t.Fatalf("the model card does not publish the context window:\n%s", text)
	}
}
