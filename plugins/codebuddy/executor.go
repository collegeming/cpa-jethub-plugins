package main

// 本文件是 Jet-Hub `src/buddy-adapter.ts` 的 Go 移植（请求构造 + SSE 消费），
// 以及 `src/sse.ts` 的 `resolveToolPairing` / `normalizeToolArguments`。
//
// 与 TS 的映射关系：
//   - buddy-adapter.ts:231-358 serializeMessages  → normalizeMessages
//   - buddy-adapter.ts:462-517 userContentParts / collectImages → 图片在 CPA 侧
//     已是 OpenAI 线格式（image_url parts），无需附件服务；工具结果内嵌图片仍按
//     TS 的做法提升为独立的 user 消息。
//   - buddy-adapter.ts:988-1044 请求体 → buildChatBody
//   - buddy-adapter.ts:1109-1146 请求头与发送 → buildChatHeaders / sendChat
//   - buddy-adapter.ts:1156-1386 SSE 消费 → handleExecutorExecuteStream / aggregateChunks
//
// ⚠️ 一处**平台性差异**（不是简化）：TS 在流结束时会把「finish_reason 缺失」
// 或「工具参数残缺」的流改写为 DSH 的 `max-tokens` 结束原因，并做工具分片
// id/name 缝合。CPA 执行器两端都是 chat-completions 线格式，客户端（CPA 及其
// 下游）按 OpenAI 规范自行处理这些情形，改写反而会破坏协议保真度，因此这里
// 只做两件必要的事：原样转发分片，以及在发现上游内联错误帧时中止并分类。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authfile"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// toolResultImageText is the carrier text for images lifted out of tool
// results (buddy-adapter.ts:229).
const toolResultImageText = "Attached image(s) from tool result:"

// executorStreamResponse is the wire shape of executor.execute_stream.
// pluginapi.ExecutorStreamChunk has no JSON tags, so Payload travels base64.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// chatCall is a fully prepared upstream chat request.
type chatCall struct {
	URL     string
	Body    []byte
	Headers http.Header
	Model   string
	Product productConfig
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// ── 消息归一化 ──

// normalizeToolArguments turns the streamed arguments into a JSON object
// literal: "" → "{}" and unparseable → "{}" (sse.ts:125-138).
func normalizeToolArguments(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	var decoded any
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &decoded); errUnmarshal != nil {
		return "{}"
	}
	if decoded == nil {
		return "{}"
	}
	if _, isObject := decoded.(map[string]any); !isObject {
		return "{}"
	}
	return trimmed
}

