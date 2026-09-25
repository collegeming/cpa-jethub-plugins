package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/openai"
)

// Ports llm-adapter.ts:212-236 (buildDsmlSystemPrompt), llm-adapter.ts:464-725
// (the DSML constants, tryParseScalar / parseDsmlInvoke / parseDsmlToolCalls and
// the DsmlContentExtractor streaming state machine).
//
// DSML is the native tool-call dialect of deepseek-v4-flash / deepseek-v4-pro:
// when the OpenAI `tools` array is not sent, the model writes tool calls inline
// into `delta.content`:
//
//	<｜DSML｜tool_calls><｜DSML｜invoke name="bash">
//	<｜DSML｜parameter name="command" string="true">ls</｜DSML｜parameter>
//	</｜DSML｜invoke></｜DSML｜tool_calls>
//
// The tags use the FULL-WIDTH vertical bar `｜` (U+FF5C), not ASCII `|`; a
// half-width tag is ordinary visible text and must never be treated as DSML.
//
// Two upstream behaviours are worth recording because they shape the parser:
//
//   - One-shot packaging of standard `tool_calls.arguments` leaves the SSE
//     connection silent for tens of seconds; the APIG gateway drops it at ~60s
//     idle. DSML keeps bytes flowing on every chunk, which is the whole point of
//     this mode.
//   - The model often mis-tags numeric / array / object parameters with
//     string="true", so values are coerced back to their JSON types (see
//     tryParseScalar below) to survive tool-schema validation.

// DSML tag literals. The vertical bars are U+FF5C, mirroring the model's own
// output; do not "fix" them to ASCII.
const (
	// DsmlToolCallsOpen opens a DSML tool-call block.
	DsmlToolCallsOpen = "<｜DSML｜tool_calls>"

	dsmlToolCallsClose  = "</｜DSML｜tool_calls>"
	dsmlInvokeOpenPfx   = "<｜DSML｜invoke"
	dsmlInvokeClose     = "</｜DSML｜invoke>"
	dsmlParamOpenPfx    = "<｜DSML｜parameter"
	dsmlParamClose      = "</｜DSML｜parameter>"
	dsmlThoughtOpen     = "<thought>"
	dsmlThoughtCloseTag = "</thought>"
)

// Parser states: normal scans for an open tag, in-thought drains reasoning until
// `</thought>`, in-dsml waits for `</｜DSML｜tool_calls>` before parsing.
const (
	dsmlStateNormal = iota
	dsmlStateThought
	dsmlStateToolCalls
)

// errDsmlInvalidRawJSON guards dsmlRawJSON against ever emitting malformed JSON;
// the arguments builder falls back to `null` for that entry if it fires.
var errDsmlInvalidRawJSON = errors.New("dsml: invalid raw JSON fragment")

// dsmlToolSchema mirrors the exact JSON projection upstream produces for the
// prompt's tool list. Field declaration order is load-bearing: encoding/json
// emits struct fields in declaration order, which is what keeps the rendered
// prompt byte-identical to `JSON.stringify(..., null, 2)` in TypeScript
// (name -> description -> parameters). A map would sort keys alphabetically and
// silently reorder the schema.
type dsmlToolSchema struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// DsmlStreamParser incrementally extracts DSML tool calls from assistant
// `delta.content` text. Ports DsmlContentExtractor (llm-adapter.ts:620-725).
//
// The parser is a three-state machine over an internal buffer, so a DSML block
// that straddles SSE chunks is neither emitted twice nor lost. Text that cannot
// begin a tag is returned immediately; only a suffix that is a proper prefix of
// an open tag (`<tho`, `<｜DSML｜tool_`) is retained until more data arrives.
//
// A parser is single-goroutine state; the caller must serialise Feed/Flush.
type DsmlStreamParser struct {
	buf       string
	state     int
	reasoning strings.Builder
	emitted   int // tool calls already emitted, used for the wire `index`
}

