// Package qr encodes a string as an ISO/IEC 18004 QR Code symbol and renders it
// as a PNG image or a `data:image/png;base64,…` URL.
//
// Scope: byte mode, error-correction level M, versions 1–10 (the largest symbol
// that holds 200 bytes at level M), automatic selection of the SMALLEST version
// that fits, and the standard penalty-based mask selection. It exists because a
// CPA plugin's management page is dispatched as GET only — no forms, no scripts —
// so the one way to hand a user a scannable QR code is an inline image the page
// already carries.
//
// This is deliberately NOT a port of the bespoke 562-line encoder the TypeScript
// reference ships (`raccoon-qr.ts`): the reference's encoder exists only because
// its repo has no QR dependency, and a QR code only has to be scannable, never
// byte-identical to another implementation's output. What must be right is the
// encoding itself, so every part of it is covered by a test that decodes the
// rendered PNG with an INDEPENDENT decoder (jsQR, see qr_test.go).
package qr

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// maxVersion is the largest symbol this package builds. Level M holds 154 data
// codewords at version 8 and 216 at version 10, which is far more than the
// 144-byte login URL this package was written for; the table stops here on
// purpose rather than carrying the 30 unused versions' block structures.
const maxVersion = 10

// ecGroup is one run of equal-sized data blocks in a version's block structure.
type ecGroup struct {
	// count is how many blocks the group holds.
	count int
	// dataCodewords is the number of DATA codewords in each block of the group.
	dataCodewords int
}

// versionSpec is one level-M version description.
type versionSpec struct {
	// totalCodewords is the number of codewords the symbol carries in total.
	totalCodewords int
	// ecCodewordsPerBlock is the Reed-Solomon parity length of every block.
	ecCodewordsPerBlock int
	// groups lists the data blocks in wire order.
	groups []ecGroup
	// alignment holds the row/column centres of the alignment patterns; empty
	// for version 1, which has none.
	alignment []int
	// remainderBits is the number of zero bits appended after the final
	// codeword to fill the last data modules.
	remainderBits int
}

// levelMSpecs is the level-M block structure of versions 1–10.
var levelMSpecs = [maxVersion]versionSpec{
	{totalCodewords: 26, ecCodewordsPerBlock: 10, groups: []ecGroup{{1, 16}}},
	{totalCodewords: 44, ecCodewordsPerBlock: 16, groups: []ecGroup{{1, 28}}, alignment: []int{6, 18}, remainderBits: 7},
	{totalCodewords: 70, ecCodewordsPerBlock: 26, groups: []ecGroup{{1, 44}}, alignment: []int{6, 22}, remainderBits: 7},
	{totalCodewords: 100, ecCodewordsPerBlock: 18, groups: []ecGroup{{2, 32}}, alignment: []int{6, 26}, remainderBits: 7},
	{totalCodewords: 134, ecCodewordsPerBlock: 24, groups: []ecGroup{{2, 43}}, alignment: []int{6, 30}, remainderBits: 7},
	{totalCodewords: 172, ecCodewordsPerBlock: 16, groups: []ecGroup{{4, 27}}, alignment: []int{6, 34}, remainderBits: 7},
	{totalCodewords: 196, ecCodewordsPerBlock: 18, groups: []ecGroup{{4, 31}}, alignment: []int{6, 22, 38}},
	{totalCodewords: 242, ecCodewordsPerBlock: 22, groups: []ecGroup{{2, 38}, {2, 39}}, alignment: []int{6, 24, 42}},
	{totalCodewords: 292, ecCodewordsPerBlock: 22, groups: []ecGroup{{3, 36}, {2, 37}}, alignment: []int{6, 26, 46}},
	{totalCodewords: 346, ecCodewordsPerBlock: 26, groups: []ecGroup{{4, 43}, {1, 44}}, alignment: []int{6, 28, 50}},
}

// Code is one encoded QR symbol.
type Code struct {
	// Version is the symbol version, 1–10.
	Version int
	// Mask is the data mask actually applied, 0–7.
	Mask int
	// Size is the module count per side, 21 + 4*(Version-1).
	Size int
	// Modules is indexed [row][column]; true means a dark module. The symbol
	// carries no quiet zone — PNG adds one.
	Modules [][]bool
}

