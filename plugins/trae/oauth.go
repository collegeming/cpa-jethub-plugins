package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/oauthcb"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Interactive login: the loopback callback listener, the 17-parameter login URL,
// the callback parser and the ExchangeToken / GetUserInfo exchange. Ported from
// trae-oauth.ts:40-773 with trae.ts:388-520.

// loginSessions tracks the in-flight sign-ins keyed by their opaque state.
var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}

	// callbackMu guards the process-wide callback listener below, which outlives
	// every sign-in (see callbackListener).
	callbackMu     sync.Mutex
	callbackServer *oauthcb.Server
	callbackKey    string
)

// loginSession is one pending interactive sign-in.
//
// The browser callback is captured by the plugin's shared loopback listener
// (see callbackListener) and only *stored* here; the token exchange happens in
// the following auth.login.poll invocation.
type loginSession struct {
	mu sync.Mutex

	State     string
	MachineID string
	DeviceID  string
	Region    string
	LoginURL  string
	Port      int
	CreatedAt time.Time
	ExpiresAt time.Time

	status     pluginapi.AuthLoginStatus
	message    string
	credential *Credential
	finished   bool

	callbackQuery    url.Values
	callbackReceived bool

	// redirectURI is the callback URL this sign-in published, snapshotted at
	// start. The session deliberately holds no listener handle: the listener
	// belongs to the plugin, so a session can never close the port the browser
	// is about to dial.
	redirectURI string
}

// callbackOptions builds the shared listener's options from the settings.
//
// Persistent is the whole point of the shared listener: TRAE's portal redirects
// the browser only once the user finishes authorising, and by then the sign-in
// that produced the URL may already be gone — its TTL lapsed, a newer auth-url
// call superseded it, or the container restarted. A listener that carried the
// session's TTL would stop accepting at that moment and the browser would land
// on a closed port.
//
// TRAE's portal accepts a full `auth_callback_url` and echoes it verbatim
// (trae-oauth.ts:115, :508-535), so the port is ours to choose. 18080 is the
// vendor's documented preference, not a constraint — the source falls back to a
// random port on EADDRINUSE/EACCES.
//
// An explicit callback_port pins the listener, which is what a container
// deployment needs: the port must match the published mapping, so silently
// moving to a random one would hand the browser an unroutable URL. Left at 0 the
// historical behaviour stands — prefer 18080, otherwise any port >= it. That
// fallback is chosen once, on the first sign-in, and then reused for the process
// lifetime, because the listener is never rebuilt for a new session: the port the
// portal was told to call stays bound until the plugin shuts down.
//
// Either way a container deployment also sets callback_bind_host=0.0.0.0,
// because the container's 127.0.0.1 is not the browser's. That is the same
// choice the host's own callback forwarder makes.
func callbackOptions(cfg Config) oauthcb.Options {
	options := oauthcb.Options{
		Path:       CallbackPath,
		MinPort:    DefaultCallbackPort,
		BindHost:   cfg.CallbackBindHost,
		PublicHost: cfg.CallbackPublicHost,
		PublicPort: cfg.CallbackPublicPort,
		Persistent: true,
	}
	if cfg.CallbackPort > 0 {
		options.Port = cfg.CallbackPort
	} else {
		options.PreferredPort = DefaultCallbackPort
	}
	return options
}

// callbackListener returns the plugin's long-lived callback listener, building
// or rebuilding it only when the settings that shape it change.
//
// The listener must outlive individual sign-ins. The portal redirects the
// browser whenever the user gets round to authorising, which is routinely after
// the session that handed out the URL has ended; while a session owned the
// listener, that moment left the published port unbound and the browser showed
// ERR_CONNECTION_REFUSED for a callback that was still perfectly valid.
func callbackListener(options oauthcb.Options) (*oauthcb.Server, error) {
	key := callbackListenerKey(options)
	callbackMu.Lock()
	defer callbackMu.Unlock()
	if callbackServer != nil && key == callbackKey {
		return callbackServer, nil
	}
	// Close before rebinding: a changed key can keep the same pinned port (only
	// the public host moved, say), and binding a port the old listener still
	// holds would fail with "address already in use".
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
	// Exactly one dispatcher per listener; it returns when Wait reports the
	// listener closed.
	go dispatchCallbacks(server)
	return server, nil
}

