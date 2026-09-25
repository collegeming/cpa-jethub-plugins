package main

// Optional WASM signer for the encrypted inference path.
//
// WHY THIS IS OPTIONAL AND NOT BUNDLED
//
// The signing headers are produced by `qoder-auth-wasm.wasm`, a ~292 KB
// third-party build artifact shipped inside the Qoder client. Redistributing
// that binary in this repository is a licensing decision that belongs to the
// repository owner, so the plugin does NOT embed it. Instead the `wasm_path`
// setting points at a local copy; when it is empty the plugin serves the public
// OpenAI-compatible endpoint and the status page says so.
//
// WHAT THE MODULE EXPORTS (all verified by loading the real artifact with
// wazero; see the report for the raw output):
//
//	generate_runtime_auth_fields(stack, ptr, len)                     -> void
//	qodercontext_new(stack, machine, mlen, version, vlen, userInfo, ilen, clientMeta, mlen2) -> void
//	qodercontext_prepareInferRequest(stack, ctx, host, hlen, body, blen, key, klen, source, slen) -> void
//	requestresult_url(stack, ptr) / requestresult_body(stack, ptr)    -> void
//	requestresult_headers(ptr) -> i32   (heap index of a JS Map)
//	requestresult_headerCount(ptr) -> i32
//	__wbindgen_export2(len, align) -> ptr  / __wbindgen_export4(ptr, len, align) -> void
//	__wbindgen_add_to_stack_pointer(delta) -> i32
//
// Every signature, argument order and result layout comes from
// `src/qoder-wasm.ts`:
//   - string results are `ptr/len/valIdx/isErr` (`qoder-wasm.ts:262-278`);
//   - object results are `ptr/errIdx/isErr` (`qoder-wasm.ts:280-292`);
//   - `requestresult_url(栈指针, ptr)` has the stack pointer FIRST
//     (`qoder-wasm.ts:36`, `:599-602`);
//   - `machineId` is mandatory (`qoder-wasm.ts:414-416`).
//
// ⚠️ ONE DELIBERATE DEVIATION FROM THE TYPESCRIPT GLUE
//
// `__wbg_set_08463b1df38a7e29` is shared by `Map.prototype.set` and
// `Uint8Array.prototype.set`. The TypeScript implements it as the typed-array
// form only (`qoder-wasm.ts:309-310`), but the encrypted path's header map is
// built through exactly this import, so Jet-Hub's `requestresult_headers` would
// yield a Map keyed by Uint8Array objects instead of the 20 string headers. The
// Go glue dispatches on the receiver type, which is what the upstream
// wasm-bindgen glue does. Without that fix the request would carry no
// `Authorization: Bearer COSY.<payload>.<signature>` at all. See the report.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// wasmImportModule is the module name embedded in the WASM binary; it must match
// exactly or instantiation fails for a missing import (`qoder-wasm.ts:68-72`).
const wasmImportModule = "./qoder_auth_wasm_bg.js"

// inferSigner produces one signed encrypted-inference request.
type inferSigner interface {
	Sign(request signRequest) (*signedRequest, error)
}

// signedRequest is the product of one signing operation.
type signedRequest struct {
	URL     string
	Headers map[string]string
	Body    string
}

// signRequest describes the account and ask handed to the signer.
type signRequest struct {
	UID           string
	Token         string
	MachineID     string
	ClientVersion string
	Metadata      clientMetadata
	// Host is the encrypted inference base URL. It must be the
	// `agent_chat_generation` host: passing the public host answers 404
	// (`qoder-wasm.ts:455-461`).
	Host string
	Ask  inferAsk
}

