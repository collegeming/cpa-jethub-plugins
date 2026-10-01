package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file provides the in-process fake host transport every test uses. NO test
// touches the network: HTTP calls issued through *abiboot.Host are answered from a
// scripted handler, and the auth/log callbacks are recorded.
//
// ⚠ That is a hard rule for this provider, not a convenience. The identity block is
// coupled to an upstream policy that penalises a mismatch with account cooldowns
// (30 minutes, then 24 hours from the third occurrence, disablement on the
// fifth), so a test must never be able to reach the live endpoint even by
// accident. Nothing in this package dials: the only transport is the host caller,
// and a test that does not install this fake gets `host_unavailable` rather than a
// request.

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
// reset forgets the recorded calls, so one test can make a second request and
// assert on it alone.
func (f *fakeHost) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

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

// sampleCredential is the canonical credential shape the plugin writes.
func sampleCredential() *Credential {
	credential := &Credential{
		ZCodeJWT:            "header.payload.signature",
		DeviceMid:           "8f14e45f-ceea-467a-9c1c-1b2c3d4e5f60",
		UserID:              "user-1234567890",
		BigModelAccessToken: "bigmodel-access-token",
		AccountLabel:        "尾号8000",
		AppVersion:          "3.14.3",
		Source:              "plugin",
	}
	return credential
}

// storedCredential installs one account in the fake host and returns its entry.
func storedCredential(t *testing.T, fake *fakeHost, authIndex, name string, credential *Credential) pluginapi.HostAuthFileEntry {
	t.Helper()
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode credential: %v", errEncode)
	}
	entry := pluginapi.HostAuthFileEntry{
		Provider: ProviderKey, AuthIndex: authIndex, Name: name, Status: "active",
	}
	fake.files = append(fake.files, entry)
	fake.auths[authIndex] = storage
	return entry
}

// executorRequest builds an executor.execute payload bound to a credential.
func executorRequest(t *testing.T, credential *Credential, payload string) pluginapi.ExecutorRequest {
	t.Helper()
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode credential: %v", errEncode)
	}
	return pluginapi.ExecutorRequest{
		AuthID:       "zcode-test",
		AuthProvider: ProviderKey,
		Model:        "GLM-5.3-Flash",
		Format:       "anthropic",
		Stream:       true,
		Payload:      []byte(payload),
		StorageJSON:  storage,
	}
}

// executorRawPayload marshals an executor request for a direct handler call.
func executorRawPayload(t *testing.T, credential *Credential, payload string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(executorRequest(t, credential, payload))
	if errMarshal != nil {
		t.Fatalf("marshal executor request: %v", errMarshal)
	}
	return raw
}

// encryptClientValue reproduces the official client's encryption, so the import
// path can be tested against a value the client itself could have written.
//
// ⚠ This is the INVERSE of `decryptClientCredentialValue` written independently
// (it builds the value from `aes.NewCipher` the same way a Node `createCipheriv`
// would), so a test passing here means the two agree on: the `enc:v1:` prefix, the
// base64url spelling of each segment, the 12-byte IV, the 16-byte tag, the GCM
// additional-data-free mode, and the tag's position as its own segment.
func encryptClientValue(t *testing.T, plain string, key []byte) string {
	t.Helper()
	iv := make([]byte, credentialIVLength)
	if _, errRead := rand.Read(iv); errRead != nil {
		t.Fatalf("generate IV: %v", errRead)
	}
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		t.Fatalf("aes.NewCipher: %v", errCipher)
	}
	aead, errAEAD := cipher.NewGCM(block)
	if errAEAD != nil {
		t.Fatalf("cipher.NewGCM: %v", errAEAD)
	}
	sealed := aead.Seal(nil, iv, []byte(plain), nil)
	tag := sealed[len(sealed)-credentialTagLength:]
	data := sealed[:len(sealed)-credentialTagLength]
	return credentialPrefix +
		base64.RawURLEncoding.EncodeToString(iv) + "." +
		base64.RawURLEncoding.EncodeToString(tag) + "." +
		base64.RawURLEncoding.EncodeToString(data)
}

// deriveTestKey builds a key from an explicit secret, the way
// `ZCODE_CREDENTIAL_SECRET` does.
func deriveTestKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// isolateClientStore points every fallback location of the official client's
// store at a fresh temporary directory and returns it.
//
// ⚠ This is required for hermeticity, not tidiness: the machine running these
// tests may genuinely have the official ZCode client installed (this development
// host does, under `~/.zcode/v2/`), and the candidate list deliberately falls back
// to the home directory. Without this, a test that means to exercise "no client
// store present" would silently read a real one — and the import path could pick
// up a real device id and a real token.
func isolateClientStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", filepath.Join(dir, "appdata"))
	t.Setenv("LOCALAPPDATA", filepath.Join(dir, "localappdata"))
	t.Setenv("ZCODE_DATA_BASE_DIR", "")
	return dir
}
