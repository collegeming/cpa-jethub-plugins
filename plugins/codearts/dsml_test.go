package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/cpa-jethub/plugins/internal/jethub/openai"
)

// 本文件覆盖 DSML 原生工具调用模式的移植结果，重点是最容易出错的两处：
// 全角 `｜`（U+FF5C）标签与跨 SSE 分片的增量提取。
// Comments are in English to match the rest of the Go package (see sign.go /
// config.go / translator.go).

// The literal worked example from the upstream system prompt. Written out in
// full (instead of composing it from the exported constants) so the test fails
// if anyone "fixes" the full-width bars to ASCII.
const dsmlExampleLine = `<｜DSML｜tool_calls><｜DSML｜invoke name="工具名"><｜DSML｜parameter name="参数名" string="true">参数值</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`

// Upstream tag literals, spelled out for the same reason.
const (
	dsmlOpenLiteral  = "<｜DSML｜tool_calls>"
	dsmlCloseLiteral = "</｜DSML｜tool_calls>"
)

type dsmlWantCall struct {
	name string
	args string
}

// dsmlSignature renders the calls compactly so table rows stay readable:
// "0:read:{...};1:write:{...}".
func dsmlSignature(calls []openai.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		index := -1
		if call.Index != nil {
			index = *call.Index
		}
		parts = append(parts, strconv.Itoa(index)+":"+call.Function.Name+":"+call.Function.Arguments)
	}
	return strings.Join(parts, ";")
}

// dsmlCheckCalls asserts names, arguments and wire indices, and that upstream's
// per-call invariants hold: every call carries a distinct non-empty id, and the
// type is the OpenAI `function` discriminator.
func dsmlCheckCalls(t *testing.T, got []openai.ToolCall, want []dsmlWantCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call count = %d, want %d (got %s)", len(got), len(want), dsmlSignature(got))
	}
	seenIDs := map[string]bool{}
	for i, call := range got {
		if call.Index == nil || *call.Index != i {
			t.Errorf("call[%d].Index = %v, want %d", i, call.Index, i)
		}
		if call.Function.Name != want[i].name {
			t.Errorf("call[%d].name = %q, want %q", i, call.Function.Name, want[i].name)
		}
		if call.Function.Arguments != want[i].args {
			t.Errorf("call[%d].arguments = %s, want %s", i, call.Function.Arguments, want[i].args)
		}
		if call.Type != "function" {
			t.Errorf("call[%d].Type = %q, want \"function\"", i, call.Type)
		}
		if call.ID == "" {
			t.Errorf("call[%d].ID is empty; the harness pairs tool/call with tool/result by id", i)
		}
		if seenIDs[call.ID] {
			t.Errorf("call[%d].ID %q is duplicated", i, call.ID)
		}
		seenIDs[call.ID] = true
	}
}

// dsmlRunChunked drives the streaming parser over the given deltas and returns
// the concatenated visible text, the drained reasoning and every call.
func dsmlRunChunked(deltas []string) (visible string, reasoning string, calls []openai.ToolCall) {
	parser := NewDsmlStreamParser()
	var visibleBuf, reasoningBuf strings.Builder
	for _, delta := range deltas {
		text, parsed := parser.Feed(delta)
		visibleBuf.WriteString(text)
		reasoningBuf.WriteString(parser.DrainReasoning())
		calls = append(calls, parsed...)
	}
	text, tail := parser.Flush()
	visibleBuf.WriteString(text)
	reasoningBuf.WriteString(parser.DrainReasoning())
	calls = append(calls, tail...)
	return visibleBuf.String(), reasoningBuf.String(), calls
}

// dsmlSplitRunes cuts text at a rune offset, so offsets can point anywhere
// inside a multi-byte full-width bar.
func dsmlSplitRunes(text string, at int) (string, string) {
	runes := []rune(text)
	if at < 0 {
		at = 0
	}
	if at > len(runes) {
		at = len(runes)
	}
	return string(runes[:at]), string(runes[at:])
}

// dsmlRuneOffset converts a byte offset into a rune offset.
func dsmlRuneOffset(text string, byteOffset int) int {
	if byteOffset < 0 {
		return 0
	}
	return len([]rune(text[:byteOffset]))
}

func dsmlTestTools() []openai.Tool {
	return []openai.Tool{
		{
			Type: "function",
			Function: openai.FunctionDef{
				Name:        "bash",
				Description: "Run a shell command",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
					"required":   []any{"command"},
				},
			},
		},
		{
			Type: "function",
			Function: openai.FunctionDef{
				Name: "write",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_path": map[string]any{"type": "string"},
						"content":   map[string]any{"type": "string"},
					},
				},
			},
		},
	}
}

