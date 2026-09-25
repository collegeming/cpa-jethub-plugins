package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestUnwrapEnvelopePayloadStripsOneLayer pins the shape of the encrypted
// endpoint's frames (`qoder-envelope.ts:7-11`).
func TestUnwrapEnvelopePayloadStripsOneLayer(t *testing.T) {
	frame := `{"headers":{"Content-Type":["application/json"]},` +
		`"body":"{\"choices\":[{\"delta\":{\"content\":\"Q\"}}]}","statusCodeValue":200,"statusCode":"OK"}`
	inner, ok := unwrapEnvelopePayload(frame)
	if !ok {
		t.Fatal("a frame with a body field must be recognised as an envelope")
	}
	if inner != `{"choices":[{"delta":{"content":"Q"}}]}` {
		t.Fatalf("inner = %q", inner)
	}
	if !looksLikeChatFrame(inner) {
		t.Fatal("the inner frame must be recognised as a chat frame")
	}
}

// TestUnwrapEnvelopePayloadPassesThroughStandardFrames keeps the tolerance for a
// server that answers plain OpenAI frames (`qoder-envelope.ts:101-103`).
func TestUnwrapEnvelopePayloadPassesThroughStandardFrames(t *testing.T) {
	for _, payload := range []string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"error":{"message":"boom"}}`,
		`not json at all`,
		`[DONE]`,
	} {
		if _, ok := unwrapEnvelopePayload(payload); ok {
			t.Errorf("payload %q was treated as an envelope", payload)
		}
	}
}

// TestUnwrapEnvelopePayloadHandlesObjectBodies covers `JSON.stringify(body)` for
// a non-string body (`qoder-envelope.ts:43`).
func TestUnwrapEnvelopePayloadHandlesObjectBodies(t *testing.T) {
	inner, ok := unwrapEnvelopePayload(`{"body":{"choices":[]},"statusCodeValue":200}`)
	if !ok {
		t.Fatal("an object body must still be an envelope")
	}
	if !strings.Contains(inner, `"choices"`) {
		t.Fatalf("inner = %q, want the re-serialised object", inner)
	}
}

// TestEnvelopeErrorMessageCarriesTheCode matches the `message (code)` suffix
// (`qoder-envelope.ts:112-119`).
func TestEnvelopeErrorMessageCarriesTheCode(t *testing.T) {
	message := envelopeErrorMessage(`{"code":"invalid_model_error","message":"Unsupported model \"qfmodel\""}`)
	if !strings.Contains(message, "Unsupported model") || !strings.Contains(message, "invalid_model_error") {
		t.Fatalf("message = %q, want both the text and the code", message)
	}
	if plain := envelopeErrorMessage("[FAIL]node:1 msg:Execution failed"); !strings.Contains(plain, "Execution failed") {
		t.Fatalf("message = %q, want the raw text when it is not JSON", plain)
	}
}

// TestErrorFrameShape keeps the single error exit a valid OpenAI error frame.
func TestErrorFrameShape(t *testing.T) {
	var decoded struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(errorFrame("boom"), &decoded); errUnmarshal != nil {
		t.Fatalf("decode error frame: %v", errUnmarshal)
	}
	if decoded.Error.Message != "boom" {
		t.Fatalf("message = %q", decoded.Error.Message)
	}
}
