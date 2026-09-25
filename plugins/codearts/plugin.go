package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for the CodeArts provider.
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
		Name:             "CodeArts",
		Version:          PluginVersion,
		Author:           "cpa-jethub",
		GitHubRepository: "https://github.com/collegeming/cpa-jethub-plugins",
		Logo:             "https://www.huaweicloud.com/favicon.ico",
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

// Quiesce is a no-op: the adapter holds no background workers.
func (p *plugin) Quiesce() {}

// Shutdown releases the loopback callback listeners.
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

// handleModelRegister reports the static fallback catalog. It is used when no
// account is bound yet, so it must not touch the network.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelRegistrationResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// handleModelStatic is the model.static variant of the same catalog.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
}

// handleModelForAuth reports the catalog for one bound account, using the
// discovered listing when enabled and available.
func handleModelForAuth(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := settings()
	if !cfg.DiscoverModels {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
	}

	ttl := time.Duration(cfg.ModelCacheTTLMS) * time.Millisecond
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if cached := discoveredModels.get(ttl); len(cached) > 0 {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: cached}, nil
	}

	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		// Fall back rather than failing the model listing entirely.
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
	}
	models := discoverModels(h, credential)
	if len(models) == 0 {
		return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos()}, nil
	}
	discoveredModels.put(models)
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: models}, nil
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
// `/v0/management/codearts/status` on the management API and
// `/v0/resource/plugins/codearts/status` on the resource path that management
// clients embed. Only the last segment identifies the route in both cases.
func managementRoute(path string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return "/" + trimmed
}

// handleManagementRegister declares exactly ONE sidebar entry (the status page)
// plus the browser pages and script endpoints behind it.
//
// Three mounts, all decided by the host:
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/<id>/<path>` AND becomes its own sidebar entry in
//     CPA-Manager-Plus. That is why only the status page carries one: the
//     manager renders one nav item per menu route and does not group them by
//     plugin, so every extra menu route is a duplicate-looking entry.
//   - a ResourceRoute is registered under the same prefix but is listed in the
//     sidebar only when it carries a Menu. Leaving Menu empty keeps the page
//     browser-reachable (the status page links to it) without adding a nav item.
//   - any other route is registered under `/v0/management/<path>`, which is a
//     GLOBAL namespace shared with every other plugin and with the host's own
//     endpoints. A collision there is skipped with a warning, so those paths are
//     prefixed with the provider key.
//
// Login deliberately has no sidebar entry: the manager's own "OAuth 登录" page
// discovers every plugin that declares the auth-provider capability and drives
// `auth.login.start` / `auth.login.poll` itself, then saves the credential.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/status", Menu: "CodeArts", Description: "账号、额度与签到状态"},
			{Method: http.MethodPost, Path: "/" + ProviderKey + "/checkin", Description: "执行每日签到（脚本与 API 用，返回 JSON）"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/login", Description: "浏览器登录 CodeArts 账号（由状态页或 OAuth 登录页进入）"},
		},
	}, nil
}

// handleManagementHandle dispatches the routes declared above.
//
// The resource route used by management clients is dispatched as GET only, so
// every action lives in the query string; the POST route exists for scripts and
// returns JSON. Both representations share the same handlers.
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
				"flow":   settings().Flow,
				"action": request.Query.Get("action"),
				"hint":   "GET ?action=login 发起登录；GET ?action=poll&state=<state> 查询结果",
			}), nil
		}
		return renderLoginPage(h, request), nil

	case "/checkin":
		return checkinJSON(h, request), nil
	}

	return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "unknown CodeArts management route"}), nil
}

// credentialForManagement loads the credential selected by an auth index.
func credentialForManagement(h *abiboot.Host, authIndex string) (*Credential, error) {
	if h == nil || authIndex == "" {
		return nil, abiboot.Errorf("missing_auth", "请通过 auth_index 指定 CodeArts 账号")
	}
	auth, errGet := h.GetAuth(authIndex)
	if errGet != nil {
		return nil, errGet
	}
	return ParseCredential(auth.JSON)
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
