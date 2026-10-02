package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/openai"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// PluginVersion is the adapter version reported in the user agent.
const PluginVersion = "0.1.0"

const (
	queueStatusPath = "/api/v1/queue/status"
	// Jet-Hub polls the queue for up to 30 minutes (180 attempts x 10s). A
	// plugin method invocation should not hold the host for that long, so this
	// reference implementation retries a bounded number of times instead.
	queueRetryAttempts = 3
	queueRetryInterval = 10 * time.Second
)

// attributionUserAgent identifies this adapter to the gateway. It is an
// unsigned header, so it never participates in the signature.
func attributionUserAgent() string {
	return "cpa-jethub-codearts/" + PluginVersion + " (+https://github.com/collegeming/cpa-jethub-plugins)"
}

// executorStreamResponse is the wire shape of executor.execute_stream.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// chatCall is a fully prepared, signed upstream chat request.
type chatCall struct {
	URL       string
	Body      []byte
	Headers   http.Header
	Model     string
	SessionID string
	// ImagesStripped counts the image parts degraded to text placeholders
	// because every model this plugin serves is text-only (vision.go).
	ImagesStripped int
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// executorRefreshRequest names the credential an executor call was dispatched
// for: the host passes the auth record id, which for a file-backed credential
// is exactly the auth file name the refreshed credential must be written back
// to, plus the credential's own attributes as a fallback.
func executorRefreshRequest(request pluginapi.ExecutorRequest) authrefresh.Request {
	return authrefresh.Request{
		Name:        request.AuthID,
		StorageJSON: request.StorageJSON,
		Attributes:  request.AuthAttributes,
	}
}

// prepareChatCall signs the upstream chat request and attaches the unsigned
// attribution headers.
func prepareChatCall(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config) (*chatCall, error) {
	sessionID := newTraceID()
	body, parsed, imagesStripped, errPrepare := prepareRequestBody(request.Payload, request.Model, cfg, sessionID)
	if errPrepare != nil {
		return nil, errPrepare
	}
	if imagesStripped > 0 && h != nil {
		// Warn, not debug: the turn still succeeds, but the model answers
		// without having seen the image. Silence here is exactly what made the
		// original 406 look like a phantom failure (vision.go).
		//
		// ⚠️ The count belongs in the MESSAGE: CPA's console formatter prints
		// only the field names in `logFieldOrder`
		// (`internal/logging/global_logger.go:55-59`, CPA v8.0.4), so a custom
		// key such as `images` is dropped silently. `model` IS whitelisted and
		// stays a field.
		h.Log("warn", "CodeArts 剥离了请求中的图片（模型为纯文本），共 "+itoa(imagesStripped)+" 处",
			map[string]any{"model": parsed.Model})
	}

	chatURL := SnapEngineBase + ChatAPIPath
	parsedURL, errParse := url.Parse(chatURL)
	if errParse != nil {
		return nil, abiboot.Errorf("invalid_url", "parse chat endpoint: %v", errParse)
	}

	// glm-5.3-flash is a benefit model and must carry a signed maas_type header.
	extraSigned := map[string]string{}
	if cfg.BenefitModels && parsed.Model == BenefitModel {
		extraSigned["maas_type"] = "benefit"
	}

	wire := &http.Request{Method: http.MethodPost, URL: parsedURL, Header: http.Header{}}
	SignRequest(wire, body, credential.AccessKeyID, credential.SecretAccessKey, credential.SecurityToken, time.Now(), extraSigned)

	wire.Header.Set("User-Agent", attributionUserAgent())
	wire.Header["Chat-Id"] = []string{newTraceID()}
	wire.Header["Session-Id"] = []string{sessionID}
	wire.Header["lang"] = []string{"en"}

	return &chatCall{URL: chatURL, Body: body, Headers: wire.Header, Model: parsed.Model, SessionID: sessionID, ImagesStripped: imagesStripped}, nil
}

// handleExecutorExecute serves a non-streaming completion. The upstream call is
// always streamed and the chunks are folded into one chat.completion.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	// The credential is renewed before it is used to sign anything: the host's
	// own refresh timer restarts with the process, so an expired token would
	// otherwise be signed into a request the gateway is bound to reject.
	fresh, errFresh := ensureCredentialFresh(h, executorRefreshRequest(request))
	if errFresh != nil {
		return nil, errFresh
	}
	credential, errCredential := ParseCredential(fresh.Storage)
	if errCredential != nil {
		return nil, errCredential
	}
	call, errPrepare := prepareChatCall(h, request, credential, settings())
	if errPrepare != nil {
		return nil, errPrepare
	}

	response, errChat := chatWithQueueRetry(h, credential, call)
	if errChat != nil {
		return nil, errChat
	}
	chunks, errCollect := collectChunks(response.Body)
	if errCollect != nil {
		return nil, errCollect
	}
	completion := aggregateCompletion(chunks, call.Model)
	payload, errMarshal := json.Marshal(completion)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_response", "encode completion: %v", errMarshal)
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Metadata: map[string]any{
			"model":      call.Model,
			"session_id": call.SessionID,
		},
	}, nil
}

