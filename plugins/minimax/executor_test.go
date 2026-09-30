package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

// Executor and protocol-layer tests: the Anthropic body the HOST builds, the
// provider-specific defensive rewrite, the thinking matrix, SSE consumption,
// usage preservation, framing, and error classification.
//
// The body-builder tests deliberately invoke the CPA SDK's own registered
// translator rather than duplicating it in this plugin. That is the design this
// provider declares (`anthropic` in and out): proving what the host hands the
// executor is stronger than proving a local helper we would never call.

func TestHostAnthropicTranslatorBuildsMeasuredBody(t *testing.T) {
	openAI := []byte(`{
		"model":"MiniMax-M3.1-Flash-Preview",
		"messages":[
			{"role":"system","content":"你是助手"},
			{"role":"user","content":[
				{"type":"text","text":"看图"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
			]},
			{"role":"assistant","reasoning_content":"不要回传这段历史思考",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"晴"},
			{"role":"user","content":"继续"}
		],
		"tools":[{"type":"function","function":{"name":"weather","description":"查天气",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
		"max_tokens":512,"temperature":0.2,"stop":["END"],"reasoning_effort":"max"
	}`)

	translated := translateOpenAIToClaude(t, "MiniMax-M3.1-Flash-Preview", openAI, true)
	body := decodeJSONMap(t, translated)

	if body["model"] != "MiniMax-M3.1-Flash-Preview" {
		t.Errorf("model = %v", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	// ⚠️ system is top-level, never a system-role message.
	system := body["system"]
	if system == nil {
		t.Fatal("translated body has no top-level system field")
	}
	messages, okMessages := body["messages"].([]any)
	if !okMessages {
		t.Fatalf("messages = %#v, want array", body["messages"])
	}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] == "system" {
			t.Errorf("system prompt leaked into messages: %#v", message)
		}
		if message["role"] == "tool" {
			t.Errorf("Anthropic Messages has no role=tool: %#v", message)
		}
	}
	if body["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v, want 512", body["max_tokens"])
	}
	stop, _ := body["stop_sequences"].([]any)
	if !reflect.DeepEqual(stop, []any{"END"}) {
		t.Errorf("stop_sequences = %#v, want [END]", stop)
	}

	// Tools are Anthropic-flat: name / description / input_schema. There is no
	// OpenAI function wrapper in the provider body.
	tools, okTools := body["tools"].([]any)
	if !okTools || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one tool", body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "weather" || tool["description"] != "查天气" {
		t.Errorf("tool = %#v", tool)
	}
	if _, hasFunction := tool["function"]; hasFunction {
		t.Errorf("tool carries an OpenAI function wrapper: %#v", tool)
	}
	if _, hasSchema := tool["input_schema"]; !hasSchema {
		t.Errorf("tool has no input_schema: %#v", tool)
	}

	// Image shape: Anthropic base64 with BARE data, not image_url and not a
	// data: prefix.
	image := findContentBlock(messages, "image")
	if image == nil {
		t.Fatalf("translated messages have no image block: %s", translated)
	}
	source, _ := image["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "aGVsbG8=" {
		t.Errorf("image source = %#v, want base64/image/png/bare data", source)
	}
	if strings.HasPrefix(fmtString(source["data"]), "data:") {
		t.Errorf("image source.data has a data: prefix: %#v", source)
	}

	// Assistant tool call is a tool_use block and its input is an OBJECT.
	toolUse := findContentBlock(messages, "tool_use")
	if toolUse == nil {
		t.Fatalf("translated messages have no tool_use block: %s", translated)
	}
	input, okInput := toolUse["input"].(map[string]any)
	if !okInput || input["city"] != "北京" {
		t.Errorf("tool_use.input = %#v, want object {city: 北京}", toolUse["input"])
	}

	// Tool result is inside a USER message with tool_use_id; there is no
	// role=tool in Anthropic Messages.
	toolResult := findContentBlock(messages, "tool_result")
	if toolResult == nil || toolResult["tool_use_id"] != "call_1" {
		t.Errorf("tool_result = %#v, want tool_use_id=call_1", toolResult)
	}

	// Historical reasoning must NOT be echoed back: the host translator emits
	// unsigned thinking only under its explicit compatibility path; this plugin
	// uses the normal route, and the body must not contain the historical text.
	if strings.Contains(string(translated), "不要回传这段历史思考") {
		t.Errorf("historical reasoning was echoed into the Anthropic body: %s", translated)
	}

	// The translator derives a Claude thinking object from reasoning_effort.
	// This is the exact reason the plugin cannot trust the body blindly: depending
	// on the host's current model metadata it may be the legacy enabled/budget
	// shape rather than adaptive. The provider-side rewrite below owns the M3.1
	// hard requirement.
	thinking, _ := body["thinking"].(map[string]any)
	if stringField(thinking, "type") == "" {
		t.Errorf("reasoning_effort produced no thinking object: %#v", body)
	}
}

