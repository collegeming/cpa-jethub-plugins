package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The executor.
//
// ## Format declaration — verified against the SDK, not guessed
//
// The plugin declares `anthropic` for BOTH input and output:
//
//	executor_input_formats  = ["anthropic"]
//	executor_output_formats = ["anthropic"]
//
// Evidence for the direction, read out of the SDK:
//
//   - `internal/pluginhost/adapters.go:85-96`
//     `normalizeExecutorFormatName` maps the literal `"anthropic"` onto
//     `sdktranslator.FormatClaude`.
//   - `internal/pluginhost/adapters_executors.go:400-426`
//     `prepareExecutorCall` computes `inputRequested` from the CLIENT's
//     protocol, picks `inputFormat` out of the declared input formats, and when
//     the two differ runs
//     `sdktranslator.TranslateRequest(inputRequested, inputFormat, …)` — i.e.
//     the HOST converts the client's body INTO the declared input format.
//     It then sets `nativeReq.Format = outputFormat` and
//     `nativeOpts.SourceFormat = inputFormat`.
//   - `internal/pluginhost/adapters_executors.go:585-604`
//     `translateExecutorStreamPayload` only translates when
//     `outputFormat != requestedFormat`; when they are equal the chunk is
//     forwarded byte for byte.
//
// So declaring `anthropic` on both sides means: the host turns an OpenAI request
// into an Anthropic one for us, and hands our Anthropic frames straight back when
// the client asked for Anthropic — or converts them itself when the client asked
// for anything else, because `sdk/translator` ships
// `claude → openai/chat-completions` (`internal/translator/claude/openai/
// chat-completions/init.go`) and `claude → openai/responses`
// (`internal/translator/claude/openai/responses/init.go`). The plugin therefore
// never has to translate anything: it injects the identity block and relays.
//
// ## Framing contract
//
// A plugin stream chunk is BARE payload bytes. For an OpenAI-format output the
// host frames `data: %s\n\n` itself and appends `data: [DONE]`
// (`sdk/api/handlers/openai/openai_handlers.go:698-720`,
// `internal/pluginhost/adapters_executors.go:638-645`). For a Claude-format
// output the frames are written to the client LITERALLY
// (`sdk/api/handlers/claude/code_handlers.go:305-308`), which is the same shape
// the native Claude executor produces: it re-emits each complete
// `event:`/`data:` block terminated by a blank line
// (`internal/runtime/executor/claude_executor_stream.go:407-445`). This plugin
// therefore emits COMPLETE Anthropic frames — `event: X\ndata: {…}\n\n` — which
// is both what the host expects and what a direct client would need.
//
// A mismatch shows up as dropped frames rather than an error: the OpenAI-shaped
// translator skips any frame that does not start with `data:`
// (`internal/translator/claude/openai/chat-completions/claude_openai_response.go:101`),
// so a bare JSON payload would silently produce an empty answer.

// executorStreamResponse is the wire shape of executor.execute_stream.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleRequestTranslate is an identity transform: the executor declares
// `anthropic` on both sides, so the host performs any cross-protocol translation
// itself and this route never has a gap to fill.
func handleRequestTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.RequestTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	return pluginapi.PayloadResponse{Body: request.Body}, nil
}

// handleResponseTranslate is an identity transform; see handleRequestTranslate.
func handleResponseTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ResponseTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	return pluginapi.PayloadResponse{Body: request.Body}, nil
}

// handleExecutorCountTokens returns a coarse estimate in the Anthropic shape.
//
// The host applies the same output translation it applies to a completion, so the
// payload has to be whatever `prepareExecutorCall` selected as the output format
// — `anthropic` here, whose count response is `{"input_tokens": n}`. Returning an
// OpenAI-shaped count would be re-mapped into a Claude body with no token field
// at all.
func handleExecutorCountTokens(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	estimated := estimateInputTokens(request.Payload)
	payload := []byte(`{"input_tokens":` + strconv.Itoa(estimated) + `}`)
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {"application/json"}},
	}, nil
}

// estimateInputTokens approximates an Anthropic request's input token count.
//
// A real tokenizer is out of scope (and would be a dependency); ~4 bytes per
// token is the usual rough ratio, and the estimate only feeds a usage display.
func estimateInputTokens(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	estimate := len(payload) / 4
	if estimate < 1 {
		estimate = 1
	}
	return estimate
}

