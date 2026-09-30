package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// noQueueSleep swaps the injectable queue wait for a recorder. No test in this
// package ever waits for a real queue delay: the delay is observed instead.
func noQueueSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waited []time.Duration
	previous := queueSleep
	queueSleep = func(delay time.Duration) { waited = append(waited, delay) }
	t.Cleanup(func() { queueSleep = previous })
	return &waited
}

// envelopeError decodes an error as the host reads it, so a test asserts the
// classification rather than the message text.
func envelopeError(t *testing.T, err error) *abiboot.EnvelopeError {
	t.Helper()
	if err == nil {
		t.Fatal("expected a failure, got nil")
	}
	typed, ok := err.(*abiboot.EnvelopeError)
	if !ok {
		t.Fatalf("error type = %T, want *abiboot.EnvelopeError", err)
	}
	return typed
}

// TestParseBusinessErrorUnwrapsNestedLayers covers the two shapes upstream must
// both support (`model-queue.ts:170-186`): a plain object, and a JSON STRING that
// itself decodes to an object one or more layers down.
func TestParseBusinessErrorUnwrapsNestedLayers(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantOK      bool
		wantDepth   int
		wantCode    string
		wantMessage string
		wantCodes   []string
	}{
		{
			name:      "plain object",
			body:      `{"code":110,"message":"Billing daily count exceeded","type":"model_error"}`,
			wantOK:    true,
			wantDepth: 1, wantCode: "110", wantMessage: "Billing daily count exceeded",
			wantCodes: []string{"110"},
		},
		{
			// The measured shape: the business code is one JSON-string layer
			// below the frame's own (transport-status) code.
			name:      "JSON-string-wrapped object",
			body:      `{"code":403,"message":"{\"code\":\"10605\",\"message\":\"queued\"}"}`,
			wantOK:    true,
			wantDepth: 2, wantCode: "10605",
			wantMessage: "queued (10605)",
			wantCodes:   []string{"403", "10605"},
		},
		{
			name:        "two JSON-string layers",
			body:        `{"code":403,"message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":false,\\\"retryAfterSeconds\\\":2}\"}"}`,
			wantOK:      true,
			wantDepth:   3,
			wantCode:    "10605",
			wantMessage: "isQueued",
			wantCodes:   []string{"403", "10605"},
		},
		{
			name:      "nested data key",
			body:      `{"data":{"code":"10605","message":"queued"}}`,
			wantOK:    true,
			wantDepth: 2, wantCode: "10605", wantMessage: "queued (10605)",
			wantCodes: []string{"10605"},
		},
		{
			name:      "code as a string number",
			body:      `{"code":"110","message":"Billing daily count exceeded"}`,
			wantOK:    true,
			wantDepth: 1, wantCode: "110", wantMessage: "Billing daily count exceeded",
			wantCodes: []string{"110"},
		},
		{
			name:   "plain text frame is not a JSON object",
			body:   `[FAIL]node:oa_qwen-plus-2025-04-28 msg:Execution failed`,
			wantOK: false,
		},
		{
			name:   "empty body",
			body:   "   ",
			wantOK: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			parsed, ok := parseBusinessError(testCase.body)
			if ok != testCase.wantOK {
				t.Fatalf("ok = %v, want %v (body %s)", ok, testCase.wantOK, testCase.body)
			}
			if !testCase.wantOK {
				return
			}
			if parsed.Depth != testCase.wantDepth {
				t.Errorf("Depth = %d, want %d", parsed.Depth, testCase.wantDepth)
			}
			if parsed.Code != testCase.wantCode {
				t.Errorf("Code = %q, want %q", parsed.Code, testCase.wantCode)
			}
			if !slices.Equal(parsed.Codes, testCase.wantCodes) {
				t.Errorf("Codes = %v, want %v (every layer's code is matchable)", parsed.Codes, testCase.wantCodes)
			}
			if !strings.Contains(parsed.text(), testCase.wantMessage) {
				t.Errorf("text() = %q, want it to contain %q", parsed.text(), testCase.wantMessage)
			}
		})
	}
}

