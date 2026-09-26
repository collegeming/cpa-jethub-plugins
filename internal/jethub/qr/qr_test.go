package qr

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// raccoonPayload is the EXACT string the raccoon login page encodes into its QR
// image: `https://xiaohuanxiong.com/login/mp?code=<32 hex>&appname=<urlencoded>`
// (raccoon-oauth.ts:127-130). It is 144 bytes, which is what forces version 8 at
// error-correction level M. The test asserts the length, so a change to the URL
// shape cannot silently move the symbol to another version.
const raccoonPayload = "https://xiaohuanxiong.com/login/mp?code=0123456789abcdef0123456789abcdef&appname=%E5%95%86%E6%B1%A4%E5%B0%8F%E6%B5%A3%E7%86%8A%E5%AE%98%E7%BD%91"

// decoderModules is where the independent decoder lives. The test skips (rather
// than fails) when it is absent, so the package still builds on a machine
// without Node — but on the machine this package was written for it is present
// and every case below really does round-trip through jsQR.
const decoderModules = "/tmp/qrverify/node_modules"

// TestRaccoonPayloadRoundTrips is the acceptance test the plugin actually
// depends on: the login URL must survive encode → PNG → independent decode
// byte-for-byte, or a user cannot log in.
func TestRaccoonPayloadRoundTrips(t *testing.T) {
	if len(raccoonPayload) != 144 {
		t.Fatalf("raccoon payload is %d bytes, want 144", len(raccoonPayload))
	}
	code, errEncode := Encode(raccoonPayload)
	if errEncode != nil {
		t.Fatalf("encode raccoon payload: %v", errEncode)
	}
	if code.Version != 8 {
		t.Errorf("version = %d, want 8 (version 7 holds 124 data codewords = 992 bits < 1164)", code.Version)
	}
	assertRoundTrip(t, code, raccoonPayload)
}

// TestPayloadsRoundTrip covers the short and long ends of the supported range,
// including the smallest version, the version-9/10 character-count boundary and
// the largest payload the package accepts.
func TestPayloadsRoundTrip(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		wantVersion int
	}{
		{"short", "HELLO WORLD", 1},
		{"single byte", "A", 1},
		{"alnum url", "https://example.com/a?b=c&d=e", 3},
		{"medium 115 bytes", strings.Repeat("Raccoon-", 14) + "end", 7},
		{"version 9 boundary", strings.Repeat("x", capacityBytes(9)), 9},
		{"version 10 boundary", strings.Repeat("y", capacityBytes(10)), 10},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			code, errEncode := Encode(testCase.text)
			if errEncode != nil {
				t.Fatalf("encode: %v", errEncode)
			}
			if code.Version != testCase.wantVersion {
				t.Errorf("version = %d, want the smallest that fits (%d)", code.Version, testCase.wantVersion)
			}
			assertRoundTrip(t, code, testCase.text)
		})
	}
}

// TestEveryVersionRoundTrips walks all ten versions: the block structures differ
// per version (one block, several equal blocks, two uneven groups), and an
// interleaving bug only shows up in the versions with more than one block.
func TestEveryVersionRoundTrips(t *testing.T) {
	for version := 1; version <= maxVersion; version++ {
		text := strings.Repeat(string(rune('a'+version-1)), capacityBytes(version))
		code, errEncode := Encode(text)
		if errEncode != nil {
			t.Fatalf("version %d capacity %d: %v", version, capacityBytes(version), errEncode)
		}
		if code.Version != version {
			t.Errorf("payload of %d bytes encoded as version %d, want %d", len(text), code.Version, version)
		}
		assertRoundTrip(t, code, text)
	}
}