// TestHostAnthropicTranslatorInvalidToolInputFallsBackToObject locks the rule
// that a tool_use block is kept even when the original arguments cannot be
// parsed. The reference specifies `{}` as the fallback: dropping the block
// would make the later tool_result an orphan and the vendor would answer 400.
func TestHostAnthropicTranslatorInvalidToolInputFallsBackToObject(t *testing.T) {
	openAI := []byte(`{"model":"MiniMax-M3","messages":[
		{"role":"user","content":"查天气"},
		{"role":"assistant","tool_calls":[{"id":"call_bad","type":"function",
			"function":{"name":"weather","arguments":"not-json"}}]},
		{"role":"tool","tool_call_id":"call_bad","content":"晴"}
	]}`)
	translated := translateOpenAIToClaude(t, "MiniMax-M3", openAI, true)
	body := decodeJSONMap(t, translated)
	messages := body["messages"].([]any)
	toolUse := findContentBlock(messages, "tool_use")
	if toolUse == nil {
		t.Fatalf("invalid arguments dropped the entire tool_use block: %s", translated)
	}
	input, okInput := toolUse["input"].(map[string]any)
	if !okInput || len(input) != 0 {
		t.Errorf("invalid tool input = %#v, want empty object", toolUse["input"])
	}
	if result := findContentBlock(messages, "tool_result"); result == nil || result["tool_use_id"] != "call_bad" {
		t.Errorf("tool_result was dropped or orphaned: %#v", result)
	}
}

