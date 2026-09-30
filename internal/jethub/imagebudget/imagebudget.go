// Package imagebudget projects inline images down to a request version that
// fits the budgets their gateway enforces.
//
// Ported from `src/image-budget.ts` of the Jet-Hub TypeScript repository
// (upstream commit `7ed3466`, issue !IKITT9). The measured problem there: a
// session that accumulates screenshots sends every image at full size, and the
// request dies upstream. The failure mode differs per product but the cause is
// one of two, and the knob differs with it — the visual-token budget is counted
// in PIXELS, the transport limit in encoded BYTES:
//
//	raccoon   4 screenshots  HTTP_413: request body exceeds 10MB   (bytes)
//	cline    32 screenshots  TRANSPORT (≈122 MiB)                  (bytes)
//
// ⇒ the per-product BYTE budgets are deliberately different and are NOT
// unified: raccoon 512 KB (its hard limit is 10 MB, so the target has to leave
// room for more than a couple of screenshots), cline 1 MiB. Both are declared in
// the owning plugin's `product.go` with the upstream citation, because they
// belong to the product and not to the geometry. The PIXEL budget
// ([PixelBudget]) IS shared: upstream calls it a product-independent
// "per image" constant and warns against pushing one product's value onto
// another.
//
// The projection is a best-effort optimisation and must never turn a working
// request into a failing one. Every input this package cannot improve — a
// remote URL, an undecodable payload, an image already inside both budgets — is
// returned unchanged. [Project] reserves its error result for the one
// case where the caller asked for the impossible and the original is still the
// right thing to send.
package imagebudget

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"strings"
)

// PixelBudget is the per-image pixel budget: **640,000 px** (≈1050×610).
//
// Source: `DEFAULT_IMAGE_PIXEL_BUDGET` in `src/image-budget.ts` of the Jet-Hub
// TypeScript repository. It is derived from the measured ≈617 px per visual
// token of the Tencent gateways: 640,000 px ≈ 1,037 token per image, so a
// 100,000-token per-request image budget holds ≈96 images instead of the 36 that
// used to break the session. Upstream warns explicitly not to lower it: UI
// small print and terminal output are read from those pixels.
//
// ⚠️ It is the SAME value for every product on purpose (upstream: "别把 buddy 的
// 640,000 当全局像素预算推给别家" refers to the byte target, not to this one);
// what differs per product is the byte budget, which each plugin declares.
const PixelBudget = 640_000

// jpegQualityLadder is the encoder ladder tried after the pixel fit, highest
// quality first. Upstream delegates this to the attachment service, which walks
// a 85/75/60 quality ladder; this package reimplements exactly that ladder so the
// port keeps the same trade-off between legibility and bytes.
var jpegQualityLadder = []int{85, 75, 60}

// maxScaleRounds bounds the geometric fallback: after the full pixel fit and
// the whole quality ladder, the image is shrunk by 3/4 per round. Three rounds
// take a 1010×630 fit down to ≈426×265, far past the point where a screenshot is
// readable, so stopping there is a deliberate floor rather than an oversight.
const maxScaleRounds = 3

