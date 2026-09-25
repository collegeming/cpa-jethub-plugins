package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// nativeRequest builds a DSH-native request: the shape that must be serialized
// before the SOLO conversion (docs/agents/trae.md:14-27).
func nativeRequest() []any {
	return []any{
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "sys prompt"}}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look at this"},
			map[string]any{"type": "image", "attachment": map[string]any{"attachmentId": "att1"}},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "reasoning", "text": "thinking hard"},
			map[string]any{"type": "text", "text": "calling read"},
			map[string]any{"type": "tool-call", "id": "call_1", "name": "read", "arguments": `{"path":"/a"}`},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool-result", "toolCallId": "call_1", "content": []any{
				map[string]any{"type": "text", "text": "file body"},
			}},
		}},
	}
}

// TestSerializationHappensBeforeConversion locks the ordering rule: native blocks
// must become tool_calls + role:"tool" messages before the SOLO conversion, or
// the model never sees its own tool calls or their results.
func TestSerializationHappensBeforeConversion(t *testing.T) {
	messages := nativeRequest()
	imageURLs := map[string]string{"att1": "data:image/png;base64,AAAA"}

	wire := SerializeMessages(messages, imageURLs)
	if len(wire) != 4 {
		t.Fatalf("expected 4 wire messages (system, user, assistant, tool), got %d: %#v", len(wire), wire)
	}
	if role := readStringField(wire[1], "role"); role != "user" {
		t.Fatalf("wire[1] role = %q, want user", role)
	}
	parts, ok := asSlice(wire[1]["content"])
	if !ok || len(parts) != 2 {
		t.Fatalf("user content parts = %#v, want 2 parts", wire[1]["content"])
	}
	if kind := readStringField(mustMap(t, parts[1]), "type"); kind != "image_url" {
		t.Fatalf("second part type = %q, want image_url", kind)
	}

	assistant := wire[2]
	if content := assistant["content"]; content != "calling read" {
		t.Fatalf("assistant content = %#v, want \"calling read\"", content)
	}
	calls, ok := asSlice(assistant["tool_calls"])
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %#v, want 1 call", assistant["tool_calls"])
	}
	call := mustMap(t, calls[0])
	if got := readStringField(mustMap(t, call["function"]), "name"); got != "read" {
		t.Fatalf("tool call name = %q, want read", got)
	}

	toolMessage := wire[3]
	if role := readStringField(toolMessage, "role"); role != "tool" {
		t.Fatalf("wire[3] role = %q, want tool", role)
	}
	if got := readStringField(toolMessage, "tool_call_id"); got != "call_1" {
		t.Fatalf("tool_call_id = %q, want call_1", got)
	}
	if got := readStringField(toolMessage, "content"); got != "file body" {
		t.Fatalf("tool content = %q, want \"file body\"", got)
	}

	// Now the full SOLO conversion, exactly as the executor performs it.
	root := map[string]any{
		"model":    "glm-5.2__dev",
		"messages": toAnySlice(wire),
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "read",
				"description": "read a file",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
			},
		}},
		"tool_choice": "auto",
	}
	solo := TransformToSOLOBody(root, configNameFor("glm-5.2__dev"), "solo_agent_remote")

	if got := readStringField(solo, "config_name"); got != "glm-5.2" {
		t.Fatalf("config_name = %q, want glm-5.2 (the __dev suffix must be stripped)", got)
	}
	if got := readStringField(solo, "model"); got != "glm-5.2" {
		t.Fatalf("model = %q, want glm-5.2", got)
	}
	if got := readStringField(solo, "function"); got != "solo_agent_remote" {
		t.Fatalf("function = %q, want the model's own channel", got)
	}
	if stream, _ := solo["stream"].(bool); !stream {
		t.Fatal("stream must be forced to true")
	}

	tools, _ := asSlice(solo["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want 1 entry", solo["tools"])
	}
	parameters := mustMap(t, tools[0])["function"].(map[string]any)["parameters"]
	if _, isString := parameters.(string); !isString {
		t.Fatalf("tools[0].function.parameters = %#v, want a JSON string", parameters)
	}

	soloMessages, _ := asSlice(solo["messages"])
	assistantOut := mustMap(t, soloMessages[2])
	outCalls, _ := asSlice(assistantOut["tool_calls"])
	if len(outCalls) != 1 {
		t.Fatalf("solo assistant tool_calls = %#v, want 1", assistantOut["tool_calls"])
	}
	outCall := mustMap(t, outCalls[0])
	if _, hasFunction := outCall["function"]; hasFunction {
		t.Fatal("SOLO tool calls must use function_call, not function")
	}
	callFunction := mustMap(t, outCall["function_call"])
	if got := readStringField(callFunction, "name"); got != "read" {
		t.Fatalf("function_call.name = %q, want read", got)
	}
	content := mustMap(t, soloMessages[3])
	if got := readStringField(content, "role"); got != "tool" {
		t.Fatalf("solo messages[3].role = %q, want tool", got)
	}
}

