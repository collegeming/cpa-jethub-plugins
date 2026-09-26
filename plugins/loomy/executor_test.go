package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
	})
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
	if _, errEmpty := chatRequestBody(pluginapi.ExecutorRequest{}); errEmpty == nil {
		t.Fatal("an empty payload must be rejected")
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
	if len(response.Chunks) != 2 {
		t.Fatalf("chunks = %d, want the payload plus a terminator", len(response.Chunks))
	}
	// A stream without [DONE] and without a finish_reason is truncated, and the
	// truncation is reported instead of being hidden by the terminator.
	if len(fake.logs) != 1 || !strings.Contains(fake.logs[0], "未正常结束") {
		t.Fatalf("logs = %#v, want one truncation warning", fake.logs)
	}
	if !strings.Contains(string(response.Chunks[0].Payload), `"content":"hi"`) {
		t.Fatalf("chunk = %s, want the upstream frame", response.Chunks[0].Payload)
	}
	if !strings.Contains(string(response.Chunks[1].Payload), sse.Done) {
		t.Fatalf("terminator = %s, want [DONE]", response.Chunks[1].Payload)
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
