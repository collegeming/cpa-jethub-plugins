package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A synthetic WASM module
// ---------------------------------------------------------------------------
//
// The Qoder signing artifact is not redistributed with this repository, so the
// signer wrapper is exercised against a module built inside the test. The module
// reproduces exactly the surface the wrapper depends on:
//
//   - the `ptr/len/valIdx/isErr` string-result layout;
//   - the `ptr/errIdx/isErr` pointer-result layout;
//   - `requestresult_url/_body(stack, ptr)` with the stack pointer FIRST;
//   - `requestresult_headers` returning a JS Map built through the host imports,
//     which is what proves the dual-mode `Map.prototype.set` /
//     `Uint8Array.prototype.set` handling works.
const (
	synthAuthJSON  = `{"encrypt_user_info":"ENC","key":"KEY"}`
	synthHeaderKey = "Host"
	synthHeaderVal = "abc"
	synthURL       = "https://api2.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?Encode=1"
	synthBody      = "encrypted-body"
)

// Data offsets inside the synthetic module's memory.
const (
	synthAuthOffset   = 1024
	synthKeyOffset    = 1280
	synthValueOffset  = 1296
	synthURLOffset    = 1400
	synthBodyOffset   = 1600
	synthCtxPointer   = 1234
	synthResultPoinr  = 4321
	synthAllocPointer = 4096
)

// Intermediate module indices.
const (
	synthTypeVoidI32    = 0 // () -> i32
	synthTypeI32I32I32  = 1 // (i32,i32) -> i32
	synthTypeI32x3I32   = 2 // (i32,i32,i32) -> i32
	synthTypeI32I32     = 3 // (i32) -> i32
	synthTypeI32x3Void  = 4 // (i32,i32,i32) -> ()
	synthTypeI32x10     = 5 // 10 x i32 -> ()
	synthTypeI32x9Void  = 6 // 9 x i32 -> ()
	synthTypeI32I32Void = 7 // (i32,i32) -> ()

	synthImportCount   = 3
	synthFuncStack     = 3
	synthFuncAlloc     = 4
	synthFuncFree      = 5
	synthFuncAuthField = 6
	synthFuncCtxNew    = 7
	synthFuncPrepare   = 8
	synthFuncHeaders   = 9
	synthFuncURL       = 10
	synthFuncBody      = 11
)

// uleb encodes an unsigned LEB128 value.
func uleb(value uint32) []byte {
	var out []byte
	for {
		current := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			current |= 0x80
		}
		out = append(out, current)
		if value == 0 {
			return out
		}
	}
}

// sleb encodes a signed LEB128 value (only small positives are used here).
func sleb(value int32) []byte {
	var out []byte
	for {
		current := byte(value & 0x7f)
		value >>= 7
		signBit := current & 0x40
		if (value == 0 && signBit == 0) || (value == -1 && signBit != 0) {
			return append(out, current)
		}
		out = append(out, current|0x80)
	}
}

// wasmVector prefixes items with their count.
func wasmVector(items ...[]byte) []byte {
	out := uleb(uint32(len(items)))
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}

// wasmSection frames a section body.
func wasmSection(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, uleb(uint32(len(payload)))...)
	return append(out, payload...)
}

// wasmName encodes a length-prefixed UTF-8 name.
func wasmName(value string) []byte {
	out := uleb(uint32(len(value)))
	return append(out, []byte(value)...)
}

// wasmFuncType encodes one function type.
func wasmFuncType(params, results int) []byte {
	out := []byte{0x60, byte(params)}
	for index := 0; index < params; index++ {
		out = append(out, 0x7f)
	}
	out = append(out, byte(results))
	for index := 0; index < results; index++ {
		out = append(out, 0x7f)
	}
	return out
}

// wasmBody frames a function body (no locals).
func wasmBody(code []byte) []byte {
	body := append([]byte{0x00}, code...)
	out := uleb(uint32(len(body)))
	return append(out, body...)
}

// storeAt emits `local.get 0; i32.const offset; i32.add?; i32.const value; i32.store`.
func storeAt(offset int32, value int32, useAdd bool) []byte {
	out := []byte{0x20, 0x00} // local.get 0
	if offset != 0 {
		out = append(out, 0x41)
		out = append(out, sleb(offset)...)
		out = append(out, 0x6a) // i32.add
	}
	out = append(out, 0x41)
	out = append(out, sleb(value)...)
	out = append(out, 0x36, 0x02, 0x00) // i32.store align=2 offset=0
	return out
}

