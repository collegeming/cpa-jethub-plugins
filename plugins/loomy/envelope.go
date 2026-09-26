package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// The response envelope shared by every Loomy business and account endpoint.
//
//	{ "ok": true, "code": "000000", "desc": "...", "message": "...", "data": {...} }
//
// Two rules dominate all error handling for this provider:
//
//   - success is `code == "000000"` and NOTHING else. Business failures are
//     returned with HTTP 200, so a status-code check reads an expired session as
//     a success (`loomy.ts:83-84`, trap #1);
//   - the message prefers `desc` and falls back to `message`; when both are empty
//     a synthetic `业务错误 <code>` is produced so a page never renders an empty
//     error box (`loomy.ts:92-94`).
const (
	// okCode is the only success code (`LOOMY_OK_CODE`, `loomy.ts:36,95`).
	okCode = "000000"
	// authExpiredCode means the session is dead. It is TERMINAL: never retry,
	// mark the credential and ask for a re-login (`loomy.ts:38-39`, trap #7).
	authExpiredCode = "100002"
	// badRequestCode is a rejected request, for example an unknown task key
	// (`loomy.ts:41-42`). The source declares it and never uses it — unknown keys
	// are filtered locally — but it is surfaced for diagnostics.
	badRequestCode = "100001"
	// accountLoginStateCode is the login-state failure observed on the ACCOUNT
	// host; it is a different code from the business `100002`
	// (`loomy-oauth.ts:117-119`, trap #9).
	accountLoginStateCode = "020002"
)

// flexText decodes a JSON field that may arrive as a string, a number, a boolean
// or null. The account host is not consistent about `code`/`desc` typing, and a
// typed decode into `string` would throw the whole envelope away — turning a
// readable upstream error into an unexplained failure.
type flexText string

// UnmarshalJSON accepts every scalar spelling and keeps the raw literal for
// anything structural.
func (t *flexText) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*t = ""
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if errUnmarshal := json.Unmarshal(data, &text); errUnmarshal != nil {
			return errUnmarshal
		}
		*t = flexText(text)
		return nil
	}
	if trimmed == "true" || trimmed == "false" {
		*t = flexText(trimmed)
		return nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&number); errDecode == nil {
		*t = flexText(number.String())
		return nil
	}
	*t = flexText(trimmed)
	return nil
}

// String returns the decoded text.
func (t flexText) String() string { return string(t) }

// envelope is the parsed wire envelope.
type envelope struct {
	Code    flexText        `json:"code"`
	Desc    flexText        `json:"desc"`
	Message flexText        `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// parseEnvelope parses a business/account response body.
//
// A non-object payload (an array, a bare string, an HTML error page) fails with
// the same wording the source uses, `响应不是 JSON 对象`, instead of being treated
// as an envelope with an empty code (`loomy.ts:87-89`).
func parseEnvelope(body []byte) (*envelope, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, transportError("empty_response", "Loomy 返回了空响应体")
	}
	if trimmed[0] != '{' {
		return nil, transportError("not_json_object", "响应不是 JSON 对象：%s", truncate(string(trimmed), 200))
	}
	parsed := &envelope{}
	if errUnmarshal := json.Unmarshal(trimmed, parsed); errUnmarshal != nil {
		return nil, transportError("bad_envelope", "解析 Loomy 响应失败：%v", errUnmarshal)
	}
	return parsed, nil
}

// success reports the ONLY success condition.
func (e *envelope) success() bool { return e != nil && strings.TrimSpace(e.Code.String()) == okCode }

// code returns the trimmed code text.
func (e *envelope) code() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.Code.String())
}

// text is `loomy.ts:92-94`: prefer `desc`, fall back to `message`, and never
// return an empty string for a failure.
func (e *envelope) text() string {
	if e == nil {
		return ""
	}
	if desc := strings.TrimSpace(e.Desc.String()); desc != "" {
		return desc
	}
	if message := strings.TrimSpace(e.Message.String()); message != "" {
		return message
	}
	if code := e.code(); code != "" {
		return "业务错误 " + code
	}
	return ""
}

// isAuthExpired reports the terminal session failure.
func (e *envelope) isAuthExpired() bool { return e.code() == authExpiredCode }

// decodeData decodes the `data` object into out. Loomy does not validate `data`
// (`loomy.ts:103`), so a missing body is not an error here either.
func (e *envelope) decodeData(out any) error {
	if e == nil || len(e.Data) == 0 || out == nil {
		return nil
	}
	if errUnmarshal := json.Unmarshal(e.Data, out); errUnmarshal != nil {
		return transportError("bad_data", "解析 Loomy data 失败：%v", errUnmarshal)
	}
	return nil
}

// envelopeFailure classifies a non-`000000` envelope.
//
// `100002` is terminal and maps to a 401 so the host can retire the credential;
// every other failure stays retryable and is reported as an upstream problem,
// because the source explicitly refuses to treat "not 100002" as a dead
// credential (`loomy-auth.ts:350-369`).
func envelopeFailure(op string, env *envelope) error {
	if env == nil {
		return transportError("empty_envelope", "%s 失败：Loomy 未返回响应信封", op)
	}
	message := env.text()
	if env.isAuthExpired() {
		return credentialError("auth_expired", "%s 失败：会话已失效（%s），请重新登录该账号", op, message)
	}
	if env.code() == badRequestCode {
		return statusError(false, "bad_request", 400, "%s 失败：%s", op, message)
	}
	return transportError("upstream_business_error", "%s 失败（code %s）：%s", op, env.code(), message)
}

// envelopeErrorText renders the `error`-style payload some gateways return
// instead of the Loomy envelope, so a failure message still carries the reason.
func envelopeErrorText(body []byte) string {
	var payload struct {
		Error struct {
			Message string   `json:"message"`
			Code    flexText `json:"code"`
		} `json:"error"`
		Code    flexText `json:"code"`
		Message string   `json:"message"`
		Msg     string   `json:"msg"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return truncate(string(body), 200)
	}
	for _, candidate := range []string{payload.Error.Message, payload.Message, payload.Msg} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	if code := payload.Error.Code.String(); code != "" {
		return "业务错误 " + code
	}
	return truncate(string(body), 200)
}

// itoaInt renders a non-negative int without pulling in fmt.
func itoaInt(value int) string { return strconv.Itoa(value) }
