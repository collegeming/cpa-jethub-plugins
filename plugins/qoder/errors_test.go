package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// envelopeStatus extracts the http_status the host actually reads.
func envelopeStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		t.Fatal("expected a failure, got nil")
	}
	typed, ok := err.(*abiboot.EnvelopeError)
	if !ok {
		t.Fatalf("error type = %T, want *abiboot.EnvelopeError", err)
	}
	return typed.HTTPStatus
}

// TestCredentialErrorsCarryUnauthorized is the contract the host relies on to
// rotate a dead credential: `http_status`, not the envelope `code`.
func TestCredentialErrorsCarryUnauthorized(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		code string
	}{
		{"empty payload", "", "invalid_credential"},
		{"not JSON", `{`, "invalid_credential"},
		{"no token", `{"machine_id":"m"}`, "invalid_credential"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, errParse := ParseCredential([]byte(testCase.raw))
			if got := envelopeStatus(t, errParse); got != http.StatusUnauthorized {
				t.Fatalf("http_status = %d, want 401", got)
			}
			if typed := errParse.(*abiboot.EnvelopeError); typed.Code != testCase.code {
				t.Fatalf("code = %q, want %q", typed.Code, testCase.code)
			}
		})
	}
}

// TestRefreshErrorsAreClassified covers the states a refresh can end in.
func TestRefreshErrorsAreClassified(t *testing.T) {
	withSettings(t, DefaultConfig())

	// A missing refresh token is a dead credential.
	if _, errRefresh := handleAuthRefresh(testHost(), json.RawMessage(
		`{"StorageJSON":"`+base64Fixture(`{"access_token":"tok"}`)+`"}`)); envelopeStatus(t, errRefresh) != http.StatusUnauthorized {
		t.Fatalf("http_status = %d, want 401 for a non-refreshable credential", envelopeStatus(t, errRefresh))
	}

	cases := []struct {
		name       string
		do         func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error)
		wantStatus int
	}{
		{"upstream 401", func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(401, "{}"), nil
		}, http.StatusUnauthorized},
		{"upstream 403", func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(403, "{}"), nil
		}, http.StatusUnauthorized},
		{"upstream 429", func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(429, "{}"), nil
		}, http.StatusTooManyRequests},
		{"upstream 500", func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(500, "{}"), nil
		}, http.StatusBadGateway},
		{"transport", func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return nil, abiboot.Errorf("transport", "connection reset")
		}, http.StatusBadGateway},
		{"tokenless 200", func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(200, `{"status":"ok"}`), nil
		}, http.StatusBadGateway},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakeHost()
			host.do = testCase.do
			host.install(t)
			credential := &Credential{AccessToken: "tok", RefreshToken: "ref", MachineID: "m"}
			_, errRefresh := refreshCredential(testHost(), credential, DefaultConfig())
			if got := envelopeStatus(t, errRefresh); got != testCase.wantStatus {
				t.Fatalf("http_status = %d, want %d", got, testCase.wantStatus)
			}
		})
	}
}

// TestUpstreamErrorStatuses locks the inference taxonomy.
func TestUpstreamErrorStatuses(t *testing.T) {
	cases := map[int]int{
		http.StatusUnauthorized:        http.StatusUnauthorized,
		http.StatusForbidden:           http.StatusUnauthorized,
		http.StatusPaymentRequired:     http.StatusPaymentRequired,
		http.StatusTooManyRequests:     http.StatusTooManyRequests,
		http.StatusInternalServerError: http.StatusBadGateway,
		http.StatusBadGateway:          http.StatusBadGateway,
		http.StatusGatewayTimeout:      http.StatusGatewayTimeout,
		http.StatusNotFound:            http.StatusBadGateway,
	}
	for upstream, want := range cases {
		errUpstream := upstreamError(httpResponse(upstream, "detail"))
		if got := envelopeStatus(t, errUpstream); got != want {
			t.Errorf("upstream %d -> http_status %d, want %d", upstream, got, want)
		}
	}
}

