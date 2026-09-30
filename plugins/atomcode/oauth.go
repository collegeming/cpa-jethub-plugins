package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The AtomGit sign-in is a browser flow brokered by `acs.atomgit.com`:
//
//	GET /auth/login?provider=atomgit   -> {login_url, state}      (oauth.rs:596)
//	  ... the user opens login_url and authorises in the browser ...
//	GET /auth/check?state=<state>      -> {valid: bool}           (oauth.rs:453)
//	GET /auth/token?state=<state>      -> the token bundle        (oauth.rs:517)
//
// The broker terminates the OAuth redirect itself, so this plugin needs no
// loopback listener and no fixed callback port: the callback goes to
// `acs.atomgit.com/callback`, never to the machine running CPA. That is the one
// structural difference from the sibling providers that pin a callback port.
//
// The flow is split across two host calls (`auth.login.start` / `auth.login.poll`)
// because the browser gesture has to happen while the user agent still considers
// it user-initiated; `start` therefore never blocks and never waits for the user.

// brokerLoginResponse is the `/auth/login` reply (`oauth.rs:181-184`).
type brokerLoginResponse struct {
	LoginURL string `json:"login_url"`
	State    string `json:"state"`
}

// brokerCheckResponse is the `/auth/check` reply (`oauth.rs:186-188`).
type brokerCheckResponse struct {
	Valid bool `json:"valid"`
}

// brokerTokenResponse is the `/auth/token` and `/oauth/refresh` reply. Both
// endpoints return the same envelope (`oauth.rs:199-207` and the `BrokerResponse`
// type at oauth.rs:929-936), which is why one struct serves both.
type brokerTokenResponse struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int64     `json:"expires_in"`
	RefreshToken string    `json:"refresh_token"`
	User         *UserInfo `json:"user"`
}

// brokerUser is the `user` member as `/auth/check` reports it. It is the same
// identity `/auth/token` returns, minus the token.
type brokerUser struct {
	UserInfo
	SID string `json:"sid"`
}

// loginSession is one in-flight sign-in.
type loginSession struct {
	mu sync.Mutex

	// State is the broker's correlation id. It is both the key of the registry
	// and the credential the poll carries back to the broker.
	State string
	// LoginURL is the AtomGit authorisation URL the user must open.
	LoginURL string
	// CreatedAt and ExpiresAt bound the session's life. The broker keeps its own
	// window; this one only stops the plugin accumulating dead sessions.
	CreatedAt time.Time
	ExpiresAt time.Time

	status     pluginapi.AuthLoginStatus
	message    string
	credential *Credential
	finished   bool
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// startLoginSession opens a broker sign-in and registers it.
func startLoginSession(h *abiboot.Host, cfg Config) (*loginSession, error) {
	response, errBroker := brokerStartLogin(h, cfg)
	if errBroker != nil {
		return nil, errBroker
	}
	if strings.TrimSpace(response.LoginURL) == "" || strings.TrimSpace(response.State) == "" {
		return nil, abiboot.Errorf("broker_protocol", "AtomGit 登录代理未返回 login_url/state")
	}

	ttl := time.Duration(cfg.LoginTimeoutMS) * time.Millisecond
	if ttl <= 0 {
		ttl = time.Duration(LoginTimeoutMS) * time.Millisecond
	}
	session := &loginSession{
		State:     response.State,
		LoginURL:  response.LoginURL,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
		status:    pluginapi.AuthLoginStatusPending,
		message:   "等待浏览器完成登录",
	}

	loginMu.Lock()
	purgeExpiredLoginSessionsLocked()
	// One live attempt at a time: the panel may still poll an older state, and a
	// silently superseded session looks like a stall. Same rule as the sibling
	// providers that pin a resource per login.
	supersedePendingLoginSessionsLocked(supersededLoginMessage)
	loginSessions[session.State] = session
	loginMu.Unlock()
	return session, nil
}

// supersededLoginMessage is what a replaced sign-in reports.
const supersededLoginMessage = "该登录已被新的登录请求取代，请重新发起"

// lookupLoginSession resolves a session by state, dropping it if it has expired.
func lookupLoginSession(state string) (*loginSession, bool) {
	trimmed := strings.TrimSpace(state)
	if trimmed == "" {
		return nil, false
	}
	loginMu.Lock()
	session, ok := loginSessions[trimmed]
	if ok && session.expired() && !session.finishedForRead() {
		delete(loginSessions, trimmed)
		ok = false
		session.expire("登录会话已超时，请重新发起")
	}
	loginMu.Unlock()
	return session, ok
}

// forgetLoginSession drops a completed session from the registry.
func forgetLoginSession(state string) {
	loginMu.Lock()
	delete(loginSessions, state)
	loginMu.Unlock()
}

// shutdownLoginSessions drops every session at plugin shutdown.
func shutdownLoginSessions() {
	loginMu.Lock()
	for state := range loginSessions {
		delete(loginSessions, state)
	}
	loginMu.Unlock()
}

// supersedePendingLoginSessionsLocked settles every live session and forgets it.
// Callers hold loginMu.
func supersedePendingLoginSessionsLocked(message string) {
	for state, session := range loginSessions {
		delete(loginSessions, state)
		session.expire(message)
	}
}

// purgeExpiredLoginSessionsLocked drops expired entries. Callers hold loginMu.
func purgeExpiredLoginSessionsLocked() {
	for state, session := range loginSessions {
		if session.expired() {
			delete(loginSessions, state)
		}
	}
}

// expired reports whether the session's window has closed.
func (s *loginSession) expired() bool {
	return time.Now().After(s.ExpiresAt)
}

// finishedForRead reports whether the session already settled. It takes the lock
// because the registry consults it while another goroutine may be finishing.
func (s *loginSession) finishedForRead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// finish settles a session with a credential.
func (s *loginSession) finish(credential *Credential, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = pluginapi.AuthLoginStatusSuccess
	s.message = message
	s.credential = credential
	s.finished = true
}

// fail settles a session with an error.
func (s *loginSession) fail(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = pluginapi.AuthLoginStatusError
	s.message = message
	s.finished = true
}

// expire settles a session that ran out of time.
func (s *loginSession) expire(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.status = pluginapi.AuthLoginStatusError
	s.message = message
	s.finished = true
}

// snapshot reports the session's current state.
func (s *loginSession) snapshot() (pluginapi.AuthLoginStatus, string, *Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.message, s.credential, s.finished
}

// brokerStartLogin calls `GET /auth/login?provider=atomgit` (oauth.rs:596-640).
func brokerStartLogin(h *abiboot.Host, cfg Config) (*brokerLoginResponse, error) {
	endpoint := cfg.brokerLoginURL() + "?" + url.Values{"provider": {BrokerProvider}}.Encode()
	response, errDo := hostDo(h, http.MethodGet, endpoint, brokerHeaders(), nil)
	if errDo != nil {
		return nil, abiboot.RetryableError("transport", "AtomGit 登录代理不可达：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.HTTPError("broker_login_failed", http.StatusBadGateway,
			"AtomGit 登录代理返回 HTTP %d：%s", response.StatusCode, truncate(string(response.Body), 200))
	}
	var decoded brokerLoginResponse
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		return nil, abiboot.Errorf("broker_protocol", "解析 /auth/login 响应失败: %v", errUnmarshal)
	}
	return &decoded, nil
}