// handleExecutorExecuteStream serves a streaming completion.
//
// The translated frames are returned in the response envelope rather than
// pushed through host.stream.emit. Both are valid ABI paths; returning them
// avoids double-emitting into the host's stream bridge at the cost of
// buffering the response. See the README for the trade-off.
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	fresh, errFresh := ensureCredentialFresh(h, executorRefreshRequest(request))
	if errFresh != nil {
		return nil, errFresh
	}
	credential, errCredential := ParseCredential(fresh.Storage)
	if errCredential != nil {
		return nil, errCredential
	}
	call, errPrepare := prepareChatCall(h, request, credential, settings())
	if errPrepare != nil {
		return nil, errPrepare
	}

	response, errChat := chatWithQueueRetry(h, credential, call)
	if errChat != nil {
		return nil, errChat
	}

	scanner := &sse.Scanner{}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	for _, payload := range scanner.Feed(response.Body) {
		if errStream := inspectStreamPayload(payload); errStream != nil {
			return nil, errStream
		}
		if payload == sse.Done {
			// The host writes the terminal event itself.
			break
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.Payload(payload)})
	}
	if len(chunks) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "CodeArts 未返回任何流式分片")
	}
	chunks = rewriteDsmlStream(chunks)
	chunks = applyToolCallIDFallback(chunks)
	return executorStreamResponse{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  chunks,
	}, nil
}

// normalizeDsmlCall fills in the fields a client expects on a streamed tool
// call. The DSML parser already supplies a stable index and id.
func normalizeDsmlCall(call openai.ToolCall) openai.ToolCall {
	if call.Type == "" {
		call.Type = "function"
	}
	return call
}

