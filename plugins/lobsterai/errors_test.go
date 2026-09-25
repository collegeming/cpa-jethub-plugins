package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestClassifyLobsteraiError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   lobsteraiErrorKind
	}{
		{name: "success", status: 200, body: `{"code":0}`, want: errorNone},
		{name: "402 is hard credit", status: http.StatusPaymentRequired, body: `{}`, want: errorHardCredit},
		{name: "402 outranks everything", status: http.StatusPaymentRequired, body: `{"code":40101}`, want: errorHardCredit},
		{name: "chinese hard credit on 400", status: 400, body: `{"code":1,"msg":"积分不足"}`, want: errorHardCredit},
		{name: "english hard credit", status: 400, body: `Insufficient Credit`, want: errorHardCredit},
		{name: "quota exceeded", status: 200, body: `{"msg":"quota exceeded"}`, want: errorHardCredit},
		{name: "free credits used", status: 403, body: `{"msg":"freecreditsused"}`, want: errorHardCredit},
		{name: "session dead business code", status: 200, body: `{"code":40100}`, want: errorSessionDead},
		{name: "session dead marker", status: 403, body: `token rejected`, want: errorSessionDead},
		{name: "session dead outranks 429", status: 429, body: `{"code":40101,"msg":"refresh token was rejected"}`, want: errorSessionDead},
		{name: "soft rate", status: http.StatusTooManyRequests, body: `slow down`, want: errorSoftRate},
		{name: "not found", status: http.StatusNotFound, body: `no such model`, want: errorNotFound},
		{name: "server", status: 503, body: `upstream down`, want: errorServer},
		{name: "client", status: 400, body: `bad request`, want: errorClient},
		{name: "forbidden without marker is a client error", status: http.StatusForbidden, body: `nope`, want: errorClient},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyLobsteraiError(test.status, test.body); got != test.want {
				t.Fatalf("classify(%d, %q) = %s, want %s", test.status, test.body, got, test.want)
			}
		})
	}
}

func TestErrorPredicates(t *testing.T) {
	// Every non-success class rotates, matching the reference handler where
	// every switch branch continues.
	for _, kind := range []lobsteraiErrorKind{
		errorHardCredit, errorSoftRate, errorSessionDead, errorNotFound, errorServer, errorClient,
	} {
		if !shouldRotateAccount(kind) {
			t.Fatalf("shouldRotateAccount(%s) = false, want true", kind)
		}
	}
	if shouldRotateAccount(errorNone) {
		t.Fatal("a success must not rotate")
	}

	// Only the three classes the reference bridge cools down leave a badge.
	records := map[lobsteraiErrorKind]bool{
		errorHardCredit: true, errorSoftRate: true, errorNotFound: true,
		errorSessionDead: false, errorServer: false, errorClient: false, errorNone: false,
	}
	for kind, want := range records {
		if got := recordsRateLimit(kind); got != want {
			t.Fatalf("recordsRateLimit(%s) = %v, want %v", kind, got, want)
		}
	}

	if !isTerminalError(errorSessionDead) {
		t.Fatal("session-dead must be terminal")
	}
	for _, kind := range []lobsteraiErrorKind{errorHardCredit, errorSoftRate, errorNotFound, errorServer, errorClient, errorNone} {
		if isTerminalError(kind) {
			t.Fatalf("isTerminalError(%s) = true, want false", kind)
		}
	}
}

func TestErrorStatusAndCode(t *testing.T) {
	tests := []struct {
		kind       lobsteraiErrorKind
		upstream   int
		wantStatus int
		wantCode   string
	}{
		{kind: errorHardCredit, upstream: 400, wantStatus: http.StatusPaymentRequired, wantCode: "quota_exceeded"},
		{kind: errorSoftRate, upstream: 429, wantStatus: http.StatusTooManyRequests, wantCode: "rate_limited"},
		{kind: errorSessionDead, upstream: 200, wantStatus: http.StatusUnauthorized, wantCode: "session_dead"},
		{kind: errorNotFound, upstream: 404, wantStatus: http.StatusNotFound, wantCode: "model_not_found"},
		{kind: errorServer, upstream: 503, wantStatus: http.StatusBadGateway, wantCode: "upstream_server_error"},
		{kind: errorClient, upstream: 422, wantStatus: 422, wantCode: "upstream_client_error"},
		{kind: errorNone, upstream: 500, wantStatus: 500, wantCode: "upstream_error"},
	}
	for _, test := range tests {
		if got := errorStatusFor(test.kind, test.upstream); got != test.wantStatus {
			t.Fatalf("errorStatusFor(%s, %d) = %d, want %d", test.kind, test.upstream, got, test.wantStatus)
		}
		if got := errorCodeFor(test.kind); got != test.wantCode {
			t.Fatalf("errorCodeFor(%s) = %q, want %q", test.kind, got, test.wantCode)
		}
	}
}

