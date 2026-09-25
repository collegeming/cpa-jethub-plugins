package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// executorRequestFixture builds an executor call with a translated payload.
func executorRequestFixture(payload, model string) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{Model: model, Payload: []byte(payload)}
}

// TestNormalizeFrameRaisesBusinessErrors is the guard for the reported "no reply
// and no error" failure: an envelope whose inner body is a business error must
// fail the stream instead of being skipped (`qoder-envelope.ts:18-22`).
func TestNormalizeFrameRaisesBusinessErrors(t *testing.T) {
	frame := `{"body":"[FAIL]node:oa_qwen-plus-2025-04-28 msg:Execution failed","statusCodeValue":500,"statusCode":"ERROR"}`
	_, errFrame := normalizeFrame(frame, true)
	if errFrame == nil {
		t.Fatal("a business error frame was accepted")
	}
	if !strings.Contains(errFrame.Error(), "Execution failed") {
		t.Fatalf("error = %v, want the upstream detail", errFrame)
	}
}

// TestNormalizeFrameUnwrapsOnTheEncryptedPathOnly keeps the public path a plain
// relay.
func TestNormalizeFrameUnwrapsOnTheEncryptedPathOnly(t *testing.T) {
	frame := `{"body":"{\"choices\":[{\"index\":0}]}"}`
	inner, errFrame := normalizeFrame(frame, true)
	if errFrame != nil {
		t.Fatalf("normalizeFrame: %v", errFrame)
	}
	if inner != `{"choices":[{"index":0}]}` {
		t.Fatalf("inner = %q", inner)
	}
	passthrough, errFrame := normalizeFrame(frame, false)
	if errFrame != nil {
		t.Fatalf("normalizeFrame(public): %v", errFrame)
	}
	if passthrough != frame {
		t.Fatalf("the public path must relay the frame unchanged, got %q", passthrough)
	}
}

// TestStreamChatChunksReEncodesFrames checks the SSE framing the host receives:
// one `data:` frame per upstream frame plus a terminating `[DONE]`.
func TestStreamChatChunksReEncodesFrames(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"he"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	chunks, errChunks := streamChatChunks([]byte(body), false)
	if errChunks != nil {
		t.Fatalf("streamChatChunks: %v", errChunks)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunk count = %d, want 3", len(chunks))
	}
	if !strings.Contains(string(chunks[0].Payload), `"he"`) {
		t.Fatalf("first chunk = %q", chunks[0].Payload)
	}
	if strings.TrimSpace(string(chunks[2].Payload)) != "data: [DONE]" {
		t.Fatalf("last chunk = %q, want the [DONE] frame", chunks[2].Payload)
	}
}

// TestStreamChatChunksAppendsDoneWhenUpstreamOmitsIt keeps every stream
// terminated, so a client never waits on a missing sentinel.
func TestStreamChatChunksAppendsDoneWhenUpstreamOmitsIt(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"
	chunks, errChunks := streamChatChunks([]byte(body), false)
	if errChunks != nil {
		t.Fatalf("streamChatChunks: %v", errChunks)
	}
	if len(chunks) != 2 || !strings.Contains(string(chunks[1].Payload), "[DONE]") {
		t.Fatalf("chunks = %d, want the frame plus a synthesised [DONE]", len(chunks))
	}
}

// TestStreamChatChunksRejectsAnEmptyBody avoids a silent empty answer.
func TestStreamChatChunksRejectsAnEmptyBody(t *testing.T) {
	if _, errChunks := streamChatChunks(nil, false); errChunks == nil {
		t.Fatal("an empty upstream body must fail")
	}
	if _, errChunks := streamChatChunks([]byte("event: ping\n\n"), false); errChunks == nil {
		t.Fatal("a body without data frames must fail")
	}
}

// TestAggregateChunksFoldsAStream covers the non-streaming route: the client did
// not ask for SSE, so the frames are folded into one completion.
func TestAggregateChunksFoldsAStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"c1","model":"qfmodel","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"llo","reasoning_content":"think"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	chunks, errChunks := collectChatChunks([]byte(body), false)
	if errChunks != nil {
		t.Fatalf("collectChatChunks: %v", errChunks)
	}
	completion := aggregateChunks(chunks, "qfmodel")
	if completion.ID != "c1" || completion.Object != "chat.completion" {
		t.Fatalf("completion header = %#v", completion)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(completion.Choices))
	}
	message := completion.Choices[0].Message
	if message.Content != "hello" || message.ReasoningContent != "think" || message.Role != "assistant" {
		t.Fatalf("message = %#v", message)
	}
	if completion.Choices[0].FinishReason == nil || *completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish reason = %v, want stop", completion.Choices[0].FinishReason)
	}
	if completion.Usage == nil || completion.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %#v, want the upstream totals", completion.Usage)
	}
}

