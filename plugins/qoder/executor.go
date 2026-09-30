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
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Inference path selection.
//
// Qoder has TWO inference endpoints that recognise TWO different model-name
// sets (`qoder-adapter.ts:4-20`, `AGENTS.md`):
//
//	encrypted  {EncryptedInferBase}/algo/api/v2/service/pro/sse/agent_chat_generation?Encode=1
//	           accepts catalog keys (qfmodel, dmodel, ...); the request body is
//	           signed by the WASM and its headers MUST be forwarded verbatim
//	           (`qoder-wasm.ts:490-496`, `qoder-adapter.ts:319-320`);
//	public     {InferBase}/model/v1/chat/completions
//	           accepts generic names (qwen-flash, ...) and rejects every catalog
//	           key with `Unsupported model`.
//
// Mixing them returns 404 or an invalid-model error, so the path is chosen
// explicitly from the configuration and the credential, never by guessing.

// inferPath names the transport a request will use.
type inferPath string

const (
	// pathEncrypted is the client-equivalent chain (needs a WASM signer).
	pathEncrypted inferPath = "encrypted"
	// pathPublic is the OpenAI-compatible fallback that needs no WASM.
	pathPublic inferPath = "public"
)

// activeInferPath reports which path the configuration selects.
func activeInferPath(cfg Config) inferPath {
	// The WASM signer is embedded in the plugin; the encrypted path is always
	// available. `wasm_path` remains as an override for custom signer builds.
	return pathEncrypted
}

// executorStreamResponse is the wire shape of executor.execute_stream.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// hostRequest performs one buffered upstream call through the host transport.
//
// Every outbound call goes through the host so proxy, TLS, and request logging
// stay under the host's control (the plugin never opens a socket itself).
func hostRequest(h *abiboot.Host, method, rawURL string, headers http.Header, body []byte, _ Config) (*pluginapi.HTTPResponse, error) {
	if h == nil {
		return nil, transportError("host_unavailable", "插件未通过宿主调用（缺少 host 句柄）")
	}
	return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: rawURL, Headers: headers, Body: body})
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleRequestTranslate is an identity transform. The executor declares
// chat-completions on both sides, so the host performs any cross-protocol
// translation itself and this route never has a gap to fill.
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

// handleExecutorExecute serves a non-streaming completion. Qoder only streams
// upstream, so the stream is folded into one chat.completion.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, cfg, errPrepare := decodeExecutorCall(h, raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, used, errInfer := performInfer(h, request, credential, cfg)
	if errInfer != nil {
		return nil, errInfer
	}
	chunks, errChunks := collectChatChunks(response.Body, used == pathEncrypted)
	if errChunks != nil {
		return nil, errChunks
	}
	completion := aggregateChunks(chunks, request.Model)
	payload, errMarshal := json.Marshal(completion)
	if errMarshal != nil {
		return nil, statusError(false, "encode_response", http.StatusInternalServerError, "encode completion: %v", errMarshal)
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: canonicalHeader("Content-Type", "application/json"),
		Metadata: map[string]any{
			"model":                  request.Model,
			"infer_path":             string(used),
			"provider":               ProviderKey,
			"region":                 string(credential.regionOr(cfg.Region)),
			"first_token_timeout_ms": cfg.FirstTokenTimeoutMS,
			"chunk_timeout_ms":       cfg.ChunkTimeoutMS,
		},
	}, nil
}

// handleExecutorExecuteStream serves a streaming completion.
//
// The frames are returned in the response envelope rather than pushed through
// host.stream.emit; both are valid ABI paths and this one is what the reference
// plugin does (see its README note on the trade-off).
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, cfg, errPrepare := decodeExecutorCall(h, raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, used, errInfer := performInfer(h, request, credential, cfg)
	if errInfer != nil {
		return nil, errInfer
	}
	chunks, errChunks := streamChatChunks(response.Body, used == pathEncrypted)
	if errChunks != nil {
		return nil, errChunks
	}
	return executorStreamResponse{
		Headers: canonicalHeader("Content-Type", "text/event-stream"),
		Chunks:  chunks,
	}, nil
}

