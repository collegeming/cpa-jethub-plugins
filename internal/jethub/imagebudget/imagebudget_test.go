package imagebudget

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// Projection behaviour: the fallbacks that keep a working request working, and
// the two budgets that actually shrink an oversized screenshot.
//
// Every fixture is generated in code. No binary test file is checked in, which
// also keeps the suite fast: the oversized case is a flat 2560×1600 PNG that
// compresses poorly enough to exceed a small byte target but encodes in
// milliseconds.

// noisyPNG renders a width×height PNG with enough per-pixel variation that the
// encoder cannot compress it away. A flat fill would sail under any byte target
// and the byte-budget assertions would pass vacuously.
func noisyPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// A cheap deterministic pattern: the high bits of a product walk the
			// palette, so neighbouring pixels rarely repeat.
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

// dataURLFor renders raw bytes as a data URL of the given media type.
func dataURLFor(mediaType string, raw []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// decodePayload splits a data URL and decodes its bytes.
func decodePayload(t *testing.T, value string) (string, []byte) {
	t.Helper()
	mediaType, encoded, ok := splitDataURL(value)
	if !ok {
		t.Fatalf("not a base64 data URL: %q", value)
	}
	raw, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		t.Fatalf("decode payload: %v", errDecode)
	}
	return mediaType, raw
}

// TestProjectOversizedImageFitsBothBudgets is the measured defect: raccoon died
// with `HTTP_413: request body exceeds 10MB` after ~4 screenshots and cline with
// a transport error at ~122 MiB, because nothing scaled the images.
func TestProjectOversizedImageFitsBothBudgets(t *testing.T) {
	original := dataURLFor("image/png", noisyPNG(t, 2560, 1600))
	if len(original) == 0 {
		t.Fatal("empty fixture")
	}
	for _, testCase := range []struct {
		name     string
		maxBytes int
	}{
		// The per-product budgets this repository ships, plus a tight one that
		// forces the quality ladder and the geometric fallback to run.
		{"raccoon 512 KiB", 512 * 1024},
		{"cline 1 MiB", 1024 * 1024},
		{"a budget the pixel fit alone cannot meet", 64 * 1024},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			projected, errProject := Project(original, testCase.maxBytes)
			if errProject != nil {
				t.Fatalf("Project returned an error for a decodable PNG: %v", errProject)
			}
			mediaType, raw := decodePayload(t, projected)
			if mediaType != "image/png" && mediaType != "image/jpeg" {
				t.Fatalf("media type = %q, want a preserved PNG/JPEG type", mediaType)
			}
			if len(raw) > testCase.maxBytes {
				t.Fatalf("projected image is %d bytes, over the %d-byte budget", len(raw), testCase.maxBytes)
			}
			// The bytes must still be a real image: a downstream parser and the
			// model both depend on that.
			config, format, errDecode := image.DecodeConfig(bytes.NewReader(raw))
			if errDecode != nil {
				t.Fatalf("projected image does not decode: %v", errDecode)
			}
			if config.Width*config.Height > PixelBudget {
				t.Fatalf("projected image is %d×%d = %d px, over the %d px budget",
					config.Width, config.Height, config.Width*config.Height, PixelBudget)
			}
			if format == "png" && mediaType != "image/png" {
				t.Fatalf("PNG bytes declared as %q", mediaType)
			}
			if format == "jpeg" && mediaType != "image/jpeg" {
				t.Fatalf("JPEG bytes declared as %q", mediaType)
			}
		})
	}
}

// TestProjectKeepsTheSourceMediaType pins the "prefer PNG when the source was
// PNG and the result fits" rule: a screenshot re-encoded to JPEG loses exactly
// the small text the pixel budget exists to preserve.
func TestProjectKeepsTheSourceMediaType(t *testing.T) {
	// A large but highly compressible PNG stays well under 512 KiB once fitted,
	// so it must come back as PNG.
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1600))
	for y := 0; y < 1600; y++ {
		for x := 0; x < 2560; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 0x20, G: 0x40, B: 0x60, A: 0xff})
		}
	}
	var buffer bytes.Buffer
	if errEncode := png.Encode(&buffer, img); errEncode != nil {
		t.Fatalf("encode fixture: %v", errEncode)
	}
	original := dataURLFor("image/png", buffer.Bytes())
	projected, errProject := Project(original, 512*1024)
	if errProject != nil {
		t.Fatalf("Project: %v", errProject)
	}
	if projected == original {
		t.Fatal("an oversized image must be projected, not passed through")
	}
	if mediaType, _ := decodePayload(t, projected); mediaType != "image/png" {
		t.Fatalf("media type = %q, want image/png (the source was PNG and it fits)", mediaType)
	}
}

