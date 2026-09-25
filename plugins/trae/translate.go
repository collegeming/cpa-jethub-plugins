package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// This file is the OpenAI <-> SOLO translation core: the outbound payload
// conversion (trae.ts:1342-1506 + trae-adapter.ts:304-391) and the inbound SOLO
// SSE parser (trae.ts:1540-1713 + trae-adapter.ts:1052-1294).
//
// Two ordering rules from docs/agents/trae.md are load-bearing and are enforced
// by the call pipeline (`prepareChatCall`):
//
//  1. DSH-native content blocks must be serialized into OpenAI wire messages
//     *before* the SOLO payload conversion. Feeding `{type:'tool-call'}` blocks
//     to the converter produces no error but hides every tool call and tool
//     result from the model (docs/agents/trae.md:14-27).
//  2. `tools[].function.parameters` must be a JSON *string* on the wire, and the
//     serialization happens inside the converter, so the tools must already be
//     part of the source object (docs/agents/trae.md:33).
//
// The Go port tolerates both input shapes: a raw OpenAI wire request (what CPA
// hands an executor declaring `chat-completions`) and a request that already
// passed through `request.translate`. Serialization is idempotent for both.

// ── JSON field readers (trae.ts:1222-1250) ──

// readStringField returns a string field, accepting numbers as their decimal
// rendering (trae.ts:1222-1227).
func readStringField(source map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := source[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			return typed
		case float64:
			return trimFloat(typed)
		case json.Number:
			return typed.String()
		}
	}
	return ""
}

// readNumberField returns a numeric field, accepting numeric strings
// (trae.ts:1230-1235).
func readNumberField(source map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := source[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return typed, true
		case int:
			return float64(typed), true
		case int64:
			return float64(typed), true
		case json.Number:
			if parsed, err := typed.Float64(); err == nil {
				return parsed, true
			}
		case string:
			if parsed, err := parseNumeric(typed); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

// readBooleanField reports an *explicit* boolean. A missing field returns
// (false, false): "upstream said false" and "upstream said nothing" are
// different answers and the filters depend on that distinction
// (trae.ts:1244-1250).
func readBooleanField(source map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		value, ok := source[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed, true
		case float64:
			if typed == 1 {
				return true, true
			}
			if typed == 0 {
				return false, true
			}
		case string:
			switch typed {
			case "true":
				return true, true
			case "false":
				return false, true
			}
		}
	}
	return false, false
}

// asMap coerces a decoded JSON value into an object.
func asMap(value any) (map[string]any, bool) {
	typed, ok := value.(map[string]any)
	return typed, ok
}

// asSlice coerces a decoded JSON value into an array.
func asSlice(value any) ([]any, bool) {
	typed, ok := value.([]any)
	return typed, ok
}

// parseNumeric parses a numeric string; unlike strconv.ParseFloat it refuses
// trailing garbage and exponents, matching the reference regex
// (trae.ts:1233).
func parseNumeric(raw string) (float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("empty number")
	}
	negative := false
	if trimmed[0] == '-' {
		negative = true
		trimmed = trimmed[1:]
	}
	intPart, fracPart, hasFrac := strings.Cut(trimmed, ".")
	if intPart == "" || (hasFrac && fracPart == "") {
		return 0, fmt.Errorf("invalid number %q", raw)
	}
	for _, r := range intPart + fracPart {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid number %q", raw)
		}
	}
	var value float64
	if _, err := fmt.Sscanf(raw, "%g", &value); err != nil {
		return 0, err
	}
	if negative && value > 0 {
		value = -value
	}
	return value, nil
}

// trimFloat renders a float without a trailing ".0" the way JavaScript's
// String(number) does for integral values.
func trimFloat(value float64) string {
	rendered := fmt.Sprintf("%f", value)
	rendered = strings.TrimRight(rendered, "0")
	rendered = strings.TrimRight(rendered, ".")
	if rendered == "" || rendered == "-" {
		return "0"
	}
	return rendered
}

// deepCopyMap round-trips a decoded JSON object through JSON so callers can
// mutate it without aliasing the caller's value.
func deepCopyMap(source map[string]any) map[string]any {
	raw, err := json.Marshal(source)
	if err != nil {
		return source
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return source
	}
	return out
}

// ── message serialization (trae-adapter.ts:175-391) ──

// toolResultImageText is the carrier text emitted ahead of tool-result images
// (trae-adapter.ts:186).
const toolResultImageText = "Attached image(s) from tool result:"

// contentToText flattens a content payload to plain text: strings pass through,
// arrays contribute their `type:"text"` blocks (trae-adapter.ts:175-183).
func contentToText(content any) string {
	switch typed := content.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, raw := range typed {
			block, ok := asMap(raw)
			if !ok {
				continue
			}
			if readStringField(block, "type") != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok {
				builder.WriteString(text)
			} else if block["text"] != nil {
				builder.WriteString(fmt.Sprintf("%v", block["text"]))
			}
		}
		return builder.String()
	case map[string]any:
		// An already-normalized SOLO text block.
		if readStringField(typed, "type") == "text" {
			if text, ok := typed["text"].(string); ok {
				return text
			}
		}
		return ""
	default:
		return ""
	}
}

