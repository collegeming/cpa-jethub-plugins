package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/openai"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Chat execution, ported from jethub-src/src/lobsterai-adapter.ts:428-1306.
//
// The LobsterAI chat endpoint is OpenAI-compatible and SSE-only: sending
// `stream:false` returns HTTP 500, so every upstream call is streamed and the
// non-streaming route aggregates the frames afterwards.
//
// Unlike the TypeScript adapter — which builds the body from harness message
// blocks — the CPA executor receives an already-serialized Chat Completions
// body from the host. Rebuilding it through the typed openai.Request struct
// would silently drop every field this plugin does not know about, so the body
// is mutated as a generic JSON object: unknown fields survive byte-for-byte and
// only the fields that must change are touched.

// executorStreamResponse is the wire shape of executor.execute_stream.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// chatCall is a prepared upstream chat request.
type chatCall struct {
	URL     string
	Body    []byte
	Headers http.Header
	Model   string
	Remote  remoteModel
	Version string
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// prepareChatBody mutates an inbound Chat Completions body into the shape the
// LobsterAI endpoint requires, consuming the remote model parameters:
//
//   - `model` is taken from the host's selected model when present;
//   - `stream` is forced to true (stream:false is rejected with HTTP 500);
//   - `max_tokens` is filled from the remote model's maxTokens (authoritative
//     single-response budget) when the caller did not supply one, then from the
//     static config; when neither exists the field stays absent rather than
//     inventing a number. `max_completion_tokens` wins when the caller used it;
//   - `reasoning_effort` accepts the product-side level name and is rewritten to
//     the wire value (openclawLevel) using the remote thinkingConfig;
//   - `tool_choice` is removed when empty / "none" / null, because an arbitrary
//     forwarded client body may contain a value the upstream rejects;
//   - `prompt_cache_key` is never added: that is a Tencent backend mechanism and
//     is not known to work here.
func prepareChatBody(payload []byte, model string, cfg Config, remote remoteModel) ([]byte, string, error) {
	record := map[string]any{}
	if len(payload) > 0 {
		if errUnmarshal := decodeJSON(payload, &record); errUnmarshal != nil {
			return nil, "", abiboot.HTTPError("invalid_request", http.StatusBadRequest, "decode chat request: %v", errUnmarshal)
		}
	}
	if strings.TrimSpace(model) != "" {
		record["model"] = strings.TrimSpace(model)
	}
	resolved := readStringField(record, "model")
	if resolved == "" {
		return nil, "", abiboot.HTTPError("invalid_request", http.StatusBadRequest, "chat request is missing a model")
	}
	if messages, ok := record["messages"].([]any); !ok || len(messages) == 0 {
		return nil, "", abiboot.HTTPError("invalid_request", http.StatusBadRequest, "chat request has no messages")
	}

	// SSE only.
	record["stream"] = true

	_, hasMaxCompletionTokens := record["max_completion_tokens"]
	existing, hasMaxTokens := readNumberField(record, "max_tokens")
	if !hasMaxCompletionTokens && (!hasMaxTokens || existing <= 0) {
		switch {
		case remote.MaxTokens != nil && *remote.MaxTokens > 0:
			record["max_tokens"] = *remote.MaxTokens
		case cfg.DefaultMaxTokens > 0:
			record["max_tokens"] = cfg.DefaultMaxTokens
		default:
			// No authoritative value anywhere: leave the field out.
			delete(record, "max_tokens")
		}
	}

	if requested, ok := record["reasoning_effort"].(string); ok {
		mapped := mapReasoningEffort(remote, requested)
		if mapped == "" {
			delete(record, "reasoning_effort")
		} else {
			record["reasoning_effort"] = mapped
		}
	}

	if toolChoice, exists := record["tool_choice"]; exists {
		switch typed := toolChoice.(type) {
		case nil:
			delete(record, "tool_choice")
		case string:
			if typed == "" || strings.EqualFold(typed, "none") {
				delete(record, "tool_choice")
			}
		}
	}

	encoded, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return nil, "", abiboot.Errorf("encode_request", "encode LobsterAI chat request: %v", errMarshal)
	}
	return encoded, resolved, nil
}

