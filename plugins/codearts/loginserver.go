package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/oauthcb"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginTTL bounds how long an interactive sign-in may stay pending.
const loginTTL = 5 * time.Minute

// minCallbackPort matches the constraint the CodeArts portal enforces on the
// loopback redirect: the port must be 10000 or above.
const minCallbackPort = 10000

// ticketCallbackHTML is the page the browser lands on after the legacy ticket
// callback; the OAuth flow redirects to the portal success URL instead.
const ticketCallbackHTML = "<html><head><meta charset=\"utf-8\"><title>CodeArts</title></head><body>登录回调已收到，请返回 CPA 管理面板完成授权。</body></html>"

// loginSession tracks one in-flight interactive sign-in. The browser callback
// is captured by a loopback listener; the credential exchange itself happens
// inside a later auth.login.poll invocation so that it can reuse that
// invocation's host callback identity.
type loginSession struct {
	mu sync.Mutex

	State     string
	Flow      string
	Port      int
	TicketID  string
	PKCE      *pkcePair
	KeyPair   *dpopKeyPair
	AuthCode  string
	Secret    string
	CreatedAt time.Time
	ExpiresAt time.Time

	status     pluginapi.AuthLoginStatus
	message    string
	credential *Credential
	finished   bool

	// Callback is the most recent browser callback captured on this session.
	Callback oauthcb.Result

	// callback owns the loopback listener. Closing it (expire/finish/fail or
	// plugin shutdown) also releases the session's callback goroutine.
	callback *oauthcb.Server
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// startLoginSession allocates a loopback callback listener and registers the
// session under a fresh random state value.
func startLoginSession(flow string) (*loginSession, error) {
	state, errState := randomHex(16)
	if errState != nil {
		return nil, errState
	}
	// Both flows use a fresh 32-byte hex ticket id.
	ticketID, errTicket := randomHex(32)
	if errTicket != nil {
		return nil, errTicket
	}

	session := &loginSession{
		State:     state,
		Flow:      flow,
		TicketID:  ticketID,
		CreatedAt: time.Now(),
		status:    pluginapi.AuthLoginStatusPending,
	}

	if flow == LoginFlowOAuth {
		pkce, errPKCE := newPKCE()
		if errPKCE != nil {
			return nil, errPKCE
		}
		keyPair, errKey := newDpopKeyPair()
		if errKey != nil {
			return nil, errKey
		}
		session.PKCE = pkce
		session.KeyPair = keyPair
	}

	options := oauthcb.Options{
		Path:    LegacyCallbackPath,
		MinPort: minCallbackPort,
		TTL:     loginTTL,
	}
	if flow == LoginFlowOAuth {
		options.Path = OAuthRedirectPath
		options.RedirectURL = oauthSuccessRedirect()
	} else {
		options.SuccessHTML = ticketCallbackHTML
	}

	callback, errListen := oauthcb.Start(options)
	if errListen != nil {
		return nil, abiboot.Errorf("callback_listen", "start CodeArts callback listener: %v", errListen)
	}
	session.callback = callback
	session.Port = callback.Port()
	session.ExpiresAt = callback.ExpiresAt()

	loginMu.Lock()
	purgeExpiredLoginSessionsLocked()
	loginSessions[state] = session
	loginMu.Unlock()

	// oauthcb.Wait blocks, so the capture runs in its own goroutine and only
	// stores the Result; the credential exchange stays in auth.login.poll (see
	// the loginSession comment).
	go session.awaitCallback()
	return session, nil
}

// awaitCallback stores the callback delivered by the browser on the session
// under its mutex. A callback missing the parameter its flow needs is
// discarded and the wait resumes, matching the old handler's 400 response.
func (s *loginSession) awaitCallback() {
	for {
		result, errWait := s.callback.Wait(context.Background())
		if errWait != nil {
			// TTL expiry, plugin shutdown, or an already-finished session.
			return
		}

		s.mu.Lock()
		if s.finished {
			s.mu.Unlock()
			return
		}
		s.Callback = result
		if s.Flow == LoginFlowOAuth {
			s.AuthCode = result.Code
		} else {
			s.Secret = result.Secret
		}
		s.mu.Unlock()

		if s.Flow == LoginFlowOAuth && result.Code == "" {
			continue
		}
		if s.Flow != LoginFlowOAuth && result.Secret == "" {
			continue
		}
		return
	}
}

// RedirectURI is the loopback redirect the portal must call back.
func (s *loginSession) RedirectURI() string {
	if s.callback != nil {
		return s.callback.RedirectURI()
	}
	if s.Flow == LoginFlowOAuth {
		return fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, OAuthRedirectPath)
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, LegacyCallbackPath)
}

// LoginURL returns the URL the user must open in a browser.
func (s *loginSession) LoginURL() string {
	if s.Flow == LoginFlowOAuth {
		return buildOAuthLoginURL(s.Port, s.PKCE, s.TicketID)
	}
	return buildLegacyLoginURL(s.Port, s.TicketID)
}

// expired reports whether the session outlived its TTL.
func (s *loginSession) expired() bool { return time.Now().After(s.ExpiresAt) }

// expire marks a session as failed and releases its listener.
func (s *loginSession) expire(message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusError
	s.message = message
	s.mu.Unlock()
	s.closeCallback()
}

// finish records a successful credential and releases the listener.
func (s *loginSession) finish(credential *Credential, message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusSuccess
	s.message = message
	s.credential = credential
	s.mu.Unlock()
	s.closeCallback()
}

// fail records a terminal failure and releases the listener.
func (s *loginSession) fail(message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusError
	s.message = message
	s.mu.Unlock()
	s.closeCallback()
}

// closeCallback releases the loopback listener, if one was bound.
func (s *loginSession) closeCallback() {
	s.mu.Lock()
	callback := s.callback
	s.mu.Unlock()
	if callback != nil {
		_ = callback.Close()
	}
}

// snapshot reads the current session outcome.
func (s *loginSession) snapshot() (pluginapi.AuthLoginStatus, string, *Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.message, s.credential, s.finished
}

// pendingCode reports the captured authorization code, if any.
func (s *loginSession) pendingCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AuthCode
}

// pendingSecret reports the captured ticket-callback secret, if any.
func (s *loginSession) pendingSecret() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Secret
}

// lookupLoginSession finds a non-expired session by its state value.
func lookupLoginSession(state string) (*loginSession, bool) {
	loginMu.Lock()
	session, ok := loginSessions[state]
	if ok && session.expired() && !session.finished {
		delete(loginSessions, state)
		ok = false
		session.expire("登录会话已超时，请重新发起")
	}
	loginMu.Unlock()
	return session, ok
}

// forgetLoginSession drops a completed session from the registry.
func forgetLoginSession(state string) {
	loginMu.Lock()
	_, ok := loginSessions[state]
	if ok {
		delete(loginSessions, state)
	}
	loginMu.Unlock()
}

// purgeExpiredLoginSessionsLocked drops expired entries. Callers must hold loginMu.
func purgeExpiredLoginSessionsLocked() {
	for state, session := range loginSessions {
		if session.expired() {
			delete(loginSessions, state)
			session.expire("登录会话已超时，请重新发起")
		}
	}
}

// shutdownLoginSessions closes every listener, used during plugin shutdown.
func shutdownLoginSessions() {
	loginMu.Lock()
	sessions := make([]*loginSession, 0, len(loginSessions))
	for state, session := range loginSessions {
		sessions = append(sessions, session)
		delete(loginSessions, state)
	}
	loginMu.Unlock()
	for _, session := range sessions {
		session.closeCallback()
	}
}
