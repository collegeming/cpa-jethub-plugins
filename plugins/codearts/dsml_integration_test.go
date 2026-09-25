package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/openai"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// dsmlExampleBlock is the literal shape the system prompt teaches, using the
// full-width pipe U+FF5C that the model actually emits.
func dsmlExampleBlock() string {
	return DsmlToolCallsOpen +
		"<｜DSML｜invoke name=\"bash\">" +
		"<｜DSML｜parameter name=\"command\" string=\"true\">ls -la</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜tool_calls>"
}

func requestWithTools(model string) []byte {
	return []byte(`{"model":"` + model + `","messages":[` +
		`{"role":"system","content":"base"},` +
		`{"role":"user","content":"list the files"}],` +
		`"tools":[{"type":"function","function":{"name":"bash","description":"run","parameters":{"type":"object"}}}]}`)
}

// TestPrepareRequestBodyUsesDsmlForDeepseekV4 covers the request-side rewrite:
// the tools array must be withheld, the instruction injected directly after the
// first system message, and tool_stream kept on.
func TestPrepareRequestBodyUsesDsmlForDeepseekV4(t *testing.T) {
	encoded, request, errPrepare := prepareRequestBody(requestWithTools("deepseek-v4-flash"), "", DefaultConfig(), "sess-1")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}

	var wire map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(encoded, &wire); errUnmarshal != nil {
		t.Fatalf("decode wire body: %v", errUnmarshal)
	}
	if _, present := wire["tools"]; present {
		t.Fatal("deepseek-v4 request still carries a tools array; the model would take the standard tool_calls path")
	}
	if string(wire["tool_stream"]) != "true" {
		t.Fatalf("tool_stream = %s, want true", wire["tool_stream"])
	}

	if len(request.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (system, injected system, user)", len(request.Messages))
	}
	if request.Messages[0].Content != "base" {
		t.Fatalf("messages[0] = %#v, want the original system message first", request.Messages[0].Content)
	}
	if request.Messages[1].Role != "system" {
		t.Fatalf("messages[1] role = %q, want the injected DSML system message", request.Messages[1].Role)
	}
	injected, _ := request.Messages[1].Content.(string)
	if !strings.Contains(injected, DsmlToolCallsOpen) {
		t.Fatalf("injected message does not teach the DSML syntax: %.120q", injected)
	}
	if !strings.Contains(injected, `"bash"`) {
		t.Fatalf("injected message omits the tool schema: %.200q", injected)
	}
	if request.Messages[2].Role != "user" {
		t.Fatalf("messages[2] role = %q, want the user message to follow the injection", request.Messages[2].Role)
	}
}