// prepareChatCall builds the outbound request for a chat execution.
func prepareChatCall(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config) (*chatCall, error) {
	now := time.Now()
	remote, _, _ := modelByID(request.Model, now)
	if request.Model != "" && remote.ID == "" {
		remote = remoteModel{ID: request.Model, Name: request.Model}
	}
	body, model, errPrepare := prepareChatBody(request.Payload, request.Model, cfg, remote)
	if errPrepare != nil {
		return nil, errPrepare
	}
	version := resolveClientVersion(h, cfg)
	return &chatCall{
		URL:     APIBase + ChatPath,
		Body:    body,
		Headers: chatHeaders(credential, version),
		Model:   model,
		Remote:  remote,
		Version: version,
	}, nil
}

// sendChat performs one upstream chat call.
func sendChat(h *abiboot.Host, credential *Credential, call *chatCall) (*pluginapi.HTTPResponse, error) {
	if h == nil {
		return nil, abiboot.Errorf("upstream_transport", "host transport 不可用")
	}
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodPost,
		URL:     call.URL,
		Headers: chatHeaders(credential, call.Version),
		Body:    call.Body,
	})
	if errDo != nil {
		return nil, abiboot.Errorf("upstream_transport", "LobsterAI 请求失败：%v", errDo)
	}
	return response, nil
}

// executorCredential renews the credential before an inference request signs
// with it: the host's own refresh timer restarts with the process, so an expired
// token would otherwise be signed into a request the gateway is bound to reject.
// The single post-401 retry below stays as the last line of defence.
func executorCredential(h *abiboot.Host, request pluginapi.ExecutorRequest) (*Credential, error) {
	fresh, errFresh := ensureCredentialFresh(h, authrefresh.Request{
		Name:        request.AuthID,
		StorageJSON: request.StorageJSON,
		Attributes:  request.AuthAttributes,
	})
	if errFresh != nil {
		return nil, errFresh
	}
	return ParseCredential(fresh.Storage)
}

// chatWithCredentialRetry performs the chat call and retries exactly once after
// a silent renewal on 401/403, matching the reference adapter
// (lobsterai-adapter.ts:940-950). The refreshed credential is also returned so
// callers can persist it.
func chatWithCredentialRetry(h *abiboot.Host, credential *Credential, call *chatCall, cfg Config) (*pluginapi.HTTPResponse, *Credential, error) {
	response, errSend := sendChat(h, credential, call)
	if errSend != nil {
		return nil, credential, errSend
	}
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		return response, credential, nil
	}
	if !credential.Refreshable() {
		return response, credential, nil
	}
	refreshed, errRenew := renewCredential(h, credential, cfg)
	if errRenew != nil || refreshed == nil {
		return response, credential, nil
	}
	retried, errRetry := sendChat(h, refreshed, call)
	if errRetry != nil {
		return nil, refreshed, errRetry
	}
	return retried, refreshed, nil
}

// handleExecutorExecute serves a non-streaming completion. The upstream call is
// always streamed and the frames are folded into one chat.completion.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := executorCredential(h, request)
	if errCredential != nil {
		return nil, errCredential
	}
	cfg := settings()
	call, errPrepare := prepareChatCall(h, request, credential, cfg)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, _, errChat := chatWithCredentialRetry(h, credential, call, cfg)
	if errChat != nil {
		return nil, errChat
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, executorErrorFor(response.StatusCode, response.Body)
	}

	outcome, errStream := consumeUpstreamStream(response.Body)
	if errStream != nil {
		return nil, errStream
	}
	completion := aggregateCompletion(outcome, call.Model)
	payload, errMarshal := json.Marshal(completion)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_response", "encode completion: %v", errMarshal)
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Metadata: map[string]any{
			"model":          call.Model,
			"client_version": call.Version,
		},
	}, nil
}

// handleExecutorExecuteStream serves a streaming completion.
//
// The frames are returned in the response envelope rather than pushed through
// host.stream.emit: both are valid ABI paths, and returning them avoids
// double-emitting into the host's stream bridge at the cost of buffering.
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := executorCredential(h, request)
	if errCredential != nil {
		return nil, errCredential
	}
	cfg := settings()
	call, errPrepare := prepareChatCall(h, request, credential, cfg)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, _, errChat := chatWithCredentialRetry(h, credential, call, cfg)
	if errChat != nil {
		return nil, errChat
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, executorErrorFor(response.StatusCode, response.Body)
	}

	outcome, errStream := consumeUpstreamStream(response.Body)
	if errStream != nil {
		return nil, errStream
	}
	if len(outcome.Payloads) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "LobsterAI 未返回任何流式分片")
	}

	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(outcome.Payloads)+1)
	for _, payload := range outcome.Payloads {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.Encode(payload)})
	}
	if !outcome.SawDone {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.DoneEvent()})
	}
	return executorStreamResponse{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  chunks,
	}, nil
}

