package main

// 本文件承载 request.translate / response.translate（恒等变换：执行器两端都声明
// chat-completions，宿主自行完成跨协议转换）以及 SSE 分片的解析/聚合工具。
//
// 关于 response.translate：Jet-Hub 的 buddy 适配器**刻意不复用**
// `openai-compat.ts`（AGENTS.md 明确记录），它消费的就是标准 OpenAI SSE
// （buddy-adapter.ts:1133, :1156）。CPA 侧同理：执行器输出 chat-completions，
// 因此分片按 OpenAI 线格式原样转发，不重写。

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleRequestTranslate is an identity transform.
func handleRequestTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.RequestTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	return pluginapi.PayloadResponse{Body: request.Body}, nil
}

// handleResponseTranslate is an identity transform.
func handleResponseTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ResponseTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	return pluginapi.PayloadResponse{Body: request.Body}, nil
}

// ── SSE 分片模型 ──

// streamFunction is the `function` object of a streamed tool call.
type streamFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// streamToolCall is one entry of `delta.tool_calls`.
type streamToolCall struct {
	Index    *int           `json:"index,omitempty"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function streamFunction `json:"function"`
}

// streamDelta is the incremental assistant message.
type streamDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          any              `json:"content,omitempty"`
	ReasoningContent any              `json:"reasoning_content,omitempty"`
	ToolCalls        []streamToolCall `json:"tool_calls,omitempty"`
}

// streamChoice is one streaming choice.
type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// streamUsage is the token accounting block, including the cache and credit
// details CodeBuddy reports (buddy-adapter.ts:1222-1235).
type streamUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	Credit                any `json:"credit"`
	PromptTokensDetails   *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// chatChunk is one decoded `chat.completion.chunk` payload.
type chatChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []streamChoice  `json:"choices"`
	Usage   *streamUsage    `json:"usage"`
	Error   json.RawMessage `json:"error"`
	// CodeBuddy sometimes reports failures with `code`/`msg`/`displayMsg`
	// instead of an `error` object (buddy-adapter.ts:395-401).
	Code       any    `json:"code"`
	Msg        string `json:"msg"`
	Message    string `json:"message"`
	DisplayMsg any    `json:"displayMsg"`
}

// stringValue renders a JSON value that should be a string (null → "").
func stringValue(value any) string {
	switch typed := value.(type) {
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

// contextWindowMarkers are the substrings that identify a context-overflow
// error. They come from the payload recorded at buddy-adapter.ts:395-401
// (`extError.code = context_length_exceeded`, `prompt is too long: N tokens >
// M maximum`) plus the generic OpenAI wording.
var contextWindowMarkers = []string{
	"context_length_exceeded",
	"context length",
	"context window",
	"prompt is too long",
	"maximum context",
	"exceeds the model context limit",
	"too many tokens",
	"reduce the length",
}

// isContextWindowExceeded reports whether an error body describes a context
// overflow. The full raw body is inspected (not a normalised excerpt) because
// buddy-adapter.ts:404-410 found that excerpting loses the fields that identify
// the overflow.
func isContextWindowExceeded(body string) bool {
	lowered := strings.ToLower(body)
	for _, marker := range contextWindowMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// httpErrorCode maps an HTTP status to a CPA/harness error code.
// buddy-adapter.ts:412-422.
func httpErrorCode(status int, body string) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "AUTH"
	case status == http.StatusTooManyRequests:
		return "RATE_LIMIT"
	case status == http.StatusBadRequest:
		if isContextWindowExceeded(body) {
			return "CONTEXT_WINDOW_EXCEEDED"
		}
		return "INVALID_REQUEST"
	case status >= 500:
		return "SERVER"
	default:
		return "HTTP_" + itoa(status)
	}
}

