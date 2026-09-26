package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Chat completions, ported from `src/loomy.ts:213-231` and
// `src/loomy-adapter.ts:266-395`.
//
// The endpoint is plain OpenAI Chat Completions: `data:` SSE frames, no
// encryption, no envelope, no format conversion (`loomy-adapter.ts:6-11`,
// `loomy.ts:23-26`). What is easy to get wrong is the wiring — which header
// family the call needs, and that the upstream is always asked to stream — so
// this file owns both.

// hostRequest performs one buffered upstream call through the host transport.
//
// Every outbound call goes through the host so proxy, TLS and request logging
// stay under the host's control; the plugin never opens a socket itself.
func hostRequest(h *abiboot.Host, method, rawURL string, headers http.Header, body []byte, _ Config) (*pluginapi.HTTPResponse, error) {
	if h == nil {
		return nil, transportError("host_unavailable", "插件未通过宿主调用（缺少 host 句柄）")
	}
	return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: rawURL, Headers: headers, Body: body})
}

// chatHeaders is `loomyChatHeaders` (`loomy.ts:225-231`).
//
// ⚠️ BOTH header families are sent on this endpoint — `Authorization: Bearer
// <session>` AND the lowercase `token` header — because that is what the official
// client does. Dropping either one, or dropping the `Bearer ` prefix, yields HTTP
// 200 with `{"code":"100002","desc":"缺少 token"}` (`loomy.ts:223`, trap #2).
func chatHeaders(credential *Credential) http.Header {
	session := credential.Session()
	headers := http.Header{}
	headers.Set("Accept", "text/event-stream")
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+session)
	// Assigned through the map so the wire name stays the literal lowercase
	// `token`; Header.Set would canonicalise it to `Token`. The header name in
	// the source is lowercase (`loomy.ts:214,230`), and the host writes map keys
	// verbatim.
	headers["token"] = []string{session}
	return headers
}

// businessHeaders is the header set for `/models`, `/points/*` and
// `/onboarding/*` (`loomy.ts:213-215`, `loomy-credits.ts:76-81`,
// `loomy-onboarding.ts:119-124`).
//
// ⚠️ The lowercase `token` header ONLY. Sending `Authorization: Bearer` here is
// measured to answer HTTP 200 with `100002 缺少 token` (`README.md:1404-1413`),
// which is why success must be judged from the envelope code and never from the
// status. `Content-Type` is added only when the call carries a body.
func businessHeaders(credential *Credential, withBody bool) http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers["token"] = []string{credential.Session()}
	if withBody {
		headers.Set("Content-Type", "application/json")
	}
	return headers
}

// executorStreamResponse is the wire shape of executor.execute_stream.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleRequestTranslate is an identity transform. The executor declares
// chat-completions on both sides, so the host performs any cross-protocol
// translation itself and this route never has a gap to fill. The message-shape
// normalisation the reference performs (orphan tool calls dropped, image parts
// rewritten to `image_url`, assistant reasoning re-sent as `reasoning_content`)
// belongs to that host-side translation, not here (`openai-compat.ts:158-262`).
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

// handleExecutorCountTokens returns a coarse estimate. CPA falls back to its own
// tokenizer when an executor does not implement counting, so an approximation is
// better than an error.
func handleExecutorCountTokens(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	estimated := len(request.Payload)/4 + 1
	return pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":` + itoaInt(estimated) + `}`)}, nil
}

// handleExecutorExecute serves a non-streaming completion.
//
// The upstream is ALWAYS asked to stream (`stream: true`, `loomy-adapter.ts:307-326`),
// so the stream is folded into one `chat.completion` here.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, cfg, errPrepare := decodeExecutorCall(raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errInfer := performInfer(h, request, credential, cfg)
	if errInfer != nil {
		return nil, errInfer
	}
	payloads, errFrames := inferFrames(response.Body)
	if errFrames != nil {
		return nil, errFrames
	}
	completion, truncated := aggregateFrames(payloads, request.Model)
	payload, errMarshal := json.Marshal(completion)
	if errMarshal != nil {
		return nil, statusError(false, "encode_response", http.StatusInternalServerError, "encode completion: %v", errMarshal)
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: canonicalHeader("Content-Type", "application/json"),
		Metadata: map[string]any{
			"provider":  ProviderKey,
			"model":     request.Model,
			"truncated": truncated,
		},
	}, nil
}

