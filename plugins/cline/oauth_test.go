package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ---------------------------------------------------------------------------
// test doubles
// ---------------------------------------------------------------------------

// recordedRequest is one captured upstream call.
type recordedRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

// fakeStep is one scripted answer.
type fakeStep struct {
	response *pluginapi.HTTPResponse
	err      error
}

// fakeTransport is a scripted `doer`. Every test in this package that exercises
// a network-shaped path uses it, so the suite never opens a socket: the whole
// device-code state machine, the refresh classifier and the executor are driven
// from canned bodies.
type fakeTransport struct {
	mu    sync.Mutex
	calls []recordedRequest
	steps []fakeStep
}

func (f *fakeTransport) do(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := http.Header{}
	for name, values := range headers {
		copied[name] = append([]string(nil), values...)
	}
	f.calls = append(f.calls, recordedRequest{
		Method:  method,
		URL:     rawURL,
		Headers: copied,
		Body:    append([]byte(nil), body...),
	})
	if len(f.steps) == 0 {
		return nil, fmt.Errorf("fakeTransport: unexpected request %s %s", method, rawURL)
	}
	step := f.steps[0]
	f.steps = f.steps[1:]
	return step.response, step.err
}

func (f *fakeTransport) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeTransport) call(index int) recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index < 0 || index >= len(f.calls) {
		return recordedRequest{}
	}
	return f.calls[index]
}

// installFakeTransport swaps the package transport for one test.
func installFakeTransport(t *testing.T, fake *fakeTransport) {
	t.Helper()
	previous := transportFor
	transportFor = func(*abiboot.Host) doer { return fake.do }
	t.Cleanup(func() { transportFor = previous })
}

// withTestSettings installs a configuration for one test.
func withTestSettings(t *testing.T, cfg Config) {
	t.Helper()
	previous := settings()
	setSettings(cfg)
	t.Cleanup(func() { setSettings(previous) })
}

// jsonResponse builds a canned answer.
func jsonResponse(status int, body string) *pluginapi.HTTPResponse {
	return &pluginapi.HTTPResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(body),
	}
}

// forceAttemptReady clears the interval guard so a test can drive consecutive
// polls. The guard itself is asserted separately.
func forceAttemptReady(t *testing.T, session *loginSession) {
	t.Helper()
	session.mu.Lock()
	session.LastAttempt = time.Time{}
	session.mu.Unlock()
}

// resetLoginState clears the session registry before and after one test.
func resetLoginState(t *testing.T) {
	t.Helper()
	shutdownLoginSessions()
	t.Cleanup(shutdownLoginSessions)
}

// ---------------------------------------------------------------------------
// device authorization (step 1)
// ---------------------------------------------------------------------------