// resolveToolPairing implements sse.ts:91-123 over OpenAI-shaped messages: a
// batch of tool_calls is kept only when every id has a matching tool result,
// and tool results are kept only for kept calls. This is the last line of
// defence against a session that replays an unpaired tool call forever.
func resolveToolPairing(messages []any) (map[string]bool, map[string]bool) {
	allResultIDs := map[string]bool{}
	for _, message := range messages {
		record, ok := message.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := record["role"].(string); role != "tool" {
			continue
		}
		if id, okID := record["tool_call_id"].(string); okID {
			allResultIDs[id] = true
		}
	}

	keepCallIDs := map[string]bool{}
	for _, message := range messages {
		record, ok := message.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := record["role"].(string); role != "assistant" {
			continue
		}
		calls, okCalls := record["tool_calls"].([]any)
		if !okCalls || len(calls) == 0 {
			continue
		}
		complete := true
		for _, call := range calls {
			entry, okEntry := call.(map[string]any)
			if !okEntry {
				continue
			}
			id, _ := entry["id"].(string)
			if !allResultIDs[id] {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		for _, call := range calls {
			if entry, okEntry := call.(map[string]any); okEntry {
				if id, okID := entry["id"].(string); okID {
					keepCallIDs[id] = true
				}
			}
		}
	}

	keepResultIDs := map[string]bool{}
	for id := range keepCallIDs {
		if allResultIDs[id] {
			keepResultIDs[id] = true
		}
	}
	return keepCallIDs, keepResultIDs
}

// normalizeMessages rewrites the inbound OpenAI messages into the exact shape
// CodeBuddy accepts. See the file header for the TS mapping.
func normalizeMessages(raw []any) []any {
	keepCallIDs, keepResultIDs := resolveToolPairing(raw)
	out := make([]any, 0, len(raw))
	var pendingImages []any
	flushImages := func() {
		if len(pendingImages) == 0 {
			return
		}
		parts := make([]any, 0, len(pendingImages)+1)
		parts = append(parts, map[string]any{"type": "text", "text": toolResultImageText})
		parts = append(parts, pendingImages...)
		out = append(out, map[string]any{"role": "user", "content": parts})
		pendingImages = nil
	}

	for _, message := range raw {
		record, ok := message.(map[string]any)
		if !ok {
			continue
		}
		role, _ := record["role"].(string)
		switch role {
		case "assistant":
			flushImages()
			out = append(out, normalizeAssistantMessage(record, keepCallIDs))
		case "system":
			flushImages()
			copied := map[string]any{"role": "system", "content": stringValue(record["content"])}
			out = append(out, copied)
		case "tool":
			id, _ := record["tool_call_id"].(string)
			// 丢弃孤儿工具结果（buddy-adapter.ts:329）。
			if !keepResultIDs[id] {
				continue
			}
			text, images := splitToolContent(record["content"])
			pendingImages = append(pendingImages, images...)
			if text == "" {
				text = "(no output)"
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      text,
			})
		default:
			// user / developer / function …: content is already OpenAI-shaped.
			flushImages()
			out = append(out, record)
		}
	}
	flushImages()
	return out
}

// normalizeAssistantMessage keeps the paired tool_calls, always emits
// reasoning_content (buddy-adapter.ts:224-226, 286-292) and uses null content
// when the text is empty but tool calls exist.
func normalizeAssistantMessage(record map[string]any, keepCallIDs map[string]bool) map[string]any {
	output := map[string]any{"role": "assistant"}
	content := record["content"]
	text := stringValue(content)

	var toolCalls []any
	if raw, ok := record["tool_calls"].([]any); ok {
		for _, call := range raw {
			entry, okEntry := call.(map[string]any)
			if !okEntry {
				continue
			}
			id, _ := entry["id"].(string)
			if !keepCallIDs[id] {
				continue
			}
			function, _ := entry["function"].(map[string]any)
			name, _ := function["name"].(string)
			arguments := normalizeToolArguments(stringValue(function["arguments"]))
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": arguments,
				},
			})
		}
	}

	if text == "" && len(toolCalls) > 0 {
		output["content"] = nil
	} else if content == nil {
		output["content"] = ""
	} else {
		output["content"] = content
	}
	// assistant 消息必须始终携带 reasoning_content（推理模型缺失会 400）。
	output["reasoning_content"] = stringValue(record["reasoning_content"])
	if len(toolCalls) > 0 {
		output["tool_calls"] = toolCalls
	}
	return output
}

// splitToolContent separates a tool message's text from images embedded in a
// multimodal content array (buddy-adapter.ts:333-348).
func splitToolContent(content any) (string, []any) {
	switch typed := content.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	case []any:
		var text strings.Builder
		var images []any
		for _, part := range typed {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if blockType, _ := block["type"].(string); blockType == "image_url" || blockType == "image" {
				images = append(images, block)
				continue
			}
			text.WriteString(stringValue(block["text"]))
		}
		return text.String(), images
	default:
		return "", nil
	}
}

// ── 请求体 ──

