// Command trae implements the CLIProxyAPI native plugin for ByteDance TRAE.
//
// The package is split after plugins/codearts:
//
//	config.go     provider keys, per-region endpoint sets, settings
//	credential.go persisted credential shape, expiry
//	session.go    deterministic device/identity derivation
//	headers.go    SOLO / check-in / OAuth request headers
//	oauth.go      login session, callback parsing, ExchangeToken
//	auth.go       auth.parse / auth.login.* / auth.refresh
//	translate.go  the OpenAI <-> SOLO translation core
//	models.go     catalog parsing, model discovery and gating
//	executor.go   executor.* and request/response translation routes
//	credits.go    daily check-in, credit balance, quota provider
//	errors.go     upstream error classification
//	plugin.go     registration and the management route table
//	pluginui.go   the HTML pages served into CPA-Manager-Plus
package main

import (
	"encoding/json"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/brandicons"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for TRAE.
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

// setSettings installs a new configuration.
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
		Name:             "TRAE",
		Version:          PluginVersion,
		Author:           "cpa-jethub",
		GitHubRepository: "https://github.com/collegeming/cpa-jethub-plugins",
		// The reference sources declare no logo asset. This is the icon the TRAE
		// site itself links, so it is the vendor's own mark rather than a guess.
		Logo:         brandicons.Trae,
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

// Shutdown releases the plugin's loopback callback listener.
func (p *plugin) Shutdown() { shutdownLoginSessions() }

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

// managementRoute reduces the request path to the registered route.
//
// The host passes the full incoming path, which differs per mount:
// `/v0/management/trae/checkin` on the management API and
// `/v0/resource/plugins/trae/status` on the resource path management clients
// embed. Only the last segment identifies the route in both cases.
func managementRoute(path string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return "/" + trimmed
}

// handleManagementRegister declares the status, login and check-in entries that
// management clients show.
//
// Two mounts with different rules, both determined by the host:
//
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/trae/<path>`, which is what CPA-Manager-Plus embeds
//     in its iframe;
//   - any other route lands in the GLOBAL `/v0/management/<path>` namespace, so
//     its path must be prefixed with the provider key or a collision with another
//     plugin is silently skipped. Those routes exist for scripts and return JSON.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			// Exactly one Menu route: the manager renders one sidebar entry per
			// menu route and does not group them by plugin, so extra menu routes
			// look like duplicates. Login lives on the manager's own OAuth page
			// (it discovers plugins declaring the auth-provider capability).
			{Method: http.MethodGet, Path: "/status", Menu: "TRAE",
				Description: "账号、凭据有效期、可用通道/模型与积分签到状态"},
			{Method: http.MethodGet, Path: "/" + ProviderKey + "/status",
				Description: "TRAE 账号状态（JSON，脚本用）"},
			{Method: http.MethodPost, Path: "/" + ProviderKey + "/checkin",
				Description: "执行 TRAE 每日签到（JSON，脚本用）"},
		},
		// Browser-reachable pages that must NOT become sidebar entries: a
		// ResourceRoute only shows in the manager nav when it carries a Menu.
		Resources: []pluginapi.ResourceRoute{
			{Path: "/login", Description: "浏览器登录 TRAE 账号（两步式：先取链接，再检查结果）"},
			{Path: "/checkin", Description: "查询并执行 TRAE 每日签到与积分余额"},
		},
	}, nil
}

// handleManagementHandle dispatches the routes declared above.
//
// The resource route embedded by management clients is dispatched as GET only, so
// every page action lives in the query string. Each handler serves both HTML and
// JSON; `?format=json` (or an Accept header without text/html) selects JSON.
func handleManagementHandle(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ManagementRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}

	switch managementRoute(request.Path) {
	case "/status":
		if wantsJSON(request) {
			return statusJSON(h, request), nil
		}
		return renderStatusPage(h, request), nil

	case "/login":
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusOK, map[string]any{
				"region": settings().Region,
				"action": request.Query.Get("action"),
				"hint":   "GET ?action=start 发起登录；GET ?action=poll&state=<state> 查询结果",
			}), nil
		}
		return renderLoginPage(h, request), nil

	case "/checkin":
		return checkinResponse(h, request), nil
	}

	return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "unknown TRAE management route"}), nil
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