// handleExecutorCountTokens returns a coarse token estimate. CPA falls back to
// its own tokenizer when the executor does not count, so an approximation beats
// an error.
func handleExecutorCountTokens(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	size := len(request.Payload)
	if size == 0 {
		size = len(request.OriginalRequest)
	}
	return pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":` + itoa(size/4+1) + `}`)}, nil
}

// streamOutcome is the result of consuming one upstream SSE body.
type streamOutcome struct {
	// Payloads are the outbound `data:` payloads in order (without framing).
	Payloads []string
	// SawDone reports whether the upstream sent `[DONE]`.
	SawDone bool
	// Collected content, used by the non-streaming aggregation.
	Content          strings.Builder
	ReasoningContent strings.Builder
	ToolCalls        []collectedToolCall
	FinishReason     string
	Usage            *openai.Usage

	// frameRecords keeps the mutable form of every outbound frame so the final
	// finish_reason rewrite can be applied to the last one.
	frameRecords []frameRecord
	// truncated reports that some tool arguments lost fragments in flight.
	truncated bool
	// finishReasonFinal is the finish_reason written to the last frame.
	finishReasonFinal string
}

// collectedToolCall is one assembled tool invocation.
type collectedToolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments string
	Truncated bool
}

// streamFrame is the subset of an SSE chunk this adapter must interpret.
//
// `content` and `reasoning_content` are declared as json.RawMessage rather than
// string on purpose: the upstream sends them as explicit JSON null in most
// frames (a model answers on one channel while the other stays null), and the
// TypeScript reference explicitly guards with `typeof === 'string'`. Raw
// messages let the Go port distinguish absent / null / wrong-type / string
// instead of collapsing all of them into "".
type streamFrame struct {
	Choices []struct {
		Delta struct {
			Content          json.RawMessage `json:"content"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message *struct {
			Content          json.RawMessage `json:"content"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openai.Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// rawString reads a JSON value that must be a string. Explicit null, absence
// and non-string types all report ok=false, which is exactly the reference
// `typeof value === 'string'` test.
func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false
	}
	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal != nil {
		return "", false
	}
	return text, true
}

// frameRecord is one outbound frame plus its mutable JSON form.
type frameRecord struct {
	payload string
	record  map[string]any
	mutated bool
	decoded bool
}

// consumeUpstreamStream parses an upstream SSE body into outbound payloads and
// aggregate state. It re-encodes a frame only when a rewrite was needed, so
// untouched frames are relayed byte-for-byte.
func consumeUpstreamStream(body []byte) (*streamOutcome, error) {
	outcome := &streamOutcome{}
	scanner := &sse.Scanner{}
	frames := make([]frameRecord, 0, 16)
	toolStates := map[int]*collectedToolCall{}
	toolOrder := make([]int, 0, 2)
	lastChoiceFrame := -1
	gotAnyContent := false

	for _, payload := range scanner.Feed(body) {
		if payload == sse.Done {
			outcome.SawDone = true
			break
		}
		frame := streamFrame{}
		_ = decodeJSON([]byte(payload), &frame)
		if frame.Error != nil {
			message := frame.Error.Message
			if message == "" {
				message = "unknown error"
			}
			return nil, abiboot.HTTPError("upstream_stream_error", http.StatusBadGateway, "LobsterAI 流内错误：%s", message)
		}

		entry := frameRecord{payload: payload}
		if errUnmarshal := decodeJSON([]byte(payload), &entry.record); errUnmarshal == nil {
			entry.decoded = true
		}

		if len(frame.Choices) > 0 {
			lastChoiceFrame = len(frames)
			choice := frame.Choices[0]

			// Content: the delta is authoritative; a chunk carrying a whole
			// `message.content` is a compatibility fallback accepted only before
			// any real delta content has been seen. The reference flags this with
			// `gotAnyContent` so the two shapes can never double up.
			deltaContent, deltaIsString := rawString(choice.Delta.Content)
			messageContent, messageIsString := "", false
			if choice.Message != nil {
				messageContent, messageIsString = rawString(choice.Message.Content)
			}
			switch {
			case deltaIsString && deltaContent != "":
				gotAnyContent = true
				outcome.Content.WriteString(deltaContent)
			case !gotAnyContent && messageIsString && messageContent != "":
				outcome.Content.WriteString(messageContent)
				entry.mutateChoice(func(choice map[string]any) bool {
					delta := ensureObject(choice, "delta")
					if existing, isString := delta["content"].(string); isString && existing == messageContent {
						return false
					}
					delta["content"] = messageContent
					return true
				})
			}

			if reasoning, ok := rawString(choice.Delta.ReasoningContent); ok && reasoning != "" {
				outcome.ReasoningContent.WriteString(reasoning)
			}

			for position, call := range choice.Delta.ToolCalls {
				index := 0
				if call.Index != nil {
					index = *call.Index
				}
				state, exists := toolStates[index]
				if !exists {
					state = &collectedToolCall{Index: index}
					toolStates[index] = state
					toolOrder = append(toolOrder, index)
				}
				if call.ID != "" {
					state.ID = call.ID
				}
				// Only a non-empty name may overwrite: later fragments carry an
				// empty name and would otherwise erase the parsed tool name.
				if call.Function.Name != "" {
					state.Name = call.Function.Name
				}
				state.Arguments += call.Function.Arguments
				if entry.decoded {
					captured := index
					entry.mutateChoice(func(choice map[string]any) bool {
						return normalizeToolCallEntry(choice, captured, position)
					})
				}
			}

			if choice.FinishReason != nil {
				outcome.FinishReason = *choice.FinishReason
			}
		}

		if frame.Usage != nil {
			outcome.Usage = frame.Usage
		}
		frames = append(frames, entry)
	}

	// Assemble the tool calls in first-seen order.
	for _, index := range toolOrder {
		state := toolStates[index]
		state.Truncated = isTruncatedArguments(state.Arguments)
		if state.Truncated {
			outcome.truncated = true
		}
		if state.ID == "" {
			state.ID = "call_" + itoa(index)
		}
		outcome.ToolCalls = append(outcome.ToolCalls, *state)
	}

	// Decide the reported finish reason. Three kinds of "incomplete" all report
	// length: an explicit `length`, a stream that ended without any
	// finish_reason while tool calls were being assembled (connection cut
	// mid-arguments), and arguments that do not parse (fragments lost).
	switch {
	case outcome.FinishReason == "length":
		outcome.finishReasonFinal = "length"
	case outcome.FinishReason == "" && len(outcome.ToolCalls) > 0:
		outcome.finishReasonFinal = "length"
	case outcome.truncated:
		outcome.finishReasonFinal = "length"
	case outcome.FinishReason != "":
		outcome.finishReasonFinal = outcome.FinishReason
	case len(outcome.ToolCalls) > 0:
		outcome.finishReasonFinal = "tool_calls"
	default:
		outcome.finishReasonFinal = "stop"
	}

	// Normalise empty tool arguments to `{}` (a no-argument tool streams a
	// single empty fragment). Truncated arguments are left alone on purpose:
	// inventing `{}` would fabricate a plausible-looking call.
	for position := range frames {
		entry := &frames[position]
		if !entry.decoded {
			continue
		}
		for _, state := range outcome.ToolCalls {
			if state.Truncated || state.Arguments != "" {
				continue
			}
			index := state.Index
			entry.mutateChoice(func(choice map[string]any) bool {
				return setEmptyToolArguments(choice, index)
			})
		}
	}

	// Apply the finish_reason rewrite to the last frame that carries a choice.
	if lastChoiceFrame >= 0 && lastChoiceFrame < len(frames) {
		entry := &frames[lastChoiceFrame]
		if entry.decoded {
			reason := outcome.finishReasonFinal
			entry.mutateChoice(func(choice map[string]any) bool {
				if existing, _ := choice["finish_reason"].(string); existing == reason {
					return false
				}
				choice["finish_reason"] = reason
				return true
			})
		}
	}

	for position := range frames {
		entry := &frames[position]
		if entry.mutated && entry.decoded {
			if encoded, errMarshal := json.Marshal(entry.record); errMarshal == nil {
				outcome.Payloads = append(outcome.Payloads, string(encoded))
				continue
			}
		}
		outcome.Payloads = append(outcome.Payloads, entry.payload)
	}
	if outcome.SawDone {
		outcome.Payloads = append(outcome.Payloads, sse.Done)
	}
	return outcome, nil
}

// mutateChoice applies fn to the first choice object of a frame. The frame is
// only marked dirty when fn reports a change, so a frame that already has the
// right shape is relayed byte-for-byte.
func (f *frameRecord) mutateChoice(fn func(choice map[string]any) bool) {
	if !f.decoded {
		return
	}
	choices, ok := f.record["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return
	}
	if fn(choice) {
		f.mutated = true
	}
}

// ensureObject returns a nested object, creating it when absent.
func ensureObject(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key].(map[string]any); ok {
		return existing
	}
	created := map[string]any{}
	parent[key] = created
	return created
}

