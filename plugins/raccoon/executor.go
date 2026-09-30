package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/imagebudget"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Inference: `executor.execute` and `executor.execute_stream`.
//
// The upstream is ALWAYS asked to stream (`stream: true`,
// `raccoon-adapter.ts:277`); the non-streaming path folds the stream into one
// `chat.completion`. Timeouts are the reference's: NO wall-clock limit for chat,
// only idle limits (`raccoon-adapter.ts:357-367`). The host transport used here
// is buffered, so an idle limit cannot be expressed; a wall-clock limit would be
// a deviation the reference explicitly does not have, so none is applied.

// executorStreamResponse is the wire shape of executor.execute_stream.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleRequestTranslate normalises the outbound chat-completions payload.
//
// The host performs protocol translation itself; what it cannot know is this
// provider's history hygiene, so the two rules that decide whether the backend
// accepts the request live here: orphan tool calls/results are dropped, and an
// assistant turn with tool calls carries `content: null` rather than `""`
// (`openai-compat.ts:154-164,203-209`, trap #24).
func handleRequestTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.RequestTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	return pluginapi.PayloadResponse{Body: normaliseChatPayload(request.Body)}, nil
}

// handleResponseTranslate is an identity transform: the upstream already speaks
// OpenAI Chat Completions, so there is nothing to convert
// (`raccoon-adapter.ts:5-8`).
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

// handleExecutorExecute serves a non-streaming completion by folding the
// upstream stream.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, errPrepare := decodeExecutorCall(h, raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errInfer := performInfer(h, request, credential)
	if errInfer != nil {
		return nil, errInfer
	}
	stream, errStream := runStream(response.Body)
	if errStream != nil {
		return nil, errStream
	}
	payload, errCompletion := stream.completion(request.Model)
	if errCompletion != nil {
		return nil, errCompletion
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: canonicalHeader("Content-Type", "application/json"),
		Metadata: map[string]any{
			"provider": ProviderKey,
			"model":    request.Model,
		},
	}, nil
}

// handleExecutorExecuteStream serves a streaming completion.
//
// Frames are returned in the response envelope rather than pushed through
// `host.stream.emit`; both are valid ABI paths and this is the one the sibling
// plugins in this repository use.
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, errPrepare := decodeExecutorCall(h, raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errInfer := performInfer(h, request, credential)
	if errInfer != nil {
		return nil, errInfer
	}
	stream, errStream := runStream(response.Body)
	if errStream != nil {
		return nil, errStream
	}
	chunks, errChunks := stream.chunks()
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
// ⚠️ A credential that cannot be parsed is a 401, not a transport failure: the
// host must be able to retire it (`raccoon-adapter.ts:257-259`).
func decodeExecutorCall(h *abiboot.Host, raw json.RawMessage) (pluginapi.ExecutorRequest, *Credential, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return request, nil, errDecode
	}
	// The credential is renewed first when it is expired or about to expire: the
	// host's own refresh timer restarts with the process, so an expired token
	// would otherwise be signed into a request the gateway is bound to reject.
	// The refresh-once-retry-once path inside performInfer stays as the last
	// line of defence.
	fresh, errFresh := ensureCredentialFresh(h, authrefresh.Request{
		Name:        request.AuthID,
		StorageJSON: request.StorageJSON,
		Attributes:  request.AuthAttributes,
	})
	if errFresh != nil {
		return request, nil, errFresh
	}
	credential, errCredential := ParseCredential(fresh.Storage)
	if errCredential != nil {
		return request, nil, credentialError("missing_credential", "raccoon: no usable credential; log in first")
	}
	return request, credential, nil
}

