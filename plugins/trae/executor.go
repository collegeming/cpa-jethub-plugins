package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// PluginVersion is the adapter version.
const PluginVersion = "0.1.0"

// machineIDRotateEvery is how many requests one machine-fingerprint generation
// lasts when rotation is enabled (trae-adapter.ts:141, mirroring the CN client's
// `max_uses = 3 + rand(0,2)`).
const machineIDRotateEvery = 4

// sendCounter counts chat requests; it only matters when machine-id rotation is
// switched on.
var sendCounter atomic.Int64

// executorStreamResponse is the wire shape of executor.execute_stream. The chunks
// are returned in the reply envelope rather than pushed through a host stream
// callback: this ABI version gives the plugin no incremental emit path, so the
// trade-off is buffering the response in exchange for a single round trip (the
// same choice plugins/codearts/executor.go documents).
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// currentMachineGeneration reports the machine-fingerprint generation for the
// next request. It is 0 — "use machine_id as stored" — unless rotation is
// explicitly enabled (trae-adapter.ts:1029-1032).
func currentMachineGeneration() int {
	cfg := settings()
	if !cfg.RotateMachineID {
		return 0
	}
	return int(sendCounter.Load() / machineIDRotateEvery)
}

// hostDo performs one buffered request through the host transport.
//
// The timeout parameter is accepted for symmetry with the Jet-Hub call sites but
// is unused: the ABI host callback carries no context, so the request lifetime is
// governed by the host's own transport timeouts.
func hostDo(h *abiboot.Host, method, url string, headers http.Header, body []byte, _ int) (*pluginapi.HTTPResponse, error) {
	if h == nil {
		return nil, abiboot.Errorf("host_unavailable", "插件未获得宿主回调上下文")
	}
	return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: url, Headers: headers, Body: body})
}

// newCompletionID mints an OpenAI-style completion id.
func newCompletionID() string {
	random, err := randomHexBytes(8)
	if err != nil {
		return "chatcmpl-trae"
	}
	return "chatcmpl-" + random
}

// toAnySlice widens a typed slice for the JSON-shaped helpers.
func toAnySlice[T any](values []T) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// chatCall is one prepared SOLO chat request.
type chatCall struct {
	URL          string
	Body         []byte
	Headers      http.Header
	Model        string
	Channel      string
	CompletionID string
}

// requestBody extracts the payload the executor must translate. CPA passes the
// translated provider payload in Payload; OriginalRequest is the fallback when no
// request translator ran.
func requestBody(request pluginapi.ExecutorRequest) []byte {
	if len(request.Payload) > 0 {
		return request.Payload
	}
	return request.OriginalRequest
}

// prepareChatCall performs the whole outbound pipeline:
//
//	serialize messages -> insert tools -> clamp max_tokens -> max-mode fields ->
//	trim history -> OpenAI -> SOLO conversion with the model's own channel
//
// The order is not cosmetic. Message serialization has to precede the SOLO
// conversion (docs/agents/trae.md:14-27), the SOLO conversion has to happen after
// tools are present because it stringifies `parameters`
// (docs/agents/trae.md:33), and the Max-mode fields have to be applied after the
// max_tokens clamp so a Max session keeps its much larger `__max` output budget
// (trae-adapter.ts:874-889).
func prepareChatCall(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config) (*chatCall, error) {
	raw := requestBody(request)
	if len(raw) == 0 {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "TRAE 执行器没有收到请求体")
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(raw, &root); errUnmarshal != nil {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "解析请求体失败: %v", errUnmarshal)
	}

	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = readStringField(root, "model")
	}
	if model == "" {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "请求缺少 model")
	}
	product := productFor(cfg.Region)

	// The catalog decides two things: which channel the model belongs to, and
	// whether the model accepts images. A failed catalog call is not fatal — the
	// default channel and the conservative "no images" answer are used instead.
	meta, metaKnown, errCatalog := remoteModelFor(h, credential, cfg, model)
	if errCatalog != nil && h != nil {
		h.Log("warn", "TRAE 模型目录不可用，使用默认通道", map[string]any{"error": errCatalog.Error(), "model": model})
	}

	messages, _ := asSlice(root["messages"])
	if len(messages) == 0 {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "请求缺少 messages")
	}
	// Image capability is per model. A model the catalog does not know is
	// treated as text-only rather than guessing (docs/agents/trae.md:204-247).
	if HasImageContent(messages) && !(metaKnown && modelSupportsImage(meta)) {
		return nil, abiboot.HTTPError("unsupported_content", http.StatusBadRequest,
			"trae: 模型「%s」不接受图片输入（远端未声明 multimodal 能力）", model)
	}

	// 1. DSH-native blocks -> OpenAI wire messages. `nil` image map: CPA carries
	// images as inline data URLs, so no attachment service is involved.
	root["messages"] = toAnySlice(SerializeMessages(messages, nil))

	// 2. Output budget, clamped to the upstream safe ceiling.
	if rawMax, ok := readNumberField(root, "max_tokens"); ok {
		root["max_tokens"] = ClampMaxTokens(int64(rawMax), int64(cfg.MaxCompletionTokens))
	}

	// 3. Max mode (1M context), only for models the account marks.
	if metaKnown && maxModeFor(meta, cfg) {
		for key, value := range MaxModeFields(meta.MaxContextWindow, meta.MaxModeOutputTokens) {
			root[key] = value
		}
	}

	// 4. History trimming, measured on wire messages.
	if wire, ok := root["messages"].([]any); ok {
		decoded := make([]map[string]any, 0, len(wire))
		for _, item := range wire {
			if message, ok := asMap(item); ok {
				decoded = append(decoded, message)
			}
		}
		trimmed := TrimTraeHistory(decoded, cfg.MaxHistoryChars)
		root["messages"] = toAnySlice(trimmed)
	}

	// 5. One-shot OpenAI -> SOLO conversion, routed to the model's channel.
	channel := channelFor(meta, metaKnown, cfg)
	solo := TransformToSOLOBody(root, configNameFor(model), channel)
	body, errMarshal := json.Marshal(solo)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_request", "序列化 SOLO 请求失败: %v", errMarshal)
	}

	sendCounter.Add(1)
	return &chatCall{
		URL:          product.AgentHost + ChatPath,
		Body:         body,
		Headers:      soloHeaders(credential, product, true, currentMachineGeneration()),
		Model:        model,
		Channel:      channel,
		CompletionID: newCompletionID(),
	}, nil
}

