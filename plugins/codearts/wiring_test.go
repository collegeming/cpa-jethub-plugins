package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file wires both fixes through the REAL executor handlers rather than
// calling the helpers directly, so a future refactor that drops the call from
// the handler (the exact shape of the original defect: the helper existed in
// spirit for DSML only, but the ordinary path never got it) fails here.
//
// The vendor is a fake host transport: it records the signed chat request body
// the plugin sends and answers with a canned SSE stream, which is enough to
// assert both directions of the conversation.

// streamTerminator ends every frame block of the canned SSE bodies.
const streamTerminator = "\n\n"

// wireHost is the fake transport for the executor wiring tests.
type wireHost struct {
	mu sync.Mutex
	// chatBodies records every upstream chat request body, decoded.
	chatBodies []map[string]any
	// stream is the SSE body the chat endpoint answers with.
	stream string
}

func (f *wireHost) bodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.chatBodies...)
}

// install registers the fake transport and returns a host handle.
func (f *wireHost) install(t *testing.T) *abiboot.Host {
	t.Helper()
	// The freshness state is process-wide, so every test starts clean.
	credentialRefresher = newCredentialRefresher()
	t.Cleanup(func() { credentialRefresher = newCredentialRefresher() })

	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case pluginabi.MethodHostHTTPDo:
			var payload struct {
				URL     string      `json:"url"`
				Body    []byte      `json:"body"`
				Headers http.Header `json:"headers"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			if strings.Contains(payload.URL, ChatAPIPath) {
				var body map[string]any
				if errDecode := json.Unmarshal(payload.Body, &body); errDecode != nil {
					return nil, errDecode
				}
				f.chatBodies = append(f.chatBodies, body)
				return abiboot.OK(map[string]any{
					"StatusCode": http.StatusOK,
					"Body":       base64.StdEncoding.EncodeToString([]byte(f.stream)),
				})
			}
			return nil, abiboot.Errorf("unexpected_url", "unexpected upstream call %s", payload.URL)
		case pluginabi.MethodHostLog:
			return abiboot.OK(map[string]any{})
		}
		return nil, abiboot.Errorf("unexpected_method", "unexpected host method %s", method)
	})
	t.Cleanup(abiboot.ClearHostCaller)
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"wire-callback"}`))
}

// wireCredential is a valid, non-expiring credential, so the executor never
// touches the renewal path.
func wireCredential(t *testing.T) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(&Credential{
		Type:            ProviderKey,
		AccessKeyID:     "WIREKEY",
		SecretAccessKey: "wire-secret",
		SecurityToken:   "wire-token",
		ExpiresAt:       time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339),
	})
	if errMarshal != nil {
		t.Fatalf("encode credential: %v", errMarshal)
	}
	return raw
}

// executorRequest builds the host request the plugin receives.
func executorWireRequest(t *testing.T, payload string) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(pluginapi.ExecutorRequest{
		AuthID:       "wire.json",
		AuthProvider: ProviderKey,
		Model:        "deepseek-v4-flash",
		StorageJSON:  wireCredential(t),
		Payload:      json.RawMessage(payload),
	})
	if errMarshal != nil {
		t.Fatalf("encode executor request: %v", errMarshal)
	}
	return raw
}