// inferMessage is one history entry (`QoderInferMessage`, `qoder-wasm.ts:104-108`).
type inferMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// inferAsk mirrors `QoderInferAsk` (`qoder-wasm.ts:110-167`).
type inferAsk struct {
	// ModelKey is the CATALOG key (`qfmodel`, ...), not a generic name.
	ModelKey string
	UserText string
	// SystemText is optional; the payload's `system` block.
	SystemText string
	// History is the conversation, oldest first.
	History []inferMessage
	// IsReasoning is written to `model_config.is_reasoning` and is taken from
	// the catalog's `is_reasoning` (`qoder-adapter.ts:296-297`).
	IsReasoning bool
	// MaxTokens is optional.
	MaxTokens *int
	// ReasoningEffort, when set, also sets `parameters.enable_thinking`
	// (`qoder-wasm.ts:508-511`).
	ReasoningEffort string
	// IsVL mirrors the catalog's `is_vl`.
	IsVL *bool
	// DisplayName / MaxInputTokens mirror the catalog's `display_name` and
	// `max_input_tokens` (`qoder-wasm.ts:544-557`).
	DisplayName    string
	MaxInputTokens int64
	// Source / Format come from the catalog (`qoder-wasm.ts:141-142`).
	Source string
	Format string
	// SessionType is `qodercli` on the global site and `qoder_work` on the CN
	// site (`qoder-wasm.ts:145-152`).
	SessionType string
	// Business decides which server pool the request is routed to. It is NOT
	// optional: without it `qfmodel` is routed to a broken node and fails with
	// `[FAIL]node:... msg:Execution failed` (`qoder-wasm.ts:153-166`,
	// `qoder-adapter.ts:305-315`).
	Business map[string]any
}

// signerCache keeps loaded modules per path.
var (
	signerMu    sync.Mutex
	signerCache = map[string]*wasmSigner{}
)

// signerFor loads (once) and returns the signer for a WASM path.
func signerFor(path string) (inferSigner, error) {
	trimmed := path
	if trimmed == "" {
		return nil, fmt.Errorf("wasm_path 未配置")
	}
	signerMu.Lock()
	defer signerMu.Unlock()
	if cached, ok := signerCache[trimmed]; ok {
		return cached, nil
	}
	signer, errLoad := newWasmSigner(trimmed)
	if errLoad != nil {
		return nil, errLoad
	}
	signerCache[trimmed] = signer
	return signer, nil
}

// resetSignerCache drops every loaded module; used by tests.
func resetSignerCache() {
	signerMu.Lock()
	signers := make([]*wasmSigner, 0, len(signerCache))
	for _, signer := range signerCache {
		signers = append(signers, signer)
	}
	signerCache = map[string]*wasmSigner{}
	signerMu.Unlock()
	for _, signer := range signers {
		_ = signer.runtime.Close(context.Background())
	}
}

// wasmSigner owns a compiled module and the host imports it needs.
type wasmSigner struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule

	// mu serialises Sign; current points at the glue of the instance in use.
	mu      sync.Mutex
	current *wasmGlue
}

// newWasmSigner compiles the module and installs the wasm-bindgen host imports.
func newWasmSigner(path string) (*wasmSigner, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, errRead)
	}
	ctx := context.Background()
	// Cap growth so a corrupt module cannot exhaust the process.
	config := wazero.NewRuntimeConfig().WithMemoryLimitPages(1024)
	runtime := wazero.NewRuntimeWithConfig(ctx, config)
	compiled, errCompile := runtime.CompileModule(ctx, raw)
	if errCompile != nil {
		_ = runtime.Close(ctx)
		return nil, fmt.Errorf("编译 %s 失败：%w", path, errCompile)
	}
	signer := &wasmSigner{runtime: runtime, compiled: compiled}
	if errImports := signer.installImports(ctx); errImports != nil {
		_ = runtime.Close(ctx)
		return nil, errImports
	}
	return signer, nil
}

// close releases the runtime backing a compiled module.
func (s *wasmSigner) close() error { return s.runtime.Close(context.Background()) }

// glueFor returns the glue of the instance currently being signed.
func (s *wasmSigner) glueFor() *wasmGlue { return s.current }