// Project returns the request version of one inline image.
//
// The contract is "return something sendable" rather than "return a strictly
// smaller image": a value the projection cannot improve is returned BYTE FOR
// BYTE, so callers never have to compare lengths to know whether anything
// happened.
//
//   - not a `data:` URL (an http(s) image URL, a bare string) → unchanged;
//   - a data URL whose media type is neither PNG nor JPEG (SVG, WebP, GIF …) →
//     unchanged, because there is no stdlib decoder to re-encode it with;
//   - an undecodable payload → unchanged;
//   - already inside BOTH the pixel and the byte budget → unchanged;
//   - otherwise: fit to [PixelBudget], re-encode (PNG first when the
//     source was PNG and the result fits, JPEG otherwise), and walk the quality
//     ladder plus a short geometric fallback until it fits `maxBytes`.
//
// The returned data URL always declares the media type of the bytes it
// carries, so a downstream parser that trusts the prefix still works.
//
// ⚠️ One honest caveat: the projection can return MORE bytes than it was given.
// A source the encoder already handled very well (a flat-colour UI mock-up, say)
// is under the byte budget but over the pixel budget, and re-encoding its scaled
// form as PNG can be larger than the original — measured at 343 KB → 531 KB on a
// synthetic 2560×1600 fixture. The pixel budget is what forces the re-encode, and
// it is the constraint the Tencent gateways enforce, so it wins; but the byte
// budget is enforced on the RESULT, never claimed as a reduction. A caller that
// needs a strict byte win must compare the two lengths itself.
//
// The error result is non-nil only when the image HAD to be projected and no
// encoding met `maxBytes`; the returned string is then the untouched original
// and the caller's correct response is to log and send it. A non-positive
// `maxBytes` names no target at all, so the original comes back with a nil error
// rather than a failure the caller cannot act on.
func Project(sourceURL string, maxBytes int) (string, error) {
	mediaType, encoded, ok := splitDataURL(sourceURL)
	if !ok {
		return sourceURL, nil
	}
	if mediaType != "image/png" && mediaType != "image/jpeg" {
		return sourceURL, nil
	}
	raw, errDecode := decodeBase64(encoded)
	if errDecode != nil {
		return sourceURL, nil
	}
	if maxBytes <= 0 {
		return sourceURL, nil
	}
	decoded, format, errImage := image.Decode(bytes.NewReader(raw))
	if errImage != nil {
		return sourceURL, nil
	}
	if format != "png" && format != "jpeg" {
		return sourceURL, nil
	}
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return sourceURL, nil
	}
	targetWidth, targetHeight := fitToPixelBudget(width, height, PixelBudget)
	// Already inside both budgets: leave the bytes alone. Re-encoding here would
	// only lose quality for nothing.
	if targetWidth == width && targetHeight == height && len(raw) <= maxBytes {
		return sourceURL, nil
	}

	sourcePNG := format == "png"
	width, height = targetWidth, targetHeight
	current := decoded
	for round := 0; ; round++ {
		if round > 0 {
			// The quality ladder is exhausted; shrink geometrically and retry it.
			width = maxInt(1, width*3/4)
			height = maxInt(1, height*3/4)
			if width == 1 && height == 1 {
				return sourceURL, errBudgetMissed(maxBytes, len(raw))
			}
		}
		current = scale(current, width, height)
		if projected, okProjected := encodeWithinBudget(current, sourcePNG, maxBytes); okProjected {
			return projected, nil
		}
		if round >= maxScaleRounds {
			return sourceURL, errBudgetMissed(maxBytes, len(raw))
		}
	}
}

// ProjectChatPayload projects every inline image of one OpenAI
// chat-completions request body.
//
// This is the call site the two plugins actually need: an image reaches the wire
// as an `image_url` content part (there is no attachment service on the CPA side,
// the host already inlined the bytes), and the part's URL is either a `data:` URL
// — projected here — or an http(s) URL, which is left alone because fetching it
// is not this function's job.
//
// The walk covers the three places a part can hide: a message whose `content` is
// an array, the `content` array inside a `tool` result block, and the `content`
// array inside a `tool-result` block. A payload that does not parse, or carries
// no image at all, is returned unchanged — the projection must never be able to
// break a request that would otherwise work.
//
// Every projected image is reported through `onProjected` (nil-safe) with the
// bytes before and after, so a caller can log the saving. Projection errors are
// reported the same way and the ORIGINAL image is kept, which is the direction
// upstream insists on: "send the original rather than fail".
//
// The returned body is always the caller's to send: it is the marshalled
// projection when anything changed and the byte-identical input otherwise.
func ProjectChatPayload(body []byte, maxBytes int, onProjected func(before, after int, err error)) []byte {
	if len(body) == 0 || maxBytes <= 0 {
		return body
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return body
	}
	messages, okMessages := root["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		return body
	}
	changed := false
	for _, candidate := range messages {
		message, okMessage := candidate.(map[string]any)
		if !okMessage {
			continue
		}
		if projectContentParts(message["content"], maxBytes, onProjected) {
			changed = true
		}
	}
	if !changed {
		return body
	}
	encoded, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return body
	}
	return encoded
}

