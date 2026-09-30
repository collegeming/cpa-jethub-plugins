package main

import (
	"encoding/json"
	"strings"
)

// Qoder encrypted-endpoint response envelope unwrapping.
//
// Ported from `src/qoder-envelope.ts`. The encrypted endpoint
// (`agent_chat_generation`) answers with SSE frames that carry one extra layer:
//
//	data:{"headers":{...},"body":"{\"choices\":[...]}","statusCodeValue":200,"statusCode":"OK"}
//	                                ^ this is the standard OpenAI frame, as a JSON string
//
// ⚠️ The inner `body` is NOT encrypted — only the REQUEST is signed/encrypted by
// the WASM (`qoder-envelope.ts:13-16`). So this is a JSON unwrap and nothing more.
//
// On failure the inner body is a business error (`[FAIL]node:... msg:...`) which
// MUST surface as an error instead of "no content", otherwise the stream stops
// silently (the reported "no reply at all" bug, `qoder-envelope.ts:18-22`).

// envelopeFrame is one wrapped frame (`QoderEnvelope`, `qoder-envelope.ts:26-31`).
type envelopeFrame struct {
	Headers         map[string]any  `json:"headers"`
	Body            json.RawMessage `json:"body"`
	StatusCodeValue int             `json:"statusCodeValue"`
	StatusCode      string          `json:"statusCode"`
}

// unwrapEnvelopePayload is `innerTextOf` (`qoder-envelope.ts:34-44`).
//
// ok is false when the payload is not an envelope (no `body` field), in which
// case the caller passes the frame through unchanged — the tolerance documented
// at `qoder-envelope.ts:101-103` for the day the server answers standard frames.
func unwrapEnvelopePayload(payload string) (string, bool) {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" || trimmed[0] != '{' {
		return "", false
	}
	var frame envelopeFrame
	if err := json.Unmarshal([]byte(trimmed), &frame); err != nil {
		return "", false
	}
	if len(frame.Body) == 0 {
		return "", false
	}
	// A string body is already the inner frame text; anything else (an object)
	// is re-serialised, exactly as `JSON.stringify(envelope.body)` does.
	var text string
	if err := json.Unmarshal(frame.Body, &text); err == nil {
		return text, true
	}
	return string(frame.Body), true
}

// envelopeErrorMessage converts a business-error frame into a readable message.
//
// `qoder-envelope.ts:106-121`: the inner body of a failed frame has no `choices`,
// so it is turned into a standard error frame. This is the Go equivalent of the
// `code`/`message` extraction plus the ` (code)` suffix.
//
// ⚠️ The extraction is RECURSIVE. The measured bodies nest the business code one
// JSON-STRING layer down —
// `{"code":403,"message":"{\"code\":\"10605\",…}"}` — and keeping only the
// top-level `code` reports the transport status as if it were the cause, while
// dropping the queue or quota code entirely (upstream `daf9fb1`, `2c1af59`).
func envelopeErrorMessage(inner string) string {
	// A plain text frame (`[FAIL]node:… msg:…`) has no layers to unwrap.
	if parsed, ok := parseBusinessError(inner); ok {
		return parsed.text()
	}
	return strings.TrimSpace(inner)
}

// looksLikeChatFrame reports whether an inner payload is a normal chat frame.
// `qoder-envelope.ts:108` uses the presence of `"choices"` (or `[DONE]`) as the
// discriminator between a success frame and a business-error frame.
func looksLikeChatFrame(inner string) bool {
	return strings.Contains(inner, `"choices"`) || strings.Contains(inner, "[DONE]")
}

// errorFrame builds the standard SSE error frame the executor turns into a
// failure, so every failure leaves through a single exit (`qoder-envelope.ts:107-120`).
func errorFrame(message string) []byte {
	encoded, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": message},
	})
	if err != nil {
		return []byte(`{"error":{"message":"qoder upstream error"}}`)
	}
	return encoded
}