// TestStructure pins the parts of the symbol geometry a decoder relies on and
// that the encoder must get right even when the payload does not exercise them.
func TestStructure(t *testing.T) {
	code, errEncode := Encode(raccoonPayload)
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	if code.Size != 21+4*(code.Version-1) {
		t.Errorf("size = %d, want %d for version %d", code.Size, 21+4*(code.Version-1), code.Version)
	}
	if len(code.Modules) != code.Size {
		t.Fatalf("module rows = %d, want %d", len(code.Modules), code.Size)
	}
	// Finder patterns: three 7x7 squares whose outer ring and 3x3 core are dark.
	for _, origin := range [][2]int{{0, 0}, {code.Size - 7, 0}, {0, code.Size - 7}} {
		for y := 0; y < 7; y++ {
			for x := 0; x < 7; x++ {
				distance := max(absInt(x-3), absInt(y-3))
				want := distance != 2
				if got := code.Module(origin[0]+x, origin[1]+y); got != want {
					t.Fatalf("finder at (%d,%d) module (%d,%d) = %v, want %v", origin[0], origin[1], x, y, got, want)
				}
			}
		}
	}
	// Separators: the row and column just outside each finder are light.
	for index := 0; index < 8; index++ {
		if code.Module(index, 7) || code.Module(7, index) {
			t.Errorf("top-left separator is not light at index %d", index)
		}
	}
	// Timing patterns alternate, starting dark at even coordinates.
	for index := 8; index < code.Size-8; index++ {
		if code.Module(index, 6) != (index%2 == 0) {
			t.Errorf("horizontal timing pattern is wrong at %d", index)
		}
		if code.Module(6, index) != (index%2 == 0) {
			t.Errorf("vertical timing pattern is wrong at %d", index)
		}
	}
	// The dark module is always dark.
	if !code.Module(8, code.Size-8) {
		t.Error("the dark module is light")
	}
	// Every non-function module is consumed by codewords plus remainder bits.
	spec := levelMSpecs[code.Version-1]
	isFunction := functionMap(code, spec)
	dataModules := 0
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if !isFunction[y][x] {
				dataModules++
			}
		}
	}
	if want := spec.totalCodewords*8 + spec.remainderBits; dataModules != want {
		t.Errorf("data modules = %d, want %d (total codewords * 8 + remainder bits)", dataModules, want)
	}
}

// TestPNGGeometry pins the module scale and quiet zone: a QR with too small a
// quiet zone is the classic "it renders but the scanner refuses it" bug.
func TestPNGGeometry(t *testing.T) {
	code, errEncode := Encode("HELLO WORLD")
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	const scale, quietZone = 5, 4
	encoded, errPNG := code.PNG(scale, quietZone)
	if errPNG != nil {
		t.Fatalf("png: %v", errPNG)
	}
	image := decodePNG(t, encoded)
	if want := (code.Size + 2*quietZone) * scale; image.Bounds().Dx() != want || image.Bounds().Dy() != want {
		t.Fatalf("image is %dx%d, want %dx%d", image.Bounds().Dx(), image.Bounds().Dy(), want, want)
	}
	// The quiet zone is white on all four sides, one module deep.
	for _, point := range [][2]int{{0, 0}, {image.Bounds().Dx() - 1, 0}, {0, image.Bounds().Dy() - 1}, {image.Bounds().Dx() - 1, image.Bounds().Dy() - 1}} {
		red, green, blue, _ := image.At(point[0], point[1]).RGBA()
		if red != 0xFFFF || green != 0xFFFF || blue != 0xFFFF {
			t.Errorf("quiet zone pixel %v is not white", point)
		}
	}
	// The default quiet zone is the standard four modules, and 0 means 0.
	if encoded, errDefault := code.PNG(0, -1); errDefault != nil {
		t.Fatalf("default png: %v", errDefault)
	} else if got := decodePNG(t, encoded).Bounds().Dx(); got != (code.Size+2*DefaultQuietZone)*DefaultScale {
		t.Errorf("default dimensions = %d, want %d", got, (code.Size+2*DefaultQuietZone)*DefaultScale)
	}
	if encoded, errNone := code.PNG(2, 0); errNone != nil {
		t.Fatalf("bare png: %v", errNone)
	} else if got := decodePNG(t, encoded).Bounds().Dx(); got != code.Size*2 {
		t.Errorf("quiet-zone-free dimensions = %d, want %d", got, code.Size*2)
	}
}

// TestDataURLShape pins the only image shape the host management page accepts.
func TestDataURLShape(t *testing.T) {
	code, errEncode := Encode("HELLO WORLD")
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	url, errURL := code.DataURL(4, 4)
	if errURL != nil {
		t.Fatalf("data url: %v", errURL)
	}
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("data url prefix = %q", url[:min(32, len(url))])
	}
	if _, errDecode := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/png;base64,")); errDecode != nil {
		t.Fatalf("data url payload is not base64: %v", errDecode)
	}
}

