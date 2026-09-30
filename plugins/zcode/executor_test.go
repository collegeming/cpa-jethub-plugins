package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Executor tests.
//
// ## The format declaration is the load-bearing part
//
// The plugin declares `anthropic` for BOTH input and output, and that decision is
// what keeps the plugin free of any translation code: the host converts the client's
// request INTO Anthropic on the way in and converts the plugin's Anthropic frames
// into whatever the client asked for on the way out.
//
// The evidence lives in the SDK and is cited file:line in executor.go. These tests
// assert the DECLARATION, because a change to it silently changes what the plugin
// receives — and the failure mode is a body that still parses but no longer carries
// the caller's messages in the shape the identity block expects.

// TestExecutorDeclaresAnthropicOnBothSides pins the capability block.
func TestExecutorDeclaresAnthropicOnBothSides(t *testing.T) {
	registration := Plugin().Registration()
	caps := registration.Capabilities

	if len(caps.ExecutorInputFormats) != 1 || caps.ExecutorInputFormats[0] != "anthropic" {
		t.Fatalf("executor_input_formats = %v, want [anthropic]", caps.ExecutorInputFormats)
	}
	if len(caps.ExecutorOutputFormats) != 1 || caps.ExecutorOutputFormats[0] != "anthropic" {
		t.Fatalf("executor_output_formats = %v, want [anthropic]", caps.ExecutorOutputFormats)
	}
	if !caps.Executor {
		t.Error("the executor capability is not declared")
	}
	if caps.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Errorf("executor_model_scope = %q, want %q: every ZCode credential comes from a login flow",
			caps.ExecutorModelScope, pluginapi.ExecutorModelScopeOAuth)
	}
	// The plugin never translates, so it must not claim a translator capability it
	// does not implement beyond the identity passthrough.
	if !caps.RequestTranslator || !caps.ResponseTranslator {
		t.Error("the identity translator routes are registered but not declared")
	}
}

// TestExecutorIdentifierIsTheProviderKey covers the route key.
func TestExecutorIdentifierIsTheProviderKey(t *testing.T) {
	value, errIdentifier := handleExecutorIdentifier(nil, nil)
	if errIdentifier != nil {
		t.Fatalf("executor.identifier: %v", errIdentifier)
	}
	reply := decodeResult[abiboot.Identifier](t, value)
	if reply.Identifier != ProviderKey {
		t.Fatalf("identifier = %q, want %q", reply.Identifier, ProviderKey)
	}
}

// TestBuildInferenceBodyReplacesSystemAndKeepsTheCallerText covers the body rewrite.
func TestBuildInferenceBodyReplacesSystemAndKeepsTheCallerText(t *testing.T) {
	payload := `{
		"model": "GLM-5.3-Flash",
		"max_tokens": 64,
		"stream": false,
		"system": "You are a harness. Obey AGENTS.md.",
		"messages": [{"role":"user","content":"hello"}]
	}`
	body, errBody := buildInferenceBody(pluginapi.ExecutorRequest{
		Model: "GLM-5.3-Flash", Payload: []byte(payload),
	}, settings())
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var built map[string]any
	if errUnmarshal := json.Unmarshal(body, &built); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}

	blocks, ok := built["system"].([]any)
	if !ok || len(blocks) != 4 {
		t.Fatalf("system = %#v, want 4 blocks", built["system"])
	}
	// The caller's text survives, appended last — the identity block keeps the head
	// position upstream checks.
	last, _ := blocks[3].(map[string]any)
	if last["text"] != "You are a harness. Obey AGENTS.md." {
		t.Errorf("the caller system was dropped or moved: %v", last["text"])
	}
	// The stream flag is forced on: upstream only streams, and the non-streaming
	// route folds the same stream.
	if built["stream"] != true {
		t.Errorf("stream = %v, want true", built["stream"])
	}
	// The caller's max_tokens is preserved.
	if value, _ := nonNegativeInt(built["max_tokens"]); value != 64 {
		t.Errorf("max_tokens = %v, want the caller's 64", built["max_tokens"])
	}
}

