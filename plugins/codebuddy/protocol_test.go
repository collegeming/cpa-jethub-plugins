package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file pins the three upstream protocol fixes the CodeBuddy port was
// missing, each with the negative cases that keep the fix from over-reaching:
//
//   - 腾讯安全策略拦截（code 11140）：HTTP 403 与「HTTP 200 + 流内错误帧」两种形态
//     都必须报 PERMISSION_DENIED / 403，**绝不能**退化成 AUTH（UI 会把 AUTH 的
//     message 整个换成「API 密钥无效」，把操作者引向重新登录）。
//   - 流内限流（code 6004）：必须报 RATE_LIMIT / 429，而不是把上游信号吞掉后
//     再回一个 502 SERVER。
//   - 空名 tool_call：既不能留在配对结果里，也不能写到线上（上游 400 code 11133）。

// contentRejectionBody is the measured Tencent safety-policy payload
// (buddy-adapter.ts:395-408).
const contentRejectionBody = `{"code":11140,"msg":"request illegal",` +
	`"requestId":"5b2240b4-efdf-40e0-94e4-cfee1aa80585",` +
	`"displayMsg":{"en":"The content did not pass the safety review.","zh":"内容未通过安全审核，请调整后重试。"}}`

// TestIsContentRejection locks the predicate both channels share. An empty body
// must be false — a bare 401/403 is an auth failure, not a policy block.
func TestIsContentRejection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"measured 11140 payload", contentRejectionBody, true},
		{"whitespace inside the code match", `{"code" :  11140 ,"msg":"x"}`, true},
		{"business code as a string", `{"code":"11140"}`, true},
		{"msg wording only", `{"msg":"request illegal"}`, true},
		{"Request Illegal is case insensitive", `{"msg":"Request Illegal"}`, true},
		{"chinese safety review wording", `{"displayMsg":{"zh":"内容未通过安全审核"}}`, true},
		{"english safety review wording", `{"message":"The content did not pass the safety review"}`, true},
		{"empty body", "", false},
		{"plain invalid token", `{"message":"invalid token"}`, false},
		{"another business code", `{"code":11133,"msg":"the request parameters were rejected"}`, false},
		{"a number that merely contains 11140", `{"requestId":"11140","msg":"ok"}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isContentRejection(test.body); got != test.want {
				t.Errorf("isContentRejection(%q) = %v, want %v", test.body, got, test.want)
			}
		})
	}
}

// TestHTTPErrorClassificationContentRejection pins the HTTP channel: 403/11140
// is PERMISSION_DENIED (switch account), a plain 403 stays AUTH (re-login), and
// every other mapping is untouched.
func TestHTTPErrorClassificationContentRejection(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"403 with 11140", http.StatusForbidden, contentRejectionBody, "PERMISSION_DENIED"},
		{"401 with 11140", http.StatusUnauthorized, `{"code":11140,"msg":"request illegal"}`, "PERMISSION_DENIED"},
		{"403 quoting only the wording", http.StatusForbidden, `{"displayMsg":{"zh":"内容未通过安全审核"}}`, "PERMISSION_DENIED"},
		{"plain 403 stays AUTH", http.StatusForbidden, `{"message":"invalid token"}`, "AUTH"},
		{"plain 401 stays AUTH", http.StatusUnauthorized, `{"message":"invalid token"}`, "AUTH"},
		{"empty 403 stays AUTH", http.StatusForbidden, ``, "AUTH"},
		{"a 400 that mentions 11140 keeps its own mapping", http.StatusBadRequest, `{"code":11140}`, "INVALID_REQUEST"},
		{"empty 429 stays RATE_LIMIT", http.StatusTooManyRequests, ``, "RATE_LIMIT"},
		{"a 500 stays SERVER", http.StatusInternalServerError, `{"message":"boom"}`, "SERVER"},
		{"a 500 mentioning the wording stays SERVER", http.StatusInternalServerError, `{"message":"safety review failed"}`, "SERVER"},
		{"context overflow keeps its code", http.StatusBadRequest,
			`{"extError":{"code":"context_length_exceeded"}}`, "CONTEXT_WINDOW_EXCEEDED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := httpErrorCode(test.status, test.body); got != test.want {
				t.Errorf("httpErrorCode(%d, %q) = %q, want %q", test.status, test.body, got, test.want)
			}
		})
	}
}

// TestErrorDetailSurfacesDisplayMsg pins the 腾讯 field layout: `displayMsg.{zh,en}`
// is the only human-readable explanation and must lead the message instead of
// being dropped in favour of the bare `msg`.
func TestErrorDetailSurfacesDisplayMsg(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		contains []string
		absent   []string
	}{
		{
			name:     "chinese displayMsg leads",
			body:     contentRejectionBody,
			contains: []string{"内容未通过安全审核", "request illegal"},
			absent:   []string{"requestId"},
		},
		{
			name:     "english displayMsg is the fallback",
			body:     `{"code":11140,"msg":"request illegal","displayMsg":{"en":"The content did not pass the safety review."}}`,
			contains: []string{"did not pass the safety review", "request illegal"},
		},
		{
			name:     "body without displayMsg keeps its old shape",
			body:     `{"error":{"code":"a","type":"b","message":"c"},"message":"d"}`,
			contains: []string{"a", "b", "c", "d"},
		},
		{
			name:     "non-JSON is surfaced verbatim",
			body:     `<html>gateway</html>`,
			contains: []string{"html"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := errorDetail(test.body)
			for _, fragment := range test.contains {
				if !strings.Contains(got, fragment) {
					t.Errorf("errorDetail(%q) = %q, missing %q", test.body, got, fragment)
				}
			}
			for _, fragment := range test.absent {
				if strings.Contains(got, fragment) {
					t.Errorf("errorDetail(%q) = %q, must not contain %q", test.body, got, fragment)
				}
			}
		})
	}
	if got := errorDetail(""); got == "" {
		t.Error("an empty body must still produce a message")
	}
}

// TestClassifyFailureContentRejection is the HTTP channel end to end: the CPA
// envelope must carry PERMISSION_DENIED plus a real 403, and must keep the
// server's own Chinese explanation.
func TestClassifyFailureContentRejection(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantStatus int
		wantIn     string
	}{
		{
			name: "403 safety policy block", status: http.StatusForbidden, body: contentRejectionBody,
			wantCode: "PERMISSION_DENIED", wantStatus: http.StatusForbidden, wantIn: "内容未通过安全审核",
		},
		{
			name: "plain 403 stays AUTH", status: http.StatusForbidden, body: `{"message":"invalid token"}`,
			wantCode: "AUTH", wantStatus: http.StatusForbidden, wantIn: "invalid token",
		},
		{
			name: "429 stays RATE_LIMIT", status: http.StatusTooManyRequests, body: "",
			wantCode: "RATE_LIMIT", wantStatus: http.StatusTooManyRequests, wantIn: "empty error body",
		},
		{
			name: "500 stays SERVER", status: http.StatusInternalServerError, body: `{"message":"boom"}`,
			wantCode: "SERVER", wantStatus: http.StatusInternalServerError, wantIn: "boom",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errFailure := classifyFailure(&pluginapi.HTTPResponse{StatusCode: test.status, Body: []byte(test.body)}, "deepseek-v4.1-flash")
			envelope, ok := errFailure.(*abiboot.EnvelopeError)
			if !ok {
				t.Fatalf("classifyFailure returned %T, want an *abiboot.EnvelopeError", errFailure)
			}
			if envelope.Code != test.wantCode {
				t.Errorf("code = %q, want %q", envelope.Code, test.wantCode)
			}
			if envelope.HTTPStatus != test.wantStatus {
				t.Errorf("http status = %d, want %d", envelope.HTTPStatus, test.wantStatus)
			}
			if !strings.Contains(envelope.Message, test.wantIn) {
				t.Errorf("message = %q, missing %q", envelope.Message, test.wantIn)
			}
		})
	}
}

// TestChunkErrorPayloadClassification pins the in-stream channel at the frame
// level, including the gate that keeps a legitimate answer untouched.
func TestChunkErrorPayloadClassification(t *testing.T) {
	tests := []struct {
		name       string
		chunk      chatChunk
		wantDetail bool
		wantCode   string
		wantStatus int
	}{
		{
			name:       "in-stream 11140 frame with no choices",
			chunk:      chatChunk{Code: float64(11140), Msg: "request illegal", DisplayMsg: map[string]any{"zh": "内容未通过安全审核，请调整后重试。"}},
			wantDetail: true, wantCode: "PERMISSION_DENIED", wantStatus: http.StatusForbidden,
		},
		{
			name:       "in-stream 6004 frame with no choices",
			chunk:      chatChunk{Code: float64(6004), Msg: "您的使用量已超出频率限制"},
			wantDetail: true, wantCode: "RATE_LIMIT", wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "in-stream rate limit expressed in words",
			chunk:      chatChunk{Code: float64(6004), Msg: "usage exceeds frequency limit, too many requests"},
			wantDetail: true, wantCode: "RATE_LIMIT", wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "in-stream business error stays SERVER",
			chunk:      chatChunk{Code: float64(11102), Msg: "model service info not found"},
			wantDetail: true, wantCode: "SERVER", wantStatus: http.StatusBadGateway,
		},
		{
			name:       "an error object is still a failure",
			chunk:      chatChunk{Error: json.RawMessage(`{"message":"boom"}`)},
			wantDetail: true, wantCode: "SERVER", wantStatus: http.StatusBadGateway,
		},
		{
			name:       "an error object carrying 11140 is a policy block",
			chunk:      chatChunk{Error: json.RawMessage(`{"code":11140,"message":"request illegal"}`)},
			wantDetail: true, wantCode: "PERMISSION_DENIED", wantStatus: http.StatusForbidden,
		},
		{
			name:       "context overflow keeps its code",
			chunk:      chatChunk{Error: json.RawMessage(`{"message":"prompt is too long","extError":{"code":"context_length_exceeded"}}`)},
			wantDetail: true, wantCode: "CONTEXT_WINDOW_EXCEEDED", wantStatus: http.StatusBadRequest,
		},
		{
			// The gate that matters: a real answer that happens to discuss the
			// safety policy must be forwarded, not treated as a block.
			name: "the same words inside choices are not a block",
			chunk: chatChunk{
				Code: float64(11140), Msg: "request illegal",
				Choices: []streamChoice{{Index: 0, Delta: streamDelta{
					Content: "安全审核 与 request illegal，业务码 11140 都只是正文里的一串字。",
				}}},
			},
			wantDetail: false,
		},
		{
			name:       "a healthy chunk is not an error",
			chunk:      chatChunk{Choices: []streamChoice{{Index: 0}}},
			wantDetail: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chunk := test.chunk
			detail, kind := chunkErrorPayload(&chunk)
			if test.wantDetail != (detail != "") {
				t.Fatalf("detail = %q, want an error reported = %v", detail, test.wantDetail)
			}
			if !test.wantDetail {
				if kind != streamErrorNone {
					t.Errorf("kind = %v, want streamErrorNone", kind)
				}
				return
			}
			errFailure := streamErrorEnvelope(kind, detail)
			envelope, ok := errFailure.(*abiboot.EnvelopeError)
			if !ok {
				t.Fatalf("streamErrorEnvelope returned %T", errFailure)
			}
			if envelope.Code != test.wantCode || envelope.HTTPStatus != test.wantStatus {
				t.Errorf("envelope = (%q, %d), want (%q, %d)", envelope.Code, envelope.HTTPStatus, test.wantCode, test.wantStatus)
			}
		})
	}
}

// TestExecutorInStreamErrorsAreClassified drives both executor entry points over
// the failures Tencent can only report inside a 200 stream. Before the fix every
// one of them reached CPA as a retryable SERVER/502 — including the policy block,
// whose only correct handling is switching the account.
func TestExecutorInStreamErrorsAreClassified(t *testing.T) {
	tests := []struct {
		name       string
		frame      string
		wantCode   string
		wantStatus int
	}{
		{
			name:       "safety policy block arrives as an SSE frame",
			frame:      `{"code":11140,"msg":"request illegal","requestId":"5b2240b4","displayMsg":{"zh":"内容未通过安全审核，请调整后重试。"}}`,
			wantCode:   "PERMISSION_DENIED",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "rate limit arrives as an SSE frame",
			frame:      `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2099-12-31 23:59:59 UTC+8 重置"}`,
			wantCode:   "RATE_LIMIT",
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "an unrelated business failure stays SERVER",
			frame:      `{"code":11102,"msg":"model service info not found"}`,
			wantCode:   "SERVER",
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, streaming := range []bool{false, true} {
				channel := "execute"
				if streaming {
					channel = "execute_stream"
				}
				t.Run(channel, func(t *testing.T) {
					fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(time.Hour).UnixMilli())
					fixture.chatBody = "data: " + test.frame + "\n\ndata: [DONE]\n\n"
					raw := codebuddyChatRequest(t)

					var errFailure error
					if streaming {
						_, errFailure = handleExecutorExecuteStream(host, raw)
					} else {
						_, errFailure = handleExecutorExecute(host, raw)
					}
					if errFailure == nil {
						t.Fatal("the in-stream failure was swallowed")
					}
					envelope, ok := errFailure.(*abiboot.EnvelopeError)
					if !ok {
						t.Fatalf("error = %T (%v), want an *abiboot.EnvelopeError", errFailure, errFailure)
					}
					if envelope.Code != test.wantCode || envelope.HTTPStatus != test.wantStatus {
						t.Errorf("envelope = (%q, %d), want (%q, %d) — message %q",
							envelope.Code, envelope.HTTPStatus, test.wantCode, test.wantStatus, envelope.Message)
					}
				})
			}
		})
	}
}

