package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fakeHost installs a scripted host transport so every network path can be
// exercised without a socket. The plugin reaches the host through the single
// abiboot host caller, so one hook covers both the HTTP and auth callbacks.
type fakeHost struct {
	mu sync.Mutex

	// do answers host.http.do. A nil do fails the test when called.
	do func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error)
	// auths maps an auth index to its stored JSON for host.auth.get.
	auths map[string][]byte
	// files is what host.auth.list reports.
	files []pluginapi.HostAuthFileEntry
	// saved records every host.auth.save call.
	saved map[string][]byte
	// calls records the host methods in order.
	calls []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{auths: map[string][]byte{}, saved: map[string][]byte{}}
}

// install registers the fake transport for the duration of the test.
func (f *fakeHost) install(t *testing.T) {
	t.Helper()
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		f.mu.Lock()
		f.calls = append(f.calls, method)
		f.mu.Unlock()

		switch method {
		case pluginabi.MethodHostHTTPDo:
			var payload abiboot.HTTPDoRequest
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			if f.do == nil {
				return nil, fmt.Errorf("unexpected host.http.do for %s", payload.URL)
			}
			response, errDo := f.do(payload)
			if errDo != nil {
				return nil, errDo
			}
			return envelopeOK(response)

		case pluginabi.MethodHostAuthList:
			files := f.files
			if files == nil {
				files = []pluginapi.HostAuthFileEntry{}
			}
			return envelopeOK(map[string]any{"files": files})

		case pluginabi.MethodHostAuthGet:
			var payload pluginapi.HostAuthGetRequest
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			stored, ok := f.auths[payload.AuthIndex]
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
			return envelopeOK(map[string]any{})

		default:
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
	})
	t.Cleanup(func() { abiboot.ClearHostCaller() })
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
