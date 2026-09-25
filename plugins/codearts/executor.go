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
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/openai"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// prepareChatCall signs the upstream chat request and attaches the unsigned
// attribution headers.
func prepareChatCall(request pluginapi.ExecutorRequest, credential *Credential, cfg Config) (*chatCall, error) {
	sessionID := newTraceID()
	body, parsed, errPrepare := prepareRequestBody(request.Payload, request.Model, cfg, sessionID)
	if errPrepare != nil {
		return nil, errPrepare
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

	return &chatCall{URL: chatURL, Body: body, Headers: wire.Header, Model: parsed.Model, SessionID: sessionID}, nil
}

// handleExecutorExecute serves a non-streaming completion. The upstream call is
// always streamed and the chunks are folded into one chat.completion.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		return nil, errCredential
	}
	call, errPrepare := prepareChatCall(request, credential, settings())
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
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		return nil, errCredential
	}
	call, errPrepare := prepareChatCall(request, credential, settings())
	if errPrepare != nil {
		return nil, errPrepare
	}

	response, errChat := chatWithQueueRetry(h, credential, call)
	if errChat != nil {
		return nil, errChat
	}

	scanner := &sse.Scanner{}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	sawDone := false
	for _, payload := range scanner.Feed(response.Body) {
		if errStream := inspectStreamPayload(payload); errStream != nil {
			return nil, errStream
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.Encode(payload)})
		if payload == sse.Done {
			sawDone = true
			break
		}
	}
	if len(chunks) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "CodeArts 未返回任何流式分片")
	}
	chunks = rewriteDsmlStream(chunks)
	if !sawDone {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.DoneEvent()})
	}
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
		encoded, errMarshal := sse.EncodeJSON(item.chunk)
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