func TestDsmlBuildSystemPrompt(t *testing.T) {
	prompt := BuildDsmlSystemPrompt(dsmlTestTools())
	if prompt == "" {
		t.Fatal("BuildDsmlSystemPrompt returned empty for a non-empty tool list")
	}
	// 1. The literal worked example must appear verbatim, full-width bars and
	//    all -- this is the single most likely thing to be broken by an editor.
	if !strings.Contains(prompt, dsmlExampleLine) {
		t.Errorf("prompt is missing the example line %q", dsmlExampleLine)
	}
	if strings.Contains(prompt, "<|DSML|") {
		t.Error("prompt contains ASCII-pipe tags; DSML uses U+FF5C")
	}
	for _, rule := range []string{
		"以下是你可用的工具及其 JSON Schema。当需要调用工具完成任务时，",
		"必须使用原生 DSML 工具调用语法输出，格式如下：",
		"规则：",
		"- 工具名必须是下面列表中的 name。",
		"- 字符串参数加 string=\"true\" 属性；对象/数组/数字/布尔参数不要加该属性。",
		"- 一次可以输出多个 <｜DSML｜invoke> 调用（工具可以并行）。",
		"- 文件内容请一次性完整写入单个 write 调用的 content 参数，不要拆分或省略。",
	} {
		if !strings.Contains(prompt, rule) {
			t.Errorf("prompt is missing upstream line %q", rule)
		}
	}

	// 2. The tool list is the tail of the prompt and must be valid JSON that
	//    round-trips to the projected {name, description, parameters} objects.
	const marker = "工具列表（JSON Schema）：\n"
	blockStart := strings.Index(prompt, marker)
	if blockStart == -1 {
		t.Fatalf("prompt is missing %q", marker)
	}
	block := prompt[blockStart+len(marker):]
	var decoded []struct {
		Name        string          `json:"name"`
		Description *string         `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(block), &decoded); err != nil {
		t.Fatalf("tool list is not valid JSON: %v\n%s", err, block)
	}
	if len(decoded) != 2 {
		t.Fatalf("tool count = %d, want 2", len(decoded))
	}
	if decoded[0].Name != "bash" || decoded[1].Name != "write" {
		t.Errorf("tool order = [%s %s], want [bash write]", decoded[0].Name, decoded[1].Name)
	}
	if decoded[0].Description == nil || *decoded[0].Description != "Run a shell command" {
		t.Errorf("tool[0].description = %v, want \"Run a shell command\"", decoded[0].Description)
	}
	// Upstream projects `description: undefined`, which JSON.stringify drops.
	if decoded[1].Description != nil {
		t.Errorf("tool[1].description = %q, want omitted", *decoded[1].Description)
	}
	for i, tool := range decoded {
		var params map[string]any
		if err := json.Unmarshal(tool.Parameters, &params); err != nil {
			t.Fatalf("tool[%d].parameters is not a JSON object: %v", i, err)
		}
		if params["type"] != "object" {
			t.Errorf("tool[%d].parameters.type = %v, want object", i, params["type"])
		}
	}
	if !strings.Contains(string(decoded[0].Parameters), `"command"`) {
		t.Error("tool[0].parameters lost its command property")
	}

	// 3. Byte-level layout: the struct must emit name -> description ->
	//    parameters (Go struct declaration order), indent with two spaces, and
	//    leave the prompt's own `<`/`>` unescaped like JSON.stringify does.
	firstToolStart := strings.Index(block, "{\n    \"name\": \"bash\",")
	if firstToolStart == -1 {
		t.Fatalf("tool list is not two-space indented as expected:\n%s", block)
	}
	// "\n  }" only matches the tool object's own closing line; nested objects
	// close with four or more spaces of indentation.
	firstToolEnd := strings.Index(block[firstToolStart:], "\n  }")
	if firstToolEnd == -1 {
		firstToolEnd = len(block) - firstToolStart
	}
	firstTool := block[firstToolStart : firstToolStart+firstToolEnd]
	nameAt := strings.Index(firstTool, `"name"`)
	descAt := strings.Index(firstTool, `"description"`)
	paramsAt := strings.Index(firstTool, `"parameters"`)
	if nameAt == -1 || descAt == -1 || paramsAt == -1 || !(nameAt < descAt && descAt < paramsAt) {
		t.Errorf("field order in %s is not name -> description -> parameters", firstTool)
	}
	if strings.Contains(block, `\u003c`) || strings.Contains(block, `\u0026`) {
		t.Error("tool JSON was HTML-escaped; JSON.stringify would not do that")
	}
}

func TestDsmlBuildSystemPromptEmptyTools(t *testing.T) {
	if got := BuildDsmlSystemPrompt(nil); got != "" {
		t.Errorf("BuildDsmlSystemPrompt(nil) = %q, want \"\"", got)
	}
	if got := BuildDsmlSystemPrompt([]openai.Tool{}); got != "" {
		t.Errorf("BuildDsmlSystemPrompt([]) = %q, want \"\"", got)
	}
}

func TestDsmlToolCallsOpenIsFullWidth(t *testing.T) {
	if DsmlToolCallsOpen != dsmlOpenLiteral {
		t.Errorf("DsmlToolCallsOpen = %q, want %q", DsmlToolCallsOpen, dsmlOpenLiteral)
	}
	if !strings.Contains(DsmlToolCallsOpen, "\uFF5C") {
		t.Error("DsmlToolCallsOpen does not contain U+FF5C")
	}
	if strings.Contains(DsmlToolCallsOpen, "|") {
		t.Error("DsmlToolCallsOpen contains an ASCII pipe")
	}
}

func TestDsmlParseSingleInvokeWithStringParam(t *testing.T) {
	text := dsmlOpenLiteral +
		`<｜DSML｜invoke name="write">` +
		`<｜DSML｜parameter name="file_path" string="true">/tmp/a.txt</｜DSML｜parameter>` +
		"<｜DSML｜parameter name=\"content\" string=\"true\">line1\nline2</｜DSML｜parameter>" +
		`</｜DSML｜invoke>` + dsmlCloseLiteral
	visible, calls := ParseDsmlToolCalls(text)
	if visible != "" {
		t.Errorf("visible = %q, want \"\"", visible)
	}
	dsmlCheckCalls(t, calls, []dsmlWantCall{
		{name: "write", args: `{"file_path":"/tmp/a.txt","content":"line1\nline2"}`},
	})
}

func TestDsmlParseParallelInvokes(t *testing.T) {
	text := "我先并行读取两个文件。" + dsmlOpenLiteral +
		`<｜DSML｜invoke name="read"><｜DSML｜parameter name="file_path" string="true">/a</｜DSML｜parameter></｜DSML｜invoke>` +
		`<｜DSML｜invoke name="read"><｜DSML｜parameter name="file_path" string="true">/b</｜DSML｜parameter><｜DSML｜parameter name="limit">20</｜DSML｜parameter></｜DSML｜invoke>` +
		`<｜DSML｜invoke name="bash"><｜DSML｜parameter name="command" string="true">echo hi</｜DSML｜parameter></｜DSML｜invoke>` +
		dsmlCloseLiteral + "\n完成。"
	visible, calls := ParseDsmlToolCalls(text)
	if visible != "我先并行读取两个文件。\n完成。" {
		t.Errorf("visible = %q", visible)
	}
	dsmlCheckCalls(t, calls, []dsmlWantCall{
		{name: "read", args: `{"file_path":"/a"}`},
		{name: "read", args: `{"file_path":"/b","limit":20}`},
		{name: "bash", args: `{"command":"echo hi"}`},
	})
}

// The typed-parameter table mirrors upstream's string="true" rule: values
// without the marker are JSON when they parse, values with the marker are
// coerced back to scalars/arrays/objects when they look like JSON, and plain
// text stays a raw string.
func TestDsmlParseTypedParameters(t *testing.T) {
	cases := []struct {
		name     string
		marker   string
		value    string
		wantArgs string
	}{
		{name: "object without marker", value: `{"a":1}`, wantArgs: `{"p":{"a":1}}`},
		{name: "array without marker", value: `[1,2,{"b":true}]`, wantArgs: `{"p":[1,2,{"b":true}]}`},
		{name: "number without marker", value: `1304`, wantArgs: `{"p":1304}`},
		{name: "float without marker", value: `1.5`, wantArgs: `{"p":1.5}`},
		{name: "true without marker", value: `true`, wantArgs: `{"p":true}`},
		{name: "false without marker", value: `false`, wantArgs: `{"p":false}`},
		{name: "null without marker", value: `null`, wantArgs: `{"p":null}`},
		{name: "quoted number without marker", value: `"840"`, wantArgs: `{"p":840}`},
		{name: "plain text without marker", value: `ls -la`, wantArgs: `{"p":"ls -la"}`},
		// The marker is a hint the model often gets wrong: a numeric value
		// marked as a string is still restored to a number so tool-schema
		// validation passes.
		{name: "number with marker", marker: ` string="true"`, value: `1304`, wantArgs: `{"p":1304}`},
		{name: "bool with marker", marker: ` string="true"`, value: `true`, wantArgs: `{"p":true}`},
		{name: "null with marker", marker: ` string="true"`, value: `null`, wantArgs: `{"p":null}`},
		{name: "json object with marker", marker: ` string="true"`, value: `{"content":"a"}`, wantArgs: `{"p":{"content":"a"}}`},
		{name: "json array with marker", marker: ` string="true"`, value: `[{"content":"a"}]`, wantArgs: `{"p":[{"content":"a"}]}`},
		{name: "quoted number with marker", marker: ` string="true"`, value: `"840"`, wantArgs: `{"p":840}`},
		{name: "text with marker", marker: ` string="true"`, value: `hello world`, wantArgs: `{"p":"hello world"}`},
		{name: "path with marker", marker: ` string="true"`, value: `/tmp/v1.2/x`, wantArgs: `{"p":"/tmp/v1.2/x"}`},
		{name: "partial number with marker", marker: ` string="true"`, value: `123abc`, wantArgs: `{"p":"123abc"}`},
		{name: "empty value with marker", marker: ` string="true"`, value: ``, wantArgs: `{"p":""}`},
		{name: "blank value with marker", marker: ` string="true"`, value: `   `, wantArgs: `{"p":"   "}`},
		{name: "padded number with marker", marker: ` string="true"`, value: ` 1304 `, wantArgs: `{"p":1304}`},
		{name: "negative number with marker", marker: ` string="true"`, value: `-42`, wantArgs: `{"p":-42}`},
		{name: "zero padded number with marker", marker: ` string="true"`, value: `01304`, wantArgs: `{"p":1304}`},
		{name: "quoted bool with marker", marker: ` string="true"`, value: `"true"`, wantArgs: `{"p":true}`},
		{name: "quoted text with marker", marker: ` string="true"`, value: `"hello"`, wantArgs: `{"p":"hello"}`},
		{name: "quoted quotes with marker", marker: ` string="true"`, value: `he said "hi"`, wantArgs: `{"p":"he said \"hi\""}`},
		{name: "unclosed quote without marker", value: `"abc`, wantArgs: `{"p":"\"abc"}`},
		{name: "trailing garbage without marker", value: `{"a":1} tail`, wantArgs: `{"p":"{\"a\":1} tail"}`},
		// HTML-ish content must survive verbatim: JSON.stringify does not
		// escape <, > or &, and neither may we.
		{name: "markup with marker", marker: ` string="true"`, value: `if a < b && c > d`, wantArgs: `{"p":"if a < b && c > d"}`},
		{name: "unicode with marker", marker: ` string="true"`, value: `中文内容`, wantArgs: `{"p":"中文内容"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text := dsmlOpenLiteral + `<｜DSML｜invoke name="f"><｜DSML｜parameter name="p"` +
				testCase.marker + ">" + testCase.value + `</｜DSML｜parameter></｜DSML｜invoke>` + dsmlCloseLiteral
			visible, calls := ParseDsmlToolCalls(text)
			if visible != "" {
				t.Errorf("visible = %q, want \"\"", visible)
			}
			dsmlCheckCalls(t, calls, []dsmlWantCall{{name: "f", args: testCase.wantArgs}})
		})
	}
}

