package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// executorPayload builds an executor.execute request bound to a credential.
func executorPayload(t *testing.T, credential *Credential, model string, body string) json.RawMessage {
	t.Helper()
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode credential: %v", errEncode)
	}
	raw, errMarshal := json.Marshal(pluginapi.ExecutorRequest{
		AuthID:       "loomy-1",
		AuthProvider: ProviderKey,
		Model:        model,
		Format:       "chat-completions",
		SourceFormat: "chat-completions",
		Stream:       true,
		Payload:      []byte(body),
		StorageJSON:  storage,
	})
	if errMarshal != nil {
		t.Fatalf("marshal executor request: %v", errMarshal)
	}
	return raw
}

// The chat endpoint sends BOTH auth header families, and the token header keeps
// its literal lowercase wire name (`loomy.ts:225-231`, trap #2).
func TestChatHeadersCarryBothFamilies(t *testing.T) {
	credential := sampleCredential(t)
	headers := chatHeaders(credential)
	if got := headers.Get("Authorization"); got != "Bearer "+credential.Session() {
		t.Fatalf("Authorization = %q, want a Bearer header with the session", got)
	}
	if got := headers["token"]; len(got) != 1 || got[0] != credential.Session() {
		t.Fatalf("token = %#v, want the session under the literal lowercase key", got)
	}
	if got := headers.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", got)
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

// The business endpoints get the lowercase `token` header ONLY: sending Bearer
// answers HTTP 200 with 100002 (`README.md:1404-1413`).
func TestBusinessHeadersNeverSendBearer(t *testing.T) {
	credential := sampleCredential(t)
	headers := businessHeaders(credential, false)
	if got := headers.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want none on business endpoints", got)
	}
	if got := headers["token"]; len(got) != 1 || got[0] != credential.Session() {
		t.Fatalf("token = %#v, want the session under the lowercase key", got)
	}
	if got := headers.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
	if got := headers.Get("Content-Type"); got != "" {
		t.Fatalf("Content-Type = %q, want it omitted for a body-less call", got)
	}
	if got := businessHeaders(credential, true).Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want it present for a body-carrying call", got)
	}
}

// The translators are identity transforms: the host owns cross-protocol work.
func TestTranslateHandlersAreIdentity(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	requestValue, errRequest := handleRequestTranslate(nil, mustJSON(t, pluginapi.RequestTransformRequest{Body: body}))
	if errRequest != nil {
		t.Fatalf("request.translate: %v", errRequest)
	}
	if got := decodeResult[pluginapi.PayloadResponse](t, requestValue).Body; string(got) != string(body) {
		t.Fatalf("request.translate body = %s, want the input unchanged", got)
	}
	responseValue, errResponse := handleResponseTranslate(nil, mustJSON(t, pluginapi.ResponseTransformRequest{Body: body}))
	if errResponse != nil {
		t.Fatalf("response.translate: %v", errResponse)
	}
	if got := decodeResult[pluginapi.PayloadResponse](t, responseValue).Body; string(got) != string(body) {
		t.Fatalf("response.translate body = %s, want the input unchanged", got)
	}
}

// count_tokens is a coarse estimate rather than an error.
func TestCountTokensReturnsAnEstimate(t *testing.T) {
	value, errCount := handleExecutorCountTokens(nil, mustJSON(t, pluginapi.ExecutorRequest{Payload: []byte(strings.Repeat("a", 40))}))
	if errCount != nil {
		t.Fatalf("count_tokens: %v", errCount)
	}
	payload := decodeResult[pluginapi.ExecutorResponse](t, value).Payload
	var counted struct {
		InputTokens int `json:"input_tokens"`
	}
	if errUnmarshal := json.Unmarshal(payload, &counted); errUnmarshal != nil {
		t.Fatalf("decode count payload %s: %v", payload, errUnmarshal)
	}
	if counted.InputTokens != 11 {
		t.Fatalf("input_tokens = %d, want len/4+1 = 11", counted.InputTokens)
	}
}

