package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// The MiniMax OAuth device-code + PKCE flow.
//
// Ported from `src/minimax-oauth.ts`. There is NO local callback listener: the
// official desktop client authorises through a device code and polls for the
// token, so this plugin never binds a port.
//
// ⚠️ THE trap of this flow: `pending` arrives as **HTTP 200 with
// `status:"pending"`**, NOT as the OAuth-standard `400 +
// error=authorization_pending`. An implementation that only recognises the
// standard form raises an error on the first poll, so the user never gets the
// chance to finish authorising in the browser. BOTH forms are handled below.
//
// ⚠️ And the second trap: `expires_in` is in SECONDS, as is `interval`
// (default 5). The reference records that the vendor's own client divides one
// of them by 1000 in a single branch, which is exactly the kind of slip that
// turns a 5-second poll into a 5-millisecond one — hammering the token
// endpoint. Every duration below is built with `time.Second`.
//
// ⚠️ We have NOT verified this login path against the live server. The
// reference's inference path was measured end to end, but its own login and
// refresh round trip was only ever unit tested: every live probe read the
// desktop client's existing token instead of logging in. There is no MiniMax
// client on this machine either, so the flow below is a faithful port of the
// source, not a measured result.

// deviceAuthorization is one in-flight device-code grant.
type deviceAuthorization struct {
	// DeviceCode is the value polled for a token.
	DeviceCode string
	// CodeVerifier is the PKCE verifier. ⚠️ It is NOT part of the parsed
	// response — the payload parser returns an empty one and the CALLER injects
	// the verifier it generated. Conflating the two produces a poll body whose
	// `code_verifier` is always `""`, which the server rejects.
	CodeVerifier string
	// UserCode is the short code the user types, when the browser flow needs it.
	UserCode string
	// VerificationURI is the bare authorisation page.
	VerificationURI string
	// VerificationURIComplete already embeds `user_code`; it is the link to
	// open, because the user then needs to do nothing but confirm.
	VerificationURIComplete string
	// ExpiresInSeconds is the device code's lifetime.
	ExpiresInSeconds int
	// IntervalSeconds is the poll interval. It is in SECONDS.
	IntervalSeconds int
}

// pkcePair is a PKCE verifier and its S256 challenge.
type pkcePair struct {
	CodeVerifier  string
	CodeChallenge string
}

