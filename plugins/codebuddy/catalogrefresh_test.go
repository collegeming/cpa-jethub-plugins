package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	catalogue "github.com/collegeming/cpa-jethub-plugins/internal/jethub/catalog"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file pins the catalogue-refresh contract of the CodeBuddy family: the
// manual 刷新目录 button refreshes through the same discovery path a client
// request uses, publishes the result so the host re-registers this provider's
// models, and reports an unreachable vendor as a FAILURE rather than as the
// built-in fallback table.

// refreshHost is a fake host for the refresh tests: one account, one scripted
// model listing, and a record of every auth save.
type refreshHost struct {
	mu sync.Mutex
	// models is the id list the enterprise model endpoint answers with. An empty
	// list makes the endpoint answer with nothing, which is how an unreachable
	// vendor is simulated: fetchRemoteModels then returns nil and the caller
	// falls back.
	models []string
	// failTransport makes every upstream call fail at the transport level.
	failTransport bool
	// agentModels are models the vendor marks as agent-referenced. They survive
	// reconcileWithFallback's whitelist and are therefore the way a remote
	// listing can actually move the PUBLISHED catalogue.
	agentModels []string
	// files is the credential listing.
	files []pluginapi.HostAuthFileEntry
	// authJSON is the credential body host.auth.get returns per index.
	authJSON map[string]string
	saved    []savedAuthRecord
}

type savedAuthRecord struct {
	Name string
	JSON []byte
}

func newRefreshHost(t *testing.T) *refreshHost {
	t.Helper()
	fixture := &refreshHost{authJSON: map[string]string{}}
	credential := &Credential{
		Type:         ProviderKey,
		AccessToken:  "REFRESH-TOKEN",
		RefreshToken: "refresh",
		UserID:       "u-refresh",
		Nickname:     "refresh",
		Product:      ProductCodeBuddy,
	}
	raw, errMarshal := json.Marshal(credential)
	if errMarshal != nil {
		t.Fatalf("encode credential: %v", errMarshal)
	}
	fixture.authJSON["idx-1"] = string(raw)
	fixture.files = []pluginapi.HostAuthFileEntry{{
		Provider: ProviderKey, Type: ProviderKey, AuthIndex: "idx-1", Name: "codebuddy-refresh.json",
	}}
	return fixture
}

