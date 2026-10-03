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
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/catalog"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// refreshHost is a fake host for the catalogue-refresh tests. It answers the two
// signed listing endpoints (or fails them), the credential listing, the auth get
// and the auth save, and records what came back.
type refreshHost struct {
	mu sync.Mutex
	// gateway/builtin are the response bodies of the two listing endpoints. An
	// empty value means "the transport fails", which is how an unreachable
	// vendor is simulated.
	gateway string
	builtin string
	// files is the credential listing.
	files []pluginapi.HostAuthFileEntry
	// authJSON is the credential body host.auth.get returns per index.
	authJSON map[string]string
	saved    []savedAuthPayload
	logs     []string
}

type savedAuthPayload struct {
	Name string
	JSON []byte
}

func newRefreshHost(files ...pluginapi.HostAuthFileEntry) *refreshHost {
	return &refreshHost{files: files, authJSON: map[string]string{}}
}

func (f *refreshHost) install(t *testing.T) *abiboot.Host {
	t.Helper()
	credentialRefresher = newCredentialRefresher()
	t.Cleanup(func() {
		credentialRefresher = newCredentialRefresher()
		discoveredModels.reset()
	})

	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case pluginabi.MethodHostHTTPDo:
			var payload struct {
				URL string `json:"url"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			var body string
			switch {
			case strings.Contains(payload.URL, OpenGWGatewayConfigURL):
				body = f.gateway
			case strings.Contains(payload.URL, SnapModelBuiltinURL):
				body = f.builtin
			default:
				return nil, fmt.Errorf("unexpected upstream call %s", payload.URL)
			}
			if body == "" {
				return nil, fmt.Errorf("upstream unreachable")
			}
			return abiboot.OK(map[string]any{
				"StatusCode": http.StatusOK,
				"Body":       []byte(body),
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
			f.saved = append(f.saved, savedAuthPayload{Name: payload.Name, JSON: payload.JSON})
			return abiboot.OK(map[string]any{"name": payload.Name, "path": "/tmp/" + payload.Name})
		case pluginabi.MethodHostLog:
			f.logs = append(f.logs, string(request))
			return abiboot.OK(map[string]any{})
		default:
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
	})
	t.Cleanup(abiboot.ClearHostCaller)
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"refresh-callback"}`))
}

// authFileFor registers a complete signed credential under one index and returns
// the listing entry that points at it.
func (f *refreshHost) authFileFor(t *testing.T, index, name string) pluginapi.HostAuthFileEntry {
	t.Helper()
	raw, errMarshal := json.Marshal(&Credential{
		Type:            ProviderKey,
		AccessKeyID:     "AK-" + index,
		SecretAccessKey: "SK-" + index,
		SecurityToken:   "token-" + index,
		ExpiresAt:       time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339),
	})
	if errMarshal != nil {
		t.Fatalf("encode credential: %v", errMarshal)
	}
	f.authJSON[index] = string(raw)
	return pluginapi.HostAuthFileEntry{
		Provider: ProviderKey, Type: ProviderKey, AuthIndex: index, Name: name,
	}
}

// gatewayBody renders a benefit-listing answer.
func gatewayBody(ids ...string) string {
	models := make([]string, 0, len(ids))
	for _, id := range ids {
		models = append(models, `{"model_id":"`+id+`","model_name":"`+id+`"}`)
	}
	return `{"result":{"models":[` + strings.Join(models, ",") + `]}}`
}

// builtinBody renders a regular-listing answer.
func builtinBody(ids ...string) string {
	models := make([]string, 0, len(ids))
	for _, id := range ids {
		models = append(models, `{"model_id":"`+id+`","model_name":"`+id+`"}`)
	}
	return `{"builtinModels":[` + strings.Join(models, ",") + `]}`
}

