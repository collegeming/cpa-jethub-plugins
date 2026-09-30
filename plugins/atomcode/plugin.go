// Command atomcode implements the CLIProxyAPI native plugin for AtomCode
// (AtomGit's coding agent, `atomgit_atomcode/atomcode`).
//
// The package is split after plugins/trae:
//
//	config.go     provider keys, endpoint sets, settings
//	credential.go persisted credential shape, expiry
//	oauth.go      the AtomGit broker: login, poll, exchange, refresh
//	auth.go       auth.parse / auth.login.* / auth.refresh
//	models.go     the models-v2 catalogue and its bundled fallback
//	upstream.go   gateway request preparation, stream consumption, error mapping
//	executor.go   executor.* routes
//	credits.go    plan status, the daily claim cascade, 60-day usage
//	errors.go     upstream error classification
//	freshness.go  credential renewal before use
//	plugin.go     registration and the management route table
//	pluginui.go   the HTML pages served into CPA-Manager-Plus
//
// Unlike every other provider in this repository the upstream speaks OpenAI chat
// completions natively, so there is no translate.go: the client's body goes
// upstream verbatim and the upstream's frames come back verbatim.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/brandicons"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for AtomCode.
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
		Name:             "AtomCode",
		Version:          PluginVersion,
		Author:           "cpa-jethub",
		GitHubRepository: "https://github.com/collegeming/cpa-jethub-plugins",
		Logo:             brandicons.AtomCode,
		ConfigFields:     configFieldsForHost(),
	}
	capabilities := abiboot.Capabilities{
		ModelRegistrar:        true,
		ModelProvider:         true,
		AuthProvider:          true,
		Executor:              true,
		ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
		ExecutorInputFormats:  []string{"chat-completions"},
		ExecutorOutputFormats: []string{"chat-completions"},
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

// Shutdown drops any in-flight login sessions.
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
// `/v0/management/atomcode/checkin` on the management API and
// `/v0/resource/plugins/atomcode/status` on the resource path management clients
// embed. Only the last segment identifies the route in both cases.
func managementRoute(path string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return "/" + trimmed
}

// handleManagementRegister declares the status, login and check-in entries that
// management clients show. This plugin contributes NO sidebar entry.
//
// Two mounts with different rules, both determined by the host:
//
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/atomcode/<path>`, which is what CPA-Manager-Plus
//     embeds in its iframe. The repository's ONE Menu belongs to the hub plugin,
//     so the status page is declared here as a Menu-less ResourceRoute instead
//     and is reached from the hub's channel overview;
//   - any other route lands in the GLOBAL `/v0/management/<path>` namespace, so
//     its path must be prefixed with the provider key or a collision with another
//     plugin is silently skipped. Those routes exist for scripts and return JSON.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/" + ProviderKey + "/status",
				Description: "AtomCode 账号状态（JSON，脚本用）"},
			{Method: http.MethodPost, Path: "/" + ProviderKey + "/checkin",
				Description: "领取 AtomCode 每日免费套餐（JSON，脚本用）"},
		},
		// Browser-reachable pages that must NOT become sidebar entries: a
		// ResourceRoute only shows in the manager nav when it carries a Menu.
		Resources: []pluginapi.ResourceRoute{
			{Path: "/status", Description: "账号、凭据有效期、套餐额度与可用模型（由 hub 的渠道总览链接进入）"},
			{Path: "/login", Description: "浏览器登录 AtomGit 账号（两步式：先取链接，再检查结果）"},
			{Path: "/checkin", Description: "查询套餐状态并执行每日领取（含 60 天用量）"},
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
				"action": request.Query.Get("action"),
				"hint":   "GET ?action=start 发起登录；GET ?action=poll&state=<state> 查询结果",
			}), nil
		}
		return renderLoginPage(h, request), nil

	case "/checkin":
		if wantsJSON(request) {
			return checkinJSON(h, request), nil
		}
		return renderCheckinPage(h, request), nil
	}

	return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "unknown AtomCode management route"}), nil
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

// wantsJSON reports whether a management request asked for JSON rather than the
// HTML page. `?format=json` wins; otherwise an explicit Accept header that
// refuses text/html does. An absent Accept header means a browser.
func wantsJSON(request pluginapi.ManagementRequest) bool {
	if value := strings.ToLower(strings.TrimSpace(request.Query.Get("format"))); value != "" {
		return value == "json"
	}
	accept := strings.ToLower(request.Headers.Get("Accept"))
	if accept == "" {
		return false
	}
	return !strings.Contains(accept, "text/html")
}
