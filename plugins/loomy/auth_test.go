package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The identifier is the provider key.
func TestAuthIdentifierIsProviderKey(t *testing.T) {
	value, errIdentifier := handleAuthIdentifier(nil, nil)
	if errIdentifier != nil {
		t.Fatalf("auth.identifier: %v", errIdentifier)
	}
	if got := decodeResult[map[string]string](t, value)["identifier"]; got != ProviderKey {
		t.Fatalf("identifier = %q, want %q", got, ProviderKey)
	}
}

// auth.parse recognises our own credentials and declines everything else so the
// host can try other providers.
func TestAuthParseHandlesOnlyLoomyCredentials(t *testing.T) {
	storage := mustJSON(t, map[string]any{
		"access_token": "0123456789abcdef0123456789abcdef",
		"userid":       "123456789012345678",
		"phone":        "13800138000",
		"expires_at":   "1790000000000",
	})

	value, errParse := handleAuthParse(nil, mustJSON(t, pluginapi.AuthParseRequest{Provider: ProviderKey, RawJSON: storage}))
	if errParse != nil {
		t.Fatalf("auth.parse: %v", errParse)
	}
	handled := decodeResult[pluginapi.AuthParseResponse](t, value)
	if !handled.Handled {
		t.Fatal("a Loomy credential must be handled")
	}
	if handled.Auth.Provider != ProviderKey || len(handled.Auth.StorageJSON) == 0 {
		t.Fatalf("auth = %#v, want a Loomy record", handled.Auth)
	}

	foreign, errForeign := handleAuthParse(nil, mustJSON(t, pluginapi.AuthParseRequest{
		Provider: ProviderKey, RawJSON: []byte(`{"apiKey":"sk-other"}`),
	}))
	if errForeign != nil {
		t.Fatalf("auth.parse(foreign): %v", errForeign)
	}
	if decodeResult[pluginapi.AuthParseResponse](t, foreign).Handled {
		t.Fatal("a foreign payload must not be claimed")
	}

	otherProvider, errOther := handleAuthParse(nil, mustJSON(t, pluginapi.AuthParseRequest{
		Provider: "codearts", RawJSON: storage,
	}))
	if errOther != nil {
		t.Fatalf("auth.parse(other provider): %v", errOther)
	}
	if decodeResult[pluginapi.AuthParseResponse](t, otherProvider).Handled {
		t.Fatal("another provider key must not be claimed")
	}
}

