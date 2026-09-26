package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The HTTP layer: the header matrix, the per-endpoint timeouts and the four
// calls the plugin makes outside inference.
//
// ⚠️ The header sets GENUINELY DIFFER per endpoint and are not interchangeable
// (`raccoon.ts:271-293`, `raccoon-adapter.ts:297-304`, `raccoon-oauth.ts:83-96`,
// `raccoon-credits.ts:87-90`). Three details are easy to get wrong and each is
// pinned by a unit test:
//
//   - auth endpoints send ONLY `Content-Type`. Adding `Accept` or `Authorization`
//     there deviates from the reference (trap #11);
//   - `X-Org-Code` is ALWAYS sent, even when it is the empty string, because
//     personal accounts have no `office_identity` and the header is still set
//     (`raccoon.ts:279-280`);
//   - `X-Client-Version` is sent on credits calls and NEVER on chat, and
//     `userAgent`/`device_id` are dead configuration that must not be invented
//     (`raccoon-product.ts:159`, trap #29).

// hostRequest performs one buffered upstream call through the host transport, so
// proxy, TLS and request logging stay under the host's control.
//
// This is the seam every test replaces: `abiboot.SetHostCaller` swaps the whole
// transport for an in-process stub, which is why no test touches the network.
func hostRequest(h *abiboot.Host, method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
	if h == nil {
		return nil, transportError("host_unavailable", "插件未通过宿主调用（缺少 host 句柄）")
	}
	return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: rawURL, Headers: headers, Body: body})
}

// hostRequestTimeout performs one buffered upstream call with a wall-clock
// budget.
//
// The host's HTTP surface carries no timeout of its own, so the budget is
// enforced here. The worker keeps running until the host call returns (its own
// transport always terminates), and the result is discarded on timeout; the
// channel is buffered so that worker can never block on a reader that left.
func hostRequestTimeout(h *abiboot.Host, budget time.Duration, method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
	if budget <= 0 {
		return hostRequest(h, method, rawURL, headers, body)
	}
	type outcome struct {
		response *pluginapi.HTTPResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, errDo := hostRequest(h, method, rawURL, headers, body)
		done <- outcome{response: response, err: errDo}
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.response, result.err
	case <-timer.C:
		return nil, statusError(true, "upstream_timeout", http.StatusGatewayTimeout,
			"Raccoon 上游超时（%s 秒）：%s", trimSeconds(budget), rawURL)
	}
}

// trimSeconds renders a duration in whole seconds for a message.
func trimSeconds(budget time.Duration) string {
	return strconv.FormatFloat(budget.Seconds(), 'f', 0, 64)
}

// authJSONHeaders is the header set of EVERY auth endpoint: `Content-Type` and
// nothing else (`raccoon-oauth.ts:83-96`).
func authJSONHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	return headers
}

// raccoonHeaders is the shared authenticated header set
// (`raccoonHeaders`, `raccoon.ts:271-293`).
//
// `platform` and `version` are omitted when empty, exactly as the reference
// conditionally adds them; `X-Org-Code` is always present, empty or not.
func raccoonHeaders(credential *Credential, platform, version string) http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+credential.Session())
	officeIdentity := ""
	if credential != nil {
		officeIdentity = strings.TrimSpace(credential.OfficeIdentity)
	}
	headers.Set("X-Org-Code", officeIdentity)
	headers.Set("X-Raccoon-Language", Language)
	if strings.TrimSpace(platform) != "" {
		headers.Set("X-Client-Platform", platform)
	}
	if strings.TrimSpace(version) != "" {
		headers.Set("X-Client-Version", version)
	}
	return headers
}

// profileHeaders is the `user_info` set: authenticated, but neither platform nor
// version.
func profileHeaders(credential *Credential) http.Header {
	return raccoonHeaders(credential, "", "")
}

// catalogueHeaders is the `model_catalog` set. It carries NO `Content-Type`:
// the request has no body (`raccoon-auth.ts:392-401`).
func catalogueHeaders(credential *Credential) http.Header {
	headers := raccoonHeaders(credential, "", "")
	headers.Del("Content-Type")
	return headers
}

// chatHeaders is the chat/completions set (`raccoon-adapter.ts:297-304`).
//
// ⚠️ `Accept: text/event-stream` replaces the JSON accept, the platform is
// pinned, and `X-Client-Version` is NOT sent here.
func chatHeaders(credential *Credential) http.Header {
	headers := raccoonHeaders(credential, ClientPlatform, "")
	headers.Set("Accept", "text/event-stream")
	return headers
}

// creditsHeaders is the points/desktop set: the full authenticated set WITH the
// platform and the client version (`raccoon-credits.ts:87-90`). The one-off
// login reward is rejected without `X-Client-Platform`.
func creditsHeaders(credential *Credential) http.Header {
	return raccoonHeaders(credential, ClientPlatform, ClientVersion)
}

