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
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file provides the in-process fake host transport used by every test. No
// test touches the network: HTTP calls issued through *abiboot.Host are answered
// from a scripted route table, and the auth/log callbacks are recorded for
// assertions.

// errFakeTransport simulates an unreachable upstream.
var errFakeTransport = errors.New("fake transport failure")

// recordedRequest is one outbound host.http.do call.
type recordedRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

// httpRoute answers one outbound request. Match is a substring of the URL.
type httpRoute struct {
	Method  string
	Match   string
	Status  int
	Body    string
	Headers http.Header
	// Err makes the transport itself fail instead of answering.
	Err error
	// HeaderName/HeaderValue additionally require a request header to contain a
	// value, which is how a route is bound to ONE account when a test holds
	// several (the credential travels in the Authorization header).
	HeaderName  string
	HeaderValue string
}

// savedAuth is one host.auth.save call.
type savedAuth struct {
	Name string
	JSON []byte
}

// fakeHost is a scriptable host transport.
type fakeHost struct {
	mu       sync.Mutex
	routes   []httpRoute
	requests []recordedRequest
	files    []pluginapi.HostAuthFileEntry
	authJSON map[string]string
	saved    []savedAuth
	logs     []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{authJSON: map[string]string{}}
}

// on appends a route to the script.
func (f *fakeHost) on(route httpRoute) *fakeHost {
	if route.Status == 0 {
		route.Status = http.StatusOK
	}
	f.mu.Lock()
	f.routes = append(f.routes, route)
	f.mu.Unlock()
	return f
}

// withFiles sets the host's credential listing.
func (f *fakeHost) withFiles(entries ...pluginapi.HostAuthFileEntry) *fakeHost {
	f.files = entries
	return f
}

// withAuthJSON registers the JSON returned by host.auth.get for an index.
func (f *fakeHost) withAuthJSON(authIndex, payload string) *fakeHost {
	f.authJSON[authIndex] = payload
	return f
}

// requestsFor returns every recorded request whose URL contains match.
func (f *fakeHost) requestsFor(match string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, 0, 2)
	for _, request := range f.requests {
		if strings.Contains(request.URL, match) {
			out = append(out, request)
		}
	}
	return out
}

// savedAuths returns a copy of every host.auth.save call.
func (f *fakeHost) savedAuths() []savedAuth {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]savedAuth, len(f.saved))
	copy(out, f.saved)
	return out
}

// handler is the transport installed into abiboot.
func (f *fakeHost) handler() func(method string, payload []byte) (any, error) {
	return func(method string, payload []byte) (any, error) {
		switch method {
		case "host.http.do":
			return f.handleHTTPDo(payload)
		case "host.auth.list":
			f.mu.Lock()
			files := append([]pluginapi.HostAuthFileEntry(nil), f.files...)
			f.mu.Unlock()
			return map[string]any{"files": files}, nil
		case "host.auth.get":
			var request pluginapi.HostAuthGetRequest
			_ = json.Unmarshal(payload, &request)
			f.mu.Lock()
			raw := f.authJSON[request.AuthIndex]
			f.mu.Unlock()
			if raw == "" {
				return nil, fmt.Errorf("auth index %s not found", request.AuthIndex)
			}
			return map[string]any{"auth_index": request.AuthIndex, "json": json.RawMessage(raw)}, nil
		case "host.auth.save":
			var request pluginapi.HostAuthSaveRequest
			if errUnmarshal := json.Unmarshal(payload, &request); errUnmarshal != nil {
				return nil, fmt.Errorf("decode auth save: %w", errUnmarshal)
			}
			f.mu.Lock()
			f.saved = append(f.saved, savedAuth{Name: request.Name, JSON: request.JSON})
			f.mu.Unlock()
			return map[string]any{"name": request.Name, "path": "/tmp/" + request.Name}, nil
		case "host.log":
			f.mu.Lock()
			f.logs = append(f.logs, string(payload))
			f.mu.Unlock()
			return map[string]any{}, nil
		default:
			return nil, fmt.Errorf("fake host does not implement %s", method)
		}
	}
}

