package main

import (
	"context"
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
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/oauthcb"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Three-step interactive sign-in, ported from
// jethub-src/src/lobsterai-oauth.ts:
//
//  1. create a login session (random uuid + firstKeyfrom) and bind a loopback
//     callback listener on a random port;
//  2. hand the browser
//     `{portalBase}/portal#/login?source=electron&redirect_uri=...&state=...`;
//     the portal redirects back to `http://127.0.0.1:<port>/auth/callback`
//     with `code` and `state`;
//  3. POST the authorization code to `/api/auth/exchange` with exactly
//     `{authCode, firstKeyfrom, latestKeyfrom, uuid, version}` and no
//     Authorization header.
//
// The reference TypeScript does the exchange inside the callback handler. This
// port captures the callback in the listener and performs the exchange in a
// later `auth.login.poll` invocation instead, so the upstream call can reuse
// that invocation's host callback identity (the same design as
// plugins/codearts/loginserver.go). The observable two-step behaviour is
// unchanged: `auth.login.start` returns the URL immediately and never blocks.

// callbackSuccessHTML is the page the browser lands on once the code has been
// captured. The exchange itself happens on the next poll.
const callbackSuccessHTML = "<html><head><meta charset=\"utf-8\"><title>LobsterAI</title></head>" +
	"<body>登录回调已收到，请返回 CPA 管理面板完成授权。</body></html>"

// loginSession tracks one in-flight interactive sign-in.
type loginSession struct {
	mu sync.Mutex

	// State is the anti-CSRF value echoed by the portal.
	State string
	// UUID is the installation uuid generated for this login.
	UUID string
	// FirstKeyfrom is the login timestamp in milliseconds (string form).
	FirstKeyfrom string
	// LatestKeyfrom is submitted with the exchange and persisted as-is
	// afterwards: the reference never refreshes it.
	LatestKeyfrom string
	// Port is the bound loopback callback port.
	Port int
	// AuthCode is the captured authorization code.
	AuthCode string
	// Callback is the most recent browser callback seen on this session.
	Callback oauthcb.Result

	CreatedAt time.Time
	ExpiresAt time.Time

	status     pluginapi.AuthLoginStatus
	message    string
	credential *Credential
	finished   bool

	callback *oauthcb.Server
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, errRead := cryptoRead(buf); errRead != nil {
		return "", abiboot.Errorf("random_generate", "generate random bytes: %v", errRead)
	}
	return hex.EncodeToString(buf), nil
}

// cryptoRead fills buf with cryptographically secure random bytes.
func cryptoRead(buf []byte) (int, error) { return rand.Read(buf) }

// createLoginSessionState builds the client-side login state
// (lobsterai-oauth.ts:83-85): uuid plus the first-login timestamp.
func createLoginSessionState(now time.Time) (loginSessionState, error) {
	uuid, errUUID := randomHex(16)
	if errUUID != nil {
		return loginSessionState{}, errUUID
	}
	return loginSessionState{
		UUID:          uuid,
		FirstKeyfrom:  fmt.Sprintf("%d", now.UnixMilli()),
		LatestKeyfrom: fmt.Sprintf("%d", now.UnixMilli()),
	}, nil
}

// startLoginSession allocates the loopback listener and registers the session
// under a fresh random state value.
func startLoginSession(now time.Time) (*loginSession, error) {
	state, errState := randomHex(16)
	if errState != nil {
		return nil, errState
	}
	sessionState, errSession := createLoginSessionState(now)
	if errSession != nil {
		return nil, errSession
	}

	session := &loginSession{
		State:         state,
		UUID:          sessionState.UUID,
		FirstKeyfrom:  sessionState.FirstKeyfrom,
		LatestKeyfrom: sessionState.LatestKeyfrom,
		CreatedAt:     now,
		status:        pluginapi.AuthLoginStatusPending,
	}

	callback, errListen := oauthcb.Start(oauthcb.Options{
		Path:        CallbackPath,
		MinPort:     MinCallbackPort,
		TTL:         LoginTimeout,
		SuccessHTML: callbackSuccessHTML,
	})
	if errListen != nil {
		return nil, abiboot.Errorf("callback_listen", "start LobsterAI callback listener: %v", errListen)
	}
	session.callback = callback
	session.Port = callback.Port()
	session.ExpiresAt = callback.ExpiresAt()

	loginMu.Lock()
	purgeExpiredLoginSessionsLocked(now)
	loginSessions[state] = session
	loginMu.Unlock()

	go session.awaitCallback()
	return session, nil
}

// awaitCallback stores the callback delivered by the browser. A callback whose
// state does not match this session is discarded and the wait resumes; the
// reference answers such a request with HTTP 400, which the shared oauthcb
// listener cannot do per-request, so the observable difference is only the
// browser page (documented deviation).
func (s *loginSession) awaitCallback() {
	for {
		result, errWait := s.callback.Wait(context.Background())
		if errWait != nil {
			// TTL expiry, plugin shutdown, or an already-finished session.
			return
		}
		if strings.TrimSpace(result.Query.Get("state")) != s.State {
			continue
		}
		if strings.TrimSpace(result.Code) == "" {
			continue
		}

		s.mu.Lock()
		if s.finished {
			s.mu.Unlock()
			return
		}
		s.Callback = result
		s.AuthCode = result.Code
		s.mu.Unlock()
		return
	}
}

// RedirectURI is the loopback redirect the portal must call back.
func (s *loginSession) RedirectURI() string {
	if s.callback != nil {
		return s.callback.RedirectURI()
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, CallbackPath)
}

// LoginURL builds the portal login URL (lobsterai-oauth.ts:104-116). The hash
// segment cannot be produced by url.Values, so the path + fragment are
// assembled explicitly and only the query is percent-encoded — the redirect_uri
// contains "://" and ":" and must be encoded for the portal's validator.
func (s *loginSession) LoginURL() string {
	return buildLoginURL(s.Port, s.State)
}

// buildLoginURL assembles the portal URL for a bound port and state value.
func buildLoginURL(port int, state string) string {
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath)
	query := url.Values{
		"source":       {"electron"},
		"redirect_uri": {redirectURI},
		"state":        {state},
	}
	return PortalBase + "/portal#/login?" + query.Encode()
}

