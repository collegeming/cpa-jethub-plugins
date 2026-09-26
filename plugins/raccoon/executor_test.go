package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Executor behaviour: history hygiene, the tool-call gating that keeps a session
// alive, and the finish-reason mapping that decides whether the harness retries.

// sseBody frames payloads as an upstream SSE body.
func sseBody(frames ...string) []byte {
	var builder strings.Builder
	for _, frame := range frames {
		builder.WriteString("data: " + frame + "\n\n")
	}
	return []byte(builder.String())
}

// decodeChunks parses emitted frames, dropping the [DONE] terminator.
func decodeChunks(t *testing.T, chunks []pluginapi.ExecutorStreamChunk) []outChunk {
	t.Helper()
	var out []outChunk
	for _, chunk := range chunks {
		scanner := &sse.Scanner{}
		for _, payload := range scanner.Feed(chunk.Payload) {
			if strings.TrimSpace(payload) == sse.Done {
				continue
			}
			var decoded outChunk
			if errUnmarshal := json.Unmarshal([]byte(payload), &decoded); errUnmarshal != nil {
				t.Fatalf("decode emitted frame %q: %v", payload, errUnmarshal)
			}
			out = append(out, decoded)
		}
	}
	return out
}

// finalReason returns the rewritten finish reason of a stream.
func finalReason(t *testing.T, chunks []pluginapi.ExecutorStreamChunk) string {
	t.Helper()
	decoded := decodeChunks(t, chunks)
	for index := len(decoded) - 1; index >= 0; index-- {
		for _, choice := range decoded[index].Choices {
			if choice.FinishReason != nil {
				return *choice.FinishReason
			}
		}
	}
	t.Fatal("no emitted frame carried a finish_reason")
	return ""
}

// toolArguments concatenates the argument fragments the stream emitted.
func toolArguments(t *testing.T, chunks []pluginapi.ExecutorStreamChunk) string {
	t.Helper()
	var builder strings.Builder
	for _, chunk := range decodeChunks(t, chunks) {
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				builder.WriteString(call.Function.Arguments)
			}
		}
	}
	return builder.String()
}

// toolCallFrames counts the emitted tool-call deltas.
func toolCallFrames(t *testing.T, chunks []pluginapi.ExecutorStreamChunk) int {
	t.Helper()
	count := 0
	for _, chunk := range decodeChunks(t, chunks) {
		for _, choice := range chunk.Choices {
			count += len(choice.Delta.ToolCalls)
		}
	}
	return count
}

// TestChatBodyKeepsToolsAtTheTopLevel pins trap #19: without a top-level `tools`
// array the model invents XML tool calls the harness cannot parse.
func TestChatBodyKeepsToolsAtTheTopLevel(t *testing.T) {
	payload := `{"model":"sn-kimi-k3","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"stream":false,"max_tokens":128,"temperature":0.2}`
	body, errBody := chatRequestBody(pluginapi.ExecutorRequest{Model: "sn-kimi-k3", Payload: []byte(payload)})
	if errBody != nil {
		t.Fatalf("build body: %v", errBody)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	tools, okTools := decoded["tools"].([]any)
	if !okTools || len(tools) != 1 {
		t.Fatalf("tools = %#v, want a top-level array of one entry", decoded["tools"])
	}
	function, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "lookup" {
		t.Errorf("tool function = %#v", function)
	}
	if decoded["stream"] != true {
		t.Error("stream must always be true: the adapter hard-codes it")
	}
	if decoded["max_tokens"] != float64(128) || decoded["temperature"] != 0.2 {
		t.Errorf("caller parameters were altered: %#v", decoded)
	}
}

// TestChatBodyDropsEmptyToolsAndNonPositiveMaxTokens pins the omit rules
// (`raccoon-adapter.ts:278-294`).
func TestChatBodyDropsEmptyToolsAndNonPositiveMaxTokens(t *testing.T) {
	for _, payload := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[],"max_tokens":0}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":-4}`,
	} {
		body, errBody := chatRequestBody(pluginapi.ExecutorRequest{Model: "m", Payload: []byte(payload)})
		if errBody != nil {
			t.Fatalf("build body: %v", errBody)
		}
		var decoded map[string]any
		if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
			t.Fatalf("decode body: %v", errUnmarshal)
		}
		if _, present := decoded["tools"]; present {
			t.Errorf("payload %s: an empty tools array must be omitted", payload)
		}
		if _, present := decoded["max_tokens"]; present {
			t.Errorf("payload %s: a non-positive max_tokens must be dropped", payload)
		}
	}
}

