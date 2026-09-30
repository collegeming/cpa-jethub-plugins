package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Login tests.
//
// The flow is a server-mediated device authorization: an init call returns an
// authorization URL, the user authorizes in a browser, and the client polls for the
// token. There is NO local callback listener — which is why the plugin has no
// `callback_port` setting, and why `auth.login.start` can return immediately without
// the popup-blocker problem a blocking call would create.

// TestLoginStartReturnsImmediatelyWithAnAuthorizeURL covers the two-step contract.
func TestLoginStartReturnsImmediatelyWithAnAuthorizeURL(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(request.URL, OAuthCLIInitPath) {
			t.Errorf("init called %q", request.URL)
		}
		return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"flow_id":           "flow-abc",
				"poll_token":        "poll-token-value",
				"authorize_url":     "https://bigmodel.cn/login?appId=zcode&state=xyz",
				"expires_at":        float64(nowMillis()/1000 + 600),
				"poll_interval_sec": float64(2),
			},
		})), nil
	}

	value, errStart := handleAuthLoginStart(testHost(), nil)
	if errStart != nil {
		t.Fatalf("auth.login.start: %v", errStart)
	}
	response := decodeResult[pluginapi.AuthLoginStartResponse](t, value)
	if response.URL != "https://bigmodel.cn/login?appId=zcode&state=xyz" {
		t.Fatalf("url = %q, want the server-issued authorize URL", response.URL)
	}
	if response.State == "" {
		t.Fatal("no state was returned, so the flow can never be polled")
	}
	if response.ExpiresAt.IsZero() {
		t.Error("no expiry was returned")
	}
	if response.Provider != ProviderKey {
		t.Errorf("provider = %q", response.Provider)
	}
	// The hint has to say that no local port is involved, because a user who
	// expects a redirect will otherwise wait for one.
	hint, _ := response.Metadata["hint"].(string)
	if !strings.Contains(hint, "不监听") && !strings.Contains(hint, "服务端") {
		t.Errorf("the hint does not explain the server-side completion: %q", hint)
	}
	// The init request carried the CLI session key as its bearer, NOT a user
	// credential: the official client generates a random one the same way.
	calls := fake.callsFor(OAuthCLIInitPath)
	if len(calls) != 1 {
		t.Fatalf("init calls = %d, want 1", len(calls))
	}
	authorization := calls[0].Headers.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) != len("Bearer ")+64 {
		t.Fatalf("init bearer = %q, want a 64-hex-character session key", authorization)
	}
	if calls[0].Headers.Get("X-ZCode-App-Version") == "" {
		t.Error("the init request carries no client version header")
	}
	var initBody map[string]any
	if errUnmarshal := json.Unmarshal(calls[0].Body, &initBody); errUnmarshal != nil {
		t.Fatalf("decode init body: %v", errUnmarshal)
	}
	if initBody["provider"] != loginProvider {
		t.Errorf("init body provider = %v, want %q", initBody["provider"], loginProvider)
	}
	// The flow id must NOT be handed to the caller as the poll state: the state is
	// this plugin's own opaque handle.
	if response.State == "flow-abc" {
		t.Error("the upstream flow id was used as the poll state")
	}
}

// TestLoginStartRejectsAnIncompleteInitResponse covers the shape checks.
func TestLoginStartRejectsAnIncompleteInitResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no flow id", `{"code":0,"data":{"authorize_url":"https://x.y/z"}}`},
		{"no authorize url", `{"code":0,"data":{"flow_id":"f1"}}`},
		{"a non-https authorize url", `{"code":0,"data":{"flow_id":"f1","authorize_url":"http://x.y/z"}}`},
		{"a business error", `{"code":2007,"msg":"internal error"}`},
		{"not json", `not json at all`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.install(t)
			fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				return httpResponse(http.StatusOK, tc.body), nil
			}
			_, errStart := handleAuthLoginStart(testHost(), nil)
			if errStart == nil {
				t.Fatal("an unusable init response was accepted")
			}
			// The session must not be left behind, or a later poll would report
			// "pending" forever for a flow that can never complete.
			loginMu.Lock()
			leftover := len(loginSessions)
			loginMu.Unlock()
			if leftover != 0 {
				t.Fatalf("%d login sessions leaked after a failed init", leftover)
			}
		})
	}
}

