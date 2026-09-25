package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNormalizeToolArguments(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "{}"},
		{"   ", "{}"},
		{`{"file_path":"/tmp/a"}`, `{"file_path":"/tmp/a"}`},
		{`{"file_path": "/tmp/a"`, "{}"}, // truncated: cannot become a fake call
		{"null", "{}"},
		{"[]", "{}"},
		{"42", "{}"},
	}
	for _, test := range tests {
		if got := normalizeToolArguments(test.in); got != test.want {
			t.Errorf("normalizeToolArguments(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestResolveToolPairing(t *testing.T) {
	// The TS rule is per batch: a batch is kept only when EVERY call in it has a
	// result, so the two calls live in separate messages here.
	messages := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "paired", "function": map[string]any{"name": "read", "arguments": "{}"}},
		}},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "orphan-call", "function": map[string]any{"name": "read", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "paired", "content": "ok"},
		map[string]any{"role": "tool", "tool_call_id": "orphan-result", "content": "stray"},
	}
	keepCalls, keepResults := resolveToolPairing(messages)
	if !keepCalls["paired"] {
		t.Error("the fully paired call must be kept")
	}
	if keepCalls["orphan-call"] {
		t.Error("a call without a result must be dropped")
	}
	if keepResults["orphan-result"] {
		t.Error("a result without a call must be dropped")
	}
	if !keepResults["paired"] {
		t.Error("the paired result must be kept")
	}
}

func TestNormalizeMessages(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "be nice"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"id": "c1", "function": map[string]any{"name": "read", "arguments": ""}},
				map[string]any{"id": "c2", "function": map[string]any{"name": "read", "arguments": `{"p":1}`}},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": ""},
		map[string]any{"role": "tool", "tool_call_id": "c2", "content": []any{
			map[string]any{"type": "text", "text": "text part"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "never-called", "content": "drop me"},
		map[string]any{"role": "assistant", "content": "final", "reasoning_content": "think"},
	}

	out := normalizeMessages(messages)

	var assistantWithCalls map[string]any
	var toolMessages []map[string]any
	var imageCarrier map[string]any
	for _, message := range out {
		record := message.(map[string]any)
		switch record["role"] {
		case "assistant":
			if _, hasCalls := record["tool_calls"]; hasCalls {
				assistantWithCalls = record
			} else {
				if record["reasoning_content"] != "think" {
					t.Errorf("existing reasoning_content lost: %+v", record)
				}
			}
		case "tool":
			toolMessages = append(toolMessages, record)
		case "user":
			content, isSlice := record["content"].([]any)
			if isSlice && len(content) > 0 {
				if first, ok := content[0].(map[string]any); ok && first["text"] == toolResultImageText {
					imageCarrier = record
				}
			}
		}
	}

	if assistantWithCalls == nil {
		t.Fatal("the assistant message with paired tool calls is missing")
	}
	// c1 and c2 both have results, so both calls stay; empty arguments become {}.
	calls := assistantWithCalls["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("kept %d tool calls, want 2", len(calls))
	}
	if calls[0].(map[string]any)["function"].(map[string]any)["arguments"] != "{}" {
		t.Errorf("empty arguments must become {}: %+v", calls[0])
	}
	if assistantWithCalls["content"] != nil {
		t.Errorf("empty content with tool calls must be null, got %#v", assistantWithCalls["content"])
	}

	if len(toolMessages) != 2 {
		t.Fatalf("kept %d tool results, want 2 (the orphan must be dropped)", len(toolMessages))
	}
	if toolMessages[0]["content"] != "(no output)" {
		t.Errorf("empty tool output must become the placeholder, got %#v", toolMessages[0]["content"])
	}
	if toolMessages[1]["content"] != "text part" {
		t.Errorf("tool text = %#v, want the text part only", toolMessages[1]["content"])
	}
	if imageCarrier == nil {
		t.Fatal("an image lifted out of a tool result must be emitted as its own user message")
	}

	// A final assistant message must always carry reasoning_content, even when
	// the client never sent one (buddy-adapter.ts:224-226).
	for _, message := range out {
		record := message.(map[string]any)
		if role, _ := record["role"].(string); role != "assistant" {
			continue
		}
		if _, present := record["reasoning_content"]; !present {
			t.Errorf("assistant message lacks reasoning_content: %+v", record)
		}
	}
}