// isDeepSeekModel reports whether the model needs the explicit thinking switch
// (buddy-adapter.ts:53-55).
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// containsString reports membership in a small slice.
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// buildChatBody assembles the upstream body from the inbound payload.
//
// The inbound payload is already an OpenAI chat-completions body (CPA performs
// the cross-protocol translation before dispatch), so unknown fields such as
// top_p / tool_choice / response_format are preserved untouched.
func buildChatBody(
	payload []byte,
	model string,
	cfg Config,
	product productConfig,
	remote *remoteModel,
	promptCacheKey string,
) ([]byte, error) {
	body := map[string]any{}
	if len(payload) > 0 {
		if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil {
			return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "decode chat request: %v", errUnmarshal)
		}
	}
	if strings.TrimSpace(model) == "" {
		if existing, ok := body["model"].(string); ok {
			model = existing
		}
	}
	if strings.TrimSpace(model) == "" {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "chat request is missing a model")
	}
	body["model"] = model
	body["stream"] = true

	if messages, ok := body["messages"].([]any); ok {
		if len(messages) == 0 {
			return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "chat request has no messages")
		}
		body["messages"] = normalizeMessages(messages)
	} else {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "chat request has no messages")
	}

	// prompt_cache_key：命中前缀缓存（buddy-adapter.ts:992-998，实测费用差约 17 倍）。
	if cfg.PromptCacheKey {
		if _, exists := body["prompt_cache_key"]; !exists && promptCacheKey != "" {
			body["prompt_cache_key"] = promptCacheKey
		}
	}

	// 单次输出上限：调用方显式给值最高，其次远端 maxOutputTokens，其次产品兜底表，
	// 最后才是插件级默认；四者皆无则**不下发**该字段（AGENTS.md 明令不得编造）。
	if _, exists := body["max_tokens"]; !exists {
		if maxTokens, ok := resolveMaxTokens(remote, product, model, cfg); ok {
			body["max_tokens"] = maxTokens
		}
	}

	efforts := resolveReasoningEfforts(remote, product, model)
	deepseek := isDeepSeekModel(model)
	if deepseek && cfg.ThinkingEnabled {
		body["thinking"] = map[string]any{"type": "enabled"}
	}
	if requested, ok := body["reasoning_effort"].(string); ok && requested != "" {
		if !containsString(efforts, requested) {
			// 模型未声明该档位：丢弃，否则服务端 400（buddy-adapter.ts:1031-1034）。
			delete(body, "reasoning_effort")
		}
	} else if deepseek && len(efforts) > 0 {
		// deepseek 少了档位就等于不思考（buddy-adapter.ts:1035-1043）。
		body["reasoning_effort"] = defaultEffort(remote, product, model, efforts)
	}

	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_request", "encode chat request: %v", errMarshal)
	}
	return encoded, nil
}

// defaultEffort picks the effort to send when the caller did not choose one:
// the declared default, then "high", then the model's cheapest declared level.
func defaultEffort(remote *remoteModel, product productConfig, model string, efforts []string) string {
	declared := ""
	if remote != nil && remote.DefaultReasoningEffort != "" {
		declared = remote.DefaultReasoningEffort
	} else if entry, ok := fallbackEntry(product, model); ok {
		declared = entry.DefaultReasoningEffort
	}
	if declared != "" && containsString(efforts, declared) {
		return declared
	}
	if containsString(efforts, "high") {
		return "high"
	}
	return efforts[0]
}

// resolveMaxTokens applies the documented priority and filters illegal values
// (positiveMaxTokens, buddy-adapter.ts:1467-1469).
func resolveMaxTokens(remote *remoteModel, product productConfig, model string, cfg Config) (int, bool) {
	if remote != nil {
		if value, ok := positiveMaxTokens(remote.MaxOutputTokens); ok {
			return value, true
		}
	}
	if entry, ok := fallbackEntry(product, model); ok {
		if value, ok := positiveMaxTokens(entry.MaxOutputTokens); ok {
			return value, true
		}
	}
	if value, ok := positiveMaxTokens(int64(cfg.DefaultMaxTokens)); ok {
		return value, true
	}
	return 0, false
}

// resolveReasoningEfforts applies remote → product fallback. The third-tier
// static table from buddy-adapter.ts:145-156 is not duplicated because every
// product declares fallbackModels, which covers the same ids (see models.go).
func resolveReasoningEfforts(remote *remoteModel, product productConfig, model string) []string {
	if remote != nil && len(remote.ReasoningEfforts) > 0 {
		return remote.ReasoningEfforts
	}
	if entry, ok := fallbackEntry(product, model); ok {
		return entry.ReasoningEfforts
	}
	return nil
}

// fallbackEntry finds a model in the product's built-in catalog.
func fallbackEntry(product productConfig, model string) (fallbackModel, bool) {
	for _, entry := range product.FallbackModels {
		if entry.ID == model {
			return entry, true
		}
	}
	return fallbackModel{}, false
}