// TestMessagesRequestBodyThinkingMatrix covers the measured per-model request
// rule and the mandatory M3.1 `disabled` → `adaptive` rewrite.
func TestMessagesRequestBodyThinkingMatrix(t *testing.T) {
	tests := []struct {
		name           string
		model          string
		incoming       string
		wantType       string
		wantAbsent     bool
		wantEffort     string
		wantNoBudget   bool
		wantByteStable bool
	}{
		{
			name:  "M3.1 rewrites disabled to adaptive and keeps the selected effort",
			model: "MiniMax-M3.1-Flash-Preview",
			incoming: `{"model":"wrong","messages":[{"role":"user","content":"hi"}],
				"thinking":{"type":"disabled","budget_tokens":1024},"output_config":{"effort":"max"}}`,
			wantType: "adaptive", wantEffort: "max", wantNoBudget: true,
		},
		{
			name:     "M3.1 with no thinking field gets adaptive",
			model:    "MiniMax-M3.1-Future",
			incoming: `{"messages":[{"role":"user","content":"hi"}]}`,
			wantType: "adaptive",
		},
		{
			name:     "M3 accepts disabled",
			model:    "MiniMax-M3",
			incoming: `{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
			wantType: "disabled",
		},
		{
			name:     "M3 adaptive is kept",
			model:    "MiniMax-M3",
			incoming: `{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`,
			wantType: "adaptive",
		},
		{
			name:       "M3 omission stays omission, which means no thinking",
			model:      "MiniMax-M3",
			incoming:   `{"messages":[{"role":"user","content":"hi"}]}`,
			wantAbsent: true,
		},
		{
			name:       "M2.7 sends nothing and lets the forced-on server think",
			model:      "MiniMax-M2.7",
			incoming:   `{"messages":[{"role":"user","content":"hi"}]}`,
			wantAbsent: true,
		},
		{
			name:       "M2.7-highspeed sends nothing",
			model:      "MiniMax-M2.7-highspeed",
			incoming:   `{"messages":[{"role":"user","content":"hi"}]}`,
			wantAbsent: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			body, errBody := messagesRequestBody(pluginapi.ExecutorRequest{
				Model: testCase.model, Payload: []byte(testCase.incoming),
			})
			if errBody != nil {
				t.Fatalf("messagesRequestBody: %v", errBody)
			}
			root := decodeJSONMap(t, body)
			if root["model"] != testCase.model {
				t.Errorf("model = %v, want host-resolved %q", root["model"], testCase.model)
			}
			if root["stream"] != true {
				t.Errorf("stream = %v, want true", root["stream"])
			}
			thinking, present := root["thinking"].(map[string]any)
			if testCase.wantAbsent {
				if present {
					t.Errorf("thinking unexpectedly present: %#v", thinking)
				}
				return
			}
			if !present {
				t.Fatalf("thinking absent, want type=%q: %s", testCase.wantType, body)
			}
			if got := thinking["type"]; got != testCase.wantType {
				t.Errorf("thinking.type = %v, want %q", got, testCase.wantType)
			}
			if testCase.wantNoBudget {
				if _, presentBudget := thinking["budget_tokens"]; presentBudget {
					t.Errorf("rewritten thinking kept budget_tokens: %#v", thinking)
				}
			}
			if testCase.wantEffort != "" {
				outputConfig, _ := root["output_config"].(map[string]any)
				if got := outputConfig["effort"]; got != testCase.wantEffort {
					t.Errorf("output_config.effort = %v, want %q", got, testCase.wantEffort)
				}
			}
		})
	}
}

// TestDefensiveRewriteIsSpecificToTheM31Prefix makes the prefix distinction
// explicit: `MiniMax-M3.1` matches and `MiniMax-M3` does not.
func TestDefensiveRewriteIsSpecificToTheM31Prefix(t *testing.T) {
	incoming := []byte(`{"thinking":{"type":"disabled"},"messages":[]}`)
	m31 := decodeJSONMap(t, rewriteAdaptiveThinking("MiniMax-M3.1", incoming))
	if m31["thinking"].(map[string]any)["type"] != "adaptive" {
		t.Errorf("M3.1 disabled was not rewritten: %#v", m31)
	}
	m3 := decodeJSONMap(t, rewriteAdaptiveThinking("MiniMax-M3", incoming))
	if m3["thinking"].(map[string]any)["type"] != "disabled" {
		t.Errorf("M3 disabled was incorrectly rewritten: %#v", m3)
	}
}

func TestPreserveOriginalTemperature(t *testing.T) {
	tests := []struct {
		name     string
		root     map[string]any
		original string
		want     any
		present  bool
	}{
		{"copies a numeric OpenAI temperature", map[string]any{}, `{"temperature":0.2}`, float64(0.2), true},
		{"does not overwrite the translated value", map[string]any{"temperature": float64(0.7)}, `{"temperature":0.2}`, float64(0.7), true},
		{"ignores a string temperature", map[string]any{}, `{"temperature":"hot"}`, nil, false},
		{"ignores malformed original JSON", map[string]any{}, `oops`, nil, false},
		{"ignores an absent temperature", map[string]any{}, `{}`, nil, false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			preserveOriginalTemperature(testCase.root, []byte(testCase.original))
			got, present := testCase.root["temperature"]
			if present != testCase.present || !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("temperature = %#v, present=%v; want %#v, present=%v",
					got, present, testCase.want, testCase.present)
			}
		})
	}
}

func TestHostBodyCarriesTopLevelTemperatureThroughPlugin(t *testing.T) {
	openAI := []byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`)
	translated := translateOpenAIToClaude(t, "MiniMax-M3", openAI, true)
	body, errBody := messagesRequestBody(pluginapi.ExecutorRequest{
		Model: "MiniMax-M3", Payload: translated, OriginalRequest: openAI,
	})
	if errBody != nil {
		t.Fatalf("messagesRequestBody: %v", errBody)
	}
	root := decodeJSONMap(t, body)
	if root["temperature"] != float64(0.2) {
		t.Errorf("temperature = %#v, want 0.2", root["temperature"])
	}
}

func TestInferenceHeadersAreExactlyTheMeasuredSet(t *testing.T) {
	headers := inferHeaders(&Credential{AccessToken: "mmoat_secret"})
	if headers.Get("Authorization") != "Bearer mmoat_secret" {
		t.Errorf("Authorization = %q", headers.Get("Authorization"))
	}
	if headers.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", headers.Get("Content-Type"))
	}
	if headers.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept = %q", headers.Get("Accept"))
	}
	if got := headers.Get("anthropic-version"); got != "" {
		t.Errorf("anthropic-version = %q, want absent (measured working without it)", got)
	}
	if len(headers) != 3 {
		t.Errorf("header count = %d, want exactly 3: %#v", len(headers), headers)
	}
}

