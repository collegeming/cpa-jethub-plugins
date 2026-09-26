// Package main is the CPA native plugin adapter for the Tencent CodeBuddy /
// WorkBuddy product family.
//
// One CPA plugin key (`codebuddy`) serves four Jet-Hub products, selected by the
// `product` setting; see config.go for the product table and the endpoints each
// one uses. The method surface mirrors plugins/codearts (the reference
// implementation): auth, model, executor, translate, quota and management.
package main

import (
	"strings"
	"sync/atomic"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for the CodeBuddy family.
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
func Registration() abiboot.Registration {
	return abiboot.NewRegistration(pluginapi.Metadata{
		Name:             DisplayName,
		Version:          Version,
		Author:           Author,
		GitHubRepository: Repository,
		// Follow the product this build serves rather than the China home, so an
		// international variant does not advertise the China site's icon.
		Logo:         productLogoURL(),
		ConfigFields: configFieldsForHost(),
	}, abiboot.Capabilities{
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
	})
}

// Registration implements abiboot.Plugin.
func (p *plugin) Registration() abiboot.Registration { return Registration() }

// Configure applies the instance settings delivered by the host. The discovered
// model cache is dropped so a product switch never serves another product's
// model pool.
func (p *plugin) Configure(configYAML []byte) error {
	setSettings(ConfigFromYAML(configYAML))
	discoveredModels.reset()
	return nil
}

// Quiesce is a no-op: the adapter holds no background workers (upstream polling
// is driven by CPA through auth.login.poll).
func (p *plugin) Quiesce() {}

// Shutdown releases the in-flight login sessions.
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

// productLogoURL is the favicon of the product this build serves. It follows the
// configured API domain so the international variants link to their own site
// instead of the China one, falling back to that home when a product declares no
// domain.
func productLogoURL() string {
	if domain := strings.TrimSpace(ProductDefault().APIDomain); domain != "" {
		return "https://" + domain + "/favicon.ico"
	}
	return WebsiteHome + "/favicon.ico"
}
