package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// In-plugin login (no ZCode IDE required).
//
// ## Protocol
//
// The official 3.12.3+ desktop client uses a SERVER-MEDIATED device
// authorization flow — it never goes through the `zcode://` custom protocol, so
// an ordinary process can complete it (`zcode-login.ts:1-30`):
//
//	① POST /api/v1/oauth/cli/init
//	     Authorization: Bearer <a 32-byte hex session key WE generate>
//	     body: {"provider": "bigmodel"}
//	   -> {code, data: {flow_id, poll_token, authorize_url, expires_at,
//	                    poll_interval_sec}}
//
//	② the user opens authorize_url and authorizes in a browser.
//	   Authorization completes SERVER-SIDE and is posted back to
//	   /oauth/cli/callback/bigmodel — there is no local listener here.
//
//	③ GET /api/v1/oauth/cli/poll/<flow_id>
//	     -> {"code":0,"data":{"status":"pending"}}
//	     -> {"code":0,"data":{"status":"ready","token":…,
//	                          "user":{…},"bigmodel":{"access_token":…}}}
//
// The bearer in ① is a session key this plugin generates, NOT a user credential;
// the official client does exactly the same. ③ does not require the poll token
// (measured: it returns 200 without it), so it is kept only for diagnostics.
//
// ## Why this path at all
//
// It avoids the broken `POST /api/v1/oauth/token`, which the reference recorded as
// answering 500 / code 2007 steadily: the token arrives through the poll and never
// touches that endpoint.
//
// ⚠ `X-Device-Mid` is a hard requirement on the billing endpoints (`400 code
// 3001` without it) while its VALUE is not bound or validated — the measured
// matrix in the reference shows the official id, a random UUID and a fresh random
// UUID all returning 200, and only the omission failing. The plugin therefore
// generates its own device id at login and persists it in the credential, which
// is what removes the dependency on the official client's
// `telemetry-state.json`.

// loginProvider is the only provider this plugin starts a flow for. The
// reference also knows `zai`, which differs only in whether the authorize URL
// carries `redirect=` or `redirect_uri=`.
const loginProvider = "bigmodel"

// loginFlow is the parsed ① response.
type loginFlow struct {
	FlowID string
	// PollToken is kept for diagnostics only; ③ does not authenticate with it.
	PollToken string
	// AuthorizeURL is what the user opens.
	AuthorizeURL string
	// ExpiresAt is the server's Unix-second deadline (0 when absent).
	ExpiresAt int64
	// PollIntervalMS is the server's requested interval in milliseconds.
	PollIntervalMS int
	// FlowSecret is the CLI session key used as the bearer.
	FlowSecret string
}

// loginResult is the parsed ③ `status: "ready"` payload.
type loginResult struct {
	ZCodeJWT             string
	BigModelAccessToken  string
	BigModelRefreshToken string
	UserID               string
	DisplayName          string
}

// loginPollKind classifies one poll answer.
type loginPollKind int

const (
	// loginPollPending keeps the flow alive.
	loginPollPending loginPollKind = iota
	// loginPollReady carries a completed credential.
	loginPollReady
	// loginPollFailed is terminal.
	loginPollFailed
)

// handleAuthLoginStart begins the device authorization and returns immediately.
//
// Nothing here blocks on the user: the URL goes back to the caller in the same
// invocation, and `auth.login.poll` collects the token afterwards.
func handleAuthLoginStart(h *abiboot.Host, raw json.RawMessage) (any, error) {
	cfg := settings()
	session, errStart := startLoginSession()
	if errStart != nil {
		return nil, errStart
	}
	flow, errInit := startLoginFlow(h, cfg)
	if errInit != nil {
		forgetLoginSession(session.State)
		return nil, errInit
	}
	session.configure(flow)
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       flow.AuthorizeURL,
		State:     session.State,
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"flow_id": flow.FlowID,
			"hint": "在浏览器中打开 URL 完成授权后，由 auth.login.poll 轮询取回凭据；" +
				"授权在服务端完成，本插件不监听任何本地端口",
			"poll_interval_ms": maxInt(flow.PollIntervalMS, 1_000),
			"device_mid_note": "设备标识由本插件生成并随凭据持久化；" +
				"它在 billing 接口上是必需项，但它的值不被服务端校验",
		},
	}, nil
}