// handleHTTPDo answers one outbound HTTP call from the route table.
func (f *fakeHost) handleHTTPDo(payload []byte) (any, error) {
	var request struct {
		Method  string      `json:"method"`
		URL     string      `json:"url"`
		Headers http.Header `json:"headers"`
		Body    []byte      `json:"body"`
	}
	if errUnmarshal := json.Unmarshal(payload, &request); errUnmarshal != nil {
		return nil, fmt.Errorf("decode http request: %w", errUnmarshal)
	}
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: request.Method, URL: request.URL, Headers: request.Headers, Body: request.Body,
	})
	routes := append([]httpRoute(nil), f.routes...)
	f.mu.Unlock()

	for _, route := range routes {
		if route.Method != "" && !strings.EqualFold(route.Method, request.Method) {
			continue
		}
		if route.Match != "" && !strings.Contains(request.URL, route.Match) {
			continue
		}
		if route.HeaderName != "" && !strings.Contains(request.Headers.Get(route.HeaderName), route.HeaderValue) {
			continue
		}
		if route.Err != nil {
			return nil, route.Err
		}
		headers := route.Headers
		if headers == nil {
			headers = http.Header{"Content-Type": []string{"application/json"}}
		}
		return pluginapi.HTTPResponse{
			StatusCode: route.Status,
			Headers:    headers,
			Body:       []byte(route.Body),
		}, nil
	}
	// No route matched: fail the way an unreachable upstream would.
	return nil, fmt.Errorf("no scripted route for %s %s", request.Method, request.URL)
}

// installFakeHost wires a fake host into the package-level transport and
// returns the *Host handlers receive. It also resets every package-level cache
// so tests cannot leak state into each other.
func installFakeHost(t *testing.T, fake *fakeHost) *abiboot.Host {
	t.Helper()
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		result, errHandle := fake.handler()(method, request)
		if errHandle != nil {
			return json.Marshal(abiboot.Envelope{
				OK:    false,
				Error: &abiboot.EnvelopeError{Code: "fake_host_error", Message: errHandle.Error()},
			})
		}
		return abiboot.OK(result)
	})
	t.Cleanup(func() {
		abiboot.ClearHostCaller()
		resetCaches()
	})
	resetCaches()
	setSettings(DefaultConfig())
	return &abiboot.Host{CallbackID: "test-callback", PluginID: ProviderKey}
}

// resetCaches clears the package-level discovery and version caches.
func resetCaches() {
	discoveredModels.mu.Lock()
	discoveredModels.models = nil
	discoveredModels.index = nil
	discoveredModels.fetchedAt = time.Time{}
	discoveredModels.mu.Unlock()
	versionResolver.reset()
	shutdownLoginSessions()
}

// mustJSON marshals a value, failing the test on error.
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal %T: %v", value, errMarshal)
	}
	return encoded
}

// managementRequest builds a management request for one route.
func managementRequest(method, path string, query url.Values, body []byte) pluginapi.ManagementRequest {
	headers := http.Header{}
	headers.Set("Accept", "text/html,application/xhtml+xml")
	return pluginapi.ManagementRequest{Method: method, Path: path, Headers: headers, Query: query, Body: body}
}

// jsonManagementRequest builds a management request that asks for JSON.
func jsonManagementRequest(method, path string, query url.Values, body []byte) pluginapi.ManagementRequest {
	request := managementRequest(method, path, query, body)
	request.Headers.Set("Accept", "application/json")
	return request
}

// rawManagementRequest builds a management request with explicit headers.
func rawManagementRequest(method, path string, query url.Values, accept string) pluginapi.ManagementRequest {
	request := managementRequest(method, path, query, nil)
	if accept == "" {
		request.Headers.Del("Accept")
	} else {
		request.Headers.Set("Accept", accept)
	}
	return request
}
