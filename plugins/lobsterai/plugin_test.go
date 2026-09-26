package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegistration(t *testing.T) {
	registration := Plugin().Registration()
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
	}
	metadata := registration.Metadata
	if metadata.Name != DisplayName || metadata.Version != Version || metadata.Author != Author {
		t.Fatalf("metadata = %+v", metadata)
	}
	if metadata.GitHubRepository == "" || metadata.Logo == "" {
		t.Fatalf("metadata is missing repository/logo: %+v", metadata)
	}
	if len(metadata.ConfigFields) == 0 {
		t.Fatal("config_fields must be non-empty for the management UI")
	}

	capabilities := registration.Capabilities
	if !capabilities.ModelRegistrar || !capabilities.ModelProvider || !capabilities.AuthProvider || !capabilities.Executor {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if !capabilities.ManagementAPI || !capabilities.QuotaProvider {
		t.Fatalf("management/quota capabilities missing: %+v", capabilities)
	}
	// LobsterAI credentials are OAuth-style, never static keys.
	if capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("ExecutorModelScope = %q", capabilities.ExecutorModelScope)
	}
	if len(capabilities.ExecutorInputFormats) != 1 || capabilities.ExecutorInputFormats[0] != "chat-completions" {
		t.Fatalf("ExecutorInputFormats = %v", capabilities.ExecutorInputFormats)
	}
	if len(capabilities.ExecutorOutputFormats) != 1 || capabilities.ExecutorOutputFormats[0] != "chat-completions" {
		t.Fatalf("ExecutorOutputFormats = %v", capabilities.ExecutorOutputFormats)
	}
	if capabilities.ThinkingApplier {
		t.Fatal("this adapter does not require a thinking applier (it maps reasoning_effort itself)")
	}
}

func TestRoutesTableIsComplete(t *testing.T) {
	routes := Plugin().Routes()
	required := []string{
		pluginabi.MethodAuthIdentifier,
		pluginabi.MethodAuthParse,
		pluginabi.MethodAuthLoginStart,
		pluginabi.MethodAuthLoginPoll,
		pluginabi.MethodAuthRefresh,
		pluginabi.MethodModelRegister,
		pluginabi.MethodModelStatic,
		pluginabi.MethodModelForAuth,
		pluginabi.MethodExecutorIdentifier,
		pluginabi.MethodExecutorExecute,
		pluginabi.MethodExecutorExecuteStream,
		pluginabi.MethodExecutorCountTokens,
		pluginabi.MethodRequestTranslate,
		pluginabi.MethodResponseTranslate,
		pluginabi.MethodQuotaIdentifier,
		pluginabi.MethodQuotaDescribe,
		pluginabi.MethodQuotaFetch,
		pluginabi.MethodQuotaReset,
		pluginabi.MethodManagementRegister,
		pluginabi.MethodManagementHandle,
	}
	for _, method := range required {
		if _, present := routes[method]; !present {
			t.Fatalf("route %s is not registered", method)
		}
	}
	if len(routes) != len(required) {
		t.Fatalf("routes table has %d entries, want %d", len(routes), len(required))
	}
}