// callbackListenerKey combines every option that changes the listener's bind or
// the URL the portal is told to call back.
func callbackListenerKey(options oauthcb.Options) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s\x00%d",
		options.Path, options.BindHost, options.Port, options.PreferredPort,
		options.PublicHost, options.PublicPort)
}

// closeCallbackListener releases the shared listener; the next sign-in binds a
// fresh one.
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

// dispatchCallbacks is the plugin's single callback pump: every captured
// callback is handed to the sessions that are pending at that moment.
//
// A persistent listener only stops Wait when it is closed — there is no TTL and
// the context is never cancelled — so an error here means this listener is gone
// and the loop must end rather than spin.
func dispatchCallbacks(server *oauthcb.Server) {
	for {
		result, errWait := server.Wait(context.Background())
		if errWait != nil {
			return
		}
		loginMu.Lock()
		sessions := make([]*loginSession, 0, len(loginSessions))
		for _, session := range loginSessions {
			sessions = append(sessions, session)
		}
		loginMu.Unlock()
		// Deliver outside the lock: each delivery takes the session's own lock
		// and may settle it, and holding loginMu would stall every panel poll.
		for _, session := range sessions {
			session.deliver(result)
		}
	}
}

// startLoginSession builds the login URL on top of the plugin's shared callback
// listener. The listener is started (or reused) **before** the URL is built,
// because the URL has to carry the port that is actually bound
// (trae-oauth.ts:683-685).
func startLoginSession(cfg Config) (*loginSession, error) {
	machineID, errMachine := newMachineID()
	if errMachine != nil {
		return nil, abiboot.Errorf("random_source", "生成 machine_id 失败: %v", errMachine)
	}
	deviceID, errDevice := newDeviceID()
	if errDevice != nil {
		return nil, abiboot.Errorf("random_source", "生成 device_id 失败: %v", errDevice)
	}
	state, errState := newState()
	if errState != nil {
		return nil, abiboot.Errorf("random_source", "生成登录 state 失败: %v", errState)
	}

	ttl := time.Duration(cfg.LoginTimeoutMS) * time.Millisecond
	if ttl <= 0 {
		ttl = DefaultLoginTimeoutMS * time.Millisecond
	}
	options := callbackOptions(cfg)

	callback, errListen := callbackListener(options)
	if errListen != nil {
		if options.Port > 0 {
			return nil, abiboot.Errorf("callback_listen",
				"TRAE 回调端口 %d 无法监听（%v）；请确认该端口未被其它程序占用", options.Port, errListen)
		}
		return nil, abiboot.Errorf("callback_listen",
			"TRAE 回调端口无法监听（%v）；端口可能已被其它程序占用，请释放后重试", errListen)
	}

	// The session carries its own deadline: the listener is persistent, so its
	// ExpiresAt is the zero time and cannot be what the panel polls against.
	now := time.Now()
	session := &loginSession{
		State:       state,
		MachineID:   machineID,
		DeviceID:    deviceID,
		Region:      cfg.Region,
		Port:        callback.Port(),
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
		status:      pluginapi.AuthLoginStatusPending,
		redirectURI: callback.RedirectURI(),
	}
	session.LoginURL = buildTraeLoginURL(productFor(cfg.Region), machineID, deviceID, session.RedirectURI())

	// Superseding used to share a critical section with the bind, because the port
	// was a resource the previous session had to release first; the shared
	// listener now holds the port, so it only settles the replaced attempt and the
	// panel stops polling a state that can never complete. Keeping that settle and
	// this registration together is what holds a pinned deployment to one pending
	// sign-in: otherwise a concurrent start could supersede the session it has
	// just created.
	loginMu.Lock()
	purgeExpiredLoginSessionsLocked()
	if options.Port > 0 {
		supersedePendingLoginSessionsLocked(supersededLoginMessage)
	}
	loginSessions[state] = session
	loginMu.Unlock()

	return session, nil
}