// sessionState returns the identity fields the exchange must submit.
func (s *loginSession) sessionState() loginSessionState {
	return loginSessionState{UUID: s.UUID, FirstKeyfrom: s.FirstKeyfrom, LatestKeyfrom: s.LatestKeyfrom}
}

// expired reports whether the session outlived its TTL.
func (s *loginSession) expired(now time.Time) bool { return now.After(s.ExpiresAt) }

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

// lookupLoginSession finds a non-expired session by its state value.
func lookupLoginSession(state string, now time.Time) (*loginSession, bool) {
	loginMu.Lock()
	session, ok := loginSessions[state]
	if ok && session.expired(now) && !sessionFinished(session) {
		delete(loginSessions, state)
		ok = false
		session.expire("登录会话已超时，请重新发起")
	}
	loginMu.Unlock()
	return session, ok
}

// sessionFinished reads the finished flag without racing.
func sessionFinished(session *loginSession) bool {
	_, _, _, finished := session.snapshot()
	return finished
}

// forgetLoginSession drops a completed session from the registry.
func forgetLoginSession(state string) {
	loginMu.Lock()
	if _, ok := loginSessions[state]; ok {
		delete(loginSessions, state)
	}
	loginMu.Unlock()
}

// purgeExpiredLoginSessionsLocked drops expired entries. Callers must hold
// loginMu.
func purgeExpiredLoginSessionsLocked(now time.Time) {
	for state, session := range loginSessions {
		if session.expired(now) {
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

// exchangeRequest is the exact exchange body. The endpoint requires these five
// fields and no Authorization header (lobsterai-oauth.ts:130-159).
func exchangeRequest(code string, session loginSessionState, clientVersion string, now time.Time) map[string]any {
	return map[string]any{
		"authCode":      code,
		"firstKeyfrom":  session.FirstKeyfrom,
		"latestKeyfrom": fmt.Sprintf("%d", now.UnixMilli()),
		"uuid":          session.UUID,
		"version":       clientVersion,
	}
}

// exchangeAuthCode trades the portal callback code for a credential.
func exchangeAuthCode(h *abiboot.Host, code string, session loginSessionState, clientVersion string, now time.Time) (*Credential, error) {
	body := exchangeRequest(code, session, clientVersion, now)
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_exchange", "encode LobsterAI exchange body: %v", errMarshal)
	}
	if h == nil {
		return nil, abiboot.Errorf("exchange_transport", "LobsterAI exchange 网络失败：host transport 不可用")
	}
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodPost,
		URL:     APIBase + ExchangePath,
		Headers: anonymousHeaders(),
		Body:    encoded,
	})
	if errDo != nil {
		return nil, abiboot.Errorf("exchange_transport", "LobsterAI exchange 网络失败：%v", errDo)
	}

	envelope := parseEnvelope(response.Body)
	if !envelope.OK {
		return nil, abiboot.Errorf("exchange_rejected", "LobsterAI exchange 失败：%s", envelope.Message)
	}
	payload := parseTokenPayload(envelope.Data)
	if strings.TrimSpace(payload.AccessToken) == "" {
		// No token means no usable credential; never store a half-built record.
		return nil, abiboot.Errorf("exchange_incomplete", "LobsterAI exchange 响应缺少 accessToken")
	}
	// latest_keyfrom submitted with the exchange is the value persisted, exactly
	// like the reference (lobsterai-oauth.ts:180-184).
	persisted := session
	persisted.LatestKeyfrom = stringifyValue(body["latestKeyfrom"])
	return buildCredential(payload, persisted, now), nil
}

