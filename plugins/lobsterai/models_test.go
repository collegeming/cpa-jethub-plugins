package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseThinkingConfig(t *testing.T) {
	validOptions := []any{
		map[string]any{"level": "off", "openclawLevel": "off"},
		map[string]any{"level": "low", "openclawLevel": "low"},
		map[string]any{"level": "max", "openclawLevel": "xhigh"},
	}

	tests := []struct {
		name        string
		value       any
		wantOK      bool
		wantDefault string
		wantWire    map[string]string
	}{
		{
			name:        "real shape with max mapped to xhigh",
			value:       map[string]any{"options": validOptions, "defaultLevel": "max"},
			wantOK:      true,
			wantDefault: "max",
			wantWire:    map[string]string{"off": "off", "low": "low", "max": "xhigh"},
		},
		{
			name:   "not an object",
			value:  []any{1},
			wantOK: false,
		},
		{
			name:   "missing options",
			value:  map[string]any{"defaultLevel": "low"},
			wantOK: false,
		},
		{
			name:   "empty options",
			value:  map[string]any{"options": []any{}, "defaultLevel": "low"},
			wantOK: false,
		},
		{
			name:   "unknown product level",
			value:  map[string]any{"options": []any{map[string]any{"level": "turbo", "openclawLevel": "low"}}, "defaultLevel": "turbo"},
			wantOK: false,
		},
		{
			name:   "wire value max is invalid",
			value:  map[string]any{"options": []any{map[string]any{"level": "max", "openclawLevel": "max"}}, "defaultLevel": "max"},
			wantOK: false,
		},
		{
			name: "duplicate level",
			value: map[string]any{"options": []any{
				map[string]any{"level": "low", "openclawLevel": "low"},
				map[string]any{"level": "low", "openclawLevel": "medium"},
			}, "defaultLevel": "low"},
			wantOK: false,
		},
		{
			name: "duplicate wire value",
			value: map[string]any{"options": []any{
				map[string]any{"level": "low", "openclawLevel": "low"},
				map[string]any{"level": "medium", "openclawLevel": "low"},
			}, "defaultLevel": "low"},
			wantOK: false,
		},
		{
			name: "off mismatch",
			value: map[string]any{"options": []any{
				map[string]any{"level": "off", "openclawLevel": "low"},
				map[string]any{"level": "high", "openclawLevel": "high"},
			}, "defaultLevel": "off"},
			wantOK: false,
		},
		{
			name:   "defaultLevel not in options",
			value:  map[string]any{"options": validOptions, "defaultLevel": "xhigh"},
			wantOK: false,
		},
		{
			name:   "missing defaultLevel",
			value:  map[string]any{"options": validOptions},
			wantOK: false,
		},
		{
			name:   "only off means no selectable levels",
			value:  map[string]any{"options": []any{map[string]any{"level": "off", "openclawLevel": "off"}}, "defaultLevel": "off"},
			wantOK: false,
		},
		{
			name:   "non-object entry",
			value:  map[string]any{"options": []any{"low"}, "defaultLevel": "low"},
			wantOK: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseThinkingConfig(test.value)
			if (got != nil) != test.wantOK {
				t.Fatalf("parseThinkingConfig = %+v, want ok=%v", got, test.wantOK)
			}
			if !test.wantOK {
				return
			}
			if got.DefaultLevel != test.wantDefault {
				t.Fatalf("DefaultLevel = %q, want %q", got.DefaultLevel, test.wantDefault)
			}
			for _, option := range got.Options {
				if want, present := test.wantWire[option.Level]; present && option.OpenclawLevel != want {
					t.Fatalf("level %s -> wire %s, want %s", option.Level, option.OpenclawLevel, want)
				}
			}
		})
	}
}

