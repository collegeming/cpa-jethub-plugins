package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fakeHost installs a scripted host transport so every loopback path can be
// exercised without a socket.
//
// The plugin reaches the host through exactly two seams — abiboot's single host
// caller (used by host.auth.list) and the transportFor package variable (used by
// host.http.do) — so one hook covers everything. Nothing in this file opens a
// network connection, which is what lets the whole suite run offline.
type fakeHost struct {
	mu sync.Mutex

	// files is what host.auth.list reports.
	files []pluginapi.HostAuthFileEntry
	// do answers host.http.do; a nil do fails the test when called.
	do func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error)
	// requests records every host.http.do call, in order.
	requests []string
	// accepts records the Accept header of every call, aligned with requests.
	accepts []string
}

func newFakeHost(files ...pluginapi.HostAuthFileEntry) *fakeHost {
	return &fakeHost{files: files}
}

// account builds one host credential entry.
func account(provider, index, name string) pluginapi.HostAuthFileEntry {
	return pluginapi.HostAuthFileEntry{Provider: provider, AuthIndex: index, Name: name}
}

// install registers the fake as both seams for the duration of the test.
func (f *fakeHost) install(t *testing.T) {
	t.Helper()

	previousTransport := transportFor
	transportFor = func(*abiboot.Host) doer {
		return func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
			f.mu.Lock()
			f.requests = append(f.requests, method+" "+rawURL)
			f.accepts = append(f.accepts, headers.Get("Accept"))
			do := f.do
			f.mu.Unlock()
			if do == nil {
				return nil, fmt.Errorf("unexpected host.http.do: %s %s", method, rawURL)
			}
			return do(method, rawURL, headers, body)
		}
	}

	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			files := f.files
			if files == nil {
				files = []pluginapi.HostAuthFileEntry{}
			}
			return envelopeOK(map[string]any{"files": files})
		case pluginabi.MethodHostLog:
			return envelopeOK(map[string]any{})
		default:
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
	})

	t.Cleanup(func() {
		transportFor = previousTransport
		abiboot.ClearHostCaller()
	})
}

// serve answers host.http.do by matching a URL fragment. Every match returns a
// fresh response so a caller cannot mutate shared state.
func (f *fakeHost) serve(routes ...route) {
	f.do = func(_, rawURL string, _ http.Header, _ []byte) (*pluginapi.HTTPResponse, error) {
		for _, item := range routes {
			if strings.Contains(rawURL, item.match) {
				if item.err != nil {
					return nil, item.err
				}
				return &pluginapi.HTTPResponse{
					StatusCode: item.status,
					Headers:    map[string][]string{"Content-Type": {item.contentType}},
					Body:       []byte(item.body),
				}, nil
			}
		}
		return nil, fmt.Errorf("no scripted route matches %s", rawURL)
	}
}

// route is one scripted answer. Routes are matched by URL fragment and the
// first match wins, so a specific route must be listed before a general one and
// catchAll must be last.
type route struct {
	match       string
	status      int
	body        string
	contentType string
	err         error
}

// jsonRoute answers a JSON document.
func jsonRoute(match, body string) route {
	return route{match: match, status: http.StatusOK, body: body, contentType: "application/json"}
}

// htmlRoute answers an HTML document.
func htmlRoute(match, body string) route {
	return route{match: match, status: http.StatusOK, body: body, contentType: "text/html; charset=utf-8"}
}

// catchAll answers the way the host's HTTP router does for an unregistered
// resource route. It matches everything, so it MUST be the last route.
func catchAll() route {
	return route{status: http.StatusNotFound, body: "404 page not found", contentType: "text/plain"}
}

// requestURLs returns every recorded host.http.do URL.
func (f *fakeHost) requestURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// requestAccepts returns the recorded Accept headers.
func (f *fakeHost) requestAccepts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.accepts...)
}

// requestFor returns the first recorded request containing a fragment.
func (f *fakeHost) requestFor(fragment string) string {
	for _, item := range f.requestURLs() {
		if strings.Contains(item, fragment) {
			return item
		}
	}
	return ""
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

// managementRequest builds an inbound management request.
func managementRequest(method, path string, query url.Values, accept string) pluginapi.ManagementRequest {
	headers := http.Header{}
	if accept != "" {
		headers.Set("Accept", accept)
	}
	if query == nil {
		query = url.Values{}
	}
	return pluginapi.ManagementRequest{Method: method, Path: path, Headers: headers, Query: query}
}

// dispatchManagement runs one request through the registered dispatcher, the way
// the host does.
func dispatchManagement(t *testing.T, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	t.Helper()
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal management request: %v", errMarshal)
	}
	value, errHandle := handleManagementHandle(testHost(), raw)
	if errHandle != nil {
		t.Fatalf("handle management request: %v", errHandle)
	}
	response, okResponse := value.(pluginapi.ManagementResponse)
	if !okResponse {
		t.Fatalf("handler returned %T, want pluginapi.ManagementResponse", value)
	}
	return response
}

// withSettings swaps the process settings for one test.
func withSettings(t *testing.T, cfg Config) {
	t.Helper()
	previous := settings()
	setSettings(cfg)
	t.Cleanup(func() { setSettings(previous) })
}

// resetLastRun clears the cached snapshot around one test.
func resetLastRun(t *testing.T) {
	t.Helper()
	previous := lastRun()
	forgetLastRun()
	t.Cleanup(func() { rememberLastRun(previous) })
}

// rowFor returns the first row of a run for one provider.
func rowsFor(t *testing.T, result *runResult, provider string) []row {
	t.Helper()
	if result == nil {
		t.Fatal("run result is nil")
	}
	out := make([]row, 0, 1)
	for _, item := range result.rows() {
		if item.Provider == provider {
			out = append(out, item)
		}
	}
	return out
}

// testConfig is the default configuration with a deterministic provider set.
func testConfig(providers ...string) Config {
	cfg := DefaultConfig()
	cfg.Providers = providers
	return cfg
}
