package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Chat completions: request body construction, SSE consumption and the
// finish-reason mapping.
//
// The upstream is standard OpenAI Chat Completions over plain SSE — no
// encryption, no envelope, no format translation (`raccoon-adapter.ts:5-8`). The
// work is in the details the reference calls out as defect classes:
//
//   - `tools` must reach the TOP LEVEL of the body, or the model invents XML
//     tool calls the harness cannot parse and the task dies (trap #19);
//   - orphan tool calls and orphan tool results must be dropped BEFORE sending,
//     because the backend answers 400 and the poisoned history is replayed on
//     every later request (trap #24);
//   - a name-less tool-call fragment must produce ZERO chunks: an empty-named
//     call that gets persisted is replayed forever and yields HTTP 400 code
//     11133 (trap #22);
//   - `length`, a missing `finish_reason`, a cut connection and truncated tool
//     arguments all map to `length` — never to `tool-calls`, or the harness
//     executes a call with missing parameters and the model retries forever
//     (trap #21).

// ── Request body ──

// normaliseChatPayload rewrites an outbound chat-completions body.
//
// It is applied by `request.translate` and again when the body is built, so the
// guarantee holds whichever path the host takes. Both applications are
// idempotent, which is why doing it twice is safe.
func normaliseChatPayload(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return body
	}
	messages, okMessages := root["messages"].([]any)
	if !okMessages {
		return body
	}
	root["messages"] = normaliseMessages(messages)
	// A non-positive or unparsable `max_tokens` is dropped rather than sent.
	if value, present := root["max_tokens"]; present {
		number, okNumber := numericAny(value)
		if !okNumber || number <= 0 {
			delete(root, "max_tokens")
		}
	}
	encoded, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return body
	}
	return encoded
}

// normaliseMessages drops orphan tool calls and orphan tool results, and applies
// the assistant wire shape (`serializeMessages`, `openai-compat.ts:158-262`).
func normaliseMessages(messages []any) []any {
	callIDs := map[string]struct{}{}
	resultIDs := map[string]struct{}{}
	for _, candidate := range messages {
		message, okMessage := candidate.(map[string]any)
		if !okMessage {
			continue
		}
		switch stringValue(message["role"]) {
		case "assistant":
			for _, call := range toolCallsOf(message) {
				if id := stringValue(call["id"]); id != "" {
					callIDs[id] = struct{}{}
				}
			}
		case "tool":
			if id := stringValue(message["tool_call_id"]); id != "" {
				resultIDs[id] = struct{}{}
			}
		}
	}

	out := make([]any, 0, len(messages))
	for _, candidate := range messages {
		message, okMessage := candidate.(map[string]any)
		if !okMessage {
			out = append(out, candidate)
			continue
		}
		switch stringValue(message["role"]) {
		case "assistant":
			calls := toolCallsOf(message)
			kept := make([]any, 0, len(calls))
			for _, call := range calls {
				id := stringValue(call["id"])
				if id == "" {
					// An identified call is required for a result to pair with;
					// an id-less call cannot be answered and is dropped.
					continue
				}
				if _, answered := resultIDs[id]; !answered {
					continue
				}
				kept = append(kept, call)
			}
			if len(kept) > 0 {
				message["tool_calls"] = kept
			} else {
				delete(message, "tool_calls")
			}
			emptyText := isBlankContent(message["content"])
			if emptyText && len(kept) > 0 {
				// `content` is NULL when text is empty and tool calls exist.
				message["content"] = nil
			}
			if emptyText && len(kept) == 0 && stringValue(message["reasoning_content"]) == "" {
				// Nothing left to say: an assistant turn with neither text nor
				// calls carries no information and the backend rejects it.
				continue
			}
			out = append(out, message)
		case "tool":
			id := stringValue(message["tool_call_id"])
			if id == "" {
				continue
			}
			if _, answered := callIDs[id]; !answered {
				// Orphan tool RESULT: its call is gone, so the pair is invalid.
				continue
			}
			out = append(out, message)
		default:
			out = append(out, message)
		}
	}
	return out
}