func TestReadModelArray(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		count int
	}{
		{name: "single layer", body: `{"code":0,"message":"success","data":[{"modelId":"a"},{"modelId":"b"}]}`, count: 2},
		{name: "double layer", body: `{"code":0,"msg":"OK","data":{"data":[{"modelId":"a"}]}}`, count: 1},
		{name: "business failure", body: `{"code":500,"msg":"boom","data":[{"modelId":"a"}]}`, count: 0},
		{name: "missing code", body: `{"msg":"OK","data":[{"modelId":"a"}]}`, count: 0},
		{name: "object data without nested array", body: `{"code":0,"data":{"items":[]}}`, count: 0},
		{name: "null data", body: `{"code":0,"data":null}`, count: 0},
		{name: "not an object", body: `[1,2]`, count: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var decoded any
			if errUnmarshal := decodeJSON([]byte(test.body), &decoded); errUnmarshal != nil {
				t.Fatalf("decode: %v", errUnmarshal)
			}
			if got := len(readModelArray(decoded)); got != test.count {
				t.Fatalf("readModelArray length = %d, want %d", got, test.count)
			}
		})
	}
}

func TestParseModelsConsumesRemoteParameters(t *testing.T) {
	body := `{"code":0,"message":"success","data":[
		{"modelId":"kimi-k3","modelName":"Kimi K3","contextWindow":1000000,"maxTokens":262144,
		 "supportsImage":true,"supportsThinking":true,"costMultiplier":0.05,
		 "requestCapabilities":["lobsterai-options-v1"],
		 "description":"远端描述",
		 "thinkingConfig":{"options":[
			{"level":"off","openclawLevel":"off"},
			{"level":"max","openclawLevel":"xhigh"}],"defaultLevel":"max"}},
		{"modelId":"glm-5","modelName":"GLM-5","series":"x"},
		{"modelName":"no id"},
		{"modelId":"","modelName":"empty id"},
		"not an object"
	]}`
	models := parseModels(decodeAny(t, body))
	if len(models) != 2 {
		t.Fatalf("parseModels returned %d models, want 2: %+v", len(models), models)
	}

	rich := models[0]
	if rich.ID != "kimi-k3" || rich.Name != "Kimi K3" {
		t.Fatalf("rich model id/name = %q/%q", rich.ID, rich.Name)
	}
	if rich.ContextWindow == nil || *rich.ContextWindow != 1000000 {
		t.Fatalf("ContextWindow = %v", rich.ContextWindow)
	}
	if rich.MaxTokens == nil || *rich.MaxTokens != 262144 {
		t.Fatalf("MaxTokens = %v", rich.MaxTokens)
	}
	if rich.SupportsImage == nil || !*rich.SupportsImage {
		t.Fatalf("SupportsImage = %v", rich.SupportsImage)
	}
	if rich.CostMultiplier == nil || *rich.CostMultiplier != 0.05 {
		t.Fatalf("CostMultiplier = %v", rich.CostMultiplier)
	}
	if len(rich.RequestCapabilities) != 1 || rich.RequestCapabilities[0] != "lobsterai-options-v1" {
		t.Fatalf("RequestCapabilities = %v", rich.RequestCapabilities)
	}
	if rich.Description != "远端描述" {
		t.Fatalf("Description = %q", rich.Description)
	}
	if rich.Thinking == nil || rich.Thinking.DefaultLevel != "max" {
		t.Fatalf("Thinking = %+v", rich.Thinking)
	}

	// Optional fields that the server did not declare must stay nil: "the
	// server said no" and "the server said nothing" are different states.
	sparse := models[1]
	if sparse.ContextWindow != nil || sparse.MaxTokens != nil || sparse.SupportsImage != nil ||
		sparse.Thinking != nil || sparse.CostMultiplier != nil {
		t.Fatalf("sparse model invented values: %+v", sparse)
	}
	if sparse.Name != "GLM-5" {
		t.Fatalf("sparse name = %q", sparse.Name)
	}
}

// decodeAny decodes a JSON document into generic Go values.
func decodeAny(t *testing.T, body string) any {
	t.Helper()
	var decoded any
	if errUnmarshal := decodeJSON([]byte(body), &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	return decoded
}

func TestCostMultiplierZeroMeansFree(t *testing.T) {
	models := parseModels(decodeAny(t, `{"code":0,"data":[{"modelId":"free-model","costMultiplier":0}]}`))
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].CostMultiplier == nil {
		t.Fatal("a declared costMultiplier of 0 must be kept (it means free)")
	}
	if got := displayNameFor(models[0]); got != "free-model · 免费" {
		t.Fatalf("displayNameFor = %q, want the free marker", got)
	}
}