// TestOrphanToolCallIsDropped covers the pairing guard: an assistant tool call
// without a result would make upstream reject the whole request.
func TestOrphanToolCallIsDropped(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool-call", "id": "call_x", "name": "read", "arguments": "{}"},
		}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}},
	}
	wire := SerializeMessages(messages, nil)
	if _, has := wire[0]["tool_calls"]; has {
		t.Fatalf("orphan tool call must be dropped, got %#v", wire[0])
	}
}

// TestNamelessToolCallIsDropped: upstream requires FunctionCall.Name.
func TestNamelessToolCallIsDropped(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "x"},
	}
	wire := SerializeMessages(messages, nil)
	if _, has := wire[0]["tool_calls"]; has {
		t.Fatalf("nameless tool call must be dropped, got %#v", wire[0])
	}
}

// TestToolResultImagesMoveToTheirOwnUserMessage: a role:"tool" message may only
// carry string content, so tool-result images are re-homed into a following
// user message. The reference also folds the tool-result content into the user
// message's multimodal parts (trae-adapter.ts:363-370), which is why four wire
// messages come out of one user turn; the port keeps that behaviour.
func TestToolResultImagesMoveToTheirOwnUserMessage(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool-call", "id": "call_1", "name": "read_image", "arguments": "{}"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool-result", "toolCallId": "call_1", "content": []any{
				map[string]any{"type": "text", "text": "captured"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,BBBB"}},
			}},
		}},
	}
	wire := SerializeMessages(messages, map[string]string{})
	if len(wire) != 4 {
		t.Fatalf("expected assistant, parts user, tool and carrier user message, got %#v", wire)
	}
	if role := readStringField(wire[1], "role"); role != "user" {
		t.Fatalf("wire[1] role = %q, want the multimodal user message", role)
	}
	if role := readStringField(wire[2], "role"); role != "tool" {
		t.Fatalf("wire[2] role = %q, want tool", role)
	}
	carrier := wire[3]
	if role := readStringField(carrier, "role"); role != "user" {
		t.Fatalf("wire[3] role = %q, want user", role)
	}
	parts, _ := asSlice(carrier["content"])
	if len(parts) != 2 || readStringField(mustMap(t, parts[0]), "type") != "text" {
		t.Fatalf("carrier parts = %#v, want a text label plus the image", carrier["content"])
	}
	if got := readStringField(mustMap(t, parts[0]), "text"); got != toolResultImageText {
		t.Fatalf("carrier label = %q, want %q", got, toolResultImageText)
	}
}

// TestImageWithoutBytesKeepsPlaceholder: an unresolvable attachment must degrade
// to a placeholder rather than silently dropping the image.
func TestImageWithoutBytesKeepsPlaceholder(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image", "attachment": map[string]any{"attachmentId": "missing"}},
		}},
	}
	wire := SerializeMessages(messages, map[string]string{})
	parts, ok := asSlice(wire[0]["content"])
	if !ok || len(parts) != 1 {
		t.Fatalf("parts = %#v, want the placeholder part", wire[0]["content"])
	}
	if got := readStringField(mustMap(t, parts[0]), "text"); got != "[image unavailable]" {
		t.Fatalf("placeholder = %q", got)
	}
}

