package abiboot

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Host is the per-invocation handle passed to every method handler. It carries
// the host_callback_id that the host minted for this call; outbound host
// callbacks must echo it back so the host can attribute them to the caller.
type Host struct {
	CallbackID string
	PluginID   string
	Config     pluginapi.HostConfigSummary
}

// NewHost extracts the callback identity and host summary from an inbound
// method payload. Unknown fields are ignored, so it is safe for every method.
func NewHost(raw json.RawMessage) *Host {
	if len(raw) == 0 {
		return &Host{}
	}
	var probe struct {
		HostCallbackID string                      `json:"host_callback_id"`
		PluginID       string                      `json:"plugin_id"`
		HostUpper      pluginapi.HostConfigSummary `json:"Host"`
		HostLower      pluginapi.HostConfigSummary `json:"host"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return &Host{}
	}
	config := probe.HostLower
	if config.AuthDir == "" && config.ProxyURL == "" && len(config.ExcludedModels) == 0 {
		config = probe.HostUpper
	}
	return &Host{CallbackID: probe.HostCallbackID, PluginID: probe.PluginID, Config: config}
}

// HTTPDoRequest is the wire shape of host.http.do. The host decodes a flat
// object with lower-case snake_case keys (internal/pluginhost/host_callbacks.go,
// rpcHostHTTPRequest), so these tags are part of the contract.
type HTTPDoRequest struct {
	Method      string                     `json:"method,omitempty"`
	URL         string                     `json:"url,omitempty"`
	Headers     http.Header                `json:"headers,omitempty"`
	Body        []byte                     `json:"body,omitempty"`
	WireProfile *pluginapi.HTTPWireProfile `json:"wire_profile,omitempty"`
}

type hostHTTPPayload struct {
	HTTPDoRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// HTTPDo performs a buffered request through the host transport, inheriting the
// host's proxy, TLS and header-profile settings.
func (h *Host) HTTPDo(req HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
	payload := hostHTTPPayload{HTTPDoRequest: req, HostCallbackID: h.CallbackID}
	out := &pluginapi.HTTPResponse{}
	if err := HostCallInto(pluginabi.MethodHostHTTPDo, payload, out); err != nil {
		return nil, err
	}
	return out, nil
}

// HTTPStream is an incremental upstream response opened with host.http.do_stream.
type HTTPStream struct {
	StatusCode int
	Headers    http.Header
	StreamID   string

	host *Host
	done bool
}

// Header exposes the response headers of the opened stream.
func (s *HTTPStream) Header() http.Header { return s.Headers }

// HTTPDoStream opens a streaming request. The host returns as soon as it has
// the response head; body bytes are pulled with Read.
func (h *Host) HTTPDoStream(req HTTPDoRequest) (*HTTPStream, error) {
	payload := hostHTTPPayload{HTTPDoRequest: req, HostCallbackID: h.CallbackID}
	var wire struct {
		StatusCode int                         `json:"status_code"`
		Headers    http.Header                 `json:"headers,omitempty"`
		StreamID   string                      `json:"stream_id,omitempty"`
		Chunks     []pluginapi.HTTPStreamChunk `json:"chunks,omitempty"`
	}
	if err := HostCallInto(pluginabi.MethodHostHTTPDoStream, payload, &wire); err != nil {
		return nil, err
	}
	return &HTTPStream{StatusCode: wire.StatusCode, Headers: wire.Headers, StreamID: wire.StreamID, host: h}, nil
}

// Read returns the next body chunk, or io.EOF once the upstream stream is
// exhausted. Callers must Close the stream.
func (s *HTTPStream) Read() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	if s.StreamID == "" {
		s.done = true
		return nil, io.EOF
	}
	payload := struct {
		StreamID string `json:"stream_id"`
	}{StreamID: s.StreamID}
	var wire struct {
		Payload []byte `json:"payload,omitempty"`
		Error   string `json:"error,omitempty"`
		Done    bool   `json:"done,omitempty"`
	}
	if err := HostCallInto(pluginabi.MethodHostHTTPStreamRead, payload, &wire); err != nil {
		return nil, err
	}
	if wire.Done {
		s.done = true
	}
	if wire.Error != "" {
		return nil, Errorf("upstream_stream_error", "%s", wire.Error)
	}
	if len(wire.Payload) == 0 && wire.Done {
		return nil, io.EOF
	}
	return wire.Payload, nil
}

// Close releases the host-side stream. It is safe to call more than once.
func (s *HTTPStream) Close() error {
	if s.StreamID == "" || s.host == nil {
		return nil
	}
	payload := struct {
		StreamID string `json:"stream_id"`
	}{StreamID: s.StreamID}
	streamID := s.StreamID
	s.StreamID = ""
	_, err := HostCall(pluginabi.MethodHostHTTPStreamClose, payload)
	_ = streamID
	return err
}

// SaveAuth persists an auth file through the host so it appears in the CPA auth
// directory and management UI. storage must be a JSON object.
func (h *Host) SaveAuth(name string, storage json.RawMessage) (*pluginapi.HostAuthSaveResponse, error) {
	payload := pluginapi.HostAuthSaveRequest{Name: name, JSON: storage}
	out := &pluginapi.HostAuthSaveResponse{}
	if err := HostCallInto(pluginabi.MethodHostAuthSave, payload, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetAuth reads a previously stored auth file by its auth index.
func (h *Host) GetAuth(authIndex string) (*pluginapi.HostAuthGetResponse, error) {
	payload := pluginapi.HostAuthGetRequest{AuthIndex: authIndex}
	out := &pluginapi.HostAuthGetResponse{}
	if err := HostCallInto(pluginabi.MethodHostAuthGet, payload, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Log emits a structured log line into the CPA log stream.
func (h *Host) Log(level, message string, fields map[string]any) {
	payload := struct {
		HostCallbackID string         `json:"host_callback_id,omitempty"`
		Level          string         `json:"level,omitempty"`
		Message        string         `json:"message,omitempty"`
		Fields         map[string]any `json:"fields,omitempty"`
	}{HostCallbackID: h.CallbackID, Level: level, Message: message, Fields: fields}
	_, _ = HostCall(pluginabi.MethodHostLog, payload)
}