func TestDisplayNameFor(t *testing.T) {
	absent := remoteModel{Name: "m"}
	if got := displayNameFor(absent); got != "m" {
		t.Fatalf("displayNameFor(absent) = %q", got)
	}
	cheap := remoteModel{Name: "m", CostMultiplier: floatPtr(0.05)}
	if got := displayNameFor(cheap); got != "m · x0.05" {
		t.Fatalf("displayNameFor(0.05) = %q", got)
	}
	expensive := remoteModel{Name: "m", CostMultiplier: floatPtr(20)}
	if got := displayNameFor(expensive); got != "m · x20" {
		t.Fatalf("displayNameFor(20) = %q", got)
	}
}

func floatPtr(value float64) *float64 { return &value }

func TestThinkingLevelsAndDisplayNames(t *testing.T) {
	model := remoteModel{
		ID:   "m",
		Name: "m",
		Thinking: &thinkingConfig{
			Options: []thinkingOption{
				{Level: "off", OpenclawLevel: "off"},
				{Level: "high", OpenclawLevel: "high"},
				{Level: "max", OpenclawLevel: "xhigh"},
			},
			DefaultLevel: "max",
		},
	}
	levels, zeroAllowed := thinkingLevelsFor(model)
	if len(levels) != 3 || levels[0] != "off" || levels[2] != "xhigh" {
		t.Fatalf("wire levels = %v", levels)
	}
	if !zeroAllowed {
		t.Fatal("an off option must allow disabling reasoning")
	}
	if !containsString(levels, "xhigh") || containsString(levels, "max") {
		t.Fatal("the wire level list must not contain max")
	}
	// The display name MUST use the product-side level, including Max.
	names := thinkingDisplayNames(model)
	if !strings.Contains(names, "Max(默认)") || !strings.Contains(names, "High") {
		t.Fatalf("display names = %q", names)
	}
}

func TestMapReasoningEffort(t *testing.T) {
	withConfig := remoteModel{
		Thinking: &thinkingConfig{
			Options: []thinkingOption{
				{Level: "low", OpenclawLevel: "low"},
				{Level: "max", OpenclawLevel: "xhigh"},
			},
			DefaultLevel: "max",
		},
	}
	tests := []struct {
		name      string
		model     remoteModel
		requested string
		want      string
	}{
		{name: "product max maps to wire xhigh", model: withConfig, requested: "max", want: "xhigh"},
		{name: "case insensitive", model: withConfig, requested: "MAX", want: "xhigh"},
		{name: "wire value passes through", model: withConfig, requested: "xhigh", want: "xhigh"},
		{name: "low stays low", model: withConfig, requested: "low", want: "low"},
		{name: "unknown passes through for upstream to judge", model: withConfig, requested: "turbo", want: "turbo"},
		{name: "no thinking config passes through", model: remoteModel{}, requested: "max", want: "max"},
		{name: "blank drops", model: withConfig, requested: "  ", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mapReasoningEffort(test.model, test.requested); got != test.want {
				t.Fatalf("mapReasoningEffort(%q) = %q, want %q", test.requested, got, test.want)
			}
		})
	}
}