// startLoginFlow performs step ① — the init request.
func startLoginFlow(h *abiboot.Host, cfg Config) (*loginFlow, error) {
	flowSecret, errSecret := randomHex(32)
	if errSecret != nil {
		return nil, errSecret
	}
	appVersion := cfg.AppVersion
	if appVersion == "" {
		appVersion = DefaultAppVersion
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+flowSecret)
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", "ZCode/"+appVersion)
	headers.Set("HTTP-Referer", Origin)
	headers.Set("X-ZCode-App-Version", appVersion)
	headers.Set("X-Platform", cfg.Platform)
	headers.Set("Accept", "application/json")

	request, errEncode := reencodeBody(map[string]any{"provider": loginProvider})
	if errEncode != nil {
		return nil, errEncode
	}
	response, errDo := hostRequest(h, http.MethodPost, Origin+OAuthCLIInitPath, headers, request)
	if errDo != nil {
		return nil, transportError("login_init_transport", "无法连接 ZCode 授权服务：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, gatewayError("login_init_failed", response,
			"授权初始化失败（HTTP %d）", response.StatusCode)
	}

	parsed := parseUpstreamEnvelope(response.Body, response.StatusCode)
	data, errData := envelopeData(parsed, response.Body)
	if errData != nil {
		return nil, errData
	}
	flowID := stringField(data, "flow_id")
	if flowID == "" {
		return nil, transportError("login_init_incomplete",
			"授权初始化响应缺 flow_id（%s）", truncate(parsed.message(string(response.Body)), 160))
	}
	authorizeURL := stringField(data, "authorize_url")
	if !strings.HasPrefix(authorizeURL, "https://") {
		return nil, transportError("login_init_incomplete",
			"授权初始化响应缺合法的 authorize_url（%s）", truncate(parsed.message(string(response.Body)), 160))
	}
	expiresAt, _ := nonNegativeInt(data["expires_at"])
	intervalSeconds, hasInterval := nonNegativeInt(data["poll_interval_sec"])
	intervalMS := DefaultPollIntervalMS
	// The reference validates the server's interval: at least one second, and
	// less than the flow's own lifetime. Anything else falls back so a nonsensical
	// value cannot turn the poll loop into a hot loop.
	if hasInterval && intervalSeconds >= 1 {
		intervalMS = intervalSeconds * 1_000
	}
	return &loginFlow{
		FlowID:         flowID,
		PollToken:      stringField(data, "poll_token"),
		AuthorizeURL:   authorizeURL,
		ExpiresAt:      int64(expiresAt),
		PollIntervalMS: intervalMS,
		FlowSecret:     flowSecret,
	}, nil
}

// handleAuthLoginPoll advances one in-flight login by at most one upstream call.
//
// The JavaScript loop sleeps between attempts; a host invocation must not block
// that long, so the interval is enforced against the previous attempt and the
// call simply answers "pending" instead.
func handleAuthLoginPoll(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthLoginPollRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	session, found := lookupLoginSession(strings.TrimSpace(request.State))
	if !found {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已超时，请重新发起登录",
		}, nil
	}

	if status, message, done := session.snapshot(); done {
		defer forgetLoginSession(request.State)
		if status == pluginapi.AuthLoginStatusSuccess {
			if credential := session.storedCredential(); credential != nil {
				auth, errAuth := authDataFor(credential, "")
				if errAuth != nil {
					return nil, errAuth
				}
				return pluginapi.AuthLoginPollResponse{Status: status, Message: message, Auth: auth}, nil
			}
		}
		if status == "" {
			status = pluginapi.AuthLoginStatusError
		}
		return pluginapi.AuthLoginPollResponse{Status: status, Message: message}, nil
	}

	cfg := settings()
	interval := time.Duration(maxInt(sessionPollInterval(session), cfg.PollIntervalMS)) * time.Millisecond
	if interval <= 0 {
		interval = DefaultPollIntervalMS * time.Millisecond
	}
	if !session.due(interval) {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户完成授权",
		}, nil
	}
	session.markAttempt()

	outcome, result, detail := pollLoginFlow(h, session, cfg)
	switch outcome {
	case loginPollPending:
		session.resetFailures()
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: detail,
		}, nil
	case loginPollFailed:
		session.fail(detail)
		forgetLoginSession(request.State)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: detail}, nil
	}

	session.resetFailures()
	credential := credentialFromLogin(result, cfg)
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		return nil, errAuth
	}
	session.setCredential(credential, "登录成功")
	session.finish()
	forgetLoginSession(request.State)
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth:    auth,
	}, nil
}

// sessionPollInterval reads the interval the server asked for.
func sessionPollInterval(session *loginSession) int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.PollInterval
}

