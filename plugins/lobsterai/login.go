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
//  1. create a login session (random uuid + firstKeyfrom) on the plugin's
//     long-lived loopback callback listener (see callbackListener);
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
	// Port is the bound port of the shared callback listener, i.e. the port
	// this sign-in's redirect_uri names.
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

	// redirectURI is the address the browser was told to call for this sign-in.
	// It is a value copied from the shared listener, not ownership of it: the
	// listener outlives every session (see callbackListener).
	redirectURI string
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// The callback listener is process-wide, not per sign-in. The portal's redirect
// arrives whenever the user finishes authorising, which can be after the session
// that started the flow has expired, been superseded, or been lost to a
// container restart. A container also publishes the callback port in advance, so
// the browser cannot follow the listener to another port: binding per session
// leaves the published port closed exactly when the browser arrives
// (ERR_CONNECTION_REFUSED). Only plugin shutdown closes this listener.
var (
	callbackMu     sync.Mutex
	callbackServer *oauthcb.Server
	callbackKey    string
)

// callbackOptions is the listener configuration the live settings imply.
func callbackOptions() oauthcb.Options {
	cfg := settings()
	return oauthcb.Options{
		Path:     CallbackPath,
		MinPort:  MinCallbackPort,
		Port:     cfg.CallbackPort,
		BindHost: cfg.CallbackBindHost,
		// PublicHost/PublicPort are the browser-visible address; they differ
		// from the bind address in a container (bind 0.0.0.0, dial 127.0.0.1).
		PublicHost:  cfg.CallbackPublicHost,
		PublicPort:  cfg.CallbackPublicPort,
		SuccessHTML: callbackSuccessHTML,
		// Persistent: Wait never times out, so the port stays bound between
		// sign-ins and the session's own TTL (LoginTimeout) decides when the
		// panel stops polling.
		Persistent: true,
	}
}

// callbackListenerKey names every option that shapes the listener's behaviour.
// Settings equal to the cached key keep the bound listener, which is what lets a
// retry on a pinned port succeed instead of racing the previous bind.
func callbackListenerKey(options oauthcb.Options) string {
	// NUL cannot occur in a host name, a query-free path or the success page,
	// so no field value can forge a key that matches another configuration.
	return fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%d\x00%s",
		options.Path, options.BindHost, options.Port, options.PublicHost, options.PublicPort, options.SuccessHTML)
}

// callbackListener returns the plugin's long-lived callback listener, building
// or rebuilding it only when the settings that shape it change.
func callbackListener(options oauthcb.Options) (*oauthcb.Server, error) {
	key := callbackListenerKey(options)

	callbackMu.Lock()
	defer callbackMu.Unlock()

	if callbackServer != nil && callbackKey == key {
		return callbackServer, nil
	}

	// A reconfigure moved the bind host, the port or the success page, so the
	// running listener answers the wrong address. Release it before rebinding:
	// a pinned port cannot be taken while the previous listener still holds it.
	// A failed bind leaves the cache empty, so the next sign-in retries instead
	// of handing out a closed server.
	if callbackServer != nil {
		_ = callbackServer.Close()
		callbackServer = nil
		callbackKey = ""
	}
	server, errStart := oauthcb.Start(options)
	if errStart != nil {
		return nil, errStart
	}
	callbackServer = server
	callbackKey = key
	// Exactly one dispatcher per listener: it is the only reader of the
	// listener's callback channel, and it ends with the listener.
	go dispatchCallbacks(server)
	return server, nil
}

// dispatchCallbacks routes every callback the shared listener captures to the
// pending sessions. The listener cannot know which sign-in a callback belongs
// to, so each session's own state match decides whether it is the recipient.
func dispatchCallbacks(server *oauthcb.Server) {
	for {
		result, errWait := server.Wait(context.Background())
		if errWait != nil {
			// A persistent listener only returns on Close — plugin shutdown or
			// a settings rebuild — so there is nothing left to route.
			return
		}

		loginMu.Lock()
		pending := make([]*loginSession, 0, len(loginSessions))
		for _, session := range loginSessions {
			pending = append(pending, session)
		}
		loginMu.Unlock()

		for _, session := range pending {
			// Outside loginMu: deliver takes the session lock, and a session
			// settling concurrently must not hold up the registry.
			session.deliver(result)
		}
	}
}

// closeCallbackListener releases the shared listener. Plugin shutdown is the
// only caller: a session that closed it would leave the published port
// unreachable for the browser redirect that follows.
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