// errorDetail extracts a readable reason from an upstream error body.
// buddy-adapter.ts:367-384.
func errorDetail(body string) string {
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal([]byte(body), &decoded); errUnmarshal == nil {
		parts := []string{}
		if nested, ok := decoded["error"].(map[string]any); ok {
			for _, key := range []string{"code", "type", "message"} {
				if text, okText := nested[key].(string); okText {
					parts = append(parts, text)
				}
			}
		}
		if text, okText := decoded["message"].(string); okText {
			parts = append(parts, text)
		}
		if text, okText := decoded["msg"].(string); okText {
			parts = append(parts, text)
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	if len(body) == 0 {
		return "empty error body"
	}
	return truncate(body, 500)
}

// chunkErrorPayload returns a non-empty error description when the frame
// carries an upstream failure, and whether the failure is a context overflow.
// buddy-adapter.ts:1242-1255.
func chunkErrorPayload(chunk *chatChunk) (string, bool) {
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		var nested map[string]any
		_ = json.Unmarshal(chunk.Error, &nested)
		detail := string(chunk.Error)
		if message, ok := nested["message"].(string); ok && message != "" {
			detail = message
		} else if msg, ok := nested["msg"].(string); ok && msg != "" {
			detail = msg
		}
		return detail, isContextWindowExceeded(string(chunk.Error))
	}
	// HTTP 200 with a business error code instead of choices.
	if len(chunk.Choices) == 0 && chunk.Code != nil {
		detail := chunk.Msg
		if detail == "" {
			detail = chunk.Message
		}
		if detail == "" {
			detail = stringValue(chunk.DisplayMsg)
		}
		if detail == "" {
			detail = marshalCompact(chunk.Code)
		}
		raw := marshalCompact(chunk)
		return detail, isContextWindowExceeded(raw)
	}
	return "", false
}

// ── 非流式聚合 ──

// toolCallAccumulator reconstructs one streamed tool call.
type toolCallAccumulator struct {
	Index     int
	ID        string
	Name      string
	Arguments strings.Builder
}

// aggregateChunks folds a streamed completion into a single chat.completion.
// It mirrors what a browser client would do: text and reasoning concatenate,
// tool calls merge by index (id/name only from the first fragment, as
// buddy-adapter.ts:1295-1300 requires), and the last usage wins.
func aggregateChunks(chunks []chatChunk, model string) map[string]any {
	var text strings.Builder
	var reasoning strings.Builder
	var id string
	var created int64
	var finishReason any
	var usage *streamUsage
	accumulators := map[int]*toolCallAccumulator{}
	order := []int{}

	for position := range chunks {
		chunk := &chunks[position]
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Created != 0 {
			created = chunk.Created
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			text.WriteString(stringValue(choice.Delta.Content))
			reasoning.WriteString(stringValue(choice.Delta.ReasoningContent))
			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
			for _, call := range choice.Delta.ToolCalls {
				index := 0
				if call.Index != nil {
					index = *call.Index
				}
				accumulator, seen := accumulators[index]
				if !seen {
					accumulator = &toolCallAccumulator{Index: index}
					accumulators[index] = accumulator
					order = append(order, index)
				}
				if call.ID != "" {
					accumulator.ID = call.ID
				}
				// 后续参数分片会带空 name，不能覆盖已解析出的真实工具名。
				if call.Function.Name != "" {
					accumulator.Name = call.Function.Name
				}
				accumulator.Arguments.WriteString(call.Function.Arguments)
			}
		}
	}

	toolCalls := make([]map[string]any, 0, len(order))
	for _, index := range order {
		accumulator := accumulators[index]
		arguments := accumulator.Arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   accumulator.ID,
			"type": "function",
			"function": map[string]any{
				"name":      accumulator.Name,
				"arguments": arguments,
			},
		})
	}

	message := map[string]any{
		"role":              "assistant",
		"content":           text.String(),
		"reasoning_content": reasoning.String(),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text.Len() == 0 {
			// 正文为空且带 tool_calls 时 content 必须为 null。
			message["content"] = nil
		}
	}
	if finishReason == nil {
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}

	if id == "" {
		id = "chatcmpl-codebuddy"
	}
	completion := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		completion["usage"] = map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
	}
	return completion
}

// parseChunk decodes one SSE payload; ok is false when the payload is not JSON.
func parseChunk(payload string) (chatChunk, bool) {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" || trimmed == "[DONE]" {
		return chatChunk{}, false
	}
	var chunk chatChunk
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &chunk); errUnmarshal != nil {
		return chatChunk{}, false
	}
	return chunk, true
}