// buildSyntheticWasm assembles the module described above.
func buildSyntheticWasm() []byte {
	var module []byte
	module = append(module, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)

	// Types.
	types := wasmVector(
		wasmFuncType(0, 1),
		wasmFuncType(2, 1),
		wasmFuncType(3, 1),
		wasmFuncType(1, 1),
		wasmFuncType(3, 0),
		wasmFuncType(10, 0),
		wasmFuncType(9, 0),
		wasmFuncType(2, 0),
	)
	module = append(module, wasmSection(1, types)...)

	// Imports: the Map builder, the string caster and the shared `set` glue.
	// The wasm import entry order is module name, field name, kind byte, type.
	importEntry := func(moduleName, fieldName string, typeIndex uint32) []byte {
		entry := wasmName(moduleName)
		entry = append(entry, wasmName(fieldName)...)
		entry = append(entry, 0x00)
		entry = append(entry, uleb(typeIndex)...)
		return entry
	}
	imports := wasmVector(
		importEntry(wasmImportModule, "__wbg_new_99cabae501c0a8a0", synthTypeVoidI32),
		importEntry(wasmImportModule, "__wbindgen_cast_0000000000000002", synthTypeI32I32I32),
		importEntry(wasmImportModule, "__wbg_set_08463b1df38a7e29", synthTypeI32x3I32),
	)
	module = append(module, wasmSection(2, imports)...)

	// Functions (defined after the three imports).
	functions := wasmVector(
		uleb(synthTypeI32I32),
		uleb(synthTypeI32I32I32),
		uleb(synthTypeI32x3Void),
		uleb(synthTypeI32x3Void),
		uleb(synthTypeI32x9Void),
		uleb(synthTypeI32x10),
		uleb(synthTypeI32I32),
		uleb(synthTypeI32I32Void),
		uleb(synthTypeI32I32Void),
	)
	module = append(module, wasmSection(3, functions)...)

	// One memory of one page.
	module = append(module, wasmSection(5, wasmVector([]byte{0x00, 0x01}))...)

	// One mutable global holding the stack pointer.
	globalInit := []byte{0x7f, 0x01, 0x41}
	globalInit = append(globalInit, sleb(65536)...)
	globalInit = append(globalInit, 0x0b)
	module = append(module, wasmSection(6, wasmVector(globalInit))...)

	// Exports.
	exportEntry := func(fieldName string, kind byte, index uint32) []byte {
		entry := wasmName(fieldName)
		entry = append(entry, kind)
		entry = append(entry, uleb(index)...)
		return entry
	}
	exports := wasmVector(
		exportEntry("memory", 0x02, 0),
		exportEntry("__wbindgen_add_to_stack_pointer", 0x00, synthFuncStack),
		exportEntry("__wbindgen_export2", 0x00, synthFuncAlloc),
		exportEntry("__wbindgen_export4", 0x00, synthFuncFree),
		exportEntry("generate_runtime_auth_fields", 0x00, synthFuncAuthField),
		exportEntry("qodercontext_new", 0x00, synthFuncCtxNew),
		exportEntry("qodercontext_prepareInferRequest", 0x00, synthFuncPrepare),
		exportEntry("requestresult_headers", 0x00, synthFuncHeaders),
		exportEntry("requestresult_url", 0x00, synthFuncURL),
		exportEntry("requestresult_body", 0x00, synthFuncBody),
	)
	module = append(module, wasmSection(7, exports)...)

	// Code.
	stackBody := []byte{0x23, 0x00, 0x20, 0x00, 0x6a, 0x24, 0x00, 0x23, 0x00, 0x0b}
	allocBody := []byte{0x41}
	allocBody = append(allocBody, sleb(synthAllocPointer)...)
	allocBody = append(allocBody, 0x0b)

	authBody := []byte{}
	authBody = append(authBody, storeAt(0, synthAuthOffset, false)...)
	authBody = append(authBody, storeAt(4, int32(len(synthAuthJSON)), true)...)
	authBody = append(authBody, storeAt(8, 0, true)...)
	authBody = append(authBody, storeAt(12, 0, true)...)
	authBody = append(authBody, 0x0b)

	ctxBody := resultTupleBody(synthCtxPointer)
	prepareBody := resultTupleBody(synthResultPoinr)

	// requestresult_headers builds a Map through the host imports.
	headersBody := []byte{0x10}
	headersBody = append(headersBody, uleb(0)...) // call new Map
	headersBody = append(headersBody, 0x41)
	headersBody = append(headersBody, sleb(synthKeyOffset)...)
	headersBody = append(headersBody, 0x41)
	headersBody = append(headersBody, sleb(int32(len(synthHeaderKey)))...)
	headersBody = append(headersBody, 0x10)
	headersBody = append(headersBody, uleb(1)...) // call cast string
	headersBody = append(headersBody, 0x41)
	headersBody = append(headersBody, sleb(synthValueOffset)...)
	headersBody = append(headersBody, 0x41)
	headersBody = append(headersBody, sleb(int32(len(synthHeaderVal)))...)
	headersBody = append(headersBody, 0x10)
	headersBody = append(headersBody, uleb(1)...) // call cast string
	headersBody = append(headersBody, 0x10)
	headersBody = append(headersBody, uleb(2)...) // call set(map, key, value)
	headersBody = append(headersBody, 0x0b)

	code := wasmVector(
		wasmBody(stackBody),
		wasmBody(allocBody),
		wasmBody([]byte{0x0b}),
		wasmBody(authBody),
		wasmBody(ctxBody),
		wasmBody(prepareBody),
		wasmBody(headersBody),
		wasmBody(stringResultBody(synthURLOffset, int32(len(synthURL)))),
		wasmBody(stringResultBody(synthBodyOffset, int32(len(synthBody)))),
	)
	module = append(module, wasmSection(10, code)...)

	// One active data segment carrying every string the module returns.
	data := make([]byte, 0, 1024)
	data = append(data, []byte(synthAuthJSON)...)
	data = append(data, make([]byte, synthKeyOffset-synthAuthOffset-len(synthAuthJSON))...)
	data = append(data, []byte(synthHeaderKey)...)
	data = append(data, make([]byte, synthValueOffset-synthKeyOffset-len(synthHeaderKey))...)
	data = append(data, []byte(synthHeaderVal)...)
	data = append(data, make([]byte, synthURLOffset-synthValueOffset-len(synthHeaderVal))...)
	data = append(data, []byte(synthURL)...)
	data = append(data, make([]byte, synthBodyOffset-synthURLOffset-len(synthURL))...)
	data = append(data, []byte(synthBody)...)

	offsetExpr := []byte{0x41}
	offsetExpr = append(offsetExpr, sleb(synthAuthOffset)...)
	offsetExpr = append(offsetExpr, 0x0b)
	segment := append([]byte{0x00}, offsetExpr...)
	segment = append(segment, uleb(uint32(len(data)))...)
	segment = append(segment, data...)
	module = append(module, wasmSection(11, wasmVector(segment))...)

	return module
}

