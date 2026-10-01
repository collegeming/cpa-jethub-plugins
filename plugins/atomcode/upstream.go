package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The AtomCode gateway speaks OpenAI chat completions natively, so unlike every
// sibling provider in this repository this adapter does NOT translate between
// dialects: the client's body goes upstream almost verbatim and the upstream's
// frames come back almost verbatim. What is left to do is the part the format
// cannot express — identity, credential freshness, the model catalogue, and the
// gateway's two silent failure modes.

// gatewayCall is one prepared upstream request.
type gatewayCall struct {
	URL     string
	Headers http.Header
	Body    []byte
	// Model is the id sent to the gateway; Published is the name the caller
	// used. They differ wherever canonicalModelNames renames a model, and error
	// messages quote Published because that is the name the user can act on.
	Model     string
	Published string
}

// requestBody extracts the payload the executor must forward. CPA passes the
// translated provider payload in Payload; OriginalRequest is the fallback when no
// request translator ran.
func requestBody(request pluginapi.ExecutorRequest) []byte {
	if len(request.Payload) > 0 {
		return request.Payload
	}
	return request.OriginalRequest
}

// gatewayHeaders is the header set the gateway request carries.
//
// `AdapterUserAgent` is load-bearing, not cosmetic: an `atomcode/<version>`
// identity makes the gateway demand the closed-source signature and answer
// `403 ATOMCODE_SIG_MISSING` (see the table on OfficialUserAgent).
func gatewayHeaders(credential *Credential) http.Header {
	headers := http.Header{
		"Authorization": []string{"Bearer " + credential.AccessToken},
		"Content-Type":  []string{"application/json"},
		"Accept":        []string{"application/json"},
		"User-Agent":    []string{AdapterUserAgent},
	}
	return headers
}

// prepareGatewayCall rewrites the client's body for one upstream call.
//
// Only three members are touched, and each for a reason the format forces:
//
//   - `model` is stripped of the account prefix this adapter adds when
//     `model_prefix` is on (the prefix exists so several accounts can offer the
//     same model; the gateway has never heard of it);
//   - `stream` is forced to match the ABI method that was called, because the
//     host dispatches on the method and the body may disagree;
//   - `stream_options` is dropped on the buffered path, where there is no stream
//     to attach usage to.
//
// Everything else — messages, tools, tool_choice, temperature, max_tokens,
// reasoning_effort, image parts — is forwarded untouched. That is the whole
// advantage of a same-dialect upstream: no field can be lost in a translation
// that does not happen.
func prepareGatewayCall(request pluginapi.ExecutorRequest, credential *Credential, cfg Config, stream bool) (*gatewayCall, error) {
	raw := requestBody(request)
	if len(raw) == 0 {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "AtomCode 执行器没有收到请求体")
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
	// The caller selects the PUBLISHED name; the gateway must receive the id it
	// serves. Skipping this lookup silently sends `Qwen3.8-27B` upstream, which
	// the gateway answers with its `参数错误` sentinel rather than an error.
	published := stripModelPrefix(model, credential)
	upstreamModel := upstreamModelName(published)
	root["model"] = upstreamModel
	root["stream"] = stream
	if stream {
		// The official client sends `stream_options:{"include_usage":true}`
		// unconditionally (`crates/atomcode-capabilities/src/provider/openai_compat.rs`),
		// which is what makes the gateway emit the trailing usage frame. It is
		// only added when the caller did not choose for itself, so an explicit
		// client preference still wins.
		if _, present := root["stream_options"]; !present {
			root["stream_options"] = map[string]any{"include_usage": true}
		}
	} else {
		delete(root, "stream_options")
	}

	if messages, ok := root["messages"].([]any); !ok || len(messages) == 0 {
		return nil, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "请求缺少 messages")
	}

	body, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_request", "序列化 AtomCode 请求失败: %v", errMarshal)
	}
	return &gatewayCall{
		URL:       cfg.gatewayChatURL(),
		Headers:   gatewayHeaders(credential),
		Body:      body,
		Model:     upstreamModel,
		Published: published,
	}, nil
}

