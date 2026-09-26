package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
// translated payload carries none, and `stream: true` (`buildBody`,
// `raccoon-adapter.ts:273-295`).
//
// ⚠️ `tools` stays at the TOP LEVEL of the body. Moving it anywhere else makes
// the model emit prose XML tool calls that the harness cannot parse (trap #19),
// so the field is never touched beyond the empty-array rule: an empty `tools`
// array is dropped rather than sent.
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
	// Always stream: the adapter hard-codes it (`raccoon-adapter.ts:277`).
	root["stream"] = true
	encoded, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return nil, statusError(false, "encode_request", http.StatusInternalServerError, "编码请求失败：%v", errMarshal)
	}
	return encoded, nil
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