// TestNormaliseMessagesDropsOrphans pins trap #24: the backend answers 400 for a
// dangling pair, and the poisoned history is replayed on every later request.
func TestNormaliseMessagesDropsOrphans(t *testing.T) {
	payload := `{"model":"m","messages":[
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"42"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_orphan","type":"function","function":{"name":"ghost","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_missing","content":"orphan result"},
		{"role":"assistant","content":"plain text","tool_calls":[{"id":"call_2","type":"function","function":{"name":"unanswered","arguments":"{}"}}]},
		{"role":"user","content":"hi"}
	]}`
	normalised := normaliseChatPayload([]byte(payload))
	var decoded struct {
		Messages []map[string]any `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(normalised, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if len(decoded.Messages) != 4 {
		t.Fatalf("kept %d messages, want 4: the orphan call, the orphan result and the emptied assistant turn must go",
			len(decoded.Messages))
	}
	// The answered pair survives intact, with `content: null` on the call turn.
	first := decoded.Messages[0]
	if first["role"] != "assistant" {
		t.Fatalf("first message = %#v", first)
	}
	if content, present := first["content"]; !present || content != nil {
		t.Errorf("assistant content = %#v, want an explicit null when the turn is pure tool calls", content)
	}
	calls, _ := first["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("answered call was dropped: %#v", first["tool_calls"])
	}
	if decoded.Messages[1]["role"] != "tool" || decoded.Messages[1]["tool_call_id"] != "call_1" {
		t.Errorf("the answered tool result is missing: %#v", decoded.Messages[1])
	}
	// The plain-text assistant turn keeps its text and loses its unanswered call.
	textTurn := decoded.Messages[2]
	if textTurn["content"] != "plain text" {
		t.Errorf("assistant text = %#v", textTurn["content"])
	}
	if _, present := textTurn["tool_calls"]; present {
		t.Errorf("an unanswered call must be dropped: %#v", textTurn["tool_calls"])
	}
	if decoded.Messages[3]["role"] != "user" || decoded.Messages[3]["content"] != "hi" {
		t.Errorf("the trailing user turn was lost: %#v", decoded.Messages[3])
	}
}

// TestRequestTranslateAppliesTheSameHygiene pins that the translated payload is
// normalised too, not just the executor-built body.
func TestRequestTranslateAppliesTheSameHygiene(t *testing.T) {
	raw, errMarshal := json.Marshal(pluginapi.RequestTransformRequest{
		FromFormat: "chat-completions",
		ToFormat:   "chat-completions",
		Body:       []byte(`{"messages":[{"role":"tool","tool_call_id":"missing","content":"x"},{"role":"user","content":"hi"}]}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	value, errTranslate := handleRequestTranslate(nil, raw)
	if errTranslate != nil {
		t.Fatalf("translate: %v", errTranslate)
	}
	body := value.(pluginapi.PayloadResponse).Body
	if strings.Contains(string(body), "missing") {
		t.Errorf("the orphan tool result survived translation: %s", body)
	}
	// A payload that is not chat-completions is passed through untouched.
	passthrough := []byte(`{"input":"not chat"}`)
	raw, _ = json.Marshal(pluginapi.RequestTransformRequest{Body: passthrough})
	value, errTranslate = handleRequestTranslate(nil, raw)
	if errTranslate != nil {
		t.Fatalf("translate: %v", errTranslate)
	}
	if string(value.(pluginapi.PayloadResponse).Body) != string(passthrough) {
		t.Error("a non-chat payload must be passed through unchanged")
	}
}

// TestStreamFinishReasonMapping pins the priority table (trap #21). `length`, a
// missing finish reason, a cut connection and truncated arguments must ALL become
// `length`; reporting `tool-calls` makes the harness run a call with missing
// parameters and the model retries forever.
func TestStreamFinishReasonMapping(t *testing.T) {
	toolFrame := `{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"SF\"}"}}]}}]}`
	truncatedFrame := `{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":"}}]}}]}`
	cases := []struct {
		name   string
		frames []string
		want   string
	}{
		{
			name:   "explicit length",
			frames: []string{`{"id":"c","choices":[{"index":0,"delta":{"content":"partial"}}]}`, `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`, sse.Done},
			want:   "length",
		},
		{
			name:   "missing finish reason with a usable tool call",
			frames: []string{toolFrame},
			want:   "length",
		},
		{
			name:   "cut connection after content",
			frames: []string{`{"id":"c","choices":[{"index":0,"delta":{"content":"half a sen"}}]}`},
			want:   "length",
		},
		{
			name:   "truncated tool arguments",
			frames: []string{truncatedFrame, `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, sse.Done},
			want:   "length",
		},
		{
			name:   "clean tool call",
			frames: []string{toolFrame, `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, sse.Done},
			want:   "tool_calls",
		},
		{
			name:   "tool call with a stop finish reason",
			frames: []string{toolFrame, `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, sse.Done},
			want:   "tool_calls",
		},
		{
			name:   "clean stop",
			frames: []string{`{"id":"c","choices":[{"index":0,"delta":{"content":"done"}}]}`, `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, sse.Done},
			want:   "stop",
		},
		{
			name:   "done without a finish reason",
			frames: []string{`{"id":"c","choices":[{"index":0,"delta":{"content":"done"}}]}`, sse.Done},
			want:   "stop",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stream, errStream := runStream(sseBody(testCase.frames...))
			if errStream != nil {
				t.Fatalf("run stream: %v", errStream)
			}
			chunks, errChunks := stream.chunks()
			if errChunks != nil {
				t.Fatalf("chunks: %v", errChunks)
			}
			if got := finalReason(t, chunks); got != testCase.want {
				t.Errorf("finish_reason = %q, want %q", got, testCase.want)
			}
			// Every stream ends with `data: [DONE]`.
			last := chunks[len(chunks)-1].Payload
			if !strings.Contains(string(last), sse.Done) {
				t.Errorf("the stream does not end with [DONE]: %q", last)
			}
		})
	}
}

