package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// managementRequest builds a management call for one route and query.
func managementRequest(path string, query map[string]string, accept string) pluginapi.ManagementRequest {
	values := map[string][]string{}
	for key, value := range query {
		values[key] = []string{value}
	}
	headers := http.Header{}
	if accept != "" {
		headers.Set("Accept", accept)
	}
	return pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    path,
		Headers: headers,
		Query:   values,
	}
}

// managementCall invokes management.handle and returns the response.
func managementCall(t *testing.T, h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	t.Helper()
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("encode management request: %v", errMarshal)
	}
	value, errHandle := handleManagementHandle(h, raw)
	if errHandle != nil {
		t.Fatalf("management.handle: %v", errHandle)
	}
	return decodeResult[pluginapi.ManagementResponse](t, value)
}

// TestManagementRouteUsesTheLastSegment covers both mounts: the resource path
// (`/v0/resource/plugins/qoder/status`) and the management API path
// (`/v0/management/qoder/checkin`) only agree on the final segment.
func TestManagementRouteUsesTheLastSegment(t *testing.T) {
	cases := map[string]string{
		"/status":                           "/status",
		"/status/":                          "/status",
		"/v0/resource/plugins/qoder/status": "/status",
		"/v0/management/qoder/checkin":      "/checkin",
		"/v0/management/qoder/status":       "/status",
		"":                                  "/",
	}
	for input, want := range cases {
		if got := managementRoute(input); got != want {
			t.Errorf("managementRoute(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestWantsJSON covers both triggers a caller may use.
func TestWantsJSON(t *testing.T) {
	if !wantsJSON(managementRequest("/status", map[string]string{"format": "json"}, "")) {
		t.Error("?format=json must select the JSON representation")
	}
	if wantsJSON(managementRequest("/status", nil, "text/html,application/xhtml+xml")) {
		t.Error("an HTML navigation must select the page")
	}
	if !wantsJSON(managementRequest("/status", nil, "application/json")) {
		t.Error("a non-HTML Accept must select the JSON representation")
	}
	if wantsJSON(managementRequest("/status", nil, "")) {
		t.Error("an absent Accept must default to the page")
	}
}

// TestManagementRegistrationFollowsTheMountRules is the regression guard for the
// silent-drop trap: a Menu-less route mounts under `/v0/management/<path>` in a
// GLOBAL namespace and must therefore carry the provider prefix, while the pages
// mount under `/v0/resource/plugins/<id>/...`. No route of this plugin may carry
// a Menu: the hub owns the repository's single sidebar entry.
func TestManagementRegistrationFollowsTheMountRules(t *testing.T) {
	value, errRegister := handleManagementRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("management.register: %v", errRegister)
	}
	registration := decodeResult[pluginapi.ManagementRegistrationResponse](t, value)
	if len(registration.Routes) == 0 {
		t.Fatal("no management routes were registered")
	}
	apiOnly := 0
	for _, route := range registration.Routes {
		if route.Path == "" {
			t.Error("a route with an empty path would collide with everything")
		}
		if route.Menu != "" {
			t.Errorf("route %s carries menu %q: the sidebar belongs to the hub plugin", route.Path, route.Menu)
		}
		apiOnly++
		if !strings.HasPrefix(route.Path, "/"+ProviderKey) {
			t.Errorf("route %s %s lives in the global management namespace without the %q prefix",
				route.Method, route.Path, ProviderKey)
		}
	}
	if apiOnly == 0 {
		t.Fatal("no script-facing route was registered")
	}
	// The embedded pages must stay reachable as GET resources: CPA-Manager-Plus
	// iframes any resource path, menu or not, and the hub's channel overview links
	// straight at /status.
	resourcePaths := map[string]bool{}
	for _, route := range registration.Resources {
		resourcePaths[route.Path] = true
		if strings.TrimSpace(route.Menu) != "" {
			t.Errorf("resource route %s carries a menu and would become another sidebar entry", route.Path)
		}
	}
	for _, wanted := range []string{"/status", "/login", "/checkin"} {
		if !resourcePaths[wanted] {
			t.Errorf("GET %s was not registered as a menu-less resource route", wanted)
		}
	}
}

// TestRegistrationDeclaresTheProviderSurface keeps the ABI registration honest.
func TestRegistrationDeclaresTheProviderSurface(t *testing.T) {
	registration := Plugin().Registration()
	if registration.Metadata.Name != DisplayName || registration.Metadata.Version != Version {
		t.Fatalf("metadata = %#v", registration.Metadata)
	}
	if len(registration.Metadata.ConfigFields) == 0 {
		t.Fatal("the host renders no settings without ConfigFields")
	}
	caps := registration.Capabilities
	if !caps.AuthProvider || !caps.ModelProvider || !caps.ModelRegistrar || !caps.Executor ||
		!caps.ManagementAPI || !caps.QuotaProvider || !caps.RequestTranslator || !caps.ResponseTranslator {
		t.Fatalf("capabilities are incomplete: %#v", caps)
	}
	if len(caps.ExecutorInputFormats) == 0 || len(caps.ExecutorOutputFormats) == 0 {
		t.Fatal("the executor must declare at least one input and output format")
	}
	if registration.SchemaVersion == 0 {
		t.Fatal("SchemaVersion must be stamped by abiboot.NewRegistration")
	}
}

// TestEveryDeclaredMethodIsRouted guards against a capability without a handler.
func TestEveryDeclaredMethodIsRouted(t *testing.T) {
	routes := Plugin().Routes()
	for _, method := range []string{
		"auth.identifier", "auth.parse", "auth.login.start", "auth.login.poll", "auth.refresh",
		"model.register", "model.static", "model.for_auth",
		"executor.identifier", "executor.execute", "executor.execute_stream", "executor.count_tokens",
		"request.translate", "response.translate",
		"management.register", "management.handle",
		"quota.identifier", "quota.describe", "quota.fetch", "quota.reset",
	} {
		if _, ok := routes[method]; !ok {
			t.Errorf("method %s is not routed", method)
		}
	}
}

// statusHost installs a fake host with one account and a healthy usage endpoint.
func statusHost(t *testing.T, accountName string) *fakeHost {
	t.Helper()
	host := newFakeHost()
	host.files = []pluginapi.HostAuthFileEntry{{
		AuthIndex: "idx-1", ID: "idx-1", Name: accountName, Provider: ProviderKey, Status: "active",
	}}
	credential := &Credential{
		AccessToken: "tok", SecurityOAuthToken: "tok", RefreshToken: "ref", MachineID: "machine-1",
		UID: "u-1", Nickname: accountName, Region: string(RegionGlobal),
		ExpireTime: timeNowPlusHour(),
	}
	stored, _ := credential.Encode()
	host.auths["idx-1"] = stored
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(request.URL, UsagePath) {
			return httpResponse(200, usageBody), nil
		}
		if strings.HasSuffix(request.URL, CampaignsPath) {
			return httpResponse(200, campaignsBody), nil
		}
		return httpResponse(404, "{}"), nil
	}
	host.install(t)
	return host
}

// TestStatusPageRendersHTML is the headline management feature: the route serves
// a themed page on a browser navigation.
func TestStatusPageRendersHTML(t *testing.T) {
	statusHost(t, "alice")
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest(
		"/v0/resource/plugins/qoder/status", nil, "text/html,application/xhtml+xml"))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Headers.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
	page := string(response.Body)
	for _, want := range []string{
		"<!DOCTYPE html>",
		"var(--bg-primary",
		"var(--primary-color",
		"推理通道",
		"公开端点",
		"alice",
		"额度",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the status page is missing %q", want)
		}
	}
	// The resource mount is GET-only, so a page must not contain a form.
	if strings.Contains(strings.ToLower(page), "<form") {
		t.Fatal("the page contains a form; the resource mount is dispatched as GET only")
	}
	// Every action names the account it acts on, so the links carry a selector;
	// the two routes must still be reachable from the page.
	if !strings.Contains(page, `href="login`) {
		t.Error("the status page has no link to the login page")
	}
	if !strings.Contains(page, `href="checkin`) {
		t.Error("the status page has no link to the check-in page")
	}
}

