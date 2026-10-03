package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestStatusPageCarriesTheRefreshCatalogueButton pins the affordance: the
// operator-facing entry point must exist on the status page, and it must be a
// GET link (the resource mount is GET only).
func TestStatusPageCarriesTheRefreshCatalogueButton(t *testing.T) {
	catalogueHost(t, remoteModels("MiniMax-M3"))
	page := callManagement(t, testHost(), pluginapi.ManagementRequest{
		Path:    "/v0/resource/plugins/loomy/status",
		Query:   url.Values{},
		Headers: map[string][]string{"Accept": {"text/html"}},
	})
	body := string(page.Body)
	if !strings.Contains(body, "刷新目录") {
		t.Fatal("the status page offers no 刷新目录 button")
	}
	if !strings.Contains(body, "action=refresh-catalog") {
		t.Fatal("the 刷新目录 button does not carry action=refresh-catalog")
	}
	if strings.Contains(body, "<form") {
		t.Fatal("the resource mount is GET only, so a form cannot be the action")
	}
	if !strings.Contains(body, "后台自动刷新") {
		t.Fatal("the status page does not report the background refresh state")
	}
}

// TestCatalogueJSONRouteIsMachineReadable pins the script-facing answer.
//
// Loomy declares NO route in the global `/v0/management/` namespace on purpose
// (management_test pins that), so the machine-readable form is the status
// resource route with `?format=json&action=refresh-catalog`.
func TestCatalogueJSONRouteIsMachineReadable(t *testing.T) {
	fake := catalogueHost(t, remoteModels("MiniMax-M3"))
	response := callManagement(t, testHost(), pluginapi.ManagementRequest{
		Path:  "/v0/resource/plugins/loomy/status",
		Query: url.Values{"format": {"json"}, "action": {"refresh-catalog"}},
	})
	if response.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("catalog JSON: %v (%s)", errUnmarshal, string(response.Body))
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %v, want ok: %s", body["status"], string(response.Body))
	}
	if body["models"] != float64(1) {
		t.Fatalf("models = %v, want 1", body["models"])
	}
	if body["changed"] != true {
		t.Fatalf("changed = %v, want true", body["changed"])
	}
	if body["published"] != true {
		t.Fatalf("published = %v, want true: %s", body["published"], string(response.Body))
	}
	if _, ok := body["duration_ms"]; !ok {
		t.Fatal("the payload carries no duration_ms")
	}
	if len(fake.savedNames()) == 0 {
		t.Fatal("the manual JSON route did not publish through the host")
	}
}

// TestCatalogueJSONRouteReportsFailure pins that a failed refresh answers with
// status=error rather than a success shape carrying zero models.
func TestCatalogueJSONRouteReportsFailure(t *testing.T) {
	fake := catalogueHost(t, remoteModels("MiniMax-M3"))
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	response := callManagement(t, testHost(), pluginapi.ManagementRequest{
		Path:  "/v0/resource/plugins/loomy/status",
		Query: url.Values{"format": {"json"}, "action": {"refresh-catalog"}},
	})
	var body map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("catalog JSON: %v", errUnmarshal)
	}
	if body["status"] != "error" {
		t.Fatalf("status = %v, want error: %s", body["status"], string(response.Body))
	}
	if body["published"] == true {
		t.Fatalf("a failed refresh published: %s", string(response.Body))
	}
}

// TestCatalogueSchedulerFollowsSettings pins the lifecycle. Enabled() reports
// whether an interval is CONFIGURED, so model_refresh_ms = 0 configures nothing,
// a positive interval configures one loop, and a reload back to 0 takes it away.
func TestCatalogueSchedulerFollowsSettings(t *testing.T) {
	t.Cleanup(stopCatalogueScheduler)
	cfg := DefaultConfig()
	startCatalogueScheduler(cfg)
	if catalogueScheduler.Enabled() {
		t.Fatal("model_refresh_ms = 0 enabled the scheduler")
	}

	cfg.ModelRefreshMS = 60_000
	startCatalogueScheduler(cfg)
	if !catalogueScheduler.Enabled() {
		t.Fatal("a positive interval did not enable the scheduler")
	}
	interval, _, _, _ := catalogueScheduler.Status()
	if interval != time.Minute {
		t.Fatalf("interval = %s, want 1m", interval)
	}
	startCatalogueScheduler(cfg)

	cfg.ModelRefreshMS = 0
	startCatalogueScheduler(cfg)
	if catalogueScheduler.Enabled() {
		t.Fatal("model_refresh_ms = 0 left the scheduler enabled")
	}
	stopCatalogueScheduler()
}