// TestOpenAIMultimodalPartsPassThrough: CPA hands images over as inline data
// URLs; SOLO accepts that shape verbatim.
func TestOpenAIMultimodalPartsPassThrough(t *testing.T) {
	url := "data:image/png;base64,CCCC"
	messages := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is this"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
		}},
	}
	if !HasImageContent(messages) {
		t.Fatal("HasImageContent must report true for an image_url part")
	}
	wire := SerializeMessages(messages, nil)
	parts, _ := asSlice(wire[0]["content"])
	if len(parts) != 2 {
		t.Fatalf("parts = %#v, want text + image_url", wire[0]["content"])
	}
	image := mustMap(t, parts[1])
	if readStringField(image, "type") != "image_url" {
		t.Fatalf("part = %#v, want image_url", image)
	}
	solo := TransformToSOLOBody(map[string]any{"model": "glm-5.2", "messages": toAnySlice(wire)}, "", "solo_agent")
	soloMessages, _ := asSlice(solo["messages"])
	outParts, _ := asSlice(mustMap(t, soloMessages[0])["content"])
	if len(outParts) != 2 {
		t.Fatalf("solo parts = %#v, want the image to survive the conversion", soloMessages[0])
	}
}

// TestTransformIsIdempotent: the executor re-runs the conversion on a payload
// that request.translate may already have converted.
func TestTransformIsIdempotent(t *testing.T) {
	root := map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "f", "parameters": map[string]any{"type": "object"},
		}}},
	}
	once := TransformToSOLOBody(root, "glm-5.2", "solo_work_lite")
	twice := TransformToSOLOBody(once, "glm-5.2", "solo_work_lite")
	first, _ := json.Marshal(once)
	second, _ := json.Marshal(twice)
	if string(first) != string(second) {
		t.Fatalf("conversion is not idempotent:\n once  = %s\n twice = %s", first, second)
	}
}

// TestStringParametersAreNotDoubleEncoded: an already-serialized parameter block
// must stay a single JSON string.
func TestStringParametersAreNotDoubleEncoded(t *testing.T) {
	root := map[string]any{
		"model": "glm-5.2",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "f", "parameters": `{"type":"object"}`,
		}}},
	}
	solo := TransformToSOLOBody(root, "glm-5.2", "")
	tools, _ := asSlice(solo["tools"])
	parameters := mustMap(t, tools[0])["function"].(map[string]any)["parameters"]
	if parameters != `{"type":"object"}` {
		t.Fatalf("parameters = %#v, want the original string", parameters)
	}
}

// TestToolChoiceNormalization is table-driven over every documented form.
func TestToolChoiceNormalization(t *testing.T) {
	cases := []struct {
		name       string
		choice     any
		wantChoice any
		wantTools  bool
	}{
		{name: "none string drops tools", choice: "none", wantChoice: nil, wantTools: false},
		{name: "auto object becomes string", choice: map[string]any{"type": "auto"}, wantChoice: "auto", wantTools: true},
		{name: "required object becomes string", choice: map[string]any{"type": "required"}, wantChoice: "required", wantTools: true},
		{name: "function object becomes its name", choice: map[string]any{"type": "function", "function": map[string]any{"name": "read"}}, wantChoice: "read", wantTools: true},
		{name: "none object drops tools", choice: map[string]any{"type": "none"}, wantChoice: nil, wantTools: false},
		{name: "unknown object form is dropped", choice: map[string]any{"type": "weird"}, wantChoice: nil, wantTools: true},
		{name: "auto string is kept", choice: "auto", wantChoice: "auto", wantTools: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := map[string]any{
				"tool_choice": testCase.choice,
				"tools":       []any{map[string]any{"type": "function"}},
			}
			NormalizeToolChoice(body)
			got, has := body["tool_choice"]
			if testCase.wantChoice == nil {
				if has {
					t.Fatalf("tool_choice = %#v, want it removed", got)
				}
			} else if got != testCase.wantChoice {
				t.Fatalf("tool_choice = %#v, want %#v", got, testCase.wantChoice)
			}
			if _, hasTools := body["tools"]; hasTools != testCase.wantTools {
				t.Fatalf("tools present = %v, want %v", hasTools, testCase.wantTools)
			}
		})
	}
}

