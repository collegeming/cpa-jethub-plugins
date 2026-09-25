package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cpa-jethub/plugins/internal/abiboot"
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
		GitHubRepository: "https://github.com/cpa-jethub/plugins",
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

// handleManagementRegister exposes the account status and check-in actions to
// the CPA management UI.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/status", Menu: "CodeArts", Description: "查询 CodeArts 账号额度与签到状态"},
			{Method: http.MethodPost, Path: "/checkin", Menu: "CodeArts", Description: "执行 CodeArts 每日签到"},
		},
	}, nil
}

// handleManagementHandle serves the routes declared above.
func handleManagementHandle(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ManagementRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	authIndex := strings.TrimSpace(request.Query.Get("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(request.Query.Get("auth_id"))
	}
	credential, errCredential := credentialForManagement(h, authIndex)
	if errCredential != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errCredential.Error()}), nil
	}

	switch strings.TrimSuffix(request.Path, "/") {
	case "/status":
		balance, errBalance := fetchCreditBalance(h, credential)
		if errBalance != nil {
			return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": errBalance.Error()}), nil
		}
		body := map[string]any{
			"credit_package": balance.IsCreditPackage,
			"remaining":      balance.Remaining,
			"used":           balance.Used,
			"total":          balance.Total,
		}
		if activity, errActivity := fetchDailyActivity(h, credential); errActivity == nil && activity != nil {
			body["daily_checkin"] = map[string]any{
				"campaign_id": activity.CampaignID,
				"claimable":   activity.Claimable,
				"status":      activity.Status,
			}
		}
		return jsonManagementResponse(http.StatusOK, body), nil

	case "/checkin":
		outcome, errClaim := claimDaily(h, credential)
		if errClaim != nil {
			return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": errClaim.Error()}), nil
		}
		body := map[string]any{
			"status":  outcome.Status,
			"message": outcome.Message,
			"amount":  outcome.Amount,
		}
		if outcome.Balance != nil {
			body["remaining"] = outcome.Balance.Remaining
		}
		return jsonManagementResponse(http.StatusOK, body), nil
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