// The upstream is ALWAYS asked to stream, and the model is forced when the
// translated payload omits it (`loomy-adapter.ts:307-326`).
func TestChatRequestBodyForcesStreamAndModel(t *testing.T) {
	body, errBody := chatRequestBody(pluginapi.ExecutorRequest{
		Model:   "MiniMax-M3",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":0.3}`),
	}, nil)
	if errBody != nil {
		t.Fatalf("chatRequestBody: %v", errBody)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if decoded["stream"] != true {
		t.Fatalf("stream = %v, want true on every request", decoded["stream"])
	}
	if decoded["model"] != "MiniMax-M3" {
		t.Fatalf("model = %v, want the requested model", decoded["model"])
	}
	if decoded["temperature"] != 0.3 {
		t.Fatalf("temperature = %v, want the caller value preserved", decoded["temperature"])
	}
	if _, present := decoded["max_tokens"]; present {
		t.Fatal("max_tokens must not be invented when the caller did not send one")
	}
	if _, errEmpty := chatRequestBody(pluginapi.ExecutorRequest{}, nil); errEmpty == nil {
		t.Fatal("an empty payload must be rejected")
	}
}

// TestChatRequestBodyReasoningEffort: `reasoning_effort` is written ONLY when the
// model declares that level. An out-of-list or absent level must leave the key
// out, because the upstream silently ignores a value it does not know — the
// "the UI offered it but the request drops it" defect
// (`loomy-adapter.ts:437-455`).
func TestChatRequestBodyReasoningEffort(t *testing.T) {
	levels := []string{"none", "low", "medium", "high", "xhigh"}
	cases := []struct {
		name    string
		payload string
		efforts []string
		want    string
		present bool
	}{
		{
			name:    "an in-list level is carried verbatim",
			payload: `{"messages":[{"role":"user"}],"reasoning_effort":"high"}`,
			efforts: levels,
			want:    "high",
			present: true,
		},
		{
			name:    "a per-model list is what counts, not the global one",
			payload: `{"messages":[{"role":"user"}],"reasoning_effort":"xhigh"}`,
			efforts: []string{"low", "high"},
			present: false,
		},
		{
			name:    "an out-of-list level is dropped",
			payload: `{"messages":[{"role":"user"}],"reasoning_effort":"max"}`,
			efforts: levels,
			present: false,
		},
		{
			name:    "a model that declares no levels never gets the key",
			payload: `{"messages":[{"role":"user"}],"reasoning_effort":"high"}`,
			efforts: nil,
			present: false,
		},
		{
			name:    "no requested level means no key is invented",
			payload: `{"messages":[{"role":"user"}]}`,
			efforts: levels,
			present: false,
		},
		{
			name:    "a blank requested level is dropped",
			payload: `{"messages":[{"role":"user"}],"reasoning_effort":"  "}`,
			efforts: levels,
			present: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body, errBody := chatRequestBody(pluginapi.ExecutorRequest{Payload: []byte(testCase.payload)}, testCase.efforts)
			if errBody != nil {
				t.Fatalf("chatRequestBody: %v", errBody)
			}
			var decoded map[string]any
			if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
				t.Fatalf("decode body: %v", errUnmarshal)
			}
			got, present := decoded["reasoning_effort"]
			if present != testCase.present {
				t.Fatalf("reasoning_effort present = %v (%v), want %v", present, got, testCase.present)
			}
			if testCase.present && got != testCase.want {
				t.Fatalf("reasoning_effort = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestPerformInferSendsReasoningEffortForDeclaredLevels wires the whole path: the
// levels the model declares are resolved from the catalogue, so the wire carries
// the field only for a declared level.
func TestPerformInferSendsReasoningEffortForDeclaredLevels(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		effort  string
		want    string
		present bool
	}{
		{name: "declared level", model: "MiniMax-M3", effort: "high", want: "high", present: true},
		{name: "undeclared level", model: "MiniMax-M3", effort: "max", present: false},
		{name: "model with no levels", model: "unknown-model", effort: "high", present: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeHost()
			var sent map[string]any
			fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				if errUnmarshal := json.Unmarshal(request.Body, &sent); errUnmarshal != nil {
					t.Errorf("decode outbound body: %v", errUnmarshal)
				}
				return httpResponse(200, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
			}
			fake.install(t)
			payload := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + testCase.effort + `"}`
			if _, errExecute := handleExecutorExecute(testHost(), executorPayload(t, sampleCredential(t), testCase.model, payload)); errExecute != nil {
				t.Fatalf("executor.execute: %v", errExecute)
			}
			got, present := sent["reasoning_effort"]
			if present != testCase.present {
				t.Fatalf("outbound reasoning_effort present = %v (%v), want %v (body %#v)", present, got, testCase.present, sent)
			}
			if testCase.present && got != testCase.want {
				t.Fatalf("outbound reasoning_effort = %v, want %v", got, testCase.want)
			}
		})
	}
}

// A non-streaming call folds the upstream SSE into one chat.completion, keeping
// content, both reasoning spellings, tool calls and usage.
func TestExecuteFoldsStreamIntoCompletion(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.URL != APIBase+ChatCompletionsPath {
			t.Errorf("url = %q, want %q", request.URL, APIBase+ChatCompletionsPath)
		}
		if got := request.Headers.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q, want a Bearer header", got)
		}
		if got := request.Headers["token"]; len(got) != 1 {
			t.Errorf("token = %#v, want the lowercase session header", got)
		}
		body := strings.Join([]string{
			`data: {"id":"c1","model":"MiniMax-M3","created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
			``,
			`data: {"choices":[{"index":0,"delta":{"content":"好","reasoning_content":null,"reasoning":"想"}}]}`,
			``,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
			``,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"bj\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":3}}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")
		return httpResponse(200, body), nil
	}
	fake.install(t)

	value, errExecute := handleExecutorExecute(testHost(), executorPayload(t, sampleCredential(t), "MiniMax-M3", `{"messages":[{"role":"user","content":"hi"}]}`))
	if errExecute != nil {
		t.Fatalf("executor.execute: %v", errExecute)
	}
	response := decodeResult[pluginapi.ExecutorResponse](t, value)
	completion := struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}{}
	if errUnmarshal := json.Unmarshal(response.Payload, &completion); errUnmarshal != nil {
		t.Fatalf("decode completion %s: %v", response.Payload, errUnmarshal)
	}
	if completion.Object != "chat.completion" || completion.ID != "c1" || completion.Model != "MiniMax-M3" {
		t.Fatalf("completion header = %#v, want the folded chat.completion", completion)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(completion.Choices))
	}
	message := completion.Choices[0].Message
	if message.Role != "assistant" || message.Content != "你好" {
		t.Fatalf("message = %#v, want the accumulated assistant content", message)
	}
	// `reasoning` is the fallback spelling and was read; the explicit null
	// reasoning_content must not have shadowed it.
	if message.ReasoningContent != "想" {
		t.Fatalf("reasoning = %q, want the `reasoning` fallback", message.ReasoningContent)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one merged call", message.ToolCalls)
	}
	call := message.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "get_weather" ||
		call.Function.Arguments != `{"city":"bj"}` {
		t.Fatalf("tool call = %#v, want the fragments merged by index", call)
	}
	if completion.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", completion.Choices[0].FinishReason)
	}
	if !strings.Contains(string(completion.Usage), "cached_tokens") {
		t.Fatalf("usage = %s, want prompt_tokens_details preserved verbatim", completion.Usage)
	}
	if truncated, _ := response.Metadata["truncated"].(bool); truncated {
		t.Fatal("a stream with a finish_reason is not truncated")
	}
}

// A stream that ends without `finish_reason` and without `[DONE]` is truncated:
// max-tokens, retryable — not a clean stop (`openai-compat.ts:915-959`).
func TestExecuteMarksTruncatedStream(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"), nil
	}
	fake.install(t)
	value, errExecute := handleExecutorExecute(testHost(), executorPayload(t, sampleCredential(t), "MiniMax-M3", `{}`))
	if errExecute != nil {
		t.Fatalf("executor.execute: %v", errExecute)
	}
	response := decodeResult[pluginapi.ExecutorResponse](t, value)
	if truncated, _ := response.Metadata["truncated"].(bool); !truncated {
		t.Fatalf("metadata = %#v, want truncated=true", response.Metadata)
	}
	if !strings.Contains(string(response.Payload), `"finish_reason":"length"`) {
		t.Fatalf("payload = %s, want the max-tokens finish reason", response.Payload)
	}
}

// A tool-call fragment that opens a new index without a name is dropped: an
// empty-named tool call poisons the conversation
// (`openai-compat.ts:691-721`).
func TestAggregateDropsNamelessToolCallFragments(t *testing.T) {
	payloads := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
	}
	completion, _ := aggregateFrames(payloads, "m")
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(completion.Choices))
	}
	if len(completion.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("tool calls = %#v, want the nameless fragment dropped", completion.Choices[0].Message.ToolCalls)
	}
	// The source's condition does not consult the finish reason, so even an
	// explicit `stop` cannot hide the dropped call.
	if got := completion.Choices[0].FinishReason; got != "length" {
		t.Fatalf("finish_reason = %q, want length for a dropped nameless call", got)
	}
}

// sseDoneFrame is the terminal SSE payload as `aggregateFrames` receives it (the
// `data:` prefix is stripped by the scanner).
const sseDoneFrame = "[DONE]"

// TestAggregateDroppedUnnamedFinishesAsLength: when the ONLY tool call had no
// usable name, the turn must finish as `length` — an incomplete, retryable turn —
// rather than `stop`, which would tell the client the model deliberately answered
// without a tool (`openai-compat.ts:889-965`).
func TestAggregateDroppedUnnamedFinishesAsLength(t *testing.T) {
	cases := []struct {
		name       string
		payloads   []string
		wantFinish string
		wantCalls  int
	}{
		{
			name: "a single nameless fragment ends as length",
			payloads: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"arguments":"{}"}}]}}]}`,
				sseDoneFrame,
			},
			wantFinish: "length",
		},
		{
			name: "a JSON null name is not the literal string null",
			payloads: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":null,"arguments":"{}"}}]}}]}`,
				sseDoneFrame,
			},
			wantFinish: "length",
		},
		{
			name: "a blank name is not usable either",
			payloads: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"   ","arguments":"{}"}}]}}]}`,
				sseDoneFrame,
			},
			wantFinish: "length",
		},
		{
			name: "a named call still reports tool_calls",
			payloads: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			},
			wantFinish: "tool_calls",
			wantCalls:  1,
		},
		{
			name: "one named call survives alongside a nameless one",
			payloads: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{}"}},{"index":1,"id":"call_x","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			},
			wantFinish: "tool_calls",
			wantCalls:  1,
		},
		{
			name: "the literal string undefined is a real name upstream accepts",
			payloads: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"undefined","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			},
			wantFinish: "tool_calls",
			wantCalls:  1,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			completion, _ := aggregateFrames(testCase.payloads, "m")
			if got := completion.Choices[0].FinishReason; got != testCase.wantFinish {
				t.Fatalf("finish_reason = %q, want %q", got, testCase.wantFinish)
			}
			if got := len(completion.Choices[0].Message.ToolCalls); got != testCase.wantCalls {
				t.Fatalf("tool calls = %d, want %d", got, testCase.wantCalls)
			}
		})
	}
}

// TestExecuteStreamDroppedUnnamedIsNotReportedAsStop drives the handler: a stream
// whose only tool call is nameless must not answer `stop`.
func TestExecuteStreamDroppedUnnamedIsNotReportedAsStop(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_x\",\"function\":{\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n"), nil
	}
	fake.install(t)
	value, errExecute := handleExecutorExecute(testHost(), executorPayload(t, sampleCredential(t), "MiniMax-M3", `{}`))
	if errExecute != nil {
		t.Fatalf("executor.execute: %v", errExecute)
	}
	response := decodeResult[pluginapi.ExecutorResponse](t, value)
	if !strings.Contains(string(response.Payload), `"finish_reason":"length"`) {
		t.Fatalf("payload = %s, want finish_reason length", response.Payload)
	}
	if !strings.Contains(string(response.Payload), `"tool_calls"`) {
		// The key must be absent from the message, not an empty array.
		if strings.Contains(string(response.Payload), `"message":{"role":"assistant","content":"","reasoning_content":"","tool_calls"`) {
			t.Fatalf("payload = %s, want no tool_calls in the message", response.Payload)
		}
	}
	if truncated, _ := response.Metadata["truncated"].(bool); !truncated {
		t.Fatalf("metadata = %#v, want truncated=true for a dropped unnamed call", response.Metadata)
	}
}

// Streaming passes every `data:` payload through and terminates the stream.
func TestExecuteStreamFramesAndTerminator(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		// No [DONE] from the upstream.
		return httpResponse(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"), nil
	}
	fake.install(t)
	value, errExecute := handleExecutorExecuteStream(testHost(), executorPayload(t, sampleCredential(t), "MiniMax-M3", `{}`))
	if errExecute != nil {
		t.Fatalf("executor.execute_stream: %v", errExecute)
	}
	response := decodeResult[executorStreamResponse](t, value)
	if got := response.Headers.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	// One payload, bare: the host frames it and writes the terminal `data:
	// [DONE]` itself, so a terminator chunk here would be duplicated.
	if len(response.Chunks) != 1 {
		t.Fatalf("chunks = %d, want the payload alone", len(response.Chunks))
	}
	// A stream without [DONE] and without a finish_reason is truncated, and the
	// truncation is reported instead of being hidden by a terminator.
	if len(fake.logs) != 1 || !strings.Contains(fake.logs[0], "未正常结束") {
		t.Fatalf("logs = %#v, want one truncation warning", fake.logs)
	}
	payload := string(response.Chunks[0].Payload)
	if strings.HasPrefix(payload, "data:") {
		t.Fatalf("payload must not carry its own framing: %s", payload)
	}
	if !strings.Contains(payload, `"content":"hi"`) {
		t.Fatalf("chunk = %s, want the upstream frame", payload)
	}
}

