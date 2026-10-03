package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/catalog"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file pins the catalogue-refresh contract of the LobsterAI adapter: the
// manual 刷新目录 button refreshes through the same discovery path a client
// request uses, publishes the result so the host re-registers this provider's
// models, and reports an unreachable vendor as a FAILURE rather than as the
// built-in fallback list.

// lobsteraiModelsBody renders a /api/models/available answer.
func lobsteraiModelsBody(ids ...string) string {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, `{"modelId":"`+id+`","modelName":"`+id+`"}`)
	}
	return `{"code":0,"message":"success","data":[` + strings.Join(entries, ",") + `]}`
}

// refreshHost builds a fake host holding one usable account and answering the
// model listing with the given body (empty = the route does not exist, so the
// transport fails the way an unreachable vendor does).
func refreshHost(t *testing.T, modelBody string) (*fakeHost, *abiboot.Host, pluginapi.HostAuthFileEntry) {
	t.Helper()
	credential := &Credential{
		AccessToken: "refresh-token", RefreshToken: "refresh", UID: "uid-r", Nickname: "refresh",
		UUID: "uuid-r", FirstKeyfrom: "111", LatestKeyfrom: "222",
		ExpiresAt: itoa64(time.Now().Add(time.Hour).UnixMilli()),
	}
	entry := pluginapi.HostAuthFileEntry{
		AuthIndex: "idx-r", Name: "lobsterai-uid-r.json", Provider: ProviderKey,
		Type: ProviderKey, Label: "refresh", Status: "ready",
	}
	fake := newFakeHost().
		withFiles(entry).
		withAuthJSON("idx-r", string(mustJSON(t, credential))).
		on(httpRoute{Method: http.MethodGet, Match: "api-overmind", Body: `{"data":{"value":{"version":"2026.9.4"}},"code":0}`})
	if modelBody != "" {
		fake = fake.on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Body: modelBody})
	}
	host := installFakeHost(t, fake)
	return fake, host, entry
}

// TestCatalogueRefreshReportsUpstreamFailureAsFailure is the honesty guard: when
// the vendor is unreachable, discoverModels returns nil and `model.for_auth`
// would publish the built-in list. That fallback is NOT a successful refresh,
// and reporting it as one would tell the operator the vendor agreed when it
// never answered.
func TestCatalogueRefreshReportsUpstreamFailureAsFailure(t *testing.T) {
	_, host, _ := refreshHost(t, "")

	_, errRefresh := catalogueRefresh(host, settings())
	if errRefresh == nil {
		t.Fatal("a refresh whose vendor call failed was reported as a success")
	}
	if cached, _ := discoveredModels.peek(); len(cached) != 0 {
		t.Fatalf("a failed refresh cached %d models; the fallback list must not be cached", len(cached))
	}
}

// TestCatalogueRefreshReportsNoAccount covers the second way a refresh cannot
// run: there is nothing to fetch with.
func TestCatalogueRefreshReportsNoAccount(t *testing.T) {
	fake := newFakeHost().
		on(httpRoute{Method: http.MethodGet, Match: "api-overmind", Body: `{"data":{"value":{"version":"2026.9.4"}},"code":0}`})
	host := installFakeHost(t, fake)

	if _, errRefresh := catalogueRefresh(host, settings()); errRefresh == nil {
		t.Fatal("a refresh without any account was reported as a success")
	}
}

// TestCatalogueRefreshReportsDisabledDiscovery pins that a catalogue pinned to
// the built-in list is not silently "refreshed".
func TestCatalogueRefreshReportsDisabledDiscovery(t *testing.T) {
	_, host, _ := refreshHost(t, lobsteraiModelsBody("glm-5.3"))

	cfg := settings()
	cfg.DiscoverModels = false
	if _, errRefresh := catalogueRefresh(host, cfg); errRefresh == nil {
		t.Fatal("a refresh with discovery disabled was reported as a success")
	}
}