// normalizeToolCallEntry applies the per-fragment tool-call normalisation: the
// index is always present, and an id-less fragment gets a stable placeholder so
// clients that key by id do not merge unrelated calls.
func normalizeToolCallEntry(choice map[string]any, index int, position int) bool {
	delta := asRecord(choice["delta"])
	if delta == nil {
		return false
	}
	rawCalls, ok := delta["tool_calls"].([]any)
	if !ok || position >= len(rawCalls) {
		return false
	}
	call, ok := rawCalls[position].(map[string]any)
	if !ok {
		return false
	}
	changed := false
	if existing, found := readNumberField(call, "index"); !found || int(existing) != index {
		call["index"] = index
		changed = true
	}
	if id, _ := call["id"].(string); id == "" {
		call["id"] = "call_" + itoa(index)
		changed = true
	}
	if typeName, _ := call["type"].(string); typeName == "" {
		call["type"] = "function"
		changed = true
	}
	function := asRecord(call["function"])
	if function == nil {
		function = map[string]any{}
		call["function"] = function
		changed = true
	}
	if _, hasArguments := function["arguments"]; !hasArguments {
		function["arguments"] = ""
		changed = true
	}
	return changed
}

// setEmptyToolArguments fills an empty `arguments` with `{}` for a call index.
func setEmptyToolArguments(choice map[string]any, index int) bool {
	delta := asRecord(choice["delta"])
	if delta == nil {
		return false
	}
	rawCalls, ok := delta["tool_calls"].([]any)
	if !ok {
		return false
	}
	for _, raw := range rawCalls {
		call := asRecord(raw)
		if call == nil {
			continue
		}
		if callIndex, found := readNumberField(call, "index"); !found || int(callIndex) != index {
			continue
		}
		function := asRecord(call["function"])
		if function == nil {
			continue
		}
		if arguments, _ := function["arguments"].(string); strings.TrimSpace(arguments) == "" {
			function["arguments"] = "{}"
			return true
		}
	}
	return false
}