func TestParseDeviceAuthorization(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name       string
		status     int
		body       string
		wantErr    string
		wantExpiry int
		wantInterv int
		wantURL    string
	}{
		{
			name:       "complete url preferred",
			status:     200,
			body:       `{"device_code":"dev","user_code":"ABCD-1234","verification_uri":"https://workos.test/device","verification_uri_complete":"https://workos.test/device?user_code=ABCD-1234","expires_in":600,"interval":7}`,
			wantExpiry: 600_000,
			wantInterv: 7_000,
			wantURL:    "https://workos.test/device?user_code=ABCD-1234",
		},
		{
			name:       "bare uri when complete is absent",
			status:     200,
			body:       `{"device_code":"dev","user_code":"ABCD","verification_uri":"https://workos.test/device"}`,
			wantExpiry: DeviceCodeTTLMS,
			wantInterv: DevicePollIntervalMS,
			wantURL:    "https://workos.test/device",
		},
		{
			name:       "server numbers win over the defaults",
			status:     200,
			body:       `{"device_code":"d","user_code":"u","verification_uri":"https://w/d","expires_in":30.9,"interval":2.5}`,
			wantExpiry: 30_000,
			wantInterv: 2_000,
			wantURL:    "https://w/d",
		},
		{
			name:       "interval is floored at one second",
			status:     200,
			body:       `{"device_code":"d","user_code":"u","verification_uri":"https://w/d","interval":0.5}`,
			wantExpiry: DeviceCodeTTLMS,
			wantInterv: PollIntervalFloorMS,
			wantURL:    "https://w/d",
		},
		{
			name:       "non-numeric knobs fall back",
			status:     200,
			body:       `{"device_code":"d","user_code":"u","verification_uri":"https://w/d","expires_in":"soon","interval":-1}`,
			wantExpiry: DeviceCodeTTLMS,
			wantInterv: DevicePollIntervalMS,
			wantURL:    "https://w/d",
		},
		{
			name:    "missing device code",
			status:  200,
			body:    `{"user_code":"u","verification_uri":"https://w/d"}`,
			wantErr: "设备码授权响应缺少必要字段",
		},
		{
			name:    "missing user code",
			status:  200,
			body:    `{"device_code":"d","verification_uri":"https://w/d"}`,
			wantErr: "设备码授权响应缺少必要字段",
		},
		{
			name:    "missing verification uri",
			status:  200,
			body:    `{"device_code":"d","user_code":"u"}`,
			wantErr: "设备码授权响应缺少必要字段",
		},
		{
			name:    "non-2xx carries the status and the description",
			status:  400,
			body:    `{"error":"invalid_client","error_description":"unknown client"}`,
			wantErr: "设备码授权失败（HTTP 400） - unknown client",
		},
		{
			name:    "html gateway error",
			status:  502,
			body:    `<html>bad gateway</html>`,
			wantErr: "设备码授权失败（HTTP 502）",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			authorization, errParse := parseDeviceAuthorization(testCase.status, []byte(testCase.body), cfg)
			if testCase.wantErr != "" {
				if errParse == nil {
					t.Fatalf("expected an error containing %q", testCase.wantErr)
				}
				if !strings.Contains(errParse.Error(), testCase.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", errParse.Error(), testCase.wantErr)
				}
				if !strings.Contains(errParse.Error(), authServiceErrorPrefix) {
					t.Errorf("the module's own errors carry the %q prefix: %q", authServiceErrorPrefix, errParse.Error())
				}
				return
			}
			if errParse != nil {
				t.Fatalf("unexpected error: %v", errParse)
			}
			if authorization.ExpiresInMS != testCase.wantExpiry {
				t.Errorf("ExpiresInMS = %d, want %d", authorization.ExpiresInMS, testCase.wantExpiry)
			}
			if authorization.IntervalMS != testCase.wantInterv {
				t.Errorf("IntervalMS = %d, want %d", authorization.IntervalMS, testCase.wantInterv)
			}
			if got := authorization.loginURL(); got != testCase.wantURL {
				t.Errorf("loginURL = %q, want %q", got, testCase.wantURL)
			}
		})
	}
}

// TestRequestDeviceAuthorizationShape pins the exact form body and the absence
// of every other header (`cline-oauth.ts:183-188`): no Authorization, no client
// headers.
func TestRequestDeviceAuthorizationShape(t *testing.T) {
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(200,
		`{"device_code":"d","user_code":"u","verification_uri":"https://w/d"}`)}}}
	authorization, errAuthorize := requestDeviceAuthorization(fake.do, DefaultConfig())
	if errAuthorize != nil {
		t.Fatalf("requestDeviceAuthorization: %v", errAuthorize)
	}
	if authorization.DeviceCode != "d" {
		t.Fatalf("device code = %q", authorization.DeviceCode)
	}
	call := fake.call(0)
	if call.Method != http.MethodPost || call.URL != WorkOSBase+DeviceAuthorizationPath {
		t.Fatalf("request = %s %s", call.Method, call.URL)
	}
	if got := call.Headers.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := call.Headers.Get("Authorization"); got != "" {
		t.Errorf("the device call must not be authenticated, got %q", got)
	}
	for _, pair := range clientHeaderPairs {
		if got := call.Headers.Get(pair[0]); got != "" {
			t.Errorf("client header %s must not be sent to WorkOS, got %q", pair[0], got)
		}
	}
	if want := "client_id=" + WorkOSClientID; string(call.Body) != want {
		t.Errorf("body = %q, want %q", string(call.Body), want)
	}
}

