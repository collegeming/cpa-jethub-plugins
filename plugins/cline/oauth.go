package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file is the Go port of `src/cline-oauth.ts`: the WorkOS device-code
// login, the WorkOS→Cline token exchange and the credential refresh.
//
// Structural difference from the TypeScript, forced by the CPA ABI: the
// TypeScript runs one blocking loop (`startClineLoginFlow` returns the URL
// immediately, the result settles later, `cline-oauth.ts:48-57`), while CPA
// splits the flow into `auth.login.start` (returns the URL at once) and
// `auth.login.poll` (advances the flow one attempt per invocation). The plugin
// therefore never sleeps inside a host call; the poll interval is enforced
// against the previous attempt instead. Every rule of the state machine is kept.
//
// Nothing here opens a socket: every request goes through the host transport
// (`host.http.do`), which owns proxy, TLS and request logging.

// authServiceErrorPrefix is the constant at `cline-oauth.ts:162`. Errors the
// module raises itself carry this prefix, which is also what tells them apart
// from transport failures: only transport failures consume the retry budget.
const authServiceErrorPrefix = "登录服务返回异常"

// doer performs one buffered upstream call. The production implementation is
// hostDo; the device-code state machine and the refresh classifier take it as a
// parameter so they can be exercised without a socket.
type doer func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error)

// hostDo routes a request through the host transport.
func hostDo(h *abiboot.Host) doer {
	return func(method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
		if h == nil {
			return nil, abiboot.Errorf("host_unavailable", "插件未通过宿主调用（缺少 host 句柄）")
		}
		return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: rawURL, Headers: headers, Body: body})
	}
}

// deviceAuthorization is the `/user_management/authorize/device` response
// (`cline-oauth.ts:189-211`).
type deviceAuthorization struct {
	// DeviceCode is the opaque code the token poll posts back.
	DeviceCode string
	// UserCode is the short code shown to the user.
	UserCode string
	// VerificationURI is the fallback browser URL.
	VerificationURI string
	// VerificationURIComplete is the preferred URL: it already carries the user
	// code, so the user does not have to type anything.
	VerificationURIComplete string
	// ExpiresInMS is the device-code lifetime. The server value wins over the
	// configured default.
	ExpiresInMS int
	// IntervalMS is the server-negotiated poll interval, floored at 1 s.
	IntervalMS int
}

// loginURL is `verification_uri_complete ?? verification_uri`
// (`cline-oauth.ts:399`).
func (a *deviceAuthorization) loginURL() string {
	if strings.TrimSpace(a.VerificationURIComplete) != "" {
		return strings.TrimSpace(a.VerificationURIComplete)
	}
	return strings.TrimSpace(a.VerificationURI)
}

// urlEncodedHeaders is the header set of the two WorkOS calls: a form content
// type and nothing else — no Authorization, no client headers
// (`cline-oauth.ts:185`, `:245`).
func urlEncodedHeaders() http.Header {
	header := http.Header{}
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	return header
}

// decodeObject decodes a JSON object, tolerating anything else. A gateway 502
// answers HTML, and `cline-oauth.ts:253` records that this must be handled as an
// empty object rather than as a parse failure.
func decodeObject(body []byte) map[string]any {
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		return map[string]any{}
	}
	return decoded
}

// errorFieldDetail renders the `[ - <error_description>]` suffix several
// messages share. An absent or non-string field renders as nothing at all.
func errorFieldDetail(payload map[string]any) string {
	description := readStringField(payload, "error_description", "error", "message")
	if description == "" {
		return ""
	}
	return " - " + description
}

// secondsToMillis converts a JSON seconds field to milliseconds.
//
// Port of the inline checks at `cline-oauth.ts:165-168`: a non-number, a
// non-finite value and a value ≤ 0 all fall back to the default. Fractional
// seconds are floored, exactly like `Math.floor(value) * 1000`.
func secondsToMillis(value any, fallback int) int {
	seconds, okNumber := value.(float64)
	if !okNumber || seconds <= 0 || seconds != seconds /* NaN */ {
		return fallback
	}
	if seconds > float64(^uint(0)>>1)/1000 {
		return fallback
	}
	return int(seconds) * 1000
}