// TestMissingUIDIsUnauthorized keeps the encrypted path's uid requirement
// classified as a credential problem, because that is how it is fixed.
func TestMissingUIDIsUnauthorized(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WASMPath = "/tmp/does-not-matter.wasm"
	withSettings(t, cfg)
	credential := &Credential{AccessToken: "tok", MachineID: "m"}
	_, _, errInfer := performInfer(testHost(), executorRequestFixture(`{"model":"qfmodel","messages":[]}`, "qfmodel"), credential, cfg)
	if got := envelopeStatus(t, errInfer); got != http.StatusUnauthorized {
		t.Fatalf("http_status = %d, want 401", got)
	}
	if !strings.Contains(errInfer.Error(), "uid") {
		t.Fatalf("error = %q, want it to name the missing uid", errInfer.Error())
	}
}

// TestUnusableSignerIsAnInternalError keeps a local misconfiguration from being
// reported as an upstream fault.
func TestUnusableSignerIsAnInternalError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WASMPath = "/nonexistent/qoder-auth-wasm.wasm"
	withSettings(t, cfg)
	credential := &Credential{AccessToken: "tok", MachineID: "m", UID: "u-1"}
	_, errSend := sendEncrypted(testHost(), executorRequestFixture(`{"model":"auto","messages":[]}`, "auto"),
		credential, cfg, productByID(string(RegionGlobal)))
	if got := envelopeStatus(t, errSend); got != http.StatusInternalServerError {
		t.Fatalf("http_status = %d, want 500", got)
	}
}

// TestCreditsErrorsAreClassified checks the balance and campaign endpoints.
func TestCreditsErrorsAreClassified(t *testing.T) {
	cases := []struct {
		status     int
		wantStatus int
	}{
		{http.StatusUnauthorized, http.StatusUnauthorized},
		{http.StatusForbidden, http.StatusUnauthorized},
		{http.StatusTooManyRequests, http.StatusTooManyRequests},
		{http.StatusInternalServerError, http.StatusBadGateway},
	}
	for _, testCase := range cases {
		host := newFakeHost()
		host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(testCase.status, "nope"), nil
		}
		host.install(t)
		_, errBalance := fetchCreditBalance(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig())
		if got := envelopeStatus(t, errBalance); got != testCase.wantStatus {
			t.Errorf("usage HTTP %d -> http_status %d, want %d", testCase.status, got, testCase.wantStatus)
		}
		_, errCampaigns := loadCampaigns(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig())
		if got := envelopeStatus(t, errCampaigns); got != testCase.wantStatus {
			t.Errorf("campaigns HTTP %d -> http_status %d, want %d", testCase.status, got, testCase.wantStatus)
		}
	}

	// A transport failure is an upstream fault.
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, abiboot.Errorf("transport", "no route")
	}
	host.install(t)
	if _, errBalance := fetchCreditBalance(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig()); envelopeStatus(t, errBalance) != http.StatusBadGateway {
		t.Fatalf("transport failure http_status = %d, want 502", envelopeStatus(t, errBalance))
	}
}

// TestCheckinWithADeadCredentialReturnsUnauthorized makes sure the failure is not
// flattened into a 200 "failed outcome".
func TestCheckinWithADeadCredentialReturnsUnauthorized(t *testing.T) {
	statusHost(t, "alice")
	host := newFakeHost()
	host.files = []pluginapi.HostAuthFileEntry{{
		AuthIndex: "idx-1", ID: "idx-1", Name: "alice", Provider: ProviderKey, Status: "active",
	}}
	stored, _ := (&Credential{AccessToken: "tok", MachineID: "m", UID: "u-1"}).Encode()
	host.auths["idx-1"] = stored
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(401, "<html>unauthorized</html>"), nil
	}
	host.install(t)
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest("/checkin", map[string]string{"format": "json"}, ""))
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if !strings.Contains(payload["error"].(string), "重新登录") {
		t.Fatalf("error = %v, want a re-login hint", payload["error"])
	}
}

// TestStatusOfFallsBackForUnknownErrors keeps the helper forgiving.
func TestStatusOfFallsBackForUnknownErrors(t *testing.T) {
	if got := statusOf(abiboot.Errorf("code", "message"), http.StatusTeapot); got != http.StatusTeapot {
		t.Fatalf("statusOf unclassified = %d, want the fallback", got)
	}
	if got := statusOf(nil, http.StatusBadGateway); got != http.StatusBadGateway {
		t.Fatalf("statusOf nil = %d, want the fallback", got)
	}
	if got := statusOf(credentialError("invalid_credential", "bad"), http.StatusBadGateway); got != http.StatusUnauthorized {
		t.Fatalf("statusOf classified = %d, want 401", got)
	}
}
