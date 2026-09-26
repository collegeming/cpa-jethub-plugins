package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// The response envelope every non-inference endpoint shares
// (`raccoon-oauth.ts:54-73`, mirrored at `raccoon-credits.ts:44-63`):
//
//	{ "code": 0, "message": "…", "details": "…", "data": { … } }
//
// Two rules dominate error handling for this provider:
//
//   - success is `code === 0` and NOTHING else. Business failures arrive with
//     HTTP 200, so a status-code check reads a dead session as a success
//     (`raccoon-oauth.ts:53`, trap #30);
//   - when `code` is absent and the HTTP status is >= 400, the status BECOMES
//     the code (`raccoon-oauth.ts:66`), which keeps a gateway error readable
//     instead of looking like success.
const (
	// codeOK is the only success code.
	codeOK = 0
	// codeParamsInvalid covers a rejected parameter set (`raccoon-oauth.ts:273`).
	codeParamsInvalid = 100002
	// codeAuthorizationVerifyError means the refresh token is dead. It is
	// TERMINAL: never retry, ask for a new login (`raccoon-oauth.ts:299-301`).
	codeAuthorizationVerifyError = 200003
	// codeAuthorizationCodeMissing is the dead authorisation-code path's code,
	// kept for diagnostics only (`raccoon-oauth.ts:273`).
	codeAuthorizationCodeMissing = 200035
)

// flexCode decodes a `code` that may arrive as a number or as a numeric string,
// and remembers whether it was present at all. The distinction matters: an
// absent code inherits the HTTP status, while a present `0` is success.
type flexCode struct {
	value   int64
	present bool
}

// UnmarshalJSON accepts a number, a numeric string, or null.
func (c *flexCode) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if errUnmarshal := json.Unmarshal(data, &text); errUnmarshal != nil {
			return errUnmarshal
		}
		trimmed = strings.TrimSpace(text)
	}
	parsed, errParse := strconv.ParseInt(trimmed, 10, 64)
	if errParse != nil {
		// A non-numeric code is kept as "present but unparsable" by storing a
		// sentinel that can never equal success.
		c.present = true
		c.value = -1
		return nil
	}
	c.value = parsed
	c.present = true
	return nil
}

// flexText decodes a field that may arrive as a string, number, boolean or null.
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
	Code    flexCode        `json:"code"`
	Message flexText        `json:"message"`
	Details flexText        `json:"details"`
	Data    json.RawMessage `json:"data"`
}

// parseEnvelope parses a response body, applying the status fallback rule.
//
// A non-object payload (an array, a bare string, an HTML error page) is an
// error rather than an envelope with an empty code, so a gateway page cannot be
// mistaken for a business answer.
func parseEnvelope(body []byte, status int) (*envelope, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, transportError("empty_response", "Raccoon 返回了空响应体")
	}
	if trimmed[0] != '{' {
		return nil, transportError("not_json_object", "响应不是 JSON 对象：%s", truncate(string(trimmed), 200))
	}
	parsed := &envelope{}
	if errUnmarshal := json.Unmarshal(trimmed, parsed); errUnmarshal != nil {
		return nil, transportError("bad_envelope", "解析 Raccoon 响应失败：%v", errUnmarshal)
	}
	if !parsed.Code.present && status >= 400 {
		parsed.Code = flexCode{value: int64(status), present: true}
	}
	return parsed, nil
}

// success reports the ONLY success condition: a present code equal to zero.
func (e *envelope) success() bool { return e != nil && e.Code.present && e.Code.value == codeOK }

// code returns the numeric code, or 0 when the envelope is nil.
func (e *envelope) code() int64 {
	if e == nil {
		return 0
	}
	return e.Code.value
}

// text renders the error sentence: `message` and `details`, empties filtered,
// joined with ": ", and prefixed by the caller's operation.
//
// The reference prefixes the whole string with `raccoon: ` and falls back to a
// caller-supplied default when both fields are empty (`raccoon-oauth.ts:76-80`).
func (e *envelope) text(fallback string) string {
	parts := make([]string, 0, 2)
	if e != nil {
		if message := strings.TrimSpace(e.Message.String()); message != "" {
			parts = append(parts, message)
		}
		if details := strings.TrimSpace(e.Details.String()); details != "" {
			parts = append(parts, details)
		}
	}
	if len(parts) == 0 {
		if strings.TrimSpace(fallback) == "" {
			return "raccoon: 未知业务错误"
		}
		return "raccoon: " + strings.TrimSpace(fallback)
	}
	return "raccoon: " + strings.Join(parts, ": ")
}

// dataObject decodes `data` into a generic object.
//
// Arrays and absent values become nil, exactly as the reference's
// `asRecord`-style guard does (`raccoon-oauth.ts:54-73`): a `data` that is not an
// object carries nothing this provider can read.
func (e *envelope) dataObject() map[string]any {
	if e == nil || len(e.Data) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(e.Data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var object map[string]any
	if errUnmarshal := json.Unmarshal(trimmed, &object); errUnmarshal != nil {
		return nil
	}
	return object
}

// envelopeFailure classifies a non-zero envelope.
//
// `200003` on refresh is terminal and maps to a 401 so the host can retire the
// credential; every other failure stays retryable and is reported as an upstream
// problem, because the reference explicitly refuses to treat "something else
// went wrong" as a dead credential (`raccoon-oauth.ts:302`).
func envelopeFailure(op string, env *envelope, terminalOnAuthCode bool) error {
	if env == nil {
		return transportError("empty_envelope", "%s 失败：Raccoon 未返回响应信封", op)
	}
	message := env.text("")
	if terminalOnAuthCode && env.code() == codeAuthorizationVerifyError {
		return credentialError("auth_expired", "%s 失败：%s（登录态已过期，请重新登录）", op, message)
	}
	if env.code() == codeParamsInvalid {
		return statusError(false, "bad_request", 400, "%s 失败：%s", op, message)
	}
	return transportError("upstream_business_error", "%s 失败（code %d）：%s", op, env.code(), message)
}

// envelopeErrorText renders the `error`-style payload some gateways return
// instead of the Raccoon envelope, so a failure message still carries a reason.
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
	if code := strings.TrimSpace(payload.Error.Code.String()); code != "" {
		return "业务错误 " + code
	}
	return truncate(string(body), 200)
}

// numericField reads a numeric field from a decoded object, accepting a JSON
// number and a numeric string.
func numericField(source map[string]any, key string) (float64, bool) {
	if source == nil {
		return 0, false
	}
	switch typed := source[key].(type) {
	case float64:
		return typed, true
	case json.Number:
		value, errParse := typed.Float64()
		return value, errParse == nil
	case string:
		value, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return value, errParse == nil
	default:
		return 0, false
	}
}

// intField reads an integer field, reporting false when it is absent, not a
// number, or not a safe positive integer.
func intField(source map[string]any, key string) (int64, bool) {
	value, ok := numericField(source, key)
	if !ok {
		return 0, false
	}
	if value <= 0 || value != float64(int64(value)) {
		return 0, false
	}
	return int64(value), true
}

// stringField reads a non-empty string field.
func stringField(source map[string]any, key string) string {
	if source == nil {
		return ""
	}
	if text, ok := source[key].(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}
