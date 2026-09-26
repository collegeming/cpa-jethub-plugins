package main

import (
	"sync/atomic"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// plugin implements the abiboot.Plugin contract for the Raccoon Work provider.
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
	cfg := ConfigFromYAML(configYAML)
	setSettings(cfg)
	// A changed setting may change what the live catalogue is allowed to be, so
	// the cached discovery result is dropped rather than reused.
	resetDiscoveredModels()
	return nil
}

// Quiesce is a no-op: the adapter holds no background workers.
func (p *plugin) Quiesce() {}

// Shutdown releases the in-flight login sessions and the catalogue cache.
func (p *plugin) Shutdown() {
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
