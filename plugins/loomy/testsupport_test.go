package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file provides the in-process fake host transport used by every test. No
// test touches the network: HTTP calls issued through *abiboot.Host are answered
// from a scripted handler, and the auth/log callbacks are recorded.
//
// The plugin reaches the host through the single abiboot host caller, so one hook
// covers host.http.do, host.auth.* and host.log.

// errFakeTransport simulates an unreachable upstream.
var errFakeTransport = errors.New("fake transport failure")

// fakeHost is a scriptable host transport.
type fakeHost struct {
	mu sync.Mutex

	// do answers host.http.do. A nil do fails the test when it is called, which
	// is how "this path must not touch the network" is asserted.
	do func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error)
	// requests records every outbound call in order.
	requests []abiboot.HTTPDoRequest
	// auths maps an auth index to its stored JSON for host.auth.get.
	auths map[string][]byte
	// files is what host.auth.list reports.
	files []pluginapi.HostAuthFileEntry
	// saved records every host.auth.save call by name.
	saved map[string][]byte
	// logs records every host.log payload.
	logs []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{auths: map[string][]byte{}, saved: map[string][]byte{}}
}

// install registers the fake transport for the duration of the test.
func (f *fakeHost) install(t *testing.T) {
	t.Helper()
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDo:
			var payload abiboot.HTTPDoRequest
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.mu.Lock()
			f.requests = append(f.requests, payload)
			handler := f.do
			f.mu.Unlock()
			if handler == nil {
				return nil, fmt.Errorf("unexpected host.http.do for %s", payload.URL)
			}
			response, errDo := handler(payload)
			if errDo != nil {
				return nil, errDo
			}
			return envelopeOK(response)

		case pluginabi.MethodHostAuthList:
			f.mu.Lock()
			files := append([]pluginapi.HostAuthFileEntry(nil), f.files...)
			f.mu.Unlock()
			return envelopeOK(map[string]any{"files": files})

		case pluginabi.MethodHostAuthGet:
			var payload pluginapi.HostAuthGetRequest
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.mu.Lock()
			stored, ok := f.auths[payload.AuthIndex]
			f.mu.Unlock()
			if !ok {
				return nil, abiboot.Errorf("auth_not_found", "no auth %s", payload.AuthIndex)
			}
			return envelopeOK(pluginapi.HostAuthGetResponse{AuthIndex: payload.AuthIndex, JSON: stored})

		case pluginabi.MethodHostAuthSave:
			var payload pluginapi.HostAuthSaveRequest
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.mu.Lock()
			f.saved[payload.Name] = payload.JSON
			f.mu.Unlock()
			return envelopeOK(pluginapi.HostAuthSaveResponse{Name: payload.Name, Path: "/auths/" + payload.Name})

		case pluginabi.MethodHostLog:
			f.mu.Lock()
			f.logs = append(f.logs, string(request))
			f.mu.Unlock()
			return envelopeOK(map[string]any{})

		default:
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
	})
	t.Cleanup(func() {
		abiboot.ClearHostCaller()
		resetTestState()
	})
	resetTestState()
	setSettings(DefaultConfig())
}

// resetTestState clears every package-level cache so tests cannot leak into each
// other.
func resetTestState() {
	shutdownLoginSessions()
	resetDiscoveredModels()
}

// callsFor returns every recorded request whose URL contains match.
func (f *fakeHost) callsFor(match string) []abiboot.HTTPDoRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]abiboot.HTTPDoRequest, 0, 2)
	for _, request := range f.requests {
		if strings.Contains(request.URL, match) {
			out = append(out, request)
		}
	}
	return out
}

// savedNames returns the auth file names saved through the host.
func (f *fakeHost) savedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.saved))
	for name := range f.saved {
		out = append(out, name)
	}
	return out
}

// envelopeOK wraps a host result in the ABI envelope.
func envelopeOK(result any) ([]byte, error) {
	raw, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(abiboot.Envelope{OK: true, Result: raw})
}

