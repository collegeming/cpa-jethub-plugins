package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Tests for the device-code login: the request bodies, PKCE, BOTH pending
// conventions, the strict grant validation, and the expiry rule.
//
// Every test is offline. Nothing here talks to account.minimax.cn; the request
// shapes are asserted against the forms the code builds, which is what the
// reference's own unit tests do — and, as the file comment on `auth.go` says,
// is the only verification this path has ever had.

// parseForm decodes an urlencoded body, failing the test on a malformed one.
func parseForm(t *testing.T, body []byte) url.Values {
	t.Helper()
	values, errParse := url.ParseQuery(string(body))
	if errParse != nil {
		t.Fatalf("parse form body: %v (body=%q)", errParse, body)
	}
	return values
}

func TestDeviceCodeBodyCarriesPKCEAndOfficialConstants(t *testing.T) {
	form := deviceCodeBody("CHALLENGE-VALUE")
	cases := []struct{ key, want string }{
		{"client_id", "mcode-public"},
		{"scope", "agent.default"},
		{"audience", "agent-backend"},
		{"code_challenge", "CHALLENGE-VALUE"},
		{"code_challenge_method", "S256"},
	}
	for _, testCase := range cases {
		if got := form.Get(testCase.key); got != testCase.want {
			t.Errorf("device code form[%s] = %q, want %q", testCase.key, got, testCase.want)
		}
	}
	// ⚠️ There is NO redirect_uri: the flow owns no callback listener, and the
	// reference does not send one (`minimax-oauth.ts:buildMinimaxDeviceCodeBody`).
	if _, present := form["redirect_uri"]; present {
		t.Error("device code form carries redirect_uri, but the flow has no callback listener")
	}
}

func TestPollBodyUsesDeviceCodeGrant(t *testing.T) {
	auth := deviceAuthorization{
		DeviceCode:   strings.Repeat("d", 43),
		CodeVerifier: "the-real-verifier",
	}
	form := pollBody(auth)
	cases := []struct{ key, want string }{
		{"grant_type", "urn:ietf:params:oauth:grant-type:device_code"},
		{"device_code", strings.Repeat("d", 43)},
		{"client_id", "mcode-public"},
		{"code_verifier", "the-real-verifier"},
	}
	for _, testCase := range cases {
		if got := form.Get(testCase.key); got != testCase.want {
			t.Errorf("poll form[%s] = %q, want %q", testCase.key, got, testCase.want)
		}
	}
}

func TestRefreshBodyUsesRefreshGrant(t *testing.T) {
	form := refreshBody("the-refresh-token")
	want := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": "the-refresh-token",
		"client_id":     "mcode-public",
		"scope":         "agent.default",
		"audience":      "agent-backend",
	}
	for key, expected := range want {
		if got := form.Get(key); got != expected {
			t.Errorf("refresh form[%s] = %q, want %q", key, got, expected)
		}
	}
}

