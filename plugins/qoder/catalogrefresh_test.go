package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Qoder's catalogue has no upstream endpoint (see catalogrefresh.go), so the
// tests here cover what this provider's refresh path actually promises: it
// publishes the catalogue the instance currently resolves, it reports a
// catalogue that resolved to nothing as a failure rather than as a success, and
// it never claims the catalogue "changed" when nothing was replaced.
//
// The other channels' `TestCatalogueRefreshReportsUpstreamFailureAsFailure`
// cannot exist in this form here — there is no upstream to fail. The equivalent
// honesty property is covered by
// TestCatalogueRefreshReportsAnEmptyCatalogueAsFailure below.

// TestCatalogueRefreshReportsTheBuiltInCatalogue pins the catalogue this provider
// publishes: the region's own table plus the operator's public_models, counted
// the same way `model_count` on the status payload counts it, so the two views
// cannot disagree.
func TestCatalogueRefreshReportsTheBuiltInCatalogue(t *testing.T) {
	statusHost(t, "alice")
	cfg := DefaultConfig()
	cfg.PublicModels = []string{"qwen-flash"}
	withSettings(t, cfg)

	outcome, errRefresh := qoderCatalogueRefresh(testHost(), pluginapi.ManagementRequest{})
	if errRefresh != nil {
		t.Fatalf("qoderCatalogueRefresh: %v", errRefresh)
	}
	want := len(staticModelInfos(cfg, activeRegion()))
	if outcome.Models != want {
		t.Fatalf("Models = %d, want the %d models this instance publishes", outcome.Models, want)
	}
}

// TestCatalogueRefreshNeverClaimsAChange pins the honest half of the outcome.
//
// This provider replaces no listing — there is no cache and no fetch — so there
// is nothing a new catalogue could differ from. `Changed` must therefore be
// false, and it must STAY false across calls: reporting true would tell the
// operator a vendor had answered with something new, which is exactly the
// misreading this flag exists to prevent.
func TestCatalogueRefreshNeverClaimsAChange(t *testing.T) {
	statusHost(t, "alice")
	withSettings(t, DefaultConfig())

	for attempt := 0; attempt < 2; attempt++ {
		outcome, errRefresh := qoderCatalogueRefresh(testHost(), pluginapi.ManagementRequest{})
		if errRefresh != nil {
			t.Fatalf("qoderCatalogueRefresh (attempt %d): %v", attempt, errRefresh)
		}
		if outcome.Changed {
			t.Fatalf("Changed = true on attempt %d: nothing was fetched, so nothing can have moved", attempt)
		}
	}
}

// TestCatalogueOutcomeRefusesAnEmptyCatalogue is this provider's honesty guard,
// and the analogue of the other channels' upstream-failure test.
//
// There is no upstream here, so the failure that must never be dressed up as a
// success is a catalogue that resolved to nothing: publishing it would offer a
// client no model to route to. The guard is exercised directly because the
// product tables are non-empty constants and no configuration can reach it
// through the handler.
func TestCatalogueOutcomeRefusesAnEmptyCatalogue(t *testing.T) {
	if _, errRefresh := qoderCatalogueOutcome(nil); errRefresh == nil {
		t.Fatal("an empty catalogue was reported as a successful refresh")
	}

	cfg := DefaultConfig()
	outcome, errRefresh := qoderCatalogueOutcome(staticModelInfos(cfg, activeRegion()))
	if errRefresh != nil {
		t.Fatalf("a populated catalogue must not be reported as a failure: %v", errRefresh)
	}
	if outcome.Models == 0 {
		t.Fatal("a zero-model outcome would be published as an empty catalogue")
	}
	if outcome.Changed {
		t.Fatal("Changed = true although this provider replaces nothing")
	}
}

