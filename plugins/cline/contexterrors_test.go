package main

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Context-overflow classification (upstream `7ed3466`, part c).
//
// ⚠️ The cost of MISSING this classification is asymmetric, which is why the
// direction taken is "rather classify one overflow too many":
//
//   - `INVALID_REQUEST` is NOT in the harness's retryable set
//     (`[EMPTY_RESPONSE, RATE_LIMIT, SERVER, TIMEOUT, TRANSPORT]`), so the turn
//     is not retried;
//   - more importantly, `dsh-compaction-basic`'s request-error listener starts
//     with `if (failure.code !== CONTEXT_WINDOW_EXCEEDED_CODE) return next()`,
//     so the client never gets to COMPACT and the session dies on every later
//     turn instead of recovering.
//
// The worst case of classifying an overflow wrongly is one useless compaction
// attempt.

// TestIsContextWindowExceededMarkerList is the classifier itself, table-driven
// over the phrasings upstream lists plus the ones that must NOT match.
func TestIsContextWindowExceededMarkerList(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		// The measured Tencent gateway form, which the harness's own classifier
		// misses in three of its four shapes.
		{"prompt is too long with a token count", `{"code":11115,"msg":"prompt is too long: 100001 tokens > 100000 maximum"}`, true},
		{"prompt is too long", `{"error":{"message":"prompt is too long"}}`, true},
		{"context length", `{"message":"this model's maximum context length is 65536 tokens"}`, true},
		{"context window", `{"error":"context window exceeded"}`, true},
		{"the structured extError code", `{"extError":{"code":"context_length_exceeded"}}`, true},
		{"maximum context", `{"error":"request exceeds the maximum context size"}`, true},
		{"exceeds the model context limit", `{"message":"input exceeds the model context limit"}`, true},
		{"too many tokens", `{"error":"too many tokens in the request"}`, true},
		{"reduce the length", `{"message":"Please reduce the length of the messages"}`, true},
		{"an upper-case body still matches", `{"message":"PROMPT IS TOO LONG"}`, true},
		// Negatives: a plain bad request must stay a bad request, and a server
		// error that merely contains "exceeded" must not be read as an overflow
		// (that would rotate credentials for an outage).
		{"a plain bad request", `{"message":"bad request"}`, false},
		{"a missing field", `{"error":"missing required parameter: messages"}`, false},
		{"an unrelated limit", `{"error":"rate limit exceeded"}`, false},
		{"an empty body", ``, false},
		{"an HTML error page", `<html><body>502 Bad Gateway</body></html>`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isContextWindowExceeded([]byte(testCase.body)); got != testCase.want {
				t.Fatalf("isContextWindowExceeded(%q) = %v, want %v", testCase.body, got, testCase.want)
			}
		})
	}
}

// TestUpstreamErrorClassifiesContextOverflow is the defect this fix closes: a
// context-overflow 400 used to become `invalid_request`, which neither retries
// nor triggers compaction.
func TestUpstreamErrorClassifiesContextOverflow(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantStatus int
	}{
		{
			name:       "the measured prompt-is-too-long 400",
			status:     http.StatusBadRequest,
			body:       `{"code":11115,"msg":"prompt is too long: 100001 tokens > 100000 maximum"}`,
			wantCode:   ContextWindowExceededCode,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a context-length 400",
			status:     http.StatusBadRequest,
			body:       `{"error":{"message":"This model's maximum context length is 65537 tokens"}}`,
			wantCode:   ContextWindowExceededCode,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "the structured extError form",
			status:     http.StatusBadRequest,
			body:       `{"extError":{"code":"context_length_exceeded"},"msg":"请求过大"}`,
			wantCode:   ContextWindowExceededCode,
			wantStatus: http.StatusBadRequest,
		},
		{
			// The negative that matters most: an ordinary 400 must NOT be read
			// as an overflow, or every malformed request would trigger a
			// pointless compaction attempt.
			name:       "a plain 400 stays invalid_request",
			status:     http.StatusBadRequest,
			body:       `{"message":"bad request"}`,
			wantCode:   "invalid_request",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a 400 that is really a quota problem stays quota",
			status:     http.StatusBadRequest,
			body:       `{"error":"insufficient balance"}`,
			wantCode:   "quota_exceeded",
			wantStatus: http.StatusPaymentRequired,
		},
		{
			// A 5xx carrying the word "exceeded" must stay a server error, or
			// the host would rotate credentials for an upstream outage.
			name:       "a 5xx with an exceeded marker stays a server error",
			status:     http.StatusInternalServerError,
			body:       `{"error":"context length exceeded, please retry"}`,
			wantCode:   "upstream_error",
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			errUpstream := upstreamError(&pluginapi.HTTPResponse{
				StatusCode: testCase.status,
				Body:       []byte(testCase.body),
			})
			if errUpstream == nil {
				t.Fatal("expected an error")
			}
			envelope := envelopeOf(errUpstream)
			if envelope == nil {
				t.Fatalf("not an envelope error: %v", errUpstream)
			}
			if envelope.Code != testCase.wantCode {
				t.Fatalf("code = %q, want %q (message: %s)", envelope.Code, testCase.wantCode, envelope.Message)
			}
			if status := statusOf(errUpstream, 0); status != testCase.wantStatus {
				t.Fatalf("status = %d, want %d", status, testCase.wantStatus)
			}
			// An overflow is the REQUEST's fault: the host must not rotate the
			// credential for it.
			if envelope.HTTPStatus == http.StatusUnauthorized {
				t.Fatal("a context overflow must never be classified as an auth failure")
			}
		})
	}
}

// TestUpstreamStatusErrorClassifiesContextOverflow covers the OTHER mapping the
// defect named: the shared status-to-taxonomy helper used by the balance call.
// It too mapped every 400 to the caller's generic code.
func TestUpstreamStatusErrorClassifiesContextOverflow(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"a context-overflow 400", `{"msg":"prompt is too long: 5000 tokens > 4096 maximum"}`, ContextWindowExceededCode},
		{"a plain 400", `{"message":"bad request"}`, "balance_status"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			errUpstream := upstreamStatusError([]byte(testCase.body), "balance_status", http.StatusBadRequest,
				"余额查询失败（HTTP %d）", http.StatusBadRequest)
			envelope := envelopeOf(errUpstream)
			if envelope == nil {
				t.Fatalf("not an envelope error: %v", errUpstream)
			}
			if envelope.Code != testCase.wantCode {
				t.Fatalf("code = %q, want %q", envelope.Code, testCase.wantCode)
			}
			if envelope.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", envelope.HTTPStatus)
			}
		})
	}
}

// TestContextOverflowCodeMatchesTheHarness pins the spelling. The harness routes
// on this exact string (`CONTEXT_WINDOW_EXCEEDED_CODE`, `@deepseek-ai/dsh-llm`);
// a near miss such as `CONTEXT_LENGTH_EXCEEDED` would silently disable both the
// retry and the compaction listener, which is the whole defect.
func TestContextOverflowCodeMatchesTheHarness(t *testing.T) {
	if ContextWindowExceededCode != "CONTEXT_WINDOW_EXCEEDED" {
		t.Fatalf("code = %q, want CONTEXT_WINDOW_EXCEEDED", ContextWindowExceededCode)
	}
}