func TestConfigureAppliesSettings(t *testing.T) {
	t.Cleanup(func() { setSettings(DefaultConfig()) })
	plugin := Plugin().(*plugin)
	// The host hands config_yaml over as raw YAML bytes after base64 decoding.
	if errConfigure := plugin.Configure([]byte("discover_models: false\ndaily_checkin: false\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	cfg := settings()
	if cfg.DiscoverModels || cfg.DailyCheckin {
		t.Fatalf("settings were not applied: %+v", cfg)
	}
	// A flow-style document, which is what the host sends back for an inline
	// config, must work too.
	if errConfigure := plugin.Configure([]byte("{discover_models: true, max_tokens: 512}")); errConfigure != nil {
		t.Fatalf("Configure(flow): %v", errConfigure)
	}
	if cfg := settings(); !cfg.DiscoverModels || cfg.DefaultMaxTokens != 512 {
		t.Fatalf("flow-style settings were not applied: %+v", cfg)
	}
}

func TestManagementRouteHelper(t *testing.T) {
	tests := map[string]string{
		"/v0/resource/plugins/lobsterai/status":  "/status",
		"/v0/resource/plugins/lobsterai/checkin": "/checkin",
		"/v0/management/lobsterai/checkin":       "/checkin",
		"/v0/management/lobsterai/login/start":   "/start",
		"/v0/management/lobsterai/login/poll":    "/poll",
		"/v0/resource/plugins/lobsterai/status/": "/status",
		"/status":                                "/status",
		"":                                       "/",
	}
	for path, want := range tests {
		if got := managementRoute(path); got != want {
			t.Fatalf("managementRoute(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestManagementRegisterDeclaresBothMounts(t *testing.T) {
	value, errRegister := handleManagementRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("handleManagementRegister: %v", errRegister)
	}
	response := value.(pluginapi.ManagementRegistrationResponse)

	managementPaths := map[string]string{}
	for _, route := range response.Routes {
		if strings.TrimSpace(route.Menu) != "" {
			t.Errorf("management route %s carries menu %q; the sidebar belongs to the hub plugin", route.Path, route.Menu)
		}
		// Every route here lands in the global management namespace, so it must
		// carry the provider prefix or it can be silently skipped on collision.
		if !strings.HasPrefix(route.Path, "/"+ProviderKey+"/") {
			t.Fatalf("management route %q is not namespaced by the provider key", route.Path)
		}
		managementPaths[route.Path] = route.Method
	}

	// NO sidebar entry: CPAMP renders one nav item per menu route and does not
	// group them by plugin, and the repository spends its single item on the hub.
	// The status page is reached from the hub's channel overview, login and
	// check-in from the status page and the manager's own OAuth page.
	if len(managementPaths) == 0 {
		t.Fatal("the script routes must still be registered on the management mount")
	}

	// The browser-reachable pages that must NOT add a sidebar entry.
	resources := map[string]bool{}
	for _, route := range response.Resources {
		if strings.TrimSpace(route.Menu) != "" {
			t.Fatalf("resource route %s carries a menu, which would add a nav item: %+v", route.Path, route)
		}
		resources[route.Path] = true
	}
	for _, path := range []string{"/status", "/login", "/checkin"} {
		if !resources[path] {
			t.Fatalf("resource route %s is missing: %v", path, resources)
		}
	}
	for path, method := range map[string]string{
		"/lobsterai/checkin":     http.MethodPost,
		"/lobsterai/login/start": http.MethodPost,
		"/lobsterai/login/poll":  http.MethodPost,
	} {
		if managementPaths[path] != method {
			t.Fatalf("management route %s = %q, want %q", path, managementPaths[path], method)
		}
	}
}

func TestWantsJSON(t *testing.T) {
	html := rawManagementRequest(http.MethodGet, "/v0/resource/plugins/lobsterai/status", nil, "text/html,application/xhtml+xml")
	if wantsJSON(html) {
		t.Fatal("a browser navigation must render HTML")
	}
	api := rawManagementRequest(http.MethodGet, "/v0/management/lobsterai/status", nil, "application/json")
	if !wantsJSON(api) {
		t.Fatal("an application/json caller must get JSON")
	}
	empty := rawManagementRequest(http.MethodGet, "/v0/management/lobsterai/status", nil, "")
	if wantsJSON(empty) {
		t.Fatal("an Accept-less GET is treated as a page navigation")
	}
	// curl sends */*, which accepts HTML: the resource page must render, which
	// is what the host install test inspects.
	wildcard := rawManagementRequest(http.MethodGet, "/v0/resource/plugins/lobsterai/status", nil, "*/*")
	if wantsJSON(wildcard) {
		t.Fatal("*/* accepts HTML and must render the page")
	}
	forcedHTML := rawManagementRequest(http.MethodGet, "/v0/management/lobsterai/status", url.Values{"format": {"html"}}, "application/json")
	if wantsJSON(forcedHTML) {
		t.Fatal("?format=html must force the page")
	}
	post := rawManagementRequest(http.MethodPost, "/v0/management/lobsterai/checkin", nil, "text/html")
	if !wantsJSON(post) {
		t.Fatal("a POST is an API call and must return JSON")
	}
	forced := rawManagementRequest(http.MethodGet, "/v0/resource/plugins/lobsterai/status", url.Values{"format": {"json"}}, "text/html")
	if !wantsJSON(forced) {
		t.Fatal("?format=json must force JSON")
	}
}

// statusFixture is a fake host with one LobsterAI account plus working credit
// and slot endpoints.
func statusFixture(t *testing.T) (*fakeHost, *abiboot.Host, pluginapi.HostAuthFileEntry) {
	t.Helper()
	credential := &Credential{
		AccessToken: "token", RefreshToken: "refresh", UID: "uid-1", Nickname: "nick",
		UUID: "uuid-1", FirstKeyfrom: "111", LatestKeyfrom: "222",
		ExpiresAt: itoa64(time.Now().Add(time.Hour).UnixMilli()),
	}
	entry := pluginapi.HostAuthFileEntry{
		AuthIndex: "idx-1", Name: "lobsterai-uid-1.json", Provider: ProviderKey,
		Type: ProviderKey, Label: "nick", Status: "ready",
	}
	fake := newFakeHost().
		withFiles(entry).
		withAuthJSON("idx-1", string(mustJSON(t, credential))).
		on(httpRoute{Method: http.MethodGet, Match: ProfileSummaryPath, Body: `{"code":0,"data":{"totalCreditsRemaining":42.5,"creditItems":[{"type":"activity","creditsRemaining":42.5}]}}`}).
		on(httpRoute{Method: http.MethodGet, Match: ActivitySlotPath, Body: `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"daily-1","configRevision":3}}}`}).
		on(httpRoute{Method: http.MethodGet, Match: "api-overmind", Body: `{"data":{"value":{"version":"2026.9.4"}},"code":0}`})
	host := installFakeHost(t, fake)
	return fake, host, entry
}

func TestHandleManagementStatusPage(t *testing.T) {
	_, host, _ := statusFixture(t)
	request := managementRequest(http.MethodGet, "/v0/resource/plugins/lobsterai/status", nil, nil)
	value, errHandle := handleManagementHandle(host, mustJSON(t, request))
	if errHandle != nil {
		t.Fatalf("handleManagementHandle: %v", errHandle)
	}
	response := value.(pluginapi.ManagementResponse)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Headers.Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Fatalf("Content-Type = %q, want HTML", got)
	}
	body := string(response.Body)
	for _, want := range []string{
		"<!DOCTYPE html>", "LobsterAI", "lobsterai-uid-1.json", "可自动续期", "42.5",
		"账号", "模型与远端参数", "签到",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("status page does not contain %q:\n%s", want, firstLines(body, 40))
		}
	}
	// Actions must be GET links carrying a query string, not POST forms. The
	// per-account actions name their account, so the check-in link is
	// `checkin?auth_index=…`.
	if strings.Contains(body, "<form") {
		t.Fatalf("the page must not rely on forms; the host dispatches resource routes as GET only")
	}
	if !strings.Contains(body, `href="checkin?`) && !strings.Contains(body, `href="?`) {
		t.Fatalf("no action links found in:\n%s", firstLines(body, 40))
	}
	if strings.Contains(body, `method="post"`) {
		t.Fatal("no POST form may be rendered on a resource page")
	}
}

func TestHandleManagementStatusJSON(t *testing.T) {
	_, host, _ := statusFixture(t)
	request := jsonManagementRequest(http.MethodGet, "/v0/management/lobsterai/status", url.Values{"format": {"json"}}, nil)
	value, errHandle := handleManagementHandle(host, mustJSON(t, request))
	if errHandle != nil {
		t.Fatalf("handleManagementHandle: %v", errHandle)
	}
	response := value.(pluginapi.ManagementResponse)
	if got := response.Headers.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
	decoded := map[string]any{}
	if errUnmarshal := decodeJSON(response.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode status JSON: %v", errUnmarshal)
	}
	if decoded["provider"] != ProviderKey {
		t.Fatalf("provider = %v", decoded["provider"])
	}
	if accounts, _ := decoded["accounts"].([]any); len(accounts) != 1 {
		t.Fatalf("accounts = %v", decoded["accounts"])
	}
	selected := asRecord(decoded["selected"])
	if selected == nil || selected["uid"] != "uid-1" || selected["refreshable"] != true {
		t.Fatalf("selected = %v", decoded["selected"])
	}
	credit := asRecord(selected["credit"])
	if credit == nil {
		t.Fatalf("credit = %v", selected["credit"])
	}
	if total, found := readNumberField(credit, "total"); !found || total != 42.5 {
		t.Fatalf("credit total = %v", credit["total"])
	}
	version := asRecord(decoded["client_version"])
	if version == nil || version["version"] != "2026.9.4" || version["source"] != "remote" {
		t.Fatalf("client_version = %v", decoded["client_version"])
	}
	models, _ := decoded["models"].([]any)
	if len(models) != len(fallbackModels) {
		t.Fatalf("models = %d, want the fallback catalog", len(models))
	}
}

func TestHandleManagementStatusWithoutAccounts(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)
	value, errHandle := handleManagementHandle(host, mustJSON(t, managementRequest(
		http.MethodGet, "/v0/resource/plugins/lobsterai/status", nil, nil)))
	if errHandle != nil {
		t.Fatalf("handleManagementHandle: %v", errHandle)
	}
	body := string(value.(pluginapi.ManagementResponse).Body)
	if !strings.Contains(body, "尚未添加账号") || !strings.Contains(body, "去登录") {
		t.Fatalf("empty-state page = %s", firstLines(body, 30))
	}
}

func TestCheckinPageAndJSON(t *testing.T) {
	t.Run("html outcome reflects the response body", func(t *testing.T) {
		fake := newFakeHost().
			withFiles(pluginapi.HostAuthFileEntry{AuthIndex: "idx-1", Name: "a.json", Provider: ProviderKey}).
			withAuthJSON("idx-1", string(mustJSON(t, &Credential{AccessToken: "a"}))).
			on(httpRoute{Method: http.MethodGet, Match: ActivitySlotPath, Body: `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"d","configRevision":1}}}`}).
			on(httpRoute{Method: http.MethodGet, Match: ActivityContextPath + "/", Body: `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`}).
			on(httpRoute{Method: http.MethodPost, Match: "actions/check_in", Body: `{"code":0,"data":{"result":{"creditsGranted":5}}}`}).
			on(httpRoute{Method: http.MethodGet, Match: ProfileSummaryPath, Body: `{"code":0,"data":{"totalCreditsRemaining":5,"creditItems":[{"creditsRemaining":5}]}}`})
		host := installFakeHost(t, fake)

		value, errHandle := handleManagementHandle(host, mustJSON(t, managementRequest(
			http.MethodGet, "/v0/resource/plugins/lobsterai/checkin", url.Values{"action": {"checkin"}}, nil)))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle: %v", errHandle)
		}
		body := string(value.(pluginapi.ManagementResponse).Body)
		if !strings.Contains(body, "签到成功") || !strings.Contains(body, "5.00") {
			t.Fatalf("checkin page = %s", firstLines(body, 40))
		}
		// Honesty about idempotency has to be visible.
		if !strings.Contains(body, "幂等") || !strings.Contains(body, "响应体") {
			t.Fatalf("checkin page must explain idempotency:\n%s", firstLines(body, 40))
		}
		if !strings.Contains(body, "kind=claimed") {
			t.Fatalf("the page must show the body-derived verdict:\n%s", firstLines(body, 40))
		}
	})

	t.Run("repeat claim is reported as already-claimed", func(t *testing.T) {
		fake := newFakeHost().
			withFiles(pluginapi.HostAuthFileEntry{AuthIndex: "idx-1", Name: "a.json", Provider: ProviderKey}).
			withAuthJSON("idx-1", string(mustJSON(t, &Credential{AccessToken: "a"}))).
			on(httpRoute{Method: http.MethodGet, Match: ActivitySlotPath, Body: `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"d","configRevision":1}}}`}).
			on(httpRoute{Method: http.MethodGet, Match: ActivityContextPath + "/", Body: `{"code":0,"data":{"state":{"claimedToday":true},"actions":["check_in"]}}`})
		host := installFakeHost(t, fake)

		value, errHandle := handleManagementHandle(host, mustJSON(t, managementRequest(
			http.MethodGet, "/v0/resource/plugins/lobsterai/checkin", url.Values{"action": {"checkin"}}, nil)))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle: %v", errHandle)
		}
		body := string(value.(pluginapi.ManagementResponse).Body)
		if !strings.Contains(body, "今天已签到") || !strings.Contains(body, "kind=already-claimed") {
			t.Fatalf("repeat claim page = %s", firstLines(body, 40))
		}
		if len(fake.requestsFor("actions/check_in")) != 0 {
			t.Fatal("an already claimed activity must not issue a claim request")
		}
	})

	t.Run("json api", func(t *testing.T) {
		fake := newFakeHost().
			withFiles(pluginapi.HostAuthFileEntry{AuthIndex: "idx-1", Name: "a.json", Provider: ProviderKey}).
			withAuthJSON("idx-1", string(mustJSON(t, &Credential{AccessToken: "a"}))).
			on(httpRoute{Method: http.MethodGet, Match: ActivitySlotPath, Body: `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"d","configRevision":1}}}`}).
			on(httpRoute{Method: http.MethodGet, Match: ActivityContextPath + "/", Body: `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`}).
			on(httpRoute{Method: http.MethodPost, Match: "actions/check_in", Body: `{"code":0,"data":{"result":{"creditsGranted":5}}}`}).
			on(httpRoute{Method: http.MethodGet, Match: ProfileSummaryPath, Body: `{"code":0,"data":{"totalCreditsRemaining":5,"creditItems":[{"creditsRemaining":5}]}}`})
		host := installFakeHost(t, fake)

		value, errHandle := handleManagementHandle(host, mustJSON(t, jsonManagementRequest(
			http.MethodPost, "/v0/management/lobsterai/checkin", nil, nil)))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle: %v", errHandle)
		}
		response := value.(pluginapi.ManagementResponse)
		decoded := map[string]any{}
		if errUnmarshal := decodeJSON(response.Body, &decoded); errUnmarshal != nil {
			t.Fatalf("decode: %v", errUnmarshal)
		}
		if decoded["status"] != "claimed" || decoded["idempotent_keyed"] != true {
			t.Fatalf("json = %v", decoded)
		}
		if credit, found := readNumberField(decoded, "credit"); !found || credit != 5 {
			t.Fatalf("credit = %v", decoded["credit"])
		}
		if remaining, found := readNumberField(decoded, "remaining"); !found || remaining != 5 {
			t.Fatalf("remaining = %v", decoded["remaining"])
		}
	})

	t.Run("disabled by configuration", func(t *testing.T) {
		fake := newFakeHost()
		host := installFakeHost(t, fake)
		cfg := DefaultConfig()
		cfg.DailyCheckin = false
		setSettings(cfg)
		value, errHandle := handleManagementHandle(host, mustJSON(t, jsonManagementRequest(
			http.MethodPost, "/v0/management/lobsterai/checkin", nil, nil)))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle: %v", errHandle)
		}
		decoded := map[string]any{}
		_ = decodeJSON(value.(pluginapi.ManagementResponse).Body, &decoded)
		if decoded["status"] != "inactive" {
			t.Fatalf("status = %v", decoded["status"])
		}
		if len(fake.requests) != 0 {
			t.Fatal("a disabled check-in must not call upstream")
		}
	})
}