// Sign builds one signed request on a FRESH module instance.
//
// A new instance per call is deliberate: the module keeps state in linear memory
// and offers no cheap way to release a context, so a fresh instance bounds both
// memory and state leakage. Instantiation measured ~0.5 ms on the real artifact.
func (s *wasmSigner) Sign(request signRequest) (*signedRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx := context.Background()
	instance, errInstantiate := s.runtime.InstantiateModule(ctx, s.compiled, wazero.NewModuleConfig())
	if errInstantiate != nil {
		return nil, fmt.Errorf("实例化 WASM 失败：%w", errInstantiate)
	}
	defer func() { _ = instance.Close(ctx) }()

	memory := instance.Memory()
	if memory == nil {
		return nil, fmt.Errorf("WASM 没有导出 memory")
	}
	glue := newWasmGlue(ctx, instance, memory)
	s.current = glue
	defer func() { s.current = nil }()

	return glue.sign(request)
}

// ---------------------------------------------------------------------------
// glue
// ---------------------------------------------------------------------------

// wasmGlue holds the per-instance object heap and memory handle.
type wasmGlue struct {
	ctx  context.Context
	mod  api.Module
	mem  api.Memory
	heap *objectHeap
}

func newWasmGlue(ctx context.Context, mod api.Module, memory api.Memory) *wasmGlue {
	return &wasmGlue{ctx: ctx, mod: mod, mem: memory, heap: newObjectHeap()}
}

// sign drives the exported entry points in the order `qoder-wasm.ts` does.
func (g *wasmGlue) sign(request signRequest) (*signedRequest, error) {
	// 1. `generate_runtime_auth_fields` (`qoder-wasm.ts:391-405`).
	if request.UID == "" {
		return nil, fmt.Errorf("凭据缺少 uid，加密推理无法签名")
	}
	authPayload, errMarshal := json.Marshal(map[string]any{
		"uid":                  request.UID,
		"security_oauth_token": request.Token,
		"organization_id":      "",
		"organization_tags":    []string{},
		"data_policy_agreed":   false,
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("encode runtime auth payload: %w", errMarshal)
	}
	authJSON, errAuth := g.callStringExport("generate_runtime_auth_fields", func(stack uint32) error {
		offset, length := g.writeString(string(authPayload))
		return g.callVoid("generate_runtime_auth_fields", stack, offset, length)
	})
	if errAuth != nil {
		return nil, fmt.Errorf("generate_runtime_auth_fields: %w", errAuth)
	}
	var fields struct {
		EncryptUserInfo string `json:"encrypt_user_info"`
		Key             string `json:"key"`
	}
	if errDecode := json.Unmarshal([]byte(authJSON), &fields); errDecode != nil {
		return nil, fmt.Errorf("decode runtime auth fields: %w", errDecode)
	}

	// 2. Build the QoderContext (`qoder-wasm.ts:469-485`).
	userInfo, errUserInfo := json.Marshal(map[string]any{
		"uid":                request.UID,
		"encrypt_user_info":  fields.EncryptUserInfo,
		"key":                fields.Key,
		"organization_id":    "",
		"organization_tags":  []string{},
		"data_policy_agreed": false,
	})
	if errUserInfo != nil {
		return nil, fmt.Errorf("encode user info: %w", errUserInfo)
	}
	metadataJSON, errMetadata := json.Marshal(request.Metadata)
	if errMetadata != nil {
		return nil, fmt.Errorf("encode client metadata: %w", errMetadata)
	}
	contextPtr, errContext := g.callPointerExport("qodercontext_new", func(stack uint32) error {
		machine, machineLen := g.writeString(request.MachineID)
		version, versionLen := g.writeString(request.ClientVersion)
		info, infoLen := g.writeString(string(userInfo))
		meta, metaLen := g.writeString(string(metadataJSON))
		return g.callVoid("qodercontext_new", stack, machine, machineLen,
			version, versionLen, info, infoLen, meta, metaLen)
	})
	if errContext != nil {
		return nil, fmt.Errorf("qodercontext_new: %w", errContext)
	}

	// 3. Build the encrypted request (`qoder-wasm.ts:567-575`).
	payload, errPayload := buildInferPayload(request.Ask)
	if errPayload != nil {
		return nil, errPayload
	}
	source := request.Ask.Source
	if source == "" {
		source = "system"
	}
	resultPtr, errPrepare := g.callPointerExport("qodercontext_prepareInferRequest", func(stack uint32) error {
		host, hostLen := g.writeString(request.Host)
		body, bodyLen := g.writeString(string(payload))
		key, keyLen := g.writeString(request.Ask.ModelKey)
		sourcePtr, sourceLen := g.writeString(source)
		return g.callVoid("qodercontext_prepareInferRequest", stack, contextPtr,
			host, hostLen, body, bodyLen, key, keyLen, sourcePtr, sourceLen)
	})
	if errPrepare != nil {
		return nil, fmt.Errorf("qodercontext_prepareInferRequest: %w", errPrepare)
	}

	// 4. Read the result (`qoder-wasm.ts:577-603`).
	headers, errHeaders := g.resultHeaders(resultPtr)
	if errHeaders != nil {
		return nil, errHeaders
	}
	url, errURL := g.callResultString("requestresult_url", resultPtr)
	if errURL != nil {
		return nil, fmt.Errorf("requestresult_url: %w", errURL)
	}
	body, errBody := g.callResultString("requestresult_body", resultPtr)
	if errBody != nil {
		return nil, fmt.Errorf("requestresult_body: %w", errBody)
	}
	return &signedRequest{URL: url, Headers: headers, Body: body}, nil
}

// resultHeaders reads the header Map produced by `requestresult_headers`.
//
// The Map is the reason the dual-mode `__wbg_set_...` implementation matters:
// with the TypeScript glue the map stays effectively empty and the request goes
// out unsigned.
func (g *wasmGlue) resultHeaders(result uint32) (map[string]string, error) {
	index, errCall := g.callI32("requestresult_headers", result)
	if errCall != nil {
		return nil, fmt.Errorf("requestresult_headers: %w", errCall)
	}
	raw := g.heap.take(int32(uint32(index)))
	values, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("requestresult_headers 返回了 %T，期望 Map", raw)
	}
	headers := make(map[string]string, len(values))
	for key, value := range values {
		headers[key] = jsString(value)
	}
	if len(headers) == 0 {
		return nil, fmt.Errorf("WASM 未生成任何请求头（签名头缺失）")
	}
	return headers, nil
}