// TestProjectFallsBackToJPEGForAGrayscaleScreenshot covers the other half of the
// rule: a JPEG source has to stay JPEG, because there is no PNG source to
// preserve and re-encoding a photograph to PNG would blow the byte budget.
func TestProjectFallsBackToJPEGForAGrayscaleScreenshot(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 2560, 1600))
	for y := 0; y < 1600; y++ {
		for x := 0; x < 2560; x++ {
			source.SetRGBA(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 5), B: uint8(x ^ y), A: 0xff})
		}
	}
	var buffer bytes.Buffer
	if errEncode := jpeg.Encode(&buffer, source, &jpeg.Options{Quality: 100}); errEncode != nil {
		t.Fatalf("encode fixture: %v", errEncode)
	}
	original := dataURLFor("image/jpeg", buffer.Bytes())
	projected, errProject := Project(original, 512*1024)
	if errProject != nil {
		t.Fatalf("Project: %v", errProject)
	}
	if mediaType, raw := decodePayload(t, projected); mediaType != "image/jpeg" {
		t.Fatalf("media type = %q, want image/jpeg (raw %d bytes)", mediaType, len(raw))
	}
}

// TestProjectReturnsSmallImagesByteIdentical pins "never turn a working image
// into a different one": an image inside both budgets is returned untouched, so
// the caller can rely on byte equality rather than comparing lengths.
func TestProjectReturnsSmallImagesByteIdentical(t *testing.T) {
	small := dataURLFor("image/png", noisyPNG(t, 64, 48))
	projected, errProject := Project(small, 512*1024)
	if errProject != nil {
		t.Fatalf("Project: %v", errProject)
	}
	if projected != small {
		t.Fatal("an image inside both budgets must be returned byte-identical")
	}
}

// TestProjectPassesThroughNonDataURLsAndUndecodablePayloads pins upstream's
// fallback direction: "send the original rather than fail". None of these may
// produce an error — the projection is an optimisation, never a new failure
// source.
func TestProjectPassesThroughNonDataURLsAndUndecodablePayloads(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"an http URL", "https://example.com/screenshot.png"},
		{"an empty string", ""},
		{"a bare base64 string without the data: prefix", base64.StdEncoding.EncodeToString([]byte("not an image"))},
		{"a data URL without a base64 marker", "data:image/png,plain"},
		{"a relative path", "./shot.png"},
		{"a data URL with no payload", "data:image/png;base64,"},
		{"a media type there is no stdlib decoder for", dataURLFor("image/webp", []byte("RIFF....WEBPVP8 "))},
		{"an SVG data URL", dataURLFor("image/svg+xml", []byte("<svg/>"))},
		{"undecodable PNG bytes", dataURLFor("image/png", []byte("this is not a PNG at all"))},
		{"undecodable JPEG bytes", dataURLFor("image/jpeg", []byte{0xFF, 0xD8, 0x00, 0x01, 0x02})},
		{"invalid base64", "data:image/png;base64,!!!!not-base64!!!!"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			projected, errProject := Project(testCase.url, 512*1024)
			if errProject != nil {
				t.Fatalf("Project must not error on an input it cannot improve: %v", errProject)
			}
			if projected != testCase.url {
				t.Fatalf("Project changed the value:\n got %q\nwant %q", projected, testCase.url)
			}
		})
	}
}

// TestProjectWithoutAByteTargetIsANoOp pins the non-positive target: there is
// nothing to hit, so the original comes back and the caller is not handed a
// failure it cannot act on.
func TestProjectWithoutAByteTargetIsANoOp(t *testing.T) {
	original := dataURLFor("image/png", noisyPNG(t, 2560, 1600))
	for _, maxBytes := range []int{0, -1} {
		projected, errProject := Project(original, maxBytes)
		if errProject != nil {
			t.Fatalf("maxBytes=%d: %v", maxBytes, errProject)
		}
		if projected != original {
			t.Fatalf("maxBytes=%d: the original must be returned untouched", maxBytes)
		}
	}
}