func TestSSEConsumerIgnoresSignatureAndPreservesUsage(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"MiniMax-M3.1-Flash-Preview","content":[],"usage":{"input_tokens":11,"cache_read_input_tokens":3}}}`,
		``,
		`event: ping`,
		`data: {"type":"ping"}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想一想"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"deadbeef"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":17,"output_tokens_details":{"thinking_tokens":7}}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	consumer := newSSEConsumer()
	if _, errConsume := consumer.consume([]byte(stream)); errConsume != nil {
		t.Fatalf("consume: %v", errConsume)
	}
	frames, errFinish := consumer.finishFrames()
	if errFinish != nil {
		t.Fatalf("finishFrames: %v", errFinish)
	}
	var concatenated []byte
	for _, chunk := range frames {
		concatenated = append(concatenated, chunk.Payload...)
	}
	text := string(concatenated)
	if strings.Contains(text, "signature_delta") || strings.Contains(text, "deadbeef") {
		t.Errorf("signature_delta leaked into the forwarded stream: %s", text)
	}
	if !strings.Contains(text, `"thinking":"想一想"`) {
		t.Errorf("thinking delta was lost: %s", text)
	}
	if !strings.Contains(text, `"text":"答案"`) {
		t.Errorf("text delta was lost: %s", text)
	}
	if !strings.Contains(text, `"input_tokens":11`) || !strings.Contains(text, `"output_tokens":17`) {
		t.Errorf("usage was lost: %s", text)
	}
	if !strings.Contains(text, `"thinking_tokens":7`) {
		t.Errorf("thinking_tokens detail was lost: %s", text)
	}
	// thinking_tokens is a SUBSET of output_tokens and must not be added to it.
	if strings.Contains(text, `"output_tokens":24`) {
		t.Errorf("thinking_tokens was added to output_tokens: %s", text)
	}
	if !strings.Contains(text, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Errorf("complete Anthropic SSE framing was not reconstructed: %q", text)
	}
	if errFrames := consumer.framesAreDataPrefixed(); errFrames != nil {
		t.Error(errFrames)
	}
}

func TestSSEConsumerEmptyStreamIsError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"truly empty", ``},
		{"pings only", "event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		{"message start then stop with no content", strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n")},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			consumer := newSSEConsumer()
			if _, errConsume := consumer.consume([]byte(testCase.body)); errConsume != nil {
				t.Fatalf("consume: %v", errConsume)
			}
			if _, errFinish := consumer.finishFrames(); errFinish == nil {
				t.Fatal("finishFrames accepted an empty stream")
			} else if !strings.Contains(errFinish.Error(), "未返回任何内容块") {
				t.Errorf("empty stream error = %v", errFinish)
			}
		})
	}
}

// TestSSEConsumerFlushesTruncatedTail is the regression that caught a real
// defect in the reference: a stream that closes before its final newline leaves
// the last `message_delta` in the buffer, and that frame is the ONLY place
// stop_reason and output usage live.
func TestSSEConsumerFlushesTruncatedTail(t *testing.T) {
	stream := "event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: message_delta\n" +
		// ⚠️ no trailing newline on purpose
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":42}}"
	consumer := newSSEConsumer()
	if _, errConsume := consumer.consume([]byte(stream)); errConsume != nil {
		t.Fatalf("consume: %v", errConsume)
	}
	frames, errFinish := consumer.finishFrames()
	if errFinish != nil {
		t.Fatalf("finishFrames: %v", errFinish)
	}
	var out []byte
	for _, frame := range frames {
		out = append(out, frame.Payload...)
	}
	text := string(out)
	if !strings.Contains(text, `"stop_reason":"max_tokens"`) {
		t.Errorf("truncated message_delta was lost: %s", text)
	}
	if !strings.Contains(text, `"output_tokens":42`) {
		t.Errorf("truncated usage was lost: %s", text)
	}
}