// NewDsmlStreamParser returns a parser in the initial (normal) state.
func NewDsmlStreamParser() *DsmlStreamParser {
	return &DsmlStreamParser{}
}

// BuildDsmlSystemPrompt renders the system message that teaches the model the
// native DSML tool-call syntax. Ports buildDsmlSystemPrompt (llm-adapter.ts:212).
// Returns "" when tools is empty.
func BuildDsmlSystemPrompt(tools []openai.Tool) string {
	if len(tools) == 0 {
		return ""
	}
	projected := make([]dsmlToolSchema, 0, len(tools))
	for _, tool := range tools {
		projected = append(projected, dsmlToolSchema{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	toolJSON := marshalDSMLJSON(projected, true)

	// The prompt body is verbatim from upstream, including the Chinese wording
	// and the literal worked example. Do not translate or reformat it: it is
	// model-facing text whose exact shape was tuned against real traffic.
	return strings.Join([]string{
		"以下是你可用的工具及其 JSON Schema。当需要调用工具完成任务时，",
		"必须使用原生 DSML 工具调用语法输出，格式如下：",
		DsmlToolCallsOpen + `<｜DSML｜invoke name="工具名"><｜DSML｜parameter name="参数名" string="true">参数值</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`,
		"",
		"规则：",
		"- 工具名必须是下面列表中的 name。",
		"- 每个参数用一个 <｜DSML｜parameter> 标签包裹，参数值放在标签之间。",
		`- 字符串参数加 string="true" 属性；对象/数组/数字/布尔参数不要加该属性。`,
		"- 一次可以输出多个 <｜DSML｜invoke> 调用（工具可以并行）。",
		"- 文件内容请一次性完整写入单个 write 调用的 content 参数，不要拆分或省略。",
		"",
		"工具列表（JSON Schema）：",
		toolJSON,
	}, "\n")
}

// Feed consumes one content delta. It returns the visible text that should be
// forwarded to the client (DSML blocks and <thought> blocks removed) and any
// tool calls that became complete during this delta.
//
// `<thought>` handling matches upstream: the delimiters are consumed and their
// content is collected as reasoning instead of visible text (see
// DrainReasoning). Upstream routes that reasoning to the Think region; this
// signature has no reasoning return value, so the integration must call
// DrainReasoning after Feed to avoid dropping it.
func (p *DsmlStreamParser) Feed(delta string) (visible string, calls []openai.ToolCall) {
	var out strings.Builder
	if delta != "" {
		p.buf += delta
	}
	for {
		if p.state == dsmlStateNormal {
			thoughtIdx := strings.Index(p.buf, dsmlThoughtOpen)
			dsmlIdx := strings.Index(p.buf, DsmlToolCallsOpen)
			openIdx := -1
			nextState := dsmlStateThought
			switch {
			case thoughtIdx != -1 && (dsmlIdx == -1 || thoughtIdx < dsmlIdx):
				openIdx, nextState = thoughtIdx, dsmlStateThought
			case dsmlIdx != -1:
				openIdx, nextState = dsmlIdx, dsmlStateToolCalls
			}
			if openIdx == -1 {
				// No complete open tag. Hold back only a suffix that could still
				// grow into one; everything else is visible text and is flushed
				// now so short replies are not delayed behind the buffer.
				keep := longestOpenPrefixTail(p.buf, []string{dsmlThoughtOpen, DsmlToolCallsOpen})
				switch {
				case keep == 0:
					out.WriteString(p.buf)
					p.buf = ""
				case len(p.buf) > keep:
					cut := len(p.buf) - keep
					out.WriteString(p.buf[:cut])
					p.buf = p.buf[cut:]
				}
				return out.String(), calls
			}
			if openIdx > 0 {
				out.WriteString(p.buf[:openIdx])
			}
			p.buf = p.buf[openIdx:]
			openLen := len(dsmlThoughtOpen)
			if nextState == dsmlStateToolCalls {
				openLen = len(DsmlToolCallsOpen)
			}
			p.buf = p.buf[openLen:]
			p.state = nextState
			continue
		}

		if p.state == dsmlStateThought {
			closeIdx := strings.Index(p.buf, dsmlThoughtCloseTag)
			if closeIdx == -1 {
				keep := longestOpenPrefixTail(p.buf, []string{dsmlThoughtCloseTag})
				switch {
				case keep == 0:
					p.reasoning.WriteString(p.buf)
					p.buf = ""
				case len(p.buf) > keep:
					cut := len(p.buf) - keep
					p.reasoning.WriteString(p.buf[:cut])
					p.buf = p.buf[cut:]
				}
				return out.String(), calls
			}
			if closeIdx > 0 {
				p.reasoning.WriteString(p.buf[:closeIdx])
			}
			p.buf = p.buf[closeIdx+len(dsmlThoughtCloseTag):]
			p.state = dsmlStateNormal
			continue
		}

		// dsmlStateToolCalls: only a complete block is parsed; upstream parses
		// nothing until the closing tag shows up, however fragmented it is.
		closeIdx := strings.Index(p.buf, dsmlToolCallsClose)
		if closeIdx == -1 {
			return out.String(), calls
		}
		block := p.buf[:closeIdx+len(dsmlToolCallsClose)]
		parsed, ok := parseDsmlToolCalls(block, p.emitted)
		if !ok {
			// Malformed block: release it as plain text rather than swallowing
			// content the user would otherwise never see.
			out.WriteString(block)
		} else {
			calls = append(calls, parsed...)
			p.emitted += len(parsed)
		}
		p.buf = p.buf[closeIdx+len(dsmlToolCallsClose):]
		p.state = dsmlStateNormal
	}
}

// Flush returns any remaining buffered text at end of stream. Upstream never
// leaves a parsed-but-unreported call behind (calls are emitted the moment their
// closing tag arrives), so the calls result is always empty; it is part of the
// signature so the caller can treat Feed and Flush uniformly.
func (p *DsmlStreamParser) Flush() (visible string, calls []openai.ToolCall) {
	remaining := p.buf
	p.buf = ""
	switch p.state {
	case dsmlStateThought:
		// Unclosed thought: stay on the reasoning channel so it cannot leak
		// into the visible body.
		p.reasoning.WriteString(remaining)
		p.state = dsmlStateNormal
		return "", nil
	case dsmlStateToolCalls:
		// Unclosed DSML block: the opener was consumed on entry, so put it back
		// to keep the released text readable instead of dumping a fragment that
		// is missing its opening tag.
		p.state = dsmlStateNormal
		return DsmlToolCallsOpen + remaining, nil
	default:
		p.state = dsmlStateNormal
		return remaining, nil
	}
}

// DrainReasoning returns the `<thought>` content seen since the previous drain
// and clears the accumulator. Upstream streams this to the Think region (it
// never reaches the visible body). The required Feed/Flush signatures cannot
// carry it, so the integration must call this after each Feed/Flush; doing so
// reproduces the upstream content/reasoning split exactly.
func (p *DsmlStreamParser) DrainReasoning() string {
	out := p.reasoning.String()
	p.reasoning.Reset()
	return out
}

// ParseDsmlToolCalls extracts tool calls from a complete, non-streamed assistant
// message. It returns the text with DSML blocks removed plus the calls.
//
// It reuses the streaming state machine on purpose: the streaming path is the
// one exercised by live traffic, and this guarantees both paths agree on a given
// input (including malformed blocks, which are released as visible text).
func ParseDsmlToolCalls(text string) (visible string, calls []openai.ToolCall) {
	if text == "" {
		return "", nil
	}
	parser := NewDsmlStreamParser()
	head, headCalls := parser.Feed(text)
	tail, tailCalls := parser.Flush()
	calls = append(headCalls, tailCalls...)
	return head + tail, calls
}

// parseDsmlToolCalls parses one closed DSML block into tool calls, assigning
// wire indices consecutively from `startIdx`. ok is false when an invoke has no
// parseable `name="..."`, mirroring upstream returning undefined so the caller
// can fall back to plain text.
func parseDsmlToolCalls(block string, startIdx int) (calls []openai.ToolCall, ok bool) {
	// block is `<｜DSML｜tool_calls>...invokes...</｜DSML｜tool_calls>`.
	inner := block
	inner = strings.TrimPrefix(inner, DsmlToolCallsOpen)
	inner = strings.TrimSuffix(inner, dsmlToolCallsClose)

	idx := startIdx
	for cursor := 0; ; {
		openStart := strings.Index(inner[cursor:], dsmlInvokeOpenPfx)
		if openStart == -1 {
			break
		}
		openStart += cursor
		openEnd := strings.IndexByte(inner[openStart:], '>')
		if openEnd == -1 {
			break
		}
		openEnd += openStart
		closeStart := strings.Index(inner[openEnd+1:], dsmlInvokeClose)
		if closeStart == -1 {
			break
		}
		closeStart += openEnd + 1
		invokeBlock := inner[openStart : closeStart+len(dsmlInvokeClose)]
		name, arguments, parsed := parseDsmlInvoke(invokeBlock)
		if !parsed {
			return nil, false
		}
		call := openai.ToolCall{
			Index: dsmlIntPtr(idx),
			ID:    newDsmlCallID(),
			Type:  "function",
			Function: openai.ToolCallFunction{
				Name:      name,
				Arguments: arguments,
			},
		}
		calls = append(calls, call)
		idx++
		cursor = closeStart + len(dsmlInvokeClose)
	}
	return calls, true
}

// parseDsmlInvoke parses one invoke block into its function name and the
// JSON-encoded arguments object. Parsed is false when no `name="..."` attribute
// exists. Ports parseDsmlInvoke (llm-adapter.ts:520).
func parseDsmlInvoke(block string) (name string, arguments string, parsed bool) {
	// name="..." — first attribute match anywhere in the block.
	value, hasName := dsmlAttrValue(block, "name")
	if !hasName {
		return "", "", false
	}
	name = value

	// Parameters are collected in emission order. Upstream accumulates them
	// into a JS object, whose insertion order is what JSON.stringify preserves;
	// encoding/json would sort a Go map alphabetically, so the object text is
	// assembled entry by entry instead. Assigning into the JS object also means
	// a repeated parameter name overwrites in place: first position, last value.
	entries := make([]string, 0, 4)
	slot := make(map[string]int, 4)
	for cursor := 0; ; {
		openStart := strings.Index(block[cursor:], dsmlParamOpenPfx)
		if openStart == -1 {
			break
		}
		openStart += cursor
		openEnd := strings.IndexByte(block[openStart:], '>')
		if openEnd == -1 {
			break
		}
		openEnd += openStart
		openTag := block[openStart : openEnd+1]
		paramName, hasParamName := dsmlAttrValue(openTag, "name")
		if !hasParamName {
			cursor = openEnd + 1
			continue
		}
		closeStart := strings.Index(block[openEnd+1:], dsmlParamClose)
		if closeStart == -1 {
			break
		}
		closeStart += openEnd + 1
		value := block[openEnd+1 : closeStart]
		encoded := marshalDSMLString(paramName) + ":" + marshalDSMLValue(pickParamValue(openTag, value))
		if at, seen := slot[paramName]; seen {
			entries[at] = encoded
		} else {
			slot[paramName] = len(entries)
			entries = append(entries, encoded)
		}
		cursor = closeStart + len(dsmlParamClose)
	}
	return name, "{" + strings.Join(entries, ",") + "}", true
}

// pickParamValue applies upstream's string="true" rule (llm-adapter.ts:545-559):
//
//	string="true"      -> tryParseScalar (coerce scalars/arrays/objects back to
//	                      their native JSON types, otherwise keep the raw string)
//	no string="true"   -> JSON.parse; a string result is run through
//	                      tryParseScalar once more, an unparseable value is kept
//	                      raw, and a quoted number such as "840" ends up as the
//	                      number 840.
func pickParamValue(openTag, value string) any {
	if dsmlAttrContains(openTag, "string", "true") {
		return tryParseScalar(value)
	}
	parsed, ok := dsmlJSONDecode(value)
	if !ok {
		return value
	}
	if s, isString := parsed.(string); isString {
		return tryParseScalar(s)
	}
	// Not a string, so the value is already the right JSON type. Re-emit the
	// original fragment rather than the decoded Go value: see dsmlRawJSON.
	if raw, isRaw := dsmlCompactJSON(value); isRaw {
		return raw
	}
	return parsed
}

// tryParseScalar is upstream's lenient coercion (llm-adapter.ts:484-518). It
// exists because the model mis-tags numbers, booleans, arrays and objects as
// string="true"; without this, `offset="1304"` or a JSON-encoded `todos` array
// fails tool-schema validation.
//
// Order matters and is reproduced exactly:
//
//  1. "" stays ""; a value that is only whitespace is returned untouched.
//
//  2. null / true / false literals.
//
//  3. a whole quoted JSON string literal is decoded and coerced recursively
//     (deepseek-v4-pro wraps numbers, e.g. "840").
//
//  4. integer / float literals only (a partial match such as "v1.2" or "123abc"
//     is deliberately left alone).
//
//  5. a value starting with [ or { is JSON-decoded when valid.
//
//     - Anything else is returned as the ORIGINAL string, whitespace included;
//     only the numeric branch trims, matching JS `Number()` semantics.
//     - Divergence: upstream uses JS `Number()`, which also accepts hex (`0x10`),
//     exponent-only forms (`1e5`) and `Infinity`. The literal gates below accept
//     only the decimal integer / float shapes a JSON number can take, because
//     the alternatives have no faithful Go equivalent on the wire.
//     - Divergence (numbers only, same JSON type): upstream's `Number(...)`
//     goes through float64 and is re-serialised by JSON.stringify, so `1.50`
//     becomes `1.5` and a 19-digit integer loses precision
//     (`12345678901234567890` -> `12345678901234567000`). Here the literal is
//     kept as a json.Number, which preserves the model's exact digits.
//     - Divergence (array/object values): upstream re-serialises the parsed
//     value, normalising number literals inside it (`1e5` -> `100000`,
//     `2.0` -> `2`). Here the fragment is only compacted, so those literals
//     stay as written. Key order is preserved by both.
func tryParseScalar(value string) any {
	if value == "" {
		return ""
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value
	}
	switch trimmed {
	case "null":
		return nil
	case "true":
		return true
	case "false":
		return false
	}
	if dsmlIsQuotedString(trimmed) {
		if decoded, ok := dsmlJSONDecode(trimmed); ok {
			if s, isString := decoded.(string); isString {
				return tryParseScalar(s)
			}
			return decoded
		}
	}
	if dsmlIsIntegerLiteral(trimmed) || dsmlIsFloatLiteral(trimmed) {
		return dsmlCanonicalNumber(trimmed)
	}
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		if raw, ok := dsmlCompactJSON(trimmed); ok {
			return raw
		}
	}
	return value
}

// dsmlRawJSON is an already-valid JSON fragment spliced into the arguments
// object verbatim.
//
// Upstream builds a JS object and lets JSON.stringify re-serialise it, which
// preserves key insertion order. A Go map would sort keys alphabetically, so
// object/array parameters are carried as raw fragments instead: key order and
// numeric literals survive exactly as the model wrote them.
type dsmlRawJSON string

// MarshalJSON writes the fragment unchanged; it was validated on construction.
func (r dsmlRawJSON) MarshalJSON() ([]byte, error) {
	if !json.Valid([]byte(r)) {
		return nil, errDsmlInvalidRawJSON
	}
	return []byte(r), nil
}

// dsmlCompactJSON validates a JSON fragment and removes insignificant
// whitespace, the closest Go equivalent of JSON.parse followed by
// JSON.stringify that keeps key order.
func dsmlCompactJSON(s string) (dsmlRawJSON, bool) {
	var buf bytes.Buffer
	if errCompact := json.Compact(&buf, []byte(s)); errCompact != nil {
		return "", false
	}
	return dsmlRawJSON(buf.String()), true
}

// dsmlCanonicalNumber renders a numeric literal as a JSON number, dropping
// redundant leading zeros ("01304" -> 1304) that JS `Number()` tolerates but
// JSON does not.
func dsmlCanonicalNumber(literal string) json.Number {
	sign := ""
	body := literal
	if strings.HasPrefix(body, "-") {
		sign, body = "-", body[1:]
	}
	intPart, fracPart := body, ""
	if dot := strings.IndexByte(body, '.'); dot >= 0 {
		intPart, fracPart = body[:dot], body[dot+1:]
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	if fracPart != "" {
		return json.Number(sign + intPart + "." + fracPart)
	}
	return json.Number(sign + intPart)
}

// dsmlAttrValue returns the value of the first `key="..."` attribute in tag.
// Upstream matches /key\s*=\s*"([^"]*)"/ over the whole text, so the first
// occurrence anywhere wins (even embedded in a longer attribute name) and an
// unterminated quote runs to the end of the input. Both quirks are preserved.
func dsmlAttrValue(tag, key string) (string, bool) {
	for cursor := 0; cursor < len(tag); {
		relative := strings.Index(tag[cursor:], key)
		if relative == -1 {
			return "", false
		}
		probe := cursor + relative + len(key)
		cursor = probe
		for probe < len(tag) && isDsmlSpace(tag[probe]) {
			probe++
		}
		if probe >= len(tag) || tag[probe] != '=' {
			continue
		}
		probe++
		for probe < len(tag) && isDsmlSpace(tag[probe]) {
			probe++
		}
		if probe >= len(tag) || tag[probe] != '"' {
			continue
		}
		probe++
		end := strings.IndexByte(tag[probe:], '"')
		if end == -1 {
			// Unterminated quote: upstream's `[^"]*` matches to end of input.
			return tag[probe:], true
		}
		return tag[probe : probe+end], true
	}
	return "", false
}

func isDsmlSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// dsmlAttrContains reports whether tag carries `key="<want>"`, the shape used by
// the string="true" marker.
func dsmlAttrContains(tag, key, want string) bool {
	value, ok := dsmlAttrValue(tag, key)
	return ok && value == want
}

// marshalDSMLString encodes s as a JSON string.
func marshalDSMLString(s string) string {
	encoded, errMarshal := marshalDSMLLikeJSONStringify(s, "")
	if errMarshal != nil {
		return strconv.Quote(s)
	}
	return encoded
}

// marshalDSMLValue encodes one value for the arguments object.
func marshalDSMLValue(v any) string {
	encoded, errMarshal := marshalDSMLLikeJSONStringify(v, "")
	if errMarshal != nil {
		return "null"
	}
	return encoded
}

// marshalDSMLJSON renders v like TypeScript's JSON.stringify: two-space indent
// when indent is set.
func marshalDSMLJSON(v any, indent bool) string {
	indentText := ""
	if indent {
		indentText = "  "
	}
	encoded, errMarshal := marshalDSMLLikeJSONStringify(v, indentText)
	if errMarshal != nil {
		return "null"
	}
	return encoded
}

// marshalDSMLLikeJSONStringify encodes v the way JSON.stringify does. The one
// trap is that Go's json.Marshal escapes <, > and & to \u003c / \u003e / \u0026
// while JavaScript does not; tool parameters are JSON Schema fragments and file
// contents full of those characters, so the prompt and the arguments payload
// would both change if the escaping were left on. SetEscapeHTML(false) restores
// the JavaScript output, and the encoder's trailing newline is trimmed because
// neither JSON.stringify nor the upstream arguments string carries one.
func marshalDSMLLikeJSONStringify(v any, indentText string) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if indentText != "" {
		encoder.SetIndent("", indentText)
	}
	if errEncode := encoder.Encode(v); errEncode != nil {
		return "", errEncode
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// dsmlJSONDecode is JSON.parse: canonical JSON only, numbers kept as
// json.Number so the re-encoded text matches the original literal exactly (a
// float64 round-trip would turn 1304 into 1304 but could rewrite large integers
// into exponent form).
func dsmlJSONDecode(s string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, false
	}
	// Reject trailing garbage, matching JSON.parse's whole-input semantics.
	var extra any
	if errExtra := decoder.Decode(&extra); errExtra != io.EOF {
		return nil, false
	}
	return value, true
}

// dsmlIsQuotedString reports whether value is a whole quoted JSON string
// literal (`"..."`), the gate upstream uses before attempting a decode.
func dsmlIsQuotedString(value string) bool {
	return len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)
}

func dsmlIsIntegerLiteral(value string) bool {
	body := strings.TrimPrefix(value, "-")
	if body == "" {
		return false
	}
	return dsmlIsAllDigits(body)
}

func dsmlIsFloatLiteral(value string) bool {
	body := strings.TrimPrefix(value, "-")
	dot := strings.IndexByte(body, '.')
	if dot <= 0 || dot == len(body)-1 {
		return false
	}
	return dsmlIsAllDigits(body[:dot]) && dsmlIsAllDigits(body[dot+1:])
}

func dsmlIsAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// longestOpenPrefixTail returns the length of the longest buffer suffix that is
// a proper prefix of one of prefixes. Ports longestOpenPrefixTail
// (llm-adapter.ts:598): the value decides how much of the tail is withheld until
// the next delta, so unrelated short text is never delayed.
func longestOpenPrefixTail(buffer string, prefixes []string) int {
	keep := 0
	// maxCheck is min(buffer length, longest prefix): upstream's
	// Math.min(buffer.length, Math.max(...prefixes.map(p => p.length))). Using
	// the SHORTEST prefix here would cap the check below the length of the
	// longer DSML opener and silently stop retaining `<｜DSML｜` tails.
	maxPrefix := 0
	for _, prefix := range prefixes {
		if len(prefix) > maxPrefix {
			maxPrefix = len(prefix)
		}
	}
	maxCheck := len(buffer)
	if maxPrefix < maxCheck {
		maxCheck = maxPrefix
	}
	for i := 1; i <= maxCheck; i++ {
		tail := buffer[len(buffer)-i:]
		for _, prefix := range prefixes {
			if strings.HasPrefix(prefix, tail) {
				keep = i
				break
			}
		}
	}
	return keep
}

// newDsmlCallID mints the synthetic call id for a DSML tool call. The DSML
// syntax carries no provider-issued id, and the harness pairs tool/call with
// tool/result by id, so every call needs a distinct one. Upstream uses
// `dsml-<uuid-without-dashes>`; 16 random bytes rendered as hex have the same
// shape and entropy and avoid pulling in a UUID dependency.
func newDsmlCallID() string {
	var raw [16]byte
	if _, errRead := rand.Read(raw[:]); errRead != nil {
		return "dsml-" + strconv.FormatUint(uint64(len(raw)), 16)
	}
	return "dsml-" + hex.EncodeToString(raw[:])
}

// dsmlIntPtr is the local pointer helper for openai.ToolCall.Index, which is
// nullable on the wire (`index` is omitted for non-streamed calls).
func dsmlIntPtr(value int) *int { return &value }