// qrPollOutcome is one poll of a scanned QR code.
type qrPollOutcome struct {
	// Status is one of the four wire statuses, or `pending` for every degraded
	// case (network error, envelope failure, missing/unknown status, success
	// without a token).
	Status string
	// Detail explains a degraded outcome for the page and the logs.
	Detail string
	// ExpiredAt is `data.expired_at` when the server sent it as a string
	// (`logging` carries it; the reference uses it only as a countdown).
	ExpiredAt string
	// Credential is set only when Status is `success`.
	Credential *Credential
}

// pollQRCodeOnce performs ONE `login_with_qrcode_code` poll.
//
// The code was generated locally; the server only reports what a WeChat scan
// produced (`raccoon-oauth.ts:140-180`).
func pollQRCodeOnce(h *abiboot.Host, cfg Config, code string) qrPollOutcome {
	payload, errMarshal := json.Marshal(map[string]string{"qrcode_code": code})
	if errMarshal != nil {
		return qrPollOutcome{Status: qrStatusPending, Detail: "编码请求失败：" + errMarshal.Error()}
	}
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.requestTimeout())*time.Millisecond,
		http.MethodPost, APIBase+QRLoginCodePath, authJSONHeaders(), payload)
	if errDo != nil {
		// ⚠️ A network error NEVER aborts the loop; it degrades to pending
		// (`raccoon-oauth.ts:153-155`).
		return qrPollOutcome{Status: qrStatusPending, Detail: "网络异常：" + errDo.Error()}
	}
	parsed, errEnvelope := parseEnvelope(response.Body, response.StatusCode)
	if errEnvelope != nil || !parsed.success() {
		// `code !== 0` or a missing `data` both mean "keep waiting"
		// (`raccoon-oauth.ts:156`).
		detail := ""
		if errEnvelope != nil {
			detail = errEnvelope.Error()
		} else {
			detail = parsed.text("等待扫码")
		}
		return qrPollOutcome{Status: qrStatusPending, Detail: detail}
	}
	data := parsed.dataObject()
	status, isText := data["status"].(string)
	if !isText || strings.TrimSpace(status) == "" {
		// A missing or non-string status is pending, never guessed
		// (`raccoon-oauth.ts:159`).
		return qrPollOutcome{Status: qrStatusPending, Detail: "服务端未返回 status 字段"}
	}
	outcome := qrPollOutcome{Status: qrStatusPending}
	if expired, ok := data["expired_at"].(string); ok {
		outcome.ExpiredAt = strings.TrimSpace(expired)
	}
	switch strings.TrimSpace(status) {
	case qrStatusPending:
		// The literal `pending` the server actually returns for an unscanned
		// code. It is handled EXPLICITLY so the page does not report a known
		// state as unknown (observed live: the vendor answers `pending`).
		outcome.Status = qrStatusPending
		outcome.Detail = "服务端状态 pending：等待扫码"
		return outcome
	case qrStatusCanceled:
		outcome.Status = qrStatusCanceled
		outcome.Detail = "本次扫码已在手机端取消"
		return outcome
	case qrStatusLogging:
		outcome.Status = qrStatusLogging
		outcome.Detail = "已扫码，等待手机确认"
		return outcome
	case qrStatusSuccess:
		accessToken, _ := data["access_token"].(string)
		if strings.TrimSpace(accessToken) == "" {
			// ⚠️ A `success` without a token would produce an EMPTY credential,
			// so it degrades to pending (`raccoon-oauth.ts:169-170`).
			outcome.Detail = "服务端报告 success 但没有 access_token，继续等待"
			return outcome
		}
		refreshToken, _ := data["refresh_token"].(string)
		officeIdentity, _ := data["office_identity"].(string)
		outcome.Status = qrStatusSuccess
		outcome.Detail = "登录成功"
		outcome.Credential = buildCredential(accessToken, refreshToken, officeIdentity)
		return outcome
	default:
		// Any other string is unknown and must NOT be guessed as success or
		// canceled (`raccoon-oauth.ts:179`).
		outcome.Detail = "未知状态 " + strings.TrimSpace(status) + "，继续等待"
		return outcome
	}
}