// resolveRemoteModel returns the remote metadata for a model, triggering the
// same lazy fetch the TS performs at the start of `stream()`
// (buddy-adapter.ts:927-930) so the remote capabilities apply.
func resolveRemoteModel(h *abiboot.Host, credential *Credential, product productConfig, cfg Config, model string) *remoteModel {
	ttl := time.Duration(cfg.ModelCacheTTLMS) * time.Millisecond
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	catalog, ok := discoveredModels.get(cachedCatalogKey(product), ttl)
	if !ok && cfg.DiscoverModels && credential != nil {
		catalog = discoveredCatalogFor(h, credential, product, cfg)
	}
	for index := range catalog {
		if catalog[index].ID == model {
			return &catalog[index]
		}
	}
	return nil
}

// promptCacheKeyFor derives a stable prefix-cache key. The TS uses a per-adapter
// session id (buddy-adapter.ts:561); a CPA executor is stateless per request, so
// the key is derived from (in order) an explicit metadata/header value or the
// conversation's stable prefix, which keeps the key identical across the turns
// of one conversation — the property that makes the cache hit at all.
func promptCacheKeyFor(request pluginapi.ExecutorRequest, body map[string]any) string {
	for _, key := range []string{"prompt_cache_key", "session_id", "sessionId", "conversation_id"} {
		if value, ok := request.Metadata[key].(string); ok && value != "" {
			return value
		}
	}
	for _, header := range []string{"X-Session-Id", "X-Conversation-Id", "Session-Id"} {
		if value := request.Headers.Get(header); value != "" {
			return value
		}
	}
	prefix := strings.Builder{}
	if system, ok := body["system"].(string); ok {
		prefix.WriteString(system)
	}
	if messages, ok := body["messages"].([]any); ok && len(messages) > 0 {
		if first, okFirst := messages[0].(map[string]any); okFirst {
			prefix.WriteString(stringValue(first["content"]))
		}
	}
	if prefix.Len() == 0 {
		return newPromptCacheKey()
	}
	sum := sha256.Sum256([]byte(prefix.String()))
	return hex.EncodeToString(sum[:16])
}

// ── 请求头（buddy-adapter.ts:1109-1131）──

// buildChatHeaders builds the identity header set CodeBuddy attributes usage by.
func buildChatHeaders(credential *Credential, product productConfig, model string) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+credential.AccessToken)
	headers.Set("Accept", "text/event-stream")
	headers.Set("Content-Type", "application/json")
	// X-Domain 跟随产品而非凭据（AGENTS.md「X-Domain 必须跟随产品」）；
	// credits.ts:180-186 的 checkinHeaders 也是同一口径。
	headers.Set(HeaderDomain, product.APIDomain)
	headers.Set(HeaderProductCode, product.ProductCode)
	// 用量归属头族：缺任一个后台「使用端」列都显示为 `-`。
	headers.Set("X-Agent-Purpose", "conversation")
	headers.Set("X-IDE-Name", product.AttributionName)
	headers.Set("X-IDE-Type", product.AttributionName)
	headers.Set("X-IDE-Version", product.ClientVersion)
	headers.Set(HeaderProduct, product.AttributionName)
	headers.Set("User-Agent", resolveUserAgent(product, model))
	return headers
}

// prepareChatCall builds the upstream request for one executor invocation.
func prepareChatCall(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, cfg Config) (*chatCall, error) {
	product := productForCredential(credential)
	model := strings.TrimSpace(request.Model)

	// Decode once to derive the cache key, then let buildChatBody re-encode.
	probe := map[string]any{}
	if len(request.Payload) > 0 {
		_ = json.Unmarshal(request.Payload, &probe)
	}
	if model == "" {
		model, _ = probe["model"].(string)
	}
	remote := resolveRemoteModel(h, credential, product, cfg, model)

	body, errBody := buildChatBody(request.Payload, model, cfg, product, remote, promptCacheKeyFor(request, probe))
	if errBody != nil {
		return nil, errBody
	}
	return &chatCall{
		URL:     product.urlFor("/v2/chat/completions"), // buddy-adapter.ts:1133
		Body:    body,
		Headers: buildChatHeaders(credential, product, model),
		Model:   model,
		Product: product,
	}, nil
}

