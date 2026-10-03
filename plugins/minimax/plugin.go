// Package main is the CPA native plugin adapter for MiniMax Code (中国版).
//
// It is this repository's first provider speaking the **Anthropic Messages**
// protocol, and the shape of the plugin follows from that. The executor
// declares `anthropic` on both sides, so the host converts the client's
// OpenAI-format request into an Anthropic Messages body before the plugin is
// called, and converts the Anthropic SSE the plugin returns back into whatever
// the client asked for. What the plugin owns is therefore narrow and specific:
//
//   - the live model catalogue plus a four-entry measured fallback (the vendor's
//     own static table omits the M3.1 preview, so this provider's fallback adds
//     it deliberately);
//   - the thinking matrix, which is genuinely model-dependent and includes a
//     hard HTTP 400 for one model (`messages.go` and `product.go` carry the
//     measured evidence);
//   - the device-code login and the refresh grant;
//   - the daily check-in and the credit ledger.
//
// ⚠️ Verification status, stated plainly because the two halves differ.
// INFERENCE was measured end to end: all four models answered HTTP 200 with
// text, thinking and structured `tool_use` blocks, images and effort levels
// were measured working. LOGIN and REFRESH were NOT: the reference's round trip
// was only ever unit tested, every live probe read an existing desktop-client
// token, and there is no MiniMax client on this machine. The login code is a
// faithful port of the source, not a measured result.
package main

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for the MiniMax Code provider.
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

func setSettings(cfg Config) { settingsSlot.Store(cfg) }

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
// ## ⚠️ The declared executor formats, and the evidence for the direction
//
// `anthropic` on BOTH sides, deliberately. The host maps the literal
// `"anthropic"` onto Claude (`normalizeExecutorFormatName` in
// `internal/pluginhost/adapters.go`), and `executorAdapter.prepareExecutorCall`
// then does two things that fix the direction:
//
//	requested := executorInputFormat(req, opts)      // what the client sent
//	inputFormat     := selectExecutorInputFormat(requested)   // the plugin's
//	outputFormat    := selectExecutorOutputFormat(requested, inputFormat)
//	if requested != inputFormat {
//	    nativeReq.Payload = TranslateRequest(requested, inputFormat, …)
//	}
//	nativeReq.Format = outputFormat
//
// So a client that speaks Chat Completions has its payload converted
// OpenAI→Claude BEFORE the executor sees it (`internal/translator/claude/openai/
// chat-completions` registers exactly that direction: `Register(OpenAI, Claude,
// ConvertOpenAIRequestToClaude, …)`), and the Anthropic SSE coming back is
// converted Claude→OpenAI by `ConvertClaudeResponseToOpenAI`. Declaring
// `anthropic` for the input as well would mean the host handed the plugin the
// client's raw OpenAI body and the plugin had to become a second Anthropic
// translator — more code and a larger defect surface for no gain.
//
// ⚠️ What that choice does NOT do is remove the need for `messages.go`: the
// streaming frame shape the host expects differs between an OpenAI-format
// output (bare bytes, which the host frames) and a Claude-format output
// (verbatim bytes, which must therefore BE complete SSE). See the long comment
// at the top of `executor.go`.
func (p *plugin) Registration() abiboot.Registration {
	metadata := pluginapi.Metadata{
		Name:             DisplayName,
		Version:          Version,
		Author:           Author,
		GitHubRepository: Repository,
		Logo:             Logo,
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
	setSettings(cfg)
	// A changed setting may change what the live catalogue is allowed to be, so
	// the cached discovery result is dropped rather than reused. In-flight login
	// sessions survive: they were authorised under the previous settings and
	// cancelling them would strand a user mid-flow.
	resetDiscoveredModels()
	// The background catalogue refresh is restarted on every Configure, so a
	// changed interval takes effect on a config reload instead of at the next
	// process start.
	startCatalogueScheduler(cfg)
	return nil
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