// parseDeviceAuthorization implements §2.1 of the porting specification.
//
// A non-2xx answer is a service failure; a 2xx answer missing any of
// device_code / user_code / verification_uri is a shape failure. Both are
// reported as terminal 502s: retrying the same request cannot help.
func parseDeviceAuthorization(status int, body []byte, cfg Config) (*deviceAuthorization, error) {
	payload := decodeObject(body)
	if status < 200 || status >= 300 {
		return nil, statusError(false, "device_authorization", http.StatusBadGateway,
			"%s：设备码授权失败（HTTP %d）%s", authServiceErrorPrefix, status, errorFieldDetail(payload))
	}
	deviceCode := readStringField(payload, "device_code")
	userCode := readStringField(payload, "user_code")
	verificationURI := readStringField(payload, "verification_uri")
	if deviceCode == "" || userCode == "" || verificationURI == "" {
		return nil, statusError(false, "device_authorization_shape", http.StatusBadGateway,
			"%s：设备码授权响应缺少必要字段", authServiceErrorPrefix)
	}
	interval := secondsToMillis(payload["interval"], cfg.PollIntervalMS)
	if interval < PollIntervalFloorMS {
		interval = PollIntervalFloorMS
	}
	return &deviceAuthorization{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: readStringField(payload, "verification_uri_complete"),
		ExpiresInMS:             secondsToMillis(payload["expires_in"], cfg.LoginTimeoutMS),
		IntervalMS:              interval,
	}, nil
}

// requestDeviceAuthorization is step 1 (`cline-oauth.ts:178-188`): a form POST
// carrying only `client_id`.
func requestDeviceAuthorization(d doer, cfg Config) (*deviceAuthorization, error) {
	form := url.Values{}
	form.Set("client_id", WorkOSClientID)
	response, errDo := d(http.MethodPost, WorkOSBase+DeviceAuthorizationPath, urlEncodedHeaders(), []byte(form.Encode()))
	if errDo != nil {
		return nil, transportError("device_authorization_transport", "无法连接 Cline 登录服务：%v", errDo)
	}
	return parseDeviceAuthorization(response.StatusCode, response.Body, cfg)
}

// devicePollStatus is the outcome of one authenticate call
// (`cline-oauth.ts:266-291`).
type devicePollStatus string

const (
	// devicePollPending means the user has not approved yet. WorkOS answers this
	// with HTTP 400, which is why the body decides and not the status.
	devicePollPending devicePollStatus = "pending"
	// devicePollSlowDown asks for a larger interval. The increase is cumulative
	// and never reset.
	devicePollSlowDown devicePollStatus = "slow_down"
	// devicePollSuccess carries usable tokens.
	devicePollSuccess devicePollStatus = "success"
	// devicePollFailed is terminal: the flow must be restarted.
	devicePollFailed devicePollStatus = "failed"
)

// deviceTokenRequest is the step-2 form body (`cline-oauth.ts:243-252`).
//
// `URLSearchParams` escaping is load-bearing and reproduced by url.Values: the
// grant type is sent as
// `grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code`
// (asserted at `tests/unit/cline-oauth.spec.ts:236`).
func deviceTokenRequest(deviceCode string) []byte {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", deviceCode)
	form.Set("client_id", WorkOSClientID)
	return []byte(form.Encode())
}

// classifyDeviceToken implements the failure state machine of
// `cline-oauth.ts:256-291`.
//
// The decision comes from the body's `error` field, never from the HTTP status:
// WorkOS returns 400 for `authorization_pending`, and a gateway 502 answers HTML
// that decodes to an empty object. Checking the body first is a strict superset
// of the TypeScript's `response.ok` check (spec risk 10) and also survives a
// server that one day answers `authorization_pending` with 2xx.
func classifyDeviceToken(status int, body []byte) (clineTokens, devicePollStatus, string) {
	payload := decodeObject(body)
	switch readStringField(payload, "error") {
	case "authorization_pending":
		return clineTokens{}, devicePollPending, "等待用户在浏览器中完成授权"
	case "slow_down":
		return clineTokens{}, devicePollSlowDown, "登录服务要求降低轮询频率"
	case "access_denied", "expired_token", "invalid_grant":
		message := readStringField(payload, "error_description")
		if message == "" {
			message = "WorkOS 授权失败"
		}
		return clineTokens{}, devicePollFailed, authServiceErrorPrefix + "：" + message
	}
	if status >= 200 && status < 300 {
		tokens := parseTokenEnvelope(body)
		if tokens.AccessToken == "" || tokens.RefreshToken == "" {
			// Deterministic failure: both tokens are required and there is no
			// retry loop for it (`cline-oauth.ts:256-263`).
			return clineTokens{}, devicePollFailed, authServiceErrorPrefix + "：WorkOS token 响应缺少必要字段"
		}
		return tokens, devicePollSuccess, ""
	}
	message := fmt.Sprintf("%s：WorkOS token 轮询失败（HTTP %d）", authServiceErrorPrefix, status)
	return clineTokens{}, devicePollFailed, message + errorFieldDetail(payload)
}