// TestPKCEChallengeIsSHA256OfVerifier locks the PKCE ALGORITHM, not merely the
// presence of two fields.
//
// ⚠️ A wrong challenge makes the SERVER reject the exchange, and the error it
// returns points at `invalid_grant` / a verifier mismatch rather than at PKCE.
// Asserting only "both fields are non-empty" would pass with any garbage.
func TestPKCEChallengeIsSHA256OfVerifier(t *testing.T) {
	pair, errPKCE := newPKCE()
	if errPKCE != nil {
		t.Fatalf("newPKCE: %v", errPKCE)
	}
	sum := sha256.Sum256([]byte(pair.CodeVerifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if pair.CodeChallenge != want {
		t.Errorf("code_challenge = %q, want base64url(sha256(verifier)) = %q", pair.CodeChallenge, want)
	}
	// 32 random bytes ⇒ 43 base64url characters, no padding.
	if len(pair.CodeVerifier) != 43 {
		t.Errorf("code_verifier length = %d, want 43", len(pair.CodeVerifier))
	}
	for _, value := range []string{pair.CodeVerifier, pair.CodeChallenge} {
		if strings.ContainsAny(value, "+/=") {
			t.Errorf("PKCE value %q uses a character that needs escaping in a URL", value)
		}
	}
}

func TestPKCEVerifierIsRandomPerCall(t *testing.T) {
	first, errFirst := newPKCE()
	if errFirst != nil {
		t.Fatalf("newPKCE: %v", errFirst)
	}
	second, errSecond := newPKCE()
	if errSecond != nil {
		t.Fatalf("newPKCE: %v", errSecond)
	}
	if first.CodeVerifier == second.CodeVerifier {
		t.Error("two PKCE verifiers are identical; the verifier must be random")
	}
}

func TestParseDeviceAuthorization(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantErr    bool
		wantInterv int
		wantURI    string
	}{
		{
			name: "standard response keeps the advertised interval",
			payload: `{"device_code":"dc","user_code":"98FJ-WYXZ",
				"verification_uri":"https://account.minimax.cn/oauth-authorize",
				"verification_uri_complete":"https://account.minimax.cn/oauth-authorize?user_code=98FJ-WYXZ",
				"expires_in":300,"interval":3}`,
			wantInterv: 3,
			wantURI:    "https://account.minimax.cn/oauth-authorize?user_code=98FJ-WYXZ",
		},
		{
			name: "interval is in SECONDS and defaults to 5 when absent",
			payload: `{"device_code":"dc","user_code":"uc",
				"verification_uri":"https://v","expires_in":300}`,
			wantInterv: 5,
			wantURI:    "https://v",
		},
		{
			name: "verification_uri_complete falls back to verification_uri",
			payload: `{"device_code":"dc","user_code":"uc","verification_uri":"https://v",
				"verification_uri_complete":"","expires_in":300,"interval":5}`,
			wantInterv: 5,
			wantURI:    "https://v",
		},
		{
			name: "verification_url is accepted as an alias",
			payload: `{"device_code":"dc","user_code":"uc","verification_url":"https://alias",
				"expires_in":300,"interval":5}`,
			wantInterv: 5,
			wantURI:    "https://alias",
		},
		{
			name:    "missing device_code is rejected",
			payload: `{"user_code":"uc","verification_uri":"https://v","expires_in":300}`,
			wantErr: true,
		},
		{
			name:    "missing user_code is rejected",
			payload: `{"device_code":"dc","verification_uri":"https://v","expires_in":300}`,
			wantErr: true,
		},
		{
			name:    "missing verification_uri is rejected",
			payload: `{"device_code":"dc","user_code":"uc","expires_in":300}`,
			wantErr: true,
		},
		{
			name:    "missing expires_in is rejected",
			payload: `{"device_code":"dc","user_code":"uc","verification_uri":"https://v"}`,
			wantErr: true,
		},
		{
			name:    "zero expires_in is rejected",
			payload: `{"device_code":"dc","user_code":"uc","verification_uri":"https://v","expires_in":0}`,
			wantErr: true,
		},
		{
			name:    "non-JSON is rejected",
			payload: `not json`,
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			auth, errParse := parseDeviceAuthorization([]byte(testCase.payload))
			if testCase.wantErr {
				if errParse == nil {
					t.Fatalf("parseDeviceAuthorization accepted %s", testCase.payload)
				}
				return
			}
			if errParse != nil {
				t.Fatalf("parseDeviceAuthorization: %v", errParse)
			}
			if auth.IntervalSeconds != testCase.wantInterv {
				t.Errorf("interval = %d, want %d", auth.IntervalSeconds, testCase.wantInterv)
			}
			if auth.VerificationURIComplete != testCase.wantURI {
				t.Errorf("verification_uri_complete = %q, want %q", auth.VerificationURIComplete, testCase.wantURI)
			}
			// ⚠️ The parser cannot know the verifier; the caller injects it.
			// Asserting it here would be asserting "" == "".
			if auth.CodeVerifier != "" {
				t.Errorf("parser invented a code_verifier (%q); the caller owns it", auth.CodeVerifier)
			}
		})
	}
}

