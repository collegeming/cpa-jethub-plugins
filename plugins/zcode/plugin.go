// Command zcode builds the CPA c-shared plugin library for the ZCode (智谱 z.ai)
// provider.
//
// Both files in this directory are package main: main.go carries the exported
// cgo ABI and plugin.go carries the abiboot.Plugin implementation. Keeping the
// plugin implementation in the same package is what lets
// `go build -buildmode=c-shared ./plugins/zcode` emit the shared object directly
// from this directory.
//
// What this provider is, in one paragraph: ZCode's free-quota channel speaks
// **Anthropic Messages** and nothing else — the OpenAI-shaped
// `/v1/chat/completions` under `zcode-plan` does not exist — and it admits a
// request only when the body carries the official client's identity block as a
// structured `system` array. Credentials are static (the JWT has no `exp`), a
// device id is required but not validated, and the daily allowance is claimed
// through `event/report` → `billing/preview` → `billing/claim`. The plugin never
// mints a captcha: that machinery was measured to be unnecessary for inference and
// the header now appears only on the claim endpoint.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for the ZCode provider.
type plugin struct {
	mux *abiboot.Mux
}

var (
	instance     *plugin
	settingsSlot atomic.Value // holds Config
)

func init() {
	settingsSlot.Store(DefaultConfig())
}

// settings returns the live instance configuration.
func settings() Config {
	if value, ok := settingsSlot.Load().(Config); ok {
		return value
	}
	return DefaultConfig()
}

// setSettings installs a configuration.
func setSettings(cfg Config) { settingsSlot.Store(cfg) }

// nowFunc is the clock, indirected so a test can pin "today".
var nowFunc = time.Now

// workingDirectory is the process working directory, indirected for the same
// reason: the environment section embeds it.
var workingDirectory = func() string {
	dir, errGetwd := os.Getwd()
	if errGetwd != nil {
		return "."
	}
	return dir
}

// Plugin returns the process-wide plugin singleton.
func Plugin() abiboot.Plugin {
	if instance == nil {
		instance = newPlugin()
	}
	return instance
}

// newPlugin wires every method this provider implements.
func newPlugin() *plugin {
	p := &plugin{mux: abiboot.NewMux()}
	p.mux.
		On(pluginabi.MethodAuthIdentifier, handleAuthIdentifier).
		On(pluginabi.MethodAuthParse, handleAuthParse).
		On(pluginabi.MethodAuthLoginStart, handleAuthLoginStart).
		On(pluginabi.MethodAuthLoginPoll, handleAuthLoginPoll).
		On(pluginabi.MethodAuthRefresh, handleAuthRefresh).
		On(pluginabi.MethodModelRegister, handleModelRegister).
		On(pluginabi.MethodModelStatic, handleModelStatic).
		On(pluginabi.MethodModelForAuth, handleModelForAuth).
		On(pluginabi.MethodExecutorIdentifier, handleExecutorIdentifier).
		On(pluginabi.MethodExecutorExecute, handleExecutorExecute).
		On(pluginabi.MethodExecutorExecuteStream, handleExecutorExecuteStream).
		On(pluginabi.MethodExecutorCountTokens, handleExecutorCountTokens).
		On(pluginabi.MethodRequestTranslate, handleRequestTranslate).
		On(pluginabi.MethodResponseTranslate, handleResponseTranslate).
		On(pluginabi.MethodQuotaIdentifier, handleQuotaIdentifier).
		On(pluginabi.MethodQuotaDescribe, handleQuotaDescribe).
		On(pluginabi.MethodQuotaFetch, handleQuotaFetch).
		On(pluginabi.MethodQuotaReset, handleQuotaReset).
		On(pluginabi.MethodManagementRegister, handleManagementRegister).
		On(pluginabi.MethodManagementHandle, handleManagementHandle)
	return p
}

// Routes exposes the method table to the ABI bootstrap.
func (p *plugin) Routes() map[string]abiboot.Handler { return p.mux.Routes() }

