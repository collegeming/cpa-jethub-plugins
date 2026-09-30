package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Streaming-framing tests.
//
// ## Why this is the highest-risk code in the plugin
//
// The plugin declares `anthropic` for BOTH its input and output formats, so the
// host passes Claude-shaped frames through LITERALLY on the way out
// (`sdk/api/handlers/claude/code_handlers.go:305-308`, the same path the native
// Claude executor feeds with complete `event:`/`data:` blocks at
// `internal/runtime/executor/claude_executor_stream.go:407-445`). When the client
// asked for something else, the host runs its own `claude → openai` translator over
// the same chunks, and that translator SKIPS any frame that does not start with
// `data:` (`internal/translator/claude/openai/chat-completions/claude_openai_response.go:101`).
//
// A framing mistake therefore does not raise an error anywhere: it produces an
// empty answer. The tests below pin the shape that both consumers need.

// capturedFixture is the recorded request/response pair under testdata/.
type capturedFixture struct {
	Description string `json:"description"`
	Request     struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    struct {
			Model        string           `json:"model"`
			MaxTokens    int              `json:"max_tokens"`
			Stream       bool             `json:"stream"`
			System       []map[string]any `json:"system"`
			Messages     []map[string]any `json:"messages"`
			OutputConfig map[string]any   `json:"output_config"`
		} `json:"body"`
	} `json:"request"`
	Response struct {
		Status      int    `json:"status"`
		ContentType string `json:"content_type"`
		SSE         string `json:"sse"`
	} `json:"response"`
}

// loadCapturedFixture reads the recorded pair.
//
// ⚠ The fixture is the recorded SHAPE, not a live token: every credential-bearing
// value in it is a placeholder. Nothing in this package can reach the network in
// the first place — the only transport is the host caller, which tests install as
// a fake — but embedding a real token would still be a leak, so it is asserted
// below.
func loadCapturedFixture(t *testing.T) capturedFixture {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", "captured_stream.json"))
	if errRead != nil {
		t.Fatalf("read fixture: %v", errRead)
	}
	var fixture capturedFixture
	if errUnmarshal := json.Unmarshal(raw, &fixture); errUnmarshal != nil {
		t.Fatalf("decode fixture: %v", errUnmarshal)
	}
	return fixture
}

// TestCapturedFixtureCarriesNoLiveCredential guards the fixture itself.
func TestCapturedFixtureCarriesNoLiveCredential(t *testing.T) {
	fixture := loadCapturedFixture(t)
	authorization := fixture.Request.Headers["Authorization"]
	if !strings.Contains(authorization, "<zcode_jwt>") {
		t.Fatalf("the fixture's Authorization header is %q; it must stay a placeholder", authorization)
	}
	// A JWT has three dot-separated segments; a real one in the fixture would be a
	// leak even though nothing here can use it.
	if strings.Count(authorization, ".") >= 2 {
		t.Fatal("the fixture appears to embed a real JWT")
	}
	if mid := fixture.Request.Headers["X-Device-Mid"]; !strings.Contains(mid, "<device_mid>") {
		t.Fatalf("the fixture's X-Device-Mid is %q; it must stay a placeholder", mid)
	}
}

