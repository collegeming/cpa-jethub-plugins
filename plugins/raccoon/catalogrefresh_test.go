package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogueBody renders a `GET /model_catalog` answer listing the given ids.
func catalogueBody(ids ...string) string {
	models := make([]string, 0, len(ids))
	for _, id := range ids {
		models = append(models, `{"name":"`+id+`","description":"`+id+`","visible":true}`)
	}
	return `{"code":0,"data":{"categories":[{"type":"chat","models":[` + strings.Join(models, ",") + `]}]}}`
}

// refreshHost installs a fake host with one account and answers the catalogue
// endpoint from `body`; an empty body simulates an unreachable vendor.
func refreshHost(t *testing.T, body string) (*fakeHost, pluginapi.HostAuthFileEntry) {
	t.Helper()
	fake := newFakeHost()
	fake.install(t)
	entry := storedCredential(t, fake, "idx-1", "raccoon-alice.json", sampleCredential(t))
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, ModelCatalogPath) {
			if body == "" {
				return httpResponse(http.StatusInternalServerError, ""), nil
			}
			return httpResponse(http.StatusOK, body), nil
		}
		return httpResponse(http.StatusNotFound, ""), nil
	}
	return fake, entry
}

// TestCatalogueRefreshReportsUpstreamFailureAsFailure is the honesty guard: when
// the catalogue endpoint does not answer, discoverModels returns an empty slice
// and `model.for_auth` would publish the bundled table. That fallback is NOT a
// successful refresh — reporting it as one would tell the operator the vendor
// agreed when it never answered — and it must not be cached.
func TestCatalogueRefreshReportsUpstreamFailureAsFailure(t *testing.T) {
	fake, _ := refreshHost(t, "")
	host := testHost()

	if _, errRefresh := catalogueRefresh(host, DefaultConfig()); errRefresh == nil {
		t.Fatal("a refresh whose endpoint failed was reported as a success")
	}
	if cached, _ := peekCachedModels(); len(cached) != 0 {
		t.Fatalf("a failed refresh cached %d models; the bundled table must not be cached", len(cached))
	}
	if names := fake.savedNames(); len(names) != 0 {
		t.Fatalf("a failed refresh published %v; the host would re-register the unchanged catalogue", names)
	}
}

// TestCatalogueRefreshReportsNoAccount covers the other way a refresh cannot run.
func TestCatalogueRefreshReportsNoAccount(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	if _, errRefresh := catalogueRefresh(testHost(), DefaultConfig()); errRefresh == nil {
		t.Fatal("a refresh without any account was reported as a success")
	}
}

// TestCatalogueRefreshReportsDisabledDiscovery pins that a catalogue the
// operator pinned to the bundled table is not silently "refreshed".
func TestCatalogueRefreshReportsDisabledDiscovery(t *testing.T) {
	_, _ = refreshHost(t, catalogueBody("sn-kimi-k3"))
	cfg := DefaultConfig()
	cfg.DiscoverModels = false
	if _, errRefresh := catalogueRefresh(testHost(), cfg); errRefresh == nil {
		t.Fatal("a refresh with discovery disabled was reported as a success")
	}
}

// TestCatalogueRefreshComputesChanged pins the Changed flag in both directions:
// a first fetch differs from the empty cache, an identical second fetch does not,
// and a listing that grew does.
func TestCatalogueRefreshComputesChanged(t *testing.T) {
	fake, _ := refreshHost(t, catalogueBody("sn-kimi-k3", "sn-glm-5-3"))
	host := testHost()

	outcome, errRefresh := catalogueRefresh(host, DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh: %v", errRefresh)
	}
	if outcome.Models != 2 {
		t.Fatalf("Models = %d, want the 2 the vendor listed", outcome.Models)
	}
	if !outcome.Changed {
		t.Fatal("Changed = false on a first fetch into an empty cache")
	}

	outcome, errRefresh = catalogueRefresh(host, DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh (second): %v", errRefresh)
	}
	if outcome.Changed {
		t.Fatal("Changed = true although the vendor answered with the same ids")
	}

	fake.mu.Lock()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, catalogueBody("sn-kimi-k3", "sn-glm-5-3", "sn-deepseek-v4-1-flash")), nil
	}
	fake.mu.Unlock()
	outcome, errRefresh = catalogueRefresh(host, DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh (third): %v", errRefresh)
	}
	if !outcome.Changed {
		t.Fatalf("Changed = false after the vendor added a model (models=%d)", outcome.Models)
	}
}