// blockHasImage reports whether one content block carries an image, in either
// the DSH-native (`{type:"image",attachment:{...}}`) or the OpenAI wire
// (`{type:"image_url",image_url:{url}}`) shape.
func blockHasImage(block map[string]any) bool {
	switch readStringField(block, "type") {
	case "image", "image_url", "input_image":
		return true
	}
	return false
}

// contentHasImage walks content blocks (including nested tool results) looking
// for an image.
func contentHasImage(content any) bool {
	blocks, ok := asSlice(content)
	if !ok {
		return false
	}
	for _, raw := range blocks {
		block, ok := asMap(raw)
		if !ok {
			continue
		}
		if blockHasImage(block) {
			return true
		}
		if readStringField(block, "type") == "tool-result" {
			if contentHasImage(block["content"]) {
				return true
			}
		}
	}
	return false
}

// HasImageContent reports whether any message in the request carries an image.
// Only `multimodal === true` models may receive such a request
// (docs/agents/trae.md:204-247).
func HasImageContent(messages []any) bool {
	for _, raw := range messages {
		message, ok := asMap(raw)
		if !ok {
			continue
		}
		if contentHasImage(message["content"]) {
			return true
		}
	}
	return false
}

// userContentParts converts content blocks into OpenAI multimodal parts
// (trae-adapter.ts:202-245).
//
// `imageURLs` maps an attachment id to a `data:` URL. A block that references an
// unresolvable attachment still produces a `[image unavailable]` placeholder
// instead of vanishing, and `image_url` blocks that are already wire-shaped pass
// through untouched (that is the only form SOLO accepts — no extra conversion).
//
// The return value is nil when there is no image at all; an array (possibly with
// only text parts) otherwise.
func userContentParts(content []any, imageURLs map[string]string) []any {
	parts := []any{}
	hasImage := false
	for _, raw := range content {
		block, ok := asMap(raw)
		if !ok {
			continue
		}
		switch readStringField(block, "type") {
		case "text":
			if text := contentToText(block); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
		case "image":
			hasImage = true
			url := ""
			if attachment, ok := asMap(block["attachment"]); ok {
				if id := readStringField(attachment, "attachmentId", "id"); id != "" {
					url = imageURLs[id]
				}
			}
			if url == "" {
				parts = append(parts, map[string]any{"type": "text", "text": "[image unavailable]"})
			} else {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case "image_url", "input_image":
			hasImage = true
			parts = append(parts, block)
		case "tool-result":
			inner, ok := asSlice(block["content"])
			if !ok {
				continue
			}
			if nested := userContentParts(inner, imageURLs); nested != nil {
				hasImage = true
				parts = append(parts, nested...)
			} else if text := contentToText(inner); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
		}
	}
	if !hasImage || len(parts) == 0 {
		return nil
	}
	return parts
}

// NormalizeToolArguments renders tool-call arguments as a legal JSON object
// literal (sse.ts:125-138). Empty or unparseable text degrades to `{}`; that
// keeps a no-argument tool working and stops a truncated stream from tearing
// down the whole conversation.
func NormalizeToolArguments(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return "{}"
	}
	if parsed == nil {
		return "{}"
	}
	if _, ok := parsed.([]any); ok {
		return "{}"
	}
	if _, ok := parsed.(map[string]any); !ok {
		return "{}"
	}
	return trimmed
}

// IsTruncatedArguments distinguishes "the tool takes no arguments" from "the
// argument fragments were lost in flight" (sse.ts:158-167).
func IsTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var parsed any
	return json.Unmarshal([]byte(trimmed), &parsed) != nil
}

// toolPairing is the set of tool-call ids that may be sent.
type toolPairing struct {
	keepCallIDs   map[string]bool
	keepResultIDs map[string]bool
}

