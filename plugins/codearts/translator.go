package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/openai"
)

// ErrQueueFull reports the CodeArts concurrency ceiling; the caller should poll
// the queue-status endpoint and retry.
var ErrQueueFull = errors.New("codearts: concurrency limit reached")

// ErrCredentialRejected reports that the account credentials were refused.
var ErrCredentialRejected = errors.New("codearts: credential rejected")

// ErrQueueErrorCode is the model-serving queue error surfaced inside the SSE
// stream rather than as an HTTP status.
const ErrQueueErrorCode = "81111"

// isDeepseekV4 reports whether the model uses the DSML tool-call dialect.
func isDeepseekV4(model string) bool {
	return model == "deepseek-v4-flash" || model == "deepseek-v4-pro"
}

// newTraceID returns 16 random bytes hex-encoded, matching the dash-free UUIDs
// Jet-Hub uses for Chat-Id / Session-Id.
func newTraceID() string {
	value, err := randomHex(16)
	if err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return value
}

// prepareRequestBody enriches an inbound Chat Completions body with the fields
// the CodeArts chat endpoint expects. It always asks upstream for a stream;
// non-streaming callers aggregate the chunks afterwards.
func prepareRequestBody(payload []byte, model string, cfg Config, sessionID string) ([]byte, *openai.Request, error) {
	request := &openai.Request{}
	if len(payload) > 0 {
		if errUnmarshal := json.Unmarshal(payload, request); errUnmarshal != nil {
			return nil, nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "decode chat request: %v", errUnmarshal)
		}
	}
	if strings.TrimSpace(model) != "" {
		request.Model = model
	}
	request.Model = normalizeModelID(request.Model)
	if request.Model == "" {
		return nil, nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "chat request is missing a model")
	}
	if len(request.Messages) == 0 {
		return nil, nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "chat request has no messages")
	}

	request.Stream = true
	if request.MaxTokens == nil || *request.MaxTokens <= 0 {
		maxTokens := cfg.DefaultMaxTokens
		if maxTokens <= 0 {
			maxTokens = defaultMaxOutputTokens
		}
		request.MaxTokens = &maxTokens
	}
	if cfg.PromptCacheKey && request.PromptCacheKey == "" {
		request.PromptCacheKey = sessionID
	}
	if request.Include == nil {
		request.Include = []string{"reasoning.encrypted_content"}
	}
	if request.ReasoningSummary == nil {
		request.ReasoningSummary = "auto"
	}
	// tool_stream is part of the canonical CodeArts body: it lets the backend
	// stream oversized tool-call arguments instead of buffering them into one
	// silent burst.
	if cfg.ToolStream {
		toolStream := true
		request.ToolStream = &toolStream
	}

	// deepseek-v4-flash/pro cannot use the standard `tools` array. They emit
	// tool calls inline in the content channel as DSML blocks, so the schema is
	// injected as a system message and `tools` is withheld; the reply is parsed
	// back into structured tool calls by ParseDsmlToolCalls. Withholding tools
	// also keeps data flowing while a large argument is generated, which is what
	// avoids the APIG gateway's ~60s idle disconnect.
	if cfg.ToolStream && isDeepseekV4(request.Model) && len(request.Tools) > 0 {
		if prompt := BuildDsmlSystemPrompt(request.Tools); prompt != "" {
			request.Messages = insertDsmlSystemPrompt(request.Messages, prompt)
			request.Tools = nil
		}
	}
	encoded, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return nil, nil, abiboot.Errorf("encode_request", "encode CodeArts chat request: %v", errMarshal)
	}
	return encoded, request, nil
}

// insertDsmlSystemPrompt places the DSML tool instruction immediately after the
// first system message, becoming the first message when none exists. The
// position is load-bearing: appending the instruction to the end of `messages`
// makes deepseek-v4 write its thinking into the visible content channel instead
// of reasoning_content.
func insertDsmlSystemPrompt(messages []openai.Message, prompt string) []openai.Message {
	index := 0
	for position := range messages {
		if messages[position].Role == "system" {
			index = position + 1
			break
		}
	}
	out := make([]openai.Message, 0, len(messages)+1)
	out = append(out, messages[:index]...)
	out = append(out, openai.Message{Role: "system", Content: prompt})
	out = append(out, messages[index:]...)
	return out
}

// dsmlThoughtOpenTag is the half-width thought delimiter that deepseek-v4 can
// inline into the content channel alongside DSML blocks. Upstream expects that
// text on the reasoning channel, not in the visible answer.
const dsmlThoughtOpenTag = "<thought>"

// containsDsmlMarker reports whether the text holds any construct the DSML
// parser must rewrite. Plain text is left completely untouched.
func containsDsmlMarker(text string) bool {
	return strings.Contains(text, DsmlToolCallsOpen) || strings.Contains(text, dsmlThoughtOpenTag)
}

// extractDsmlToolCalls moves any DSML tool calls out of an assistant message
// into the structured call map, replaces the content with the visible text, and
// routes inlined `<thought>` content to the reasoning channel.
func extractDsmlToolCalls(message *openai.Message, toolCalls map[int]*openai.ToolCall, finishReason **string) {
	text, isText := message.Content.(string)
	if !isText || !containsDsmlMarker(text) {
		return
	}
	// Drive the streaming state machine so both paths agree byte for byte on a
	// given input, then drain the thought channel rather than discarding it.
	parser := NewDsmlStreamParser()
	visible, calls := parser.Feed(text)
	tail, tailCalls := parser.Flush()
	visible += tail
	calls = append(calls, tailCalls...)
	if thought := parser.DrainReasoning(); thought != "" {
		message.ReasoningContent += thought
	}
	message.Content = visible
	for _, call := range calls {
		position := len(toolCalls)
		copied := call
		copied.Index = &position
		if copied.Type == "" {
			copied.Type = "function"
		}
		toolCalls[position] = &copied
	}
	if len(calls) > 0 && (finishReason == nil || *finishReason == nil || **finishReason == "stop") {
		reason := "tool_calls"
		*finishReason = &reason
	}
}