// TestStatusPageOffersAddAccount pins the affordance: a SECOND LobsterAI account
// must be reachable from the status page itself instead of only through the
// manager's OAuth page. The link stays a GET navigation into this plugin's own
// login route — 新建账号 adds a link, not a route.
func TestStatusPageOffersAddAccount(t *testing.T) {
	_, host, _ := statusFixture(t)
	response := renderStatusPage(host, managementRequest(http.MethodGet, "/status", nil, nil))
	body := string(response.Body)
	if !strings.Contains(body, "新建账号") {
		t.Fatalf("the status page offers no way to add a second account:\n%s", firstLines(body, 40))
	}
	if !strings.Contains(body, `href="login?add=1"`) {
		t.Fatalf("新建账号 must be a GET link into the login route:\n%s", firstLines(body, 40))
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestLoginPageExplainsAddingAnAccount pins the one-line caveat. 新建账号 and
// 重新登录 open the SAME browser flow on purpose (the credential file name comes
// from the account's own uid), so the page has to say which one this is.
func TestLoginPageExplainsAddingAnAccount(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)

	plain := string(renderLoginPage(host, managementRequest(http.MethodGet, "/login", nil, nil)).Body)
	if strings.Contains(plain, "新增账号") {
		t.Fatalf("the ordinary login page must not claim to add an account:\n%s", firstLines(plain, 40))
	}
	adding := string(renderLoginPage(host,
		managementRequest(http.MethodGet, "/login", url.Values{"add": {"1"}}, nil)).Body)
	if !strings.Contains(adding, "已有账号的凭据不受影响") {
		t.Fatalf("the add-account login page must state that the existing account survives:\n%s", firstLines(adding, 40))
	}
	if strings.Contains(adding, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestSecondAccountGetsItsOwnCredentialFile is the guarantee 新建账号 relies on:
// the auth file name is `lobsterai-<uid>.json` (the reference layout), so a
// second account writes a NEW file and leaves the first credential alone. The
// host saves by exactly this name (it feeds AuthData.FileName), and no code path
// in this plugin deletes a credential.
func TestSecondAccountGetsItsOwnCredentialFile(t *testing.T) {
	first := &Credential{AccessToken: "tok-a", UID: "101989", UserID: "y-1"}
	second := &Credential{AccessToken: "tok-b", UID: "202989", UserID: "y-2"}
	firstName, secondName := defaultAuthFileName(first), defaultAuthFileName(second)
	if firstName == secondName {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", firstName)
	}
	if firstName != ProviderKey+"-101989.json" {
		t.Fatalf("auth file name = %q, want the reference layout %s-101989.json", firstName, ProviderKey)
	}
}

func TestLoginPage(t *testing.T) {
	t.Run("landing page offers a start action", func(t *testing.T) {
		fake := newFakeHost()
		host := installFakeHost(t, fake)
		value, errHandle := handleManagementHandle(host, mustJSON(t, managementRequest(
			http.MethodGet, "/v0/resource/plugins/lobsterai/login", nil, nil)))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle: %v", errHandle)
		}
		body := string(value.(pluginapi.ManagementResponse).Body)
		if !strings.Contains(body, "开始登录") || !strings.Contains(body, "action=start") {
			t.Fatalf("login landing page = %s", firstLines(body, 40))
		}
		if strings.Contains(body, "<form") {
			t.Fatal("login page must use GET links")
		}
	})

	t.Run("start renders the authorization link without blocking", func(t *testing.T) {
		fake := newFakeHost()
		host := installFakeHost(t, fake)
		value, errHandle := handleManagementHandle(host, mustJSON(t, managementRequest(
			http.MethodGet, "/v0/resource/plugins/lobsterai/login", url.Values{"action": {"start"}}, nil)))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle: %v", errHandle)
		}
		body := string(value.(pluginapi.ManagementResponse).Body)
		if !strings.Contains(body, PortalBase+"/portal#/login?") {
			t.Fatalf("login page does not carry the portal URL: %s", firstLines(body, 40))
		}
		if !strings.Contains(body, "action=poll") {
			t.Fatalf("login page does not offer the poll action: %s", firstLines(body, 40))
		}
	})

	t.Run("poll for an unknown state reports a failure", func(t *testing.T) {
		fake := newFakeHost()
		host := installFakeHost(t, fake)
		value, errHandle := handleManagementHandle(host, mustJSON(t, managementRequest(
			http.MethodGet, "/v0/resource/plugins/lobsterai/login",
			url.Values{"action": {"poll"}, "state": {"nope"}}, nil)))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle: %v", errHandle)
		}
		body := string(value.(pluginapi.ManagementResponse).Body)
		if !strings.Contains(body, "登录失败") {
			t.Fatalf("poll page = %s", firstLines(body, 40))
		}
	})

	t.Run("json start and poll endpoints", func(t *testing.T) {
		fake := newFakeHost()
		host := installFakeHost(t, fake)
		value, errHandle := handleManagementHandle(host, mustJSON(t, jsonManagementRequest(
			http.MethodPost, "/v0/management/lobsterai/login/start", nil, nil)))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle: %v", errHandle)
		}
		decoded := map[string]any{}
		if errUnmarshal := decodeJSON(value.(pluginapi.ManagementResponse).Body, &decoded); errUnmarshal != nil {
			t.Fatalf("decode: %v", errUnmarshal)
		}
		if !strings.HasPrefix(readStringField(decoded, "url"), PortalBase) || readStringField(decoded, "state") == "" {
			t.Fatalf("start response = %v", decoded)
		}

		// Polling the fresh session without a callback reports "pending".
		state := readStringField(decoded, "state")
		value, errHandle = handleManagementHandle(host, mustJSON(t, jsonManagementRequest(
			http.MethodPost, "/v0/management/lobsterai/login/poll", nil,
			mustJSON(t, map[string]string{"State": state}))))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle(poll): %v", errHandle)
		}
		polled := map[string]any{}
		_ = decodeJSON(value.(pluginapi.ManagementResponse).Body, &polled)
		if polled["status"] != string(pluginapi.AuthLoginStatusPending) {
			t.Fatalf("poll = %v", polled)
		}
	})

	t.Run("poll without state is rejected", func(t *testing.T) {
		fake := newFakeHost()
		host := installFakeHost(t, fake)
		value, errHandle := handleManagementHandle(host, mustJSON(t, jsonManagementRequest(
			http.MethodPost, "/v0/management/lobsterai/login/poll", nil, nil)))
		if errHandle != nil {
			t.Fatalf("handleManagementHandle: %v", errHandle)
		}
		if value.(pluginapi.ManagementResponse).StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d", value.(pluginapi.ManagementResponse).StatusCode)
		}
	})
}