// sendChat posts a prepared request and maps upstream failures onto plugin
// errors. A 401/403 is surfaced as AUTH so the host refreshes the credential;
// credential rotation is CPA's scheduler's job, not the plugin's.
func sendChat(h *abiboot.Host, credential *Credential, call *chatCall, cfg Config) (*pluginapi.HTTPResponse, error) {
	response, errDo := hostDo(h, http.MethodPost, call.URL, call.Headers, call.Body, cfg.RequestTimeoutMS)
	if errDo != nil {
		return nil, abiboot.RetryableError("transport", "TRAE 请求失败: %v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, upstreamError(response.StatusCode, string(response.Body))
	}
	return response, nil
}

// handleExecutorExecute serves a non-streaming completion by folding the SOLO
// stream into one chat.completion object.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := decodeCredential(request.StorageJSON)
	if errCredential != nil {
		return nil, errCredential
	}
	cfg := settings()
	call, errPrepare := prepareChatCall(h, request, credential, cfg)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errChat := sendChat(h, credential, call, cfg)
	if errChat != nil {
		return nil, errChat
	}

	completion, streamErr, sawEvent := AggregateSOLOToCompletion(response.Body, call.Model, call.CompletionID, time.Now().Unix())
	if streamErr != nil {
		return nil, streamErrorToPlugin(streamErr)
	}
	if !sawEvent {
		// HTTP 200 with no events at all: upstream silently ended the stream.
		// That is retryable *only* before the first model event
		// (docs/agents/trae.md:494-505).
		return nil, abiboot.RetryableError("empty_upstream",
			"trae: 上游未返回任何事件（流在首个模型事件之前结束），可重试")
	}
	payload, errMarshal := json.Marshal(completion)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_response", "序列化 completion 失败: %v", errMarshal)
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Metadata: map[string]any{
			"model":   call.Model,
			"channel": call.Channel,
		},
	}, nil
}

// handleExecutorExecuteStream serves a streaming completion, converting the SOLO
// event stream into OpenAI `chat.completion.chunk` frames.
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := decodeCredential(request.StorageJSON)
	if errCredential != nil {
		return nil, errCredential
	}
	cfg := settings()
	call, errPrepare := prepareChatCall(h, request, credential, cfg)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errChat := sendChat(h, credential, call, cfg)
	if errChat != nil {
		return nil, errChat
	}

	frames, streamErr, sawEvent := TranslateSOLOStream(response.Body, call.Model, call.CompletionID, time.Now().Unix())
	if streamErr != nil {
		// An `event:error` frame is a business rejection, not a transport
		// failure; the reference adapter throws rather than emitting a partial
		// answer (trae-adapter.ts:1217-1228).
		return nil, streamErrorToPlugin(streamErr)
	}
	if !sawEvent {
		return nil, abiboot.RetryableError("empty_upstream",
			"trae: 上游未返回任何事件（流在首个模型事件之前结束），可重试")
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(frames))
	for _, frame := range frames {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: frame})
	}
	return executorStreamResponse{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  chunks,
	}, nil
}

