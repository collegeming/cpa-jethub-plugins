package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/openai"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file is the port of the wire-facing half of the TypeScript adapter:
// `openai-compat.ts` (request body, SSE frame parsing, usage), `sse.ts` (the
// tool-call and finish-reason rules) and `cline-adapter.ts` (§5.1, §5.2, §5.4).
//
// The upstream contract is ordinary OpenAI chat-completions over SSE, so the
// request body is rebuilt from a whitelist instead of being forwarded: that is
// what guarantees the "no stream_options / user / n / top_p / response_format /
// tool_choice are ever sent" rule of §5.2, and it is where the two body quirks
// (the `max_tokens` clamp and the tool-schema enum sanitation) are applied.
//
// Deliberately NOT ported (spec §14): the DSH-specific post-processing that
// exists to satisfy a client contract rather than the upstream wire — course-leak
// scrubbing, the reasoning/prose loop guards, the blank-reasoning block
// suppressor and the block-start/block-end buffering. Dropping them changes
// robustness (a looping model burns tokens) but not wire compatibility.

// ---------------------------------------------------------------------------
// headers
// ---------------------------------------------------------------------------

// clientHeaders is `DEFAULT_CLINE_REQUEST_HEADERS` (`cline-product.ts:257-262`)
// as a header map. It is shared by every api.cline.bot call and by none of the
// WorkOS calls.
func clientHeaders() http.Header {
	header := http.Header{}
	for _, pair := range clientHeaderPairs {
		header.Set(pair[0], pair[1])
	}
	return header
}

// clineHeaders is `clineHeaders` (`cline.ts:276-288`): the authenticated header
// set for JSON calls.
func clineHeaders(credential *Credential) http.Header {
	header := clientHeaders()
	header.Set("Accept", "application/json")
	header.Set("Authorization", "Bearer "+credential.bearerValue())
	return header
}

// chatHeaders is `clineChatHeaders` (`cline-adapter.ts:546-550`).
//
// ⚠️ The merge order matters: `clineHeaders` contributes
// `Accept: application/json`, which is then OVERRIDDEN to `text/event-stream`.
// Only SSE is used — there is no non-streaming upstream path
// (`cline-adapter.ts:174`).
func chatHeaders(credential *Credential) http.Header {
	header := clineHeaders(credential)
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
	return header
}

// ---------------------------------------------------------------------------
// request body
// ---------------------------------------------------------------------------

// messageFields is the whitelist of message keys the outgoing body carries.
// Everything else the client sent is dropped, which is the same effect as the
// TypeScript rebuilding each message from scratch (`openai-compat.ts:194-258`).
var messageFields = []string{"role", "content", "name", "tool_call_id", "tool_calls", "reasoning_content"}

// buildChatBody rebuilds a chat-completions body the way
// `cline-adapter.ts:414-448` does.
//
// `stream` is always true, `tools` is only present when non-empty, and the
// optional fields (`temperature`, `max_tokens`, `stop`, `reasoning_effort`) are
// only emitted when the client supplied them — with two deliberate additions on
// top of the TypeScript, both opt-in through configuration: a default
// `max_tokens` and a default `reasoning_effort`.
func buildChatBody(payload []byte, model string, cfg Config) ([]byte, string, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, "", statusError(false, "invalid_request", http.StatusBadRequest, "Cline 收到空请求体")
	}
	var inbound map[string]any
	if errUnmarshal := json.Unmarshal(payload, &inbound); errUnmarshal != nil {
		return nil, "", statusError(false, "invalid_request", http.StatusBadRequest, "解码 chat-completions 请求失败：%v", errUnmarshal)
	}

	// The host's routed model wins; the body is the fallback. Both normally
	// agree, and using the routed value keeps an alias or a rotation decision
	// from being silently undone by the payload.
	modelID := strings.TrimSpace(model)
	if modelID == "" {
		modelID = readStringField(inbound, "model")
	}
	if modelID == "" {
		return nil, "", statusError(false, "invalid_request", http.StatusBadRequest, "Cline 请求缺少 model")
	}

	messages, okMessages := inbound["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		return nil, "", statusError(false, "invalid_request", http.StatusBadRequest, "Cline 请求缺少 messages")
	}

	out := map[string]any{
		"model":    modelID,
		"messages": normalizeMessages(messages),
		// Always stream: the upstream has no non-streaming path.
		"stream": true,
	}
	if tools, okTools := inbound["tools"].([]any); okTools && len(tools) > 0 {
		out["tools"] = sanitizeToolParameters(tools)
	}
	if temperature, present := inbound["temperature"]; present && temperature != nil {
		out["temperature"] = temperature
	}
	if maxTokens, okMax := clampMaxTokens(inbound["max_tokens"], cfg); okMax {
		out["max_tokens"] = maxTokens
	}
	if stop, present := inbound["stop"]; present && isNonEmptyList(stop) {
		out["stop"] = stop
	}
	// Passed through verbatim with NO whitelist: upstream silently ignores
	// unknown values (`reasoning_effort:"banana"` ⇒ HTTP 200, 0 reasoning
	// tokens), so validating here would invent a failure the server does not
	// have (`cline-adapter.ts:437-446`).
	effort := readStringField(inbound, "reasoning_effort")
	if effort == "" {
		effort = strings.TrimSpace(cfg.DefaultReasoningEffort)
	}
	if effort != "" {
		out["reasoning_effort"] = effort
	}

	encoded, errMarshal := json.Marshal(out)
	if errMarshal != nil {
		return nil, "", statusError(false, "encode_request", http.StatusInternalServerError, "encode Cline 请求失败：%v", errMarshal)
	}
	return encoded, modelID, nil
}

