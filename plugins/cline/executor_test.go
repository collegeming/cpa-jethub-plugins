package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// marshalRequest renders a method payload the way the host does, which matters
// for the []byte fields: `Payload` and `StorageJSON` travel base64-encoded.
func marshalRequest(t *testing.T, request any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	return raw
}

// chatStreamBody is a minimal, complete upstream SSE answer.
const chatStreamBody = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// executorRequest builds the host-shaped request for one execution.
func executorRequest(credential *Credential, payload string) (pluginapi.ExecutorRequest, error) {
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		return pluginapi.ExecutorRequest{}, errEncode
	}
	return pluginapi.ExecutorRequest{
		AuthID:       "cline-user@example.com.json",
		AuthProvider: ProviderKey,
		Model:        "cline-free/deepseek-v4.1-flash",
		Format:       "chat-completions",
		Stream:       false,
		Payload:      []byte(payload),
		StorageJSON:  storage,
	}, nil
}

// TestExecutorIdentifier pins the provider key the executor answers for, in the
// wire form the host decodes.
func TestExecutorIdentifier(t *testing.T) {
	value, errHandler := handleExecutorIdentifier(nil, nil)
	if errHandler != nil {
		t.Fatalf("handleExecutorIdentifier: %v", errHandler)
	}
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if string(raw) != `{"identifier":"cline"}` {
		t.Fatalf("identifier payload = %s", raw)
	}
}

// TestExecuteFoldsTheStream pins the non-streaming path: Cline only streams
// upstream, so the request always asks for a stream and the answer is folded.
func TestExecuteFoldsTheStream(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{AccessToken: "eyJ", RefreshToken: "r", AccountID: "usr-1"}
	request, errRequest := executorRequest(credential, `{"model":"ignored","messages":[{"role":"user","content":"hi"}],"stream":false,"top_p":0.5}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(200, chatStreamBody)}}}
	installFakeTransport(t, fake)

	value, errHandler := handleExecutorExecute(nil, marshalRequest(t, request))
	if errHandler != nil {
		t.Fatalf("handleExecutorExecute: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.ExecutorResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	var completion completionPayload
	if errUnmarshal := json.Unmarshal(response.Payload, &completion); errUnmarshal != nil {
		t.Fatalf("decode completion: %v", errUnmarshal)
	}
	if completion.ID != "c1" || completion.Object != "chat.completion" {
		t.Fatalf("completion = %+v", completion)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].FinishReason == nil || *completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices = %+v", completion.Choices)
	}
	if completion.Choices[0].Message.Content == nil || *completion.Choices[0].Message.Content != "hello" {
		t.Fatalf("message = %+v", completion.Choices[0].Message)
	}
	if response.Metadata["model"] != "cline-free/deepseek-v4.1-flash" {
		t.Errorf("metadata = %+v", response.Metadata)
	}

	call := fake.call(0)
	if call.URL != APIBase+ChatPath || call.Method != http.MethodPost {
		t.Fatalf("upstream call = %s %s", call.Method, call.URL)
	}
	if got := call.Headers.Get("Authorization"); got != "Bearer workos:eyJ" {
		t.Fatalf("Authorization = %q — the prefix is load-bearing", got)
	}
	if got := call.Headers.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q", got)
	}
	body := decodeBody(t, call.Body)
	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	if _, present := body["top_p"]; present {
		t.Errorf("top_p must not be forwarded: %s", call.Body)
	}
}

// TestExecuteStreamReturnsFrames pins the streaming path.
func TestExecuteStreamReturnsFrames(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{AccessToken: "workos:eyJ"}
	request, errRequest := executorRequest(credential, `{"messages":[{"role":"user","content":"hi"}]}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(200, chatStreamBody)}}}
	installFakeTransport(t, fake)

	value, errHandler := handleExecutorExecuteStream(nil, marshalRequest(t, request))
	if errHandler != nil {
		t.Fatalf("handleExecutorExecuteStream: %v", errHandler)
	}
	response, okResponse := value.(executorStreamResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	if got := response.Headers.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	// Three upstream frames; the [DONE] marker is not re-emitted as a chunk
	// because it terminates the frame list.
	if len(response.Chunks) != 3 {
		t.Fatalf("chunks = %d", len(response.Chunks))
	}
	if last := string(response.Chunks[len(response.Chunks)-1].Payload); last != "data: [DONE]\n\n" {
		t.Fatalf("last chunk = %q", last)
	}
	if !strings.Contains(string(response.Chunks[0].Payload), `"content":"hello"`) {
		t.Fatalf("first chunk = %q", response.Chunks[0].Payload)
	}
}

