package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// decodeBody decodes a built request body for assertions.
func decodeBody(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(payload, &decoded); errUnmarshal != nil {
		t.Fatalf("decode body: %v (%s)", errUnmarshal, payload)
	}
	return decoded
}

// TestBuildChatBodyIsAWhitelist pins §5.2: no `stream_options`, `user`, `n`,
// `top_p`, `response_format` or `tool_choice` is ever sent, `stream` is always
// true, and the optional fields only appear when the client supplied them.
func TestBuildChatBodyIsAWhitelist(t *testing.T) {
	payload := []byte(`{
	  "model": "body-model",
	  "messages": [{"role": "user", "content": "hi", "name": "u"}],
	  "stream": false,
	  "stream_options": {"include_usage": true},
	  "user": "someone",
	  "n": 2,
	  "top_p": 0.9,
	  "response_format": {"type": "json_object"},
	  "tool_choice": "auto",
	  "temperature": 0.3,
	  "max_tokens": 100,
	  "stop": ["END"],
	  "reasoning_effort": "banana"
	}`)
	body, model, errBuild := buildChatBody(payload, "routed-model", DefaultConfig())
	if errBuild != nil {
		t.Fatalf("buildChatBody: %v", errBuild)
	}
	if model != "routed-model" {
		t.Fatalf("model = %q, want the routed model", model)
	}
	decoded := decodeBody(t, body)
	for _, forbidden := range []string{"stream_options", "user", "n", "top_p", "response_format", "tool_choice"} {
		if _, present := decoded[forbidden]; present {
			t.Errorf("the body must not carry %q: %s", forbidden, body)
		}
	}
	if decoded["stream"] != true {
		t.Errorf("stream = %v, want true: SSE is the only upstream mode", decoded["stream"])
	}
	if decoded["model"] != "routed-model" {
		t.Errorf("model = %v", decoded["model"])
	}
	if decoded["temperature"] != 0.3 {
		t.Errorf("temperature = %v", decoded["temperature"])
	}
	if decoded["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v", decoded["max_tokens"])
	}
	// reasoning_effort is passed through verbatim with no whitelist.
	if decoded["reasoning_effort"] != "banana" {
		t.Errorf("reasoning_effort = %v", decoded["reasoning_effort"])
	}
	if _, okStop := decoded["stop"].([]any); !okStop {
		t.Errorf("stop = %v", decoded["stop"])
	}
	messages, okMessages := decoded["messages"].([]any)
	if !okMessages || len(messages) != 1 {
		t.Fatalf("messages = %v", decoded["messages"])
	}
	message := messages[0].(map[string]any)
	if message["name"] != "u" {
		t.Errorf("the message whitelist dropped a known field: %v", message)
	}
}

// TestBuildChatBodyModelFallback pins that the payload's model is used when the
// executor request carries none.
func TestBuildChatBodyModelFallback(t *testing.T) {
	_, model, errBuild := buildChatBody([]byte(`{"model":"from-body","messages":[{"role":"user","content":"x"}]}`), "", DefaultConfig())
	if errBuild != nil {
		t.Fatalf("buildChatBody: %v", errBuild)
	}
	if model != "from-body" {
		t.Fatalf("model = %q", model)
	}
}

// TestBuildChatBodyRejectsUnusableRequests pins the request-side failures.
func TestBuildChatBodyRejectsUnusableRequests(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		model   string
	}{
		{"empty body", ``, "m"},
		{"not json", `nope`, "m"},
		{"no model", `{"messages":[{"role":"user","content":"x"}]}`, ""},
		{"no messages", `{"model":"m","messages":[]}`, "m"},
		{"messages of the wrong shape", `{"model":"m","messages":"nope"}`, "m"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, errBuild := buildChatBody([]byte(testCase.payload), testCase.model, DefaultConfig())
			if errBuild == nil {
				t.Fatal("expected a request error")
			}
			if status := statusOf(errBuild, 0); status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", status)
			}
		})
	}
}