func TestModelInfoForConsumesParameters(t *testing.T) {
	model := remoteModel{
		ID:             "kimi-k3",
		Name:           "Kimi K3",
		ContextWindow:  int64Ptr(1000000),
		MaxTokens:      int64Ptr(262144),
		SupportsImage:  boolPtr(true),
		CostMultiplier: floatPtr(0.05),
		Thinking: &thinkingConfig{
			Options: []thinkingOption{
				{Level: "off", OpenclawLevel: "off"},
				{Level: "max", OpenclawLevel: "xhigh"},
			},
			DefaultLevel: "max",
		},
	}
	info := modelInfoFor(model)

	if info.ID != "kimi-k3" || info.Name != "kimi-k3" {
		t.Fatalf("id/name = %q/%q", info.ID, info.Name)
	}
	if info.DisplayName != "Kimi K3 · x0.05" {
		t.Fatalf("DisplayName = %q (the multiplier must reach the selector name)", info.DisplayName)
	}
	if info.ContextLength != 1000000 || info.InputTokenLimit != 1000000 {
		t.Fatalf("context = %d/%d", info.ContextLength, info.InputTokenLimit)
	}
	if info.MaxCompletionTokens != 262144 || info.OutputTokenLimit != 262144 {
		t.Fatalf("output limits = %d/%d", info.MaxCompletionTokens, info.OutputTokenLimit)
	}
	if info.SupportedInputModalities[0] != "text" || len(info.SupportedInputModalities) != 2 {
		t.Fatalf("input modalities = %v", info.SupportedInputModalities)
	}
	if info.Thinking == nil {
		t.Fatal("Thinking must be advertised when the model declares a config")
	}
	// Wire values are what CPA sends, so the level list must be the openclaw
	// values, not the product-side names.
	if !containsString(info.Thinking.Levels, "xhigh") || containsString(info.Thinking.Levels, "max") {
		t.Fatalf("Thinking.Levels = %v, want wire values", info.Thinking.Levels)
	}
	if !info.Thinking.ZeroAllowed {
		t.Fatal("ZeroAllowed must be true when the model offers off")
	}
	// The product-side name is still surfaced (in the description).
	if !strings.Contains(info.Description, "Max") {
		t.Fatalf("Description = %q, want the product-side level names", info.Description)
	}
}

func boolPtr(value bool) *bool { return &value }

func TestModelInfoForConservativeDefaults(t *testing.T) {
	info := modelInfoFor(remoteModel{ID: "plain", Name: "plain"})
	if len(info.SupportedInputModalities) != 1 || info.SupportedInputModalities[0] != "text" {
		t.Fatalf("an undeclared image capability must stay text-only: %v", info.SupportedInputModalities)
	}
	if info.Thinking != nil {
		t.Fatalf("no thinkingConfig must mean no advertised reasoning: %+v", info.Thinking)
	}
	if info.ContextLength != 0 || info.MaxCompletionTokens != 0 {
		t.Fatalf("no invented limits: context=%d max=%d", info.ContextLength, info.MaxCompletionTokens)
	}
}

func TestFallbackCatalog(t *testing.T) {
	if len(fallbackModels) != 19 {
		t.Fatalf("fallback catalog has %d models, want 19", len(fallbackModels))
	}
	if fallbackModels[0].ID != "deepseek-v4-flash" || fallbackModels[len(fallbackModels)-1].ID != "glm-5" {
		t.Fatalf("fallback order drifted: first=%s last=%s", fallbackModels[0].ID, fallbackModels[len(fallbackModels)-1].ID)
	}
	infos := staticModelInfos()
	if len(infos) != len(fallbackModels) {
		t.Fatalf("staticModelInfos length = %d", len(infos))
	}
	// The fallback table carries no multiplier and must not invent one.
	if infos[0].DisplayName != "deepseek-v4-flash" {
		t.Fatalf("fallback DisplayName = %q", infos[0].DisplayName)
	}
	if _, ok := fallbackByID("glm-5"); !ok {
		t.Fatal("fallbackByID missed a model")
	}
	if _, ok := fallbackByID("nope"); ok {
		t.Fatal("fallbackByID invented a model")
	}
}

