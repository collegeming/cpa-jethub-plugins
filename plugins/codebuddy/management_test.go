package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// stubAuthList installs a host transport whose credential listing holds exactly
// the given entries and answers nothing else. The status page only needs the
// listing to get past its empty state; a credential that cannot be read is a
// state the page already renders.
func stubAuthList(t *testing.T, entries ...pluginapi.HostAuthFileEntry) *abiboot.Host {
	t.Helper()
	abiboot.SetHostCaller(func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthList {
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
		return abiboot.OK(map[string]any{"files": entries})
	})
	t.Cleanup(abiboot.ClearHostCaller)
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"test-callback"}`))
}

func TestManagementRoute(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v0/management/codebuddy/status", "/status"},
		{"/v0/resource/plugins/codebuddy/status", "/status"},
		{"/v0/resource/plugins/codebuddy/checkin/", "/checkin"},
		{"/v0/management/codebuddy/checkin", "/checkin"},
		{"/status", "/status"},
		{"status", "/status"},
		{"  /a/b/login/  ", "/login"},
		{"/v0/resource/plugins/codebuddy/status?format=json", "/status"},
		{"/v0/management/codebuddy/checkin#top", "/checkin"},
	}
	for _, test := range tests {
		if got := managementRoute(test.path); got != test.want {
			t.Errorf("managementRoute(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestWantsJSON(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		query  string
		accept string
		want   bool
	}{
		{"explicit format on a page route", "/v0/resource/plugins/codebuddy/status", "format=json", "text/html", true},
		{"explicit html on a script route", "/v0/management/codebuddy/status", "format=html", "application/json", false},
		{"browser navigation", "/v0/resource/plugins/codebuddy/status", "", "text/html,application/xhtml+xml", false},
		{"curl on the resource route shows the page", "/v0/resource/plugins/codebuddy/status", "", "*/*", false},
		{"curl without an Accept header on the resource route shows the page", "/v0/resource/plugins/codebuddy/status", "", "", false},
		{"a script route with a bare Accept is data", "/v0/management/codebuddy/status", "", "*/*", true},
		{"a script route with no Accept is data", "/v0/management/codebuddy/status", "", "", true},
		{"explicit json accept", "/v0/resource/plugins/codebuddy/status", "", "application/json", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := pluginapi.ManagementRequest{Path: test.path, Query: url.Values{}, Headers: http.Header{}}
			if test.query != "" {
				request.Query, _ = url.ParseQuery(test.query)
			}
			if test.accept != "" {
				request.Headers.Set("Accept", test.accept)
			}
			if got := wantsJSON(request); got != test.want {
				t.Fatalf("wantsJSON = %v, want %v", got, test.want)
			}
		})
	}
}

func TestManagementRegisterRoutes(t *testing.T) {
	previous := settings()
	defer setSettings(previous)

	setSettings(Config{Product: ProductCodeBuddy})
	value, errRegister := handleManagementRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	response, ok := value.(pluginapi.ManagementRegistrationResponse)
	if !ok {
		t.Fatalf("register returned %T", value)
	}

	menus := 0
	globalRoutes := 0
	for _, route := range response.Routes {
		if route.Path == "" {
			t.Errorf("route without a path: %+v", route)
		}
		if route.Menu != "" {
			menus++
			t.Errorf("menu route %s uses menu %q; the sidebar belongs to the hub plugin", route.Path, route.Menu)
		}
		globalRoutes++
		// The management namespace is global: every path must carry the
		// provider prefix or a collision silently drops it.
		if len(route.Path) < len(ProviderKey)+2 || route.Path[1:1+len(ProviderKey)] != ProviderKey {
			t.Errorf("global route %s must be prefixed with /%s", route.Path, ProviderKey)
		}
	}
	// CPA-Manager-Plus renders ONE sidebar entry per menu route and does not
	// group them by plugin. The repository gives that one entry to the hub, so
	// this plugin declares none: the status page, login and check-in all ride the
	// Menu-less resource list and are reached from the hub or from each other.
	if menus != 0 {
		t.Errorf("menus = %d, want zero; extra menu routes duplicate sidebar entries", menus)
	}
	if globalRoutes < 2 {
		t.Errorf("script routes = %d, want the status and checkin JSON endpoints", globalRoutes)
	}
	// Every interactive page must still be reachable, menu or not.
	for _, wanted := range []string{"/status", "/login", "/checkin"} {
		found := false
		for _, route := range response.Resources {
			if route.Path == wanted {
				found = true
			}
		}
		for _, route := range response.Routes {
			if route.Path == wanted {
				found = true
			}
		}
		if !found {
			t.Errorf("page %s is not reachable on any mount", wanted)
		}
	}

	// A product without check-in must not advertise the check-in page.
	setSettings(Config{Product: ProductWorkBuddy})
	value, errRegister = handleManagementRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	response = value.(pluginapi.ManagementRegistrationResponse)
	for _, route := range response.Resources {
		if route.Path == "/checkin" {
			t.Error("WorkBuddy has no check-in endpoint, so the page must not be advertised")
		}
	}
	for _, route := range response.Routes {
		if route.Path == "/checkin" && route.Menu != "" {
			t.Error("WorkBuddy has no check-in endpoint, so the page must not be advertised")
		}
	}
}

// mustMarshal encodes a management request the way the ABI layer would.
func mustMarshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	return encoded
}

func TestManagementHandleRejectsUnknownRoute(t *testing.T) {
	request := pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/codebuddy/nope", Query: url.Values{}, Headers: http.Header{}}
	raw := mustMarshal(t, request)
	value, errHandle := handleManagementHandle(nil, raw)
	if errHandle != nil {
		t.Fatalf("handle: %v", errHandle)
	}
	response := value.(pluginapi.ManagementResponse)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}

func TestLoginJSONHint(t *testing.T) {
	request := pluginapi.ManagementRequest{Query: url.Values{"action": {"login"}}}
	response := loginJSON(request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if len(response.Body) == 0 {
		t.Fatal("empty body")
	}
}

// TestStatusPageOffersAddAccount pins the affordance: a SECOND account of the
// same product must be reachable from the status page itself instead of only
// through the manager's OAuth page. The link stays a GET navigation into this
// plugin's own login route — 新建账号 adds a link, not a route.
func TestStatusPageOffersAddAccount(t *testing.T) {
	previous := settings()
	defer setSettings(previous)
	setSettings(Config{Product: ProductCodeBuddy})

	host := stubAuthList(t, pluginapi.HostAuthFileEntry{
		Provider: ProviderKey, AuthIndex: "idx-1", Name: "codebuddy-alice-1a2b.json",
	})
	response := renderStatusPage(host, pluginapi.ManagementRequest{Query: map[string][]string{}})
	body := string(response.Body)
	if !strings.Contains(body, "新建账号") {
		t.Fatalf("the status page offers no way to add a second account:\n%s", body)
	}
	if !strings.Contains(body, `href="login?add=1"`) {
		t.Fatalf("新建账号 must be a GET link into the login route:\n%s", body)
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestLoginPageExplainsAddingAnAccount pins the one-line caveat. 新建账号 and
// 重新登录 open the SAME login flow on purpose, so the page has to say which one
// this is.
func TestLoginPageExplainsAddingAnAccount(t *testing.T) {
	plain := string(renderLoginPage(nil, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	if strings.Contains(plain, "新增账号") {
		t.Fatalf("the ordinary login page must not claim to add an account:\n%s", plain)
	}
	adding := string(renderLoginPage(nil, pluginapi.ManagementRequest{Query: map[string][]string{"add": {"1"}}}).Body)
	if !strings.Contains(adding, "已有账号的凭据不受影响") {
		t.Fatalf("the add-account login page must state that the existing account survives:\n%s", adding)
	}
	if strings.Contains(adding, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestSecondAccountGetsItsOwnCredentialFile is the guarantee 新建账号 relies on:
// the auth file name is derived from the account's own identity, so a second
// account writes a NEW file and leaves the credentials already on disk alone.
// The host saves by exactly this name (it feeds AuthData.FileName), and no code
// path in this plugin deletes a credential.
func TestSecondAccountGetsItsOwnCredentialFile(t *testing.T) {
	alice := &Credential{AccessToken: "tok-a", UserID: "u-1", Nickname: "alice", Product: ProductCodeBuddy}
	bob := &Credential{AccessToken: "tok-b", UserID: "u-2", Nickname: "bob", Product: ProductCodeBuddy}
	firstName, secondName := defaultAuthFileName(alice), defaultAuthFileName(bob)
	if firstName == secondName {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", firstName)
	}
	// The same account must land in the SAME file: this name is what the host
	// writes a completed login — and every later renewal — to, so a name that
	// moved would leave one duplicate entry per login behind (Bug A).
	if again := defaultAuthFileName(&Credential{AccessToken: "tok-a2", UserID: "u-1", Nickname: "alice"}); again != firstName {
		t.Fatalf("a repeat login of the same account derived %q after %q, want the same file", again, firstName)
	}
	if !strings.HasPrefix(firstName, ProviderKey+"-") || !strings.HasSuffix(firstName, ".json") {
		t.Fatalf("auth file name = %q, want %s-*.json", firstName, ProviderKey)
	}
	// A Chinese nickname (the live case: 黎明文铮) sanitises down to nothing, so
	// the user id has to decide — collapsing to the shared constant "account"
	// is what produced two files named codebuddy-account-<random>.json on disk.
	cjkA := defaultAuthFileName(&Credential{AccessToken: "tok", UserID: "u-3", Nickname: "黎明文铮"})
	cjkB := defaultAuthFileName(&Credential{AccessToken: "tok", UserID: "u-4", Nickname: "张三"})
	if cjkA == cjkB {
		t.Fatalf("two CJK-nicknamed accounts share the file name %q", cjkA)
	}
	if cjkA != ProviderKey+"-u-3.json" {
		t.Fatalf("CJK nickname derived %q, want the user id to decide", cjkA)
	}
}

// The product is pinned at build time (one artifact per product), so the page for
// a fresh instance must not send the user into the config file: that instruction
// died with the build-variant change and is wrong for every artifact.
func TestNoAccountPageDoesNotAskForTheProductSetting(t *testing.T) {
	body := string(renderStatusPage(nil, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	if strings.Contains(body, "请先在插件配置里选定") {
		t.Fatalf("the no-account page still asks for the product setting:\n%s", body)
	}
	for _, want := range []string{"构建产物里已经固定", "独立插件"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the no-account page does not explain the build-pinned product (%q missing):\n%s", want, body)
		}
	}
}

func TestCheckinStatusText(t *testing.T) {
	tests := []struct {
		status *checkinStatus
		want   string
	}{
		{&checkinStatus{Active: false, TodayCheckedIn: true}, "活动未开启"},
		{&checkinStatus{Active: true, TodayCheckedIn: true}, "今日已签到"},
		{&checkinStatus{Active: true}, "今日可签到"},
	}
	for _, test := range tests {
		if got := checkinStatusText(test.status); got != test.want {
			t.Errorf("checkinStatusText(%+v) = %q, want %q", test.status, got, test.want)
		}
	}
}

func TestCatalogSizeUsesTheBuiltInTableWhenCold(t *testing.T) {
	previous := settings()
	defer setSettings(previous)
	setSettings(DefaultConfig())
	discoveredModels.reset()

	product, _ := productByConfigValue(ProductCodeBuddy)
	if got, want := catalogSize(product), len(codeBuddyFallbackModels); got != want {
		t.Fatalf("cold catalog size = %d, want %d", got, want)
	}
	discoveredModels.put(cachedCatalogKey(product), []remoteModel{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	if got := catalogSize(product); got != 3 {
		t.Fatalf("warm catalog size = %d, want the cached 3", got)
	}
	discoveredModels.reset()
}

func TestJSONManagementResponse(t *testing.T) {
	response := jsonManagementResponse(http.StatusTeapot, map[string]any{"a": 1})
	if response.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d", response.StatusCode)
	}
	if response.Headers.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("content type = %q", response.Headers.Get("Content-Type"))
	}
	if string(response.Body) != `{"a":1}` {
		t.Errorf("body = %s", response.Body)
	}
}
