package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// The executor error path returns `error` (the interface), not the concrete
// *abiboot.EnvelopeError. The encoder uses a direct type assertion, so this
// test drives the real dispatch encoder to prove the code and HTTP status
// survive the interface round-trip instead of degrading to "plugin_error".
func TestStreamErrorEnvelopeSurvivesDispatchEncoding(t *testing.T) {
	cases := []struct {
		kind     streamErrorKind
		wantCode string
		wantHTTP int
	}{
		{streamErrorContentRejection, "PERMISSION_DENIED", http.StatusForbidden},
		{streamErrorRateLimit, "RATE_LIMIT", http.StatusTooManyRequests},
		{streamErrorContextWindow, "CONTEXT_WINDOW_EXCEEDED", http.StatusBadRequest},
		{streamErrorServer, "SERVER", http.StatusBadGateway},
	}
	for _, tc := range cases {
		var err error = streamErrorEnvelope(tc.kind, "detail")
		raw := dispatchErrorEnvelope(t, err)
		var env struct {
			OK    bool `json:"ok"`
			Error struct {
				Code       string `json:"code"`
				HTTPStatus int    `json:"http_status"`
			} `json:"error"`
		}
		if uerr := json.Unmarshal(raw, &env); uerr != nil {
			t.Fatalf("%v: unmarshal %s: %v", tc.kind, raw, uerr)
		}
		if env.OK {
			t.Fatalf("%v: envelope reported ok=true", tc.kind)
		}
		if env.Error.Code != tc.wantCode || env.Error.HTTPStatus != tc.wantHTTP {
			t.Fatalf("%v: got code=%q status=%d, want code=%q status=%d (raw=%s)",
				tc.kind, env.Error.Code, env.Error.HTTPStatus, tc.wantCode, tc.wantHTTP, raw)
		}
	}
}

// failingPlugin is the minimal abiboot.Plugin used to reach the real encoder.
type failingPlugin struct{ routes map[string]abiboot.Handler }

func (p failingPlugin) Registration() abiboot.Registration { return abiboot.Registration{} }
func (p failingPlugin) Routes() map[string]abiboot.Handler { return p.routes }

// dispatchErrorEnvelope routes a handler that returns err through the real
// dispatcher, so the assertion covers encodeError rather than a copy of it.
func dispatchErrorEnvelope(t *testing.T, err error) []byte {
	t.Helper()
	mux := abiboot.NewMux()
	mux.On("test.fail", func(*abiboot.Host, json.RawMessage) (any, error) { return nil, err })
	return abiboot.Dispatch(failingPlugin{routes: mux.Routes()}, "test.fail", []byte(`{}`))
}