// resultTupleBody writes `ptr/errIdx/isErr` into the caller's stack slot.
func resultTupleBody(pointer int32) []byte {
	out := []byte{}
	out = append(out, storeAt(0, pointer, false)...)
	out = append(out, storeAt(4, 0, true)...)
	out = append(out, storeAt(8, 0, true)...)
	return append(out, 0x0b)
}

// stringResultBody writes `ptr/len` into the caller's stack slot.
func stringResultBody(offset, length int32) []byte {
	out := []byte{}
	out = append(out, storeAt(0, offset, false)...)
	out = append(out, storeAt(4, length, true)...)
	return append(out, 0x0b)
}

// TestSyntheticWasmImportLayout asserts the import section byte for byte.
//
// A hand-written encoder is easy to get wrong in a way wazero reports only
// indirectly ("invalid byte for importdesc"), so the expected bytes are spelled
// out here: module name (length-prefixed), field name (length-prefixed), the
// extern kind byte, then the type index. Getting the kind byte before the names
// is the classic mistake.
func TestSyntheticWasmImportLayout(t *testing.T) {
	raw := buildSyntheticWasm()
	if len(raw) < 8 || string(raw[0:4]) != "\x00asm" {
		t.Fatalf("module does not start with the wasm magic: % x", raw[:8])
	}

	records := []struct {
		field     string
		typeIndex uint32
	}{
		{"__wbg_new_99cabae501c0a8a0", synthTypeVoidI32},
		{"__wbindgen_cast_0000000000000002", synthTypeI32I32I32},
		{"__wbg_set_08463b1df38a7e29", synthTypeI32x3I32},
	}
	expected := uleb(uint32(len(records)))
	for _, record := range records {
		expected = append(expected, wasmName(wasmImportModule)...)
		expected = append(expected, wasmName(record.field)...)
		expected = append(expected, 0x00) // extern kind: func
		expected = append(expected, uleb(record.typeIndex)...)
	}

	section := wasmSection(2, expected)
	if !bytes.Contains(raw, section) {
		marker := bytes.Index(raw, []byte("qoder_auth_wasm_bg.js"))
		hint := ""
		if marker > 0 {
			hint = fmt.Sprintf("; the module name sits at offset %d and the byte before it is %#x "+
				"(that byte must be the LEB name length 0x17)", marker, raw[marker-1])
		}
		t.Fatalf("the import section does not match the expected bytes%s", hint)
	}
}

