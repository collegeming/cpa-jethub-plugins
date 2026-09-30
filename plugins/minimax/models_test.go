package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Catalogue tests: object-keyed models, model_order, max-of-options windows,
// strict effort/default parsing, and the host-facing thinking matrix.

func TestParseModelsPayloadObjectKeysOrderAndMaxContextTier(t *testing.T) {
	payload := []byte(`{
		"providers": [
			{"providerId":"other","config":{"models":{"ignored":{"name":"ignored"}}}},
			{"providerId":"minimax","config":{
				"models":{
					"MiniMax-M2.7":{"name":"M2.7","limit":{"context":200000,"output":128000},
						"context_window_options":[200000],"modalities":{"input":["text"]},
						"thinking_config":{"mode":"forced_on"}},
					"MiniMax-M3.1-Flash-Preview":{"name":"M3.1-Flash-Preview",
						"limit":{"context":512000,"output":128000},
						"context_window_options":[512000,1000000],
						"modalities":{"input":["text","image"]},
						"effort_options":["default","low","medium","high","xhigh","max"],
						"default_effort":"default","thinking_config":{"mode":"forced_on"}},
					"MiniMax-M3":{"name":"M3","limit":{"context":512000,"output":128000},
						"context_window_options":[512000,1000000],"modalities":{"input":["image","text"]},
						"thinking_config":{"mode":"switchable"}},
					"MiniMax-M2.7-highspeed":{"name":"M2.7-highspeed",
						"limit":{"context":200000,"output":128000},"context_window_options":[200000],
						"modalities":{"input":["text"]},"thinking_config":{"mode":"forced_on"}}
				},
				"model_order":["MiniMax-M3.1-Flash-Preview","MiniMax-M3","MiniMax-M2.7-highspeed","MiniMax-M2.7"]
			}}
		]
	}`)
	entries := parseModelsPayload(payload)
	if len(entries) != 4 {
		t.Fatalf("model count = %d, want 4: %+v", len(entries), entries)
	}
	wantIDs := []string{
		"MiniMax-M3.1-Flash-Preview", "MiniMax-M3", "MiniMax-M2.7-highspeed", "MiniMax-M2.7",
	}
	gotIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		gotIDs = append(gotIDs, entry.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("model order = %v, want %v", gotIDs, wantIDs)
	}

	m31 := entries[0]
	if m31.Name != "M3.1-Flash-Preview" {
		t.Errorf("M3.1 short name = %q, want M3.1-Flash-Preview (the object key must not overwrite it)", m31.Name)
	}
	// ⚠️ The endpoint says limit.context=512000 but the tier table reaches 1M.
	// The official client offers the MAX tier, and the host has a single
	// ContextLength field, so 1M is the honest number.
	if m31.ContextWindow != 1_000_000 {
		t.Errorf("M3.1 context = %d, want max(context_window_options)=1000000, not limit.context=512000", m31.ContextWindow)
	}
	if m31.MaxTokens != 128_000 {
		t.Errorf("M3.1 output = %d, want 128000", m31.MaxTokens)
	}
	if !m31.SupportsImage {
		t.Error("M3.1 should support images because modalities.input contains image")
	}
	if !reflect.DeepEqual(m31.EffortOptions, DefaultEffortLevels) {
		t.Errorf("M3.1 effort options = %v, want %v", m31.EffortOptions, DefaultEffortLevels)
	}
	if m31.DefaultEffort != "default" {
		t.Errorf("M3.1 default effort = %q, want default", m31.DefaultEffort)
	}
	if m31.ThinkingMode != thinkingForcedOn {
		t.Errorf("M3.1 thinking mode = %q, want forced_on", m31.ThinkingMode)
	}

	m3 := entries[1]
	if m3.ContextWindow != 1_000_000 {
		t.Errorf("M3 context = %d, want 1000000", m3.ContextWindow)
	}
	if m3.ThinkingMode != thinkingSwitchable {
		t.Errorf("M3 thinking mode = %q, want switchable", m3.ThinkingMode)
	}
	if len(m3.EffortOptions) != 0 {
		t.Errorf("M3 effort options = %v, want NONE (it is a switch, not a tier ladder)", m3.EffortOptions)
	}

	for _, entry := range entries[2:] {
		if entry.ContextWindow != 200_000 {
			t.Errorf("%s context = %d, want 200000", entry.ID, entry.ContextWindow)
		}
		if entry.SupportsImage {
			t.Errorf("%s advertises image support, want text only", entry.ID)
		}
	}
}