// TestStatusJSONIsMachineReadable keeps the same handler usable from scripts.
func TestStatusJSONIsMachineReadable(t *testing.T) {
	statusHost(t, "alice")
	cfg := DefaultConfig()
	withSettings(t, cfg)

	response := managementCall(t, testHost(), managementRequest("/status", map[string]string{"format": "json"}, ""))
	if got := response.Headers.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode status JSON: %v", errUnmarshal)
	}
	if payload["provider"] != ProviderKey {
		t.Errorf("provider = %v, want %q", payload["provider"], ProviderKey)
	}
	if payload["infer_path"] != string(pathPublic) {
		t.Errorf("infer_path = %v, want %q without a wasm_path", payload["infer_path"], pathPublic)
	}
	if payload["wasm_configured"] != false {
		t.Errorf("wasm_configured = %v, want false", payload["wasm_configured"])
	}
	if payload["model_count"] != float64(len(qoderModelCatalog)) {
		t.Errorf("model_count = %v, want %d", payload["model_count"], len(qoderModelCatalog))
	}
	credits, okCredits := payload["credits"].(map[string]any)
	if !okCredits {
		t.Fatalf("credits = %v, want the parsed balance", payload["credits"])
	}
	if credits["total"] != float64(100) {
		t.Errorf("credits.total = %v, want 100", credits["total"])
	}
	account, okAccount := payload["account"].(map[string]any)
	if !okAccount {
		t.Fatalf("account = %v", payload["account"])
	}
	if account["has_uid"] != true || account["refreshable"] != true {
		t.Errorf("account flags = %v, want uid and refreshable true", account)
	}
}

