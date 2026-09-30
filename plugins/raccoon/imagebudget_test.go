package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Image projection at the executor level (upstream `7ed3466`, issue !IKITT9).
//
// Measured: two 2560×1600 screenshots inline to ≈3.9 MB each, so roughly four of
// them made the gateway answer `HTTP_413: request body exceeds 10MB` and the
// session could not continue. The projection is what keeps the request inside
// that limit; the geometry itself is covered by the shared package's own tests,
// so what is pinned HERE is the wiring — the budget this product uses and the
// fact that the executor actually applies it.

// noisyScreenshot renders a large PNG the encoder cannot compress away, so the
// byte-budget assertions are not vacuous.
func noisyScreenshot(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x*7 + y*13),
				G: uint8(x*29 + y*3),
				B: uint8(x*11 ^ y*17),
				A: 0xff,
			})
		}
	}
	var buffer bytes.Buffer
	if errEncode := png.Encode(&buffer, img); errEncode != nil {
		t.Fatalf("encode fixture: %v", errEncode)
	}
	return buffer.Bytes()
}

// imagePayload renders a chat-completions request carrying one data-URL image.
func imagePayload(t *testing.T, dataURL string) string {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{
		"model": "sn-sensenova-6-8-flash",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "what is in this screenshot?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			},
		}},
	})
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	return string(payload)
}

// imageURLOf reads the image URL out of a built body.
func imageURLOf(t *testing.T, body []byte) string {
	t.Helper()
	var decoded struct {
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	for _, message := range decoded.Messages {
		for _, part := range message.Content {
			if part.Type == "image_url" {
				return part.ImageURL.URL
			}
		}
	}
	t.Fatal("no image_url part in the built body")
	return ""
}

// TestOutboundImageFitsTheProductByteBudget is the measured defect: with no
// scaling the request grew until the gateway answered 413.
//
// ⚠️ The budget asserted here is THIS product's 512 KB, not cline's 1 MiB.
// Unifying them would put ten images over the 10 MB hard limit again.
func TestOutboundImageFitsTheProductByteBudget(t *testing.T) {
	original := "data:image/png;base64," + base64.StdEncoding.EncodeToString(noisyScreenshot(t, 2560, 1600))
	body, errBody := chatRequestBody(pluginapi.ExecutorRequest{
		Model:   "sn-sensenova-6-8-flash",
		Payload: []byte(imagePayload(t, original)),
	})
	if errBody != nil {
		t.Fatalf("build body: %v", errBody)
	}
	projected := imageURLOf(t, body)
	if projected == original {
		t.Fatal("an oversized image was sent at full size: the request would 413 after ~4 screenshots")
	}
	encoded := projected[strings.IndexByte(projected, ',')+1:]
	raw, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		t.Fatalf("projected payload is not base64: %v", errDecode)
	}
	if len(raw) > ImageMaxBytes {
		t.Fatalf("projected image is %d bytes, over this product's %d-byte budget", len(raw), ImageMaxBytes)
	}
	// It must still be a real image with its media type intact: the model and any
	// downstream parser both depend on that.
	if !strings.HasPrefix(projected, "data:image/jpeg;base64,") && !strings.HasPrefix(projected, "data:image/png;base64,") {
		t.Fatalf("media type was lost: %.40s", projected)
	}
	config, _, errImage := image.DecodeConfig(bytes.NewReader(raw))
	if errImage != nil {
		t.Fatalf("projected image does not decode: %v", errImage)
	}
	if config.Width*config.Height > ImagePixelBudget {
		t.Fatalf("projected image is %d×%d, over the %d px budget",
			config.Width, config.Height, ImagePixelBudget)
	}
}

// TestOutboundImageBelowBudgetIsUntouched pins the "never turn a working image
// into a different one" rule at the executor level: an image inside both budgets
// comes out byte-identical, so nothing downstream sees a surprise re-encode.
func TestOutboundImageBelowBudgetIsUntouched(t *testing.T) {
	original := "data:image/png;base64," + base64.StdEncoding.EncodeToString(noisyScreenshot(t, 64, 48))
	body, errBody := chatRequestBody(pluginapi.ExecutorRequest{
		Model:   "sn-sensenova-6-8-flash",
		Payload: []byte(imagePayload(t, original)),
	})
	if errBody != nil {
		t.Fatalf("build body: %v", errBody)
	}
	if projected := imageURLOf(t, body); projected != original {
		t.Fatal("a small image must be sent byte-identical")
	}
}

// TestOutboundBodyWithoutImagesIsUnchanged pins that the projection is inert on
// the overwhelmingly common text-only request: the body must be exactly what it
// was before the feature existed.
func TestOutboundBodyWithoutImagesIsUnchanged(t *testing.T) {
	payload := `{"model":"sn-kimi-k3","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	body, errBody := chatRequestBody(pluginapi.ExecutorRequest{Model: "sn-kimi-k3", Payload: []byte(payload)})
	if errBody != nil {
		t.Fatalf("build body: %v", errBody)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded["temperature"] != 0.5 {
		t.Errorf("caller parameters were altered: %#v", decoded)
	}
	messages, _ := decoded["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	if content, _ := messages[0].(map[string]any)["content"].(string); content != "hi" {
		t.Errorf("content was rewritten: %#v", messages[0])
	}
}

// TestOutboundRemoteImageURLIsLeftAlone pins that the projection only touches
// inline data URLs. Fetching a remote URL is not this plugin's job, and
// rewriting it would break the request outright.
func TestOutboundRemoteImageURLIsLeftAlone(t *testing.T) {
	remote := "https://example.com/very-large-screenshot.png"
	body, errBody := chatRequestBody(pluginapi.ExecutorRequest{
		Model:   "sn-sensenova-6-8-flash",
		Payload: []byte(imagePayload(t, remote)),
	})
	if errBody != nil {
		t.Fatalf("build body: %v", errBody)
	}
	if projected := imageURLOf(t, body); projected != remote {
		t.Fatalf("a remote URL was rewritten to %q", projected)
	}
}

// TestOutboundUndecodableImageIsLeftAlone pins upstream's fallback direction:
// "send the original rather than fail". A payload this plugin cannot decode must
// still reach the upstream, because the upstream may well accept it.
func TestOutboundUndecodableImageIsLeftAlone(t *testing.T) {
	broken := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not really a PNG"))
	body, errBody := chatRequestBody(pluginapi.ExecutorRequest{
		Model:   "sn-sensenova-6-8-flash",
		Payload: []byte(imagePayload(t, broken)),
	})
	if errBody != nil {
		t.Fatalf("an undecodable image must not fail the request: %v", errBody)
	}
	if projected := imageURLOf(t, body); projected != broken {
		t.Fatalf("an undecodable image was rewritten to %.40q", projected)
	}
}

// TestImageBudgetsAreNotUnified pins the deliberate per-product split. Upstream
// kept raccoon at 512 KB and cline at 1 MiB because their constraints differ: at
// 1 MiB this gateway's 10 MB hard limit is reached again after ten images.
func TestImageBudgetsAreNotUnified(t *testing.T) {
	if ImageMaxBytes != 512*1024 {
		t.Fatalf("raccoon byte budget = %d, want the measured 512 KiB", ImageMaxBytes)
	}
	if ImageMaxBytes > 1024*1024 {
		t.Fatal("raccoon's budget must stay below cline's 1 MiB: its gateway hard-caps the body at 10 MB")
	}
}