// handleExecutorCountTokens returns a coarse estimate. CPA falls back to its own
// tokenizer when an executor does not implement counting, so an approximation is
// preferable to an error.
func handleExecutorCountTokens(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	estimated := len(requestBody(request))/4 + 1
	return pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":` + itoa(estimated) + `}`)}, nil
}

// decodeCredential parses the executor's credential payload.
func decodeCredential(storage []byte) (*Credential, error) {
	credential, err := ParseCredential(storage)
	if err != nil {
		return nil, abiboot.HTTPError("invalid_credential", http.StatusUnauthorized, "%s", err.Error())
	}
	return credential, nil
}

// streamErrorToPlugin converts an in-stream SOLO error into a plugin error.
func streamErrorToPlugin(streamErr *SOLOStreamError) error {
	status := http.StatusBadGateway
	code := "upstream_error"
	switch streamErr.Code {
	case 1005:
		status, code = http.StatusForbidden, "plan_limit"
	case 4008:
		status, code = http.StatusTooManyRequests, "quota_exceeded"
	case 4011:
		status, code = http.StatusTooManyRequests, "soft_rate"
	case 4001:
		status, code = http.StatusBadRequest, "invalid_param"
	}
	return abiboot.HTTPError(code, status, "%s", streamErr.Error())
}

// ── request.translate / response.translate ──

// handleRequestTranslate converts an OpenAI chat-completions request into the
// SOLO body. The executor re-runs the same conversion because only it knows the
// model's real channel and the account's catalog; this route exists so the host
// sees a genuine provider payload.
func handleRequestTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.RequestTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(request.Body, &root); errUnmarshal != nil {
		// Not JSON: nothing to translate, hand it back untouched.
		return pluginapi.PayloadResponse{Body: request.Body}, nil
	}
	// Already SOLO: leave it alone (the executor will still fix the channel).
	if _, ok := root["config_name"]; ok {
		if function := readStringField(root, "function"); function != "" {
			return pluginapi.PayloadResponse{Body: request.Body}, nil
		}
	}
	if messages, ok := asSlice(root["messages"]); ok {
		root["messages"] = toAnySlice(SerializeMessages(messages, nil))
	}
	cfg := settings()
	model := request.Model
	if model == "" {
		model = readStringField(root, "model")
	}
	channel := cfg.DefaultChannel
	if channel == "" {
		channel = DefaultFunction
	}
	encoded, errMarshal := json.Marshal(TransformToSOLOBody(root, configNameFor(model), channel))
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_request", "序列化 SOLO 请求失败: %v", errMarshal)
	}
	return pluginapi.PayloadResponse{Body: encoded}, nil
}

// handleResponseTranslate converts a SOLO response into OpenAI. Non-SOLO bodies
// (already OpenAI, or empty) pass through unchanged so the route is safe to
// register even when the host also translates.
func handleResponseTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ResponseTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	if !looksLikeSOLO(request.Body) {
		return pluginapi.PayloadResponse{Body: request.Body}, nil
	}
	model := request.Model
	created := time.Now().Unix()
	if request.Stream {
		frames, streamErr, _ := TranslateSOLOStream(request.Body, model, newCompletionID(), created)
		if streamErr != nil {
			return nil, streamErrorToPlugin(streamErr)
		}
		var builder strings.Builder
		for _, frame := range frames {
			builder.Write(frame)
		}
		return pluginapi.PayloadResponse{Body: []byte(builder.String())}, nil
	}
	completion, streamErr, _ := AggregateSOLOToCompletion(request.Body, model, newCompletionID(), created)
	if streamErr != nil {
		return nil, streamErrorToPlugin(streamErr)
	}
	encoded, errMarshal := json.Marshal(completion)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_response", "序列化 completion 失败: %v", errMarshal)
	}
	return pluginapi.PayloadResponse{Body: encoded}, nil
}

// looksLikeSOLO reports whether a payload carries SOLO event frames.
func looksLikeSOLO(body []byte) bool {
	text := string(body)
	if !strings.Contains(text, "data:") {
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(text), "event:") {
		return true
	}
	return strings.Contains(text, "\nevent:") || strings.Contains(text, "\r\nevent:")
}

// describeChatError renders a short diagnosis for the management UI.
func describeChatError(err error) string {
	if err == nil {
		return ""
	}
	var envelope *abiboot.EnvelopeError
	if typed, ok := err.(*abiboot.EnvelopeError); ok {
		envelope = typed
	}
	if envelope != nil && envelope.HTTPStatus != 0 {
		return fmt.Sprintf("%s (HTTP %d)", envelope.Message, envelope.HTTPStatus)
	}
	return err.Error()
}