// TestExecutorStreamForwardsProseQuotingThePolicyWords is the negative control
// for the in-stream gate: an answer that merely talks about 安全审核 / 11140 must
// reach the client byte for byte.
func TestExecutorStreamForwardsProseQuotingThePolicyWords(t *testing.T) {
	prose := `安全审核 request illegal 11140 frequency limit 限流`
	fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(time.Hour).UnixMilli())
	frame := `{"id":"c1","choices":[{"index":0,"delta":{"content":"` + prose + `"},"finish_reason":"stop"}]}`
	fixture.chatBody = "data: " + frame + "\n\ndata: [DONE]\n\n"

	value, errStream := handleExecutorExecuteStream(host, codebuddyChatRequest(t))
	if errStream != nil {
		t.Fatalf("a legitimate answer was classified as a failure: %v", errStream)
	}
	response := value.(executorStreamResponse)
	if len(response.Chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(response.Chunks))
	}
	if got := string(response.Chunks[0].Payload); !strings.Contains(got, prose) {
		t.Errorf("payload = %q, want the prose untouched", got)
	}
}

// codebuddyChatRequest is one minimal chat request against the freshness fixture.
func codebuddyChatRequest(t *testing.T) json.RawMessage {
	t.Helper()
	request := pluginapi.ExecutorRequest{
		AuthID:       codebuddyLiveName,
		AuthProvider: ProviderKey,
		Model:        "deepseek-v4.1-flash",
		StorageJSON:  codebuddyCredential("token", time.Now().Add(time.Hour).UnixMilli()),
		Payload:      json.RawMessage(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`),
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	return raw
}

// TestHasUsableToolName pins sse.ts:81-83. The `String(name)` trap is the whole
// point: "undefined"/"null" are non-empty strings but carry no tool name.
func TestHasUsableToolName(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want bool
	}{
		{"a real name", "read_file", true},
		{"empty string", "", false},
		{"blank string", "   ", false},
		{"missing field", nil, false},
		{"a number", 42, false},
		{"an object", map[string]any{}, false},
		{"the literal string null", "null", true}, // not a name we invent: only the raw value matters
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasUsableToolName(test.raw); got != test.want {
				t.Errorf("hasUsableToolName(%#v) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
	// `null` and a missing key must both fail: the port reads the raw JSON value,
	// so a JSON null arrives as nil.
	if hasUsableToolName(nil) {
		t.Error("a JSON null name is not usable")
	}
}

// TestResolveToolPairingDropsUnusableNames pins the self-healing half of
// a391dcc: a nameless call must never enter keepCallIDs — and must not drag a
// healthy sibling call down with it.
func TestResolveToolPairingDropsUnusableNames(t *testing.T) {
	tests := []struct {
		name        string
		calls       []any
		wantKept    []string
		wantDropped []string
	}{
		{
			name: "an empty name is unusable",
			calls: []any{
				map[string]any{"id": "empty", "function": map[string]any{"name": "", "arguments": "{}"}},
			},
			wantDropped: []string{"empty"},
		},
		{
			name: "a missing function object is unusable",
			calls: []any{
				map[string]any{"id": "no-function"},
			},
			wantDropped: []string{"no-function"},
		},
		{
			name: "a JSON null name is unusable",
			calls: []any{
				map[string]any{"id": "null-name", "function": map[string]any{"name": nil, "arguments": "{}"}},
			},
			wantDropped: []string{"null-name"},
		},
		{
			name: "a blank name is unusable",
			calls: []any{
				map[string]any{"id": "blank", "function": map[string]any{"name": "  ", "arguments": "{}"}},
			},
			wantDropped: []string{"blank"},
		},
		{
			name: "a nameless call does not poison a healthy sibling",
			calls: []any{
				map[string]any{"id": "nameless", "function": map[string]any{"name": "", "arguments": "{}"}},
				map[string]any{"id": "healthy", "function": map[string]any{"name": "pwsh", "arguments": "{}"}},
			},
			wantKept:    []string{"healthy"},
			wantDropped: []string{"nameless"},
		},
		{
			name: "a usable but unpaired call is still dropped",
			calls: []any{
				map[string]any{"id": "unpaired", "function": map[string]any{"name": "read", "arguments": "{}"}},
			},
			wantDropped: []string{"unpaired"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messages := []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "tool_calls": test.calls},
				map[string]any{"role": "tool", "tool_call_id": "empty", "content": "r"},
				map[string]any{"role": "tool", "tool_call_id": "no-function", "content": "r"},
				map[string]any{"role": "tool", "tool_call_id": "null-name", "content": "r"},
				map[string]any{"role": "tool", "tool_call_id": "blank", "content": "r"},
				map[string]any{"role": "tool", "tool_call_id": "nameless", "content": "r"},
				map[string]any{"role": "tool", "tool_call_id": "healthy", "content": "r"},
			}
			keepCalls, keepResults := resolveToolPairing(messages)
			for _, id := range test.wantKept {
				if !keepCalls[id] || !keepResults[id] {
					t.Errorf("call %q must be kept (calls=%v results=%v)", id, keepCalls, keepResults)
				}
			}
			for _, id := range test.wantDropped {
				if keepCalls[id] {
					t.Errorf("call %q must be dropped", id)
				}
				if keepResults[id] {
					t.Errorf("the orphan result of %q must be dropped", id)
				}
			}
		})
	}
}

// TestNormalizeMessagesDropsUnusableToolCalls pins the serialisation half: the
// wire body must never carry a nameless call, because the provider answers it
// with HTTP 400 code 11133 and the whole session stays broken.
func TestNormalizeMessagesDropsUnusableToolCalls(t *testing.T) {
	tests := []struct {
		name       string
		call       map[string]any
		wantOnWire bool
	}{
		{
			name:       "a healthy call is written",
			call:       map[string]any{"id": "c1", "function": map[string]any{"name": "read", "arguments": "{}"}},
			wantOnWire: true,
		},
		{
			name: "an empty name is not written",
			call: map[string]any{"id": "c1", "function": map[string]any{"name": "", "arguments": "{}"}},
		},
		{
			name: "a missing function object is not written",
			call: map[string]any{"id": "c1"},
		},
		{
			name: "a JSON null name is not written",
			call: map[string]any{"id": "c1", "function": map[string]any{"name": nil, "arguments": "{}"}},
		},
		{
			name: "a blank name is not written",
			call: map[string]any{"id": "c1", "function": map[string]any{"name": "   ", "arguments": "{}"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := []any{
				map[string]any{"role": "assistant", "tool_calls": []any{test.call}},
				map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
			}
			out := normalizeMessages(raw)
			body, errMarshal := json.Marshal(out)
			if errMarshal != nil {
				t.Fatalf("marshal: %v", errMarshal)
			}
			onWire := strings.Contains(string(body), `"tool_calls"`)
			if onWire != test.wantOnWire {
				t.Fatalf("tool_calls on the wire = %v, want %v (%s)", onWire, test.wantOnWire, body)
			}
			// The result of a dropped call must never be written either: a tool
			// message without its call is rejected the same way. When the call
			// itself survives, its result must of course survive with it.
			sawResult := false
			for _, message := range out {
				record, _ := message.(map[string]any)
				if record["role"] == "tool" {
					sawResult = true
				}
			}
			if sawResult != test.wantOnWire {
				t.Fatalf("tool result on the wire = %v, want %v (%s)", sawResult, test.wantOnWire, body)
			}
		})
	}
}

// TestStateRequestTimeoutMatchesUpstream pins buddy.ts:60 (10s). The 5s value
// was measurably too tight for www.workbuddy.ai (5860–7525 ms).
func TestStateRequestTimeoutMatchesUpstream(t *testing.T) {
	if StateRequestTimeoutMS != 10000 {
		t.Errorf("StateRequestTimeoutMS = %d, want 10000 (buddy.ts:60)", StateRequestTimeoutMS)
	}
	if StateRequestTimeoutMS <= 5000 {
		t.Errorf("StateRequestTimeoutMS = %d must not be the legacy 5s value", StateRequestTimeoutMS)
	}
}