// decodeExecutorCall decodes the request and the credential it is bound to.
//
// The credential is renewed first when it is expired or about to expire: the
// host's own refresh timer restarts with the process, so an expired token would
// otherwise be signed into a request the gateway is bound to reject.
func decodeExecutorCall(h *abiboot.Host, raw json.RawMessage) (pluginapi.ExecutorRequest, *Credential, Config, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return request, nil, Config{}, errDecode
	}
	fresh, errFresh := ensureCredentialFresh(h, authrefresh.Request{
		Name:        request.AuthID,
		StorageJSON: request.StorageJSON,
		Attributes:  request.AuthAttributes,
	})
	if errFresh != nil {
		return request, nil, Config{}, errFresh
	}
	credential, errCredential := ParseCredential(fresh.Storage)
	if errCredential != nil {
		return request, nil, Config{}, errCredential
	}
	return request, credential, settings(), nil
}

// performInfer runs one upstream inference and returns the buffered response.
//
// A 401/403 triggers one refresh-and-retry when the credential is refreshable,
// mirroring `qoder-adapter.ts:321-330`.
func performInfer(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config) (*pluginapi.HTTPResponse, inferPath, error) {
	path := activeInferPath(cfg)
	if path == pathEncrypted && strings.TrimSpace(credential.UID) == "" {
		// Without uid the WASM derives an invalid `encrypt_user_info` and the
		// server answers `Signature invalid (101)`, which looks like a signing
		// bug rather than an incomplete credential (`qoder-adapter.ts:345-362`).
		return nil, path, abiboot.HTTPError("missing_uid", http.StatusUnauthorized,
			"Qoder 凭据缺少 uid，加密推理无法签名（请重新登录该账号）")
	}

	response, errSend := sendInfer(h, request, credential, cfg, path)
	if errSend != nil {
		return nil, path, errSend
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		if !credential.Refreshable() {
			return nil, path, upstreamError(response)
		}
		refreshed, errRefresh := refreshCredential(h, credential, cfg)
		if errRefresh != nil {
			return nil, path, abiboot.HTTPError("auth", http.StatusUnauthorized,
				"Qoder 凭据已过期且续期失败：%v", errRefresh)
		}
		response, errSend = sendInfer(h, request, refreshed, cfg, path)
		if errSend != nil {
			return nil, path, errSend
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, path, upstreamError(response)
	}
	return response, path, nil
}

// sendInfer performs exactly one upstream request on the selected path.
func sendInfer(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config, path inferPath) (*pluginapi.HTTPResponse, error) {
	p := credential.product(cfg.Region)
	if path == pathPublic {
		return sendPublic(h, request, credential, cfg, p)
	}
	return sendEncrypted(h, request, credential, cfg, p)
}

// sendPublic posts the payload to the OpenAI-compatible endpoint.
//
// The model name is forwarded VERBATIM: the public endpoint's generic names do
// not appear anywhere in the TypeScript sources, so this plugin must not invent a
// mapping (`qoder-product.ts:240-249`). A catalog key sent here will be rejected
// with `Unsupported model` — that is the documented upstream behaviour.
func sendPublic(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config, p *product) (*pluginapi.HTTPResponse, error) {
	body, errBody := publicRequestBody(request, cfg)
	if errBody != nil {
		return nil, errBody
	}
	headers := chatHeaders(credential, p, randomUUID(), randomUUID())
	return hostRequest(h, http.MethodPost, p.InferBase+PublicChatPath, headers, body, cfg)
}

// sendEncrypted builds the signed request through the WASM signer and forwards
// the returned headers UNCHANGED.
//
// ⚠️ The WASM produces `Authorization: Bearer COSY.<payload>.<signature>`.
// Replacing it with an ordinary bearer token makes the server answer
// `Signature invalid` (`qoder-wasm.ts:490-496`), which is why only `Accept` is
// allowed to be set afterwards (`qoder-adapter.ts:369-371`).
func sendEncrypted(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config, p *product) (*pluginapi.HTTPResponse, error) {
	signer, errSigner := signerFor(cfg.WASMPath)
	if errSigner != nil {
		// A plugin-side misconfiguration, not an upstream fault: the request never
		// leaves the process, so 500 is the honest classification.
		return nil, statusError(false, "wasm_signer", http.StatusInternalServerError,
			"无法加载 Qoder 签名 WASM（%s）：%v", cfg.WASMPath, errSigner)
	}
	ask, errAsk := inferAskFromRequest(request, credential, cfg, p)
	if errAsk != nil {
		return nil, errAsk
	}
	signed, errSign := signer.Sign(signRequest{
		UID:           credential.UID,
		Token:         credential.bearerToken(),
		MachineID:     credential.MachineID,
		ClientVersion: clientVersion(cfg),
		Metadata:      sharedClientMetadata,
		Host:          p.EncryptedInferBase,
		Ask:           ask,
	})
	if errSign != nil {
		return nil, statusError(false, "wasm_sign", http.StatusInternalServerError, "WASM 签名失败：%v", errSign)
	}
	signedHeaders := signed.Headers
	// The WASM fills `Cosy-MachineToken` with the session machine_id and
	// `Cosy-MachineType` with a constant — placeholders the server does not
	// credit as a device. The official client's identity replaces them when
	// configured, so inference rides an activated device like the IDE's own.
	applyMachineHeaders(signedHeaders, cfg)
	headers := http.Header{}
	for name, value := range signedHeaders {
		headers.Set(name, value)
	}
	headers.Set("Accept", "text/event-stream")
	return hostRequest(h, http.MethodPost, signed.URL, headers, []byte(signed.Body), cfg)
}

// clientVersion resolves the Cosy-Version value.
func clientVersion(cfg Config) string {
	if strings.TrimSpace(cfg.ClientVersion) == "" {
		return DefaultClientVersion
	}
	return strings.TrimSpace(cfg.ClientVersion)
}

// sessionType resolves the `session_type` written into the encrypted payload.
func sessionType(cfg Config, p *product) string {
	if trimmed := strings.TrimSpace(cfg.SessionType); trimmed != "" {
		return trimmed
	}
	return p.SessionType
}

// wireRequest is the subset of a chat-completions body this adapter reads.
type wireRequest struct {
	Model           string        `json:"model"`
	Stream          bool          `json:"stream"`
	MaxTokens       *int          `json:"max_tokens"`
	ReasoningEffort string        `json:"reasoning_effort"`
	Messages        []wireMessage `json:"messages"`
}

// wireMessage keeps the raw content so both the plain-string form and the
// multimodal parts array are handled.
type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseWireRequest decodes the translated chat-completions payload.
func parseWireRequest(payload []byte) (wireRequest, error) {
	var request wireRequest
	if len(payload) == 0 {
		return request, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "Qoder 收到空请求体")
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		return request, abiboot.HTTPError("invalid_request", http.StatusBadRequest,
			"解码 chat-completions 请求失败：%v", err)
	}
	return request, nil
}