// TestOverCapacityIsAnError checks the failure mode: too much data must be a
// returned error, never a panic or a silently truncated symbol.
func TestOverCapacityIsAnError(t *testing.T) {
	tooLong := strings.Repeat("z", capacityBytes(maxVersion)+1)
	if _, errEncode := Encode(tooLong); errEncode == nil {
		t.Fatalf("encoding %d bytes succeeded, want an error above the version-%d capacity", len(tooLong), maxVersion)
	} else if !strings.Contains(errEncode.Error(), "capacity") {
		t.Errorf("error = %v, want it to name the capacity", errEncode)
	}
}

// assertRoundTrip renders the symbol and requires an INDEPENDENT decoder to
// return the identical string.
func assertRoundTrip(t *testing.T, code *Code, want string) {
	t.Helper()
	encoded, errPNG := code.PNG(4, 4)
	if errPNG != nil {
		t.Fatalf("png: %v", errPNG)
	}
	got := decodeWithJSR(t, encoded)
	if got != want {
		t.Fatalf("jsQR decoded %q, want %q", truncate(got), truncate(want))
	}
	// Logged so the acceptance evidence (an INDEPENDENT decoder reading the
	// rendered symbol back) is visible in `go test -v` output.
	t.Logf("version %d, mask %d, %d modules: jsQR decoded %q", code.Version, code.Mask, code.Size, truncate(got))
	t.Logf("  round trip: %d bytes in, %d bytes out, identical=%v", len(want), len(got), got == want)
}

// decodeWithJSR writes the PNG to a temporary file and asks the independent
// decoder what it sees.
func decodeWithJSR(t *testing.T, encoded []byte) string {
	t.Helper()
	node, errLook := exec.LookPath("node")
	if errLook != nil {
		t.Skip("node is not installed; the independent jsQR decode cannot run")
	}
	if _, errStat := os.Stat(filepath.Join(decoderModules, "jsqr")); errStat != nil {
		t.Skipf("jsQR is not installed at %s; the independent decode cannot run", decoderModules)
	}
	path := filepath.Join(t.TempDir(), "symbol.png")
	if errWrite := os.WriteFile(path, encoded, 0o600); errWrite != nil {
		t.Fatalf("write png: %v", errWrite)
	}
	command := exec.Command(node, filepath.Join("testdata", "decode.js"), path)
	command.Env = append(os.Environ(), "QR_NODE_MODULES="+decoderModules)
	output, errRun := command.CombinedOutput()
	if errRun != nil {
		t.Fatalf("jsQR could not decode the symbol: %v\n%s", errRun, output)
	}
	return strings.TrimRight(string(output), "\r\n")
}

// decodePNG decodes a rendered PNG for the geometry assertions.
func decodePNG(t *testing.T, encoded []byte) image.Image {
	t.Helper()
	decoded, errDecode := png.Decode(bytes.NewReader(encoded))
	if errDecode != nil {
		t.Fatalf("decode png: %v", errDecode)
	}
	return decoded
}

// functionMap rebuilds the function-module map for the geometry assertion. It
// mirrors drawFunctionPatterns; keeping it in the test means the production code
// cannot "prove" its own bookkeeping.
func functionMap(code *Code, spec versionSpec) [][]bool {
	isFunction := make([][]bool, code.Size)
	for row := range isFunction {
		isFunction[row] = make([]bool, code.Size)
	}
	mark := func(x, y int) {
		if x >= 0 && x < code.Size && y >= 0 && y < code.Size {
			isFunction[y][x] = true
		}
	}
	for index := 0; index < code.Size; index++ {
		mark(6, index)
		mark(index, 6)
	}
	for _, centre := range [][2]int{{3, 3}, {code.Size - 4, 3}, {3, code.Size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				mark(centre[0]+dx, centre[1]+dy)
			}
		}
	}
	for _, y := range spec.alignment {
		for _, x := range spec.alignment {
			if (x == 6 && y == 6) || (x == 6 && y == code.Size-7) || (x == code.Size-7 && y == 6) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					mark(x+dx, y+dy)
				}
			}
		}
	}
	mark(8, code.Size-8)
	for index := 0; index <= 8; index++ {
		mark(index, 8)
		mark(8, index)
	}
	for index := 0; index < 8; index++ {
		mark(code.Size-1-index, 8)
		mark(8, code.Size-1-index)
	}
	if code.Version >= 7 {
		for index := 0; index < 18; index++ {
			a := code.Size - 11 + index%3
			b := index / 3
			mark(a, b)
			mark(b, a)
		}
	}
	return isFunction
}

// truncate shortens a payload for a readable failure message while keeping the
// 144-byte raccoon URL fully visible in the acceptance evidence.
func truncate(value string) string {
	const limit = 200
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