// performInfer runs one upstream chat completion.
//
// A locally expired credential is refreshed FIRST, once
// (`raccoon-adapter.ts:251-259`); a 401/403 refreshes and retries ONCE
// (`raccoon-adapter.ts:331-341`). The refreshed credential cannot be written back
// from here — the host owns persistence and will pick it up on its own refresh
// schedule — so it is used for this request only.
func performInfer(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential) (*pluginapi.HTTPResponse, error) {
	cfg := settings()
	body, errBody := chatRequestBody(request)
	if errBody != nil {
		return nil, errBody
	}
	active := credential
	if active.NeedsRefresh(nowTime(), refreshWindow(cfg)) && active.Refreshable() {
		if refreshed, errRefresh := refreshCredential(h, cfg, active); errRefresh == nil {
			active = refreshed
		}
	}
	response, errDo := hostRequest(h, http.MethodPost, APIBase+ChatCompletionsPath, chatHeaders(active), body)
	if errDo != nil {
		return nil, transportError("infer_transport", "raccoon: transport error: %v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		// Refresh ONCE and retry ONCE; a second failure is terminal for this
		// request.
		if !active.Refreshable() {
			return nil, credentialError("auth", "raccoon: credential expired and refresh failed (HTTP %d)%s", response.StatusCode, credentialAdvice())
		}
		refreshed, errRefresh := refreshCredential(h, cfg, active)
		if errRefresh != nil {
			return nil, credentialError("auth", "raccoon: credential expired and refresh failed: %v%s", errRefresh, credentialAdvice())
		}
		retry, errRetry := hostRequest(h, http.MethodPost, APIBase+ChatCompletionsPath, chatHeaders(refreshed), body)
		if errRetry != nil {
			return nil, transportError("infer_transport", "raccoon: transport error: %v", errRetry)
		}
		if retry.StatusCode < 200 || retry.StatusCode >= 300 {
			return nil, inferFailure(retry)
		}
		return retry, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, inferFailure(response)
	}
	return response, nil
}

// chatRequestBody rewrites the outbound body: history hygiene, a model when the
// translated payload carries none, `stream: true` and the thinking control
// (`buildBody`, `raccoon-adapter.ts:376-410`).
//
// ⚠️ `tools` stays at the TOP LEVEL of the body. Moving it anywhere else makes
// the model emit prose XML tool calls that the harness cannot parse (trap #19),
// so the field is never touched beyond the empty-array rule: an empty `tools`
// array is dropped rather than sent.
//
// ⚠️ The thinking control is nested (`extra_body.thinking.type`), which is a
// DIFFERENT dialect from the top-level `thinking` another gateway uses. Putting
// it at the top level is silently ignored by this server — it does not even
// reject an illegal value there (upstream `05873c1`).
func chatRequestBody(request pluginapi.ExecutorRequest) ([]byte, error) {
	if len(request.Payload) == 0 {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "raccoon: 收到空请求体")
	}
	body := normaliseChatPayload(request.Payload)
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "raccoon: 解码 chat-completions 请求失败：%v", errUnmarshal)
	}
	if _, present := root["model"]; !present && strings.TrimSpace(request.Model) != "" {
		root["model"] = request.Model
	}
	if tools, present := root["tools"].([]any); present && len(tools) == 0 {
		delete(root, "tools")
	}
	// Thinking control. `reasoning_effort` is deliberately NOT consulted: the
	// server accepts it but nothing observable changes (see `EffortOn`).
	effort := readReasoningEffort(root)
	if extra := thinkingExtraBody(effort); extra != nil {
		root["extra_body"] = extra
	}
	// ⚠️ The inert field is REMOVED rather than forwarded. This provider's
	// reference never writes it (`raccoon-adapter.ts:398-410`), and leaving a
	// field on the wire that looks like a thinking control but does nothing is
	// how a future reader concludes the control is broken.
	delete(root, "reasoning_effort")
	delete(root, "reasoning")
	// Always stream: the adapter hard-codes it (`raccoon-adapter.ts:277`).
	root["stream"] = true
	encoded, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return nil, statusError(false, "encode_request", http.StatusInternalServerError, "编码请求失败：%v", errMarshal)
	}
	// Image projection LAST, on the marshalled body: it is the one step that can
	// be skipped without changing the request's meaning, so a failure anywhere in
	// it costs nothing (`projectRequestImage`, `raccoon-adapter.ts:344-356`).
	return projectRequestImages(encoded), nil
}

// projectRequestImages fits every inline image into this product's budgets.
//
// The measured defect: two 2560×1600 screenshots inline to ≈3.9 MB each, so
// roughly four of them answered `HTTP_413: request body exceeds 10MB` and the
// session could not continue (upstream `7ed3466`, issue !IKITT9).
//
// ⚠️ Per-product budgets are deliberately different and must stay that way: this
// gateway limits the REQUEST BODY, so 512 KB is what lets ten images through,
// whereas a 1 MiB target would 413 again after ten (see `ImageMaxBytes`).
//
// A projection that cannot improve an image leaves it byte-identical, so the
// only observable effect here is on images that were actually over budget.
func projectRequestImages(body []byte) []byte {
	return imagebudget.ProjectChatPayload(body, ImageMaxBytes, nil)
}