// handleExecutorExecuteStream serves a streaming completion.
//
// Frames are returned in the response envelope rather than pushed through
// `host.stream.emit`; both are valid ABI paths and this is the one the reference
// plugins in this repository use.
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, cfg, errPrepare := decodeExecutorCall(raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errInfer := performInfer(h, request, credential, cfg)
	if errInfer != nil {
		return nil, errInfer
	}
	frames, truncated, errFrames := streamFrames(response.Body)
	if errFrames != nil {
		return nil, errFrames
	}
	if truncated && h != nil {
		h.Log("warn", "Loomy 流未正常结束（缺少 finish_reason 与 [DONE]）", map[string]any{
			"provider": ProviderKey,
			"model":    request.Model,
		})
	}
	return executorStreamResponse{
		Headers: canonicalHeader("Content-Type", "text/event-stream"),
		Chunks:  frames,
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

// performInfer runs one upstream chat completion.
//
// There is NO refresh-and-retry: Loomy has no refresh endpoint, so a 401/403 can
// only mean "log in again" (`loomy.ts:195-205`). The reference does call
// `refresh()` once here and that call is what throws
// `RefreshTokenExpiredError` (`loomy-adapter.ts:354-361`), i.e. the retry never
// succeeds.
func performInfer(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config) (*pluginapi.HTTPResponse, error) {
	body, errBody := chatRequestBody(request)
	if errBody != nil {
		return nil, errBody
	}
	response, errDo := hostRequest(h, http.MethodPost, APIBase+ChatCompletionsPath, chatHeaders(credential), body, cfg)
	if errDo != nil {
		return nil, transportError("infer_transport", "连接 Loomy 推理服务失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, inferFailure(response)
	}
	return response, nil
}

// chatRequestBody rewrites the outbound body.
//
// Only two things are forced or defaulted: the model (when the translated payload
// does not carry one) and `stream: true`. `max_tokens`, `temperature`, `stop` and
// `tools` are whatever the host sent — the source only includes them when the
// caller provided them (`loomy-adapter.ts:307-326`).
func chatRequestBody(request pluginapi.ExecutorRequest) ([]byte, error) {
	if len(request.Payload) == 0 {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "Loomy 收到空请求体")
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(request.Payload, &body); errUnmarshal != nil {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "解码 chat-completions 请求失败：%v", errUnmarshal)
	}
	if _, present := body["model"]; !present && strings.TrimSpace(request.Model) != "" {
		body["model"] = request.Model
	}
	body["stream"] = true
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, statusError(false, "encode_request", http.StatusInternalServerError, "encode request: %v", errMarshal)
	}
	return encoded, nil
}

// inferFailure classifies a non-2xx answer.
//
// The upstream status is normalised rather than passed through: the host uses it
// to decide whether the request was at fault, and an upstream 403 says nothing
// useful about that.
func inferFailure(response *pluginapi.HTTPResponse) error {
	detail := envelopeErrorText(response.Body)
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return credentialError("auth", "Loomy 凭据无效（HTTP %d）：%s%s", response.StatusCode, detail, credentialAdvice())
	case http.StatusPaymentRequired:
		return statusError(false, "quota_exhausted", http.StatusPaymentRequired, "Loomy 积分不足：%s", detail)
	case http.StatusTooManyRequests:
		return statusError(true, "rate_limited", http.StatusTooManyRequests, "Loomy 限流：%s", detail)
	case http.StatusGatewayTimeout:
		return statusError(true, "upstream_timeout", http.StatusGatewayTimeout, "Loomy 上游超时：%s", detail)
	default:
		return transportError("upstream_error", "Loomy 返回 HTTP %d：%s", response.StatusCode, detail)
	}
}