// TestPrepareRequestBodyDsmlInjectionWithoutSystemMessage pins the no-system
// case: the instruction becomes the first message because findIndex misses.
func TestPrepareRequestBodyDsmlInjectionWithoutSystemMessage(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}]}`)
	_, request, errPrepare := prepareRequestBody(payload, "", DefaultConfig(), "sess-2")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}
	if len(request.Messages) != 2 || request.Messages[0].Role != "system" || request.Messages[1].Role != "user" {
		t.Fatalf("messages = %#v, want the DSML system message prepended", request.Messages)
	}
}

// TestPrepareRequestBodyKeepsNativeToolsForOtherModels guards the other half:
// non-deepseek models keep the standard tools array and get no instruction.
func TestPrepareRequestBodyKeepsNativeToolsForOtherModels(t *testing.T) {
	encoded, request, errPrepare := prepareRequestBody(requestWithTools("GLM-5.2"), "", DefaultConfig(), "sess-3")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}
	if !strings.Contains(string(encoded), `"tools"`) {
		t.Fatal("GLM-5.2 request lost its tools array")
	}
	if len(request.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (no injection for non-deepseek models)", len(request.Messages))
	}
	if strings.Contains(string(encoded), DsmlToolCallsOpen) {
		t.Fatal("a non-deepseek request was given the DSML instruction")
	}
}

// TestAggregateCompletionExtractsDsmlToolCalls covers the non-streaming path:
// the DSML block must leave the visible content and become a structured call.
func TestAggregateCompletionExtractsDsmlToolCalls(t *testing.T) {
	stop := "stop"
	chunks := []openai.Chunk{{
		ID:    "chunk-1",
		Model: "deepseek-v4-flash",
		Choices: []openai.ChunkChoice{{
			Index:        0,
			Delta:        openai.Delta{Content: "listing now " + dsmlExampleBlock() + " done"},
			FinishReason: &stop,
		}},
	}}

	completion := aggregateCompletion(chunks, "deepseek-v4-flash")
	message := completion.Choices[0].Message
	text, _ := message.Content.(string)
	if strings.Contains(text, "DSML") {
		t.Fatalf("DSML markup survived into the content: %q", text)
	}
	if !strings.Contains(text, "listing now") || !strings.Contains(text, "done") {
		t.Fatalf("visible text around the DSML block was lost: %q", text)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(message.ToolCalls))
	}
	if message.ToolCalls[0].Function.Name != "bash" {
		t.Fatalf("tool name = %q, want bash", message.ToolCalls[0].Function.Name)
	}
	if !strings.Contains(message.ToolCalls[0].Function.Arguments, "ls -la") {
		t.Fatalf("arguments = %q, want the command value", message.ToolCalls[0].Function.Arguments)
	}
	reason := completion.Choices[0].FinishReason
	if reason == nil || *reason != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", reason)
	}
}

// TestAggregateCompletionLeavesPlainTextAlone is the no-regression guard: an
// ordinary reply must not be rewritten.
func TestAggregateCompletionLeavesPlainTextAlone(t *testing.T) {
	completion := aggregateCompletion([]openai.Chunk{{
		Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: "just prose"}}},
	}}, "GLM-5.2")
	if text, _ := completion.Choices[0].Message.Content.(string); text != "just prose" {
		t.Fatalf("content = %q, want it untouched", text)
	}
	if len(completion.Choices[0].Message.ToolCalls) != 0 {
		t.Fatal("plain text produced tool calls")
	}
}

func streamChunk(t *testing.T, delta openai.Delta, finish *string) pluginapi.ExecutorStreamChunk {
	t.Helper()
	payload, errMarshal := sse.EncodeJSON(openai.Chunk{
		ID:      "chunk-1",
		Object:  "chat.completion.chunk",
		Model:   "deepseek-v4-flash",
		Choices: []openai.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	})
	if errMarshal != nil {
		t.Fatalf("encode chunk: %v", errMarshal)
	}
	return pluginapi.ExecutorStreamChunk{Payload: payload}
}

func decodeStream(t *testing.T, chunks []pluginapi.ExecutorStreamChunk) []openai.Chunk {
	t.Helper()
	out := make([]openai.Chunk, 0, len(chunks))
	for _, item := range chunks {
		line := strings.TrimSpace(string(item.Payload))
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if line == "" || line == sse.Done {
			continue
		}
		var parsed openai.Chunk
		if errUnmarshal := json.Unmarshal([]byte(line), &parsed); errUnmarshal != nil {
			t.Fatalf("decode streamed chunk %q: %v", line, errUnmarshal)
		}
		out = append(out, parsed)
	}
	return out
}

// TestRewriteDsmlStreamHandlesSplitBlock is the key streaming test: the DSML
// block is delivered in two halves, so the rewrite must rely on the
// concatenated content rather than a single chunk.
func TestRewriteDsmlStreamHandlesSplitBlock(t *testing.T) {
	block := dsmlExampleBlock()
	midpoint := len(block) / 2
	stop := "stop"

	chunks := []pluginapi.ExecutorStreamChunk{
		streamChunk(t, openai.Delta{Content: "here " + block[:midpoint]}, nil),
		streamChunk(t, openai.Delta{Content: block[midpoint:] + " end"}, &stop),
	}

	rewritten := rewriteDsmlStream(chunks)
	decoded := decodeStream(t, rewritten)
	if len(decoded) != 2 {
		t.Fatalf("chunks = %d, want 2", len(decoded))
	}

	var text strings.Builder
	var calls []openai.ToolCall
	for _, chunk := range decoded {
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
			calls = append(calls, choice.Delta.ToolCalls...)
		}
	}
	if strings.Contains(text.String(), "DSML") {
		t.Fatalf("DSML markup survived into the stream: %q", text.String())
	}
	if !strings.Contains(text.String(), "here") || !strings.Contains(text.String(), "end") {
		t.Fatalf("visible text was lost: %q", text.String())
	}
	if len(calls) != 1 {
		t.Fatalf("streamed tool calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Name != "bash" {
		t.Fatalf("tool name = %q, want bash", calls[0].Function.Name)
	}
	if calls[0].Index == nil || *calls[0].Index != 0 {
		t.Fatalf("tool call index = %v, want 0", calls[0].Index)
	}
	last := decoded[len(decoded)-1].Choices[0].FinishReason
	if last == nil || *last != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", last)
	}
}

// TestRewriteDsmlStreamIsIdentityWithoutDsml guards against gratuitous churn on
// every ordinary streamed reply.
func TestRewriteDsmlStreamIsIdentityWithoutDsml(t *testing.T) {
	stop := "stop"
	chunks := []pluginapi.ExecutorStreamChunk{
		streamChunk(t, openai.Delta{Content: "hello "}, nil),
		streamChunk(t, openai.Delta{Content: "world"}, &stop),
	}
	rewritten := rewriteDsmlStream(chunks)
	if len(rewritten) != len(chunks) {
		t.Fatalf("chunk count changed: %d -> %d", len(chunks), len(rewritten))
	}
	for index := range chunks {
		if string(rewritten[index].Payload) != string(chunks[index].Payload) {
			t.Fatalf("chunk %d was rewritten despite carrying no DSML", index)
		}
	}
}

// TestAggregateCompletionRoutesThoughtToReasoning covers the inlined-thought
// case: <thought> text must reach the reasoning channel rather than being
// dropped or leaking into the visible answer.
func TestAggregateCompletionRoutesThoughtToReasoning(t *testing.T) {
	completion := aggregateCompletion([]openai.Chunk{{
		Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{
			Content: "answer <thought>internal reasoning</thought> end",
		}}},
	}}, "deepseek-v4-flash")

	message := completion.Choices[0].Message
	text, _ := message.Content.(string)
	if strings.Contains(text, "<thought>") || strings.Contains(text, "internal reasoning") {
		t.Fatalf("thought content leaked into the visible answer: %q", text)
	}
	if !strings.Contains(text, "answer") || !strings.Contains(text, "end") {
		t.Fatalf("visible text around the thought block was lost: %q", text)
	}
	if !strings.Contains(message.ReasoningContent, "internal reasoning") {
		t.Fatalf("reasoning content = %q, want the thought text", message.ReasoningContent)
	}
}

// TestRewriteDsmlStreamRoutesThoughtToReasoning is the streaming counterpart.
func TestRewriteDsmlStreamRoutesThoughtToReasoning(t *testing.T) {
	chunks := []pluginapi.ExecutorStreamChunk{
		streamChunk(t, openai.Delta{Content: "answer <thought>rea"}, nil),
		streamChunk(t, openai.Delta{Content: "soning</thought> end"}, nil),
	}
	decoded := decodeStream(t, rewriteDsmlStream(chunks))

	var visible, reasoning strings.Builder
	for _, chunk := range decoded {
		for _, choice := range chunk.Choices {
			visible.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
		}
	}
	if strings.Contains(visible.String(), "thought") || strings.Contains(visible.String(), "soning") {
		t.Fatalf("thought leaked into visible content: %q", visible.String())
	}
	if !strings.Contains(visible.String(), "answer") || !strings.Contains(visible.String(), "end") {
		t.Fatalf("visible text was lost: %q", visible.String())
	}
	if !strings.Contains(reasoning.String(), "reasoning") {
		t.Fatalf("reasoning channel = %q, want the thought text", reasoning.String())
	}
}
