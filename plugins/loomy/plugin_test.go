package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The method surface must match every other provider plugin in this repository.
func TestRoutesCoverTheProviderSurface(t *testing.T) {
	routes := Plugin().Routes()
	for _, method := range []string{
		pluginabi.MethodAuthIdentifier,
		pluginabi.MethodAuthParse,
		pluginabi.MethodAuthLoginStart,
		pluginabi.MethodAuthLoginPoll,
		pluginabi.MethodAuthRefresh,
		pluginabi.MethodModelRegister,
		pluginabi.MethodModelStatic,
		pluginabi.MethodModelForAuth,
		pluginabi.MethodExecutorIdentifier,
		pluginabi.MethodExecutorExecute,
		pluginabi.MethodExecutorExecuteStream,
		pluginabi.MethodExecutorCountTokens,
		pluginabi.MethodRequestTranslate,
		pluginabi.MethodResponseTranslate,
		pluginabi.MethodQuotaIdentifier,
		pluginabi.MethodQuotaDescribe,
		pluginabi.MethodQuotaFetch,
		pluginabi.MethodQuotaReset,
		pluginabi.MethodManagementRegister,
		pluginabi.MethodManagementHandle,
	} {
		if _, present := routes[method]; !present {
			t.Errorf("route %s is not implemented", method)
		}
	}
	if len(routes) != 20 {
		t.Fatalf("routes = %d, want the 20 methods of the shared surface", len(routes))
	}
}

// The registration declares the identity and the capabilities the host routes on.
func TestRegistrationCapabilities(t *testing.T) {
	lifecycle, errLifecycle := json.Marshal(map[string]any{
		"config_yaml": base64.StdEncoding.EncodeToString([]byte("discover_models: true")),
	})
	if errLifecycle != nil {
		t.Fatalf("marshal lifecycle: %v", errLifecycle)
	}
	raw := abiboot.Dispatch(Plugin(), pluginabi.MethodPluginRegister, lifecycle)
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !envelope.OK {
		t.Fatalf("register failed: %s", raw)
	}

	var decoded struct {
		SchemaVersion uint32               `json:"schema_version"`
		Metadata      pluginapi.Metadata   `json:"metadata"`
		Capabilities  abiboot.Capabilities `json:"capabilities"`
	}
	if errUnmarshal := json.Unmarshal(envelope.Result, &decoded); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}
	if decoded.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", decoded.SchemaVersion, pluginabi.SchemaVersion)
	}
	if decoded.Metadata.Name != DisplayName || decoded.Metadata.Version != Version {
		t.Fatalf("metadata = %#v, want the Loomy identity", decoded.Metadata)
	}
	if len(decoded.Metadata.ConfigFields) != len(ConfigFields()) {
		t.Fatalf("config fields = %d, want %d", len(decoded.Metadata.ConfigFields), len(ConfigFields()))
	}
	capabilities := decoded.Capabilities
	if !capabilities.AuthProvider || !capabilities.ModelProvider || !capabilities.ModelRegistrar ||
		!capabilities.Executor || !capabilities.RequestTranslator || !capabilities.ResponseTranslator ||
		!capabilities.QuotaProvider || !capabilities.ManagementAPI {
		t.Fatalf("capabilities = %#v, want the full provider surface", capabilities)
	}
	if capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("executor scope = %q, want oauth-bound models", capabilities.ExecutorModelScope)
	}
	if len(capabilities.ExecutorInputFormats) != 1 || capabilities.ExecutorInputFormats[0] != "chat-completions" {
		t.Fatalf("input formats = %#v, want chat-completions", capabilities.ExecutorInputFormats)
	}
	// The register payload also configured the instance.
	if !settings().DiscoverModels {
		t.Fatal("plugin.register must apply the delivered config_yaml")
	}
	setSettings(DefaultConfig())
}

// Reconfiguring drops the discovered catalogue so a changed setting cannot serve
// a stale list.
func TestConfigureResetsDiscoveryCache(t *testing.T) {
	putCachedModels([]modelDescriptor{{ID: "cached", Name: "Cached"}})
	if cached := cachedModels(time.Duration(DefaultConfig().modelCacheTTL()) * time.Millisecond); len(cached) != 1 {
		t.Fatalf("cache = %#v, want the seeded entry", cached)
	}
	if errConfigure := Plugin().(*plugin).Configure([]byte("discover_models: false")); errConfigure != nil {
		t.Fatalf("configure: %v", errConfigure)
	}
	if cached := cachedModels(time.Duration(DefaultConfig().modelCacheTTL()) * time.Millisecond); len(cached) != 0 {
		t.Fatalf("cache = %#v, want it dropped after reconfiguration", cached)
	}
	if !strings.HasPrefix(Version, "0.") {
		t.Fatalf("version = %q, want a 0.x release line", Version)
	}
	setSettings(DefaultConfig())
}