// TestCatalogueRefreshJSONPublishesAndReports pins the machine-readable route: it
// publishes, and the publish is a real write to the account's auth file that
// carries the change marker without dropping the credential.
func TestCatalogueRefreshJSONPublishesAndReports(t *testing.T) {
	host := statusHost(t, "alice")
	withSettings(t, DefaultConfig())

	response := catalogueRefreshJSON(testHost(), pluginapi.ManagementRequest{})
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
	// The route must state that nothing was fetched, so a script cannot read
	// "ok" as "the vendor was contacted".
	if body["refetch"] != false {
		t.Fatalf("refetch = %v, want false: this provider has no upstream endpoint", body["refetch"])
	}
	if body["source"] != "builtin" {
		t.Fatalf("source = %v, want builtin", body["source"])
	}

	host.mu.Lock()
	saved := host.saved["alice"]
	host.mu.Unlock()
	if len(saved) == 0 {
		t.Fatal("host.auth.save was never called: the publish is what makes the host re-register")
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

// TestCatalogueRefreshJSONReportsNoAccountWithoutWriting pins the no-account
// path: there is no auth file to publish through, so nothing can be registered
// and the route must not answer "ok".
//
// ⚠️ The status is `warning` and not `error` on purpose, and the distinction is
// this provider's own: for a channel with a real cache, an un-published refresh
// still refreshed layer ① and `ok` is truthful. Here there is no layer ① — the
// publish IS the operation — so an un-published run achieved nothing and saying
// `ok` would be a plain falsehood.
func TestCatalogueRefreshJSONReportsNoAccountWithoutWriting(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	withSettings(t, DefaultConfig())

	response := catalogueRefreshJSON(testHost(), pluginapi.ManagementRequest{})
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "warning" {
		t.Fatalf("status = %v, want warning with no account (body %s)", body["status"], response.Body)
	}
	if body["published"] != false {
		t.Fatalf("published = %v, want false with no account", body["published"])
	}
	if body["publish_skipped"] == nil {
		t.Fatalf("the body does not explain why nothing was published: %s", response.Body)
	}
	host.mu.Lock()
	saved := len(host.saved)
	host.mu.Unlock()
	if saved != 0 {
		t.Fatalf("a refresh with no account wrote %d auth files", saved)
	}
}

// TestStatusPageShowsTheRefreshControl pins the affordance: the status page
// carries the publish control as a GET link, and it states why there is no
// background refresh rather than leaving the row silently absent.
func TestStatusPageShowsTheRefreshControl(t *testing.T) {
	statusHost(t, "alice")
	withSettings(t, DefaultConfig())

	body := string(renderStatusPage(testHost(), pluginapi.ManagementRequest{}).Body)
	for _, want := range []string{"刷新目录", "action=refresh-catalog", "后台自动刷新"} {
		if !strings.Contains(body, want) {
			t.Errorf("the status page is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestRefreshPageRendersTheOutcome pins that the manual button's page states the
// result instead of merely responding, and that it names the catalogue source so
// the page cannot be read as evidence of a fetch.
func TestRefreshPageRendersTheOutcome(t *testing.T) {
	statusHost(t, "alice")
	withSettings(t, DefaultConfig())

	body := string(catalogueRefreshPage(testHost(), pluginapi.ManagementRequest{}).Body)
	for _, want := range []string{"刷新模型目录", "目录无变化", "已通知宿主重新注册", "内置静态表"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refresh page is missing %q:\n%s", want, body)
		}
	}
}

// TestRefreshPageStatesASkippedPublish pins the page on the no-account path: it
// must not render a green "目录无变化" over a catalogue that was never published.
func TestRefreshPageStatesASkippedPublish(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	withSettings(t, DefaultConfig())

	body := string(catalogueRefreshPage(testHost(), pluginapi.ManagementRequest{}).Body)
	for _, want := range []string{"未发布", "没有可用的账号文件", "目录来源"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refresh page is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `class="notice success"`) {
		t.Fatalf("an un-published refresh was rendered as a success:\n%s", body)
	}
}

// TestAutoRefreshTextExplainsTheAbsence pins the wording: the row exists to
// answer "why is this not like the other channels", so it must say that there is
// no endpoint and no cache, not merely that the feature is off.
func TestAutoRefreshTextExplainsTheAbsence(t *testing.T) {
	got := autoRefreshText()
	for _, want := range []string{"不适用", "内置静态表", "没有上游端点", "刷新目录"} {
		if !strings.Contains(got, want) {
			t.Errorf("autoRefreshText() = %q, missing %q", got, want)
		}
	}
}

// TestNoBackgroundSchedulerIsArmed pins the deliberate omission. Qoder has no
// layer ①, so `Configure` must not start a timer: a loop whose every tick
// reported "no change" would look like a working feature while nothing was ever
// fetched. This test would fail the moment such a scheduler is added without the
// discovery path that would justify it.
func TestNoBackgroundSchedulerIsArmed(t *testing.T) {
	// There is no catalogueScheduler in this package at all; the assertion is
	// that the configuration surface has no interval to arm one with.
	for _, field := range ConfigFields() {
		if field.Name == "model_refresh_ms" {
			t.Fatal("model_refresh_ms is declared, but this provider has no cache to refresh; " +
				"a timer here would report success without fetching anything")
		}
	}
}

// TestCredentialRegionSelectsTheCatalogue pins that the account's own region
// decides which table is published — the same resolution `handleModelForAuth`
// performs, so the published registry matches what a client request would get.
func TestCredentialRegionSelectsTheCatalogue(t *testing.T) {
	host := newFakeHost()
	credential := &Credential{
		AccessToken: "tok", SecurityOAuthToken: "tok", RefreshToken: "ref", MachineID: "machine-1",
		UID: "u-1", Nickname: "cn", Region: string(RegionCN), ExpireTime: timeNowPlusHour(),
	}
	stored, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode credential: %v", errEncode)
	}
	host.files = []pluginapi.HostAuthFileEntry{{
		AuthIndex: "idx-1", ID: "idx-1", Name: "alice", Provider: ProviderKey, Status: "active",
	}}
	host.auths["idx-1"] = stored
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, "{}"), nil
	}
	host.install(t)
	// The configuration default is the global site; the credential's own region
	// must win.
	cfg := DefaultConfig()
	withSettings(t, cfg)
	if region := credential.regionOr(cfg.Region); region != RegionCN {
		t.Fatalf("credential region resolution = %q, want the credential's own %q", region, RegionCN)
	}
	outcome, errRefresh := qoderCatalogueRefresh(testHost(), pluginapi.ManagementRequest{})
	if errRefresh != nil {
		t.Fatalf("qoderCatalogueRefresh: %v", errRefresh)
	}
	want := len(staticModelInfos(cfg, RegionCN))
	if outcome.Models != want {
		t.Fatalf("Models = %d, want the credential region's %d models", outcome.Models, want)
	}
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

	statusHost(t, "alice")
	withSettings(t, DefaultConfig())
	response := managementCall(t, testHost(), managementRequest(
		"/v0/management/"+ProviderKey+"/catalog", nil, ""))
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if body["status"] != "ok" || body["published"] != true {
		t.Fatalf("the declared route did not perform the publish: %s", response.Body)
	}
}