// pollLoginFlow performs step ③ and classifies the answer.
//
// Failure rules, taken verbatim from the reference:
//
//   - HTTP 4xx EXCEPT 408 and 429 is TERMINAL. Those two are transient by
//     definition, and a 5xx or a transport error keeps the flow alive.
//   - a body that is not `code == 0` with data keeps polling: the server answers
//     that way while the user has not finished.
//   - `status: "failed"` is the user declining or the authorization failing.
//   - `status: "ready"` requires ALL THREE of `token`, `bigmodel.access_token`
//     and `user.user_id`; a ready answer missing one of them is reported as a
//     failure rather than stored as a half-credential.
func pollLoginFlow(h *abiboot.Host, session *loginSession, cfg Config) (loginPollKind, *loginResult, string) {
	session.mu.Lock()
	flowSecret := session.FlowSecret
	pollURL := Origin + OAuthCLIPollPrefix + session.FlowID
	session.mu.Unlock()

	appVersion := cfg.AppVersion
	if appVersion == "" {
		appVersion = DefaultAppVersion
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+flowSecret)
	headers.Set("User-Agent", "ZCode/"+appVersion)
	headers.Set("HTTP-Referer", Origin)
	headers.Set("X-ZCode-App-Version", appVersion)
	headers.Set("Accept", "application/json")

	response, errDo := hostRequest(h, http.MethodGet, pollURL, headers, nil)
	if errDo != nil {
		// A network hiccup is NOT a failure: return pending so the caller keeps
		// polling, which is what the reference does.
		return loginPollPending, nil, "网络抖动，继续等待：" + errDo.Error()
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 &&
		response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusTooManyRequests {
		return loginPollFailed, nil, "轮询被拒（HTTP " + strconv.Itoa(response.StatusCode) + "）：" +
			truncate(string(response.Body), 200)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return loginPollPending, nil, "登录服务暂时不可用（HTTP " + strconv.Itoa(response.StatusCode) + "），继续等待"
	}

	parsed := parseUpstreamEnvelope(response.Body, response.StatusCode)
	if !parsed.success() {
		return loginPollPending, nil, "等待用户在浏览器中完成授权"
	}
	data, errData := envelopeData(parsed, response.Body)
	if errData != nil {
		return loginPollPending, nil, "登录服务尚未下发数据，继续等待"
	}
	switch status := stringField(data, "status"); status {
	case "pending", "":
		return loginPollPending, nil, "等待用户在浏览器中完成授权"
	case "failed":
		return loginPollFailed, nil, "用户拒绝了授权或授权失败"
	case "ready":
		result, errResult := loginResultFrom(data, response.Body)
		if errResult != nil {
			return loginPollFailed, nil, errResult.Error()
		}
		return loginPollReady, result, "登录成功"
	default:
		return loginPollFailed, nil, "轮询响应状态无法识别：" + status
	}
}

// loginResultFrom reads the three mandatory fields of a ready answer.
func loginResultFrom(data map[string]any, body []byte) (*loginResult, error) {
	bigmodel, _ := data["bigmodel"].(map[string]any)
	user, _ := data["user"].(map[string]any)
	result := &loginResult{
		ZCodeJWT:            stringField(data, "token"),
		BigModelAccessToken: firstNonEmpty(stringField(bigmodel, "access_token"), stringField(bigmodel, "accessToken")),
		UserID:              firstNonEmpty(stringField(user, "user_id"), stringField(user, "id")),
		DisplayName:         firstNonEmpty(stringField(user, "name"), stringField(user, "email")),
	}
	result.BigModelRefreshToken = firstNonEmpty(
		stringField(bigmodel, "refresh_token"), stringField(bigmodel, "refreshToken"))
	if result.ZCodeJWT == "" || result.BigModelAccessToken == "" || result.UserID == "" {
		return nil, transportError("login_incomplete",
			"轮询响应缺关键字段（token / bigmodel.access_token / user.user_id）：%s",
			truncate(string(body), 200))
	}
	if result.DisplayName == "" {
		result.DisplayName = result.UserID
	}
	return result, nil
}

// credentialFromLogin assembles the credential a successful login produces.
func credentialFromLogin(result *loginResult, cfg Config) *Credential {
	appVersion := cfg.AppVersion
	if appVersion == "" {
		appVersion = DefaultAppVersion
	}
	credential := &Credential{
		ZCodeJWT:             result.ZCodeJWT,
		DeviceMid:            generateDeviceMid(),
		UserID:               result.UserID,
		BigModelAccessToken:  result.BigModelAccessToken,
		BigModelRefreshToken: result.BigModelRefreshToken,
		AccountLabel:         result.DisplayName,
		AppVersion:           appVersion,
		Source:               "plugin",
	}
	return credential
}

// envelopeData extracts the `data` object of a business envelope, tolerating a
// `data` that is absent, null or not an object.
func envelopeData(envelope *upstreamEnvelope, _ []byte) (map[string]any, error) {
	if envelope == nil {
		return map[string]any{}, nil
	}
	trimmed := strings.TrimSpace(string(envelope.Data))
	if trimmed == "" || trimmed == "null" || !strings.HasPrefix(trimmed, "{") {
		return map[string]any{}, nil
	}
	var data map[string]any
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&data); errDecode != nil {
		return nil, transportError("bad_envelope", "解析响应 data 失败：%v", errDecode)
	}
	if data == nil {
		return map[string]any{}, nil
	}
	return data, nil
}

// gatewayError renders a non-2xx answer from a non-inference endpoint.
func gatewayError(code string, response *pluginapi.HTTPResponse, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	envelope := parseUpstreamEnvelope(response.Body, response.StatusCode)
	if detail := envelope.message(""); detail != "" {
		message += "：" + detail
	}
	return transportError(code, "%s", message)
}

// firstNonEmpty returns the first non-empty value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// maxInt returns the larger of two ints.
func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