// TestCatalogueRefreshReportsUpstreamFailureAsFailure is the honesty guard: when
// both listing endpoints are unreachable, discoverModels returns nil and
// `model.for_auth` would publish the built-in table. That fallback is NOT a
// successful refresh, and reporting it as one would tell the operator the vendor
// agreed when it never answered.
func TestCatalogueRefreshReportsUpstreamFailureAsFailure(t *testing.T) {
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	handle := host.install(t)

	_, errRefresh := catalogueRefresh(handle, DefaultConfig())
	if errRefresh == nil {
		t.Fatal("a refresh whose endpoints both failed was reported as a success")
	}
	if cached, _ := discoveredModels.peek(); len(cached) != 0 {
		t.Fatalf("a failed refresh cached %d models; the fallback table must not be cached", len(cached))
	}
}

// TestCatalogueRefreshReportsNoAccount covers the other way a refresh cannot
// run: there is nothing to fetch with.
func TestCatalogueRefreshReportsNoAccount(t *testing.T) {
	host := newRefreshHost()
	handle := host.install(t)

	if _, errRefresh := catalogueRefresh(handle, DefaultConfig()); errRefresh == nil {
		t.Fatal("a refresh without any account was reported as a success")
	}
}

// TestCatalogueRefreshReportsDisabledDiscovery pins that a catalogue the
// operator pinned to the built-in table is not silently "refreshed".
func TestCatalogueRefreshReportsDisabledDiscovery(t *testing.T) {
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	handle := host.install(t)

	cfg := DefaultConfig()
	cfg.DiscoverModels = false
	if _, errRefresh := catalogueRefresh(handle, cfg); errRefresh == nil {
		t.Fatal("a refresh with discovery disabled was reported as a success")
	}
}

// TestCatalogueRefreshComputesChanged pins the Changed flag in both directions:
// a first fetch differs from the empty cache, and an identical second fetch does
// not.
func TestCatalogueRefreshComputesChanged(t *testing.T) {
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	host.gateway = gatewayBody("glm-5.3-flash", "openpangu-2.0-pro")
	handle := host.install(t)

	outcome, errRefresh := catalogueRefresh(handle, DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh: %v", errRefresh)
	}
	if outcome.Models != 2 {
		t.Fatalf("Models = %d, want the 2 the vendor listed", outcome.Models)
	}
	if !outcome.Changed {
		t.Fatal("Changed = false on a first fetch into an empty cache")
	}

	// The very same listing must not claim a change.
	outcome, errRefresh = catalogueRefresh(handle, DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh (second): %v", errRefresh)
	}
	if outcome.Changed {
		t.Fatal("Changed = true although the vendor answered with the same ids")
	}

	// A grown listing does.
	host.mu.Lock()
	host.builtin = builtinBody("deepseek-v4-pro-0815")
	host.mu.Unlock()
	outcome, errRefresh = catalogueRefresh(handle, DefaultConfig())
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
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	host.gateway = gatewayBody("glm-5.3-flash")
	host.builtin = builtinBody("deepseek-v4-pro")
	handle := host.install(t)

	outcome, errRefresh := catalogueRefresh(handle, DefaultConfig())
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
	ids := modelIDSet(cached)
	if !sameIDSet(ids, []string{"GLM-5.3-Flash", "deepseek-v4-pro"}) {
		t.Fatalf("cached ids = %v", ids)
	}
}

// TestCatalogueRefreshJSONPublishesAndReports pins the machine-readable route:
// it refreshes, then reports the publish, and the publish is a real write to the
// account's auth file.
func TestCatalogueRefreshJSONPublishesAndReports(t *testing.T) {
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	host.gateway = gatewayBody("glm-5.3-flash")
	handle := host.install(t)

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

	host.mu.Lock()
	saved := append([]savedAuthPayload(nil), host.saved...)
	host.mu.Unlock()
	if len(saved) != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1: the publish is what makes the host re-register", len(saved))
	}
	if saved[0].Name != entry.Name {
		t.Fatalf("saved %q, want the account's file %q", saved[0].Name, entry.Name)
	}
	var members map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(saved[0].JSON, &members); errUnmarshal != nil {
		t.Fatalf("the published auth file is not a JSON object: %v", errUnmarshal)
	}
	if _, present := members["model_catalog_published_at"]; !present {
		t.Fatalf("the published auth file carries no change marker: %s", saved[0].JSON)
	}
	// The credential itself must survive the nudge.
	if _, present := members["access_key_id"]; !present {
		t.Fatalf("the publish dropped the credential: %s", saved[0].JSON)
	}
}