// resolveToolPairing drops tool calls without results and results without calls
// (sse.ts:91-123). A batch of calls survives only when *every* call in that
// assistant message has a matching result; a partial batch leaves a call the
// upstream would reject.
//
// The Go port accepts both the DSH-native block shape and the OpenAI wire shape,
// because both can reach the executor depending on whether the host invoked
// `request.translate` first.
func resolveToolPairing(messages []map[string]any) toolPairing {
	allResultIDs := map[string]bool{}
	for _, message := range messages {
		for _, resultID := range toolResultIDs(message) {
			allResultIDs[resultID] = true
		}
	}

	keepCallIDs := map[string]bool{}
	for _, message := range messages {
		if readStringField(message, "role") != "assistant" {
			continue
		}
		callIDs := assistantCallIDs(message)
		if len(callIDs) == 0 {
			continue
		}
		complete := true
		for _, id := range callIDs {
			if !allResultIDs[id] {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		for _, id := range callIDs {
			keepCallIDs[id] = true
		}
	}

	keepResultIDs := map[string]bool{}
	for id := range keepCallIDs {
		if allResultIDs[id] {
			keepResultIDs[id] = true
		}
	}
	return toolPairing{keepCallIDs: keepCallIDs, keepResultIDs: keepResultIDs}
}

// assistantCallIDs lists the tool-call ids of one assistant message, covering
// both `content:[{type:"tool-call"}]` and a top-level `tool_calls` array.
func assistantCallIDs(message map[string]any) []string {
	ids := []string{}
	if blocks, ok := asSlice(message["content"]); ok {
		for _, raw := range blocks {
			block, ok := asMap(raw)
			if !ok || readStringField(block, "type") != "tool-call" {
				continue
			}
			if id := readStringField(block, "id", "toolCallId"); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if calls, ok := asSlice(message["tool_calls"]); ok {
		for _, raw := range calls {
			call, ok := asMap(raw)
			if !ok {
				continue
			}
			if id := readStringField(call, "id"); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// toolResultIDs lists the result ids referenced by one message, covering both
// the DSH-native (`content:[{type:"tool-result"}]`) and the OpenAI wire
// (`role:"tool"`, `tool_call_id`) shapes.
func toolResultIDs(message map[string]any) []string {
	ids := []string{}
	if blocks, ok := asSlice(message["content"]); ok {
		for _, raw := range blocks {
			block, ok := asMap(raw)
			if !ok || readStringField(block, "type") != "tool-result" {
				continue
			}
			if id := readStringField(block, "toolCallId", "tool_call_id"); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if readStringField(message, "role") == "tool" {
		if id := readStringField(message, "tool_call_id", "toolCallId"); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// wireToolCall normalizes one tool call to the OpenAI wire shape, accepting both
// `function` and SOLO's `function_call` as the source of the name/arguments.
func wireToolCall(call map[string]any) (map[string]any, bool) {
	source, ok := asMap(call["function"])
	if !ok {
		source, ok = asMap(call["function_call"])
	}
	if !ok {
		return nil, false
	}
	name := readStringField(source, "name")
	if strings.TrimSpace(name) == "" {
		return nil, false
	}
	out := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": NormalizeToolArguments(readStringField(source, "arguments")),
		},
	}
	if id := readStringField(call, "id"); id != "" {
		out["id"] = id
	}
	return out, true
}

// SerializeMessages converts DSH-native content blocks into OpenAI wire messages
// (trae-adapter.ts:304-391). It is the step whose absence silently blinds the
// model to its own tool calls and results.
//
// Output shape:
//   - assistant -> `{role, content: string|null, tool_calls?}`
//   - system    -> `{role:"system", content: string}`
//   - user      -> one text/multimodal message plus one `role:"tool"` message
//     per tool result
//
// `imageURLs` non-nil (even empty) enables multimodal parts; image blocks that
// reference attachments that cannot be resolved keep a placeholder instead of
// disappearing (trae-adapter.ts:296-302).
func SerializeMessages(messages []any, imageURLs map[string]string) []map[string]any {
	decoded := make([]map[string]any, 0, len(messages))
	for _, raw := range messages {
		if message, ok := asMap(raw); ok {
			decoded = append(decoded, message)
		}
	}

	pairing := resolveToolPairing(decoded)
	wire := make([]map[string]any, 0, len(decoded))
	pendingToolImages := []any{}
	flushToolImages := func() {
		if len(pendingToolImages) == 0 {
			return
		}
		content := append([]any{map[string]any{"type": "text", "text": toolResultImageText}}, pendingToolImages...)
		wire = append(wire, map[string]any{"role": "user", "content": content})
		pendingToolImages = []any{}
	}

	for _, message := range decoded {
		role := readStringField(message, "role")
		switch role {
		case "assistant":
			content := message["content"]
			toolCalls := []any{}
			// Native `content:[{type:"tool-call"}]` blocks.
			if blocks, ok := asSlice(content); ok {
				for _, raw := range blocks {
					block, ok := asMap(raw)
					if !ok || readStringField(block, "type") != "tool-call" {
						continue
					}
					id := readStringField(block, "id", "toolCallId")
					if id != "" && !pairing.keepCallIDs[id] {
						continue
					}
					call := map[string]any{
						"type": "function",
						"function": map[string]any{
							"name":      readStringField(block, "name"),
							"arguments": NormalizeToolArguments(readStringField(block, "arguments")),
						},
					}
					if id != "" {
						call["id"] = id
					}
					if readStringField(block, "name") == "" {
						continue
					}
					toolCalls = append(toolCalls, call)
				}
			}
			// Already-wire `tool_calls`.
			if raw, ok := asSlice(message["tool_calls"]); ok {
				for _, item := range raw {
					call, ok := asMap(item)
					if !ok {
						continue
					}
					id := readStringField(call, "id")
					if id != "" && !pairing.keepCallIDs[id] {
						continue
					}
					normalized, ok := wireToolCall(call)
					if !ok {
						continue
					}
					if id != "" {
						normalized["id"] = id
					}
					toolCalls = append(toolCalls, normalized)
				}
			}

			text := contentToText(content)
			out := map[string]any{"role": "assistant"}
			// A pure tool-call assistant message must not carry an empty string
			// content (trae-adapter.ts:342-347).
			if text == "" && len(toolCalls) > 0 {
				out["content"] = nil
			} else {
				out["content"] = text
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			if reasoning := readStringField(message, "reasoning_content"); reasoning != "" && text == "" && len(toolCalls) == 0 {
				// A reasoning-only assistant turn keeps its text so the model
				// still sees the previous thinking instead of an empty message.
				out["content"] = reasoning
			}
			wire = append(wire, out)

		case "system", "developer":
			wire = append(wire, map[string]any{"role": "system", "content": contentToText(message["content"])})

		case "tool":
			id := readStringField(message, "tool_call_id", "toolCallId")
			if id != "" && !pairing.keepResultIDs[id] {
				continue
			}
			out := map[string]any{"role": "tool", "content": contentToText(message["content"])}
			if out["content"] == "" {
				out["content"] = "(no output)"
			}
			if id != "" {
				out["tool_call_id"] = id
			}
			wire = append(wire, out)

		default: // "user" and anything else behaves like a user turn
			content := message["content"]
			blocks, _ := asSlice(content)

			// Tool results are expanded into standalone role:"tool" messages.
			order := []string{}
			for _, raw := range blocks {
				block, ok := asMap(raw)
				if !ok || readStringField(block, "type") != "tool-result" {
					continue
				}
				order = append(order, readStringField(block, "toolCallId", "tool_call_id"))
			}

			text := contentToText(content)
			parts := userContentParts(blocks, imageURLs)
			if parts != nil {
				flushToolImages()
				wire = append(wire, map[string]any{"role": "user", "content": parts})
			} else if text != "" || len(order) == 0 {
				wire = append(wire, map[string]any{"role": "user", "content": text})
			}

			for _, raw := range blocks {
				block, ok := asMap(raw)
				if !ok || readStringField(block, "type") != "tool-result" {
					continue
				}
				id := readStringField(block, "toolCallId", "tool_call_id")
				if id != "" && !pairing.keepResultIDs[id] {
					continue
				}
				inner, _ := asSlice(block["content"])
				var innerParts []any
				if imageURLs != nil {
					innerParts = userContentParts(inner, imageURLs)
				}
				if innerParts != nil {
					for _, part := range innerParts {
						if partMap, ok := asMap(part); ok && readStringField(partMap, "type") == "image_url" {
							pendingToolImages = append(pendingToolImages, part)
						}
					}
				}
				resultText := contentToText(block["content"])
				if resultText == "" {
					if innerParts != nil {
						resultText = toolResultImageText
					} else {
						resultText = "(no output)"
					}
				}
				out := map[string]any{"role": "tool", "content": resultText}
				if id != "" {
					out["tool_call_id"] = id
				}
				wire = append(wire, out)
			}
			flushToolImages()
		}
	}
	return wire
}

// ── OpenAI -> SOLO body conversion (trae.ts:1342-1506) ──

// MaxModeFields builds the wire fields that pin a session to Max mode (1M
// context), ported from trae.ts:1305-1322. The three size fields must travel
// together: upstream decides "this is a Max session" from `strategy=max` plus
// `model_auto_selection.strategy=max`, and without them it validates the input
// against the regular 200K window and rejects it (trae.ts:1294-1300).
//
// Only models whose remote config sets `display_config.max_mode === true` may
// receive these fields.
func MaxModeFields(maxContext int64, outputMax int64) map[string]any {
	context := maxContext
	if context <= 0 {
		context = MaxContextTokens
	}
	maxTokens := outputMax
	if maxTokens <= 0 {
		maxTokens = MaxOutputTokens
	}
	return map[string]any{
		"model_auto_selection": map[string]any{
			"strategy":                  "max",
			"fallback_to_advance_model": nil,
			"entitlement_id":            nil,
		},
		"model_selection_strategy": "max",
		"mode_type":                MaxModeType,
		"context_window_size":      context,
		"prompt_max_tokens":        MaxPromptTokens,
		"max_tokens":               maxTokens,
	}
}

// ClampMaxTokens bounds a requested output budget (trae.ts:1210-1217). A limit
// of 0 disables clamping; non-positive requests pass through untouched so no
// value is invented.
func ClampMaxTokens(value int64, limit int64) int64 {
	if value <= 0 || limit <= 0 {
		return value
	}
	if value > limit {
		return limit
	}
	return value
}

// TransformToSOLOBody converts an OpenAI request body into the SOLO body
// (trae.ts:1342-1374). `modelMapping` overrides the model/config_name;
// `channel` is the SOLO `function`, which must be the channel that listed the
// model (trae-adapter.ts:549-560).
func TransformToSOLOBody(body map[string]any, modelMapping, channel string) map[string]any {
	out := deepCopyMap(body)
	out["stream"] = true
	if strings.TrimSpace(channel) != "" {
		out["function"] = channel
	} else {
		out["function"] = DefaultFunction
	}

	if messages, ok := asSlice(out["messages"]); ok {
		converted := make([]any, 0, len(messages))
		for _, raw := range messages {
			message, ok := asMap(raw)
			if !ok {
				continue
			}
			converted = append(converted, transformSOLOMessage(message))
		}
		out["messages"] = converted
	}

	model := readStringField(out, "model")
	base := model
	if idx := strings.Index(model, "__"); idx >= 0 {
		base = model[:idx]
	}
	configName := strings.TrimSpace(modelMapping)
	if configName == "" {
		configName = base
	}
	if configName == "" {
		configName = DefaultConfigName
	}
	out["config_name"] = configName
	out["model"] = configName

	NormalizeToolChoice(out)
	NormalizeTools(out)
	return out
}

// transformSOLOMessage rewrites one message for SOLO (trae.ts:1379-1418).
func transformSOLOMessage(message map[string]any) map[string]any {
	out := deepCopyMap(message)

	if readStringField(out, "role") == "assistant" {
		if calls, ok := asSlice(out["tool_calls"]); ok {
			kept := []any{}
			for _, raw := range calls {
				call, ok := asMap(raw)
				if !ok {
					continue
				}
				if fn, ok := asMap(call["function"]); ok {
					call["function_call"] = fn
					delete(call, "function")
				}
				fn, ok := asMap(call["function_call"])
				if !ok {
					continue
				}
				// Upstream requires FunctionCall.Name, so nameless entries are
				// dropped (docs/agents/trae.md:32).
				if strings.TrimSpace(readStringField(fn, "name")) == "" {
					continue
				}
				kept = append(kept, call)
			}
			if len(kept) > 0 {
				out["tool_calls"] = kept
			} else {
				delete(out, "tool_calls")
			}
		}
	}

	switch content := out["content"].(type) {
	case nil:
		// A content-less message (for example a pure tool_calls assistant) is
		// left alone (trae.ts:1410-1411).
	case string:
		out["content"] = []any{map[string]any{"type": "text", "text": content}}
	default:
		// Arrays (multimodal parts, already-SOLO text blocks) pass through.
	}
	return out
}

// NormalizeToolChoice folds OpenAI's `tool_choice` into the string form SOLO
// accepts (trae.ts:1428-1476).
func NormalizeToolChoice(body map[string]any) {
	choice, ok := body["tool_choice"]
	if !ok {
		return
	}
	suppress := func() {
		delete(body, "tools")
		delete(body, "functions")
	}
	switch typed := choice.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(typed), "none") {
			delete(body, "tool_choice")
			suppress()
		}
	case map[string]any:
		switch strings.ToLower(strings.TrimSpace(readStringField(typed, "type"))) {
		case "none":
			delete(body, "tool_choice")
			suppress()
		case "auto", "required":
			body["tool_choice"] = strings.ToLower(strings.TrimSpace(readStringField(typed, "type")))
		case "function":
			name := ""
			if fn, ok := asMap(typed["function"]); ok {
				name = readStringField(fn, "name")
			}
			if name == "" {
				name = readStringField(typed, "name")
			}
			if strings.TrimSpace(name) != "" {
				body["tool_choice"] = strings.TrimSpace(name)
			} else {
				body["tool_choice"] = "auto"
			}
		default:
			delete(body, "tool_choice")
		}
	default:
		delete(body, "tool_choice")
	}
}

// NormalizeTools serializes `tools[].function.parameters` to a JSON string,
// which is what the SOLO upstream requires (trae.ts:1485-1506,
// docs/agents/trae.md:33).
func NormalizeTools(body map[string]any) {
	raw, ok := asSlice(body["tools"])
	if !ok || len(raw) == 0 {
		return
	}
	out := []any{}
	for _, item := range raw {
		tool, ok := asMap(item)
		if !ok {
			continue
		}
		fn, ok := asMap(tool["function"])
		if !ok {
			continue
		}
		switch params := fn["parameters"].(type) {
		case map[string]any, []any:
			encoded, err := json.Marshal(params)
			if err == nil {
				fn["parameters"] = string(encoded)
			}
		}
		out = append(out, tool)
	}
	if len(out) > 0 {
		body["tools"] = out
	} else {
		delete(body, "tools")
	}
}

// TrimTraeHistory trims wire messages to a character budget (trae.ts:1345-1380).
// Upstream silently ends the event stream above roughly 500K characters, with no
// error code (docs/agents/trae.md:477-492). Three constraints:
//
//  1. drop from the oldest non-system message;
//  2. never split a tool_call / tool pair — an assistant with `tool_calls` is
//     dropped together with the `tool` results that follow it;
//  3. system messages are never dropped, wherever they appear.
//
// It must run *after* message serialization, because it measures wire messages.
func TrimTraeHistory(messages []map[string]any, maxChars int) []map[string]any {
	if maxChars <= 0 {
		return messages
	}
	total := 0
	for _, message := range messages {
		total += wireMessageSize(message)
	}
	if total <= maxChars {
		return messages
	}

	drop := map[int]bool{}
	index := 0
	for index < len(messages) && total > maxChars {
		if readStringField(messages[index], "role") == "system" {
			index++
			continue
		}
		roundStart := index
		roundEnd := index + 1
		if calls, ok := asSlice(messages[index]["tool_calls"]); ok && len(calls) > 0 {
			for roundEnd < len(messages) && readStringField(messages[roundEnd], "role") == "tool" {
				roundEnd++
			}
		}
		for cursor := roundStart; cursor < roundEnd; cursor++ {
			drop[cursor] = true
			total -= wireMessageSize(messages[cursor])
		}
		index = roundEnd
	}

	out := make([]map[string]any, 0, len(messages))
	for position, message := range messages {
		if drop[position] {
			continue
		}
		out = append(out, message)
	}
	return out
}

// wireMessageSize estimates one wire message's character weight as its JSON
// length (trae.ts:1318-1324).
func wireMessageSize(message map[string]any) int {
	encoded, err := json.Marshal(message)
	if err != nil {
		return 0
	}
	return len(encoded)
}

// ── SOLO SSE parsing (trae.ts:1510-1713) ──

// SOLO event names (trae.ts:1511-1518).
const (
	soloEventMetadata   = "metadata"
	soloEventTimingCost = "timing_cost"
	soloEventOutput     = "output"
	soloEventExtraInfo  = "extra_info"
	soloEventTokenUsage = "token_usage"
	soloEventDone       = "done"
	soloEventError      = "error"
)

// SOLOEvent is one parsed SOLO SSE event (trae.ts:1521-1530).
type SOLOEvent struct {
	Event            string
	Response         string
	ReasoningContent string
	ToolCalls        []map[string]any
	Usage            map[string]any
	FinishReason     string
	ErrorCode        int
	ErrorMessage     string
	HasError         bool
}

// ParseSOLOLine parses one accumulated SOLO event (trae.ts:1540-1574). The event
// name is authoritative; a malformed JSON body degrades to a bare event rather
// than an error, matching the reference parser.
func ParseSOLOLine(eventName, dataLine string) (SOLOEvent, bool) {
	event := strings.TrimSpace(eventName)
	if event == "" {
		return SOLOEvent{}, false
	}
	out := SOLOEvent{Event: event}
	trimmed := strings.TrimSpace(dataLine)
	if trimmed == "" {
		return out, true
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return out, true
	}

	switch event {
	case soloEventOutput:
		if response, ok := raw["response"].(string); ok {
			out.Response = response
		}
		if reasoning, ok := raw["reasoning_content"].(string); ok {
			out.ReasoningContent = reasoning
		}
		if calls, ok := asSlice(raw["tool_calls"]); ok {
			out.ToolCalls = normalizeSOLOToolCalls(calls)
		}
	case soloEventTokenUsage:
		out.Usage = raw
	case soloEventDone:
		if finish, ok := raw["finish_reason"].(string); ok {
			out.FinishReason = finish
		}
	case soloEventError:
		// An `event:error` frame is an error even when it carries no code.
		out.HasError = true
		// The reference accepts a numeric code only; a numeric string is also
		// accepted here because some gateways return one.
		if code, ok := readNumberField(raw, "code"); ok {
			out.ErrorCode = int(code)
		}
		out.ErrorMessage = readStringField(raw, "message", "msg")
	}
	return out, true
}

// normalizeSOLOToolCalls rewrites SOLO's tool-call shape into the OpenAI shape:
// `function_call` -> `function`, and the SOLO-only `namespace` /
// `partial_arguments` fields are removed (trae.ts:1581-1598).
func normalizeSOLOToolCalls(calls []any) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, raw := range calls {
		call, ok := asMap(raw)
		if !ok {
			continue
		}
		normalized := map[string]any{}
		for key, value := range call {
			normalized[key] = value
		}
		if fn, ok := asMap(normalized["function_call"]); ok {
			function := map[string]any{}
			for key, value := range fn {
				function[key] = value
			}
			delete(function, "namespace")
			delete(function, "partial_arguments")
			normalized["function"] = function
			delete(normalized, "function_call")
		} else if fn, ok := asMap(normalized["function"]); ok {
			function := map[string]any{}
			for key, value := range fn {
				function[key] = value
			}
			delete(function, "namespace")
			delete(function, "partial_arguments")
			normalized["function"] = function
		}
		out = append(out, normalized)
	}
	return out
}

// SOLOScanner extracts SOLO events from a byte stream that may be split at
// arbitrary boundaries. It mirrors the line loop of trae-adapter.ts:1108-1242:
// `event:` sets the name, `data:` accumulates (possibly across lines) and a
// blank line closes the event.
type SOLOScanner struct {
	buffer []byte
	event  string
	data   string
	active bool
}

// Feed appends upstream bytes and returns every complete event.
func (s *SOLOScanner) Feed(chunk []byte) []SOLOEvent {
	if len(chunk) > 0 {
		s.buffer = append(s.buffer, chunk...)
	}
	events := []SOLOEvent{}
	for {
		idx := bytes.IndexByte(s.buffer, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(s.buffer[:idx]), "\r")
		s.buffer = s.buffer[idx+1:]
		if event, ok := s.consumeLine(line); ok {
			events = append(events, event)
		}
	}
	return events
}

// Flush treats the end of the stream as an event terminator. It first consumes a
// final line that arrived without a trailing newline, then emits the event still
// pending. The reference stream loop discards both fragments; the aggregated
// parser is more forgiving because the body is already complete when it is
// parsed, and dropping the tail would lose the last content delta.
func (s *SOLOScanner) Flush() (SOLOEvent, bool) {
	if len(s.buffer) > 0 {
		line := strings.TrimRight(string(s.buffer), "\r")
		s.buffer = nil
		if event, ok := s.consumeLine(line); ok {
			return event, true
		}
	}
	if !s.active {
		return SOLOEvent{}, false
	}
	event, ok := ParseSOLOLine(s.event, s.data)
	s.event, s.data, s.active = "", "", false
	if !ok {
		return SOLOEvent{}, false
	}
	return event, true
}

// consumeLine handles one physical line, returning a completed event.
func (s *SOLOScanner) consumeLine(line string) (SOLOEvent, bool) {
	if strings.TrimSpace(line) == "" {
		if !s.active {
			return SOLOEvent{}, false
		}
		event, ok := ParseSOLOLine(s.event, s.data)
		s.event, s.data, s.active = "", "", false
		if !ok {
			return SOLOEvent{}, false
		}
		return event, true
	}
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(trimmed, "event:"):
		s.event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		s.active = true
	case strings.HasPrefix(trimmed, "data:"):
		// SOLO data may span several lines; both `data: {...}` and `data:{...}`
		// occur in the wild (docs/agents/trae.md:34).
		s.data += strings.TrimPrefix(trimmed, "data:")
		s.active = true
	}
	return SOLOEvent{}, false
}

// SOLOStreamError is an `event:error` frame surfaced as a Go error.
type SOLOStreamError struct {
	Code    int
	Message string
	Model   string
}

// Error renders the client-facing message. For 4001 an actionable hint is
// appended: the upstream text blames the request parameters, while the observed
// cause is a model the account cannot call (trae-adapter.ts:416-437).
func (e *SOLOStreamError) Error() string {
	message := e.Message
	if message == "" {
		message = "unknown error"
	}
	base := fmt.Sprintf("trae: %s (code=%d)", message, e.Code)
	if e.Code != 4001 {
		return base
	}
	return fmt.Sprintf("%s —— 模型「%s」不被上游接受：它通常是「仅可见但不可调用」的自定义模型"+
		"（需先在 TRAE IDE 内自行配置供应商），或该模型不属于当前请求的通道；请改用模型列表中的其它模型",
		base, e.Model)
}

// AggregateSOLO folds a complete SOLO body into one aggregated result
// (trae.ts:1657-1713).
type SOLOAggregate struct {
	Content          string
	ReasoningContent string
	ToolCalls        []map[string]any
	FinishReason     string
	Usage            map[string]any
	Error            *SOLOStreamError
	SawEvent         bool
}

// AggregateSOLO parses a whole SOLO SSE body. The second return value reports
// whether any event was seen at all, which is the retry signal for the upstream
// "HTTP 200 then no events" failure (docs/agents/trae.md:494-505).
func AggregateSOLO(body []byte, model string) (SOLOAggregate, bool) {
	result := SOLOAggregate{FinishReason: "stop"}
	scanner := &SOLOScanner{}
	events := scanner.Feed(body)
	if tail, ok := scanner.Flush(); ok {
		events = append(events, tail)
	}
	for _, event := range events {
		result.SawEvent = true
		switch event.Event {
		case soloEventOutput:
			result.Content += event.Response
			result.ReasoningContent += event.ReasoningContent
			result.ToolCalls = append(result.ToolCalls, event.ToolCalls...)
		case soloEventTokenUsage:
			result.Usage = event.Usage
		case soloEventDone:
			if event.FinishReason != "" {
				result.FinishReason = event.FinishReason
			}
		case soloEventError:
			result.Error = &SOLOStreamError{Code: event.ErrorCode, Message: event.ErrorMessage, Model: model}
		}
	}
	return result, result.SawEvent
}

// ── SOLO SSE -> OpenAI chunks (trae.ts:1605-1636, trae-adapter.ts:1127-1294) ──

// toolCallFields reads the name, arguments and id of one normalized tool call,
// accepting both the OpenAI `function` and SOLO's `function_call` key.
func toolCallFields(call map[string]any) (name, arguments, id string) {
	fn, ok := asMap(call["function"])
	if !ok {
		fn, _ = asMap(call["function_call"])
	}
	return readStringField(fn, "name"), readStringField(fn, "arguments"), readStringField(call, "id")
}

// openAIChunk is one `chat.completion.chunk` payload. The reference helper emits
// an empty model and no `finish_reason` unless one is supplied
// (trae.ts:1605-1633); this adapter always fills the requested model so clients
// see the model they asked for.
type openAIChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   map[string]any `json:"usage,omitempty"`
}

type openAIChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// openAIDone is the terminal SSE frame.
const openAIDone = "data: [DONE]\n\n"

// TranslateSOLOStream converts a SOLO SSE body into OpenAI SSE frames. It
// returns the frames, an error when an `event:error` frame was present, and
// whether any upstream event was seen at all.
//
// The `sawEvent` flag matters: upstream sometimes accepts the session, answers
// HTTP 200 and then ends the stream without a single event. That is a retryable
// transport failure, not an empty answer, and it may only be retried before the
// first event because a replay would re-bill and may re-run tools
// (docs/agents/trae.md:494-505).
func TranslateSOLOStream(body []byte, model, completionID string, created int64) ([][]byte, *SOLOStreamError, bool) {
	scanner := &SOLOScanner{}
	events := scanner.Feed(body)
	if tail, ok := scanner.Flush(); ok {
		events = append(events, tail)
	}

	sawEvent := false
	roleSent := false
	frames := [][]byte{}
	var usage map[string]any
	finishReason := ""
	toolCallCount := 0

	emit := func(delta map[string]any, finish *string) {
		if !roleSent && finish == nil {
			delta["role"] = "assistant"
			roleSent = true
		}
		chunk := openAIChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []openAIChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
		if encoded, err := json.Marshal(chunk); err == nil {
			frames = append(frames, []byte("data: "+string(encoded)+"\n\n"))
		}
	}

	for _, event := range events {
		sawEvent = true
		switch event.Event {
		case soloEventOutput:
			delta := map[string]any{}
			if event.Response != "" {
				delta["content"] = event.Response
			}
			if event.ReasoningContent != "" {
				delta["reasoning_content"] = event.ReasoningContent
			}
			if len(event.ToolCalls) > 0 {
				calls := make([]any, 0, len(event.ToolCalls))
				for position, call := range event.ToolCalls {
					index := position
					if raw, ok := readNumberField(call, "index"); ok {
						index = int(raw)
					}
					name, arguments, id := toolCallFields(call)
					entry := map[string]any{
						"index": index,
						"type":  "function",
						"function": map[string]any{
							"name":      name,
							"arguments": arguments,
						},
					}
					if id != "" {
						entry["id"] = id
					}
					calls = append(calls, entry)
					toolCallCount++
				}
				delta["tool_calls"] = calls
			}
			if len(delta) > 0 {
				emit(delta, nil)
			}
		case soloEventTokenUsage:
			usage = event.Usage
		case soloEventDone:
			if event.FinishReason != "" {
				finishReason = event.FinishReason
			}
		case soloEventError:
			return frames, &SOLOStreamError{Code: event.ErrorCode, Message: event.ErrorMessage, Model: model}, sawEvent
		}
	}

	if finishReason == "" {
		if toolCallCount > 0 {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}
	// The closing chunk always carries `finish_reason`; `usage` rides along when
	// the upstream sent a token_usage event.
	finalDelta := map[string]any{}
	if !roleSent && toolCallCount == 0 {
		finalDelta["role"] = "assistant"
		roleSent = true
	}
	reason := finishReason
	chunk := openAIChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openAIChoice{{Index: 0, Delta: finalDelta, FinishReason: &reason}},
		Usage:   openAIUsage(usage),
	}
	if encoded, err := json.Marshal(chunk); err == nil {
		frames = append(frames, []byte("data: "+string(encoded)+"\n\n"))
	}
	frames = append(frames, []byte(openAIDone))
	return frames, nil, sawEvent
}

// AggregateSOLOToCompletion folds a SOLO SSE body into one OpenAI
// `chat.completion` object for non-streaming requests.
func AggregateSOLOToCompletion(body []byte, model, completionID string, created int64) (map[string]any, *SOLOStreamError, bool) {
	aggregate, sawEvent := AggregateSOLO(body, model)
	toolCalls := []any{}
	for position, call := range aggregate.ToolCalls {
		name, arguments, id := toolCallFields(call)
		entry := map[string]any{
			"index": position,
			"type":  "function",
			"function": map[string]any{
				"name":      name,
				"arguments": NormalizeToolArguments(arguments),
			},
		}
		if name != "" {
			if id == "" {
				id = fmt.Sprintf("call_%d", position)
			}
			entry["id"] = id
		}
		toolCalls = append(toolCalls, entry)
	}

	finish := aggregate.FinishReason
	if finish == "" {
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	message := map[string]any{"role": "assistant", "content": aggregate.Content}
	if aggregate.ReasoningContent != "" {
		message["reasoning_content"] = aggregate.ReasoningContent
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	completion := map[string]any{
		"id":      completionID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage := openAIUsage(aggregate.Usage); usage != nil {
		completion["usage"] = usage
	}
	return completion, aggregate.Error, sawEvent
}

// openAIUsage maps the SOLO token_usage payload onto OpenAI's usage object
// (trae-adapter.ts:1195-1210).
func openAIUsage(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	promptTokens, _ := readNumberField(raw, "prompt_tokens")
	completionTokens, _ := readNumberField(raw, "completion_tokens")
	usage := map[string]any{
		"prompt_tokens":     int64(promptTokens),
		"completion_tokens": int64(completionTokens),
		"total_tokens":      int64(promptTokens + completionTokens),
	}
	if reasoning, ok := readNumberField(raw, "reasoning_tokens"); ok && reasoning > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": int64(reasoning)}
	}
	return usage
}