// ── 发送 ──

// sendChat performs the chat request, refreshing once on 401/403 the way
// buddy-adapter.ts:1046-1055 does. It returns the response and the credential
// that actually produced it.
func sendChat(h *abiboot.Host, call *chatCall, credential *Credential, authID string) (*pluginapi.HTTPResponse, *Credential, error) {
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodPost,
		URL:     call.URL,
		Headers: call.Headers,
		Body:    call.Body,
	})
	if errDo != nil {
		return nil, credential, abiboot.HTTPError("TRANSPORT", http.StatusBadGateway,
			"CodeBuddy 传输错误：%v", errDo)
	}
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		return response, credential, nil
	}
	if !credential.Refreshable() {
		return response, credential, nil
	}
	refreshed, errRefresh := refreshCredential(h, credential, call.Product)
	if errRefresh != nil {
		return nil, credential, abiboot.HTTPError("AUTH", http.StatusUnauthorized,
			"CodeBuddy 凭据已过期且刷新失败：%v", errRefresh)
	}
	retryHeaders := buildChatHeaders(refreshed, call.Product, call.Model)
	retry, errRetry := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodPost,
		URL:     call.URL,
		Headers: retryHeaders,
		Body:    call.Body,
	})
	if errRetry != nil {
		return nil, refreshed, abiboot.HTTPError("TRANSPORT", http.StatusBadGateway,
			"CodeBuddy 传输错误（刷新后重试）：%v", errRetry)
	}
	persistRefreshed(h, authID, refreshed)
	return retry, refreshed, nil
}

// persistRefreshed writes a refreshed credential back to its auth file. It is
// best effort: the host's save callback only accepts a file name, and the
// executor receives the auth record id — the file name for a file-backed
// credential, a runtime index otherwise. A runtime index is skipped by name
// resolution rather than turned into a duplicate file.
func persistRefreshed(h *abiboot.Host, authID string, credential *Credential) {
	name := authfile.Name(nil, authID)
	if h == nil || name == "" {
		return
	}
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		return
	}
	if _, errSave := h.SaveAuth(name, storage); errSave != nil {
		h.Log("warn", "CodeBuddy 刷新后的凭据写回失败", map[string]any{
			"auth_id": authID,
			"error":   errSave.Error(),
		})
	}
}

// classifyFailure converts a non-2xx upstream reply into a CPA error envelope.
func classifyFailure(response *pluginapi.HTTPResponse, model string) error {
	bodyText := string(response.Body)
	code := httpErrorCode(response.StatusCode, bodyText)
	message := errorDetail(bodyText)
	if code == "CONTEXT_WINDOW_EXCEEDED" {
		return abiboot.HTTPError(code, http.StatusBadRequest,
			"CodeBuddy 上下文超限（模型 %s）：%s", model, message)
	}
	return abiboot.HTTPError(code, response.StatusCode, "CodeBuddy: %s", message)
}

// ── executor.execute ──

// executorCredential resolves the credential for one execution, renewing it
// first when it is expired or about to expire: the host's own refresh timer
// restarts with the process, so an expired token would otherwise be signed into
// a request the gateway is bound to reject.
func executorCredential(h *abiboot.Host, request pluginapi.ExecutorRequest) (*Credential, error) {
	fresh, errFresh := ensureCredentialFresh(h, authrefresh.Request{
		Name:        request.AuthID,
		StorageJSON: request.StorageJSON,
		Attributes:  request.AuthAttributes,
	})
	if errFresh != nil {
		return nil, errFresh
	}
	credential, errCredential := ParseCredential(fresh.Storage)
	if errCredential != nil {
		return nil, abiboot.HTTPError("AUTH", http.StatusUnauthorized,
			"CodeBuddy 凭据不可用：%v", errCredential)
	}
	return credential, nil
}