// TestBuildInferenceBodyFlattensABlockArraySystem covers the other system spelling.
//
// The host hands the plugin whichever shape the client's protocol produced, and an
// Anthropic-native caller sends blocks. Either way the caller's content is ONE blob
// appended after the official blocks.
func TestBuildInferenceBodyFlattensABlockArraySystem(t *testing.T) {
	payload := `{
		"model": "GLM-5.3",
		"system": [{"type":"text","text":"part one "},{"type":"text","text":"part two"}],
		"messages": [{"role":"user","content":"hi"}]
	}`
	body, errBody := buildInferenceBody(pluginapi.ExecutorRequest{Model: "GLM-5.3", Payload: []byte(payload)}, settings())
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var built map[string]any
	if errUnmarshal := json.Unmarshal(body, &built); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	blocks, _ := built["system"].([]any)
	if len(blocks) != 4 {
		t.Fatalf("system block count = %d, want 4", len(blocks))
	}
	last, _ := blocks[3].(map[string]any)
	if last["text"] != "part one part two" {
		t.Errorf("the caller's blocks were not flattened in order: %v", last["text"])
	}
}

// TestBuildInferenceBodyWithoutACallerSystemYieldsThreeBlocks covers the
// system-less request.
func TestBuildInferenceBodyWithoutACallerSystemYieldsThreeBlocks(t *testing.T) {
	payload := `{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}`
	body, errBody := buildInferenceBody(pluginapi.ExecutorRequest{Model: "GLM-5.3", Payload: []byte(payload)}, settings())
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var built map[string]any
	if errUnmarshal := json.Unmarshal(body, &built); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	blocks, _ := built["system"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("system block count = %d, want 3", len(blocks))
	}
}

// TestBuildInferenceBodyPrefixesTheFirstUserMessage covers the date block.
func TestBuildInferenceBodyPrefixesTheFirstUserMessage(t *testing.T) {
	payload := `{"model":"GLM-5.3","messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":"reply"},
		{"role":"user","content":"second"}
	]}`
	body, errBody := buildInferenceBody(pluginapi.ExecutorRequest{Model: "GLM-5.3", Payload: []byte(payload)}, settings())
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var built map[string]any
	if errUnmarshal := json.Unmarshal(body, &built); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	messages, _ := built["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(messages))
	}
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("the first message has %d blocks, want 2", len(content))
	}
	prefix, _ := content[0].(map[string]any)
	text, _ := prefix["text"].(string)
	if !strings.HasPrefix(text, "<system-reminder>") {
		t.Errorf("the first block is not the date block: %q", text)
	}
	// The SECOND user message is untouched.
	third, _ := messages[2].(map[string]any)
	if third["content"] != "second" {
		t.Errorf("the second user message was modified: %#v", third["content"])
	}
}

// TestBuildInferenceBodyAppliesTheDefaultMaxTokens covers the configured default.
func TestBuildInferenceBodyAppliesTheDefaultMaxTokens(t *testing.T) {
	payload := `{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}`
	cfg := DefaultConfig()
	cfg.DefaultMaxTokens = 4096
	body, errBody := buildInferenceBody(pluginapi.ExecutorRequest{Model: "GLM-5.3", Payload: []byte(payload)}, cfg)
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var built map[string]any
	if errUnmarshal := json.Unmarshal(body, &built); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if value, _ := nonNegativeInt(built["max_tokens"]); value != 4096 {
		t.Fatalf("max_tokens = %v, want the configured default 4096", built["max_tokens"])
	}

	// A caller-supplied value wins over the default.
	// ⚠ Each case decodes into a FRESH map: `json.Unmarshal` into a populated
	// map merges rather than replaces, so reusing one would let the previous
	// case's members leak in and make the next assertion meaningless.
	body, errBody = buildInferenceBody(pluginapi.ExecutorRequest{
		Model: "GLM-5.3", Payload: []byte(`{"model":"GLM-5.3","max_tokens":16,"messages":[]}`),
	}, cfg)
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var fromCaller map[string]any
	if errUnmarshal := json.Unmarshal(body, &fromCaller); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if value, _ := nonNegativeInt(fromCaller["max_tokens"]); value != 16 {
		t.Fatalf("max_tokens = %v, want the caller's 16", fromCaller["max_tokens"])
	}

	// With the default switched off and no caller value, nothing is sent.
	cfg.DefaultMaxTokens = 0
	body, errBody = buildInferenceBody(pluginapi.ExecutorRequest{
		Model: "GLM-5.3", Payload: []byte(`{"model":"GLM-5.3","messages":[]}`),
	}, cfg)
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var withoutDefault map[string]any
	if errUnmarshal := json.Unmarshal(body, &withoutDefault); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if _, present := withoutDefault["max_tokens"]; present {
		t.Fatal("max_tokens was sent although no default is configured")
	}
}