// clampMaxTokens is `clampClineMaxTokens` (`cline-adapter.ts:75-80`):
// `Math.floor(v)`, dropped when ≤ 0 or non-finite, clamped to the ceiling.
//
// The second result reports whether a value should be sent at all.
func clampMaxTokens(value any, cfg Config) (int, bool) {
	ceiling := cfg.MaxOutputTokens
	if ceiling <= 0 {
		ceiling = MaxOutputTokensCeiling
	}
	if value == nil {
		if cfg.DefaultMaxTokens > 0 {
			// A configured default is clamped like any other value.
			return clampMaxTokens(float64(cfg.DefaultMaxTokens), Config{MaxOutputTokens: ceiling})
		}
		return 0, false
	}
	number, okNumber := numericValue(value)
	if !okNumber {
		return 0, false
	}
	if number <= 0 || number != number /* NaN */ {
		return 0, false
	}
	rounded := int(number)
	if rounded > ceiling {
		return ceiling, true
	}
	return rounded, true
}

// numericValue accepts a JSON number or a numeric string.
func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, errParse := typed.Float64()
		return parsed, errParse == nil
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, errParse == nil
	default:
		return 0, false
	}
}

// isNonEmptyList reports whether `stop` carries at least one entry, which is the
// only condition under which it is sent (`cline-adapter.ts:436`).
func isNonEmptyList(value any) bool {
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return false
	}
}

// sanitizeToolParameters is `sanitizeClineToolParameters`
// (`cline-adapter.ts:110-123`).
//
// The measured upstream 400 it prevents:
//
//	GenerateContentRequest.tools[0].function_declarations[34]
//	  .parameters.properties[permission].enum[3]: cannot be empty
//
// Rules: recursively drop `enum` members that are empty or whitespace-only
// strings, KEEP non-string members (numeric/boolean enums are legal), drop the
// `enum` key entirely when nothing survives, and recurse through `properties`
// and `items` — which the generic map/slice walk below covers.
func sanitizeToolParameters(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if key == "enum" {
				if cleaned, keep := sanitizeEnum(item); keep {
					out[key] = cleaned
				}
				continue
			}
			out[key] = sanitizeToolParameters(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = sanitizeToolParameters(item)
		}
		return out
	default:
		return value
	}
}

// sanitizeEnum cleans one `enum` array. The second result is false when the key
// must be dropped.
func sanitizeEnum(value any) (any, bool) {
	items, okItems := value.([]any)
	if !okItems {
		// A non-array enum is malformed but not ours to repair.
		return value, true
	}
	cleaned := make([]any, 0, len(items))
	for _, item := range items {
		if text, isText := item.(string); isText && strings.TrimSpace(text) == "" {
			continue
		}
		cleaned = append(cleaned, item)
	}
	if len(cleaned) == 0 {
		return nil, false
	}
	return cleaned, true
}

