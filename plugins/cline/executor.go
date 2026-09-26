package main

import (
	"encoding/json"
	"net/http"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Executor surface, ported from `src/cline-adapter.ts` §5.
//
// The upstream has ONE inference endpoint, `POST {APIBase}/api/v1/chat/completions`,
// and it is SSE-only: there is no non-streaming path (`cline-adapter.ts:174`), so
// the non-streaming route asks for a stream and folds it into one
// `chat.completion`.
//
// Credential handling follows `cline-adapter.ts:402-473` exactly:
//
//	resolve → expired? refresh → still unusable ⇒ MISSING_CREDENTIAL
//	send    → 403 region-restricted? ⇒ PERMISSION_DENIED, NO refresh attempt
//	        → other 401/403?       ⇒ refresh once, retry once, else AUTH
//
// The account rotation the TypeScript performs afterwards
// (`cline-adapter.ts:475-519`) is host-owned in CPA: the executor's job is to
// classify the failure honestly so the host's scheduler can rotate. That is why
// the quota markers below map onto 402/429 rather than onto a private pool.

// executorStreamResponse is the wire shape of executor.execute_stream.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// chatCall is one fully prepared upstream chat request.
type chatCall struct {
	URL        string
	Headers    http.Header
	Body       []byte
	Model      string
	Credential *Credential
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

// handleExecutorExecute serves a non-streaming completion. Cline only streams
// upstream, so the stream is folded into one chat.completion.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, cfg, errDecode := decodeExecutorCall(raw)
	if errDecode != nil {
		return nil, errDecode
	}
	d := transportFor(h)
	stream, used, model, errInfer := runChat(d, request, credential, cfg)
	if errInfer != nil {
		return nil, errInfer
	}
	completion, merged, errAggregate := stream.completion(model)
	if errAggregate != nil {
		return nil, errAggregate
	}
	payload, errMarshal := json.Marshal(completion)
	if errMarshal != nil {
		return nil, statusError(false, "encode_response", http.StatusInternalServerError, "encode completion: %v", errMarshal)
	}
	metadata := map[string]any{
		"provider":      ProviderKey,
		"model":         model,
		"finish_reason": merged.FinishReason,
		"account_id":    used.AccountID,
	}
	if stream.usage != nil {
		metadata["usage"] = stream.usage
	}
	return pluginapi.ExecutorResponse{
		Payload:  payload,
		Headers:  jsonResponseHeaders(),
		Metadata: metadata,
	}, nil
}

// handleExecutorExecuteStream serves a streaming completion.
//
// The frames are returned in the response envelope rather than pushed through
// host.stream.emit. Both are valid ABI paths and this is the one the other
// adapters in this repository use; the trade-off is that the upstream body is
// buffered, which is also why the SSE idle timeouts of `cline-adapter.ts:135-140`
// cannot be enforced here (the host owns the transport deadline).
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, cfg, errDecode := decodeExecutorCall(raw)
	if errDecode != nil {
		return nil, errDecode
	}
	stream, _, _, errInfer := runChat(transportFor(h), request, credential, cfg)
	if errInfer != nil {
		return nil, errInfer
	}
	chunks, errChunks := stream.streamChunks()
	if errChunks != nil {
		return nil, errChunks
	}
	return executorStreamResponse{Headers: sseResponseHeaders(), Chunks: chunks}, nil
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

// runChat resolves the credential, sends the request and decodes the stream. The
// resolved model name comes back because the request body may have supplied it
// rather than the executor request.
func runChat(d doer, request pluginapi.ExecutorRequest, credential *Credential, cfg Config) (*chatStream, *Credential, string, error) {
	usable, errUsable := usableCredential(d, credential)
	if errUsable != nil {
		return nil, nil, "", errUsable
	}
	call, errCall := prepareChatCall(request, usable, cfg)
	if errCall != nil {
		return nil, nil, "", errCall
	}
	response, used, errSend := executeChat(d, call)
	if errSend != nil {
		return nil, nil, "", errSend
	}
	stream, errParse := parseChatStream(response.Body)
	if errParse != nil {
		return nil, nil, "", errParse
	}
	return stream, used, call.Model, nil
}

// prepareChatCall builds the upstream request for one execution.
func prepareChatCall(request pluginapi.ExecutorRequest, credential *Credential, cfg Config) (*chatCall, error) {
	body, model, errBody := buildChatBody(request.Payload, request.Model, cfg)
	if errBody != nil {
		return nil, errBody
	}
	return &chatCall{
		URL:        APIBase + ChatPath,
		Headers:    chatHeaders(credential),
		Body:       body,
		Model:      model,
		Credential: credential,
	}, nil
}

// usableCredential resolves the credential for one execution, renewing it first
// when it is already expired (`cline-adapter.ts:402-410`).
//
// A credential with no expiry information is used as-is: the source treats
// "absent expire_time" as non-expired and lets a server 401 decide
// (`cline.ts:276-279`).
func usableCredential(d doer, credential *Credential) (*Credential, error) {
	if !credential.Expired(0) {
		return credential, nil
	}
	if !credential.Refreshable() {
		return nil, credentialError("missing_credential", "Cline 凭据已过期且没有 refresh_token，请重新登录")
	}
	refreshed, errRefresh := refreshCredential(d, credential)
	if errRefresh != nil {
		return nil, credentialError("missing_credential", "Cline 凭据已过期且续期失败：%v", errRefresh)
	}
	return refreshed, nil
}

// executeChat performs one chat request and applies the failure rules of
// `cline-adapter.ts:457-473`.
func executeChat(d doer, call *chatCall) (*pluginapi.HTTPResponse, *Credential, error) {
	response, errDo := d(http.MethodPost, call.URL, call.Headers, call.Body)
	if errDo != nil {
		return nil, nil, transportError("transport", "Cline 请求失败：%v", errDo)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, call.Credential, nil
	}
	if isRegionForbidden(response.StatusCode, response.Body) {
		// Region-restricted 403 ⇒ PERMISSION_DENIED with the REAL message, and
		// the refresh is skipped entirely: it can never help, and burning it
		// wastes a round trip (`cline-adapter.ts:634-646`).
		return nil, nil, upstreamError(response)
	}
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		return nil, nil, upstreamError(response)
	}
	if !call.Credential.Refreshable() {
		return nil, nil, upstreamError(response)
	}
	refreshed, errRefresh := refreshCredential(d, call.Credential)
	if errRefresh != nil {
		// `cline: credential expired and refresh failed` (`cline-adapter.ts:470-473`).
		return nil, nil, credentialError("auth", "Cline 凭据已失效且续期失败：%v", errRefresh)
	}
	retry := &chatCall{
		URL:        call.URL,
		Headers:    chatHeaders(refreshed),
		Body:       call.Body,
		Model:      call.Model,
		Credential: refreshed,
	}
	retried, errRetry := d(http.MethodPost, retry.URL, retry.Headers, retry.Body)
	if errRetry != nil {
		return nil, nil, transportError("transport", "Cline 重试请求失败：%v", errRetry)
	}
	if retried.StatusCode < 200 || retried.StatusCode >= 300 {
		return nil, nil, upstreamError(retried)
	}
	return retried, refreshed, nil
}

// jsonResponseHeaders is the header set of a folded completion.
func jsonResponseHeaders() http.Header {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return header
}

// sseResponseHeaders is the header set of a streamed response.
func sseResponseHeaders() http.Header {
	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	return header
}
