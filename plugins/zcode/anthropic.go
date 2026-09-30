package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// Folding an Anthropic event stream into one message object.
//
// The free channel only streams, so the non-streaming executor route has to
// reconstruct what a single `POST /v1/messages` with `stream: false` would have
// returned. The reconstructed object is the standard Anthropic message:
//
//	{"id":…,"type":"message","role":"assistant","model":…,
//	 "content":[{"type":"thinking",…},{"type":"text",…},{"type":"tool_use",…}],
//	 "stop_reason":…,"stop_sequence":null,
//	 "usage":{"input_tokens":…,"output_tokens":…,…}}
//
// Two properties are load-bearing downstream:
//
//   - the host's own `ConvertClaudeResponseToOpenAINonStream` splits the body on
//     newlines and reads every `data:` line, so a single JSON object without SSE
//     framing is exactly the input it wants
//     (`internal/translator/claude/openai/chat-completions/claude_openai_response.go:340-349`);
//   - `thinking` blocks must keep their `signature`. A later turn replays them,
//     and a thinking block whose signature was dropped is what makes the next
//     request fail validation upstream.
//
// Usage is collected from BOTH places it appears: `message_start` carries
// `input_tokens` (plus the cache counters) and `message_delta` carries
// `output_tokens`. Reading only one of them necessarily loses a field.