// handleAuthLoginStart opens the loopback listener and returns the browser URL
// immediately (two-step login; a blocking implementation would break the
// browser gesture).
func handleAuthLoginStart(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	session, errStart := startLoginSession(time.Now())
	if errStart != nil {
		return nil, errStart
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       session.LoginURL(),
		State:     session.State,
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"uuid":          session.UUID,
			"first_keyfrom": session.FirstKeyfrom,
			"port":          session.Port,
			"redirect_uri":  session.RedirectURI(),
			"flow":          "callback+authCode",
		},
	}, nil
}

// pollLogin advances one in-flight sign-in. Network work happens here rather
// than in the callback goroutine so it can use this invocation's host callback
// identity.
func pollLogin(h *abiboot.Host, state string, now time.Time) (pluginapi.AuthLoginPollResponse, error) {
	session, found := lookupLoginSession(state, now)
	if !found {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已超时，请重新发起登录",
		}, nil
	}

	if status, message, credential, finished := session.snapshot(); finished {
		defer forgetLoginSession(state)
		if status == pluginapi.AuthLoginStatusSuccess && credential != nil {
			auth, errAuth := authDataFor(credential, "")
			if errAuth != nil {
				return pluginapi.AuthLoginPollResponse{}, errAuth
			}
			return pluginapi.AuthLoginPollResponse{Status: status, Message: message, Auth: auth}, nil
		}
		return pluginapi.AuthLoginPollResponse{Status: status, Message: message}, nil
	}

	code := session.pendingCode()
	if code == "" {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待浏览器完成登录",
		}, nil
	}

	cfg := settings()
	clientVersion := resolveClientVersion(h, cfg)
	credential, errExchange := exchangeAuthCode(h, code, session.sessionState(), clientVersion, now)
	if errExchange != nil {
		message := errExchange.Error()
		session.fail(message)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
	}
	session.finish(credential, "登录成功")
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		return pluginapi.AuthLoginPollResponse{}, errAuth
	}
	forgetLoginSession(state)
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "登录成功", Auth: auth}, nil
}

// handleAuthLoginPoll answers one poll for the CPA host.
func handleAuthLoginPoll(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthLoginPollRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	if strings.TrimSpace(request.State) == "" {
		return nil, abiboot.Errorf("invalid_request", "auth.login.poll 缺少 state")
	}
	return pollLogin(h, request.State, time.Now())
}