// projectContentParts projects the image parts of one `content` value.
//
// `content` is a plain string in the common case and an array of parts when the
// turn carries an image. A `tool` result and a `tool-result` block each nest
// their own `content` array, which is walked the same way. Reports whether
// anything was rewritten.
func projectContentParts(content any, maxBytes int, onProjected func(before, after int, err error)) bool {
	parts, okParts := content.([]any)
	if !okParts {
		return false
	}
	changed := false
	for _, candidate := range parts {
		part, okPart := candidate.(map[string]any)
		if !okPart {
			continue
		}
		switch partType, _ := part["type"].(string); partType {
		case "image_url", "input_image", "image":
			if projectImagePart(part, maxBytes, onProjected) {
				changed = true
			}
		case "tool-result", "tool_result":
			if projectContentParts(part["content"], maxBytes, onProjected) {
				changed = true
			}
		}
	}
	return changed
}

// projectImagePart rewrites one image part's URL.
//
// Two shapes carry it: the OpenAI wire form
// `{type:"image_url",image_url:{url}}` and the flat `{type:"image",url}` some
// clients emit. Both are handled, and a part in neither shape is skipped rather
// than guessed at.
func projectImagePart(part map[string]any, maxBytes int, onProjected func(before, after int, err error)) bool {
	nested, okNested := part["image_url"].(map[string]any)
	if okNested {
		if url, okURL := nested["url"].(string); okURL {
			projected, changed := projectURL(url, maxBytes, onProjected)
			if changed {
				nested["url"] = projected
			}
			return changed
		}
		return false
	}
	if url, okURL := part["url"].(string); okURL {
		projected, changed := projectURL(url, maxBytes, onProjected)
		if changed {
			part["url"] = projected
		}
		return changed
	}
	return false
}

// projectURL projects one URL and reports whether it changed.
func projectURL(url string, maxBytes int, onProjected func(before, after int, err error)) (string, bool) {
	projected, errProject := Project(url, maxBytes)
	if onProjected != nil && (projected != url || errProject != nil) {
		onProjected(len(url), len(projected), errProject)
	}
	if errProject != nil {
		// The original is what `Project` returned in that case; nothing to write.
		return url, false
	}
	return projected, projected != url
}

// splitDataURL parses `data:<media type>;base64,<payload>`.
// The media type is lower-cased and stripped of its parameters so
// `data:image/png;charset=utf-8;base64,…` is recognised too. Only the `;base64`
// form is accepted: a percent-encoded data URL is not something any client in
// this repository produces, and guessing at one is how a URL gets corrupted.
func splitDataURL(dataURL string) (string, string, bool) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", "", false
	}
	rest := dataURL[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	header := rest[:comma]
	payload := rest[comma+1:]
	semicolon := strings.IndexByte(header, ';')
	if semicolon < 0 {
		return "", "", false
	}
	mediaType := strings.ToLower(strings.TrimSpace(header[:semicolon]))
	parameters := strings.Split(header[semicolon+1:], ";")
	isBase64 := false
	for _, parameter := range parameters {
		if strings.EqualFold(strings.TrimSpace(parameter), "base64") {
			isBase64 = true
		}
	}
	if !isBase64 || mediaType == "" || payload == "" {
		return "", "", false
	}
	return mediaType, payload, true
}

// decodeBase64 accepts a padded or an unpadded payload: some clients emit the
// latter and it decodes to the very same bytes.
func decodeBase64(payload string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(payload); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "="))
}

// fitToPixelBudget scales a size down into the budget, preserving the aspect
// ratio and NEVER enlarging.
//
// Same geometry as `fitImageToPixelBudget` (`src/image-budget.ts`): a small image
// is returned as it is (scaling only makes its text blurrier and saves nothing),
// and the linear factor is the square root of the area ratio because area grows
// with the square of the scale.
func fitToPixelBudget(width, height, maxPixels int) (int, int) {
	if width <= 0 || height <= 0 || maxPixels <= 0 {
		return width, height
	}
	if width*height <= maxPixels {
		return width, height
	}
	scale := math.Sqrt(float64(maxPixels) / float64(width*height))
	return maxInt(1, int(float64(width)*scale)), maxInt(1, int(float64(height)*scale))
}