// inferFrames extracts the SSE payloads of a successful response.
//
// A body with NO `data:` frame at all is an ERROR, not an empty stream: that is
// what a gateway returning JSON (for example `{"code":"100002"}` because the
// wrong auth header was sent) looks like, and treating it as "no content" is how
// a broken credential turns into a silent empty reply
// (`openai-compat.ts:901-913`, §6.2).
func inferFrames(body []byte) ([]string, error) {
	scanner := &sse.Scanner{}
	frames := scanner.Feed(body)
	if len(frames) == 0 {
		// Only a real Loomy envelope (a non-empty `code`) is reported as a
		// business failure; any other JSON is a body that is simply not SSE, and
		// the raw snippet explains that best.
		if parsed, errEnvelope := parseEnvelope(body); errEnvelope == nil && parsed.code() != "" && !parsed.success() {
			return nil, envelopeFailure("Loomy 推理请求", parsed)
		}
		return nil, abiboot.HTTPError("not_sse", http.StatusBadGateway,
			"Loomy 响应不是 SSE：%s", truncate(string(body), 400))
	}
	if trailing := trailingDataPayload(body); trailing != "" {
		frames = append(frames, trailing)
	}
	return frames, nil
}

// trailingDataPayload returns the payload of a final `data:` line that the
// upstream did not terminate with a newline. Anything else is framing noise and
// is ignored.
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

// streamFrames turns the buffered upstream body into SSE frames for the client.
//
// The second result reports a TRUNCATED stream: no `finish_reason` and no
// `[DONE]`. The client still needs a terminator frame, so one is appended and the
// truncation is surfaced as a host warning instead of being silently converted
// into a clean stop (`openai-compat.ts:915-959`).
func streamFrames(body []byte) ([]pluginapi.ExecutorStreamChunk, bool, error) {
	payloads, errFrames := inferFrames(body)
	if errFrames != nil {
		return nil, false, errFrames
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(payloads)+1)
	sawDone := false
	for _, payload := range payloads {
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" {
			continue
		}
		if trimmed == sse.Done {
			sawDone = true
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.DoneEvent()})
			break
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.Encode(trimmed)})
	}
	if len(chunks) == 0 {
		return nil, false, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "Loomy 流中没有可解析的分片")
	}
	truncated := !sawDone && !sawFinishReason(payloads)
	if !sawDone {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.DoneEvent()})
	}
	return chunks, truncated, nil
}

// sawFinishReason reports whether any frame carried a usable finish reason.
func sawFinishReason(payloads []string) bool {
	for _, payload := range payloads {
		chunk := streamChunk{}
		if errUnmarshal := json.Unmarshal([]byte(strings.TrimSpace(payload)), &chunk); errUnmarshal != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
				return true
			}
		}
	}
	return false
}

// streamChunk is the subset of one `chat.completion.chunk` this adapter reads.
//
// Content and reasoning are POINTERS on purpose: the upstream sends them as
// explicit `null` as well as absent, and `delta.reasoning_content ??
// delta.reasoning` must be resolved with a `typeof === 'string'` test
// (`openai-compat.ts:592-594,629-638`, §6.2). Usage is kept raw so
// `prompt_tokens_details.cached_tokens` and the reasoning counters survive
// untouched.
type streamChunk struct {
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

// toolCall is the merged, non-streaming tool call.
type toolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// completionMessage is the assistant message of the folded completion.
type completionMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
}

// chatCompletion is the non-streaming answer built from the stream.
type chatCompletion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   json.RawMessage    `json:"usage,omitempty"`
}

// completionChoice is one non-streaming choice.
type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

