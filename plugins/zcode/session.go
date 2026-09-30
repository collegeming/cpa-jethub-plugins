package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// loginSession tracks one in-flight device-authorization sign-in.
//
// The flow is two-step by construction. `auth.login.start` returns the
// authorization URL immediately and never waits for the user, because a browser
// popup opened after a blocking call loses the transient activation window — and
// `auth.login.poll` then advances the flow one upstream call at a time.
//
// There is NO local callback listener: the authorization completes on the SERVER
// and the CLI polls `/api/v1/oauth/cli/poll/<flow_id>` for the token
// (`zcode-login.ts:1-30`). That is the same shape as the Qoder device-code flow,
// and it is why this plugin has no `callback_port` setting.
type loginSession struct {
	mu sync.Mutex

	// State is the opaque value the host echoes back when polling.
	State string
	// FlowID is the upstream flow identifier (used to build the poll URL).
	FlowID string
	// FlowSecret is the CLI session key this login generated. The official client
	// uses it as the bearer on BOTH the init request and every poll
	// (`zcode-login.ts:151-160`).
	FlowSecret string
	// AuthorizeURL is what the user opens in a browser.
	AuthorizeURL string
	// PollInterval is the interval the SERVER asked for, in milliseconds.
	PollInterval int

	CreatedAt time.Time
	ExpiresAt time.Time

	// Failures counts consecutive transport failures.
	Failures int
	// LastAttempt is when the last upstream poll was issued, used to honour the
	// poll interval without blocking a host invocation.
	LastAttempt time.Time
	// Status / Message are the terminal outcome once the flow settles.
	Status  pluginapi.AuthLoginStatus
	Message string
	// Done marks a terminal session so later polls report the same outcome.
	Done bool
	// credential is the account produced by a successful login, kept so a
	// repeated poll (and the management page) can re-read the outcome.
	credential *Credential
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// startLoginSession registers a fresh authorization session.
//
// The FlowID/secret/URL are filled in by the caller once the init request
// answers; the session is registered first so a failure can be recorded on it.
func startLoginSession() (*loginSession, error) {
	state, errState := randomHex(16)
	if errState != nil {
		return nil, errState
	}
	timeout := time.Duration(settings().LoginTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = DefaultLoginTimeoutMS * time.Millisecond
	}
	session := &loginSession{
		State:     state,
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

// configure records the upstream flow details.
func (s *loginSession) configure(flow *loginFlow) {
	s.mu.Lock()
	s.FlowID = flow.FlowID
	s.FlowSecret = flow.FlowSecret
	s.AuthorizeURL = flow.AuthorizeURL
	s.PollInterval = flow.PollIntervalMS
	if flow.ExpiresAt > 0 {
		// The server's own validity window wins when it is SHORTER than ours: a
		// local deadline that outlives the server's keeps polling a flow the user
		// can no longer authorize, and reports "timed out" instead of the more
		// accurate "the flow expired".
		if serverDeadline := time.Unix(flow.ExpiresAt, 0).Add(-time.Second); serverDeadline.Before(s.ExpiresAt) {
			s.ExpiresAt = serverDeadline
		}
	}
	s.mu.Unlock()
}

// pollURL is the endpoint this session polls.
func (s *loginSession) pollURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Origin + OAuthCLIPollPrefix + s.FlowID
}

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
// listener to close: the device-authorization flow owns no socket.
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

// generateDeviceMid produces a fresh device id.
//
// The reference measured that the VALUE is not validated by the server: the same
// JWT returns 200 with the official id, with a random UUID, and with a brand-new
// random UUID on every attempt — while omitting the header entirely returns
// `400 code 3001`. So the plugin generates its own and persists it in the
// credential, which is what makes it independent of the official client.
func generateDeviceMid() string {
	raw := make([]byte, 16)
	if _, errRead := rand.Read(raw); errRead != nil {
		// A device id is not security material; a timestamp-derived fallback
		// still satisfies the "must be present" requirement.
		return hex.EncodeToString([]byte(time.Now().UTC().Format("20060102150405.000000000")))
	}
	// RFC 4122 version 4 layout, so the value looks like the official one.
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(raw)
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// nowMillis is the current time as a millisecond timestamp.
func nowMillis() int64 { return time.Now().UnixMilli() }
