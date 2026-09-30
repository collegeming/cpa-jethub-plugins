package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The MiniMax **Anthropic Messages** protocol layer, plus the framing helpers
// the executor uses to return Claude-shaped SSE frames to the host.
//
// ## What crosses this boundary, and what does NOT
//
// This executor declares `anthropic` on BOTH sides. The host therefore converts
// the client's request into an Anthropic Messages body and converts the
// Anthropic SSE back into whatever the client asked for (see
// `executorAdapter.prepareExecutorCall`, which sets the plugin's native format
// to `outputFormat` and translates the client request into `inputFormat`
// first). What arrives in `ExecutorRequest.Payload` is already Anthropic
// Messages, and what this package must return is Anthropic SSE — complete
// `event: X\ndata: {…}\n\n` frames, because the Claude client writer forwards
// each chunk VERBATIM (`ClaudeCodeAPIHandler.forwardClaudeStream` does
// `c.Writer.Write(chunk)` with no framing of its own). That is the opposite of
// the OpenAI path, where the host frames bare payload bytes as `data: %s\n\n`
// and appends `data: [DONE]`.
//
// ⚠️ The Anthropic body is therefore NOT built here. The host already did it,
// via the registered `openai -> claude` request translator. What this file owns
// is the small number of things the generic translator cannot know:
//
//   - the model ids and the thinking matrix (below);
//   - that a request which arrives with thinking DISABLED for an adaptive-only
//     model must be repaired before dispatch, or the vendor hard-fails it.
//
// ## Measured protocol facts (2026-09-29, real requests)
//
// Endpoint `POST {APIHost}/mavis/api/v1/llm/v1/messages`, `stream: true`.
// Headers: ONLY `Authorization`, `Content-Type` and `Accept: text/event-stream`
// — measured HTTP 200 with NO `anthropic-version` header, so none is sent.
//
// SSE events: `message_start`, `ping`, `content_block_start/delta/stop`,
// `message_delta`, `message_stop`, `error`.
//
// ⚠️ `signature_delta` is part of the thinking block's signature, not its text.
// Treating it as content injects a run of hex into the answer.
//
// ## ⚠️ The thinking matrix — fully measured, and the trickiest part
//
//	model                       remote mode    effort_options   what to send
//	--------------------------- -------------- ---------------- -------------
//	MiniMax-M3.1-Flash-Preview  forced_on      6 tiers          thinking:{type:"adaptive"}
//	                                                           (+ output_config.effort)
//	MiniMax-M3                  switchable     none             nothing => no thinking
//	                                                           adaptive => thinking
//	                                                           disabled => accepted
//	MiniMax-M2.7*               forced_on      none             nothing (server thinks anyway;
//	                                                           disabled is silently ignored)
//
// The hard failure that makes this a correctness issue rather than a
// preference, captured from the live server:
//
//	{"type":"error","error":{"type":"invalid_request_error","message":
//	 "invalid params, model \"MiniMax-M3.1-Flash-Preview\" requires adaptive
//	  thinking; thinking.type=\"disabled\" (including reasoning.effort=none)
//	  is not allowed (2013)"}}
//
// ⇒ a defensive rewrite is REQUIRED, because the host may hand us a payload
// whose `thinking.type` is already `"disabled"`: its OpenAI→Anthropic
// translator derives thinking from `reasoning_effort` and emits exactly that
// for `none`. Whatever this plugin declares in `Thinking.Levels` is not
// enforced by the host on that path, so declaring the levels is not enough.

// Anthropic content-block and event names used by the rewriter.
const (
	blockTypeThinking = "thinking"
	blockTypeImage    = "image"
	thinkingDisabled  = "disabled"
	thinkingAdaptive  = "adaptive"
	thinkingEnabled   = "enabled"
)

