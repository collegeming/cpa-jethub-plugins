package main

import (
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Upstream error classification, ported from
// jethub-src/src/lobsterai-errors.ts:22-171.
//
// Only the classification is ported, not the reference Go bridge's cooldown
// state machine: that silently disables accounts, which conflicts with a
// management UI that shows rate-limit badges and lets the user re-test. Pure
// functions only, no side effects.

// lobsteraiErrorKind is the upstream error class.
type lobsteraiErrorKind string

const (
	// errorNone means success (HTTP < 400 and no marker matched).
	errorNone lobsteraiErrorKind = "none"
	// errorHardCredit means the balance is exhausted; LobsterAI's dominant
	// failure mode.
	errorHardCredit lobsteraiErrorKind = "hard-credit"
	// errorSoftRate is a 429 soft rate limit.
	errorSoftRate lobsteraiErrorKind = "soft-rate"
	// errorSessionDead means the refresh token was rejected (40100/40101);
	// only a new login helps.
	errorSessionDead lobsteraiErrorKind = "session-dead"
	// errorNotFound is an upstream sporadic 404.
	errorNotFound lobsteraiErrorKind = "not-found"
	// errorServer is a 5xx upstream fault.
	errorServer lobsteraiErrorKind = "server"
	// errorClient is any other 4xx or business error.
	errorClient lobsteraiErrorKind = "client"
)

// hardCreditMarkers match an exhausted balance. Both languages are required:
// the same backend answers in Chinese or English depending on the scenario, and
// matching only one would miss half the cases (lobsterai-errors.ts:54-59).
var hardCreditMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "freecreditsused", "free credits used",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分", "积分耗尽",
}

// sessionDeadMarkers mark a rejected refresh (lobsterai-errors.ts:66-68).
var sessionDeadMarkers = []string{
	"40100", "40101", "token rejected", "refresh token was rejected",
}

// classifyLobsteraiError classifies a failed upstream response. The order is
// the priority and must not be reordered (lobsterai-errors.ts:75-117):
//
//  1. 402 -> hard-credit (the status code is the strongest signal);
//  2. a hard-credit marker in the body -> hard-credit;
//  3. a session-dead marker -> session-dead;
//  4. 429 -> soft-rate; 5. 404 -> not-found; 6. >=500 -> server;
//  7. >=400 -> client; 8. otherwise none.
//
// Body markers outrank the status code (except 402) because upstream reports an
// exhausted balance as 400 plus "积分不足"; reading only the status would call
// that a retryable client error and loop on an account that can never succeed.
func classifyLobsteraiError(status int, body string) lobsteraiErrorKind {
	if status == http.StatusPaymentRequired {
		return errorHardCredit
	}
	lower := strings.ToLower(body)
	for _, marker := range hardCreditMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) || strings.Contains(body, marker) {
			return errorHardCredit
		}
	}
	for _, marker := range sessionDeadMarkers {
		if strings.Contains(body, marker) {
			return errorSessionDead
		}
	}
	switch {
	case status == http.StatusTooManyRequests:
		return errorSoftRate
	case status == http.StatusNotFound:
		return errorNotFound
	case status >= 500:
		return errorServer
	case status >= 400:
		return errorClient
	default:
		return errorNone
	}
}

// shouldRotateAccount reports whether the host should try the next account.
// Every non-success class rotates, matching the reference Go handler where
// every switch branch ends in `continue`.
func shouldRotateAccount(kind lobsteraiErrorKind) bool {
	return kind != errorNone
}

// recordsRateLimit reports whether the failure should be recorded as a
// model-level rate limit (the badge in the account UI). Only the three classes
// the reference bridge actually cools down: hard-credit, soft-rate and
// not-found. session-dead and server/client leave no badge — otherwise a plain
// 400 would be displayed as "this model is rate limited for an hour".
func recordsRateLimit(kind lobsteraiErrorKind) bool {
	return kind == errorHardCredit || kind == errorSoftRate || kind == errorNotFound
}

// isTerminalError reports whether retrying is pointless and the account needs a
// fresh login. This is an improvement over the reference Go bridge, which only
// checked "does the response carry an accessToken" and therefore treated a
// network blip as terminal.
func isTerminalError(kind lobsteraiErrorKind) bool {
	return kind == errorSessionDead
}

// errorStatusFor maps a class onto the HTTP status CPA should show the client.
func errorStatusFor(kind lobsteraiErrorKind, upstreamStatus int) int {
	switch kind {
	case errorHardCredit:
		return http.StatusPaymentRequired
	case errorSoftRate:
		return http.StatusTooManyRequests
	case errorSessionDead:
		return http.StatusUnauthorized
	case errorNotFound:
		return http.StatusNotFound
	case errorServer:
		return http.StatusBadGateway
	case errorClient:
		if upstreamStatus >= 400 && upstreamStatus < 500 {
			return upstreamStatus
		}
		return http.StatusBadRequest
	default:
		if upstreamStatus >= 400 {
			return upstreamStatus
		}
		return http.StatusBadGateway
	}
}

// errorCodeFor names the class for the envelope error code.
func errorCodeFor(kind lobsteraiErrorKind) string {
	switch kind {
	case errorHardCredit:
		return "quota_exceeded"
	case errorSoftRate:
		return "rate_limited"
	case errorSessionDead:
		return "session_dead"
	case errorNotFound:
		return "model_not_found"
	case errorServer:
		return "upstream_server_error"
	case errorClient:
		return "upstream_client_error"
	default:
		return "upstream_error"
	}
}

// executorErrorFor converts a failed upstream chat response into the error CPA
// should return, preserving the class as an HTTP status so the host's scheduler
// can rotate accounts.
func executorErrorFor(status int, body []byte) *abiboot.EnvelopeError {
	kind := classifyLobsteraiError(status, string(body))
	if kind == errorNone {
		return abiboot.HTTPError("upstream_error", status, "LobsterAI 返回 HTTP %d：%s", status, errorDetail(body))
	}
	return abiboot.HTTPError(errorCodeFor(kind), errorStatusFor(kind, status),
		"LobsterAI 上游错误（HTTP %d / %s）：%s", status, kind, errorDetail(body))
}

// errorDetail extracts a readable message from an upstream error body, falling
// back to the raw text (lobsterai-adapter.ts:508-522).
func errorDetail(body []byte) string {
	record := map[string]any{}
	if errUnmarshal := decodeJSON(body, &record); errUnmarshal == nil {
		parts := make([]string, 0, 3)
		if _, found := record["code"]; found {
			parts = append(parts, "code="+stringifyValue(record["code"]))
		}
		if message := readStringField(record, "message"); message != "" {
			parts = append(parts, message)
		}
		if message := readStringField(record, "msg"); message != "" {
			parts = append(parts, message)
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	return truncate(strings.TrimSpace(string(body)), 400)
}

// truncate shortens s for inclusion in an error message.
func truncate(s string, limit int) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}

// itoa renders a non-negative int without importing strconv at call sites.
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