// TestProjectAcceptsUnpaddedBase64 pins the tolerant decoder: some clients omit
// the padding and the bytes are identical.
func TestProjectAcceptsUnpaddedBase64(t *testing.T) {
	raw := noisyPNG(t, 64, 48)
	padded := dataURLFor("image/png", raw)
	unpadded := "data:image/png;base64," + strings.TrimRight(base64.StdEncoding.EncodeToString(raw), "=")
	if projected, errProject := Project(unpadded, 512*1024); errProject != nil || projected != unpadded {
		t.Fatalf("unpadded payload: got (%q, %v)", projected, errProject)
	}
	if projected, _ := Project(padded, 512*1024); projected != padded {
		t.Fatal("padded payload must also pass through untouched when it fits")
	}
}

// TestFitToPixelBudget pins the geometry: the aspect ratio is preserved, the
// budget is honoured, and a small image is NEVER enlarged.
func TestFitToPixelBudget(t *testing.T) {
	cases := []struct {
		name                     string
		width, height, maxPixels int
		wantWidth, wantHeight    int
	}{
		{"the measured 2560×1600 screenshot", 2560, 1600, PixelBudget, 1011, 632},
		{"already inside the budget is unchanged", 800, 600, PixelBudget, 800, 600},
		{"exactly at the budget is unchanged", 800, 800, PixelBudget, 800, 800},
		{"a one-pixel overage is scaled", 801, 800, PixelBudget, 800, 799},
		{"a tall image keeps its ratio", 400, 4000, 100_000, 100, 1000},
		{"a non-positive budget is a no-op", 2560, 1600, 0, 2560, 1600},
		{"a non-positive size is a no-op", 0, 1600, PixelBudget, 0, 1600},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			width, height := fitToPixelBudget(testCase.width, testCase.height, testCase.maxPixels)
			if width != testCase.wantWidth || height != testCase.wantHeight {
				t.Fatalf("fit = %d×%d, want %d×%d", width, height, testCase.wantWidth, testCase.wantHeight)
			}
			if testCase.maxPixels > 0 && testCase.width > 0 && testCase.height > 0 {
				if width*height > testCase.maxPixels {
					t.Fatalf("fit %d×%d = %d px exceeds %d", width, height, width*height, testCase.maxPixels)
				}
				if width > testCase.width || height > testCase.height {
					t.Fatalf("fit %d×%d enlarged the %d×%d source", width, height, testCase.width, testCase.height)
				}
			}
		})
	}
}

// TestScaleHonoursTheTargetSize pins the scaler: exact output dimensions, no
// accidental upscale, and a bounded error against the source average (which is
// what "correct downscale" means for a box filter).
func TestScaleHonoursTheTargetSize(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			// Left half black, right half white: the box average of any
			// destination pixel straddling the seam is mid grey.
			shade := uint8(0x00)
			if x >= 4 {
				shade = 0xff
			}
			source.SetRGBA(x, y, color.RGBA{R: shade, G: shade, B: shade, A: 0xff})
		}
	}
	scaled := scale(source, 4, 4)
	if got := scaled.Bounds().Dx(); got != 4 {
		t.Fatalf("width = %d, want 4", got)
	}
	if got := scaled.Bounds().Dy(); got != 4 {
		t.Fatalf("height = %d, want 4", got)
	}
	leftRed, _, _, _ := scaled.At(0, 0).RGBA()
	rightRed, _, _, _ := scaled.At(3, 0).RGBA()
	if leftRed>>8 != 0x00 {
		t.Errorf("left half = %#x, want black", leftRed>>8)
	}
	if rightRed>>8 != 0xff {
		t.Errorf("right half = %#x, want white", rightRed>>8)
	}
	// A target that is not smaller returns the source untouched.
	if same := scale(source, 8, 8); same != image.Image(source) {
		t.Error("a same-size target must return the source image itself")
	}
	if larger := scale(source, 16, 16); larger != image.Image(source) {
		t.Error("an upscale target must return the source image itself")
	}
}

