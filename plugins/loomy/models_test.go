package main

import (
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The multiplier lives in the model NAME, in two bracket spellings plus the
// normalised `· x` form this plugin emits (`loomy.ts:111-148`).
func TestSplitLoomyRateBothBracketStyles(t *testing.T) {
	cases := []struct {
		input string
		name  string
		rate  string
	}{
		// Full-width brackets with a space — a real wire value.
		{"MiniMax M3 （x4.0）", "MiniMax M3", "x4.0"},
		// Half-width brackets with a space — a real wire value.
		{"Qwen 3.8 Max (x12.0)", "Qwen 3.8 Max", "x12.0"},
		// Half-width brackets without a space — a real wire value.
		{"GLM 5.3 Flash(x0.8)", "GLM 5.3 Flash", "x0.8"},
		// The normalised form the display name uses.
		{"DeepSeek V4 Flash 0731 · x3.0", "DeepSeek V4 Flash 0731", "x3.0"},
		// Whitespace inside the multiplier is squeezed and lower-cased.
		{"Spark X2.5 ( X 0.1 )", "Spark X2.5", "x0.1"},
		// No multiplier at all (image models have none).
		{"MiMo V2.5", "MiMo V2.5", ""},
		// A bracket in the MIDDLE belongs to the model name.
		{"Qwen (Preview) Max", "Qwen (Preview) Max", ""},
		// A trailing bracket that is not a multiplier is part of the name.
		{"Some Model (beta)", "Some Model (beta)", ""},
		// A bracket-only name keeps the original text and reports no rate.
		{"（x4.0）", "（x4.0）", ""},
		{"", "", ""},
	}
	for _, testCase := range cases {
		name, rate := splitLoomyRate(testCase.input)
		if name != testCase.name || rate != testCase.rate {
			t.Errorf("splitLoomyRate(%q) = (%q, %q), want (%q, %q)",
				testCase.input, name, rate, testCase.name, testCase.rate)
		}
	}
}

// The function must be IDEMPOTENT (`loomy.ts:118-119`): splitting a display name
// yields the same pair as splitting the raw name. Without that, resolveModel
// returns `Spark X2.5 · x0.1` instead of `Spark X2.5` (trap #15).
func TestSplitLoomyRateIsIdempotent(t *testing.T) {
	for _, raw := range []string{
		"MiniMax M3 （x4.0）",
		"Qwen 3.8 Max (x12.0)",
		"GLM 5.3 Flash(x0.8)",
		"Spark X2.5 · x0.1",
		"MiMo V2.5",
	} {
		display := loomyDisplayName(raw)
		rawName, rawRate := splitLoomyRate(raw)
		displayName, displayRate := splitLoomyRate(display)
		if rawName != displayName || rawRate != displayRate {
			t.Errorf("splitLoomyRate is not idempotent for %q: raw=(%q,%q) display=(%q,%q)",
				raw, rawName, rawRate, displayName, displayRate)
		}
	}
}

// loomyDisplayName never leaves a dangling separator (`loomy.ts:157-160`).
func TestLoomyDisplayName(t *testing.T) {
	cases := map[string]string{
		"MiniMax M3 （x4.0）":    "MiniMax M3 · x4.0",
		"Qwen 3.8 Max (x12.0)": "Qwen 3.8 Max · x12.0",
		"GLM 5.3 Flash(x0.8)":  "GLM 5.3 Flash · x0.8",
		"MiMo V2.5":            "MiMo V2.5",
	}
	for input, want := range cases {
		if got := loomyDisplayName(input); got != want {
			t.Errorf("loomyDisplayName(%q) = %q, want %q", input, got, want)
		}
	}
	if strings.HasSuffix(loomyDisplayName("MiMo V2.5"), "· ") {
		t.Fatal("a model without a multiplier must not carry a dangling separator")
	}
}

// The bundled table is 8 entries with the documented ids, display names and
// context windows (`loomy-product.ts:74-83`).
func TestFallbackCatalogueMatchesSpec(t *testing.T) {
	want := []struct {
		id      string
		display string
		context int64
	}{
		{"deepseek-v4-flash-0731", "DeepSeek V4 Flash 0731 · x3.0", 1_048_576},
		{"MiniMax-M3", "MiniMax M3 · x4.0", 1_048_576},
		{"Kimi-k2.6", "Kimi k2.6 · x6.5", 262_144},
		{"qwen-3.8-max", "Qwen 3.8 Max · x12.0", 1_000_000},
		{"GLM-5.3-Flash", "GLM 5.3 Flash · x0.8", 1_048_576},
		{"qwen3.8-flash", "qwen 3.8 flash · x0.8", 1_000_000},
		{"spark-x", "Spark X2.5 · x0.1", 1_048_576},
		{"mimo-v2.5", "MiMo V2.5 · x3.3", 1_048_576},
	}
	if len(fallbackCatalogue) != len(want) {
		t.Fatalf("catalogue has %d entries, want %d", len(fallbackCatalogue), len(want))
	}
	for index, expected := range want {
		entry := fallbackCatalogue[index]
		if entry.ID != expected.id {
			t.Errorf("entry %d id = %q, want %q", index, entry.ID, expected.id)
		}
		if got := entry.displayName(); got != expected.display {
			t.Errorf("entry %d display = %q, want %q", index, got, expected.display)
		}
		if entry.ContextLength != expected.context {
			t.Errorf("entry %d context = %d, want %d", index, entry.ContextLength, expected.context)
		}
		if entry.SupportsImage {
			t.Errorf("entry %d must under-report image support, not advertise it", index)
		}
		if !entry.SupportsThinking {
			t.Errorf("entry %d must declare thinking support", index)
		}
	}
}

// `spark-x` is the documented open discrepancy: the port keeps the remote
// 1_048_576 and notes the client-side 262144 override.
func TestSparkXKeepsRemoteContextWindow(t *testing.T) {
	for _, entry := range fallbackCatalogue {
		if entry.ID != "spark-x" {
			continue
		}
		if entry.ContextLength != 1_048_576 {
			t.Fatalf("spark-x context = %d, want the remote 1048576", entry.ContextLength)
		}
		return
	}
	t.Fatal("spark-x is missing from the fallback catalogue")
}

// The remote parser accepts a bare array and an object with `data`
// (`loomy-adapter.ts:73-99`), keeps only `type == "chat"` entries with an id, and
// never filters on input modalities (trap #16).
func TestParseLoomyRemoteModels(t *testing.T) {
	body := []byte(`[
	  {"id":"deepseek-v4-flash-0731","type":"chat","name":"DeepSeek V4 Flash 0731 （x3.0）","context_length":1048576,
	   "capabilities":{"input_modalities":["text","image"],"reasoning":true}},
	  {"id":"image-model","type":"image","name":"Image (x1.0)","context_length":4096},
	  {"id":"","type":"chat","name":"nameless"},
	  {"id":"GLM-5.3-Flash","type":"chat","name":"","context_length":0,
	   "capabilities":{"input_modalities":["text"],"reasoning":false}}
	]`)
	entries := parseLoomyRemoteModels(body)
	if len(entries) != 2 {
		t.Fatalf("kept %d entries, want 2 chat entries", len(entries))
	}
	first := entries[0]
	if first.ID != "deepseek-v4-flash-0731" || first.ContextLength != 1_048_576 || !first.SupportsImage || !first.SupportsThinking {
		t.Fatalf("first entry = %#v, want the chat entry with image+reasoning", first)
	}
	if got := first.displayName(); got != "DeepSeek V4 Flash 0731 · x3.0" {
		t.Fatalf("display name = %q, want the normalised multiplier", got)
	}
	// An empty `name` falls back to the id and an unknown context stays 0.
	second := entries[1]
	if second.Name != "GLM-5.3-Flash" {
		t.Fatalf("name = %q, want the id fallback", second.Name)
	}
	if second.ContextLength != 0 {
		t.Fatalf("context = %d, want 0 (unknown), never a fabricated value", second.ContextLength)
	}
	if second.SupportsImage {
		t.Fatal("a text-only model must not advertise image input")
	}

	// The same payload wrapped in `data`.
	wrapped := []byte(`{"code":"000000","data":` + string(body) + `}`)
	if got := parseLoomyRemoteModels(wrapped); len(got) != len(entries) {
		t.Fatalf("wrapped payload kept %d entries, want %d", len(got), len(entries))
	}
	if got := parseLoomyRemoteModels([]byte(`{"nonsense":true}`)); len(got) != 0 {
		t.Fatalf("a non-catalogue payload must yield no entries, got %d", len(got))
	}
}

// modelForAuth falls back to the bundled table instead of failing, and the
// static listings never touch the network.
func TestStaticModelListingsNeverTouchTheNetwork(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	h := testHost()

	for _, value := range []any{
		mustRegister(t, h),
		mustStatic(t, h),
		mustForAuth(t, h, sampleCredential(t)),
	} {
		models := decodeResult[map[string]any](t, value)["Models"]
		if models == nil {
			t.Fatal("every model listing must carry a models array")
		}
	}
	if len(fake.requests) != 0 {
		t.Fatalf("model listings issued %d network calls, want 0", len(fake.requests))
	}
}

// The live catalogue uses the lowercase `token` header — never `Bearer` — and is
// cached between calls.
func TestDiscoverModelsUsesTokenHeader(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `[{"id":"MiniMax-M3","type":"chat","name":"MiniMax M3 （x4.0）","context_length":1048576}]`), nil
	}
	fake.install(t)
	cfg := DefaultConfig()
	cfg.DiscoverModels = true
	withSettings(t, cfg)
	credential := sampleCredential(t)

	value, err := handleModelForAuth(testHost(), authModelPayload(t, credential))
	if err != nil {
		t.Fatalf("model.for_auth: %v", err)
	}
	response := decodeResult[pluginapi.ModelResponse](t, value)
	if len(response.Models) != 1 || response.Models[0].ID != "MiniMax-M3" {
		t.Fatalf("models = %#v, want the discovered MiniMax-M3", response.Models)
	}
	if got := response.Models[0].DisplayName; got != "MiniMax M3 · x4.0" {
		t.Fatalf("display name = %q, want the normalised multiplier", got)
	}
	if response.Models[0].Thinking != nil {
		t.Fatal("§7.6: this provider declares no thinking levels")
	}

	calls := fake.callsFor(ModelsPath)
	if len(calls) != 1 {
		t.Fatalf("issued %d /models calls, want 1", len(calls))
	}
	if got := calls[0].Headers["token"]; len(got) != 1 || got[0] != credential.Session() {
		t.Fatalf("token header = %#v, want [%s]", got, credential.Session())
	}
	if got := calls[0].Headers.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want NO bearer header on /models", got)
	}

	// A second call inside the TTL is served from the cache.
	if _, errAgain := handleModelForAuth(testHost(), authModelPayload(t, credential)); errAgain != nil {
		t.Fatalf("second model.for_auth: %v", errAgain)
	}
	if calls := fake.callsFor(ModelsPath); len(calls) != 1 {
		t.Fatalf("issued %d /models calls after caching, want 1", len(calls))
	}
}

// A failed live fetch silently falls back to the bundled table
// (`loomy-adapter.ts:174-191`) and a dead session in the envelope is not mistaken
// for a catalogue.
func TestDiscoverModelsFallsBackOnFailure(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"100002","desc":"缺少 token"}`), nil
	}
	fake.install(t)
	cfg := DefaultConfig()
	cfg.DiscoverModels = true
	withSettings(t, cfg)

	value, err := handleModelForAuth(testHost(), authModelPayload(t, sampleCredential(t)))
	if err != nil {
		t.Fatalf("model.for_auth must not fail on a remote error: %v", err)
	}
	response := decodeResult[pluginapi.ModelResponse](t, value)
	if len(response.Models) != len(fallbackCatalogue) {
		t.Fatalf("models = %d, want the %d bundled entries", len(response.Models), len(fallbackCatalogue))
	}
}
