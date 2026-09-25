package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginStartFixture invokes auth.login.start and decodes the reply.
func loginStartFixture(t *testing.T) pluginapi.AuthLoginStartResponse {
	t.Helper()
	value, errStart := handleAuthLoginStart(testHost(), json.RawMessage(`{}`))
	if errStart != nil {
		t.Fatalf("auth.login.start: %v", errStart)
	}
	return decodeResult[pluginapi.AuthLoginStartResponse](t, value)
}

// loginPollFixture invokes auth.login.poll for one state.
func loginPollFixture(t *testing.T, state string) pluginapi.AuthLoginPollResponse {
	t.Helper()
	raw, _ := json.Marshal(pluginapi.AuthLoginPollRequest{State: state})
	value, errPoll := handleAuthLoginPoll(testHost(), raw)
	if errPoll != nil {
		t.Fatalf("auth.login.poll: %v", errPoll)
	}
	return decodeResult[pluginapi.AuthLoginPollResponse](t, value)
}

// TestLoginStartReturnsImmediately is the two-step contract: the URL comes back
// without waiting for the user, because a browser popup opened after a blocking
// call loses the transient activation window (`qoder-oauth.ts:151-159`).
func TestLoginStartReturnsImmediately(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatal("auth.login.start must not perform any upstream request")
		return nil, nil
	}
	host.install(t)
	withSettings(t, DefaultConfig())

	start := time.Now()
	response := loginStartFixture(t)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("auth.login.start blocked for %v", elapsed)
	}
	if response.Provider != ProviderKey || response.State == "" {
		t.Fatalf("response = %#v, want a provider key and an opaque state", response)
	}
	if response.URL == "" {
		t.Fatal("response.URL is empty; the caller has nothing to open")
	}
	if !contains(response.URL, DeviceSelectPath) || !contains(response.URL, "client_id=") {
		t.Fatalf("login URL = %q, want the device select path with a client id", response.URL)
	}
	if response.ExpiresAt.Before(time.Now()) {
		t.Fatalf("ExpiresAt = %v, want a future deadline", response.ExpiresAt)
	}
	if got, _ := response.Metadata["region"]; got != string(RegionGlobal) {
		t.Fatalf("metadata region = %v, want the configured region", got)
	}
}

// TestLoginStartHonoursTheRequestedRegion lets a caller sign in to the other site
// without changing the instance configuration.
func TestLoginStartHonoursTheRequestedRegion(t *testing.T) {
	withSettings(t, DefaultConfig())
	raw := json.RawMessage(`{"metadata":{"region":"qoder-cn"}}`)
	value, errStart := handleAuthLoginStart(testHost(), raw)
	if errStart != nil {
		t.Fatalf("auth.login.start: %v", errStart)
	}
	response := decodeResult[pluginapi.AuthLoginStartResponse](t, value)
	if !contains(response.URL, "qoder.cn") {
		t.Fatalf("login URL = %q, want the CN auth host", response.URL)
	}
}

// TestLoginPollStateMachine walks the device-code flow the way the upstream
// contract describes it: 404 and a token-less 2xx both mean "keep waiting", a
// token means success (`qoder-oauth.ts:247-302`).
func TestLoginPollStateMachine(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	withSettings(t, DefaultConfig())

	start := loginStartFixture(t)
	session, found := lookupLoginSession(start.State)
	if !found {
		t.Fatal("the session started by auth.login.start was not registered")
	}

	// 1. 404 = the user has not authorised yet.
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(404, `{"errorCode":"NotFound"}`), nil
	}
	advancePollClock(session)
	pending := loginPollFixture(t, start.State)
	if pending.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending on 404", pending.Status)
	}
	if len(host.saved) != 0 {
		t.Fatal("a pending poll must not save a credential")
	}

	// 2. 200 without a token also means "keep waiting".
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"status":"pending"}`), nil
	}
	advancePollClock(session)
	stillPending := loginPollFixture(t, start.State)
	if stillPending.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending when no token is present", stillPending.Status)
	}

	// 3. A token completes the flow.
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"token":"tok","refresh_token":"ref","user_id":"u-1","user_name":"nick"}`), nil
	}
	advancePollClock(session)
	success := loginPollFixture(t, start.State)
	if success.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success", success.Status)
	}
	credential, errParse := ParseCredential(success.Auth.StorageJSON)
	if errParse != nil {
		t.Fatalf("parse returned credential: %v", errParse)
	}
	if credential.UID != "u-1" || credential.Nickname != "nick" {
		t.Fatalf("credential identity = %#v, want the device-token user fields", credential)
	}
	if credential.MachineID != session.Device.MachineID {
		t.Fatalf("machine id = %q, want the session's persisted value", credential.MachineID)
	}
	if credential.Region != string(RegionGlobal) {
		t.Fatalf("region = %q, want the session region", credential.Region)
	}
	if _, found := lookupLoginSession(start.State); found {
		t.Fatal("a completed session must be forgotten")
	}
}

