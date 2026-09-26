package main

// 本文件是 Jet-Hub `src/buddy-oauth.ts` 的 Go 移植：external-link-v2 轮询式登录、
// 静默续期与登录会话管理。
//
// 与 TS 的结构性差异（CPA ABI 决定，非简化）：
//   - TS 的 `runBuddyLoginFlow` 是一个「取 state → 开浏览器 → 轮询 token → 轮询
//     account」的**阻塞循环**；CPA 把它拆成 `auth.login.start`（返回 URL，立即返回）
//     与 `auth.login.poll`（每次调用推进一轮）。本文件的两个 handler 分别对应，
//     **不阻塞**，符合 AGENTS.md「两步式登录必须立即返回 loginUrl」。
//   - 上游轮询阶段（1s 间隔 / 5min 超时）由 CPA 宿主按调用节奏驱动；插件侧只保留
//     会话 TTL（LoginTimeoutMS），不再自行 sleep。

import (
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
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ── HTTP 响应辅助（buddy-oauth.ts:78-97）──

// responseCode reads the business `code` field. buddy-oauth.ts:79-83.
func responseCode(body any) int {
	record, ok := body.(map[string]any)
	if !ok {
		return 0
	}
	if code, isNumber := record["code"].(float64); isNumber {
		return int(code)
	}
	return 0
}

// responseMessage reads the `message` field. buddy-oauth.ts:86-90.
func responseMessage(body any) string {
	record, ok := body.(map[string]any)
	if !ok {
		return ""
	}
	if message, isString := record["message"].(string); isString {
		return message
	}
	return ""
}

// responseData reads the `data` field, treating null as absent.
// buddy-oauth.ts:93-97.
func responseData(body any) (any, bool) {
	record, ok := body.(map[string]any)
	if !ok {
		return nil, false
	}
	data, present := record["data"]
	if !present || data == nil {
		return nil, false
	}
	return data, true
}

// doJSON performs one control-plane request through the host transport and
// decodes the JSON body when there is one. buddy-oauth.ts:109-131.
//
// NOTE: the TS applies a per-request AbortSignal timeout (5s for auth/state,
// 60s otherwise). The CPA host transport is synchronous and offers no
// per-request deadline, so the timeout is delegated to the host. This is a
// platform difference, not an omission of a TS behaviour.
func doJSON(h *abiboot.Host, method, rawURL string, headers map[string]string, body []byte) (int, any, error) {
	if h == nil {
		return 0, nil, abiboot.Errorf("host_unavailable", "no host handle for %s %s", method, rawURL)
	}
	wire := http.Header{}
	for key, value := range headers {
		if value == "" {
			continue
		}
		wire.Set(key, value)
	}
	if len(body) > 0 {
		wire.Set("Content-Type", "application/json")
	}
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  method,
		URL:     rawURL,
		Headers: wire,
		Body:    body,
	})
	if errDo != nil {
		return 0, nil, abiboot.Errorf("network", "%s %s network error: %v", method, rawURL, errDo)
	}
	var decoded any
	if len(response.Body) > 0 {
		_ = json.Unmarshal(response.Body, &decoded)
	}
	return response.StatusCode, decoded, nil
}

// lookupsFailed reports whether the decoded body is usable at all. The probe
// host and a dead transport both return status 0 with an empty body.
func lookupsFailed(status int, body any) bool {
	return status == 0 && body == nil
}

// ── auth/state（buddy-oauth.ts:137-167）──

