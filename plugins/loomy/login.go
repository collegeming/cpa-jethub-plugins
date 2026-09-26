package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// SMS login, ported from `src/loomy-oauth.ts:131-180`, and the WeChat QR login
// of `src/loomy-wechat.ts` + `src/loomy-oauth.ts:182-318`.
//
// The TypeScript plugin's RPC surface is `login.sendSms` + `login.submitSms`,
// i.e. the DSH frontend collects the phone and the code and calls the plugin
// twice. The CPA `auth_provider` surface has no such pair: `auth.login.start`
// must return a URL immediately and `auth.login.poll` only reports progress. So
// both paths are driven from THIS plugin's own management page, which the `start`
// URL points at:
//
//	auth.login.start   -> { url: <host>/v0/resource/plugins/loomy/login?state=…&action=qr, state }
//	WeChat QR (primary, `wechatlogin.go`):
//	  page ?action=qr          fetch uuid+QR, then ONE long poll per page load
//	  page ?action=bindsend    bind a phone to a WeChat account that has none
//	  page ?action=bindverify  submit that code -> session+userid -> credential
//	SMS (backup, `oauth.go`):
//	  page ?action=send&phone=…            sendMsgCode -> msgid (kept in session)
//	  page ?action=code&digit=… (keypad)   accumulates the 6-digit code
//	  page ?action=verify&phone=…&code=…   checkCode -> session+userid -> credential
//
//	auth.login.poll    -> pending until the credential is stored, then success
//
// The page is a RESOURCE route, which the host dispatches as GET only, so every
// control is a query-string link and never a form (`plugui.Action`), and the QR
// page advances itself with `<meta http-equiv="refresh">` — a page load performs
// one poll and re-renders, no JavaScript anywhere.
//
// The reference needed a loopback popup server plus a browser window for WeChat
// (`loomy-wechat-login.ts:31,174-175,332-345`) only because it is not itself a
// web page; here the QR is rendered inline, which is why WeChat is the primary
// path and SMS the alternative (§2.4, §11).

// loginResourcePath is the browser route that drives the flow. It is registered
// in `management.register` as a Resource (empty Menu: reachable, no sidebar
// entry) and is mounted under /v0/resource/plugins/<pluginID>/.
const loginResourcePath = "/v0/resource/plugins/" + ProviderKey + "/login"

// wechatState is what the QR half of a login session remembers between page
// loads. Every field is written under the session mutex.
type wechatState struct {
	// UUID is the QR identity the authorize page embedded.
	UUID string
	// Image is the QR as a `data:` URL, fetched once per uuid so a re-render
	// costs no WeChat call.
	Image string
	// Last is the previous `wx_errcode`, passed back as `last` on the next poll
	// so WeChat can answer a state change immediately.
	Last string
	// Stage is the last outcome (`waiting`, `scanned`, …) and Detail explains it.
	Stage  string
	Detail string

	// Code is the one-time WeChat code; it is used by `bind/auth` exactly once.
	Code string
	// RCode is the binding context `bind/auth` returned; every later step needs
	// it and the WeChat code is meaningless from here on.
	RCode string
	// Bind is 1 when 讯飞 already has a phone for this WeChat account.
	Bind int
	// Nickname is the WeChat nickname, display-only.
	Nickname string

	// BindPhone is the number a binding code was requested for.
	BindPhone string
	// BindMsgID is the `msgid` of `bind/sendMsg`.
	BindMsgID string
	// BindDigits accumulates the binding code typed on the keypad.
	BindDigits string
}

// loginSession tracks one in-flight interactive sign-in.
type loginSession struct {
	mu sync.Mutex

	// State is the anti-CSRF value returned to the host and echoed by the page.
	// It must stay within [A-Za-z0-9-_.] (the host validates it) — 32 hex
	// characters, no dashes.
	State string
	// Phone is the number the code was requested for.
	Phone string
	// MsgID is the `msgid` from sendMsgCode; `checkCode` is meaningless without
	// it.
	MsgID string
	// digits accumulates the code typed on the keypad page. The page cannot use
	// an input field, so each digit is a link.
	digits string
	// phoneDraft accumulates a phone number typed on the page's own keypad. It
	// exists so the SMS path never requires editing plugin settings: the number
	// is entered here like any other value, one link per digit.
	phoneDraft string

	// wechat is the QR flow's state; the zero value means "no QR requested yet".
	wechat wechatState

	CreatedAt time.Time
	ExpiresAt time.Time

	status     pluginapi.AuthLoginStatus
	message    string
	credential *Credential
	fileName   string
	finished   bool
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// newLoginState mints a state value the host's ValidateOAuthState accepts.
func newLoginState() string {
	raw := make([]byte, 16)
	if _, errRead := rand.Read(raw); errRead != nil {
		return strings.ReplaceAll(randomUUID(), "-", "")
	}
	return hex.EncodeToString(raw)
}

// startLoginSession registers a fresh session.
func startLoginSession(cfg Config, phone string) *loginSession {
	now := time.Now()
	session := &loginSession{
		State:     newLoginState(),
		Phone:     normalizePhone(phone),
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(cfg.loginSessionTTL()) * time.Millisecond),
	}
	loginMu.Lock()
	sweepLoginSessionsLocked(now)
	loginSessions[session.State] = session
	loginMu.Unlock()
	return session
}

