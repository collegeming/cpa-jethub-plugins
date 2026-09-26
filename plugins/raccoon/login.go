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
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/qr"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The QR login flow.
//
// The reference hosts a temporary loopback HTTP page with a QR tab and an SMS
// tab (`raccoon-login-page.ts:51-56,312-325`). A CPA plugin needs none of that
// plumbing: a resource route can render the QR inline and advance itself with
// `<meta http-equiv="refresh">`, so the page IS the flow:
//
//	page load with no code     -> mint code + QR image, render, reload in 2 s
//	page load with a code      -> ONE poll, then re-render
//	canceled                   -> mint a BRAND-NEW code and QR image
//	success (+ access_token)   -> save the credential, stop refreshing
//	300 s wall clock           -> the session ends and the page asks to restart
//
// Nothing here loops: one page load is exactly one vendor poll, and the page
// carries no form and no script (`plugui.HTMLWithRefresh`).
//
// ⚠️ SMS login is deliberately NOT implemented. Every `send_sms` needs an Aliyun
// captcha token that only Aliyun's browser JS mints, after a human solves a
// slider (`raccoon-login-page.ts:417,505-524`, spec §2.3) — there is no headless
// path, so shipping it would be a dead button. The login page says so in one
// line and offers the WeChat scan only.

// loginResourcePath is the browser route that drives the flow. It is registered
// as a Menu-less Resource, so it is reachable without adding a sidebar entry.
const loginResourcePath = "/v0/resource/plugins/" + ProviderKey + "/login"

// qrImageScale and qrImageQuietZone render a version-8 symbol at 228 px, close
// to the 200 px the reference renders (`raccoon-login-page.ts:346`).
const (
	qrImageScale     = 4
	qrImageQuietZone = 4
)

// qrState is what the QR half of a login session remembers between page loads.
type qrState struct {
	// Code is the 32-hex code generated LOCALLY. The server accepts self-made
	// codes, which is the entire reason this path needs no callback
	// (`raccoon.ts:15`) — UNVERIFIED against production, see the README note.
	Code string
	// Content is the exact URL the QR encodes, kept so the page can show it and
	// so a test can assert the 144-byte shape without re-deriving it.
	Content string
	// Image is the QR as a `data:image/png;base64,…` URL, rendered once per code
	// so a page reload costs no encoding work.
	Image string
	// Stage is the last observed vendor status.
	Stage string
	// Detail explains the last outcome.
	Detail string
	// ExpiredAt is `data.expired_at` when the server sent one.
	ExpiredAt string
	// Rotations counts regenerated codes, for diagnostics.
	Rotations int
}

// loginSession tracks one in-flight interactive sign-in.
type loginSession struct {
	mu sync.Mutex

	// State is the anti-CSRF value returned to the host and echoed by the page.
	// The host validates its shape, so it stays within [A-Za-z0-9-_.]: 32 hex
	// characters, no dashes.
	State string
	// qr is the QR flow's state; an empty Code means "not started yet".
	qr qrState

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
		return strings.ReplaceAll(itoaInt64(time.Now().UnixNano()), "-", "")
	}
	return hex.EncodeToString(raw)
}

// generateLoginCode mints the 32-lowercase-hex code the QR points at
// (`randomBytes(16).toString('hex')`, `raccoon-oauth.ts:111-118`).
func generateLoginCode() (string, error) {
	raw := make([]byte, 16)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", statusError(true, "code_generation", http.StatusInternalServerError, "生成扫码 code 失败：%v", errRead)
	}
	return hex.EncodeToString(raw), nil
}

// qrContentFor builds the exact URL the QR encodes.
//
// ⚠️ Parameter ORDER matters for fidelity: `code` first, then `appname`
// percent-encoded (`URLSearchParams({code, appname})`, `raccoon-oauth.ts:127-130`).
// `url.Values.Encode()` would sort the keys and put `appname` first, so the string
// is assembled by hand. The result is 144 bytes for a 32-hex code, which needs
// version 8 at level M.
func qrContentFor(code string) string {
	return QRLoginPageBase + "?code=" + code + "&appname=" + url.QueryEscape(QRLoginAppName)
}

// renderQRDataURL encodes a login code's URL as a PNG data URL.
func renderQRDataURL(code string) (string, string, error) {
	content := qrContentFor(code)
	symbol, errEncode := qr.Encode(content)
	if errEncode != nil {
		return "", "", statusError(false, "qr_encode", http.StatusInternalServerError, "生成二维码失败：%v", errEncode)
	}
	image, errURL := symbol.DataURL(qrImageScale, qrImageQuietZone)
	if errURL != nil {
		return "", "", statusError(false, "qr_render", http.StatusInternalServerError, "渲染二维码失败：%v", errURL)
	}
	return content, image, nil
}