// handleExecutorExecute serves a non-streaming completion. The upstream call is
// always streamed (buddy-adapter.ts:991 `stream: true`) and the frames are
// folded into one chat.completion.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := executorCredential(h, request)
	if errCredential != nil {
		return nil, errCredential
	}
	call, errPrepare := prepareChatCall(h, request, credential, settings())
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, _, errSend := sendChat(h, call, credential, request.AuthID)
	if errSend != nil {
		return nil, errSend
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyFailure(response, call.Model)
	}

	chunks, errChunks := collectChunks(response.Body)
	if errChunks != nil {
		return nil, errChunks
	}
	payload, errMarshal := json.Marshal(aggregateChunks(chunks, call.Model))
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_response", "encode completion: %v", errMarshal)
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Metadata: map[string]any{
			"model":   call.Model,
			"product": call.Product.ConfigValue,
		},
	}, nil
}

// collectChunks decodes a buffered upstream body into stream chunks.
func collectChunks(body []byte) ([]chatChunk, error) {
	trimmed := []byte(strings.TrimSpace(string(body)))
	if len(trimmed) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "CodeBuddy 返回了空响应体")
	}
	// A whole completion object (no SSE framing) is accepted as-is.
	if trimmed[0] == '{' && !strings.Contains(string(trimmed), "data:") {
		if chunk, ok := parseChunk(string(trimmed)); ok && len(chunk.Choices) > 0 {
			return []chatChunk{chunk}, nil
		}
	}
	scanner := &sse.Scanner{}
	chunks := make([]chatChunk, 0, 64)
	for _, payload := range scanner.Feed(trimmed) {
		if payload == sse.Done {
			break
		}
		chunk, ok := parseChunk(payload)
		if !ok {
			continue
		}
		if detail, exceeded := chunkErrorPayload(&chunk); detail != "" {
			if exceeded {
				return nil, abiboot.HTTPError("CONTEXT_WINDOW_EXCEEDED", http.StatusBadRequest,
					"CodeBuddy 上下文超限：%s", detail)
			}
			return nil, abiboot.HTTPError("SERVER", http.StatusBadGateway, "CodeBuddy: %s", detail)
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) == 0 {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "CodeBuddy 流中没有可解析的分片")
	}
	return chunks, nil
}

// ── executor.execute_stream ──

// handleExecutorExecuteStream serves a streaming completion.
//
// Like the CodeArts reference the frames are returned in the response envelope
// rather than pushed through host.stream.emit; both are valid ABI paths.
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := executorCredential(h, request)
	if errCredential != nil {
		return nil, errCredential
	}
	call, errPrepare := prepareChatCall(h, request, credential, settings())
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, _, errSend := sendChat(h, call, credential, request.AuthID)
	if errSend != nil {
		return nil, errSend
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyFailure(response, call.Model)
	}

	scanner := &sse.Scanner{}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	sawDone := false
	sawChunk := false
	for _, payload := range scanner.Feed(response.Body) {
		if payload == sse.Done {
			sawDone = true
			break
		}
		if parsed, ok := parseChunk(payload); ok {
			// 上游把 400 塞进 HTTP 200 的 SSE 帧里（buddy-adapter.ts:1242-1255）；
			// 必须中止并分类，否则压缩子系统收不到溢出信号。
			if detail, exceeded := chunkErrorPayload(&parsed); detail != "" {
				if exceeded {
					return nil, abiboot.HTTPError("CONTEXT_WINDOW_EXCEEDED", http.StatusBadRequest,
						"CodeBuddy 上下文超限：%s", detail)
				}
				return nil, abiboot.HTTPError("SERVER", http.StatusBadGateway, "CodeBuddy: %s", detail)
			}
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.Encode(payload)})
		sawChunk = true
	}
	if !sawChunk {
		return nil, abiboot.HTTPError("empty_upstream", http.StatusBadGateway, "CodeBuddy 未返回任何流式分片")
	}
	if !sawDone {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: sse.DoneEvent()})
	}
	return executorStreamResponse{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  chunks,
	}, nil
}

// ── executor.count_tokens ──

// handleExecutorCountTokens returns a coarse estimate; CPA falls back to its
// own tokenizer when the executor does not implement counting, so an
// approximation beats an error.
func handleExecutorCountTokens(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	estimated := len(request.Payload)/4 + 1
	return pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":` + itoa(estimated) + `}`)}, nil
}

// itoa renders an int without importing strconv at call sites.
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
