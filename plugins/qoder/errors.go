package main

import (
	"errors"
	"fmt"
	"net/http"

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
//	malformed request, missing model                                 400
//	hard quota exhausted                                             402
//	rate limited / queued                                            429
//	upstream 5xx, transport failure                                  502
//	upstream timeout                                                 504
//	plugin-side misconfiguration (e.g. an unusable wasm_path)        500

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
		return statusError(false, code, http.StatusPaymentRequired, format, args...)
	case status == http.StatusTooManyRequests:
		return statusError(true, code, http.StatusTooManyRequests, format, args...)
	case status == http.StatusGatewayTimeout:
		return statusError(true, code, http.StatusGatewayTimeout, format, args...)
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