// sseFrame renders one complete Anthropic SSE frame.
//
// The trailing blank line is MANDATORY: it is the SSE event terminator, and the
// Claude client writer forwards the bytes verbatim, so a frame without it
// leaves the client waiting for the rest of the event.
func sseFrame(event string, payload []byte) []byte {
	out := make([]byte, 0, len(event)+len(payload)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}

// jsonFrame marshals a frame body and wraps it; a marshal failure yields nil so
// the caller can skip the frame instead of emitting half an event.
func jsonFrame(event string, body map[string]any) []byte {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil
	}
	return sseFrame(event, encoded)
}

// frameKind reports which Anthropic event a frame carries.
//
// ⚠️ The `event:` line is preferred over the body's `type` member only when the
// body has no `type`; both are read because a proxy may drop either one. Frames
// that are neither (a `ping`, a bare blank line, a non-JSON tail) report "".
func frameKind(frame []byte) (string, []byte) {
	text := strings.TrimSpace(string(frame))
	if text == "" {
		return "", nil
	}
	event := ""
	var dataLines []string
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) == 0 {
		return "", nil
	}
	payload := []byte(strings.Join(dataLines, "\n"))
	kind := ""
	if len(payload) > 0 {
		var probe struct {
			Type string `json:"type"`
		}
		if errUnmarshal := json.Unmarshal(payload, &probe); errUnmarshal == nil {
			kind = strings.TrimSpace(probe.Type)
		}
	}
	if kind == "" {
		kind = event
	}
	return kind, payload
}

// rewriteAdaptiveThinking repairs an Anthropic request body so an
// adaptive-only model cannot be sent `thinking.type="disabled"`.
//
// This is the defensive rewrite the measured HTTP 400 above makes mandatory.
// It does three things, in order:
//
//  1. an absent or non-object `thinking` on an adaptive-only model gets
//     `{"type":"adaptive"}` — the model thinks regardless, so saying so is the
//     honest request (and adding it cannot break the models that already work,
//     because this only runs for the `MiniMax-M3.1` prefix);
//  2. a `disabled` (or the legacy `enabled` with no budget) is rewritten to
//     `adaptive`, which is what turns a hard 400 into a normal answer;
//  3. a stray `budget_tokens` is dropped — the endpoint takes a thinking TYPE
//     and an `output_config.effort`, not a token budget, and forwarding an
//     unknown member next to a rewritten type is exactly the kind of payload
//     change that has no upside.
//
// ⚠️ The level selection is NOT touched here: whatever `output_config.effort`
// the host produced from the client's `reasoning_effort` is left alone, so the
// user's chosen tier still reaches the model. The rewriter only decides that
// thinking is ON.
//
// The function returns the body unchanged for every other model, and returns it
// unchanged when the body cannot be parsed as a JSON object — a payload this
// plugin cannot understand is not one it should silently replace.
func rewriteAdaptiveThinking(model string, body []byte) []byte {
	if !requiresAdaptiveThinking(model) || len(body) == 0 {
		return body
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return body
	}

	changed := false
	thinking, okThinking := root["thinking"].(map[string]any)
	if !okThinking {
		thinking = map[string]any{}
		changed = true
	}
	switch strings.ToLower(strings.TrimSpace(stringField(thinking, "type"))) {
	case thinkingAdaptive:
		// Already what the model requires.
	default:
		thinking["type"] = thinkingAdaptive
		changed = true
	}
	if _, present := thinking["budget_tokens"]; present {
		delete(thinking, "budget_tokens")
		changed = true
	}
	if !changed {
		return body
	}
	root["thinking"] = thinking
	encoded, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return body
	}
	return encoded
}