// lookupLoginSession finds a live session by state.
func lookupLoginSession(state string) (*loginSession, bool) {
	key := strings.TrimSpace(state)
	if key == "" {
		return nil, false
	}
	loginMu.Lock()
	defer loginMu.Unlock()
	sweepLoginSessionsLocked(time.Now())
	session, found := loginSessions[key]
	if !found || session.expired() {
		return nil, false
	}
	return session, true
}

// forgetLoginSession drops a finished session.
func forgetLoginSession(state string) {
	loginMu.Lock()
	delete(loginSessions, strings.TrimSpace(state))
	loginMu.Unlock()
}

// lastLoginState returns the most recently created pending session, so a page
// that lost its `state` parameter can still finish the login that is in flight.
func lastLoginState() string {
	loginMu.Lock()
	defer loginMu.Unlock()
	newest := ""
	var newestAt time.Time
	for state, session := range loginSessions {
		if session.expired() {
			continue
		}
		if _, _, _, done := session.snapshot(); done {
			continue
		}
		if session.CreatedAt.After(newestAt) {
			newest = state
			newestAt = session.CreatedAt
		}
	}
	return newest
}

// shutdownLoginSessions drops every session; called on plugin shutdown.
func shutdownLoginSessions() {
	loginMu.Lock()
	loginSessions = map[string]*loginSession{}
	loginMu.Unlock()
}

// sweepLoginSessionsLocked removes expired sessions. The caller holds loginMu.
func sweepLoginSessionsLocked(now time.Time) {
	for state, session := range loginSessions {
		if session.expiredAt(now) {
			delete(loginSessions, state)
		}
	}
}

// expired reports whether the session is past its lifetime.
func (s *loginSession) expired() bool {
	if s == nil {
		return true
	}
	return s.expiredAt(time.Now())
}

func (s *loginSession) expiredAt(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !now.Before(s.ExpiresAt)
}

// notePhone records the number a code was requested for.
func (s *loginSession) notePhone(phone string) {
	s.mu.Lock()
	s.Phone = normalizePhone(phone)
	s.digits = ""
	s.mu.Unlock()
}

// noteMsgID records the msgid returned by sendMsgCode.
func (s *loginSession) noteMsgID(msgID string) {
	s.mu.Lock()
	s.MsgID = strings.TrimSpace(msgID)
	s.mu.Unlock()
}

// state returns the immutable session identity.
func (s *loginSession) stateValue() string { return s.State }

// phoneValue returns the current number.
func (s *loginSession) phoneValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Phone
}

// hasMsgID reports whether a code has been requested for this session.
func (s *loginSession) hasMsgID() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.MsgID) != ""
}

// appendDigit adds one keypad press, ignoring anything that is not a digit and
// capping the code at six characters.
func (s *loginSession) appendDigit(digit string) {
	trimmed := strings.TrimSpace(digit)
	if trimmed == "" || !isAllDigits(trimmed) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.digits) < 6 {
		s.digits += trimmed
	}
}

// clearDigits resets the keypad buffer.
func (s *loginSession) clearDigits() {
	s.mu.Lock()
	s.digits = ""
	s.mu.Unlock()
}

// accumulatedCode returns the code the keypad has collected so far.
func (s *loginSession) accumulatedCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.digits
}

// appendPhoneDigit adds one keypad press to the phone draft, ignoring anything
// that is not a digit and capping the draft at 11 characters.
func (s *loginSession) appendPhoneDigit(digit string) {
	trimmed := strings.TrimSpace(digit)
	if trimmed == "" || !isAllDigits(trimmed) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.phoneDraft) < 11 {
		s.phoneDraft += trimmed
	}
}

// clearPhoneDraft resets the phone keypad buffer.
func (s *loginSession) clearPhoneDraft() {
	s.mu.Lock()
	s.phoneDraft = ""
	s.mu.Unlock()
}

