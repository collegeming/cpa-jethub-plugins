package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBuildLoginURL(t *testing.T) {
	loginURL := buildLoginURL(34567, "state-value")
	wantQuery := url.Values{
		"source":       {"electron"},
		"redirect_uri": {"http://127.0.0.1:34567/auth/callback"},
		"state":        {"state-value"},
	}.Encode()
	want := PortalBase + "/portal#/login?" + wantQuery
	if loginURL != want {
		t.Fatalf("buildLoginURL = %q, want %q", loginURL, want)
	}
	// The hash fragment is a fragment, not a query: the portal path must keep
	// its literal "#/login".
	if !strings.Contains(loginURL, "/portal#/login?") {
		t.Fatalf("login URL lost the hash fragment: %q", loginURL)
	}
	// redirect_uri contains :// and : and must be percent-encoded, otherwise the
	// portal's validator rejects the login.
	if !strings.Contains(loginURL, "redirect_uri=http%3A%2F%2F127.0.0.1%3A34567%2Fauth%2Fcallback") {
		t.Fatalf("redirect_uri is not percent-encoded: %q", loginURL)
	}
}

func TestCreateLoginSessionState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	session, errSession := createLoginSessionState(now)
	if errSession != nil {
		t.Fatalf("createLoginSessionState: %v", errSession)
	}
	if len(session.UUID) != 32 {
		t.Fatalf("UUID = %q, want 32 hex characters", session.UUID)
	}
	want := "1800000000000"
	if session.FirstKeyfrom != want || session.LatestKeyfrom != want {
		t.Fatalf("keyfrom = %q/%q, want %q", session.FirstKeyfrom, session.LatestKeyfrom, want)
	}
}

func TestExchangeRequestExactShape(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_123)
	session := loginSessionState{UUID: "uuid-1", FirstKeyfrom: "111", LatestKeyfrom: "222"}
	body := exchangeRequest("code-1", session, "2026.9.4", now)

	want := map[string]any{
		"authCode":      "code-1",
		"firstKeyfrom":  "111",
		"latestKeyfrom": "1800000000123",
		"uuid":          "uuid-1",
		"version":       "2026.9.4",
	}
	if len(body) != len(want) {
		t.Fatalf("exchange body has %d keys, want exactly %d: %v", len(body), len(want), body)
	}
	for key, value := range want {
		if body[key] != value {
			t.Fatalf("exchange body[%s] = %v, want %v", key, body[key], value)
		}
	}
}

