package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Inference: `executor.execute` and `executor.execute_stream`.
//
// The upstream is ALWAYS asked to stream (`stream: true`): the endpoint is an
// Anthropic Messages streaming endpoint and the reference hard-codes the flag
// (`minimax-messages.ts:buildMinimaxMessagesPayload`). The non-streaming method
// therefore folds the same stream; the folding is done by the HOST, not here —
// see the framing note below.
//
// ## ⚠️ The framing contract — why frames are emitted as TWO chunks
//
// This executor declares `anthropic` for both input and output, so the host
// treats the plugin's native protocol as Claude and translates it to whatever
// the client asked for. That yields two DIFFERENT consumers of the very same
// bytes, and they disagree about what a chunk must look like:
//
//   - an OpenAI client goes through `ConvertClaudeResponseToOpenAI`
//     (`internal/translator/claude/openai/chat-completions/claude_openai_response.go`),
//     which begins with
//     `if !bytes.HasPrefix(rawJSON, dataTag) { return [][]byte{} }`
//     where `dataTag` is `"data:"`. A chunk that STARTS with `event: ` is
//     therefore DISCARDED — silently, with no error anywhere;
//   - a Claude client has its chunks written verbatim by the host
//     (`ClaudeCodeAPIHandler.forwardClaudeStream` does `c.Writer.Write(chunk)`
//     with no framing of its own), so it needs real SSE: the `event:` name line
//     AND the blank line that terminates the event.
//
// Emitting the `event:` line inside the same chunk as the `data:` line would
// satisfy the second consumer and starve the first; emitting only `data:` would
// satisfy the first and hand the second an event with no name. Emitting the
// name line as its OWN chunk satisfies both: the OpenAI translator drops it
// (zero frames back, nothing forwarded), while the Claude writer emits it
// immediately ahead of its data line, reconstructing the exact upstream frames
// on the wire.
//
// ⚠️ This is also why the non-streaming method returns the same SSE text rather
// than one Anthropic `message` object: the host folds it with
// `ConvertClaudeResponseToOpenAINonStream`, which walks the payload looking for
// `data:` lines and produces an EMPTY completion from a plain JSON object. The
// tradeoff is stated plainly: a client speaking Claude that asks for a
// NON-streaming answer receives SSE text, because the plugin cannot see the
// client's requested format (`buildExecutorRequest` passes `outputFormat` in
// `Format`, and `outputFormat` is `claude` for every client protocol this
// executor serves).

// executorStreamResponse is the wire shape of executor.execute_stream.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// inferHeaders builds the inference request headers.
//
// ⚠️ MEASURED: ONLY these three headers are needed. In particular there is NO
// `anthropic-version` header — the reference recorded HTTP 200 across all four
// models with exactly this set (`minimax.ts:minimaxInferHeaders`), so adding
// the header Anthropic's own documentation asks for would be an unverified
// guess. `Accept: text/event-stream` (not `application/json`) matches the
// `stream: true` body.
func inferHeaders(credential *Credential) http.Header {
	return canonicalHeader(
		"Authorization", "Bearer "+credential.Session(),
		"Content-Type", "application/json",
		"Accept", "text/event-stream",
	)
}

// businessHeaders builds the header set for the catalogue, sign-in and credit
// endpoints. They need only the bearer token and a JSON accept
// (`minimax.ts:minimaxHeaders`); no machine identity or signature is involved.
func businessHeaders(credential *Credential) http.Header {
	return canonicalHeader(
		"Authorization", "Bearer "+credential.Session(),
		"Accept", "application/json",
	)
}

// handleExecutorIdentifier advertises the provider key this executor serves.
func handleExecutorIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleRequestTranslate is the plugin's slot in the host's request-translation
// chain.
//
// It returns the body unchanged, and that is the honest answer rather than a
// stub: every route this provider uses is served by the host's own native
// translators (the host converts the client's OpenAI body into Anthropic
// Messages before the executor is ever called, see the package comment on
// `messages.go`). The method exists so the provider presents the same complete
// method surface as its siblings.
func handleRequestTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.RequestTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	return pluginapi.PayloadResponse{Body: request.Body}, nil
}

