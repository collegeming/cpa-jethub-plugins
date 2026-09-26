// Package main is the CPA native plugin adapter for LobsterAI (有道 LobsterAI).
//
// Both files in this directory are package main: main.go carries the exported
// cgo ABI and this file carries the abiboot.Plugin implementation. Keeping the
// plugin implementation in the same package is what lets
// `go build -buildmode=c-shared ./plugins/lobsterai` emit the shared object
// directly from this directory.
//
// LobsterAI is not part of the Tencent CodeBuddy family: it has its own login
// flow (loopback callback + authCode exchange), its own header set
// (Bearer + X-LobsterAI-Client-*), its own renewal payload (refreshToken plus a
// keyfrom identity block), its own check-in protocol and its own client version
// source. Ported from jethub-src/src/lobsterai*.ts; see docs/PORTING.md §6.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for the LobsterAI provider.
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
		Logo:             Logo,
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

// Quiesce is a no-op: the only background worker is the callback dispatcher, and
// stopping it early would close the published callback port while a browser
// redirect may still be on its way. Shutdown is the only place it is released.
func (p *plugin) Quiesce() {}

// Shutdown releases the plugin's long-lived loopback callback listener.
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

// handleRequestTranslate is an identity transform. The executor declares
// chat-completions on both sides, so the host performs any cross-protocol
// translation itself and never has a gap for this route to fill.
func handleRequestTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.RequestTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	return pluginapi.PayloadResponse{Body: request.Body}, nil
}

// handleResponseTranslate is an identity transform; see handleRequestTranslate.
func handleResponseTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ResponseTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	return pluginapi.PayloadResponse{Body: request.Body}, nil
}

// managementRoute reduces the request path to the route the plugin registered.
//
// The host passes the full incoming path, which differs between the two mounts:
// `/v0/management/lobsterai/checkin` on the management API and
// `/v0/resource/plugins/lobsterai/checkin` on the resource path management
// clients embed. Only the last segment identifies the route in both cases.
func managementRoute(path string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return "/" + trimmed
}

// handleManagementRegister declares exactly ONE sidebar entry (the status page)
// plus the browser pages and JSON endpoints behind it.
//
// Three mounts, all decided by the host:
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/<id>/<path>` AND becomes its own sidebar entry in
//     CPAMP. The manager renders one nav item per menu route and does not group
//     them by plugin, so every extra menu route looks like a duplicate entry.
//     That is why only the status page carries one.
//   - a ResourceRoute is registered under the same prefix but is listed in the
//     sidebar only when it carries a Menu. Leaving Menu empty keeps the page
//     browser-reachable (the status page links to it) without adding a nav item.
//   - any other route is registered under `/v0/management/<path>`, a GLOBAL
//     namespace, so its path must be prefixed with the provider key or a
//     collision is skipped with a warning.
//
// Login has no sidebar entry on purpose: the manager's own "OAuth 登录" page
// discovers every plugin that declares the auth-provider capability and drives
// `auth.login.start` / `auth.login.poll` itself, then saves the credential.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/status", Menu: "LobsterAI", Description: "账号、凭据有效期、模型参数与积分余额"},
			{Method: http.MethodPost, Path: "/" + ProviderKey + "/checkin", Description: "执行每日签到（脚本与 API 用，返回 JSON）"},
			{Method: http.MethodPost, Path: "/" + ProviderKey + "/login/start", Description: "发起登录并返回授权 URL（JSON）"},
			{Method: http.MethodPost, Path: "/" + ProviderKey + "/login/poll", Description: "轮询登录结果并保存凭据（JSON）"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/login", Description: "浏览器两步式登录（本地回调 + authCode 换 token）"},
			{Path: "/checkin", Description: "每日签到领取积分（客户端幂等）"},
		},
	}, nil
}

// handleManagementHandle dispatches the routes declared above.
//
// The resource route embedded by management clients is dispatched as GET only,
// so every page action is a link carrying a query string; the prefixed
// management routes exist for scripts and return JSON. Both representations
// share the same handlers.
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
				"hint":   "GET ?action=start 发起登录；GET ?action=poll&state=<state> 查询结果；POST /v0/management/lobsterai/login/start|poll 为脚本接口",
			}), nil
		}
		return renderLoginPage(h, request), nil

	case "/start":
		return loginStartResponse(h, request), nil

	case "/poll":
		return loginPollResponse(h, request), nil

	case "/checkin":
		return checkinResponse(h, request), nil
	}

	return jsonManagementResponse(http.StatusNotFound, map[string]any{
		"error": "unknown LobsterAI management route: " + request.Path,
	}), nil
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