// messageText flattens a message content value to text.
//
// Only text parts are used: the encrypted endpoint takes plain strings, and the
// public path forwards the original body untouched.
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var builder strings.Builder
		for _, part := range parts {
			if part.Type == "text" {
				builder.WriteString(part.Text)
			}
		}
		return builder.String()
	}
	return ""
}

// itoaInt renders a non-negative int.
func itoaInt(value int) string {
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

// upstreamError classifies a non-2xx upstream answer.
//
// The status is normalised rather than passed through: the host uses it to decide
// whether the request was at fault (and therefore whether the credential should
// be rotated), and an upstream 404 or 403 says nothing useful about that.
//
// ⚠️ A 401/403 body is READ before the status decides anything, because Qoder
// sends TWO conditions that arrive as 403 and neither of which is an auth
// failure: the queue code `10605` and the quota code `110`
// (`qoder-adapter.ts:747-757`, `:795-820`). Classifying them by status alone
// rotates a perfectly healthy credential, and the rotation cannot help because
// the credential was never the problem.
func upstreamError(response *pluginapi.HTTPResponse) error {
	detail := truncate(string(response.Body), 300)
	status := response.StatusCode
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if kind, parsed := classifyBusinessBody(string(response.Body)); kind != businessUnknown {
			return businessErrorFor(kind, parsed, status)
		}
		return credentialError("auth", "Qoder 凭据无效（HTTP %d）：%s%s", status, detail, credentialAdvice(status))
	case status == http.StatusPaymentRequired:
		return statusError(false, "quota_exhausted", http.StatusPaymentRequired, "Qoder 额度已耗尽：%s", detail)
	case status == http.StatusTooManyRequests:
		return statusError(true, "rate_limited", http.StatusTooManyRequests, "Qoder 限流：%s", detail)
	case status == http.StatusGatewayTimeout:
		return statusError(true, "upstream_timeout", http.StatusGatewayTimeout, "Qoder 上游超时：%s", detail)
	default:
		return transportError("upstream_error", "Qoder 返回 HTTP %d：%s", status, detail)
	}
}

