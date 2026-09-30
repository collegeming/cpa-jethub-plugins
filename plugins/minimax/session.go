package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The in-flight device-code login table.
//
// The flow is two-step by construction: `auth.login.start` requests a device
// code and RETURNS IMMEDIATELY with the authorisation link, because a browser
// popup opened after a blocking call loses the transient-activation window (and
// a front-end fallback then navigates the whole settings page away).
// `auth.login.poll` advances the flow one upstream attempt at a time.
//
// ⚠️ The poll interval is enforced against the PREVIOUS attempt rather than by
// sleeping inside a call: a host invocation must return promptly, and a handler
// that slept 5 seconds would hold a host goroutine for the length of an
// authorisation a user may never finish.
//
// ⚠️ VERIFICATION STATUS: this flow has not been exercised against the live
// server by us. See the note at the top of `auth.go`.

// loginSession tracks one in-flight device-code sign-in.
type loginSession struct {
	mu sync.Mutex

	// State is the opaque handle the host passes back on every poll. The host
	// validates its shape, so it stays within [A-Za-z0-9-_.]: 32 hex characters.
	State string
	// Auth is the device-code grant, including the PKCE verifier.
	Auth deviceAuthorization
	// ExpiresAt is the session's own deadline; see sessionTTL.
	ExpiresAt time.Time
	// LastAttempt is when the last upstream poll was issued, used to honour the
	// interval without blocking.
	LastAttempt time.Time

	// IntervalSeconds is the CURRENT poll interval. It grows by 5 s on every
	// `slow_down`, which is why it is mutable per session rather than read from
	// the configuration on each poll.
	IntervalSeconds int
	// Failures counts consecutive transport failures.
	Failures int

	// Status / Message are the outcome once the session is terminal.
	Status  pluginapi.AuthLoginStatus
	Message string
	// Done marks a terminal session so later polls report the same outcome.
	Done bool
	// credential is the account produced by a successful login; kept so a
	// repeated poll can re-read the outcome instead of re-polling the vendor.
	credential *Credential
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// startLoginSession requests a device code and registers a session.
func startLoginSession(h *abiboot.Host, cfg Config) (*loginSession, error) {
	auth, errAuth := requestDeviceAuthorization(h, cfg)
	if errAuth != nil {
		return nil, errAuth
	}
	state, errState := randomHex(16)
	if errState != nil {
		return nil, errState
	}
	// The session outlives the device code by a small margin so the last poll a
	// user triggers still reaches the vendor rather than failing locally.
	ttl := time.Duration(cfg.loginSessionTTL()) * time.Millisecond
	if ttl <= 0 {
		ttl = LoginTimeoutMS * time.Millisecond
	}
	if granted := time.Duration(auth.ExpiresInSeconds) * time.Second; granted > 0 && granted < ttl {
		ttl = granted
	}
	interval := auth.IntervalSeconds
	if interval <= 0 {
		interval = cfg.pollIntervalSeconds()
	}
	session := &loginSession{
		State:           state,
		Auth:            auth,
		ExpiresAt:       nowTime().Add(ttl),
		IntervalSeconds: interval,
		Status:          pluginapi.AuthLoginStatusPending,
	}
	loginMu.Lock()
	purgeExpiredLoginSessionsLocked()
	loginSessions[state] = session
	loginMu.Unlock()
	return session, nil
}

// stateValue is the handle the host echoes back.
func (s *loginSession) stateValue() string { return s.State }

// loginURL is the browser link the user opens. It is the COMPLETE uri, which
// already embeds `user_code`, so the user has nothing to type.
func (s *loginSession) loginURL() string {
	if strings.TrimSpace(s.Auth.VerificationURIComplete) != "" {
		return s.Auth.VerificationURIComplete
	}
	return s.Auth.VerificationURI
}

// expired reports whether the session outlived its budget.
func (s *loginSession) expired() bool { return nowTime().After(s.ExpiresAt) }

// fail records a terminal failure.
func (s *loginSession) fail(message string) {
	s.mu.Lock()
	s.Done = true
	s.Status = pluginapi.AuthLoginStatusError
	s.Message = message
	s.mu.Unlock()
}

// setCredential records a successful login.
func (s *loginSession) setCredential(credential *Credential, message string) {
	s.mu.Lock()
	s.credential = credential
	s.Status = pluginapi.AuthLoginStatusSuccess
	s.Message = message
	s.Done = true
	s.mu.Unlock()
}

// snapshot reads the current outcome.
func (s *loginSession) snapshot() (pluginapi.AuthLoginStatus, string, *Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status, s.Message, s.credential, s.Done
}

// due reports whether the current interval has elapsed since the last attempt.
func (s *loginSession) due() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	interval := time.Duration(s.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = time.Duration(PollIntervalSeconds) * time.Second
	}
	return nowTime().Sub(s.LastAttempt) >= interval
}