// TestClassifyBusinessErrorIsIndependentOfDepth is the load-bearing rule from
// upstream `daf9fb1`: the wrapper's own `code` must never gate the decision.
func TestClassifyBusinessErrorIsIndependentOfDepth(t *testing.T) {
	cases := []struct {
		name string
		body string
		want businessKind
	}{
		{"quota at the top level", `{"code":110,"message":"Billing daily count exceeded"}`, businessQuota},
		{"quota as a string code", `{"code":"110","message":"Billing daily count exceeded"}`, businessQuota},
		{"quota by message only", `{"code":500,"message":"Billing daily count exceeded"}`, businessQuota},
		{"quota in Chinese", `{"code":500,"message":"账户余额不足，请充值"}`, businessQuota},
		{"quota nested", `{"code":403,"message":"{\"code\":110,\"message\":\"Billing daily count exceeded\"}"}`, businessQuota},
		{"queue at the top level", `{"code":10605,"message":"queued"}`, businessQueue},
		{"queue nested under a 403 wrapper", `{"code":403,"message":"{\"code\":\"10605\",\"message\":\"queued\"}"}`, businessQueue},
		{"queue by isQueued marker without any code", `{"message":"{\"isQueued\":true,\"retryAfterSeconds\":3}"}`, businessQueue},
		// The momentary queue the user reported as "one retry succeeds":
		// `isQueued` is FALSE, so a `=== true` test would miss it.
		{"momentary queue has isQueued false", `{"message":"{\"isQueued\":false,\"serviceAvailable\":true,\"waitTime\":0,\"retryAfterSeconds\":2}"}`, businessQueue},
		{"queue by serviceAvailable alone", `{"message":"{\"serviceAvailable\":true}"}`, businessQueue},
		{"unknown code stays unknown", `{"code":500,"message":"Execution failed"}`, businessUnknown},
		{"generic failure text stays unknown", `{"code":500,"message":"Balance of the account is fine"}`, businessUnknown},
		{"gateway shape without a code", `{"statusCodeValue":500,"message":"boom","type":"model_error"}`, businessUnknown},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			parsed, ok := parseBusinessError(testCase.body)
			if !ok {
				t.Fatalf("body %s did not parse", testCase.body)
			}
			if got := classifyBusinessError(parsed); got != testCase.want {
				t.Fatalf("classifyBusinessError = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestBillingTextFallbackStaysNarrow guards the trap recorded at
// `model-queue.ts:120-126`: the fallback must not fire on ordinary prose that
// merely mentions a balance or a quota, or a healthy failure becomes a
// non-retryable 402.
func TestBillingTextFallbackStaysNarrow(t *testing.T) {
	billing := []string{
		"Billing daily count exceeded",
		"billing_error",
		"daily count exceeded",
		"Quota exceeded for this model",
		"insufficient balance",
		"账户余额不足",
		"额度已耗尽",
	}
	for _, text := range billing {
		if !looksLikeBillingError(text) {
			t.Errorf("looksLikeBillingError(%q) = false, want true", text)
		}
	}
	// Model prose that happens to discuss the same nouns must NOT match.
	harmless := []string{
		"The account balance field is described below.",
		"Please write a function that returns the remaining quota.",
		"Execution failed",
		"node unavailable",
		"模型会返回余额字段",
	}
	for _, text := range harmless {
		if looksLikeBillingError(text) {
			t.Errorf("looksLikeBillingError(%q) = true, want false (too broad)", text)
		}
	}
}

// TestQueueDelayUsesTheServersValueAndCapsIt pins `queueDelayMs`
// (`model-queue.ts:248-255`): the three encodings in priority order, then the
// 10s cap.
func TestQueueDelayUsesTheServersValueAndCapsIt(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		want   int
		wantOK bool
	}{
		{"retry_after_ms wins", `{"retry_after_ms":2000,"retryAfterMs":99,"retryAfterSeconds":7}`, 2000, true},
		{"retryAfterMs next", `{"retryAfterMs":250,"retryAfterSeconds":7}`, 250, true},
		{"retryAfterSeconds is milliseconds", `{"retryAfterSeconds":3}`, 3000, true},
		{"below the cap is honoured exactly", `{"retryAfterSeconds":2}`, 2000, true},
		{"at the cap stays", `{"retryAfterSeconds":10}`, 10_000, true},
		{"above the cap is clamped", `{"retryAfterSeconds":30}`, QueueMaxDelayMS, true},
		{"a negative value is ignored, never zero", `{"retryAfterSeconds":-5}`, 0, false},
		{"no delay fields at all", `{"isQueued":true}`, 0, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			state := &queueState{}
			mergeQueueFields(&businessError{Queue: state}, decodeJSONObject(testCase.body))
			got, ok := state.delayMS()
			if ok != testCase.wantOK {
				t.Fatalf("ok = %v, want %v", ok, testCase.wantOK)
			}
			if ok && got != testCase.want {
				t.Fatalf("delay = %d, want %d", got, testCase.want)
			}
		})
	}
}

// TestNormalizeFrameClassifiesBusinessErrors is the reported defect: a frame
// without `choices` used to become a generic retryable 502 whatever its business
// code said.
func TestNormalizeFrameClassifiesBusinessErrors(t *testing.T) {
	cases := []struct {
		name       string
		inner      string
		wantStatus int
		wantCode   string
		retryable  bool
	}{
		{
			name:       "quota 110 is a non-retryable 402",
			inner:      `{"code":110,"message":"Billing daily count exceeded","type":"model_error"}`,
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "QUOTA_EXCEEDED",
			retryable:  false,
		},
		{
			name:       "quota reached by message alone is still a 402",
			inner:      `{"code":500,"message":"Billing daily count exceeded"}`,
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "QUOTA_EXCEEDED",
			retryable:  false,
		},
		{
			// The frame the user's `(403/model_error)` suffix came from: the
			// business code is one JSON-string layer down.
			name:       "nested queue 10605 is a distinguishable queue condition",
			inner:      `{"code":403,"message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"retryAfterSeconds\\\":30}\"}","type":"model_error"}`,
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "QUEUE",
			retryable:  true,
		},
		{
			// Only the NESTED layer names the code — no queue marker anywhere.
			// A reader that gates on the outer `code` (403) sees no business code
			// at all and falls through to the generic 502.
			name:       "a bare nested 10605 is still a queue",
			inner:      `{"code":403,"message":"{\"code\":\"10605\"}"}`,
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "QUEUE",
			retryable:  true,
		},
		{
			name:       "a bare nested 110 is still a quota",
			inner:      `{"code":403,"message":"{\"code\":\"110\"}"}`,
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "QUOTA_EXCEEDED",
			retryable:  false,
		},
		{
			name:       "unknown error frame keeps the generic 502",
			inner:      `{"code":500,"message":"Execution failed"}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_error",
			retryable:  true,
		},
		{
			name:       "gateway shape without a code keeps the generic 502",
			inner:      `{"statusCodeValue":500,"message":"boom","type":"model_error"}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_error",
			retryable:  true,
		},
		{
			name:       "plain text failure keeps the generic 502",
			inner:      `[FAIL]node:oa_qwen-plus-2025-04-28 msg:Execution failed`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_error",
			retryable:  true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			waited := noQueueSleep(t)
			frame := `{"body":` + jsonString(testCase.inner) + `,"statusCodeValue":200,"statusCode":"OK"}`
			inner, errFrame := normalizeFrame(frame, true)
			if errFrame == nil {
				t.Fatalf("frame was accepted as a chat frame: %q", inner)
			}
			typed := envelopeError(t, errFrame)
			if typed.HTTPStatus != testCase.wantStatus {
				t.Errorf("http_status = %d, want %d", typed.HTTPStatus, testCase.wantStatus)
			}
			if typed.Code != testCase.wantCode {
				t.Errorf("code = %q, want %q", typed.Code, testCase.wantCode)
			}
			if typed.Retryable != testCase.retryable {
				t.Errorf("retryable = %v, want %v", typed.Retryable, testCase.retryable)
			}
			if testCase.wantCode == "QUOTA_EXCEEDED" && len(*waited) != 0 {
				t.Errorf("a quota error must not wait: %v", *waited)
			}
		})
	}
}

// TestNormalizeFrameHonoursAndCapsTheQueueDelay asserts the computed wait rather
// than sleeping it, so the test stays fast and deterministic.
func TestNormalizeFrameHonoursAndCapsTheQueueDelay(t *testing.T) {
	cases := []struct {
		name      string
		inner     string
		wantDelay time.Duration
	}{
		{
			name:      "the server value below the cap is used as-is",
			inner:     `{"code":"10605","message":"{\"isQueued\":false,\"serviceAvailable\":true,\"retryAfterSeconds\":2}"}`,
			wantDelay: 2 * time.Second,
		},
		{
			name:      "retry_after_ms is taken directly",
			inner:     `{"code":"10605","message":"{\"retry_after_ms\":1500}"}`,
			wantDelay: 1500 * time.Millisecond,
		},
		{
			name:      "a 30s request is capped at 10s",
			inner:     `{"code":"10605","message":"{\"isQueued\":true,\"retryAfterSeconds\":30}"}`,
			wantDelay: QueueMaxDelayMS * time.Millisecond,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			waited := noQueueSleep(t)
			frame := `{"body":` + jsonString(testCase.inner) + `,"statusCodeValue":200}`
			_, errFrame := normalizeFrame(frame, true)
			typed := envelopeError(t, errFrame)
			if typed.Code != "QUEUE" {
				t.Fatalf("code = %q, want QUEUE", typed.Code)
			}
			if len(*waited) != 1 {
				t.Fatalf("waits = %v, want exactly one honoured delay", *waited)
			}
			if (*waited)[0] != testCase.wantDelay {
				t.Fatalf("delay = %v, want %v", (*waited)[0], testCase.wantDelay)
			}
		})
	}

	// No stated delay → no wait at all; the host's own backoff applies.
	t.Run("no server delay means no wait", func(t *testing.T) {
		waited := noQueueSleep(t)
		frame := `{"body":"{\"code\":\"10605\",\"message\":\"queued\"}","statusCodeValue":200}`
		_, errFrame := normalizeFrame(frame, true)
		if typed := envelopeError(t, errFrame); typed.Code != "QUEUE" {
			t.Fatalf("code = %q, want QUEUE", typed.Code)
		}
		if len(*waited) != 0 {
			t.Fatalf("waits = %v, want none", *waited)
		}
	})
}