// newPKCE generates a verifier and its challenge.
//
// The verifier is 32 random bytes, base64url encoded without padding (43
// characters). The challenge is `base64url(sha256(verifier))` — a wrong
// challenge makes the SERVER reject the exchange, and the error it returns
// points at `invalid_grant` / a verifier mismatch rather than at PKCE, which is
// the hardest possible thing to debug. It is therefore exercised directly by a
// test rather than asserted to be merely non-empty.
func newPKCE() (pkcePair, error) {
	raw := make([]byte, 32)
	if _, errRead := rand.Read(raw); errRead != nil {
		return pkcePair{}, abiboot.Errorf("random_failed", "生成 PKCE 随机数失败：%v", errRead)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return pkcePair{
		CodeVerifier:  verifier,
		CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// deviceCodeBody builds the device-code request form.
//
// ⚠️ There is NO `redirect_uri`: the flow has no callback, and sending one is
// not merely unnecessary but absent from the reference
// (`minimax-oauth.ts:buildMinimaxDeviceCodeBody`).
func deviceCodeBody(challenge string) url.Values {
	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("scope", Scope)
	form.Set("audience", Audience)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")
	return form
}

// pollBody builds the device-code token request form.
func pollBody(auth deviceAuthorization) url.Values {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", auth.DeviceCode)
	form.Set("client_id", ClientID)
	form.Set("code_verifier", auth.CodeVerifier)
	return form
}

// refreshBody builds the refresh-token grant form.
//
// MiniMax DOES have a refresh grant (unlike some sibling providers), so a
// credential carrying a refresh token is genuinely renewable. `scope` and
// `audience` ride along exactly as the reference sends them.
func refreshBody(refreshToken string) url.Values {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", ClientID)
	form.Set("scope", Scope)
	form.Set("audience", Audience)
	return form
}

// formHeaders is the header set every OAuth call uses.
func formHeaders() http.Header {
	return canonicalHeader(
		"Accept", "application/json",
		"Content-Type", "application/x-www-form-urlencoded",
	)
}

// parseDeviceAuthorization decodes a device-code response.
//
// `device_code`, `user_code`, `verification_uri` (or the alias
// `verification_url`) and a positive `expires_in` are all required; anything
// missing means the response shape is wrong and the flow must not start.
// `interval` defaults to 5 SECONDS when absent, and `verification_uri_complete`
// falls back to the bare `verification_uri`.
func parseDeviceAuthorization(payload []byte) (deviceAuthorization, error) {
	var record map[string]any
	if errUnmarshal := json.Unmarshal(payload, &record); errUnmarshal != nil {
		return deviceAuthorization{}, transportError("device_code_decode",
			"MiniMax 设备码响应不是 JSON：%v", errUnmarshal)
	}
	deviceCode := nonEmptyString(record["device_code"])
	userCode := nonEmptyString(record["user_code"])
	verificationURI := nonEmptyString(record["verification_uri"])
	if verificationURI == "" {
		verificationURI = nonEmptyString(record["verification_url"])
	}
	expiresIn := positiveInt(record["expires_in"])
	if deviceCode == "" || userCode == "" || verificationURI == "" || expiresIn == 0 {
		return deviceAuthorization{}, transportError("device_code_shape",
			"MiniMax 设备码响应缺少必需字段（device_code / user_code / verification_uri / expires_in 之一）")
	}
	interval := positiveInt(record["interval"])
	if interval == 0 {
		// ⚠️ The fallback is 5 SECONDS, matching the client's own default.
		interval = PollIntervalSeconds
	}
	complete := nonEmptyString(record["verification_uri_complete"])
	if complete == "" {
		complete = verificationURI
	}
	return deviceAuthorization{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: complete,
		ExpiresInSeconds:        expiresIn,
		IntervalSeconds:         interval,
	}, nil
}

// parseTokenGrant decodes a token response and validates it strictly.
//
// The validation is the reference's, verbatim, and each rule catches a real
// failure mode rather than being defensive noise:
//
//   - `access_token` non-empty;
//   - `refresh_token` non-empty, falling back to the PREVIOUS one (a grant that
//     rotates only the access token must not silently drop renewal);
//   - `token_type` present and case-insensitively `bearer`;
//   - `expires_in` positive — it is the ONLY source of the expiry, because the
//     access token is not a JWT;
//   - `scope` CONTAINS `agent.default`. The vendor's authoritative client
//     requires it, and a grant without it is not a usable credential.
//
// The expiry is computed from `expires_in`, never decoded from the token.
func parseTokenGrant(payload []byte, previousRefreshToken string, now time.Time) (*Credential, error) {
	var record map[string]any
	if errUnmarshal := json.Unmarshal(payload, &record); errUnmarshal != nil {
		return nil, transportError("token_decode", "MiniMax 令牌响应不是 JSON：%v", errUnmarshal)
	}
	accessToken := nonEmptyString(record["access_token"])
	refreshToken := nonEmptyString(record["refresh_token"])
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(previousRefreshToken)
	}
	tokenType := nonEmptyString(record["token_type"])
	expiresIn := positiveInt(record["expires_in"])
	switch {
	case accessToken == "":
		return nil, transportError("token_shape", "MiniMax 令牌响应缺少 access_token")
	case refreshToken == "":
		return nil, transportError("token_shape", "MiniMax 令牌响应缺少 refresh_token（且没有可沿用的旧值）")
	case tokenType == "" || !strings.EqualFold(tokenType, "bearer"):
		return nil, transportError("token_shape", "MiniMax 令牌响应的 token_type 不是 Bearer（收到 %q）", tokenType)
	case expiresIn == 0:
		return nil, transportError("token_shape",
			"MiniMax 令牌响应缺少 expires_in；access_token 不是 JWT，没有它就无法计算过期时间")
	}
	scope := ""
	if text, ok := record["scope"].(string); ok {
		scope = text
	}
	if !scopeContains(scope, Scope) {
		return nil, transportError("token_scope",
			"MiniMax 令牌响应的 scope 不含 %s（收到 %q）", Scope, scope)
	}
	expiresAt := now.Add(time.Duration(expiresIn) * time.Second)
	credential := &Credential{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
		ExpiresAt:    strconv.FormatInt(expiresAt.UnixMilli(), 10),
		Scope:        scope,
		Type:         ProviderKey,
	}
	// ⚠️ Compatibility only: a real access_token is not a JWT, so `sub` is
	// normally absent and no account id is invented.
	if subject := jwtSubject(accessToken); subject != "" {
		credential.AccountID = subject
	}
	return credential, nil
}

// scopeContains reports whether a space-separated scope string names wanted.
func scopeContains(scope, wanted string) bool {
	for _, item := range strings.Fields(scope) {
		if item == wanted {
			return true
		}
	}
	return false
}

// pollDecision is what one token-endpoint answer means.
type pollDecision int

const (
	// pollPending means the user has not finished authorising yet.
	pollPending pollDecision = iota
	// pollSlowDown means the same, and the interval must grow by 5 s.
	pollSlowDown
	// pollSuccess means the payload carries a usable grant.
	pollSuccess
	// pollDenied means the user refused.
	pollDenied
	// pollExpired means the device code is no longer valid.
	pollExpired
	// pollFailed means the answer is an unrecognised failure.
	pollFailed
)

// classifyPoll interprets one token-endpoint answer.
//
// This is the single most important function in the login flow, and it exists
// because MiniMax uses BOTH pending conventions:
//
//	form 1  HTTP 200 + {"status":"pending"}   ← what the vendor actually sends
//	form 2  HTTP 400 + {"error":"authorization_pending"}  ← the OAuth standard
//
// Recognising only form 2 makes the very first poll a hard error, so the user —
// who is still looking at the browser page — can never complete the login.
// `slow_down` likewise exists in both forms and adds 5 s to the interval.
func classifyPoll(status int, payload []byte) (pollDecision, string) {
	var record map[string]any
	if len(payload) > 0 {
		if errUnmarshal := json.Unmarshal(payload, &record); errUnmarshal != nil {
			record = nil
		}
	}
	statusText := nonEmptyString(record["status"])
	errorText := nonEmptyString(record["error"])
	ok := status >= 200 && status < 300

	// Form 1: HTTP 200 carrying a `status` field.
	if ok {
		switch statusText {
		case "pending":
			return pollPending, "等待用户在浏览器中完成授权"
		case "slow_down":
			return pollSlowDown, "服务端要求放慢轮询"
		case "denied", "access_denied":
			return pollDenied, "用户在浏览器中拒绝了 MiniMax 授权"
		case "expired", "expired_token":
			return pollExpired, "MiniMax 设备码已过期，请重新发起登录"
		}
	}

	// Form 2: the OAuth-standard error codes, regardless of status code.
	switch errorText {
	case "authorization_pending":
		return pollPending, "等待用户在浏览器中完成授权"
	case "slow_down":
		return pollSlowDown, "服务端要求放慢轮询"
	case "access_denied":
		return pollDenied, "用户在浏览器中拒绝了 MiniMax 授权"
	case "expired_token":
		return pollExpired, "MiniMax 设备码已过期，请重新发起登录"
	}

	if ok && statusText == "" {
		// A 2xx with neither a `status` nor an `error` is a plain grant.
		return pollSuccess, ""
	}
	if ok {
		// A 2xx that names a status we do not know: refuse to guess. Treating
		// an unknown status as success would store a credential that does not
		// exist; treating it as pending would poll forever.
		return pollFailed, "MiniMax 授权返回了未知状态 " + statusText
	}
	detail := errorText
	if detail == "" {
		detail = truncate(string(payload), 200)
	}
	if detail == "" {
		detail = "HTTP " + strconv.Itoa(status)
	}
	return pollFailed, "MiniMax 授权失败：" + detail
}

// requestDeviceAuthorization performs the device-code request.
func requestDeviceAuthorization(h *abiboot.Host, cfg Config) (deviceAuthorization, error) {
	pkce, errPKCE := newPKCE()
	if errPKCE != nil {
		return deviceAuthorization{}, errPKCE
	}
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.oauthTimeout())*time.Millisecond,
		http.MethodPost, AccountHost+DeviceCodePath, formHeaders(),
		[]byte(deviceCodeBody(pkce.CodeChallenge).Encode()))
	if errDo != nil {
		return deviceAuthorization{}, transportError("device_code_transport", "申请 MiniMax 设备码失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return deviceAuthorization{}, upstreamStatusError("device_code_failed", response.StatusCode,
			"申请 MiniMax 设备码返回 HTTP %d：%s", response.StatusCode, truncate(string(response.Body), 300))
	}
	auth, errParse := parseDeviceAuthorization(response.Body)
	if errParse != nil {
		return deviceAuthorization{}, errParse
	}
	// ⚠️ The verifier is injected HERE, not by the parser: the response cannot
	// carry it, and a parser that returned its own empty value would turn every
	// poll body into `code_verifier=""`.
	auth.CodeVerifier = pkce.CodeVerifier
	return auth, nil
}