// rewriteDsmlStream extracts DSML tool calls from the collected content deltas
// and re-encodes the affected chunks.
//
// This adapter buffers the upstream stream before returning it, so the rewrite
// is a single pass over chunks that are already in hand. The content of every
// delta is concatenated first and parsed once, which keeps DSML blocks that
// straddle two upstream chunks intact; the DSML text is then removed and the
// extracted calls are attached to the final delta as ordinary tool_calls. A
// stream that contains no DSML is returned untouched.
func rewriteDsmlStream(chunks []pluginapi.ExecutorStreamChunk) []pluginapi.ExecutorStreamChunk {
	type decodedChunk struct {
		position int
		chunk    openai.Chunk
	}

	decoded := make([]decodedChunk, 0, len(chunks))
	var content strings.Builder
	for position, item := range chunks {
		line := strings.TrimSpace(string(item.Payload))
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == sse.Done {
			continue
		}
		var parsed openai.Chunk
		if errUnmarshal := json.Unmarshal([]byte(line), &parsed); errUnmarshal != nil {
			// An unrecognised frame means the stream is not shaped the way this
			// rewrite assumes; leave it exactly as upstream sent it.
			return chunks
		}
		decoded = append(decoded, decodedChunk{position: position, chunk: parsed})
		for _, choice := range parsed.Choices {
			content.WriteString(choice.Delta.Content)
		}
	}

	// lastDelta and lastChoice locate the final delta of the buffered stream,
	// which is where flushed tail content and the finish_reason rewrite belong.
	lastDelta := func() *openai.Delta {
		for order := len(decoded) - 1; order >= 0; order-- {
			choices := decoded[order].chunk.Choices
			if len(choices) > 0 {
				return &decoded[order].chunk.Choices[len(choices)-1].Delta
			}
		}
		return nil
	}
	lastChoice := func() *openai.ChunkChoice {
		for order := len(decoded) - 1; order >= 0; order-- {
			choices := decoded[order].chunk.Choices
			if len(choices) > 0 {
				return &decoded[order].chunk.Choices[len(choices)-1]
			}
		}
		return nil
	}

	full := content.String()
	if !containsDsmlMarker(full) {
		return chunks
	}

	// A single parser drives every delta in order, so DSML blocks that straddle
	// chunk boundaries stay intact and `<thought>` text lands on the reasoning
	// channel of the delta it belongs to. The parser numbers calls sequentially
	// across the whole stream, which is exactly the index space a client needs.
	parser := NewDsmlStreamParser()
	totalCalls := 0
	for order := range decoded {
		for choiceIndex := range decoded[order].chunk.Choices {
			delta := &decoded[order].chunk.Choices[choiceIndex].Delta
			if delta.Content == "" {
				continue
			}
			visible, found := parser.Feed(delta.Content)
			if thought := parser.DrainReasoning(); thought != "" {
				delta.ReasoningContent += thought
			}
			delta.Content = visible
			for _, call := range found {
				delta.ToolCalls = append(delta.ToolCalls, normalizeDsmlCall(call))
				totalCalls++
			}
		}
	}

	// Release what the parser still holds: an unterminated DSML block degrades
	// to visible text, an unterminated thought block stays on the reasoning
	// channel, and neither is allowed to vanish.
	tail, tailCalls := parser.Flush()
	if last := lastDelta(); last != nil {
		if thought := parser.DrainReasoning(); thought != "" {
			last.ReasoningContent += thought
		}
		if tail != "" {
			last.Content += tail
		}
		for _, call := range tailCalls {
			last.ToolCalls = append(last.ToolCalls, normalizeDsmlCall(call))
			totalCalls++
		}
	}

	if totalCalls > 0 {
		if last := lastChoice(); last != nil {
			if last.FinishReason == nil || *last.FinishReason == "stop" {
				reason := "tool_calls"
				last.FinishReason = &reason
			}
		}
	}

	out := make([]pluginapi.ExecutorStreamChunk, len(chunks))
	copy(out, chunks)
	for _, item := range decoded {
		encoded, errMarshal := sse.PayloadJSON(item.chunk)
		if errMarshal != nil {
			return chunks
		}
		out[item.position] = pluginapi.ExecutorStreamChunk{Payload: encoded}
	}
	return out
}

// fallbackToolCallID is the stable synthetic id for one wire index.
//
// ⚠️ It must be derived from the index rather than drawn at random: every
// fragment of the same call has to end up with the same id, otherwise the tool
// result can never be paired with its call (`llm-adapter.ts:1168`).
func fallbackToolCallID(wireIndex int) string {
	return "call_" + itoa(wireIndex)
}