// normalizeMessages rebuilds every message from the whitelist and normalises
// assistant tool-call arguments.
//
// Two rules of §5.3 are applied and one is deliberately not:
//
//   - `role: "developer"` messages are DROPPED (`message-shape.ts:111-145`).
//     The role is an alias for `system` in the newest OpenAI schema, and the
//     upstream catalogue spans providers that predate it;
//   - assistant tool-call arguments are normalised by normalizeMessage;
//   - the `role: "tool"` → `role: "user"` downgrade with `tool-result` blocks is
//     NOT applied. That rule exists for DSH's internal message shape; the payload
//     this executor receives is already OpenAI chat-completions, where
//     `role:"tool"` plus `tool_call_id` is the correct and accepted spelling.
func normalizeMessages(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		if role := readStringField(message, "role"); role == "developer" {
			continue
		}
		out = append(out, normalizeMessage(message))
	}
	return out
}

// normalizeMessage keeps only the known keys and applies the two assistant
// rules of §5.3: tool-call arguments are normalised, and `content` becomes null
// when the text is empty and tool calls exist (`openai-compat.ts:194-209`).
func normalizeMessage(message map[string]any) map[string]any {
	out := make(map[string]any, len(messageFields))
	for _, field := range messageFields {
		if value, present := message[field]; present {
			out[field] = value
		}
	}
	if calls, okCalls := out["tool_calls"].([]any); okCalls {
		normalized := make([]any, 0, len(calls))
		for _, item := range calls {
			call, okCall := item.(map[string]any)
			if !okCall {
				continue
			}
			function, okFunction := call["function"].(map[string]any)
			if okFunction {
				function["arguments"] = normalizeToolArguments(function["arguments"])
			}
			normalized = append(normalized, call)
		}
		out["tool_calls"] = normalized
	}
	if role := readStringField(out, "role"); role == "assistant" {
		if calls, okCalls := out["tool_calls"].([]any); okCalls && len(calls) > 0 {
			if text := rawString(out["content"]); strings.TrimSpace(text) == "" {
				out["content"] = nil
			}
		}
	}
	return out
}

// normalizeToolArguments is `normalizeToolArguments` (`sse.ts:291-304`): empty,
// invalid and non-object arguments all become "{}", because a no-argument tool
// legitimately streams an empty fragment.
func normalizeToolArguments(value any) string {
	text := rawString(value)
	if strings.TrimSpace(text) == "" {
		return "{}"
	}
	if !isJSONObject(text) {
		return "{}"
	}
	return text
}

// rawString renders a JSON value as the string it is, or "" for anything else
// (including null). Every check in the reference is `typeof x === 'string'`,
// never `!== undefined` (`openai-compat.ts:20-23`).
func rawString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.RawMessage:
		return jsonString(typed)
	default:
		return ""
	}
}

// isJSONObject reports whether the text parses as a JSON object.
func isJSONObject(text string) bool {
	var decoded map[string]any
	return json.Unmarshal([]byte(text), &decoded) == nil
}

// ---------------------------------------------------------------------------
// SSE frames
// ---------------------------------------------------------------------------

// upstreamFrame is the subset of one `data:` frame this adapter reads.
//
// Text fields that the reference guards with `typeof x === 'string'` are kept as
// json.RawMessage so a `null` in the unused channel — which the deltas
// legitimately carry — cannot fail the decode of the whole frame.
type upstreamFrame struct {
	ID              string          `json:"id"`
	Object          string          `json:"object"`
	Created         int64           `json:"created"`
	Model           string          `json:"model"`
	Usage           json.RawMessage `json:"usage"`
	Error           *frameErrorBody `json:"error"`
	Message         string          `json:"message"`
	Code            string          `json:"code"`
	Type            string          `json:"type"`
	StackTrace      json.RawMessage `json:"stackTrace"`
	StatusCodeValue int             `json:"statusCodeValue"`
	Choices         []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string            `json:"role"`
			Content          json.RawMessage   `json:"content"`
			Reasoning        json.RawMessage   `json:"reasoning"`
			ReasoningContent json.RawMessage   `json:"reasoning_content"`
			ToolCalls        []openai.ToolCall `json:"tool_calls"`
		} `json:"delta"`
		// message is the non-streaming shape some gateways echo inside a stream;
		// the reference uses it only while no delta content has been seen.
		Message *struct {
			Content          json.RawMessage `json:"content"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
			Reasoning        json.RawMessage `json:"reasoning"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// frameErrorBody is the `error` object of a stream frame.
type frameErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// jsonString reads a JSON string, tolerating null and every non-string value.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal != nil {
		return ""
	}
	return text
}