// TestClampMaxTokens pins `clampClineMaxTokens` (`cline-adapter.ts:75-80`).
func TestClampMaxTokens(t *testing.T) {
	ceiling := Config{MaxOutputTokens: MaxOutputTokensCeiling}
	cases := []struct {
		name     string
		value    any
		cfg      Config
		want     int
		wantSend bool
	}{
		{"above the ceiling", float64(1e9), ceiling, MaxOutputTokensCeiling, true},
		{"fraction is floored", 10.9, ceiling, 10, true},
		{"zero is dropped", float64(0), ceiling, 0, false},
		{"negative is dropped", float64(-5), ceiling, 0, false},
		{"absent without a default", nil, ceiling, 0, false},
		{"absent with a configured default", nil, Config{DefaultMaxTokens: 4096, MaxOutputTokens: MaxOutputTokensCeiling}, 4096, true},
		{"configured default is clamped too", nil, Config{DefaultMaxTokens: 1_000_000, MaxOutputTokens: MaxOutputTokensCeiling}, MaxOutputTokensCeiling, true},
		{"non-numeric is dropped", "lots", ceiling, 0, false},
		{"numeric string is accepted", "2048", ceiling, 2048, true},
		{"a zero ceiling falls back to the constant", float64(2_000_000), Config{MaxOutputTokens: 0}, MaxOutputTokensCeiling, true},
		{"configured ceiling is honoured", float64(999_999), Config{MaxOutputTokens: 65_536}, 65_536, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			value, send := clampMaxTokens(testCase.value, testCase.cfg)
			if send != testCase.wantSend {
				t.Fatalf("send = %v, want %v", send, testCase.wantSend)
			}
			if send && value != testCase.want {
				t.Fatalf("value = %d, want %d", value, testCase.want)
			}
		})
	}
}

