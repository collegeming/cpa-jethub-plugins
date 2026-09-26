package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/oauthcb"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginTTL bounds how long an interactive sign-in may stay pending. It is the
// plugin's own deadline, not the listener's: the shared callback listener is
// persistent and reports the zero time for ExpiresAt, so the panel's polling and
// the state lookup read this instead.
const loginTTL = 5 * time.Minute

// minCallbackPort matches the constraint the CodeArts portal enforces on the
// loopback redirect: the port must be 10000 or above.
const minCallbackPort = 10000

// ticketCallbackHTML is the page the browser lands on after the legacy ticket
// callback; the OAuth flow redirects to the portal success URL instead.
const ticketCallbackHTML = "<html><head><meta charset=\"utf-8\"><title>CodeArts</title></head><body>登录回调已收到，请返回 CPA 管理面板完成授权。</body></html>"

// loginSession tracks one in-flight interactive sign-in. The browser callback
// is captured by the plugin's shared loopback listener; the credential exchange
// itself happens inside a later auth.login.poll invocation so that it can reuse
// that invocation's host callback identity.
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

	// Callback is the most recent browser callback routed to this session.
	Callback oauthcb.Result

	// callback is the plugin's long-lived listener, shared with every other
	// session. It is owned by callbackListener and closed only at plugin
	// shutdown: a session finishing, failing or expiring must leave the port
	// bound, because the browser reaches it after the session that started the
	// flow can already be gone.
	callback *oauthcb.Server
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// callbackServer is the one listener this plugin binds for interactive sign-in.
// It is created on the first sign-in and reused for the rest of the process, so
// the port a container publishes stays bound between attempts. A per-session
// bind released the port whenever that session ended — timed out, superseded, or
// lost to a container restart — and the browser was then redirected to a closed
// port, which is the ERR_CONNECTION_REFUSED the container deployment showed.
//
// callbackKey records the options that built the cached server: only a change to
// those options (a flow switch, a reconfigured callback address) justifies a
// rebind.
var (
	callbackMu     sync.Mutex
	callbackServer *oauthcb.Server
	callbackKey    string
)

// callbackListener returns the plugin's long-lived callback listener, building
// or rebuilding it only when the settings that shape it change.
func callbackListener(options oauthcb.Options) (*oauthcb.Server, error) {
	key := callbackOptionsKey(options)

	callbackMu.Lock()
	defer callbackMu.Unlock()
	if callbackServer != nil && callbackKey == key {
		return callbackServer, nil
	}
	// The cached listener no longer answers what this sign-in needs. The old one
	// is released BEFORE the new bind because a pinned port cannot be held twice
	// and a flow switch keeps the same pinned port.
	if callbackServer != nil {
		_ = callbackServer.Close()
		callbackServer = nil
		callbackKey = ""
	}

	server, errStart := oauthcb.Start(options)
	if errStart != nil {
		// Cache nothing: the next sign-in retries the bind instead of reusing a
		// half-configured listener.
		return nil, errStart
	}
	callbackServer = server
	callbackKey = key
	go dispatchCallbacks(server)
	return server, nil
}

// closeCallbackListener releases the shared listener, if one is bound. Only
// plugin shutdown calls it: session teardown deliberately leaves the port bound.
func closeCallbackListener() {
	callbackMu.Lock()
	server := callbackServer
	callbackServer = nil
	callbackKey = ""
	callbackMu.Unlock()
	if server != nil {
		_ = server.Close()
	}
}

// callbackOptionsKey encodes every option that changes what the listener serves
// or reports. Path and the success answer separate the two flows, which answer
// different callbacks; the bind and public addresses decide where the port is
// reachable and what redirect_uri the portal and the STS token exchange see.
func callbackOptionsKey(options oauthcb.Options) string {
	return strings.Join([]string{
		options.Path,
		options.RedirectURL,
		options.SuccessHTML,
		options.BindHost,
		strconv.Itoa(options.Port),
		options.PublicHost,
		strconv.Itoa(options.PublicPort),
	}, "\x00")
}

// dispatchCallbacks routes every captured callback to the sessions waiting for
// one. It is the listener's only reader, so callbacks arriving while no sign-in
// is pending are no longer lost with the session that started the flow.
func dispatchCallbacks(server *oauthcb.Server) {
	for {
		result, errWait := server.Wait(context.Background())
		if errWait != nil {
			// The listener was closed or replaced; nothing more can arrive.
			return
		}

		// The portal's callback carries no value this plugin controls, so a
		// result cannot be attributed to one attempt: every pending session is
		// offered it, and deliver applies the per-session rules.
		loginMu.Lock()
		sessions := make([]*loginSession, 0, len(loginSessions))
		for _, session := range loginSessions {
			sessions = append(sessions, session)
		}
		loginMu.Unlock()

		// Outside the lock: deliver takes each session's own mutex, so one
		// session can never hold up another one's callback.
		for _, session := range sessions {
			session.deliver(result)
		}
	}
}

