package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// sseBody joins payloads into one upstream SSE document.
func sseBody(payloads ...string) string {
	var builder strings.Builder
	for _, payload := range payloads {
		builder.WriteString("data: ")
		builder.WriteString(payload)
		builder.WriteString("\n\n")
	}
	return builder.String()
}

func prepareChatBodyFor(t *testing.T, payload string, model string, cfg Config, remote remoteModel) (map[string]any, string) {
	t.Helper()
	body, resolved, errPrepare := prepareChatBody([]byte(payload), model, cfg, remote)
	if errPrepare != nil {
		t.Fatalf("prepareChatBody: %v", errPrepare)
	}
	decoded := map[string]any{}
	if errUnmarshal := decodeJSON(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode prepared body: %v", errUnmarshal)
	}
	return decoded, resolved
}

// bodyNumber reads a numeric field from a decoded body.
func bodyNumber(t *testing.T, body map[string]any, key string) float64 {
	t.Helper()
	value, found := readNumberField(body, key)
	if !found {
		t.Fatalf("body has no numeric %s: %v", key, body)
	}
	return value
}

func TestPrepareChatBody(t *testing.T) {
	remote := remoteModel{
		ID:        "kimi-k3",
		Name:      "kimi-k3",
		MaxTokens: int64Ptr(262144),
		Thinking: &thinkingConfig{
			Options:      []thinkingOption{{Level: "max", OpenclawLevel: "xhigh"}},
			DefaultLevel: "max",
		},
	}

	t.Run("forces stream and keeps unknown fields", func(t *testing.T) {
		body, model := prepareChatBodyFor(t,
			`{"model":"kimi-k3","stream":false,"messages":[{"role":"user","content":"hi"}],"presence_penalty":0.5,"seed":7}`,
			"", DefaultConfig(), remote)
		if model != "kimi-k3" {
			t.Fatalf("model = %q", model)
		}
		if body["stream"] != true {
			t.Fatalf("stream = %v, want true (upstream rejects stream:false)", body["stream"])
		}
		if bodyNumber(t, body, "presence_penalty") != 0.5 || bodyNumber(t, body, "seed") != 7 {
			t.Fatalf("unknown fields were dropped: %v", body)
		}
	})

	t.Run("host-selected model wins", func(t *testing.T) {
		body, model := prepareChatBodyFor(t,
			`{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`,
			"selected-model", DefaultConfig(), remoteModel{})
		if model != "selected-model" || body["model"] != "selected-model" {
			t.Fatalf("model = %q / %v", model, body["model"])
		}
	})

	t.Run("remote maxTokens fills an absent max_tokens", func(t *testing.T) {
		body, _ := prepareChatBodyFor(t, `{"model":"m","messages":[{"role":"user"}]}`, "", DefaultConfig(), remote)
		if bodyNumber(t, body, "max_tokens") != 262144 {
			t.Fatalf("max_tokens = %v, want the remote value", body["max_tokens"])
		}
	})

	t.Run("caller max_tokens wins over remote", func(t *testing.T) {
		body, _ := prepareChatBodyFor(t, `{"model":"m","max_tokens":64,"messages":[{"role":"user"}]}`, "", DefaultConfig(), remote)
		if bodyNumber(t, body, "max_tokens") != 64 {
			t.Fatalf("max_tokens = %v", body["max_tokens"])
		}
	})

	t.Run("max_completion_tokens suppresses max_tokens", func(t *testing.T) {
		body, _ := prepareChatBodyFor(t, `{"model":"m","max_completion_tokens":32,"messages":[{"role":"user"}]}`, "", DefaultConfig(), remote)
		if _, present := body["max_tokens"]; present {
			t.Fatalf("max_tokens must not be added when max_completion_tokens is set: %v", body)
		}
	})

	t.Run("static default is the last resort", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DefaultMaxTokens = 4096
		body, _ := prepareChatBodyFor(t, `{"model":"m","messages":[{"role":"user"}]}`, "", cfg, remoteModel{})
		if bodyNumber(t, body, "max_tokens") != 4096 {
			t.Fatalf("max_tokens = %v", body["max_tokens"])
		}
	})

	t.Run("no limit anywhere leaves the field absent", func(t *testing.T) {
		body, _ := prepareChatBodyFor(t, `{"model":"m","max_tokens":0,"messages":[{"role":"user"}]}`, "", DefaultConfig(), remoteModel{})
		if _, present := body["max_tokens"]; present {
			t.Fatalf("max_tokens must stay absent rather than invented: %v", body)
		}
	})

	t.Run("reasoning_effort is mapped to the wire level", func(t *testing.T) {
		body, _ := prepareChatBodyFor(t,
			`{"model":"m","reasoning_effort":"max","messages":[{"role":"user"}]}`, "", DefaultConfig(), remote)
		if body["reasoning_effort"] != "xhigh" {
			t.Fatalf("reasoning_effort = %v, want xhigh", body["reasoning_effort"])
		}
	})

	t.Run("no reasoning_effort is invented", func(t *testing.T) {
		body, _ := prepareChatBodyFor(t, `{"model":"m","messages":[{"role":"user"}]}`, "", DefaultConfig(), remote)
		if _, present := body["reasoning_effort"]; present {
			t.Fatalf("reasoning_effort must not be added: %v", body)
		}
	})

	t.Run("blank reasoning_effort is dropped", func(t *testing.T) {
		body, _ := prepareChatBodyFor(t, `{"model":"m","reasoning_effort":"  ","messages":[{"role":"user"}]}`, "", DefaultConfig(), remote)
		if _, present := body["reasoning_effort"]; present {
			t.Fatalf("blank reasoning_effort must be dropped: %v", body)
		}
	})

	t.Run("tool_choice normalisation", func(t *testing.T) {
		for _, payload := range []string{
			`{"model":"m","tool_choice":"","messages":[{"role":"user"}]}`,
			`{"model":"m","tool_choice":"none","messages":[{"role":"user"}]}`,
			`{"model":"m","tool_choice":null,"messages":[{"role":"user"}]}`,
		} {
			body, _ := prepareChatBodyFor(t, payload, "", DefaultConfig(), remoteModel{})
			if _, present := body["tool_choice"]; present {
				t.Fatalf("tool_choice must be removed for %s: %v", payload, body)
			}
		}
		body, _ := prepareChatBodyFor(t, `{"model":"m","tool_choice":"auto","messages":[{"role":"user"}]}`, "", DefaultConfig(), remoteModel{})
		if body["tool_choice"] != "auto" {
			t.Fatalf("a real tool_choice must survive: %v", body)
		}
	})

	t.Run("prompt_cache_key is never added", func(t *testing.T) {
		body, _ := prepareChatBodyFor(t, `{"model":"m","messages":[{"role":"user"}]}`, "", DefaultConfig(), remote)
		if _, present := body["prompt_cache_key"]; present {
			t.Fatalf("prompt_cache_key is a Tencent mechanism and must not be added: %v", body)
		}
	})

	t.Run("invalid payloads are rejected with 400", func(t *testing.T) {
		cases := []struct {
			name    string
			payload string
		}{
			{name: "not json", payload: `{`},
			{name: "no model", payload: `{"messages":[{"role":"user"}]}`},
			{name: "no messages", payload: `{"model":"m"}`},
			{name: "empty messages", payload: `{"model":"m","messages":[]}`},
		}
		for _, test := range cases {
			if _, _, errPrepare := prepareChatBody([]byte(test.payload), "", DefaultConfig(), remoteModel{}); errPrepare == nil {
				t.Fatalf("%s: expected a rejection", test.name)
			} else if envelope, ok := errPrepare.(interface{ Error() string }); !ok || envelope == nil {
				t.Fatalf("%s: unexpected error type", test.name)
			}
		}
	})
}