// TestBuildInferenceBodyRejectsAnEmptyPayload covers the guard.
func TestBuildInferenceBodyRejectsAnEmptyPayload(t *testing.T) {
	for _, payload := range []string{"", "   ", "\n"} {
		if _, errBody := buildInferenceBody(pluginapi.ExecutorRequest{Payload: []byte(payload)}, settings()); errBody == nil {
			t.Fatalf("an empty payload %q was accepted", payload)
		}
	}
}

// TestToolCacheBreakpointMarksOnlyTheLastTool covers the prompt-caching placement.
//
// A prefix-shaped cache needs one breakpoint, on the last tool: that covers
// "system + every tool". Marking each tool would exhaust the per-request budget
// (Anthropic allows four) that the identity block's own marker also draws on.
func TestToolCacheBreakpointMarksOnlyTheLastTool(t *testing.T) {
	tools := []any{
		map[string]any{"name": "first", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "second", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "third", "input_schema": map[string]any{"type": "object"}},
	}
	body := map[string]any{"tools": tools}
	withToolCacheBreakpoint(body)

	for index, raw := range body["tools"].([]any) {
		tool := raw.(map[string]any)
		_, marked := tool["cache_control"]
		wantMarked := index == len(tools)-1
		if marked != wantMarked {
			t.Errorf("tool[%d] (%v) cache_control present = %v, want %v", index, tool["name"], marked, wantMarked)
		}
	}
	marker, _ := body["tools"].([]any)[len(tools)-1].(map[string]any)["cache_control"].(map[string]any)
	if marker["type"] != "ephemeral" {
		t.Errorf("marker = %#v, want {type: ephemeral}", marker)
	}

	// A body with no tools, or with an unexpected shape, must not panic.
	withToolCacheBreakpoint(map[string]any{})
	withToolCacheBreakpoint(map[string]any{"tools": "not-an-array"})
	withToolCacheBreakpoint(map[string]any{"tools": []any{}})
	withToolCacheBreakpoint(map[string]any{"tools": []any{"not-an-object"}})
}

// TestBuildInferenceBodyDropsAnUnsupportedEffort covers the effort guard.
//
// ⚠ The field is `output_config.effort`, not `reasoning_effort`. Upstream
// publishes that spelling itself in `client/configs`, where each level carries
// `{"path":["output_config","effort"],"value":…}`. Dropping a level the model does
// not declare matters because upstream would reject the whole request over a purely
// cosmetic field.
func TestBuildInferenceBodyDropsAnUnsupportedEffort(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		wantKept  bool
		wantValue string
	}{
		{
			name:      "a declared level is kept",
			payload:   `{"model":"GLM-5.3","output_config":{"effort":"low"},"messages":[]}`,
			wantKept:  true,
			wantValue: "low",
		},
		{
			name:      "the max level is declared too",
			payload:   `{"model":"GLM-5.3","output_config":{"effort":"max"},"messages":[]}`,
			wantKept:  true,
			wantValue: "max",
		},
		{
			name:     "an undeclared level is dropped",
			payload:  `{"model":"GLM-5.3","output_config":{"effort":"turbo"},"messages":[]}`,
			wantKept: false,
		},
		{
			name:     "a level declared for another model is dropped",
			payload:  `{"model":"GLM-5.3","output_config":{"effort":"minimal"},"messages":[]}`,
			wantKept: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, errBody := buildInferenceBody(pluginapi.ExecutorRequest{
				Model: "GLM-5.3", Payload: []byte(tc.payload),
			}, settings())
			if errBody != nil {
				t.Fatalf("buildInferenceBody: %v", errBody)
			}
			var built map[string]any
			if errUnmarshal := json.Unmarshal(body, &built); errUnmarshal != nil {
				t.Fatalf("decode: %v", errUnmarshal)
			}
			outputConfig, present := built["output_config"].(map[string]any)
			if !tc.wantKept {
				if present {
					t.Fatalf("output_config = %#v, want it removed entirely", outputConfig)
				}
				return
			}
			if !present {
				t.Fatal("output_config was removed although the level is declared")
			}
			if outputConfig["effort"] != tc.wantValue {
				t.Fatalf("effort = %v, want %v", outputConfig["effort"], tc.wantValue)
			}
		})
	}
}