// TestCapturedRequestShapeMatchesWhatThePluginBuilds compares the recorded request
// against the body the plugin actually produces for the same input.
//
// The recorded body carries shortened stand-ins for the identity text, so the
// comparison is structural: block COUNT and ORDER, the identity prefix, the
// environment block, the caller system in last position, the date block on the
// first user message, and the `output_config.effort` spelling.
func TestCapturedRequestShapeMatchesWhatThePluginBuilds(t *testing.T) {
	fixture := loadCapturedFixture(t)
	if len(fixture.Request.Body.System) != 4 {
		t.Fatalf("the fixture's system has %d blocks, want 4", len(fixture.Request.Body.System))
	}

	// Build the same request through the plugin's own path.
	payload, errMarshal := json.Marshal(map[string]any{
		"model":      fixture.Request.Body.Model,
		"max_tokens": fixture.Request.Body.MaxTokens,
		"stream":     fixture.Request.Body.Stream,
		"system":     "The caller's own system prompt.",
		"messages": []map[string]any{
			{"role": "user", "content": "Reply with the single word: pong"},
		},
		"output_config": map[string]any{"effort": "low"},
	})
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	body, errBody := buildInferenceBody(pluginapi.ExecutorRequest{
		Model: fixture.Request.Body.Model, Payload: payload,
	}, settings())
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var built map[string]any
	if errUnmarshal := json.Unmarshal(body, &built); errUnmarshal != nil {
		t.Fatalf("decode built body: %v", errUnmarshal)
	}

	// 1. `stream` is always true: upstream only streams, and the non-streaming
	//    route folds the same stream.
	if built["stream"] != true {
		t.Errorf("stream = %v, want true", built["stream"])
	}
	// 2. The identity block: four blocks in the recorded order.
	systemBlocks, ok := built["system"].([]any)
	if !ok {
		t.Fatalf("system is %T, want an array of blocks", built["system"])
	}
	if len(systemBlocks) != len(fixture.Request.Body.System) {
		t.Fatalf("system block count = %d, want %d", len(systemBlocks), len(fixture.Request.Body.System))
	}
	first, _ := systemBlocks[0].(map[string]any)
	if first["text"] != officialCLIPrefix {
		t.Errorf("block[0] = %v, want the CLI prefix", first["text"])
	}
	last, _ := systemBlocks[len(systemBlocks)-1].(map[string]any)
	if last["text"] != "The caller's own system prompt." {
		t.Errorf("the last block is not the caller's system: %v", last["text"])
	}
	// 3. The environment block sits third and names the model.
	third, _ := systemBlocks[2].(map[string]any)
	thirdText, _ := third["text"].(string)
	if !strings.HasPrefix(thirdText, "# Environment") {
		t.Errorf("block[2] is not the environment section: %v", third["text"])
	}
	if !strings.Contains(thirdText, "named zcode/"+fixture.Request.Body.Model) {
		t.Errorf("the environment block does not name the model: %s", thirdText)
	}
	// 4. The date block is the first content block of the first user message.
	messages, _ := built["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(messages))
	}
	message, _ := messages[0].(map[string]any)
	content, _ := message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("first user content blocks = %d, want 2 (date block + original text)", len(content))
	}
	dateBlock, _ := content[0].(map[string]any)
	dateText, _ := dateBlock["text"].(string)
	if !strings.HasPrefix(dateText, "<system-reminder>") || !strings.HasSuffix(dateText, "</system-reminder>") {
		t.Errorf("content[0] is not the date block: %q", dateText)
	}
	// 5. `output_config.effort` keeps its spelling and its value.
	outputConfig, _ := built["output_config"].(map[string]any)
	if outputConfig["effort"] != fixture.Request.Body.OutputConfig["effort"] {
		t.Errorf("output_config = %v, want the recorded %v", outputConfig, fixture.Request.Body.OutputConfig)
	}
}