// businessErrorFor builds the classified failure for a recognised business body.
// The SSE-frame channel and this HTTP-level one share it so they can never drift
// into handling only one of them — the defect recorded at
// `qoder-adapter.ts:786-788`.
//
// `origin` is only woven into the message (the frame channel passes 0 because the
// HTTP status there was a successful 200 and naming it would mislead); the
// classification itself is identical on both channels.
func businessErrorFor(kind businessKind, parsed businessError, origin int) error {
	where := "加密端点帧内"
	if origin > 0 {
		where = "HTTP " + itoaInt(origin)
	}
	switch kind {
	case businessQuota:
		// Non-retryable: the daily quota cannot recover within a retry budget,
		// so retrying only burns the backoff schedule (`2c1af59`) and the host
		// must rotate the credential instead.
		return statusError(false, "QUOTA_EXCEEDED", http.StatusPaymentRequired,
			"Qoder 额度已耗尽（不可重试，%s）：%s", where, parsed.text())
	case businessQueue:
		delay, hasDelay := parsed.Queue.delayMS()
		if hasDelay {
			// The server stated how long the queue needs; honour it, clamped to
			// QueueMaxDelayMS (`model-queue.ts:138`). Without a stated delay the
			// host applies its own backoff — the `QUEUE` code already tells it
			// this is a queue and not an upstream fault.
			queueSleep(time.Duration(delay) * time.Millisecond)
		}
		return statusError(true, "QUEUE", http.StatusTooManyRequests,
			"Qoder 模型排队中（等待 %dms，%s）：%s", delay, where, parsed.text())
	default:
		return statusError(true, "upstream_error", http.StatusBadGateway,
			"Qoder 加密端点返回业务错误（%s）：%s", where, parsed.text())
	}
}

// classifyBusinessBody parses a body and reports what it means. A body that is
// not a JSON object at all is businessUnknown, so an HTML 403 error page keeps
// the auth classification.
func classifyBusinessBody(body string) (businessKind, businessError) {
	parsed, ok := parseBusinessError(body)
	if !ok {
		return businessUnknown, businessError{}
	}
	return classifyBusinessError(parsed), parsed
}

// publicRequestBody rewrites the model and stream fields of the outgoing body.
func publicRequestBody(request pluginapi.ExecutorRequest, cfg Config) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(request.Payload, &body); err != nil {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "解码请求体失败：%v", err)
	}
	if _, ok := body["model"]; !ok && request.Model != "" {
		body["model"] = request.Model
	}
	body["stream"] = true
	if cfg.DefaultMaxTokens > 0 {
		if _, ok := body["max_tokens"]; !ok {
			body["max_tokens"] = cfg.DefaultMaxTokens
		}
	}
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, statusError(false, "encode_request", http.StatusInternalServerError, "encode request: %v", errMarshal)
	}
	return encoded, nil
}

// streamChatChunks converts a buffered upstream body into SSE frames.
func streamChatChunks(body []byte, encrypted bool) ([]pluginapi.ExecutorStreamChunk, error) {
	if len(body) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "Qoder 返回了空响应体")
	}
	scanner := &sse.Scanner{}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	for _, payload := range scanner.Feed(body) {
		inner, errFrame := normalizeFrame(payload, encrypted)
		if errFrame != nil {
			return nil, errFrame
		}
		if inner == "" {
			continue
		}
		if inner == sse.Done {
			// The host writes the terminal event itself.
			break
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.Payload(inner)})
	}
	if len(chunks) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "Qoder 流中没有可解析的分片")
	}
	return chunks, nil
}

// collectChatChunks turns a buffered upstream body into chat completion chunks.
func collectChatChunks(body []byte, encrypted bool) ([]openai.Chunk, error) {
	if len(body) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "Qoder 返回了空响应体")
	}
	// A whole completion object (no SSE framing) is accepted as-is.
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") && !strings.Contains(trimmed, "data:") {
		completion := openai.Completion{}
		if err := json.Unmarshal([]byte(trimmed), &completion); err == nil && len(completion.Choices) > 0 {
			return chunksFromCompletion(completion), nil
		}
	}

	scanner := &sse.Scanner{}
	chunks := make([]openai.Chunk, 0, 64)
	for _, payload := range scanner.Feed(body) {
		inner, errFrame := normalizeFrame(payload, encrypted)
		if errFrame != nil {
			return nil, errFrame
		}
		if inner == "" || inner == sse.Done {
			continue
		}
		chunk := openai.Chunk{}
		if err := json.Unmarshal([]byte(inner), &chunk); err != nil {
			continue
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "Qoder 流中没有可解析的分片")
	}
	return chunks, nil
}