// syntheticSigner writes the module to a temporary file and loads it.
func syntheticSigner(t *testing.T) *wasmSigner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.wasm")
	if errWrite := os.WriteFile(path, buildSyntheticWasm(), 0o600); errWrite != nil {
		t.Fatalf("write synthetic module: %v", errWrite)
	}
	signer, errLoad := newWasmSigner(path)
	if errLoad != nil {
		t.Fatalf("load synthetic module: %v", errLoad)
	}
	t.Cleanup(func() { _ = signer.close() })
	return signer
}

// syntheticRequest is a sign request the wrapper can satisfy.
func syntheticRequest() signRequest {
	return signRequest{
		UID:           "u-1",
		Token:         "tok",
		MachineID:     "machine-1",
		ClientVersion: DefaultClientVersion,
		Metadata:      sharedClientMetadata,
		Host:          "https://api2.qoder.sh",
		Ask: inferAsk{
			ModelKey:    "qfmodel",
			UserText:    "hi",
			SessionType: "qodercli",
			Business:    map[string]any{"type": "agent"},
		},
	}
}

// TestSyntheticWasmSignRoundTrip drives the whole signer against a module built
// in the test: string results, pointer results, both stack layouts and the header
// Map.
func TestSyntheticWasmSignRoundTrip(t *testing.T) {
	signer := syntheticSigner(t)
	signed, errSign := signer.Sign(syntheticRequest())
	if errSign != nil {
		t.Fatalf("Sign: %v", errSign)
	}
	if signed.URL != synthURL {
		t.Fatalf("URL = %q, want %q", signed.URL, synthURL)
	}
	if signed.Body != synthBody {
		t.Fatalf("Body = %q, want %q", signed.Body, synthBody)
	}
	if got := signed.Headers[synthHeaderKey]; got != synthHeaderVal {
		t.Fatalf("headers = %v, want %s=%s (Map.set must populate the map)", signed.Headers, synthHeaderKey, synthHeaderVal)
	}
}

// TestSyntheticWasmSignIsRepeatable checks that a fresh instance per call leaves
// no state behind.
func TestSyntheticWasmSignIsRepeatable(t *testing.T) {
	signer := syntheticSigner(t)
	for attempt := 0; attempt < 3; attempt++ {
		signed, errSign := signer.Sign(syntheticRequest())
		if errSign != nil {
			t.Fatalf("Sign attempt %d: %v", attempt, errSign)
		}
		if signed.URL != synthURL || len(signed.Headers) == 0 {
			t.Fatalf("attempt %d produced %#v", attempt, signed)
		}
	}
}

// TestSyntheticWasmRequiresUID mirrors the adapter's refusal to send a request it
// knows would be rejected as `Signature invalid`.
func TestSyntheticWasmRequiresUID(t *testing.T) {
	signer := syntheticSigner(t)
	request := syntheticRequest()
	request.UID = ""
	if _, errSign := signer.Sign(request); errSign == nil {
		t.Fatal("Sign accepted a request without uid")
	}
}