// TestExecuteStreamEmitsNonEmptyToolCallIDs is the end-to-end guard for Fix 1:
// a real upstream stream whose tool-call fragments carry NO id must leave the
// handler with every fragment carrying the same non-empty id.
func TestExecuteStreamEmitsNonEmptyToolCallIDs(t *testing.T) {
	host := &wireHost{stream: strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"pwsh","arguments":"{\"command\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"\"ls\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + streamTerminator}
	handle := host.install(t)

	value, errExecute := handleExecutorExecuteStream(handle, executorWireRequest(t,
		`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"list"}],"tools":[{"type":"function","function":{"name":"pwsh","parameters":{"type":"object"}}}]}`))
	if errExecute != nil {
		t.Fatalf("handleExecutorExecuteStream: %v", errExecute)
	}
	envelope, ok := value.(executorStreamResponse)
	if !ok {
		t.Fatalf("response = %#v, want executorStreamResponse", value)
	}

	ids := map[string]bool{}
	for _, chunk := range envelope.Chunks {
		for _, id := range frameToolCallIDs(t, []pluginapi.ExecutorStreamChunk{chunk})[0] {
			if strings.TrimSpace(id) == "" {
				t.Fatalf("handler emitted an empty tool call id: %s", chunk.Payload)
			}
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		t.Fatal("no tool-call fragment reached the client at all")
	}
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want ONE stable id across every fragment", ids)
	}
	if !ids["call_0"] {
		t.Fatalf("ids = %v, want the synthetic call_0", ids)
	}
}

// TestExecuteStreamDeclaresThinkingOff is the end-to-end guard for Fix 2: the
// handler must translate the client's `off` into the top-level switch on the
// body it SIGNS and sends, and must omit the key for every other effort.
func TestExecuteStreamDeclaresThinkingOff(t *testing.T) {
	cases := []struct {
		name         string
		effort       string
		wantDisabled bool
	}{
		{name: "off is declared", effort: "off", wantDisabled: true},
		{name: "none is declared", effort: "none", wantDisabled: true},
		{name: "on is omitted", effort: "on", wantDisabled: false},
		{name: "no effort is omitted", effort: "", wantDisabled: false},
		{name: "high is omitted", effort: "high", wantDisabled: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			host := &wireHost{stream: "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]" + streamTerminator}
			handle := host.install(t)

			payload := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
			if testCase.effort != "" {
				payload = `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + testCase.effort + `"}`
			}
			if _, errExecute := handleExecutorExecuteStream(handle, executorWireRequest(t, payload)); errExecute != nil {
				t.Fatalf("handleExecutorExecuteStream: %v", errExecute)
			}

			bodies := host.bodies()
			if len(bodies) != 1 {
				t.Fatalf("upstream chat calls = %d, want 1", len(bodies))
			}
			thinking, present := bodies[0]["thinking"]
			if !testCase.wantDisabled {
				if present {
					t.Fatalf("thinking = %#v, want the key absent", thinking)
				}
				return
			}
			switched, ok := thinking.(map[string]any)
			if !ok {
				t.Fatalf("thinking = %#v, want an object", thinking)
			}
			if switched["type"] != "disabled" {
				t.Fatalf("thinking.type = %v, want disabled", switched["type"])
			}
			if _, nested := bodies[0]["extra_body"]; nested {
				t.Fatal("the switch was written into the extra_body dialect instead of the top level")
			}
		})
	}
}

// TestExecuteNonStreamEmitsNonEmptyToolCallIDs is the end-to-end guard for the
// non-streaming half: handleExecutorExecute folds the stream itself and never
// runs the streaming rewrite, so it needs its own fallback.
func TestExecuteNonStreamEmitsNonEmptyToolCallIDs(t *testing.T) {
	host := &wireHost{stream: strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"pwsh","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + streamTerminator}
	handle := host.install(t)

	value, errExecute := handleExecutorExecute(handle, executorWireRequest(t,
		`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"list"}]}`))
	if errExecute != nil {
		t.Fatalf("handleExecutorExecute: %v", errExecute)
	}
	response, ok := value.(pluginapi.ExecutorResponse)
	if !ok {
		t.Fatalf("response = %#v, want pluginapi.ExecutorResponse", value)
	}

	var completion openaiCompletion
	if errUnmarshal := json.Unmarshal(response.Payload, &completion); errUnmarshal != nil {
		t.Fatalf("decode completion: %v", errUnmarshal)
	}
	calls := completion.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if strings.TrimSpace(calls[0].ID) == "" {
		t.Fatal("the completion carried an empty tool call id")
	}
	if calls[0].ID != "call_0" {
		t.Fatalf("id = %q, want the synthetic call_0", calls[0].ID)
	}
}

// openaiCompletion is the minimal read shape of the aggregated completion.
type openaiCompletion struct {
	Choices []struct {
		Message struct {
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}