// RedirectURI is the loopback URL the portal must call back.
func (s *loginSession) RedirectURI() string { return s.redirectURI }

// deliver routes one captured callback to this session: a usable one is stored
// for the panel's poll, an unusable one settles the session with its reason.
func (s *loginSession) deliver(result oauthcb.Result) {
	s.mu.Lock()
	if s.finished {
		// The sign-in already reached a terminal state; a late callback must not
		// revive it.
		s.mu.Unlock()
		return
	}
	s.callbackQuery = result.Query
	s.callbackReceived = true
	parsed := parseTraeCallbackQuery(result.Query)
	s.mu.Unlock()
	if parsed.OK {
		// Kept for the panel's poll, which performs the token exchange.
		return
	}
	// Unusable callback: report it through the session and settle it so the user
	// sees a reason instead of a spinner.
	s.fail("TRAE 登录回调无效：" + parsed.Reason)
}

// takeCallback returns the captured callback once.
func (s *loginSession) takeCallback() (url.Values, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.callbackReceived || s.callbackQuery == nil {
		return nil, false
	}
	query := s.callbackQuery
	s.callbackQuery = nil
	s.callbackReceived = false
	return query, true
}

// expired reports whether the session outlived its TTL.
func (s *loginSession) expired() bool { return time.Now().After(s.ExpiresAt) }

// expire marks a session as failed. The callback listener is shared and stays
// bound until plugin shutdown, so the browser can still land on the port.
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

// lookupLoginSession finds a live session by its state value.
func lookupLoginSession(state string) (*loginSession, bool) {
	if strings.TrimSpace(state) == "" {
		return nil, false
	}
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
	delete(loginSessions, state)
	loginMu.Unlock()
}

// supersededLoginMessage is what a replaced sign-in reports. The panel may still
// be polling the older state, so it has to say the attempt was replaced rather
// than look like a silent stall.
const supersededLoginMessage = "该登录已被新的登录请求取代，请重新发起"

// supersedePendingLoginSessionsLocked settles every live session and forgets it.
// A pinned callback port carries one sign-in at a time, and the panel may still
// be polling the replaced state, so it has to be told the attempt is over rather
// than left pending forever. Callers hold loginMu.
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
			session.expire("登录会话已超时，请重新发起")
		}
	}
}

// shutdownLoginSessions drops every pending sign-in and closes the plugin's
// callback listener; called from plugin shutdown. This is the only place that
// closes it — no session lifetime may.
func shutdownLoginSessions() {
	loginMu.Lock()
	for state := range loginSessions {
		delete(loginSessions, state)
	}
	loginMu.Unlock()
	closeCallbackListener()
}