func TestBuildModelsQueryAndURL(t *testing.T) {
	credential := &Credential{
		AccessToken:   "token",
		RefreshToken:  "must-not-leak",
		UserID:        "yid",
		UUID:          "uuid",
		FirstKeyfrom:  "111",
		LatestKeyfrom: "222",
	}
	query := buildModelsQuery(credential, "2026.9.4")
	for _, want := range []string{"firstKeyfrom=111", "latestKeyfrom=222", "version=2026.9.4", "uuid=uuid", "userId=yid"} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q is missing %s", query, want)
		}
	}
	// A refresh token in a query string would land in server access logs.
	if strings.Contains(query, "refresh") || strings.Contains(query, "must-not-leak") {
		t.Fatalf("query leaked the refresh token: %q", query)
	}
	if !strings.HasPrefix(buildModelsURL(credential, "v"), APIBase+ModelsPath+"?") {
		t.Fatalf("models URL = %q", buildModelsURL(credential, "v"))
	}

	empty := buildModelsQuery(&Credential{}, "")
	if empty != "" {
		t.Fatalf("empty identity payload must produce no query, got %q", empty)
	}
	if got := buildModelsURL(&Credential{}, ""); got != APIBase+ModelsPath {
		t.Fatalf("models URL without identity = %q", got)
	}
}

func TestDiscoverModels(t *testing.T) {
	body := `{"code":0,"message":"success","data":[{"modelId":"kimi-k3","contextWindow":1000000}]}`
	fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Body: body})
	host := installFakeHost(t, fake)
	credential := &Credential{AccessToken: "a", FirstKeyfrom: "1", LatestKeyfrom: "2"}

	models := discoverModels(host, credential, DefaultConfig())
	if len(models) != 1 || models[0].ID != "kimi-k3" {
		t.Fatalf("discoverModels = %+v", models)
	}
	requests := fake.requestsFor(ModelsPath)
	if len(requests) != 1 {
		t.Fatalf("model requests = %d", len(requests))
	}
	// The capability header is required or the server hides models.
	if got := requests[0].Headers.Get("X-LobsterAI-Client-Capabilities"); got == "" {
		t.Fatal("model listing must declare client capabilities")
	}
	if got := requests[0].Headers.Get("Authorization"); got != "Bearer a" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestDiscoverModelsFailuresReturnNil(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeHost
	}{
		{name: "transport error", fake: newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Err: errFakeTransport})},
		{name: "http error", fake: newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Status: 500, Body: "boom"})},
		{name: "non json", fake: newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Body: "<html>"})},
		{name: "business failure", fake: newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Body: `{"code":500,"msg":"boom"}`})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := installFakeHost(t, test.fake)
			if models := discoverModels(host, &Credential{AccessToken: "a"}, DefaultConfig()); len(models) != 0 {
				t.Fatalf("discoverModels = %+v, want nil so the caller falls back", models)
			}
		})
	}
}