// TestSignerForCachesAndValidatesPaths covers the loader contract.
func TestSignerForCachesAndValidatesPaths(t *testing.T) {
	resetSignerCache()
	t.Cleanup(resetSignerCache)

	if _, errSigner := signerFor(""); errSigner == nil {
		t.Fatal("signerFor(\"\") must fail: the public path needs no signer")
	}
	if _, errSigner := signerFor(filepath.Join(t.TempDir(), "missing.wasm")); errSigner == nil {
		t.Fatal("signerFor must fail for a missing file")
	}

	path := filepath.Join(t.TempDir(), "module.wasm")
	if errWrite := os.WriteFile(path, buildSyntheticWasm(), 0o600); errWrite != nil {
		t.Fatalf("write module: %v", errWrite)
	}
	first, errFirst := signerFor(path)
	if errFirst != nil {
		t.Fatalf("signerFor: %v", errFirst)
	}
	second, errSecond := signerFor(path)
	if errSecond != nil {
		t.Fatalf("signerFor (cached): %v", errSecond)
	}
	if first != second {
		t.Fatal("signerFor must reuse the compiled module for the same path")
	}

	garbage := filepath.Join(t.TempDir(), "garbage.wasm")
	if errWrite := os.WriteFile(garbage, []byte("not wasm at all"), 0o600); errWrite != nil {
		t.Fatalf("write garbage: %v", errWrite)
	}
	if _, errSigner := signerFor(garbage); errSigner == nil {
		t.Fatal("signerFor must fail to compile garbage")
	}
}

// TestObjectHeapMatchesWasmBindgen pins the 1028 reserved slots and the free-list
// reuse (`qoder-wasm.ts:231-251`).
func TestObjectHeapMatchesWasmBindgen(t *testing.T) {
	heap := newObjectHeap()
	if heap.get(0) == nil {
		t.Fatal("slot 0 must hold a sentinel")
	}
	if _, ok := heap.get(1025).(jsNull); !ok {
		t.Fatalf("slot 1025 = %T, want the null sentinel", heap.get(1025))
	}
	if value, ok := heap.get(1026).(bool); !ok || !value {
		t.Fatalf("slot 1026 = %#v, want true", heap.get(1026))
	}
	if value, ok := heap.get(1027).(bool); !ok || value {
		t.Fatalf("slot 1027 = %#v, want false", heap.get(1027))
	}

	first := heap.push("a")
	if first != 1028 {
		t.Fatalf("first push = %d, want 1028", first)
	}
	second := heap.push("b")
	if second != 1029 {
		t.Fatalf("second push = %d, want 1029", second)
	}
	heap.take(first)
	if reused := heap.push("c"); reused != first {
		t.Fatalf("reused slot = %d, want the freed %d", reused, first)
	}
	if heap.take(1027) != false {
		t.Fatal("taking a sentinel must not disturb the free list")
	}
}

// TestJSStringCoercion covers the two shapes the header map holds.
func TestJSStringCoercion(t *testing.T) {
	if got := jsString("text"); got != "text" {
		t.Fatalf("jsString(string) = %q", got)
	}
	if got := jsString([]byte("bytes")); got != "bytes" {
		t.Fatalf("jsString([]byte) = %q", got)
	}
	if got := jsString(float64(0.5)); got != "0.5" {
		t.Fatalf("jsString(number) = %q", got)
	}
	if got := jsString(jsNull{}); got != "null" {
		t.Fatalf("jsString(null) = %q", got)
	}
}