// buildTraeLoginURL builds the portal URL with the full 17-parameter set
// (trae-oauth.ts:99-127).
//
// Every parameter matters. `auth_callback_url` is the real name of the callback
// parameter (`callback_url` / `redirect_uri` are not recognised, and a wrong name
// leaves the page stuck on "authenticating"); `login_trace_id` is how the
// callback is correlated with this pending login (docs/agents/trae.md:290-309).
// The query is rendered in the reference order rather than sorted, so the URL
// stays byte-comparable with the Jet-Hub implementation.
func buildTraeLoginURL(product product, machineID, deviceID, callbackURL string) string {
	traceID := machineTraceID(machineID, deviceID)
	pairs := [][2]string{
		{"login_version", "1"},
		{"auth_from", "solo"},
		{"login_channel", "native_ide"},
		{"plugin_version", product.PluginVersion},
		{"auth_type", "local"},
		{"client_id", product.ClientID},
		{"redirect", "0"},
		{"login_trace_id", traceID},
		{"auth_callback_url", callbackURL},
		{"machine_id", machineID},
		{"device_id", deviceID},
		{"x_device_id", deviceID},
		{"x_machine_id", machineID},
		{"x_device_brand", "PC"},
		{"x_device_type", "PC"},
		{"x_os_version", "1.0"},
		{"x_app_version", product.IDEVersion},
		{"x_app_type", "stable"},
	}
	var builder strings.Builder
	builder.WriteString(product.ConsoleHost)
	builder.WriteString("/authorization?")
	for index, pair := range pairs {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(url.QueryEscape(pair[0]))
		builder.WriteByte('=')
		builder.WriteString(url.QueryEscape(pair[1]))
	}
	return builder.String()
}

// traeCallbackInfo is the credential material carried by the callback
// (trae-oauth.ts:134-162).
type traeCallbackInfo struct {
	RefreshToken string
	AccessToken  string
	UID          string
	Nickname     string
	EnterpriseID string
	AuthCode     string
}

// traeCallbackResult is the detailed parse result; the reason is what the user
// sees, so it has to name the real cause.
type traeCallbackResult struct {
	OK           bool
	Info         traeCallbackInfo
	Reason       string
	AuthCodeFlow bool
}

// parseTraeCallbackRaw parses a callback URL (absolute or relative).
func parseTraeCallbackRaw(rawURL string) traeCallbackResult {
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		return traeCallbackResult{Reason: "回调 URL 无法解析"}
	}
	if parsed.Host == "" {
		// Relative form: `/authorize?...`.
		parsed, errParse = url.Parse("http://127.0.0.1" + rawURL)
		if errParse != nil {
			return traeCallbackResult{Reason: "回调 URL 无法解析"}
		}
	}
	return parseTraeCallbackQuery(parsed.Query())
}

// parseTraeCallbackQuery parses the callback query parameters
// (trae-oauth.ts:273-345).
//
// TRAE has two parallel flows: the legacy one hands back `refreshToken` (and a
// `userJwt`), the newer PKCE one hands back `code` / `authCodeInfo`. Both are
// parsed; a PKCE-only callback is reported as "unsupported flow" rather than as a
// malformed callback, because mislabelling it points the user at the wrong fix
// (docs/agents/trae.md:329-349).
func parseTraeCallbackQuery(query url.Values) traeCallbackResult {
	userInfo, _ := parseJSONParam(query.Get("userInfo"))
	userJwt, _ := parseJSONParam(query.Get("userJwt"))

	refreshToken := query.Get("refreshToken")
	uid := jsonStringField(userInfo, "UserID")
	nicknameRaw := jsonStringField(userInfo, "ScreenName")
	// The callback's field name is TenantID, not EnterpriseID.
	enterpriseID := jsonStringField(userInfo, "TenantID")

	jwtToken := jsonStringField(userJwt, "Token")
	jwtRefresh := jsonStringField(userJwt, "RefreshToken")
	if refreshToken == "" {
		refreshToken = jwtRefresh
	}

	authCodeInfo, _ := parseJSONParam(query.Get("authCodeInfo"))
	authCode := ""
	for _, candidate := range []string{
		query.Get("code"),
		query.Get("authCode"),
		jsonStringField(authCodeInfo, "code"),
		jsonStringField(authCodeInfo, "authCode"),
		query.Get("authCodeInfo"),
	} {
		if strings.TrimSpace(candidate) != "" {
			authCode = strings.TrimSpace(candidate)
			break
		}
	}

	info := traeCallbackInfo{
		RefreshToken: refreshToken,
		UID:          uid,
		Nickname:     fixNicknameMojibake(nicknameRaw, uid),
		EnterpriseID: enterpriseID,
		AuthCode:     authCode,
	}
	if refreshToken == "" {
		// `userJwt.Token` is only a fallback when there is no refresh token.
		info.AccessToken = jwtToken
	}

	if info.RefreshToken == "" && info.AccessToken == "" {
		if authCode != "" {
			return traeCallbackResult{
				AuthCodeFlow: true,
				Reason: "上游返回了 PKCE 授权码（code/authCodeInfo），本实现暂不支持该流程；" +
					"请确认 TRAE 授权页是否已切换到新流程",
			}
		}
		return traeCallbackResult{Reason: "回调未携带 refreshToken / userJwt.Token / code"}
	}
	return traeCallbackResult{OK: true, Info: info}
}