// TestDeviceTokenRequestEscaping pins the URLSearchParams escaping that the
// TypeScript unit test asserts byte for byte
// (`tests/unit/cline-oauth.spec.ts:236`).
func TestDeviceTokenRequestEscaping(t *testing.T) {
	body := string(deviceTokenRequest("dev-1"))
	// url.Values.Encode sorts by key, so the order is client_id, device_code,
	// grant_type; what matters is the escaping of the grant type.
	if !strings.Contains(body, "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code") {
		t.Fatalf("grant type must be percent-encoded, got %q", body)
	}
	if strings.Contains(body, "grant_type=urn:ietf") {
		t.Fatalf("the grant type must not be sent unescaped: %q", body)
	}
	for _, fragment := range []string{"device_code=dev-1", "client_id=" + WorkOSClientID} {
		if !strings.Contains(body, fragment) {
			t.Errorf("body %q is missing %q", body, fragment)
		}
	}
}

// ---------------------------------------------------------------------------
// token poll classification (step 2)
// ---------------------------------------------------------------------------

// TestClassifyDeviceToken is the poll state machine of `cline-oauth.ts:256-291`.
// The decision comes from the body, never from the HTTP status: WorkOS answers
// 400 for authorization_pending.
func TestClassifyDeviceToken(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantStatus  devicePollStatus
		wantMessage string
	}{
		{
			name:   "authorization_pending on 400 is not an error",
			status: 400, body: `{"error":"authorization_pending"}`,
			wantStatus: devicePollPending,
		},
		{
			name:   "authorization_pending on 200 is still pending",
			status: 200, body: `{"error":"authorization_pending"}`,
			wantStatus: devicePollPending,
		},
		{
			name:   "slow_down",
			status: 400, body: `{"error":"slow_down"}`,
			wantStatus: devicePollSlowDown,
		},
		{
			name:   "access_denied keeps the description",
			status: 400, body: `{"error":"access_denied","error_description":"user denied"}`,
			wantStatus: devicePollFailed, wantMessage: "user denied",
		},
		{
			name:   "access_denied without a description",
			status: 400, body: `{"error":"access_denied"}`,
			wantStatus: devicePollFailed, wantMessage: "WorkOS 授权失败",
		},
		{
			name:   "expired_token is terminal",
			status: 400, body: `{"error":"expired_token"}`,
			wantStatus: devicePollFailed, wantMessage: "WorkOS 授权失败",
		},
		{
			name:   "invalid_grant is terminal",
			status: 400, body: `{"error":"invalid_grant"}`,
			wantStatus: devicePollFailed, wantMessage: "WorkOS 授权失败",
		},
		{
			name:   "unknown error code carries the HTTP status",
			status: 400, body: `{"error":"invalid_request","error_description":"nope"}`,
			wantStatus: devicePollFailed, wantMessage: "WorkOS token 轮询失败（HTTP 400） - nope",
		},
		{
			name:   "html 502 decodes to an empty object",
			status: 502, body: `<html>bad gateway</html>`,
			wantStatus: devicePollFailed, wantMessage: "WorkOS token 轮询失败（HTTP 502）",
		},
		{
			name:   "empty body on 502",
			status: 502, body: `{}`,
			wantStatus: devicePollFailed, wantMessage: "WorkOS token 轮询失败（HTTP 502）",
		},
		{
			name:   "success needs both tokens",
			status: 200, body: `{"access_token":"workos:eyJ","refresh_token":"tmg"}`,
			wantStatus: devicePollSuccess,
		},
		{
			name:   "success without a refresh token is a deterministic failure",
			status: 200, body: `{"access_token":"workos:eyJ"}`,
			wantStatus: devicePollFailed, wantMessage: "WorkOS token 响应缺少必要字段",
		},
		{
			name:   "success without an access token",
			status: 200, body: `{"refresh_token":"tmg"}`,
			wantStatus: devicePollFailed, wantMessage: "WorkOS token 响应缺少必要字段",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tokens, status, message := classifyDeviceToken(testCase.status, []byte(testCase.body))
			if status != testCase.wantStatus {
				t.Fatalf("status = %q, want %q (message %q)", status, testCase.wantStatus, message)
			}
			if testCase.wantMessage != "" && !strings.Contains(message, testCase.wantMessage) {
				t.Fatalf("message = %q, want it to contain %q", message, testCase.wantMessage)
			}
			if testCase.wantStatus == devicePollSuccess {
				if tokens.AccessToken == "" || tokens.RefreshToken == "" {
					t.Fatalf("success must carry both tokens: %+v", tokens)
				}
				if message != "" {
					t.Errorf("a success must not carry a message: %q", message)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the full login session
// ---------------------------------------------------------------------------

// TestLoginSessionStateMachine drives one login from the device authorization to
// a stored credential, exercising the immediate first poll, the interval guard
// and the cumulative slow_down penalty.
func TestLoginSessionStateMachine(t *testing.T) {
	resetLoginState(t)
	cfg := DefaultConfig()
	withTestSettings(t, cfg)

	fake := &fakeTransport{steps: []fakeStep{
		{response: jsonResponse(200, `{"device_code":"dev-1","user_code":"ABCD-1234","verification_uri":"https://workos.test/device","verification_uri_complete":"https://workos.test/device?user_code=ABCD-1234","expires_in":300,"interval":5}`)},
		{response: jsonResponse(400, `{"error":"authorization_pending"}`)},
		{response: jsonResponse(400, `{"error":"authorization_pending"}`)},
		{response: jsonResponse(400, `{"error":"slow_down"}`)},
		{response: jsonResponse(200, `{"access_token":"workos:eyJ","refresh_token":"workos-refresh"}`)},
		{response: jsonResponse(200, `{"success":true,"data":{"accessToken":"workos:eyJ","refreshToken":"tmgEeM","expiresAt":"2026-09-25T05:23:47.000Z","userInfo":{"clineUserId":"usr-1","email":"a@b.c","firstName":"Ada","lastName":"Lovelace"}}}`)},
	}}
	installFakeTransport(t, fake)

	session, errStart := startLoginSession(fake.do, cfg)
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}
	if session.LoginURL() != "https://workos.test/device?user_code=ABCD-1234" {
		t.Fatalf("login url = %q", session.LoginURL())
	}
	if session.IntervalMS != 5_000 {
		t.Fatalf("negotiated interval = %d", session.IntervalMS)
	}
	if session.Device.UserCode != "ABCD-1234" {
		t.Fatalf("user code = %q", session.Device.UserCode)
	}

	// The FIRST poll is immediate: there is no pre-sleep.
	response, errPoll := advanceLoginSession(fake.do, session, session.State, cfg)
	if errPoll != nil {
		t.Fatalf("first poll: %v", errPoll)
	}
	if response.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("first poll status = %q", response.Status)
	}
	if fake.callCount() != 2 {
		t.Fatalf("the first poll must reach upstream immediately: %d calls", fake.callCount())
	}

	// A second poll inside the interval must NOT reach upstream.
	response, errPoll = advanceLoginSession(fake.do, session, session.State, cfg)
	if errPoll != nil {
		t.Fatalf("second poll: %v", errPoll)
	}
	if response.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("second poll status = %q", response.Status)
	}
	if fake.callCount() != 2 {
		t.Fatalf("the interval guard did not hold: %d calls", fake.callCount())
	}

	// slow_down accumulates: +1 s and never a reset.
	forceAttemptReady(t, session)
	if _, errPoll = advanceLoginSession(fake.do, session, session.State, cfg); errPoll != nil {
		t.Fatalf("third poll: %v", errPoll)
	}
	forceAttemptReady(t, session)
	response, errPoll = advanceLoginSession(fake.do, session, session.State, cfg)
	if errPoll != nil {
		t.Fatalf("fourth poll: %v", errPoll)
	}
	if response.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("slow_down must stay pending, got %q", response.Status)
	}
	if session.IntervalMS != 6_000 {
		t.Fatalf("slow_down interval = %d, want 6000", session.IntervalMS)
	}
	// The next poll succeeds, and the accumulated penalty is still there.
	forceAttemptReady(t, session)
	response, errPoll = advanceLoginSession(fake.do, session, session.State, cfg)
	if errPoll != nil {
		t.Fatalf("fifth poll: %v", errPoll)
	}
	if session.IntervalMS != 6_000 {
		t.Fatalf("the slow_down penalty was reset: %d", session.IntervalMS)
	}

	// Success: step 3 exchanges the WorkOS tokens for Cline tokens.
	if response.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("final status = %q (%s)", response.Status, response.Message)
	}
	credential, errParse := ParseCredential(response.Auth.StorageJSON)
	if errParse != nil {
		t.Fatalf("the returned credential must be parseable: %v", errParse)
	}
	if credential.AccessToken != "workos:eyJ" {
		t.Errorf("stored access token = %q", credential.AccessToken)
	}
	if credential.AccountID != "usr-1" || credential.Email != "a@b.c" {
		t.Errorf("stored identity = %+v", credential)
	}
	if response.Auth.FileName == "" {
		t.Error("the auth record needs a file name")
	}

	// The register exchange must be camelCase JSON with the client headers and
	// no Authorization header.
	register := fake.call(5)
	if register.URL != APIBase+RegisterPath {
		t.Fatalf("register url = %q", register.URL)
	}
	if got := register.Headers.Get("Authorization"); got != "" {
		t.Errorf("register must not be authenticated, got %q", got)
	}
	if got := register.Headers.Get("X-CLIENT-TYPE"); got != "cline-sdk" {
		t.Errorf("register is missing the client headers, got %q", got)
	}
	body := string(register.Body)
	if !strings.Contains(body, `"accessToken":"workos:eyJ"`) || !strings.Contains(body, `"refreshToken":"workos-refresh"`) {
		t.Errorf("register body = %s", body)
	}

	// A finished session is forgotten: a repeat poll reports it as missing.
	after, errPoll := advanceLoginSession(fake.do, session, session.State, cfg)
	if errPoll != nil {
		t.Fatalf("post-success poll: %v", errPoll)
	}
	if after.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("a terminal session keeps reporting its outcome, got %q", after.Status)
	}
	if _, found := lookupLoginSession(session.State); found {
		t.Error("a completed session must not stay registered")
	}
}