// applyToolCallIDFallback gives every streamed tool-call fragment a non-empty,
// stable id (`llm-adapter.ts:1171`, `:1484-1485`).
//
// This is a port of the standard `delta.tool_calls` branch of the CodeArts
// adapter. The Huawei backend intermittently omits `tool_calls[].id`
// altogether, or sends it as an empty string; forwarding that verbatim leaves
// the assistant message holding a tool call whose id is the empty string
// (`llm-adapter.ts:1150-1166`). The damage is not one failed turn but a bricked
// session: DSH's format-v4 check (`assertV4ToolResultMessage`) requires the
// tool result's `toolCallId` to equal the call's `callId` and BOTH to be
// non-empty (`length === 0`), so the `tool/result` write is rejected and
// crash recovery synthesises the same rejected patch from the empty id — the
// log stops at `tool/call` and only a human can repair it. Measured: 3/3
// CodeArts tool-call blocks across 31 sessions carried an empty id, while
// ~14,000 blocks from every other provider had none.
//
// The judgment criterion must therefore be "non-empty string", not "present":
// an empty string fails that check exactly like a missing one.
//
// The DSML branch needs no help here — its parser mints a real id
// (`dsml.go` `newDsmlCallID`) — and because that id is non-empty it is
// preserved untouched by the map below.
//
// Like rewriteDsmlStream, an unrecognised frame means the stream is not shaped
// the way this rewrite assumes, so the input is returned exactly as upstream
// sent it. A stream carrying no tool calls is likewise returned untouched.
func applyToolCallIDFallback(chunks []pluginapi.ExecutorStreamChunk) []pluginapi.ExecutorStreamChunk {
	// toolIds maps a wire index to the real id the backend issued for it.
	// ⚠️ Only a NON-EMPTY id is ever recorded: an unconditional overwrite would
	// let a later fragment's empty/missing id erase the real id the first
	// fragment delivered (same cause as the `function.name` empty-string
	// overwrite guarded elsewhere in this adapter).
	toolIds := map[int]string{}

	type decodedChunk struct {
		position int
		chunk    openai.Chunk
	}
	decoded := make([]decodedChunk, 0, len(chunks))
	for position, item := range chunks {
		line := strings.TrimSpace(string(item.Payload))
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == sse.Done {
			continue
		}
		var parsed openai.Chunk
		if errUnmarshal := json.Unmarshal([]byte(line), &parsed); errUnmarshal != nil {
			// An unrecognised frame means the stream is not shaped the way this
			// rewrite assumes; leave it exactly as upstream sent it.
			return chunks
		}
		rewritten := false
		for choiceIndex := range parsed.Choices {
			delta := &parsed.Choices[choiceIndex].Delta
			for callIndex := range delta.ToolCalls {
				call := &delta.ToolCalls[callIndex]
				// `call.index ?? 0` (`llm-adapter.ts:1483`): a fragment that
				// omits the index belongs to the call at index 0.
				wireIndex := 0
				if call.Index != nil {
					wireIndex = *call.Index
				}
				if call.ID != "" {
					toolIds[wireIndex] = call.ID
				}
				callID, known := toolIds[wireIndex]
				if !known {
					callID = fallbackToolCallID(wireIndex)
				}
				if call.ID != callID {
					call.ID = callID
					rewritten = true
				}
			}
		}
		if rewritten {
			// Every emitted fragment of the call carries the resolved id, not
			// only the first one.
			decoded = append(decoded, decodedChunk{position: position, chunk: parsed})
		}
	}
	if len(decoded) == 0 {
		return chunks
	}

	out := make([]pluginapi.ExecutorStreamChunk, len(chunks))
	copy(out, chunks)
	for _, item := range decoded {
		encoded, errMarshal := sse.PayloadJSON(item.chunk)
		if errMarshal != nil {
			return chunks
		}
		out[item.position] = pluginapi.ExecutorStreamChunk{Payload: encoded}
	}
	return out
}

// handleExecutorCountTokens returns a coarse token estimate. CPA falls back to
// its own tokenizer when the executor does not implement counting, so an
// approximation is preferable to an error.
func handleExecutorCountTokens(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	estimated := len(request.Payload)/4 + 1
	return pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":` + itoa(estimated) + `}`)}, nil
}

