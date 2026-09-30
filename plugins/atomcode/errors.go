package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// errorCodeOf extracts the plugin error code from an error chain, so a caller
// can branch on "this is an auth failure" without string-matching a message.
func errorCodeOf(err error) (string, bool) {
	var envelope *abiboot.EnvelopeError
	if errors.As(err, &envelope) && envelope != nil && envelope.Code != "" {
		return envelope.Code, true
	}
	return "", false
}

// Upstream failure classification for the AtomGit gateway and the CodingPlan
// REST API. The reference keeps the two error families separate —
// `format_api_error` for the REST API (`crates/atomcode-codingplan/src/client.rs:368`)
// and the gateway's own JSON for chat — so this file does too.

// gatewayErrorCode extracts the machine-readable code from a gateway error body.
//
// The gateway answers in two shapes:
//
//	{"error":{"message":"...","type":"auth_error","code":"403"}}   LiteLLM envelope
//	{"detail":{"code":"ATOMCODE_SIG_MISSING","message":"..."}}     AtomCode envelope
//
// The `detail.code` form is the one that carries actionable information, so it
// is read first.
func gatewayErrorCode(body string) string {
	var decoded struct {
		Detail *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"detail"`
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(body), &decoded); errUnmarshal != nil {
		return ""
	}
	if decoded.Detail != nil && decoded.Detail.Code != "" {
		return decoded.Detail.Code
	}
	if decoded.Error != nil {
		if code, ok := decoded.Error.Code.(string); ok {
			return code
		}
	}
	return ""
}

// gatewayErrorMessage extracts the human-readable message from either envelope.
func gatewayErrorMessage(body string) string {
	var decoded struct {
		Detail *struct {
			Message string `json:"message"`
		} `json:"detail"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(body), &decoded); errUnmarshal != nil {
		return ""
	}
	if decoded.Detail != nil && decoded.Detail.Message != "" {
		return decoded.Detail.Message
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		return decoded.Error.Message
	}
	return ""
}

// signatureMissingCode is the gateway's answer when a request reached the host
// that requires the closed-source request signature.
const signatureMissingCode = "ATOMCODE_SIG_MISSING"

// upstreamError converts a non-2xx gateway response into a plugin error.
//
// `ATOMCODE_SIG_MISSING` gets its own branch because it is not a credential
// problem and not a quota problem: it means the request went to
// `llm-api.atomgit.com` (or a sibling host) instead of the unsigned gateway.
// Reporting it as a generic 403 would send the user hunting for a login issue
// that does not exist.
func upstreamError(status int, body string) error {
	code := gatewayErrorCode(body)
	message := gatewayErrorMessage(body)
	detail := message
	if detail == "" {
		detail = truncate(body, 200)
	}

	switch {
	case code == signatureMissingCode:
		return abiboot.HTTPError("signature_required", http.StatusBadGateway,
			"AtomCode 网关要求官方版请求签名（%s）：当前 gateway_base 指向了需要签名的官方网关，"+
				"请改回无需签名的 %s（%s）", signatureMissingCode, DefaultGatewayBase, detail)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// A 403 without a recognisable code is the plan gate the gateway reports
		// as `user has no codingplan` before the daily claim has run.
		if strings.Contains(strings.ToLower(detail), "codingplan") {
			return abiboot.HTTPError("no_codingplan", http.StatusForbidden,
				"该 AtomGit 账号还没有领取免费套餐，请先在 AtomCode 页面领取（%s）", detail)
		}
		return abiboot.HTTPError("AUTH", status, "AtomCode 网关拒绝请求：%s", detail)
	case status == http.StatusTooManyRequests:
		return abiboot.HTTPError("RATE_LIMIT", status, "AtomCode 网关限流：%s", detail)
	case status >= 500:
		return abiboot.RetryableError("SERVER", "AtomCode 网关错误（HTTP %d）：%s", status, detail)
	default:
		return abiboot.HTTPError(httpErrorCode(status), status, "AtomCode 网关返回 HTTP %d：%s", status, detail)
	}
}

// parameterErrorMessage is the literal body the gateway puts in a SUCCESSFUL
// completion when it cannot serve the request.
//
// Measured 2026-10-01: an unknown model name gets `HTTP 200` with
// `choices[0].message.content == "参数错误"` instead of an error status. Passing
// that through would show the user "参数错误" as if the model had answered, so
// the executor turns it into a real error while the stream is still empty.
const parameterErrorMessage = "参数错误"

// looksLikeParameterError reports whether a buffered completion body is the
// gateway's silent parameter rejection rather than a model answer.
func looksLikeParameterError(body []byte) bool {
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		return false
	}
	if len(decoded.Choices) != 1 {
		return false
	}
	return strings.TrimSpace(decoded.Choices[0].Message.Content) == parameterErrorMessage
}

// classifyCodingPlanError renders a CodingPlan REST failure the way the
// reference does (`crates/atomcode-codingplan/src/client.rs:368-388`):
//
//  1. a product payload with a non-empty `message`;
//  2. a Spring error body with `path` -> `HTTP <code> — 接口暂不可用 (<path>)`;
//  3. a 200-character-capped raw fallback.
func classifyCodingPlanError(descriptor string, status int, body string) string {
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal([]byte(body), &decoded); errUnmarshal == nil {
		if message, ok := decoded["message"].(string); ok && message != "" {
			return message
		}
		if path, ok := decoded["path"].(string); ok && path != "" {
			return "HTTP " + itoa(status) + " — 接口暂不可用 (" + path + ")"
		}
	}
	return "CodingPlan " + descriptor + " returned " + itoa(status) + " — " + truncate(body, 200)
}

// httpErrorCode maps an HTTP status onto the client-facing CPA error code.
func httpErrorCode(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "AUTH"
	case status == http.StatusTooManyRequests:
		return "RATE_LIMIT"
	case status == http.StatusBadRequest:
		return "INVALID_REQUEST"
	case status >= 500:
		return "SERVER"
	default:
		return "HTTP_" + itoa(status)
	}
}

// truncate caps a string for an error message, marking the cut.
func truncate(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// itoa renders an int without importing strconv at call sites.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}

// itoa64 renders an int64.
func itoa64(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [24]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}
