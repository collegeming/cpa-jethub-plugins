package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// executorStreamResponse is the wire shape of executor.execute_stream. The chunks
// are returned in the reply envelope rather than pushed through a host stream
// callback: this ABI version gives the plugin no incremental emit path, so the
// trade-off is buffering the response in exchange for a single round trip (the
// same choice plugins/lobsterai/executor.go documents).
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// executorCredential resolves the account for one request and renews it first if
// it is due.
//
// The host's own refresh timer restarts with the process and every plugin deploy
// restarts the process, so a credential that expires inside such a window is
// never renewed by the host; the plugin renews it before use instead. This
// matters more here than for the sibling providers: an AtomGit refresh ROTATES
// the credential, so renewing late is not merely slower, it is the difference
// between a working account and a re-login.
func executorCredential(h *abiboot.Host, request pluginapi.ExecutorRequest) (*Credential, error) {
	fresh, errFresh := ensureCredentialFresh(h, authrefresh.Request{
		Name:        request.AuthID,
		StorageJSON: request.StorageJSON,
		Attributes:  request.AuthAttributes,
	})
	if errFresh != nil {
		return nil, errFresh
	}
	credential, errParse := ParseCredential(fresh.Storage)
	if errParse != nil {
		return nil, abiboot.HTTPError("invalid_credential", http.StatusUnauthorized, "%s", errParse.Error())
	}
	return credential, nil
}

// handleExecutorExecute serves a non-streaming completion.
//
// The upstream already answers in this dialect, so the body is returned as the
// gateway wrote it. The one thing that is checked is the gateway's habit of
// answering HTTP 200 with `content:"参数错误"` when it rejects the model or a
// parameter: passing that through would show the user a rejection message as if
// it were the model's answer.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := executorCredential(h, request)
	if errCredential != nil {
		return nil, errCredential
	}
	call, errPrepare := prepareGatewayCall(request, credential, settings(), false)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errGateway := sendGateway(h, call)
	if errGateway != nil {
		return nil, errGateway
	}
	if looksLikeParameterError(response.Body) {
		return nil, parameterErrorFor(call.Published)
	}
	if looksLikeSSE(response.Body) {
		// The format was forced to `stream:false` and the gateway streamed
		// anyway. Returning the frames as a completion would hand the client
		// something it cannot parse, so say so instead.
		return nil, abiboot.RetryableError("upstream_protocol",
			"AtomCode 网关在非流式请求上返回了 SSE 分片，请重试或改用流式")
	}
	return pluginapi.ExecutorResponse{
		Payload: response.Body,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Metadata: map[string]any{
			"model": call.Model,
		},
	}, nil
}

// handleExecutorExecuteStream serves a streaming completion.
//
// The upstream's own SSE frames become the host's chunk payloads: framing is
// stripped here and re-applied by the host, and the upstream's terminal
// `data: [DONE]` is dropped because the host writes its own.
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := executorCredential(h, request)
	if errCredential != nil {
		return nil, errCredential
	}
	call, errPrepare := prepareGatewayCall(request, credential, settings(), true)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errGateway := sendGateway(h, call)
	if errGateway != nil {
		return nil, errGateway
	}

	outcome, errStream := consumeUpstreamStream(response.Body)
	if errStream != nil {
		return nil, errStream
	}
	if len(outcome.Payloads) == 0 {
		// HTTP 200 with no frames at all: the stream ended before it started.
		// Retryable, because nothing has been shown to the user yet.
		return nil, abiboot.RetryableError("empty_upstream", "AtomCode 网关没有返回任何流式分片")
	}
	if !outcome.SawContent {
		// Frames arrived but none carried output — the gateway ended the stream
		// before producing anything. Still retryable for the same reason.
		return nil, abiboot.RetryableError("empty_upstream",
			"AtomCode 网关的流在首个内容分片之前就结束了")
	}

	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(outcome.Payloads))
	for _, payload := range outcome.Payloads {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: payload})
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
	size := len(request.Payload)
	if size == 0 {
		size = len(request.OriginalRequest)
	}
	return pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":` + itoa(size/4+1) + `}`)}, nil
}

// looksLikeSSE reports whether a body carries SSE framing rather than one JSON
// document.
func looksLikeSSE(body []byte) bool {
	text := strings.TrimSpace(string(body))
	return strings.HasPrefix(text, "data:") || strings.HasPrefix(text, "event:")
}