// TestNamelessToolFragmentsEmitZeroChunks pins trap #22: a persisted empty-name
// call is replayed forever and yields HTTP 400 code 11133.
func TestNamelessToolFragmentsEmitZeroChunks(t *testing.T) {
	frames := []string{
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{\"a\":1}"}}]}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		sse.Done,
	}
	stream, errStream := runStream(sseBody(frames...))
	if errStream != nil {
		t.Fatalf("run stream: %v", errStream)
	}
	chunks, errChunks := stream.chunks()
	if errChunks != nil {
		t.Fatalf("chunks: %v", errChunks)
	}
	if got := toolCallFrames(t, chunks); got != 0 {
		t.Fatalf("emitted %d tool-call frames for a nameless call, want 0", got)
	}
	// Nothing usable was emitted, so the stream is reported as truncated rather
	// than as a clean stop.
	if got := finalReason(t, chunks); got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
}

// TestToolCallArgumentsAreFlushedOnceTheNameArrives pins the gating order: no
// chunk before a name, then the accumulated arguments in one delta, then further
// argument fragments.
func TestToolCallArgumentsAreFlushedOnceTheNameArrives(t *testing.T) {
	frames := []string{
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{\"city\""}}]}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"lookup"}}]}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"SF\"}"}}]}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		sse.Done,
	}
	stream, errStream := runStream(sseBody(frames...))
	if errStream != nil {
		t.Fatalf("run stream: %v", errStream)
	}
	chunks, errChunks := stream.chunks()
	if errChunks != nil {
		t.Fatalf("chunks: %v", errChunks)
	}
	if got := toolArguments(t, chunks); got != `{"city":"SF"}` {
		t.Errorf("accumulated arguments = %q, want %q", got, `{"city":"SF"}`)
	}
	// The first emitted tool frame must already carry the name, and no tool frame
	// may appear before it.
	decoded := decodeChunks(t, chunks)
	sawName := false
	for _, chunk := range decoded {
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				if !sawName {
					if call.Function.Name != "lookup" {
						t.Fatalf("the first tool frame has no name: %+v", call)
					}
					sawName = true
				} else if call.Function.Name != "" && call.Function.Name != "lookup" {
					t.Fatalf("unexpected tool name %q", call.Function.Name)
				}
			}
		}
	}
	if !sawName {
		t.Fatal("no tool frame was emitted at all")
	}
	if got := finalReason(t, chunks); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool-calls", got)
	}
}

