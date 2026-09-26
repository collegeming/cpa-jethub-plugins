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
//
//	credential missing / unparseable / dead session                401
//	malformed request, rejected parameters                         400
//	rate limited                                                   429
//	upstream 5xx, transport failure, retryable business failure     502
//	upstream timeout                                               504
//
// ⚠️ A transport failure must NEVER be classified as credential expiry
// (`raccoon-adapter.ts:318-328`, trap #15): that would force a re-login on every
// network hiccup.

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

// statusOf reads the HTTP status a failure carries, falling back to the given
// value when the error is unclassified or not a plugin failure.
func statusOf(err error, fallback int) int {
	var envelopeError *abiboot.EnvelopeError
	if errors.As(err, &envelopeError) && envelopeError != nil && envelopeError.HTTPStatus != 0 {
		return envelopeError.HTTPStatus
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

// boolString renders a boolean attribute for the host.
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// itoaInt renders an int without pulling fmt into page code.
func itoaInt(value int) string { return fmt.Sprintf("%d", value) }