// Module reports whether the module at column x, row y is dark. Coordinates
// outside the symbol are reported light, which is what a quiet zone is.
func (c *Code) Module(x, y int) bool {
	if c == nil || y < 0 || y >= len(c.Modules) {
		return false
	}
	if x < 0 || x >= len(c.Modules[y]) {
		return false
	}
	return c.Modules[y][x]
}

// Encode returns the smallest level-M byte-mode symbol that holds text.
//
// The payload is encoded as UTF-8 bytes, exactly as the caller wrote them: this
// package performs no URL encoding of its own, because the raccoon login URL
// embeds an already-encoded `appname` and re-encoding it would change the URL
// the WeChat client opens.
func Encode(text string) (*Code, error) {
	data := []byte(text)
	version, spec, found := pickVersion(len(data))
	if !found {
		return nil, fmt.Errorf("qr: %d bytes exceed the version-%d level-M capacity of %d bytes",
			len(data), maxVersion, capacityBytes(maxVersion))
	}
	codewords := encodeCodewords(data, version, spec)
	return buildSymbol(version, spec, codewords), nil
}

// PNG renders the symbol as a PNG image.
//
// scale is the pixel edge of one module and defaults to 4 when it is not
// positive; quietZone is the light border in modules and defaults to 4 (the
// ISO/IEC 18004 minimum) when it is NEGATIVE. A quietZone of 0 is therefore
// honoured rather than replaced — a caller that wants no border gets none, and
// one that omits the argument gets the standard border.
func (c *Code) PNG(scale, quietZone int) ([]byte, error) {
	if c == nil || c.Size <= 0 || len(c.Modules) != c.Size {
		return nil, errors.New("qr: symbol is empty")
	}
	if scale <= 0 {
		scale = defaultScale
	}
	if quietZone < 0 {
		quietZone = defaultQuietZone
	}
	dimension := (c.Size + 2*quietZone) * scale
	canvas := image.NewRGBA(image.Rect(0, 0, dimension, dimension))
	light := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	dark := color.RGBA{R: 0x00, G: 0x00, B: 0x00, A: 0xFF}
	for y := 0; y < dimension; y++ {
		moduleY := y/scale - quietZone
		for x := 0; x < dimension; x++ {
			moduleX := x/scale - quietZone
			pixel := light
			if c.Module(moduleX, moduleY) {
				pixel = dark
			}
			canvas.SetRGBA(x, y, pixel)
		}
	}
	var buffer bytes.Buffer
	if errEncode := png.Encode(&buffer, canvas); errEncode != nil {
		return nil, fmt.Errorf("qr: encode PNG: %w", errEncode)
	}
	return buffer.Bytes(), nil
}

// DataURL renders PNG(scale, quietZone) as a `data:image/png;base64,…` URL.
//
// This is the only image shape the host's management page accepts
// (plugui.Image refuses anything else), and a data URL keeps the page free of a
// second request.
func (c *Code) DataURL(scale, quietZone int) (string, error) {
	encoded, errPNG := c.PNG(scale, quietZone)
	if errPNG != nil {
		return "", errPNG
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded), nil
}

// Default rendering parameters, exported so callers can be explicit about the
// values PNG falls back to.
const (
	// DefaultScale is the module edge in pixels.
	DefaultScale = 4
	// DefaultQuietZone is the light border in modules.
	DefaultQuietZone = 4
)

const (
	defaultScale      = DefaultScale
	defaultQuietZone  = DefaultQuietZone
	byteModeIndicator = 0x4
)

// pickVersion returns the smallest version whose data capacity holds size bytes,
// along with its block structure.
func pickVersion(size int) (int, versionSpec, bool) {
	for index, spec := range levelMSpecs {
		version := index + 1
		needed := 4 + characterCountBits(version) + 8*size
		if needed <= dataCodewordCount(spec)*8 {
			return version, spec, true
		}
	}
	return 0, versionSpec{}, false
}

