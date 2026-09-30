package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Upstream failure classification.
//
// The host reads `http_status` from the failure envelope (it becomes
// `rpcError.statusCode`, which `clienterror.HTTPStatusFromError` and
// `IsRequestFault` consume) to decide whether a failure is the REQUEST's fault —
// which drives credential rotation and the status code returned to the client.
// The envelope `code` is only consulted for a couple of dispatch cases, so a
// failure that carries no status is effectively unclassified.
//
// The mapping used throughout this plugin, and where each case comes from:
//
//	credential missing / unparseable / dead, refresh token invalid   401
//	region-restricted 403 (PERMISSION_DENIED, no refresh attempted)  403
//	malformed request, missing model                                400
//	quota exhausted / credit marker on a 4xx                        402
//	rate limited (429, or a rate marker)                            429
//	upstream 5xx, transport failure                                 502
//
// The TypeScript spreads the same taxonomy over its own `LlmError` codes
// (`openai-compat.ts:295-301`), and the account rotation it then performs
// (`cline-adapter.ts:475-519`) is host-owned in CPA: the plugin only has to
// classify honestly so the host's scheduler can rotate.

// statusError builds a classified failure, keeping the retry hint for hosts that
// still look at it.
func statusError(retryable bool, code string, status int, format string, args ...any) *abiboot.EnvelopeError {
	return &abiboot.EnvelopeError{
		Code:       code,
		Message:    fmt.Sprintf(format, args...),
		Retryable:  retryable,
		HTTPStatus: status,
	}
}

// credentialError marks a credential as unusable so the host can rotate it.
func credentialError(code, format string, args ...any) *abiboot.EnvelopeError {
	return statusError(false, code, http.StatusUnauthorized, format, args...)
}

// transportError marks a network or upstream failure.
func transportError(code, format string, args ...any) *abiboot.EnvelopeError {
	return statusError(true, code, http.StatusBadGateway, format, args...)
}

// upstreamStatusError maps an upstream HTTP status onto the failure taxonomy.
//
// 400 is the request's fault and must NOT rotate the credential
// (`cline-adapter.ts:571-577`); 5xx is the server's and is never rotated either,
// which is why both end up as the same 502 family rather than an AUTH.
//
// ⚠️ A 400 that names a context overflow is NOT a generic invalid request. It
// becomes CONTEXT_WINDOW_EXCEEDED, which is the only code that lets the client
// compact the conversation; INVALID_REQUEST is neither retryable nor recognised
// by the compaction listener, so the session dies on every later turn
// (upstream `7ed3466`, issue !IKITT9 part c). `body` is what that decision reads.
func upstreamStatusError(body []byte, code string, status int, format string, args ...any) *abiboot.EnvelopeError {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return credentialError(code, format, args...)
	case status == http.StatusPaymentRequired:
		return statusError(false, code, http.StatusPaymentRequired, format, args...)
	case status == http.StatusTooManyRequests:
		return statusError(true, code, http.StatusTooManyRequests, format, args...)
	case status == http.StatusBadRequest && isContextWindowExceeded(body):
		return statusError(false, ContextWindowExceededCode, http.StatusBadRequest, format, args...)
	case status == http.StatusBadRequest:
		return statusError(false, code, http.StatusBadRequest, format, args...)
	default:
		return transportError(code, format, args...)
	}
}

// contextWindowMarkers are the substrings that identify a context-overflow error.
//
// Mirrored from the sibling port that already classifies this correctly
// (`plugins/codebuddy/translator.go:124-133`), which in turn takes them from the
// measured payloads recorded at `buddy-adapter.ts:395-410`
// (`extError.code = context_length_exceeded`,
// `prompt is too long: N tokens > M maximum`) plus the generic OpenAI wording.
//
// ⚠️ Upstream additionally requires the `prompt is too long` form to be paired
// with an `N tokens > M` figure, because the bare phrase could otherwise catch an
// ordinary content-policy 400 (upstream `7ed3466`: "已加窄判据：必须同时命中").
// This port keeps the plain list deliberately: the sibling plugin's list is the
// shared convention across this repository, and mis-classifying an overflow costs
// at most one useless compaction attempt, whereas MISSING one kills the session.
var contextWindowMarkers = []string{
	"context_length_exceeded",
	"context length",
	"context window",
	"prompt is too long",
	"maximum context",
	"exceeds the model context limit",
	"too many tokens",
	"reduce the length",
}

// ContextWindowExceededCode is the provider-neutral code the harness routes on
// (`CONTEXT_WINDOW_EXCEEDED_CODE`, `@deepseek-ai/dsh-llm`). It is spelled out
// rather than imported: this plugin owns its own taxonomy.
const ContextWindowExceededCode = "CONTEXT_WINDOW_EXCEEDED"

// isContextWindowExceeded reports whether an error body describes a context
// overflow. The full raw body is inspected, not a normalised excerpt, because
// excerpting loses the fields that identify the overflow
// (`buddy-adapter.ts:404-410`).
func isContextWindowExceeded(body []byte) bool {
	lowered := strings.ToLower(string(body))
	for _, marker := range contextWindowMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// credentialAdvice appends the advice every dead-credential answer deserves.
func credentialAdvice(status int) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "（凭据已失效，请重新登录该账号）"
	}
	return ""
}

// statusOf reads the HTTP status a failure carries, falling back to the given
// value when the error is unclassified or not a plugin failure.
func statusOf(err error, fallback int) int {
	var envelope *abiboot.EnvelopeError
	if errors.As(err, &envelope) && envelope != nil && envelope.HTTPStatus != 0 {
		return envelope.HTTPStatus
	}
	return fallback
}

// truncate shortens a diagnostic string so error messages stay readable.
func truncate(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "…"
}

// itoaInt renders a non-negative int.
func itoaInt(value int) string {
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

// boolString renders a boolean the way the host attribute maps expect it.
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