// TestInferAskFromRequestSplitsTheSystemPrompt keeps the system message out of
// the conversation body: the payload carries it in its own `system` block.
func TestInferAskFromRequestSplitsTheSystemPrompt(t *testing.T) {
	payload := `{"model":"qfmodel","max_tokens":64,"reasoning_effort":"low","messages":[
		{"role":"system","content":"be terse"},
		{"role":"user","content":"first"},
		{"role":"assistant","content":"ok"},
		{"role":"user","content":[{"type":"text","text":"second"}]}]}`
	request := executorRequestFixture(payload, "qfmodel")
	cfg := DefaultConfig()
	ask, errAsk := inferAskFromRequest(request, &Credential{AccessToken: "tok"}, cfg, productByID(string(RegionGlobal)))
	if errAsk != nil {
		t.Fatalf("inferAskFromRequest: %v", errAsk)
	}
	if ask.ModelKey != "qfmodel" || ask.UserText != "second" {
		t.Fatalf("ask = %#v, want the catalog key and the last user turn", ask)
	}
	if ask.SystemText != "be terse" {
		t.Fatalf("SystemText = %q, want the system message lifted out", ask.SystemText)
	}
	for _, message := range ask.History {
		if message.Role == "system" {
			t.Fatalf("the system message must not appear twice: %#v", ask.History)
		}
	}
	if len(ask.History) != 3 {
		t.Fatalf("history = %#v, want the three conversation turns", ask.History)
	}
	if !ask.IsReasoning || ask.IsVL == nil || !*ask.IsVL {
		t.Fatalf("catalog capability flags were not applied: %#v", ask)
	}
	if ask.DisplayName != "Qwen3.8-Flash" || ask.MaxInputTokens != 180_000 {
		t.Fatalf("catalog metadata was not applied: %#v", ask)
	}
	if ask.MaxTokens == nil || *ask.MaxTokens != 64 {
		t.Fatalf("MaxTokens = %v, want the request value", ask.MaxTokens)
	}
	if ask.ReasoningEffort != "low" {
		t.Fatalf("ReasoningEffort = %q", ask.ReasoningEffort)
	}
	if ask.SessionType != "qodercli" {
		t.Fatalf("SessionType = %q, want the global default", ask.SessionType)
	}
	if ask.Business["type"] != "agent" {
		t.Fatalf("Business = %v, want the mandatory agent routing field", ask.Business)
	}
}

// TestInferAskUsesTheCNSessionType keeps the per-region `session_type` rule from
// `qoder-wasm.ts:145-152`.
func TestInferAskUsesTheCNSessionType(t *testing.T) {
	request := executorRequestFixture(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, "auto")
	cfg := DefaultConfig()
	ask, errAsk := inferAskFromRequest(request, &Credential{AccessToken: "tok"}, cfg, productByID(string(RegionCN)))
	if errAsk != nil {
		t.Fatalf("inferAskFromRequest: %v", errAsk)
	}
	if ask.SessionType != "qoder_work" {
		t.Fatalf("SessionType = %q, want qoder_work on the CN site", ask.SessionType)
	}
	cfg.SessionType = "override"
	overridden, _ := inferAskFromRequest(request, &Credential{AccessToken: "tok"}, cfg, productByID(string(RegionCN)))
	if overridden.SessionType != "override" {
		t.Fatalf("SessionType = %q, want the explicit override", overridden.SessionType)
	}
}

