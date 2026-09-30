package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	catalogueMu.Lock()
	catalogueCache = map[string]catalogue{}
	catalogueMu.Unlock()
	credentialRefresher = newCredentialRefresher()
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

// sampleCredential builds a credential issued now with a chosen lifetime, so it
// sits outside the renewal lead window unless a test deliberately says otherwise.
func sampleCredential(expiresIn int64) *Credential {
	return &Credential{
		Type:         ProviderKey,
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		CreatedAt:    time.Now().Unix(),
		User: UserInfo{
			ID:       "6746ebd581efe24face84197",
			Username: "qq_23240873",
			Name:     "黎明文铮",
		},
	}
}

// mustJSON marshals a value or fails the test.
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	return encoded
}

// executorRequest builds an ExecutorRequest carrying a body and credential.
func executorRequest(t *testing.T, body any, storage []byte) pluginapi.ExecutorRequest {
	t.Helper()
	return pluginapi.ExecutorRequest{
		AuthID:      "atomcode-qq_23240873.json",
		StorageJSON: storage,
		Payload:     mustJSON(t, body),
	}
}

// runExecutorExecute invokes the buffered executor and returns its response.
func runExecutorExecute(t *testing.T, h *abiboot.Host, request pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	t.Helper()
	value, errHandle := handleExecutorExecute(h, mustJSON(t, request))
	if errHandle != nil {
		return pluginapi.ExecutorResponse{}, errHandle
	}
	response, ok := value.(pluginapi.ExecutorResponse)
	if !ok {
		t.Fatalf("execute returned %T", value)
	}
	return response, nil
}

// runExecutorStream invokes the streaming executor and returns its chunks.
func runExecutorStream(t *testing.T, h *abiboot.Host, request pluginapi.ExecutorRequest) ([]pluginapi.ExecutorStreamChunk, error) {
	t.Helper()
	value, errHandle := handleExecutorExecuteStream(h, mustJSON(t, request))
	if errHandle != nil {
		return nil, errHandle
	}
	response, ok := value.(executorStreamResponse)
	if !ok {
		t.Fatalf("execute_stream returned %T", value)
	}
	return response.Chunks, nil
}

// decodeBody unmarshals a recorded request body.
func decodeBody(t *testing.T, request abiboot.HTTPDoRequest) map[string]any {
	t.Helper()
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(request.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode request body %q: %v", string(request.Body), errUnmarshal)
	}
	return decoded
}