// TestConsumeUpstreamStreamNullableDeltas is the regression guard for the
// documented upstream behaviour: one channel carries text while the other is an
// explicit JSON null. Go's plain string decoding would collapse null and absent
// into "", so the decoder uses raw messages and a typeof-string test.
func TestConsumeUpstreamStreamNullableDeltas(t *testing.T) {
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":"think-1"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hello","reasoning_content":null},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":null,"reasoning_content":"think-2"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
		`[DONE]`,
	}
	outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
	if errStream != nil {
		t.Fatalf("consumeUpstreamStream: %v", errStream)
	}
	if outcome.Content.String() != "hello" {
		t.Fatalf("Content = %q, want hello (explicit null must not become text)", outcome.Content.String())
	}
	if outcome.ReasoningContent.String() != "think-1think-2" {
		t.Fatalf("ReasoningContent = %q", outcome.ReasoningContent.String())
	}
	if outcome.FinishReason != "stop" || outcome.finishReasonFinal != "stop" {
		t.Fatalf("finish reason = %q/%q", outcome.FinishReason, outcome.finishReasonFinal)
	}
	if !outcome.SawDone {
		t.Fatal("SawDone = false")
	}
	if len(outcome.Payloads) != len(frames) {
		t.Fatalf("payloads = %d, want %d", len(outcome.Payloads), len(frames))
	}
	if outcome.Payloads[len(outcome.Payloads)-1] != "[DONE]" {
		t.Fatalf("[DONE] must be last, got %q", outcome.Payloads[len(outcome.Payloads)-1])
	}
	// Frames that need no rewrite are relayed byte-for-byte.
	for index := 0; index < 4; index++ {
		if outcome.Payloads[index] != frames[index] {
			t.Fatalf("frame %d was rewritten:\n got  %s\n want %s", index, outcome.Payloads[index], frames[index])
		}
	}
	if outcome.Usage == nil || outcome.Usage.PromptTokens != 10 || outcome.Usage.CompletionTokens != 4 {
		t.Fatalf("Usage = %+v", outcome.Usage)
	}
}