// handleExecutorExecute serves a non-streaming completion.
//
// Upstream only streams, so this folds the SSE into ONE Anthropic message object,
// which is what the host's non-stream translation path consumes
// (`sdk/translator/registry.go:TranslateNonStream` →
// `internal/translator/claude/openai/chat-completions/claude_openai_response.go:340`
// `ConvertClaudeResponseToOpenAINonStream`, which splits the body on newlines and
// reads every `data:` line).
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, cfg, errPrepare := decodeExecutorCall(raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	body, errBody := buildInferenceBody(request, cfg)
	if errBody != nil {
		return nil, errBody
	}
	response, errInfer := performInference(h, request, credential, cfg, body, false)
	if errInfer != nil {
		return nil, errInfer
	}
	message, errFold := foldAnthropicStream(response.Body, request.Model)
	if errFold != nil {
		return nil, errFold
	}
	payload, errMarshal := marshalCompact(message)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Metadata: map[string]any{
			"provider": ProviderKey,
			"model":    request.Model,
			"endpoint": MessagesPath,
		},
	}, nil
}

// handleExecutorExecuteStream serves a streaming completion.
//
// The frames are returned in the response envelope rather than pushed through
// `host.stream.emit`; both are valid ABI paths and this is the one the sibling
// plugins use.
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, cfg, errPrepare := decodeExecutorCall(raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	body, errBody := buildInferenceBody(request, cfg)
	if errBody != nil {
		return nil, errBody
	}
	response, errInfer := performInference(h, request, credential, cfg, body, true)
	if errInfer != nil {
		return nil, errInfer
	}
	chunks, errChunks := relayAnthropicFrames(response.Body, request.Model)
	if errChunks != nil {
		return nil, errChunks
	}
	return executorStreamResponse{
		Headers: map[string][]string{"Content-Type": {"text/event-stream"}},
		Chunks:  chunks,
	}, nil
}

// decodeExecutorCall decodes the request and the credential it is bound to.
func decodeExecutorCall(raw json.RawMessage) (pluginapi.ExecutorRequest, *Credential, Config, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return request, nil, Config{}, errDecode
	}
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		return request, nil, Config{}, errCredential
	}
	return request, credential, settings(), nil
}

// buildInferenceBody rewrites the host-translated Anthropic body.
//
// The host already produced a valid Anthropic Messages body from whatever the
// client sent. Three amendments are this plugin's own:
//
//  1. the `system` field is REPLACED by the official identity block with the
//     caller's own system appended last;
//  2. the first user message gains the `<system-reminder>` date block;
//  3. `output_config.effort` is dropped when the model does not declare that
//     level, and the LAST tool gets a prompt-caching breakpoint.
//
// ⚠ The identity block is a hard requirement, not a nicety: upstream inspects the
// body and answers `3012` when the structure is missing or flattened. See
// identity.go.
func buildInferenceBody(request pluginapi.ExecutorRequest, cfg Config) ([]byte, error) {
	if len(bytes.TrimSpace(request.Payload)) == 0 {
		return nil, requestError("empty_request", "ZCode 收到空请求体")
	}
	bodyMap, errDecode := decodeJSONObject(request.Payload)
	if errDecode != nil {
		return nil, errDecode
	}

	callerSystem := flattenSystemField(bodyMap["system"])
	modelName := firstNonEmpty(stringField(bodyMap, "model"), request.Model)

	// The working directory comes from the host process. The reference declares
	// `cwd` mandatory because the official client never sends "unknown" for it.
	cwd := workingDirectory()
	blocks := buildSystemBlocks(identityOptions{
		CallerSystem: callerSystem,
		Environment: environmentOptions{
			CWD:       cwd,
			Provider:  ProviderKey,
			Model:     modelName,
			Platform:  cfg.Platform,
			OSGroup:   cfg.OSGroup,
			IsGitRepo: isGitRepository(cwd),
		},
		CacheBreakpoint: true,
	})
	encodedBlocks, errBlocks := json.Marshal(blocks)
	if errBlocks != nil {
		return nil, statusError(false, "encode_identity", http.StatusInternalServerError,
			"编码身份块失败：%v", errBlocks)
	}
	bodyMap["system"] = json.RawMessage(encodedBlocks)

	// The first user turn carries the date block, and the prefix must be part of
	// the CONTENT ARRAY — folding it into the text string changes the structure
	// and is still judged a bare request.
	if messages, ok := bodyMap["messages"].([]any); ok && len(messages) > 0 {
		out, errMessages := applyContextPrefixToMessages(messages)
		if errMessages != nil {
			return nil, errMessages
		}
		bodyMap["messages"] = out
	}

	// Upstream streams; the non-streaming route folds the same stream, so the
	// flag is always true.
	bodyMap["stream"] = true

	if maxTokens, ok := nonNegativeInt(bodyMap["max_tokens"]); !ok || maxTokens <= 0 {
		if cfg.DefaultMaxTokens > 0 {
			bodyMap["max_tokens"] = cfg.DefaultMaxTokens
		}
	}

	pruneUnsupportedEffort(bodyMap, modelName)
	if cfg.ToolCacheBreakpoint {
		withToolCacheBreakpoint(bodyMap)
	}
	return reencodeBody(bodyMap)
}