// TestExecuteRegionForbiddenSkipsRefresh is the rule that separates a region
// block from a dead credential (`cline-adapter.ts:609-646`).
func TestExecuteRegionForbiddenSkipsRefresh(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{AccessToken: "workos:eyJ", RefreshToken: "r"}
	request, errRequest := executorRequest(credential, `{"messages":[{"role":"user","content":"hi"}]}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(403,
		`{"error":"access forbidden: cline-free/deepseek-v4.1-flash is not available in your region","success":false}`)}}}
	installFakeTransport(t, fake)

	_, errHandler := handleExecutorExecute(nil, marshalRequest(t, request))
	if errHandler == nil {
		t.Fatal("a region-restricted model must fail")
	}
	if status := statusOf(errHandler, 0); status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if envelope := envelopeOf(errHandler); envelope == nil || envelope.Code != "region_forbidden" {
		t.Fatalf("code = %v", envelopeOf(errHandler))
	}
	if !strings.Contains(errHandler.Error(), "not available in your region") {
		t.Errorf("the real message must survive: %q", errHandler.Error())
	}
	if fake.callCount() != 1 {
		t.Fatalf("a region 403 must NOT trigger a refresh: %d calls", fake.callCount())
	}
}

// TestExecuteRefreshesOnceOn401 pins the other half of the rule: an ordinary
// 401/403 refreshes once, retries once, and uses the refreshed token.
func TestExecuteRefreshesOnceOn401(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{AccessToken: "workos:old", RefreshToken: "old-refresh", AccountID: "usr-1", Email: "a@b.c"}
	request, errRequest := executorRequest(credential, `{"messages":[{"role":"user","content":"hi"}]}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	fake := &fakeTransport{steps: []fakeStep{
		{response: jsonResponse(401, `{"error":"Unauthorized: Please make sure you're using the latest version of Cline"}`)},
		{response: jsonResponse(200, `{"success":true,"data":{"accessToken":"workos:new","refreshToken":"new-refresh","expiresAt":"2999-01-01T00:00:00.000Z"}}`)},
		{response: jsonResponse(200, chatStreamBody)},
	}}
	installFakeTransport(t, fake)

	value, errHandler := handleExecutorExecute(nil, marshalRequest(t, request))
	if errHandler != nil {
		t.Fatalf("handleExecutorExecute: %v", errHandler)
	}
	if _, okResponse := value.(pluginapi.ExecutorResponse); !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	if fake.callCount() != 3 {
		t.Fatalf("calls = %d, want chat + refresh + retry", fake.callCount())
	}
	if fake.call(0).URL != APIBase+ChatPath || fake.call(0).Headers.Get("Authorization") != "Bearer workos:old" {
		t.Fatalf("first call = %+v", fake.call(0))
	}
	if fake.call(1).URL != APIBase+RefreshPath {
		t.Fatalf("second call = %s", fake.call(1).URL)
	}
	if got := fake.call(2).Headers.Get("Authorization"); got != "Bearer workos:new" {
		t.Fatalf("the retry must use the refreshed token, got %q", got)
	}
	if string(fake.call(2).Body) != string(fake.call(0).Body) {
		t.Errorf("the retry body must be unchanged")
	}
}

// TestExecuteExpiredCredentialRefreshesFirst pins the resolve step of
// `cline-adapter.ts:402-410`.
func TestExecuteExpiredCredentialRefreshesFirst(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{
		AccessToken:  "workos:old",
		RefreshToken: "old-refresh",
		ExpireTime:   time.Now().Add(-time.Hour).UnixMilli(),
	}
	request, errRequest := executorRequest(credential, `{"messages":[{"role":"user","content":"hi"}]}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	fake := &fakeTransport{steps: []fakeStep{
		{response: jsonResponse(200, `{"data":{"accessToken":"workos:new","refreshToken":"new-refresh"}}`)},
		{response: jsonResponse(200, chatStreamBody)},
	}}
	installFakeTransport(t, fake)

	if _, errHandler := handleExecutorExecute(nil, marshalRequest(t, request)); errHandler != nil {
		t.Fatalf("handleExecutorExecute: %v", errHandler)
	}
	if fake.callCount() != 2 || fake.call(0).URL != APIBase+RefreshPath {
		t.Fatalf("an expired credential must be refreshed first: %+v", fake.calls)
	}
	if got := fake.call(1).Headers.Get("Authorization"); got != "Bearer workos:new" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestExecuteUnusableCredential pins MISSING_CREDENTIAL: an expired credential
// whose refresh fails is a 401, so the host parks it.
func TestExecuteUnusableCredential(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{
		AccessToken:  "workos:old",
		RefreshToken: "dead",
		ExpireTime:   time.Now().Add(-time.Hour).UnixMilli(),
	}
	request, errRequest := executorRequest(credential, `{"messages":[{"role":"user","content":"hi"}]}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(401, `{"error":"Unauthorized"}`)}}}
	installFakeTransport(t, fake)

	_, errHandler := handleExecutorExecute(nil, marshalRequest(t, request))
	if errHandler == nil {
		t.Fatal("expected a credential failure")
	}
	if status := statusOf(errHandler, 0); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if fake.callCount() != 1 {
		t.Errorf("no chat request may be attempted with a dead credential: %d calls", fake.callCount())
	}
}