func TestParseModelsPayloadFailureShapes(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{"empty", ``},
		{"non-json", `oops`},
		{"array root", `[]`},
		{"missing providers", `{}`},
		{"providers not an array", `{"providers":{}}`},
		{"no minimax provider", `{"providers":[{"providerId":"other","config":{"models":{}}}]}`},
		{"missing config", `{"providers":[{"providerId":"minimax"}]}`},
		{"models not an object", `{"providers":[{"providerId":"minimax","config":{"models":[]}}]}`},
		{"empty models", `{"providers":[{"providerId":"minimax","config":{"models":{}}}]}`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if entries := parseModelsPayload([]byte(testCase.payload)); len(entries) != 0 {
				t.Errorf("parseModelsPayload(%s) = %+v, want empty", testCase.payload, entries)
			}
		})
	}
}

func TestNormaliseModelFallsBackToLimitContextAndFiltersDefaults(t *testing.T) {
	tests := []struct {
		name       string
		raw        any
		wantOK     bool
		wantWindow int64
		wantOutput int64
		wantImage  bool
		wantEffort []string
		wantDef    string
		wantMode   string
	}{
		{
			name: "limit.context is used only when no options exist",
			raw: map[string]any{
				"name": "M", "limit": map[string]any{"context": float64(512000), "output": float64(128000)},
				"modalities": map[string]any{"input": []any{"text"}},
			},
			wantOK: true, wantWindow: 512000, wantOutput: 128000,
		},
		{
			name: "the largest valid option wins and bad entries are skipped",
			raw: map[string]any{
				"name": "M", "limit": map[string]any{"context": float64(512000)},
				"context_window_options": []any{float64(0), float64(512000), "bad", float64(1000000)},
			},
			wantOK: true, wantWindow: 1000000,
		},
		{
			name: "effort options are de-duplicated in order and a valid default survives",
			raw: map[string]any{
				"name": "M", "effort_options": []any{"low", "high", "low", "", float64(1)},
				"default_effort": "high", "thinking_config": map[string]any{"mode": "switchable"},
			},
			wantOK: true, wantEffort: []string{"low", "high"}, wantDef: "high", wantMode: "switchable",
		},
		{
			name: "a default outside the ladder is dropped",
			raw: map[string]any{
				"name": "M", "effort_options": []any{"low", "high"}, "default_effort": "max",
			},
			wantOK: true, wantEffort: []string{"low", "high"}, wantDef: "",
		},
		{
			name: "image is true when modalities.input contains image",
			raw: map[string]any{
				"name": "M", "modalities": map[string]any{"input": []any{"text", "image"}},
			},
			wantOK: true, wantImage: true,
		},
		{name: "non-object rejected", raw: []any{}, wantOK: false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			entry, ok := normaliseModel(testCase.raw)
			if ok != testCase.wantOK {
				t.Fatalf("normaliseModel ok = %v, want %v", ok, testCase.wantOK)
			}
			if !ok {
				return
			}
			if entry.ContextWindow != testCase.wantWindow {
				t.Errorf("context = %d, want %d", entry.ContextWindow, testCase.wantWindow)
			}
			if entry.MaxTokens != testCase.wantOutput {
				t.Errorf("output = %d, want %d", entry.MaxTokens, testCase.wantOutput)
			}
			if entry.SupportsImage != testCase.wantImage {
				t.Errorf("image = %v, want %v", entry.SupportsImage, testCase.wantImage)
			}
			if !reflect.DeepEqual(entry.EffortOptions, testCase.wantEffort) {
				t.Errorf("efforts = %v, want %v", entry.EffortOptions, testCase.wantEffort)
			}
			if entry.DefaultEffort != testCase.wantDef {
				t.Errorf("default = %q, want %q", entry.DefaultEffort, testCase.wantDef)
			}
			if entry.ThinkingMode != testCase.wantMode {
				t.Errorf("mode = %q, want %q", entry.ThinkingMode, testCase.wantMode)
			}
		})
	}
}