// fetchAuthState performs POST /v2/plugin/auth/state?platform=<platform>.
func fetchAuthState(h *abiboot.Host, product productConfig) (state string, authURL string, err error) {
	endpoint := product.urlFor(AuthStatePath) + "?platform=" + url.QueryEscape(product.Platform)
	headers := map[string]string{
		HeaderDomain:           product.APIDomain,
		HeaderNoAuthorization:  "true",
		HeaderNoUserID:         "true",
		HeaderNoEnterpriseID:   "true",
		HeaderNoDepartmentInfo: "true",
		"User-Agent":           product.UserAgent,
	}
	status, body, errDo := doJSON(h, http.MethodPost, endpoint, headers, nil)
	if errDo != nil {
		return "", "", errDo
	}
	if status != http.StatusOK {
		return "", "", abiboot.Errorf("auth_state_status", "auth/state HTTP %d: %s", status, responseMessage(body))
	}
	data, ok := responseData(body)
	if !ok {
		return "", "", abiboot.Errorf("auth_state_shape", "auth/state 响应缺少 data 字段: %s", truncate(marshalCompact(body), 300))
	}
	record, _ := data.(map[string]any)
	if record == nil {
		return "", "", abiboot.Errorf("auth_state_shape", "auth/state data 不是对象")
	}
	state, _ = record["state"].(string)
	authURL, _ = record["authUrl"].(string)
	if state == "" {
		return "", "", abiboot.Errorf("auth_state_shape", "auth/state 响应缺少 state 字段")
	}
	if authURL == "" {
		return "", "", abiboot.Errorf("auth_state_shape", "auth/state 响应缺少 authUrl 字段")
	}
	return state, authURL, nil
}

// ── token / account 轮询（buddy-oauth.ts:178-277）──

// pollTokenOnce performs one GET /v2/plugin/auth/token?state=<state>.
// A nil token with a nil error means "not ready yet".
func pollTokenOnce(h *abiboot.Host, product productConfig, state string) (*buddyToken, error) {
	endpoint := product.urlFor(AuthTokenPath) + "?state=" + url.QueryEscape(state)
	headers := map[string]string{
		HeaderNoAuthorization: "true",
		"User-Agent":          product.UserAgent,
	}
	status, body, errDo := doJSON(h, http.MethodGet, endpoint, headers, nil)
	if errDo != nil {
		// 网络错误继续轮询（buddy-oauth.ts:207-210）：返回 pending。
		return nil, nil
	}
	if lookupsFailed(status, body) {
		return nil, nil
	}
	if status == http.StatusOK {
		data, ok := responseData(body)
		if !ok {
			return nil, nil
		}
		token := parseTokenData(data)
		if token.AccessToken == "" {
			return nil, nil
		}
		return &token, nil
	}
	if responseCode(body) == CodeTokenNotReady {
		return nil, nil
	}
	return nil, abiboot.Errorf("auth_token_status", "auth/token HTTP %d code=%d: %s", status, responseCode(body), responseMessage(body))
}

// pollAccountOnce performs one GET /v2/plugin/login/account?state=<state>.
// A nil account with a nil error means "not ready yet".
func pollAccountOnce(h *abiboot.Host, product productConfig, state string, token buddyToken) (*buddyAccount, error) {
	endpoint := product.urlFor(LoginAccountPath) + "?state=" + url.QueryEscape(state)
	domain := token.Domain
	if domain == "" {
		// TS 直接下发 token.domain（可能为空串）；这里用产品域名兜底，
		// 与 AGENTS.md「X-Domain 必须跟随产品」一致。
		domain = product.APIDomain
	}
	headers := map[string]string{
		HeaderDomain:         domain,
		"Authorization":      "Bearer " + token.AccessToken,
		HeaderNoUserID:       "true",
		HeaderNoEnterpriseID: "true",
		"User-Agent":         product.UserAgent,
	}
	status, body, errDo := doJSON(h, http.MethodGet, endpoint, headers, nil)
	if errDo != nil {
		return nil, nil
	}
	if lookupsFailed(status, body) {
		return nil, nil
	}
	if status == http.StatusOK {
		data, ok := responseData(body)
		if !ok {
			return nil, nil
		}
		account := parseAccountData(data)
		return &account, nil
	}
	if responseCode(body) == CodeAccountNotReady {
		return nil, nil
	}
	return nil, abiboot.Errorf("login_account_status", "login/account HTTP %d code=%d: %s", status, responseCode(body), responseMessage(body))
}

// ── 静默续期（buddy-oauth.ts:289-329）──

// errRefreshTokenExpired marks a refresh token the backend has rejected, so the
// scheduler stops retrying and the user is asked to sign in again.
var errRefreshTokenExpired = abiboot.Errorf("refresh_token_expired", "CodeBuddy refresh_token 已失效，请重新登录")