// TestLoginStartReportsATransportFailure covers the unreachable-service path.
func TestLoginStartReportsATransportFailure(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	_, errStart := handleAuthLoginStart(testHost(), nil)
	if errStart == nil {
		t.Fatal("expected an error")
	}
	envelope := &abiboot.EnvelopeError{}
	if !asEnvelope(errStart, envelope) {
		t.Fatalf("error type = %T", errStart)
	}
	if !envelope.Retryable {
		t.Error("a transport failure is marked non-retryable")
	}
	// It must NOT be reported as a credential problem: nothing has been
	// authenticated yet.
	if envelope.HTTPStatus == http.StatusUnauthorized {
		t.Error("a login transport failure surfaced as 401, which reads as a dead credential")
	}
}

// TestLoginPollAdvancesOneStepAtATime covers the poll loop's shape.
func TestLoginPollAdvancesOneStepAtATime(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	// A zero interval keeps the test instant; the interval LOGIC is asserted
	// separately below.
	cfg := settings()
	cfg.PollIntervalMS = 1
	withSettings(t, cfg)

	initBody := jsonBody(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"flow_id": "flow-abc", "authorize_url": "https://bigmodel.cn/login",
			"poll_interval_sec": float64(1),
		},
	})
	state := ""
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, OAuthCLIInitPath):
			return httpResponse(http.StatusOK, initBody), nil
		case strings.Contains(request.URL, OAuthCLIPollPrefix):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"status":"pending"}}`), nil
		default:
			t.Fatalf("unexpected call to %s", request.URL)
			return nil, nil
		}
	}

	startValue, errStart := handleAuthLoginStart(testHost(), nil)
	if errStart != nil {
		t.Fatalf("start: %v", errStart)
	}
	state = decodeResult[pluginapi.AuthLoginStartResponse](t, startValue).State

	pollRaw, _ := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: ProviderKey, State: state})
	value, errPoll := handleAuthLoginPoll(testHost(), pollRaw)
	if errPoll != nil {
		t.Fatalf("poll: %v", errPoll)
	}
	response := decodeResult[pluginapi.AuthLoginPollResponse](t, value)
	if response.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending", response.Status)
	}
	// Exactly ONE upstream poll per invocation.
	polls := fake.callsFor(OAuthCLIPollPrefix)
	if len(polls) != 1 {
		t.Fatalf("poll calls = %d, want 1", len(polls))
	}
	// The poll carried the flow id in the path and the session key as its bearer.
	if !strings.HasSuffix(polls[0].URL, "/flow-abc") {
		t.Errorf("poll url = %q, want the flow id appended", polls[0].URL)
	}
	if !strings.HasPrefix(polls[0].Headers.Get("Authorization"), "Bearer ") {
		t.Error("the poll carries no bearer")
	}
}

// TestLoginPollCompletesTheFlow covers the ready path.
func TestLoginPollCompletesTheFlow(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	cfg := settings()
	cfg.PollIntervalMS = 1
	withSettings(t, cfg)

	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, OAuthCLIInitPath):
			return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
				"code": 0,
				"data": map[string]any{
					"flow_id": "flow-abc", "authorize_url": "https://bigmodel.cn/login",
					"poll_interval_sec": float64(1),
				},
			})), nil
		default:
			return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
				"code": 0,
				"data": map[string]any{
					"status": "ready",
					"token":  "the-zcode-jwt",
					"user": map[string]any{
						"user_id": "user-987", "name": "尾号8000",
					},
					"bigmodel": map[string]any{
						"access_token": "the-access-token", "refresh_token": "the-refresh-token",
					},
				},
			})), nil
		}
	}

	startValue, errStart := handleAuthLoginStart(testHost(), nil)
	if errStart != nil {
		t.Fatalf("start: %v", errStart)
	}
	state := decodeResult[pluginapi.AuthLoginStartResponse](t, startValue).State

	pollRaw, _ := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: ProviderKey, State: state})
	value, errPoll := handleAuthLoginPoll(testHost(), pollRaw)
	if errPoll != nil {
		t.Fatalf("poll: %v", errPoll)
	}
	response := decodeResult[pluginapi.AuthLoginPollResponse](t, pollValue(t, value))
	if response.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success", response.Status)
	}
	if len(response.Auth.StorageJSON) == 0 {
		t.Fatal("no credential was returned")
	}
	credential, errParse := ParseCredential(response.Auth.StorageJSON)
	if errParse != nil {
		t.Fatalf("the returned credential does not parse: %v", errParse)
	}
	if credential.ZCodeJWT != "the-zcode-jwt" {
		t.Errorf("zcode_jwt = %q", credential.ZCodeJWT)
	}
	// ⚠ `user_id` must be carried: it is the ONLY stable account identity, and
	// dropping it here is precisely the real defect the reference records (two
	// logins of one account become two separate accounts).
	if credential.UserID != "user-987" {
		t.Errorf("user_id = %q; it must be carried or de-duplication is impossible", credential.UserID)
	}
	// The device id is generated locally, because the server accepts any value.
	if credential.DeviceMid == "" {
		t.Error("no device id was produced, so every billing call would answer 400 code 3001")
	}
	if credential.Source != "plugin" {
		t.Errorf("source = %q, want plugin", credential.Source)
	}
	if response.Auth.Provider != ProviderKey {
		t.Errorf("auth provider = %q", response.Auth.Provider)
	}
	// The session is gone: a second poll reports an expired session rather than
	// replaying the same success.
	if _, found := lookupLoginSession(state); found {
		t.Error("the completed session is still registered")
	}
}

// pollValue normalises a handler result for decoding.
func pollValue(t *testing.T, value any) any {
	t.Helper()
	return value
}

// TestLoginPollGeneratesADistinctDeviceIdPerLogin documents the device-id rule: it
// is NOT an account identity and changes on every login.
func TestLoginPollGeneratesADistinctDeviceIdPerLogin(t *testing.T) {
	first := credentialFromLogin(&loginResult{
		ZCodeJWT: "jwt", BigModelAccessToken: "at", UserID: "user-1", DisplayName: "one",
	}, settings())
	second := credentialFromLogin(&loginResult{
		ZCodeJWT: "jwt", BigModelAccessToken: "at", UserID: "user-1", DisplayName: "one",
	}, settings())
	if first.DeviceMid == second.DeviceMid {
		t.Fatal("two logins produced the same device id, which contradicts the measured behaviour")
	}
	// But the ACCOUNT identity is stable — that is what de-duplication uses.
	if first.identityKey() != second.identityKey() {
		t.Fatalf("the account identity changed between logins: %q vs %q",
			first.identityKey(), second.identityKey())
	}
}

// TestGenerateDeviceMidIsAWellFormedUUID covers the generator.
func TestGenerateDeviceMidIsAWellFormedUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		mid := generateDeviceMid()
		if len(mid) != 36 {
			t.Fatalf("device id %q has length %d, want 36", mid, len(mid))
		}
		if mid[8] != '-' || mid[13] != '-' || mid[18] != '-' || mid[23] != '-' {
			t.Fatalf("device id %q is not hyphenated like a UUID", mid)
		}
		if mid[14] != '4' {
			t.Fatalf("device id %q is not version 4", mid)
		}
		switch mid[19] {
		case '8', '9', 'a', 'b':
		default:
			t.Fatalf("device id %q has a bad variant nibble", mid)
		}
		if seen[mid] {
			t.Fatalf("device id %q was produced twice", mid)
		}
		seen[mid] = true
	}
}

// TestLoginPollTerminalFailureRules covers the failure classification.
//
// The rules come straight from the reference, which copied them from the official
// client: a 4xx other than 408/429 is TERMINAL, while a 5xx or a transport error
// keeps the flow alive.
//
// ⚠ Note that 404 is TERMINAL here, unlike the sibling Qoder device-code flow where
// it means "the user has not finished yet". The official ZCode client draws the line
// at "4xx except 408 and 429", and the reference reproduces that verbatim; a 404 on
// this endpoint means the flow id is unknown to the server, which polling cannot
// fix.
func TestLoginPollTerminalFailureRules(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		transport  error
		wantStatus pluginapi.AuthLoginStatus
	}{
		{"a 400 is terminal", http.StatusBadRequest, `{"msg":"bad request"}`, nil, pluginapi.AuthLoginStatusError},
		{"a 403 is terminal", http.StatusForbidden, `{"msg":"forbidden"}`, nil, pluginapi.AuthLoginStatusError},
		// Terminal by the official client's own rule (4xx except 408/429).
		{"a 404 is terminal", http.StatusNotFound, ``, nil, pluginapi.AuthLoginStatusError},
		{"an unknown flow id is terminal", http.StatusNotFound, `{"code":404,"msg":"flow not found"}`,
			nil, pluginapi.AuthLoginStatusError},
		{"a 408 keeps polling", http.StatusRequestTimeout, ``, nil, pluginapi.AuthLoginStatusPending},
		{"a 429 keeps polling", http.StatusTooManyRequests, ``, nil, pluginapi.AuthLoginStatusPending},
		{"a 500 keeps polling", http.StatusInternalServerError, ``, nil, pluginapi.AuthLoginStatusPending},
		{"a 502 keeps polling", http.StatusBadGateway, ``, nil, pluginapi.AuthLoginStatusPending},
		{"a transport failure keeps polling", 0, ``, errFakeTransport, pluginapi.AuthLoginStatusPending},
		{"a business error keeps polling", http.StatusOK, `{"code":2007,"msg":"internal error"}`, nil, pluginapi.AuthLoginStatusPending},
		{"a failed authorization is terminal", http.StatusOK, `{"code":0,"data":{"status":"failed"}}`, nil, pluginapi.AuthLoginStatusError},
		{"an unknown status is terminal", http.StatusOK, `{"code":0,"data":{"status":"sideways"}}`, nil, pluginapi.AuthLoginStatusError},
		{"a ready response with no token is terminal", http.StatusOK,
			`{"code":0,"data":{"status":"ready","user":{"user_id":"u"}}}`, nil, pluginapi.AuthLoginStatusError},
		{"a ready response missing user_id is terminal", http.StatusOK,
			`{"code":0,"data":{"status":"ready","token":"j","bigmodel":{"access_token":"a"}}}`, nil,
			pluginapi.AuthLoginStatusError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.install(t)
			cfg := settings()
			cfg.PollIntervalMS = 1
			withSettings(t, cfg)
			fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				if strings.HasSuffix(request.URL, OAuthCLIInitPath) {
					return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
						"code": 0,
						"data": map[string]any{
							"flow_id": "flow-abc", "authorize_url": "https://bigmodel.cn/login",
							"poll_interval_sec": float64(1),
						},
					})), nil
				}
				if tc.transport != nil {
					return nil, tc.transport
				}
				return httpResponse(tc.status, tc.body), nil
			}

			startValue, errStart := handleAuthLoginStart(testHost(), nil)
			if errStart != nil {
				t.Fatalf("start: %v", errStart)
			}
			state := decodeResult[pluginapi.AuthLoginStartResponse](t, startValue).State
			pollRaw, _ := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: ProviderKey, State: state})
			value, errPoll := handleAuthLoginPoll(testHost(), pollRaw)
			if errPoll != nil {
				t.Fatalf("poll: %v", errPoll)
			}
			response := decodeResult[pluginapi.AuthLoginPollResponse](t, value)
			if response.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (message: %s)", response.Status, tc.wantStatus, response.Message)
			}
			if response.Message == "" {
				t.Error("no message was returned, so the user sees an unexplained state")
			}
			if tc.wantStatus == pluginapi.AuthLoginStatusError {
				if strings.TrimSpace(response.Message) == "" {
					t.Error("a terminal failure carries no explanation")
				}
			}
		})
	}
}

// TestLoginPollRejectsAnUnknownState covers the stale-session case.
func TestLoginPollRejectsAnUnknownState(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	pollRaw, _ := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: ProviderKey, State: "never-existed"})
	value, errPoll := handleAuthLoginPoll(testHost(), pollRaw)
	if errPoll != nil {
		t.Fatalf("poll: %v", errPoll)
	}
	response := decodeResult[pluginapi.AuthLoginPollResponse](t, value)
	if response.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %q, want error", response.Status)
	}
	if !strings.Contains(response.Message, "重新发起") {
		t.Errorf("the message does not tell the user to restart the flow: %q", response.Message)
	}
}

// TestLoginSessionExpiryIsBoundedByTheServer covers the deadline rule.
//
// ⚠ The server's own validity window WINS when it is shorter. A local deadline that
// outlives it keeps polling a flow the user can no longer authorize, and reports
// "timed out" instead of the more accurate "the flow expired".
func TestLoginSessionExpiryIsBoundedByTheServer(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	// A long local timeout, a short server validity.
	cfg := settings()
	cfg.LoginTimeoutMS = 600_000
	withSettings(t, cfg)

	serverExpiry := nowMillis()/1000 + 5
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"flow_id": "f", "authorize_url": "https://bigmodel.cn/login",
				"expires_at": float64(serverExpiry), "poll_interval_sec": float64(1),
			},
		})), nil
	}
	startValue, errStart := handleAuthLoginStart(testHost(), nil)
	if errStart != nil {
		t.Fatalf("start: %v", errStart)
	}
	response := decodeResult[pluginapi.AuthLoginStartResponse](t, startValue)
	// The reported expiry is the server's, minus the one-second margin.
	if response.ExpiresAt.Unix() > serverExpiry {
		t.Fatalf("expiry = %d, want it bounded by the server's %d", response.ExpiresAt.Unix(), serverExpiry)
	}
}

// TestLoginStartUsesTheConfiguredLocaleHeaders covers the header pass-through.
//
// The login endpoints are on the same origin as everything else, so the source
// identity headers travel with them.
func TestLoginStartUsesTheConfiguredLocaleHeaders(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"flow_id": "f", "authorize_url": "https://bigmodel.cn/login", "poll_interval_sec": float64(2),
			},
		})), nil
	}
	cfg := settings()
	cfg.AppVersion = "9.9.9"
	cfg.Platform = "darwin"
	withSettings(t, cfg)

	if _, errStart := handleAuthLoginStart(testHost(), nil); errStart != nil {
		t.Fatalf("start: %v", errStart)
	}
	calls := fake.callsFor(OAuthCLIInitPath)
	if len(calls) != 1 {
		t.Fatalf("init calls = %d", len(calls))
	}
	if got := calls[0].Headers.Get("User-Agent"); got != "ZCode/9.9.9" {
		t.Errorf("user agent = %q, want the configured version", got)
	}
	if got := calls[0].Headers.Get("X-Platform"); got != "darwin" {
		t.Errorf("platform = %q, want the configured value", got)
	}
}

// TestShutdownClearsLoginSessions covers the lifecycle.
func TestShutdownClearsLoginSessions(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"flow_id": "f", "authorize_url": "https://bigmodel.cn/login", "poll_interval_sec": float64(2),
			},
		})), nil
	}
	if _, errStart := handleAuthLoginStart(testHost(), nil); errStart != nil {
		t.Fatalf("start: %v", errStart)
	}
	loginMu.Lock()
	before := len(loginSessions)
	loginMu.Unlock()
	if before == 0 {
		t.Fatal("no session was registered")
	}
	shutdownLoginSessions()
	loginMu.Lock()
	after := len(loginSessions)
	loginMu.Unlock()
	if after != 0 {
		t.Fatalf("%d sessions survived the shutdown", after)
	}
}

// TestAuthRefreshIsAProbeNotARenewal covers the static-credential contract.
//
// ⚠ ZCode has NO refresh endpoint. The JWT carries no `exp` claim and the server
// knows no renewal. Reporting this honestly is the difference between "log in again"
// and an endless silent retry against an endpoint that does not exist.
func TestAuthRefreshIsAProbeNotARenewal(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(request.URL, BalancePath) {
			t.Fatalf("the probe called %s, want the balance endpoint", request.URL)
		}
		return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"balances": []map[string]any{{
					"plan_id": "p", "show_name": "GLM-5.3-Flash", "unit_type": "token",
					"meter": "model_usage", "total_units": 100_000_000,
					"used_units": 5_460_725, "remaining_units": 94_539_275,
				}},
			},
		})), nil
	}
	credential := sampleCredential()
	storage, _ := credential.Encode()
	refreshRaw, _ := json.Marshal(pluginapi.AuthRefreshRequest{
		AuthID: "zcode-1", AuthProvider: ProviderKey, StorageJSON: storage,
	})
	value, errRefresh := handleAuthRefresh(testHost(), refreshRaw)
	if errRefresh != nil {
		t.Fatalf("auth.refresh: %v", errRefresh)
	}
	response := decodeResult[pluginapi.AuthRefreshResponse](t, value)
	// The credential comes back UNCHANGED: there is nothing to renew.
	parsed, errParse := ParseCredential(response.Auth.StorageJSON)
	if errParse != nil {
		t.Fatalf("the returned credential does not parse: %v", errParse)
	}
	if parsed.ZCodeJWT != credential.ZCodeJWT || parsed.DeviceMid != credential.DeviceMid {
		t.Fatalf("the probe altered the credential: %#v", parsed)
	}
	if response.Auth.Attributes["refreshable"] != "false" {
		t.Errorf("refreshable = %q, want false", response.Auth.Attributes["refreshable"])
	}
	// A re-probe is scheduled: a zero time would mean "never", leaving a
	// server-side death unnoticed until a request failed.
	if response.NextRefreshAfter.IsZero() {
		t.Error("no re-probe was scheduled")
	}
	// No token endpoint was contacted.
	if calls := fake.callsFor("oauth/token"); len(calls) != 0 {
		t.Fatalf("the probe called a token endpoint (%d times), but ZCode has none", len(calls))
	}
}

// TestAuthRefreshReportsADeadCredentialAs401 covers the rotation signal.
func TestAuthRefreshReportsADeadCredentialAs401(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusUnauthorized, `{"code":1002,"msg":"invalid token"}`), nil
	}
	storage, _ := sampleCredential().Encode()
	refreshRaw, _ := json.Marshal(pluginapi.AuthRefreshRequest{AuthID: "zcode-1", StorageJSON: storage})
	_, errRefresh := handleAuthRefresh(testHost(), refreshRaw)
	if errRefresh == nil {
		t.Fatal("a dead credential was reported as healthy")
	}
	envelope := &abiboot.EnvelopeError{}
	if !asEnvelope(errRefresh, envelope) {
		t.Fatalf("error type = %T", errRefresh)
	}
	if envelope.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("http status = %d, want 401 so the host retires the credential", envelope.HTTPStatus)
	}
}

// TestAuthRefreshDoesNotRetireACredentialOnANetworkBlip is the other half of the
// rule: a flaky network must never look like a dead account.
func TestAuthRefreshDoesNotRetireACredentialOnANetworkBlip(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	storage, _ := sampleCredential().Encode()
	refreshRaw, _ := json.Marshal(pluginapi.AuthRefreshRequest{AuthID: "zcode-1", StorageJSON: storage})
	_, errRefresh := handleAuthRefresh(testHost(), refreshRaw)
	if errRefresh == nil {
		t.Fatal("expected an error")
	}
	envelope := &abiboot.EnvelopeError{}
	if !asEnvelope(errRefresh, envelope) {
		t.Fatalf("error type = %T", errRefresh)
	}
	if envelope.HTTPStatus == http.StatusUnauthorized {
		t.Fatal("a transport failure must not retire the credential")
	}
	if !envelope.Retryable {
		t.Error("a transport failure should be retryable")
	}
}

// TestAuthParseRecognisesOurOwnFiles covers the auth-directory discovery.
func TestAuthParseRecognisesOurOwnFiles(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	storage, _ := sampleCredential().Encode()

	parseRaw, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: ProviderKey, FileName: "zcode-existing.json", RawJSON: storage,
	})
	value, errParse := handleAuthParse(nil, parseRaw)
	if errParse != nil {
		t.Fatalf("auth.parse: %v", errParse)
	}
	response := decodeResult[pluginapi.AuthParseResponse](t, value)
	if !response.Handled {
		t.Fatal("our own credential was not recognised")
	}
	// ⚠ The host-supplied file name wins. Deriving a new one would leave the file
	// the host is reading behind and grow a second entry for the same account.
	if response.Auth.FileName != "zcode-existing.json" {
		t.Fatalf("file name = %q, want the host's own name", response.Auth.FileName)
	}
	if response.Auth.Provider != ProviderKey {
		t.Errorf("provider = %q", response.Auth.Provider)
	}

	// A credential that is not ours is declined rather than failing.
	foreignRaw, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: ProviderKey, FileName: "x.json", RawJSON: []byte(`{"access_token":"other-provider"}`),
	})
	foreignValue, errForeign := handleAuthParse(nil, foreignRaw)
	if errForeign != nil {
		t.Fatalf("auth.parse for a foreign credential: %v", errForeign)
	}
	if decodeResult[pluginapi.AuthParseResponse](t, foreignValue).Handled {
		t.Fatal("a foreign credential was claimed")
	}

	// A different provider's request is declined outright.
	otherRaw, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: "qoder", FileName: "q.json", RawJSON: storage,
	})
	otherValue, errOther := handleAuthParse(nil, otherRaw)
	if errOther != nil {
		t.Fatalf("auth.parse for another provider: %v", errOther)
	}
	if decodeResult[pluginapi.AuthParseResponse](t, otherValue).Handled {
		t.Fatal("a request addressed to another provider was claimed")
	}
}

// TestAuthParseMarksTheCredentialAsNotRefreshable covers the metadata contract.
func TestAuthParseMarksTheCredentialAsNotRefreshable(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	storage, _ := sampleCredential().Encode()
	parseRaw, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: ProviderKey, FileName: "z.json", RawJSON: storage})
	value, errParse := handleAuthParse(nil, parseRaw)
	if errParse != nil {
		t.Fatalf("auth.parse: %v", errParse)
	}
	response := decodeResult[pluginapi.AuthParseResponse](t, value)
	if response.Auth.NextRefreshAfter != (pluginapi.AuthData{}).NextRefreshAfter {
		t.Error("a refresh was scheduled for a credential that cannot be refreshed")
	}
	if response.Auth.Metadata["refreshable"] != false {
		t.Errorf("metadata refreshable = %v, want false", response.Auth.Metadata["refreshable"])
	}
	// The account identity is exposed for de-duplication.
	if response.Auth.Metadata["user_id"] != "user-1234567890" {
		t.Errorf("metadata user_id = %v", response.Auth.Metadata["user_id"])
	}
	// ⚠ No `api_key` attribute: the host reads that member as "this is an
	// API-key credential", which silently disables the model alias and exclusion
	// configuration.
	for name := range response.Auth.Attributes {
		if name == "api_key" {
			t.Fatal("the credential carries an api_key attribute, which makes the host treat it as an API-key credential")
		}
	}
}