// TestScalePreservesUniformColour pins the channel arithmetic on a flat fill: the
// box average of identical pixels is that pixel, so the compositing path must not
// drift the colour.
func TestScalePreservesUniformColour(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 100, 100))
	fill := color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff}
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			source.SetRGBA(x, y, fill)
		}
	}
	scaled := scale(source, 25, 25)
	red, green, blue, alpha := scaled.At(0, 0).RGBA()
	if uint8(red>>8) != fill.R || uint8(green>>8) != fill.G || uint8(blue>>8) != fill.B || uint8(alpha>>8) != fill.A {
		t.Fatalf("colour drifted to %#x %#x %#x %#x", red>>8, green>>8, blue>>8, alpha>>8)
	}
}

// TestSplitDataURL pins the header parser the whole package keys off.
func TestSplitDataURL(t *testing.T) {
	cases := []struct {
		name          string
		value         string
		wantMediaType string
		wantPayload   string
		wantOK        bool
	}{
		{"a plain PNG", "data:image/png;base64,AAAA", "image/png", "AAAA", true},
		{"an upper-case media type is lowered", "data:IMAGE/PNG;base64,AAAA", "image/png", "AAAA", true},
		{"a media type parameter is stripped", "data:image/png;charset=utf-8;base64,AAAA", "image/png", "AAAA", true},
		{"no data prefix", "image/png;base64,AAAA", "", "", false},
		{"no comma", "data:image/png;base64", "", "", false},
		{"no semicolon", "data:image/png,AAAA", "", "", false},
		{"not base64", "data:image/png;utf8,AAAA", "", "", false},
		{"an empty payload", "data:image/png;base64,", "", "", false},
		{"an empty media type", "data:;base64,AAAA", "", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mediaType, payload, ok := splitDataURL(testCase.value)
			if ok != testCase.wantOK {
				t.Fatalf("ok = %v, want %v", ok, testCase.wantOK)
			}
			if mediaType != testCase.wantMediaType || payload != testCase.wantPayload {
				t.Fatalf("got (%q, %q), want (%q, %q)", mediaType, payload, testCase.wantMediaType, testCase.wantPayload)
			}
		})
	}
}

// TestProjectReportsAnImpossibleBudget pins the one error path: when no encoding
// can meet the target the ORIGINAL comes back (so the caller still has something
// sendable) and the error says so.
func TestProjectReportsAnImpossibleBudget(t *testing.T) {
	original := dataURLFor("image/png", noisyPNG(t, 2560, 1600))
	// One byte is below the smallest possible PNG or JPEG, so the ladder cannot
	// terminate successfully.
	projected, errProject := Project(original, 1)
	if errProject == nil {
		t.Fatal("an unreachable byte target must be reported")
	}
	if projected != original {
		t.Fatal("the original must still be returned when the budget cannot be met")
	}
	if !strings.Contains(errProject.Error(), "sending the original") {
		t.Fatalf("error = %q, want it to state the fallback", errProject)
	}
}

// TestProjectChatPayloadWalksEveryImageLocation pins the walk: an image can sit
// in a message's content array, inside a `tool` result's content array, or inside
// a nested `tool-result` block, and all three reach the wire.
func TestProjectChatPayloadWalksEveryImageLocation(t *testing.T) {
	big := dataURLFor("image/png", noisyPNG(t, 2560, 1600))
	body, errMarshal := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": big}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": big}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool-result", "content": []any{
					map[string]any{"type": "image", "url": big},
				}},
			}},
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	projected := ProjectChatPayload(body, 512*1024, nil)
	if string(projected) == string(body) {
		t.Fatal("no image was projected: the walk missed every location")
	}
	if strings.Contains(string(projected), big) {
		t.Fatal("at least one image location was left at full size")
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(projected, &decoded); errUnmarshal != nil {
		t.Fatalf("the projected body does not parse: %v", errUnmarshal)
	}
	if decoded["model"] != "m" {
		t.Fatalf("unrelated fields were lost: %#v", decoded)
	}
}