// sendGateway posts a prepared request and maps upstream failures onto plugin
// errors.
func sendGateway(h *abiboot.Host, call *gatewayCall) (*pluginapi.HTTPResponse, error) {
	response, errDo := hostDo(h, http.MethodPost, call.URL, call.Headers, call.Body)
	if errDo != nil {
		return nil, abiboot.RetryableError("transport", "AtomCode 网关请求失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, upstreamError(response.StatusCode, string(response.Body))
	}
	return response, nil
}

// streamOutcome is the result of consuming one upstream SSE body.
type streamOutcome struct {
	// Payloads are the outbound chunk payloads in order, already sanitized and
	// without SSE framing.
	Payloads [][]byte
	// SawContent reports whether any frame carried model output. A stream that
	// ended before its first content frame is retryable; one that produced
	// output and then stopped is not.
	SawContent bool
}

// consumeUpstreamStream turns an upstream SSE body into host chunk payloads.
//
// Three upstream conventions are normalised here:
//
//   - the terminal `data: [DONE]` is DROPPED. The host writes its own `[DONE]`
//     after the last plugin chunk, so forwarding this one would emit it twice
//     (`internal/jethub/sse/sse.go:68`).
//   - every payload goes through `sse.Payload`, which strips a `tool_calls: []`
//     member that some gateways put on reasoning frames. That empty array is not
//     harmless: it terminates the active reasoning segment in the Vercel AI SDK's
//     openai-compatible provider, which is what renders as one reasoning block
//     per token (`internal/jethub/sse/sanitize.go:17-45`).
//   - the gateway's silent `参数错误` rejection is turned into a real error
//     instead of being streamed to the user as if the model had said it.
func consumeUpstreamStream(body []byte) (streamOutcome, error) {
	var outcome streamOutcome
	scanner := &sse.Scanner{}
	for _, payload := range scanner.Feed(body) {
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" || trimmed == sse.Done {
			continue
		}
		if errSilent, failed := silentFrameFailure(trimmed); failed {
			return streamOutcome{}, errSilent
		}
		chunk := sse.Payload(trimmed)
		outcome.Payloads = append(outcome.Payloads, chunk)
		if frameCarriesOutput(chunk) {
			outcome.SawContent = true
		}
	}
	return outcome, nil
}

// silentFrameFailure reports whether one stream frame carries a silent failure
// rather than a model chunk.
func silentFrameFailure(payload string) (error, bool) {
	var decoded struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &decoded); errUnmarshal != nil {
		return nil, false
	}
	if len(decoded.Choices) != 1 {
		return nil, false
	}
	content := decoded.Choices[0].Delta.Content
	if content == "" {
		content = decoded.Choices[0].Message.Content
	}
	return silentUpstreamFailure(content)
}

// frameCarriesOutput reports whether a chunk holds anything the user will see.
func frameCarriesOutput(chunk []byte) bool {
	var decoded struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []any  `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal(chunk, &decoded); errUnmarshal != nil {
		return false
	}
	for _, choice := range decoded.Choices {
		if choice.Delta.Content != "" || choice.Delta.ReasoningContent != "" || len(choice.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// parameterErrorFor renders the gateway's silent rejection as a real plugin error.
//
// It is a 400 rather than a 502: the request was well-formed HTTP and the
// gateway answered 200, but the model or a parameter inside the body was
// rejected. Reporting the upstream status would tell the client nothing.
func parameterErrorFor(model string) error {
	subject := "该请求"
	if model != "" {
		subject = "模型「" + model + "」"
	}
	return abiboot.HTTPError("invalid_request", http.StatusBadRequest,
		"AtomCode 网关拒绝了%s：返回成功状态但正文是「%s」。通常表示模型名不在该账号的套餐内，或请求参数不被接受",
		subject, parameterErrorMessage)
}

// readStringField reads the first present string member among keys.
func readStringField(source map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := source[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