// refreshCredential exchanges the refresh token for a new access token.
//
// Semantics that matter (trap #14):
//   - HTTP 401 or code 200003 is TERMINAL: the session is gone, no retry;
//   - a response WITHOUT `refresh_token` keeps the OLD one, otherwise the
//     account becomes unrefreshable after a single rotation;
//   - every other field is preserved, so nickname/identity/phone survive;
//   - `expires_at` is recomputed from the new JWT and omitted when it does not
//     decode.
func refreshCredential(h *abiboot.Host, cfg Config, current *Credential) (*Credential, error) {
	if current == nil {
		return nil, credentialError("missing_credential", "Raccoon 凭据缺失，请重新登录")
	}
	if !current.Refreshable() {
		return nil, credentialError("missing_refresh_token", "凭据缺少 refresh_token，请重新登录")
	}
	payload, errMarshal := json.Marshal(map[string]string{"refresh_token": strings.TrimSpace(current.RefreshToken)})
	if errMarshal != nil {
		return nil, statusError(false, "encode_request", http.StatusInternalServerError, "编码续期请求失败：%v", errMarshal)
	}
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.requestTimeout())*time.Millisecond,
		http.MethodPost, APIBase+RefreshPath, authJSONHeaders(), payload)
	if errDo != nil {
		return nil, transportError("refresh_transport", "连接 Raccoon 续期接口失败：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized {
		return nil, credentialError("auth_expired", "Raccoon 登录态已过期，请重新登录（HTTP 401）")
	}
	parsed, errEnvelope := parseEnvelope(response.Body, response.StatusCode)
	if errEnvelope != nil {
		return nil, errEnvelope
	}
	if parsed.code() == codeAuthorizationVerifyError {
		return nil, credentialError("auth_expired", "Raccoon 登录态已过期，请重新登录（code %d）", codeAuthorizationVerifyError)
	}
	if !parsed.success() {
		return nil, envelopeFailure("Raccoon 续期", parsed, true)
	}
	data := parsed.dataObject()
	accessToken := stringField(data, "access_token")
	if accessToken == "" {
		return nil, transportError("refresh_no_token", "续期响应缺少 access_token")
	}
	merged := *current
	merged.AccessToken = accessToken
	// Keep the old refresh token when the server omits it; an empty string is
	// "omitted" here.
	if rotated := stringField(data, "refresh_token"); rotated != "" {
		merged.RefreshToken = rotated
	}
	merged.ExpiresAt = ""
	if expiry := jwtExpiryMS(merged.AccessToken); expiry > 0 {
		merged.ExpiresAt = itoaInt64(expiry)
	}
	return &merged, nil
}

// userInfo is the subset of `GET /user_info` the credential needs
// (`persistLogin`, `raccoon-auth.ts:220-238`).
type userInfo struct {
	ID             string
	Name           string
	Phone          string
	OfficeIdentity string
}

// fetchUserInfo reads the account profile.
func fetchUserInfo(h *abiboot.Host, cfg Config, credential *Credential) (userInfo, error) {
	response, errDo := hostRequestTimeout(h, time.Duration(cfg.requestTimeout())*time.Millisecond,
		http.MethodGet, APIBase+UserInfoPath, profileHeaders(credential), nil)
	if errDo != nil {
		return userInfo{}, transportError("user_info_transport", "读取 Raccoon 账号信息失败：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return userInfo{}, credentialError("auth_expired", "Raccoon 凭据已失效（HTTP %d），请重新登录", response.StatusCode)
	}
	parsed, errEnvelope := parseEnvelope(response.Body, response.StatusCode)
	if errEnvelope != nil {
		return userInfo{}, errEnvelope
	}
	if !parsed.success() {
		return userInfo{}, envelopeFailure("读取 Raccoon 账号信息", parsed, true)
	}
	data := parsed.dataObject()
	if data == nil {
		return userInfo{}, transportError("user_info_empty", "Raccoon 账号信息响应缺少 data")
	}
	info := userInfo{
		ID:             stringField(data, "id"),
		Name:           stringField(data, "name"),
		Phone:          stringField(data, "phone"),
		OfficeIdentity: stringField(data, "office_identity"),
	}
	if info.ID == "" {
		info.ID = stringField(data, "user_id")
	}
	return info, nil
}

// enrichCredential fills ONLY the fields the credential is missing.
//
// A failed call never fails the login (`raccoon-auth.ts:220-238`): the
// credential is already usable, and the profile is a convenience that the
// startup repair path can also backfill later.
func enrichCredential(h *abiboot.Host, cfg Config, credential *Credential) {
	if credential == nil {
		return
	}
	if strings.TrimSpace(credential.Nickname) != "" && strings.TrimSpace(credential.UserID) != "" && strings.TrimSpace(credential.Phone) != "" {
		return
	}
	info, errInfo := fetchUserInfo(h, cfg, credential)
	if errInfo != nil {
		return
	}
	if strings.TrimSpace(credential.Nickname) == "" {
		credential.Nickname = info.Name
	}
	if strings.TrimSpace(credential.UserID) == "" {
		credential.UserID = info.ID
	}
	if strings.TrimSpace(credential.Phone) == "" {
		credential.Phone = info.Phone
	}
	if strings.TrimSpace(credential.OfficeIdentity) == "" {
		credential.OfficeIdentity = info.OfficeIdentity
	}
}

// itoaInt64 renders an int64 as decimal text.
func itoaInt64(value int64) string { return strconv.FormatInt(value, 10) }