// TestProjectChatPayloadLeavesCleanBodiesByteIdentical pins that a body with no
// image, no messages, or invalid JSON comes back untouched — the projection must
// never be able to break a request that would otherwise work.
func TestProjectChatPayloadLeavesCleanBodiesByteIdentical(t *testing.T) {
	small := dataURLFor("image/png", noisyPNG(t, 64, 48))
	cases := []struct {
		name string
		body string
	}{
		{"a text-only request", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"no messages key", `{"model":"m"}`},
		{"an empty messages array", `{"model":"m","messages":[]}`},
		{"a string content", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"invalid JSON", `{"model":"m","messages":[`},
		{"an empty body", ``},
		{"an image already inside the budget", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + small + `"}}]}]}`},
		{"an empty content array", `{"model":"m","messages":[{"role":"user","content":[]}]}`},
		{"a content part that carries no url", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			projected := ProjectChatPayload([]byte(testCase.body), 512*1024, nil)
			if string(projected) != testCase.body {
				t.Fatalf("body changed:\n got %s\nwant %s", projected, testCase.body)
			}
		})
	}
	// A non-positive budget is a no-op regardless of the content.
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` +
		dataURLFor("image/png", noisyPNG(t, 2560, 1600)) + `"}}]}]}`
	if projected := ProjectChatPayload([]byte(body), 0, nil); string(projected) != body {
		t.Fatal("a non-positive budget must leave the body untouched")
	}
}

// TestProjectChatPayloadReportsEveryProjection pins the reporting hook: a caller
// that wants to log the projection has to be told about it, and a projection
// that cannot meet the target has to be reported WITHOUT the image being dropped.
func TestProjectChatPayloadReportsEveryProjection(t *testing.T) {
	big := dataURLFor("image/png", noisyPNG(t, 2560, 1600))
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + big + `"}}]}]}`
	type report struct {
		before, after int
		failed        bool
	}
	var reports []report
	projected := ProjectChatPayload([]byte(body), 512*1024, func(before, after int, err error) {
		reports = append(reports, report{before: before, after: after, failed: err != nil})
	})
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}
	if reports[0].failed {
		t.Fatal("a projection that succeeded must not be reported as failed")
	}
	// The report is what the caller logs, so it must describe the same change
	// the body received.
	if reports[0].before != len(big) {
		t.Fatalf("reported before = %d, want the original URL length %d", reports[0].before, len(big))
	}
	if !strings.Contains(string(projected), "data:image/") {
		t.Fatal("the projected body carries no data URL")
	}
	// The pixel budget is the hard constraint here: this source only just fits
	// the byte target, so the value that must hold is the pixel count.
	mediaType, raw := decodePayload(t, imageURLFromBody(t, projected))
	if mediaType != "image/png" && mediaType != "image/jpeg" {
		t.Fatalf("media type = %q", mediaType)
	}
	config, _, errDecode := image.DecodeConfig(bytes.NewReader(raw))
	if errDecode != nil {
		t.Fatalf("projected image does not decode: %v", errDecode)
	}
	if config.Width*config.Height > PixelBudget {
		t.Fatalf("projected image is %d×%d, over the %d px budget",
			config.Width, config.Height, PixelBudget)
	}

	// An unreachable target must be reported, and the ORIGINAL image must still
	// be on the wire: "send the original rather than fail".
	reports = nil
	projected = ProjectChatPayload([]byte(body), 1, func(before, after int, err error) {
		reports = append(reports, report{before: before, after: after, failed: err != nil})
	})
	if len(reports) != 1 || !reports[0].failed {
		t.Fatalf("an unreachable target must be reported: %#v", reports)
	}
	if !strings.Contains(string(projected), big) {
		t.Fatal("the original image must stay on the wire when the budget cannot be met")
	}
}

// imageURLFromBody reads the first inline image URL out of a chat body.
func imageURLFromBody(t *testing.T, body []byte) string {
	t.Helper()
	var decoded struct {
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
				URL string `json:"url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	for _, message := range decoded.Messages {
		for _, part := range message.Content {
			if part.ImageURL.URL != "" {
				return part.ImageURL.URL
			}
			if part.URL != "" && (part.Type == "image" || part.Type == "input_image") {
				return part.URL
			}
		}
	}
	t.Fatal("no image URL in the body")
	return ""
}