// reasoningEffortFields are the payload keys the client's thinking choice may
// arrive under, highest priority first.
//
// ⚠️ Read from the callers, not guessed. Both the CPA host and the sibling
// adapters put the client's OpenAI chat-completions body in `Payload` verbatim,
// and a DSH client expresses its thinking level as the OpenAI top-level field:
//
//   - `plugins/cline/adapter.go:133` reads `reasoning_effort` out of exactly this
//     payload (`effort := readStringField(inbound, "reasoning_effort")`) — the
//     same field the reference passes through to the upstream
//     (`cline-adapter.ts:466-474`);
//   - `internal/jethub/openai/openai.go:60` declares it on the shared request
//     type with the JSON tag `reasoning_effort,omitempty`;
//   - the CPA host itself extracts it under the same name
//     (`setReasoningEffortMetadata`, `sdk/api/handlers/handlers.go:318`, key
//     `cliproxyexecutor.ReasoningEffortMetadataKey = "reasoning_effort"`).
//
// The DSH-native block form `reasoning:{effort:…}` is accepted as a fallback
// because the host's thinking applier is not invoked on the plugin executor path
// (only `internal/runtime/executor/**` calls it), so a client that used the
// native shape would otherwise silently keep the server default.
var reasoningEffortFields = []string{"reasoning_effort", "reasoning"}

// readReasoningEffort extracts the client's chosen thinking level from the
// outbound chat-completions body. An absent or unusable value yields "", which
// means "send nothing and keep the server default" (measured = thinking on).
func readReasoningEffort(root map[string]any) string {
	for _, field := range reasoningEffortFields {
		switch typed := root[field].(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		case map[string]any:
			if nested, okNested := typed["effort"].(string); okNested {
				if trimmed := strings.TrimSpace(nested); trimmed != "" {
					return trimmed
				}
			}
			if nested, okNested := typed["level"].(string); okNested {
				if trimmed := strings.TrimSpace(nested); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

// thinkingExtraBody maps a level id onto the request's `extra_body` value.
//
// `nil` means "send no `extra_body` field at all" — that is the case for an
// absent level, and it preserves the server's own default (measured to be the
// same as an explicit `enabled`, but one field fewer on the wire is steadier).
//
// An UNKNOWN level is treated as ON, never as off: thinking a bit too much costs
// quota, whereas silently switching thinking off makes the user think the model
// broke, with nothing in the UI to explain it (`raccoonThinkingExtraBody`,
// `raccoon-adapter.ts:58-97`).
func thinkingExtraBody(effort string) map[string]any {
	if effort == "" {
		return nil
	}
	thinkingType := ThinkingTypeEnabled
	if effort == EffortOff {
		thinkingType = ThinkingTypeDisabled
	}
	return map[string]any{"thinking": map[string]any{"type": thinkingType}}
}

// inferFailure classifies a non-2xx chat answer.
//
// The upstream status is normalised rather than passed through: the host uses it
// to decide whether the request was at fault, and an upstream 403 says nothing
// useful about that (`httpErrorCode`, `openai-compat.ts:295-301`).
func inferFailure(response *pluginapi.HTTPResponse) error {
	detail := envelopeErrorText(response.Body)
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return credentialError("auth", "raccoon: credential expired and refresh failed (HTTP %d)：%s%s",
			response.StatusCode, detail, credentialAdvice())
	case http.StatusPaymentRequired:
		return statusError(false, "quota_exhausted", http.StatusPaymentRequired, "Raccoon 积分不足：%s", detail)
	case http.StatusTooManyRequests:
		return statusError(true, "rate_limited", http.StatusTooManyRequests, "Raccoon 限流：%s", detail)
	case http.StatusBadRequest:
		return statusError(false, "invalid_request", http.StatusBadRequest, "Raccoon 拒绝请求（HTTP 400）：%s", detail)
	default:
		if response.StatusCode >= 500 {
			return transportError("upstream_error", "Raccoon 上游错误（HTTP %d）：%s", response.StatusCode, detail)
		}
		return transportError("upstream_error", "Raccoon 返回 HTTP %d：%s", response.StatusCode, detail)
	}
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