// TestPollLoginForManagementSavesAuth is the guard for the rule that the page,
// which drives auth.login.poll itself, must persist the credential: the host
// only saves what the normal poll reply returns.
func TestPollLoginForManagementSavesAuth(t *testing.T) {
	fake := newFakeHost().on(httpRoute{
		Method: http.MethodPost,
		Match:  ExchangePath,
		Body:   `{"code":0,"msg":"OK","data":{"accessToken":"access-1","refreshToken":"refresh-1","expiresIn":3600,"user":{"id":"uid-9"}}}`,
	})
	host := installFakeHost(t, fake)

	startValue, errStart := handleAuthLoginStart(host, []byte(`{}`))
	if errStart != nil {
		t.Fatalf("handleAuthLoginStart: %v", errStart)
	}
	session := startValue.(pluginapi.AuthLoginStartResponse)

	// Capture the callback in process, exactly like the browser would.
	response, errGet := http.Get(session.Metadata["redirect_uri"].(string) + "?code=good&state=" + url.QueryEscape(session.State))
	if errGet != nil {
		t.Fatalf("loopback callback: %v", errGet)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	var poll pluginapi.AuthLoginPollResponse
	deadline := time.Now().Add(3 * time.Second)
	for {
		value, errPoll := pollLoginForManagement(host, session.State)
		if errPoll != nil {
			t.Fatalf("pollLoginForManagement: %v", errPoll)
		}
		poll = value
		if poll.Status != pluginapi.AuthLoginStatusPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll never advanced past pending")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %s (%s)", poll.Status, poll.Message)
	}

	saved := fake.savedAuths()
	if len(saved) != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1 (the page must persist the credential)", len(saved))
	}
	if saved[0].Name != "lobsterai-uid-9.json" {
		t.Fatalf("saved name = %q, want the derived file name", saved[0].Name)
	}
	credential, errParse := ParseCredential(saved[0].JSON)
	if errParse != nil {
		t.Fatalf("saved credential is unusable: %v", errParse)
	}
	if credential.AccessToken != "access-1" || credential.UID != "uid-9" || credential.UUID != session.Metadata["uuid"] {
		t.Fatalf("saved credential = %+v", credential)
	}
}

