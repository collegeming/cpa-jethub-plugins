package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogueHost installs a host with one account whose models-v2 answer is
// controlled by the test.
func catalogueHost(t *testing.T, models string) *fakeHost {
	t.Helper()
	host := newFakeHost()
	host.auths["idx-1"] = mustJSON(t, sampleCredential(7*24*3600))
	host.files = []pluginapi.HostAuthFileEntry{
		{AuthIndex: "idx-1", Name: "atomcode-qq_23240873.json", Provider: ProviderKey, Type: ProviderKey},
	}
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, codingPlanModelsPath):
			return httpResponse(200, models), nil
		case strings.Contains(request.URL, codingPlanStatusPath):
			return httpResponse(200, `{"codingplan_free":{"plan_name":"CodingPlan Lite-体验版","plan_type":"Lite-beta"}}`), nil
		}
		return httpResponse(404, `{"message":"unexpected"}`), nil
	}
	host.install(t)
	return host
}

const oneModel = `[{"display_model_name":"glm5.3-flash","context_window":512000,` +
	`"supports_vision":true,"plan_available":true,"reasoning_effort_levels":["low","high"]}]`

const twoModels = `[{"display_model_name":"glm5.3-flash","context_window":512000,` +
	`"supports_vision":true,"plan_available":true,"reasoning_effort_levels":["low","high"]},` +
	`{"display_model_name":"qwen3.8-27b","context_window":262144,` +
	`"supports_vision":true,"plan_available":true,"reasoning_effort_levels":["low","medium"]}]`

// TestCatalogueRefreshReportsUpstreamFailure pins the rule the shared package
// states: when the vendor never answered, the refresh must report a failure and
// must NOT publish, or the operator would be told the vendor confirmed a
// catalogue it never supplied.
func TestCatalogueRefreshReportsUpstreamFailure(t *testing.T) {
	host := catalogueHost(t, `{"message":"boom"}`)
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, codingPlanModelsPath) {
			return httpResponse(500, `{"message":"boom"}`), nil
		}
		return httpResponse(200, `{}`), nil
	}

	page := catalogueRefreshPage(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/atomcode/status", Query: url.Values{"action": {"refresh-catalog"}},
	})
	if !strings.Contains(string(page.Body), "刷新失败") {
		t.Fatalf("a failed refresh did not say so: %s", string(page.Body))
	}
	if names := host.saved; len(names) != 0 {
		t.Fatalf("a failed refresh published to the host: %v", names)
	}
	if len(host.callsFor(codingPlanModelsPath)) == 0 {
		t.Fatal("the refresh never contacted models-v2: the failure was not an upstream failure")
	}
}

// TestCatalogueRefreshComputesChanged pins the before/after comparison: the
// first successful fetch moves the catalogue off the bundled table (changed),
// an identical fetch does not, and a different listing does.
func TestCatalogueRefreshComputesChanged(t *testing.T) {
	host := catalogueHost(t, oneModel)
	cfg := DefaultConfig()
	cfg.PlanType = planTypeMax

	outcome, errRefresh := catalogueRefresh(testHost(), cfg)
	if errRefresh != nil {
		t.Fatalf("first refresh: %v", errRefresh)
	}
	if outcome.Models != 1 || !outcome.Changed {
		t.Fatalf("first refresh = %+v, want 1 model marked changed", outcome)
	}

	outcome, errRefresh = catalogueRefresh(testHost(), cfg)
	if errRefresh != nil {
		t.Fatalf("second refresh: %v", errRefresh)
	}
	if outcome.Models != 1 || outcome.Changed {
		t.Fatalf("identical refresh = %+v, want no change", outcome)
	}

	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, codingPlanModelsPath) {
			return httpResponse(200, twoModels), nil
		}
		return httpResponse(200, `{}`), nil
	}
	outcome, errRefresh = catalogueRefresh(testHost(), cfg)
	if errRefresh != nil {
		t.Fatalf("third refresh: %v", errRefresh)
	}
	if outcome.Models != 2 || !outcome.Changed {
		t.Fatalf("grown refresh = %+v, want 2 models marked changed", outcome)
	}
	// The refresh function itself never publishes: an automatic tick and the
	// manual button call the same function, and only the manual path adds the
	// AuthName that makes catalog.Run rewrite an auth file.
	if len(host.saved) != 0 {
		t.Fatalf("the refresh function published on its own: %v", host.saved)
	}
}

// TestCatalogueRefreshFailsCleanlyWithoutAnAccount covers the other honest
// failure: a refresh with nothing to refresh must say so, not report success.
func TestCatalogueRefreshFailsCleanlyWithoutAnAccount(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	if _, errRefresh := catalogueRefresh(testHost(), DefaultConfig()); errRefresh == nil {
		t.Fatal("a refresh without an account reported success")
	}
}

// TestCatalogueRefreshRefusesWhenDiscoveryIsOff pins that a disabled discovery
// is not silently refreshed from the bundled table.
func TestCatalogueRefreshRefusesWhenDiscoveryIsOff(t *testing.T) {
	catalogueHost(t, oneModel)
	cfg := DefaultConfig()
	cfg.DiscoverModels = false
	if _, errRefresh := catalogueRefresh(testHost(), cfg); errRefresh == nil {
		t.Fatal("a refresh with discovery disabled reported success")
	}
}