// toolCallsOf reads a message's `tool_calls` array of objects.
func toolCallsOf(message map[string]any) []map[string]any {
	items, okItems := message["tool_calls"].([]any)
	if !okItems {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, okObject := item.(map[string]any); okObject {
			out = append(out, object)
		}
	}
	return out
}

// isBlankContent reports whether a `content` value carries no text.
func isBlankContent(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case []any:
		// Multimodal parts: only an empty array counts as blank.
		return len(typed) == 0
	default:
		return false
	}
}

// stringValue reads a string field.
func stringValue(value any) string {
	if text, okText := value.(string); okText {
		return strings.TrimSpace(text)
	}
	return ""
}

// numericAny reads a numeric value that may be a float64 or a numeric string.
func numericAny(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		number, errParse := typed.Float64()
		return number, errParse == nil
	case string:
		number, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return number, errParse == nil
	default:
		return 0, false
	}
}

// ── SSE framing ──

// upstreamChunk is the subset of one `chat.completion.chunk` this adapter reads.
//
// Content and reasoning are POINTERS on purpose: the upstream sends them as
// explicit `null` as well as absent, and the `delta.reasoning_content ??
// delta.reasoning` fallback must be resolved with a string test
// (`openai-compat.ts:589-598,629-673`).
type upstreamChunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Created int64           `json:"created"`
	Usage   json.RawMessage `json:"usage"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string             `json:"role"`
			Content          *string            `json:"content"`
			ReasoningContent *string            `json:"reasoning_content"`
			Reasoning        *string            `json:"reasoning"`
			ToolCalls        []toolCallFragment `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// toolCallFragment is one streamed tool-call delta.
type toolCallFragment struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// sseFrames extracts the `data:` payloads of an upstream body.
//
// ⚠️ A body with NO `data:` frame at all is an ERROR, not an empty stream: that
// is what a gateway returning JSON looks like, and treating it as "no content"
// turns a broken credential into a silent empty reply
// (`openai-compat.ts:901-913`, trap #20).
func sseFrames(body []byte) ([]string, error) {
	if len(body) == 0 {
		return nil, abiboot.HTTPError("empty_response", http.StatusBadGateway, "raccoon: empty model response body")
	}
	scanner := &sse.Scanner{}
	frames := scanner.Feed(body)
	if trailing := trailingDataPayload(body); trailing != "" {
		frames = append(frames, trailing)
	}
	if len(frames) == 0 {
		return nil, abiboot.HTTPError("not_sse", http.StatusBadGateway,
			"raccoon: 响应不是 SSE（没有任何 data: 帧），原文片段：%s", truncate(string(body), 400))
	}
	return frames, nil
}

// trailingDataPayload returns the payload of a final `data:` line that the
// upstream did not terminate with a newline.
func trailingDataPayload(body []byte) string {
	text := string(body)
	if strings.HasSuffix(text, "\n") {
		return ""
	}
	if index := strings.LastIndex(text, "\n"); index >= 0 {
		text = text[index+1:]
	}
	text = strings.TrimRight(text, "\r")
	if !strings.HasPrefix(text, "data:") {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(text, "data:"), " ")
}

// errorFrameIn detects the three error shapes that arrive inside an HTTP 200 SSE
// response (`openai-compat.ts:552-583`, trap #20). Missing one makes a failure
// present as a clean stop with no error at all.
func errorFrameIn(payload string) error {
	var probe struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Choices         json.RawMessage `json:"choices"`
		Code            *flexCode       `json:"code"`
		Message         string          `json:"message"`
		Msg             string          `json:"msg"`
		StatusCodeValue *int            `json:"statusCodeValue"`
		StackTrace      string          `json:"stackTrace"`
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &probe); errUnmarshal != nil {
		return nil
	}
	if probe.Error != nil && strings.TrimSpace(probe.Error.Message) != "" {
		return abiboot.HTTPError("upstream_error", http.StatusBadGateway, "raccoon: %s", strings.TrimSpace(probe.Error.Message))
	}
	hasChoices := len(probe.Choices) > 0 && string(probe.Choices) != "null"
	if hasChoices {
		return nil
	}
	// Shape 2: a business envelope with no choices.
	if probe.Code != nil && probe.Code.present {
		message := firstNonEmpty(probe.Message, probe.Msg)
		if message != "" {
			return abiboot.HTTPError("upstream_error", http.StatusBadGateway, "raccoon: %s", message)
		}
	}
	// Shape 3: the gateway form — a message plus a status or a stack trace.
	message := firstNonEmpty(probe.Message, probe.Msg)
	if message == "" {
		return nil
	}
	status := 0
	if probe.StatusCodeValue != nil {
		status = *probe.StatusCodeValue
	}
	if status >= 400 || strings.TrimSpace(probe.StackTrace) != "" {
		if status == 0 {
			status = http.StatusBadGateway
		}
		return abiboot.HTTPError("upstream_error", status, "raccoon: %s", message)
	}
	return nil
}