// TestToolArgumentsNormalization covers the truncated-argument distinction.
func TestToolArgumentsNormalization(t *testing.T) {
	cases := []struct {
		raw           string
		wantNormal    string
		wantTruncated bool
	}{
		{raw: "", wantNormal: "{}", wantTruncated: false},
		{raw: `{"a":1}`, wantNormal: `{"a":1}`, wantTruncated: false},
		{raw: `{"a":`, wantNormal: "{}", wantTruncated: true},
		{raw: `[1,2]`, wantNormal: "{}", wantTruncated: false},
		{raw: `null`, wantNormal: "{}", wantTruncated: false},
	}
	for _, testCase := range cases {
		if got := NormalizeToolArguments(testCase.raw); got != testCase.wantNormal {
			t.Fatalf("NormalizeToolArguments(%q) = %q, want %q", testCase.raw, got, testCase.wantNormal)
		}
		if got := IsTruncatedArguments(testCase.raw); got != testCase.wantTruncated {
			t.Fatalf("IsTruncatedArguments(%q) = %v, want %v", testCase.raw, got, testCase.wantTruncated)
		}
	}
}

// TestTrimTraeHistory checks the three constraints: system survives, the oldest
// non-system round is dropped, and a tool_call/tool pair is never split.
func TestTrimTraeHistory(t *testing.T) {
	big := strings.Repeat("x", 400)
	messages := []map[string]any{
		{"role": "system", "content": big},
		{"role": "user", "content": "old question " + big},
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "f"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "old result " + big},
		{"role": "user", "content": "new question"},
	}
	trimmed := TrimTraeHistory(messages, 500)
	roles := []string{}
	for _, message := range trimmed {
		roles = append(roles, readStringField(message, "role"))
	}
	joined := strings.Join(roles, ",")
	if !strings.HasPrefix(joined, "system") {
		t.Fatalf("system message must survive, roles = %s", joined)
	}
	if strings.Contains(joined, "tool") {
		t.Fatalf("a tool result must never outlive its assistant tool_calls, roles = %s", joined)
	}
	if !strings.HasSuffix(joined, "user") {
		t.Fatalf("the newest message must survive, roles = %s", joined)
	}
}

// TestTrimTraeHistoryNoopUnderBudget keeps small requests untouched.
func TestTrimTraeHistoryNoopUnderBudget(t *testing.T) {
	messages := []map[string]any{{"role": "user", "content": "short"}}
	if got := TrimTraeHistory(messages, 1000); len(got) != 1 {
		t.Fatalf("trimmed = %#v, want the input untouched", got)
	}
}

// TestClampAndMaxModeFields covers the numeric wire helpers.
func TestClampAndMaxModeFields(t *testing.T) {
	if got := ClampMaxTokens(131072, 64000); got != 64000 {
		t.Fatalf("ClampMaxTokens = %d, want 64000", got)
	}
	if got := ClampMaxTokens(100, 64000); got != 100 {
		t.Fatalf("ClampMaxTokens must not raise a value, got %d", got)
	}
	if got := ClampMaxTokens(100, 0); got != 100 {
		t.Fatalf("a zero limit disables clamping, got %d", got)
	}
	fields := MaxModeFields(1000000, 384000)
	if fields["model_selection_strategy"] != "max" || fields["mode_type"] != 1 {
		t.Fatalf("max-mode fields = %#v", fields)
	}
	if fields["context_window_size"] != int64(1000000) || fields["max_tokens"] != int64(384000) {
		t.Fatalf("max-mode sizes = %#v", fields)
	}
	auto := mustMap(t, fields["model_auto_selection"])
	if auto["strategy"] != "max" {
		t.Fatalf("model_auto_selection = %#v", auto)
	}
	fallback := MaxModeFields(0, 0)
	if fallback["context_window_size"] != int64(MaxContextTokens) || fallback["max_tokens"] != int64(MaxOutputTokens) {
		t.Fatalf("fallback max-mode fields = %#v", fallback)
	}
}