// isTruncatedArguments reports whether tool arguments are non-empty but
// unparseable, which means fragments were lost in flight. An empty string is
// NOT truncated — a no-argument tool legitimately streams one empty fragment.
func isTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var parsed any
	return decodeJSON([]byte(trimmed), &parsed) != nil
}

// normalizeToolArguments returns arguments that are valid JSON objects, `{}`
// for empty or non-object payloads, and the original text when it parses to an
// object.
func normalizeToolArguments(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	var parsed any
	if errUnmarshal := decodeJSON([]byte(trimmed), &parsed); errUnmarshal != nil {
		return "{}"
	}
	if _, isObject := parsed.(map[string]any); !isObject {
		return "{}"
	}
	return trimmed
}

// aggregateCompletion folds the consumed stream into one chat.completion
// response for a non-streaming client.
func aggregateCompletion(outcome *streamOutcome, model string) openai.Completion {
	completion := openai.Completion{
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		ID:      "chatcmpl-" + mustRandomHex(16),
	}
	message := openai.Message{Role: "assistant"}
	content := outcome.Content.String()
	// OpenAI requires content:null when the message carries only tool calls.
	if content == "" && len(outcome.ToolCalls) > 0 {
		message.Content = nil
	} else {
		message.Content = content
	}
	if reasoning := outcome.ReasoningContent.String(); reasoning != "" {
		message.ReasoningContent = reasoning
	}
	for _, call := range outcome.ToolCalls {
		index := call.Index
		arguments := call.Arguments
		if !call.Truncated {
			arguments = normalizeToolArguments(arguments)
		}
		message.ToolCalls = append(message.ToolCalls, openai.ToolCall{
			Index:    &index,
			ID:       call.ID,
			Type:     "function",
			Function: openai.ToolCallFunction{Name: call.Name, Arguments: arguments},
		})
	}
	finishReason := outcome.finishReasonFinal
	completion.Usage = outcome.Usage
	completion.Choices = []openai.Choice{{Index: 0, Message: message, FinishReason: &finishReason}}
	return completion
}

// mustRandomHex returns n random hex bytes, degrading to a timestamp when the
// entropy source is unavailable.
func mustRandomHex(n int) string {
	value, errRandom := randomHex(n)
	if errRandom != nil {
		return itoa(int(time.Now().UnixNano() & 0x7fffffff))
	}
	return value
}
