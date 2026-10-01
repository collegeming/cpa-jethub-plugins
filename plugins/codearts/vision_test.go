package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const tinyImageURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// imageMessage builds a user message carrying one text part and one image part.
func imageMessage(text string) map[string]any {
	return map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": tinyImageURL}},
		},
	}
}

// textMessage builds a plain-text user message.
func textMessage(text string) map[string]any {
	return map[string]any{"role": "user", "content": text}
}

// bodyFor renders messages as a request payload for one model.
func bodyFor(t *testing.T, model string, messages ...map[string]any) []byte {
	t.Helper()
	encoded, errMarshal := json.Marshal(map[string]any{"model": model, "messages": messages})
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	return encoded
}

// wireMessages decodes the messages array out of a prepared wire body.
func wireMessages(t *testing.T, encoded []byte) []map[string]any {
	t.Helper()
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(encoded, &body); errUnmarshal != nil {
		t.Fatalf("decode wire body: %v", errUnmarshal)
	}
	return body.Messages
}

// flatWire renders the whole wire body as a string for absence assertions.
func flatWire(t *testing.T, encoded []byte) string {
	t.Helper()
	return string(encoded)
}

// TestImageInLatestMessageIsStripped covers the reported failure: a request
// whose newest turn carries an image reached the gateway and was refused whole
// with `InferHub.001001020.406: The request model is not multimodal`.
func TestImageInLatestMessageIsStripped(t *testing.T) {
	encoded, _, stripped, errPrepare := prepareRequestBody(
		bodyFor(t, "deepseek-v4.1-flash", textMessage("hello"), imageMessage("what colour is this?")),
		"", DefaultConfig(), "sess-vision-1")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}
	if stripped != 1 {
		t.Fatalf("stripped = %d, want 1", stripped)
	}
	if wire := flatWire(t, encoded); strings.Contains(wire, "image_url") || strings.Contains(wire, "base64") {
		t.Fatalf("wire body still carries the image: %s", wire)
	}
	if wire := flatWire(t, encoded); !strings.Contains(wire, "[image omitted") {
		t.Fatalf("dropped image left no placeholder: %s", wire)
	}
}

// TestImageOnlyInHistoryIsStripped is the case that made the failure look
// inexplicable: the newest turn is plain text, so nothing about it suggests an
// image is involved. Measured 2026-10-01, this shape failed 5 times in 12.
//
// Reverse-verified: removing the stripImagesForTextModel call turns this red,
// because the image then survives into the wire body.
func TestImageOnlyInHistoryIsStripped(t *testing.T) {
	encoded, _, stripped, errPrepare := prepareRequestBody(
		bodyFor(t, "deepseek-v4.1-flash",
			imageMessage("what colour is this?"),
			map[string]any{"role": "assistant", "content": "It is a light blue square."},
			textMessage("now just say hello in one word")),
		"", DefaultConfig(), "sess-vision-2")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}
	if stripped != 1 {
		t.Fatalf("stripped = %d, want 1", stripped)
	}
	if wire := flatWire(t, encoded); strings.Contains(wire, "image_url") {
		t.Fatalf("history image survived into the wire body: %s", wire)
	}
	// The surrounding conversation must be untouched: only the image part is
	// rewritten, so the assistant turn and the final question still reach the
	// gateway verbatim.
	wire := flatWire(t, encoded)
	for _, want := range []string{"It is a light blue square.", "now just say hello in one word"} {
		if !strings.Contains(wire, want) {
			t.Fatalf("wire body lost %q: %s", want, wire)
		}
	}
}

// TestImageFreeRequestIsByteIdentical pins the cost of the projection: a request
// that never had an image must not be rewritten at all, so the strip cannot
// perturb the ordinary path.
func TestImageFreeRequestIsByteIdentical(t *testing.T) {
	payload := bodyFor(t, "deepseek-v4.1-flash", textMessage("hello"), textMessage("again"))
	encoded, _, stripped, errPrepare := prepareRequestBody(payload, "", DefaultConfig(), "sess-vision-3")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}
	if stripped != 0 {
		t.Fatalf("stripped = %d for an image-free request, want 0", stripped)
	}
	if strings.Contains(flatWire(t, encoded), imagePlaceholder) {
		t.Fatal("an image-free request gained a placeholder")
	}
}