// dataCodewordCount is the number of data (non-parity) codewords a version
// carries.
func dataCodewordCount(spec versionSpec) int {
	total := 0
	for _, group := range spec.groups {
		total += group.count * group.dataCodewords
	}
	return total
}

// capacityBytes is the largest byte-mode payload a version holds: its data bits
// minus the 4-bit mode indicator and the character-count indicator, rounded down
// to whole bytes (a partial byte is not usable).
func capacityBytes(version int) int {
	return (dataCodewordCount(levelMSpecs[version-1])*8 - 4 - characterCountBits(version)) / 8
}

// characterCountBits is the byte-mode character-count indicator width: 8 bits
// below version 10, 16 bits from version 10 up.
func characterCountBits(version int) int {
	if version < 10 {
		return 8
	}
	return 16
}

// encodeCodewords turns the payload into the final, interleaved codeword
// sequence: mode indicator, character count, data, terminator, pad codewords,
// then block-wise Reed-Solomon parity interleaved in the order the standard
// prescribes.
func encodeCodewords(data []byte, version int, spec versionSpec) []byte {
	capacityBits := dataCodewordCount(spec) * 8
	bits := make([]bool, 0, capacityBits)
	appendBits(&bits, byteModeIndicator, 4)
	appendBits(&bits, uint32(len(data)), characterCountBits(version))
	for _, value := range data {
		appendBits(&bits, uint32(value), 8)
	}
	// Terminator: up to four zero bits, fewer when the payload nearly fills the
	// symbol.
	terminator := 4
	if remaining := capacityBits - len(bits); remaining < terminator {
		terminator = remaining
	}
	appendBits(&bits, 0, terminator)
	// Pad to a codeword boundary, then alternate 0xEC / 0x11.
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	for index := 0; len(bits) < capacityBits; index++ {
		pad := byte(0xEC)
		if index%2 == 1 {
			pad = 0x11
		}
		appendBits(&bits, uint32(pad), 8)
	}

	dataCodewords := make([]byte, len(bits)/8)
	for index, bit := range bits {
		if bit {
			dataCodewords[index/8] |= 1 << (7 - uint(index%8))
		}
	}

	// Split into blocks in wire order, then compute each block's parity.
	blocks := make([][]byte, 0, 8)
	position := 0
	for _, group := range spec.groups {
		for block := 0; block < group.count; block++ {
			blocks = append(blocks, dataCodewords[position:position+group.dataCodewords])
			position += group.dataCodewords
		}
	}
	parities := make([][]byte, len(blocks))
	divisor := rsDivisor(spec.ecCodewordsPerBlock)
	for index, block := range blocks {
		parities[index] = rsRemainder(block, divisor)
	}

	// Interleave: one codeword per block, in block order, for the longest
	// data block; then the same for the parity codewords.
	out := make([]byte, 0, spec.totalCodewords)
	longest := 0
	for _, block := range blocks {
		if len(block) > longest {
			longest = len(block)
		}
	}
	for index := 0; index < longest; index++ {
		for _, block := range blocks {
			if index < len(block) {
				out = append(out, block[index])
			}
		}
	}
	for index := 0; index < spec.ecCodewordsPerBlock; index++ {
		for _, parity := range parities {
			out = append(out, parity[index])
		}
	}
	return out
}

// appendBits appends the low count bits of value, most significant first.
func appendBits(bits *[]bool, value uint32, count int) {
	for shift := count - 1; shift >= 0; shift-- {
		*bits = append(*bits, (value>>uint(shift))&1 == 1)
	}
}