// TestLoginSessionSlowDownIsCumulativeAcrossCalls pins the "never reset" rule
// with two consecutive penalties.
func TestLoginSessionSlowDownIsCumulativeAcrossCalls(t *testing.T) {
	resetLoginState(t)
	cfg := DefaultConfig()
	withTestSettings(t, cfg)

	fake := &fakeTransport{steps: []fakeStep{
		{response: jsonResponse(200, `{"device_code":"d","user_code":"u","verification_uri":"https://w/d","interval":5}`)},
		{response: jsonResponse(400, `{"error":"slow_down"}`)},
		{response: jsonResponse(400, `{"error":"slow_down"}`)},
	}}
	installFakeTransport(t, fake)
	session, errStart := startLoginSession(fake.do, cfg)
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}
	for _, want := range []int{6_000, 7_000} {
		forceAttemptReady(t, session)
		response, errPoll := advanceLoginSession(fake.do, session, session.State, cfg)
		if errPoll != nil {
			t.Fatalf("poll: %v", errPoll)
		}
		if response.Status != pluginapi.AuthLoginStatusPending {
			t.Fatalf("status = %q", response.Status)
		}
		if session.IntervalMS != want {
			t.Fatalf("interval = %d, want %d", session.IntervalMS, want)
		}
	}
}