// TestCatalogueRefreshJSONReportsFailure pins that the script route reports a
// failed refresh as an error instead of a success.
func TestCatalogueRefreshJSONReportsFailure(t *testing.T) {
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	handle := host.install(t)

	response := catalogueRefreshJSON(handle, pluginapi.ManagementRequest{Query: map[string][]string{}})
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "error" {
		t.Fatalf("status = %v, want error when both listing endpoints fail (body %s)", body["status"], response.Body)
	}
	host.mu.Lock()
	saved := len(host.saved)
	host.mu.Unlock()
	if saved != 0 {
		t.Fatal("a failed refresh must not publish: the host would re-register the unchanged catalogue")
	}
}

// TestAutomaticRefreshNeverWritesAnAuthFile pins the split between the two
// layers, at the level of the refresh function itself: `catalogueRefresh` only
// warms the plugin cache and never writes an auth file. Publishing is the
// scheduler's decision (it passes PublishOnChange and the host name is resolved
// per tick) and the manual button's, not this function's.
func TestAutomaticRefreshNeverWritesAnAuthFile(t *testing.T) {
	previous := settings()
	t.Cleanup(func() {
		setSettings(previous)
		stopCatalogueScheduler()
		catalogueScheduler.SetInterval(0)
	})
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	host.gateway = gatewayBody("glm-5.3-flash")
	handle := host.install(t)

	// Two automatic refreshes, driven directly the way the scheduler drives
	// them: the Request carries no AuthName, so nothing may be published.
	for index := 0; index < 2; index++ {
		outcome, errRefresh := catalogueRefresh(handle, DefaultConfig())
		if errRefresh != nil {
			t.Fatalf("automatic refresh %d: %v", index, errRefresh)
		}
		if outcome.Models == 0 {
			t.Fatalf("automatic refresh %d warmed no cache", index)
		}
	}

	host.mu.Lock()
	saved := len(host.saved)
	host.mu.Unlock()
	if saved != 0 {
		t.Fatalf("the automatic refresh wrote %d auth file(s); only the manual button may publish", saved)
	}

	// The manual path, by contrast, does publish.
	if result := catalog.Run(catalog.Request{
		Host: handle, Provider: ProviderKey, AuthName: entry.Name,
		Refresh: func() (catalog.Outcome, error) { return catalogueRefresh(handle, DefaultConfig()) },
	}); !result.Published {
		t.Fatalf("the manual refresh did not publish: %+v", result)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.saved) != 1 {
		t.Fatalf("host.auth.save calls = %d after the manual refresh, want 1", len(host.saved))
	}
}