// startLoginSession registers a fresh session.
//
// It deliberately mints NO code: the first page load does that, and only that
// load renders without polling. Minting here would make the very first load poll
// a code nobody has seen yet.
func startLoginSession(cfg Config) (*loginSession, error) {
	now := time.Now()
	session := &loginSession{
		State:     newLoginState(),
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(cfg.loginSessionTTL()) * time.Millisecond),
		status:    pluginapi.AuthLoginStatusPending,
	}
	loginMu.Lock()
	sweepLoginSessionsLocked(now)
	loginSessions[session.State] = session
	loginMu.Unlock()
	return session, nil
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

// lastLoginState returns the most recently created pending session, so a page
// that lost its `state` parameter can still finish the login in flight.
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

// forgetLoginSession drops a finished session.
func forgetLoginSession(state string) {
	loginMu.Lock()
	delete(loginSessions, strings.TrimSpace(state))
	loginMu.Unlock()
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

// stateValue returns the immutable session identity.
func (s *loginSession) stateValue() string { return s.State }

// rotateQR mints a new code and its QR image, replacing the previous one.
//
// This is the response to `canceled` (`raccoon-login-page.ts:211-221`): the old
// code is abandoned — a fresh one costs nothing and the server treats codes
// independently.
func (s *loginSession) rotateQR() error {
	code, errCode := generateLoginCode()
	if errCode != nil {
		return errCode
	}
	content, image, errImage := renderQRDataURL(code)
	if errImage != nil {
		return errImage
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.qr.Code != "" {
		s.qr.Rotations++
	}
	s.qr.Code = code
	s.qr.Content = content
	s.qr.Image = image
	s.qr.Stage = qrStatusPending
	return nil
}

// qrStateValue returns a copy of the QR state.
func (s *loginSession) qrStateValue() qrState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qr
}

// updateQR applies a mutation under the session lock.
func (s *loginSession) updateQR(apply func(*qrState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply(&s.qr)
}

// recordVerified marks the session successful with a stored credential.
func (s *loginSession) recordVerified(credential *Credential, fileName, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = pluginapi.AuthLoginStatusSuccess
	s.message = message
	s.credential = credential
	s.fileName = fileName
	s.finished = true
}

// fail marks the session terminally failed.
func (s *loginSession) fail(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = pluginapi.AuthLoginStatusError
	s.message = message
	s.finished = true
}

// snapshot reports the session's host-facing state.
func (s *loginSession) snapshot() (pluginapi.AuthLoginStatus, string, *Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.message, s.credential, s.finished
}

// storedFileName returns the auth file the credential was written to.
func (s *loginSession) storedFileName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fileName
}

// loginOutcome is what one page load should render.
type loginOutcome struct {
	// Stage is the vendor status, or one of this plugin's own stages.
	Stage string
	// Message is the notice the page shows.
	Message string
	// RefreshSeconds is the meta-refresh interval; 0 means "do not reload".
	RefreshSeconds int
	// Credential is set once the login succeeded.
	Credential *Credential
}

// advanceQR moves the flow by exactly one step and reports what to show.
//
// One call = at most one vendor poll. The reference's cadence is 2 s
// (`raccoon-login-page.ts:421`), which is what the page's meta refresh uses.
func advanceQR(h *abiboot.Host, cfg Config, session *loginSession, fresh bool) loginOutcome {
	if session == nil {
		return loginOutcome{Stage: "failed", Message: "登录会话不存在或已超时，请重新发起登录"}
	}
	current := session.qrStateValue()
	if fresh || strings.TrimSpace(current.Code) == "" {
		if errRotate := session.rotateQR(); errRotate != nil {
			return loginOutcome{Stage: "error", Message: "获取二维码失败：" + errRotate.Error(), RefreshSeconds: cfg.qrPollSeconds()}
		}
		return loginOutcome{
			Stage:          qrStatusPending,
			Message:        "二维码已生成，请用微信扫描（本页每 " + itoaInt(cfg.qrPollSeconds()) + " 秒自动刷新一次）",
			RefreshSeconds: cfg.qrPollSeconds(),
		}
	}

	outcome := pollQRCodeOnce(h, cfg, current.Code)
	session.updateQR(func(state *qrState) {
		state.Stage = outcome.Status
		state.Detail = outcome.Detail
		if outcome.ExpiredAt != "" {
			state.ExpiredAt = outcome.ExpiredAt
		}
	})

	switch outcome.Status {
	case qrStatusLogging:
		return loginOutcome{
			Stage:          qrStatusLogging,
			Message:        "已扫码，请在手机上确认登录…",
			RefreshSeconds: cfg.qrPollSeconds(),
		}

	case qrStatusCanceled:
		// ⚠️ `canceled` regenerates the code, after which a FRESH QR is shown
		// (`raccoon-login-page.ts:211-221`).
		if errRotate := session.rotateQR(); errRotate != nil {
			return loginOutcome{Stage: "error", Message: "二维码已取消，重新获取也失败了：" + errRotate.Error(), RefreshSeconds: cfg.qrPollSeconds()}
		}
		return loginOutcome{
			Stage:          "canceled",
			Message:        "本次扫码已在手机端取消，已自动换了一张新二维码，请重新扫描",
			RefreshSeconds: cfg.qrPollSeconds(),
		}

	case qrStatusSuccess:
		credential := outcome.Credential
		if credential == nil {
			// Defensive: pollQRCodeOnce never reports success without one.
			return loginOutcome{Stage: qrStatusPending, Message: "等待登录结果…", RefreshSeconds: cfg.qrPollSeconds()}
		}
		if errStore := storeLoginCredential(h, cfg, session, credential, "登录成功，账号已保存（微信扫码）"); errStore != nil {
			return loginOutcome{Stage: "failed", Message: errStore.Error()}
		}
		return loginOutcome{Stage: "done", Message: "登录成功，账号已保存", Credential: credential}

	default:
		message := "等待微信扫码…（每次页面加载只做一次轮询）"
		if strings.TrimSpace(outcome.Detail) != "" && outcome.Status != qrStatusPending {
			message = outcome.Detail
		}
		return loginOutcome{Stage: qrStatusPending, Message: message, RefreshSeconds: cfg.qrPollSeconds()}
	}
}

// storeLoginCredential persists one freshly logged-in credential and marks the
// session successful.
//
// The profile is filled in best-effort BEFORE the name is derived, so the auth
// file name uses the account's own identity (`user_id`, then nickname, then
// phone) rather than a constant. The one-off desktop login reward is NOT claimed
// here: it is an explicit action on the reward page, deliberately outside every
// automatic path.
func storeLoginCredential(h *abiboot.Host, cfg Config, session *loginSession, credential *Credential, message string) error {
	if credential == nil || credential.Session() == "" {
		return statusError(false, "empty_credential", http.StatusBadGateway, "Raccoon 登录响应没有可用的 access_token")
	}
	enrichCredential(h, cfg, credential)
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		return errAuth
	}
	if h != nil {
		if _, errSave := h.SaveAuth(auth.FileName, auth.StorageJSON); errSave != nil {
			return transportError("save_auth", "保存 Raccoon 凭据失败：%v", errSave)
		}
	}
	if session != nil {
		session.recordVerified(credential, auth.FileName, message)
	}
	return nil
}