func TestSSEConsumerErrorEventIsNotSilent(t *testing.T) {
	consumer := newSSEConsumer()
	stream := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"boom\"}}\n\n"
	if _, errConsume := consumer.consume([]byte(stream)); errConsume == nil {
		t.Fatal("consume accepted a stream error event")
	} else if !strings.Contains(errConsume.Error(), "boom") {
		t.Errorf("stream error = %v, want message boom", errConsume)
	}
}

func TestSSEConsumerAcceptsDataTypeWhenEventLineIsMissing(t *testing.T) {
	consumer := newSSEConsumer()
	stream := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	if _, errConsume := consumer.consume([]byte(stream)); errConsume != nil {
		t.Fatalf("consume: %v", errConsume)
	}
	frames, errFinish := consumer.finishFrames()
	if errFinish != nil {
		t.Fatalf("finishFrames: %v", errFinish)
	}
	if len(frames) != 2 {
		t.Fatalf("frame count = %d, want event-name and data chunks", len(frames))
	}
	if string(frames[0].Payload) != "event: content_block_start\n" {
		t.Errorf("synthesized event name = %q", frames[0].Payload)
	}
}

func TestNormaliseStreamFrameDropsSignatureAndClassifiesError(t *testing.T) {
	tests := []struct {
		name    string
		frame   string
		wantNil bool
		wantErr bool
	}{
		{
			name: "signature delta dropped",
			frame: "event: content_block_delta\ndata: " +
				`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"abc"}}` + "\n\n",
			wantNil: true,
		},
		{
			name: "text delta kept",
			frame: "event: content_block_delta\ndata: " +
				`{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}` + "\n\n",
		},
		{
			name:    "error event becomes a Go error",
			frame:   "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"boom\"}}\n\n",
			wantNil: true, wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, errFrame := normaliseStreamFrame([]byte(testCase.frame))
			if (errFrame != nil) != testCase.wantErr {
				t.Fatalf("error = %v, wantErr=%v", errFrame, testCase.wantErr)
			}
			if testCase.wantNil && got != nil {
				t.Errorf("frame = %q, want nil", got)
			}
			if !testCase.wantNil && string(got) != testCase.frame {
				t.Errorf("frame changed: %q", got)
			}
		})
	}
}

func TestInferFailureClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantCode  string
		wantHTTP  int
		wantRetry bool
	}{
		{
			name:     "402 is QUOTA_EXCEEDED, never server or auth",
			status:   http.StatusPaymentRequired,
			body:     `{"type":"error","error":{"type":"billing_error","message":"insufficient balance"}}`,
			wantCode: "QUOTA_EXCEEDED", wantHTTP: 402,
		},
		{name: "401 is auth", status: 401, body: `{}`, wantCode: "auth", wantHTTP: 401},
		{name: "403 is auth", status: 403, body: `{}`, wantCode: "auth", wantHTTP: 401},
		{name: "400 is invalid request", status: 400, body: `{}`, wantCode: "invalid_request", wantHTTP: 400},
		{name: "429 is retryable rate limit", status: 429, body: `{}`, wantCode: "rate_limited", wantHTTP: 429, wantRetry: true},
		{name: "500 is retryable upstream failure", status: 500, body: `{}`, wantCode: "upstream_error", wantHTTP: 502, wantRetry: true},
		{name: "504 is retryable timeout", status: 504, body: `{}`, wantCode: "upstream_timeout", wantHTTP: 504, wantRetry: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := inferFailure(httpResponse(testCase.status, testCase.body))
			var envelope *abiboot.EnvelopeError
			if !errors.As(err, &envelope) {
				t.Fatalf("inferFailure returned %T, want EnvelopeError", err)
			}
			if envelope.Code != testCase.wantCode {
				t.Errorf("code = %q, want %q", envelope.Code, testCase.wantCode)
			}
			if envelope.HTTPStatus != testCase.wantHTTP {
				t.Errorf("http = %d, want %d", envelope.HTTPStatus, testCase.wantHTTP)
			}
			if envelope.Retryable != testCase.wantRetry {
				t.Errorf("retryable = %v, want %v", envelope.Retryable, testCase.wantRetry)
			}
			if testCase.status == 402 && (!strings.Contains(envelope.Message, "insufficient balance") ||
				strings.Contains(strings.ToLower(envelope.Code), "auth") || strings.Contains(strings.ToLower(envelope.Code), "server")) {
				t.Errorf("402 classification hides the top-up signal: %#v", envelope)
			}
		})
	}
}

