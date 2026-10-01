package main

import (
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestExtraModelsRideOnTopOfTheDiscoveredCatalogue pins the shape of the merge.
//
// `models-v2` is the entitlement list the official client trusts; the gateway
// serves more than that, and the gap is real (measured 2026-10-01:
// `deepseek-flash` answers HTTP 200 with real content while being absent from
// every tier's `models-v2`). The list is an operator setting rather than a
// bundled default because the same gateway also answers HTTP 200 for an id
// whose content is `三方请求失败: 502`, so "reachable" is not "usable".
func TestExtraModelsRideOnTopOfTheDiscoveredCatalogue(t *testing.T) {
	discovered := []modelEntry{{DisplayModelName: "glm5.3-flash", PlanAvailable: true}}

	// Configured once, the id is added and marked as an addition.
	entries := append(append([]modelEntry(nil), discovered...), extraEntries([]string{"deepseek-flash"}, discovered)...)
	if len(entries) != 2 {
		t.Fatalf("merged catalogue = %d entries, want 2", len(entries))
	}
	extra := entries[1]
	if !extra.FromConfig {
		t.Fatal("the added entry must be marked as coming from settings")
	}
	// Measured metadata is carried, because it came from a real server payload.
	if extra.ContextWindow == nil || *extra.ContextWindow != 1_000_000 {
		t.Fatalf("deepseek-flash context window = %v, want the captured 1000000", extra.ContextWindow)
	}
	if len(extra.ReasoningEffortLevels) != 2 {
		t.Fatalf("deepseek-flash effort levels = %v, want the captured high/max", extra.ReasoningEffortLevels)
	}
	if !strings.Contains(modelDescription(extra.DisplayModelName, extra.FromConfig), "补充模型") {
		t.Fatal("an added model must be labelled as such in its description")
	}

	// An unadvertised-and-unknown id carries NO claims rather than a guess.
	unknown := extraEntries([]string{"Some/Model"}, discovered)[0]
	if unknown.ContextWindow != nil {
		t.Fatalf("an unknown id must not invent a context window, got %v", *unknown.ContextWindow)
	}
	if unknown.SupportsVision != nil {
		t.Fatalf("an unknown id must not claim vision, got %v", *unknown.SupportsVision)
	}
	if len(unknown.ReasoningEffortLevels) != 0 {
		t.Fatalf("an unknown id must not claim effort levels, got %v", unknown.ReasoningEffortLevels)
	}

	// A duplicate of something the server already advertised is dropped, so the
	// model list cannot show the same id twice.
	if again := extraEntries([]string{"glm5.3-flash"}, discovered); len(again) != 0 {
		t.Fatalf("a duplicate was added: %#v", again)
	}
}

// TestServingCatalogueCarriesTheConfiguredExtras goes through the real path —
// discovery, fallback, merge — rather than calling the helper directly. The
// helper-level test above cannot catch a merge that was never wired in, which is
// exactly the mistake this guards against.
func TestServingCatalogueCarriesTheConfiguredExtras(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, codingPlanModelsPath) {
			return httpResponse(200, `[{"display_model_name":"glm5.3-flash","context_window":512000,`+
				`"supports_vision":true,"plan_available":true,"reasoning_effort_levels":["low","high"]}]`), nil
		}
		if strings.Contains(request.URL, codingPlanStatusPath) {
			return httpResponse(200, `{"codingplan_free":{"plan_name":"CodingPlan Lite-体验版","expires_at":"2026-10-08"}}`), nil
		}
		return httpResponse(404, `{"message":"unexpected"}`), nil
	}
	host.install(t)

	cfg := settings()
	cfg.ExtraModels = []string{"deepseek-flash"}
	withSettings(t, cfg)

	entries := staticModelEntries(testHost(), settings(), sampleCredential(7*24*3600))
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.DisplayModelName)
	}
	if len(names) != 2 || names[0] != "glm5.3-flash" || names[1] != "deepseek-flash" {
		t.Fatalf("serving catalogue = %v, want the discovered model followed by the configured extra", names)
	}
	if !entries[1].FromConfig {
		t.Fatal("the extra reached the catalogue without its addition marker")
	}
}