// TestConsumeUpstreamStreamMessageFallback pins the two-shape compatibility
// rule: a chunk carrying a whole `message.content` is accepted only while no
// delta content has been seen, otherwise the two shapes would double up.
func TestConsumeUpstreamStreamMessageFallback(t *testing.T) {
	t.Run("fallback applies before any delta content", func(t *testing.T) {
		frames := []string{
			`{"id":"c1","choices":[{"index":0,"delta":{"content":null},"message":{"content":"from-message"},"finish_reason":"stop"}]}`,
		}
		outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
		if errStream != nil {
			t.Fatalf("consumeUpstreamStream: %v", errStream)
		}
		if outcome.Content.String() != "from-message" {
			t.Fatalf("Content = %q", outcome.Content.String())
		}
		// The outbound frame must expose it as a delta so OpenAI clients see it.
		if !strings.Contains(outcome.Payloads[0], `"content":"from-message"`) {
			t.Fatalf("frame was not rewritten: %s", outcome.Payloads[0])
		}
	})

	t.Run("fallback is ignored after delta content", func(t *testing.T) {
		frames := []string{
			`{"id":"c1","choices":[{"index":0,"delta":{"content":"real"},"finish_reason":null}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"content":null},"message":{"content":"injected"},"finish_reason":"stop"}]}`,
		}
		outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
		if errStream != nil {
			t.Fatalf("consumeUpstreamStream: %v", errStream)
		}
		if outcome.Content.String() != "real" {
			t.Fatalf("Content = %q, want only the delta text", outcome.Content.String())
		}
		// The frame is relayed untouched and the delta stays null: the fallback
		// must not be promoted into delta.content.
		frame := map[string]any{}
		if errUnmarshal := decodeJSON([]byte(outcome.Payloads[1]), &frame); errUnmarshal != nil {
			t.Fatalf("decode frame: %v", errUnmarshal)
		}
		choices, _ := frame["choices"].([]any)
		delta := asRecord(asRecord(choices[0])["delta"])
		if value, present := delta["content"]; present && value != nil {
			t.Fatalf("delta.content was promoted after real content: %v", value)
		}
	})
}