// TestNormalizeFrameKeepsWorkingFramesWorking guards the two shapes that must be
// untouched by the classification above.
func TestNormalizeFrameKeepsWorkingFramesWorking(t *testing.T) {
	chat := `{"body":"{\"choices\":[{\"index\":0}]}","statusCodeValue":200}`
	inner, errFrame := normalizeFrame(chat, true)
	if errFrame != nil {
		t.Fatalf("a chat frame was rejected: %v", errFrame)
	}
	if inner != `{"choices":[{"index":0}]}` {
		t.Fatalf("inner = %q", inner)
	}
	// A done frame still terminates the stream.
	if _, errFrame := normalizeFrame(`{"body":"[DONE]","statusCodeValue":200}`, true); errFrame != nil {
		t.Fatalf("[DONE] was rejected: %v", errFrame)
	}
}

// TestEnvelopeErrorMessageCarriesTheNestedCode is the envelope-level half of the
// defect: only the top-level `code` used to survive, so the nested business code
// was dropped from every message the user sees. The assertions are EXACT because
// the wrapper's own text contains the nested code as data — a substring check
// would pass even with the top-level-only extraction restored.
func TestEnvelopeErrorMessageCarriesTheNestedCode(t *testing.T) {
	cases := []struct {
		name  string
		inner string
		want  string
	}{
		{
			// With top-level-only extraction this renders
			// `{"code":"10605","message":"queued"} (403)` — the raw wrapper,
			// carrying the transport status as the cause.
			name:  "nested queue code replaces the transport code",
			inner: `{"code":403,"message":"{\"code\":\"10605\",\"message\":\"queued\"}"}`,
			want:  "queued (10605)",
		},
		{
			name:  "top-level code still renders",
			inner: `{"code":"invalid_model_error","message":"Unsupported model \"qfmodel\""}`,
			want:  `Unsupported model "qfmodel" (invalid_model_error)`,
		},
		{
			// A text frame has no layers, so it passes through untouched —
			// including its own `node:`/`msg:` decoration.
			name:  "plain text passes through",
			inner: `[FAIL]node:1 msg:Execution failed`,
			want:  `[FAIL]node:1 msg:Execution failed`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := envelopeErrorMessage(testCase.inner); got != testCase.want {
				t.Fatalf("envelopeErrorMessage = %q, want %q", got, testCase.want)
			}
		})
	}
}