// brokerCheck calls `GET /auth/check?state=<state>` (oauth.rs:399-411).
//
// A transport failure is returned as an error but one poll's failure is never
// terminal: the caller keeps polling, exactly as the reference's
// `spawn_poller` does (oauth.rs:494-515). "Not yet authorised" is `false, nil`,
// not an error.
func brokerCheck(h *abiboot.Host, cfg Config, state string) (bool, error) {
	endpoint := cfg.brokerCheckURL() + "?" + url.Values{"state": {state}}.Encode()
	response, errDo := hostDo(h, http.MethodGet, endpoint, brokerHeaders(), nil)
	if errDo != nil {
		return false, abiboot.RetryableError("transport", "查询 AtomGit 登录状态失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, abiboot.HTTPError("broker_check_failed", http.StatusBadGateway,
			"AtomGit 登录代理返回 HTTP %d", response.StatusCode)
	}
	var decoded brokerCheckResponse
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		return false, abiboot.Errorf("broker_protocol", "解析 /auth/check 响应失败: %v", errUnmarshal)
	}
	return decoded.Valid, nil
}

// brokerExchange calls `GET /auth/token?state=<state>` and converts the reply
// into a credential (oauth.rs:517-575).
func brokerExchange(h *abiboot.Host, cfg Config, state string) (*Credential, error) {
	endpoint := cfg.brokerTokenURL() + "?" + url.Values{"state": {state}}.Encode()
	response, errDo := hostDo(h, http.MethodGet, endpoint, brokerHeaders(), nil)
	if errDo != nil {
		return nil, abiboot.RetryableError("transport", "换取 AtomGit 令牌失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.HTTPError("broker_token_failed", http.StatusBadGateway,
			"AtomGit 登录代理返回 HTTP %d：%s", response.StatusCode, truncate(string(response.Body), 200))
	}
	var decoded brokerTokenResponse
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		return nil, abiboot.Errorf("broker_protocol", "解析 /auth/token 响应失败: %v", errUnmarshal)
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		// A 2xx with no token is a terminal protocol fault, not a retry: the
		// state has been consumed and polling again cannot produce a token.
		return nil, abiboot.Errorf("broker_protocol", "AtomGit 登录代理未返回 access_token")
	}
	return credentialFromBroker(decoded, time.Now()), nil
}