// TestCatalogueRefreshRepopulatesTheCache pins that a successful refresh goes
// through the same path a client request would: the cache ends up holding
// exactly what discoverModels produced, so the two cannot disagree.
func TestCatalogueRefreshRepopulatesTheCache(t *testing.T) {
	_, _ = refreshHost(t, catalogueBody("sn-kimi-k3"))
	host := testHost()

	outcome, errRefresh := catalogueRefresh(host, DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh: %v", errRefresh)
	}
	cached, fetchedAt := peekCachedModels()
	if len(cached) != outcome.Models {
		t.Fatalf("cached %d models but reported %d", len(cached), outcome.Models)
	}
	if fetchedAt.IsZero() {
		t.Fatal("the cache carries no fetch time after a refresh")
	}
	if !sameIDSet(catalogueIDSet(cached), []string{"sn-kimi-k3"}) {
		t.Fatalf("cached ids = %v", catalogueIDSet(cached))
	}
}

// TestCatalogueRefreshJSONPublishesAndReports pins the machine-readable route: it
// refreshes, then reports the publish, and the publish is a real write to the
// account's auth file that preserves the credential.
func TestCatalogueRefreshJSONPublishesAndReports(t *testing.T) {
	fake, entry := refreshHost(t, catalogueBody("sn-kimi-k3"))
	host := testHost()

	response := catalogueRefreshJSON(host, pluginapi.ManagementRequest{Query: url.Values{}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %v, want ok (body %s)", body["status"], response.Body)
	}
	if body["published"] != true {
		t.Fatalf("published = %v, want true (body %s)", body["published"], response.Body)
	}
	if body["changed"] != true {
		t.Fatalf("changed = %v, want true on a first refresh", body["changed"])
	}

	fake.mu.Lock()
	saved := fake.saved[entry.Name]
	fake.mu.Unlock()
	if len(saved) == 0 {
		t.Fatalf("host.auth.save calls = 0, want 1: the publish is what makes the host re-register")
	}
	var members map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(saved, &members); errUnmarshal != nil {
		t.Fatalf("the published auth file is not a JSON object: %v", errUnmarshal)
	}
	if _, present := members["model_catalog_published_at"]; !present {
		t.Fatalf("the published auth file carries no change marker: %s", saved)
	}
	// The credential itself must survive the nudge.
	if _, present := members["access_token"]; !present {
		t.Fatalf("the publish dropped the credential: %s", saved)
	}
}

// TestCatalogueRefreshJSONReportsFailure pins that the script route reports a
// failed refresh as an error instead of a success, and that it publishes nothing.
func TestCatalogueRefreshJSONReportsFailure(t *testing.T) {
	fake, _ := refreshHost(t, "")
	host := testHost()

	response := catalogueRefreshJSON(host, pluginapi.ManagementRequest{Query: url.Values{}})
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "error" {
		t.Fatalf("status = %v, want error when the catalogue endpoint fails (body %s)", body["status"], response.Body)
	}
	if names := fake.savedNames(); len(names) != 0 {
		t.Fatalf("a failed refresh published %v", names)
	}
}

// TestStatusPageShowsTheRefreshControl pins the affordance: the status page has
// to carry the manual refresh, and the page it renders must be a GET link.
func TestStatusPageShowsTheRefreshControl(t *testing.T) {
	_, _ = refreshHost(t, catalogueBody("sn-kimi-k3"))
	body := string(renderStatusPage(testHost(), pluginapi.ManagementRequest{Query: url.Values{}}).Body)
	for _, want := range []string{"刷新目录", "action=refresh-catalog", "后台自动刷新"} {
		if !strings.Contains(body, want) {
			t.Errorf("the status page is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestRefreshPageRendersTheOutcome pins that the manual button's page states the
// result instead of merely responding.
func TestRefreshPageRendersTheOutcome(t *testing.T) {
	_, _ = refreshHost(t, catalogueBody("sn-kimi-k3"))
	body := string(catalogueRefreshPage(testHost(), pluginapi.ManagementRequest{Query: url.Values{}}).Body)
	for _, want := range []string{"刷新模型目录", "目录已更新", "已通知宿主重新注册"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refresh page is missing %q:\n%s", want, body)
		}
	}
}

// TestRefreshPageReportsAFailureHonestly pins the same page on the failure path.
func TestRefreshPageReportsAFailureHonestly(t *testing.T) {
	_, _ = refreshHost(t, "")
	body := string(catalogueRefreshPage(testHost(), pluginapi.ManagementRequest{Query: url.Values{}}).Body)
	if !strings.Contains(body, "刷新失败") {
		t.Fatalf("a failed refresh page does not say it failed:\n%s", body)
	}
}

// TestAutoRefreshTextReportsTheOffState pins the default: the background refresh
// is opt-in and the page says so.
//
// ⚠️ The enabled wording is asserted POSITIVELY, and it deliberately does NOT say
// "no credential writes": the automatic path publishes whenever the catalogue
// changes, and claiming otherwise on the page would be a lie the operator would
// only discover by watching the auth file. What the page must promise is the
// narrower, true thing — a stable catalogue costs nothing.
func TestAutoRefreshTextReportsTheOffState(t *testing.T) {
	if got := autoRefreshText(DefaultConfig()); !strings.Contains(got, "已关闭") {
		t.Fatalf("autoRefreshText(default) = %q, want the disabled state", got)
	}
	enabled := DefaultConfig()
	enabled.ModelRefreshMS = 60_000
	got := autoRefreshText(enabled)
	if !strings.Contains(got, "1m0s") {
		t.Fatalf("autoRefreshText(enabled) = %q, want the interval", got)
	}
	if !strings.Contains(got, "仅目录变化时通知宿主重新注册") || !strings.Contains(got, "稳定期零写入") {
		t.Fatalf("autoRefreshText(enabled) = %q, want the publish-on-change contract", got)
	}
	if strings.Contains(got, "不写凭据文件") {
		t.Fatalf("autoRefreshText(enabled) = %q still promises no credential write, "+
			"but the automatic path publishes on change", got)
	}
}

// TestConfigureStartsAndStopsTheScheduler pins the lifecycle: Configure with an
// interval arms a loop that really ticks, and Quiesce/Shutdown end it. A leak
// here would accumulate one loop per config reload.
func TestConfigureStartsAndStopsTheScheduler(t *testing.T) {
	previous := settings()
	t.Cleanup(func() {
		setSettings(previous)
		stopCatalogueScheduler()
		catalogueScheduler.SetInterval(0)
	})
	_ = newFakeHost()
	fake := newFakeHost()
	fake.install(t)
	storedCredential(t, fake, "idx-1", "raccoon-alice.json", sampleCredential(t))
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, catalogueBody("sn-kimi-k3")), nil
	}
	plugin := Plugin().(*plugin)

	if errConfigure := plugin.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	// The run tally describes one armed loop, and the scheduler is a
	// package-level singleton: without clearing it, a tick from another test in
	// the same binary reads as "this loop already ran" (fails under -shuffle).
	catalogueScheduler.ResetCounters()
	if _, runs, _, _ := catalogueScheduler.Status(); runs != 0 {
		t.Fatalf("runs = %d before the first tick", runs)
	}
	if !waitForSchedulerRuns(2 * time.Second) {
		t.Fatal("Configure did not arm a running background refresh")
	}

	plugin.Quiesce()
	time.Sleep(80 * time.Millisecond)
	_, settled, _, _ := catalogueScheduler.Status()
	time.Sleep(120 * time.Millisecond)
	if _, runs, _, _ := catalogueScheduler.Status(); runs != settled {
		t.Fatalf("the background refresh still ticked after Quiesce (%d -> %d)", settled, runs)
	}

	// Shutdown is the other exit, and the default (0) must stay disarmed.
	if errConfigure := plugin.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if !waitForSchedulerRuns(2 * time.Second) {
		t.Fatal("a reconfigure did not restart the background refresh")
	}
	plugin.Shutdown()
	time.Sleep(80 * time.Millisecond)
	_, settled, _, _ = catalogueScheduler.Status()
	time.Sleep(120 * time.Millisecond)
	if _, runs, _, _ := catalogueScheduler.Status(); runs != settled {
		t.Fatalf("the background refresh still ticked after Shutdown (%d -> %d)", settled, runs)
	}

	if errConfigure := plugin.Configure(nil); errConfigure != nil {
		t.Fatalf("Configure(nil): %v", errConfigure)
	}
	if catalogueScheduler.Enabled() {
		t.Fatal("the default configuration armed the background refresh")
	}
	if _, runs, _, _ := catalogueScheduler.Status(); runs != settled {
		t.Fatalf("the refresh ticked with model_refresh_ms = 0 (%d -> %d)", settled, runs)
	}
}

// waitForSchedulerRuns waits until the scheduler's run counter has moved at least
// twice, so the caller knows a loop — not a single stray tick — is running.
func waitForSchedulerRuns(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, runs, _, _ := catalogueScheduler.Status(); runs >= 2 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestCatalogRouteIsDeclaredAndReachable is the regression guard for a trap the
// reference implementation walks into: the host dispatches ONLY management routes
// a plugin declares, so a `/catalog` case in the dispatcher is dead code unless
// the path is also registered. Asserting both halves here means the script route
// cannot silently stop working.
func TestCatalogRouteIsDeclaredAndReachable(t *testing.T) {
	value, errRegister := handleManagementRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("management.register: %v", errRegister)
	}
	registration := value.(pluginapi.ManagementRegistrationResponse)
	declared := false
	for _, route := range registration.Routes {
		if route.Path == "/"+ProviderKey+"/catalog" {
			declared = true
		}
	}
	if !declared {
		t.Fatalf("the /%s/catalog route is not declared: the host only dispatches declared routes", ProviderKey)
	}

	fake, _ := refreshHost(t, catalogueBody("sn-kimi-k3"))
	_ = fake
	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/management/"+ProviderKey+"/catalog", url.Values{}, nil))
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "ok" || body["published"] != true {
		t.Fatalf("the declared route did not perform the refresh: %s", response.Body)
	}
}

// TestAutomaticTickPublishesOnlyWhenTheCatalogueMoved drives the REAL wiring —
// Configure -> startCatalogueScheduler -> the shared scheduler — and observes
// auth-file writes, rather than asserting on a Request literal built here.
//
// That distinction matters: a test that constructs its own PublishOnChange would
// stay green even if the production wiring dropped the flag, which is exactly the
// regression this has to catch. A stable catalogue must cost zero writes after
// the initial one, and the initial one is what makes `/v1/models` follow.
func TestAutomaticTickPublishesOnlyWhenTheCatalogueMoved(t *testing.T) {
	fake, _ := refreshHost(t, catalogueBody("sn-kimi-k3"))
	plugin := Plugin().(*plugin)

	previous := settings()
	t.Cleanup(func() {
		setSettings(previous)
		stopCatalogueScheduler()
		catalogueScheduler.SetInterval(0)
	})

	if errConfigure := plugin.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if !waitForSchedulerRuns(2 * time.Second) {
		t.Fatal("the background refresh never ticked")
	}
	// Let a few more ticks land so "stable means silent" is really exercised.
	time.Sleep(150 * time.Millisecond)
	stopCatalogueScheduler()

	fake.mu.Lock()
	writes := len(fake.saved)
	fake.mu.Unlock()
	if writes != 1 {
		t.Fatalf("auth-file writes = %d, want exactly 1: the first tick publishes the moved "+
			"catalogue, every later tick must stay silent while it does not move", writes)
	}
}
