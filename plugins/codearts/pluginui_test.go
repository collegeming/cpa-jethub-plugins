package main

import (
	"encoding/json"
	"fmt"
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

// TestStatusPageOffersAddAccount pins the affordance: a SECOND CodeArts account
// must be reachable from the status page itself instead of only through the
// manager's OAuth page. The link stays a GET navigation into this plugin's own
// login route — 新建账号 adds a link, not a route.
func TestStatusPageOffersAddAccount(t *testing.T) {
	host := stubAuthList(t, pluginapi.HostAuthFileEntry{
		Provider: ProviderKey, AuthIndex: "idx-1", Name: "codearts-alice.json",
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
// 重新登录 open the SAME login flow on purpose (the credential file name comes
// from the account's own identity), so the page has to say which one this is.
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
// login writes a NEW file and leaves the first credential alone. The host saves
// by exactly this name (it feeds AuthData.FileName), and no code path in this
// plugin deletes a credential.
func TestSecondAccountGetsItsOwnCredentialFile(t *testing.T) {
	first := &Credential{Type: ProviderKey, UserName: "alice", DomainID: "tenant-1", AccessKeyID: "AKALICE"}
	second := &Credential{Type: ProviderKey, UserName: "bob", DomainID: "tenant-1", AccessKeyID: "AKBOB"}
	firstName, secondName := defaultAuthFileName(first), defaultAuthFileName(second)
	if firstName == secondName {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", firstName)
	}
	if !strings.HasPrefix(firstName, ProviderKey+"-") || !strings.HasSuffix(firstName, ".json") {
		t.Fatalf("auth file name = %q, want %s-*.json", firstName, ProviderKey)
	}
	// An account without a user name still gets a name of its own: the fallbacks
	// walk down to the access key id instead of collapsing to a constant.
	anonymousA := defaultAuthFileName(&Credential{Type: ProviderKey, AccessKeyID: "AKONE"})
	anonymousB := defaultAuthFileName(&Credential{Type: ProviderKey, AccessKeyID: "AKTWO"})
	if anonymousA == anonymousB {
		t.Fatalf("two anonymous accounts share the file name %q", anonymousA)
	}
}

// TestManagementRegisteredRoutesFollowHostMountRules pins the three constraints
// that decide whether a route is reachable at all and how the manager renders it:
// no route may carry a menu (the hub plugin owns the single sidebar entry),
// non-menu routes must self-namespace because the management path is global, and
// the pages the hub links to must stay mounted on the resource path.
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
			t.Errorf("route %s carries menu %q: the sidebar belongs to the hub plugin", route.Path, route.Menu)
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
	// does not group them by plugin. This repository gives that one item to the
	// hub, so every page below must be a Menu-less resource route.
	pages := map[string]bool{}
	for _, resource := range registration.Resources {
		if resource.Path == "" || !strings.HasPrefix(resource.Path, "/") {
			t.Errorf("resource %q must be an absolute path", resource.Path)
		}
		if resource.Menu != "" {
			t.Errorf("resource %s carries menu %q, which would add a sidebar entry", resource.Path, resource.Menu)
		}
		pages[resource.Path] = true
	}
	// /status is what the hub's channel overview links to; /login is reached from
	// the status page.
	for _, want := range []string{"/status", "/login"} {
		if !pages[want] {
			t.Errorf("resource %s is not registered, so the page would 404", want)
		}
	}
}