// parseJSONParam parses a URL-encoded JSON parameter (trae-oauth.ts:182-198).
// Chinese nicknames arrive double-encoded, so one extra unescape is attempted.
func parseJSONParam(raw string) (map[string]any, bool) {
	if raw == "" {
		return nil, false
	}
	candidates := []string{raw}
	if unescaped, errUnescape := url.QueryUnescape(raw); errUnescape == nil && unescaped != raw {
		candidates = append(candidates, unescaped)
	}
	for _, candidate := range candidates {
		var parsed any
		if err := json.Unmarshal([]byte(candidate), &parsed); err != nil {
			continue
		}
		if object, ok := asMap(parsed); ok {
			return object, true
		}
	}
	return nil, false
}

// jsonStringField reads a string field from an optional object, accepting
// numbers as their decimal rendering (trae-oauth.ts:201-207).
func jsonStringField(source map[string]any, key string) string {
	if source == nil {
		return ""
	}
	return readStringField(source, key)
}

// fixNicknameMojibake repairs the double-encoded nickname the callback carries
// (trae-oauth.ts:216-232, matching login.sh's `fix_mojibake`). The reference is
// read back as latin-1 and re-interpreted as UTF-8; when that fails and the text
// contains no CJK at all, a readable placeholder is used instead of writing
// mojibake into the credential.
func fixNicknameMojibake(raw, uid string) string {
	if raw == "" {
		return raw
	}
	bytes := make([]byte, 0, len(raw))
	for _, r := range raw {
		bytes = append(bytes, byte(r&0xFF))
	}
	fixed := string(bytes)
	if fixed != "" && utf8.ValidString(fixed) && !strings.ContainsRune(fixed, '\uFFFD') {
		clean := true
		for _, r := range fixed {
			if r < 32 {
				clean = false
				break
			}
		}
		if clean {
			return fixed
		}
	}
	if !containsCJK(raw) {
		suffix := uid
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		return "用户" + suffix
	}
	return raw
}