// Registration declares identity, settings and capabilities.
//
// `executor_model_scope` is `oauth`: every ZCode credential comes from a login
// flow, so the executor has no static-model path.
func (p *plugin) Registration() abiboot.Registration {
	metadata := pluginapi.Metadata{
		Name:             DisplayName,
		Version:          Version,
		Author:           Author,
		GitHubRepository: Repository,
		Logo:             logo,
		ConfigFields:     configFieldsForHost(),
	}
	capabilities := abiboot.Capabilities{
		ModelRegistrar:        true,
		ModelProvider:         true,
		AuthProvider:          true,
		Executor:              true,
		ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
		ExecutorInputFormats:  []string{"anthropic"},
		ExecutorOutputFormats: []string{"anthropic"},
		RequestTranslator:     true,
		ResponseTranslator:    true,
		QuotaProvider:         true,
		ManagementAPI:         true,
	}
	return abiboot.NewRegistration(metadata, capabilities)
}

// Configure applies the instance settings delivered by the host.
func (p *plugin) Configure(configYAML []byte) error {
	cfg := ConfigFromYAML(configYAML)
	// The version is a header value, not a hard dependency: when the user leaves
	// it empty, probe the installed official client and keep the built-in
	// fallback if that fails. A failed probe must never make the provider
	// unavailable.
	if trimmed := trimmedVersion(cfg.AppVersion); trimmed == "" {
		cfg.AppVersion = detectInstalledAppVersion()
	}
	setSettings(cfg)
	// A changed setting may change the catalogue, so the cached discovery result
	// is dropped rather than reused.
	resetDiscoveredModels()
	// The background catalogue refresh is restarted on every Configure, so a
	// changed interval takes effect on a config reload instead of at the next
	// process start.
	startCatalogueScheduler(cfg)
	return nil
}

// trimmedVersion normalises a version string.
func trimmedVersion(value string) string {
	if value == DefaultAppVersion {
		// The built-in default is a valid value, but it is also the "unset"
		// marker the probe replaces — a user who explicitly wants it can set
		// `app_version` to it and gets the same outcome either way.
		return ""
	}
	return strings.TrimSpace(value)
}

// Quiesce stops the background refresh: the host calls it before unloading the
// plugin, and a tick firing against a closed instance would only log failures.
func (p *plugin) Quiesce() { stopCatalogueScheduler() }

// Shutdown stops the background refresh, then releases the in-flight login
// sessions and the catalogue cache.
func (p *plugin) Shutdown() {
	stopCatalogueScheduler()
	shutdownLoginSessions()
	resetDiscoveredModels()
}

// configFieldsForHost converts the settings description into the host type.
func configFieldsForHost() []pluginapi.ConfigField {
	fields := ConfigFields()
	out := make([]pluginapi.ConfigField, 0, len(fields))
	for _, field := range fields {
		out = append(out, pluginapi.ConfigField{
			Name:        field.Name,
			Type:        pluginapi.ConfigFieldType(field.Type),
			EnumValues:  field.EnumValues,
			Description: field.Description,
		})
	}
	return out
}

// jsonManagementResponse renders a management API reply.
func jsonManagementResponse(status int, body any) pluginapi.ManagementResponse {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		encoded = []byte(`{"error":"failed to encode response"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       encoded,
	}
}

// jsonTime renders a timestamp for the machine-readable status payload.
func jsonTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// sleepMillis blocks for a linear-backoff step.
//
// The executor runs inside a host invocation, so this is a bounded, synchronous
// wait; the value is capped so a misconfigured retry base cannot stall the host
// for minutes.
func sleepMillis(milliseconds int) {
	if milliseconds <= 0 {
		return
	}
	if milliseconds > 30_000 {
		milliseconds = 30_000
	}
	time.Sleep(time.Duration(milliseconds) * time.Millisecond)
}