// refreshCredential performs POST /v2/plugin/auth/token/refresh and merges the
// new token into the existing credential the way buddy-auth.ts:257-268 does:
// every other field (user id / nickname / enterprise / account type / product)
// is preserved, and the domain is only replaced when the backend returns one.
func refreshCredential(h *abiboot.Host, credential *Credential, product productConfig) (*Credential, error) {
	if !credential.Refreshable() {
		return nil, errRefreshTokenExpired
	}
	endpoint := product.urlFor(AuthRefreshPath)
	headers := map[string]string{
		HeaderDomain:            product.APIDomain,
		"User-Agent":            product.UserAgent,
		"Authorization":         "Bearer " + credential.AccessToken,
		HeaderRefreshToken:      credential.RefreshToken,
		HeaderAuthRefreshSource: AuthRefreshSource,
	}
	if credential.EnterpriseID != "" {
		headers[HeaderEnterpriseID] = credential.EnterpriseID
		headers[HeaderTenantID] = credential.EnterpriseID
	}
	status, body, errDo := doJSON(h, http.MethodPost, endpoint, headers, nil)
	if errDo != nil {
		return nil, errDo
	}
	if status != http.StatusOK {
		code := responseCode(body)
		message := responseMessage(body)
		// 终态判定（buddy-oauth.ts:312-325）。
		expired := status == http.StatusUnauthorized || status == http.StatusForbidden ||
			code == http.StatusUnauthorized || code == http.StatusForbidden ||
			strings.Contains(message, "expired") || strings.Contains(message, "invalid")
		if expired {
			return nil, errRefreshTokenExpired
		}
		return nil, abiboot.Errorf("refresh_status", "刷新 token HTTP %d code=%d: %s", status, code, message)
	}
	data, ok := responseData(body)
	if !ok {
		return nil, abiboot.Errorf("refresh_shape", "刷新 token 响应缺少 data 字段")
	}
	token := parseTokenData(data)
	if token.AccessToken == "" {
		return nil, abiboot.Errorf("refresh_shape", "刷新 token 响应缺少 accessToken")
	}
	refreshed := *credential
	refreshed.AccessToken = token.AccessToken
	refreshed.RefreshToken = token.RefreshToken
	refreshed.ExpiresAt = token.ExpiresAt
	refreshed.RefreshExpiresAt = token.RefreshExpiresAt
	refreshed.TokenType = token.TokenType
	refreshed.Scope = token.Scope
	if token.Domain != "" {
		refreshed.Domain = token.Domain
	}
	refreshed.sanitize()
	return &refreshed, nil
}

// ── 登录 URL 装饰（buddy-oauth.ts:525-538）──

// decorateLoginURL appends `version` + `loginSessionId` for products whose
// appendSessionParams is set (WorkBuddy variants). CodeBuddy's server-issued
// URL is returned unchanged — the platform/state/path all come from upstream.
func decorateLoginURL(authURL string, product productConfig) string {
	if !product.AppendSessionParams {
		return authURL
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		return authURL
	}
	query := parsed.Query()
	if product.PluginVersion != "" {
		query.Set("version", product.PluginVersion)
	}
	query.Set("loginSessionId", randomUUIDv4())
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// randomUUIDv4 builds a version-4 UUID, the Go equivalent of the TS
// `crypto.randomUUID()` calls (buddy-oauth.ts:532, buddy-adapter.ts:561).
func randomUUIDv4() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%08x-0000-4000-8000-%012x", time.Now().UnixNano(), time.Now().UnixNano()&0xffffffffffff)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// newPromptCacheKey returns the dash-free UUID used as prompt_cache_key
// (buddy-adapter.ts:561: `crypto.randomUUID().replace(/-/g, ”)`).
func newPromptCacheKey() string {
	return strings.ReplaceAll(randomUUIDv4(), "-", "")
}

// randomHex returns n random bytes hex-encoded (a CodeArts-ported helper the
// auth-file naming and fallback state share).
func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", abiboot.Errorf("random", "generate random bytes: %v", err)
	}
	return hex.EncodeToString(raw), nil
}

// ── 登录会话 ──