// TestRequestDeviceAuthorizationInjectsTheVerifier guards the trap the
// reference calls out explicitly: the parsed grant carries an EMPTY verifier,
// so a caller that forwards the parser's value sends `code_verifier=""` on
// every poll and the server rejects the exchange.
func TestRequestDeviceAuthorizationInjectsTheVerifier(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"device_code":"dc","user_code":"uc",
			"verification_uri":"https://v","expires_in":300,"interval":5}`), nil
	}
	host.install(t)

	auth, errAuth := requestDeviceAuthorization(testHost(), DefaultConfig())
	if errAuth != nil {
		t.Fatalf("requestDeviceAuthorization: %v", errAuth)
	}
	if strings.TrimSpace(auth.CodeVerifier) == "" {
		t.Fatal("code_verifier is empty: the poll body would send code_verifier=\"\"")
	}
	if len(auth.CodeVerifier) != 43 {
		t.Errorf("code_verifier length = %d, want 43", len(auth.CodeVerifier))
	}
	// The challenge that WAS sent must match this verifier.
	sent := formValue(t, host.requests[0].Body, "code_challenge")
	sum := sha256.Sum256([]byte(auth.CodeVerifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); sent != want {
		t.Errorf("code_challenge sent = %q, want the digest of the stored verifier %q", sent, want)
	}
}

// TestRequestDeviceAuthorizationUsesOAuthConstants pins the transport facts.
func TestRequestDeviceAuthorizationUsesOAuthConstants(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"device_code":"dc","user_code":"uc",
			"verification_uri":"https://v","expires_in":300}`), nil
	}
	host.install(t)
	if _, errAuth := requestDeviceAuthorization(testHost(), DefaultConfig()); errAuth != nil {
		t.Fatalf("requestDeviceAuthorization: %v", errAuth)
	}
	request := host.requests[0]
	if request.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", request.Method)
	}
	if want := AccountHost + DeviceCodePath; request.URL != want {
		t.Errorf("url = %s, want %s", request.URL, want)
	}
	if contentType := request.Headers.Get("Content-Type"); contentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", contentType)
	}
}

// TestClassifyPollAcceptsBothPendingForms is the central test of this file.
//
// ⚠️ MiniMax signals "the user has not finished yet" as **HTTP 200 with
// `status:"pending"`**, NOT as the OAuth-standard `400 +
// error=authorization_pending`. An implementation that only knows the standard
// form errors out on the very first poll, so the user — who is still looking at
// the browser page — can never complete the login.
func TestClassifyPollAcceptsBothPendingForms(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		payload     string
		want        pollDecision
		wantMessage bool
	}{
		{
			name:    "form 1: HTTP 200 with status pending",
			status:  http.StatusOK,
			payload: `{"status":"pending"}`,
			want:    pollPending,
		},
		{
			name:    "form 2: HTTP 400 with error authorization_pending",
			status:  http.StatusBadRequest,
			payload: `{"error":"authorization_pending"}`,
			want:    pollPending,
		},
		{
			name:    "form 1: HTTP 200 with status slow_down",
			status:  http.StatusOK,
			payload: `{"status":"slow_down"}`,
			want:    pollSlowDown,
		},
		{
			name:    "form 2: HTTP 400 with error slow_down",
			status:  http.StatusBadRequest,
			payload: `{"error":"slow_down"}`,
			want:    pollSlowDown,
		},
		{
			name:    "form 1: HTTP 200 with status denied",
			status:  http.StatusOK,
			payload: `{"status":"denied"}`,
			want:    pollDenied,
		},
		{
			name:    "form 1: HTTP 200 with status access_denied",
			status:  http.StatusOK,
			payload: `{"status":"access_denied"}`,
			want:    pollDenied,
		},
		{
			name:    "form 2: HTTP 400 with error access_denied",
			status:  http.StatusBadRequest,
			payload: `{"error":"access_denied"}`,
			want:    pollDenied,
		},
		{
			name:    "form 1: HTTP 200 with status expired",
			status:  http.StatusOK,
			payload: `{"status":"expired"}`,
			want:    pollExpired,
		},
		{
			name:    "form 2: HTTP 400 with error expired_token",
			status:  http.StatusBadRequest,
			payload: `{"error":"expired_token"}`,
			want:    pollExpired,
		},
		{
			name:    "a plain 2xx grant succeeds",
			status:  http.StatusOK,
			payload: `{"access_token":"mmoat_x","refresh_token":"mmort_y","token_type":"Bearer","expires_in":3600,"scope":"agent.default"}`,
			want:    pollSuccess,
		},
		{
			name:    "an unknown 2xx status is refused rather than guessed",
			status:  http.StatusOK,
			payload: `{"status":"something_new"}`,
			want:    pollFailed,
		},
		{
			name:    "an unrecognised failure is reported",
			status:  http.StatusBadRequest,
			payload: `{"error":"invalid_request"}`,
			want:    pollFailed,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			decision, message := classifyPoll(testCase.status, []byte(testCase.payload))
			if decision != testCase.want {
				t.Fatalf("classifyPoll(%d, %s) = %v, want %v (message=%q)",
					testCase.status, testCase.payload, decision, testCase.want, message)
			}
			if testCase.want != pollSuccess && strings.TrimSpace(message) == "" {
				t.Error("a non-success decision carries no message for the user")
			}
		})
	}
}