// soloStream is a realistic SOLO SSE body: metadata, text, reasoning, a tool
// call, usage and done. The data line deliberately uses `data:{` without a space
// in one place, which occurs in the wild.
const soloStream = "event:metadata\ndata:{\"session_id\":\"abc\"}\n\n" +
	"event:output\ndata:{\"response\":\"Hel\",\"reasoning_content\":\"why\"}\n\n" +
	"event:timing_cost\ndata:{\"ttft\":10}\n\n" +
	"event:output\ndata:{\"response\":\"lo\"}\n\n" +
	"event:output\ndata:{\"tool_calls\":[{\"index\":0,\"id\":\"call_9\",\"function_call\":{\"name\":\"read\",\"arguments\":\"{\\\"p\\\":1}\",\"namespace\":\"x\",\"partial_arguments\":\"y\"}}]}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":11,\"completion_tokens\":22,\"reasoning_tokens\":3}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"tool_calls\"}\n\n"

// TestTranslateSOLOStream parses a full SOLO stream into OpenAI chunks.
func TestTranslateSOLOStream(t *testing.T) {
	frames, streamErr, sawEvent := TranslateSOLOStream([]byte(soloStream), "glm-5.2", "chatcmpl-test", 1)
	if streamErr != nil {
		t.Fatalf("unexpected stream error: %v", streamErr)
	}
	if !sawEvent {
		t.Fatal("sawEvent must be true")
	}
	joined := string(joinFrames(frames))
	if !strings.Contains(joined, `"content":"Hel"`) || !strings.Contains(joined, `"content":"lo"`) {
		t.Fatalf("content deltas missing from %s", joined)
	}
	if !strings.Contains(joined, `"reasoning_content":"why"`) {
		t.Fatalf("reasoning delta missing from %s", joined)
	}
	if !strings.Contains(joined, `"name":"read"`) {
		t.Fatalf("tool call name missing from %s", joined)
	}
	if strings.Contains(joined, "namespace") || strings.Contains(joined, "partial_arguments") {
		t.Fatalf("SOLO-only tool fields must be stripped: %s", joined)
	}
	if !strings.Contains(joined, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish reason missing from %s", joined)
	}
	if !strings.Contains(joined, `"prompt_tokens":11`) || !strings.Contains(joined, "reasoning_tokens") {
		t.Fatalf("usage missing from %s", joined)
	}
	if !strings.HasSuffix(joined, "data: [DONE]\n\n") {
		t.Fatalf("stream must terminate with [DONE]: %s", joined)
	}
	// The declared model must be echoed so clients see what they asked for.
	if !strings.Contains(joined, `"model":"glm-5.2"`) {
		t.Fatalf("model missing from %s", joined)
	}
	// The very first delta carries the assistant role.
	if !strings.Contains(joined, `"role":"assistant"`) {
		t.Fatalf("first delta must declare the assistant role: %s", joined)
	}
}

// TestTranslateSOLOStreamErrorFrames covers the in-stream error path, including
// the actionable 4001 hint and the quota classification.
func TestTranslateSOLOStreamErrorFrames(t *testing.T) {
	body := "event:output\ndata:{\"response\":\"partial\"}\n\n" +
		"event:error\ndata:{\"code\":4001,\"message\":\"We're sorry, the param is invalid.\"}\n\n"
	_, streamErr, sawEvent := TranslateSOLOStream([]byte(body), "glm-5.1", "id", 1)
	if streamErr == nil {
		t.Fatal("expected a stream error")
	}
	if !sawEvent {
		t.Fatal("sawEvent must be true once an event was parsed")
	}
	if streamErr.Code != 4001 {
		t.Fatalf("code = %d, want 4001", streamErr.Code)
	}
	if !strings.Contains(streamErr.Error(), "glm-5.1") || !strings.Contains(streamErr.Error(), "code=4001") {
		t.Fatalf("error text must name the model and the code: %s", streamErr.Error())
	}

	quota := "event:error\ndata:{\"code\":4008,\"message\":\"quota exceeded\"}\n\n"
	_, quotaErr, _ := TranslateSOLOStream([]byte(quota), "glm-5.2", "id", 1)
	if quotaErr == nil || quotaErr.Code != 4008 {
		t.Fatalf("quota error = %#v", quotaErr)
	}
	if strings.Contains(quotaErr.Error(), "不支持图片") {
		// no-op guard: only 4001 gets the hint
		t.Fatal("the 4001 hint must not be attached to other codes")
	}
}