// anthropicMessage is the assembled non-streaming message.
type anthropicMessage struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Model        string             `json:"model"`
	Content      []anthropicContent `json:"content"`
	StopReason   *string            `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        anthropicUsage     `json:"usage"`
}

// anthropicContent is one content block of the assembled message.
type anthropicContent struct {
	Type string `json:"type"`
	// Text carries both a `text` block's text and a `thinking` block's thinking.
	Text string `json:"text,omitempty"`
	// Thinking mirrors Text for a thinking block; upstream spells the member
	// differently per block type, so both are emitted.
	Thinking string `json:"thinking,omitempty"`
	// Signature is the thinking block's signature and MUST survive the fold.
	Signature string `json:"signature,omitempty"`
	// Tool-use members.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// anthropicUsage is the assembled usage block.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// foldAnthropicStream reconstructs one message from an SSE body.
func foldAnthropicStream(body []byte, model string) (*anthropicMessage, error) {
	frames := splitSSEFrames(body)
	if len(frames) == 0 {
		return nil, statusError(true, "empty_upstream", http.StatusBadGateway,
			"ZCode 返回了空响应体（上游 200 但没有任何内容）")
	}

	message := &anthropicMessage{
		Type: "message", Role: "assistant", Model: model,
		Content: []anthropicContent{},
	}
	// Blocks are keyed by their stream index; the order of first appearance is
	// preserved so interleaved thinking/text/tool blocks come back in the order
	// the model produced them.
	type partialBlock struct {
		kind      string
		text      strings.Builder
		signature string
		id        string
		name      string
		arguments strings.Builder
		// sawPartial marks that at least one `input_json_delta` arrived, so the
		// `input` member from `content_block_start` (which upstream sends as an
		// empty object) is not concatenated in front of the fragments.
		sawPartial bool
	}
	order := make([]int, 0, 4)
	blocks := map[int]*partialBlock{}
	blockFor := func(index int, kind string) *partialBlock {
		if existing, ok := blocks[index]; ok {
			if existing.kind == "" {
				existing.kind = kind
			}
			return existing
		}
		block := &partialBlock{kind: kind}
		blocks[index] = block
		order = append(order, index)
		return block
	}

	hasUsage := false
	hasContent := false
	for _, frame := range frames {
		event, data := parseSSEFrame(frame)
		if strings.TrimSpace(data) == "[DONE]" {
			break
		}
		if event == "error" || eventTypeOf(data) == "error" {
			return nil, anthropicErrorFrame(data)
		}
		var payload map[string]any
		if errUnmarshal := json.Unmarshal([]byte(strings.TrimSpace(data)), &payload); errUnmarshal != nil {
			// A frame that is not JSON (a comment, a keep-alive) is not content.
			continue
		}
		frameType := stringField(payload, "type")
		if frameType == "" {
			frameType = event
		}
		index, _ := nonNegativeInt(payload["index"])

		switch frameType {
		case "message_start":
			if messageBody, ok := payload["message"].(map[string]any); ok {
				if id := stringField(messageBody, "id"); id != "" {
					message.ID = id
				}
				if name := stringField(messageBody, "model"); name != "" {
					message.Model = name
				}
				if usage, ok := messageBody["usage"].(map[string]any); ok {
					mergeUsage(&message.Usage, usage)
					hasUsage = true
				}
			}

		case "content_block_start":
			blockBody, _ := payload["content_block"].(map[string]any)
			kind := stringField(blockBody, "type")
			block := blockFor(index, kind)
			switch kind {
			case "tool_use":
				block.id = stringField(blockBody, "id")
				block.name = stringField(blockBody, "name")
				if input, ok := blockBody["input"]; ok {
					if encoded, errMarshal := json.Marshal(input); errMarshal == nil {
						block.arguments.Write(encoded)
					}
				}
			case "thinking", "redacted_thinking":
				block.signature = stringField(blockBody, "signature")
				if text := stringField(blockBody, "thinking"); text != "" {
					block.text.WriteString(text)
				}
			default:
				if text := stringField(blockBody, "text"); text != "" {
					block.text.WriteString(text)
				}
			}

		case "content_block_delta":
			delta, _ := payload["delta"].(map[string]any)
			block := blockFor(index, "")
			switch stringField(delta, "type") {
			case "text_delta":
				if block.kind == "" {
					block.kind = "text"
				}
				block.text.WriteString(stringField(delta, "text"))
			case "thinking_delta":
				if block.kind == "" {
					block.kind = "thinking"
				}
				block.text.WriteString(stringField(delta, "thinking"))
			case "signature_delta":
				// ⚠ The signature may ONLY arrive here, so appending it (rather
				// than assigning) matters when the server splits it across deltas.
				block.signature += stringField(delta, "signature")
			case "input_json_delta":
				if block.kind == "" {
					block.kind = "tool_use"
				}
				// ⚠ The FIRST fragment REPLACES whatever `content_block_start`
				// carried. Upstream sends `input: {}` there, and concatenating the
				// fragments after it produces `{}{"path":…}` — invalid JSON, which
				// would silently discard the whole tool call.
				if !block.sawPartial {
					block.sawPartial = true
					block.arguments.Reset()
				}
				block.arguments.WriteString(stringField(delta, "partial_json"))
			}

		case "message_delta":
			if usage, ok := payload["usage"].(map[string]any); ok {
				mergeUsage(&message.Usage, usage)
				hasUsage = true
			}
			if delta, ok := payload["delta"].(map[string]any); ok {
				if reason := stringField(delta, "stop_reason"); reason != "" {
					message.StopReason = &reason
				}
			}
		}
	}

	for _, index := range order {
		block := blocks[index]
		switch block.kind {
		case "tool_use":
			if block.name == "" {
				continue
			}
			arguments := block.arguments.String()
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			if !json.Valid([]byte(arguments)) {
				// A truncated argument blob is passed through as an empty object
				// rather than invented: the tool-call contract is that a malformed
				// payload surfaces as a schema error the caller can retry.
				arguments = "{}"
			}
			message.Content = append(message.Content, anthropicContent{
				Type: "tool_use", ID: block.id, Name: block.name, Input: json.RawMessage(arguments),
			})
			hasContent = true
		case "thinking", "redacted_thinking":
			text := block.text.String()
			if strings.TrimSpace(text) == "" {
				continue
			}
			message.Content = append(message.Content, anthropicContent{
				Type: block.kind, Thinking: text, Signature: block.signature,
			})
			hasContent = true
		default:
			text := block.text.String()
			if text == "" {
				continue
			}
			message.Content = append(message.Content, anthropicContent{Type: "text", Text: text})
			hasContent = true
		}
	}

	// ⚠ An empty stream is reported, not returned. A 200 with no content blocks
	// is how "this account has no entitlement for this model" presents itself,
	// and handing the caller an empty message turns that into a silent
	// non-answer.
	if !hasContent {
		return nil, statusError(false, "quota_exhausted", http.StatusPaymentRequired,
			"ZCode 账号在模型 %q 上没有可用权益：上游返回 200 但没有任何内容块"+
				"（额度已用尽或该模型未对本账号开放）", model)
	}
	if !hasUsage {
		// The host's non-stream converter tolerates a missing usage block, so a
		// zeroed one is emitted rather than omitting the member entirely.
		message.Usage = anthropicUsage{}
	}
	if message.ID == "" {
		message.ID = "msg_zcode"
	}
	return message, nil
}

// mergeUsage folds one usage object into the accumulated usage.
//
// ⚠ Every member is merged with "keep the previous value when the incoming one is
// absent", never assigned outright: the two events carry DIFFERENT members, so a
// plain assignment lets the later `message_delta` (which has no `input_tokens`)
// wipe out the input count that `message_start` supplied.
func mergeUsage(target *anthropicUsage, source map[string]any) {
	if value, ok := nonNegativeInt(source["input_tokens"]); ok {
		target.InputTokens = value
	}
	if value, ok := nonNegativeInt(source["output_tokens"]); ok {
		target.OutputTokens = value
	}
	if value, ok := nonNegativeInt(source["cache_creation_input_tokens"]); ok {
		target.CacheCreationInputTokens = value
	}
	if value, ok := nonNegativeInt(source["cache_read_input_tokens"]); ok {
		target.CacheReadInputTokens = value
	}
}

// marshalCompact renders the folded message in one line, which is the shape the
// host's non-stream converter expects (it splits on newlines).
func marshalCompact(message *anthropicMessage) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(message); errEncode != nil {
		return nil, statusError(false, "encode_response", http.StatusInternalServerError,
			"编码 Anthropic 响应失败：%v", errEncode)
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}