// TestLoginSessionTerminalErrors pins that a terminal poll answer fails the
// session, reports the message and drops the session.
func TestLoginSessionTerminalErrors(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{"access_denied", `{"error":"access_denied","error_description":"user denied"}`, "user denied"},
		{"expired_token", `{"error":"expired_token"}`, "WorkOS 授权失败"},
		{"invalid_grant", `{"error":"invalid_grant"}`, "WorkOS 授权失败"},
		{"unknown error", `{"error":"server_error"}`, "WorkOS token 轮询失败（HTTP 400）"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resetLoginState(t)
			cfg := DefaultConfig()
			withTestSettings(t, cfg)
			fake := &fakeTransport{steps: []fakeStep{
				{response: jsonResponse(200, `{"device_code":"d","user_code":"u","verification_uri":"https://w/d"}`)},
				{response: jsonResponse(400, testCase.body)},
			}}
			installFakeTransport(t, fake)
			session, errStart := startLoginSession(fake.do, cfg)
			if errStart != nil {
				t.Fatalf("startLoginSession: %v", errStart)
			}
			response, errPoll := advanceLoginSession(fake.do, session, session.State, cfg)
			if errPoll != nil {
				t.Fatalf("poll: %v", errPoll)
			}
			if response.Status != pluginapi.AuthLoginStatusError {
				t.Fatalf("status = %q, want error", response.Status)
			}
			if !strings.Contains(response.Message, testCase.wantMessage) {
				t.Fatalf("message = %q, want it to contain %q", response.Message, testCase.wantMessage)
			}
			if !strings.Contains(response.Message, authServiceErrorPrefix) {
				t.Errorf("the module's own errors carry the %q prefix: %q", authServiceErrorPrefix, response.Message)
			}
			if _, found := lookupLoginSession(session.State); found {
				t.Error("a failed session must be dropped")
			}
		})
	}
}