// callStringExport implements the `ptr/len/valIdx/isErr` result layout
// (`qoder-wasm.ts:262-278`).
func (g *wasmGlue) callStringExport(name string, invoke func(stack uint32) error) (string, error) {
	stack, errStack := g.pushStack()
	if errStack != nil {
		return "", errStack
	}
	if errInvoke := invoke(stack); errInvoke != nil {
		return "", errInvoke
	}
	pointer := g.readInt32(stack, 0)
	length := g.readInt32(stack, 4)
	valueIndex := g.readInt32(stack, 8)
	isError := g.readInt32(stack, 12)
	if errPop := g.popStack(); errPop != nil {
		return "", errPop
	}
	if isError != 0 {
		return "", fmt.Errorf("WASM 抛出异常：%v", g.heap.take(valueIndex))
	}
	if pointer == 0 {
		return "", nil
	}
	text := g.readString(pointer, length)
	if _, errFree := g.callI32("__wbindgen_export4", uint32(pointer), uint32(length), 1); errFree != nil {
		// A failed deallocation is not fatal: the instance is dropped anyway.
		_ = errFree
	}
	return text, nil
}

// callPointerExport implements the `ptr/errIdx/isErr` result layout
// (`qoder-wasm.ts:280-292`).
func (g *wasmGlue) callPointerExport(name string, invoke func(stack uint32) error) (uint32, error) {
	stack, errStack := g.pushStack()
	if errStack != nil {
		return 0, errStack
	}
	if errInvoke := invoke(stack); errInvoke != nil {
		return 0, errInvoke
	}
	pointer := g.readInt32(stack, 0)
	errorIndex := g.readInt32(stack, 4)
	isError := g.readInt32(stack, 8)
	if errPop := g.popStack(); errPop != nil {
		return 0, errPop
	}
	if isError != 0 {
		return 0, fmt.Errorf("WASM 抛出异常：%v", g.heap.take(errorIndex))
	}
	return uint32(pointer), nil
}

