package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Upstream error classification, ported from trae-errors.ts:20-157.

// traeErrorKind is one TRAE failure category.
type traeErrorKind string

const (
	errNone          traeErrorKind = "none"
	errHardPlan      traeErrorKind = "hard-plan"      // 1005: plan entitlement missing (long cooldown)
	errSoftRate      traeErrorKind = "soft-rate"      // 429 / 4011: short cooldown
	errSessionDead   traeErrorKind = "session-dead"   // token invalid: re-login
	errQuotaExceeded traeErrorKind = "quota-exceeded" // 4008: credits exhausted
	errNotFound      traeErrorKind = "not-found"
	errServer        traeErrorKind = "server"
	errClient        traeErrorKind = "client"
)

// sessionDeadMarkers mirror trae-errors.ts:46-48.
var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// planLimitMarkers mirror trae-errors.ts:55.
var planLimitMarkers = []string{`"code":1005`, "1005"}

// quotaExceededMarkers mirror trae-errors.ts:60.
var quotaExceededMarkers = []string{"4008", `"code":4008`, "quota", "exceeded the quota"}

// classifyTraeError decides the failure category from the HTTP status and the
// raw body (trae-errors.ts:80-126).
//
// Order matters: `quota-exceeded` (4008) must be checked **before** `soft-rate`
// (4011) because a gateway can concatenate both codes into one message. The
// heavier category needs the long cooldown; letting the lighter one win makes an
// exhausted account retried after 60 seconds while the user is told to "try
// again later" (docs/agents/trae.md:406-410).
func classifyTraeError(status int, body string) traeErrorKind {
	lower := strings.ToLower(body)

	for _, marker := range planLimitMarkers {
		if strings.Contains(body, marker) && strings.Contains(lower, "plan") {
			return errHardPlan
		}
	}
	for _, marker := range quotaExceededMarkers {
		if strings.Contains(body, marker) {
			return errQuotaExceeded
		}
	}
	if strings.Contains(body, "4011") {
		return errSoftRate
	}
	if status == http.StatusUnauthorized {
		for _, marker := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(marker)) {
				return errSessionDead
			}
		}
		return errSessionDead
	}
	if status == http.StatusTooManyRequests {
		return errSoftRate
	}
	if status == http.StatusNotFound {
		return errNotFound
	}
	if status >= 500 {
		return errServer
	}
	if status >= 400 {
		return errClient
	}
	return errNone
}

// shouldRotateAccount reports whether the failure should move to another
// account. Every category except "none" does (trae-errors.ts:133-135). CPA owns
// credential selection, so this port only uses it for logging decisions.
func shouldRotateAccount(kind traeErrorKind) bool { return kind != errNone }

// recordsRateLimit reports whether the category belongs in a rate-limit marker
// (trae-errors.ts:148-150).
func recordsRateLimit(kind traeErrorKind) bool {
	switch kind {
	case errHardPlan, errSoftRate, errNotFound, errQuotaExceeded:
		return true
	default:
		return false
	}
}

// isTerminalError reports whether retrying can only succeed after a new login
// (trae-errors.ts:155-157).
func isTerminalError(kind traeErrorKind) bool { return kind == errSessionDead }

// httpErrorCode maps an HTTP status onto the client-facing CPA error code
// (trae-adapter.ts:408-414).
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

// errorDetail renders a readable detail from an upstream error body
// (trae-adapter.ts:394-405).
func errorDetail(body string) string {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err == nil {
		parts := []string{}
		if code, ok := decoded["code"]; ok && code != nil {
			parts = append(parts, "code="+renderScalar(code))
		}
		if message := readStringField(decoded, "message"); message != "" {
			parts = append(parts, message)
		}
		if msg := readStringField(decoded, "msg"); msg != "" {
			parts = append(parts, msg)
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	return body
}

// renderScalar renders a JSON scalar for an error message.
func renderScalar(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return trimFloat(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// upstreamError converts a non-2xx chat response into a plugin error, keeping
// the upstream status visible to the client and flagging the two categories that
// mean "this credential is done" (trae-adapter.ts:963-966).
func upstreamError(status int, body string) error {
	kind := classifyTraeError(status, body)
	detail := errorDetail(body)
	switch kind {
	case errQuotaExceeded:
		return abiboot.HTTPError("quota_exceeded", status, "TRAE 积分不足（%s）", detail)
	case errSessionDead:
		return abiboot.HTTPError("session_dead", http.StatusUnauthorized, "TRAE 登录态失效，请重新登录（%s）", detail)
	case errHardPlan:
		return abiboot.HTTPError("plan_limit", status, "TRAE 当前套餐权益不足（%s）", detail)
	default:
		return abiboot.HTTPError(httpErrorCode(status), status, "trae: %s", detail)
	}
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