// TestStatusPageWithNoAccountsInvitesALogin keeps the first-run experience usable.
func TestStatusPageWithNoAccountsInvitesALogin(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest("/status", nil, "text/html"))
	page := string(response.Body)
	if !strings.Contains(page, "尚未添加账号") || !strings.Contains(page, `href="login"`) {
		t.Fatalf("the empty state does not point at the login page:\n%s", page)
	}
}

// TestStatusPageEscapesHostileValues is the injection guard for data the plugin
// only obtains at runtime.
func TestStatusPageEscapesHostileValues(t *testing.T) {
	statusHost(t, `<script>alert(1)</script>`)
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest("/status", nil, "text/html"))
	if strings.Contains(string(response.Body), "<script>alert(1)</script>") {
		t.Fatal("an account name was rendered as live markup")
	}
}

// TestStatusPageOffersAddAccount pins the affordance: a SECOND Qoder account must
// be reachable from the status page itself instead of only through the manager's
// OAuth page. The link stays a GET navigation into this plugin's own login route
// — 新建账号 adds a link, not a route.
func TestStatusPageOffersAddAccount(t *testing.T) {
	statusHost(t, "alice")
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest("/status", nil, "text/html"))
	page := string(response.Body)
	if !strings.Contains(page, "新建账号") {
		t.Fatalf("the status page offers no way to add a second account:\n%s", page)
	}
	if !strings.Contains(page, `href="login?add=1"`) {
		t.Fatalf("新建账号 must be a GET link into the login route:\n%s", page)
	}
	if strings.Contains(strings.ToLower(page), "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestLoginPageExplainsAddingAnAccount pins the one-line caveat. 新建账号 and
// 重新登录 open the SAME device flow on purpose (the credential file name comes
// from the account's own identity), so the page has to say which one this is.
func TestLoginPageExplainsAddingAnAccount(t *testing.T) {
	newFakeHost().install(t)
	withSettings(t, DefaultConfig())

	plain := string(managementCall(t, testHost(), managementRequest("/login", nil, "text/html")).Body)
	if strings.Contains(plain, "新增账号") {
		t.Fatalf("the ordinary login page must not claim to add an account:\n%s", plain)
	}
	adding := string(managementCall(t, testHost(),
		managementRequest("/login", map[string]string{"add": "1"}, "text/html")).Body)
	if !strings.Contains(adding, "已有账号的凭据不受影响") {
		t.Fatalf("the add-account login page must state that the existing account survives:\n%s", adding)
	}
	if strings.Contains(strings.ToLower(adding), "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestSecondAccountGetsItsOwnCredentialFile is the guarantee 新建账号 relies on:
// the auth file name is `<region>-<identity>.json`, so a second account in the
// same region writes a NEW file and leaves the first credential alone. The host
// saves by exactly this name (it feeds AuthData.FileName), and no code path in
// this plugin deletes a credential.
func TestSecondAccountGetsItsOwnCredentialFile(t *testing.T) {
	first := &Credential{AccessToken: "tok-a", SecurityOAuthToken: "tok-a", UID: "u-1", Nickname: "alice", Region: string(RegionGlobal)}
	second := &Credential{AccessToken: "tok-b", SecurityOAuthToken: "tok-b", UID: "u-2", Nickname: "bob", Region: string(RegionGlobal)}
	firstName, secondName := defaultAuthFileName(first), defaultAuthFileName(second)
	if firstName == secondName {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", firstName)
	}
	if !strings.HasSuffix(firstName, ".json") || !strings.HasPrefix(firstName, string(RegionGlobal)+"-") {
		t.Fatalf("auth file name = %q, want <region>-<identity>.json", firstName)
	}
}

// TestLoginPageTwoStepFlow walks the login page: the first load offers a button,
// `?action=start` shows the device-code URL, and `?action=poll` completes and
// SAVES the credential (the host does not save what a page-driven poll returns).
func TestLoginPageTwoStepFlow(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"token":"tok","refresh_token":"ref","user_id":"u-9","user_name":"bob"}`), nil
	}
	host.files = []pluginapi.HostAuthFileEntry{}
	host.install(t)
	withSettings(t, DefaultConfig())

	intro := managementCall(t, testHost(), managementRequest("/login", nil, "text/html"))
	if !strings.Contains(string(intro.Body), "action=start") {
		t.Fatalf("the login page offers no start action:\n%s", intro.Body)
	}
	if strings.Contains(strings.ToLower(string(intro.Body)), "<form") {
		t.Fatal("the login page contains a form; the resource mount is GET only")
	}

	started := managementCall(t, testHost(), managementRequest("/login", map[string]string{"action": "start"}, "text/html"))
	page := string(started.Body)
	if !strings.Contains(page, DeviceSelectPath) {
		t.Fatalf("the start page does not show the authorization URL:\n%s", page)
	}
	if !strings.Contains(page, "action=poll&amp;state=") {
		t.Fatalf("the start page has no poll link:\n%s", page)
	}

	state := lastLoginState()
	if state == "" {
		t.Fatal("no login session was registered by the start action")
	}
	polled := managementCall(t, testHost(), managementRequest("/login", map[string]string{"action": "poll", "state": state}, "text/html"))
	if !strings.Contains(string(polled.Body), "登录成功") {
		t.Fatalf("the poll page does not report success:\n%s", polled.Body)
	}
	if len(host.saved) != 1 {
		t.Fatalf("saved credentials = %d, want the page-driven poll to persist one", len(host.saved))
	}
	for name, storage := range host.saved {
		if !strings.HasPrefix(name, "qoder-") || !strings.HasSuffix(name, ".json") {
			t.Errorf("saved auth name = %q, want a qoder-*.json name", name)
		}
		if _, errParse := ParseCredential(storage); errParse != nil {
			t.Errorf("saved credential is not parseable: %v", errParse)
		}
	}
}

// TestLoginPagePollWithoutStateIsAnError keeps a stale bookmark from silently
// starting a new flow.
func TestLoginPagePollWithoutStateIsAnError(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	withSettings(t, DefaultConfig())
	response := managementCall(t, testHost(), managementRequest("/login", map[string]string{"action": "poll"}, "text/html"))
	if !strings.Contains(string(response.Body), "缺少 state") {
		t.Fatalf("the poll page did not report the missing state:\n%s", response.Body)
	}
}

// TestCheckinPageReportsTheBodyOutcome is the idempotency rule on the page: a
// repeated claim answers 200, so the page must read `replayed`.
func TestCheckinPageReportsTheBodyOutcome(t *testing.T) {
	host := statusHost(t, "alice")
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(request.URL, UsagePath) {
			return httpResponse(200, usageBody), nil
		}
		if strings.HasSuffix(request.URL, CampaignsPath) {
			return httpResponse(200, campaignsBody), nil
		}
		if strings.HasSuffix(request.URL, "/claim") {
			return httpResponse(200, `{"status":"CLAIMED","replayed":true}`), nil
		}
		return httpResponse(404, "{}"), nil
	}
	host.install(t)
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest("/checkin", nil, "text/html"))
	if !strings.Contains(string(response.Body), "今天已领取") {
		t.Fatalf("the check-in page did not report the replay:\n%s", response.Body)
	}

	jsonResponse := managementCall(t, testHost(), managementRequest(
		"/v0/management/qoder/checkin", map[string]string{"format": "json"}, ""))
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(jsonResponse.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode check-in JSON: %v", errUnmarshal)
	}
	if payload["status"] != "already-claimed" {
		t.Fatalf("status = %v, want already-claimed", payload["status"])
	}
}

// TestCheckinPageWithNoAccount explains the failure instead of rendering an empty
// page.
func TestCheckinPageWithNoAccount(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	withSettings(t, DefaultConfig())
	response := managementCall(t, testHost(), managementRequest("/checkin", nil, "text/html"))
	if !strings.Contains(string(response.Body), "指定的账号不存在") {
		t.Fatalf("unexpected page:\n%s", response.Body)
	}
	jsonResponse := managementCall(t, testHost(), managementRequest("/checkin", map[string]string{"format": "json"}, ""))
	if jsonResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", jsonResponse.StatusCode)
	}
}

// TestUnknownManagementRouteIsNotFound keeps a typo visible.
func TestUnknownManagementRouteIsNotFound(t *testing.T) {
	response := managementCall(t, testHost(), managementRequest("/nope", map[string]string{"format": "json"}, ""))
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

// TestStatusPageReportsASignerFailure keeps a misconfigured wasm_path visible
// instead of silently falling back.
func TestStatusPageReportsASignerFailure(t *testing.T) {
	statusHost(t, "alice")
	cfg := DefaultConfig()
	cfg.WASMPath = "/nonexistent/qoder.wasm"
	withSettings(t, cfg)

	response := managementCall(t, testHost(), managementRequest("/status", map[string]string{"format": "json"}, ""))
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode status JSON: %v", errUnmarshal)
	}
	if payload["infer_path"] != string(pathEncrypted) {
		t.Fatalf("infer_path = %v, want %q once wasm_path is set", payload["infer_path"], pathEncrypted)
	}
	if _, ok := payload["wasm_error"]; !ok {
		t.Fatalf("status JSON = %v, want the signer load error reported", payload)
	}

	page := managementCall(t, testHost(), managementRequest("/status", nil, "text/html"))
	if !strings.Contains(string(page.Body), "加载失败") {
		t.Fatalf("the page does not report the signer failure:\n%s", page.Body)
	}
}

// TestExecutorUsesThePublicPathWithoutASigner is the behavioural half of the
// wasm_path switch: with no signer configured the request goes to the public
// OpenAI-compatible endpoint and the catalog key is forwarded verbatim.
func TestExecutorUsesThePublicPathWithoutASigner(t *testing.T) {
	host := newFakeHost()
	var seen abiboot.HTTPDoRequest
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		seen = request
		return httpResponse(200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"), nil
	}
	host.install(t)
	withSettings(t, DefaultConfig())

	credential := &Credential{AccessToken: "tok", MachineID: "m", Region: string(RegionGlobal)}
	storage, _ := credential.Encode()
	raw, errMarshal := json.Marshal(pluginapi.ExecutorRequest{
		Model:       "qfmodel",
		Payload:     []byte(`{"model":"qfmodel","messages":[{"role":"user","content":"hi"}]}`),
		StorageJSON: storage,
	})
	if errMarshal != nil {
		t.Fatalf("encode executor request: %v", errMarshal)
	}
	value, errExecute := handleExecutorExecute(testHost(), raw)
	if errExecute != nil {
		t.Fatalf("executor.execute: %v", errExecute)
	}
	response := decodeResult[pluginapi.ExecutorResponse](t, value)
	if !strings.Contains(string(response.Payload), `"ok"`) {
		t.Fatalf("payload = %s", response.Payload)
	}
	if !strings.HasPrefix(seen.URL, "https://api2-v2.qoder.sh"+PublicChatPath) {
		t.Fatalf("upstream URL = %q, want the public endpoint (no wasm_path configured)", seen.URL)
	}
	if got := seen.Headers.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q, want the bearer token", got)
	}
	if response.Metadata["infer_path"] != string(pathPublic) {
		t.Fatalf("metadata infer_path = %v, want %q", response.Metadata["infer_path"], pathPublic)
	}
}

// timeNowPlusHour returns a millisecond timestamp one hour ahead.
func timeNowPlusHour() int64 { return nowMillis() + 3_600_000 }
