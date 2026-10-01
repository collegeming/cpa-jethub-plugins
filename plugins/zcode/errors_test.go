package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Error-classification tests.
//
// The classification decides three things the user notices:
//
//   - whether the host ROTATES the credential (401 / 402) or retries it (429 / 5xx);
//   - whether the request is treated as the REQUEST's fault (`http_status` drives
//     `IsRequestFault` in the host);
//   - what the user is told to do, which differs completely per code: `3009` means
//     "wait a moment", `1005` means "this account is done for the day", and `3012`
//     means "stop immediately, retrying costs you the account".
//
// Two rules matter most, and both are regressions from the reference:
//
//   - `3012` is NEVER an authentication failure and NEVER retried. It carries an
//     account penalty, so a retry loop turns one block into a disablement.
//   - `3009` is never mistaken for `1005`. They share HTTP 429, but rotating the
//     account on a concurrency rejection retires a perfectly usable credential.

// classifiedEnvelope runs the classifier and returns the envelope.
func classifiedEnvelope(t *testing.T, status int, body string) *abiboot.EnvelopeError {
	t.Helper()
	errClassified := classifyUpstreamError(status, []byte(body))
	envelope := &abiboot.EnvelopeError{}
	if !asEnvelope(errClassified, envelope) {
		t.Fatalf("classifyUpstreamError returned %T, want a plugin envelope error", errClassified)
	}
	return envelope
}