// buildSymbol draws the function patterns, places the codewords and picks the
// mask with the lowest penalty score.
func buildSymbol(version int, spec versionSpec, codewords []byte) *Code {
	size := 21 + 4*(version-1)
	symbol := &Code{Version: version, Size: size}
	symbol.Modules = make([][]bool, size)
	isFunction := make([][]bool, size)
	for row := range symbol.Modules {
		symbol.Modules[row] = make([]bool, size)
		isFunction[row] = make([]bool, size)
	}
	drawFunctionPatterns(symbol, spec, isFunction)
	drawCodewords(symbol, isFunction, codewords, spec)

	bestMask := 0
	bestPenalty := -1
	for mask := 0; mask < 8; mask++ {
		applyMask(symbol, isFunction, mask)
		drawFormatBits(symbol, mask)
		penalty := penaltyScore(symbol)
		applyMask(symbol, isFunction, mask) // XOR is its own inverse
		if bestPenalty < 0 || penalty < bestPenalty {
			bestPenalty = penalty
			bestMask = mask
		}
	}
	applyMask(symbol, isFunction, bestMask)
	drawFormatBits(symbol, bestMask)
	symbol.Mask = bestMask
	return symbol
}

// drawFunctionPatterns reserves and draws every non-data module: the three finder
// patterns with their separators, the timing patterns, the alignment patterns,
// the dark module and the format-information areas.
func drawFunctionPatterns(symbol *Code, spec versionSpec, isFunction [][]bool) {
	size := symbol.Size

	// Timing patterns first; the finders drawn afterwards own the overlapping
	// cells, which is what keeps the pattern correct at the corners.
	for index := 0; index < size; index++ {
		setFunction(symbol, isFunction, 6, index, index%2 == 0)
		setFunction(symbol, isFunction, index, 6, index%2 == 0)
	}

	// Finder patterns plus their separators.
	for _, centre := range [][2]int{{3, 3}, {size - 4, 3}, {3, size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x := centre[0] + dx
				y := centre[1] + dy
				if x < 0 || x >= size || y < 0 || y >= size {
					continue
				}
				distance := max(absInt(dx), absInt(dy))
				setFunction(symbol, isFunction, x, y, distance != 2 && distance != 4)
			}
		}
	}

	// Alignment patterns, skipping the three finder corners.
	for _, y := range spec.alignment {
		for _, x := range spec.alignment {
			if (x == 6 && y == 6) || (x == 6 && y == size-7) || (x == size-7 && y == 6) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					dark := max(absInt(dx), absInt(dy)) != 1
					setFunction(symbol, isFunction, x+dx, y+dy, dark)
				}
			}
		}
	}

	// The dark module, always at (row size-8, column 8).
	setFunction(symbol, isFunction, 8, size-8, true)

	// Reserve the format-information areas (their values are written per mask)
	// and the version-information blocks.
	for index := 0; index <= 8; index++ {
		reserve(symbol, isFunction, index, 8)
		reserve(symbol, isFunction, 8, index)
	}
	for index := 0; index < 8; index++ {
		reserve(symbol, isFunction, size-1-index, 8)
		reserve(symbol, isFunction, 8, size-1-index)
	}
	if symbol.Version >= 7 {
		for index := 0; index < 18; index++ {
			a := size - 11 + index%3
			b := index / 3
			reserve(symbol, isFunction, a, b)
			reserve(symbol, isFunction, b, a)
		}
		drawVersionBits(symbol)
	}
}

// drawVersionBits writes the 18-bit version information of versions 7 and up.
func drawVersionBits(symbol *Code) {
	size := symbol.Size
	remainder := symbol.Version
	for index := 0; index < 12; index++ {
		remainder = (remainder << 1) ^ ((remainder >> 11) * 0x1F25)
	}
	bits := symbol.Version<<12 | remainder
	for index := 0; index < 18; index++ {
		bit := (bits>>uint(index))&1 == 1
		a := size - 11 + index%3
		b := index / 3
		symbol.Modules[b][a] = bit
		symbol.Modules[a][b] = bit
	}
}