// TestLoginSessionTimeout pins the deadline rule: expiry is reported as a
// timeout and the session is dropped.
func TestLoginSessionTimeout(t *testing.T) {
	resetLoginState(t)
	cfg := DefaultConfig()
	withTestSettings(t, cfg)
	fake := &fakeTransport{steps: []fakeStep{
		{response: jsonResponse(200, `{"device_code":"d","user_code":"u","verification_uri":"https://w/d"}`)},
	}}
	installFakeTransport(t, fake)
	session, errStart := startLoginSession(fake.do, cfg)
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}
	session.mu.Lock()
	session.ExpiresAt = time.Now().Add(-time.Second)
	session.mu.Unlock()

	response, errPoll := advanceLoginSession(fake.do, session, session.State, cfg)
	if errPoll != nil {
		t.Fatalf("poll: %v", errPoll)
	}
	if response.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %q, want error", response.Status)
	}
	if !strings.Contains(response.Message, "登录等待已超时") {
		t.Fatalf("message = %q", response.Message)
	}
	if fake.callCount() != 1 {
		t.Errorf("an expired session must not reach upstream: %d calls", fake.callCount())
	}
	if _, found := lookupLoginSession(session.State); found {
		t.Error("an expired session must be dropped")
	}
}

// TestLoginSessionExpiryThroughLookup pins that the registry itself refuses an
// expired session, which is the path `auth.login.poll` takes after a restart of
// the page.
func TestLoginSessionExpiryThroughLookup(t *testing.T) {
	resetLoginState(t)
	session := &loginSession{
		State:     "expired-state",
		Device:    &deviceAuthorization{DeviceCode: "d"},
		CreatedAt: time.Now().Add(-time.Hour),
		ExpiresAt: time.Now().Add(-time.Minute),
		Status:    pluginapi.AuthLoginStatusPending,
	}
	loginMu.Lock()
	loginSessions[session.State] = session
	loginMu.Unlock()
	if _, found := lookupLoginSession(session.State); found {
		t.Fatal("an expired session must not be found")
	}
	if status, message, _ := session.snapshot(); status != pluginapi.AuthLoginStatusError ||
		!strings.Contains(message, "登录等待已超时") {
		t.Fatalf("the session must record the timeout: %q %q", status, message)
	}
}

