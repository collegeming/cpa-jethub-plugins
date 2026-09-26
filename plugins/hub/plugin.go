// Package main is the CPA native plugin that reproduces Jet-Hub's 一键签到
// (one-click daily check-in) across every provider plugin installed in the same
// CPA process.
//
// CPA gives each provider its own plugin, so the panel shows one sidebar entry
// per provider and there is no built-in cross-provider button. A plugin cannot
// call another plugin's Go code directly, and the management API
// (`/v0/management/...`) requires a key this plugin does not have. What IS
// reachable is the host's unauthenticated resource mount:
//
//	GET <host>/v0/resource/plugins/<pluginID>/<path>
//
// (internal/pluginhost/management.go: ServeResourceHTTP dispatches GET only and
// resolves the path against the routes each plugin declared through
// `management.register`). Every provider plugin already serves machine-readable
// JSON from those pages when handed `format=json`, and every check-in action is
// a query string on one of them. This plugin is therefore a thin orchestrator:
// it reads the host's credential list, drives each provider's own resource
// routes over loopback through `host.http.do`, and renders ONE aggregated table.
//
// Three rules shape the implementation:
//
//   - the plugin writes ONLY when the caller asked for it (`?action=checkin`).
//     A bare page load is a read-only report, and the tests assert no check-in
//     request is issued without that parameter;
//   - nothing is invented. Each provider is idempotent and reports "claimed" vs
//     "already claimed today" in its own vocabulary, so the verdict shown is the
//     field the provider actually returned, with the provider's own message next
//     to it. A provider without a check-in endpoint is shown as 不支持;
//   - all HTTP goes through the host (`host.http.do`). This package never
//     constructs an http.Client, which also makes the whole run testable by
//     swapping one package-level function.
package main

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for the one-click check-in hub.
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

// newPlugin wires the only two methods this plugin implements.
//
// The hub is not a model provider and not an auth provider: it owns no
// credentials of its own and never appears in the OAuth login list. It only
// needs the management surface.
func newPlugin() *plugin {
	p := &plugin{mux: abiboot.NewMux()}
	p.mux.
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
		// No logo is claimed: the plugin has no vendor of its own, and an empty
		// logo simply renders nothing.
		ConfigFields: configFieldsForHost(),
	}
	capabilities := abiboot.Capabilities{
		ManagementAPI: true,
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

// Shutdown drops the cached last-run snapshot.
func (p *plugin) Shutdown() { forgetLastRun() }

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

// doer performs one buffered call through the host transport.
//
// It is a function type, and transportFor below is a package variable, for the
// same reason plugins/cline and plugins/qoder do it: the whole orchestration —
// URL construction, JSON interpretation, aggregation — can then be exercised in
// tests with a scripted transport and without a socket.
type doer func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error)

// transportFor resolves the transport for one host invocation. Production always
// resolves to hostDo, which routes through `host.http.do` and therefore inherits
// the host's proxy, TLS and request-log settings.
var transportFor = hostDo

// hostDo routes a request through the host transport.
func hostDo(h *abiboot.Host) doer {
	return func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
		if h == nil {
			return nil, abiboot.Errorf("host_unavailable", "插件未通过宿主调用（缺少 host 句柄）")
		}
		return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: rawURL, Headers: headers, Body: body})
	}
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

// jsonTime renders a timestamp for the machine-readable documents.
func jsonTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