// pollDeviceToken is step 2: one authenticate call. A non-nil error is a
// TRANSPORT failure (the caller counts it against the retry budget); every HTTP
// answer, including an error answer, comes back through the classification.
func pollDeviceToken(d doer, deviceCode string) (clineTokens, devicePollStatus, string, error) {
	response, errDo := d(http.MethodPost, WorkOSBase+DeviceAuthenticatePath, urlEncodedHeaders(), deviceTokenRequest(deviceCode))
	if errDo != nil {
		return clineTokens{}, devicePollFailed, "", errDo
	}
	tokens, outcome, message := classifyDeviceToken(response.StatusCode, response.Body)
	return tokens, outcome, message, nil
}

// registerHeaders is the step-3 header set (`cline-oauth.ts:320-331`): JSON
// content type plus the four client headers. There is deliberately NO
// Authorization header and no Accept header.
func registerHeaders() http.Header {
	header := clientHeaders()
	header.Set("Content-Type", "application/json")
	return header
}

// exchangeWorkOSTokens is step 3 (`cline-oauth.ts:320-336`): the WorkOS tokens
// are traded for Cline tokens. The body fields are camelCase
// (`cline-oauth.ts:312-313`).
func exchangeWorkOSTokens(d doer, tokens clineTokens) (*Credential, error) {
	body, errMarshal := json.Marshal(map[string]string{
		"accessToken":  tokens.AccessToken,
		"refreshToken": tokens.RefreshToken,
	})
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_register", "encode Cline register body: %v", errMarshal)
	}
	response, errDo := d(http.MethodPost, APIBase+RegisterPath, registerHeaders(), body)
	if errDo != nil {
		return nil, transportError("register_transport", "无法连接 Cline 登录服务：%v", errDo)
	}
	payload := decodeObject(response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, statusError(false, "register_failed", http.StatusBadGateway,
			"%s：token 注册失败（HTTP %d）%s", authServiceErrorPrefix, response.StatusCode, errorFieldDetail(payload))
	}
	parsed := parseTokenEnvelope(response.Body)
	if parsed.AccessToken == "" {
		// `登录响应缺少访问令牌` (`cline-oauth.ts:405-407`). The device code is
		// spent by now, so the flow has to be restarted.
		return nil, statusError(false, "register_shape", http.StatusBadGateway,
			"%s：登录响应缺少访问令牌", authServiceErrorPrefix)
	}
	// The register response's refreshToken is the CLINE refresh token and is
	// stored as-is. The WorkOS refresh token is deliberately NOT substituted
	// when the field is absent: they are different token types, and a wrong
	// token here would look like a valid credential that can never renew.
	return buildCredential(parsed), nil
}