// handleResponseTranslate is the response-side twin of
// handleRequestTranslate, and is an identity for the same reason.
func handleResponseTranslate(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ResponseTransformRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	return pluginapi.PayloadResponse{Body: request.Body}, nil
}

// handleExecutorCountTokens returns a coarse estimate.
//
// MiniMax's Anthropic-compatible endpoint may well expose a
// `/v1/messages/count_tokens` route, but NOTHING in the reference or in the
// measured probes calls one — inventing the path would be a guess that fails at
// runtime. CPA falls back to its own tokenizer when an executor does not
// implement counting, so a documented approximation is strictly better than an
// error.
func handleExecutorCountTokens(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	estimated := len(request.Payload)/4 + 1
	return pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":` + strconv.Itoa(estimated) + `}`)}, nil
}

// handleExecutorExecute serves a non-streaming completion by consuming the
// upstream stream and returning the SSE text the host folds.
func handleExecutorExecute(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, errPrepare := decodeExecutorCall(h, raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errInfer := performInferBuffered(h, request, credential)
	if errInfer != nil {
		return nil, errInfer
	}
	consumer := newSSEConsumer()
	if _, errConsume := consumer.consume(response.Body); errConsume != nil {
		return nil, errConsume
	}
	payload, errFinish := consumer.finish()
	if errFinish != nil {
		return nil, errFinish
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
// The frames travel in the response envelope rather than through
// `host.stream.emit`; both are valid ABI paths and this is the one every
// sibling plugin in this repository uses. It means the whole upstream stream is
// collected before the host starts forwarding it — the same behaviour the other
// providers here have, stated plainly rather than implied.
func handleExecutorExecuteStream(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, credential, errPrepare := decodeExecutorCall(h, raw)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, errInfer := performInferStreaming(h, request, credential)
	if errInfer != nil {
		return nil, errInfer
	}
	consumer := newSSEConsumer()
	if _, errConsume := consumer.consume(response.Body); errConsume != nil {
		return nil, errConsume
	}
	chunks, errFinish := consumer.finishFrames()
	if errFinish != nil {
		return nil, errFinish
	}
	return executorStreamResponse{
		Headers: canonicalHeader("Content-Type", "text/event-stream"),
		Chunks:  chunks,
	}, nil
}

// decodeExecutorCall decodes the request and the credential it is bound to.
//
// ⚠️ A credential that cannot be parsed is a 401, not a transport failure: the
// host must be able to retire it.
func decodeExecutorCall(h *abiboot.Host, raw json.RawMessage) (pluginapi.ExecutorRequest, *Credential, error) {
	request, errDecode := abiboot.Decode[pluginapi.ExecutorRequest](raw)
	if errDecode != nil {
		return request, nil, errDecode
	}
	// The credential is renewed first when it is expired or close to expiring:
	// the host's own refresh timer restarts with the process, so an expired
	// token would otherwise be signed into a request the vendor is bound to
	// reject. The refresh-once-retry-once path inside performInfer stays as the
	// last line of defence.
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
		return request, nil, credentialError("missing_credential", "minimax: 没有可用凭据，请先登录")
	}
	return request, credential, nil
}

// inferResponse is the response head and body the two executor paths share.
type inferResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// performInferBuffered runs inference through host.http.do. It is used by the
// non-streaming executor path and by deterministic unit tests.
func performInferBuffered(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential) (*inferResponse, error) {
	return performInferWith(h, request, credential, func(headers http.Header, body []byte) (*inferResponse, error) {
		response, errDo := hostRequest(h, http.MethodPost, APIHost+InferPath, headers, body)
		if errDo != nil {
			return nil, errDo
		}
		return &inferResponse{StatusCode: response.StatusCode, Headers: response.Headers, Body: response.Body}, nil
	})
}

// performInferStreaming runs inference through host.http.do_stream, reads every
// host-owned body chunk, and closes the stream bridge on every exit. The plugin
// never opens a network connection of its own.
func performInferStreaming(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential) (*inferResponse, error) {
	return performInferWith(h, request, credential, func(headers http.Header, body []byte) (*inferResponse, error) {
		if h == nil {
			return nil, transportError("host_unavailable", "插件未通过宿主调用（缺少 host 句柄）")
		}
		stream, errOpen := h.HTTPDoStream(abiboot.HTTPDoRequest{
			Method: http.MethodPost, URL: APIHost + InferPath, Headers: headers, Body: body,
		})
		if errOpen != nil {
			return nil, errOpen
		}
		defer func() { _ = stream.Close() }()
		response := &inferResponse{StatusCode: stream.StatusCode, Headers: stream.Headers}
		for {
			chunk, errRead := stream.Read()
			if errors.Is(errRead, io.EOF) {
				break
			}
			if errRead != nil {
				return nil, errRead
			}
			response.Body = append(response.Body, chunk...)
		}
		return response, nil
	})
}

// performInfer is the buffered package-level entry point used by focused tests.
func performInfer(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential) (*inferResponse, error) {
	return performInferBuffered(h, request, credential)
}

// inferDo is one host-mediated transport call.
type inferDo func(headers http.Header, body []byte) (*inferResponse, error)

// performInferWith owns the shared freshness, 401/403 refresh-and-retry and
// failure-classification policy. `do` is host.http.do or host.http.do_stream.
func performInferWith(h *abiboot.Host, request pluginapi.ExecutorRequest, credential *Credential, do inferDo) (*inferResponse, error) {
	cfg := settings()
	body, errBody := messagesRequestBody(request)
	if errBody != nil {
		return nil, errBody
	}
	active := credential
	if active.NeedsRefresh(nowTime(), refreshWindow(cfg)) && active.Refreshable() {
		if refreshed, errRefresh := refreshCredential(h, cfg, active); errRefresh == nil {
			active = refreshed
		}
	}
	response, errDo := do(inferHeaders(active), body)
	if errDo != nil {
		return nil, transportError("infer_transport", "minimax: 传输错误：%v", errDo)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		if !active.Refreshable() {
			return nil, credentialError("auth", "minimax: 凭据已失效且无法续期（HTTP %d）%s",
				response.StatusCode, credentialAdvice(response.StatusCode))
		}
		refreshed, errRefresh := refreshCredential(h, cfg, active)
		if errRefresh != nil {
			return nil, credentialError("auth", "minimax: 凭据已失效且续期失败：%v%s",
				errRefresh, credentialAdvice(response.StatusCode))
		}
		retry, errRetry := do(inferHeaders(refreshed), body)
		if errRetry != nil {
			return nil, transportError("infer_transport", "minimax: 传输错误：%v", errRetry)
		}
		if retry.StatusCode < 200 || retry.StatusCode >= 300 {
			return nil, inferFailureResponse(retry)
		}
		return retry, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, inferFailureResponse(response)
	}
	return response, nil
}

// messagesRequestBody prepares the outbound Anthropic Messages body.
//
// Two edits, and both are load-bearing:
//
//   - `stream` is forced to true, because the endpoint is streaming-first and
//     the reference never sends anything else;
//   - the adaptive-thinking rewrite runs, which is what keeps an
//     `MiniMax-M3.1*` request from hard-failing when the body arrives carrying
//     `thinking.type="disabled"` (the measured HTTP 400 is quoted in full on
//     `adaptiveOnlyPrefix`).
//
// The model is taken from the host-resolved `request.Model`, falling back to
// the body's own `model`, so a client-side alias cannot leak upstream.
func messagesRequestBody(request pluginapi.ExecutorRequest) ([]byte, error) {
	if errValidate := validateMessagesBody(request.Payload); errValidate != nil {
		return nil, errValidate
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(request.Payload, &root); errUnmarshal != nil {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest,
			"minimax: 解码 Anthropic 请求失败：%v", errUnmarshal)
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = stringField(root, "model")
	}
	if model == "" {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "minimax: 请求缺少 model 字段")
	}
	root["model"] = model
	root["stream"] = true
	// The current CPA SDK's OpenAI→Claude translator deliberately omits
	// temperature. The provider endpoint accepts it as a top-level Anthropic
	// field, so preserve it from the original client request when the translated
	// body does not already carry one.
	preserveOriginalTemperature(root, request.OriginalRequest)
	encoded, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return nil, statusError(false, "encode_request", http.StatusInternalServerError,
			"minimax: 编码请求失败：%v", errMarshal)
	}
	return rewriteAdaptiveThinking(model, encoded), nil
}