// TestClassifyUpstreamError covers the whole table.
func TestClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantStatus int
		retryable  bool
		wantInText string
	}{
		{
			name: "3009 concurrency limit is a retryable 429",
			// The reference captured this exact body.
			status: http.StatusTooManyRequests,
			body:   `{"code":3009,"msg":"model concurrency limit exceeded"}`,
			// Retryable, but NOT a credential fault: 429 is not in the
			// rotate-on-this set.
			wantCode: "concurrency_limited", wantStatus: http.StatusTooManyRequests, retryable: true,
			wantInText: "并发限流",
		},
		{
			name: "1005 quota exhausted is a NON-retryable 402",
			// 402 is what makes the host rotate the credential instead of backing
			// off and retrying the same exhausted one.
			status: http.StatusTooManyRequests,
			body:   `{"code":1005,"msg":"exceed quota limit"}`,
			// ⚠ Note the status: the upstream sends 429 here too. Only the body
			// distinguishes the two.
			wantCode: "quota_exhausted", wantStatus: http.StatusPaymentRequired, retryable: false,
			wantInText: "额度已用尽",
		},
		{
			name:   "1113 insufficient balance is also a quota failure",
			status: http.StatusTooManyRequests,
			body:   `{"code":1113,"msg":"余额不足或无可用资源包"}`,
			// The ultra/coding-plan side spells the same condition differently.
			wantCode: "quota_exhausted", wantStatus: http.StatusPaymentRequired, retryable: false,
			wantInText: "额度已用尽",
		},
		{
			name:   "3012 risk control is a NON-retryable 403 and never an auth failure",
			status: http.StatusMethodNotAllowed,
			// The measured body is a 405, which is unusual enough that a
			// status-only classifier would misread it as a routing problem.
			body:       `{"code":3012,"msg":"request has been blocked due to unusual activity."}`,
			wantCode:   "risk_control",
			wantStatus: http.StatusForbidden,
			retryable:  false,
			// The message has to warn about the penalty, because the user is the
			// only one who can stop the retry loop.
			wantInText: "冷却惩罚",
		},
		{
			name:       "3007 captcha is a retryable 429",
			status:     http.StatusBadRequest,
			body:       `{"code":3007,"msg":"captcha verify failed"}`,
			wantCode:   "captcha_required",
			wantStatus: http.StatusTooManyRequests,
			retryable:  true,
			wantInText: "人机验证",
		},
		{
			name:       "3001 parameter error is the request's fault",
			status:     http.StatusBadRequest,
			body:       `{"code":3001,"msg":"parameter error"}`,
			wantCode:   "parameter_error",
			wantStatus: http.StatusBadRequest,
			retryable:  false,
			// The message names the usual cause, which is a missing device id.
			wantInText: "X-Device-Mid",
		},
		{
			name:       "401 is a credential failure",
			status:     http.StatusUnauthorized,
			body:       `{"code":1002,"msg":"invalid token"}`,
			wantCode:   "auth",
			wantStatus: http.StatusUnauthorized,
			retryable:  false,
			wantInText: "重新登录",
		},
		{
			name:       "403 is a credential failure",
			status:     http.StatusForbidden,
			body:       `{"msg":"forbidden"}`,
			wantCode:   "auth",
			wantStatus: http.StatusUnauthorized,
			retryable:  false,
			wantInText: "重新登录",
		},
		{
			name:       "an unclassified 429 is a retryable rate limit",
			status:     http.StatusTooManyRequests,
			body:       `{"msg":"slow down"}`,
			wantCode:   "rate_limited",
			wantStatus: http.StatusTooManyRequests,
			retryable:  true,
			wantInText: "限流",
		},
		{
			name:       "a 502 stays retryable",
			status:     http.StatusBadGateway,
			body:       `<html>bad gateway</html>`,
			wantCode:   "upstream_error",
			wantStatus: http.StatusBadGateway,
			retryable:  true,
			wantInText: "上游服务异常",
		},
		{
			name:       "a 500 stays retryable",
			status:     http.StatusInternalServerError,
			body:       ``,
			wantCode:   "upstream_error",
			wantStatus: http.StatusBadGateway,
			retryable:  true,
			wantInText: "HTTP 500",
		},
		{
			name:       "a 408 is a retryable timeout",
			status:     http.StatusRequestTimeout,
			body:       `{"msg":"timeout"}`,
			wantCode:   "upstream_timeout",
			wantStatus: http.StatusGatewayTimeout,
			retryable:  true,
			wantInText: "timeout",
		},
		{
			name:       "a 504 is a retryable timeout",
			status:     http.StatusGatewayTimeout,
			body:       `{"msg":"gateway timeout"}`,
			wantCode:   "upstream_timeout",
			wantStatus: http.StatusGatewayTimeout,
			retryable:  true,
			wantInText: "timeout",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope := classifiedEnvelope(t, tc.status, tc.body)
			if envelope.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", envelope.Code, tc.wantCode)
			}
			if envelope.HTTPStatus != tc.wantStatus {
				t.Errorf("http status = %d, want %d", envelope.HTTPStatus, tc.wantStatus)
			}
			if envelope.Retryable != tc.retryable {
				t.Errorf("retryable = %v, want %v", envelope.Retryable, tc.retryable)
			}
			if !strings.Contains(envelope.Message, tc.wantInText) {
				t.Errorf("message %q does not contain %q", envelope.Message, tc.wantInText)
			}
			// Every classification must carry a status: the host reads it to decide
			// whether the request was at fault, and a status-less failure is
			// effectively unclassified.
			if envelope.HTTPStatus == 0 {
				t.Error("the failure carries no HTTP status")
			}
		})
	}
}