func TestThinkingMatrix(t *testing.T) {
	tests := []struct {
		name       string
		entry      ModelCatalogEntry
		wantLevels []string
		wantZero   bool
		wantBlock  bool
		wantAdapt  bool
	}{
		{
			name: "M3.1 forced-on with six tiers, never zero",
			entry: ModelCatalogEntry{
				ID: "MiniMax-M3.1-Flash-Preview", Name: "M3.1-Flash-Preview",
				EffortOptions: DefaultEffortLevels, ThinkingMode: thinkingForcedOn,
			},
			wantLevels: DefaultEffortLevels, wantBlock: true, wantAdapt: true,
		},
		{
			name: "M3 is a switch: on then none, and zero is honest",
			entry: ModelCatalogEntry{
				ID: "MiniMax-M3", Name: "M3", ThinkingMode: thinkingSwitchable,
			},
			wantLevels: []string{"on", "none"}, wantZero: true, wantBlock: true,
		},
		{
			name: "M2.7 forced-on gets no fake controls",
			entry: ModelCatalogEntry{
				ID: "MiniMax-M2.7", Name: "M2.7", ThinkingMode: thinkingForcedOn,
			},
			wantLevels: nil, wantBlock: false,
		},
		{
			name: "M2.7-highspeed forced-on gets no fake controls",
			entry: ModelCatalogEntry{
				ID: "MiniMax-M2.7-highspeed", Name: "M2.7-highspeed", ThinkingMode: thinkingForcedOn,
			},
			wantLevels: nil, wantBlock: false,
		},
		{
			// ⚠️ THE prefix fact: MiniMax-M3.1 must match, MiniMax-M3 must not.
			name: "future M3.1 names are adaptive-only by prefix",
			entry: ModelCatalogEntry{
				ID: "MiniMax-M3.1", Name: "M3.1", ThinkingMode: thinkingForcedOn,
			},
			wantLevels: nil, wantBlock: false, wantAdapt: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			levels := reasoningEffortsFor(testCase.entry)
			if !reflect.DeepEqual(levels, testCase.wantLevels) {
				t.Errorf("levels = %v, want %v", levels, testCase.wantLevels)
			}
			info := modelInfoFor(testCase.entry, time.Unix(1, 0))
			if (info.Thinking != nil) != testCase.wantBlock {
				t.Fatalf("Thinking present = %v, want %v: %#v", info.Thinking != nil, testCase.wantBlock, info.Thinking)
			}
			if info.Thinking != nil {
				if !reflect.DeepEqual(info.Thinking.Levels, testCase.wantLevels) {
					t.Errorf("host levels = %v, want %v", info.Thinking.Levels, testCase.wantLevels)
				}
				if info.Thinking.ZeroAllowed != testCase.wantZero {
					t.Errorf("ZeroAllowed = %v, want %v", info.Thinking.ZeroAllowed, testCase.wantZero)
				}
			}
			if got := requiresAdaptiveThinking(testCase.entry.ID); got != testCase.wantAdapt {
				t.Errorf("requiresAdaptiveThinking(%q) = %v, want %v", testCase.entry.ID, got, testCase.wantAdapt)
			}
		})
	}
}

func TestAdaptivePrefixDoesNotMatchM3(t *testing.T) {
	if !requiresAdaptiveThinking("MiniMax-M3.1-Flash-Preview") {
		t.Error("M3.1-Flash-Preview must be adaptive-only")
	}
	if !requiresAdaptiveThinking("MiniMax-M3.1") {
		t.Error("future M3.1 must be covered by the MiniMax-M3.1 prefix")
	}
	if requiresAdaptiveThinking("MiniMax-M3") {
		t.Error("MiniMax-M3 must NOT match the MiniMax-M3.1 prefix; it accepts disabled")
	}
	if requiresAdaptiveThinking("MiniMax-M2.7") {
		t.Error("MiniMax-M2.7 must not be adaptive-only; the server thinks without a field")
	}
}