// startLoginSession registers a sign-in under a fresh random state value on the
// plugin's shared callback listener.
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
		// The listener does not expire, so the session carries its own deadline:
		// it is what stops the panel from polling an abandoned sign-in.
		ExpiresAt: now.Add(LoginTimeout),
		status:    pluginapi.AuthLoginStatusPending,
	}

	// LobsterAI takes a full `redirect_uri` and echoes the `state` we generate,
	// so the port is ours to choose (lobsterai-oauth.ts:95-114). That still does
	// not make an ephemeral port work in a container: it cannot be published in
	// advance, and the container's 127.0.0.1 is not the browser's. Such a
	// deployment pins the port and binds 0.0.0.0, which is what the host's own
	// callback forwarder does. Reusing the shared listener is also what keeps
	// that pinned port bound between sign-ins.
	callback, errListen := callbackListener(callbackOptions())
	if errListen != nil {
		if cfg := settings(); cfg.CallbackPort > 0 {
			return nil, abiboot.Errorf("callback_listen",
				"LobsterAI 回调端口 %d 无法监听（%v）；请确认该端口未被其它程序占用", cfg.CallbackPort, errListen)
		}
		return nil, abiboot.Errorf("callback_listen", "start LobsterAI callback listener: %v", errListen)
	}
	session.Port = callback.Port()
	session.redirectURI = callback.RedirectURI()

	// Superseding no longer has to protect a bind — the listener already holds
	// the port for the process lifetime — but a pinned deployment still carries
	// one sign-in at a time, and the replaced attempt has to settle so the panel
	// stops polling a state that can never complete. Settling it in the same
	// critical section as the registration keeps a concurrent start from
	// superseding the session it just created.
	loginMu.Lock()
	purgeExpiredLoginSessionsLocked(now)
	if settings().CallbackPort > 0 {
		supersedePendingLoginSessionsLocked(supersededLoginMessage)
	}
	loginSessions[state] = session
	loginMu.Unlock()

	return session, nil
}

// deliver routes one browser callback to this session. A callback whose state
// does not match belongs to another session (or to a stale page) and is
// discarded, so the shared listener keeps routing later callbacks here. The
// reference answers such a request with HTTP 400; the shared oauthcb listener
// cannot answer per-request, so only the browser page differs (documented
// deviation).
func (s *loginSession) deliver(result oauthcb.Result) {
	if strings.TrimSpace(result.Query.Get("state")) != s.State {
		return
	}
	if strings.TrimSpace(result.Code) == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.Callback = result
	s.AuthCode = result.Code
}

// RedirectURI is the loopback redirect the portal must call back.
func (s *loginSession) RedirectURI() string {
	if s.redirectURI != "" {
		return s.redirectURI
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

// expire marks a session as failed. The shared listener stays bound: a browser
// redirect may still be on its way.
func (s *loginSession) expire(message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusError
	s.message = message
	s.mu.Unlock()
}

// finish records a successful credential.
func (s *loginSession) finish(credential *Credential, message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusSuccess
	s.message = message
	s.credential = credential
	s.mu.Unlock()
}

// fail records a terminal failure.
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

// supersededLoginMessage is what a replaced sign-in reports. The panel may still
// be polling the older state, so it has to say the attempt was replaced rather
// than look like a silent stall.
const supersededLoginMessage = "该登录已被新的登录请求取代，请重新发起"

// supersedePendingLoginSessionsLocked settles every live session and forgets it.
// A pinned deployment carries one sign-in at a time — the panel's 重试 replaces
// the pending attempt rather than adding a second one — so the replaced session
// has to report that it lost, or the panel keeps polling a state that can never
// complete. Releasing the port is no longer part of this: the shared listener
// holds it for the process lifetime. Callers must hold loginMu.
func supersedePendingLoginSessionsLocked(message string) {
	for state, session := range loginSessions {
		delete(loginSessions, state)
		session.expire(message)
	}
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

// shutdownLoginSessions drops every pending session and releases the shared
// callback listener. Only plugin shutdown may do the latter: as long as the
// plugin runs, a sign-in that is not in flight must not take the published
// callback port down with it.
func shutdownLoginSessions() {
	loginMu.Lock()
	for state := range loginSessions {
		delete(loginSessions, state)
	}
	loginMu.Unlock()
	closeCallbackListener()
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