func TestManagementHandleUnknownRoute(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)
	value, errHandle := handleManagementHandle(host, mustJSON(t, jsonManagementRequest(
		http.MethodGet, "/v0/management/lobsterai/nope", nil, nil)))
	if errHandle != nil {
		t.Fatalf("handleManagementHandle: %v", errHandle)
	}
	response := value.(pluginapi.ManagementResponse)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

func TestStatusPageReportsUpstreamFailureHonestly(t *testing.T) {
	fake := newFakeHost().
		withFiles(pluginapi.HostAuthFileEntry{AuthIndex: "idx-1", Name: "a.json", Provider: ProviderKey}).
		withAuthJSON("idx-1", string(mustJSON(t, &Credential{AccessToken: "a"})))
	host := installFakeHost(t, fake)
	value, errHandle := handleManagementHandle(host, mustJSON(t, managementRequest(
		http.MethodGet, "/v0/resource/plugins/lobsterai/status", nil, nil)))
	if errHandle != nil {
		t.Fatalf("handleManagementHandle: %v", errHandle)
	}
	body := string(value.(pluginapi.ManagementResponse).Body)
	// No network in tests: the page must show the reason, never a fake 0.
	if !strings.Contains(body, "查询失败") {
		t.Fatalf("status page hid the upstream failure:\n%s", firstLines(body, 60))
	}
	if !strings.Contains(body, "未知（无法解析") {
		t.Fatalf("status page must report an unknown expiry honestly:\n%s", firstLines(body, 60))
	}
}

func TestQuiesceAndShutdown(t *testing.T) {
	plugin := Plugin().(*plugin)
	plugin.Quiesce()
	plugin.Shutdown()
}

func firstLines(text string, count int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}