// TestStreamFramesGatewayErrorFrame: a gateway error frame carries `message` plus
// `statusCodeValue` (and often `stackTrace`) with NO `error`, NO `code` and NO
// `choices`. It must surface as a real upstream failure carrying that message —
// before the fix it fell through to the truncation rule and was reported as an
// ordinary `length` stop with the reason swallowed (`a3501f0`,
// `openai-compat.ts:552-583`).
func TestStreamFramesGatewayErrorFrame(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{
			name:        "statusCodeValue 500 with a message",
			body:        "data: {\"message\":\"模型服务暂时不可用\",\"statusCodeValue\":500}\n\n",
			wantMessage: "模型服务暂时不可用",
		},
		{
			name:        "a stack trace alone marks the frame as an error",
			body:        "data: {\"message\":\"内部错误\",\"stackTrace\":[\"at x\"]}\n\n",
			wantMessage: "内部错误",
		},
		{
			name:        "the real gateway shape with both fields",
			body:        "data: {\"stackTrace\":[\"at com.x\"],\"message\":\"网关拒绝\",\"statusCodeValue\":502}\n\n",
			wantMessage: "网关拒绝",
		},
		{
			name:        "an error object is reported too",
			body:        "data: {\"error\":{\"message\":\"积分不足\"}}\n\n",
			wantMessage: "积分不足",
		},
		{
			name:        "a business envelope with a non-success code",
			body:        "data: {\"code\":\"100002\",\"desc\":\"缺少 token\"}\n\n",
			wantMessage: "缺少 token",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			chunks, truncated, errFrames := streamFrames([]byte(testCase.body))
			if errFrames == nil {
				t.Fatalf("chunks = %#v, truncated = %v; want an upstream error", chunks, truncated)
			}
			if !strings.Contains(errFrames.Error(), testCase.wantMessage) {
				t.Fatalf("error = %v, want it to carry %q", errFrames, testCase.wantMessage)
			}
			if status := statusOf(errFrames, 0); status != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", status)
			}
			if truncated {
				t.Fatal("a reported upstream failure is not a truncation")
			}
		})
	}
}