// applyContextPrefixToMessages marshals the message list, prefixes the first user
// message and unmarshals it back.
func applyContextPrefixToMessages(messages []any) ([]any, error) {
	raw := make([]json.RawMessage, 0, len(messages))
	for _, message := range messages {
		encoded, errMarshal := json.Marshal(message)
		if errMarshal != nil {
			return nil, statusError(false, "encode_messages", http.StatusInternalServerError,
				"编码消息失败：%v", errMarshal)
		}
		raw = append(raw, encoded)
	}
	prefixed := withContextPrefix(raw, nowFunc())
	out := make([]any, 0, len(prefixed))
	for _, message := range prefixed {
		var decoded any
		if errUnmarshal := json.Unmarshal(message, &decoded); errUnmarshal != nil {
			return nil, statusError(false, "decode_messages", http.StatusInternalServerError,
				"解码消息失败：%v", errUnmarshal)
		}
		out = append(out, decoded)
	}
	return out, nil
}

// flattenSystemField turns whatever the host put in `system` back into one string.
//
// The host may hand us a plain string or an array of text blocks; either way the
// caller's content is one blob that gets appended AFTER the official blocks, so
// the identity block keeps the head position upstream checks for.
func flattenSystemField(raw any) string {
	switch typed := raw.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, item := range typed {
			switch block := item.(type) {
			case string:
				builder.WriteString(block)
			case map[string]any:
				if text, ok := block["text"].(string); ok {
					builder.WriteString(text)
				}
			}
		}
		return builder.String()
	default:
		return ""
	}
}

// pruneUnsupportedEffort removes `output_config.effort` when the model does not
// declare that level.
//
// ⚠ The protocol name is `output_config.effort`, NOT `reasoning_effort`. The
// authority is upstream's own `client/configs`, where each level publishes how to
// express itself:
//
//	{"path": ["output_config", "effort"], "value": "low" | "high" | "max"}
//
// The host's own translator already maps an OpenAI `reasoning_effort` onto this
// exact field for Claude bodies
// (`internal/translator/claude/openai/chat-completions/claude_openai_request.go:57-100`),
// so nothing has to be invented here — this only guards against a level the model
// does not accept, which upstream would reject outright and thereby fail the whole
// request for a purely cosmetic field.
func pruneUnsupportedEffort(body map[string]any, modelName string) {
	outputConfig, ok := body["output_config"].(map[string]any)
	if !ok {
		return
	}
	effort, ok := outputConfig["effort"].(string)
	if !ok || strings.TrimSpace(effort) == "" {
		return
	}
	if reasoningLevelSupported(currentCatalogue(), modelName, strings.TrimSpace(effort)) {
		return
	}
	delete(outputConfig, "effort")
	if len(outputConfig) == 0 {
		delete(body, "output_config")
	}
}