// TestConcurrencyIsNeverClassifiedAsQuota is the most consequential single rule
// here.
//
// Both codes arrive on HTTP 429, so a status-only classifier cannot tell them
// apart. Getting it wrong in this direction retires a usable credential; getting it
// wrong in the other direction makes a deterministic quota failure look retryable,
// which is what produced the reference's "retried model request (5/5)" screenshot.
func TestConcurrencyIsNeverClassifiedAsQuota(t *testing.T) {
	concurrencyBody := `{"code":3009,"msg":"model concurrency limit exceeded"}`
	if isQuotaExhausted(http.StatusTooManyRequests, parseUpstreamEnvelope([]byte(concurrencyBody), http.StatusTooManyRequests)) {
		t.Fatal("3009 was classified as quota exhaustion, which would retire a usable account")
	}
	if !isConcurrencyLimited(http.StatusTooManyRequests, parseUpstreamEnvelope([]byte(concurrencyBody), http.StatusTooManyRequests)) {
		t.Fatal("3009 was not recognised as concurrency limiting")
	}

	quotaBody := `{"code":1005,"msg":"exceed quota limit"}`
	if isConcurrencyLimited(http.StatusTooManyRequests, parseUpstreamEnvelope([]byte(quotaBody), http.StatusTooManyRequests)) {
		t.Fatal("1005 was classified as concurrency limiting, which would retry a deterministic failure")
	}
	if !isQuotaExhausted(http.StatusTooManyRequests, parseUpstreamEnvelope([]byte(quotaBody), http.StatusTooManyRequests)) {
		t.Fatal("1005 was not recognised as quota exhaustion")
	}
}

// TestQuotaWordingFallbackStaysNarrow covers the text fallback.
//
// ⚠ The fallback only exists for a server that changes the numeric code while
// keeping the meaning, so the patterns have to be tight. A generic word such as
// "quota" also appears in a model's own answer about quotas, and matching that
// would turn a normal completion into a quota failure.
func TestQuotaWordingFallbackStaysNarrow(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		isQuota bool
	}{
		{"the documented wording", http.StatusTooManyRequests, `{"msg":"exceed quota limit"}`, true},
		{"a synonym", http.StatusTooManyRequests, `{"msg":"Quota has been exhausted"}`, true},
		{"the Chinese wording", http.StatusTooManyRequests, `{"msg":"余额不足"}`, true},
		{"a generic mention of quota is NOT a quota failure", http.StatusOK,
			`{"msg":"Here is how you check your quota in the console"}`, false},
		{"a model explaining rate limits is NOT a quota failure", http.StatusOK,
			`{"msg":"The API returns 429 when the limit is reached"}`, false},
		{"an unrelated 429 stays a rate limit", http.StatusTooManyRequests, `{"msg":"too many requests"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope := parseUpstreamEnvelope([]byte(tc.body), tc.status)
			if got := isQuotaExhausted(tc.status, envelope); got != tc.isQuota {
				t.Fatalf("isQuotaExhausted = %v, want %v", got, tc.isQuota)
			}
		})
	}
}

// TestRiskControlIsNeverRetryable is asserted separately from the table because it
// is the one classification whose failure mode is destructive.
func TestRiskControlIsNeverRetryable(t *testing.T) {
	for _, status := range []int{
		http.StatusMethodNotAllowed, http.StatusBadRequest, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusOK,
	} {
		envelope := classifiedEnvelope(t, status, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`)
		if envelope.Code != "risk_control" {
			t.Errorf("status %d: code = %q, want risk_control", status, envelope.Code)
		}
		if envelope.Retryable {
			t.Errorf("status %d: 3012 is marked retryable, but retrying escalates the penalty", status)
		}
		if envelope.HTTPStatus == http.StatusUnauthorized {
			t.Errorf("status %d: 3012 was classified as an authentication failure; "+
				"it is a risk-control block, and reporting it as a dead credential sends the user to re-login for nothing", status)
		}
		// The message must warn, because the user is the only one who can stop.
		for _, want := range []string{"风控", "冷却惩罚", "不会自动重试"} {
			if !strings.Contains(envelope.Message, want) {
				t.Errorf("status %d: message %q does not contain %q", status, envelope.Message, want)
			}
		}
	}
}