// TestStreamFramesNormalContentIsNotAnErrorFrame is the negative case: a content
// delta that merely MENTIONS "message" (or carries a code-shaped word) must pass
// through untouched, because a frame with `choices` is always a normal chunk.
func TestStreamFramesNormalContentIsNotAnErrorFrame(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "content merely mentioning message",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"please read the message field\"}}]}\n\n",
		},
		{
			name: "content discussing a status code",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"statusCodeValue 500 means server error\"}}]}\n\n",
		},
		{
			name: "a normal chunk carrying usage",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":3}}\n\n",
		},
		{
			name: "a message field alongside choices is still a chunk",
			body: "data: {\"message\":\"not an error\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			chunks, _, errFrames := streamFrames([]byte(testCase.body))
			if errFrames != nil {
				t.Fatalf("error = %v, want a normal frame", errFrames)
			}
			if len(chunks) != 1 || !strings.Contains(string(chunks[0].Payload), `"choices"`) {
				t.Fatalf("chunks = %#v, want the frame forwarded untouched", chunks)
			}
		})
	}
}

// TestInferGatewayErrorFrameDoesNotDegradeToLength drives the whole handler: a
// response whose only frame is a gateway error must fail the request rather than
// answer with a `length` truncation.
func TestInferGatewayErrorFrameDoesNotDegradeToLength(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, "data: {\"message\":\"upstream exploded\",\"statusCodeValue\":500}\n\n"), nil
	}
	fake.install(t)
	_, errExecute := handleExecutorExecuteStream(testHost(), executorPayload(t, sampleCredential(t), "MiniMax-M3", `{}`))
	if errExecute == nil {
		t.Fatal("a gateway error frame must fail the request")
	}
	if !strings.Contains(errExecute.Error(), "upstream exploded") {
		t.Fatalf("error = %v, want the frame's message", errExecute)
	}
	if strings.Contains(errExecute.Error(), "LENGTH") || strings.Contains(strings.ToLower(errExecute.Error()), "截断") {
		t.Fatalf("error = %v, want no truncation wording", errExecute)
	}
}