// stringField reads a trimmed string member of a decoded object.
func stringField(source map[string]any, key string) string {
	if text, ok := source[key].(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// upstreamErrorMessage extracts the vendor's error text from a failed
// inference answer, which is an Anthropic error object:
// `{"type":"error","error":{"type":…,"message":…}}`.
func upstreamErrorMessage(body []byte) string {
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		return ""
	}
	message := strings.TrimSpace(envelope.Error.Message)
	kind := strings.TrimSpace(envelope.Error.Type)
	switch {
	case message != "" && kind != "":
		return message + " (" + kind + ")"
	case message != "":
		return message
	default:
		return kind
	}
}

// streamErrorMessage extracts the message from a mid-stream `error` event.
//
// ⚠️ A mid-stream error MUST become a Go error, not a quiet end of stream. The
// earlier providers in this repository learned that the hard way: a consumer
// that only recognised the OpenAI error shape treated the frame as "finished,
// no content", so the UI showed a clean stop with no explanation at all.
func streamErrorMessage(payload []byte) string {
	var frame struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(payload, &frame); errUnmarshal != nil {
		return ""
	}
	message := strings.TrimSpace(frame.Error.Message)
	if message == "" {
		message = strings.TrimSpace(frame.Error.Type)
	}
	return message
}

// errorEvent renders the Anthropic error frame the executor emits when the
// upstream stream itself reports a failure.
func errorEvent(message string) []byte {
	if strings.TrimSpace(message) == "" {
		message = "minimax: 上游返回了流内错误"
	}
	return jsonFrame("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": message,
		},
	})
}

// normaliseStreamFrame validates one upstream frame and returns the bytes to
// forward.
//
// It exists to enforce the two rules that are invisible until they are broken:
//
//   - an `error` frame becomes a Go error, never a silent end of stream;
//   - a `signature_delta` is DROPPED. It carries the thinking block's
//     cryptographic signature, not its text; forwarding it as content makes the
//     host's Anthropic→OpenAI translator inject a hex string into the answer's
//     reasoning, and the reference records that exact defect.
//
// A frame that carries no recognisable event is forwarded untouched: `ping` and
// any future keep-alive must reach the client, and dropping unknown traffic is
// how a stream silently stalls.
func normaliseStreamFrame(frame []byte) ([]byte, error) {
	kind, payload := frameKind(frame)
	switch kind {
	case "error":
		message := streamErrorMessage(payload)
		if message == "" {
			message = "minimax: 上游返回了流内错误"
		}
		return nil, transportError("stream_error", "minimax: 流内错误：%s", message)
	case "content_block_delta":
		if deltaType := deltaKind(payload); deltaType == "signature_delta" {
			return nil, nil
		}
	}
	return frame, nil
}

// deltaKind reads `delta.type` out of a `content_block_delta` payload.
func deltaKind(payload []byte) string {
	var frame struct {
		Delta struct {
			Type string `json:"type"`
		} `json:"delta"`
	}
	if errUnmarshal := json.Unmarshal(payload, &frame); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(frame.Delta.Type)
}

// emptyStreamError builds the failure an empty upstream stream produces.
//
// ⚠️ An empty stream is an ERROR, never a quiet success. A consumer that ends
// silently makes the harness believe the model answered with no content, so it
// does not retry and the user sees nothing at all — the reported "no reply"
// defect the sibling plugins document.
func emptyStreamError() error {
	return transportError("empty_stream", "minimax: 模型未返回任何内容块")
}

// sawContentFrame reports whether a forwarded frame is evidence the model
// produced something. `message_start` and keep-alives do not count: a stream
// that only ever produced those is exactly the empty stream above.
func sawContentFrame(kind string) bool {
	switch kind {
	case "content_block_start", "content_block_delta", "content_block_stop":
		return true
	default:
		return false
	}
}

// validateMessagesBody rejects a body that cannot be an Anthropic Messages
// request before it costs an upstream round trip. It is deliberately shallow:
// the host built this payload, and second-guessing its message shaping would be
// the plugin re-implementing a translator it does not own.
func validateMessagesBody(body []byte) error {
	if len(body) == 0 {
		return abiboot.HTTPError("invalid_request", http.StatusBadRequest, "minimax: 收到空请求体")
	}
	var probe struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(body, &probe); errUnmarshal != nil {
		return abiboot.HTTPError("invalid_request", http.StatusBadRequest,
			"minimax: 解码 Anthropic 请求失败：%v", errUnmarshal)
	}
	return nil
}

