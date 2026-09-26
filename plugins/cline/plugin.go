// Package main is the CPA native plugin adapter for Cline.
//
// It is the Go port of the Jet-Hub `cline` provider (a TypeScript DSH plugin),
// covering the whole method surface a CPA plugin can expose: WorkOS device-code
// login, credential refresh, the dual model catalogue, the SSE executor, the
// quota (balance) provider and one management page.
//
// What makes this provider unusual, and therefore what most of the comments in
// this package are about:
//
//   - the `workos:` token prefix is load-bearing. `Authorization: Bearer
//     workos:<jwt>` answers 200; the same token without the prefix answers 401
//     with a body that blames the client version (`cline.ts:166-176`);
//   - the device-code poll decides from the response BODY, not the HTTP status:
//     WorkOS answers 400 for `authorization_pending`, and `slow_down` accumulates
//     its penalty (`cline-oauth.ts:266-291`);
//   - a region-restricted 403 must be recognised BEFORE any refresh attempt,
//     because refreshing cannot help and the resulting AUTH is rendered to the
//     user as "API key invalid" (`cline-adapter.ts:609-646`);
//   - free models are a server-side marketing list, fetched from an endpoint the
//     plugin trusts over its own static table (`cline-models.ts:85-94`).
//
// There is no signing, no PKCE, no device fingerprint and no local callback
// listener anywhere in this provider: it is a plain HTTPS + bearer-token job
// (spec §9).
package main

import (
	"encoding/json"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/brandicons"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for the Cline provider.
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
func (p *plugin) Registration() abiboot.Registration {
	metadata := pluginapi.Metadata{
		Name:             DisplayName,
		Version:          Version,
		Author:           Author,
		GitHubRepository: Repository,
		// The vendor publishes no stable logo asset this repository can point
		// at, so none is claimed.
		// The icon the Cline site itself links. An empty logo renders nothing in
		// the panel, which is what every plugin looked like before this was set.
		Logo:         brandicons.Cline,
		ConfigFields: configFieldsForHost(),
	}
	capabilities := abiboot.Capabilities{
		ModelRegistrar:        true,
		ModelProvider:         true,
		AuthProvider:          true,
		Executor:              true,
		ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
		ExecutorInputFormats:  []string{"chat-completions"},
		ExecutorOutputFormats: []string{"chat-completions"},
		RequestTranslator:     true,
		ResponseTranslator:    true,
		QuotaProvider:         true,
		ManagementAPI:         true,
	}
	return abiboot.NewRegistration(metadata, capabilities)
}

// Configure applies the instance settings delivered by the host.
func (p *plugin) Configure(configYAML []byte) error {
	setSettings(ConfigFromYAML(configYAML))
	return nil
}

// Quiesce is a no-op: the adapter holds no background workers.
func (p *plugin) Quiesce() {}

// Shutdown releases the login sessions and the catalogue cache.
func (p *plugin) Shutdown() {
	shutdownLoginSessions()
	discoveredModels.reset()
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

// transportFor resolves the upstream transport for one host invocation.
//
// It is a package variable rather than a direct call so the unit tests can drive
// the whole login and execution path against a fake transport; production always
// resolves to hostDo, which routes through `host.http.do` and therefore inherits
// the host's proxy, TLS and request-log settings.
var transportFor = hostDo

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

// jsonTime renders a timestamp for the machine-readable payloads.
func jsonTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