func TestModelHandlers(t *testing.T) {
	t.Run("register and static never touch the network", func(t *testing.T) {
		fake := newFakeHost()
		installFakeHost(t, fake)
		value, errRegister := handleModelRegister(nil, nil)
		if errRegister != nil {
			t.Fatalf("handleModelRegister: %v", errRegister)
		}
		registration := value.(pluginapi.ModelRegistrationResponse)
		if registration.Provider != ProviderKey || len(registration.Models) != len(fallbackModels) {
			t.Fatalf("registration = %+v", registration)
		}
		value, errStatic := handleModelStatic(nil, nil)
		if errStatic != nil {
			t.Fatalf("handleModelStatic: %v", errStatic)
		}
		if response := value.(pluginapi.ModelResponse); response.Provider != ProviderKey {
			t.Fatalf("static response = %+v", response)
		}
		if len(fake.requests) != 0 {
			t.Fatal("model.register/model.static must not touch the network")
		}
	})

	t.Run("for_auth uses discovery and caches it", func(t *testing.T) {
		body := `{"code":0,"message":"success","data":[{"modelId":"kimi-k3"}]}`
		fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Body: body})
		host := installFakeHost(t, fake)
		request := mustJSON(t, pluginapi.AuthModelRequest{
			StorageJSON: mustJSON(t, &Credential{AccessToken: "a", FirstKeyfrom: "1", LatestKeyfrom: "2"}),
		})
		value, errModels := handleModelForAuth(host, request)
		if errModels != nil {
			t.Fatalf("handleModelForAuth: %v", errModels)
		}
		response := value.(pluginapi.ModelResponse)
		if len(response.Models) != 1 || response.Models[0].ID != "kimi-k3" {
			t.Fatalf("models = %+v", response.Models)
		}
		if _, errAgain := handleModelForAuth(host, request); errAgain != nil {
			t.Fatalf("second handleModelForAuth: %v", errAgain)
		}
		if got := len(fake.requestsFor(ModelsPath)); got != 1 {
			t.Fatalf("model discovery ran %d times, want 1 (cached)", got)
		}
	})

	t.Run("for_auth falls back when discovery is disabled", func(t *testing.T) {
		fake := newFakeHost()
		host := installFakeHost(t, fake)
		cfg := DefaultConfig()
		cfg.DiscoverModels = false
		setSettings(cfg)
		value, errModels := handleModelForAuth(host, mustJSON(t, pluginapi.AuthModelRequest{
			StorageJSON: mustJSON(t, &Credential{AccessToken: "a"}),
		}))
		if errModels != nil {
			t.Fatalf("handleModelForAuth: %v", errModels)
		}
		if response := value.(pluginapi.ModelResponse); len(response.Models) != len(fallbackModels) {
			t.Fatalf("models = %d, want the fallback catalog", len(response.Models))
		}
		if len(fake.requests) != 0 {
			t.Fatal("discover_models=false must not touch the network")
		}
	})

	t.Run("for_auth falls back on a bad credential", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Body: `{"code":0,"data":[{"modelId":"x"}]}`})
		host := installFakeHost(t, fake)
		value, errModels := handleModelForAuth(host, mustJSON(t, pluginapi.AuthModelRequest{StorageJSON: []byte("{}")}))
		if errModels != nil {
			t.Fatalf("handleModelForAuth: %v", errModels)
		}
		if response := value.(pluginapi.ModelResponse); len(response.Models) != len(fallbackModels) {
			t.Fatalf("models = %d, want the fallback catalog", len(response.Models))
		}
	})

	t.Run("for_auth falls back when discovery returns nothing", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ModelsPath, Body: `{"code":0,"data":[]}`})
		host := installFakeHost(t, fake)
		value, errModels := handleModelForAuth(host, mustJSON(t, pluginapi.AuthModelRequest{
			StorageJSON: mustJSON(t, &Credential{AccessToken: "a"}),
		}))
		if errModels != nil {
			t.Fatalf("handleModelForAuth: %v", errModels)
		}
		if response := value.(pluginapi.ModelResponse); len(response.Models) != len(fallbackModels) {
			t.Fatalf("models = %d, want the fallback catalog", len(response.Models))
		}
	})
}

func TestModelCacheTTL(t *testing.T) {
	cfg := DefaultConfig()
	if got := modelCacheTTL(cfg); got != 2*time.Hour {
		t.Fatalf("modelCacheTTL = %v", got)
	}
	cfg.ModelCacheTTLMS = 1000
	if got := modelCacheTTL(cfg); got != time.Second {
		t.Fatalf("modelCacheTTL = %v", got)
	}
	cfg.ModelCacheTTLMS = -1
	if got := modelCacheTTL(cfg); got != 2*time.Hour {
		t.Fatalf("modelCacheTTL fallback = %v", got)
	}
}

func TestCurrentCatalogFallsBack(t *testing.T) {
	fake := newFakeHost()
	installFakeHost(t, fake)
	if got := len(currentCatalog(time.Now())); got != len(fallbackModels) {
		t.Fatalf("currentCatalog without a cache = %d models", got)
	}
	models := []remoteModel{{ID: "only", Name: "only"}}
	discoveredModels.put(models, time.Now())
	got := currentCatalog(time.Now())
	if len(got) != 1 || got[0].ID != "only" {
		t.Fatalf("currentCatalog with a cache = %+v", got)
	}
	if _, fromRemote, _ := modelByID("only", time.Now()); !fromRemote {
		t.Fatal("modelByID must find a cached model")
	}
	if model, found, fromRemote := modelByID("glm-5", time.Now()); !found || fromRemote || model.ID != "glm-5" {
		t.Fatalf("modelByID fallback = %+v,%v,%v", model, found, fromRemote)
	}
	if _, found, _ := modelByID("nope", time.Now()); found {
		t.Fatal("modelByID invented a model")
	}
	_ = json.Marshal
}