// A body with no `data:` frame at all is an error, not an empty stream
// (`openai-compat.ts:901-913`).
func TestInferRejectsNonSSEBody(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"choices":[{"message":{"content":"json, not sse"}}]}`), nil
	}
	fake.install(t)
	_, errExecute := handleExecutorExecuteStream(testHost(), executorPayload(t, sampleCredential(t), "MiniMax-M3", `{}`))
	if errExecute == nil {
		t.Fatal("a JSON body must be reported as a broken stream")
	}
	if !strings.Contains(errExecute.Error(), "不是 SSE") {
		t.Fatalf("error = %v, want the SSE wording", errExecute)
	}
}

// The wrong header family answers HTTP 200 with 100002: that must surface as a
// dead credential, not as an empty successful stream.
func TestInferMapsHTTP200EnvelopeFailure(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"ok":false,"code":"100002","desc":"缺少 token"}`), nil
	}
	fake.install(t)
	_, errExecute := handleExecutorExecute(testHost(), executorPayload(t, sampleCredential(t), "MiniMax-M3", `{}`))
	if errExecute == nil {
		t.Fatal("a business failure at HTTP 200 must fail")
	}
	if status := statusOf(errExecute, 0); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if !strings.Contains(errExecute.Error(), "缺少 token") {
		t.Fatalf("error = %v, want the server desc", errExecute)
	}
}

