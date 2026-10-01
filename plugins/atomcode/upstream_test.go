package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The gateway path is where this adapter earns its keep, so these tests pin the
// three things a same-dialect proxy must not get wrong: the identity it presents,
// the framing it strips, and the silent rejection it must not pass through.

// TestExecutorSendsAdapterIdentityAndNotTheOfficialOne is the regression guard
// for the single most expensive trap in this provider.
//
// The gateway enforces the closed-source request signature *by User-Agent*: any
// `atomcode/<version>` identity is answered
// `403 {"detail":{"code":"ATOMCODE_SIG_MISSING"}}` while any other identity is
// served normally. Measured 2026-10-01 with one unchanged bearer token:
// `atomcode/5.2.0` -> 403, `cpa-jethub-atomcode/0.1.0` -> 200, on the same model
// and body. A future edit that "restores" the vendor fingerprint in the name of
// fidelity would take the whole channel down, and the failure would look like a
// credential problem.
func TestExecutorSendsAdapterIdentityAndNotTheOfficialOne(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"id":"x","choices":[{"message":{"content":"hi"}}]}`), nil
	}
	host.install(t)
	h := testHost()

	if _, errRun := runExecutorExecute(t, h, executorRequest(t,
		map[string]any{"model": "glm5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		mustJSON(t, sampleCredential(3600)))); errRun != nil {
		t.Fatalf("execute: %v", errRun)
	}

	calls := host.callsFor("chat/completions")
	if len(calls) != 1 {
		t.Fatalf("want 1 chat call, got %d", len(calls))
	}
	agent := calls[0].Headers.Get("User-Agent")
	if agent != AdapterUserAgent {
		t.Fatalf("User-Agent = %q, want the adapter's own %q", agent, AdapterUserAgent)
	}
	if strings.HasPrefix(strings.ToLower(agent), "atomcode/") {
		t.Fatalf("User-Agent %q claims to be the official client, which arms the signature gate", agent)
	}
	if got := calls[0].Headers.Get("Authorization"); got != "Bearer test-access-token" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestPrepareGatewayCallForwardsTheBodyVerbatim pins the advantage of a
// same-dialect upstream: nothing is translated, so nothing can be lost.
func TestPrepareGatewayCallForwardsTheBodyVerbatim(t *testing.T) {
	credential := sampleCredential(3600)
	request := executorRequest(t, map[string]any{
		"model":       "glm5.3-flash",
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"temperature": 0.25,
		"max_tokens":  1234,
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}},
		}},
		"reasoning_effort": "high",
	}, mustJSON(t, credential))

	call, errPrepare := prepareGatewayCall(request, credential, settings(), false)
	if errPrepare != nil {
		t.Fatalf("prepare: %v", errPrepare)
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(call.Body, &root); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	for key, want := range map[string]any{
		"temperature":      0.25,
		"max_tokens":       float64(1234),
		"reasoning_effort": "high",
		"model":            "glm5.3-flash",
	} {
		if root[key] != want {
			t.Fatalf("%s = %#v, want %#v", key, root[key], want)
		}
	}
	if _, ok := root["tools"]; !ok {
		t.Fatal("tools were dropped")
	}
	if root["stream"] != false {
		t.Fatalf("stream = %#v, want false on the buffered path", root["stream"])
	}
}

// TestPrepareGatewayCallForcesStreamAndAddsUsageFrames covers the two body edits
// the streaming path makes. The official client sends `stream_options` with
// `include_usage` unconditionally; an explicit caller choice still wins.
func TestPrepareGatewayCallForcesStreamAndAddsUsageFrames(t *testing.T) {
	credential := sampleCredential(3600)

	call, errPrepare := prepareGatewayCall(executorRequest(t,
		map[string]any{"model": "glm5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		mustJSON(t, credential)), credential, settings(), true)
	if errPrepare != nil {
		t.Fatalf("prepare: %v", errPrepare)
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(call.Body, &root); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if root["stream"] != true {
		t.Fatalf("stream = %#v, want true", root["stream"])
	}
	options, ok := root["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage true", root["stream_options"])
	}

	// An explicit caller preference is respected.
	explicit, errExplicit := prepareGatewayCall(executorRequest(t, map[string]any{
		"model":          "glm5.3-flash",
		"messages":       []any{map[string]any{"role": "user", "content": "hi"}},
		"stream_options": map[string]any{"include_usage": false},
	}, mustJSON(t, credential)), credential, settings(), true)
	if errExplicit != nil {
		t.Fatalf("prepare explicit: %v", errExplicit)
	}
	var explicitRoot map[string]any
	if errUnmarshal := json.Unmarshal(explicit.Body, &explicitRoot); errUnmarshal != nil {
		t.Fatalf("decode explicit: %v", errUnmarshal)
	}
	if got := explicitRoot["stream_options"].(map[string]any)["include_usage"]; got != false {
		t.Fatalf("caller's stream_options was overwritten: %#v", got)
	}
}

// TestPrepareGatewayCallStripsTheAccountPrefix checks that a model id decorated
// for routing reaches the gateway under its real name.
func TestPrepareGatewayCallStripsTheAccountPrefix(t *testing.T) {
	credential := sampleCredential(3600)
	prefix := modelPrefixFor(credential)
	if prefix == "" {
		t.Fatal("expected a non-empty prefix for this account id")
	}
	call, errPrepare := prepareGatewayCall(executorRequest(t,
		map[string]any{"model": prefix + "glm5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		mustJSON(t, credential)), credential, settings(), false)
	if errPrepare != nil {
		t.Fatalf("prepare: %v", errPrepare)
	}
	if call.Model != "glm5.3-flash" {
		t.Fatalf("model = %q, want the prefix stripped", call.Model)
	}
}

// TestConsumeUpstreamStreamDropsDoneAndEmptyToolCalls pins the two chunk
// normalisations. The host writes its own `data: [DONE]`, so forwarding the
// upstream's emits it twice; and an empty `tool_calls` array terminates the
// active reasoning segment in the Vercel AI SDK, which renders as one reasoning
// block per token.
func TestConsumeUpstreamStreamDropsDoneAndEmptyToolCalls(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think","tool_calls":[]}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"OK"}}]}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	outcome, errStream := consumeUpstreamStream([]byte(body))
	if errStream != nil {
		t.Fatalf("consume: %v", errStream)
	}
	if len(outcome.Payloads) != 2 {
		t.Fatalf("want 2 payloads, got %d: %q", len(outcome.Payloads), payloadStrings(outcome.Payloads))
	}
	for _, payload := range outcome.Payloads {
		text := string(payload)
		if strings.Contains(text, "data:") {
			t.Fatalf("payload still carries SSE framing: %q", text)
		}
		if strings.Contains(text, "[DONE]") {
			t.Fatalf("the upstream [DONE] was forwarded: %q", text)
		}
	}
	if strings.Contains(string(outcome.Payloads[0]), "tool_calls") {
		t.Fatalf("empty tool_calls survived sanitisation: %q", string(outcome.Payloads[0]))
	}
	if !outcome.SawContent {
		t.Fatal("SawContent = false although a content frame was present")
	}
}

// TestConsumeUpstreamStreamRejectsSilentParameterError covers the gateway's habit
// of answering HTTP 200 with `content:"参数错误"`. Passing it through would show
// the user a rejection message as if the model had said it.
func TestConsumeUpstreamStreamRejectsSilentParameterError(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"参数错误"},"finish_reason":"stop"}]}` + "\n\n"
	if _, errStream := consumeUpstreamStream([]byte(body)); errStream == nil {
		t.Fatal("want an error for the gateway's silent parameter rejection")
	} else if code, _ := errorCodeOf(errStream); code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", code)
	}
}