// TestUpstreamEnvelopeCodeParsing covers the flexible code decoding.
func TestUpstreamEnvelopeCodeParsing(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		status      int
		code        int
		present     bool
		wantSuccess bool
	}{
		{"a numeric code", `{"code":0}`, http.StatusOK, 0, true, true},
		{"a string code", `{"code":"0"}`, http.StatusOK, 0, true, true},
		{"a non-zero code", `{"code":3009}`, http.StatusOK, 3009, true, false},
		{"an absent code inherits the HTTP status", `{"msg":"no code here"}`, http.StatusTeapot, http.StatusTeapot, true, false},
		{"a present 0 on a 200 is the only success", `{"code":0}`, http.StatusOK, 0, true, true},
		{"a present 0 on a 500 is NOT success", `{"code":0}`, http.StatusInternalServerError, 0, true, true},
		{"a null code is absent", `{"code":null}`, http.StatusBadRequest, http.StatusBadRequest, true, false},
		{"a non-numeric code never reads as success", `{"code":"abc"}`, http.StatusOK, -1, true, false},
		{"a non-object body keeps the status as the code", `<html>oops</html>`, http.StatusBadGateway, http.StatusBadGateway, true, false},
		{"an empty body keeps the status as the code", ``, http.StatusBadGateway, http.StatusBadGateway, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope := parseUpstreamEnvelope([]byte(tc.body), tc.status)
			if envelope.Code.present != tc.present {
				t.Fatalf("code present = %v, want %v", envelope.Code.present, tc.present)
			}
			if envelope.Code.value != tc.code {
				t.Fatalf("code = %d, want %d", envelope.Code.value, tc.code)
			}
			if envelope.success() != tc.wantSuccess {
				t.Fatalf("success = %v, want %v", envelope.success(), tc.wantSuccess)
			}
		})
	}
}