// TestOutputConfigKeepsSiblingFieldsWhenTheEffortIsDropped covers the partial
// removal: `effort` is one member of an object the client may also use for other
// settings.
func TestOutputConfigKeepsSiblingFieldsWhenTheEffortIsDropped(t *testing.T) {
	body := map[string]any{
		"output_config": map[string]any{"effort": "turbo", "format": map[string]any{"type": "json"}},
	}
	pruneUnsupportedEffort(body, "GLM-5.3")
	outputConfig, _ := body["output_config"].(map[string]any)
	if _, present := outputConfig["effort"]; present {
		t.Error("the unsupported effort was not dropped")
	}
	if _, present := outputConfig["format"]; !present {
		t.Error("a sibling field was dropped with it")
	}
}

// TestBuildInferenceBodyKeepsToolsInTheFlatAnthropicShape covers the tool passthrough.
//
// The host's `openai → claude` translator already produces Anthropic's FLAT
// `{name, description, input_schema}` shape, so the plugin must not reshape it —
// nesting it back under `function` is the mistake that makes a model invent XML
// tool calls in prose.
func TestBuildInferenceBodyKeepsToolsInTheFlatAnthropicShape(t *testing.T) {
	payload := `{
		"model":"GLM-5.3",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]
	}`
	body, errBody := buildInferenceBody(pluginapi.ExecutorRequest{Model: "GLM-5.3", Payload: []byte(payload)}, settings())
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var built map[string]any
	if errUnmarshal := json.Unmarshal(body, &built); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	tools, _ := built["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tool count = %d, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if _, nested := tool["function"]; nested {
		t.Error("the tool carries an OpenAI-style `function` wrapper; Anthropic wants the flat shape")
	}
	if tool["name"] != "read_file" {
		t.Errorf("tool name = %v", tool["name"])
	}
	if _, ok := tool["input_schema"].(map[string]any); !ok {
		t.Errorf("input_schema = %#v, want the flat schema object", tool["input_schema"])
	}
	// The cache breakpoint was applied because the default is on.
	if _, marked := tool["cache_control"]; !marked {
		t.Error("the tool cache breakpoint is missing although the setting defaults to on")
	}
}

// TestToolCacheBreakpointCanBeDisabled covers the switch.
func TestToolCacheBreakpointCanBeDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ToolCacheBreakpoint = false
	payload := `{"model":"GLM-5.3","messages":[],"tools":[{"name":"t","input_schema":{}}]}`
	body, errBody := buildInferenceBody(pluginapi.ExecutorRequest{Model: "GLM-5.3", Payload: []byte(payload)}, cfg)
	if errBody != nil {
		t.Fatalf("buildInferenceBody: %v", errBody)
	}
	var built map[string]any
	_ = json.Unmarshal(body, &built)
	tool, _ := built["tools"].([]any)[0].(map[string]any)
	if _, marked := tool["cache_control"]; marked {
		t.Error("a cache breakpoint was applied although the setting is off")
	}
}