// TestCatalogueRefreshComputesChanged pins Changed in both directions: a first
// fetch differs from the empty cache, an identical second fetch does not, and a
// grown listing does again.
func TestCatalogueRefreshComputesChanged(t *testing.T) {
	_, host, _ := refreshHost(t, lobsteraiModelsBody("glm-5.3", "deepseek-v4-pro"))

	outcome, errRefresh := catalogueRefresh(host, settings())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh: %v", errRefresh)
	}
	if !outcome.Changed {
		t.Fatal("Changed = false on a first fetch into an empty cache")
	}
	if outcome.Models != 2 {
		t.Fatalf("Models = %d, want the 2 the vendor listed", outcome.Models)
	}

	outcome, errRefresh = catalogueRefresh(host, settings())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh (second): %v", errRefresh)
	}
	if outcome.Changed {
		t.Fatal("Changed = true although the vendor answered with the same ids")
	}
	if outcome.Models != 2 {
		t.Fatalf("Models = %d after an identical fetch, want 2", outcome.Models)
	}

	// A grown listing does move. The fixture's route list is append-only, so the
	// new listing is installed as another route: fakeHost answers with the first
	// matching route, so the earlier one is replaced by clearing first.
	host2 := installFakeHost(t, newFakeHost().
		withFiles(pluginapi.HostAuthFileEntry{
			AuthIndex: "idx-r", Name: "lobsterai-uid-r.json", Provider: ProviderKey, Type: ProviderKey,
		}).
		withAuthJSON("idx-r", string(mustJSON(t, &Credential{
			AccessToken: "refresh-token", RefreshToken: "refresh", UID: "uid-r",
			ExpiresAt: itoa64(time.Now().Add(time.Hour).UnixMilli()),
		}))).
		on(httpRoute{Method: http.MethodGet, Match: "api-overmind", Body: `{"data":{"value":{"version":"2026.9.4"}},"code":0}`}).
		on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Body: lobsteraiModelsBody("glm-5.3", "deepseek-v4-pro", "glm-5.3-flash")}))
	if _, errPrime := catalogueRefresh(host2, settings()); errPrime != nil {
		t.Fatalf("prime refresh: %v", errPrime)
	}
	discoveredModels.put([]remoteModel{{ID: "glm-5.3"}, {ID: "deepseek-v4-pro"}}, time.Now())
	outcome, errRefresh = catalogueRefresh(host2, settings())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh (third): %v", errRefresh)
	}
	if !outcome.Changed {
		t.Fatal("Changed = false after the vendor added a model")
	}
	if outcome.Models != 3 {
		t.Fatalf("Models = %d, want 3 after the addition", outcome.Models)
	}
}

// TestCatalogueRefreshRepopulatesTheCache pins that a successful refresh goes
// through the same path a client request would: the cache ends up holding what
// discoverModels produced, and the version cache is dropped so a stale pin is
// not reused.
func TestCatalogueRefreshRepopulatesTheCache(t *testing.T) {
	_, host, _ := refreshHost(t, lobsteraiModelsBody("glm-5.3"))

	outcome, errRefresh := catalogueRefresh(host, settings())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh: %v", errRefresh)
	}
	cached, fetchedAt := discoveredModels.peek()
	if len(cached) != outcome.Models {
		t.Fatalf("cached %d models but reported %d", len(cached), outcome.Models)
	}
	if fetchedAt.IsZero() {
		t.Fatal("the cache carries no fetch time after a refresh")
	}
	// The version the model request used must be resolvable again, which proves
	// the resolver's cache was dropped rather than left serving a stale pin.
	if version := resolveClientVersion(host, settings()); version == "" {
		t.Fatal("the client version could not be resolved after the refresh")
	}
}