func TestExecutorErrorFor(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "hard credit", status: 400, body: `{"msg":"积分不足"}`, wantStatus: 402, wantCode: "quota_exceeded"},
		{name: "rate limited", status: 429, body: `slow`, wantStatus: 429, wantCode: "rate_limited"},
		{name: "session dead", status: 401, body: `{"code":40101}`, wantStatus: 401, wantCode: "session_dead"},
		{name: "server", status: 502, body: `bad gateway`, wantStatus: 502, wantCode: "upstream_server_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errExecutor := executorErrorFor(test.status, []byte(test.body))
			if errExecutor == nil {
				t.Fatal("expected an error")
			}
			if errExecutor.HTTPStatus != test.wantStatus || errExecutor.Code != test.wantCode {
				t.Fatalf("error = %+v, want %s/%d", errExecutor, test.wantCode, test.wantStatus)
			}
			if !strings.Contains(errExecutor.Message, "LobsterAI") {
				t.Fatalf("message = %q", errExecutor.Message)
			}
		})
	}
}

func TestErrorDetail(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "code and message", body: `{"code":500,"message":"boom"}`, want: "code=500 boom"},
		{name: "code and msg", body: `{"code":500,"msg":"boom"}`, want: "code=500 boom"},
		{name: "message only", body: `{"message":"boom"}`, want: "boom"},
		{name: "plain text", body: `  boom  `, want: "boom"},
		{name: "empty", body: ``, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := errorDetail([]byte(test.body)); got != test.want {
				t.Fatalf("errorDetail(%q) = %q, want %q", test.body, got, test.want)
			}
		})
	}
	// A very long body is truncated so an error message cannot grow unbounded.
	long := strings.Repeat("x", 1000)
	if got := errorDetail([]byte(long)); len(got) > 420 {
		t.Fatalf("errorDetail did not truncate: %d chars", len(got))
	}
}

func TestErrorMarkersAreBilingual(t *testing.T) {
	// The same backend answers in Chinese or English, so both marker families
	// must stay registered.
	for _, marker := range []string{"积分不足", "额度不足", "余额不足", "insufficient credit", "quota exceeded", "payment required"} {
		if !containsString(hardCreditMarkers, marker) {
			t.Fatalf("hard-credit markers lost %q", marker)
		}
	}
	for _, marker := range []string{"40100", "40101", "token rejected"} {
		if !containsString(sessionDeadMarkers, marker) {
			t.Fatalf("session-dead markers lost %q", marker)
		}
	}
}

func TestItoa(t *testing.T) {
	tests := map[int]string{0: "0", 1: "1", 9: "9", 10: "10", 123: "123", -5: "-5", 1000000: "1000000"}
	for input, want := range tests {
		if got := itoa(input); got != want {
			t.Fatalf("itoa(%d) = %q, want %q", input, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("  abc  ", 10); got != "abc" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc..." {
		t.Fatalf("truncate = %q", got)
	}
}

// TestRefreshFailureStatusKeepsTheClass checks that a retryable failure is not
// flattened into a generic 500.
func TestRefreshFailureStatusKeepsTheClass(t *testing.T) {
	tests := map[int]int{
		http.StatusTooManyRequests:     http.StatusTooManyRequests,
		http.StatusInternalServerError: http.StatusBadGateway,
		422:                            422,
		301:                            http.StatusBadGateway,
		0:                              http.StatusBadGateway,
	}
	for input, want := range tests {
		if got := refreshFailureStatus(input); got != want {
			t.Fatalf("refreshFailureStatus(%d) = %d, want %d", input, got, want)
		}
	}
}