// isQueueErrorText reports whether an upstream error body signals the
// concurrency ceiling.
func isQueueErrorText(text string) bool {
	if strings.Contains(text, "TM.00001041") {
		return true
	}
	if strings.Contains(text, ErrQueueErrorCode) && (strings.Contains(text, "429") || strings.Contains(text, "queue")) {
		return true
	}
	return strings.Contains(text, "并发会话数已达上限")
}

// isCredentialErrorText reports whether an upstream error body signals that the
// credential must be refreshed or re-created.
func isCredentialErrorText(text string) bool {
	return strings.Contains(text, "APIG.0602") ||
		strings.Contains(text, "APIG.0301") ||
		strings.Contains(text, "InvalidAccessKeyId") ||
		strings.Contains(text, "SignatureDoesNotMatch") ||
		strings.Contains(text, "security token")
}

// classifyUpstreamFailure maps an upstream HTTP failure onto a sentinel or a
// status-carrying plugin error.
func classifyUpstreamFailure(status int, body []byte) error {
	text := string(body)
	switch {
	case isQueueErrorText(text):
		return ErrQueueFull
	case status == http.StatusUnauthorized || status == http.StatusForbidden || isCredentialErrorText(text):
		return ErrCredentialRejected
	default:
		return abiboot.HTTPError("upstream_error", status, "CodeArts returned HTTP %d: %s", status, truncate(text, 400))
	}
}

// streamErrorProbe recognises error payloads delivered inside the SSE stream.
type streamErrorProbe struct {
	Error     *streamErrorBody `json:"error"`
	ErrorCode string           `json:"error_code"`
	ErrorMsg  string           `json:"error_msg"`
	Code      string           `json:"code"`
	Message   string           `json:"message"`
}

type streamErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// inspectStreamPayload returns a non-nil error when an SSE payload carries an
// upstream failure instead of a completion chunk.
func inspectStreamPayload(payload string) error {
	if payload == "" {
		return nil
	}
	probe := streamErrorProbe{}
	if errUnmarshal := json.Unmarshal([]byte(payload), &probe); errUnmarshal != nil {
		return nil
	}
	code := probe.ErrorCode
	message := probe.ErrorMsg
	if probe.Error != nil && probe.Error.Message != "" {
		code = probe.Error.Code
		message = probe.Error.Message
	}
	if code == "" && message == "" {
		// A bare {code,message} envelope is only an error when it has no choices.
		if strings.Contains(payload, "\"choices\"") {
			return nil
		}
		code = probe.Code
		message = probe.Message
	}
	if code == "" && message == "" {
		return nil
	}
	combined := code + " " + message
	switch {
	case isQueueErrorText(combined):
		return ErrQueueFull
	case isCredentialErrorText(combined):
		return ErrCredentialRejected
	default:
		return abiboot.Errorf("upstream_stream_error", "CodeArts stream error %s: %s", code, message)
	}
}

// aggregateCompletion folds a stream of chunks into one non-streaming
// chat.completion response.
func aggregateCompletion(chunks []openai.Chunk, model string) openai.Completion {
	completion := openai.Completion{
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
	}
	message := openai.Message{Role: "assistant"}
	var content strings.Builder
	var reasoning strings.Builder
	toolCalls := map[int]*openai.ToolCall{}
	var finishReason *string

	for _, chunk := range chunks {
		if chunk.ID != "" {
			completion.ID = chunk.ID
		}
		if chunk.Model != "" {
			completion.Model = chunk.Model
		}
		if chunk.Usage != nil {
			completion.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role != "" {
				message.Role = choice.Delta.Role
			}
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			for _, call := range choice.Delta.ToolCalls {
				index := 0
				if call.Index != nil {
					index = *call.Index
				}
				existing, ok := toolCalls[index]
				if !ok {
					copied := call
					toolCalls[index] = &copied
					continue
				}
				if call.ID != "" {
					existing.ID = call.ID
				}
				if call.Type != "" {
					existing.Type = call.Type
				}
				if call.Function.Name != "" {
					existing.Function.Name = call.Function.Name
				}
				existing.Function.Arguments += call.Function.Arguments
			}
			if choice.FinishReason != nil {
				finishReason = choice.FinishReason
			}
		}
	}

	if completion.ID == "" {
		completion.ID = "chatcmpl-" + newTraceID()
	}
	if completion.Model == "" {
		completion.Model = model
	}
	message.Content = content.String()
	extractDsmlToolCalls(&message, toolCalls, &finishReason)
	if reasoning.Len() > 0 {
		message.ReasoningContent = reasoning.String()
	}
	if len(toolCalls) > 0 {
		indexes := make([]int, 0, len(toolCalls))
		for index := range toolCalls {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for _, index := range indexes {
			call := *toolCalls[index]
			position := index
			call.Index = &position
			if call.Type == "" {
				call.Type = "function"
			}
			message.ToolCalls = append(message.ToolCalls, call)
		}
	}
	completion.Choices = []openai.Choice{{Index: 0, Message: message, FinishReason: finishReason}}
	return completion
}