// TestSlowDownGrowsTheIntervalByFiveSeconds checks the measured 5 s increment
// on a live session.
func TestSlowDownGrowsTheIntervalByFiveSeconds(t *testing.T) {
	session := &loginSession{IntervalSeconds: 5}
	session.slowDown()
	if got := session.currentIntervalSeconds(); got != 10 {
		t.Fatalf("interval after one slow_down = %d, want 10", got)
	}
	session.slowDown()
	if got := session.currentIntervalSeconds(); got != 15 {
		t.Fatalf("interval after two slow_downs = %d, want 15", got)
	}
}

func TestParseTokenGrantValidation(t *testing.T) {
	// ⚠️ The REAL token shape: `mmoat_`-prefixed, 60 characters, ZERO dots —
	// not a JWT. Building the fixture from a JWT would hide the fact that the
	// expiry must come from `expires_in`.
	realAccess := "mmoat_" + strings.Repeat("a", 54)
	now := time.UnixMilli(1_700_000_000_000)

	tests := []struct {
		name                 string
		payload              string
		previousRefreshToken string
		wantErr              bool
		wantExpirySeconds    int
		wantRefresh          string
		wantAccountID        string
	}{
		{
			name: "a complete grant is accepted",
			payload: `{"access_token":"` + realAccess + `","refresh_token":"` + strings.Repeat("r", 60) + `",
				"token_type":"Bearer","expires_in":3600,"scope":"agent.default"}`,
			wantExpirySeconds: 3600,
			wantRefresh:       strings.Repeat("r", 60),
		},
		{
			name: "token_type is matched case-insensitively",
			payload: `{"access_token":"a","refresh_token":"r","token_type":"bearer",
				"expires_in":60,"scope":"agent.default"}`,
			wantExpirySeconds: 60,
			wantRefresh:       "r",
		},
		{
			name: "a scope LIST containing agent.default is accepted",
			payload: `{"access_token":"a","refresh_token":"r","token_type":"Bearer",
				"expires_in":60,"scope":"openid agent.default profile"}`,
			wantExpirySeconds: 60,
			wantRefresh:       "r",
		},
		{
			name: "the previous refresh token is carried forward when the grant omits one",
			payload: `{"access_token":"a","token_type":"Bearer",
				"expires_in":60,"scope":"agent.default"}`,
			previousRefreshToken: "previous-token",
			wantExpirySeconds:    60,
			wantRefresh:          "previous-token",
		},
		{
			name:    "missing access_token is rejected",
			payload: `{"refresh_token":"r","token_type":"Bearer","expires_in":60,"scope":"agent.default"}`,
			wantErr: true,
		},
		{
			name:    "missing refresh_token with no previous value is rejected",
			payload: `{"access_token":"a","token_type":"Bearer","expires_in":60,"scope":"agent.default"}`,
			wantErr: true,
		},
		{
			name:    "a non-bearer token_type is rejected",
			payload: `{"access_token":"a","refresh_token":"r","token_type":"mac","expires_in":60,"scope":"agent.default"}`,
			wantErr: true,
		},
		{
			name:    "a missing token_type is rejected",
			payload: `{"access_token":"a","refresh_token":"r","expires_in":60,"scope":"agent.default"}`,
			wantErr: true,
		},
		{
			name:    "a missing expires_in is rejected",
			payload: `{"access_token":"a","refresh_token":"r","token_type":"Bearer","scope":"agent.default"}`,
			wantErr: true,
		},
		{
			name:    "a zero expires_in is rejected",
			payload: `{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":0,"scope":"agent.default"}`,
			wantErr: true,
		},
		{
			// ⚠️ THE load-bearing rule: the vendor's authoritative client
			// requires `agent.default`, and a grant without it is not usable.
			name:    "a scope without agent.default is rejected",
			payload: `{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":60,"scope":"openid profile"}`,
			wantErr: true,
		},
		{
			name:    "a missing scope is rejected",
			payload: `{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":60}`,
			wantErr: true,
		},
		{
			name:    "a scope that merely mentions the string is rejected",
			payload: `{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":60,"scope":"agent.default.read"}`,
			wantErr: true,
		},
		{
			name: "a JWT-shaped token yields an account id and a JWT expiry",
			payload: `{"access_token":"` + makeTestJWT(t, map[string]any{
				"sub": "acct-1", "exp": 4_000_000_000,
			}) + `","refresh_token":"r","token_type":"Bearer","expires_in":60,"scope":"agent.default"}`,
			wantAccountID: "acct-1",
			// The JWT exp wins over expires_in only through Credential.Expiry,
			// never through the stored value; see the assertion below.
			wantExpirySeconds: 60,
			wantRefresh:       "r",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			credential, errParse := parseTokenGrant([]byte(testCase.payload), testCase.previousRefreshToken, now)
			if testCase.wantErr {
				if errParse == nil {
					t.Fatalf("parseTokenGrant accepted %s", testCase.payload)
				}
				return
			}
			if errParse != nil {
				t.Fatalf("parseTokenGrant: %v", errParse)
			}
			if credential.RefreshToken != testCase.wantRefresh {
				t.Errorf("refresh_token = %q, want %q", credential.RefreshToken, testCase.wantRefresh)
			}
			if credential.TokenType != "Bearer" {
				t.Errorf("token_type = %q, want Bearer", credential.TokenType)
			}
			if credential.AccountID != testCase.wantAccountID {
				t.Errorf("account_id = %q, want %q", credential.AccountID, testCase.wantAccountID)
			}
			wantExpiry := now.Add(time.Duration(testCase.wantExpirySeconds) * time.Second).UnixMilli()
			if got := credential.ExpiresAtMS(); got != wantExpiry {
				t.Errorf("expires_at = %d, want %d (computed from expires_in, now+%ds)",
					got, wantExpiry, testCase.wantExpirySeconds)
			}
		})
	}
}