// TestLoginSessionTransportFailureBudget pins the consecutive-failure rule: the
// budget is consumed only by TRANSPORT failures and reset by any HTTP answer
// (`cline-oauth.ts:254`, `:296-302`).
func TestLoginSessionTransportFailureBudget(t *testing.T) {
	resetLoginState(t)
	cfg := DefaultConfig()
	withTestSettings(t, cfg)
	steps := []fakeStep{{response: jsonResponse(200, `{"device_code":"d","user_code":"u","verification_uri":"https://w/d"}`)}}
	for index := 0; index < 4; index++ {
		steps = append(steps, fakeStep{err: fmt.Errorf("dial tcp: connection refused")})
	}
	// One HTTP answer resets the counter…
	steps = append(steps, fakeStep{response: jsonResponse(400, `{"error":"authorization_pending"}`)})
	for index := 0; index < 4; index++ {
		steps = append(steps, fakeStep{err: fmt.Errorf("dial tcp: connection refused")})
	}
	// …and the fifth consecutive failure is terminal.
	steps = append(steps, fakeStep{err: fmt.Errorf("dial tcp: connection refused")})
	fake := &fakeTransport{steps: steps}
	installFakeTransport(t, fake)

	session, errStart := startLoginSession(fake.do, cfg)
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}
	for index := 0; index < 4; index++ {
		forceAttemptReady(t, session)
		response, errPoll := advanceLoginSession(fake.do, session, session.State, cfg)
		if errPoll != nil {
			t.Fatalf("failure %d: %v", index, errPoll)
		}
		if response.Status != pluginapi.AuthLoginStatusPending {
			t.Fatalf("failure %d status = %q", index, response.Status)
		}
	}
	forceAttemptReady(t, session)
	if _, errPoll := advanceLoginSession(fake.do, session, session.State, cfg); errPoll != nil {
		t.Fatalf("reset poll: %v", errPoll)
	}
	if session.Failures != 0 {
		t.Fatalf("any HTTP response must reset the failure counter: %d", session.Failures)
	}
	var last pluginapi.AuthLoginPollResponse
	for index := 0; index < 5; index++ {
		forceAttemptReady(t, session)
		response, errPoll := advanceLoginSession(fake.do, session, session.State, cfg)
		if errPoll != nil {
			t.Fatalf("second round %d: %v", index, errPoll)
		}
		last = response
	}
	if last.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("the fifth consecutive failure must be terminal, got %q", last.Status)
	}
	if !strings.Contains(last.Message, "无法连接 Cline 登录服务（连续 5 次失败）") {
		t.Fatalf("message = %q", last.Message)
	}
}

// TestUnknownLoginState pins the host-facing answer for a state nobody knows.
func TestUnknownLoginState(t *testing.T) {
	resetLoginState(t)
	if _, found := lookupLoginSession("nope"); found {
		t.Fatal("an unknown state must not resolve")
	}
	value, errHandler := handleAuthLoginPoll(nil, []byte(`{"state":"nope"}`))
	if errHandler != nil {
		t.Fatalf("handleAuthLoginPoll: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.AuthLoginPollResponse)
	if !okResponse {
		t.Fatalf("unexpected reply type %T", value)
	}
	if response.Status != pluginapi.AuthLoginStatusError || !strings.Contains(response.Message, "登录会话不存在或已超时") {
		t.Fatalf("reply = %+v", response)
	}
}

// ---------------------------------------------------------------------------
// refresh (step 4)
// ---------------------------------------------------------------------------

// TestRefreshRequestBodyShape pins the contract that a wrong field name turns
// into a generic auth failure: the body is camelCase `refreshToken` +
// `grantType`, NOT OAuth's `refresh_token`/`grant_type` (`cline.ts:251-268`).
func TestRefreshRequestBodyShape(t *testing.T) {
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(200,
		`{"success":true,"data":{"accessToken":"workos:new","refreshToken":"new-refresh","expiresAt":"2026-09-25T05:23:47.000Z"}}`)}}}
	credential := &Credential{
		AccessToken:  "workos:old",
		RefreshToken: "old-refresh",
		AccountID:    "usr-1",
		Email:        "a@b.c",
	}
	refreshed, errRefresh := refreshCredential(fake.do, credential)
	if errRefresh != nil {
		t.Fatalf("refreshCredential: %v", errRefresh)
	}

	call := fake.call(0)
	if call.Method != http.MethodPost || call.URL != APIBase+RefreshPath {
		t.Fatalf("request = %s %s", call.Method, call.URL)
	}
	if got := call.Headers.Get("Authorization"); got != "" {
		t.Errorf("the refresh call must not be authenticated, got %q", got)
	}
	if got := call.Headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := call.Headers.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
	for _, pair := range clientHeaderPairs {
		if got := call.Headers.Get(pair[0]); got != pair[1] {
			t.Errorf("client header %s = %q, want %q", pair[0], got, pair[1])
		}
	}

	want := `{"grantType":"refresh_token","refreshToken":"old-refresh"}`
	if string(call.Body) != want {
		t.Fatalf("body = %s, want %s", call.Body, want)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(call.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	for _, forbidden := range []string{"refresh_token", "grant_type"} {
		if _, present := decoded[forbidden]; present {
			t.Errorf("the snake_case field %q must not be sent", forbidden)
		}
	}
	if refreshed.AccessToken != "workos:new" || refreshed.RefreshToken != "new-refresh" {
		t.Errorf("refreshed credential = %+v", refreshed)
	}
	if refreshed.AccountID != "usr-1" || refreshed.Email != "a@b.c" {
		t.Errorf("identity fields must survive a refresh: %+v", refreshed)
	}
}

// TestRefreshWithoutRefreshToken pins the terminal case of `cline-auth.ts:304-306`.
func TestRefreshWithoutRefreshToken(t *testing.T) {
	fake := &fakeTransport{}
	_, errRefresh := refreshCredential(fake.do, &Credential{AccessToken: "workos:t"})
	if errRefresh == nil {
		t.Fatal("a credential without a refresh token must be rejected before any request")
	}
	if fake.callCount() != 0 {
		t.Errorf("no request may be issued: %d", fake.callCount())
	}
	if status := statusOf(errRefresh, 0); status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 so the host parks the credential", status)
	}
}

