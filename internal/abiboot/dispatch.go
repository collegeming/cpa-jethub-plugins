package abiboot

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// Handler processes a single method invocation. The returned value is wrapped
// in a success envelope; returning an error produces a failure envelope.
type Handler func(h *Host, raw json.RawMessage) (any, error)

// Configurer is implemented by plugins that read their instance settings from
// the config_yaml subtree delivered with plugin.register / plugin.reconfigure.
type Configurer interface {
	Configure(configYAML []byte) error
}

// Quiescer is implemented by plugins that need to drain in-flight work.
type Quiescer interface{ Quiesce() }

// Shutdowner is implemented by plugins that hold background resources.
type Shutdowner interface{ Shutdown() }

// Plugin is the contract between a provider plugin and the ABI bootstrap.
type Plugin interface {
	Registration() Registration
	Routes() map[string]Handler
}

// Mux is a small helper for building the route table.
type Mux struct {
	routes map[string]Handler
}

// NewMux returns an empty route table.
func NewMux() *Mux { return &Mux{routes: map[string]Handler{}} }

// On registers a handler for a pluginabi method constant.
func (m *Mux) On(method string, handler Handler) *Mux {
	m.routes[method] = handler
	return m
}

// Routes exposes the accumulated table.
func (m *Mux) Routes() map[string]Handler { return m.routes }

// Dispatch routes one inbound call and always returns a complete envelope, so
// the cgo layer never has to synthesise an error reply itself.
func Dispatch(p Plugin, method string, payload []byte) []byte {
	result, err := dispatch(p, method, payload)
	if err != nil {
		return encodeError(err)
	}
	encoded, errEncode := OK(result)
	if errEncode != nil {
		return encodeError(errEncode)
	}
	return encoded
}

func dispatch(p Plugin, method string, payload []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if configurer, ok := p.(Configurer); ok {
			var lifecycle struct {
				ConfigYAML []byte `json:"config_yaml"`
			}
			if len(payload) > 0 {
				if errUnmarshal := json.Unmarshal(payload, &lifecycle); errUnmarshal != nil {
					return nil, Errorf("invalid_config", "decode plugin lifecycle request: %v", errUnmarshal)
				}
			}
			if errConfigure := configurer.Configure(lifecycle.ConfigYAML); errConfigure != nil {
				return nil, errConfigure
			}
		}
		return p.Registration(), nil
	case pluginabi.MethodPluginQuiesce:
		if quiescer, ok := p.(Quiescer); ok {
			quiescer.Quiesce()
		}
		return map[string]any{}, nil
	case pluginabi.MethodPluginShutdown:
		if shutdowner, ok := p.(Shutdowner); ok {
			shutdowner.Shutdown()
		}
		return map[string]any{}, nil
	}

	handler, ok := p.Routes()[method]
	if !ok {
		return nil, Errorf("unsupported_method", "method %s is not implemented by this plugin", method)
	}
	raw := json.RawMessage(payload)
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	return handler(NewHost(raw), raw)
}

// Decode unmarshals a method payload into a typed request, tolerating the extra
// host_callback_id field the host appends.
func Decode[T any](raw json.RawMessage) (T, error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, Errorf("invalid_request", "decode request: %v", err)
	}
	return out, nil
}

// Identifier is the shared reply shape for auth.identifier / executor.identifier.
type Identifier struct {
	Identifier string `json:"identifier"`
}

// IdentifierReply builds an identifier reply.
func IdentifierReply(id string) Identifier { return Identifier{Identifier: id} }