// install wires the fixture into abiboot and returns the host handle.
func (f *refreshHost) install(t *testing.T) *abiboot.Host {
	t.Helper()
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case pluginabi.MethodHostHTTPDo:
			if f.failTransport {
				return nil, fmt.Errorf("upstream unreachable")
			}
			var payload struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(request, &payload)
			if !strings.Contains(payload.URL, "/models") {
				return nil, fmt.Errorf("unexpected upstream call %s", payload.URL)
			}
			return abiboot.OK(map[string]any{
				"StatusCode": http.StatusOK,
				"Body":       []byte(listingBody(f.models, f.agentModels)),
			})
		case pluginabi.MethodHostAuthList:
			return abiboot.OK(map[string]any{"files": f.files})
		case pluginabi.MethodHostAuthGet:
			var payload pluginapi.HostAuthGetRequest
			_ = json.Unmarshal(request, &payload)
			raw := f.authJSON[payload.AuthIndex]
			if raw == "" {
				return nil, fmt.Errorf("auth index %s not found", payload.AuthIndex)
			}
			return abiboot.OK(map[string]any{"auth_index": payload.AuthIndex, "json": json.RawMessage(raw)})
		case pluginabi.MethodHostAuthSave:
			var payload pluginapi.HostAuthSaveRequest
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.saved = append(f.saved, savedAuthRecord{Name: payload.Name, JSON: payload.JSON})
			return abiboot.OK(map[string]any{"name": payload.Name, "path": "/tmp/" + payload.Name})
		case pluginabi.MethodHostLog:
			return abiboot.OK(map[string]any{})
		default:
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
	})
	t.Cleanup(func() {
		abiboot.ClearHostCaller()
		discoveredModels.reset()
	})
	discoveredModels.reset()
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"refresh-callback"}`))
}

// listingBody renders the enterprise model listing for the given ids.
//
// agentID, when non-empty, is additionally listed under the `cli` agent. The
// published catalogue is reconciled against the product's built-in table with
// whitelist semantics, so an agent-referenced id is the one shape of remote
// addition that actually reaches the published list.
func listingBody(ids []string, agentModels []string) string {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, fmt.Sprintf(`{"id":%q,"name":%q}`, id, id))
	}
	agents := ""
	if len(agentModels) > 0 {
		referenced := make([]string, 0, len(agentModels))
		for _, id := range agentModels {
			referenced = append(referenced, fmt.Sprintf("%q", id))
		}
		agents = `,"agents":[{"name":"cli","models":[` + strings.Join(referenced, ",") + `]}]`
	}
	return `{"data":{"models":[` + strings.Join(entries, ",") + `]` + agents + `}}`
}

// savedRecords returns a copy of every auth save.
func (f *refreshHost) savedRecords() []savedAuthRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]savedAuthRecord(nil), f.saved...)
}

// setModels replaces the vendor's listing.
func (f *refreshHost) setModels(ids ...string) {
	f.mu.Lock()
	f.models = ids
	f.mu.Unlock()
}

// setAgentModels replaces the agent-referenced portion of the listing. Those
// entries bypass the built-in whitelist, so they move the published catalogue.
func (f *refreshHost) setAgentModels(ids ...string) {
	f.mu.Lock()
	f.agentModels = ids
	f.mu.Unlock()
}

// TestCatalogueRefreshReportsUpstreamFailureAsFailure is the honesty guard: with
// the vendor unreachable, discoveredCatalogFor hands back the built-in fallback
// table. That fallback is NOT a successful refresh, and reporting it as one
// would tell the operator the vendor agreed when it never answered.
func TestCatalogueRefreshReportsUpstreamFailureAsFailure(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	fixture := newRefreshHost(t)
	fixture.failTransport = true
	handle := fixture.install(t)

	_, errRefresh := catalogueRefresh(handle, settings())
	if errRefresh == nil {
		t.Fatal("a refresh whose vendor call failed was reported as a success")
	}
	if _, _, ok := discoveredModels.peek(cachedCatalogKey(ProductDefault())); ok {
		t.Fatal("a failed refresh cached the fallback table; a client request must not be served it as remote data")
	}
}

// TestCatalogueRefreshReportsNoAccount covers the second way a refresh cannot
// run: there is nothing to fetch with.
func TestCatalogueRefreshReportsNoAccount(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	fixture := newRefreshHost(t)
	fixture.files = nil
	handle := fixture.install(t)

	if _, errRefresh := catalogueRefresh(handle, settings()); errRefresh == nil {
		t.Fatal("a refresh without any account was reported as a success")
	}
}

// TestCatalogueRefreshReportsDisabledDiscovery pins that a catalogue pinned to
// the built-in table is not silently "refreshed".
func TestCatalogueRefreshReportsDisabledDiscovery(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	cfg := DefaultConfig()
	cfg.DiscoverModels = false
	setSettings(cfg)

	handle := newRefreshHost(t).install(t)
	if _, errRefresh := catalogueRefresh(handle, cfg); errRefresh == nil {
		t.Fatal("a refresh with discovery disabled was reported as a success")
	}
}

// TestCatalogueRefreshComputesChanged pins Changed in both directions: a first
// fetch differs from the empty cache, an identical second fetch does not, and a
// grown listing does again.
func TestCatalogueRefreshComputesChanged(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	fixture := newRefreshHost(t)
	fixture.setModels("glm-5.3", "deepseek-v4-pro")
	handle := fixture.install(t)

	outcome, errRefresh := catalogueRefresh(handle, settings())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh: %v", errRefresh)
	}
	if !outcome.Changed {
		t.Fatal("Changed = false on a first fetch into an empty cache")
	}
	first := outcome.Models
	if first == 0 {
		t.Fatal("the refresh reported zero models although the vendor listed two")
	}

	outcome, errRefresh = catalogueRefresh(handle, settings())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh (second): %v", errRefresh)
	}
	if outcome.Changed {
		t.Fatal("Changed = true although the vendor answered with the same ids")
	}
	if outcome.Models != first {
		t.Fatalf("Models = %d after an identical fetch, want %d", outcome.Models, first)
	}

	fixture.setAgentModels("glm-5.3-agent-experimental")
	outcome, errRefresh = catalogueRefresh(handle, settings())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh (third): %v", errRefresh)
	}
	if !outcome.Changed {
		t.Fatal("Changed = false after the vendor added an agent-referenced model")
	}
	if outcome.Models != first+1 {
		t.Fatalf("Models = %d, want %d after one addition", outcome.Models, first+1)
	}
}

// TestCatalogueRefreshRepopulatesTheCache pins that a successful refresh goes
// through the same path a client request would: the cache ends up holding what
// discoveredCatalogFor produced.
func TestCatalogueRefreshRepopulatesTheCache(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	fixture := newRefreshHost(t)
	fixture.setModels("glm-5.3", "deepseek-v4-pro")
	handle := fixture.install(t)

	outcome, errRefresh := catalogueRefresh(handle, settings())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh: %v", errRefresh)
	}
	cached, fetchedAt, ok := discoveredModels.peek(cachedCatalogKey(ProductDefault()))
	if !ok {
		t.Fatal("a successful refresh left the cache empty")
	}
	if len(cached) != outcome.Models {
		t.Fatalf("cached %d models but reported %d", len(cached), outcome.Models)
	}
	if fetchedAt.IsZero() {
		t.Fatal("the cache carries no fetch time after a refresh")
	}
}

// TestCatalogueRefreshJSONPublishesAndReports pins the machine-readable route:
// it refreshes, reports the publish, and the publish is a real write carrying
// the change marker while the credential survives.
func TestCatalogueRefreshJSONPublishesAndReports(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	fixture := newRefreshHost(t)
	fixture.setModels("glm-5.3", "deepseek-v4-pro")
	handle := fixture.install(t)

	response := catalogueRefreshJSON(handle, pluginapi.ManagementRequest{Query: map[string][]string{}})
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

	saved := fixture.savedRecords()
	if len(saved) != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1: the publish is what makes the host re-register", len(saved))
	}
	if saved[0].Name != "codebuddy-refresh.json" {
		t.Fatalf("saved %q, want the account's own file", saved[0].Name)
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
// failed refresh as an error and does not publish: the host would otherwise
// re-register an unchanged catalogue.
func TestCatalogueRefreshJSONReportsFailure(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	fixture := newRefreshHost(t)
	fixture.failTransport = true
	handle := fixture.install(t)

	response := catalogueRefreshJSON(handle, pluginapi.ManagementRequest{Query: map[string][]string{}})
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "error" {
		t.Fatalf("status = %v, want error when the vendor is unreachable (body %s)", body["status"], response.Body)
	}
	if saved := fixture.savedRecords(); len(saved) != 0 {
		t.Fatal("a failed refresh must not publish")
	}
}

// TestAutomaticRefreshNeverWritesAnAuthFile pins the split between the two
// layers, at the level of the refresh function itself: `catalogueRefresh` only
// warms the plugin cache and never writes an auth file. Publishing is the
// scheduler's decision (it passes PublishOnChange and the host name is resolved
// per tick) and the manual button's, not this function's.
func TestAutomaticRefreshNeverWritesAnAuthFile(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	fixture := newRefreshHost(t)
	fixture.setModels("glm-5.3")
	handle := fixture.install(t)

	// Two automatic refreshes, driven the way the scheduler drives them: the
	// Request carries no AuthName, so nothing may be published.
	for index := 0; index < 2; index++ {
		outcome, errRefresh := catalogueRefresh(handle, settings())
		if errRefresh != nil {
			t.Fatalf("automatic refresh %d: %v", index, errRefresh)
		}
		if outcome.Models == 0 {
			t.Fatalf("automatic refresh %d warmed no cache", index)
		}
	}
	if saved := fixture.savedRecords(); len(saved) != 0 {
		t.Fatalf("the automatic refresh wrote %d auth file(s); only the manual button may publish", len(saved))
	}

	// The manual path, by contrast, does publish.
	if result := catalogue.Run(catalogue.Request{
		Host: handle, Provider: ProviderKey, AuthName: "codebuddy-refresh.json",
		Refresh: func() (catalogue.Outcome, error) { return catalogueRefresh(handle, settings()) },
	}); !result.Published {
		t.Fatalf("the manual refresh did not publish: %+v", result)
	}
	if saved := fixture.savedRecords(); len(saved) != 1 {
		t.Fatalf("host.auth.save calls = %d after the manual refresh, want 1", len(saved))
	}
}

// TestStatusPageShowsTheRefreshControl pins the affordance and the GET-only
// rule that every resource page obeys.
func TestStatusPageShowsTheRefreshControl(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	handle := newRefreshHost(t).install(t)
	body := string(renderStatusPage(handle, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	for _, want := range []string{"刷新目录", "action=refresh-catalog", "后台自动刷新", "线上目录缓存"} {
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
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	fixture := newRefreshHost(t)
	fixture.setModels("glm-5.3")
	handle := fixture.install(t)

	body := string(catalogueRefreshPage(handle, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	for _, want := range []string{"刷新模型目录", "目录已更新", "已通知宿主重新注册"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refresh page is missing %q:\n%s", want, body)
		}
	}
}

// TestRefreshPageReportsAFailureHonestly pins the same page on the failure path.
func TestRefreshPageReportsAFailureHonestly(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(DefaultConfig())

	fixture := newRefreshHost(t)
	fixture.failTransport = true
	handle := fixture.install(t)

	body := string(catalogueRefreshPage(handle, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	if !strings.Contains(body, "刷新失败") {
		t.Fatalf("a failed refresh page does not say it failed:\n%s", body)
	}
}

// TestAutoRefreshTextReportsTheOffState pins the default: the background refresh
// is opt-in and the page says so.
func TestAutoRefreshTextReportsTheOffState(t *testing.T) {
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
	previous := settings()
	t.Cleanup(func() {
		setSettings(previous)
		stopCatalogueScheduler()
		catalogueScheduler.SetInterval(0)
	})
	_ = newRefreshHost(t).install(t) // the tick needs a host transport, even a minimal one
	plugin := Plugin().(*plugin)

	baseline := scheduledRuns()
	if errConfigure := plugin.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if !waitForScheduledRuns(baseline+2, 2*time.Second) {
		t.Fatal("Configure did not arm a running background refresh")
	}

	plugin.Quiesce()
	time.Sleep(80 * time.Millisecond)
	settled := scheduledRuns()
	time.Sleep(120 * time.Millisecond)
	if runs := scheduledRuns(); runs != settled {
		t.Fatalf("the background refresh still ticked after Quiesce (%d -> %d)", settled, runs)
	}

	if errConfigure := plugin.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if !waitForScheduledRuns(settled+2, 2*time.Second) {
		t.Fatal("a reconfigure did not restart the background refresh")
	}
	plugin.Shutdown()
	time.Sleep(80 * time.Millisecond)
	settled = scheduledRuns()
	time.Sleep(120 * time.Millisecond)
	if runs := scheduledRuns(); runs != settled {
		t.Fatalf("the background refresh still ticked after Shutdown (%d -> %d)", settled, runs)
	}

	if errConfigure := plugin.Configure(nil); errConfigure != nil {
		t.Fatalf("Configure(nil): %v", errConfigure)
	}
	if catalogueScheduler.Enabled() {
		t.Fatal("the default configuration armed the background refresh")
	}
	if runs := scheduledRuns(); runs != settled {
		t.Fatalf("the refresh ticked with model_refresh_ms = 0 (%d -> %d)", settled, runs)
	}
}

// scheduledRuns reads the scheduler's completed-refresh counter.
func scheduledRuns() int {
	_, runs, _, _ := catalogueScheduler.Status()
	return runs
}

// waitForScheduledRuns waits until the scheduler's counter reaches want, so the
// caller knows a loop — not a stray tick — is running.
func waitForScheduledRuns(want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if scheduledRuns() >= want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