// TestRefreshCredentialTransportFailure pins the retryable classification.
func TestRefreshCredentialTransportFailure(t *testing.T) {
	fake := &fakeTransport{steps: []fakeStep{{err: fmt.Errorf("proxy unreachable")}}}
	_, errRefresh := refreshCredential(fake.do, &Credential{AccessToken: "workos:t", RefreshToken: "r"})
	if errRefresh == nil {
		t.Fatal("a transport failure must surface")
	}
	if status := statusOf(errRefresh, 0); status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (retryable)", status)
	}
}

// TestClassifyRefreshResponse is the classification table of
// `cline-auth.ts:372-394`.
func TestClassifyRefreshResponse(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantClass refreshClass
		wantText  string
	}{
		{"401 is terminal", 401, `{"error":"Unauthorized"}`, refreshTerminal, "refresh_token 已失效"},
		{"403 is terminal", 403, `{"error":"forbidden"}`, refreshTerminal, "refresh_token 已失效"},
		{"500 is retryable", 500, `{"error":"boom"}`, refreshRetryable, "HTTP 500"},
		{"429 is retryable", 429, `{"error":"slow down"}`, refreshRetryable, "HTTP 429"},
		{"non-json 2xx is retryable", 200, `<html>ok</html>`, refreshRetryable, "不是 JSON"},
		{"json array is terminal (no access token)", 200, `[1,2]`, refreshTerminal, "续期响应缺少访问令牌"},
		{"empty access token is terminal", 200, `{"success":true,"data":{"refreshToken":"r"}}`, refreshTerminal, "续期响应缺少访问令牌"},
		{"usable answer", 200, `{"success":true,"data":{"accessToken":"workos:new"}}`, refreshOK, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tokens, class, message := classifyRefreshResponse(testCase.status, []byte(testCase.body))
			if class != testCase.wantClass {
				t.Fatalf("class = %v, want %v (message %q)", class, testCase.wantClass, message)
			}
			if testCase.wantText != "" && !strings.Contains(message, testCase.wantText) {
				t.Fatalf("message = %q, want it to contain %q", message, testCase.wantText)
			}
			if testCase.wantClass == refreshOK && tokens.AccessToken == "" {
				t.Fatal("a successful classification must carry the token")
			}
		})
	}
}

// TestRefreshTerminalStatusIsAuth pins that a terminal refresh reaches the host
// as a 401, which is what makes it park the credential.
func TestRefreshTerminalStatusIsAuth(t *testing.T) {
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(401,
		`{"error":"Unauthorized: Please make sure you're using the latest version of Cline and re-authenticate your Cline account."}`)}}}
	_, errRefresh := refreshCredential(fake.do, &Credential{AccessToken: "workos:t", RefreshToken: "dead"})
	if errRefresh == nil {
		t.Fatal("a 401 refresh must surface")
	}
	if status := statusOf(errRefresh, 0); status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}
