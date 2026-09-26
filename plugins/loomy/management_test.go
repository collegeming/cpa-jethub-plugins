package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// registerResponse decodes the management registration.
func registerResponse(t *testing.T) pluginapi.ManagementRegistrationResponse {
	t.Helper()
	value, errRegister := handleManagementRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("management.register: %v", errRegister)
	}
	return decodeResult[pluginapi.ManagementRegistrationResponse](t, value)
}

// Exactly ONE route carries a Menu. The manager renders one sidebar entry per
// menu route and does not group them by plugin, so extra menus look like
// duplicates (README "挂载规则").
func TestManagementRegistrationHasOneMenuAndNoLooseRoutes(t *testing.T) {
	registration := registerResponse(t)
	if len(registration.Routes) != 1 {
		t.Fatalf("routes = %d, want exactly 1 (the status menu route)", len(registration.Routes))
	}
	status := registration.Routes[0]
	if status.Menu == "" {
		t.Fatal("the single route must carry a Menu, otherwise there is no sidebar entry at all")
	}
	if status.Method != http.MethodGet || status.Path != "/status" {
		t.Fatalf("menu route = %s %s, want GET /status", status.Method, status.Path)
	}
	if len(registration.Resources) == 0 {
		t.Fatal("the sub-pages must be declared as resources")
	}
	seen := map[string]bool{}
	for _, resource := range registration.Resources {
		if resource.Menu != "" {
			t.Errorf("resource %s carries Menu %q, which would add a sidebar entry", resource.Path, resource.Menu)
		}
		if resource.Description == "" {
			t.Errorf("resource %s needs a description", resource.Path)
		}
		if seen[resource.Path] {
			t.Errorf("resource %s is declared twice", resource.Path)
		}
		seen[resource.Path] = true
	}
	for _, want := range []string{"/login", "/checkin", "/onboarding"} {
		if !seen[want] {
			t.Errorf("resource %s is missing", want)
		}
	}
}

// Page navigations get HTML; scripts get JSON from the very same route.
func TestManagementDispatchHTMLAndJSON(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"balance":15000,"dailyBalance":4992,"availableBalance":19992}}`), nil
	}
	fake.install(t)
	withAccount(t, fake)

	page := callManagement(t, testHost(), managementRequest(http.MethodGet, "/v0/resource/plugins/loomy/status", nil, nil))
	if page.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", page.StatusCode)
	}
	if got := page.Headers.Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Fatalf("content type = %q, want text/html", got)
	}
	if !strings.Contains(string(page.Body), "Loomy") {
		t.Fatalf("page = %s, want the plugin heading", truncate(string(page.Body), 200))
	}

	forced := callManagement(t, testHost(), managementRequest(http.MethodGet,
		"/v0/resource/plugins/loomy/status", url.Values{"format": {"json"}}, nil))
	if got := forced.Headers.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("forced content type = %q, want JSON", got)
	}

	byAccept := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/v0/resource/plugins/loomy/status", nil))
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(byAccept.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode status json: %v", errUnmarshal)
	}
	if payload["provider"] != ProviderKey {
		t.Fatalf("provider = %v, want %q", payload["provider"], ProviderKey)
	}
	if payload["refreshable"] != false {
		t.Fatalf("refreshable = %v, want false: there is no refresh endpoint", payload["refreshable"])
	}
	if accounts, ok := payload["accounts"].(float64); !ok || accounts != 1 {
		t.Fatalf("accounts = %v, want 1", payload["accounts"])
	}
	points, ok := payload["points"].(map[string]any)
	if !ok {
		t.Fatalf("points = %#v, want the parsed pools", payload["points"])
	}
	if points["balance"] != float64(15000) || points["daily_balance"] != float64(4992) {
		t.Fatalf("points = %#v, want the two pools", points)
	}

	// A trailing slash is tolerated: the host may or may not keep it.
	withSlash := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/v0/resource/plugins/loomy/status/", nil))
	if withSlash.StatusCode != 200 {
		t.Fatalf("trailing-slash status = %d, want 200", withSlash.StatusCode)
	}
}

// An unknown route is a 404 JSON, not a panic.
func TestManagementUnknownRoute(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	response := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/v0/resource/plugins/loomy/nope", nil))
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
	if !strings.Contains(string(response.Body), "unknown Loomy management route") {
		t.Fatalf("body = %s, want the unknown-route error", response.Body)
	}
}

// Only this provider's credentials are listed.
func TestStatusCountsOnlyLoomyAccounts(t *testing.T) {
	fake := newFakeHost()
	fake.files = []pluginapi.HostAuthFileEntry{
		{Provider: "codearts", AuthIndex: "x", Name: "codearts.json"},
		{Provider: ProviderKey, AuthIndex: "idx-1", Name: "loomy-13800138000.json"},
	}
	fake.install(t)
	response := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/status", nil))
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode status json: %v", errUnmarshal)
	}
	if accounts := payload["accounts"].(float64); accounts != 1 {
		t.Fatalf("accounts = %v, want only the Loomy credential", accounts)
	}
	account, ok := payload["account"].(map[string]any)
	if !ok {
		t.Fatalf("account = %#v, want the selected account", payload["account"])
	}
	if account["name"] != "loomy-13800138000.json" {
		t.Fatalf("account = %#v, want the Loomy credential", account)
	}
}

// The daily grant is a WRITE: a bare request only confirms, and `action=claim`
// performs it. The probe endpoint is never used for a read of the quota.
func TestCheckinRequiresExplicitAction(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"alreadyProcessed":false,"permanentBalance":5000,"dailyBalance":5000,"dailyQuota":5000,"dailyConsumed":0}}`), nil
	}
	fake.install(t)
	withAccount(t, fake)

	confirm := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/checkin", nil))
	if calls := fake.callsFor(PointsFirstLoginPath); len(calls) != 0 {
		t.Fatalf("a bare check-in page issued %d write calls, want 0", len(calls))
	}
	if !strings.Contains(string(confirm.Body), "confirm") {
		t.Fatalf("body = %s, want a confirmation payload", confirm.Body)
	}

	claimed := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/checkin", url.Values{"action": {"claim"}}))
	if calls := fake.callsFor(PointsFirstLoginPath); len(calls) != 1 {
		t.Fatalf("issued %d write calls, want 1", len(calls))
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(claimed.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode claim json: %v", errUnmarshal)
	}
	if payload["status"] != "claimed" {
		t.Fatalf("status = %v, want claimed", payload["status"])
	}
}