// TestExecuteRejectsSilentParameterError is the buffered half of the same rule.
func TestExecuteRejectsSilentParameterError(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK,
			`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"参数错误"},"finish_reason":"stop"}]}`), nil
	}
	host.install(t)

	_, errRun := runExecutorExecute(t, testHost(), executorRequest(t,
		map[string]any{"model": "no-such-model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		mustJSON(t, sampleCredential(3600))))
	if errRun == nil {
		t.Fatal("want an error for a 200 body carrying 参数错误")
	}
	code, _ := errorCodeOf(errRun)
	if code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", code)
	}
}

// TestUpstreamErrorNamesTheSignatureTrap checks that the one failure mode a user
// can actually fix by editing settings says so, instead of looking like a dead
// credential.
func TestUpstreamErrorNamesTheSignatureTrap(t *testing.T) {
	errMapped := upstreamError(http.StatusForbidden,
		`{"detail":{"code":"ATOMCODE_SIG_MISSING","message":"请升级到最新版 AtomCode 后再试。"}}`)
	code, _ := errorCodeOf(errMapped)
	if code != "signature_required" {
		t.Fatalf("error code = %q, want signature_required", code)
	}
	if !strings.Contains(errMapped.Error(), DefaultGatewayBase) {
		t.Fatalf("error should name the working gateway, got %q", errMapped.Error())
	}
}

// TestUpstreamErrorClassifiesAuthPlanAndRateLimit covers the remaining branches.
func TestUpstreamErrorClassifiesAuthPlanAndRateLimit(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized,
			`{"error":{"message":"Authentication Error, No api key passed in.","type":"auth_error","code":"401"}}`, "AUTH"},
		{"no plan", http.StatusForbidden,
			`{"error":{"message":"user has no codingplan","type":"auth_error","code":"403"}}`, "no_codingplan"},
		{"rate limited", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, "RATE_LIMIT"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			code, _ := errorCodeOf(upstreamError(testCase.status, testCase.body))
			if code != testCase.want {
				t.Fatalf("code = %q, want %q", code, testCase.want)
			}
		})
	}
}