// TestTranslateSOLOStreamEmptyResponse: no events at all is the retryable
// silent-EOF case, distinguishable from a legitimate empty answer.
func TestTranslateSOLOStreamEmptyResponse(t *testing.T) {
	frames, streamErr, sawEvent := TranslateSOLOStream([]byte(""), "glm-5.2", "id", 1)
	if streamErr != nil {
		t.Fatalf("unexpected error: %v", streamErr)
	}
	if sawEvent {
		t.Fatal("sawEvent must be false for an empty body")
	}
	if len(frames) == 0 {
		t.Fatal("even an empty stream must produce a terminal chunk and [DONE]")
	}
}

// TestParseSOLOLineTolerance covers the parser shapes, including the data line
// without a space and an unrecognised event name.
func TestParseSOLOLineTolerance(t *testing.T) {
	cases := []struct {
		name      string
		event     string
		data      string
		wantEvent string
		wantText  string
		wantOK    bool
	}{
		{name: "output", event: "output", data: `{"response":"hi"}`, wantEvent: "output", wantText: "hi", wantOK: true},
		{name: "empty data", event: "done", data: "", wantEvent: "done", wantOK: true},
		{name: "broken json", event: "output", data: "{oops", wantEvent: "output", wantOK: true},
		{name: "unknown event", event: "extra_info", data: `{"a":1}`, wantEvent: "extra_info", wantOK: true},
		{name: "missing event name", event: "  ", data: `{}`, wantOK: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			event, ok := ParseSOLOLine(testCase.event, testCase.data)
			if ok != testCase.wantOK {
				t.Fatalf("ok = %v, want %v", ok, testCase.wantOK)
			}
			if !ok {
				return
			}
			if event.Event != testCase.wantEvent {
				t.Fatalf("event = %q, want %q", event.Event, testCase.wantEvent)
			}
			if testCase.wantText != "" && event.Response != testCase.wantText {
				t.Fatalf("response = %q, want %q", event.Response, testCase.wantText)
			}
		})
	}
}

// TestAggregateSOLOToCompletion folds a stream into one chat.completion.
func TestAggregateSOLOToCompletion(t *testing.T) {
	completion, streamErr, sawEvent := AggregateSOLOToCompletion([]byte(soloStream), "glm-5.2", "chatcmpl-x", 7)
	if streamErr != nil || !sawEvent {
		t.Fatalf("streamErr = %v, sawEvent = %v", streamErr, sawEvent)
	}
	if completion["object"] != "chat.completion" {
		t.Fatalf("object = %#v", completion["object"])
	}
	choices, _ := asSlice(completion["choices"])
	message := mustMap(t, mustMap(t, choices[0])["message"])
	if message["content"] != "Hello" {
		t.Fatalf("content = %#v, want Hello", message["content"])
	}
	if message["reasoning_content"] != "why" {
		t.Fatalf("reasoning_content = %#v", message["reasoning_content"])
	}
	calls, _ := asSlice(message["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v, want 1", message["tool_calls"])
	}
	call := mustMap(t, calls[0])
	if readStringField(call, "id") != "call_9" {
		t.Fatalf("tool call id = %#v", call["id"])
	}
	usage := mustMap(t, completion["usage"])
	if usage["prompt_tokens"] != int64(11) || usage["completion_tokens"] != int64(22) {
		t.Fatalf("usage = %#v", usage)
	}
}

// TestScannerFlushesUnterminatedEvent: the last event may arrive without its
// trailing newline, without its blank line, or both.
func TestScannerFlushesUnterminatedEvent(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "no trailing newline", body: "event:output\ndata:{\"response\":\"tail\"}"},
		{name: "no blank line", body: "event:output\ndata:{\"response\":\"tail\"}\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scanner := &SOLOScanner{}
			if events := scanner.Feed([]byte(testCase.body)); len(events) != 0 {
				t.Fatalf("events before flush = %#v", events)
			}
			tail, ok := scanner.Flush()
			if !ok || tail.Response != "tail" {
				t.Fatalf("flushed = %#v ok=%v", tail, ok)
			}
			if _, again := scanner.Flush(); again {
				t.Fatal("a second flush must not repeat the event")
			}
		})
	}
}

