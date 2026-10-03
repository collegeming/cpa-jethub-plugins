package abiboot

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credjson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Host is the per-invocation handle passed to every method handler. It carries
// the host_callback_id that the host minted for this call; outbound host
// callbacks must echo it back so the host can attribute them to the caller.
type Host struct {
	CallbackID string
	PluginID   string
	Config     pluginapi.HostConfigSummary
	// Incoming is the raw auth payload the host supplied for this invocation
	// (`RawJSON` on auth.parse, `StorageJSON` on auth.refresh and the model and
	// execution paths). It is the auth file as the host sees it, so SaveAuth can
	// carry forward the members this plugin does not own: the host reads
	// `priority` and `weight` from that same file to choose a credential, and a
	// rewrite that drops them silently resets the credential's routing tier.
	Incoming json.RawMessage
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
		// The auth structs carry no JSON tags, so their members arrive under the
		// Go field names; the quota and model structs tag the same members
		// snake_case. All spellings are []byte on the wire, so encoding/json
		// hands each one back already base64-decoded.
		RawJSON          []byte `json:"RawJSON"`
		RawJSONSnake     []byte `json:"raw_json"`
		StorageJSON      []byte `json:"StorageJSON"`
		StorageJSONSnake []byte `json:"storage_json"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return &Host{}
	}
	config := probe.HostLower
	if config.AuthDir == "" && config.ProxyURL == "" && len(config.ExcludedModels) == 0 {
		config = probe.HostUpper
	}
	host := &Host{CallbackID: probe.HostCallbackID, PluginID: probe.PluginID, Config: config}
	host.rememberIncoming(probe.RawJSON)
	host.rememberIncoming(probe.RawJSONSnake)
	host.rememberIncoming(probe.StorageJSON)
	host.rememberIncoming(probe.StorageJSONSnake)
	return host
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
//
// host.auth.save replaces the whole file (pluginapi.HostAuthSaveRequest carries
// only a name and a JSON blob), so the members the plugin does not define are
// carried forward from Incoming. Without that, every self-initiated refresh
// would erase host-owned routing members such as `priority` and `weight`, and
// the credential would silently fall back to the default tier.
//
// Incoming is empty on the paths whose payload carries no auth file — a device
// login poll is the common one, and a background refresh has no payload at all.
// The host's own copy of the file is then the only source for those members, so
// it is read back through host.auth.get before the save. That lookup is best
// effort: a first login has no stored file, and a lookup failure must not stop
// the credential from being written.
func (h *Host) SaveAuth(name string, storage json.RawMessage) (*pluginapi.HostAuthSaveResponse, error) {
	base := h.Incoming
	if len(bytes.TrimSpace(base)) == 0 {
		base = h.storedAuthFile(name)
	}
	payload := pluginapi.HostAuthSaveRequest{Name: name, JSON: credjson.MergePreserved(base, storage)}
	out := &pluginapi.HostAuthSaveResponse{}
	if err := HostCallInto(pluginabi.MethodHostAuthSave, payload, out); err != nil {
		return nil, err
	}
	return out, nil
}

// storedAuthFile returns the auth file the host currently holds for name, or
// nil when there is none. The match is on the file name, because the callers
// that need this (a login poll, a background refresh) know the target file but
// not its runtime index; host.auth.get is addressed by index, so the entry has
// to be resolved from the host's own list first.
func (h *Host) storedAuthFile(name string) json.RawMessage {
	target := strings.TrimSpace(name)
	if target == "" {
		return nil
	}
	entries, errList := h.ListAuth()
	if errList != nil {
		return nil
	}
	for _, entry := range entries {
		if !authEntryMatchesName(entry, target) {
			continue
		}
		if strings.TrimSpace(entry.AuthIndex) == "" {
			return nil
		}
		auth, errGet := h.GetAuth(entry.AuthIndex)
		if errGet != nil {
			return nil
		}
		return auth.JSON
	}
	return nil
}

// authEntryMatchesName reports whether an auth entry is the one backed by the
// given file name. A credential with no backing file has none of these members
// and is matched by its runtime identifier instead.
//
// filepath.Base is only applied to a non-empty path: Base("") is ".", and a
// target of "." would otherwise match an entry that has no file at all.
func authEntryMatchesName(entry pluginapi.HostAuthFileEntry, target string) bool {
	candidates := []string{entry.Name, entry.ID}
	if path := strings.TrimSpace(entry.Path); path != "" {
		candidates = append(candidates, filepath.Base(path))
	}
	for _, candidate := range candidates {
		if candidate != "" && candidate == target {
			return true
		}
	}
	return false
}

// GetAuth reads a previously stored auth file by its auth index.
//
// The payload is also remembered as this invocation's Incoming file, because
// management routes reach a credential this way rather than through a parse or
// refresh callback: a page that reads a quota and then persists a renewed token
// must still merge into the file the host is holding.
func (h *Host) GetAuth(authIndex string) (*pluginapi.HostAuthGetResponse, error) {
	payload := pluginapi.HostAuthGetRequest{AuthIndex: authIndex}
	out := &pluginapi.HostAuthGetResponse{}
	if err := HostCallInto(pluginabi.MethodHostAuthGet, payload, out); err != nil {
		return nil, err
	}
	h.rememberIncoming(out.JSON)
	return out, nil
}

// rememberIncoming records the auth file the host is holding when the
// invocation did not already carry it. The first known file wins: every save in
// one invocation targets the same credential.
func (h *Host) rememberIncoming(raw json.RawMessage) {
	if h == nil || len(bytes.TrimSpace(raw)) == 0 || len(bytes.TrimSpace(h.Incoming)) != 0 {
		return
	}
	h.Incoming = append(json.RawMessage(nil), raw...)
}

// ListAuth enumerates every credential the host currently tracks. The list is
// host-wide, so callers must filter it down to their own provider key.
func (h *Host) ListAuth() ([]pluginapi.HostAuthFileEntry, error) {
	payload := struct {
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}{HostCallbackID: h.CallbackID}
	out := struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}{}
	if errCall := HostCallInto(pluginabi.MethodHostAuthList, payload, &out); errCall != nil {
		return nil, errCall
	}
	return out.Files, nil
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