// phoneDraftValue returns the number typed on the page, if any.
func (s *loginSession) phoneDraftValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phoneDraft
}

// wechatStateValue copies the QR state out under the lock.
func (s *loginSession) wechatStateValue() wechatState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wechat
}

// updateWechat mutates the QR state under the lock.
func (s *loginSession) updateWechat(apply func(*wechatState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply(&s.wechat)
}

// recordVerified stores the credential produced by a successful verify.
func (s *loginSession) recordVerified(credential *Credential, fileName, message string) {
	s.mu.Lock()
	s.credential = credential
	s.fileName = fileName
	s.status = pluginapi.AuthLoginStatusSuccess
	s.message = message
	s.finished = true
	s.digits = ""
	s.mu.Unlock()
}

// fail marks the session as failed.
func (s *loginSession) fail(message string) {
	s.mu.Lock()
	s.status = pluginapi.AuthLoginStatusError
	s.message = strings.TrimSpace(message)
	s.finished = true
	s.mu.Unlock()
}

// snapshot reports the terminal state of the session, if any.
func (s *loginSession) snapshot() (pluginapi.AuthLoginStatus, string, *Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.finished {
		return "", "", nil, false
	}
	return s.status, s.message, s.credential, true
}

// storedFileName returns the auth file name the verify step saved under.
func (s *loginSession) storedFileName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fileName
}

// isAllDigits reports whether every rune is an ASCII digit.
func isAllDigits(text string) bool {
	for index := 0; index < len(text); index++ {
		if text[index] < '0' || text[index] > '9' {
			return false
		}
	}
	return true
}

// loginPageURL points the browser at the plugin's own login page.
//
// The host hands `auth.login.start` a `baseURL` for its management endpoint
// (`http://host:port/v0/management/oauth-callback`), so its scheme+authority is
// the CPA server the resource route lives on. When the host supplies nothing
// usable the relative resource path is returned and the manager resolves it
// against the API base.
//
// `action` makes the entry point land directly on the flow it names — the WeChat
// QR page for `auth.login.start` — instead of on a menu the user has to pick
// from. An empty action leaves the route's own default page.
func loginPageURL(baseURL, state, action string) string {
	query := url.Values{}
	query.Set("state", state)
	if strings.TrimSpace(action) != "" {
		query.Set("action", strings.TrimSpace(action))
	}
	parsed, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return loginResourcePath + "?" + query.Encode()
	}
	return parsed.Scheme + "://" + parsed.Host + loginResourcePath + "?" + query.Encode()
}

// loginQRAction is the page action that shows a live WeChat QR code. It is what
// `auth.login.start` returns, so the host's sign-in flow leads with the scan
// instead of asking the user anything first.
const loginQRAction = "qr"

// handleAuthLoginStart begins a login and returns immediately.
//
// Nothing here talks to WeChat or to the account host: the returned URL opens
// this plugin's own page with `action=qr`, and THAT page load fetches a live QR
// code from WeChat and renders it. Doing the fetch here would still not be able
// to show anything (the host expects a URL back), and it would block the host's
// own request for as long as WeChat takes.
func handleAuthLoginStart(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	cfg := settings()
	request, _ := abiboot.Decode[pluginapi.AuthLoginStartRequest](raw)
	phone := cfg.Phone
	if candidate, ok := request.Metadata["phone"]; ok {
		if text, isText := candidate.(string); isText {
			phone = text
		}
	}
	session := startLoginSession(cfg, phone)
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       loginPageURL(request.BaseURL, session.stateValue(), loginQRAction),
		State:     session.stateValue(),
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"flow":  "wechat",
			"phone": session.phoneValue(),
			"hint": "打开的页面会立刻显示微信扫码二维码；扫码确认后自动保存凭据。" +
				"页面用 meta refresh 自刷新，每次加载做一次长轮询，全程没有表单也没有脚本。" +
				"需要手机验证码登录时，同一页面提供备用入口（不需要改插件设置）",
		},
	}, nil
}

// handleAuthLoginPoll reports login progress.
//
// It performs NO upstream call: the page already owns the two SMS steps, and a
// poll that re-sent a code would invalidate the msgid the user is about to use.
// The status stays `pending` until the credential is stored.
func handleAuthLoginPoll(_ *abiboot.Host, raw json.RawMessage) (any, error) {
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
	status, message, credential, done := session.snapshot()
	if !done {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待在 Loomy 登录页完成微信扫码（或改用手机验证码）",
		}, nil
	}
	defer forgetLoginSession(request.State)
	if status == pluginapi.AuthLoginStatusSuccess && credential != nil {
		auth, errAuth := authDataFor(credential, session.storedFileName())
		if errAuth != nil {
			return nil, errAuth
		}
		if strings.TrimSpace(message) == "" {
			message = "登录成功"
		}
		return pluginapi.AuthLoginPollResponse{Status: status, Message: message, Auth: auth}, nil
	}
	if status == "" {
		status = pluginapi.AuthLoginStatusError
	}
	return pluginapi.AuthLoginPollResponse{Status: status, Message: message}, nil
}