// jsonString renders a Go string as a JSON string literal, so a test can build an
// envelope whose `body` is the inner frame text.
func jsonString(value string) string {
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return string(encoded)
}

// TestStreamChatChunksClassifiesBusinessErrorsOnTheEncryptedPath drives the
// executor-level entry point the user's failure actually travelled: a buffered
// encrypted SSE body whose error frame must arrive at the host classified, not as
// a bare 502.
func TestStreamChatChunksClassifiesBusinessErrorsOnTheEncryptedPath(t *testing.T) {
	const inner = `{"code":403,"message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"retryAfterSeconds\\\":30}\"}","type":"model_error"}`
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
		// wantDelay is the single delay the frame must produce. It is a POINTER
		// so nil can assert "no wait at all", which is the quota case: a
		// deterministic failure must never spend time.
		wantDelay *time.Duration
	}{
		{
			name:       "quota frame",
			body:       "data: " + `{"body":"{\"code\":110,\"message\":\"Billing daily count exceeded\",\"type\":\"model_error\"}","statusCodeValue":200}` + "\n\n",
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "QUOTA_EXCEEDED",
		},
		{
			name:       "nested queue frame riding inside a 200 SSE response",
			body:       "data: " + `{"body":` + jsonString(inner) + `,"statusCodeValue":200}` + "\n\n",
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "QUEUE",
			wantDelay:  durationPtr(10 * time.Second),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			waited := noQueueSleep(t)
			_, errChunks := streamChatChunks([]byte(testCase.body), true)
			typed := envelopeError(t, errChunks)
			if typed.HTTPStatus != testCase.wantStatus || typed.Code != testCase.wantCode {
				t.Fatalf("got %d/%s, want %d/%s", typed.HTTPStatus, typed.Code,
					testCase.wantStatus, testCase.wantCode)
			}
			switch {
			case testCase.wantDelay == nil && len(*waited) != 0:
				t.Fatalf("waits = %v, want none", *waited)
			case testCase.wantDelay != nil && (len(*waited) != 1 || (*waited)[0] != *testCase.wantDelay):
				t.Fatalf("waits = %v, want [%v]", *waited, *testCase.wantDelay)
			}
		})
	}

	// A quota frame must not wait, even though it travels the same path.
	t.Run("quota does not wait", func(t *testing.T) {
		waited := noQueueSleep(t)
		body := "data: " + `{"body":"{\"code\":110,\"message\":\"Billing daily count exceeded\"}","statusCodeValue":200}` + "\n\n"
		if _, errChunks := streamChatChunks([]byte(body), true); errChunks == nil {
			t.Fatal("a quota frame must fail the stream")
		}
		if len(*waited) != 0 {
			t.Fatalf("waits = %v, want none (the quota is deterministic)", *waited)
		}
	})

	// The public path stays a plain relay: no classification, no waiting.
	t.Run("the public path does not classify", func(t *testing.T) {
		waited := noQueueSleep(t)
		body := "data: " + `{"body":"{\"code\":110,\"message\":\"Billing daily count exceeded\"}"}` + "\n\n"
		chunks, errChunks := streamChatChunks([]byte(body), false)
		if errChunks != nil {
			t.Fatalf("the public path must relay the frame: %v", errChunks)
		}
		if len(chunks) != 1 {
			t.Fatalf("chunks = %d, want the relayed frame", len(chunks))
		}
		if len(*waited) != 0 {
			t.Fatalf("waits = %v, want none on the public path", *waited)
		}
	})
}