// withToolCacheBreakpoint puts ONE `cache_control` breakpoint on the LAST tool.
//
// Anthropic's prompt caching is prefix-shaped: a breakpoint at a position covers
// everything before it, so one marker on the final tool pulls "system + every
// tool" into the cache. Marking every tool instead would burn the per-request
// breakpoint budget (Anthropic allows four) that the system block also needs —
// which is exactly why the identity block puts its single marker on its own last
// block.
func withToolCacheBreakpoint(body map[string]any) {
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}
	last, ok := tools[len(tools)-1].(map[string]any)
	if !ok {
		return
	}
	last["cache_control"] = map[string]any{"type": "ephemeral"}
}

// performInference sends one request, retrying ONLY the concurrency rejection.
//
// The two 429s mean opposite things and this loop is where that matters:
//
//   - `3009` concurrency limit: "wait a moment", so it is retried with a linear
//     backoff and the credential is left alone — rotating the account would
//     retire a usable credential for a server-side window;
//   - `1005` / `1113` quota exhausted: deterministic, so it is raised as a 402 and
//     the host rotates the credential instead. Retrying it just burns wall time.
//
// `3012` (risk control) is never retried here either: it carries an account
// penalty, so it fails immediately with its own message.
func performInference(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config, body []byte, stream bool) (*pluginapi.HTTPResponse, error) {
	headers := buildHeaders(credential, cfg, headerOptions{
		JSON:          true,
		Authorization: true,
		// The reference sends `Accept: text/event-stream` on the inference path.
		Accept: "text/event-stream",
	})
	attempts := cfg.ConcurrencyRetryMax
	if attempts < 0 {
		attempts = 0
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		response, errDo := hostRequest(h, http.MethodPost, Origin+MessagesPath, headers, body)
		if errDo != nil {
			return nil, transportError("inference_transport", "请求 ZCode 失败：%v", errDo)
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		envelope := parseUpstreamEnvelope(response.Body, response.StatusCode)
		lastErr = classifyUpstreamError(response.StatusCode, response.Body)
		if !isConcurrencyLimited(response.StatusCode, envelope) || attempt >= attempts {
			return nil, lastErr
		}
		// Linear backoff, exactly as the reference measures it: 900ms still hit
		// the window, so the default base is 1500ms. A configured 0 means "no
		// wait", which is what a test uses; a negative value falls back to the
		// default so a typo cannot turn the loop into a hot one.
		base := cfg.ConcurrencyRetryBaseMS
		if base < 0 {
			base = DefaultConcurrencyRetryBaseMS
		}
		sleepMillis(base * (attempt + 1))
	}
}

// relayAnthropicFrames splits a buffered upstream SSE body into complete
// Anthropic frames.
//
// Each emitted chunk is one `event: …\ndata: …\n\n` block, which is what the host
// forwards literally for a Claude-format output and what its
// claude→openai translator expects (it iterates the stream line by line and reads
// every `data:` line).
//
// Three frame kinds are handled explicitly:
//
//   - `ping` is KEPT as a frame but never becomes content: the translators skip
//     it, and dropping it here would also discard the keep-alive an intermediate
//     proxy may depend on;
//   - an `error` event is raised as a failure — silently ignoring it is the
//     "clean stop with no output and no error" defect the reference records;
//   - `[DONE]` is not part of the Anthropic protocol and is treated as the end of
//     the stream if some gateway adds it.
func relayAnthropicFrames(body []byte, model string) ([]pluginapi.ExecutorStreamChunk, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, statusError(true, "empty_upstream", http.StatusBadGateway,
			"ZCode 返回了空响应体（上游 200 但没有任何内容）")
	}
	frames := splitSSEFrames(body)
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(frames)+1)
	contentSeen := false
	for _, frame := range frames {
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		event, data := parseSSEFrame(frame)
		if strings.TrimSpace(data) == "[DONE]" {
			break
		}
		if event == "error" || eventTypeOf(data) == "error" {
			return nil, anthropicErrorFrame(data)
		}
		// ⚠ Only a CONTENT frame counts as content. `message_start`, `ping`,
		// `message_delta` and `message_stop` all arrive on a stream that never
		// produced a block, and that shape is exactly what "this account has no
		// entitlement for this model" looks like: a fast 200 with no content.
		if isContentFrame(event, data) {
			contentSeen = true
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: normalizeFrame(frame)})
	}
	if len(chunks) == 0 {
		return nil, statusError(true, "empty_upstream", http.StatusBadGateway,
			"ZCode 流中没有可解析的分帧")
	}
	// A 200 with zero content is the shape "this account has no entitlement for
	// this model" takes — the request never reaches the model. Saying so is far
	// more useful than handing the client an empty answer.
	if !contentSeen {
		return nil, statusError(false, "quota_exhausted", http.StatusPaymentRequired,
			"ZCode 账号在模型 %q 上没有可用权益：上游返回 200 但没有任何内容块"+
				"（额度已用尽或该模型未对本账号开放）。请等待额度按自然日重置、改用其它模型，或在状态页添加账号",
			model)
	}
	return chunks, nil
}