// TestAutomaticTickPublishesOnlyOnChange pins the automatic path against a REAL
// scheduler tick.
//
// The host registry (`GET /v1/models`) is what clients read, so an automatic
// refresh that never published would leave it stale forever. It therefore
// publishes WHEN the catalogue moved — and only then, because a write rewrites
// the same auth file the host rewrites when it renews a token, so a stable
// catalogue must cost zero writes.
func TestAutomaticTickPublishesOnlyOnChange(t *testing.T) {
	fake := catalogueHost(t, remoteModels("MiniMax-M3"))
	cfg := DefaultConfig()
	cfg.DiscoverModels = true
	cfg.ModelRefreshMS = 20
	withSettings(t, cfg)
	startCatalogueScheduler(cfg)
	t.Cleanup(stopCatalogueScheduler)

	waitForScheduledRun(t, catalogueScheduler)
	if writes := fake.saveWrites(); writes != 1 {
		t.Fatalf("cold-start tick wrote %d auth files, want exactly 1 (the catalogue moved)", writes)
	}
	// The tick must have WORKED, not merely run: a background refresh that
	// silently failed every time would leave the cache cold.
	if len(peekDiscoveredModels()) == 0 {
		t.Fatal("the automatic refresh ran but never populated the cache")
	}
	if _, _, _, lastErr := catalogueScheduler.Status(); lastErr != "" {
		t.Fatalf("the automatic refresh failed: %s", lastErr)
	}

	// Steady state: the upstream is unchanged, so several more ticks must not
	// write again. The counter (not the `saved` map) is what proves it.
	runsAtFirstWrite := scheduledRuns(t, catalogueScheduler)
	waitForScheduledRuns(t, catalogueScheduler, runsAtFirstWrite+3)
	if writes := fake.saveWrites(); writes != 1 {
		t.Fatalf("a stable catalogue kept publishing: %d auth-file writes, want 1", writes)
	}
}

// schedulerStatus is the slice of catalog.Scheduler a test needs.
type schedulerStatus interface {
	Status() (time.Duration, int, time.Time, string)
}

// waitForScheduledRun blocks until the scheduler has completed one refresh, so a
// test can assert on what that run did.
func waitForScheduledRun(t *testing.T, scheduler schedulerStatus) {
	t.Helper()
	waitForScheduledRuns(t, scheduler, 1)
}

// scheduledRuns reports how many automatic refreshes have completed.
func scheduledRuns(t *testing.T, scheduler schedulerStatus) int {
	t.Helper()
	_, runs, _, _ := scheduler.Status()
	return runs
}

// waitForScheduledRuns blocks until at least want refreshes have completed, so a
// test can assert that a STEADY STATE kept behaving after several more ticks.
func waitForScheduledRuns(t *testing.T, scheduler schedulerStatus, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if scheduledRuns(t, scheduler) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the background scheduler completed %d refreshes, want at least %d",
		scheduledRuns(t, scheduler), want)
}

// TestAutoRefreshTextStatesWhetherItPublishes pins the wording that keeps an
// operator from believing the timer writes credentials.
func TestAutoRefreshTextStatesWhetherItPublishes(t *testing.T) {
	cfg := DefaultConfig()
	if text := autoRefreshText(cfg); !strings.Contains(text, "已关闭") {
		t.Fatalf("disabled text = %q", text)
	}
	cfg.ModelRefreshMS = 30 * 60 * 1000
	// The timer writes an auth file ONLY when the catalogue moved; the page must
	// say exactly that, or an operator will not know why a tick ever wrote.
	if text := autoRefreshText(cfg); !strings.Contains(text, "仅在目录变化时才写凭据文件") {
		t.Fatalf("enabled text = %q, want it to state the write-on-change rule", text)
	}
}

// TestManualRefreshPublishesEvenWithoutChange pins the other half of the
// contract. The automatic path writes only on change; the MANUAL path is
// unconditional, because an operator pressing the button expects the host to
// re-register whether or not the listing moved. A second press on an unchanged
// catalogue must therefore still reach the host.
func TestManualRefreshPublishesEvenWithoutChange(t *testing.T) {
	fake := catalogueHost(t, remoteModels("MiniMax-M3"))
	request := pluginapi.ManagementRequest{
		Path:  "/v0/resource/plugins/loomy/status",
		Query: url.Values{"format": {"json"}, "action": {"refresh-catalog"}},
	}
	first := callManagement(t, testHost(), request)
	var firstBody map[string]any
	if errUnmarshal := json.Unmarshal(first.Body, &firstBody); errUnmarshal != nil {
		t.Fatalf("catalog JSON: %v", errUnmarshal)
	}
	if firstBody["changed"] != true || firstBody["published"] != true {
		t.Fatalf("first manual call = %v / %v, want changed and published", firstBody["changed"], firstBody["published"])
	}

	second := callManagement(t, testHost(), request)
	var secondBody map[string]any
	if errUnmarshal := json.Unmarshal(second.Body, &secondBody); errUnmarshal != nil {
		t.Fatalf("catalog JSON: %v", errUnmarshal)
	}
	if secondBody["changed"] != false {
		t.Fatalf("second manual call changed = %v, want false (the listing did not move)", secondBody["changed"])
	}
	if secondBody["published"] != true {
		t.Fatalf("second manual call published = %v, want true (the operator asked)", secondBody["published"])
	}
	if writes := fake.saveWrites(); writes != 2 {
		t.Fatalf("auth-file writes = %d, want 2 (one per manual call)", writes)
	}
}