// durationPtr is a helper for the table's "must wait exactly this long" column.
func durationPtr(value time.Duration) *time.Duration { return &value }

// TestUpstreamErrorReadsTheBodyBeforeThe403Status covers the SECOND channel the
// same conditions arrive on (`qoder-adapter.ts:786-788`, `:795-820`): a 403 whose
// body names the queue or the quota is not an auth failure, and classifying it as
// one rotates a credential that was never the problem.
func TestUpstreamErrorReadsTheBodyBeforeThe403Status(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantCode   string
		retryable  bool
		wantWait   *time.Duration
	}{
		{
			name:       "a 403 carrying the queue code is a queue, not auth",
			status:     http.StatusForbidden,
			body:       `{"code":403,"message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"retryAfterSeconds\\\":30}\"}"}`,
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "QUEUE",
			retryable:  true,
			wantWait:   durationPtr(10 * time.Second),
		},
		{
			name:       "a 403 carrying the quota code is a quota, not auth",
			status:     http.StatusForbidden,
			body:       `{"code":110,"message":"Billing daily count exceeded"}`,
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "QUOTA_EXCEEDED",
			retryable:  false,
		},
		{
			// The regression guard: a genuine auth failure must STILL rotate.
			name:       "an HTML 403 stays an auth failure",
			status:     http.StatusForbidden,
			body:       `<html><body>Forbidden</body></html>`,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "auth",
			retryable:  false,
		},
		{
			name:       "a JSON 401 without a business code stays an auth failure",
			status:     http.StatusUnauthorized,
			body:       `{"code":105,"message":"authentication_failed"}`,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "auth",
			retryable:  false,
		},
		{
			name:       "an unrelated 403 JSON body stays an auth failure",
			status:     http.StatusForbidden,
			body:       `{"code":101,"message":"Signature invalid"}`,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "auth",
			retryable:  false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			waited := noQueueSleep(t)
			typed := envelopeError(t, upstreamError(httpResponse(testCase.status, testCase.body)))
			if typed.HTTPStatus != testCase.wantStatus || typed.Code != testCase.wantCode {
				t.Fatalf("got %d/%s, want %d/%s", typed.HTTPStatus, typed.Code,
					testCase.wantStatus, testCase.wantCode)
			}
			if typed.Retryable != testCase.retryable {
				t.Errorf("retryable = %v, want %v", typed.Retryable, testCase.retryable)
			}
			switch {
			case testCase.wantWait == nil && len(*waited) != 0:
				t.Errorf("waits = %v, want none", *waited)
			case testCase.wantWait != nil && (len(*waited) != 1 || (*waited)[0] != *testCase.wantWait):
				t.Errorf("waits = %v, want [%v]", *waited, *testCase.wantWait)
			}
		})
	}
}