// loginSession tracks one in-flight interactive sign-in. Upstream issues the
// `state`, so the registry is keyed by that value: auth.login.poll hands it back
// verbatim and the plugin must not have to remember anything else.
type loginSession struct {
	mu sync.Mutex

	State     string
	Product   string
	AuthURL   string
	CreatedAt time.Time
	ExpiresAt time.Time

	// Fallback marks a session whose URL was synthesised locally because
	// auth/state was unreachable; such a session cannot complete.
	Fallback bool
	// Token caches the token once the token poll succeeds, so the next poll
	// only has to fetch the account.
	Token *buddyToken

	status     pluginapi.AuthLoginStatus
	message    string
	credential *Credential
	finished   bool
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// registerLoginSession stores a session under its upstream state.
func registerLoginSession(session *loginSession) {
	loginMu.Lock()
	purgeExpiredLoginSessionsLocked()
	loginSessions[session.State] = session
	loginMu.Unlock()
}

// lookupLoginSession finds a non-expired session by state.
func lookupLoginSession(state string) (*loginSession, bool) {
	loginMu.Lock()
	session, ok := loginSessions[state]
	if ok && session.expired() && !session.finished {
		delete(loginSessions, state)
		session.fail("登录会话已超时，请重新发起")
		ok = false
	}
	loginMu.Unlock()
	return session, ok
}

// forgetLoginSession drops a completed session.
func forgetLoginSession(state string) {
	loginMu.Lock()
	delete(loginSessions, state)
	loginMu.Unlock()
}

// purgeExpiredLoginSessionsLocked drops expired entries; callers hold loginMu.
func purgeExpiredLoginSessionsLocked() {
	for state, session := range loginSessions {
		if session.expired() {
			delete(loginSessions, state)
		}
	}
}

// shutdownLoginSessions drops every session during plugin shutdown.
func shutdownLoginSessions() {
	loginMu.Lock()
	loginSessions = map[string]*loginSession{}
	loginMu.Unlock()
}

func (s *loginSession) expired() bool {
	return time.Now().After(s.ExpiresAt)
}

func (s *loginSession) fail(message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusError
	s.message = message
	s.mu.Unlock()
}

func (s *loginSession) finish(credential *Credential, message string) {
	s.mu.Lock()
	s.finished = true
	s.status = pluginapi.AuthLoginStatusSuccess
	s.message = message
	s.credential = credential
	s.mu.Unlock()
}

func (s *loginSession) snapshot() (pluginapi.AuthLoginStatus, string, *Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.message, s.credential, s.finished
}

func (s *loginSession) cachedToken() *buddyToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Token
}

func (s *loginSession) storeToken(token *buddyToken) {
	s.mu.Lock()
	s.Token = token
	s.mu.Unlock()
}

// ── auth.login.start ──

// loginStartResult is the internal outcome of starting a login, shared by the
// auth.login.start handler and the management login page.
type loginStartResult struct {
	Session     *loginSession
	AuthURL     string
	State       string
	ExpiresAt   time.Time
	Fallback    bool
	FallbackWhy string
}

// startLogin acquires an upstream state and registers a session.
//
// When auth/state is unreachable the CodeBuddy (China) product falls back to the
// locally built login URL documented at buddy.ts:6
// (`https://www.codebuddy.cn/login/?platform=ide&state=...`, with the home from
// buddy.ts:26). That URL is exactly the one the TS comment names, but unlike the
// server-issued URL it cannot complete the poll loop, so the result is flagged
// as a fallback. No other product gets a synthesised URL: the TS source does not
// contain a website home for them.
func startLogin(h *abiboot.Host, cfg Config) (*loginStartResult, error) {
	product, _ := productByConfigValue(cfg.Product)
	state, authURL, errState := fetchAuthState(h, product)
	if errState != nil {
		if product.ConfigValue != ProductCodeBuddy {
			return nil, errState
		}
		fallbackState, errRandom := randomHex(16)
		if errRandom != nil {
			return nil, errState
		}
		fallbackURL := WebsiteHome + "/login/?platform=" + url.QueryEscape(product.Platform) + "&state=" + fallbackState
		session := &loginSession{
			State:     fallbackState,
			Product:   product.ConfigValue,
			AuthURL:   fallbackURL,
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(LoginTimeoutMS * time.Millisecond),
			Fallback:  true,
			status:    pluginapi.AuthLoginStatusPending,
		}
		registerLoginSession(session)
		return &loginStartResult{
			Session: session, AuthURL: fallbackURL, State: fallbackState,
			ExpiresAt: session.ExpiresAt, Fallback: true, FallbackWhy: errState.Error(),
		}, nil
	}

	decorated := decorateLoginURL(authURL, product)
	session := &loginSession{
		State:     state,
		Product:   product.ConfigValue,
		AuthURL:   decorated,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(LoginTimeoutMS * time.Millisecond),
		status:    pluginapi.AuthLoginStatusPending,
	}
	registerLoginSession(session)
	return &loginStartResult{Session: session, AuthURL: decorated, State: state, ExpiresAt: session.ExpiresAt}, nil
}