// normalizeFrame unwraps the encrypted endpoint's envelope and rejects business
// error frames.
//
// The encrypted endpoint wraps every frame, so a frame without `choices` is an
// error and must be raised rather than skipped — silently dropping it is exactly
// the "no reply, no error" bug (`qoder-envelope.ts:18-22`).
//
// ⚠️ A frame without `choices` is NOT automatically an upstream fault. The
// measured bodies carry a business `code`, and two of them must reach the host
// as something other than 502/retryable:
//
//   - quota (`110`, `Billing daily count exceeded`) is DETERMINISTIC, so it
//     becomes a non-retryable 402 and the host rotates the credential instead of
//     paying five backoffs for a result that cannot change (`2c1af59`);
//   - queue (`10605`) is TEMPORARY and carries the delay the server wants, which
//     is waited out here, the same place upstream waits (`daf9fb1`).
//
// Everything else keeps the historical generic classification.
func normalizeFrame(payload string, encrypted bool) (string, error) {
	if payload == sse.Done {
		return sse.Done, nil
	}
	if !encrypted {
		return payload, nil
	}
	inner, isEnvelope := unwrapEnvelopePayload(payload)
	if !isEnvelope {
		// Tolerance for a server that answers standard frames one day.
		return payload, nil
	}
	if strings.TrimSpace(inner) == "" {
		return "", nil
	}
	if !looksLikeChatFrame(inner) {
		return "", businessFrameError(inner)
	}
	if strings.TrimSpace(inner) == sse.Done {
		return sse.Done, nil
	}
	return inner, nil
}

// businessFrameError classifies the inner body of an error frame.
//
// The body is parsed recursively before anything is decided; the frame's own
// `code` is never used as a gate, because the measured queue frame wraps the
// business code one JSON-string layer down (`model-queue.ts:14-22`, upstream
// `daf9fb1`).
//
// The frame channel and the HTTP channel deliberately share `businessErrorFor`,
// so a future change to either classification lands on both. Handling only one
// of them is the defect recorded at `qoder-adapter.ts:786-788`.
func businessFrameError(inner string) error {
	parsed, ok := parseBusinessError(inner)
	if !ok {
		// Not a JSON object at all: the `[FAIL]node:… msg:…` text frame keeps
		// the generic upstream failure and its own text, which is the only
		// detail available.
		return statusError(true, "upstream_error", http.StatusBadGateway,
			"Qoder 加密端点返回业务错误：%s", envelopeErrorMessage(inner))
	}
	return businessErrorFor(classifyBusinessError(parsed), parsed, 0)
}

// chunksFromCompletion projects a whole completion into one delta chunk.
func chunksFromCompletion(completion openai.Completion) []openai.Chunk {
	out := make([]openai.Chunk, 0, len(completion.Choices))
	for _, choice := range completion.Choices {
		out = append(out, openai.Chunk{
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
	return out
}

// aggregateChunks folds a stream into one chat.completion so the non-streaming
// route can answer clients that did not ask for SSE.
func aggregateChunks(chunks []openai.Chunk, model string) openai.Completion {
	completion := openai.Completion{Object: "chat.completion", Model: model}
	var content, reasoning strings.Builder
	var toolCalls []openai.ToolCall
	var finishReason *string
	seenToolIndex := map[int]int{}
	for _, chunk := range chunks {
		if chunk.ID != "" {
			completion.ID = chunk.ID
		}
		if chunk.Created != 0 {
			completion.Created = chunk.Created
		}
		if chunk.Model != "" {
			completion.Model = chunk.Model
		}
		if chunk.Usage != nil {
			completion.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				content.WriteString(choice.Delta.Content)
			}
			if choice.Delta.ReasoningContent != "" {
				reasoning.WriteString(choice.Delta.ReasoningContent)
			}
			for _, call := range choice.Delta.ToolCalls {
				index := len(toolCalls)
				if call.Index != nil {
					index = *call.Index
				}
				if position, ok := seenToolIndex[index]; ok {
					mergeToolCall(&toolCalls[position], call)
					continue
				}
				seenToolIndex[index] = len(toolCalls)
				toolCalls = append(toolCalls, call)
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
	message := openai.Message{
		Role:             "assistant",
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        toolCalls,
	}
	completion.Choices = []openai.Choice{{Index: 0, Message: message, FinishReason: finishReason}}
	return completion
}

// mergeToolCall accumulates the fragments of one streamed tool call. Only a
// non-empty name overwrites what was already parsed, and arguments concatenate
// (`openai-compat.ts:20-30`).
func mergeToolCall(target *openai.ToolCall, fragment openai.ToolCall) {
	if fragment.ID != "" {
		target.ID = fragment.ID
	}
	if fragment.Type != "" {
		target.Type = fragment.Type
	}
	if fragment.Function.Name != "" {
		target.Function.Name = fragment.Function.Name
	}
	target.Function.Arguments += fragment.Function.Arguments
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