// brokerRefresh calls `POST /oauth/refresh` (oauth.rs:901-975).
//
// ⚠️ A successful refresh ROTATES the refresh token and immediately invalidates
// the access token it replaces (measured 2026-10-01: the superseded token answers
// `401 {"message":"401 Unauthorized"}`). The caller must persist the returned
// pair; discarding the new refresh token costs the user a full re-login.
func brokerRefresh(h *abiboot.Host, cfg Config, refreshToken string) (*brokerTokenResponse, error) {
	body, errMarshal := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_request", "序列化续期请求失败: %v", errMarshal)
	}
	headers := brokerHeaders()
	headers.Set("Content-Type", "application/json")
	response, errDo := hostDo(h, http.MethodPost, cfg.brokerRefreshURL(), headers, body)
	if errDo != nil {
		// The request never reached the broker, so re-sending cannot rotate twice.
		return nil, abiboot.RetryableError("transport", "AtomGit 续期请求失败：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized,
			"AtomGit refresh_token 已失效，请重新登录")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.HTTPError(httpErrorCode(response.StatusCode), response.StatusCode,
			"AtomGit 续期失败（HTTP %d）：%s", response.StatusCode, truncate(string(response.Body), 200))
	}
	var decoded brokerTokenResponse
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		// Fully-received but unparseable: terminal, so a deterministic bad
		// response cannot loop (oauth.rs:944-951).
		return nil, abiboot.Errorf("broker_protocol", "解析 AtomGit 续期响应失败: %v", errUnmarshal)
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		return nil, abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized,
			"AtomGit 续期响应没有 access_token，请重新登录")
	}
	return &decoded, nil
}

// credentialFromBroker projects a broker reply onto the stored credential shape.
//
// `created_at` is stamped here rather than derived: the reference does the same
// (oauth.rs:535-541) because the opaque token carries no `exp` claim to read.
func credentialFromBroker(response brokerTokenResponse, now time.Time) *Credential {
	user := UserInfo{}
	if response.User != nil {
		user = *response.User
	}
	return &Credential{
		Type:         ProviderKey,
		AccessToken:  response.AccessToken,
		RefreshToken: response.RefreshToken,
		TokenType:    response.TokenType,
		ExpiresIn:    response.ExpiresIn,
		CreatedAt:    now.Unix(),
		User:         user,
	}
}

// applyRefresh folds a refresh reply into an existing credential.
//
// The broker may omit `refresh_token`/`expires_in`/`user`; each absent field
// keeps the previous value (`oauth.rs:952-972`). Keeping the old refresh token on
// an omission is what stops a stripped-down reply from making the account
// unrefreshable.
func applyRefresh(existing *Credential, response brokerTokenResponse, now time.Time) *Credential {
	refreshed := *existing
	refreshed.Type = ProviderKey
	refreshed.AccessToken = response.AccessToken
	if strings.TrimSpace(response.RefreshToken) != "" {
		refreshed.RefreshToken = response.RefreshToken
	}
	if strings.TrimSpace(response.TokenType) != "" {
		refreshed.TokenType = response.TokenType
	}
	if response.ExpiresIn > 0 {
		refreshed.ExpiresIn = response.ExpiresIn
	}
	if response.User != nil {
		refreshed.User = *response.User
	}
	refreshed.CreatedAt = now.Unix()
	return &refreshed
}

// brokerHeaders is the header set every broker call carries.
//
// The reference sends its own client User-Agent here (`oauth.rs:63-67`), but this
// adapter identifies as itself on every request. The broker does not require the
// vendor fingerprint, and reusing it would be one careless copy-paste away from
// arming the gateway's signature gate (see AdapterUserAgent).
func brokerHeaders() http.Header {
	return http.Header{
		"Accept":     []string{"application/json"},
		"User-Agent": []string{AdapterUserAgent},
	}
}

// authHeaders is the header set every CodingPlan REST call carries.
func authHeaders(credential *Credential) http.Header {
	headers := brokerHeaders()
	if credential != nil {
		headers.Set("Authorization", "Bearer "+credential.AccessToken)
	}
	return headers
}

// hostDo performs one buffered request through the host transport.
//
// The timeout parameter the sibling plugins carry is omitted: the ABI host
// callback takes no context, so the request lifetime is governed by the host's
// own transport timeouts.
func hostDo(h *abiboot.Host, method, url string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
	if h == nil {
		return nil, abiboot.Errorf("host_unavailable", "插件未获得宿主回调上下文")
	}
	return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: url, Headers: headers, Body: body})
}