// auth.refresh PROBES: a live credential is returned unchanged, and the next
// probe is scheduled before the local expiry.
func TestAuthRefreshProbesWithoutChangingTheCredential(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"balance":15000,"dailyBalance":4992,"availableBalance":19992}}`), nil
	}
	fake.install(t)

	credential := sampleCredential(t)
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	value, errRefresh := handleAuthRefresh(testHost(), mustJSON(t, pluginapi.AuthRefreshRequest{
		AuthID: "loomy-13800138000.json", StorageJSON: storage,
	}))
	if errRefresh != nil {
		t.Fatalf("auth.refresh: %v", errRefresh)
	}
	response := decodeResult[pluginapi.AuthRefreshResponse](t, value)
	refreshed, errParse := ParseCredential(response.Auth.StorageJSON)
	if errParse != nil {
		t.Fatalf("parse refreshed credential: %v", errParse)
	}
	if refreshed.Session() != credential.Session() || refreshed.ExpiresAtMS() != credential.ExpiresAtMS() {
		t.Fatalf("credential changed: %#v, want the same session and expiry", refreshed)
	}
	if response.NextRefreshAfter.IsZero() || !response.NextRefreshAfter.Before(credential.Expiry()) {
		t.Fatalf("next refresh = %s, want it before the expiry %s", response.NextRefreshAfter, credential.Expiry())
	}
	// The probe is the READ endpoint, never the write one.
	if calls := fake.callsFor(PointsRecordsPath); len(calls) != 1 {
		t.Fatalf("issued %d /points/records calls, want 1", len(calls))
	}
	if calls := fake.callsFor(PointsFirstLoginPath); len(calls) != 0 {
		t.Fatalf("the probe must never call the write endpoint (trap #12), got %d calls", len(calls))
	}
}

// A `100002` probe is terminal: 401 with the "log in again" advice.
func TestAuthRefreshReportsExpiryOnAuthExpiredCode(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		// HTTP 200 with a business failure: the status code says nothing.
		return httpResponse(200, `{"ok":false,"code":"100002","desc":"缺少 token"}`), nil
	}
	fake.install(t)

	storage, errEncode := sampleCredential(t).Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	_, errRefresh := handleAuthRefresh(testHost(), mustJSON(t, pluginapi.AuthRefreshRequest{StorageJSON: storage}))
	if errRefresh == nil {
		t.Fatal("a dead credential must fail the probe")
	}
	if status := statusOf(errRefresh, 0); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 so the host retires the credential", status)
	}
	if !strings.Contains(errRefresh.Error(), "重新登录") {
		t.Fatalf("error = %v, want the re-login advice", errRefresh)
	}
}

// A transport failure is NOT expiry (trap #10): network jitter must never force
// a re-login.
func TestAuthRefreshTransportFailureIsNotExpiry(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	fake.install(t)

	storage, errEncode := sampleCredential(t).Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	_, errRefresh := handleAuthRefresh(testHost(), mustJSON(t, pluginapi.AuthRefreshRequest{StorageJSON: storage}))
	if errRefresh == nil {
		t.Fatal("a transport failure must surface")
	}
	if status := statusOf(errRefresh, 0); status == http.StatusUnauthorized {
		t.Fatalf("status = %d, want a retryable failure rather than 401", status)
	}
	if strings.Contains(errRefresh.Error(), "重新登录") {
		t.Fatalf("error = %v, must not tell the user to log in again", errRefresh)
	}
}

// Any other business failure stays retryable too (`loomy-auth.ts:350-369`).
func TestAuthRefreshOtherBusinessFailureIsRetryable(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"100500","desc":"服务器开小差了"}`), nil
	}
	fake.install(t)

	storage, errEncode := sampleCredential(t).Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	_, errRefresh := handleAuthRefresh(testHost(), mustJSON(t, pluginapi.AuthRefreshRequest{StorageJSON: storage}))
	if errRefresh == nil {
		t.Fatal("a business failure must surface")
	}
	if status := statusOf(errRefresh, 0); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want a retryable 502", status)
	}
}

// A non-JSON response is reported with its HTTP status, not as expiry.
func TestProbeNonJSONResponseKeepsTheStatus(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(502, `<html>bad gateway</html>`), nil
	}
	fake.install(t)
	errProbe := probeCredential(testHost(), sampleCredential(t), DefaultConfig())
	if errProbe == nil {
		t.Fatal("a gateway error must surface")
	}
	if status := statusOf(errProbe, 0); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if statusOf(errProbe, 0) == http.StatusUnauthorized {
		t.Fatal("a gateway error must never be classified as credential expiry")
	}
}

// A credential without a local expiry gets no probe schedule; a locally expired
// one is re-probed on the reference's 30-minute health cadence instead of
// spinning (`index.ts:490`).
func TestNextProbeAfter(t *testing.T) {
	now := time.Now()
	if got := nextProbeAfter(&Credential{AccessToken: "s"}, now); !got.IsZero() {
		t.Fatalf("nextProbeAfter(no expiry) = %s, want the zero time", got)
	}
	fresh := buildCredential("s", "u", "13800138000", "", DefaultConfig(), now)
	want := fresh.Expiry().Add(-refreshLead)
	if got := nextProbeAfter(fresh, now); !got.Equal(want) {
		t.Fatalf("nextProbeAfter(fresh) = %s, want %s", got, want)
	}
	stale := &Credential{AccessToken: "s", ExpiresAt: "1000"}
	got := nextProbeAfter(stale, now)
	if got.Before(now.Add(29*time.Minute)) || got.After(now.Add(31*time.Minute)) {
		t.Fatalf("nextProbeAfter(stale) = %s, want about 30 minutes out", got)
	}
}

// mustJSON marshals a value for a handler payload.
func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	return raw
}