// TestStripCountsEveryImageAndEveryShape covers the part shapes that can carry
// an image and the count used for the warning log.
func TestStripCountsEveryImageAndEveryShape(t *testing.T) {
	payload := bodyFor(t, "glm-5.2",
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": tinyImageURL}},
			// The flat shape some clients emit instead of the nested one.
			map[string]any{"type": "image", "url": tinyImageURL},
			// The Responses-style spelling.
			map[string]any{"type": "input_image", "image_url": tinyImageURL},
			map[string]any{"type": "text", "text": "and this text stays"},
		}})
	encoded, _, stripped, errPrepare := prepareRequestBody(payload, "", DefaultConfig(), "sess-vision-4")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}
	if stripped != 3 {
		t.Fatalf("stripped = %d, want 3 (all three image spellings)", stripped)
	}
	wire := flatWire(t, encoded)
	if strings.Contains(wire, "image_url") || strings.Contains(wire, "base64") {
		t.Fatalf("an image survived: %s", wire)
	}
	if !strings.Contains(wire, "and this text stays") {
		t.Fatalf("sibling text part was lost: %s", wire)
	}
}

// TestImagesNestedInToolResultsAreStripped covers an image hiding one level
// down: a tool result nests its own content list, and the gateway rejects the
// whole request regardless of which message it sits in.
func TestImagesNestedInToolResultsAreStripped(t *testing.T) {
	payload := bodyFor(t, "glm-5.2", map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type": "tool-result",
				"content": []any{
					map[string]any{"type": "text", "text": "screenshot follows"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": tinyImageURL}},
				},
			},
		},
	})
	encoded, _, stripped, errPrepare := prepareRequestBody(payload, "", DefaultConfig(), "sess-vision-5")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}
	if stripped != 1 {
		t.Fatalf("stripped = %d, want 1 (nested inside a tool result)", stripped)
	}
	wire := flatWire(t, encoded)
	if strings.Contains(wire, "image_url") {
		t.Fatalf("nested image survived: %s", wire)
	}
	if !strings.Contains(wire, "screenshot follows") {
		t.Fatalf("tool-result text was lost: %s", wire)
	}
}

// TestStrippedMessageBecomesTextParts checks the placeholder is a TEXT part: a
// content array must not be left empty, because an empty parts list is a
// different (and differently-rejected) payload shape.
func TestStrippedMessageBecomesTextParts(t *testing.T) {
	encoded, _, stripped, errPrepare := prepareRequestBody(
		bodyFor(t, "glm-5.2", imageMessage("look")),
		"", DefaultConfig(), "sess-vision-6")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}
	if stripped != 1 {
		t.Fatalf("stripped = %d, want 1", stripped)
	}
	messages := wireMessages(t, encoded)
	parts, okParts := messages[0]["content"].([]any)
	if !okParts {
		t.Fatalf("content = %#v, want a parts array", messages[0]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %#v, want the text part plus the placeholder", parts)
	}
	last, _ := parts[1].(map[string]any)
	if last["type"] != "text" || last["text"] != imagePlaceholder {
		t.Fatalf("placeholder part = %#v", last)
	}
}

// TestStripImagesForTextModelIgnoresNonPartContent pins that a plain string
// body and a malformed part list are passed through rather than guessed at.
func TestStripImagesForTextModelIgnoresNonPartContent(t *testing.T) {
	if got, count := stripImagesFromContent("just text"); count != 0 || got != "just text" {
		t.Fatalf("string content = %#v/%d, want it untouched", got, count)
	}
	// A nil element and an unknown type must both survive.
	content := []any{nil, map[string]any{"type": "text", "text": "keep"}, "bare"}
	got, count := stripImagesFromContent(content)
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
	if len(got.([]any)) != 3 {
		t.Fatalf("content = %#v, want all three elements retained", got)
	}
}