// outboundChunk is the canonical `chat.completion.chunk` this adapter emits.
type outboundChunk struct {
	ID      string           `json:"id,omitempty"`
	Object  string           `json:"object,omitempty"`
	Created int64            `json:"created,omitempty"`
	Model   string           `json:"model,omitempty"`
	Choices []outboundChoice `json:"choices"`
	// Usage is kept verbatim: rewriting it would change what the host accounts
	// for, and the derived counters are reported in the response metadata
	// instead.
	Usage json.RawMessage `json:"usage,omitempty"`
}

// outboundChoice is one streamed choice.
type outboundChoice struct {
	Index        int           `json:"index"`
	Delta        outboundDelta `json:"delta"`
	FinishReason *string       `json:"finish_reason"`
}

// outboundDelta is the streamed assistant delta. `reasoning` is normalised to
// `reasoning_content`: inbound streaming uses the former, and every other
// adapter in this repository (and the CPA clients behind it) consume the latter
// (`openai-compat.ts:637`).
type outboundDelta struct {
	Role             string            `json:"role,omitempty"`
	Content          string            `json:"content,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []openai.ToolCall `json:"tool_calls,omitempty"`
}

// streamFrames extracts the `data:` payloads of a buffered SSE body.
//
// `openai-compat.ts:474-484`: line-oriented, split on '\n', each line trimmed,
// ONLY lines starting with `data:` count, the payload is the remainder trimmed,
// and `[DONE]` terminates the stream.
//
// The shared `internal/jethub/sse.Scanner` is not used here on purpose: it is an
// incremental scanner that buffers an unterminated trailing line, and the host
// transport is synchronous, so the whole body is already in hand. Splitting the
// buffered body processes a final frame the server did not terminate with a
// newline, which is what the reference does.
func streamFrames(body []byte) (frames []string, sawDone bool) {
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(trimmed[len("data:"):])
		if payload == "[DONE]" {
			return frames, true
		}
		if payload == "" {
			continue
		}
		frames = append(frames, payload)
	}
	return frames, false
}

// frameError turns a frame that carries an upstream failure into a classified
// error, and returns nil for a normal chunk (`openai-compat.ts:552-583`).
func frameError(frame *upstreamFrame) error {
	if frame.Error != nil {
		message := strings.TrimSpace(frame.Error.Message)
		if message == "" {
			message = "unknown error"
		}
		return statusError(false, "server_error", http.StatusBadGateway, "Cline 上游返回错误：%s", message)
	}
	if len(frame.Choices) > 0 {
		return nil
	}
	message := strings.TrimSpace(frame.Message)
	if message == "" {
		return nil
	}
	if code := strings.TrimSpace(frame.Code); code != "" {
		detail := code
		if frame.Type != "" {
			detail = code + "/" + frame.Type
		}
		return statusError(false, "server_error", http.StatusBadGateway, "Cline 上游错误 %s：%s", detail, message)
	}
	if frame.StatusCodeValue >= 400 || len(bytes.TrimSpace(frame.StackTrace)) > 0 {
		return statusError(false, "server_error", http.StatusBadGateway, "Cline 网关错误：%s", message)
	}
	return nil
}

// frameChunk converts one decoded frame into a canonical chunk. The second
// result is false when the frame carries nothing this adapter emits.
//
// Only `choices[0]` is read (`openai-compat.ts:584`). `message.content` is used
// as a fallback only while no delta content has been seen
// (`openai-compat.ts:595-628`).
func frameChunk(frame *upstreamFrame, allowMessageFallback bool) (outboundChunk, bool) {
	if len(frame.Choices) == 0 {
		return outboundChunk{}, false
	}
	choice := frame.Choices[0]
	delta := outboundDelta{
		Role:      strings.TrimSpace(choice.Delta.Role),
		Content:   jsonString(choice.Delta.Content),
		ToolCalls: choice.Delta.ToolCalls,
	}
	// Inbound streaming thinking arrives in `delta.reasoning`, NOT
	// `reasoning_content` (`openai-compat.ts:637`).
	delta.ReasoningContent = jsonString(choice.Delta.ReasoningContent)
	if delta.ReasoningContent == "" {
		delta.ReasoningContent = jsonString(choice.Delta.Reasoning)
	}
	if delta.Content == "" && allowMessageFallback && choice.Message != nil {
		delta.Content = jsonString(choice.Message.Content)
		if delta.ReasoningContent == "" {
			delta.ReasoningContent = jsonString(choice.Message.ReasoningContent)
			if delta.ReasoningContent == "" {
				delta.ReasoningContent = jsonString(choice.Message.Reasoning)
			}
		}
	}
	return outboundChunk{
		ID:      frame.ID,
		Object:  frame.Object,
		Created: frame.Created,
		Model:   frame.Model,
		Choices: []outboundChoice{{Index: choice.Index, Delta: delta, FinishReason: choice.FinishReason}},
		Usage:   frame.Usage,
	}, true
}

// chatStream is the folded result of one upstream SSE body.
type chatStream struct {
	chunks       []outboundChunk
	sawDone      bool
	sawDelta     bool
	finishReason *string
	usageRaw     json.RawMessage
	usage        *usageCounters
}

// parseChatStream decodes a buffered upstream body into canonical chunks.
//
// A body with no `data:` frame at all is the "not SSE" case
// (`openai-compat.ts:904-913`); a single unparseable frame is skipped silently,
// exactly like a `JSON.parse` failure in the reference.
func parseChatStream(body []byte) (*chatStream, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, statusError(false, "empty_upstream", http.StatusBadGateway, "Cline 返回了空响应体")
	}
	frames, sawDone := streamFrames(body)
	if len(frames) == 0 {
		return nil, statusError(false, "not_sse", http.StatusBadGateway,
			"Cline 响应不是 SSE（没有任何 data: 帧），原文片段：%s", truncate(string(trimmed), 400))
	}
	stream := &chatStream{sawDone: sawDone}
	sawDeltaContent := false
	for _, payload := range frames {
		var frame upstreamFrame
		if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
			continue
		}
		if errFrame := frameError(&frame); errFrame != nil {
			return nil, errFrame
		}
		chunk, okChunk := frameChunk(&frame, !sawDeltaContent)
		if !okChunk {
			continue
		}
		stream.chunks = append(stream.chunks, chunk)
		if len(chunk.Usage) > 0 {
			stream.usageRaw = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				reason := *choice.FinishReason
				stream.finishReason = &reason
			}
			if choice.Delta.Content != "" || choice.Delta.ReasoningContent != "" || len(choice.Delta.ToolCalls) > 0 {
				stream.sawDelta = true
				if choice.Delta.Content != "" {
					sawDeltaContent = true
				}
			}
		}
	}
	stream.usage = deriveUsage(stream.usageRaw)
	return stream, nil
}

// usageCounters is the derived accounting of §5.5.
//
// `inputTokens = prompt_tokens - cached_tokens` (0 when there is no cache
// block), `outputTokens = completion_tokens ?? 0`,
// `cacheReadTokens = prompt_tokens_details.cached_tokens ??
// prompt_cache_hit_tokens`, and `reasoningTokens` only when it is > 0
// (`openai-compat.ts:730-748`).
type usageCounters struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

// deriveUsage reads the counters out of an upstream usage block.
func deriveUsage(raw json.RawMessage) *usageCounters {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var usage struct {
		PromptTokens         int64  `json:"prompt_tokens"`
		CompletionTokens     *int64 `json:"completion_tokens"`
		PromptCacheHitTokens *int64 `json:"prompt_cache_hit_tokens"`
		PromptTokensDetails  *struct {
			CachedTokens *int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens *int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if errUnmarshal := json.Unmarshal(raw, &usage); errUnmarshal != nil {
		return nil
	}
	cached := int64(0)
	hasCached := false
	if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens != nil {
		cached = *usage.PromptTokensDetails.CachedTokens
		hasCached = true
	}
	cacheRead := cached
	if !hasCached && usage.PromptCacheHitTokens != nil {
		cacheRead = *usage.PromptCacheHitTokens
	}
	input := usage.PromptTokens - cached
	if input < 0 {
		input = 0
	}
	output := int64(0)
	if usage.CompletionTokens != nil {
		output = *usage.CompletionTokens
	}
	reasoning := int64(0)
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens != nil && *usage.CompletionTokensDetails.ReasoningTokens > 0 {
		reasoning = *usage.CompletionTokensDetails.ReasoningTokens
	}
	return &usageCounters{
		InputTokens:     input,
		OutputTokens:    output,
		CacheReadTokens: cacheRead,
		ReasoningTokens: reasoning,
	}
}

// folded is the merged view of a stream: what the finish-reason rules and the
// non-streaming aggregation both operate on.
type folded struct {
	Content            string
	Reasoning          string
	ToolCalls          []openai.ToolCall
	Announced          int
	DroppedUnnamed     bool
	TruncatedArguments bool
	FinishReason       string
	Blocks             int
}

// fold merges the decoded deltas: tool calls by index (id from the first
// non-empty value, synthesised as `call_<index>` otherwise; name overwritten only
// by non-empty values; argument fragments concatenated; no chunk emitted until
// the name is usable), and derives the finish reason
// (`openai-compat.ts:674-729`, `:889-965`).
func (s *chatStream) fold() folded {
	result := folded{}
	var content, reasoning strings.Builder
	order := make([]int, 0, 4)
	calls := map[int]*openai.ToolCall{}
	named := map[int]bool{}
	for _, chunk := range s.chunks {
		for _, choice := range chunk.Choices {
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			for _, call := range choice.Delta.ToolCalls {
				index := 0
				if call.Index != nil {
					index = *call.Index
				}
				existing, okExisting := calls[index]
				if !okExisting {
					copied := call
					calls[index] = &copied
					order = append(order, index)
					if strings.TrimSpace(call.Function.Name) != "" {
						named[index] = true
					}
					continue
				}
				if call.ID != "" {
					existing.ID = call.ID
				}
				if call.Type != "" {
					existing.Type = call.Type
				}
				if strings.TrimSpace(call.Function.Name) != "" {
					existing.Function.Name = call.Function.Name
					named[index] = true
				}
				existing.Function.Arguments += call.Function.Arguments
			}
		}
	}
	result.Content = content.String()
	result.Reasoning = reasoning.String()

	sort.Ints(order)
	for _, index := range order {
		call := *calls[index]
		if !named[index] {
			// A call whose name never became usable emits no block at all
			// (`openai-compat.ts:686-721`).
			result.DroppedUnnamed = true
			continue
		}
		position := len(result.ToolCalls)
		call.Index = &position
		if call.ID == "" {
			call.ID = "call_" + itoaInt(index)
		}
		if call.Type == "" {
			call.Type = "function"
		}
		arguments := strings.TrimSpace(call.Function.Arguments)
		switch {
		case arguments == "":
			// A no-argument tool legitimately streams nothing.
			arguments = "{}"
		case !isJSONObject(arguments):
			// A non-empty unparseable fragment is truncation, never patched to
			// "{}" (`sse.ts:291-333`).
			result.TruncatedArguments = true
		}
		call.Function.Arguments = arguments
		result.ToolCalls = append(result.ToolCalls, call)
	}
	result.Announced = len(result.ToolCalls)

	// Block accounting for the empty-response rule. A whitespace-only reasoning
	// block does not count (`sse.ts:159-198`).
	if result.Content != "" {
		result.Blocks++
	}
	if strings.TrimSpace(result.Reasoning) != "" {
		result.Blocks++
	}
	result.Blocks += len(result.ToolCalls)

	result.FinishReason = resolveFinishReason(s.finishReason, s.sawDone, result)
	return result
}

// resolveFinishReason is the finish-reason mapping of `openai-compat.ts:889-965`,
// expressed in OpenAI's vocabulary: the reference's internal `max-tokens` is
// `length`, its `tool-calls` is `tool_calls`.
//
// The four `length` cases are, in the source's order: truncated tool arguments,
// an explicit `length`, no finish reason while tool calls had been announced
// (the connection was cut mid-tool-call), and no finish reason without `[DONE]`
// at all (the connection was cut).
func resolveFinishReason(finishReason *string, sawDone bool, merged folded) string {
	usable := merged.Announced > 0
	switch {
	case merged.TruncatedArguments:
		return "length"
	case finishReason != nil && *finishReason == "length":
		return "length"
	case finishReason == nil && usable:
		return "length"
	case finishReason == nil && !sawDone:
		return "length"
	case merged.DroppedUnnamed && !usable:
		return "length"
	case finishReason != nil && *finishReason == "tool_calls":
		return "tool_calls"
	case usable:
		return "tool_calls"
	default:
		return "stop"
	}
}

// emptyResponseError is `resolveEmptyResponseReason` (`sse.ts:132-141`): a
// response that completed with `stop` but produced no content, no reasoning and
// no tool call is a silent-empty turn, which the reference deliberately reports
// as an error instead of as a successful empty answer.
func emptyResponseError() error {
	return statusError(false, "empty_response", http.StatusBadGateway,
		"Cline 空回复（EMPTY_RESPONSE）：model returned a completed response with no content")
}

// streamChunks renders the decoded chunks as SSE frames, appending the terminal
// `[DONE]` when upstream did not send one.
func (s *chatStream) streamChunks() ([]pluginapi.ExecutorStreamChunk, error) {
	merged := s.fold()
	if merged.FinishReason == "stop" && merged.Blocks == 0 {
		return nil, emptyResponseError()
	}
	if !s.sawDelta && len(s.chunks) == 0 {
		return nil, statusError(false, "empty_upstream", http.StatusBadGateway, "Cline 流中没有可解析的分片")
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(s.chunks)+1)
	for _, chunk := range s.chunks {
		encoded, errMarshal := json.Marshal(chunk)
		if errMarshal != nil {
			continue
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: encodeSSE(string(encoded))})
	}
	// The terminal marker is ALWAYS emitted: streamFrames consumes the upstream
	// `[DONE]` as a terminator rather than returning it as a payload, so
	// forwarding the decoded chunks verbatim would leave the client without one.
	chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: encodeSSE("[DONE]")})
	return chunks, nil
}