// isContentFrame reports whether a frame is part of a CONTENT BLOCK.
//
// ⚠ The list is deliberately just the three content-block events. `message_start`,
// `message_delta` and `message_stop` all appear on a stream that never produced a
// block — and that shape is precisely how "this account has no entitlement for this
// model" presents itself, so counting them as content would classify the failure as
// a successful empty answer.
func isContentFrame(event, data string) bool {
	switch eventTypeOf(data) {
	case "content_block_start", "content_block_delta", "content_block_stop":
		return true
	}
	switch event {
	case "content_block_start", "content_block_delta", "content_block_stop":
		return true
	}
	return false
}

// eventTypeOf reads the `type` member of a data payload without allocating a
// struct for it.
func eventTypeOf(data string) string {
	trimmed := strings.TrimSpace(data)
	if !strings.HasPrefix(trimmed, "{") {
		return ""
	}
	var probe struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &probe); errUnmarshal != nil {
		return ""
	}
	return probe.Type
}

// anthropicErrorFrame turns an `error` event into a classified failure.
//
// `overloaded_error` is transient and stays retryable; everything else is a
// server-side failure. The message is carried through verbatim because the model
// provider's own wording is the most useful thing available.
func anthropicErrorFrame(data string) error {
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(data)), &payload)
	message := strings.TrimSpace(payload.Error.Message)
	if message == "" {
		message = truncate(data, 300)
	}
	if strings.EqualFold(payload.Error.Type, "overloaded_error") {
		return statusError(true, "upstream_overloaded", http.StatusTooManyRequests,
			"ZCode 上游过载：%s", message)
	}
	return transportError("upstream_error_frame", "ZCode 上游返回错误事件：%s", message)
}

// normalizeFrame guarantees the trailing blank line the host and the direct
// clients both need, and rewrites bare LF separators to LF (never CRLF): the
// translators split on `\n` and would keep a stray `\r` inside the JSON.
func normalizeFrame(frame []byte) []byte {
	cleaned := bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n"))
	cleaned = bytes.TrimRight(cleaned, "\n")
	var builder bytes.Buffer
	builder.Write(cleaned)
	builder.WriteString("\n\n")
	return builder.Bytes()
}

// splitSSEFrames cuts a body into raw frames on the earliest blank-line
// separator, accepting both LF and CRLF.
//
// ⚠ The boundary search must take whichever separator comes FIRST. A CRLF stream
// contains `\n\n` inside `\r\n\r\n` (bytes 2-3), so testing `\n\n` first splits in
// the wrong place and leaves a leading `\r` on the next frame.
func splitSSEFrames(body []byte) [][]byte {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	parts := bytes.Split(normalized, []byte("\n\n"))
	frames := make([][]byte, 0, len(parts))
	for _, part := range parts {
		if len(bytes.TrimSpace(part)) == 0 {
			continue
		}
		frames = append(frames, part)
	}
	return frames
}

// parseSSEFrame reads the `event:` and joined `data:` lines of one frame.
func parseSSEFrame(frame []byte) (string, string) {
	event := ""
	dataLines := make([]string, 0, 2)
	for _, line := range bytes.Split(frame, []byte("\n")) {
		text := string(line)
		switch {
		case strings.HasPrefix(text, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(text, "event:"))
		case strings.HasPrefix(text, "data:"):
			dataLines = append(dataLines, strings.TrimLeft(strings.TrimPrefix(text, "data:"), " "))
		}
	}
	return event, strings.Join(dataLines, "\n")
}