// startLoginSession publishes a sign-in under a fresh random state value, using
// the plugin's shared callback listener as the portal's redirect target.
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

	// The CodeArts portal derives host and path from the `port` parameter we
	// send, so there is no way to point the browser at the host's own callback
	// endpoint: this listener is the only option. That makes the bind address a
	// deployment concern — under Docker the container's 127.0.0.1 is not the
	// browser's, and an ephemeral port cannot be published in advance, so such a
	// deployment pins the port and binds 0.0.0.0.
	cfg := settings()
	// The portal rejects a callback below 10000 (login.ts:338-341). A pinned
	// port bypasses the MinPort retry loop, so catch it here: otherwise the
	// portal simply never calls back and the failure looks like a network
	// problem instead of a misconfiguration.
	if cfg.CallbackPort > 0 && cfg.CallbackPort < minCallbackPort {
		return nil, abiboot.Errorf("callback_port",
			"callback_port 必须 ≥ %d（华为 portal 的硬性要求），当前为 %d",
			minCallbackPort, cfg.CallbackPort)
	}
	options := oauthcb.Options{
		Path:       LegacyCallbackPath,
		MinPort:    minCallbackPort,
		Persistent: true, // the listener outlives the sign-in; see callbackServer
		Port:       cfg.CallbackPort,
		BindHost:   cfg.CallbackBindHost,
		PublicHost: cfg.CallbackPublicHost,
		PublicPort: cfg.CallbackPublicPort,
	}
	if flow == LoginFlowOAuth {
		options.Path = OAuthRedirectPath
		options.RedirectURL = oauthSuccessRedirect()
	} else {
		options.SuccessHTML = ticketCallbackHTML
	}

	// Settling a superseded session and publishing the new one are one step, so
	// the panel never sees two pending sign-ins on one pinned port. Superseding
	// no longer protects a bind — the listener already holds the port — but the
	// shared listener offers each callback to every pending session and the
	// plugin cannot tell which attempt a code belongs to, so the replaced attempt
	// would otherwise answer with a code meant for the new one.
	loginMu.Lock()
	purgeExpiredLoginSessionsLocked()
	if options.Port > 0 {
		supersedePendingLoginSessionsLocked(supersededLoginMessage)
	}
	callback, errListen := callbackListener(options)
	if errListen != nil {
		loginMu.Unlock()
		if options.Port > 0 {
			return nil, abiboot.Errorf("callback_listen",
				"CodeArts 回调端口 %d 无法监听（%v）；请确认该端口未被其它程序占用", options.Port, errListen)
		}
		return nil, abiboot.Errorf("callback_listen", "start CodeArts callback listener: %v", errListen)
	}
	session.callback = callback
	session.Port = callback.Port()
	// A persistent listener reports the zero time for ExpiresAt, so the sign-in
	// deadline is the plugin's own TTL — the five minutes the panel advertises.
	session.ExpiresAt = session.CreatedAt.Add(loginTTL)
	loginSessions[state] = session
	loginMu.Unlock()

	return session, nil
}

// deliver routes one browser callback to the session under its mutex. A callback
// missing the parameter its flow needs is ignored and the session stays pending,
// which is the persistent equivalent of the old per-session wait loop retrying.
// A captured value is never overwritten by a later unusable callback: the old
// loop stopped reading its listener once it had a usable result, and the panel
// may not have polled it yet.
func (s *loginSession) deliver(result oauthcb.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.Callback = result
	if s.Flow == LoginFlowOAuth {
		if result.Code != "" {
			s.AuthCode = result.Code
		}
		return
	}
	if result.Secret != "" {
		s.Secret = result.Secret
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

// expire marks a session as failed. The shared listener stays bound.
func (s *loginSession) expire(message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusError
	s.message = message
	s.mu.Unlock()
}

// finish records a successful credential. The shared listener stays bound.
func (s *loginSession) finish(credential *Credential, message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusSuccess
	s.message = message
	s.credential = credential
	s.mu.Unlock()
}

// fail records a terminal failure. The shared listener stays bound.
func (s *loginSession) fail(message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusError
	s.message = message
	s.mu.Unlock()
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

// supersededLoginMessage is what a replaced sign-in reports. The panel may still
// be polling the older state, so it has to say the attempt was replaced rather
// than look like a silent stall.
const supersededLoginMessage = "该登录已被新的登录请求取代，请重新发起"

// supersedePendingLoginSessionsLocked settles every live session and forgets it.
// A pinned port carries one sign-in at a time — the callback cannot be attributed
// to a specific attempt — so an earlier one is settled explicitly to stop the
// panel polling a state that can never complete. Callers must hold loginMu.
func supersedePendingLoginSessionsLocked(message string) {
	for state, session := range loginSessions {
		delete(loginSessions, state)
		session.expire(message)
	}
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

// shutdownLoginSessions drops every pending session and releases the shared
// listener, the only lifetime event that closes it.
func shutdownLoginSessions() {
	loginMu.Lock()
	for state := range loginSessions {
		delete(loginSessions, state)
	}
	loginMu.Unlock()

	closeCallbackListener()
}