// firstNonEmpty returns the first non-empty trimmed value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ── Stream state machine ──

// outChunk is one `chat.completion.chunk` frame emitted downstream.
type outChunk struct {
	ID      string          `json:"id,omitempty"`
	Object  string          `json:"object"`
	Created int64           `json:"created,omitempty"`
	Model   string          `json:"model,omitempty"`
	Choices []outChoice     `json:"choices"`
	Usage   json.RawMessage `json:"usage,omitempty"`
}

// outChoice is one emitted choice.
type outChoice struct {
	Index        int      `json:"index"`
	Delta        outDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

// outDelta is one emitted delta.
type outDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []outToolCall `json:"tool_calls,omitempty"`
}

// outToolCall is one emitted (or aggregated) tool call, in the exact OpenAI
// shape: the name and arguments live under `function`, never at the top level.
type outToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// toolState accumulates one tool call across streamed fragments.
type toolState struct {
	// outIndex is the index emitted downstream, assigned on first sight.
	outIndex int
	// id is the first non-empty id seen for this call.
	id string
	// name may only ever be set by a NON-EMPTY string: a later empty string
	// must not clear a parsed name (`openai-compat.ts:684-688`).
	name string
	// args accumulates argument fragments until the call is emitted.
	args string
	// emitted reports whether the call has been flushed once a name existed.
	emitted bool
}

// chatStream consumes one upstream SSE body and produces the downstream shape.
type chatStream struct {
	id      string
	model   string
	created int64
	usage   json.RawMessage

	finishReason    string
	hasFinishReason bool
	sawDone         bool

	content   strings.Builder
	reasoning strings.Builder
	// sawReasoningText suppresses leading whitespace-only reasoning, which would
	// otherwise create an empty reasoning block downstream
	// (`openai-compat.ts:656-671`).
	sawReasoningText bool

	toolOrder []int
	tools     map[int]*toolState

	frames []pluginapi.ExecutorStreamChunk

	// droppedNameless records that a tool-call block was discarded for having no
	// name, which is itself evidence of a truncated stream.
	droppedNameless bool
}

// runStream consumes an upstream body.
func runStream(body []byte) (*chatStream, error) {
	frames, errFrames := sseFrames(body)
	if errFrames != nil {
		return nil, errFrames
	}
	state := &chatStream{tools: map[int]*toolState{}}
	for _, payload := range frames {
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" {
			continue
		}
		if trimmed == sse.Done {
			state.sawDone = true
			continue
		}
		if errFrame := errorFrameIn(trimmed); errFrame != nil {
			return nil, errFrame
		}
		var chunk upstreamChunk
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &chunk); errUnmarshal != nil {
			// Non-JSON `data:` payloads are skipped silently
			// (`openai-compat.ts:547-551`).
			continue
		}
		state.absorb(chunk)
	}
	return state, nil
}

// absorb merges one upstream chunk and emits the corresponding output frames.
func (s *chatStream) absorb(chunk upstreamChunk) {
	if chunk.ID != "" {
		s.id = chunk.ID
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Created != 0 {
		s.created = chunk.Created
	}
	if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
		s.usage = chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			s.finishReason = strings.TrimSpace(*choice.FinishReason)
			s.hasFinishReason = true
		}
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			s.content.WriteString(*choice.Delta.Content)
			s.emit(outDelta{Content: *choice.Delta.Content})
		}
		if text := reasoningOf(choice.Delta.ReasoningContent, choice.Delta.Reasoning); text != "" {
			if strings.TrimSpace(text) != "" || s.sawReasoningText {
				if strings.TrimSpace(text) != "" {
					s.sawReasoningText = true
				}
				s.reasoning.WriteString(text)
				s.emit(outDelta{ReasoningContent: text})
			}
		}
		for _, fragment := range choice.Delta.ToolCalls {
			s.absorbToolCall(fragment)
		}
	}
}