func TestConsumeUpstreamStreamToolCalls(t *testing.T) {
	t.Run("assembled and empty arguments become an object", func(t *testing.T) {
		frames := []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":"tool_calls"}]}`,
		}
		outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
		if errStream != nil {
			t.Fatalf("consumeUpstreamStream: %v", errStream)
		}
		if len(outcome.ToolCalls) != 1 {
			t.Fatalf("tool calls = %+v", outcome.ToolCalls)
		}
		call := outcome.ToolCalls[0]
		if call.ID != "call_1" || call.Name != "read_file" {
			t.Fatalf("call = %+v", call)
		}
		if call.Arguments != `{"path":"a.txt"}` {
			t.Fatalf("arguments = %q", call.Arguments)
		}
		if call.Truncated {
			t.Fatal("valid arguments must not be reported as truncated")
		}
		if outcome.finishReasonFinal != "tool_calls" {
			t.Fatalf("finish reason = %q", outcome.finishReasonFinal)
		}
	})

	t.Run("a later empty name never erases the parsed name", func(t *testing.T) {
		frames := []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"list_dir","arguments":""}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":""}}]},"finish_reason":"tool_calls"}]}`,
		}
		outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
		if errStream != nil {
			t.Fatalf("consumeUpstreamStream: %v", errStream)
		}
		if outcome.ToolCalls[0].Name != "list_dir" {
			t.Fatalf("name = %q, want list_dir", outcome.ToolCalls[0].Name)
		}
		// A no-argument tool legitimately streams one empty fragment: it is
		// normalised to {} on the wire.
		if !strings.Contains(outcome.Payloads[1], `"arguments":"{}"`) {
			t.Fatalf("empty arguments were not normalised: %s", outcome.Payloads[1])
		}
		if outcome.ToolCalls[0].Truncated {
			t.Fatal("an empty argument list is not truncation")
		}
	})

	t.Run("truncated arguments report length and are left untouched", func(t *testing.T) {
		frames := []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"path\":\"a"}}]},"finish_reason":null}]}`,
		}
		outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
		if errStream != nil {
			t.Fatalf("consumeUpstreamStream: %v", errStream)
		}
		if !outcome.ToolCalls[0].Truncated {
			t.Fatal("unparseable arguments must be reported as truncated")
		}
		if outcome.finishReasonFinal != "length" {
			t.Fatalf("finish reason = %q, want length", outcome.finishReasonFinal)
		}
		if !strings.Contains(outcome.Payloads[0], `"finish_reason":"length"`) {
			t.Fatalf("the frame was not rewritten: %s", outcome.Payloads[0])
		}
		// The incomplete fragment must NOT be replaced by {}.
		if strings.Contains(outcome.Payloads[0], `"arguments":"{}"`) {
			t.Fatalf("truncated arguments must not be faked into {}: %s", outcome.Payloads[0])
		}
	})

	t.Run("a stream that ends mid tool call reports length", func(t *testing.T) {
		frames := []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
		}
		outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
		if errStream != nil {
			t.Fatalf("consumeUpstreamStream: %v", errStream)
		}
		if outcome.finishReasonFinal != "length" {
			t.Fatalf("finish reason = %q, want length for a cut stream", outcome.finishReasonFinal)
		}
	})

	t.Run("length is preserved", func(t *testing.T) {
		frames := []string{`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"length"}]}`}
		outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
		if errStream != nil {
			t.Fatalf("consumeUpstreamStream: %v", errStream)
		}
		if outcome.finishReasonFinal != "length" {
			t.Fatalf("finish reason = %q", outcome.finishReasonFinal)
		}
	})

	t.Run("a text-only stream without finish_reason becomes stop", func(t *testing.T) {
		frames := []string{`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`}
		outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
		if errStream != nil {
			t.Fatalf("consumeUpstreamStream: %v", errStream)
		}
		if outcome.finishReasonFinal != "stop" {
			t.Fatalf("finish reason = %q, want stop", outcome.finishReasonFinal)
		}
		if !strings.Contains(outcome.Payloads[0], `"finish_reason":"stop"`) {
			t.Fatalf("frame not rewritten: %s", outcome.Payloads[0])
		}
	})
}

func TestConsumeUpstreamStreamErrors(t *testing.T) {
	outcome, errStream := consumeUpstreamStream([]byte(sseBody(`{"error":{"message":"boom"}}`)))
	if errStream == nil {
		t.Fatal("an in-stream error must fail the request")
	}
	if outcome != nil {
		t.Fatalf("outcome = %+v, want nil", outcome)
	}

	// Unparseable frames are skipped, not fatal.
	outcome, errStream = consumeUpstreamStream([]byte("data: {not json}\n\ndata: [DONE]\n\n"))
	if errStream != nil {
		t.Fatalf("unparseable frame: %v", errStream)
	}
	if !outcome.SawDone {
		t.Fatal("SawDone = false")
	}
}

func TestIsTruncatedAndNormalizeToolArguments(t *testing.T) {
	tests := []struct {
		raw        string
		truncated  bool
		normalized string
	}{
		{raw: "", truncated: false, normalized: "{}"},
		{raw: "   ", truncated: false, normalized: "{}"},
		{raw: `{"a":1}`, truncated: false, normalized: `{"a":1}`},
		{raw: ` {"a":1} `, truncated: false, normalized: `{"a":1}`},
		{raw: `{"a":`, truncated: true, normalized: "{}"},
		{raw: `null`, truncated: false, normalized: "{}"},
		{raw: `[1,2]`, truncated: false, normalized: "{}"},
		{raw: `"scalar"`, truncated: false, normalized: "{}"},
		{raw: `42`, truncated: false, normalized: "{}"},
	}
	for _, test := range tests {
		if got := isTruncatedArguments(test.raw); got != test.truncated {
			t.Fatalf("isTruncatedArguments(%q) = %v, want %v", test.raw, got, test.truncated)
		}
		if got := normalizeToolArguments(test.raw); got != test.normalized {
			t.Fatalf("normalizeToolArguments(%q) = %q, want %q", test.raw, got, test.normalized)
		}
	}
}

func TestAggregateCompletion(t *testing.T) {
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"why"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hello","reasoning_content":null},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
	}
	outcome, errStream := consumeUpstreamStream([]byte(sseBody(frames...)))
	if errStream != nil {
		t.Fatalf("consumeUpstreamStream: %v", errStream)
	}
	completion := aggregateCompletion(outcome, "m")
	if completion.Object != "chat.completion" || completion.Model != "m" {
		t.Fatalf("completion = %+v", completion)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d", len(completion.Choices))
	}
	choice := completion.Choices[0]
	if choice.FinishReason == nil || *choice.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %v", choice.FinishReason)
	}
	message := choice.Message
	if message.Content != "hello" {
		t.Fatalf("content = %v", message.Content)
	}
	if message.ReasoningContent != "why" {
		t.Fatalf("reasoning = %q", message.ReasoningContent)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "read" {
		t.Fatalf("tool calls = %+v", message.ToolCalls)
	}
	if completion.Usage == nil || completion.Usage.PromptTokens != 5 {
		t.Fatalf("usage = %+v", completion.Usage)
	}
	if !strings.HasPrefix(completion.ID, "chatcmpl-") {
		t.Fatalf("id = %q", completion.ID)
	}

	// A tool-only message must carry content:null, per the OpenAI spec.
	toolOnly := aggregateCompletion(&streamOutcome{
		ToolCalls:         []collectedToolCall{{Index: 0, ID: "call_1", Name: "read", Arguments: ""}},
		finishReasonFinal: "tool_calls",
	}, "m")
	if toolOnly.Choices[0].Message.Content != nil {
		t.Fatalf("tool-only content = %v, want nil", toolOnly.Choices[0].Message.Content)
	}
	if toolOnly.Choices[0].Message.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("arguments = %q", toolOnly.Choices[0].Message.ToolCalls[0].Function.Arguments)
	}
}

func TestExecutorHandlersEndToEnd(t *testing.T) {
	stream := sseBody(
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi","reasoning_content":null},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		`[DONE]`,
	)
	fake := newFakeHost().
		on(httpRoute{Method: http.MethodGet, Match: "api-overmind", Body: `{"data":{"value":{"version":"2026.9.4"}},"code":0}`}).
		on(httpRoute{Method: http.MethodPost, Match: ChatPath, Headers: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: stream})
	host := installFakeHost(t, fake)

	credential := &Credential{AccessToken: "token", FirstKeyfrom: "1", LatestKeyfrom: "2"}
	request := pluginapi.ExecutorRequest{
		Model:       "kimi-k3",
		Payload:     []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`),
		StorageJSON: mustJSON(t, credential),
	}
	raw := mustJSON(t, request)

	t.Run("execute aggregates", func(t *testing.T) {
		value, errExecute := handleExecutorExecute(host, raw)
		if errExecute != nil {
			t.Fatalf("handleExecutorExecute: %v", errExecute)
		}
		response := value.(pluginapi.ExecutorResponse)
		if got := string(response.Headers.Get("Content-Type")); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		var completion map[string]any
		if errUnmarshal := decodeJSON(response.Payload, &completion); errUnmarshal != nil {
			t.Fatalf("decode completion: %v", errUnmarshal)
		}
		if completion["object"] != "chat.completion" {
			t.Fatalf("completion = %v", completion)
		}
	})

	t.Run("execute_stream relays frames and appends DONE", func(t *testing.T) {
		value, errStreamCall := handleExecutorExecuteStream(host, raw)
		if errStreamCall != nil {
			t.Fatalf("handleExecutorExecuteStream: %v", errStreamCall)
		}
		response := value.(executorStreamResponse)
		if len(response.Chunks) != 3 {
			t.Fatalf("chunks = %d, want 3", len(response.Chunks))
		}
		if !strings.HasSuffix(string(response.Chunks[len(response.Chunks)-1].Payload), "[DONE]\n\n") {
			t.Fatalf("last chunk = %q", response.Chunks[len(response.Chunks)-1].Payload)
		}
	})

	t.Run("the upstream call forces stream and declares identity headers", func(t *testing.T) {
		requests := fake.requestsFor(ChatPath)
		if len(requests) == 0 {
			t.Fatal("no chat request recorded")
		}
		request := requests[0]
		if got := request.Headers.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := request.Headers.Get("X-LobsterAI-Client-Version"); got != "2026.9.4" {
			t.Fatalf("X-LobsterAI-Client-Version = %q, want the dynamically resolved version", got)
		}
		assertNoTencentHeaders(t, request.Headers)
		body := map[string]any{}
		if errUnmarshal := decodeJSON(request.Body, &body); errUnmarshal != nil {
			t.Fatalf("decode chat body: %v", errUnmarshal)
		}
		if body["stream"] != true {
			t.Fatalf("stream = %v, want true", body["stream"])
		}
	})
}