// drawFormatBits writes both copies of the 15-bit format information for one
// mask, plus the always-dark module.
//
// Error-correction level M is 0b00, so the five data bits are just the mask.
func drawFormatBits(symbol *Code, mask int) {
	data := mask // level M << 3 | mask
	remainder := data
	for index := 0; index < 10; index++ {
		remainder = (remainder << 1) ^ ((remainder >> 9) * 0x537)
	}
	bits := (data<<10 | remainder) ^ 0x5412
	size := symbol.Size

	// First copy: around the top-left finder.
	for index := 0; index <= 5; index++ {
		symbol.Modules[index][8] = bitAt(bits, index)
	}
	symbol.Modules[7][8] = bitAt(bits, 6)
	symbol.Modules[8][8] = bitAt(bits, 7)
	symbol.Modules[8][7] = bitAt(bits, 8)
	for index := 9; index < 15; index++ {
		symbol.Modules[8][14-index] = bitAt(bits, index)
	}

	// Second copy: split between the bottom-left and top-right finders.
	for index := 0; index < 8; index++ {
		symbol.Modules[8][size-1-index] = bitAt(bits, index)
	}
	for index := 8; index < 15; index++ {
		symbol.Modules[size-15+index][8] = bitAt(bits, index)
	}
	symbol.Modules[size-8][8] = true
}

// bitAt reports bit index of a 15-bit format value, least significant first.
func bitAt(value, index int) bool { return (value>>uint(index))&1 == 1 }

// drawCodewords places the interleaved codewords and the remainder bits in the
// standard two-module zigzag, moving upward from the bottom-right and skipping
// the vertical timing column.
func drawCodewords(symbol *Code, isFunction [][]bool, codewords []byte, spec versionSpec) {
	size := symbol.Size
	totalBits := spec.totalCodewords*8 + spec.remainderBits
	index := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vertical := 0; vertical < size; vertical++ {
			upward := ((right + 1) & 2) == 0
			for offset := 0; offset < 2; offset++ {
				x := right - offset
				y := vertical
				if upward {
					y = size - 1 - vertical
				}
				if isFunction[y][x] || index >= totalBits {
					continue
				}
				if index < len(codewords)*8 {
					symbol.Modules[y][x] = (codewords[index/8]>>(7-uint(index%8)))&1 == 1
				}
				index++
			}
		}
	}
}

// applyMask XORs the data mask over every non-function module.
func applyMask(symbol *Code, isFunction [][]bool, mask int) {
	for y := 0; y < symbol.Size; y++ {
		for x := 0; x < symbol.Size; x++ {
			if isFunction[y][x] {
				continue
			}
			if maskBit(mask, x, y) {
				symbol.Modules[y][x] = !symbol.Modules[y][x]
			}
		}
	}
}

// maskBit evaluates one of the eight standard data-mask conditions.
func maskBit(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	case 7:
		return ((x+y)%2+x*y%3)%2 == 0
	default:
		return false
	}
}

// Penalty weights from ISO/IEC 18004 table 24.
const (
	penaltyRunLength   = 3
	penaltyBlock       = 3
	penaltyFinder      = 40
	penaltyBalance     = 10
	penaltyRunOverflow = 5
)

// penaltyScore implements the four standard mask-evaluation rules.
func penaltyScore(symbol *Code) int {
	size := symbol.Size
	score := 0
	dark := 0

	// Rule 1: runs of five or more same-coloured modules, in rows and columns.
	for y := 0; y < size; y++ {
		run := 1
		for x := 1; x < size; x++ {
			if symbol.Modules[y][x] == symbol.Modules[y][x-1] {
				run++
				continue
			}
			if run >= 5 {
				score += penaltyRunLength + (run - 5)
			}
			run = 1
		}
		if run >= 5 {
			score += penaltyRunLength + (run - 5)
		}
	}
	for x := 0; x < size; x++ {
		run := 1
		for y := 1; y < size; y++ {
			if symbol.Modules[y][x] == symbol.Modules[y-1][x] {
				run++
				continue
			}
			if run >= 5 {
				score += penaltyRunLength + (run - 5)
			}
			run = 1
		}
		if run >= 5 {
			score += penaltyRunLength + (run - 5)
		}
	}

	// Rule 2: every 2x2 block of one colour.
	for y := 0; y < size-1; y++ {
		for x := 0; x < size-1; x++ {
			value := symbol.Modules[y][x]
			if symbol.Modules[y][x+1] == value && symbol.Modules[y+1][x] == value && symbol.Modules[y+1][x+1] == value {
				score += penaltyBlock
			}
		}
	}

	// Rule 3: the 1:1:3:1:1 finder-like pattern with four light modules on one
	// side, in rows and columns.
	for y := 0; y < size; y++ {
		for x := 0; x+10 < size; x++ {
			if finderLike(symbol.Modules[y][x : x+11]) {
				score += penaltyFinder
			}
		}
	}
	for x := 0; x < size; x++ {
		column := make([]bool, size)
		for y := 0; y < size; y++ {
			column[y] = symbol.Modules[y][x]
		}
		for y := 0; y+10 < size; y++ {
			if finderLike(column[y : y+11]) {
				score += penaltyFinder
			}
		}
	}

	// Rule 4: deviation of the dark-module proportion from 50%.
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if symbol.Modules[y][x] {
				dark++
			}
		}
	}
	total := size * size
	deviation := absInt(dark*20 - total*10)
	steps := (deviation + total - 1) / total
	if steps > 0 {
		score += (steps - 1) * penaltyBalance
	}
	return score
}