// pollDeviceToken performs ONE token-endpoint poll.
//
// It never sleeps and never loops: a host invocation must return promptly, so
// the interval is honoured by the caller against the previous attempt. That is
// also why this function returns the decision rather than retrying internally.
func pollDeviceToken(h *abiboot.Host, cfg Config, auth deviceAuthorization) (pollDecision, string, *Credential, error) {
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.oauthTimeout())*time.Millisecond,
		http.MethodPost, AccountHost+TokenPath, formHeaders(), []byte(pollBody(auth).Encode()))
	if errDo != nil {
		return pollFailed, "", nil, transportError("token_transport", "MiniMax 令牌轮询失败：%v", errDo)
	}
	decision, message := classifyPoll(response.StatusCode, response.Body)
	if decision != pollSuccess {
		return decision, message, nil, nil
	}
	credential, errParse := parseTokenGrant(response.Body, "", nowTime())
	if errParse != nil {
		return pollFailed, "", nil, errParse
	}
	return pollSuccess, "登录成功", credential, nil
}

// refreshCredential performs one refresh-token grant.
//
// The previous refresh token is carried forward when the grant does not rotate
// it, so a credential that receives only a new access token keeps its renewal
// path. On a dead grant the error is classified as a credential failure, which
// is what lets the freshness layer mark the credential terminal instead of
// retrying forever.
func refreshCredential(h *abiboot.Host, cfg Config, credential *Credential) (*Credential, error) {
	refreshToken := strings.TrimSpace(credential.RefreshToken)
	if refreshToken == "" {
		return nil, credentialError("not_refreshable", "该 MiniMax 账号缺少 refresh_token，请重新登录")
	}
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.oauthTimeout())*time.Millisecond,
		http.MethodPost, AccountHost+TokenPath, formHeaders(), []byte(refreshBody(refreshToken).Encode()))
	if errDo != nil {
		return nil, transportError("refresh_transport", "MiniMax 续期请求失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := refreshErrorText(response.Body, response.StatusCode)
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden ||
			isTerminalRefreshError(response.Body) {
			return nil, credentialError("refresh_token_expired",
				"MiniMax refresh_token 已失效，请重新登录：%s", detail)
		}
		return nil, upstreamStatusError("refresh_failed", response.StatusCode,
			"MiniMax 续期返回 HTTP %d：%s", response.StatusCode, detail)
	}
	refreshed, errParse := parseTokenGrant(response.Body, refreshToken, nowTime())
	if errParse != nil {
		return nil, errParse
	}
	// The account identity the old credential established is not re-derived
	// from a non-JWT token, so an existing one is carried forward.
	refreshed.AccountID = firstNonEmpty(refreshed.AccountID, credential.AccountID)
	refreshed.Nickname = credential.Nickname
	return refreshed, nil
}