// TestCapturedResponseFramesAreRelayedVerbatim covers the streaming relay against
// the recorded response.
func TestCapturedResponseFramesAreRelayedVerbatim(t *testing.T) {
	fixture := loadCapturedFixture(t)
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		// The relay must be reached through the real path, with the recorded
		// content type.
		if !strings.Contains(request.URL, MessagesPath) {
			t.Errorf("relay posted to %q, want the Anthropic messages path", request.URL)
		}
		return &pluginapi.HTTPResponse{
			StatusCode: fixture.Response.Status,
			Headers:    map[string][]string{"Content-Type": {fixture.Response.ContentType}},
			Body:       []byte(fixture.Response.SSE),
		}, nil
	}

	value, errStream := handleExecutorExecuteStream(testHost(),
		executorRawPayload(t, sampleCredential(), `{"model":"GLM-5.3-Flash","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
	if errStream != nil {
		t.Fatalf("execute_stream: %v", errStream)
	}
	response := decodeResult[executorStreamResponse](t, value)
	if len(response.Chunks) == 0 {
		t.Fatal("no chunks were produced")
	}

	// ⚠ Every chunk must be a COMPLETE Anthropic frame: `event: …`, a `data: …`
	// line, and a trailing blank line. The blank line is what makes the host's
	// stream forwarder emit the frame as its own SSE event.
	var joined strings.Builder
	for index, chunk := range response.Chunks {
		text := string(chunk.Payload)
		joined.Write(chunk.Payload)
		if !strings.HasPrefix(text, "event: ") {
			t.Errorf("chunk[%d] does not start with an `event: ` line: %q", index, truncate(text, 120))
		}
		if !strings.Contains(text, "\ndata: ") {
			t.Errorf("chunk[%d] has no `data: ` line: %q", index, truncate(text, 120))
		}
		if !strings.HasSuffix(text, "\n\n") {
			t.Errorf("chunk[%d] does not end with the blank line SSE needs: %q", index, truncate(text, 120))
		}
		if strings.HasSuffix(text, "\n\n\n") {
			t.Errorf("chunk[%d] has more than one trailing blank line: %q", index, truncate(text, 120))
		}
		if strings.Contains(text, "\r\n") {
			t.Errorf("chunk[%d] uses CRLF, which leaves a stray \\r inside the JSON once split on \\n", index)
		}
	}

	// Fidelity: everything the fixture sent came through, in order.
	relayed := joined.String()
	// Every EVENT name survives...
	for _, event := range []string{
		"message_start", "ping", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	} {
		if !strings.Contains(relayed, "event: "+event+"\n") {
			t.Errorf("the %s frame was dropped", event)
		}
	}
	// ...and so does every DELTA inside them: `thinking_delta` and
	// `signature_delta` are real deltas the client needs, not internal noise.
	for _, delta := range []string{"thinking_delta", "signature_delta", "text_delta"} {
		if !strings.Contains(relayed, `"type":"`+delta+`"`) {
			t.Errorf("the %s delta was dropped", delta)
		}
	}
	// The usage numbers survive intact.
	if !strings.Contains(relayed, `"input_tokens":3120`) {
		t.Error("the input token count was lost")
	}
	if !strings.Contains(relayed, `"output_tokens":24`) {
		t.Error("the output token count was lost")
	}
	// `ping` is relayed but carries no user-visible content.
	pingIndex := strings.Index(relayed, "event: ping\n")
	if pingIndex < 0 {
		t.Fatal("the ping frame is missing")
	}
	if strings.Contains(relayed[pingIndex:pingIndex+40], "text_delta") {
		t.Error("the ping frame was turned into visible content")
	}
}

// TestEveryRelayedFrameIsParseableJSON proves the payloads a downstream translator
// reads are well-formed.
//
// The host's `claude → openai` translator parses each `data:` line with gjson and
// silently produces nothing for an unparsable one, so a JSON error here would
// present as an empty answer rather than as a failure.
func TestEveryRelayedFrameIsParseableJSON(t *testing.T) {
	fixture := loadCapturedFixture(t)
	chunks, errRelay := relayAnthropicFrames([]byte(fixture.Response.SSE), "GLM-5.3-Flash")
	if errRelay != nil {
		t.Fatalf("relayAnthropicFrames: %v", errRelay)
	}
	for index, chunk := range chunks {
		event, data := parseSSEFrame(bytes.TrimRight(chunk.Payload, "\n"))
		if event == "" {
			t.Errorf("chunk[%d] has no event name", index)
		}
		if !json.Valid([]byte(data)) {
			t.Errorf("chunk[%d] carries invalid JSON: %q", index, truncate(data, 200))
		}
		// The inner `type` must match the event name: the translator dispatches on
		// `type` when it is present and falls back to the event only otherwise.
		if got := eventTypeOf(data); got != "" && got != event {
			t.Errorf("chunk[%d] event = %q but payload type = %q", index, event, got)
		}
	}
}

// TestRelayRejectsAnEmptyStream covers the "200 with nothing in it" case.
//
// That shape is how "this account has no entitlement for this model" presents
// itself — the request never reaches the model — so it is raised as a quota
// failure rather than handed to the client as an empty answer.
func TestRelayRejectsAnEmptyStream(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"an entirely empty body", "", "empty_upstream"},
		{"only whitespace", "   \n  ", "empty_upstream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errRelay := relayAnthropicFrames([]byte(tc.body), "GLM-5.3")
			if errRelay == nil {
				t.Fatal("expected an error")
			}
			envelope := &abiboot.EnvelopeError{}
			if !asEnvelope(errRelay, envelope) {
				t.Fatalf("error type = %T", errRelay)
			}
			if envelope.Code != tc.code {
				t.Errorf("code = %q, want %q", envelope.Code, tc.code)
			}
		})
	}
}

// TestRelayTreatsAContentlessStreamAsAQuotaFailure is the important half of the
// empty-stream rule.
//
// A stream carrying `message_start` and `message_stop` but NO content block is a
// 200 that the model never answered. The reference's measured signature for "no
// entitlement" is a fast empty answer, and reporting it as a quota failure — a 402,
// which the host does NOT retry — stops the client from burning five retries on a
// deterministic condition.
func TestRelayTreatsAContentlessStreamAsAQuotaFailure(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":9}}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	_, errRelay := relayAnthropicFrames([]byte(body), "GLM-5.3")
	if errRelay == nil {
		t.Fatal("a contentless stream was accepted as a successful answer")
	}
	envelope := &abiboot.EnvelopeError{}
	if !asEnvelope(errRelay, envelope) {
		t.Fatalf("error type = %T", errRelay)
	}
	if envelope.Code != "quota_exhausted" {
		t.Errorf("code = %q, want quota_exhausted", envelope.Code)
	}
	if envelope.HTTPStatus != http.StatusPaymentRequired {
		t.Errorf("http status = %d, want 402 so the host rotates instead of retrying", envelope.HTTPStatus)
	}
	if envelope.Retryable {
		t.Error("the failure is marked retryable, but a missing entitlement is deterministic")
	}
	if !strings.Contains(envelope.Message, "GLM-5.3") {
		t.Errorf("the message does not name the model: %q", envelope.Message)
	}
}

// TestRelayRaisesAnErrorFrame covers the `error` event.
//
// ⚠ Ignoring it is the "clean stop with no output and no error" defect: the client
// sees a stream that simply ended.
func TestRelayRaisesAnErrorFrame(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantStatus  int
		wantMessage string
	}{
		{
			name: "an overloaded upstream stays retryable",
			body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
			// 429: a momentary condition, so a retry is right.
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "Overloaded",
		},
		{
			name: "any other error is a server failure",
			body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"boom\"}}\n\n",
			// 502 so the host treats it as an upstream fault rather than the
			// request's own.
			wantStatus:  http.StatusBadGateway,
			wantMessage: "boom",
		},
		{
			name:        "an error type inside a normal data frame is caught too",
			body:        "data: {\"type\":\"error\",\"error\":{\"message\":\"from the payload\"}}\n\n",
			wantStatus:  http.StatusBadGateway,
			wantMessage: "from the payload",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errRelay := relayAnthropicFrames([]byte(tc.body), "GLM-5.3")
			if errRelay == nil {
				t.Fatal("an error frame was swallowed")
			}
			envelope := &abiboot.EnvelopeError{}
			if !asEnvelope(errRelay, envelope) {
				t.Fatalf("error type = %T", errRelay)
			}
			if envelope.HTTPStatus != tc.wantStatus {
				t.Errorf("http status = %d, want %d", envelope.HTTPStatus, tc.wantStatus)
			}
			if !strings.Contains(envelope.Message, tc.wantMessage) {
				t.Errorf("message = %q, want it to carry %q", envelope.Message, tc.wantMessage)
			}
		})
	}
}

// TestSplitSSEFramesHandlesBothLineEndings covers the boundary search.
//
// ⚠ A CRLF stream CONTAINS `\n\n` inside `\r\n\r\n` (bytes 2-3), so testing the LF
// separator first splits in the wrong place and leaves a leading `\r` on the next
// frame — which then ends up inside a JSON string value.
func TestSplitSSEFramesHandlesBothLineEndings(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		count int
	}{
		{"LF separated", "event: a\ndata: {}\n\nevent: b\ndata: {}\n\n", 2},
		{"CRLF separated", "event: a\r\ndata: {}\r\n\r\nevent: b\r\ndata: {}\r\n\r\n", 2},
		{"mixed", "event: a\ndata: {}\n\r\nevent: b\r\ndata: {}\n\n", 2},
		{"a trailing frame without a blank line", "event: a\ndata: {}", 1},
		{"empty blank runs are skipped", "\n\n\n\nevent: a\ndata: {}\n\n\n\n", 1},
		{"an empty body", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := splitSSEFrames([]byte(tc.body))
			if len(frames) != tc.count {
				t.Fatalf("frame count = %d, want %d (%q)", len(frames), tc.count, frames)
			}
			for index, frame := range frames {
				if strings.HasPrefix(string(frame), "\r") || strings.HasPrefix(string(frame), "\n") {
					t.Errorf("frame[%d] starts with a stray line ending: %q", index, frame)
				}
				if strings.Contains(string(frame), "\r") {
					t.Errorf("frame[%d] still carries a carriage return: %q", index, frame)
				}
			}
		})
	}
}

// TestParseSSEFrameReadsEventAndJoinedData covers the frame parser.
func TestParseSSEFrameReadsEventAndJoinedData(t *testing.T) {
	cases := []struct {
		name      string
		frame     string
		wantEvent string
		wantData  string
	}{
		{"a normal frame", "event: content_block_delta\ndata: {\"a\":1}", "content_block_delta", `{"a":1}`},
		{"no space after the colon", "event:x\ndata:{\"a\":1}", "x", `{"a":1}`},
		{"multiple data lines are joined with a newline", "event: x\ndata: line1\ndata: line2", "x", "line1\nline2"},
		{"a data-only frame", "data: {\"a\":1}", "", `{"a":1}`},
		{"a comment line is ignored", ": keep-alive\ndata: {}", "", "{}"},
		{"an event-only frame", "event: ping", "ping", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, data := parseSSEFrame([]byte(tc.frame))
			if event != tc.wantEvent {
				t.Errorf("event = %q, want %q", event, tc.wantEvent)
			}
			if data != tc.wantData {
				t.Errorf("data = %q, want %q", data, tc.wantData)
			}
		})
	}
}

// TestRelayStopsAtAnOpenAIStyleDoneMarker covers a gateway that appends `[DONE]`.
//
// `[DONE]` is not part of the Anthropic protocol, but the host's OpenAI path appends
// one, so tolerating it on the way IN costs nothing and stops a stray marker from
// reaching a client as an unparsable frame.
func TestRelayStopsAtAnOpenAIStyleDoneMarker(t *testing.T) {
	body := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: [DONE]\n\n"
	chunks, errRelay := relayAnthropicFrames([]byte(body), "GLM-5.3")
	if errRelay != nil {
		t.Fatalf("relayAnthropicFrames: %v", errRelay)
	}
	for _, chunk := range chunks {
		if strings.Contains(string(chunk.Payload), "[DONE]") {
			t.Fatalf("the [DONE] marker was relayed: %q", chunk.Payload)
		}
	}
}

// TestRelayKeepsAPartialFinalFrame covers a body that does not end with a blank
// line, which is what a truncated-but-valid stream looks like.
func TestRelayKeepsAPartialFinalFrame(t *testing.T) {
	body := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}"
	chunks, errRelay := relayAnthropicFrames([]byte(body), "GLM-5.3")
	if errRelay != nil {
		t.Fatalf("relayAnthropicFrames: %v", errRelay)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunk count = %d, want 2", len(chunks))
	}
	// The final frame still has to be terminated, or the forwarder never emits it.
	if !strings.HasSuffix(string(chunks[1].Payload), "\n\n") {
		t.Fatalf("the final frame is not terminated: %q", chunks[1].Payload)
	}
}

// TestNonStreamFoldingProducesAnAnthropicMessage covers `executor.execute`.
//
// Upstream only streams, so the non-streaming route folds the SSE into ONE message
// object — the shape `sdk/translator` consumes for a non-stream Claude response
// (`ConvertClaudeResponseToOpenAINonStream` splits the body on newlines and reads
// each `data:` line).
func TestNonStreamFoldingProducesAnAnthropicMessage(t *testing.T) {
	fixture := loadCapturedFixture(t)
	message, errFold := foldAnthropicStream([]byte(fixture.Response.SSE), "GLM-5.3-Flash")
	if errFold != nil {
		t.Fatalf("foldAnthropicStream: %v", errFold)
	}

	if message.Type != "message" || message.Role != "assistant" {
		t.Errorf("type/role = %q/%q, want message/assistant", message.Type, message.Role)
	}
	if message.ID != "msg_01ZCODEFIXTURE0000000000000" {
		t.Errorf("id = %q, want the upstream message id", message.ID)
	}
	if message.Model != "GLM-5.3-Flash" {
		t.Errorf("model = %q", message.Model)
	}
	if message.StopReason == nil || *message.StopReason != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", message.StopReason)
	}
	// Thinking first, then text — the order the stream produced them.
	if len(message.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (thinking + text)", len(message.Content))
	}
	if message.Content[0].Type != "thinking" {
		t.Errorf("block[0] type = %q, want thinking", message.Content[0].Type)
	}
	if message.Content[0].Thinking != "The user asks for a single word." {
		t.Errorf("thinking text = %q", message.Content[0].Thinking)
	}
	// ⚠ The signature has to survive: a later turn replays the thinking block, and
	// one whose signature was dropped fails validation upstream.
	if message.Content[0].Signature != "EqQBCgIYAhIM1gbcDa9GJwZA2b3lGAIqD" {
		t.Errorf("signature = %q, want it preserved", message.Content[0].Signature)
	}
	if message.Content[1].Type != "text" || message.Content[1].Text != "pong" {
		t.Errorf("block[1] = %#v, want the text block", message.Content[1])
	}
	// Usage is read from BOTH events.
	if message.Usage.InputTokens != 3120 {
		t.Errorf("input_tokens = %d, want 3120 (from message_start)", message.Usage.InputTokens)
	}
	if message.Usage.OutputTokens != 24 {
		t.Errorf("output_tokens = %d, want 24 (from message_delta)", message.Usage.OutputTokens)
	}
}

// TestFoldingNeverLosesInputTokensToTheLaterUsageEvent is the regression for the
// split-usage shape.
//
// Anthropic publishes usage in TWO places, each carrying different members. An
// implementation that assigns instead of merging lets the later `message_delta`
// (which has no `input_tokens`) wipe the input count `message_start` supplied.
func TestFoldingNeverLosesInputTokensToTheLaterUsageEvent(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":500,\"cache_read_input_tokens\":40,\"cache_creation_input_tokens\":12}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n"

	message, errFold := foldAnthropicStream([]byte(body), "GLM-5.3")
	if errFold != nil {
		t.Fatalf("foldAnthropicStream: %v", errFold)
	}
	if message.Usage.InputTokens != 500 {
		t.Errorf("input_tokens = %d, want 500 (the later event must not clear it)", message.Usage.InputTokens)
	}
	if message.Usage.OutputTokens != 20 {
		t.Errorf("output_tokens = %d, want 20", message.Usage.OutputTokens)
	}
	if message.Usage.CacheReadInputTokens != 40 || message.Usage.CacheCreationInputTokens != 12 {
		t.Errorf("cache counters = %d/%d, want 40/12",
			message.Usage.CacheReadInputTokens, message.Usage.CacheCreationInputTokens)
	}
}

// TestFoldingAssemblesToolUseBlocks covers the tool-call path.
//
// ⚠ `tool_use.input` is an OBJECT, not the JSON string OpenAI uses, and the
// arguments arrive as `input_json_delta` fragments that have to be concatenated.
func TestFoldingAssemblesToolUseBlocks(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read_file\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"/tmp/a\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":30}}\n\n"

	message, errFold := foldAnthropicStream([]byte(body), "GLM-5.3")
	if errFold != nil {
		t.Fatalf("foldAnthropicStream: %v", errFold)
	}
	if len(message.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(message.Content))
	}
	block := message.Content[0]
	if block.Type != "tool_use" || block.ID != "toolu_1" || block.Name != "read_file" {
		t.Fatalf("tool block = %#v", block)
	}
	// The members must be valid JSON with the fragments joined.
	if !json.Valid(block.Input) {
		t.Fatalf("tool input is not valid JSON: %s", block.Input)
	}
	var input map[string]any
	if errUnmarshal := json.Unmarshal(block.Input, &input); errUnmarshal != nil {
		t.Fatalf("decode tool input: %v", errUnmarshal)
	}
	if input["path"] != "/tmp/a" {
		t.Errorf("tool input = %v, want the concatenated fragments", input)
	}
}

// TestFoldingTreatsTruncatedToolArgumentsAsAnEmptyObject covers the malformed
// fragment rule: an incomplete payload is NOT invented, it becomes `{}` so the
// tool-call contract surfaces as a schema error the caller can retry.
func TestFoldingTreatsTruncatedToolArgumentsAsAnEmptyObject(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"read_file\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	message, errFold := foldAnthropicStream([]byte(body), "GLM-5.3")
	if errFold != nil {
		t.Fatalf("foldAnthropicStream: %v", errFold)
	}
	if len(message.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(message.Content))
	}
	if string(message.Content[0].Input) != "{}" {
		t.Fatalf("input = %s, want {}", message.Content[0].Input)
	}
}

// TestFoldingSkipsEmptyContentBlocks covers the "no empty blocks" rule: an empty
// block pollutes a conversation.
func TestFoldingSkipsEmptyContentBlocks(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"real\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n"
	message, errFold := foldAnthropicStream([]byte(body), "GLM-5.3")
	if errFold != nil {
		t.Fatalf("foldAnthropicStream: %v", errFold)
	}
	if len(message.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1 (the empty one must be dropped)", len(message.Content))
	}
	if message.Content[0].Text != "real" {
		t.Errorf("the surviving block = %#v", message.Content[0])
	}
}

// TestFoldingReportsAContentlessStream covers the non-streaming half of the quota
// detection.
func TestFoldingReportsAContentlessStream(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	_, errFold := foldAnthropicStream([]byte(body), "GLM-5.3")
	if errFold == nil {
		t.Fatal("a contentless stream folded into a successful message")
	}
	envelope := &abiboot.EnvelopeError{}
	if !asEnvelope(errFold, envelope) {
		t.Fatalf("error type = %T", errFold)
	}
	if envelope.Code != "quota_exhausted" || envelope.HTTPStatus != http.StatusPaymentRequired {
		t.Fatalf("code/status = %q/%d, want quota_exhausted/402", envelope.Code, envelope.HTTPStatus)
	}
}

// TestMarshalCompactProducesOneLine is the shape the host's non-stream converter
// needs: it splits the body on newlines and reads each `data:` line, so a
// pretty-printed body would have only its first line recognised.
func TestMarshalCompactProducesOneLine(t *testing.T) {
	message := &anthropicMessage{
		ID: "m1", Type: "message", Role: "assistant", Model: "GLM-5.3",
		Content:    []anthropicContent{{Type: "text", Text: "line one\nline two"}},
		StopReason: nil,
	}
	encoded, errMarshal := marshalCompact(message)
	if errMarshal != nil {
		t.Fatalf("marshalCompact: %v", errMarshal)
	}
	if bytes.Contains(encoded, []byte("\n")) {
		t.Fatalf("the output contains a raw newline, so only its first line would be read: %s", encoded)
	}
	// The embedded newline inside the text is escaped, not dropped.
	if !json.Valid(encoded) {
		t.Fatalf("the output is not valid JSON: %s", encoded)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(encoded, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded["model"] != "GLM-5.3" {
		t.Errorf("model = %v", decoded["model"])
	}
}

// asEnvelope is a small helper for the error assertions above.
func asEnvelope(err error, target *abiboot.EnvelopeError) bool {
	typed, ok := err.(*abiboot.EnvelopeError)
	if !ok || typed == nil {
		return false
	}
	*target = *typed
	return true
}