// containsCJK reports whether the text holds at least one CJK ideograph.
func containsCJK(value string) bool {
	for _, r := range value {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// ── ExchangeToken / GetUserInfo (trae-oauth.ts:365-440, trae.ts:388-446) ──

// traeExchangeResult is the ExchangeToken `Result` payload (trae.ts:391-400).
type traeExchangeResult struct {
	AccessToken         string
	RefreshToken        string
	TokenExpireAt       int64
	TokenExpireDuration int64
	RefreshExpireAt     int64
}

// traeUserInfoResult is the GetUserInfo `Result` payload (trae.ts:424-428).
type traeUserInfoResult struct {
	UID          string
	ScreenName   string
	EnterpriseID string
}

// parseTraeExchangeResponse parses ExchangeToken (trae.ts:407-421).
func parseTraeExchangeResponse(body []byte) (traeExchangeResult, bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return traeExchangeResult{}, false
	}
	result, ok := asMap(root["Result"])
	if !ok {
		result, ok = asMap(root["result"])
	}
	if !ok {
		return traeExchangeResult{}, false
	}
	accessToken := readStringField(result, "Token", "token", "accessToken")
	if accessToken == "" {
		return traeExchangeResult{}, false
	}
	expireAt, _ := readNumberField(result, "TokenExpireAt", "tokenExpireAt")
	expireDuration, _ := readNumberField(result, "TokenExpireDuration", "tokenExpireDuration")
	refreshExpireAt, _ := readNumberField(result, "RefreshExpireAt", "refreshExpireAt")
	return traeExchangeResult{
		AccessToken:         accessToken,
		RefreshToken:        readStringField(result, "RefreshToken", "refreshToken"),
		TokenExpireAt:       int64(expireAt),
		TokenExpireDuration: int64(expireDuration),
		RefreshExpireAt:     int64(refreshExpireAt),
	}, true
}

// parseTraeUserInfoResponse parses GetUserInfo (trae.ts:435-446).
func parseTraeUserInfoResponse(body []byte) (traeUserInfoResult, bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return traeUserInfoResult{}, false
	}
	result, ok := asMap(root["Result"])
	if !ok {
		result, ok = asMap(root["result"])
	}
	if !ok {
		return traeUserInfoResult{}, false
	}
	uid := readStringField(result, "UserID", "userId", "uid")
	if uid == "" {
		return traeUserInfoResult{}, false
	}
	screenName := readStringField(result, "ScreenName", "screenName")
	if screenName == "" {
		screenName = uid
	}
	return traeUserInfoResult{
		UID:          uid,
		ScreenName:   screenName,
		EnterpriseID: readStringField(result, "EnterpriseID", "enterpriseId"),
	}, true
}

// requestExchangeToken performs one ExchangeToken call. A nil result with a nil
// error means "no token in the response" — the caller decides whether that is a
// terminal failure. A non-nil error is a transport failure.
func requestExchangeToken(h *abiboot.Host, host, refreshToken string, product product, cfg Config) (*traeExchangeResult, int, string, error) {
	body, errMarshal := json.Marshal(map[string]any{
		"ClientID":     product.ClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	})
	if errMarshal != nil {
		return nil, 0, "", errMarshal
	}
	response, errDo := hostDo(h, http.MethodPost, host+ExchangePath, oauthHeaders(product), body, cfg.RequestTimeoutMS)
	if errDo != nil {
		return nil, 0, "", errDo
	}
	text := string(response.Body)
	exchange, ok := parseTraeExchangeResponse(response.Body)
	if !ok {
		return nil, response.StatusCode, text, nil
	}
	return &exchange, response.StatusCode, text, nil
}