// TestTokenGrantExpiryComesFromExpiresInNotTheToken is the measured fact that
// forced the whole expiry design: a real access_token is NOT a JWT, so there is
// nothing to decode and `expires_in` is the only source.
func TestTokenGrantExpiryComesFromExpiresInNotTheToken(t *testing.T) {
	realAccess := "mmoat_" + strings.Repeat("a", 54)
	// Guard the fixture itself: if this ever becomes a JWT the test stops
	// proving anything.
	if strings.Contains(realAccess, ".") {
		t.Fatal("the fixture token contains a dot; it is supposed to model a NON-JWT token")
	}
	before := time.Now()
	credential, errParse := parseTokenGrant([]byte(`{"access_token":"`+realAccess+`",
		"refresh_token":"r","token_type":"Bearer","expires_in":3600,"scope":"agent.default"}`), "", before)
	if errParse != nil {
		t.Fatalf("parseTokenGrant: %v", errParse)
	}
	got := credential.ExpiresAtMS()
	if got < before.Add(3600*time.Second).UnixMilli() || got > time.Now().Add(3600*time.Second).UnixMilli() {
		t.Fatalf("expires_at = %d, want it inside [now+3600s, now+3600s]", got)
	}
	// And the JWT fallback must NOT invent anything for it.
	if expiry := jwtExpiryMS(realAccess); expiry != 0 {
		t.Errorf("jwtExpiryMS decoded %d out of a non-JWT token", expiry)
	}
	if subject := jwtSubject(realAccess); subject != "" {
		t.Errorf("jwtSubject decoded %q out of a non-JWT token", subject)
	}
}

// TestScopeContainsIsExact pins the membership rule: a prefix is not a match.
func TestScopeContainsIsExact(t *testing.T) {
	tests := []struct {
		scope string
		want  bool
	}{
		{"agent.default", true},
		{"openid agent.default profile", true},
		{"  agent.default  ", true},
		{"agent.default.read", false},
		{"agent.defaults", false},
		{"", false},
		{"agent", false},
	}
	for _, testCase := range tests {
		if got := scopeContains(testCase.scope, Scope); got != testCase.want {
			t.Errorf("scopeContains(%q, %q) = %v, want %v", testCase.scope, Scope, got, testCase.want)
		}
	}
}

// TestTerminalRefreshErrors pins which refresh failures end a credential's life
// rather than being retried forever.
func TestTerminalRefreshErrors(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{"invalid_grant is terminal", `{"error":"invalid_grant"}`, true},
		{"expired_token is terminal", `{"error":"expired_token"}`, true},
		{"invalid_refresh_token is terminal", `{"error":"invalid_refresh_token"}`, true},
		{"case is ignored", `{"error":"INVALID_GRANT"}`, true},
		{"a transport-shaped answer is not terminal", `{"error":"server_error"}`, false},
		{"a non-JSON answer is not terminal", `oops`, false},
		{"an empty answer is not terminal", `{}`, false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isTerminalRefreshError([]byte(testCase.payload)); got != testCase.want {
				t.Errorf("isTerminalRefreshError(%s) = %v, want %v", testCase.payload, got, testCase.want)
			}
		})
	}
}