// TestSanitizeToolParameters pins the measured upstream 400 fix
// (`cline-adapter.ts:110-123`).
func TestSanitizeToolParameters(t *testing.T) {
	input := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "read_file",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"permission": map[string]any{"enum": []any{"read", "  ", "write", "", 3, true}},
					"onlyEmpty":  map[string]any{"enum": []any{"", "   "}},
					"items": map[string]any{
						"type":  "array",
						"items": map[string]any{"enum": []any{"a", ""}},
					},
					"notAnEnumArray": map[string]any{"enum": "keep-me"},
				},
			},
		},
	}}
	sanitized := sanitizeToolParameters(input)
	encoded, errMarshal := json.Marshal(sanitized)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	text := string(encoded)

	if strings.Contains(text, `"  "`) || strings.Contains(text, `""`) {
		t.Errorf("whitespace-only enum members must be dropped: %s", text)
	}
	if !strings.Contains(text, `"notAnEnumArray":{"enum":"keep-me"}`) {
		t.Errorf("a non-array enum must be left alone: %s", text)
	}
	// Walk to the property of interest.
	properties := sanitized.([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	permission := properties["permission"].(map[string]any)
	enum, okEnum := permission["enum"].([]any)
	if !okEnum {
		t.Fatalf("permission enum = %v", permission["enum"])
	}
	if len(enum) != 4 {
		t.Fatalf("enum = %v, want the 4 usable members (strings + number + boolean)", enum)
	}
	if _, present := properties["onlyEmpty"].(map[string]any)["enum"]; present {
		t.Error("an enum with nothing left must be dropped entirely")
	}
	items := properties["items"].(map[string]any)["items"].(map[string]any)
	if got := items["enum"].([]any); len(got) != 1 || got[0] != "a" {
		t.Errorf("nested items enum = %v", got)
	}
}

// TestNormalizeToolArguments pins `normalizeToolArguments` (`sse.ts:291-304`).
func TestNormalizeToolArguments(t *testing.T) {
	cases := map[string]string{
		"":              "{}",
		"   ":           "{}",
		"not json":      "{}",
		`[1,2]`:         "{}",
		`"text"`:        "{}",
		`{"a":1}`:       `{"a":1}`,
		`{"a":{"b":2}}`: `{"a":{"b":2}}`,
	}
	for input, want := range cases {
		if got := normalizeToolArguments(input); got != want {
			t.Errorf("normalizeToolArguments(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestNormalizeMessagesAssistantRules pins §5.3: tool arguments are normalised
// and `content` becomes null when the text is empty and calls exist.
func TestNormalizeMessagesAssistantRules(t *testing.T) {
	messages := normalizeMessages([]any{
		map[string]any{
			"role":    "assistant",
			"content": "",
			"unknown": "dropped",
			"tool_calls": []any{map[string]any{
				"id":       "call_1",
				"type":     "function",
				"function": map[string]any{"name": "f", "arguments": ""},
			}},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
	})
	assistant := messages[0].(map[string]any)
	if assistant["content"] != nil {
		t.Errorf("content = %v, want null when tool calls exist and the text is empty", assistant["content"])
	}
	if _, present := assistant["unknown"]; present {
		t.Error("unknown message keys must be dropped")
	}
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["arguments"] != "{}" {
		t.Errorf("arguments = %v", call["function"])
	}
	toolMessage := messages[1].(map[string]any)
	if toolMessage["tool_call_id"] != "call_1" || toolMessage["content"] != "ok" {
		t.Errorf("a plain tool message must survive: %v", toolMessage)
	}
}

// TestNormalizeMessagesDropsDeveloper pins §5.3: `role: "developer"` is an alias
// the older upstream providers behind this catalogue do not accept.
func TestNormalizeMessagesDropsDeveloper(t *testing.T) {
	messages := normalizeMessages([]any{
		map[string]any{"role": "developer", "content": "instructions"},
		map[string]any{"role": "system", "content": "system"},
		map[string]any{"role": "user", "content": "hi"},
	})
	if len(messages) != 2 {
		t.Fatalf("messages = %+v", messages)
	}
	for _, message := range messages {
		if message.(map[string]any)["role"] == "developer" {
			t.Fatalf("a developer message survived: %+v", message)
		}
	}
}

// TestStreamFrames pins the line rules of `openai-compat.ts:474-484`.
func TestStreamFrames(t *testing.T) {
	body := "event: ping\n" +
		": comment\n" +
		"data: {\"a\":1}\n" +
		"\n" +
		"data:{\"b\":2}\r\n" +
		"data:  \n" +
		"data: [DONE]\n" +
		"data: {\"after\":\"done\"}\n"
	frames, sawDone := streamFrames([]byte(body))
	if !sawDone {
		t.Fatal("[DONE] must terminate the stream")
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %v", frames)
	}
	if frames[0] != `{"a":1}` || frames[1] != `{"b":2}` {
		t.Fatalf("frames = %v", frames)
	}

	// A final frame without a trailing newline is still processed.
	frames, sawDone = streamFrames([]byte("data: {\"last\":true}"))
	if sawDone || len(frames) != 1 || frames[0] != `{"last":true}` {
		t.Fatalf("frames = %v, sawDone = %v", frames, sawDone)
	}
}

// TestParseChatStreamNormalisation pins the frame handling of §5.5: only
// `data:` lines, only choices[0], `reasoning` mapped to `reasoning_content`,
// null deltas tolerated, usage preserved verbatim.
func TestParseChatStreamNormalisation(t *testing.T) {
	body := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":null,\"reasoning\":\"think\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n" +
		"data: [DONE]\n\n"
	stream, errParse := parseChatStream([]byte(body))
	if errParse != nil {
		t.Fatalf("parseChatStream: %v", errParse)
	}
	if !stream.sawDone {
		t.Error("[DONE] was not recorded")
	}
	if len(stream.chunks) != 4 {
		t.Fatalf("chunks = %d", len(stream.chunks))
	}
	if got := stream.chunks[1].Choices[0].Delta.ReasoningContent; got != "think" {
		t.Errorf("reasoning = %q, want it mapped to reasoning_content", got)
	}
	if got := stream.chunks[1].Choices[0].Delta.Content; got != "" {
		t.Errorf("a null content must not become text: %q", got)
	}
	if string(stream.usageRaw) == "" {
		t.Fatal("usage must be preserved verbatim")
	}
	if stream.usage == nil || stream.usage.InputTokens != 6 || stream.usage.OutputTokens != 2 || stream.usage.CacheReadTokens != 4 {
		t.Fatalf("derived usage = %+v", stream.usage)
	}
	encoded, errMarshal := json.Marshal(stream.chunks[1])
	if errMarshal != nil {
		t.Fatalf("marshal chunk: %v", errMarshal)
	}
	if strings.Contains(string(encoded), `"reasoning"`) {
		t.Errorf("the outbound chunk must not carry the inbound field name: %s", encoded)
	}
}

// TestParseChatStreamFrameErrors pins the three error shapes of §5.5.
func TestParseChatStreamFrameErrors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{"error object", `data: {"error":{"message":"upstream exploded"}}`, "upstream exploded"},
		{"error object without a message", `data: {"error":{}}`, "unknown error"},
		{"code and message", `data: {"code":"invalid_api_key","message":"bad key"}`, "invalid_api_key"},
		{"gateway frame", `data: {"message":"gateway blew up","statusCodeValue":502}`, "gateway blew up"},
		{"gateway frame with a stack trace", `data: {"message":"kaboom","stackTrace":["x"]}`, "kaboom"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, errParse := parseChatStream([]byte(testCase.body))
			if errParse == nil {
				t.Fatal("expected a classified error")
			}
			if !strings.Contains(errParse.Error(), testCase.wantSub) {
				t.Fatalf("error = %q, want it to contain %q", errParse.Error(), testCase.wantSub)
			}
			if status := statusOf(errParse, 0); status != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", status)
			}
		})
	}
}

// TestParseChatStreamSkipsUnusableFrames pins the silent-skip rule for a frame
// that is not JSON, and the hard failure for a body that is not SSE at all.
func TestParseChatStreamSkipsUnusableFrames(t *testing.T) {
	stream, errParse := parseChatStream([]byte("data: {not json}\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n"))
	if errParse != nil {
		t.Fatalf("an unparseable frame must be skipped, got %v", errParse)
	}
	if len(stream.chunks) != 1 {
		t.Fatalf("chunks = %d", len(stream.chunks))
	}

	_, errParse = parseChatStream([]byte("<!DOCTYPE html><html>502 Bad Gateway</html>"))
	if errParse == nil {
		t.Fatal("a non-SSE body must fail")
	}
	if !strings.Contains(errParse.Error(), "没有任何 data: 帧") {
		t.Fatalf("error = %q", errParse.Error())
	}
	if !strings.Contains(errParse.Error(), "502 Bad Gateway") {
		t.Errorf("the raw snippet must be included: %q", errParse.Error())
	}

	if _, errParse = parseChatStream(nil); errParse == nil {
		t.Fatal("an empty body must fail")
	}
}

// TestMessageContentFallback pins `message.content` being used only while no
// delta content has been seen (`openai-compat.ts:595-628`).
func TestMessageContentFallback(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"message\":{\"content\":\"from message\"}}]}\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"from delta\"}}]}\n" +
		"data: {\"choices\":[{\"index\":0,\"message\":{\"content\":\"ignored\"}}]}\n"
	stream, errParse := parseChatStream([]byte(body))
	if errParse != nil {
		t.Fatalf("parseChatStream: %v", errParse)
	}
	if len(stream.chunks) != 3 {
		t.Fatalf("chunks = %d", len(stream.chunks))
	}
	if got := stream.chunks[0].Choices[0].Delta.Content; got != "from message" {
		t.Errorf("first frame content = %q", got)
	}
	if got := stream.chunks[2].Choices[0].Delta.Content; got != "" {
		t.Errorf("the message fallback must stop after delta content: %q", got)
	}
}

// TestFoldToolCalls pins the tool-call merge rules of §5.5.
func TestFoldToolCalls(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	stream, errParse := parseChatStream([]byte(body))
	if errParse != nil {
		t.Fatalf("parseChatStream: %v", errParse)
	}
	merged := stream.fold()
	if merged.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", merged.FinishReason)
	}
	if len(merged.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v (the nameless one must be dropped)", merged.ToolCalls)
	}
	call := merged.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "read" {
		t.Fatalf("call = %+v", call)
	}
	if call.Function.Arguments != `{"path":"a"}` {
		t.Fatalf("arguments = %q", call.Function.Arguments)
	}
	if !merged.DroppedUnnamed {
		t.Error("the dropped nameless call must be recorded")
	}
	if merged.Announced != 1 {
		t.Errorf("announced = %d", merged.Announced)
	}
}

// TestFoldSynthesisesToolCallIdentity pins the two synthesis rules: an id and
// the `function` type.
func TestFoldSynthesisesToolCallIdentity(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":2,\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]}}]}\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\ndata: [DONE]\n"
	stream, errParse := parseChatStream([]byte(body))
	if errParse != nil {
		t.Fatalf("parseChatStream: %v", errParse)
	}
	merged := stream.fold()
	if len(merged.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", merged.ToolCalls)
	}
	call := merged.ToolCalls[0]
	if call.ID != "call_2" {
		t.Errorf("id = %q, want a synthesised call_<index>", call.ID)
	}
	if call.Type != "function" {
		t.Errorf("type = %q", call.Type)
	}
	if call.Function.Arguments != "{}" {
		t.Errorf("an empty argument fragment must become {}: %q", call.Function.Arguments)
	}
}

// TestFoldFinishReasonRules pins the finish-reason table of
// `openai-compat.ts:889-965`.
func TestFoldFinishReasonRules(t *testing.T) {
	content := `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n"
	cases := []struct {
		name string
		body string
		want string
	}{
		{"explicit length", content + `data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\ndata: [DONE]\n", "length"},
		{"explicit stop", content + `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\ndata: [DONE]\n", "stop"},
		{"explicit tool_calls", content + `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\ndata: [DONE]\n", "tool_calls"},
		{"announced tool call without a finish reason", content + `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{}"}}]}}]}`, "length"},
		{"no finish reason and no done marker", content, "length"},
		{"truncated arguments", content + `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{\"a\":"}}]}}]}` + "\ndata: [DONE]\n", "length"},
		{"tool call announced and finished", content + `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{}"}}]}}]}` + "\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\ndata: [DONE]\n", "tool_calls"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stream, errParse := parseChatStream([]byte(testCase.body))
			if errParse != nil {
				t.Fatalf("parseChatStream: %v", errParse)
			}
			if got := stream.fold().FinishReason; got != testCase.want {
				t.Fatalf("finish reason = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestStreamChunksAndEmptyResponse pins the streaming output and the
// EMPTY_RESPONSE rule (`sse.ts:132-141`).
func TestStreamChunksAndEmptyResponse(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n"
	stream, errParse := parseChatStream([]byte(body))
	if errParse != nil {
		t.Fatalf("parseChatStream: %v", errParse)
	}
	chunks, errChunks := stream.streamChunks()
	if errChunks != nil {
		t.Fatalf("streamChunks: %v", errChunks)
	}
	// Two decoded frames plus the appended [DONE].
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	for _, chunk := range chunks {
		if !strings.HasPrefix(string(chunk.Payload), "data: ") || !strings.HasSuffix(string(chunk.Payload), "\n\n") {
			t.Fatalf("frame = %q", chunk.Payload)
		}
	}
	if last := string(chunks[len(chunks)-1].Payload); last != "data: [DONE]\n\n" {
		t.Fatalf("last frame = %q", last)
	}

	// A stream that ends with stop but produced nothing is an error.
	empty, errParse := parseChatStream([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\ndata: [DONE]\n"))
	if errParse != nil {
		t.Fatalf("parseChatStream: %v", errParse)
	}
	if _, errChunks = empty.streamChunks(); errChunks == nil {
		t.Fatal("an empty completion must be reported as an error")
	}
	if status := statusOf(errChunks, 0); status != http.StatusBadGateway {
		t.Errorf("status = %d", status)
	}
	// Whitespace-only reasoning does not count as content.
	blank, errParse := parseChatStream([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"   \"},\"finish_reason\":\"stop\"}]}\ndata: [DONE]\n"))
	if errParse != nil {
		t.Fatalf("parseChatStream: %v", errParse)
	}
	if _, errChunks = blank.streamChunks(); errChunks == nil {
		t.Error("a whitespace-only reasoning block must not count as content")
	}
}

// TestChatStreamCompletion pins the non-streaming fold.
func TestChatStreamCompletion(t *testing.T) {
	body := "data: {\"id\":\"c1\",\"created\":7,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"he\"}}]}\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\",\"reasoning\":\"think\"}}]}\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"prompt_cache_hit_tokens\":2}}\n" +
		"data: [DONE]\n"
	stream, errParse := parseChatStream([]byte(body))
	if errParse != nil {
		t.Fatalf("parseChatStream: %v", errParse)
	}
	completion, merged, errCompletion := stream.completion("fallback-model")
	if errCompletion != nil {
		t.Fatalf("completion: %v", errCompletion)
	}
	if completion.ID != "c1" || completion.Model != "m" || completion.Created != 7 {
		t.Fatalf("completion metadata = %+v", completion)
	}
	if merged.Content != "hello" || merged.Reasoning != "think" {
		t.Fatalf("merged = %+v", merged)
	}
	if merged.FinishReason != "stop" {
		t.Fatalf("finish = %q", merged.FinishReason)
	}
	// `prompt_tokens_details.cached_tokens` is absent here, so nothing is
	// subtracted from prompt_tokens; the cache-read counter falls back to
	// `prompt_cache_hit_tokens`.
	if stream.usage == nil || stream.usage.CacheReadTokens != 2 || stream.usage.InputTokens != 5 {
		t.Fatalf("usage = %+v", stream.usage)
	}
	choice := completion.Choices[0]
	if choice.Message.Content == nil || *choice.Message.Content != "hello" || choice.Message.ReasoningContent != "think" {
		t.Fatalf("message = %+v", choice.Message)
	}
	if completion.Usage == nil {
		t.Error("the aggregated completion must keep the upstream usage block")
	}
	if choice.FinishReason == nil || *choice.FinishReason != "stop" {
		t.Fatalf("finish reason = %v", choice.FinishReason)
	}
}

// TestChatStreamCompletionNullContent pins the `content: null` rule for a
// tool-call-only answer.
func TestChatStreamCompletionNullContent(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\ndata: [DONE]\n"
	stream, errParse := parseChatStream([]byte(body))
	if errParse != nil {
		t.Fatalf("parseChatStream: %v", errParse)
	}
	completion, _, errCompletion := stream.completion("m")
	if errCompletion != nil {
		t.Fatalf("completion: %v", errCompletion)
	}
	if completion.Choices[0].Message.Content != nil {
		t.Fatalf("content = %v, want null", completion.Choices[0].Message.Content)
	}
	encoded, errMarshal := json.Marshal(completion)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if !strings.Contains(string(encoded), `"content":null`) {
		t.Errorf("completion = %s", encoded)
	}
}

// TestDeriveUsage pins the accounting of `openai-compat.ts:730-748`.
func TestDeriveUsage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *usageCounters
	}{
		{"no usage block", ``, nil},
		{"prompt details win", `{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"prompt_cache_hit_tokens":9,"completion_tokens_details":{"reasoning_tokens":2}}`,
			&usageCounters{InputTokens: 6, OutputTokens: 3, CacheReadTokens: 4, ReasoningTokens: 2}},
		{"cache hit fallback", `{"prompt_tokens":10,"completion_tokens":3,"prompt_cache_hit_tokens":7}`,
			&usageCounters{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 7}},
		{"negative input is clamped", `{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":5}}`,
			&usageCounters{InputTokens: 0, CacheReadTokens: 5}},
		{"zero reasoning is omitted", `{"prompt_tokens":1,"completion_tokens":1,"completion_tokens_details":{"reasoning_tokens":0}}`,
			&usageCounters{InputTokens: 1, OutputTokens: 1}},
		{"garbage", `[1]`, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var raw json.RawMessage
			if testCase.raw != "" {
				raw = json.RawMessage(testCase.raw)
			}
			got := deriveUsage(raw)
			if testCase.want == nil {
				if got != nil {
					t.Fatalf("usage = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("usage = nil")
			}
			if *got != *testCase.want {
				t.Fatalf("usage = %+v, want %+v", *got, *testCase.want)
			}
		})
	}
}

// TestIsRegionForbidden pins the four lowercased markers (`cline-adapter.ts:641-646`).
func TestIsRegionForbidden(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"measured body", 403, `{"error":"access forbidden: claude-x is not available in your region","success":false}`, true},
		{"region not supported", 403, `{"error":"Region not supported"}`, true},
		{"country", 403, `{"error":"This model is not available in your country"}`, true},
		{"403 without a marker is not a region block", 403, `{"error":"forbidden"}`, false},
		{"401 with the same text is not a region block", 401, `{"error":"not available in your region"}`, false},
		{"403 without a marker", 403, `{"error":"quota exceeded"}`, false},
		{"200 is never a region block", 200, `{"error":"not available in your region"}`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isRegionForbidden(testCase.status, []byte(testCase.body)); got != testCase.want {
				t.Fatalf("isRegionForbidden = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestUpstreamErrorClassification is the status taxonomy of spec §8.
func TestUpstreamErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantCode   string
	}{
		{"region 403 is PERMISSION_DENIED", 403, `{"error":"access forbidden: m is not available in your region"}`, 403, "region_forbidden"},
		{"plain 403 is AUTH", 403, `{"error":"forbidden"}`, 401, "auth"},
		{"401 is AUTH", 401, `{"error":"Unauthorized: Please make sure you're using the latest version of Cline"}`, 401, "auth"},
		{"429 is RATE_LIMIT", 429, `{"error":"too many requests"}`, 429, "rate_limited"},
		{"402 is quota", 402, `{"error":"payment required"}`, 402, "quota_exceeded"},
		{"400 is the request's fault", 400, `{"error":"bad request"}`, 400, "invalid_request"},
		{"4xx with a quota marker rotates", 400, `{"error":"insufficient balance"}`, 402, "quota_exceeded"},
		{"a Chinese quota marker rotates too", 400, `{"error":"余额不足"}`, 402, "quota_exceeded"},
		{"5xx is not rotated", 500, `{"error":"insufficient capacity, exceeded limits"}`, 502, "upstream_error"},
		{"unknown status", 418, `{"error":"teapot"}`, 502, "upstream_error"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			errUpstream := upstreamError(&pluginapi.HTTPResponse{StatusCode: testCase.status, Body: []byte(testCase.body)})
			if errUpstream == nil {
				t.Fatal("expected an error")
			}
			if status := statusOf(errUpstream, 0); status != testCase.wantStatus {
				t.Fatalf("status = %d, want %d (%v)", status, testCase.wantStatus, errUpstream)
			}
			envelope := envelopeOf(errUpstream)
			if envelope == nil || envelope.Code != testCase.wantCode {
				t.Fatalf("code = %v, want %q", envelope, testCase.wantCode)
			}
		})
	}
}

// TestUpstreamErrorDetail pins the body reader used by the region-403 message.
func TestUpstreamErrorDetail(t *testing.T) {
	cases := map[string]string{
		`{"error":"access forbidden: x is not available in your region"}`: "access forbidden: x is not available in your region",
		`{"error":{"message":"nested message"}}`:                          "nested message",
		`{"message":"top level"}`:                                         "top level",
		`<html>502</html>`:                                                "<html>502</html>",
	}
	for body, want := range cases {
		if got := upstreamErrorDetail([]byte(body)); got != want {
			t.Errorf("upstreamErrorDetail(%q) = %q, want %q", body, got, want)
		}
	}
}

// TestChatHeadersOverrideAccept pins the merge order of
// `cline-adapter.ts:546-550`.
func TestChatHeadersOverrideAccept(t *testing.T) {
	header := chatHeaders(&Credential{AccessToken: "eyJ"})
	if got := header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q", got)
	}
	if got := header.Get("Authorization"); got != "Bearer workos:eyJ" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	for _, pair := range clientHeaderPairs {
		if got := header.Get(pair[0]); got != pair[1] {
			t.Errorf("client header %s = %q", pair[0], got)
		}
	}
	// The JSON header set used by the balance and listing calls keeps the JSON
	// accept.
	if got := clineHeaders(&Credential{AccessToken: "eyJ"}).Get("Accept"); got != "application/json" {
		t.Errorf("clineHeaders Accept = %q", got)
	}
}

// envelopeOf extracts the classified failure envelope for code assertions.
func envelopeOf(err error) *abiboot.EnvelopeError {
	var envelope *abiboot.EnvelopeError
	if errors.As(err, &envelope) {
		return envelope
	}
	return nil
}