// TestExecuteRejectsBadRequests pins that a malformed payload is the client's
// fault (400) and never reaches upstream.
func TestExecuteRejectsBadRequests(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{AccessToken: "workos:eyJ"}
	fake := &fakeTransport{}
	installFakeTransport(t, fake)

	cases := []struct {
		name    string
		request pluginapi.ExecutorRequest
	}{
		{"no messages", pluginapi.ExecutorRequest{Model: "m", Payload: []byte(`{"model":"m","messages":[]}`)}},
		{"undecodable payload", pluginapi.ExecutorRequest{Model: "m", Payload: []byte(`nope`)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			storage, errEncode := credential.Encode()
			if errEncode != nil {
				t.Fatalf("encode: %v", errEncode)
			}
			request := testCase.request
			request.StorageJSON = storage
			_, errHandler := handleExecutorExecute(nil, marshalRequest(t, request))
			if errHandler == nil {
				t.Fatal("expected a request error")
			}
			if status := statusOf(errHandler, 0); status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
		})
	}
	if fake.callCount() != 0 {
		t.Errorf("no upstream call may be issued: %d", fake.callCount())
	}
}

// TestExecuteRejectsAnEmptyCompletion pins the EMPTY_RESPONSE classification on
// the aggregate path.
func TestExecuteRejectsAnEmptyCompletion(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{AccessToken: "workos:eyJ"}
	request, errRequest := executorRequest(credential, `{"messages":[{"role":"user","content":"hi"}]}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(200,
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\ndata: [DONE]\n")}}}
	installFakeTransport(t, fake)

	_, errHandler := handleExecutorExecute(nil, marshalRequest(t, request))
	if errHandler == nil {
		t.Fatal("an empty completion must be reported as an error")
	}
	if envelope := envelopeOf(errHandler); envelope == nil || envelope.Code != "empty_response" {
		t.Fatalf("code = %v", envelopeOf(errHandler))
	}
}

// TestExecuteTransportFailureIsRetryable pins the TRANSPORT classification.
func TestExecuteTransportFailureIsRetryable(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{AccessToken: "workos:eyJ"}
	request, errRequest := executorRequest(credential, `{"messages":[{"role":"user","content":"hi"}]}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	fake := &fakeTransport{steps: []fakeStep{{err: errTestTransport}}}
	installFakeTransport(t, fake)

	_, errHandler := handleExecutorExecute(nil, marshalRequest(t, request))
	if errHandler == nil {
		t.Fatal("expected a transport failure")
	}
	if status := statusOf(errHandler, 0); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if envelope := envelopeOf(errHandler); envelope == nil || !envelope.Retryable {
		t.Fatalf("a transport failure must be retryable: %v", envelopeOf(errHandler))
	}
}

// TestExecuteRejectsMissingCredential pins that an absent credential never
// reaches upstream.
func TestExecuteRejectsMissingCredential(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	fake := &fakeTransport{}
	installFakeTransport(t, fake)
	_, errHandler := handleExecutorExecute(nil, marshalRequest(t, pluginapi.ExecutorRequest{
		Model:   "m",
		Payload: []byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`),
	}))
	if errHandler == nil {
		t.Fatal("expected a credential error")
	}
	if status := statusOf(errHandler, 0); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// TestTranslateRoutesAreIdentity pins the two translator routes: the executor
// declares chat-completions on both sides, so nothing needs translating.
func TestTranslateRoutesAreIdentity(t *testing.T) {
	request := pluginapi.RequestTransformRequest{Body: []byte(`{"a":1}`)}
	value, errHandler := handleRequestTranslate(nil, marshalRequest(t, request))
	if errHandler != nil {
		t.Fatalf("handleRequestTranslate: %v", errHandler)
	}
	if payload, okPayload := value.(pluginapi.PayloadResponse); !okPayload || string(payload.Body) != `{"a":1}` {
		t.Fatalf("request translate = %T %+v", value, value)
	}

	response := pluginapi.ResponseTransformRequest{Body: []byte(`{"b":2}`)}
	value, errHandler = handleResponseTranslate(nil, marshalRequest(t, response))
	if errHandler != nil {
		t.Fatalf("handleResponseTranslate: %v", errHandler)
	}
	if payload, okPayload := value.(pluginapi.PayloadResponse); !okPayload || string(payload.Body) != `{"b":2}` {
		t.Fatalf("response translate = %T %+v", value, value)
	}
}

// TestCountTokensIsAnEstimate pins that token counting answers instead of
// failing, because CPA falls back to its own tokenizer when a plugin does not
// implement it.
func TestCountTokensIsAnEstimate(t *testing.T) {
	value, errHandler := handleExecutorCountTokens(nil, marshalRequest(t, pluginapi.ExecutorRequest{
		Payload: []byte(strings.Repeat("x", 40)),
	}))
	if errHandler != nil {
		t.Fatalf("handleExecutorCountTokens: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.ExecutorResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	var decoded struct {
		InputTokens int `json:"input_tokens"`
	}
	if errUnmarshal := json.Unmarshal(response.Payload, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded.InputTokens != 11 {
		t.Fatalf("input_tokens = %d, want len/4+1", decoded.InputTokens)
	}
}