// markAttempt records that an upstream poll is being issued now.
func (s *loginSession) markAttempt() {
	s.mu.Lock()
	s.LastAttempt = nowTime()
	s.mu.Unlock()
}

// noteFailure counts a consecutive transport failure and returns the total.
func (s *loginSession) noteFailure() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Failures++
	return s.Failures
}

// resetFailures clears the consecutive-failure counter after any real answer.
func (s *loginSession) resetFailures() {
	s.mu.Lock()
	s.Failures = 0
	s.mu.Unlock()
}

// slowDown grows the poll interval by the measured 5 s.
func (s *loginSession) slowDown() {
	s.mu.Lock()
	s.IntervalSeconds += SlowDownIncrementSeconds
	s.mu.Unlock()
}

// currentIntervalSeconds reads the live interval, for the page and diagnostics.
func (s *loginSession) currentIntervalSeconds() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.IntervalSeconds <= 0 {
		return PollIntervalSeconds
	}
	return s.IntervalSeconds
}

// lookupLoginSession finds a session by its state value.
func lookupLoginSession(state string) (*loginSession, bool) {
	trimmed := strings.TrimSpace(state)
	if trimmed == "" {
		return nil, false
	}
	loginMu.Lock()
	defer loginMu.Unlock()
	session, ok := loginSessions[trimmed]
	if !ok {
		return nil, false
	}
	if session.expired() && !session.Done {
		delete(loginSessions, trimmed)
		session.fail("登录等待已超时，请重新发起登录")
		return nil, false
	}
	return session, true
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

// currentLoginSession returns the newest live session, for the login page.
func currentLoginSession() (*loginSession, bool) {
	loginMu.Lock()
	defer loginMu.Unlock()
	var newest *loginSession
	for _, session := range loginSessions {
		if session.expired() {
			continue
		}
		if newest == nil || session.ExpiresAt.After(newest.ExpiresAt) {
			newest = session
		}
	}
	return newest, newest != nil
}

// shutdownLoginSessions clears every session during plugin shutdown. There is no
// listener to close: the device-code flow owns no socket.
func shutdownLoginSessions() {
	loginMu.Lock()
	loginSessions = map[string]*loginSession{}
	loginMu.Unlock()
}

// ── auth.login.start / auth.login.poll ──

// handleAuthLoginStart requests a device code and returns the link immediately.
func handleAuthLoginStart(h *abiboot.Host, _ json.RawMessage) (any, error) {
	cfg := settings()
	session, errStart := startLoginSession(h, cfg)
	if errStart != nil {
		return nil, errStart
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       session.loginURL(),
		State:     session.stateValue(),
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"flow": "device-code",
			"hint": "在浏览器中打开 URL 完成授权后，由 auth.login.poll 轮询取回凭据；" +
				"设备码流程无需本地回调端口",
			"user_code":                session.Auth.UserCode,
			"poll_interval_seconds":    session.currentIntervalSeconds(),
			"device_code_expires_secs": session.Auth.ExpiresInSeconds,
			"login_verified":           LoginVerified,
		},
	}, nil
}