// absorbToolCall merges one tool-call fragment.
//
// ⚠️ NO chunk is emitted until a usable name exists; once it does, the
// accumulated arguments are flushed in ONE tool-call delta
// (`openai-compat.ts:691-721`).
func (s *chatStream) absorbToolCall(fragment toolCallFragment) {
	wireIndex := -1
	if fragment.Index != nil {
		wireIndex = *fragment.Index
	} else {
		wireIndex = len(s.toolOrder)
	}
	tool, known := s.tools[wireIndex]
	if !known {
		tool = &toolState{outIndex: len(s.toolOrder)}
		s.tools[wireIndex] = tool
		s.toolOrder = append(s.toolOrder, wireIndex)
	}
	if id := strings.TrimSpace(fragment.ID); id != "" {
		tool.id = id
	}
	if strings.TrimSpace(fragment.Function.Name) != "" {
		tool.name = strings.TrimSpace(fragment.Function.Name)
	}
	arguments := fragment.Function.Arguments
	if tool.emitted {
		// The call is already on the wire: later fragments are deltas.
		if arguments != "" {
			tool.args += arguments
			s.emit(outDelta{ToolCalls: []outToolCall{toolCallFrame(tool, arguments)}})
		}
		return
	}
	tool.args += arguments
	if tool.name == "" {
		return
	}
	s.emit(outDelta{ToolCalls: []outToolCall{toolCallFrame(tool, tool.args)}})
	tool.emitted = true
}

// toolCallFrame renders one emitted tool-call frame.
func toolCallFrame(tool *toolState, arguments string) outToolCall {
	frame := outToolCall{Index: tool.outIndex, ID: tool.id, Type: "function"}
	frame.Function.Name = tool.name
	frame.Function.Arguments = arguments
	return frame
}

// emit appends one output frame carrying a single delta.
func (s *chatStream) emit(delta outDelta) {
	if delta.Role == "" && delta.Content == "" && delta.ReasoningContent == "" && len(delta.ToolCalls) == 0 {
		return
	}
	chunk := outChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []outChoice{{Index: 0, Delta: delta}},
	}
	encoded, errMarshal := json.Marshal(chunk)
	if errMarshal != nil {
		return
	}
	s.frames = append(s.frames, pluginapi.ExecutorStreamChunk{Payload: sse.Encode(string(encoded))})
}

// reasoningOf resolves `delta.reasoning_content ?? delta.reasoning`.
func reasoningOf(reasoningContent, reasoning *string) string {
	if reasoningContent != nil {
		return *reasoningContent
	}
	if reasoning != nil {
		return *reasoning
	}
	return ""
}

// usableToolCalls counts the calls that were actually emitted.
func (s *chatStream) usableToolCalls() int {
	count := 0
	for _, tool := range s.tools {
		if tool.emitted {
			count++
		}
	}
	return count
}

// truncatedArguments reports whether an emitted call carries arguments that do
// not parse as a JSON object.
//
// ⚠️ A truncated argument string must NOT be normalised to `{}`: that fakes
// validity and produces a schema error instead of a retry (trap #23).
func (s *chatStream) truncatedArguments() bool {
	for _, tool := range s.tools {
		if !tool.emitted {
			continue
		}
		if !isTruncatedArguments(tool.args) {
			continue
		}
		return true
	}
	return false
}

// isTruncatedArguments is true only when non-empty input fails to parse
// (`isTruncatedArguments`, `sse.ts:324-333`).
func isTruncatedArguments(arguments string) bool {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return false
	}
	var probe any
	return json.Unmarshal([]byte(trimmed), &probe) != nil
}