func TestExecutorHandlersUpstreamFailure(t *testing.T) {
	fake := newFakeHost().
		on(httpRoute{Method: http.MethodPost, Match: ChatPath, Status: http.StatusUnauthorized, Body: `{"code":40101,"message":"session dead"}`})
	host := installFakeHost(t, fake)
	raw := mustJSON(t, pluginapi.ExecutorRequest{
		Model:       "m",
		Payload:     []byte(`{"model":"m","messages":[{"role":"user"}]}`),
		StorageJSON: mustJSON(t, &Credential{AccessToken: "token"}),
	})

	_, errExecute := handleExecutorExecute(host, raw)
	if errExecute == nil {
		t.Fatal("expected a failure")
	}
	envelopeError, ok := errExecute.(*abiboot.EnvelopeError)
	if !ok {
		t.Fatalf("error type = %T", errExecute)
	}
	// A dead session must surface as 401 so the host's scheduler can rotate.
	if envelopeError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("HTTPStatus = %d, want 401", envelopeError.HTTPStatus)
	}
}

func TestExecutorHandlersRejectBadCredential(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)
	raw := mustJSON(t, pluginapi.ExecutorRequest{Model: "m", Payload: []byte(`{}`), StorageJSON: []byte(`{}`)})
	if _, errExecute := handleExecutorExecute(host, raw); errExecute == nil {
		t.Fatal("an unusable credential must fail before any upstream call")
	}
	if _, errStream := handleExecutorExecuteStream(host, raw); errStream == nil {
		t.Fatal("an unusable credential must fail before any upstream call")
	}
	if len(fake.requests) != 0 {
		t.Fatal("no upstream call may happen without a credential")
	}
}