// The Anthropic SSE consumer.
//
// It turns an upstream Anthropic Messages stream into the chunk list the host
// envelope carries. Three rules are encoded here, each of which was a real
// defect somewhere in this repository before it was written down:
//
//  1. **`signature_delta` is dropped.** It is the thinking block's signature,
//     not its text; forwarding it as content injects a run of hex into the
//     answer's reasoning.
//  2. **An empty stream is an ERROR.** A consumer that ends quietly makes the
//     harness believe the model answered with nothing, so it does not retry and
//     the user sees no reply and no explanation.
//  3. **A truncated stream's tail is still processed.** The last buffered bytes
//     are replayed as whole lines before the stream is declared finished. Real
//     SSE usually ends on a blank line, which hides the omission — but a
//     connection that closes early leaves the final `message_delta`, the one
//     carrying `stop_reason` and `usage`, sitting in the buffer. Dropping it
//     makes `max_tokens` look like a normal stop and reports zero usage.
//
// ⚠️ Frames are emitted as TWO chunks: the `event:` name line on its own, then
// the `data:` line with its terminating blank line. See the long comment in
// `executor.go` — an OpenAI client's translator rejects any chunk that does not
// START with `data:` (it discards the name-line chunk, which is exactly right),
// while a Claude client needs real SSE, which the two chunks reconstruct
// verbatim. Emitting them as one chunk breaks the first consumer; emitting only
// the data line breaks the second.

// sseConsumer accumulates frames and tracks whether the stream produced content.
type sseConsumer struct {
	// frames holds the forwarded chunk payloads, in order.
	frames [][]byte
	// buffer holds the bytes of an incomplete trailing line.
	buffer []byte
	// eventName is the most recent `event:` field. It is reset by every blank
	// line, which is the SSE event terminator; failing to reset it lets one
	// event's name leak onto the next event's data line.
	eventName string
	// sawContent reports whether any content block was seen.
	sawContent bool
	// eventCount counts the SSE events consumed, for diagnostics.
	eventCount int
}

// newSSEConsumer returns an empty consumer.
func newSSEConsumer() *sseConsumer { return &sseConsumer{} }

// consume feeds one buffered body and returns the frames produced.
//
// The collector reports an error only for a mid-stream `error` event, which must
// surface as a failure rather than as a silent stop.
func (c *sseConsumer) consume(body []byte) ([][]byte, error) {
	if len(body) == 0 {
		return nil, nil
	}
	start := len(c.frames)
	c.buffer = append(c.buffer, body...)
	for {
		index := indexByte(c.buffer, '\n')
		if index < 0 {
			break
		}
		line := string(c.buffer[:index])
		c.buffer = c.buffer[index+1:]
		if errLine := c.processLine(line); errLine != nil {
			return c.frames[start:], errLine
		}
	}
	return c.frames[start:], nil
}

// finish flushes the truncated tail and reports whether the stream carried any
// content, returning the complete SSE text.
//
// ⚠️ The tail replay is mandatory (rule 3 above): a stream cut short leaves the
// final `message_delta` in the buffer, and that frame is the only source of
// `stop_reason` and `usage`.
func (c *sseConsumer) finish() ([]byte, error) {
	if _, errFinish := c.finishFrames(); errFinish != nil {
		return nil, errFinish
	}
	return joinFrames(c.frames), nil
}

// finishFrames flushes the truncated tail, enforces the non-empty rule, and
// returns the frames as chunks.
func (c *sseConsumer) finishFrames() ([]pluginapi.ExecutorStreamChunk, error) {
	if len(c.buffer) > 0 {
		residual := string(c.buffer)
		c.buffer = nil
		for _, line := range strings.Split(residual, "\n") {
			if errLine := c.processLine(line); errLine != nil {
				return nil, errLine
			}
		}
	}
	if !c.sawContent {
		// ⚠️ Rule 2: an empty stream (or a stream of nothing but pings and a
		// message_start) is an error, not a successful empty answer.
		return nil, emptyStreamError()
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(c.frames))
	for _, frame := range c.frames {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: frame})
	}
	return chunks, nil
}