// finderLike reports whether an 11-module window matches the finder-like pattern
// that rule 3 penalises: dark-light-dark-dark-dark-light-dark plus four light
// modules on either side.
func finderLike(window []bool) bool {
	pattern := [11]bool{true, false, true, true, true, false, true, false, false, false, false}
	reversed := [11]bool{false, false, false, false, true, false, true, true, true, false, true}
	matches := true
	for index, want := range pattern {
		if window[index] != want {
			matches = false
			break
		}
	}
	if matches {
		return true
	}
	for index, want := range reversed {
		if window[index] != want {
			return false
		}
	}
	return true
}

// setFunction writes a module and marks it as reserved.
func setFunction(symbol *Code, isFunction [][]bool, x, y int, dark bool) {
	if y < 0 || y >= symbol.Size || x < 0 || x >= symbol.Size {
		return
	}
	symbol.Modules[y][x] = dark
	isFunction[y][x] = true
}

// reserve marks a module as a function module without changing its colour.
func reserve(symbol *Code, isFunction [][]bool, x, y int) {
	if y < 0 || y >= symbol.Size || x < 0 || x >= symbol.Size {
		return
	}
	isFunction[y][x] = true
}

// absInt is the absolute value of an int, spelled out to keep the drawing code
// free of imports.
func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// ── Reed-Solomon over GF(256), primitive polynomial 0x11D ──

var (
	gfExp [512]byte
	gfLog [256]int
)

func init() {
	value := 1
	for index := 0; index < 255; index++ {
		gfExp[index] = byte(value)
		gfLog[value] = index
		value <<= 1
		if value&0x100 != 0 {
			value ^= 0x11D
		}
	}
	for index := 255; index < 512; index++ {
		gfExp[index] = gfExp[index-255]
	}
}

// gfMultiply multiplies two field elements.
func gfMultiply(left, right byte) byte {
	if left == 0 || right == 0 {
		return 0
	}
	return gfExp[gfLog[left]+gfLog[right]]
}

// rsDivisor builds the Reed-Solomon generator polynomial of the given degree,
// with the leading coefficient omitted (it is always 1).
func rsDivisor(degree int) []byte {
	result := make([]byte, degree)
	result[degree-1] = 1
	root := byte(1)
	for index := 0; index < degree; index++ {
		for position := 0; position < degree; position++ {
			result[position] = gfMultiply(result[position], root)
			if position+1 < degree {
				result[position] ^= result[position+1]
			}
		}
		root = gfMultiply(root, 0x02)
	}
	return result
}

// rsRemainder returns the parity codewords of one block: the remainder of
// data * x^degree divided by the generator polynomial.
func rsRemainder(data, divisor []byte) []byte {
	result := make([]byte, len(divisor))
	for _, value := range data {
		factor := value ^ result[0]
		copy(result, result[1:])
		result[len(result)-1] = 0
		for index, coefficient := range divisor {
			result[index] ^= gfMultiply(coefficient, factor)
		}
	}
	return result
}