// TestGatewayErrorMessageReadsBothEnvelopes covers the two error shapes the
// gateway family uses.
func TestGatewayErrorMessageReadsBothEnvelopes(t *testing.T) {
	if got := gatewayErrorCode(`{"detail":{"code":"ATOMCODE_SIG_MISSING"}}`); got != "ATOMCODE_SIG_MISSING" {
		t.Fatalf("detail envelope: %q", got)
	}
	if got := gatewayErrorMessage(`{"error":{"message":"user has no codingplan"}}`); got != "user has no codingplan" {
		t.Fatalf("error envelope: %q", got)
	}
	if got := gatewayErrorCode(`not json`); got != "" {
		t.Fatalf("non-JSON body produced %q", got)
	}
}

// payloadStrings renders payloads for a failure message.
func payloadStrings(payloads [][]byte) []string {
	out := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		out = append(out, string(payload))
	}
	return out
}

// TestThirdPartyFailureIsNotPassedOffAsAnAnswer covers the second shape the
// gateway uses for a listed-but-broken model.
//
// Measured 2026-10-01: `Qwen/Qwen3-4B-Instruct-2507` answered HTTP 200 whose
// entire content was `三方请求失败: 502 <html>…`. Without this guard that HTML
// error page becomes the assistant's turn — and inside an agent loop it becomes
// the model's next input.
func TestThirdPartyFailureIsNotPassedOffAsAnAnswer(t *testing.T) {
	body := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"三方请求失败: 502 <html>\r\n<head><title>502 Bad Gateway</title>"},"finish_reason":"stop"}]}`
	if !looksLikeParameterError([]byte(body)) {
		t.Fatal("a relayed third-party failure must be treated as a failure, not as model output")
	}
	if _, errStream := consumeUpstreamStream([]byte(
		`data: {"choices":[{"delta":{"content":"三方请求失败: 502 <html>"}}]}` + "\n\n")); errStream == nil {
		t.Fatal("the streaming path must reject it too")
	}

	// A model that merely TALKS about the phrase is not a failure.
	benign := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"The gateway reported 三方请求失败 to the caller."},"finish_reason":"stop"}]}`
	if looksLikeParameterError([]byte(benign)) {
		t.Fatal("the marker must only match at the start of the content")
	}
}
