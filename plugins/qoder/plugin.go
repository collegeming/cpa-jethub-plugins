// Package main is the CPA native plugin adapter for Qoder.
//
// SCOPE: this is a compiling scaffold. Registration and the full method surface
// are wired so the host can load and inspect the plugin, but every method other
// than auth.identifier is a stub that returns a not_implemented error. See
// docs/PORTING.md for the Jet-Hub source mapping this will be ported from.
package main

import (
	"encoding/json"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// ProviderKey is the stable provider identifier written into CPA auth files.
	ProviderKey = "qoder"
	// DisplayName is the human-readable name shown by management clients.
	DisplayName = "Qoder"
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
)

// plugin is the package-level abiboot.Plugin implementation.
type plugin struct {
	routes map[string]abiboot.Handler
}

// singleton is returned by Plugin; CPA loads the library once per process.
var singleton = newPlugin()

// Plugin returns the process-wide plugin singleton.
func Plugin() abiboot.Plugin { return singleton }

func newPlugin() *plugin {
	mux := abiboot.NewMux().
		On(pluginabi.MethodAuthIdentifier, authIdentifier).
		On(pluginabi.MethodAuthParse, stubHandler(pluginabi.MethodAuthParse)).
		On(pluginabi.MethodAuthLoginStart, stubHandler(pluginabi.MethodAuthLoginStart)).
		On(pluginabi.MethodAuthLoginPoll, stubHandler(pluginabi.MethodAuthLoginPoll)).
		On(pluginabi.MethodAuthRefresh, stubHandler(pluginabi.MethodAuthRefresh)).
		On(pluginabi.MethodModelRegister, stubHandler(pluginabi.MethodModelRegister)).
		On(pluginabi.MethodModelForAuth, stubHandler(pluginabi.MethodModelForAuth)).
		On(pluginabi.MethodExecutorIdentifier, stubHandler(pluginabi.MethodExecutorIdentifier)).
		On(pluginabi.MethodExecutorExecute, stubHandler(pluginabi.MethodExecutorExecute)).
		On(pluginabi.MethodExecutorExecuteStream, stubHandler(pluginabi.MethodExecutorExecuteStream)).
		On(pluginabi.MethodRequestTranslate, stubHandler(pluginabi.MethodRequestTranslate)).
		On(pluginabi.MethodResponseTranslate, stubHandler(pluginabi.MethodResponseTranslate))
	return &plugin{routes: mux.Routes()}
}

// Registration declares the scaffold's identity and the capabilities it will
// satisfy once the adapter is ported. The capability block is already the final
// shape so the host wires the plugin into every relevant extension point.
func (p *plugin) Registration() abiboot.Registration {
	return abiboot.NewRegistration(pluginapi.Metadata{
		Name:             DisplayName,
		Version:          Version,
		Author:           Author,
		GitHubRepository: Repository,
		Logo:             "",
		ConfigFields:     []pluginapi.ConfigField{},
	}, abiboot.Capabilities{
		ModelRegistrar:        true,
		ModelProvider:         true,
		AuthProvider:          true,
		Executor:              true,
		ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
		ExecutorInputFormats:  []string{"chat-completions"},
		ExecutorOutputFormats: []string{"chat-completions"},
		ManagementAPI:         true,
		QuotaProvider:         true,
	})
}

// Routes exposes the method table to abiboot.Dispatch.
func (p *plugin) Routes() map[string]abiboot.Handler { return p.routes }

// authIdentifier answers auth.identifier. The provider key is the identity the
// host uses to bind stored auth files to this plugin.
func authIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// stubHandler builds a clearly-marked placeholder for a method that is part of
// the declared capability surface but has not been ported yet.
func stubHandler(method string) abiboot.Handler {
	return func(_ *abiboot.Host, _ json.RawMessage) (any, error) {
		return nil, abiboot.Errorf("not_implemented", "%s is not implemented by the %s scaffold plugin", method, ProviderKey)
	}
}