// processLine handles one SSE line.
func (c *sseConsumer) processLine(rawLine string) error {
	line := strings.TrimRight(rawLine, "\r")
	if line == "" {
		// The event terminator: the next `data:` starts a fresh event.
		c.eventName = ""
		return nil
	}
	if strings.HasPrefix(line, "event:") {
		c.eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return nil
	}
	if !strings.HasPrefix(line, "data:") {
		// Comments (`:`), `id:` and `retry:` carry nothing this layer needs.
		return nil
	}
	payloadText := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payloadText == "" {
		return nil
	}
	payload := []byte(payloadText)
	kind := c.eventName
	if bodyKind := frameType(payload); bodyKind != "" {
		// ⚠️ The frame's own `type` wins: an `event:` line can be missing or be
		// eaten by a proxy, and every real MiniMax frame carries `type`.
		kind = bodyKind
	}
	if kind == "" {
		return nil
	}
	c.eventCount++

	switch kind {
	case "error":
		message := streamErrorMessage(payload)
		if message == "" {
			message = "minimax: 上游返回了流内错误"
		}
		return transportError("stream_error", "minimax: 流内错误：%s", message)
	case "ping":
		// Keep-alive: forwarded so a Claude client sees it, but it is not
		// content and must not satisfy the non-empty rule.
		c.emitName(kind)
		c.emitData(payload)
		return nil
	case "content_block_delta":
		if deltaKind(payload) == "signature_delta" {
			// ⚠️ Rule 1: the signature is not text. Dropped entirely — not even
			// the event name is emitted, so no empty delta reaches the client.
			return nil
		}
	}

	if sawContentFrame(kind) {
		c.sawContent = true
	}
	c.emitName(kind)
	c.emitData(payload)
	return nil
}

// emitName appends the `event:` name line as its own chunk.
func (c *sseConsumer) emitName(kind string) {
	if kind == "" {
		return
	}
	c.frames = append(c.frames, []byte("event: "+kind+"\n"))
}

// emitData appends the `data:` line and the blank line that terminates the
// event.
func (c *sseConsumer) emitData(payload []byte) {
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	c.frames = append(c.frames, frame)
}

// frameType reads the `type` member of a frame payload.
func frameType(payload []byte) string {
	var probe struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(payload, &probe); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(probe.Type)
}

// joinFrames concatenates frames into the complete SSE text.
func joinFrames(frames [][]byte) []byte {
	size := 0
	for _, frame := range frames {
		size += len(frame)
	}
	out := make([]byte, 0, size)
	for _, frame := range frames {
		out = append(out, frame...)
	}
	return out
}

// indexByte is bytes.IndexByte for a single newline, kept local so this file
// carries no dependency the reader has to chase.
func indexByte(haystack []byte, needle byte) int {
	for index, value := range haystack {
		if value == needle {
			return index
		}
	}
	return -1
}

// nonNegativeInt reads a non-negative integer from a decoded JSON value,
// accepting a number or a numeric string. Absent or unusable values report 0.
func nonNegativeInt(value any) int {
	switch typed := value.(type) {
	case float64:
		if typed < 0 || typed != typed {
			return 0
		}
		return int(typed)
	case int:
		if typed < 0 {
			return 0
		}
		return typed
	case json.Number:
		parsed, errParse := strconv.ParseFloat(string(typed), 64)
		if errParse != nil || parsed < 0 || parsed != parsed {
			return 0
		}
		return int(parsed)
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if errParse != nil || parsed < 0 || parsed != parsed {
			return 0
		}
		return int(parsed)
	default:
		return 0
	}
}

// sseSanityCheck is a compile-time reminder that the consumer's frames are bare
// bytes on the wire: it fails loudly if a frame ever loses its `data:` prefix,
// which the host's Anthropic→OpenAI translator would silently discard.
func (c *sseConsumer) framesAreDataPrefixed() error {
	for _, frame := range c.frames {
		if len(frame) == 0 {
			continue
		}
		if strings.HasPrefix(string(frame), "event: ") {
			continue
		}
		if !strings.HasPrefix(string(frame), "data: ") {
			return abiboot.Errorf("bad_frame", "minimax: 生成了既非 event 也非 data 的帧：%q", truncate(string(frame), 80))
		}
	}
	return nil
}
