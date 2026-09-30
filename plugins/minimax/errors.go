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
// The mapping used throughout this plugin:
//
//	credential missing / unparseable / dead, refresh token invalid   401
//	malformed request, rejected parameters                           400
//	hard quota exhausted (the "go top up" signal)                    402
//	rate limited                                                     429
//	upstream 5xx, transport failure                                  502
//	upstream timeout                                                 504
//	plugin-side misconfiguration (e.g. a rejected timezone_id)        500
//
// ⚠️ A transport failure must NEVER be classified as credential expiry: that
// would force a re-login on every network hiccup.
//
// ⚠️ And 402 is its own code. It is the single most common real failure on this
// provider, and folding it into SERVER or AUTH hides the only action that helps
// the user, which is to top the account up.

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
func upstreamStatusError(code string, status int, format string, args ...any) *abiboot.EnvelopeError {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return credentialError(code, format, args...)
	case status == http.StatusPaymentRequired:
		return statusError(false, "QUOTA_EXCEEDED", http.StatusPaymentRequired, format, args...)
	case status == http.StatusTooManyRequests:
		return statusError(true, code, http.StatusTooManyRequests, format, args...)
	case status == http.StatusGatewayTimeout, status == http.StatusRequestTimeout:
		return statusError(true, code, status, format, args...)
	case status == http.StatusBadRequest:
		return statusError(false, code, http.StatusBadRequest, format, args...)
	default:
		return transportError(code, format, args...)
	}
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

// isRetryable reports whether a classified failure is worth another attempt.
//
// It reads the envelope's own retry hint rather than matching error text: a
// transport fault is retryable and a rejected request is not, and that
// distinction is already carried on the failure.
func isRetryable(err error) bool {
	var envelope *abiboot.EnvelopeError
	if !errors.As(err, &envelope) || envelope == nil {
		return false
	}
	return envelope.Retryable
}

// truncate shortens a diagnostic string so error messages stay readable.
func truncate(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "…"
}

// boolString renders a boolean attribute for the host.
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// itoaInt renders an int without pulling fmt into page code.
func itoaInt(value int) string { return fmt.Sprintf("%d", value) }