// TestEmptyResponseIsAnError pins the final guard: a "completed" response with no
// content at all must not look like a clean stop.
func TestEmptyResponseIsAnError(t *testing.T) {
	stream, errStream := runStream(sseBody(
		`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, sse.Done))
	if errStream != nil {
		t.Fatalf("run stream: %v", errStream)
	}
	if _, errChunks := stream.chunks(); errChunks == nil {
		t.Fatal("an empty completed response must be an error")
	} else if !strings.Contains(errChunks.Error(), "no content") {
		t.Errorf("error = %v, want it to say there was no content", errChunks)
	}
}

// TestStreamErrorFrames pins the three in-band error shapes (trap #20).
func TestStreamErrorFrames(t *testing.T) {
	cases := []struct {
		name    string
		frame   string
		wantSub string
	}{
		{name: "error object", frame: `{"error":{"message":"upstream exploded"}}`, wantSub: "upstream exploded"},
		{name: "business envelope", frame: `{"code":100002,"message":"params invalid"}`, wantSub: "params invalid"},
		{name: "gateway form", frame: `{"message":"bad gateway","statusCodeValue":502}`, wantSub: "bad gateway"},
		{name: "gateway stack trace", frame: `{"message":"boom","stackTrace":"at x"}`, wantSub: "boom"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, errStream := runStream(sseBody(testCase.frame))
			if errStream == nil {
				t.Fatal("an error frame must surface as an error")
			}
			if !strings.Contains(errStream.Error(), testCase.wantSub) {
				t.Errorf("error = %v, want it to contain %q", errStream, testCase.wantSub)
			}
		})
	}
	// A valid chunk with `choices` must NOT be mistaken for an error frame.
	stream, errStream := runStream(sseBody(
		`{"id":"c","code":0,"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, sse.Done))
	if errStream != nil {
		t.Fatalf("run stream: %v", errStream)
	}
	if _, errChunks := stream.chunks(); errChunks != nil {
		t.Fatalf("a valid chunk was treated as an error: %v", errChunks)
	}
}

// TestNonSSEBodyIsAnError pins that a gateway JSON body is reported as "not SSE"
// with a snippet, instead of silently becoming an empty stream.
func TestNonSSEBodyIsAnError(t *testing.T) {
	_, errStream := runStream([]byte(`{"code":100002,"message":"缺少 token"}`))
	if errStream == nil {
		t.Fatal("a body with no data: frame must be an error")
	}
	if !strings.Contains(errStream.Error(), "不是 SSE") {
		t.Errorf("error = %v, want the not-SSE wording", errStream)
	}
	if _, errEmpty := runStream(nil); errEmpty == nil {
		t.Fatal("an empty body must be an error")
	}
}

// TestNonStreamingFold pins the folded `chat.completion` shape.
func TestNonStreamingFold(t *testing.T) {
	frames := []string{
		`{"id":"chatcmpl-1","model":"sn-kimi-k3","created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"world"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"SF\"}"}}]}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl-1","usage":{"prompt_tokens":11,"completion_tokens":7}}`,
		sse.Done,
	}
	stream, errStream := runStream(sseBody(frames...))
	if errStream != nil {
		t.Fatalf("run stream: %v", errStream)
	}
	payload, errCompletion := stream.completion("sn-kimi-k3")
	if errCompletion != nil {
		t.Fatalf("fold: %v", errCompletion)
	}
	var completion struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(payload, &completion); errUnmarshal != nil {
		t.Fatalf("decode completion: %v", errUnmarshal)
	}
	if completion.Object != "chat.completion" || completion.Model != "sn-kimi-k3" {
		t.Errorf("completion envelope = %+v", completion)
	}
	choice := completion.Choices[0]
	if choice.Message.Content != "Hello world" {
		t.Errorf("content = %q, want the concatenated deltas", choice.Message.Content)
	}
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 || choice.Message.ToolCalls[0].Function.Name != "lookup" {
		t.Errorf("tool calls = %+v", choice.Message.ToolCalls)
	}
	if choice.Message.ToolCalls[0].Function.Arguments != `{"city":"SF"}` {
		t.Errorf("arguments = %q", choice.Message.ToolCalls[0].Function.Arguments)
	}
	if completion.Usage["prompt_tokens"] != float64(11) {
		t.Errorf("usage was dropped: %+v", completion.Usage)
	}
}

// TestExecutorRejectsAMissingCredential pins the 401 classification
// (`raccoon-adapter.ts:257-259`).
func TestExecutorRejectsAMissingCredential(t *testing.T) {
	for _, payload := range []string{`{}`, `{"access_token":""}`, `not json`} {
		raw, errMarshal := json.Marshal(pluginapi.ExecutorRequest{
			Model:   "sn-kimi-k3",
			Payload: []byte(`{"model":"sn-kimi-k3","messages":[]}`),
			// no StorageJSON
		})
		if errMarshal != nil {
			t.Fatalf("marshal: %v", errMarshal)
		}
		_ = payload
		if _, errExecute := handleExecutorExecute(testHost(), raw); errExecute == nil {
			t.Fatal("a request with no credential must fail")
		} else if got := statusOf(errExecute, 0); got != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", got)
		}
	}
}

// TestPerformInferRetriesOnceAfterRefreshing pins the 401 → refresh → retry
// contract (`raccoon-adapter.ts:331-341`), and that a credential with no refresh
// token fails instead of looping.
func TestPerformInferRetriesOnceAfterRefreshing(t *testing.T) {
	fake := newFakeHost()
	credential := sampleCredential(t)
	attempts := 0
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, RefreshPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"access_token":"`+fakeJWT(t, nowTime().Add(3*time.Hour))+`","refresh_token":"rotated"}}`), nil
		default:
			attempts++
			if attempts == 1 {
				return httpResponse(http.StatusUnauthorized, `{"code":200003,"message":"expired"}`), nil
			}
			return httpResponse(http.StatusOK, string(sseBody(
				`{"id":"c","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, sse.Done))), nil
		}
	}
	fake.install(t)

	request := executorRequest(t, credential, `{"model":"sn-kimi-k3","messages":[{"role":"user","content":"hi"}]}`)
	response, errInfer := performInfer(testHost(), request, credential)
	if errInfer != nil {
		t.Fatalf("a 401 must be retried once after a refresh: %v", errInfer)
	}
	if response.StatusCode != http.StatusOK || attempts != 2 {
		t.Fatalf("status = %d after %d attempts, want 200 after exactly 2", response.StatusCode, attempts)
	}

	// Without a refresh token the failure is terminal.
	noRefresh := &Credential{AccessToken: "opaque", RefreshToken: ""}
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusUnauthorized, `{"message":"expired"}`), nil
	}
	fake.install(t)
	_, errInfer = performInfer(testHost(), request, noRefresh)
	if errInfer == nil {
		t.Fatal("a 401 with no refresh token must fail")
	}
	if got := statusOf(errInfer, 0); got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}
