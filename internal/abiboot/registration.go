package abiboot

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Capabilities is the wire form of the capability block. The host decodes it
// into rpcCapabilities (see internal/pluginhost/rpc_schema.go) so the JSON tags
// below must match that struct exactly.
type Capabilities struct {
	ModelRegistrar                bool                         `json:"model_registrar"`
	ModelProvider                 bool                         `json:"model_provider"`
	AuthProvider                  bool                         `json:"auth_provider"`
	FrontendAuthProvider          bool                         `json:"frontend_auth_provider"`
	FrontendAuthProviderExclusive bool                         `json:"frontend_auth_provider_exclusive"`
	Scheduler                     bool                         `json:"scheduler"`
	SchedulerAcrossPriorities     bool                         `json:"scheduler_across_priorities,omitempty"`
	ModelRouter                   bool                         `json:"model_router"`
	Executor                      bool                         `json:"executor"`
	ExecutorModelScope            pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats          []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats         []string                     `json:"executor_output_formats,omitempty"`
	RequestTranslator             bool                         `json:"request_translator"`
	RequestNormalizer             bool                         `json:"request_normalizer"`
	RequestInterceptor            bool                         `json:"request_interceptor"`
	RequestLifecyclePlugin        bool                         `json:"request_lifecycle_plugin"`
	ResponseTranslator            bool                         `json:"response_translator"`
	ResponseBeforeTranslator      bool                         `json:"response_before_translator"`
	ResponseAfterTranslator       bool                         `json:"response_after_translator"`
	ResponseInterceptor           bool                         `json:"response_interceptor"`
	StreamChunkInterceptor        bool                         `json:"response_stream_interceptor"`
	WebSocketResponseObserver     bool                         `json:"websocket_response_observer"`
	ThinkingApplier               bool                         `json:"thinking_applier"`
	UsagePlugin                   bool                         `json:"usage_plugin"`
	CommandLinePlugin             bool                         `json:"command_line_plugin"`
	ManagementAPI                 bool                         `json:"management_api"`
	QuotaProvider                 bool                         `json:"quota_provider"`
}

// Registration is returned from plugin.register and plugin.reconfigure.
type Registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  Capabilities       `json:"capabilities"`
}

// NewRegistration stamps the current schema version onto a registration so
// plugins never hard-code the negotiated revision.
func NewRegistration(meta pluginapi.Metadata, caps Capabilities) Registration {
	return Registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata:      meta,
		Capabilities:  caps,
	}
}
