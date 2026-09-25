package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestManagementRouteMatchesBothMounts guards a real defect: the host passes the
// full request path, which differs between the two mounts, so matching on the
// whole path silently 404s one of them.
func TestManagementRouteMatchesBothMounts(t *testing.T) {
	cases := map[string]string{
		"/status":                                        "/status",
		"/status/":                                       "/status",
		"/v0/management/status":                          "/status",
		"/v0/resource/plugins/codearts/status":           "/status",
		"/v0/management/codearts/checkin":                "/checkin",
		"/v0/resource/plugins/codearts/login":            "/login",
		"/v0/resource/plugins/codearts/codearts/checkin": "/checkin",
	}
	for input, want := range cases {
		if got := managementRoute(input); got != want {
			t.Errorf("managementRoute(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWantsJSON(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		accept   string
		wantJSON bool
	}{
		{"browser navigation", "", "text/html,application/xhtml+xml,*/*;q=0.8", false},
		{"explicit json", "format=json", "text/html", true},
		// An absent Accept is ambiguous; defaulting to the page keeps a UI from
		// ever showing raw JSON. Scripts ask for JSON explicitly.
		{"absent accept defaults to the page", "", "", false},
		{"api client", "", "application/json", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := pluginapi.ManagementRequest{Headers: map[string][]string{}}
			if testCase.accept != "" {
				request.Headers.Set("Accept", testCase.accept)
			}
			request.Query = map[string][]string{}
			if testCase.query != "" {
				parts := strings.SplitN(testCase.query, "=", 2)
				request.Query.Set(parts[0], parts[1])
			}
			if got := wantsJSON(request); got != testCase.wantJSON {
				t.Fatalf("wantsJSON = %v, want %v", got, testCase.wantJSON)
			}
		})
	}
}

// TestRenderLoginPageIsThemedHTML pins the contract management clients rely on:
// a full HTML document that consumes the host theme variables and offers its
// action as a GET link.
func TestRenderLoginPageIsThemedHTML(t *testing.T) {
	response := renderLoginPage(nil, pluginapi.ManagementRequest{Query: map[string][]string{}})
	if response.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Headers["Content-Type"]; len(got) != 1 || !strings.Contains(got[0], "text/html") {
		t.Fatalf("Content-Type = %#v, want text/html", got)
	}
	body := string(response.Body)
	for _, want := range []string{
		"<!DOCTYPE html>",
		"href=\"?action=login\"",
		"var(--bg-primary",
		"var(--primary-color",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page is missing %q", want)
		}
	}
	if strings.Contains(body, "<form") {
		t.Error("the resource route is dispatched as GET only, so actions must not be forms")
	}
}

// TestRenderStatusPageWithoutAccountsOffersLogin covers the empty state, which is
// what a fresh install sees.
func TestRenderStatusPageWithoutAccountsOffersLogin(t *testing.T) {
	response := renderStatusPage(nil, pluginapi.ManagementRequest{Query: map[string][]string{}})
	body := string(response.Body)
	if !strings.Contains(body, "尚未添加账号") {
		t.Fatalf("empty state is missing its explanation:\n%s", body)
	}
	if !strings.Contains(body, "href=\"login\"") {
		t.Error("empty state must link the user to the login page")
	}
}

// TestManagementRegisteredRoutesFollowHostMountRules pins the three constraints
// that decide whether a route is reachable at all and how the manager renders it:
// menu routes are resource-only, non-menu routes must self-namespace because the
// management path is global, and exactly one route may carry a menu.
func TestManagementRegisteredRoutesFollowHostMountRules(t *testing.T) {
	value, errRegister := handleManagementRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("handleManagementRegister: %v", errRegister)
	}
	registration, ok := value.(pluginapi.ManagementRegistrationResponse)
	if !ok {
		t.Fatalf("unexpected registration type %T", value)
	}

	var plain int
	for _, route := range registration.Routes {
		if route.Path == "" || !strings.HasPrefix(route.Path, "/") {
			t.Errorf("route %q must be an absolute path", route.Path)
		}
		if route.Menu != "" {
			if !strings.EqualFold(route.Method, "GET") {
				t.Errorf("route %s carries a menu but is %s; only GET+menu mounts on the resource path", route.Path, route.Method)
			}
			continue
		}
		plain++
		// Without a menu the route lives in the global management namespace, so
		// it has to be prefixed or it will collide and be dropped silently.
		if !strings.HasPrefix(route.Path, "/"+ProviderKey+"/") {
			t.Errorf("route %q has no menu and is not namespaced under /%s/, so it can be skipped as a collision", route.Path, ProviderKey)
		}
	}
	if plain == 0 {
		t.Fatal("expected at least one plain script route")
	}

	// The manager turns every menu-carrying route into its own sidebar item and
	// does not group them by plugin, so a second menu route is a second entry
	// that looks like a duplicate. Only the status page may carry one.
	menuPaths := make([]string, 0, 2)
	for _, route := range registration.Routes {
		if route.Menu != "" {
			menuPaths = append(menuPaths, route.Path)
		}
	}
	for _, resource := range registration.Resources {
		if resource.Path == "" || !strings.HasPrefix(resource.Path, "/") {
			t.Errorf("resource %q must be an absolute path", resource.Path)
		}
		if resource.Menu != "" {
			menuPaths = append(menuPaths, resource.Path)
		}
	}
	if len(menuPaths) != 1 || menuPaths[0] != "/status" {
		t.Fatalf("menu routes = %v, want exactly [/status]; every extra menu route becomes another sidebar entry", menuPaths)
	}

	// The login page must stay reachable from the status page while adding no
	// sidebar entry of its own.
	var loginResource bool
	for _, resource := range registration.Resources {
		if resource.Path == "/login" {
			loginResource = true
		}
	}
	if !loginResource {
		t.Error("login page is not registered as a resource route, so the status page link would 404")
	}
}