// callResultString implements `requestresult_url/_body(stack, ptr)`, whose stack
// pointer comes first (`qoder-wasm.ts:36`, `:583-596`).
func (g *wasmGlue) callResultString(name string, pointer uint32) (string, error) {
	stack, errStack := g.pushStack()
	if errStack != nil {
		return "", errStack
	}
	if errCall := g.callVoid(name, stack, pointer); errCall != nil {
		return "", errCall
	}
	offset := g.readInt32(stack, 0)
	length := g.readInt32(stack, 4)
	if errPop := g.popStack(); errPop != nil {
		return "", errPop
	}
	if offset == 0 {
		return "", nil
	}
	return g.readString(offset, length), nil
}

// pushStack reserves 16 bytes of WASM stack for a result tuple.
func (g *wasmGlue) pushStack() (uint32, error) {
	// -16 as an unsigned 32-bit value; the module sign-handles it.
	stack, errCall := g.callI32("__wbindgen_add_to_stack_pointer", ^uint32(15))
	if errCall != nil {
		return 0, errCall
	}
	return stack, nil
}

// popStack releases the 16 bytes reserved by pushStack.
func (g *wasmGlue) popStack() error {
	_, errCall := g.callI32("__wbindgen_add_to_stack_pointer", 16)
	return errCall
}

// writeString copies text into WASM memory and returns its offset and length.
func (g *wasmGlue) writeString(text string) (uint32, uint32) {
	encoded := []byte(text)
	offset, errAlloc := g.callI32("__wbindgen_export2", uint32(len(encoded)), 1)
	if errAlloc != nil || offset == 0 {
		return 0, 0
	}
	g.mem.Write(offset, encoded)
	return offset, uint32(len(encoded))
}

// readString reads a UTF-8 string out of WASM memory.
func (g *wasmGlue) readString(offset, length int32) string {
	if length <= 0 {
		return ""
	}
	raw, ok := g.mem.Read(uint32(offset), uint32(length))
	if !ok {
		return ""
	}
	return string(raw)
}

// readInt32 reads one little-endian int32 from WASM memory.
func (g *wasmGlue) readInt32(base uint32, offset int) int32 {
	raw, ok := g.mem.Read(base+uint32(offset), 4)
	if !ok {
		return 0
	}
	return int32(uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24)
}

// callI32 invokes an exported function and returns its i32 result.
func (g *wasmGlue) callI32(name string, args ...uint32) (uint32, error) {
	function := g.mod.ExportedFunction(name)
	if function == nil {
		return 0, fmt.Errorf("WASM 缺少导出 %s", name)
	}
	params := make([]uint64, 0, len(args))
	for _, arg := range args {
		params = append(params, uint64(arg))
	}
	results, errCall := function.Call(g.ctx, params...)
	if errCall != nil {
		return 0, errCall
	}
	if len(results) == 0 {
		return 0, nil
	}
	return uint32(results[0]), nil
}

// callVoid invokes an exported function that returns nothing.
func (g *wasmGlue) callVoid(name string, args ...uint32) error {
	_, errCall := g.callI32(name, args...)
	return errCall
}

// ---------------------------------------------------------------------------
// object heap
// ---------------------------------------------------------------------------

// The wasm-bindgen object heap: JavaScript values live behind integer indices,
// the first 1028 of which are reserved sentinels (`qoder-wasm.ts:231-251`).
type objectHeap struct {
	objects   []any
	firstFree int
}

// jsUndefined / jsNull mark the two sentinel values the module can hold.
type jsUndefined struct{}
type jsNull struct{}

// jsGlobal stands in for `globalThis`; jsCrypto / jsProcess / jsVersions stand in
// for the objects wasm-bindgen reaches for when it needs randomness.
type jsGlobal struct{}
type jsCrypto struct{}
type jsProcess struct{}
type jsVersions struct{}
type jsError struct{ Message string }

// memView is a Uint8Array view over WASM memory.
type memView struct{ offset, length int32 }

// newObjectHeap builds the heap exactly as `qoder-wasm.ts:231-235` does: 1024
// undefined slots followed by the undefined/null/true/false sentinels.
func newObjectHeap() *objectHeap {
	heap := &objectHeap{objects: make([]any, 1024)}
	for index := range heap.objects {
		heap.objects[index] = jsUndefined{}
	}
	heap.objects = append(heap.objects, jsUndefined{}, jsNull{}, true, false)
	heap.firstFree = len(heap.objects)
	return heap
}

