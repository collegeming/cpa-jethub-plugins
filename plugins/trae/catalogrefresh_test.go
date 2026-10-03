package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// refreshHost installs a fake host with one TRAE account and answers the batch
// catalog endpoint from `body`; an empty body simulates an unreachable upstream.
// It also records the auth saves, which is how a publish is observed.
type refreshHost struct {
	files []pluginapi.HostAuthFileEntry
	auths map[string]json.RawMessage
	saved []savedTraeAuth
}

type savedTraeAuth struct {
	Name string
	JSON json.RawMessage
}

// install registers the fake transport for one test and clears the package
// caches that would otherwise leak between tests.
func (f *refreshHost) install(t *testing.T, body string) *abiboot.Host {
	t.Helper()
	credentialRefresher = newCredentialRefresher()
	catalogMu.Lock()
	catalogCache = map[string]catalog{}
	catalogMu.Unlock()
	t.Cleanup(func() {
		credentialRefresher = newCredentialRefresher()
		catalogMu.Lock()
		catalogCache = map[string]catalog{}
		catalogMu.Unlock()
	})

	fakeHost(t, func(method string, request []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPDo:
			var payload struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(request, &payload)
			if !strings.Contains(payload.URL, BatchModelsPath) {
				return map[string]any{"StatusCode": 404, "Body": ""}, nil
			}
			if body == "" {
				return map[string]any{"StatusCode": 500, "Body": ""}, nil
			}
			return map[string]any{
				"StatusCode": 200,
				"Body":       base64.StdEncoding.EncodeToString([]byte(body)),
			}, nil
		case pluginabi.MethodHostAuthList:
			return map[string]any{"files": f.files}, nil
		case pluginabi.MethodHostAuthGet:
			var payload pluginapi.HostAuthGetRequest
			_ = json.Unmarshal(request, &payload)
			raw, ok := f.auths[payload.AuthIndex]
			if !ok {
				return nil, abiboot.Errorf("auth_not_found", "no auth %s", payload.AuthIndex)
			}
			return map[string]any{"auth_index": payload.AuthIndex, "json": raw}, nil
		case pluginabi.MethodHostAuthSave:
			var payload pluginapi.HostAuthSaveRequest
			_ = json.Unmarshal(request, &payload)
			f.saved = append(f.saved, savedTraeAuth{Name: payload.Name, JSON: payload.JSON})
			return map[string]any{"name": payload.Name, "path": "/auths/" + payload.Name}, nil
		case pluginabi.MethodHostLog:
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"trae-refresh-callback"}`))
}

// addAccount registers a complete TRAE credential and returns its listing entry.
func (f *refreshHost) addAccount(t *testing.T, index, name string) pluginapi.HostAuthFileEntry {
	t.Helper()
	credential := &Credential{
		Type:         ProviderKey,
		AccessToken:  "token-" + index,
		RefreshToken: "refresh-" + index,
		ExpiresAt:    "99999999999999",
		UID:          "uid-" + index,
		MachineID:    "0123456789abcdef0123456789abcdef",
		DeviceID:     "fedcba9876543210fedcba9876543210",
		Region:       RegionCN,
	}
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode credential: %v", errEncode)
	}
	if f.auths == nil {
		f.auths = map[string]json.RawMessage{}
	}
	f.auths[index] = storage
	entry := pluginapi.HostAuthFileEntry{
		Provider: ProviderKey, Type: ProviderKey, AuthIndex: index, Name: name, Status: "active",
	}
	f.files = append(f.files, entry)
	return entry
}

// traeCatalogBody renders a `batch_get_detail_param` answer listing the ids.
func traeCatalogBody(ids ...string) string {
	list := make([]string, 0, len(ids))
	for _, id := range ids {
		list = append(list, `{"config_name":"`+id+`","usage":"chat_completion","config_switch":true,`+
			`"context_window_tokens":{"dev":200000},"display_config":{"display_name":"`+id+`"}}`)
	}
	return `{"function_configs":[{"function":"solo_work_lite","config_info_list":[` + strings.Join(list, ",") + `]}]}`
}

// TestCatalogueRefreshReportsUpstreamFailureAsFailure is the honesty guard: when
// the catalog endpoint fails, fetchCatalog returns an error and `model.for_auth`
// would publish the static list. That fallback is NOT a successful refresh —
// reporting it as one would tell the operator the vendor agreed when it never
// answered — and it must not be cached.
func TestCatalogueRefreshReportsUpstreamFailureAsFailure(t *testing.T) {
	fake := &refreshHost{}
	fake.addAccount(t, "idx-1", "trae-alice.json")
	h := fake.install(t, "")

	cfg := DefaultConfig()
	credential, _, errCredential := credentialOf(h, fake.files[0])
	if errCredential != nil {
		t.Fatalf("credentialOf: %v", errCredential)
	}
	if _, errRefresh := catalogueRefresh(h, cfg); errRefresh == nil {
		t.Fatal("a refresh whose endpoint failed was reported as a success")
	}
	if _, ok := peekCatalog(cfg, credential); ok {
		t.Fatal("a failed refresh cached an entry; the static fallback must not be cached")
	}
	if len(fake.saved) != 0 {
		t.Fatalf("a failed refresh published %d auth files", len(fake.saved))
	}
}

// TestCatalogueRefreshReportsNoAccount covers the other way a refresh cannot run.
func TestCatalogueRefreshReportsNoAccount(t *testing.T) {
	fake := &refreshHost{}
	h := fake.install(t, traeCatalogBody("glm-5.1"))
	if _, errRefresh := catalogueRefresh(h, DefaultConfig()); errRefresh == nil {
		t.Fatal("a refresh without any account was reported as a success")
	}
}

// TestCatalogueRefreshReportsDisabledDiscovery pins that a catalog the operator
// pinned to the built-in list is not silently "refreshed".
func TestCatalogueRefreshReportsDisabledDiscovery(t *testing.T) {
	fake := &refreshHost{}
	fake.addAccount(t, "idx-1", "trae-alice.json")
	h := fake.install(t, traeCatalogBody("glm-5.1"))
	cfg := DefaultConfig()
	cfg.DiscoverModels = false
	if _, errRefresh := catalogueRefresh(h, cfg); errRefresh == nil {
		t.Fatal("a refresh with discovery disabled was reported as a success")
	}
}

// TestCatalogueRefreshComputesChanged pins the Changed flag in both directions:
// a first fetch differs from the empty cache, an identical second fetch does not,
// and a listing that grew does.
func TestCatalogueRefreshComputesChanged(t *testing.T) {
	fake := &refreshHost{}
	fake.addAccount(t, "idx-1", "trae-alice.json")
	h := fake.install(t, traeCatalogBody("glm-5.1", "kimi-k3"))

	outcome, errRefresh := catalogueRefresh(h, DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh: %v", errRefresh)
	}
	if outcome.Models != 2 {
		t.Fatalf("Models = %d, want the 2 the vendor listed", outcome.Models)
	}
	if !outcome.Changed {
		t.Fatal("Changed = false on a first fetch into an empty cache")
	}

	outcome, errRefresh = catalogueRefresh(h, DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh (second): %v", errRefresh)
	}
	if outcome.Changed {
		t.Fatal("Changed = true although the vendor answered with the same ids")
	}
}

// TestCatalogueRefreshRepopulatesTheCache pins that a successful refresh goes
// through the same path a client request would: after it, catalogFor serves the
// refreshed listing from the cache rather than refetching, so the two cannot
// disagree.
func TestCatalogueRefreshRepopulatesTheCache(t *testing.T) {
	fake := &refreshHost{}
	fake.addAccount(t, "idx-1", "trae-alice.json")
	h := fake.install(t, traeCatalogBody("glm-5.1"))

	outcome, errRefresh := catalogueRefresh(h, DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("catalogueRefresh: %v", errRefresh)
	}
	credential, _, errCredential := credentialOf(h, fake.files[0])
	if errCredential != nil {
		t.Fatalf("credentialOf: %v", errCredential)
	}
	cached, ok := peekCatalog(DefaultConfig(), credential)
	if !ok || len(cached.models) != outcome.Models {
		t.Fatalf("cache holds %#v, want the %d refreshed models", cached, outcome.Models)
	}
}

// TestCatalogueRefreshJSONPublishesAndReports pins the machine-readable route: it
// refreshes, then reports the publish, and the publish is a real write to the
// account's auth file that preserves the credential.
func TestCatalogueRefreshJSONPublishesAndReports(t *testing.T) {
	fake := &refreshHost{}
	entry := fake.addAccount(t, "idx-1", "trae-alice.json")
	h := fake.install(t, traeCatalogBody("glm-5.1"))

	response := catalogueRefreshJSON(h, pluginapi.ManagementRequest{Query: map[string][]string{}})
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

	if len(fake.saved) != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1: the publish is what makes the host re-register", len(fake.saved))
	}
	if fake.saved[0].Name != entry.Name {
		t.Fatalf("saved %q, want the account's file %q", fake.saved[0].Name, entry.Name)
	}
	var members map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(fake.saved[0].JSON, &members); errUnmarshal != nil {
		t.Fatalf("the published auth file is not a JSON object: %v", errUnmarshal)
	}
	if _, present := members["model_catalog_published_at"]; !present {
		t.Fatalf("the published auth file carries no change marker: %s", fake.saved[0].JSON)
	}
	// The credential itself must survive the nudge.
	if _, present := members["access_token"]; !present {
		t.Fatalf("the publish dropped the credential: %s", fake.saved[0].JSON)
	}
}

// TestCatalogueRefreshJSONReportsFailure pins that the script route reports a
// failed refresh as an error instead of a success, and publishes nothing.
func TestCatalogueRefreshJSONReportsFailure(t *testing.T) {
	fake := &refreshHost{}
	fake.addAccount(t, "idx-1", "trae-alice.json")
	h := fake.install(t, "")

	response := catalogueRefreshJSON(h, pluginapi.ManagementRequest{Query: map[string][]string{}})
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "error" {
		t.Fatalf("status = %v, want error when the catalog endpoint fails (body %s)", body["status"], response.Body)
	}
	if len(fake.saved) != 0 {
		t.Fatalf("a failed refresh published %d auth files", len(fake.saved))
	}
}

// TestStatusPageShowsTheRefreshControl pins the affordance: the status page has
// to carry the publishing refresh, and it must be a GET link.
func TestStatusPageShowsTheRefreshControl(t *testing.T) {
	accountHost(t, nil)
	body := string(renderStatusPage(abiboot.NewHost(nil), pluginapi.ManagementRequest{}).Body)
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
	fake := &refreshHost{}
	fake.addAccount(t, "idx-1", "trae-alice.json")
	h := fake.install(t, traeCatalogBody("glm-5.1"))

	body := string(catalogueRefreshPage(h, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	for _, want := range []string{"刷新模型目录", "目录已更新", "已通知宿主重新注册"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refresh page is missing %q:\n%s", want, body)
		}
	}
}

// TestRefreshPageReportsAFailureHonestly pins the same page on the failure path.
func TestRefreshPageReportsAFailureHonestly(t *testing.T) {
	fake := &refreshHost{}
	fake.addAccount(t, "idx-1", "trae-alice.json")
	h := fake.install(t, "")

	body := string(catalogueRefreshPage(h, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	if !strings.Contains(body, "刷新失败") {
		t.Fatalf("a failed refresh page does not say it failed:\n%s", body)
	}
}

// TestAutoRefreshTextReportsTheOffState pins the default: the background refresh
// is opt-in and the page says so.
//
// ⚠️ The enabled wording is asserted POSITIVELY, and it deliberately does NOT say
// "no credential writes": the automatic path publishes whenever the catalog
// changes, and claiming otherwise on the page would be a lie the operator would
// only discover by watching the auth file. What the page must promise is the
// narrower, true thing — a stable catalog costs nothing.
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
	fake := &refreshHost{}
	fake.addAccount(t, "idx-1", "trae-alice.json")
	_ = fake.install(t, traeCatalogBody("glm-5.1"))
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
	if !waitForTraeSchedulerRuns(2 * time.Second) {
		t.Fatal("Configure did not arm a running background refresh")
	}

	plugin.Quiesce()
	time.Sleep(80 * time.Millisecond)
	_, settled, _, _ := catalogueScheduler.Status()
	time.Sleep(120 * time.Millisecond)
	if _, runs, _, _ := catalogueScheduler.Status(); runs != settled {
		t.Fatalf("the background refresh still ticked after Quiesce (%d -> %d)", settled, runs)
	}

	if errConfigure := plugin.Configure([]byte("model_refresh_ms: 20\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if !waitForTraeSchedulerRuns(2 * time.Second) {
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

// waitForTraeSchedulerRuns waits until the scheduler's run counter has moved at
// least twice, so the caller knows a loop — not a single stray tick — is running.
func waitForTraeSchedulerRuns(timeout time.Duration) bool {
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
// the path is also registered. Both halves are asserted here.
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

	fake := &refreshHost{}
	fake.addAccount(t, "idx-1", "trae-alice.json")
	h := fake.install(t, traeCatalogBody("glm-5.1"))
	raw, errMarshal := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/management/" + ProviderKey + "/catalog",
	})
	if errMarshal != nil {
		t.Fatalf("encode request: %v", errMarshal)
	}
	value, errHandle := handleManagementHandle(h, raw)
	if errHandle != nil {
		t.Fatalf("management.handle: %v", errHandle)
	}
	response, ok := value.(pluginapi.ManagementResponse)
	if !ok {
		t.Fatalf("management.handle returned %T", value)
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "ok" || body["published"] != true {
		t.Fatalf("the declared route did not perform the refresh: %s", response.Body)
	}
}