// clineRefreshBody is `clineRefreshBody` (`cline.ts:263-268`).
//
// ⚠️ The field names are camelCase `refreshToken` + `grantType` — NOT OAuth's
// `refresh_token`/`grant_type`. Both are mandatory, and a wrong name yields a
// generic auth failure rather than a "missing field" error
// (`cline.ts:251-262`).
func clineRefreshBody(refreshToken string) ([]byte, error) {
	body, errMarshal := json.Marshal(map[string]string{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_refresh", "encode Cline refresh body: %v", errMarshal)
	}
	return body, nil
}

// refreshHeaders is the refresh header set (`cline-auth.ts:355-365`): JSON
// content type + JSON accept + the four client headers, and NO Authorization.
func refreshHeaders() http.Header {
	header := clientHeaders()
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json")
	return header
}

// refreshClass is the refresh failure taxonomy of `cline-auth.ts:352-396`.
type refreshClass int

const (
	// refreshOK means a usable access token came back.
	refreshOK refreshClass = iota
	// refreshRetryable means the caller should try again later.
	refreshRetryable
	// refreshTerminal means the refresh token is dead and only a new login can
	// fix the account. The TypeScript signals this with a structural
	// `RefreshTokenExpiredError` (name check at `refresh.ts:21-25`); in CPA it is
	// a 401 the host uses to park the credential.
	refreshTerminal
)

// classifyRefreshResponse is the port of the refresh failure classification
// (`cline-auth.ts:372-394`).
//
// Terminal cases: HTTP 401/403, and a 2xx whose body carries no access token.
// Retryable cases: a 2xx body that is not JSON at all, and any other non-2xx
// (5xx, 429, …) — those are reported with at most 200 characters of the body.
func classifyRefreshResponse(status int, body []byte) (clineTokens, refreshClass, string) {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return clineTokens{}, refreshTerminal, "Cline refresh_token 已失效，请重新登录"
	}
	if status < 200 || status >= 300 {
		return clineTokens{}, refreshRetryable,
			fmt.Sprintf("Cline 续期返回 HTTP %d：%s", status, truncate(string(body), 200))
	}
	if !json.Valid(bytes.TrimSpace(body)) {
		// `response.json()` throws here; the TypeScript classifies that as
		// retryable (`cline-auth.ts:376-381`).
		return clineTokens{}, refreshRetryable, "Cline 续期响应不是 JSON：" + truncate(string(body), 200)
	}
	tokens := parseTokenEnvelope(body)
	if tokens.AccessToken == "" {
		// A 2xx without a token is TERMINAL, not retryable
		// (`cline-auth.ts:389-394`): retrying a partial body would loop forever.
		return clineTokens{}, refreshTerminal, "续期响应缺少访问令牌，请重新登录"
	}
	return tokens, refreshOK, ""
}

// refreshCredential performs one `/api/v1/auth/refresh` call and merges the
// answer into the credential.
func refreshCredential(d doer, credential *Credential) (*Credential, error) {
	if !credential.Refreshable() {
		return nil, credentialError("refresh_not_refreshable", "Cline 凭据没有 refresh_token，请重新登录")
	}
	body, errBody := clineRefreshBody(credential.RefreshToken)
	if errBody != nil {
		return nil, errBody
	}
	response, errDo := d(http.MethodPost, APIBase+RefreshPath, refreshHeaders(), body)
	if errDo != nil {
		// A transport failure is retryable (`cline-auth.ts:366-370`).
		return nil, transportError("refresh_transport", "Cline 续期网络失败：%v", errDo)
	}
	tokens, class, message := classifyRefreshResponse(response.StatusCode, response.Body)
	switch class {
	case refreshTerminal:
		return nil, credentialError("refresh_token_expired", "%s", message)
	case refreshRetryable:
		return nil, transportError("refresh_failed", "%s", message)
	}
	return credential.applyRefresh(tokens), nil
}

// ---------------------------------------------------------------------------
// login sessions
// ---------------------------------------------------------------------------