// TestLoginPollRespectsTheIntervalWithoutBlocking checks that a host invocation
// is never held for the poll interval: the call returns "pending" and issues no
// request until the interval elapsed (`qoder-oauth.ts:269-302`).
func TestLoginPollRespectsTheIntervalWithoutBlocking(t *testing.T) {
	host := newFakeHost()
	calls := 0
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		calls++
		return httpResponse(404, `{}`), nil
	}
	host.install(t)
	withSettings(t, DefaultConfig())

	start := loginStartFixture(t)
	if response := loginPollFixture(t, start.State); response.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending", response.Status)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	// The immediate second poll must not hit the network.
	second := loginPollFixture(t, start.State)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want the interval to suppress the second poll", calls)
	}
	if second.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending", second.Status)
	}
}

// TestLoginPollFailsOnServerFaults keeps a 5xx from being mistaken for "the user
// is still busy" (`qoder-oauth.ts:282-284`).
func TestLoginPollFailsOnServerFaults(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(503, `{}`), nil
	}
	host.install(t)
	withSettings(t, DefaultConfig())

	start := loginStartFixture(t)
	response := loginPollFixture(t, start.State)
	if response.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %q, want error on HTTP 503", response.Status)
	}
	if _, found := lookupLoginSession(start.State); found {
		t.Fatal("a failed session must be forgotten")
	}
}

// TestLoginPollFailureBudget covers the consecutive transport-failure limit
// (`qoder-oauth.ts:290-299`).
func TestLoginPollFailureBudget(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, abiboot.Errorf("transport", "connection reset")
	}
	host.install(t)
	cfg := DefaultConfig()
	cfg.PollMaxFailures = 2
	withSettings(t, cfg)

	start := loginStartFixture(t)
	session, _ := lookupLoginSession(start.State)

	for attempt := 0; attempt < 2; attempt++ {
		advancePollClock(session)
		response := loginPollFixture(t, start.State)
		if attempt == 0 && response.Status != pluginapi.AuthLoginStatusPending {
			t.Fatalf("attempt 1 status = %q, want pending", response.Status)
		}
		if attempt == 1 && response.Status != pluginapi.AuthLoginStatusError {
			t.Fatalf("attempt 2 status = %q, want error after the failure budget", response.Status)
		}
	}
}

// TestLoginPollUnknownStateIsAnError keeps a stale page from polling forever.
func TestLoginPollUnknownStateIsAnError(t *testing.T) {
	withSettings(t, DefaultConfig())
	response := loginPollFixture(t, "does-not-exist")
	if response.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %q, want error", response.Status)
	}
}

// TestLoginSessionExpiresAfterTheBudget covers the login timeout.
func TestLoginSessionExpiresAfterTheBudget(t *testing.T) {
	withSettings(t, DefaultConfig())
	start := loginStartFixture(t)
	session, found := lookupLoginSession(start.State)
	if !found {
		t.Fatal("session missing")
	}
	session.ExpiresAt = time.Now().Add(-time.Second)
	if _, found := lookupLoginSession(start.State); found {
		t.Fatal("an expired session must not be returned")
	}
	response := loginPollFixture(t, start.State)
	if response.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %q, want error after expiry", response.Status)
	}
}

// advancePollClock rewinds the session's last attempt so the next poll is due.
func advancePollClock(session *loginSession) {
	session.mu.Lock()
	session.LastAttempt = time.Now().Add(-time.Hour)
	session.mu.Unlock()
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}