// get reads a heap slot without consuming it.
func (h *objectHeap) get(index int32) any { return h.objects[index] }

// push stores a value and returns its index (`pushObject`, `qoder-wasm.ts:236-242`).
func (h *objectHeap) push(value any) int32 {
	if h.firstFree == len(h.objects) {
		h.objects = append(h.objects, len(h.objects)+1)
	}
	index := h.firstFree
	h.firstFree = h.objects[index].(int)
	h.objects[index] = value
	return int32(index)
}

// take reads and frees a heap slot (`takeObject`, `qoder-wasm.ts:243-251`).
func (h *objectHeap) take(index int32) any {
	value := h.objects[index]
	if index >= 1028 {
		h.objects[index] = h.firstFree
		h.firstFree = int(index)
	}
	return value
}

// jsString renders a JavaScript value the way string coercion would.
func jsString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case string:
		return typed
	case jsUndefined:
		return "undefined"
	case jsNull:
		return "null"
	case []byte:
		return string(typed)
	case memView:
		return fmt.Sprintf("%d bytes", typed.length)
	case float64:
		return formatFactor(typed)
	case int32:
		return itoaInt(int(typed))
	case int:
		return itoaInt(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// ---------------------------------------------------------------------------
// host imports (the JS side of wasm-bindgen)
// ---------------------------------------------------------------------------

// i32 is a short alias for the value type used by every import below.
var vtI32 = api.ValueTypeI32

// installImports registers every function the module imports. The set and the
// exact signatures were taken from the compiled module itself; they match the
// glue implemented in `qoder-wasm.ts:303-366` one for one.
func (s *wasmSigner) installImports(ctx context.Context) error {
	builder := s.runtime.NewHostModuleBuilder(wasmImportModule)
	glue := s.glueFor

	one := []api.ValueType{vtI32}
	two := []api.ValueType{vtI32, vtI32}
	three := []api.ValueType{vtI32, vtI32, vtI32}
	zero := []api.ValueType{}
	result := []api.ValueType{vtI32}

	// define registers a function returning i32.
	define := func(name string, params []api.ValueType, body func(g *wasmGlue, args []uint64) uint32) {
		builder.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(
			func(_ context.Context, _ api.Module, stack []uint64) {
				stack[0] = uint64(body(glue(), stack))
			},
		), params, result).Export(name)
	}
	// defineVoid registers a function returning nothing.
	defineVoid := func(name string, params []api.ValueType, body func(g *wasmGlue, args []uint64)) {
		builder.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(
			func(_ context.Context, _ api.Module, stack []uint64) { body(glue(), stack) },
		), params, nil).Export(name)
	}
	defineF64 := func(name string, params []api.ValueType, body func(g *wasmGlue, args []uint64) float64) {
		builder.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(
			func(_ context.Context, _ api.Module, stack []uint64) {
				stack[0] = math.Float64bits(body(glue(), stack))
			},
		), params, []api.ValueType{api.ValueTypeF64}).Export(name)
	}

	defineVoid("__wbindgen_object_drop_ref", one, func(g *wasmGlue, args []uint64) {
		g.heap.take(i32At(args, 0))
	})
	define("__wbindgen_object_clone_ref", one, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(g.heap.get(i32At(args, 0))))
	})
	define("__wbindgen_cast_0000000000000001", two, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(memView{offset: i32At(args, 0), length: i32At(args, 1)}))
	})
	define("__wbindgen_cast_0000000000000002", two, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(g.readString(i32At(args, 0), i32At(args, 1))))
	})

	// Shared glue: Map.prototype.set OR Uint8Array.prototype.set.
	define("__wbg_set_08463b1df38a7e29", three, func(g *wasmGlue, args []uint64) uint32 {
		container := g.heap.get(i32At(args, 0))
		key := g.heap.get(i32At(args, 1))
		value := g.heap.get(i32At(args, 2))
		switch target := container.(type) {
		case map[string]any:
			target[jsString(key)] = jsString(value)
			return uint32(g.heap.push(target))
		case []byte:
			g.copyInto(target, value, 0)
			return uint32(g.heap.push(nil))
		case memView:
			offset := int32(0)
			if number, ok := value.(float64); ok {
				offset = 0
				_ = number
			}
			g.copyIntoMemory(target.offset+offset, value)
			return uint32(g.heap.push(nil))
		}
		return uint32(g.heap.push(nil))
	})

	// Randomness. Two differently shaped entry points, exactly as the module
	// imports them (`qoder-wasm.ts:311-319`).
	defineVoid("__wbg_getRandomValues_d49329ff89a07af1", two, func(g *wasmGlue, args []uint64) {
		g.fillRandom(i32At(args, 0), i32At(args, 1))
	})
	defineVoid("__wbg_getRandomValues_c44a50d8cfdaebeb", two, func(g *wasmGlue, args []uint64) {
		g.fillRandomView(g.heap.get(i32At(args, 1)))
	})
	define("__wbg_crypto_38df2bab126b63dc", one, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(&jsCrypto{}))
	})
	define("__wbg_process_44c7a14e11e9f69e", one, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(&jsProcess{}))
	})
	define("__wbg_versions_276b2795b1c6a219", one, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(&jsVersions{}))
	})
	define("__wbg_node_84ea875411254db1", one, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push("v22.0.0"))
	})
	define("__wbg_require_b4edbdcf3e2a1ef0", zero, func(g *wasmGlue, args []uint64) uint32 {
		// The TypeScript glue returns the compiled WebAssembly.Module here
		// (`qoder-wasm.ts:328`); the value is never dereferenced by the module.
		return uint32(g.heap.push(&jsCrypto{}))
	})
	define("__wbg_msCrypto_bd5a034af96bcba6", one, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(&jsCrypto{}))
	})
	defineVoid("__wbg_randomFillSync_6c25eac9869eb53c", two, func(g *wasmGlue, args []uint64) {
		g.fillRandomView(g.heap.take(i32At(args, 1)))
	})

	define("__wbg_call_d578befcc3145dee", three, func(g *wasmGlue, args []uint64) uint32 {
		// Function.prototype.call; the module never passes a function in this
		// plugin, so an unknown callee returns undefined instead of throwing.
		return uint32(g.heap.push(jsUndefined{}))
	})

	define("__wbg_new_with_length_9cedd08484b73942", one, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(make([]byte, i32At(args, 0))))
	})
	define("__wbg_length_0c32cb8543c8e4c8", one, func(g *wasmGlue, args []uint64) uint32 {
		switch value := g.heap.get(i32At(args, 0)).(type) {
		case []byte:
			return uint32(len(value))
		case string:
			return uint32(len(value))
		case memView:
			return uint32(value.length)
		case map[string]any:
			return uint32(len(value))
		default:
			return 0
		}
	})
	defineVoid("__wbg_prototypesetcall_3e05eb9545565046", three, func(g *wasmGlue, args []uint64) {
		// Uint8Array.prototype.set.call(memory.subarray(dst, dst+len), view).
		// The source is BORROWED here — the TypeScript glue uses heapObject,
		// not takeObject (`qoder-wasm.ts:342-344`).
		g.copyIntoMemory(i32At(args, 0), g.heap.get(i32At(args, 2)))
	})
	define("__wbg_subarray_0f98d3fb634508ad", three, func(g *wasmGlue, args []uint64) uint32 {
		container := g.heap.get(i32At(args, 0))
		start := i32At(args, 1)
		end := i32At(args, 2)
		switch target := container.(type) {
		case []byte:
			if start < 0 || end > int32(len(target)) || start > end {
				return uint32(g.heap.push(jsUndefined{}))
			}
			return uint32(g.heap.push(target[start:end]))
		case memView:
			return uint32(g.heap.push(memView{offset: target.offset + start, length: end - start}))
		case string:
			if start < 0 || end > int32(len(target)) || start > end {
				return uint32(g.heap.push(jsUndefined{}))
			}
			return uint32(g.heap.push(target[start:end]))
		default:
			return uint32(g.heap.push(jsUndefined{}))
		}
	})
	define("__wbg_new_99cabae501c0a8a0", zero, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(map[string]any{}))
	})
	defineF64("__wbg_now_88621c9c9a4f3ffc", zero, func(g *wasmGlue, args []uint64) float64 {
		return float64(time.Now().UnixMilli())
	})

	global := func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(&jsGlobal{}))
	}
	define("__wbg_static_accessor_GLOBAL_THIS_a1248013d790bf5f", zero, global)
	define("__wbg_static_accessor_GLOBAL_f2e0f995a21329ff", zero, global)
	define("__wbg_static_accessor_SELF_24f78b6d23f286ea", zero, global)
	define("__wbg_static_accessor_WINDOW_59fd959c540fe405", zero, global)

	defineVoid("__wbg___wbindgen_throw_81fc77679af83bc6", two, func(g *wasmGlue, args []uint64) {
		panic("qoder wasm throw: " + g.readString(i32At(args, 0), i32At(args, 1)))
	})
	define("__wbg_Error_2e59b1b37a9a34c3", two, func(g *wasmGlue, args []uint64) uint32 {
		return uint32(g.heap.push(&jsError{Message: g.readString(i32At(args, 0), i32At(args, 1))}))
	})

	isObject := func(g *wasmGlue, args []uint64) uint32 {
		switch g.heap.get(i32At(args, 0)).(type) {
		case jsUndefined, jsNull, nil:
			return 0
		default:
			return 1
		}
	}
	define("__wbg___wbindgen_is_object_40c5a80572e8f9d3", one, isObject)
	define("__wbg___wbindgen_is_string_b29b5c5a8065ba1a", one, func(g *wasmGlue, args []uint64) uint32 {
		if _, ok := g.heap.get(i32At(args, 0)).(string); ok {
			return 1
		}
		return 0
	})
	define("__wbg___wbindgen_is_function_49868bde5eb1e745", one, func(g *wasmGlue, args []uint64) uint32 {
		return 0
	})
	define("__wbg___wbindgen_is_undefined_c0cca72b82b86f4d", one, func(g *wasmGlue, args []uint64) uint32 {
		if _, ok := g.heap.get(i32At(args, 0)).(jsUndefined); ok {
			return 1
		}
		return 0
	})

	_, errInstantiate := builder.Instantiate(ctx)
	return errInstantiate
}