// loginSession tracks one in-flight device-code sign-in.
//
// The TypeScript keeps the same state in a closure plus a local
// `intervalMs`/`failures` pair (`cline-oauth.ts:233-307`); it lives here so a
// host invocation can advance the flow without blocking and without losing the
// accumulated `slow_down` penalty.
type loginSession struct {
	mu sync.Mutex

	// State is the opaque value the host echoes back on every poll.
	State string
	// Device is the device authorization this session polls.
	Device *deviceAuthorization
	// CreatedAt / ExpiresAt bound the whole wait.
	CreatedAt time.Time
	ExpiresAt time.Time
	// IntervalMS is the CURRENT poll interval. `slow_down` adds 1 s to it and
	// nothing ever resets it (`cline-oauth.ts:273-278`).
	IntervalMS int
	// Failures counts CONSECUTIVE transport failures. Any HTTP response resets
	// it to zero (`cline-oauth.ts:254`, `:296`).
	Failures int
	// LastAttempt is when the last upstream poll was issued; the interval is
	// enforced against it instead of sleeping inside a host call.
	LastAttempt time.Time
	// Status / Message / Done describe a terminal outcome.
	Status  pluginapi.AuthLoginStatus
	Message string
	Done    bool
	// credential is the account produced by a successful login.
	credential *Credential
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// startLoginSession performs the device authorization and registers a session.
//
// Unlike the other device-code providers in this repository there is no local
// device state to mint: WorkOS issues both codes, so this step is a network call
// and it is the one place `auth.login.start` can fail.
func startLoginSession(d doer, cfg Config) (*loginSession, error) {
	authorization, errAuthorize := requestDeviceAuthorization(d, cfg)
	if errAuthorize != nil {
		return nil, errAuthorize
	}
	state, errState := randomHex(16)
	if errState != nil {
		return nil, errState
	}
	timeout := authorization.ExpiresInMS
	if timeout <= 0 {
		timeout = cfg.LoginTimeoutMS
	}
	if timeout <= 0 {
		// A configuration that zeroes the fallback must not produce a session
		// that is born expired.
		timeout = DeviceCodeTTLMS
	}
	now := time.Now()
	session := &loginSession{
		State:      state,
		Device:     authorization,
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Duration(timeout) * time.Millisecond),
		IntervalMS: authorization.IntervalMS,
		Status:     pluginapi.AuthLoginStatusPending,
	}

	loginMu.Lock()
	purgeExpiredLoginSessionsLocked()
	loginSessions[state] = session
	loginMu.Unlock()
	return session, nil
}

// LoginURL is the browser URL the user must open.
func (s *loginSession) LoginURL() string { return s.Device.loginURL() }

// expired reports whether the session outlived its budget.
func (s *loginSession) expired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().After(s.ExpiresAt)
}

// fail records a terminal failure.
func (s *loginSession) fail(message string) {
	s.mu.Lock()
	s.Done = true
	s.Status = pluginapi.AuthLoginStatusError
	s.Message = message
	s.mu.Unlock()
}

// finish marks the session terminal with the outcome already recorded.
func (s *loginSession) finish() {
	s.mu.Lock()
	s.Done = true
	s.mu.Unlock()
}

// setCredential records a successful login.
func (s *loginSession) setCredential(credential *Credential, message string) {
	s.mu.Lock()
	s.credential = credential
	s.Status = pluginapi.AuthLoginStatusSuccess
	s.Message = message
	s.mu.Unlock()
}

// storedCredential returns the account produced by a successful login.
func (s *loginSession) storedCredential() *Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credential
}

// snapshot reads the current outcome.
func (s *loginSession) snapshot() (pluginapi.AuthLoginStatus, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status, s.Message, s.Done
}

// due reports whether the poll interval has elapsed since the last attempt. A
// fresh session has no attempt recorded, so the FIRST poll is immediate — there
// is no pre-sleep (`cline-oauth.ts:268-304`, asserted at
// `tests/unit/cline-oauth.spec.ts:228-239`).
func (s *loginSession) due() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	interval := time.Duration(s.IntervalMS) * time.Millisecond
	if interval < PollIntervalFloorMS*time.Millisecond {
		interval = PollIntervalFloorMS * time.Millisecond
	}
	return time.Since(s.LastAttempt) >= interval
}

// markAttempt records that an upstream poll is being issued now.
func (s *loginSession) markAttempt() {
	s.mu.Lock()
	s.LastAttempt = time.Now()
	s.mu.Unlock()
}

// noteFailure counts a consecutive transport failure.
func (s *loginSession) noteFailure() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Failures++
	return s.Failures
}

// resetFailures clears the consecutive-failure counter after any HTTP answer.
func (s *loginSession) resetFailures() {
	s.mu.Lock()
	s.Failures = 0
	s.mu.Unlock()
}

// slowDown applies the cumulative +1 s penalty and reports the new interval.
func (s *loginSession) slowDown() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.IntervalMS += 1000
	return s.IntervalMS
}

// lookupLoginSession finds a session by its state value. An expired session is
// dropped and reported as missing: the flow has to be restarted.
func lookupLoginSession(state string) (*loginSession, bool) {
	loginMu.Lock()
	defer loginMu.Unlock()
	session, okSession := loginSessions[state]
	if !okSession {
		return nil, false
	}
	if session.expired() && !session.snapshotDone() {
		delete(loginSessions, state)
		session.fail("登录等待已超时，请重新发起登录")
		return nil, false
	}
	return session, true
}