// Key order inside an object parameter follows the model's output order, which
// is what JSON.stringify preserves and what a Go map would have destroyed.
// Upstream parses with JSON.parse and re-serialises with JSON.stringify, which
// normalises numeric literals (1.50 -> 1.5, 1e5 -> 100000) and rounds integers
// that do not fit a float64. The Go port deliberately keeps the literal the
// model wrote: identical JSON type, no precision loss, same key order. This test
// pins that documented divergence.
func TestDsmlParseKeepsNumberLiterals(t *testing.T) {
	cases := []struct {
		name     string
		marker   string
		value    string
		wantArgs string
	}{
		{name: "trailing zero", marker: ` string="true"`, value: `1.50`, wantArgs: `{"p":1.50}`},
		{name: "big integer", marker: ` string="true"`, value: `12345678901234567890`, wantArgs: `{"p":12345678901234567890}`},
		{name: "exponent in object", value: `{"a":1e5,"b":2.0}`, wantArgs: `{"p":{"a":1e5,"b":2.0}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text := dsmlOpenLiteral + `<｜DSML｜invoke name="f"><｜DSML｜parameter name="p"` +
				testCase.marker + ">" + testCase.value + `</｜DSML｜parameter></｜DSML｜invoke>` + dsmlCloseLiteral
			_, calls := ParseDsmlToolCalls(text)
			dsmlCheckCalls(t, calls, []dsmlWantCall{{name: "f", args: testCase.wantArgs}})
		})
	}
}

// A repeated parameter name behaves like a JS object assignment: the key keeps
// its first position and takes the last value, so the emitted JSON never carries
// duplicate keys.
func TestDsmlParseDuplicateParameterName(t *testing.T) {
	text := dsmlOpenLiteral +
		`<｜DSML｜invoke name="f"><｜DSML｜parameter name="offset" string="true">1</｜DSML｜parameter><｜DSML｜parameter name="limit" string="true">10</｜DSML｜parameter><｜DSML｜parameter name="offset" string="true">2</｜DSML｜parameter></｜DSML｜invoke>` +
		dsmlCloseLiteral
	_, calls := ParseDsmlToolCalls(text)
	dsmlCheckCalls(t, calls, []dsmlWantCall{{name: "f", args: `{"offset":2,"limit":10}`}})
}

func TestDsmlParsePreservesObjectKeyOrder(t *testing.T) {
	text := dsmlOpenLiteral + `<｜DSML｜invoke name="f"><｜DSML｜parameter name="p">{"z":1,"a":2,"m":{"y":3,"b":4}}</｜DSML｜parameter></｜DSML｜invoke>` + dsmlCloseLiteral
	_, calls := ParseDsmlToolCalls(text)
	dsmlCheckCalls(t, calls, []dsmlWantCall{
		{name: "f", args: `{"p":{"z":1,"a":2,"m":{"y":3,"b":4}}}`},
	})
}

func TestDsmlParseVisibleTextAroundBlock(t *testing.T) {
	text := "前一段可见文本。" + dsmlOpenLiteral +
		`<｜DSML｜invoke name="bash"><｜DSML｜parameter name="command" string="true">ls</｜DSML｜parameter></｜DSML｜invoke>` +
		dsmlCloseLiteral + "后一段可见文本。"
	visible, calls := ParseDsmlToolCalls(text)
	if visible != "前一段可见文本。后一段可见文本。" {
		t.Errorf("visible = %q", visible)
	}
	dsmlCheckCalls(t, calls, []dsmlWantCall{{name: "bash", args: `{"command":"ls"}`}})
	if _, calls := ParseDsmlToolCalls("没有工具调用的纯文本"); len(calls) != 0 {
		t.Errorf("plain text produced %d calls", len(calls))
	}
}

func TestDsmlParseMalformedBlockFallsBackToText(t *testing.T) {
	// With no name="..." anywhere in the invoke, upstream's parseDsmlInvoke
	// returns undefined, parseDsmlToolCalls propagates that, and the whole block
	// is released as visible text instead of being swallowed.
	text := "before " + dsmlOpenLiteral +
		`<｜DSML｜invoke foo="bar"><｜DSML｜parameter x="y" string="true">ls</｜DSML｜parameter></｜DSML｜invoke>` +
		dsmlCloseLiteral
	visible, calls := ParseDsmlToolCalls(text)
	if len(calls) != 0 {
		t.Errorf("malformed block produced %d calls, want 0", len(calls))
	}
	if !strings.Contains(visible, "before ") || !strings.Contains(visible, dsmlCloseLiteral) {
		t.Errorf("visible = %q, want the block released verbatim", visible)
	}
}

// Upstream matches name="..." with a regexp over the WHOLE invoke block, so an
// invoke that forgot its own name silently adopts the first parameter's name.
// Odd, but it is the ported behaviour, so pin it.
func TestDsmlParseNamelessInvokeAdoptsParameterName(t *testing.T) {
	text := dsmlOpenLiteral +
		`<｜DSML｜invoke><｜DSML｜parameter name="command" string="true">ls</｜DSML｜parameter></｜DSML｜invoke>` +
		dsmlCloseLiteral
	_, calls := ParseDsmlToolCalls(text)
	dsmlCheckCalls(t, calls, []dsmlWantCall{{name: "command", args: `{"command":"ls"}`}})
}

func TestDsmlParseHalfWidthPipeIsOrdinaryText(t *testing.T) {
	text := `<|DSML|tool_calls><|DSML|invoke name="bash"><|DSML|parameter name="command" string="true">ls</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>`
	visible, calls := ParseDsmlToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("ASCII-pipe tags produced %d calls, want 0", len(calls))
	}
	if visible != text {
		t.Errorf("visible = %q, want the input unchanged", visible)
	}
}

// The streaming regression test: the same block split at arbitrary offsets must
// produce exactly the single-shot result. This covers the four required offsets
// (inside the opener, inside a tag name, inside a parameter value, right after
// the closing tag) plus every other rune boundary.
func TestDsmlStreamChunkBoundaries(t *testing.T) {
	sample := "我先说明。" +
		"<thought>需要读取输入文件，\n然后写出结果。</thought>" +
		dsmlOpenLiteral +
		`<｜DSML｜invoke name="read"><｜DSML｜parameter name="file_path" string="true">/tmp/in.txt</｜DSML｜parameter><｜DSML｜parameter name="offset">1304</｜DSML｜parameter></｜DSML｜invoke>` +
		`<｜DSML｜invoke name="write"><｜DSML｜parameter name="file_path" string="true">/tmp/out.txt</｜DSML｜parameter><｜DSML｜parameter name="content" string="true">hello <b>world</b></｜DSML｜parameter></｜DSML｜invoke>` +
		dsmlCloseLiteral + "完成。"

	wantVisible, wantReasoning, wantCalls := dsmlRunChunked([]string{sample})
	if wantVisible != "我先说明。完成。" {
		t.Fatalf("single-shot visible = %q", wantVisible)
	}
	if wantReasoning != "需要读取输入文件，\n然后写出结果。" {
		t.Fatalf("single-shot reasoning = %q", wantReasoning)
	}
	if len(wantCalls) != 2 {
		t.Fatalf("single-shot calls = %d, want 2", len(wantCalls))
	}
	wantSignature := dsmlSignature(wantCalls)

	namedOffsets := map[string]int{
		"first rune":               0,
		"inside opener":            dsmlRuneOffset(sample, strings.Index(sample, "<｜DSM")+len("<｜DSM")),
		"inside tag name":          dsmlRuneOffset(sample, strings.Index(sample, "<｜DSML｜invoke")+len("<｜DSML｜inv")),
		"inside attribute value":   dsmlRuneOffset(sample, strings.Index(sample, "name=\"read\"")+len("name=\"re")),
		"inside parameter value":   dsmlRuneOffset(sample, strings.Index(sample, "/tmp/in.txt")+len("/tmp")),
		"inside closing tag":       dsmlRuneOffset(sample, strings.Index(sample, dsmlCloseLiteral)+len("</｜DS")),
		"right after closing tag":  dsmlRuneOffset(sample, strings.Index(sample, dsmlCloseLiteral)+len(dsmlCloseLiteral)),
		"inside thought":           dsmlRuneOffset(sample, strings.Index(sample, "需要读取")+len("需要")),
		"inside thought close tag": dsmlRuneOffset(sample, strings.Index(sample, "</thought>")+len("</th")),
	}
	for name, offset := range namedOffsets {
		t.Run(name, func(t *testing.T) {
			head, tail := dsmlSplitRunes(sample, offset)
			visible, reasoning, calls := dsmlRunChunked([]string{head, tail})
			if visible != wantVisible {
				t.Errorf("visible = %q, want %q", visible, wantVisible)
			}
			if reasoning != wantReasoning {
				t.Errorf("reasoning = %q, want %q", reasoning, wantReasoning)
			}
			if got := dsmlSignature(calls); got != wantSignature {
				t.Errorf("calls = %s, want %s", got, wantSignature)
			}
		})
	}

	// Every rune boundary as well, to catch offsets that only break on a
	// particular multi-byte character.
	runes := []rune(sample)
	for offset := 0; offset <= len(runes); offset++ {
		visible, reasoning, calls := dsmlRunChunked([]string{string(runes[:offset]), string(runes[offset:])})
		if visible != wantVisible || reasoning != wantReasoning || dsmlSignature(calls) != wantSignature {
			t.Fatalf("offset %d: visible=%q reasoning=%q calls=%s; want visible=%q reasoning=%q calls=%s",
				offset, visible, reasoning, dsmlSignature(calls), wantVisible, wantReasoning, wantSignature)
		}
	}

	// Three-way and byte-at-a-time splits, which stress the retained prefix
	// logic much harder than a single cut.
	perRune := make([]string, 0, len(runes))
	for _, r := range runes {
		perRune = append(perRune, string(r))
	}
	visible, reasoning, calls := dsmlRunChunked(perRune)
	if visible != wantVisible || reasoning != wantReasoning || dsmlSignature(calls) != wantSignature {
		t.Fatalf("byte-at-a-time: visible=%q reasoning=%q calls=%s", visible, reasoning, dsmlSignature(calls))
	}

	third := len(runes) / 3
	visible, reasoning, calls = dsmlRunChunked([]string{
		string(runes[:third]), string(runes[third : 2*third]), string(runes[2*third:]),
	})
	if visible != wantVisible || reasoning != wantReasoning || dsmlSignature(calls) != wantSignature {
		t.Fatalf("three-way: visible=%q reasoning=%q calls=%s", visible, reasoning, dsmlSignature(calls))
	}
}

// A partial opener at the end of a delta must be retained, not flushed.
func TestDsmlStreamRetainsPartialOpener(t *testing.T) {
	for _, partial := range []string{"<", "<｜", "<｜D", "<｜DSML｜tool_", "<｜DSML｜tool_calls", "<th", "<thought"} {
		parser := NewDsmlStreamParser()
		visible, calls := parser.Feed("可见文本" + partial)
		if visible != "可见文本" {
			t.Errorf("Feed(%q) visible = %q, want %q", partial, visible, "可见文本")
		}
		if len(calls) != 0 {
			t.Errorf("Feed(%q) produced %d calls", partial, len(calls))
		}
		flushed, tail := parser.Flush()
		if flushed != partial {
			t.Errorf("Flush after Feed(%q) = %q, want %q", partial, flushed, partial)
		}
		if len(tail) != 0 {
			t.Errorf("Flush after Feed(%q) produced %d calls", partial, len(tail))
		}
	}

	// Text that merely ends in `<` followed by unrelated content must reach the
	// client as soon as the next delta disproves a tag.
	parser := NewDsmlStreamParser()
	head, _ := parser.Feed("a <")
	tail, _ := parser.Feed("b")
	if head+tail != "a <b" {
		t.Errorf("visible = %q, want %q", head+tail, "a <b")
	}
}

// Flush must not swallow a stream that ends mid-block or mid-thought.
func TestDsmlStreamFlushMidBlock(t *testing.T) {
	parser := NewDsmlStreamParser()
	head, calls := parser.Feed(dsmlOpenLiteral + `<｜DSML｜invoke name="bash"><｜DSML｜parameter name="command" string="true">ls`)
	if head != "" {
		t.Errorf("visible mid-block = %q, want \"\"", head)
	}
	if len(calls) != 0 {
		t.Fatalf("mid-block produced %d calls, want 0", len(calls))
	}
	flushed, tail := parser.Flush()
	if len(tail) != 0 {
		t.Errorf("Flush mid-block produced %d calls, want 0", len(tail))
	}
	// The opener was consumed on entry, so Flush puts it back to keep the
	// released fragment readable.
	if !strings.HasPrefix(flushed, DsmlToolCallsOpen) {
		t.Errorf("Flush mid-block = %q, want it to re-prepend the opener", flushed)
	}
	if !strings.Contains(flushed, "command") {
		t.Errorf("Flush mid-block = %q, want the buffered fragment", flushed)
	}
	if again, _ := parser.Flush(); again != "" {
		t.Errorf("second Flush = %q, want \"\"", again)
	}

	// Mid-thought content stays on the reasoning channel.
	thoughtParser := NewDsmlStreamParser()
	thoughtVisible, _ := thoughtParser.Feed("<thought>尚未结束的推理")
	if thoughtVisible != "" {
		t.Errorf("visible mid-thought = %q, want \"\"", thoughtVisible)
	}
	if got := thoughtParser.DrainReasoning(); got != "尚未结束的推理" {
		t.Errorf("reasoning mid-thought = %q", got)
	}
	if flushed, _ := thoughtParser.Flush(); flushed != "" {
		t.Errorf("Flush mid-thought = %q, want \"\"", flushed)
	}
}

// <thought> handling matches upstream: the delimiters are consumed and the
// content is routed to the reasoning channel, never to the visible body.
func TestDsmlThoughtHandling(t *testing.T) {
	parser := NewDsmlStreamParser()
	visible, calls := parser.Feed("答案前。<thought>内部推理 A<")
	if visible != "答案前。" {
		t.Errorf("visible = %q, want %q", visible, "答案前。")
	}
	if len(calls) != 0 {
		t.Errorf("thought text produced %d calls", len(calls))
	}
	if got := parser.DrainReasoning(); got != "内部推理 A" {
		t.Errorf("reasoning = %q, want %q", got, "内部推理 A")
	}
	// The leading '<' of </thought> was retained in the buffer, so the next
	// delta only has to carry the remainder of the closing tag.
	visible, _ = parser.Feed("/thought>答案后。")
	if visible != "答案后。" {
		t.Errorf("visible after thought = %q, want %q", visible, "答案后。")
	}
	if got := parser.DrainReasoning(); got != "" {
		t.Errorf("reasoning after close = %q, want \"\"", got)
	}

	// A thought block containing a DSML block: the DSML inside is hidden by the
	// thought state machine, exactly like upstream's single-pass extractor.
	inner := "<thought>看起来要调用工具" + dsmlOpenLiteral +
		`<｜DSML｜invoke name="bash"><｜DSML｜parameter name="command" string="true">ls</｜DSML｜parameter></｜DSML｜invoke>` +
		dsmlCloseLiteral + "</thought>完成"
	visible, calls = ParseDsmlToolCalls(inner)
	if visible != "完成" {
		t.Errorf("visible = %q, want %q", visible, "完成")
	}
	if len(calls) != 0 {
		t.Errorf("DSML nested in <thought> produced %d calls, want 0", len(calls))
	}
}

// Once a call's closing tag has been seen, trailing text in the same or later
// deltas must not duplicate it.
func TestDsmlStreamNoDuplicateCall(t *testing.T) {
	block := dsmlOpenLiteral +
		`<｜DSML｜invoke name="bash"><｜DSML｜parameter name="command" string="true">ls</｜DSML｜parameter></｜DSML｜invoke>` +
		dsmlCloseLiteral

	parser := NewDsmlStreamParser()
	visible, first := parser.Feed(block)
	if visible != "" || len(first) != 1 {
		t.Fatalf("Feed(block) = (%q, %d calls), want (\"\", 1)", visible, len(first))
	}
	visible, second := parser.Feed("after")
	if len(second) != 0 {
		t.Errorf("trailing text produced %d extra calls", len(second))
	}
	visible2, third := parser.Feed(" more")
	if len(third) != 0 {
		t.Errorf("later trailing text produced %d extra calls", len(third))
	}
	if visible+visible2 != "after more" {
		t.Errorf("visible = %q, want %q", visible+visible2, "after more")
	}

	// A second, identical block is a genuinely new call, not a duplicate.
	_, fourth := parser.Feed(block)
	if len(fourth) != 1 {
		t.Fatalf("second block produced %d calls, want 1", len(fourth))
	}
	all := append(append(append(append([]openai.ToolCall{}, first...), second...), third...), fourth...)
	if len(all) != 2 {
		t.Fatalf("total calls = %d, want 2", len(all))
	}
	// Indices are consecutive across deltas so the client can merge shards.
	if all[0].Index == nil || *all[0].Index != 0 || all[1].Index == nil || *all[1].Index != 1 {
		t.Errorf("indices = [%v %v], want [0 1]", all[0].Index, all[1].Index)
	}
	if all[0].ID == all[1].ID {
		t.Error("two calls share an id")
	}
}

// Multiple DSML blocks in one response, and blocks interleaved with thought,
// must all be collected in order.
func TestDsmlStreamMultipleBlocksAndIndices(t *testing.T) {
	parser := NewDsmlStreamParser()
	var calls []openai.ToolCall
	var visible strings.Builder
	for _, delta := range []string{
		"开始",
		dsmlOpenLiteral + `<｜DSML｜invoke name="a"><｜DSML｜parameter name="x" string="true">1</｜DSML｜parameter></｜DSML｜invoke>` + dsmlCloseLiteral,
		"中间",
		"<thought>想想</thought>",
		dsmlOpenLiteral + `<｜DSML｜invoke name="b"><｜DSML｜parameter name="y" string="true">2</｜DSML｜parameter></｜DSML｜invoke><｜DSML｜invoke name="c"><｜DSML｜parameter name="z" string="true">3</｜DSML｜parameter></｜DSML｜invoke>` + dsmlCloseLiteral,
		"结束",
	} {
		text, parsed := parser.Feed(delta)
		visible.WriteString(text)
		calls = append(calls, parsed...)
	}
	flushed, tail := parser.Flush()
	visible.WriteString(flushed)
	calls = append(calls, tail...)
	if visible.String() != "开始中间结束" {
		t.Errorf("visible = %q, want %q", visible.String(), "开始中间结束")
	}
	// string="true" does not force a string: the numeric literals are coerced
	// back to numbers so tool-schema validation passes.
	dsmlCheckCalls(t, calls, []dsmlWantCall{
		{name: "a", args: `{"x":1}`},
		{name: "b", args: `{"y":2}`},
		{name: "c", args: `{"z":3}`},
	})
}

// Empty and whitespace deltas are legal SSE payloads and must be no-ops.
func TestDsmlStreamEmptyDeltas(t *testing.T) {
	visible, calls := ParseDsmlToolCalls("")
	if visible != "" || len(calls) != 0 {
		t.Errorf("ParseDsmlToolCalls(\"\") = (%q, %d calls)", visible, len(calls))
	}
	parser := NewDsmlStreamParser()
	for _, delta := range []string{"", "", ""} {
		if text, parsed := parser.Feed(delta); text != "" || len(parsed) != 0 {
			t.Errorf("Feed(%q) = (%q, %d calls)", delta, text, len(parsed))
		}
	}
	if text, parsed := parser.Flush(); text != "" || len(parsed) != 0 {
		t.Errorf("Flush on empty stream = (%q, %d calls)", text, len(parsed))
	}
}

// ParseDsmlToolCalls must agree with the streaming path, since both are exposed
// to the integration layer.
func TestDsmlParseMatchesStreaming(t *testing.T) {
	text := "前言<thought>推理</thought>" + dsmlOpenLiteral +
		`<｜DSML｜invoke name="bash"><｜DSML｜parameter name="command" string="true">ls -la</｜DSML｜parameter></｜DSML｜invoke>` +
		dsmlCloseLiteral + "结尾"
	parseVisible, parseCalls := ParseDsmlToolCalls(text)
	streamVisible, streamReasoning, streamCalls := dsmlRunChunked([]string{text})
	if parseVisible != streamVisible {
		t.Errorf("visible: parse=%q stream=%q", parseVisible, streamVisible)
	}
	if dsmlSignature(parseCalls) != dsmlSignature(streamCalls) {
		t.Errorf("calls: parse=%s stream=%s", dsmlSignature(parseCalls), dsmlSignature(streamCalls))
	}
	if streamReasoning != "推理" {
		t.Errorf("stream reasoning = %q, want %q", streamReasoning, "推理")
	}
}