// TestRefreshCredentialClassifiesADeadGrantAsTerminal checks that the OAuth
// error code — not just the HTTP status — decides terminality. The vendor
// answers `400 invalid_grant` for a dead grant, and treating that as retryable
// would make the scheduler retry it forever.
func TestRefreshCredentialClassifiesADeadGrantAsTerminal(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusBadRequest, `{"error":"invalid_grant"}`), nil
	}
	host.install(t)

	_, errRefresh := refreshCredential(testHost(), DefaultConfig(), &Credential{
		AccessToken:  "mmoat_x",
		RefreshToken: "mmort_y",
	})
	if errRefresh == nil {
		t.Fatal("refreshCredential accepted a dead grant")
	}
	if status := statusOf(errRefresh, 0); status != http.StatusUnauthorized {
		t.Errorf("dead grant classified as HTTP %d, want 401 so the host can retire the credential", status)
	}
}

// TestRefreshCredentialCarriesTheRefreshTokenForward covers a grant that
// rotates only the access token: dropping renewal would silently kill the
// credential on its next cycle.
func TestRefreshCredentialCarriesTheRefreshTokenForward(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"access_token":"mmoat_new","token_type":"Bearer",
			"expires_in":3600,"scope":"agent.default"}`), nil
	}
	host.install(t)

	previous := &Credential{AccessToken: "mmoat_old", RefreshToken: "mmort_keep", AccountID: "acct-9", Nickname: "工作号"}
	refreshed, errRefresh := refreshCredential(testHost(), DefaultConfig(), previous)
	if errRefresh != nil {
		t.Fatalf("refreshCredential: %v", errRefresh)
	}
	if refreshed.RefreshToken != "mmort_keep" {
		t.Errorf("refresh_token = %q, want the previous value carried forward", refreshed.RefreshToken)
	}
	if refreshed.AccountID != "acct-9" {
		t.Errorf("account_id = %q, want the previous value carried forward", refreshed.AccountID)
	}
	if refreshed.Nickname != "工作号" {
		t.Errorf("nickname = %q, want the previous value carried forward", refreshed.Nickname)
	}
	// The refresh grant is a POST form to the token endpoint.
	if sent := formValue(t, host.requests[0].Body, "grant_type"); sent != "refresh_token" {
		t.Errorf("grant_type sent = %q, want refresh_token", sent)
	}
}

// TestRefreshCredentialSendsTheRefreshForm checks the whole body, not just the
// grant type.
func TestRefreshCredentialSendsTheRefreshForm(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"access_token":"mmoat_new","refresh_token":"mmort_new",
			"token_type":"Bearer","expires_in":3600,"scope":"agent.default"}`), nil
	}
	host.install(t)
	if _, errRefresh := refreshCredential(testHost(), DefaultConfig(), &Credential{
		AccessToken: "mmoat_old", RefreshToken: "mmort_old",
	}); errRefresh != nil {
		t.Fatalf("refreshCredential: %v", errRefresh)
	}
	form := parseForm(t, host.requests[0].Body)
	want := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": "mmort_old",
		"client_id":     "mcode-public",
		"scope":         "agent.default",
		"audience":      "agent-backend",
	}
	for key, expected := range want {
		if got := form.Get(key); got != expected {
			t.Errorf("refresh form[%s] = %q, want %q", key, got, expected)
		}
	}
	if host.requests[0].URL != AccountHost+TokenPath {
		t.Errorf("refresh url = %s, want %s", host.requests[0].URL, AccountHost+TokenPath)
	}
}

// TestRefreshCredentialRefusesWithoutARefreshToken keeps a credential with no
// renewal path from costing an upstream round trip.
func TestRefreshCredentialRefusesWithoutARefreshToken(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatal("a credential without a refresh token must not reach the network")
		return nil, nil
	}
	host.install(t)
	_, errRefresh := refreshCredential(testHost(), DefaultConfig(), &Credential{AccessToken: "mmoat_x"})
	if errRefresh == nil {
		t.Fatal("refreshCredential accepted a credential with no refresh token")
	}
	if status := statusOf(errRefresh, 0); status != http.StatusUnauthorized {
		t.Errorf("HTTP status = %d, want 401", status)
	}
}

// makeTestJWT builds a three-segment token with the given claims. It exists
// ONLY for the compatibility branch; real MiniMax tokens are not JWTs.
func makeTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		raw, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			t.Fatalf("marshal JWT segment: %v", errMarshal)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]any{"alg": "RS256"}) + "." + encode(claims) + ".sig"
}