func TestModelInfoUsesMaxTierAndImageModality(t *testing.T) {
	entry := ModelCatalogEntry{
		ID: "MiniMax-M3.1-Flash-Preview", Name: "M3.1-Flash-Preview",
		ContextWindow: 1_000_000, MaxTokens: 128_000, SupportsImage: true,
		EffortOptions: DefaultEffortLevels, ThinkingMode: thinkingForcedOn,
	}
	info := modelInfoFor(entry, time.Unix(100, 0))
	if info.ContextLength != 1_000_000 || info.InputTokenLimit != 1_000_000 {
		t.Errorf("context metadata = (%d, %d), want (1000000, 1000000)", info.ContextLength, info.InputTokenLimit)
	}
	if info.MaxCompletionTokens != 128_000 || info.OutputTokenLimit != 128_000 {
		t.Errorf("output metadata = (%d, %d), want (128000, 128000)", info.MaxCompletionTokens, info.OutputTokenLimit)
	}
	if !reflect.DeepEqual(info.SupportedInputModalities, []string{"text", "image"}) {
		t.Errorf("input modalities = %v, want [text image]", info.SupportedInputModalities)
	}
	if !reflect.DeepEqual(info.SupportedGenerationMethods, []string{"chat.completions"}) {
		t.Errorf("generation methods = %v, want [chat.completions]", info.SupportedGenerationMethods)
	}
}

// TestCatalogRequestCarriesTheRequiredQuery locks the remote endpoint's two
// query parameters. Without them the endpoint does not select the China
// production snapshot the model ids above came from.
func TestCatalogRequestCarriesTheRequiredQuery(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"providers":[{"providerId":"minimax","config":{"models":{
			"MiniMax-M3":{"name":"M3","context_window_options":[1000000],"limit":{"output":128000}}
		}}}]}`), nil
	}
	host.install(t)
	credential := &Credential{AccessToken: "mmoat_test"}
	entries := discoverModels(testHost(), credential, DefaultConfig())
	if len(entries) != 1 {
		t.Fatalf("discoverModels = %+v, want 1 entry", entries)
	}
	request := host.requests[0]
	if !strings.Contains(request.URL, "region=cn") || !strings.Contains(request.URL, "buildEnv=prod") {
		t.Errorf("catalog URL = %q, want region=cn and buildEnv=prod", request.URL)
	}
	if request.Headers.Get("Authorization") != "Bearer mmoat_test" {
		t.Errorf("Authorization = %q, want Bearer mmoat_test", request.Headers.Get("Authorization"))
	}
}

func TestRegistrationDeclaresAnthropicBothWays(t *testing.T) {
	registration := newPlugin().Registration()
	if !reflect.DeepEqual(registration.Capabilities.ExecutorInputFormats, []string{"anthropic"}) {
		t.Errorf("ExecutorInputFormats = %v, want [anthropic]", registration.Capabilities.ExecutorInputFormats)
	}
	if !reflect.DeepEqual(registration.Capabilities.ExecutorOutputFormats, []string{"anthropic"}) {
		t.Errorf("ExecutorOutputFormats = %v, want [anthropic]", registration.Capabilities.ExecutorOutputFormats)
	}
	if registration.Capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Errorf("ExecutorModelScope = %q, want oauth", registration.Capabilities.ExecutorModelScope)
	}
}

func TestFallbackCatalogueContainsAllFourExposedModels(t *testing.T) {
	// The vendor's own static table has only three, but this provider's fallback
	// deliberately adds the remote-only M3.1 preview so all required exposed ids
	// survive a catalogue outage.
	entries := fallbackModels()
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.ID)
	}
	want := []string{"MiniMax-M3.1-Flash-Preview", "MiniMax-M3", "MiniMax-M2.7-highspeed", "MiniMax-M2.7"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fallback ids = %v, want %v", got, want)
	}
}

func TestModelPayloadRoundTripJSONNumbers(t *testing.T) {
	// Ensure the generic map path is exercised with the same float64 numbers
	// encoding/json supplies (rather than hand-built ints only).
	raw := []byte(`{"name":"M","limit":{"context":512000,"output":128000},"context_window_options":[512000,1000000]}`)
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	entry, ok := normaliseModel(decoded)
	if !ok || entry.ContextWindow != 1_000_000 || entry.MaxTokens != 128_000 {
		t.Errorf("normaliseModel(JSON) = %+v, %v", entry, ok)
	}
}
