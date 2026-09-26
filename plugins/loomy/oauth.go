package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// The iFlytek CAccount client, ported from `src/loomy-oauth.ts:55-180`.
//
// Every account request is `POST {AccountBase}{path}` carrying the fixed client
// envelope below and signed with the AK/SK (see sign.go). There is NO session
// bootstrap call and NO refresh endpoint: `checkCode` is the last login step and
// the returned `session` is the only credential (`loomy-oauth.ts:159-180`).

// accountCountryCode is `ccode`, hard-coded to mainland China (`loomy-oauth.ts:137`).
const accountCountryCode = "86"

// accountBasePayload is the `base` object every account request carries
// (`loomy-oauth.ts:55-71`). Field order is fixed by the struct so the marshalled
// bytes are deterministic — which the signature requires.
type accountBasePayload struct {
	AppID    string `json:"appid"`
	ModelID  string `json:"modelid"`
	Version  string `json:"version"`
	DeviceID string `json:"devid"`
	UA       string `json:"ua"`
	TraceID  string `json:"traceid"`
}

// accountRequestBody is the request envelope.
type accountRequestBody struct {
	Base  accountBasePayload `json:"base"`
	Param any                `json:"param"`
}

// sendMsgCodeParam is the `param` of `/login/phone/sendMsgCode`
// (`loomy-oauth.ts:131-151`).
type sendMsgCodeParam struct {
	CCode  string `json:"ccode"`
	Phone  string `json:"phone"`
	Expire int    `json:"expire"`
}

// checkCodeParam is the `param` of `/login/phone/checkCode`
// (`loomy-oauth.ts:159-180`). `expire` is the SESSION lifetime the client
// declares, not the SMS lifetime.
type checkCodeParam struct {
	CCode  string `json:"ccode"`
	Phone  string `json:"phone"`
	MCode  string `json:"mcode"`
	MsgID  string `json:"msgid"`
	Expire int    `json:"expire"`
}

// randomTraceID is `base.traceid`: `randomUUID()` with the dashes stripped, i.e.
// 32 lowercase hex characters, freshly generated per request
// (`loomy-oauth.ts:67`).
func randomTraceID() string {
	return strings.ReplaceAll(randomUUID(), "-", "")
}

// randomUUID returns a lower-case RFC 4122 v4 UUID, the Go equivalent of the
// `randomUUID()` calls in `loomy-oauth.ts:67,130`.
func randomUUID() string {
	raw := make([]byte, 16)
	if _, errRead := rand.Read(raw); errRead != nil {
		// crypto/rand failing is unrecoverable; a timestamp-derived identifier
		// still keeps the wire shape valid instead of panicking a plugin call.
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// accountBody marshals one account request.
//
// The bytes returned here are BOTH signed and sent — never re-marshalled later
// (`loomy-oauth.ts:84-92`, trap #4). The trace id is generated here so the
// signature covers it.
func accountBody(param any, traceID string) ([]byte, error) {
	if strings.TrimSpace(traceID) == "" {
		traceID = randomTraceID()
	}
	body, errMarshal := json.Marshal(accountRequestBody{
		Base: accountBasePayload{
			AppID:    AccountAppID,
			ModelID:  AccountModelID,
			Version:  AccountClientVersion,
			DeviceID: AccountDeviceID,
			UA:       AccountUA,
			TraceID:  traceID,
		},
		Param: param,
	})
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_account_request", "encode account request: %v", errMarshal)
	}
	return body, nil
}

// accountCall performs one signed account request and returns its envelope.
//
// A transport failure is reported as a transport error, never as a credential
// failure; the account host answers business failures inside the envelope, so the
// envelope is parsed even for a non-2xx status when it is well-formed.
func accountCall(h *abiboot.Host, cfg Config, path string, param any, now time.Time) (*envelope, error) {
	body, errBody := accountBody(param, randomTraceID())
	if errBody != nil {
		return nil, errBody
	}
	headers := accountHeaders(cfg, signInput{
		Method:      http.MethodPost,
		Path:        path,
		Body:        body,
		ContentType: accountContentType,
		Date:        now,
		Nonce:       randomUUID(),
	})
	response, errDo := hostRequest(h, http.MethodPost, AccountBase+path, headers, body, cfg)
	if errDo != nil {
		return nil, transportError("account_transport", "连接讯飞账号服务失败：%v", errDo)
	}
	parsed, errParse := parseEnvelope(response.Body)
	if errParse != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, transportError("account_status", "讯飞账号服务返回 HTTP %d：%s",
				response.StatusCode, truncate(string(response.Body), 300))
		}
		return nil, errParse
	}
	if !parsed.success() {
		// The server `desc` is surfaced verbatim so the page can distinguish
		// "验证码错误" from "手机号格式不正确" (`loomy-oauth.ts:119-120`).
		return nil, envelopeFailure("讯飞账号服务 "+path, parsed)
	}
	return parsed, nil
}