// loginPollAuth builds the host record for a finished sign-in.
//
// The credential belongs in the file the same account already lives in — the
// host saves whichever name this record carries — and only an account the host
// does not know yet gets a freshly derived one. Without this, logging into an
// account whose file was named by an older build (a random suffix, which no
// derivation can reproduce) adds a second entry beside it.
func loginPollAuth(h *abiboot.Host, credential *Credential) (pluginapi.AuthData, error) {
	return authDataFor(credential, authNameForHost(existingAuthFileName(h, credential), "", "", credential))
}

// pollLogin advances one login session by a single poll step and returns the
// CPA status reply. It performs at most one token request and one account
// request per invocation.
func pollLogin(h *abiboot.Host, state string) pluginapi.AuthLoginPollResponse {
	session, found := lookupLoginSession(state)
	if !found {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已超时，请重新发起登录",
		}
	}
	if status, message, credential, finished := session.snapshot(); finished {
		forgetLoginSession(state)
		if status == pluginapi.AuthLoginStatusSuccess && credential != nil {
			auth, errAuth := loginPollAuth(h, credential)
			if errAuth != nil {
				return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errAuth.Error()}
			}
			return pluginapi.AuthLoginPollResponse{Status: status, Message: message, Auth: auth}
		}
		return pluginapi.AuthLoginPollResponse{Status: status, Message: message}
	}

	product, _ := productByConfigValue(session.Product)

	token := session.cachedToken()
	if token == nil {
		polled, errToken := pollTokenOnce(h, product, state)
		if errToken != nil {
			session.fail(errToken.Error())
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errToken.Error()}
		}
		if polled == nil {
			return pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusPending,
				Message: "等待用户在浏览器完成授权（token 尚未就绪）",
			}
		}
		token = polled
		session.storeToken(token)
	}

	account, errAccount := pollAccountOnce(h, product, state, *token)
	if errAccount != nil {
		session.fail(errAccount.Error())
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errAccount.Error()}
	}
	if account == nil {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "已取得 token，等待账户信息就绪",
		}
	}

	credential := buildCredential(*token, *account, product)
	session.finish(credential, "登录成功")
	forgetLoginSession(state)
	auth, errAuth := loginPollAuth(h, credential)
	if errAuth != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errAuth.Error()}
	}
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "登录成功", Auth: auth}
}

// handleAuthLoginStart answers auth.login.start: it returns the browser URL
// immediately and never blocks on the user.
func handleAuthLoginStart(h *abiboot.Host, _ json.RawMessage) (any, error) {
	result, errStart := startLogin(h, settings())
	if errStart != nil {
		return nil, errStart
	}
	metadata := map[string]any{
		"product": result.Session.Product,
		"source":  "auth/state",
	}
	if result.Fallback {
		metadata["source"] = "fallback"
		metadata["fallback_reason"] = result.FallbackWhy
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       result.AuthURL,
		State:     result.State,
		ExpiresAt: result.ExpiresAt,
		Metadata:  metadata,
	}, nil
}

// handleAuthLoginPoll answers auth.login.poll: one poll step per invocation.
func handleAuthLoginPoll(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthLoginPollRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	if strings.TrimSpace(request.State) == "" {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "缺少 state，无法推进登录",
		}, nil
	}
	return pollLogin(h, request.State), nil
}

// marshalCompact renders a value as compact JSON for log/error messages.
func marshalCompact(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}