// aggregateFrames folds a stream into one `chat.completion`.
//
// The second result reports a TRUNCATED stream: no `finish_reason` was seen and
// the stream did not end with `[DONE]`. That is retryable (max-tokens) rather
// than a clean stop (`openai-compat.ts:915-959`).
func aggregateFrames(payloads []string, model string) (chatCompletion, bool) {
	completion := chatCompletion{Object: "chat.completion", Model: model}
	var content, reasoning strings.Builder
	var toolCalls []toolCall
	toolIndex := map[int]int{}
	var finishReason *string
	sawDone := false

	for _, payload := range payloads {
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" {
			continue
		}
		if trimmed == sse.Done {
			sawDone = true
			continue
		}
		chunk := streamChunk{}
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &chunk); errUnmarshal != nil {
			continue
		}
		if chunk.ID != "" {
			completion.ID = chunk.ID
		}
		if chunk.Model != "" {
			completion.Model = chunk.Model
		}
		if chunk.Created != 0 {
			completion.Created = chunk.Created
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			completion.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				content.WriteString(*choice.Delta.Content)
			}
			if text := reasoningOf(choice.Delta.ReasoningContent, choice.Delta.Reasoning); text != "" {
				reasoning.WriteString(text)
			}
			for _, fragment := range choice.Delta.ToolCalls {
				mergeToolCall(&toolCalls, toolIndex, fragment)
			}
			if choice.FinishReason != nil {
				value := *choice.FinishReason
				finishReason = &value
			}
		}
	}

	if completion.Created == 0 {
		completion.Created = time.Now().Unix()
	}
	message := completionMessage{
		Role:             "assistant",
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        toolCalls,
	}
	resolved := "stop"
	truncated := false
	switch {
	case finishReason != nil && strings.TrimSpace(*finishReason) != "":
		resolved = *finishReason
	case sawDone:
		resolved = "stop"
	default:
		// No finish_reason and no [DONE]: truncated, i.e. the max-tokens case.
		resolved = "length"
		truncated = true
	}
	completion.Choices = []completionChoice{{Index: 0, Message: message, FinishReason: resolved}}
	return completion, truncated
}

// reasoningOf resolves `delta.reasoning_content ?? delta.reasoning`.
//
// Both may be explicitly null, so only a real string counts
// (`openai-compat.ts:629-638`).
func reasoningOf(reasoningContent, reasoning *string) string {
	if reasoningContent != nil {
		return *reasoningContent
	}
	if reasoning != nil {
		return *reasoning
	}
	return ""
}

// mergeToolCall accumulates one streamed tool call.
//
// A fragment that opens a NEW index without a usable function name is dropped:
// persisting an empty-named tool call poisons the conversation
// (`openai-compat.ts:691-721`). The id is remembered per index and the name may
// only be overwritten by a NON-EMPTY string (`openai-compat.ts:674-689`).
func mergeToolCall(target *[]toolCall, index map[int]int, fragment toolCallFragment) {
	position, ok := index[valueOr(fragment.Index, -1)]
	if !ok {
		if strings.TrimSpace(fragment.Function.Name) == "" {
			return
		}
		position = len(*target)
		index[valueOr(fragment.Index, len(*target))] = position
		*target = append(*target, toolCall{})
	}
	call := &(*target)[position]
	if fragment.ID != "" {
		call.ID = fragment.ID
	}
	if fragment.Type != "" {
		call.Type = fragment.Type
	}
	if strings.TrimSpace(fragment.Function.Name) != "" {
		call.Function.Name = fragment.Function.Name
	}
	call.Function.Arguments += fragment.Function.Arguments
}

// valueOr dereferences an optional int, falling back to fallback.
func valueOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

// canonicalHeader builds a header map through Set.
//
// Building the literal map instead would store the caller's spelling verbatim,
// and a later `Header.Get` (which canonicalises its argument) would then miss the
// entry — the classic "the header was silently dropped" bug.
func canonicalHeader(pairs ...string) http.Header {
	header := http.Header{}
	for index := 0; index+1 < len(pairs); index += 2 {
		header.Set(pairs[index], pairs[index+1])
	}
	return header
}