// refreshErrorText renders the `error` member of a failed refresh answer.
func refreshErrorText(payload []byte, status int) string {
	var record map[string]any
	if errUnmarshal := json.Unmarshal(payload, &record); errUnmarshal == nil {
		if text := nonEmptyString(record["error"]); text != "" {
			if description := nonEmptyString(record["error_description"]); description != "" {
				return text + " — " + description
			}
			return text
		}
	}
	if body := truncate(string(payload), 300); body != "" {
		return body
	}
	return "HTTP " + strconv.Itoa(status)
}

// terminalRefreshErrors are the OAuth error codes that mean "log in again".
//
// ⚠️ Reading these is what keeps a dead grant from being retried forever: the
// scheduler treats a credential failure as terminal, and a `400
// invalid_grant` is exactly as dead as a 401 — the vendor just chose a 400.
var terminalRefreshErrors = map[string]bool{
	"invalid_grant":          true,
	"expired_token":          true,
	"invalid_refresh_token":  true,
	"refresh_token_expired":  true,
	"unauthorized_client":    true,
	"invalid_token_response": true,
}

// isTerminalRefreshError reports whether the answer names a dead grant.
func isTerminalRefreshError(payload []byte) bool {
	var record map[string]any
	if errUnmarshal := json.Unmarshal(payload, &record); errUnmarshal != nil {
		return false
	}
	code := strings.ToLower(nonEmptyString(record["error"]))
	return code != "" && terminalRefreshErrors[code]
}

// ── small readers ──

// nonEmptyString reads a trimmed, non-empty string member.
func nonEmptyString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// positiveInt reads a positive integer, accepting either a JSON number or a
// numeric string. Non-finite and non-positive values read as 0 ("absent").
func positiveInt(value any) int {
	switch typed := value.(type) {
	case float64:
		if typed <= 0 || typed != typed { // NaN check without importing math
			return 0
		}
		return int(typed)
	case int:
		if typed <= 0 {
			return 0
		}
		return typed
	case int64:
		if typed <= 0 {
			return 0
		}
		return int(typed)
	case json.Number:
		parsed, errParse := strconv.ParseFloat(string(typed), 64)
		if errParse != nil || parsed <= 0 || parsed != parsed {
			return 0
		}
		return int(parsed)
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if errParse != nil || parsed <= 0 || parsed != parsed {
			return 0
		}
		return int(parsed)
	default:
		return 0
	}
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