// exchangeTraeCallback turns callback material into a credential
// (trae-oauth.ts:365-440).
//
// Branch 1: a refresh token exists, so ExchangeToken mints the access token (and
// rotates the refresh token). Branch 2: only `userJwt.Token` exists, which is
// used directly. GetUserInfo then fills uid/nickname/enterprise, and its failure
// must not abort the login.
func exchangeTraeCallback(h *abiboot.Host, callback traeCallbackInfo, session *loginSession, cfg Config) (*Credential, error) {
	product := productFor(session.Region)
	exchange := traeExchangeResult{}
	if callback.RefreshToken != "" {
		result, status, body, errExchange := requestExchangeToken(h, product.OAuthHost, callback.RefreshToken, product, cfg)
		if errExchange != nil {
			return nil, abiboot.RetryableError("transport", "TRAE ExchangeToken 失败：%v", errExchange)
		}
		if result == nil || result.AccessToken == "" {
			return nil, abiboot.Errorf("exchange_failed", "TRAE ExchangeToken 失败（HTTP %d）：%s",
				status, truncate(errorDetail(body), 200))
		}
		exchange = *result
	} else {
		exchange = traeExchangeResult{AccessToken: callback.AccessToken}
	}

	userInfo := traeUserInfoResult{
		UID:          callback.UID,
		ScreenName:   callback.Nickname,
		EnterpriseID: callback.EnterpriseID,
	}
	if exchange.AccessToken != "" {
		headers := oauthHeaders(product)
		headers.Set("X-Cloudide-Token", exchange.AccessToken)
		requestBody, errMarshal := json.Marshal(map[string]any{
			"ReqSource":  "IDE",
			"IDEVersion": product.IDEVersion,
		})
		if errMarshal == nil {
			if response, errDo := hostDo(h, http.MethodPost, product.OAuthHost+UserInfoPath, headers, requestBody, cfg.RequestTimeoutMS); errDo == nil {
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					if fetched, ok := parseTraeUserInfoResponse(response.Body); ok {
						userInfo.UID = fetched.UID
						if fetched.ScreenName != "" {
							userInfo.ScreenName = fetched.ScreenName
						}
						if fetched.EnterpriseID != "" {
							userInfo.EnterpriseID = fetched.EnterpriseID
						}
					}
				}
			}
		}
	}

	if userInfo.UID == "" {
		return nil, abiboot.Errorf("missing_uid", "TRAE 未能确定 uid（回调 userInfo 与 GetUserInfo 均为空）")
	}
	if exchange.AccessToken == "" {
		return nil, abiboot.Errorf("missing_token", "TRAE 换 token 后没有 accessToken")
	}
	return buildTraeCredential(exchange, userInfo, session, product, time.Now().UnixMilli()), nil
}

// buildTraeCredential assembles the stored credential (trae.ts:458-489).
//
// `expires_at` precedence: an absolute `tokenExpireAt` (milliseconds when above
// 1e12, otherwise seconds), then the relative `tokenExpireDuration`, then the JWT
// `exp` claim; an empty string when none of them is available.
func buildTraeCredential(exchange traeExchangeResult, userInfo traeUserInfoResult, session *loginSession, product product, nowMS int64) *Credential {
	return &Credential{
		Type:         ProviderKey,
		AccessToken:  exchange.AccessToken,
		RefreshToken: exchange.RefreshToken,
		ExpiresAt:    ExpiresAtValue(expiresAtString(exchange, nowMS)),
		UID:          userInfo.UID,
		Nickname:     userInfo.ScreenName,
		MachineID:    session.MachineID,
		DeviceID:     session.DeviceID,
		APIHost:      product.OAuthHost,
		Region:       product.ID,
		EnterpriseID: userInfo.EnterpriseID,
	}
}

// applyTraeRefresh folds an ExchangeToken result into an existing credential
// (trae.ts:497-520). The device fingerprints and identity fields are preserved
// verbatim: `machine_id` must never be regenerated on refresh
// (docs/agents/trae.md:35-37).
func applyTraeRefresh(previous *Credential, exchange traeExchangeResult, nowMS int64) *Credential {
	refreshed := *previous
	refreshed.AccessToken = exchange.AccessToken
	// The refresh token rotates; only an empty answer keeps the old one.
	if exchange.RefreshToken != "" {
		refreshed.RefreshToken = exchange.RefreshToken
	}
	refreshed.ExpiresAt = ExpiresAtValue(expiresAtString(exchange, nowMS))
	return &refreshed
}

// expiresAtString renders the credential expiry string.
func expiresAtString(exchange traeExchangeResult, nowMS int64) string {
	switch {
	case exchange.TokenExpireAt > 1_000_000_000_000:
		return fmt.Sprintf("%d", exchange.TokenExpireAt)
	case exchange.TokenExpireAt > 0:
		return fmt.Sprintf("%d", exchange.TokenExpireAt*1000)
	case exchange.TokenExpireDuration > 0:
		return fmt.Sprintf("%d", nowMS+exchange.TokenExpireDuration*1000)
	default:
		if exp, ok := jwtExpiresAtMS(exchange.AccessToken); ok {
			return fmt.Sprintf("%d", exp)
		}
		return ""
	}
}