// TestInferenceHeadersMatchTheMeasuredSet pins the header contract.
//
// `X-Device-Mid` is the hard requirement (its absence produces
// `400 {"code":3001}`), and `anthropic-version` is what makes the endpoint treat the
// body as a Messages request.
func TestInferenceHeadersMatchTheMeasuredSet(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, fixtureSSEBody(t)), nil
	}
	credential := sampleCredential()
	if _, errStream := handleExecutorExecuteStream(testHost(),
		executorRawPayload(t, credential, `{"model":"GLM-5.3-Flash","messages":[{"role":"user","content":"hi"}]}`)); errStream != nil {
		t.Fatalf("execute_stream: %v", errStream)
	}
	calls := fake.callsFor(MessagesPath)
	if len(calls) != 1 {
		t.Fatalf("inference calls = %d, want 1", len(calls))
	}
	headers := calls[0].Headers

	expected := map[string]string{
		"Authorization":       credential.Bearer(),
		"Content-Type":        "application/json",
		"Accept":              "text/event-stream",
		"anthropic-version":   "2023-06-01",
		"HTTP-Referer":        Origin,
		"X-Device-Mid":        credential.DeviceMid,
		"X-ZCode-App-Version": "3.14.3",
		"X-Release-Channel":   "stable",
		"X-Client-Language":   "zh-CN",
		"X-Client-Timezone":   "Asia/Shanghai",
		"X-Platform":          "win32",
		"X-Os-Category":       "windows",
		"User-Agent":          "ZCode/3.14.3",
	}
	for name, want := range expected {
		if got := headers.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	// ⚠ No captcha header. The reference verified empirically that inference needs
	// none: a correct identity block returns 200 with no captcha header at all, and
	// even a deliberately bogus captcha parameter returns 200. Emitting one here
	// would be cargo-culting a mechanism that no longer exists.
	for name := range headers {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "captcha") {
			t.Errorf("header %s was sent; this plugin does not mint captchas", name)
		}
	}
	// The body went to the Anthropic path, which is the ONLY inference endpoint
	// that exists.
	if !strings.HasSuffix(calls[0].URL, MessagesPath) {
		t.Errorf("url = %q, want the Anthropic messages path", calls[0].URL)
	}
	if strings.Contains(calls[0].URL, "chat/completions") {
		t.Error("the request used an OpenAI-shaped path, which does not exist under zcode-plan")
	}
}

// TestExecutorRejectsACredentialMissingItsDeviceId covers the 3001 guard before it
// becomes a round trip.
func TestExecutorRejectsACredentialMissingItsDeviceId(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatal("a request was issued with an unusable credential")
		return nil, nil
	}
	broken := sampleCredential()
	broken.DeviceMid = ""
	storage, _ := broken.Encode()
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		AuthID: "zcode-1", Model: "GLM-5.3", Format: "anthropic",
		Payload: []byte(`{"model":"GLM-5.3","messages":[]}`), StorageJSON: storage,
	})
	_, errStream := handleExecutorExecuteStream(testHost(), raw)
	if errStream == nil {
		t.Fatal("a credential without a device id was accepted")
	}
	envelope := &abiboot.EnvelopeError{}
	if !asEnvelope(errStream, envelope) {
		t.Fatalf("error type = %T", errStream)
	}
	if envelope.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("http status = %d, want 401 so the host rotates the credential", envelope.HTTPStatus)
	}
}