// handleAuthLoginPoll advances one in-flight login by at most one upstream call.
//
// Both "still waiting" conventions are recognised (see `classifyPoll`): the
// vendor's own HTTP 200 + `status:"pending"` AND the OAuth-standard
// `error=authorization_pending`. A `slow_down` grows the interval by 5 s in
// either form. The interval is enforced against the previous attempt, so a fast
// poller simply gets `pending` back without costing an upstream call.
func handleAuthLoginPoll(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthLoginPollRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	session, found := lookupLoginSession(request.State)
	if !found {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已超时，请重新发起登录",
		}, nil
	}

	if status, message, credential, done := session.snapshot(); done {
		defer forgetLoginSession(request.State)
		if status == pluginapi.AuthLoginStatusSuccess && credential != nil {
			auth, errAuth := authDataFor(credential, "")
			if errAuth != nil {
				return nil, errAuth
			}
			return pluginapi.AuthLoginPollResponse{Status: status, Message: message, Auth: auth}, nil
		}
		if status == "" {
			status = pluginapi.AuthLoginStatusError
		}
		return pluginapi.AuthLoginPollResponse{Status: status, Message: message}, nil
	}

	if !session.due() {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户完成授权",
		}, nil
	}
	session.markAttempt()

	decision, message, credential, errPoll := pollDeviceToken(h, settings(), session.Auth)
	switch decision {
	case pollSuccess:
		session.resetFailures()
		session.setCredential(credential, firstNonEmpty(message, "登录成功"))
		auth, errAuth := authDataFor(credential, "")
		if errAuth != nil {
			return nil, errAuth
		}
		forgetLoginSession(request.State)
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: firstNonEmpty(message, "登录成功"),
			Auth:    auth,
		}, nil

	case pollPending:
		session.resetFailures()
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: firstNonEmpty(message, "等待用户完成授权"),
		}, nil

	case pollSlowDown:
		session.resetFailures()
		session.slowDown()
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "服务端要求放慢轮询，已把间隔调整为 " + itoaInt(session.currentIntervalSeconds()) + " 秒",
		}, nil

	case pollDenied:
		session.fail(firstNonEmpty(message, "用户在浏览器中拒绝了授权"))
		forgetLoginSession(request.State)
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: firstNonEmpty(message, "用户在浏览器中拒绝了授权"),
		}, nil

	case pollExpired:
		session.fail(firstNonEmpty(message, "设备码已过期，请重新发起登录"))
		forgetLoginSession(request.State)
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: firstNonEmpty(message, "设备码已过期，请重新发起登录"),
		}, nil
	}

	// A genuine failure. A TRANSPORT fault is retried a bounded number of times
	// so a flaky network does not abandon a user who is still authorising; any
	// other failure ends the flow immediately. The distinction is drawn from the
	// error's own retryability, which is how every other classified failure in
	// this plugin is read — never from the message text.
	if errPoll != nil && isRetryable(errPoll) {
		if failures := session.noteFailure(); failures < PollMaxFailures {
			return pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusPending,
				Message: "轮询失败（第 " + itoaInt(failures) + " 次），稍后重试：" + errPoll.Error(),
			}, nil
		}
	}
	detail := message
	if errPoll != nil {
		detail = errPoll.Error()
	}
	if strings.TrimSpace(detail) == "" {
		detail = "MiniMax 授权失败"
	}
	session.fail(detail)
	forgetLoginSession(request.State)
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusError,
		Message: detail,
	}, nil
}

// randomHex returns n random bytes as a hex string.
func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", abiboot.Errorf("random_failed", "生成随机数失败：%v", errRead)
	}
	return hex.EncodeToString(raw), nil
}

// sanitizeFileName strips anything that cannot appear in an auth file name.
func sanitizeFileName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.' || r == '_' || r == '@' || r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}