// TestUpstreamEnvelopeMessagePrefersTheServersOwnWording covers the message
// extraction, which is what makes a failure diagnosable.
func TestUpstreamEnvelopeMessagePrefersTheServersOwnWording(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		fallback string
		want     string
	}{
		{"msg wins", `{"code":1,"msg":"from msg","message":"from message"}`, "fallback", "from msg"},
		{"message is the second choice", `{"code":1,"message":"from message"}`, "fallback", "from message"},
		{"a non-JSON body is shown verbatim", `<html>gateway error</html>`, "fallback", "<html>gateway error</html>"},
		{"the fallback is used when nothing else is present", `{"code":1}`, "fallback", "fallback"},
		{"blank fields fall through", `{"msg":"   ","message":""}`, "fallback", "fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope := parseUpstreamEnvelope([]byte(tc.body), http.StatusOK)
			if got := envelope.message(tc.fallback); got != tc.want {
				t.Fatalf("message = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDescribeUpstreamErrorCarriesTheRawText(t *testing.T) {
	envelope := parseUpstreamEnvelope([]byte(`{"code":3009,"msg":"model concurrency limit exceeded"}`),
		http.StatusTooManyRequests)
	text := describeUpstreamError(http.StatusTooManyRequests, envelope)
	if !strings.Contains(text, "model concurrency limit exceeded") {
		t.Fatalf("the description dropped the server's wording: %q", text)
	}
	// The advice is specific to the code: "wait a moment", NOT "switch accounts".
	if !strings.Contains(text, "稍后重试") {
		t.Errorf("the concurrency advice does not say to wait: %q", text)
	}
	if strings.Contains(text, "添加账号") {
		t.Errorf("the concurrency advice suggests adding an account, which does not help: %q", text)
	}
}

// TestExecutorErrorMappingEndToEnd drives the classifier through the executor, so
// the mapping is asserted where it is actually consumed.
func TestExecutorErrorMappingEndToEnd(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name: "concurrency", status: http.StatusTooManyRequests,
			body:       `{"code":3009,"msg":"model concurrency limit exceeded"}`,
			wantStatus: http.StatusTooManyRequests, wantCode: "concurrency_limited",
		},
		{
			name: "quota", status: http.StatusTooManyRequests,
			body:       `{"code":1005,"msg":"exceed quota limit"}`,
			wantStatus: http.StatusPaymentRequired, wantCode: "quota_exhausted",
		},
		{
			name: "risk control", status: http.StatusMethodNotAllowed,
			body:       `{"code":3012,"msg":"request has been blocked due to unusual activity."}`,
			wantStatus: http.StatusForbidden, wantCode: "risk_control",
		},
		{
			name: "a dead credential", status: http.StatusUnauthorized,
			body:       `{"code":1002,"msg":"invalid token"}`,
			wantStatus: http.StatusUnauthorized, wantCode: "auth",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.install(t)
			// The concurrency case is the one that retries; a zero backoff keeps
			// the assertion about the MAPPING from costing 4.5 seconds.
			cfg := settings()
			cfg.ConcurrencyRetryBaseMS = 0
			withSettings(t, cfg)
			fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				return httpResponse(tc.status, tc.body), nil
			}
			_, errStream := handleExecutorExecuteStream(testHost(),
				executorRawPayload(t, sampleCredential(),
					`{"model":"GLM-5.3-Flash","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
			if errStream == nil {
				t.Fatal("expected a failure")
			}
			envelope := &abiboot.EnvelopeError{}
			if !asEnvelope(errStream, envelope) {
				t.Fatalf("error type = %T", errStream)
			}
			if envelope.HTTPStatus != tc.wantStatus {
				t.Errorf("http status = %d, want %d (message: %s)",
					envelope.HTTPStatus, tc.wantStatus, envelope.Message)
			}
			if envelope.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", envelope.Code, tc.wantCode)
			}
		})
	}
}

// TestConcurrencyIsRetriedButQuotaIsNot is the behavioural half of the two-429
// rule, asserted through the executor.
//
// The retry only ever happens for the concurrency code, and it does not rotate the
// credential, so the SAME credential appears in every attempt.
func TestConcurrencyIsRetriedButQuotaIsNot(t *testing.T) {
	t.Run("3009 is retried and keeps the same credential", func(t *testing.T) {
		fake := newFakeHost()
		// ⚠ install() finishes by installing DefaultConfig, so it has to run
		// BEFORE withSettings — the other order silently restores the production
		// backoff and the test waits 4.5 seconds for nothing.
		fake.install(t)
		cfg := settings()
		// A zero backoff keeps the test instant; the retry COUNT is what is under
		// test here.
		cfg.ConcurrencyRetryMax = 2
		cfg.ConcurrencyRetryBaseMS = 0
		withSettings(t, cfg)
		attempts := 0
		fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			attempts++
			if attempts <= 2 {
				return httpResponse(http.StatusTooManyRequests,
					`{"code":3009,"msg":"model concurrency limit exceeded"}`), nil
			}
			return httpResponse(http.StatusOK, fixtureSSEBody(t)), nil
		}

		value, errStream := handleExecutorExecuteStream(testHost(),
			executorRawPayload(t, sampleCredential(),
				`{"model":"GLM-5.3-Flash","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
		if errStream != nil {
			t.Fatalf("execute_stream: %v", errStream)
		}
		// 1 initial + 2 retries = 3, then success.
		if attempts != 3 {
			t.Fatalf("attempts = %d, want 3 (one initial plus two retries)", attempts)
		}
		if response, ok := value.(executorStreamResponse); !ok || len(response.Chunks) == 0 {
			t.Fatalf("the retried stream produced no chunks: %#v", value)
		}
		// Every attempt carried the SAME bearer token: the credential was not
		// rotated, which is the point.
		calls := fake.callsFor(MessagesPath)
		if len(calls) != 3 {
			t.Fatalf("inference calls = %d, want 3", len(calls))
		}
		first := calls[0].Headers.Get("Authorization")
		for index, call := range calls {
			if call.Headers.Get("Authorization") != first {
				t.Errorf("call %d used a different credential, but a concurrency rejection must not rotate", index)
			}
		}
	})

	t.Run("1005 is not retried at all", func(t *testing.T) {
		fake := newFakeHost()
		fake.install(t)
		cfg := settings()
		cfg.ConcurrencyRetryMax = 2
		cfg.ConcurrencyRetryBaseMS = 0
		withSettings(t, cfg)
		attempts := 0
		fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			attempts++
			return httpResponse(http.StatusTooManyRequests, `{"code":1005,"msg":"exceed quota limit"}`), nil
		}
		_, errStream := handleExecutorExecuteStream(testHost(),
			executorRawPayload(t, sampleCredential(),
				`{"model":"GLM-5.3-Flash","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
		if errStream == nil {
			t.Fatal("expected a failure")
		}
		// Exactly one attempt: a deterministic failure must not be retried, which
		// is what produced the reference's "retried model request (5/5)".
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (a quota failure is deterministic)", attempts)
		}
	})

	t.Run("3012 is not retried either", func(t *testing.T) {
		fake := newFakeHost()
		fake.install(t)
		cfg := settings()
		cfg.ConcurrencyRetryMax = 2
		cfg.ConcurrencyRetryBaseMS = 0
		withSettings(t, cfg)
		attempts := 0
		fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			attempts++
			return httpResponse(http.StatusMethodNotAllowed,
				`{"code":3012,"msg":"request has been blocked due to unusual activity."}`), nil
		}
		_, errStream := handleExecutorExecuteStream(testHost(),
			executorRawPayload(t, sampleCredential(),
				`{"model":"GLM-5.3-Flash","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
		if errStream == nil {
			t.Fatal("expected a failure")
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1: every retry of a 3012 escalates the account penalty", attempts)
		}
		envelope := &abiboot.EnvelopeError{}
		if !asEnvelope(errStream, envelope) {
			t.Fatalf("error type = %T", errStream)
		}
		if envelope.HTTPStatus == http.StatusUnauthorized {
			t.Error("3012 surfaced as an authentication failure, which would send the user to re-login pointlessly")
		}
	})
}

// TestParameterErrorNamesTheDeviceIdAsTheUsualCause covers the diagnostic that
// saves the most time: `3001` is almost always a missing `X-Device-Mid`.
func TestParameterErrorNamesTheDeviceIdAsTheUsualCause(t *testing.T) {
	envelope := classifiedEnvelope(t, http.StatusBadRequest, `{"code":3001,"msg":"parameter error"}`)
	if !strings.Contains(envelope.Message, "X-Device-Mid") {
		t.Fatalf("the message does not mention the usual cause: %q", envelope.Message)
	}
}

// fixtureSSEBody renders a minimal successful Anthropic stream.
func fixtureSSEBody(t *testing.T) string {
	t.Helper()
	fixture := loadCapturedFixture(t)
	return fixture.Response.SSE
}

// TestStatusOfReadsTheEnvelopeStatus covers the helper the pages use.
func TestStatusOfReadsTheEnvelopeStatus(t *testing.T) {
	if got := statusOf(transportError("x", "y"), http.StatusTeapot); got != http.StatusBadGateway {
		t.Errorf("statusOf = %d, want the envelope's own status", got)
	}
	if got := statusOf(errFakeTransport, http.StatusTeapot); got != http.StatusTeapot {
		t.Errorf("statusOf = %d, want the fallback for a plain error", got)
	}
}

// TestErrorMessagesAreValidJSONWhenMarshalled guards the envelope contract: the
// host decodes the failure envelope, so a message containing a raw control
// character would break the whole reply rather than just look wrong.
func TestErrorMessagesAreValidJSONWhenMarshalled(t *testing.T) {
	envelope := classifiedEnvelope(t, http.StatusBadGateway, "{\"code\":500,\"msg\":\"line one\\nline two\\ttab\"}")
	encoded, errMarshal := json.Marshal(envelope)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if !json.Valid(encoded) {
		t.Fatalf("the marshalled envelope is not valid JSON: %s", encoded)
	}
	var decoded abiboot.EnvelopeError
	if errUnmarshal := json.Unmarshal(encoded, &decoded); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if decoded.Code != envelope.Code || decoded.HTTPStatus != envelope.HTTPStatus {
		t.Fatalf("the round-tripped envelope differs: %#v vs %#v", decoded, envelope)
	}
}