// encodeSSE frames one payload as an SSE event.
func encodeSSE(payload string) []byte {
	var builder strings.Builder
	builder.Grow(len(payload) + 8)
	builder.WriteString("data: ")
	builder.WriteString(payload)
	builder.WriteString("\n\n")
	return []byte(builder.String())
}

// completionMessage is the aggregated assistant message.
//
// `Content` is a pointer, not the shared `openai.Message`'s `any` with
// `omitempty`: §5.3 requires an EXPLICIT `"content": null` when the text is empty
// and tool calls exist (`openai-compat.ts:194-209`), which an omitted field
// cannot express.
type completionMessage struct {
	Role             string            `json:"role"`
	Content          *string           `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []openai.ToolCall `json:"tool_calls,omitempty"`
}

// completionChoice is one non-streaming choice.
type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason *string           `json:"finish_reason"`
}

// completionPayload is the non-streaming answer.
type completionPayload struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   json.RawMessage    `json:"usage,omitempty"`
}

// completion folds the stream into one `chat.completion` for the non-streaming
// route.
func (s *chatStream) completion(model string) (completionPayload, folded, error) {
	merged := s.fold()
	if merged.FinishReason == "stop" && merged.Blocks == 0 {
		return completionPayload{}, merged, emptyResponseError()
	}
	completion := completionPayload{Object: "chat.completion", Model: model, Usage: s.usageRaw}
	for _, chunk := range s.chunks {
		if chunk.ID != "" {
			completion.ID = chunk.ID
		}
		if chunk.Created != 0 {
			completion.Created = chunk.Created
		}
		if chunk.Model != "" {
			completion.Model = chunk.Model
		}
	}
	if completion.ID == "" {
		completion.ID = "chatcmpl-" + completionID()
	}
	if completion.Model == "" {
		completion.Model = model
	}
	if completion.Created == 0 {
		completion.Created = time.Now().Unix()
	}
	message := completionMessage{
		Role:             "assistant",
		ToolCalls:        merged.ToolCalls,
		ReasoningContent: merged.Reasoning,
	}
	// `content` is null when the text is empty and tool calls exist, and the
	// plain text otherwise.
	if merged.Content != "" || len(merged.ToolCalls) == 0 {
		content := merged.Content
		message.Content = &content
	}
	finish := merged.FinishReason
	completion.Choices = []completionChoice{{Index: 0, Message: message, FinishReason: &finish}}
	return completion, merged, nil
}

// completionID synthesises a completion id for a stream that carried none.
func completionID() string {
	value, errRandom := randomHex(12)
	if errRandom != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return value
}

// ---------------------------------------------------------------------------
// upstream failure classification
// ---------------------------------------------------------------------------

// regionForbiddenMarkers is the text classification that separates a
// REGION-restricted 403 from an expired credential (`cline-adapter.ts:634-646`).
//
// The distinction is not cosmetic: every 403 used to be treated as an expired
// credential, which ended as AUTH and was rendered to the user as "API key
// invalid" (`cline-adapter.ts:609-633`). Refreshing cannot help with a region
// block, so the check runs BEFORE any refresh attempt and skips it entirely.
var regionForbiddenMarkers = []string{
	"not available in your region",
	"access forbidden",
	"region not supported",
	"not available in your country",
}

// isRegionForbidden reports whether a 403 is the region-restricted one.
func isRegionForbidden(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	lowered := strings.ToLower(string(body))
	for _, marker := range regionForbiddenMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// quotaMarkers is `shouldRotateAccount`'s text half
// (`cline-adapter.ts:578-594`). A client-side 4xx carrying one of these means
// "this account cannot serve the request", which CPA expresses as 402 so the
// host's scheduler rotates away from the credential.
var quotaMarkers = []string{
	"insufficient", "quota", "rate limit", "too many requests", "balance",
	"credit", "payment required", "exceeded",
	"积分不足", "额度不足", "余额不足", "频率限制", "超出限制",
}

// hasQuotaMarker reports whether the body carries a rotation marker.
func hasQuotaMarker(body []byte) bool {
	lowered := strings.ToLower(string(body))
	for _, marker := range quotaMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// upstreamErrorDetail extracts the human-readable message from an error body.
//
// The measured region-403 body is `{"error":"access forbidden: <model> is not
// available in your region","success":false}` (`cline-adapter.ts:617-622`), so
// both a string `error` and the nested object form are read.
func upstreamErrorDetail(body []byte) string {
	payload := decodeObject(body)
	if nested, okNested := payload["error"].(map[string]any); okNested {
		if message := readStringField(nested, "message", "detail"); message != "" {
			return message
		}
	}
	if message := readStringField(payload, "error", "message", "detail"); message != "" {
		return message
	}
	return truncate(string(body), 400)
}

// upstreamError classifies a non-2xx upstream answer.
//
// Text markers are only consulted below 500: a 5xx that happens to contain the
// word "exceeded" must stay a server error, or the host would rotate credentials
// for an upstream outage (spec §8 keeps 5xx out of the rotation set).
func upstreamError(response *pluginapi.HTTPResponse) error {
	status := response.StatusCode
	detail := upstreamErrorDetail(response.Body)
	switch {
	case isRegionForbidden(status, response.Body):
		return statusError(false, "region_forbidden", http.StatusForbidden,
			"Cline 地区限制（PERMISSION_DENIED，不触发续期）：%s", detail)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return credentialError("auth", "Cline 凭据无效（HTTP %d）：%s%s", status, detail, credentialAdvice(status))
	case status == http.StatusPaymentRequired:
		return statusError(false, "quota_exceeded", http.StatusPaymentRequired, "Cline 额度已耗尽：%s", detail)
	case status == http.StatusTooManyRequests:
		return statusError(true, "rate_limited", http.StatusTooManyRequests, "Cline 限流：%s", detail)
	case status >= 400 && status < 500 && hasQuotaMarker(response.Body):
		return statusError(false, "quota_exceeded", http.StatusPaymentRequired,
			"Cline 账号额度不足（HTTP %d）：%s", status, detail)
	case status == http.StatusBadRequest:
		return statusError(false, "invalid_request", http.StatusBadRequest, "Cline 拒绝请求（HTTP 400）：%s", detail)
	default:
		return transportError("upstream_error", "Cline 返回 HTTP %d：%s", status, detail)
	}
}
