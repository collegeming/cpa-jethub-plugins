package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestRegistration: the hub is a management-only plugin. It owns no credentials
// and no models, so declaring more would put it into the OAuth login list and
// the model registry for no reason.
func TestRegistration(t *testing.T) {
	registration := Plugin().Registration()
	if registration.Metadata.Name != DisplayName {
		t.Fatalf("name = %q, want %q", registration.Metadata.Name, DisplayName)
	}
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
	}
	capabilities := registration.Capabilities
	if !capabilities.ManagementAPI {
		t.Fatal("management_api must be declared")
	}
	for name, declared := range map[string]bool{
		"model_registrar": capabilities.ModelRegistrar,
		"model_provider":  capabilities.ModelProvider,
		"auth_provider":   capabilities.AuthProvider,
		"executor":        capabilities.Executor,
		"quota_provider":  capabilities.QuotaProvider,
	} {
		if declared {
			t.Fatalf("%s must not be declared by the hub plugin", name)
		}
	}
	if len(registration.Metadata.ConfigFields) != len(ConfigFields()) {
		t.Fatalf("config fields = %d, want %d", len(registration.Metadata.ConfigFields), len(ConfigFields()))
	}
	for _, method := range []string{pluginabi.MethodManagementRegister, pluginabi.MethodManagementHandle} {
		if _, ok := Plugin().Routes()[method]; !ok {
			t.Fatalf("method %s is not wired", method)
		}
	}
}

// TestABIDispatch exercises the two methods through the real envelope contract.
func TestABIDispatch(t *testing.T) {
	resetLastRun(t)
	withSettings(t, testConfig())

	register := dispatchEnvelope(t, pluginabi.MethodPluginRegister, []byte(`{"config_yaml":"ZW5hYmxlZDogdHJ1ZQ=="}`))
	if !register.OK {
		t.Fatalf("plugin.register failed: %+v", register.Error)
	}
	// plugin.register applies its config_yaml, so re-pin the test settings.
	withSettings(t, testConfig())

	payload, errMarshal := json.Marshal(managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status",
		url.Values{"format": {"json"}}, "application/json"))
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	status := dispatchEnvelope(t, pluginabi.MethodManagementHandle, payload)
	if !status.OK {
		t.Fatalf("management.handle failed: %+v", status.Error)
	}

	unknown, errMarshal := json.Marshal(managementRequest(http.MethodGet, "/v0/resource/plugins/hub/nope", nil, "application/json"))
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	response := dispatchEnvelope(t, pluginabi.MethodManagementHandle, unknown)
	if !response.OK {
		t.Fatalf("unknown route should answer a 404 document, got error %+v", response.Error)
	}
	var decoded pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(response.Result, &decoded); errUnmarshal != nil {
		t.Fatalf("decode management response: %v", errUnmarshal)
	}
	if decoded.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", decoded.StatusCode)
	}

	unsupported := dispatchEnvelope(t, "executor.execute", []byte(`{}`))
	if unsupported.OK {
		t.Fatal("an unimplemented method must fail the envelope")
	}
}

// TestConfigureAppliesSettingsAndShutdownForgets: the lifecycle methods the
// host calls must actually take effect.
func TestConfigureAppliesSettingsAndShutdownForgets(t *testing.T) {
	withSettings(t, DefaultConfig())
	configurer, okConfigurer := Plugin().(abiboot.Configurer)
	if !okConfigurer {
		t.Fatal("the plugin must implement abiboot.Configurer so the host can deliver settings")
	}
	if errConfigure := configurer.Configure([]byte("host_base_url: http://127.0.0.1:9999\nproviders: qoder\n")); errConfigure != nil {
		t.Fatalf("configure: %v", errConfigure)
	}
	cfg := settings()
	if cfg.HostBaseURL != "http://127.0.0.1:9999" || len(cfg.Providers) != 1 {
		t.Fatalf("settings were not applied: %+v", cfg)
	}

	now := time.Now()
	rememberLastRun(&runResult{BaseURL: cfg.HostBaseURL, StartedAt: now, FinishedAt: now})
	if lastRun() == nil {
		t.Fatal("snapshot was not stored")
	}
	shutdowner, okShutdowner := Plugin().(abiboot.Shutdowner)
	if !okShutdowner {
		t.Fatal("the plugin must implement abiboot.Shutdowner")
	}
	shutdowner.Shutdown()
	if lastRun() != nil {
		t.Fatal("shutdown must drop the cached snapshot")
	}
}

// TestStatusRouteUnknownActionIsRejected: only the documented action performs a
// write; a typo must not be silently treated as "run anyway".
func TestStatusRouteUnknownActionIsRejected(t *testing.T) {
	resetLastRun(t)
	withSettings(t, testConfig())
	response := dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status",
		url.Values{"action": {"checkout"}, "format": {"json"}}, "application/json"))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	if !strings.Contains(string(response.Body), "action=checkin") {
		t.Fatalf("rejection does not explain the accepted action: %s", response.Body)
	}
}

// dispatchEnvelope runs one ABI call and decodes the envelope.
func dispatchEnvelope(t *testing.T, method string, payload []byte) abiboot.Envelope {
	t.Helper()
	raw := abiboot.Dispatch(Plugin(), method, payload)
	var envelope abiboot.Envelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode envelope for %s: %v (%s)", method, errUnmarshal, raw)
	}
	return envelope
}