func TestPerformInferUsesMeasuredEndpointHeadersAndRewrite(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"), nil
	}
	host.install(t)
	request := pluginapi.ExecutorRequest{
		Model:   "MiniMax-M3.1-Flash-Preview",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`),
	}
	credential := &Credential{AccessToken: "mmoat_secret"}
	if _, errInfer := performInfer(testHost(), request, credential); errInfer != nil {
		t.Fatalf("performInfer: %v", errInfer)
	}
	sent := host.requests[0]
	if sent.URL != APIHost+InferPath || sent.Method != http.MethodPost {
		t.Errorf("request = %s %s, want POST %s", sent.Method, sent.URL, APIHost+InferPath)
	}
	if len(sent.Headers) != 3 || sent.Headers.Get("anthropic-version") != "" {
		t.Errorf("headers = %#v, want exactly the measured three and no anthropic-version", sent.Headers)
	}
	body := decodeJSONMap(t, sent.Body)
	if body["thinking"].(map[string]any)["type"] != "adaptive" {
		t.Errorf("defensive rewrite was not applied before dispatch: %s", sent.Body)
	}
}

func TestExecutorStreamChunksReconstructCompleteAnthropicFrames(t *testing.T) {
	host := newFakeHost()
	host.streamStatus = 200
	host.streamHeaders = map[string][]string{"Content-Type": {"text/event-stream"}}
	host.streamChunks = [][]byte{[]byte(strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"))}
	host.install(t)
	credential := &Credential{AccessToken: "mmoat_secret", Type: ProviderKey}
	storage, _ := credential.Encode()
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: "minimax-test.json", Model: "MiniMax-M3",
		StorageJSON: storage, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	})
	value, errStream := handleExecutorExecuteStream(testHost(), raw)
	if errStream != nil {
		t.Fatalf("handleExecutorExecuteStream: %v", errStream)
	}
	response := value.(executorStreamResponse)
	if response.Headers.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q", response.Headers.Get("Content-Type"))
	}
	var out []byte
	for _, chunk := range response.Chunks {
		out = append(out, chunk.Payload...)
	}
	text := string(out)
	if !strings.Contains(text, "event: content_block_delta\ndata: ") {
		t.Errorf("event and data chunks do not reconstruct Anthropic SSE: %q", text)
	}
	if !strings.HasSuffix(text, "\n\n") {
		t.Errorf("stream lacks the trailing blank line: %q", text)
	}
}

// translateOpenAIToClaude invokes the CPA SDK's native translator. It is
// imported through the SDK's builtin package so the default registry is
// populated with all built-in directions.
func translateOpenAIToClaude(t *testing.T, model string, raw []byte, stream bool) []byte {
	t.Helper()
	return builtin.Registry().TranslateRequest(
		sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, model, raw, stream,
	)
}

// findContentBlock searches every message's content array for a block type.
func findContentBlock(messages []any, blockType string) map[string]any {
	for _, rawMessage := range messages {
		message, okMessage := rawMessage.(map[string]any)
		if !okMessage {
			continue
		}
		blocks, okBlocks := message["content"].([]any)
		if !okBlocks {
			continue
		}
		for _, rawBlock := range blocks {
			block, okBlock := rawBlock.(map[string]any)
			if okBlock && block["type"] == blockType {
				return block
			}
		}
	}
	return nil
}

// fmtString renders one JSON scalar for prefix checks.
func fmtString(value any) string {
	text, _ := value.(string)
	return text
}

// TestBase64FixtureIsBare is a small guard on the image fixture itself: a
// request test that accidentally supplied a data URL where the translated
// field should be bare base64 would prove nothing.
func TestBase64FixtureIsBare(t *testing.T) {
	const bare = "aGVsbG8="
	decoded, errDecode := base64.StdEncoding.DecodeString(bare)
	if errDecode != nil || string(decoded) != "hello" {
		t.Fatalf("fixture %q is not base64('hello'): %q, %v", bare, decoded, errDecode)
	}
	if strings.Contains(bare, ":") {
		t.Errorf("fixture %q is not bare base64", bare)
	}
}