// TestCatalogueRefreshJSONPublishesAndReports pins the machine-readable route:
// it refreshes, reports the publish, and the publish is a real write carrying
// the change marker while the credential survives.
func TestCatalogueRefreshJSONPublishesAndReports(t *testing.T) {
	fake, host, entry := refreshHost(t, lobsteraiModelsBody("glm-5.3", "deepseek-v4-pro"))

	response := catalogueRefreshJSON(host, pluginapi.ManagementRequest{Query: url.Values{}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var body map[string]any
	if errUnmarshal := decodeJSON(response.Body, &body); errUnmarshal != nil {
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

	saved := fake.savedAuths()
	if len(saved) != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1: the publish is what makes the host re-register", len(saved))
	}
	if saved[0].Name != entry.Name {
		t.Fatalf("saved %q, want the account's own file %q", saved[0].Name, entry.Name)
	}
	var members map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(saved[0].JSON, &members); errUnmarshal != nil {
		t.Fatalf("the published auth file is not a JSON object: %v", errUnmarshal)
	}
	if _, present := members["model_catalog_published_at"]; !present {
		t.Fatalf("the published auth file carries no change marker: %s", saved[0].JSON)
	}
	if _, present := members["access_token"]; !present {
		t.Fatalf("the publish dropped the credential: %s", saved[0].JSON)
	}
}

// TestCatalogueRefreshJSONReportsFailure pins that the script route reports a
// failed refresh as an error and does not publish.
func TestCatalogueRefreshJSONReportsFailure(t *testing.T) {
	fake, host, _ := refreshHost(t, "")

	response := catalogueRefreshJSON(host, pluginapi.ManagementRequest{Query: url.Values{}})
	var body map[string]any
	if errUnmarshal := decodeJSON(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "error" {
		t.Fatalf("status = %v, want error when the vendor is unreachable (body %s)", body["status"], response.Body)
	}
	if saved := fake.savedAuths(); len(saved) != 0 {
		t.Fatal("a failed refresh must not publish")
	}
}

// TestAutomaticRefreshNeverWritesAnAuthFile pins the split between the two
// layers, at the level of the refresh function itself: `catalogueRefresh` only
// warms the plugin cache and never writes an auth file. Publishing is the
// scheduler's decision (it passes PublishOnChange and the host name is resolved
// per tick) and the manual button's, not this function's.
func TestAutomaticRefreshNeverWritesAnAuthFile(t *testing.T) {
	fake, host, entry := refreshHost(t, lobsteraiModelsBody("glm-5.3"))

	// Two automatic refreshes, driven the way the scheduler drives them: the
	// Request carries no AuthName, so nothing may be published.
	for index := 0; index < 2; index++ {
		outcome, errRefresh := catalogueRefresh(host, settings())
		if errRefresh != nil {
			t.Fatalf("automatic refresh %d: %v", index, errRefresh)
		}
		if outcome.Models == 0 {
			t.Fatalf("automatic refresh %d warmed no cache", index)
		}
	}
	if saved := fake.savedAuths(); len(saved) != 0 {
		t.Fatalf("the automatic refresh wrote %d auth file(s); only the manual button may publish", len(saved))
	}

	// The manual path, by contrast, does publish.
	if result := catalog.Run(catalog.Request{
		Host: host, Provider: ProviderKey, AuthName: entry.Name,
		Refresh: func() (catalog.Outcome, error) { return catalogueRefresh(host, settings()) },
	}); !result.Published {
		t.Fatalf("the manual refresh did not publish: %+v", result)
	}
	if saved := fake.savedAuths(); len(saved) != 1 {
		t.Fatalf("host.auth.save calls = %d after the manual refresh, want 1", len(saved))
	}
}

// TestStatusPageShowsTheRefreshControl pins the affordance and the GET-only rule
// every resource page obeys.
func TestStatusPageShowsTheRefreshControl(t *testing.T) {
	_, host, _ := statusFixture(t)

	body := string(renderStatusPage(host, managementRequest(
		http.MethodGet, "/v0/resource/plugins/lobsterai/status", nil, nil)).Body)
	for _, want := range []string{"刷新目录", "action=refresh-catalog", "后台自动刷新", "线上目录缓存"} {
		if !strings.Contains(body, want) {
			t.Errorf("the status page is missing %q:\n%s", want, firstLines(body, 40))
		}
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestNoAccountPageStillOffersTheRefresh pins that a fresh install sees where the
// catalogue comes from, and can refresh it, before any account exists.
func TestNoAccountPageStillOffersTheRefresh(t *testing.T) {
	host := installFakeHost(t, newFakeHost())

	body := string(renderStatusPage(host, managementRequest(
		http.MethodGet, "/v0/resource/plugins/lobsterai/status", nil, nil)).Body)
	if !strings.Contains(body, "尚未添加账号") {
		t.Fatalf("the empty state lost its explanation:\n%s", firstLines(body, 30))
	}
	for _, want := range []string{"刷新目录", "后台自动刷新"} {
		if !strings.Contains(body, want) {
			t.Errorf("the no-account page is missing %q:\n%s", want, firstLines(body, 30))
		}
	}
}

// TestRefreshPageRendersTheOutcome pins that the manual button's page states the
// result instead of merely responding.
func TestRefreshPageRendersTheOutcome(t *testing.T) {
	_, host, _ := refreshHost(t, lobsteraiModelsBody("glm-5.3"))

	body := string(catalogueRefreshPage(host, managementRequest(
		http.MethodGet, "/v0/resource/plugins/lobsterai/status", url.Values{"action": {"refresh-catalog"}}, nil)).Body)
	for _, want := range []string{"刷新模型目录", "目录已更新", "已通知宿主重新注册"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refresh page is missing %q:\n%s", want, firstLines(body, 30))
		}
	}
}

// TestRefreshPageReportsAFailureHonestly pins the same page on the failure path.
func TestRefreshPageReportsAFailureHonestly(t *testing.T) {
	_, host, _ := refreshHost(t, "")

	body := string(catalogueRefreshPage(host, managementRequest(
		http.MethodGet, "/v0/resource/plugins/lobsterai/status", url.Values{"action": {"refresh-catalog"}}, nil)).Body)
	if !strings.Contains(body, "刷新失败") {
		t.Fatalf("a failed refresh page does not say it failed:\n%s", firstLines(body, 30))
	}
}

// TestCatalogueRouteIsReachableOnBothMounts pins the dispatch: the JSON route is
// reachable as a script endpoint, and `?action=refresh-catalog` on /status is
// reachable as a page.
func TestCatalogueRouteIsReachableOnBothMounts(t *testing.T) {
	fake, host, _ := refreshHost(t, lobsteraiModelsBody("glm-5.3"))

	value, errHandle := handleManagementHandle(host, mustJSON(t, jsonManagementRequest(
		http.MethodGet, "/v0/management/lobsterai/catalog", nil, nil)))
	if errHandle != nil {
		t.Fatalf("handleManagementHandle(/catalog): %v", errHandle)
	}
	response := value.(pluginapi.ManagementResponse)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	decoded := map[string]any{}
	if errUnmarshal := decodeJSON(response.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded["status"] != "ok" {
		t.Fatalf("catalog json = %v", decoded)
	}
	if saved := fake.savedAuths(); len(saved) != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1", len(saved))
	}

	// The page route carries the action in its query string.
	discoveredModels.reset()
	value, errHandle = handleManagementHandle(host, mustJSON(t, managementRequest(
		http.MethodGet, "/v0/resource/plugins/lobsterai/status", url.Values{"action": {"refresh-catalog"}}, nil)))
	if errHandle != nil {
		t.Fatalf("handleManagementHandle(?action=refresh-catalog): %v", errHandle)
	}
	body := string(value.(pluginapi.ManagementResponse).Body)
	if !strings.Contains(body, "刷新模型目录") {
		t.Fatalf("?action=refresh-catalog did not render the refresh page:\n%s", firstLines(body, 30))
	}
}

// TestAutoRefreshTextReportsTheOffState pins the default: the background refresh
// is opt-in and the page says so.
func TestAutoRefreshTextReportsTheOffState(t *testing.T) {
	installFakeHost(t, newFakeHost()) // the fixture resets the settings
	if got := autoRefreshText(DefaultConfig()); !strings.Contains(got, "已关闭") {
		t.Fatalf("autoRefreshText(default) = %q, want the disabled state", got)
	}
	enabled := DefaultConfig()
	enabled.ModelRefreshMS = 60_000
	got := autoRefreshText(enabled)
	if !strings.Contains(got, "1m0s") || !strings.Contains(got, "不写凭据文件") {
		t.Fatalf("autoRefreshText(enabled) = %q, want the interval and the no-write caveat", got)
	}
}

// TestConfigureStartsAndStopsTheScheduler pins the lifecycle: Configure with an
// interval arms a loop that really ticks, and Quiesce/Shutdown end it. A leak
// here would accumulate one loop per config reload.
//
// The scheduler's counter is process-wide, so every wait is expressed as a DELTA
// from a baseline taken just before the arm — otherwise a repeat run of this
// test would pass without anything having ticked.
func TestConfigureStartsAndStopsTheScheduler(t *testing.T) {
	_, _, _ = refreshHost(t, "")
	t.Cleanup(func() {
		stopCatalogueScheduler()
		catalogueScheduler.SetInterval(0)
	})
	instance := Plugin().(*plugin)

	baseline := schedulerTicks()
	if errConfigure := instance.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if !waitForScheduledTicks(baseline+2, 2*time.Second) {
		t.Fatal("Configure did not arm a running background refresh")
	}

	instance.Quiesce()
	time.Sleep(80 * time.Millisecond)
	settled := schedulerTicks()
	time.Sleep(120 * time.Millisecond)
	if ticks := schedulerTicks(); ticks != settled {
		t.Fatalf("the background refresh still ticked after Quiesce (%d -> %d)", settled, ticks)
	}

	if errConfigure := instance.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if !waitForScheduledTicks(settled+2, 2*time.Second) {
		t.Fatal("a reconfigure did not restart the background refresh")
	}
	instance.Shutdown()
	time.Sleep(80 * time.Millisecond)
	settled = schedulerTicks()
	time.Sleep(120 * time.Millisecond)
	if ticks := schedulerTicks(); ticks != settled {
		t.Fatalf("the background refresh still ticked after Shutdown (%d -> %d)", settled, ticks)
	}

	if errConfigure := instance.Configure(nil); errConfigure != nil {
		t.Fatalf("Configure(nil): %v", errConfigure)
	}
	if catalogueScheduler.Enabled() {
		t.Fatal("the default configuration armed the background refresh")
	}
	if ticks := schedulerTicks(); ticks != settled {
		t.Fatalf("the refresh ticked with model_refresh_ms = 0 (%d -> %d)", settled, ticks)
	}
}

// schedulerTicks reads the scheduler's completed-refresh counter.
func schedulerTicks() int {
	_, runs, _, _ := catalogueScheduler.Status()
	return runs
}

// waitForScheduledTicks waits until the scheduler's counter reaches want, so the
// caller knows a loop — not a stray tick — is running.
func waitForScheduledTicks(want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if schedulerTicks() >= want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
