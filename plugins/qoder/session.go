package main

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginSession tracks one in-flight device-code sign-in.
//
// The device-code flow is two-step by construction (`qoder-oauth.ts:151-159`):
// `auth.login.start` returns the authorization URL immediately and never waits
// for the user, because a browser popup opened after a blocking call loses the
// transient activation window (and a front-end fallback then navigates the whole
// settings page away). `auth.login.poll` advances the flow one attempt at a time.
type loginSession struct {
	mu sync.Mutex

	State     string
	Region    Region
	Device    *deviceSession
	CreatedAt time.Time
	ExpiresAt time.Time

	// Failures counts consecutive transport failures, matching
	// `QODER_POLL_MAX_FAILURES` (`qoder-oauth.ts:290-299`).
	Failures int
	// LastAttempt is when the last upstream poll was issued, used to honour the
	// poll interval without blocking a host invocation.
	LastAttempt time.Time
	// Status / Message are terminal outcomes once the flow fails.
	Status  pluginapi.AuthLoginStatus
	Message string
	// Done marks a terminal session so later polls report the same outcome.
	Done bool
	// credential is the account produced by a successful login. It is kept so a
	// repeated poll (and the management page) can re-read the outcome.
	credential *Credential
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// startLoginSession registers a fresh device-code session for one region.
func startLoginSession(region Region) (*loginSession, error) {
	state, errState := randomHex(16)
	if errState != nil {
		return nil, errState
	}
	device, errDevice := newDeviceSession("")
	if errDevice != nil {
		return nil, errDevice
	}
	timeout := time.Duration(settings().LoginTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = LoginTimeoutMS * time.Millisecond
	}
	session := &loginSession{
		State:     state,
		Region:    normalizeRegion(string(region)),
		Device:    device,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(timeout),
		Status:    pluginapi.AuthLoginStatusPending,
	}

	loginMu.Lock()
	purgeExpiredLoginSessionsLocked()
	loginSessions[state] = session
	loginMu.Unlock()
	return session, nil
}

// LoginURL is the browser URL the user must open.
func (s *loginSession) LoginURL() string { return s.Device.authURL(productByID(string(s.Region))) }

// PollURL is the endpoint the plugin polls; shown for diagnostics.
func (s *loginSession) PollURL() string { return s.Device.pollURL(productByID(string(s.Region))) }

// expired reports whether the session outlived its budget.
func (s *loginSession) expired() bool { return time.Now().After(s.ExpiresAt) }

// fail records a terminal failure.
func (s *loginSession) fail(message string) {
	s.mu.Lock()
	s.Done = true
	s.Status = pluginapi.AuthLoginStatusError
	s.Message = message
	s.mu.Unlock()
}

// finish marks the session terminal with a non-error outcome.
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

// due reports whether the poll interval has elapsed since the last attempt.
func (s *loginSession) due(interval time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// resetFailures clears the consecutive-failure counter after any answer.
func (s *loginSession) resetFailures() {
	s.mu.Lock()
	s.Failures = 0
	s.mu.Unlock()
}

// lookupLoginSession finds a session by its state value.
func lookupLoginSession(state string) (*loginSession, bool) {
	loginMu.Lock()
	defer loginMu.Unlock()
	session, ok := loginSessions[state]
	if !ok {
		return nil, false
	}
	if session.expired() && !session.Done {
		delete(loginSessions, state)
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

// shutdownLoginSessions clears every session during plugin shutdown. There is no
// listener to close: the device-code flow owns no socket (`qoder-oauth.ts:144-150`).
func shutdownLoginSessions() {
	loginMu.Lock()
	loginSessions = map[string]*loginSession{}
	loginMu.Unlock()
}

// randomHex returns n random bytes as a hex string.
func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", abiboot.Errorf("random_failed", "generate random bytes: %v", err)
	}
	return hex.EncodeToString(raw), nil
}

// safeFileNameChars strips anything that cannot appear in an auth file name.
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

// nowMillis is the current time as a millisecond timestamp.
func nowMillis() int64 { return time.Now().UnixMilli() }