// TestAggregateChunksMergesToolCallFragments implements the rule that a later
// empty function name must not erase the parsed one and that arguments
// concatenate (`openai-compat.ts:20-30`).
func TestAggregateChunksMergesToolCallFragments(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"a\":"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"1}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	chunks, errChunks := collectChatChunks([]byte(body), false)
	if errChunks != nil {
		t.Fatalf("collectChatChunks: %v", errChunks)
	}
	completion := aggregateChunks(chunks, "qfmodel")
	if len(completion.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1 merged call", len(completion.Choices[0].Message.ToolCalls))
	}
	call := completion.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "lookup" {
		t.Fatalf("function name = %q, want the non-empty name to win", call.Function.Name)
	}
	if call.Function.Arguments != `{"a":1}` {
		t.Fatalf("arguments = %q, want the fragments concatenated", call.Function.Arguments)
	}
	if call.ID != "call_1" || call.Type != "function" {
		t.Fatalf("call identity = %#v", call)
	}
}

// TestPublicRequestBodyForwardsTheModelVerbatim is the load-bearing rule that no
// model name is invented: the public endpoint's generic names appear nowhere in
// the sources, so whatever the client asked for is sent as-is.
func TestPublicRequestBodyForwardsTheModelVerbatim(t *testing.T) {
	request := executorRequestFixture(`{"model":"qwen-flash","messages":[{"role":"user","content":"hi"}]}`, "qwen-flash")
	body, errBody := publicRequestBody(request, DefaultConfig())
	if errBody != nil {
		t.Fatalf("publicRequestBody: %v", errBody)
	}
	if !strings.Contains(string(body), `"model":"qwen-flash"`) {
		t.Fatalf("body = %s, want the model name untouched", body)
	}
	if !strings.Contains(string(body), `"stream":true`) {
		t.Fatalf("body = %s, want stream forced to true", body)
	}
}

// TestPublicRequestBodyAppliesTheDefaultMaxTokensOnlyWhenAbsent.
func TestPublicRequestBodyAppliesTheDefaultMaxTokensOnlyWhenAbsent(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultMaxTokens = 4096
	request := executorRequestFixture(`{"model":"m","messages":[]}`, "m")
	body, _ := publicRequestBody(request, cfg)
	if !strings.Contains(string(body), `"max_tokens":4096`) {
		t.Fatalf("body = %s, want the configured default", body)
	}

	explicit := executorRequestFixture(`{"model":"m","messages":[],"max_tokens":32}`, "m")
	body, _ = publicRequestBody(explicit, cfg)
	if !strings.Contains(string(body), `"max_tokens":32`) {
		t.Fatalf("body = %s, want the client value preserved", body)
	}
}

// TestUpstreamErrorClassification keeps rate limits, auth failures and server
// faults distinguishable, which is what the host's retry and rotation rely on.
func TestUpstreamErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		wantCode  string
		retryable bool
	}{
		{http.StatusTooManyRequests, "rate_limited", true},
		{http.StatusUnauthorized, "auth", false},
		{http.StatusForbidden, "auth", false},
		{http.StatusBadGateway, "upstream_error", true},
		{http.StatusNotFound, "upstream_error", true},
	}
	for _, testCase := range cases {
		errUpstream := upstreamError(httpResponse(testCase.status, "nope"))
		typed, ok := errUpstream.(*abiboot.EnvelopeError)
		if !ok {
			t.Fatalf("status %d produced %T, want *abiboot.EnvelopeError", testCase.status, errUpstream)
		}
		if typed.Code != testCase.wantCode {
			t.Errorf("status %d code = %q, want %q", testCase.status, typed.Code, testCase.wantCode)
		}
		if typed.Retryable != testCase.retryable {
			t.Errorf("status %d retryable = %v, want %v", testCase.status, typed.Retryable, testCase.retryable)
		}
		if !strings.Contains(typed.Message, "nope") {
			t.Errorf("status %d message = %q, want the upstream detail", testCase.status, typed.Message)
		}
	}
}