// preserveOriginalTemperature copies a numeric top-level temperature from the
// original client request into the translated Anthropic body when the host's
// translator omitted it. It never overwrites a value the host did set, and it
// ignores a malformed/non-numeric original rather than inventing one.
func preserveOriginalTemperature(root map[string]any, original []byte) {
	if _, present := root["temperature"]; present || len(original) == 0 {
		return
	}
	var source map[string]any
	if errUnmarshal := json.Unmarshal(original, &source); errUnmarshal != nil {
		return
	}
	if temperature, ok := numericValue(source["temperature"]); ok {
		root["temperature"] = temperature
	}
}

// inferFailure classifies a non-2xx inference answer.
//
// ⚠️ 402 is QUOTA_EXCEEDED and must be surfaced as itself. The reference calls
// this out explicitly: insufficient balance is the single most common real
// failure on this provider, and folding it into SERVER or AUTH hides the only
// action that helps the user, which is to top the account up.
func inferFailure(response *pluginapi.HTTPResponse) error {
	if response == nil {
		return transportError("upstream_error", "MiniMax 返回了空响应")
	}
	return inferFailureResponse(&inferResponse{StatusCode: response.StatusCode, Headers: response.Headers, Body: response.Body})
}

// inferFailureResponse is the transport-neutral failure classifier.
func inferFailureResponse(response *inferResponse) error {
	detail := inferErrorText(response)
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return credentialError("auth", "minimax: 凭据被拒绝（HTTP %d）：%s%s",
			response.StatusCode, detail, credentialAdvice(response.StatusCode))
	case http.StatusPaymentRequired:
		return statusError(false, "QUOTA_EXCEEDED", http.StatusPaymentRequired,
			"MiniMax 余额不足（HTTP 402）：%s", detail)
	case http.StatusTooManyRequests:
		return statusError(true, "rate_limited", http.StatusTooManyRequests, "MiniMax 限流：%s", detail)
	case http.StatusBadRequest:
		return statusError(false, "invalid_request", http.StatusBadRequest,
			"MiniMax 拒绝请求（HTTP 400）：%s", detail)
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return statusError(true, "upstream_timeout", response.StatusCode, "MiniMax 上游超时（HTTP %d）：%s",
			response.StatusCode, detail)
	default:
		if response.StatusCode >= 500 {
			return transportError("upstream_error", "MiniMax 上游错误（HTTP %d）：%s",
				response.StatusCode, detail)
		}
		return transportError("upstream_error", "MiniMax 返回 HTTP %d：%s", response.StatusCode, detail)
	}
}

// inferErrorText renders the vendor's error text, falling back to the raw body.
func inferErrorText(response *inferResponse) string {
	if message := upstreamErrorMessage(response.Body); message != "" {
		return message
	}
	if body := truncate(string(response.Body), 400); body != "" {
		return body
	}
	return http.StatusText(response.StatusCode)
}

// hostRequest performs one buffered upstream call through the host transport.
//
// Every outbound call goes through the host so proxy, TLS, header profiles and
// request logging stay under the host's control — the plugin never opens a
// socket of its own, which is why `net/http` appears in this package only for
// `http.Header`/`http.Method*` definitions.
func hostRequest(h *abiboot.Host, method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
	return hostRequestTimeout(h, 0, method, rawURL, headers, body)
}

// hostRequestTimeout is hostRequest with an explicit budget. A non-positive
// budget means "no plugin-side limit": the host transport applies its own, and
// inventing a wall-clock limit the reference does not have would be a deviation
// rather than a safeguard.
func hostRequestTimeout(h *abiboot.Host, budget time.Duration, method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
	if h == nil {
		return nil, transportError("host_unavailable", "插件未通过宿主调用（缺少 host 句柄）")
	}
	_ = budget
	return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: rawURL, Headers: headers, Body: body})
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