// TestUpstreamErrorKeepsTheExistingTaxonomy is the "do not break what already
// works" guard for the statuses that were correct before this change.
func TestUpstreamErrorKeepsTheExistingTaxonomy(t *testing.T) {
	cases := []struct {
		status     int
		wantStatus int
		wantCode   string
		retryable  bool
	}{
		{http.StatusPaymentRequired, http.StatusPaymentRequired, "quota_exhausted", false},
		{http.StatusTooManyRequests, http.StatusTooManyRequests, "rate_limited", true},
		{http.StatusGatewayTimeout, http.StatusGatewayTimeout, "upstream_timeout", true},
		{http.StatusBadGateway, http.StatusBadGateway, "upstream_error", true},
		{http.StatusNotFound, http.StatusBadGateway, "upstream_error", true},
		{http.StatusInternalServerError, http.StatusBadGateway, "upstream_error", true},
	}
	for _, testCase := range cases {
		t.Run(itoaInt(testCase.status), func(t *testing.T) {
			typed := envelopeError(t, upstreamError(httpResponse(testCase.status, "detail")))
			if typed.HTTPStatus != testCase.wantStatus || typed.Code != testCase.wantCode {
				t.Fatalf("got %d/%s, want %d/%s", typed.HTTPStatus, typed.Code,
					testCase.wantStatus, testCase.wantCode)
			}
			if typed.Retryable != testCase.retryable {
				t.Errorf("retryable = %v, want %v", typed.Retryable, testCase.retryable)
			}
		})
	}
}