// sendMsgCode requests one SMS code and returns the mandatory `msgid`
// (`loomy-oauth.ts:131-151`).
//
// The phone gate is the source's own `/^1[3-9]\d{9}$/` applied to a trimmed
// string (`jet-hub-rpc.ts:1124-1127`); the server may still reject the number,
// and its `desc` travels back unchanged.
func sendMsgCode(h *abiboot.Host, cfg Config, phone string, now time.Time) (string, error) {
	normalized := normalizePhone(phone)
	if !phonePattern.MatchString(normalized) {
		return "", statusError(false, "invalid_phone", http.StatusBadRequest,
			"手机号格式不正确：%q（需要 11 位大陆手机号）", strings.TrimSpace(phone))
	}
	parsed, errCall := accountCall(h, cfg, SendMsgCodePath, sendMsgCodeParam{
		CCode:  accountCountryCode,
		Phone:  normalized,
		Expire: cfg.smsCodeTTL(),
	}, now)
	if errCall != nil {
		return "", errCall
	}
	var data struct {
		MsgID string `json:"msgid"`
	}
	if errDecode := parsed.decodeData(&data); errDecode != nil {
		return "", errDecode
	}
	if strings.TrimSpace(data.MsgID) == "" {
		// An empty msgid must never be passed downstream: `checkCode` would fail
		// with a confusing server error instead of "the code was not sent"
		// (`loomy-oauth.ts:142-149`).
		return "", transportError("missing_msgid", "短信验证码响应缺少 msgid")
	}
	return strings.TrimSpace(data.MsgID), nil
}

// checkCode exchanges the SMS code for a session (`loomy-oauth.ts:159-180`).
//
// `session` and `userid` are both mandatory; a missing one is an upstream
// contract violation, not a successful login.
func checkCode(h *abiboot.Host, cfg Config, phone, code, msgid string, now time.Time) (session, userID string, err error) {
	normalized := normalizePhone(phone)
	if !phonePattern.MatchString(normalized) {
		return "", "", statusError(false, "invalid_phone", http.StatusBadRequest,
			"手机号格式不正确：%q（需要 11 位大陆手机号）", strings.TrimSpace(phone))
	}
	if strings.TrimSpace(code) == "" {
		return "", "", statusError(false, "missing_code", http.StatusBadRequest, "请先填写短信验证码")
	}
	parsed, errCall := accountCall(h, cfg, CheckCodePath, checkCodeParam{
		CCode:  accountCountryCode,
		Phone:  normalized,
		MCode:  strings.TrimSpace(code),
		MsgID:  strings.TrimSpace(msgid),
		Expire: cfg.sessionTTL(),
	}, now)
	if errCall != nil {
		return "", "", errCall
	}
	var data struct {
		Session string `json:"session"`
		UserID  string `json:"userid"`
	}
	if errDecode := parsed.decodeData(&data); errDecode != nil {
		return "", "", errDecode
	}
	if strings.TrimSpace(data.Session) == "" {
		return "", "", transportError("missing_session", "登录响应缺少 session")
	}
	if strings.TrimSpace(data.UserID) == "" {
		return "", "", transportError("missing_userid", "登录响应缺少 userid")
	}
	return strings.TrimSpace(data.Session), strings.TrimSpace(data.UserID), nil
}