// Transport and status failures are classified for the host's rotation logic.
func TestInferFailureClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
	}{
		{http.StatusUnauthorized, `{"error":{"message":"bad token"}}`, http.StatusUnauthorized},
		{http.StatusForbidden, `{"error":{"message":"forbidden"}}`, http.StatusUnauthorized},
		{http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests},
		{http.StatusPaymentRequired, `{"error":{"message":"no points"}}`, http.StatusPaymentRequired},
		{http.StatusInternalServerError, `{"error":{"message":"boom"}}`, http.StatusBadGateway},
	}
	for _, testCase := range cases {
		fake := newFakeHost()
		fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(testCase.status, testCase.body), nil
		}
		fake.install(t)
		_, errExecute := handleExecutorExecute(testHost(), executorPayload(t, sampleCredential(t), "MiniMax-M3", `{}`))
		if errExecute == nil {
			t.Fatalf("status %d must fail", testCase.status)
		}
		if got := statusOf(errExecute, 0); got != testCase.want {
			t.Errorf("status %d mapped to %d, want %d", testCase.status, got, testCase.want)
		}
	}
}

// A transport failure is retryable and never claims the credential is dead.
func TestInferTransportFailureIsRetryable(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	fake.install(t)
	_, errExecute := handleExecutorExecute(testHost(), executorPayload(t, sampleCredential(t), "MiniMax-M3", `{}`))
	if errExecute == nil {
		t.Fatal("a transport failure must fail")
	}
	if got := statusOf(errExecute, 0); got != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", got)
	}
}

// A credential that cannot be parsed fails before any request is sent.
func TestExecutorRejectsBrokenCredential(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatal("a broken credential must not reach the network")
		return nil, nil
	}
	fake.install(t)
	raw, errMarshal := json.Marshal(pluginapi.ExecutorRequest{StorageJSON: []byte(`{}`), Payload: []byte(`{}`)})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if _, errExecute := handleExecutorExecute(testHost(), raw); errExecute == nil {
		t.Fatal("an unparseable credential must fail")
	}
	if len(fake.requests) != 0 {
		t.Fatalf("issued %d requests, want 0", len(fake.requests))
	}
}