func TestExchangeAuthCodeSuccess(t *testing.T) {
	fake := newFakeHost().on(httpRoute{
		Method: http.MethodPost,
		Match:  ExchangePath,
		Body: `{"code":0,"msg":"OK","data":{"accessToken":"access-1","refreshToken":"refresh-1",` +
			`"expiresIn":3600,"user":{"id":"user-id","userId":"account-id","yid":"yid","nickname":"昵称"}}}`,
	})
	host := installFakeHost(t, fake)
	session := loginSessionState{UUID: "uuid-1", FirstKeyfrom: "111", LatestKeyfrom: "222"}

	credential, errExchange := exchangeAuthCode(host, "code-1", session, "2026.9.4", time.Now())
	if errExchange != nil {
		t.Fatalf("exchangeAuthCode: %v", errExchange)
	}
	if credential.AccessToken != "access-1" || credential.RefreshToken != "refresh-1" {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.UID != "user-id" || credential.UserID != "account-id" || credential.Nickname != "昵称" {
		t.Fatalf("credential identity = %+v", credential)
	}
	if credential.UUID != "uuid-1" || credential.FirstKeyfrom != "111" {
		t.Fatalf("session identity lost: %+v", credential)
	}
	if credential.LatestKeyfrom == "222" {
		t.Fatalf("latest_keyfrom must come from the exchange request, got %q", credential.LatestKeyfrom)
	}
	if credential.ExpiresAt == "" {
		t.Fatal("expiresIn must produce an expiry")
	}

	requests := fake.requestsFor(ExchangePath)
	if len(requests) != 1 {
		t.Fatalf("exchange requests = %d", len(requests))
	}
	request := requests[0]
	if request.Method != http.MethodPost {
		t.Fatalf("method = %s", request.Method)
	}
	if got := request.Headers.Get("Authorization"); got != "" {
		t.Fatalf("exchange must not send Authorization, got %q", got)
	}
	assertNoTencentHeaders(t, request.Headers)
	sent := map[string]any{}
	if errUnmarshal := json.Unmarshal(request.Body, &sent); errUnmarshal != nil {
		t.Fatalf("decode exchange body: %v", errUnmarshal)
	}
	for _, key := range []string{"authCode", "firstKeyfrom", "latestKeyfrom", "uuid", "version"} {
		if _, present := sent[key]; !present {
			t.Fatalf("exchange body is missing %s: %v", key, sent)
		}
	}
}

func TestExchangeAuthCodeFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "business failure", body: `{"code":40001,"msg":"invalid code"}`},
		{name: "no access token", body: `{"code":0,"msg":"OK","data":{"refreshToken":"r"}}`},
		{name: "null data", body: `{"code":0,"msg":"expired","data":null}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeHost().on(httpRoute{Method: http.MethodPost, Match: ExchangePath, Body: test.body})
			host := installFakeHost(t, fake)
			_, errExchange := exchangeAuthCode(host, "code", loginSessionState{UUID: "u"}, "v", time.Now())
			if errExchange == nil {
				t.Fatal("expected the exchange to fail")
			}
		})
	}

	t.Run("transport failure", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{Method: http.MethodPost, Match: ExchangePath, Err: errFakeTransport})
		host := installFakeHost(t, fake)
		if _, errExchange := exchangeAuthCode(host, "code", loginSessionState{UUID: "u"}, "v", time.Now()); errExchange == nil {
			t.Fatal("expected a transport failure")
		}
	})

	t.Run("nil host", func(t *testing.T) {
		if _, errExchange := exchangeAuthCode(nil, "code", loginSessionState{UUID: "u"}, "v", time.Now()); errExchange == nil {
			t.Fatal("expected a transport failure")
		}
	})
}

// TestLoginSessionLoopbackCallback drives the whole three-step flow in process:
// start the session, hit its loopback callback exactly like the browser would,
// then poll. This proves the callback listener captures the code and that only
// a matching state advances the session.
func TestLoginSessionLoopbackCallback(t *testing.T) {
	fake := newFakeHost().on(httpRoute{
		Method: http.MethodPost,
		Match:  ExchangePath,
		Body:   `{"code":0,"msg":"OK","data":{"accessToken":"access-1","refreshToken":"refresh-1","expiresIn":3600}}`,
	})
	host := installFakeHost(t, fake)

	session, errStart := startLoginSession(time.Now())
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}
	if session.Port <= 0 {
		t.Fatalf("session port = %d", session.Port)
	}
	redirectURI := session.RedirectURI()
	if !strings.Contains(redirectURI, "/auth/callback") {
		t.Fatalf("redirect URI = %q", redirectURI)
	}
	if !strings.Contains(session.LoginURL(), "state="+session.State) {
		t.Fatalf("login URL does not carry the state: %q", session.LoginURL())
	}

	// A callback with a foreign state must be discarded, not accepted.
	response, errGet := http.Get(redirectURI + "?code=bad&state=not-this-state")
	if errGet != nil {
		t.Fatalf("loopback callback (wrong state): %v", errGet)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	// The real callback.
	response, errGet = http.Get(redirectURI + "?code=good-code&state=" + url.QueryEscape(session.State))
	if errGet != nil {
		t.Fatalf("loopback callback: %v", errGet)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	// The poll must advance past "waiting" once the code has been captured.
	var poll pluginapi.AuthLoginPollResponse
	deadline := time.Now().Add(3 * time.Second)
	for {
		value, errPoll := pollLogin(host, session.State, time.Now())
		if errPoll != nil {
			t.Fatalf("pollLogin: %v", errPoll)
		}
		poll = value
		if poll.Status != pluginapi.AuthLoginStatusPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("poll never advanced past pending: %+v", poll)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll status = %s (%s), want success", poll.Status, poll.Message)
	}
	credential, errParse := ParseCredential(poll.Auth.StorageJSON)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if credential.AccessToken != "access-1" {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.UUID != session.UUID || credential.FirstKeyfrom != session.FirstKeyfrom {
		t.Fatalf("credential lost the session identity: %+v", credential)
	}

	// The session is dropped once it has produced a credential; the shared
	// callback listener stays bound for the next sign-in.
	if _, found := lookupLoginSession(session.State, time.Now()); found {
		t.Fatal("a completed session must be forgotten")
	}
	if _, errSecond := pollLogin(host, session.State, time.Now()); errSecond != nil {
		t.Fatalf("second poll: %v", errSecond)
	}
}

func TestPollLoginUnknownState(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)
	response, errPoll := pollLogin(host, "deadbeef", time.Now())
	if errPoll != nil {
		t.Fatalf("pollLogin: %v", errPoll)
	}
	if response.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %s, want error", response.Status)
	}
	if response.Message == "" {
		t.Fatal("an unknown session must explain itself")
	}
}

func TestHandleAuthLoginPollRequiresState(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)
	if _, errPoll := handleAuthLoginPoll(host, []byte(`{}`)); errPoll == nil {
		t.Fatal("a missing state must be rejected")
	}
	value, errPoll := handleAuthLoginPoll(host, mustJSON(t, pluginapi.AuthLoginPollRequest{State: "unknown"}))
	if errPoll != nil {
		t.Fatalf("handleAuthLoginPoll: %v", errPoll)
	}
	response := value.(pluginapi.AuthLoginPollResponse)
	if response.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %s", response.Status)
	}
}

func TestHandleAuthLoginStartReturnsURLImmediately(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)
	value, errStart := handleAuthLoginStart(host, []byte(`{}`))
	if errStart != nil {
		t.Fatalf("handleAuthLoginStart: %v", errStart)
	}
	response := value.(pluginapi.AuthLoginStartResponse)
	if response.Provider != ProviderKey {
		t.Fatalf("Provider = %q", response.Provider)
	}
	if !strings.HasPrefix(response.URL, PortalBase+"/portal#/login?") {
		t.Fatalf("URL = %q", response.URL)
	}
	if response.State == "" {
		t.Fatal("State must be set")
	}
	if !response.ExpiresAt.After(time.Now()) {
		t.Fatalf("ExpiresAt = %v", response.ExpiresAt)
	}
	for _, key := range []string{"uuid", "first_keyfrom", "port", "redirect_uri", "flow"} {
		if _, present := response.Metadata[key]; !present {
			t.Fatalf("metadata is missing %s: %v", key, response.Metadata)
		}
	}
	// The listener must actually be bound while the session is pending.
	session, found := lookupLoginSession(response.State, time.Now())
	if !found {
		t.Fatal("the started session is not registered")
	}
	if session.Port == 0 {
		t.Fatal("the session has no bound port")
	}
}

// TestShutdownLoginSessionsClosesListeners covers the one moment the shared
// callback listener may be released: plugin shutdown. A session lifetime must
// never do it, or the browser finds the published port closed.
func TestShutdownLoginSessionsClosesListeners(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)
	_ = host
	value, errStart := handleAuthLoginStart(nil, nil)
	if errStart != nil {
		t.Fatalf("handleAuthLoginStart: %v", errStart)
	}
	response := value.(pluginapi.AuthLoginStartResponse)
	callbackURI := fmt.Sprintf("http://127.0.0.1:%v%s", response.Metadata["port"], CallbackPath)
	shutdownLoginSessions()
	if _, found := lookupLoginSession(response.State, time.Now()); found {
		t.Fatal("shutdown must drop every pending session")
	}
	if _, errGet := callbackClient().Get(callbackURI); errGet == nil {
		t.Fatalf("shutdown left the listener bound at %s", callbackURI)
	}
}

func TestLoginSessionExpiry(t *testing.T) {
	fake := newFakeHost()
	installFakeHost(t, fake)
	session, errStart := startLoginSession(time.Now())
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}
	// Simulate an expired session.
	session.mu.Lock()
	session.ExpiresAt = time.Now().Add(-time.Second)
	session.mu.Unlock()
	if _, found := lookupLoginSession(session.State, time.Now()); found {
		t.Fatal("an expired session must not be returned")
	}
	_ = session.snapshot
}