// The onboarding status read is read-only; `action=claim` posts the completions.
func TestOnboardingActionDispatch(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.Method == http.MethodGet {
			return httpResponse(200, `{"code":"000000","data":{"tasks":{"first_message":true},"total":10000}}`), nil
		}
		return httpResponse(200, `{"code":"000000","data":{"alreadyCompleted":true,"balance":10000}}`), nil
	}
	fake.install(t)
	withAccount(t, fake)

	state := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/onboarding", nil))
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(state.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode state json: %v", errUnmarshal)
	}
	if payload["earned"] != float64(500) {
		t.Fatalf("earned = %v, want the locally recomputed 500", payload["earned"])
	}
	if posts := countPosts(fake, OnboardingCompletePath); posts != 0 {
		t.Fatalf("the status read posted %d completions, want 0", posts)
	}

	claimed := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/onboarding", url.Values{"action": {"claim"}}))
	if !strings.Contains(string(claimed.Body), "claimed") {
		t.Fatalf("body = %s, want the claim result", claimed.Body)
	}
	if posts := countPosts(fake, OnboardingCompletePath); posts != len(onboardingTasks)-1 {
		t.Fatalf("posted %d completions, want %d (the completed task is skipped)", posts, len(onboardingTasks)-1)
	}
}

// withAccount registers one Loomy credential in the fake host.
func withAccount(t *testing.T, fake *fakeHost) {
	t.Helper()
	storage, errEncode := sampleCredential(t).Encode()
	if errEncode != nil {
		t.Fatalf("encode credential: %v", errEncode)
	}
	fake.mu.Lock()
	fake.files = []pluginapi.HostAuthFileEntry{{Provider: ProviderKey, AuthIndex: "idx-1", Name: "loomy-13800138000.json", Status: "active"}}
	fake.auths["idx-1"] = storage
	fake.mu.Unlock()
}

// countPosts counts the recorded requests that used POST on a matching URL.
func countPosts(fake *fakeHost, match string) int {
	count := 0
	for _, request := range fake.callsFor(match) {
		if request.Method == http.MethodPost {
			count++
		}
	}
	return count
}