// encodeWithinBudget re-encodes the image and reports whether it met the budget.
//
// PNG is preferred when the source was PNG and the result fits, because
// re-encoding a screenshot to JPEG softens exactly the small text the pixel
// budget was chosen to preserve. Any other case goes to JPEG, which is what
// actually gets an oversized PNG under a byte target.
func encodeWithinBudget(img image.Image, sourcePNG bool, maxBytes int) (string, bool) {
	if sourcePNG {
		var buffer bytes.Buffer
		if errEncode := png.Encode(&buffer, img); errEncode == nil && buffer.Len() <= maxBytes {
			return dataURL("image/png", buffer.Bytes()), true
		}
	}
	opaque := flattenOntoWhite(img)
	for _, quality := range jpegQualityLadder {
		var buffer bytes.Buffer
		if errEncode := jpeg.Encode(&buffer, opaque, &jpeg.Options{Quality: quality}); errEncode != nil {
			continue
		}
		if buffer.Len() <= maxBytes {
			return dataURL("image/jpeg", buffer.Bytes()), true
		}
	}
	return "", false
}

// flattenOntoWhite composites the image over an opaque white background.
//
// JPEG has no alpha channel, so a transparent pixel has to become SOMETHING;
// left to the encoder it becomes black, which turns a transparent-background
// screenshot into an unreadable dark block. White is what the screenshot was
// drawn against in the first place.
func flattenOntoWhite(img image.Image) image.Image {
	bounds := img.Bounds()
	flat := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, bounds.Min, draw.Over)
	return flat
}

// scale resizes the image to exactly width×height with a box filter.
//
// Each destination pixel averages the source rectangle it covers, which is the
// cheap-and-correct downscale for the large integer ratios this package deals
// with (2560→1011 is 2.5:1, so the box is a real average and not a point
// sample). `image.RGBA` and `color.RGBA` share the alpha-premultiplied
// convention, so averaging the 16-bit channel values is consistent.
//
// The destination size is validated to lie inside the source, so an upscale is
// never performed by accident: a caller that wants no scaling gets the original
// image back untouched.
func scale(img image.Image, width, height int) image.Image {
	bounds := img.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 || sourceWidth <= 0 || sourceHeight <= 0 {
		return img
	}
	if width >= sourceWidth && height >= sourceHeight {
		return img
	}
	destination := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		startY := bounds.Min.Y + y*sourceHeight/height
		endY := bounds.Min.Y + (y+1)*sourceHeight/height
		if endY <= startY {
			endY = startY + 1
		}
		for x := 0; x < width; x++ {
			startX := bounds.Min.X + x*sourceWidth/width
			endX := bounds.Min.X + (x+1)*sourceWidth/width
			if endX <= startX {
				endX = startX + 1
			}
			var red, green, blue, alpha, count uint64
			for sourceY := startY; sourceY < endY; sourceY++ {
				for sourceX := startX; sourceX < endX; sourceX++ {
					channelRed, channelGreen, channelBlue, channelAlpha := img.At(sourceX, sourceY).RGBA()
					red += uint64(channelRed)
					green += uint64(channelGreen)
					blue += uint64(channelBlue)
					alpha += uint64(channelAlpha)
					count++
				}
			}
			if count == 0 {
				continue
			}
			destination.SetRGBA(x, y, color.RGBA{
				R: uint8(red / count >> 8),
				G: uint8(green / count >> 8),
				B: uint8(blue / count >> 8),
				A: uint8(alpha / count >> 8),
			})
		}
	}
	return destination
}

// dataURL renders encoded bytes back into a data URL.
func dataURL(mediaType string, raw []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// errBudgetMissed reports that the projection could not reach the byte target.
//
// It is a plain error rather than an abiboot envelope on purpose: this package
// is shared geometry and imports nothing from the host ABI. Callers log it and
// send the original.
func errBudgetMissed(maxBytes, originalBytes int) error {
	return &budgetError{maxBytes: maxBytes, originalBytes: originalBytes}
}

// budgetError is the "no encoding met the target" failure.
type budgetError struct {
	maxBytes      int
	originalBytes int
}

func (e *budgetError) Error() string {
	return "image budget: no encoding met the " + itoa(e.maxBytes) +
		"-byte target (original " + itoa(e.originalBytes) + " bytes); sending the original"
}

// itoa renders a non-negative int without pulling in strconv for one call site.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 12)
	for value > 0 {
		digits = append(digits, byte('0'+value%10))
		value /= 10
	}
	for left, right := 0, len(digits)-1; left < right; left, right = left+1, right-1 {
		digits[left], digits[right] = digits[right], digits[left]
	}
	return string(digits)
}

// maxInt returns the larger of two ints.
func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