// i32At reads an argument as the unsigned 32-bit value the module passed.
//
// The stack slots are uint64 and carry unrelated high bits (wazero keeps the raw
// register content), so every read must truncate to 32 bits.
func i32At(args []uint64, index int) int32 {
	if index >= len(args) {
		return 0
	}
	return int32(uint32(args[index]))
}

// fillRandom writes random bytes into WASM memory at the given offset.
func (g *wasmGlue) fillRandom(offset, length int32) {
	if length <= 0 {
		return
	}
	buffer := make([]byte, length)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return
	}
	g.mem.Write(uint32(offset), buffer)
}

// fillRandomView fills a typed array held in the heap.
func (g *wasmGlue) fillRandomView(value any) {
	switch target := value.(type) {
	case []byte:
		_, _ = rand.Read(target)
	case memView:
		g.fillRandom(target.offset, target.length)
	}
}

// copyInto copies a heap value into a Go byte slice.
func (g *wasmGlue) copyInto(destination []byte, source any, offset int) {
	switch typed := source.(type) {
	case []byte:
		copy(destination[offset:], typed)
	case memView:
		if raw, ok := g.mem.Read(uint32(typed.offset), uint32(typed.length)); ok {
			copy(destination[offset:], raw)
		}
	case string:
		copy(destination[offset:], typed)
	}
}

// copyIntoMemory copies a heap value into WASM memory at the given offset.
func (g *wasmGlue) copyIntoMemory(offset int32, source any) {
	switch typed := source.(type) {
	case []byte:
		g.mem.Write(uint32(offset), typed)
	case memView:
		if raw, ok := g.mem.Read(uint32(typed.offset), uint32(typed.length)); ok {
			g.mem.Write(uint32(offset), raw)
		}
	case string:
		g.mem.Write(uint32(offset), []byte(typed))
	}
}