func TestHandleExecutorIdentifier(t *testing.T) {
	value, errIdentifier := handleExecutorIdentifier(nil, nil)
	if errIdentifier != nil {
		t.Fatalf("handleExecutorIdentifier: %v", errIdentifier)
	}
	if encoded := mustJSON(t, value); string(encoded) != `{"identifier":"lobsterai"}` {
		t.Fatalf("executor.identifier = %s", encoded)
	}
}

func TestHandleExecutorCountTokens(t *testing.T) {
	value, errCount := handleExecutorCountTokens(nil, mustJSON(t, pluginapi.ExecutorRequest{Payload: []byte(strings.Repeat("a", 40))}))
	if errCount != nil {
		t.Fatalf("handleExecutorCountTokens: %v", errCount)
	}
	response := value.(pluginapi.ExecutorResponse)
	if !strings.Contains(string(response.Payload), `"input_tokens":11`) {
		t.Fatalf("payload = %s", response.Payload)
	}
}

func TestExecutorStreamEmptyUpstream(t *testing.T) {
	fake := newFakeHost().on(httpRoute{Method: http.MethodPost, Match: ChatPath, Body: ""})
	host := installFakeHost(t, fake)
	raw := mustJSON(t, pluginapi.ExecutorRequest{
		Model:       "m",
		Payload:     []byte(`{"model":"m","messages":[{"role":"user"}]}`),
		StorageJSON: mustJSON(t, &Credential{AccessToken: "token"}),
	})
	if _, errStream := handleExecutorExecuteStream(host, raw); errStream == nil {
		t.Fatal("an empty upstream body must fail")
	}
}

func TestRawStringNullHandling(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "absent", raw: ``, ok: false},
		{name: "null", raw: `null`, ok: false},
		{name: "empty string", raw: `""`, want: "", ok: true},
		{name: "string", raw: `"text"`, want: "text", ok: true},
		{name: "number", raw: `12`, ok: false},
		{name: "object", raw: `{}`, ok: false},
		{name: "array", raw: `[]`, ok: false},
		{name: "true", raw: `true`, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := rawString(json.RawMessage(test.raw))
			if ok != test.ok || got != test.want {
				t.Fatalf("rawString(%s) = %q,%v want %q,%v", test.raw, got, ok, test.want, test.ok)
			}
		})
	}
}