// snapshotDone reports whether the session is already terminal.
func (s *loginSession) snapshotDone() bool {
	_, _, done := s.snapshot()
	return done
}

// forgetLoginSession drops a completed session.
func forgetLoginSession(state string) {
	loginMu.Lock()
	delete(loginSessions, state)
	loginMu.Unlock()
}

// purgeExpiredLoginSessionsLocked drops expired entries. Callers hold loginMu.
func purgeExpiredLoginSessionsLocked() {
	for state, session := range loginSessions {
		if session.expired() {
			delete(loginSessions, state)
			session.fail("登录等待已超时，请重新发起登录")
		}
	}
}

// shutdownLoginSessions clears every session during plugin shutdown. There is no
// listener to close: the device-code flow owns no socket.
func shutdownLoginSessions() {
	loginMu.Lock()
	loginSessions = map[string]*loginSession{}
	loginMu.Unlock()
}

// randomHex returns n random bytes as a hex string.
func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", abiboot.Errorf("random_failed", "generate random bytes: %v", errRead)
	}
	return hex.EncodeToString(raw), nil
}

// advanceLoginSession performs at most one upstream poll and reports the
// host-facing status.
//
// The loop of `cline-oauth.ts:233-307` is unrolled into separate invocations:
// the deadline check, the immediate first attempt, the interval guard, the
// cumulative `slow_down` penalty and the reset-on-any-response rule are all
// preserved, but nothing sleeps inside a host call. `auth.login.poll` and the
// management login page share this function so both behave identically.
func advanceLoginSession(d doer, session *loginSession, state string, cfg Config) (pluginapi.AuthLoginPollResponse, error) {
	var empty pluginapi.AuthLoginPollResponse

	// A terminal session keeps reporting its outcome until it is forgotten.
	if status, message, done := session.snapshot(); done {
		response := pluginapi.AuthLoginPollResponse{Status: status, Message: message}
		if status == pluginapi.AuthLoginStatusSuccess {
			if credential := session.storedCredential(); credential != nil {
				auth, errAuth := authDataFor(credential, "")
				if errAuth != nil {
					return empty, errAuth
				}
				response.Auth = auth
			}
		}
		if response.Status == "" {
			response.Status = pluginapi.AuthLoginStatusError
		}
		return response, nil
	}

	// `Date.now() <= deadline` (`cline-oauth.ts:240`): the wait is over.
	if session.expired() {
		message := "登录等待已超时，请重新发起登录"
		session.fail(message)
		forgetLoginSession(state)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
	}

	if !session.due() {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户完成授权",
		}, nil
	}
	session.markAttempt()

	tokens, outcome, message, errPoll := pollDeviceToken(d, session.Device.DeviceCode)
	if errPoll != nil {
		limit := cfg.PollMaxFailures
		if limit <= 0 {
			limit = PollMaxFailures
		}
		if failures := session.noteFailure(); failures >= limit {
			// `无法连接 Cline 登录服务（连续 5 次失败）：<msg>`
			// (`cline-oauth.ts:297-302`).
			terminal := "无法连接 Cline 登录服务（连续 " + itoaInt(failures) + " 次失败）：" + errPoll.Error()
			session.fail(terminal)
			forgetLoginSession(state)
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: terminal}, nil
		}
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "轮询失败，稍后重试：" + errPoll.Error(),
		}, nil
	}
	// Any HTTP response resets the transport-failure counter
	// (`cline-oauth.ts:254`).
	session.resetFailures()

	switch outcome {
	case devicePollPending:
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: message}, nil

	case devicePollSlowDown:
		interval := session.slowDown()
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "登录服务要求降低轮询频率，间隔已调整为 " + itoaInt(interval/1000) + " 秒",
		}, nil

	case devicePollFailed:
		session.fail(message)
		forgetLoginSession(state)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
	}

	// Success: step 3 trades the WorkOS tokens for Cline tokens. A failure here
	// is terminal — the device code has been consumed.
	credential, errExchange := exchangeWorkOSTokens(d, tokens)
	if errExchange != nil {
		session.fail(errExchange.Error())
		forgetLoginSession(state)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errExchange.Error()}, nil
	}
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		return empty, errAuth
	}
	session.setCredential(credential, "登录成功")
	session.finish()
	forgetLoginSession(state)
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth:    auth,
	}, nil
}