// TestTranslateRoutes exercises request.translate and response.translate as the
// host would call them.
func TestTranslateRoutes(t *testing.T) {
	requestBody := map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "f", "parameters": map[string]any{"type": "object"},
		}}},
	}
	encoded, _ := json.Marshal(requestBody)
	rawRequest, _ := json.Marshal(pluginapi.RequestTransformRequest{FromFormat: "chat-completions", ToFormat: "solo", Model: "glm-5.2", Body: encoded})
	value, errTranslate := handleRequestTranslate(nil, rawRequest)
	if errTranslate != nil {
		t.Fatalf("request.translate: %v", errTranslate)
	}
	response, ok := value.(pluginapi.PayloadResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", value)
	}
	var solo map[string]any
	if err := json.Unmarshal(response.Body, &solo); err != nil {
		t.Fatalf("translated body is not JSON: %v", err)
	}
	if readStringField(solo, "config_name") != "glm-5.2" || readStringField(solo, "function") == "" {
		t.Fatalf("translated body = %#v", solo)
	}
	messages, _ := asSlice(solo["messages"])
	if len(messages) != 3 {
		t.Fatalf("translated messages = %#v, want 3", solo["messages"])
	}

	// An already-SOLO body must pass through untouched.
	soloBody, _ := json.Marshal(solo)
	rawSOLO, _ := json.Marshal(pluginapi.RequestTransformRequest{Model: "glm-5.2", Body: soloBody})
	valueSOLO, errSOLO := handleRequestTranslate(nil, rawSOLO)
	if errSOLO != nil {
		t.Fatalf("request.translate on a SOLO body: %v", errSOLO)
	}
	if got := valueSOLO.(pluginapi.PayloadResponse).Body; string(got) != string(soloBody) {
		t.Fatalf("SOLO body must pass through: %s", got)
	}

	// response.translate converts a SOLO SSE body and leaves OpenAI alone.
	rawResponse, _ := json.Marshal(pluginapi.ResponseTransformRequest{Model: "glm-5.2", Stream: true, Body: []byte(soloStream)})
	valueResponse, errResponse := handleResponseTranslate(nil, rawResponse)
	if errResponse != nil {
		t.Fatalf("response.translate: %v", errResponse)
	}
	converted := valueResponse.(pluginapi.PayloadResponse)
	if !strings.Contains(string(converted.Body), "chat.completion.chunk") {
		t.Fatalf("converted body = %s", converted.Body)
	}
	openAI := []byte(`{"choices":[]}`)
	rawOpenAI, _ := json.Marshal(pluginapi.ResponseTransformRequest{Stream: false, Body: openAI})
	valueOpenAI, errOpenAI := handleResponseTranslate(nil, rawOpenAI)
	if errOpenAI != nil {
		t.Fatalf("response.translate on an OpenAI body: %v", errOpenAI)
	}
	if string(valueOpenAI.(pluginapi.PayloadResponse).Body) != string(openAI) {
		t.Fatal("an OpenAI body must pass through response.translate")
	}

	// A non-streaming SOLO body folds into one chat.completion.
	rawAggregate, _ := json.Marshal(pluginapi.ResponseTransformRequest{Model: "glm-5.2", Body: []byte(soloStream)})
	valueAggregate, errAggregate := handleResponseTranslate(nil, rawAggregate)
	if errAggregate != nil {
		t.Fatalf("response.translate (aggregate): %v", errAggregate)
	}
	var completion map[string]any
	if err := json.Unmarshal(valueAggregate.(pluginapi.PayloadResponse).Body, &completion); err != nil {
		t.Fatalf("aggregated body is not JSON: %v", err)
	}
	if completion["object"] != "chat.completion" {
		t.Fatalf("aggregated object = %#v", completion["object"])
	}
}

// joinFrames concatenates encoded SSE frames.
func joinFrames(frames [][]byte) []byte {
	out := []byte{}
	for _, frame := range frames {
		out = append(out, frame...)
	}
	return out
}

// mustMap asserts an `any` is a JSON object.
func mustMap(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := asMap(value)
	if !ok {
		t.Fatalf("value %#v is not a JSON object", value)
	}
	return object
}