// TestBuildInferPayloadShape pins the fields whose absence fails at runtime: a
// populated `chat_context`, the ten `model_config` fields and `business`
// (`qoder-wasm.ts:503-565`).
func TestBuildInferPayloadShape(t *testing.T) {
	maxTokens := 128
	ask := inferAsk{
		ModelKey: "qfmodel", UserText: "hi", SystemText: "sys",
		History:     []inferMessage{{Role: "user", Content: "hi"}},
		IsReasoning: true, MaxTokens: &maxTokens, ReasoningEffort: "low",
		DisplayName: "Qwen3.8-Flash", MaxInputTokens: 180_000,
		SessionType: "qodercli", Business: map[string]any{"type": "agent"},
	}
	encoded, errPayload := buildInferPayload(ask)
	if errPayload != nil {
		t.Fatalf("buildInferPayload: %v", errPayload)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(encoded, &decoded); errUnmarshal != nil {
		t.Fatalf("decode payload: %v", errUnmarshal)
	}
	for _, required := range []string{
		"request_id", "request_set_id", "chat_record_id", "session_id", "chat_context",
		"model_config", "messages", "system", "parameters", "business",
		"chat_task", "session_type", "agent_id", "task_id", "version", "source",
	} {
		if _, ok := decoded[required]; !ok {
			t.Errorf("payload is missing %q", required)
		}
	}
	if decoded["chat_task"] != "FREE_INPUT" || decoded["stream"] != true {
		t.Fatalf("payload header = %v/%v", decoded["chat_task"], decoded["stream"])
	}
	context, ok := decoded["chat_context"].(map[string]any)
	if !ok || context["text"] != "hi" {
		t.Fatalf("chat_context = %v, want the question text (an empty one fails upstream)", decoded["chat_context"])
	}
	modelConfig, ok := decoded["model_config"].(map[string]any)
	if !ok {
		t.Fatalf("model_config = %v", decoded["model_config"])
	}
	for _, field := range []string{
		"key", "display_name", "model", "format", "is_vl", "is_reasoning",
		"api_key", "url", "source", "max_input_tokens",
	} {
		if _, ok := modelConfig[field]; !ok {
			t.Errorf("model_config is missing %q", field)
		}
	}
	parameters, ok := decoded["parameters"].(map[string]any)
	if !ok || parameters["max_tokens"] != float64(128) || parameters["enable_thinking"] != true {
		t.Fatalf("parameters = %v", decoded["parameters"])
	}
	// The three identifiers are the same UUID, as upstream does.
	if decoded["request_id"] != decoded["request_set_id"] || decoded["request_id"] != decoded["chat_record_id"] {
		t.Fatal("request_id / request_set_id / chat_record_id must share one value")
	}
	if decoded["session_id"] == decoded["request_id"] {
		t.Fatal("session_id must be its own identifier")
	}
}

// TestBuildInferPayloadDisablesThinkingForNoneEffort covers the `enable_thinking`
// rule.
func TestBuildInferPayloadDisablesThinkingForNoneEffort(t *testing.T) {
	encoded, errPayload := buildInferPayload(inferAsk{ModelKey: "auto", UserText: "x", ReasoningEffort: "none"})
	if errPayload != nil {
		t.Fatalf("buildInferPayload: %v", errPayload)
	}
	if !strings.Contains(string(encoded), `"enable_thinking":false`) {
		t.Fatalf("payload = %s, want enable_thinking false for effort=none", encoded)
	}
}

// TestBuildInferPayloadDefaultsAnEmptyHistory keeps a request with no history
// usable.
func TestBuildInferPayloadDefaultsAnEmptyHistory(t *testing.T) {
	encoded, errPayload := buildInferPayload(inferAsk{ModelKey: "auto", UserText: "only"})
	if errPayload != nil {
		t.Fatalf("buildInferPayload: %v", errPayload)
	}
	if !strings.Contains(string(encoded), `"messages":[{"role":"user","content":"only"}]`) {
		t.Fatalf("payload = %s, want the question as the only message", encoded)
	}
}

// TestRealWasmArtifact is gated: the artifact is not redistributed with this
// repository, so the test runs only when QODER_TEST_WASM points at a local copy.
//
//	QODER_TEST_WASM=/path/qoder-auth-wasm.wasm go test ./plugins/qoder/ -run RealWasm -v
func TestRealWasmArtifact(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("QODER_TEST_WASM"))
	if path == "" {
		t.Skip("set QODER_TEST_WASM to a local copy of the signing artifact to run this")
	}
	signer, errLoad := newWasmSigner(path)
	if errLoad != nil {
		t.Fatalf("load real artifact: %v", errLoad)
	}
	defer func() { _ = signer.close() }()

	signed, errSign := signer.Sign(syntheticRequest())
	if errSign != nil {
		t.Fatalf("Sign with the real artifact: %v", errSign)
	}
	if !strings.Contains(signed.URL, "agent_chat_generation") {
		t.Fatalf("URL = %q, want the encrypted inference endpoint", signed.URL)
	}
	if !strings.Contains(signed.URL, "Encode=1") {
		t.Fatalf("URL = %q, want Encode=1", signed.URL)
	}
	authorization := signed.Headers["Authorization"]
	if !strings.HasPrefix(authorization, "Bearer COSY.") {
		t.Fatalf("Authorization = %q, want the WASM-generated Bearer COSY.<payload>.<signature>", authorization)
	}
	if len(signed.Headers) < 10 {
		t.Fatalf("header count = %d, want the full signed header set: %v", len(signed.Headers), signed.Headers)
	}
	if signed.Body == "" {
		t.Fatal("the encrypted body is empty")
	}
}

// binaryLittleEndian keeps the encoding/binary import honest for readers of this
// file: the stack tuples the module writes are little-endian by construction.
var _ = binary.LittleEndian
