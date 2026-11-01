package main

import (
	"encoding/json"
	htmltemplate "html/template"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/brandicons"
	"github.com/collegeming/cpa-jethub-plugins/plugins/codearts/pool"
	"github.com/collegeming/cpa-jethub-plugins/plugins/codearts/reg"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	// One sidebar entry (/pool) shows both pool and registrar. /reg stays a
	// Menu-less resource that renders the same combined surface.
	pool.ExtraPageCards = func(h *abiboot.Host) []htmltemplate.HTML {
		return reg.PageCards()
	}
	pool.RefreshSeconds = func() int {
		if reg.IsJobRunning() {
			return 4
		}
		return 0
	}
	reg.ExtraPageCards = func(h *abiboot.Host) []htmltemplate.HTML {
		return pool.PageCards(h)
	}
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
		// The site root favicon answers with HTML, not an image; the portal's own
		// /favicon.ico is the one that actually serves an icon.
		Logo:         brandicons.CodeArts,
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
	cfg := ConfigFromYAML(configYAML)
	setSettings(cfg)
	// The background catalogue refresh is restarted on every Configure, so a
	// changed interval takes effect on a config reload instead of at the next
	// process start.
	startCatalogueScheduler(cfg)
	// The pool scanner and the registrar share this plugin's one lifecycle.
	pool.ApplyConfig(cfg.Pool)
	reg.SetConfig(cfg.Reg)
	return nil
}

// Quiesce stops the background refresh: the host calls it before unloading the
// plugin, and a tick firing against a closed instance would only log failures.
func (p *plugin) Quiesce() {
	stopCatalogueScheduler()
	pool.Shutdown()
}

// Shutdown releases the loopback callback listeners and the catalogue cache.
func (p *plugin) Shutdown() {
	stopCatalogueScheduler()
	pool.Shutdown()
	shutdownLoginSessions()
	discoveredModels.reset()
}

// configFieldsForHost converts the settings description into the host type:
// the provider's own fields plus the pool's ("pool_*") and the registrar's
// ("reg_*") settings, all editable inside this plugin's single config block.
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
	for _, field := range pool.HostConfigFields() {
		field.Name = "pool_" + field.Name
		out = append(out, field)
	}
	for _, field := range reg.HostConfigFields() {
		field.Name = "reg_" + field.Name
		out = append(out, field)
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
// The host passes the full incoming path, which differs per mount:
// `/v0/management/codearts/checkin` for the script route and
// `/v0/resource/plugins/codearts/status` for the page the hub links to. Only the
// last segment identifies the route in both cases.
func managementRoute(path string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return "/" + trimmed
}

// handleManagementRegister declares the status/login pages as Menu-less
// resources, the script endpoints, and exactly ONE sidebar ops entry (/pool).
//
// Three mounts, all decided by the host:
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/<id>/<path>` AND becomes its own sidebar entry in
//     CPA-Manager-Plus. The manager renders one nav item per menu route and does
//     not group them by plugin, so this repository gives that one entry to the
//     hub plugin and none to any provider: the sidebar is a single "Jet Hub" row
//     that links to `/v0/resource/plugins/codearts/status`.
//   - a ResourceRoute is registered under the same prefix and is listed in the
//     sidebar only when it carries a Menu. Leaving Menu empty keeps the page
//     browser-reachable — the hub and the login page link to it — without adding
//     a nav item.
//   - any other route is registered under `/v0/management/<path>`, which is a
//     GLOBAL namespace shared with every other plugin and with the host's own
//     endpoints. A collision there is skipped with a warning, so those paths are
//     prefixed with the provider key.
//
// Login deliberately has no sidebar entry either: the manager's own "OAuth 登录"
// page discovers every plugin that declares the auth-provider capability and
// drives `auth.login.start` / `auth.login.poll` itself, then saves the
// credential.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	routes := []pluginapi.ManagementRoute{
		{Method: http.MethodPost, Path: "/" + ProviderKey + "/checkin", Description: "执行每日签到（脚本与 API 用，返回 JSON）"},
		{Method: http.MethodGet, Path: "/" + ProviderKey + "/catalog", Description: "刷新模型目录并发布到宿主（JSON；等价于状态页的「刷新目录」按钮）"},
	}
	// Pool owns the single sidebar ops page (/pool). Registrar contributes
	// only its POST /codearts/register action; /reg is a Menu-less resource
	// that shows the same combined page.
	routes = append(routes, pool.ManagementRegister()...)
	routes = append(routes, reg.ManagementRegister()...)
	return pluginapi.ManagementRegistrationResponse{
		Routes: routes,
		Resources: []pluginapi.ResourceRoute{
			{Path: "/status", Description: "账号、额度与签到状态（由 hub 的渠道总览链接进入）"},
			{Path: "/login", Description: "浏览器登录 CodeArts 账号（由状态页或 OAuth 登录页进入）"},
			{Path: "/reg", Description: "自动注册（已并入运维页；直达仍可用）"},
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
	case "/pool", "/scan":
		return pool.ManagementHandle(h, raw)
	case "/reg", "/register":
		return reg.ManagementHandle(h, raw)

	case "/status":
		switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
		case "refresh-catalog":
			return catalogueRefreshPage(h, request), nil
		}
		if wantsJSON(request) {
			return statusJSON(h, request), nil
		}
		return renderStatusPage(h, request), nil

	case "/catalog":
		return catalogueRefreshJSON(h, request), nil

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
