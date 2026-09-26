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

// SMS login, ported from `src/loomy-oauth.ts:131-180` with one structural
// difference forced by the host contract.
//
// The TypeScript plugin's RPC surface is `login.sendSms` + `login.submitSms`,
// i.e. the DSH frontend collects the phone and the code and calls the plugin
// twice. The CPA `auth_provider` surface has no such pair: `auth.login.start`
// must return a URL immediately and `auth.login.poll` only reports progress. So
// the two SMS steps are driven from THIS plugin's own management page, which the
// `start` URL points at:
//
//	auth.login.start   -> { url: <host>/v0/resource/plugins/loomy/login?state=…, state }
//	page  ?action=send&phone=…            -> sendMsgCode -> msgid (kept in session)
//	page  ?action=code&digit=… (keypad)   -> accumulates the 6-digit code
//	page  ?action=verify&phone=…&code=…   -> checkCode -> session+userid -> credential saved
//	auth.login.poll    -> pending until the credential is stored, then success
//
// The page is a RESOURCE route, which the host dispatches as GET only, so every
// control is a query-string link and never a form (`plugui.Action`).
//
// The WeChat QR path of `src/loomy-wechat.ts` is deliberately NOT ported: it needs
// a loopback HTTP popup server plus a user-visible browser window
// (`loomy-wechat-login.ts:31,174-175,332-345`), which the CPA sign-in surface
// cannot host, and its callback page
// `https://loomy.xunfei.cn/oauth/wechat/callback` 404s by design — see §11 of the
// porting spec and its risk #7. SMS is therefore the only login path here, and a
// WeChat-issued credential (empty `phone`) is still accepted by auth.parse.

// loginResourcePath is the browser route that drives the flow. It is registered
// in `management.register` as a Resource (empty Menu: reachable, no sidebar
// entry) and is mounted under /v0/resource/plugins/<pluginID>/.
const loginResourcePath = "/v0/resource/plugins/" + ProviderKey + "/login"

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
func loginPageURL(baseURL, state string) string {
	query := url.Values{}
	query.Set("state", state)
	parsed, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return loginResourcePath + "?" + query.Encode()
	}
	return parsed.Scheme + "://" + parsed.Host + loginResourcePath + "?" + query.Encode()
}

// handleAuthLoginStart begins an SMS login and returns immediately.
//
// Nothing here talks to the account host: the phone number is collected on the
// plugin's own page. Doing the sendMsgCode call here would block the host's
// request and still could not receive the code, which is the ordering deadlock
// the reference documents for `loginMode: 'sms'` (`jet-hub-rpc.ts:937-941`).
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
		URL:       loginPageURL(request.BaseURL, session.stateValue()),
		State:     session.stateValue(),
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"flow":  "sms",
			"phone": session.phoneValue(),
			"hint": "在打开的页面里发送短信验证码并提交：Loomy 只有手机验证码登录，" +
				"没有回调端口，也没有 refresh_token",
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
			Message: "等待在 Loomy 登录页完成短信验证",
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
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		return nil, errAuth
	}
	if h != nil {
		if _, errSave := h.SaveAuth(auth.FileName, auth.StorageJSON); errSave != nil {
			return nil, transportError("save_auth", "保存 Loomy 凭据失败：%v", errSave)
		}
	}
	// The daily pool initialisation is best-effort and warn-only: a failure here
	// must not fail a successful login (`loomy-auth.ts:241-249`).
	ensureDailyGrant(h, credential, cfg)
	session.recordVerified(credential, auth.FileName, "登录成功，账号已保存")
	return credential, nil
}

// msgIDOf reads the message id a session is holding.
func msgIDOf(session *loginSession) string {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.MsgID
}