// testHost returns a Host carrying a callback identity.
func testHost() *abiboot.Host {
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"test-callback"}`))
}

// httpResponse builds a canned host HTTP response.
func httpResponse(status int, body string) *pluginapi.HTTPResponse {
	return &pluginapi.HTTPResponse{
		StatusCode: status,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(body),
	}
}

// jsonBody builds a JSON response body from a value.
func jsonBody(t *testing.T, value any) string {
	t.Helper()
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal fake body: %v", errMarshal)
	}
	return string(encoded)
}

// withSettings swaps the process settings for one test.
func withSettings(t *testing.T, cfg Config) {
	t.Helper()
	previous := settings()
	setSettings(cfg)
	t.Cleanup(func() { setSettings(previous) })
}

// decodeResult unmarshals a handler result into T.
func decodeResult[T any](t *testing.T, value any) T {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal handler result: %v", errMarshal)
	}
	var out T
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		t.Fatalf("unmarshal handler result: %v", errUnmarshal)
	}
	return out
}

// managementRequest builds a management request for one route.
func managementRequest(method, path string, query url.Values, body []byte) pluginapi.ManagementRequest {
	headers := http.Header{}
	headers.Set("Accept", "text/html,application/xhtml+xml")
	return pluginapi.ManagementRequest{Method: method, Path: path, Headers: headers, Query: query, Body: body}
}

// jsonManagementRequest builds a management request that asks for JSON.
func jsonManagementRequest(method, path string, query url.Values) pluginapi.ManagementRequest {
	request := managementRequest(method, path, query, nil)
	request.Headers.Set("Accept", "application/json")
	return request
}

// callManagement invokes the management dispatcher with a typed request.
func callManagement(t *testing.T, h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	t.Helper()
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal management request: %v", errMarshal)
	}
	value, errHandle := handleManagementHandle(h, raw)
	if errHandle != nil {
		t.Fatalf("management handler error: %v", errHandle)
	}
	response, ok := value.(pluginapi.ManagementResponse)
	if !ok {
		t.Fatalf("management handler returned %T, want pluginapi.ManagementResponse", value)
	}
	return response
}

// mustRegister invokes model.register.
func mustRegister(t *testing.T, h *abiboot.Host) any {
	t.Helper()
	value, err := handleModelRegister(h, nil)
	if err != nil {
		t.Fatalf("model.register: %v", err)
	}
	return value
}

// mustStatic invokes model.static.
func mustStatic(t *testing.T, h *abiboot.Host) any {
	t.Helper()
	value, err := handleModelStatic(h, nil)
	if err != nil {
		t.Fatalf("model.static: %v", err)
	}
	return value
}

// mustForAuth invokes model.for_auth for one credential.
func mustForAuth(t *testing.T, h *abiboot.Host, credential *Credential) any {
	t.Helper()
	value, err := handleModelForAuth(h, authModelPayload(t, credential))
	if err != nil {
		t.Fatalf("model.for_auth: %v", err)
	}
	return value
}

// authModelPayload builds the model.for_auth request payload.
func authModelPayload(t *testing.T, credential *Credential) []byte {
	t.Helper()
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode credential: %v", errEncode)
	}
	raw, errMarshal := json.Marshal(pluginapi.AuthModelRequest{
		AuthID:       "loomy-test",
		AuthProvider: ProviderKey,
		StorageJSON:  storage,
	})
	if errMarshal != nil {
		t.Fatalf("marshal model request: %v", errMarshal)
	}
	return raw
}

// sampleCredential builds a credential with a known 14-day expiry.
func sampleCredential(t *testing.T) *Credential {
	t.Helper()
	cfg := DefaultConfig()
	return buildCredential("0123456789abcdef0123456789abcdef", "123456789012345678", "13800138000", "", cfg, time.Now())
}