// TestStatusPageShowsTheRefreshControl pins the affordance: the status page has
// to carry the manual refresh, and the page it renders must be a GET link.
func TestStatusPageShowsTheRefreshControl(t *testing.T) {
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	handle := host.install(t)

	body := string(renderStatusPage(handle, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	if !strings.Contains(body, "刷新目录") {
		t.Fatalf("the status page has no 刷新目录 control:\n%s", body)
	}
	if !strings.Contains(body, "action=refresh-catalog") {
		t.Fatalf("the 刷新目录 control does not call the refresh action:\n%s", body)
	}
	if !strings.Contains(body, "后台自动刷新") {
		t.Fatalf("the status page does not report the background refresh state:\n%s", body)
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestRefreshPageRendersTheOutcome pins that the manual button's page states the
// result instead of merely responding.
func TestRefreshPageRendersTheOutcome(t *testing.T) {
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	host.gateway = gatewayBody("glm-5.3-flash")
	handle := host.install(t)

	body := string(catalogueRefreshPage(handle, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	for _, want := range []string{"刷新模型目录", "目录已更新", "已通知宿主重新注册"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refresh page is missing %q:\n%s", want, body)
		}
	}
}

// TestRefreshPageReportsAFailureHonestly pins the same page on the failure path.
func TestRefreshPageReportsAFailureHonestly(t *testing.T) {
	host := newRefreshHost()
	entry := host.authFileFor(t, "idx-1", "codearts-alice.json")
	host.files = []pluginapi.HostAuthFileEntry{entry}
	handle := host.install(t)

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
	if !strings.Contains(got, "1m0s") {
		t.Fatalf("autoRefreshText(enabled) = %q, want the interval", got)
	}
	if !strings.Contains(got, "不写凭据文件") {
		t.Fatalf("autoRefreshText(enabled) = %q, want the no-write caveat", got)
	}
}

// TestConfigureStartsAndStopsTheScheduler pins the lifecycle: Configure with an
// interval arms a loop that really ticks, and Quiesce/Shutdown end it. A leak
// here would accumulate one loop per config reload.
//
// The scheduler exposes no "is running" flag on purpose (Enabled() reports the
// configured interval), so the loop is observed the way an operator would: by
// its own run counter. The counter is process-wide, so every wait is expressed
// as a DELTA from a baseline taken just before the arm — otherwise a repeat run
// of this test would pass without anything having ticked.
func TestConfigureStartsAndStopsTheScheduler(t *testing.T) {
	previous := settings()
	t.Cleanup(func() {
		setSettings(previous)
		stopCatalogueScheduler()
		catalogueScheduler.SetInterval(0)
	})
	_ = newRefreshHost().install(t) // the tick needs a host transport, even an empty one
	plugin := Plugin().(*plugin)

	baseline := schedulerRuns()
	if errConfigure := plugin.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if !waitForRuns(baseline+2, 2*time.Second) {
		t.Fatal("Configure did not arm a running background refresh")
	}

	// Quiesce must stop it. Any tick already in flight is given time to land
	// before the baseline is taken.
	plugin.Quiesce()
	time.Sleep(80 * time.Millisecond)
	settled := schedulerRuns()
	time.Sleep(120 * time.Millisecond)
	if runs := schedulerRuns(); runs != settled {
		t.Fatalf("the background refresh still ticked after Quiesce (%d -> %d)", settled, runs)
	}

	// Shutdown is the other exit, and the default (0) must stay disarmed.
	if errConfigure := plugin.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if !waitForRuns(settled+2, 2*time.Second) {
		t.Fatal("a reconfigure did not restart the background refresh")
	}
	plugin.Shutdown()
	time.Sleep(80 * time.Millisecond)
	settled = schedulerRuns()
	time.Sleep(120 * time.Millisecond)
	if runs := schedulerRuns(); runs != settled {
		t.Fatalf("the background refresh still ticked after Shutdown (%d -> %d)", settled, runs)
	}

	if errConfigure := plugin.Configure(nil); errConfigure != nil {
		t.Fatalf("Configure(nil): %v", errConfigure)
	}
	if catalogueScheduler.Enabled() {
		t.Fatal("the default configuration armed the background refresh")
	}
	if runs := schedulerRuns(); runs != settled {
		t.Fatalf("the refresh ticked with model_refresh_ms = 0 (%d -> %d)", settled, runs)
	}
}

// schedulerRuns reads the scheduler's completed-refresh counter.
func schedulerRuns() int {
	_, runs, _, _ := catalogueScheduler.Status()
	return runs
}

// waitForRuns waits until the scheduler's run counter reaches want, so the
// caller knows a loop — not a single stray tick — is running.
func waitForRuns(want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if schedulerRuns() >= want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
