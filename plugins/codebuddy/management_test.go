package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
		switch {
		case route.Menu != "":
			menus++
			// Resource routes are dispatched as GET only, so a menu route must
			// be a GET.
			if route.Method != http.MethodGet {
				t.Errorf("menu route %s uses %s, want GET", route.Path, route.Method)
			}
		default:
			globalRoutes++
			// The management namespace is global: every path must carry the
			// provider prefix or a collision silently drops it.
			if len(route.Path) < len(ProviderKey)+2 || route.Path[1:1+len(ProviderKey)] != ProviderKey {
				t.Errorf("global route %s must be prefixed with /%s", route.Path, ProviderKey)
			}
		}
	}
	// CPA-Manager-Plus renders ONE sidebar entry per menu route and does not
	// group them by plugin, so only the status page may carry a menu. Login and
	// check-in ride the menu-less resource list and are reached from the page.
	if menus != 1 {
		t.Errorf("menus = %d, want exactly the status page; extra menu routes duplicate sidebar entries", menus)
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