// chatWithQueueRetry performs the signed chat call, retrying while upstream
// reports the concurrency ceiling.
func chatWithQueueRetry(h *abiboot.Host, credential *Credential, call *chatCall) (*pluginapi.HTTPResponse, error) {
	var queueErr error
	for attempt := 0; attempt < queueRetryAttempts; attempt++ {
		if attempt > 0 {
			if errWait := waitForQueue(h, credential, call); errWait != nil {
				return nil, errWait
			}
		}
		response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
			Method:  http.MethodPost,
			URL:     call.URL,
			Headers: call.Headers,
			Body:    call.Body,
		})
		if errDo != nil {
			return nil, errDo
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		classified := classifyUpstreamFailure(response.StatusCode, response.Body)
		if errors.Is(classified, ErrQueueFull) {
			queueErr = classified
			time.Sleep(queueRetryInterval)
			continue
		}
		return nil, classified
	}
	if queueErr == nil {
		queueErr = ErrQueueFull
	}
	return nil, abiboot.HTTPError("queue_timeout", http.StatusTooManyRequests,
		"CodeArts 并发会话已满，重试 %d 次后仍不可用", queueRetryAttempts)
}

// waitForQueue asks the queue-status endpoint whether a retry is worthwhile.
// An unreachable or still-waiting queue is treated as retryable.
func waitForQueue(h *abiboot.Host, credential *Credential, call *chatCall) error {
	endpoint, errParse := url.Parse(SnapEngineBase + queueStatusPath)
	if errParse != nil {
		return nil
	}
	query := endpoint.Query()
	query.Set("model", call.Model)
	query.Set("task_id", call.SessionID)
	endpoint.RawQuery = query.Encode()

	traceID, _ := randomHex(16)
	unsigned := map[string]string{
		"x-snap-traceid": traceID,
		"Agent-Type":     "INFERHUB_AGENT",
		"X-Language":     "en",
	}
	response, errDo := signedGET(h, credential, endpoint.String(), nil, unsigned)
	if errDo != nil {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	var status struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if errDecode := json.Unmarshal(response.Body, &status); errDecode != nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(status.Status)) {
	case "error", "queue_full":
		return abiboot.HTTPError("queue_full", http.StatusTooManyRequests,
			"CodeArts 队列不可用：%s", truncate(status.Message, 200))
	default:
		return nil
	}
}

// collectChunks turns a buffered upstream body into chat completion chunks.
func collectChunks(body []byte) ([]openai.Chunk, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "CodeArts 返回了空响应体")
	}

	// A whole completion object (no SSE framing) is accepted as-is.
	if trimmed[0] == '{' && !bytes.Contains(trimmed, []byte("data:")) {
		completion := openai.Completion{}
		if errUnmarshal := json.Unmarshal(trimmed, &completion); errUnmarshal == nil && len(completion.Choices) > 0 {
			return chunksFromCompletion(completion), nil
		}
	}

	scanner := &sse.Scanner{}
	chunks := make([]openai.Chunk, 0, 64)
	for _, payload := range scanner.Feed(body) {
		if errStream := inspectStreamPayload(payload); errStream != nil {
			return nil, errStream
		}
		if payload == sse.Done {
			break
		}
		chunk := openai.Chunk{}
		if errUnmarshal := json.Unmarshal([]byte(payload), &chunk); errUnmarshal != nil {
			continue
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "CodeArts 流中没有可解析的分片")
	}
	return chunks, nil
}

// chunksFromCompletion projects a whole completion into a single delta chunk.
func chunksFromCompletion(completion openai.Completion) []openai.Chunk {
	chunks := make([]openai.Chunk, 0, len(completion.Choices))
	for _, choice := range completion.Choices {
		chunks = append(chunks, openai.Chunk{
			ID:      completion.ID,
			Object:  "chat.completion.chunk",
			Created: completion.Created,
			Model:   completion.Model,
			Usage:   completion.Usage,
			Choices: []openai.ChunkChoice{{
				Index: choice.Index,
				Delta: openai.Delta{
					Role:             choice.Message.Role,
					Content:          stringifyContent(choice.Message.Content),
					ReasoningContent: choice.Message.ReasoningContent,
					ToolCalls:        choice.Message.ToolCalls,
				},
				FinishReason: choice.FinishReason,
			}},
		})
	}
	return chunks
}

// stringifyContent renders a message content value as plain text.
func stringifyContent(content any) string {
	switch typed := content.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		encoded, errMarshal := json.Marshal(typed)
		if errMarshal != nil {
			return ""
		}
		return string(encoded)
	}
}

// itoa renders a non-negative int without importing strconv at call sites.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}