// resolveFinishReason implements the reference's priority table exactly
// (`openai-compat.ts:883-965`).
func (s *chatStream) resolveFinishReason() string {
	usable := s.usableToolCalls()
	switch {
	case s.hasFinishReason && s.finishReason == "length":
		return "length"
	case !s.hasFinishReason && usable > 0:
		// A usable tool call with NO finish_reason is a truncated stream.
		return "length"
	case !s.hasFinishReason && !s.sawDone:
		// No finish_reason and no [DONE]: the connection was cut.
		return "length"
	case s.truncatedArguments():
		return "length"
	case s.droppedNamelessToolCalls() && usable == 0:
		return "length"
	case (s.hasFinishReason && s.finishReason == "tool_calls") || usable > 0:
		return "tool_calls"
	default:
		return "stop"
	}
}

// droppedNamelessToolCalls reports whether any tool-call block never received a
// usable name.
func (s *chatStream) droppedNamelessToolCalls() bool {
	for _, tool := range s.tools {
		if !tool.emitted {
			return true
		}
	}
	return false
}

// finalized returns the stream's terminal verdict, applying the empty-response
// guard.
func (s *chatStream) finalized() (string, error) {
	reason := s.resolveFinishReason()
	if reason == "stop" && s.content.Len() == 0 && s.reasoning.Len() == 0 && s.usableToolCalls() == 0 {
		return "", abiboot.HTTPError("EMPTY_RESPONSE", http.StatusBadGateway,
			"raccoon: model returned a completed response with no content")
	}
	return reason, nil
}

// chunks returns the downstream frames, terminated by a rewritten finish reason
// and `data: [DONE]`.
func (s *chatStream) chunks() ([]pluginapi.ExecutorStreamChunk, error) {
	reason, errFinal := s.finalized()
	if errFinal != nil {
		return nil, errFinal
	}
	final := outChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []outChoice{{Index: 0, FinishReason: &reason}},
		Usage:   s.usage,
	}
	if encoded, errMarshal := json.Marshal(final); errMarshal == nil {
		s.frames = append(s.frames, pluginapi.ExecutorStreamChunk{Payload: sse.Encode(string(encoded))})
	}
	s.frames = append(s.frames, pluginapi.ExecutorStreamChunk{Payload: sse.DoneEvent()})
	return s.frames, nil
}

// completionMessage is the assistant message of the folded completion.
type completionMessage struct {
	Role             string        `json:"role"`
	Content          *string       `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []outToolCall `json:"tool_calls,omitempty"`
}

// foldedCompletion is the non-streaming answer built from the stream.
type foldedCompletion struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []foldedChoice  `json:"choices"`
	Usage   json.RawMessage `json:"usage,omitempty"`
}

// foldedChoice is one non-streaming choice.
type foldedChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

// completion folds the stream into one `chat.completion`.
//
// `content` is a POINTER so it can be `null` when the turn is pure tool calls,
// which is the shape the backend expects on replay
// (`openai-compat.ts:203-209`).
func (s *chatStream) completion(model string) ([]byte, error) {
	reason, errFinal := s.finalized()
	if errFinal != nil {
		return nil, errFinal
	}
	created := s.created
	if created == 0 {
		created = time.Now().Unix()
	}
	resolvedModel := s.model
	if resolvedModel == "" {
		resolvedModel = model
	}
	message := completionMessage{Role: "assistant"}
	text := s.content.String()
	if text != "" {
		message.Content = &text
	}
	message.ReasoningContent = s.reasoning.String()
	for _, wireIndex := range s.toolOrder {
		tool := s.tools[wireIndex]
		if !tool.emitted {
			continue
		}
		message.ToolCalls = append(message.ToolCalls, toolCallFrame(tool, tool.args))
	}
	if message.Content == nil && len(message.ToolCalls) == 0 {
		empty := ""
		message.Content = &empty
	}
	completion := foldedCompletion{
		ID:      firstNonEmpty(s.id, "chatcmpl-raccoon"),
		Object:  "chat.completion",
		Created: created,
		Model:   resolvedModel,
		Choices: []foldedChoice{{Index: 0, Message: message, FinishReason: reason}},
		Usage:   s.usage,
	}
	encoded, errMarshal := json.Marshal(completion)
	if errMarshal != nil {
		return nil, statusError(false, "encode_response", http.StatusInternalServerError, "encode completion: %v", errMarshal)
	}
	return encoded, nil
}