// sendLoginCode drives the `?action=send` step for the management page.
func sendLoginCode(h *abiboot.Host, cfg Config, session *loginSession, phone string, now time.Time) error {
	if session == nil {
		return statusError(false, "missing_state", http.StatusBadRequest, "缺少 state 参数，请重新开始登录")
	}
	normalized := cfg.defaultPhone(phone)
	if !validPhone(normalized) {
		return statusError(false, "invalid_phone", http.StatusBadRequest,
			"手机号格式不正确：%q（需要 11 位大陆手机号）", strings.TrimSpace(phone))
	}
	msgID, errSend := sendMsgCode(h, cfg, normalized, now)
	if errSend != nil {
		return errSend
	}
	// A failed submitKEEPS the pending {phone, msgid} entry so the user can retry
	// (`jet-hub-rpc.ts:1174-1183`); a resend replaces both.
	session.notePhone(normalized)
	session.noteMsgID(msgID)
	return nil
}

// verifyLoginCode drives the `?action=verify` step and STORES the credential.
//
// Storing here (rather than in the poll handler) is what lets `auth.login.poll`
// report `pending` until the credential exists, exactly as the reference's
// `account.create` + poll pair behaves. The auth file name is deterministic, so
// a later host-side save of the same credential overwrites the same file instead
// of creating a duplicate.
func verifyLoginCode(h *abiboot.Host, cfg Config, session *loginSession, phone, code string, now time.Time) (*Credential, error) {
	if session == nil {
		return nil, statusError(false, "missing_state", http.StatusBadRequest, "缺少 state 参数，请重新开始登录")
	}
	if !session.hasMsgID() {
		return nil, statusError(false, "missing_msgid", http.StatusBadRequest, "请先发送验证码")
	}
	resolved := strings.TrimSpace(code)
	if resolved == "" {
		resolved = session.accumulatedCode()
	}
	if resolved == "" {
		return nil, statusError(false, "missing_code", http.StatusBadRequest, "请填写短信验证码")
	}
	// The number the code was SENT to wins over the configured default: a bare
	// `?action=verify` link must not verify against a different number.
	resolvedPhone := normalizePhone(phone)
	if !phonePattern.MatchString(resolvedPhone) {
		resolvedPhone = normalizePhone(session.phoneValue())
	}
	if !phonePattern.MatchString(resolvedPhone) {
		return nil, statusError(false, "invalid_phone", http.StatusBadRequest,
			"手机号格式不正确：%q（需要 11 位大陆手机号）", strings.TrimSpace(phone))
	}

	sessionToken, userID, errCheck := checkCode(h, cfg, resolvedPhone, resolved, msgIDOf(session), now)
	if errCheck != nil {
		// A failed submit keeps the pending entry so the user can retry
		// (`jet-hub-rpc.ts:1174-1183`).
		return nil, errCheck
	}
	credential := buildCredential(sessionToken, userID, resolvedPhone, "", cfg, now)
	if errStore := storeLoginCredential(h, cfg, session, credential, "登录成功，账号已保存"); errStore != nil {
		return nil, errStore
	}
	return credential, nil
}

// storeLoginCredential persists one freshly logged-in credential and marks the
// session successful — the single place both login paths finish at.
//
// The auth file name comes from `authDataFor`, i.e. it is derived from the
// account's own identity, so a later host-side save of the same credential
// overwrites the same file instead of creating a duplicate. The daily-grant
// initialisation is the reference's best-effort follow-up: warn-only, it never
// fails a successful login (`loomy-auth.ts:241-249`).
func storeLoginCredential(h *abiboot.Host, cfg Config, session *loginSession, credential *Credential, message string) error {
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		return errAuth
	}
	if h != nil {
		if _, errSave := h.SaveAuth(auth.FileName, auth.StorageJSON); errSave != nil {
			return transportError("save_auth", "保存 Loomy 凭据失败：%v", errSave)
		}
	}
	ensureDailyGrant(h, credential, cfg)
	if session != nil {
		session.recordVerified(credential, auth.FileName, message)
	}
	return nil
}

// msgIDOf reads the message id a session is holding.
func msgIDOf(session *loginSession) string {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.MsgID
}