func TestBuildChatBody(t *testing.T) {
	product, _ := productByConfigValue(ProductCodeBuddy)
	payload := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"top_p":0.9}`)

	remote := &remoteModel{ID: "glm-5.3", MaxOutputTokens: 41_000, ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high"}
	body, errBody := buildChatBody(payload, "glm-5.3", DefaultConfig(), product, remote, "cache-key-1")
	if errBody != nil {
		t.Fatalf("buildChatBody: %v", errBody)
	}
	decoded := map[string]any{}
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if decoded["stream"] != true {
		t.Error("the upstream call must always stream (buddy-adapter.ts:991)")
	}
	if decoded["prompt_cache_key"] != "cache-key-1" {
		t.Errorf("prompt_cache_key = %v", decoded["prompt_cache_key"])
	}
	// The remote maxOutputTokens must reach max_tokens (AGENTS.md requirement).
	if decoded["max_tokens"] != float64(41_000) {
		t.Errorf("max_tokens = %v, want the remote 41000", decoded["max_tokens"])
	}
	// Unknown fields survive.
	if decoded["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want it preserved", decoded["top_p"])
	}
	// glm is not deepseek, so no thinking switch and no injected effort.
	if _, present := decoded["thinking"]; present {
		t.Error("thinking must only be sent for deepseek models")
	}
	if _, present := decoded["reasoning_effort"]; present {
		t.Error("a non-deepseek model without a requested effort must not get one")
	}

	// A caller-supplied max_tokens wins over the remote value.
	explicit, errExplicit := buildChatBody(
		[]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"x"}],"max_tokens":99,"reasoning_effort":"high"}`),
		"glm-5.3", DefaultConfig(), product, remote, "")
	if errExplicit != nil {
		t.Fatalf("buildChatBody(explicit): %v", errExplicit)
	}
	var explicitBody map[string]any
	_ = json.Unmarshal(explicit, &explicitBody)
	if explicitBody["max_tokens"] != float64(99) {
		t.Errorf("explicit max_tokens = %v, want 99", explicitBody["max_tokens"])
	}
	if explicitBody["reasoning_effort"] != "high" {
		t.Errorf("a supported effort must pass through: %v", explicitBody["reasoning_effort"])
	}
	if _, present := explicitBody["prompt_cache_key"]; present {
		t.Error("an empty cache key must not be attached")
	}

	// An unsupported effort is dropped rather than sent (upstream would 400).
	unsupported, errUnsupported := buildChatBody(
		[]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"x"}],"reasoning_effort":"xhigh"}`),
		"glm-5.3", DefaultConfig(), product, remote, "")
	if errUnsupported != nil {
		t.Fatalf("buildChatBody(unsupported): %v", errUnsupported)
	}
	var unsupportedBody map[string]any
	_ = json.Unmarshal(unsupported, &unsupportedBody)
	if _, present := unsupportedBody["reasoning_effort"]; present {
		t.Error("an effort the model does not declare must be dropped")
	}

	// deepseek: thinking is injected and an effort is always sent.
	deepseekRemote := &remoteModel{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high"}
	deepseekBody, errDeepseek := buildChatBody(
		[]byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"x"}]}`),
		"deepseek-v4-flash", DefaultConfig(), product, deepseekRemote, "")
	if errDeepseek != nil {
		t.Fatalf("buildChatBody(deepseek): %v", errDeepseek)
	}
	var deepseek map[string]any
	_ = json.Unmarshal(deepseekBody, &deepseek)
	if thinking, ok := deepseek["thinking"].(map[string]any); !ok || thinking["type"] != "enabled" {
		t.Errorf("thinking = %v, want {type: enabled}", deepseek["thinking"])
	}
	if deepseek["reasoning_effort"] != "high" {
		t.Errorf("deepseek effort = %v, want the declared default high", deepseek["reasoning_effort"])
	}

	// max_tokens chain: nothing declared anywhere means no field at all.
	bare, errBare := buildChatBody([]byte(`{"model":"unknown-model","messages":[{"role":"user","content":"x"}]}`), "unknown-model", DefaultConfig(), product, nil, "")
	if errBare != nil {
		t.Fatalf("buildChatBody(bare): %v", errBare)
	}
	var bareBody map[string]any
	_ = json.Unmarshal(bare, &bareBody)
	if _, present := bareBody["max_tokens"]; present {
		t.Error("no value anywhere means max_tokens must be omitted, never invented")
	}

	// The product fallback table fills in when the remote says nothing.
	fallback, errFallback := buildChatBody(
		[]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"x"}]}`),
		"deepseek-v4.1-flash", DefaultConfig(), product, nil, "")
	if errFallback != nil {
		t.Fatalf("buildChatBody(fallback): %v", errFallback)
	}
	var fallbackBody map[string]any
	_ = json.Unmarshal(fallback, &fallbackBody)
	if fallbackBody["max_tokens"] != float64(128_000) {
		t.Errorf("fallback max_tokens = %v, want the 128000 from product.ts", fallbackBody["max_tokens"])
	}

	// Errors.
	if _, errInvalid := buildChatBody([]byte("{"), "m", DefaultConfig(), product, nil, ""); errInvalid == nil {
		t.Error("malformed JSON body must be rejected")
	}
	if _, errNoModel := buildChatBody([]byte(`{"messages":[]}`), "", DefaultConfig(), product, nil, ""); errNoModel == nil {
		t.Error("a request without a model must be rejected")
	}
	if _, errNoMessages := buildChatBody([]byte(`{"model":"m"}`), "m", DefaultConfig(), product, nil, ""); errNoMessages == nil {
		t.Error("a request without messages must be rejected")
	}
	if _, errEmptyMessages := buildChatBody([]byte(`{"model":"m","messages":[]}`), "m", DefaultConfig(), product, nil, ""); errEmptyMessages == nil {
		t.Error("an empty message list must be rejected")
	}
}

func TestBuildChatHeaders(t *testing.T) {
	workbuddy, _ := productByConfigValue(ProductWorkBuddy)
	credential := &Credential{AccessToken: "tok", Domain: "copilot.tencent.com"}
	headers := buildChatHeaders(credential, workbuddy, "glm-5.3")

	expectations := map[string]string{
		"Authorization":   "Bearer tok",
		"Accept":          "text/event-stream",
		HeaderDomain:      "www.workbuddy.ai", // follows the product, not the credential
		HeaderProductCode: "workbuddy",
		"X-Agent-Purpose": "conversation",
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
		"X-IDE-Version":   "5.5.2",
		HeaderProduct:     "WorkBuddy",
		"User-Agent":      workBuddyUACN, // glm- is a China-line model
	}
	for key, want := range expectations {
		if got := headers.Get(key); got != want {
			t.Errorf("header %s = %q, want %q", key, got, want)
		}
	}
	if got := headers.Get("User-Agent"); got == workBuddyUAIntl {
		t.Error("a glm model must not use the international WorkBuddy UA")
	}
	if got := buildChatHeaders(credential, workbuddy, "gpt-5.6-sol").Get("User-Agent"); got != workBuddyUAIntl {
		t.Errorf("gpt UA = %q, want the international WorkBuddy UA", got)
	}
}

func TestHTTPErrorClassification(t *testing.T) {
	contextBody := `{"code":11115,"msg":"prompt is too long: 1061554 tokens > 1048576 maximum",` +
		`"extError":{"code":"context_length_exceeded","type":"invalid_request_error"}}`
	tests := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, `{}`, "AUTH"},
		{http.StatusForbidden, `{}`, "AUTH"},
		{http.StatusTooManyRequests, `{}`, "RATE_LIMIT"},
		{http.StatusBadRequest, contextBody, "CONTEXT_WINDOW_EXCEEDED"},
		{http.StatusBadRequest, `{"error":{"message":"bad field"}}`, "INVALID_REQUEST"},
		{http.StatusInternalServerError, `{}`, "SERVER"},
		{http.StatusTeapot, `{}`, "HTTP_418"},
	}
	for _, test := range tests {
		if got := httpErrorCode(test.status, test.body); got != test.want {
			t.Errorf("httpErrorCode(%d) = %q, want %q", test.status, got, test.want)
		}
	}
	if !isContextWindowExceeded(`{"displayMsg":{"en":"The request exceeds the model context limit."}}`) {
		t.Error("the displayMsg wording must also be recognised")
	}
	if isContextWindowExceeded(`{"error":{"message":"unknown tool"}}`) {
		t.Error("an unrelated 400 must not be classified as an overflow")
	}
}

func TestErrorDetail(t *testing.T) {
	got := errorDetail(`{"error":{"code":"a","type":"b","message":"c"},"message":"d"}`)
	for _, fragment := range []string{"a", "b", "c", "d"} {
		if !strings.Contains(got, fragment) {
			t.Errorf("errorDetail = %q, missing %q", got, fragment)
		}
	}
	if got := errorDetail("<html>gateway</html>"); !strings.Contains(got, "html") {
		t.Errorf("a non-JSON body must be surfaced verbatim, got %q", got)
	}
	if got := errorDetail(""); got == "" {
		t.Error("an empty body must still produce a message")
	}
}

func TestChunkErrorPayload(t *testing.T) {
	overflow := chatChunk{Error: json.RawMessage(
		`{"message":"prompt is too long","extError":{"code":"context_length_exceeded"}}`)}
	detail, exceeded := chunkErrorPayload(&overflow)
	if detail == "" || !exceeded {
		t.Errorf("inline overflow = (%q, %v), want a detail and exceeded=true", detail, exceeded)
	}

	plain := chatChunk{Error: json.RawMessage(`{"message":"boom"}`)}
	detail, exceeded = chunkErrorPayload(&plain)
	if detail != "boom" || exceeded {
		t.Errorf("inline error = (%q, %v)", detail, exceeded)
	}

	// A business code with no choices (HTTP 200) is also a failure.
	coded := chatChunk{Code: float64(11102), Msg: "model service info not found"}
	detail, exceeded = chunkErrorPayload(&coded)
	if !strings.Contains(detail, "11102") && !strings.Contains(detail, "model service") {
		t.Errorf("coded failure detail = %q", detail)
	}
	if exceeded {
		t.Error("a coded failure is not a context overflow")
	}

	healthy := chatChunk{Choices: []streamChoice{{Index: 0}}}
	if detail, exceeded = chunkErrorPayload(&healthy); detail != "" || exceeded {
		t.Errorf("a healthy chunk must not be an error: (%q, %v)", detail, exceeded)
	}
}

func TestAggregateChunks(t *testing.T) {
	chunks := []chatChunk{
		{ID: "chatcmpl-1", Created: 123, Model: "glm-5.3", Choices: []streamChoice{{
			Delta: streamDelta{Role: "assistant", Content: "Hel"},
		}}},
		{Choices: []streamChoice{{Delta: streamDelta{ReasoningContent: "why"}}}},
		{Choices: []streamChoice{{
			Delta: streamDelta{Content: "lo", ToolCalls: []streamToolCall{{
				Index: intPointer(0), ID: "call_1", Function: streamFunction{Name: "read", Arguments: `{"a"`},
			}}},
		}}},
		{Choices: []streamChoice{{
			Delta: streamDelta{ToolCalls: []streamToolCall{{
				Index: intPointer(0), Function: streamFunction{Name: "", Arguments: `:1}`},
			}}},
			FinishReason: stringPointer("tool_calls"),
		}}},
	}
	completion := aggregateChunks(chunks, "glm-5.3")
	if completion["id"] != "chatcmpl-1" || completion["model"] != "glm-5.3" {
		t.Errorf("completion identity = %+v", completion)
	}
	choices := completion["choices"].([]map[string]any)
	message := choices[0]["message"].(map[string]any)
	if message["content"] != "Hello" {
		t.Errorf("content = %q, want the concatenation", message["content"])
	}
	if message["reasoning_content"] != "why" {
		t.Errorf("reasoning = %q", message["reasoning_content"])
	}
	toolCalls := message["tool_calls"].([]map[string]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(toolCalls))
	}
	function := toolCalls[0]["function"].(map[string]any)
	if function["arguments"] != `{"a":1}` {
		t.Errorf("arguments = %q, want the fragments joined", function["arguments"])
	}
	// An empty name fragment must not overwrite the real tool name.
	if function["name"] != "read" {
		t.Errorf("name = %q, want read", function["name"])
	}
	if choices[0]["finish_reason"] != "tool_calls" {
		t.Errorf("finish reason = %v", choices[0]["finish_reason"])
	}

	// No tool calls: the finish reason defaults to stop.
	plain := aggregateChunks([]chatChunk{{Choices: []streamChoice{{Delta: streamDelta{Content: "x"}}}}}, "m")
	plainChoices := plain["choices"].([]map[string]any)
	if plainChoices[0]["finish_reason"] != "stop" {
		t.Errorf("default finish reason = %v", plainChoices[0]["finish_reason"])
	}
}

func TestParseChunk(t *testing.T) {
	if _, ok := parseChunk("[DONE]"); ok {
		t.Error("[DONE] is not a chunk")
	}
	if _, ok := parseChunk("{not json"); ok {
		t.Error("malformed JSON is not a chunk")
	}
	chunk, ok := parseChunk(`{"choices":[{"delta":{"content":"x"}}]}`)
	if !ok || len(chunk.Choices) != 1 || chunk.Choices[0].Delta.Content != "x" {
		t.Fatalf("parseChunk = (%+v, %v)", chunk, ok)
	}
	if got := stringValue(chunk.Choices[0].Delta.Content); got != "x" {
		t.Errorf("stringValue = %q", got)
	}
	if got := stringValue(nil); got != "" {
		t.Errorf("stringValue(nil) = %q", got)
	}
}

func TestPromptCacheKeyFor(t *testing.T) {
	request := pluginapi.ExecutorRequest{Metadata: map[string]any{"session_id": "s-1"}}
	if got := promptCacheKeyFor(request, nil); got != "s-1" {
		t.Errorf("metadata session key = %q", got)
	}
	request = pluginapi.ExecutorRequest{Headers: http.Header{"X-Session-Id": []string{"h-1"}}}
	if got := promptCacheKeyFor(request, nil); got != "h-1" {
		t.Errorf("header session key = %q", got)
	}
	// Derivation from the stable conversation prefix must be deterministic.
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "stable prefix"}}}
	first := promptCacheKeyFor(pluginapi.ExecutorRequest{}, body)
	second := promptCacheKeyFor(pluginapi.ExecutorRequest{}, body)
	if first != second || len(first) != 32 {
		t.Errorf("derived key = %q / %q, want a stable 32-char key", first, second)
	}
	if len(promptCacheKeyFor(pluginapi.ExecutorRequest{}, nil)) != 32 {
		t.Error("with no material at all a random key must still be produced")
	}
}

func TestTranslateIsIdentity(t *testing.T) {
	raw := json.RawMessage(`{"Body":"aGVsbG8="}`)
	value, errRequest := handleRequestTranslate(nil, raw)
	if errRequest != nil {
		t.Fatalf("request translate: %v", errRequest)
	}
	payload, _ := value.(pluginapi.PayloadResponse)
	if string(payload.Body) != "hello" {
		t.Errorf("request translate body = %q", payload.Body)
	}

	responseRaw := json.RawMessage(`{"Body":"d29ybGQ="}`)
	value, errResponse := handleResponseTranslate(nil, responseRaw)
	if errResponse != nil {
		t.Fatalf("response translate: %v", errResponse)
	}
	payload, _ = value.(pluginapi.PayloadResponse)
	if string(payload.Body) != "world" {
		t.Errorf("response translate body = %q", payload.Body)
	}
}

func TestExecutorIdentifierAndCountTokens(t *testing.T) {
	value, errIdentifier := handleExecutorIdentifier(nil, nil)
	if errIdentifier != nil {
		t.Fatalf("identifier: %v", errIdentifier)
	}
	identifier, _ := value.(abiboot.Identifier)
	if identifier.Identifier != ProviderKey {
		t.Errorf("identifier = %q", identifier.Identifier)
	}

	raw, errMarshal := json.Marshal(pluginapi.ExecutorRequest{Payload: []byte(strings.Repeat("a", 400))})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	value, errTokens := handleExecutorCountTokens(nil, raw)
	if errTokens != nil {
		t.Fatalf("count tokens: %v", errTokens)
	}
	response, _ := value.(pluginapi.ExecutorResponse)
	if string(response.Payload) != `{"input_tokens":101}` {
		t.Errorf("count tokens payload = %q", response.Payload)
	}
}

func intPointer(value int) *int          { return &value }
func stringPointer(value string) *string { return &value }