// TestCountTokensReturnsTheAnthropicShape covers the token-count response.
//
// The host applies the SAME output translation to a count as to a completion, so the
// payload has to be in the declared output format. An OpenAI-shaped count would be
// re-mapped into a Claude body carrying no token field at all.
func TestCountTokensReturnsTheAnthropicShape(t *testing.T) {
	raw, errMarshal := json.Marshal(pluginapi.ExecutorRequest{
		Model:   "GLM-5.3",
		Format:  "anthropic",
		Payload: []byte(`{"model":"GLM-5.3","system":[{"type":"text","text":"` + strings.Repeat("x", 400) + `"}],"messages":[]}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	value, errCount := handleExecutorCountTokens(nil, raw)
	if errCount != nil {
		t.Fatalf("count_tokens: %v", errCount)
	}
	response := decodeResult[pluginapi.ExecutorResponse](t, value)
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(response.Payload, &decoded); errUnmarshal != nil {
		t.Fatalf("decode count: %v", errUnmarshal)
	}
	tokens, ok := decoded["input_tokens"]
	if !ok {
		t.Fatalf("the count payload has no input_tokens member: %s", response.Payload)
	}
	if _, isNumber := tokens.(float64); !isNumber {
		t.Fatalf("input_tokens = %#v, want a number", tokens)
	}
	if tokens.(float64) <= 0 {
		t.Fatalf("input_tokens = %v, want a positive estimate", tokens)
	}
	if response.Headers.Get("Content-Type") == "" {
		t.Error("no content type was set on the count response")
	}
}

// TestCountTokensOnAnEmptyPayloadStaysZero covers the degenerate input.
func TestCountTokensOnAnEmptyPayloadStaysZero(t *testing.T) {
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{Model: "GLM-5.3", Format: "anthropic"})
	value, errCount := handleExecutorCountTokens(nil, raw)
	if errCount != nil {
		t.Fatalf("count_tokens: %v", errCount)
	}
	response := decodeResult[pluginapi.ExecutorResponse](t, value)
	if string(response.Payload) != `{"input_tokens":0}` {
		t.Fatalf("payload = %s, want a zero count", response.Payload)
	}
}

// TestTranslateRoutesAreIdentity covers the pass-through translators.
//
// The plugin declares `anthropic` on both sides, so the host owns every conversion
// and these routes exist only to satisfy the capability declaration.
func TestTranslateRoutesAreIdentity(t *testing.T) {
	body := []byte(`{"model":"x","messages":[]}`)
	requestRaw, _ := json.Marshal(pluginapi.RequestTransformRequest{
		FromFormat: "chat-completions", ToFormat: "anthropic", Body: body,
	})
	value, errTranslate := handleRequestTranslate(nil, requestRaw)
	if errTranslate != nil {
		t.Fatalf("request.translate: %v", errTranslate)
	}
	if got := decodeResult[pluginapi.PayloadResponse](t, value); string(got.Body) != string(body) {
		t.Fatalf("request.translate changed the body: %s", got.Body)
	}

	responseRaw, _ := json.Marshal(pluginapi.ResponseTransformRequest{
		FromFormat: "anthropic", ToFormat: "chat-completions", Body: body,
	})
	value, errTranslate = handleResponseTranslate(nil, responseRaw)
	if errTranslate != nil {
		t.Fatalf("response.translate: %v", errTranslate)
	}
	if got := decodeResult[pluginapi.PayloadResponse](t, value); string(got.Body) != string(body) {
		t.Fatalf("response.translate changed the body: %s", got.Body)
	}
}

// TestNonStreamingExecuteFoldsTheStream covers `executor.execute`.
func TestNonStreamingExecuteFoldsTheStream(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, fixtureSSEBody(t)), nil
	}
	value, errExecute := handleExecutorExecute(testHost(),
		executorRawPayload(t, sampleCredential(), `{"model":"GLM-5.3-Flash","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	response := decodeResult[pluginapi.ExecutorResponse](t, value)
	if strings.Contains(string(response.Payload), "event:") {
		t.Fatalf("the non-streaming payload still carries SSE framing: %s", truncate(string(response.Payload), 200))
	}
	var message anthropicMessage
	if errUnmarshal := json.Unmarshal(response.Payload, &message); errUnmarshal != nil {
		t.Fatalf("decode message: %v", errUnmarshal)
	}
	if message.Type != "message" || len(message.Content) == 0 {
		t.Fatalf("folded message = %#v", message)
	}
	if response.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q, want application/json", response.Headers.Get("Content-Type"))
	}
	if response.Metadata["provider"] != ProviderKey {
		t.Errorf("metadata provider = %v", response.Metadata["provider"])
	}
}
