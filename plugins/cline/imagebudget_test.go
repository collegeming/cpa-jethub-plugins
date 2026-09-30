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
)

// Image projection at the adapter level (upstream `7ed3466`, issue !IKITT9).
//
// Measured: 24 full-size 2560×1600 images (≈159K image tokens — far past the
// 100,000 the Tencent gateways allow) all succeeded, and 32 of them (≈122 MiB of
// request body) died with `TRANSPORT`. So this product's constraint is REQUEST
// VOLUME, and the byte target backs that off while staying more generous than
// raccoon's, whose gateway hard-caps the body at 10 MB.
//
// The geometry is covered by the shared package's own tests; what is pinned here
// is the wiring and this product's budget.

// noisyScreenshot renders a large PNG the encoder cannot compress away.
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
		"model": "cline-free/deepseek-v4.1-flash",
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

// buildWithImage builds a body for one inline image.
func buildWithImage(t *testing.T, dataURL string) []byte {
	t.Helper()
	body, _, errBuild := buildChatBody([]byte(imagePayload(t, dataURL)), "cline-free/deepseek-v4.1-flash", DefaultConfig())
	if errBuild != nil {
		t.Fatalf("build body: %v", errBuild)
	}
	return body
}

// TestOutboundImageFitsTheProductByteBudget is the measured defect: with no
// scaling the request grew until the transport failed.
//
// ⚠️ The budget asserted here is THIS product's 1 MiB, which is deliberately
// larger than raccoon's 512 KiB: their constraints differ, and upstream kept the
// two values apart on purpose.
func TestOutboundImageFitsTheProductByteBudget(t *testing.T) {
	original := "data:image/png;base64," + base64.StdEncoding.EncodeToString(noisyScreenshot(t, 2560, 1600))
	body := buildWithImage(t, original)
	projected := imageURLOf(t, body)
	if projected == original {
		t.Fatal("an oversized image was sent at full size: 32 of them failed with TRANSPORT")
	}
	encoded := projected[strings.IndexByte(projected, ',')+1:]
	raw, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		t.Fatalf("projected payload is not base64: %v", errDecode)
	}
	if len(raw) > ImageMaxBytes {
		t.Fatalf("projected image is %d bytes, over this product's %d-byte budget", len(raw), ImageMaxBytes)
	}
	// The media type must survive so a downstream parser still recognises it.
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

// TestOutboundImageBelowBudgetIsUntouched pins the no-op direction: an image
// inside both budgets must be sent byte-identical rather than re-encoded.
func TestOutboundImageBelowBudgetIsUntouched(t *testing.T) {
	original := "data:image/png;base64," + base64.StdEncoding.EncodeToString(noisyScreenshot(t, 64, 48))
	if projected := imageURLOf(t, buildWithImage(t, original)); projected != original {
		t.Fatal("a small image must be sent byte-identical")
	}
}

// TestOutboundRemoteImageURLIsLeftAlone pins that only inline data URLs are
// touched: fetching or rewriting a remote URL would break the request.
func TestOutboundRemoteImageURLIsLeftAlone(t *testing.T) {
	remote := "https://example.com/very-large-screenshot.png"
	if projected := imageURLOf(t, buildWithImage(t, remote)); projected != remote {
		t.Fatalf("a remote URL was rewritten to %q", projected)
	}
}

// TestOutboundUndecodableImageIsLeftAlone pins upstream's fallback direction:
// "send the original rather than fail". The upstream may well accept a payload
// this plugin cannot decode, so it must still be sent.
func TestOutboundUndecodableImageIsLeftAlone(t *testing.T) {
	broken := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not really a PNG"))
	body, _, errBuild := buildChatBody([]byte(imagePayload(t, broken)), "cline-free/deepseek-v4.1-flash", DefaultConfig())
	if errBuild != nil {
		t.Fatalf("an undecodable image must not fail the request: %v", errBuild)
	}
	if projected := imageURLOf(t, body); projected != broken {
		t.Fatalf("an undecodable image was rewritten to %.40q", projected)
	}
}

// TestOutboundBodyWithoutImagesIsUnchanged pins that the projection is inert on
// a text-only request. `buildChatBody` rebuilds from a whitelist, so this also
// proves the projection did not disturb that whitelist.
func TestOutboundBodyWithoutImagesIsUnchanged(t *testing.T) {
	payload := `{"model":"cline-free/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	body, _, errBuild := buildChatBody([]byte(payload), "cline-free/deepseek-v4.1-flash", DefaultConfig())
	if errBuild != nil {
		t.Fatalf("build body: %v", errBuild)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	messages, _ := decoded["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	if content, _ := messages[0].(map[string]any)["content"].(string); content != "hi" {
		t.Errorf("content was rewritten: %#v", messages[0])
	}
	if decoded["temperature"] != 0.5 || decoded["stream"] != true {
		t.Errorf("the whitelist changed: %#v", decoded)
	}
}

// TestImageBudgetsAreNotUnified pins the deliberate per-product split. Upstream
// kept raccoon at 512 KB and cline at 1 MiB because their constraints differ:
// this product has no image-token quota, only a request-volume limit.
func TestImageBudgetsAreNotUnified(t *testing.T) {
	if ImageMaxBytes != 1024*1024 {
		t.Fatalf("cline byte budget = %d, want the documented 1 MiB", ImageMaxBytes)
	}
	if ImageMaxBytes <= 512*1024 {
		t.Fatal("cline's budget must stay above raccoon's 512 KiB: upstream kept them different on purpose")
	}
	if ImagePixelBudget != 640_000 {
		t.Fatalf("cline pixel budget = %d, want the shared 640,000", ImagePixelBudget)
	}
}