// loginPageURL builds the page URL `auth.login.start` hands back to the host.
//
// The host's BaseURL wins when it is usable; otherwise the relative resource
// path is returned and the manager resolves it against the API base.
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

// loginQRAction is the page action that shows a live QR code. It is what
// `auth.login.start` returns, so the host's sign-in flow lands directly on the
// scan instead of on a menu.
const loginQRAction = "qr"

// handleAuthLoginStart begins a login and returns immediately.
//
// Nothing here talks to the vendor: the returned URL opens this plugin's own
// page, and THAT page load mints the code and renders the QR. Doing it here would
// block the host's request and could not display anything anyway.
func handleAuthLoginStart(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	cfg := settings()
	request, _ := abiboot.Decode[pluginapi.AuthLoginStartRequest](raw)
	session, errStart := startLoginSession(cfg)
	if errStart != nil {
		return nil, errStart
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       loginPageURL(request.BaseURL, session.stateValue(), loginQRAction),
		State:     session.stateValue(),
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"flow": "wechat-qr",
			"hint": "打开的页面会立刻显示微信扫码二维码；扫码确认后自动保存凭据。" +
				"页面用 meta refresh 自刷新，每次加载做一次轮询，全程没有表单也没有脚本。" +
				"手机验证码登录需要人机验证（阿里云滑块），本插件无法完成，因此只提供微信扫码",
		},
	}, nil
}

// handleAuthLoginPoll reports login progress.
//
// It performs NO upstream call of its own: the page owns the single poll per
// load, and polling here as well would double the vendor traffic and could
// consume the code. The status stays `pending` until the credential is stored.
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
			Message: "等待在 Raccoon 登录页完成微信扫码",
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
