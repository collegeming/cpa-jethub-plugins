package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// batchBody builds a `batch_get_detail_param` response with two channels. The
// same config_name appears in both so the "later channel wins" rule is testable,
// and the non-chat / disabled / hidden entries exercise the hard filters.
func batchBody() []byte {
	return []byte(`{
	  "function_configs": [
	    {
	      "function": "solo_work_lite",
	      "config_info_list": [
	        {
	          "config_name": "glm-5.1",
	          "usage": "chat_completion",
	          "config_switch": true,
	          "context_window_tokens": {"dev": 200000, "max": 1000000},
	          "model_detail_list": [
	            {"model_name": "glm-5.1__dev", "max_tokens": 32000},
	            {"model_name": "glm-5.1__max", "max_tokens": 384000}
	          ],
	          "display_config": {
	            "display_name": "GLM-5.1",
	            "max_mode": true,
	            "multimodal": false,
	            "is_custom_model": false
	          },
	          "reasoning_effort_config": {"default_level": "high", "options": ["light", "high", "extra_high"], "support_thinking": true},
	          "display_contact_config": "{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0.08}}}"
	        },
	        {
	          "config_name": "should-be-dropped-usage",
	          "usage": "multimodal",
	          "config_switch": true,
	          "display_config": {"display_name": "Multimodal"}
	        },
	        {
	          "config_name": "should-be-dropped-disabled",
	          "usage": "chat_completion",
	          "config_switch": false,
	          "display_config": {"display_name": "Disabled"}
	        },
	        {
	          "config_name": "should-be-dropped-hidden",
	          "usage": "chat_completion",
	          "config_switch": true,
	          "is_invisible_to_user": true,
	          "display_config": {"display_name": "Hidden"}
	        }
	      ]
	    },
	    {
	      "function": "solo_agent_remote",
	      "config_info_list": [
	        {
	          "config_name": "glm-5.1",
	          "usage": "chat_completion",
	          "config_switch": true,
	          "context_window_tokens": {"dev": 200000, "max": 1000000},
	          "model_detail_list": [
	            {"model_name": "glm-5.1__dev", "max_tokens": 32000},
	            {"model_name": "glm-5.1__max", "max_tokens": 384000}
	          ],
	          "display_config": {"display_name": "GLM-5.1", "max_mode": true, "multimodal": true},
	          "reasoning_effort_config": {"default_level": "high", "options": ["light", "high", "extra_high"], "support_thinking": true},
	          "display_contact_config": "{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0}},\"activity_discount\":{\"enable\":true,\"data\":{\"current\":{\"discount_type\":\"limited\",\"before_consumption_rate\":0.5,\"consumption_rate\":0},\"limited\":{\"end_at\":4102444800}}}}"
	        },
	        {
	          "config_name": "image-only",
	          "usage": "chat_completion",
	          "config_switch": true,
	          "context_window_tokens": {"dev": 128000},
	          "display_config": {"display_name": "Image Only", "multimodal": true}
	        }
	      ]
	    }
	  ]
	}`)
}

// TestParseBatchModelList covers merging order, the three hard filters and the
// metadata consumption.
func TestParseBatchModelList(t *testing.T) {
	models := ParseBatchModelList(batchBody(), 1000)
	byID := map[string]remoteModel{}
	order := []string{}
	for _, model := range models {
		byID[model.ID] = model
		order = append(order, model.ID)
	}
	if len(models) != 2 {
		// glm-5.1 (seen in both channels) and image-only; everything else is
		// filtered.
		t.Fatalf("expected 2 kept models, got %#v", order)
	}
	for _, dropped := range []string{"should-be-dropped-usage", "should-be-dropped-disabled", "should-be-dropped-hidden"} {
		if _, present := byID[dropped]; present {
			t.Fatalf("%s must be filtered out", dropped)
		}
	}

	glm := byID["glm-5.1"]
	// The later channel overwrites the earlier entry, which is how the fuller
	// configuration survives (trae.ts:1079-1082).
	if glm.Channel != "solo_agent_remote" {
		t.Fatalf("channel = %q, want the later channel", glm.Channel)
	}
	if glm.ContextWindow != 200000 {
		t.Fatalf("context window = %d, want the dev window 200000", glm.ContextWindow)
	}
	if glm.MaxContextWindow != 1000000 {
		t.Fatalf("max context window = %d, want 1000000", glm.MaxContextWindow)
	}
	if glm.MaxOutputTokens != 32000 {
		t.Fatalf("max output tokens = %d, want 32000", glm.MaxOutputTokens)
	}
	if glm.MaxModeOutputTokens != 384000 {
		t.Fatalf("max-mode output tokens = %d, want 384000", glm.MaxModeOutputTokens)
	}
	if glm.Multimodal == nil || !*glm.Multimodal {
		t.Fatal("multimodal must come from the winning entry")
	}
	if glm.Reasoning == nil || len(glm.Reasoning.Options) != 3 {
		t.Fatalf("reasoning config = %#v", glm.Reasoning)
	}
	if glm.CreditsRate == nil || *glm.CreditsRate != 0 {
		t.Fatalf("credits rate = %#v, want 0 (free is a legal rate)", glm.CreditsRate)
	}
	if glm.OriginalCreditsRate == nil || *glm.OriginalCreditsRate != 0.5 {
		t.Fatalf("original credits rate = %#v, want the active discount", glm.OriginalCreditsRate)
	}
	if glm.DiscountEndsAtSec != 4102444800 {
		t.Fatalf("discount end = %d", glm.DiscountEndsAtSec)
	}

	// The first entry keeps its insertion position even though it was overwritten.
	if order[0] != "glm-5.1" {
		t.Fatalf("order = %#v, want the first id to keep its position", order)
	}
}

// TestParseBatchModelListTolerantOfGarbage: a response that is not the expected
// shape yields no models rather than a panic.
func TestParseBatchModelListTolerantOfGarbage(t *testing.T) {
	for _, body := range []string{"", "not json", "{}", `{"function_configs": 5}`, `{"function_configs":[{"function":"x"}]}`} {
		if models := ParseBatchModelList([]byte(body), 0); len(models) != 0 {
			t.Fatalf("body %q produced %#v", body, models)
		}
	}
}

// TestParseModelListSingleChannel covers the get_detail_param (non-batch) shape.
func TestParseModelListSingleChannel(t *testing.T) {
	body := []byte(`{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM-5.2"}}]}`)
	models := ParseModelList(body, 0)
	if len(models) != 1 || models[0].ID != "glm-5.2" || models[0].Name != "GLM-5.2" {
		t.Fatalf("models = %#v", models)
	}
}

// TestConsumptionRateTraps: every documented trap of the double-encoded
// `display_contact_config` field.
func TestConsumptionRateTraps(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *float64
	}{
		{name: "plain rate", raw: `{"consumption_rate":{"enable":true,"data":{"rate":0.08}}}`, want: floatPtr(0.08)},
		{name: "zero is free, not absent", raw: `{"consumption_rate":{"enable":true,"data":{"rate":0}}}`, want: floatPtr(0)},
		{name: "disabled means no rate at all", raw: `{"consumption_rate":{"enable":false,"data":{"rate":0.08}}}`, want: nil},
		{name: "not an object", raw: `[]`, want: nil},
		{name: "not json", raw: `nope`, want: nil},
		{name: "missing data", raw: `{"consumption_rate":{"enable":true}}`, want: nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			entry := map[string]any{"display_contact_config": testCase.raw}
			got := readConsumptionRate(entry)
			if testCase.want == nil {
				if got != nil {
					t.Fatalf("rate = %v, want none", *got)
				}
				return
			}
			if got == nil || *got != *testCase.want {
				t.Fatalf("rate = %v, want %v", got, *testCase.want)
			}
		})
	}
}

// TestActivityDiscountTraps: `enable: true` alone does not mean a discount is in
// effect, and an expired window must not be shown.
func TestActivityDiscountTraps(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		nowSec   int64
		wantRate float64
		wantOK   bool
	}{
		{
			name:     "active limited discount",
			raw:      `{"activity_discount":{"enable":true,"data":{"current":{"discount_type":"limited","before_consumption_rate":0.13,"consumption_rate":0.08},"limited":{"end_at":2000}}}}`,
			nowSec:   1000,
			wantRate: 0.13,
			wantOK:   true,
		},
		{
			name:   "expired discount is not in effect",
			raw:    `{"activity_discount":{"enable":true,"data":{"current":{"discount_type":"limited","before_consumption_rate":0.13,"consumption_rate":0.08},"limited":{"end_at":500}}}}`,
			nowSec: 1000,
		},
		{
			name:   "type none means no activity",
			raw:    `{"activity_discount":{"enable":true,"data":{"current":{"discount_type":"none","before_consumption_rate":0.13,"consumption_rate":0.13}}}}`,
			nowSec: 1000,
		},
		{
			name:   "no real price drop",
			raw:    `{"activity_discount":{"enable":true,"data":{"current":{"discount_type":"subsidy","before_consumption_rate":0.08,"consumption_rate":0.08}}}}`,
			nowSec: 1000,
		},
		{
			name:   "explicitly disabled",
			raw:    `{"activity_discount":{"enable":false,"data":{"current":{"discount_type":"limited","before_consumption_rate":0.13,"consumption_rate":0.08}}}}`,
			nowSec: 1000,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			entry := map[string]any{"display_contact_config": testCase.raw}
			rate, _, ok := readActivityDiscount(entry, testCase.nowSec)
			if ok != testCase.wantOK {
				t.Fatalf("ok = %v, want %v (rate %v)", ok, testCase.wantOK, rate)
			}
			if ok && rate != testCase.wantRate {
				t.Fatalf("rate = %v, want %v", rate, testCase.wantRate)
			}
		})
	}
}

// TestDisplayNameWithRates covers the presentation rules, including that a zero
// rate renders as 免费 rather than x0.
func TestDisplayNameWithRates(t *testing.T) {
	cases := []struct {
		name  string
		model remoteModel
		want  string
	}{
		{name: "no rate", model: remoteModel{Name: "GLM-5.2"}, want: "GLM-5.2"},
		{name: "plain rate", model: remoteModel{Name: "GLM-5.2", CreditsRate: floatPtr(0.08)}, want: "GLM-5.2 · x0.08"},
		{name: "free", model: remoteModel{Name: "GLM-5.2", CreditsRate: floatPtr(0)}, want: "GLM-5.2 · 免费"},
		{
			name:  "discounted",
			model: remoteModel{Name: "Doubao", CreditsRate: floatPtr(0.08), OriginalCreditsRate: floatPtr(0.8)},
			want:  "Doubao · x0.8→x0.08",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := displayName(testCase.model); got != testCase.want {
				t.Fatalf("displayName = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestModelGating covers callability, Max mode and the per-model image answer.
func TestModelGating(t *testing.T) {
	cfg := DefaultConfig()

	callable := remoteModel{ID: "glm-5.2"}
	if !isModelCallable(callable) {
		t.Fatal("a model with no flags is callable")
	}
	if isModelCallable(remoteModel{ID: "x", IsCustomModel: boolPtr(true)}) {
		t.Fatal("is_custom_model must be rejected")
	}
	if isModelCallable(remoteModel{ID: "x", IsEnabled: boolPtr(false)}) {
		t.Fatal("config_switch=false must be rejected")
	}
	// isModelCallable deliberately ignores is_invisible_to_user: that flag is a
	// hard filter at parse time, and conflating the two would drop models that
	// are merely not shown by the official picker.
	if !isModelCallable(remoteModel{ID: "x", IsHidden: boolPtr(true)}) {
		t.Fatal("isModelCallable must not decide on is_invisible_to_user")
	}

	if maxModeFor(remoteModel{ID: "m"}, cfg) {
		t.Fatal("a model without max_mode must not get Max fields")
	}
	if !maxModeFor(remoteModel{ID: "m", MaxMode: boolPtr(true)}, cfg) {
		t.Fatal("max_mode=true with the product switch on must enable Max mode")
	}
	off := cfg
	off.MaxMode = false
	if maxModeFor(remoteModel{ID: "m", MaxMode: boolPtr(true)}, off) {
		t.Fatal("the product switch must be able to disable Max mode")
	}
	whitelisted := cfg
	whitelisted.MaxModeModels = []string{"other"}
	if maxModeFor(remoteModel{ID: "m", MaxMode: boolPtr(true)}, whitelisted) {
		t.Fatal("a whitelist that does not contain the model must exclude it")
	}
	whitelisted.MaxModeModels = []string{"*"}
	if !maxModeFor(remoteModel{ID: "m", MaxMode: boolPtr(true)}, whitelisted) {
		t.Fatal("* must whitelist every model")
	}

	if modelSupportsImage(remoteModel{ID: "m"}) {
		t.Fatal("an absent multimodal flag means unsupported")
	}
	if modelSupportsImage(remoteModel{ID: "m", Multimodal: boolPtr(false)}) {
		t.Fatal("multimodal=false means unsupported")
	}
	if !modelSupportsImage(remoteModel{ID: "m", Multimodal: boolPtr(true)}) {
		t.Fatal("multimodal=true means supported")
	}
}

// TestModelInfoForRemote checks that the two context windows are never mixed: a
// model that is not in Max mode reports the dev window.
func TestModelInfoForRemote(t *testing.T) {
	cfg := DefaultConfig()
	model := remoteModel{
		ID:                  "custom_model_1M",
		Name:                "Custom 1M",
		ContextWindow:       200000,
		MaxContextWindow:    1000000,
		MaxOutputTokens:     64000,
		MaxModeOutputTokens: 384000,
		MaxMode:             boolPtr(true),
		Multimodal:          boolPtr(true),
		Reasoning:           &reasoningConfig{Options: []string{"light", "high"}},
	}
	info := modelInfoForRemote(model, cfg)
	if info.ContextLength != 1000000 || info.MaxCompletionTokens != 384000 {
		t.Fatalf("Max-mode info = %#v", info)
	}
	if len(info.SupportedInputModalities) != 2 || info.SupportedInputModalities[1] != "image" {
		t.Fatalf("input modalities = %#v", info.SupportedInputModalities)
	}
	if info.Thinking == nil || len(info.Thinking.Levels) != 2 {
		t.Fatalf("thinking levels = %#v", info.Thinking)
	}
	if !strings.Contains(info.DisplayName, "Custom 1M") {
		t.Fatalf("display name = %q", info.DisplayName)
	}

	// Without the remote max_mode flag the dev window must be reported.
	model.MaxMode = boolPtr(false)
	info = modelInfoForRemote(model, cfg)
	if info.ContextLength != 200000 || info.MaxCompletionTokens != 64000 {
		t.Fatalf("non-Max info = %#v", info)
	}
	// support_thinking=false must suppress the reasoning control entirely.
	model.Reasoning = &reasoningConfig{Options: []string{"high"}, SupportThinking: boolPtr(false)}
	if got := modelInfoForRemote(model, cfg); got.Thinking != nil {
		t.Fatalf("support_thinking=false must declare no levels, got %#v", got.Thinking)
	}
}

// TestStaticModelInfosDropsHidden: the fallback list must not leak upstream
// internal entries.
func TestStaticModelInfosDropsHidden(t *testing.T) {
	infos := staticModelInfos(DefaultConfig())
	for _, info := range infos {
		switch info.ID {
		case "summary", "browser_use_subagent", "explore_sub_agent_v2", "explore_sub_agent_v13":
			t.Fatalf("hidden fallback model %q must not be advertised", info.ID)
		}
	}
	if len(infos) == 0 {
		t.Fatal("the fallback catalog must not be empty")
	}
	if infos[0].OwnedBy != ProviderKey {
		t.Fatalf("owned by = %q", infos[0].OwnedBy)
	}
}

// TestCatalogFunctionsAddsExtraChannels: the 22-function baseline mirrors the
// real IDE, and configured channels extend it without duplicates.
func TestCatalogFunctionsAddsExtraChannels(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Channels = append(cfg.Channels, "experimental_channel", "solo_agent")
	functions := catalogFunctions(cfg)
	counts := map[string]int{}
	for _, function := range functions {
		counts[function]++
	}
	if counts["experimental_channel"] != 1 {
		t.Fatalf("configured channel missing: %#v", counts)
	}
	if counts["solo_agent"] != 1 {
		t.Fatalf("duplicate channel inserted: %#v", counts)
	}
	if counts["solo_work_lite"] != 1 {
		t.Fatalf("baseline channel missing: %#v", counts)
	}
}

// TestChannelForPrefersTheModelsOwnChannel is the channel-routing rule: sending
// the wrong channel fails inside the stream with 4001.
func TestChannelForPrefersTheModelsOwnChannel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultChannel = "solo_work_lite"
	if got := channelFor(remoteModel{Channel: "solo_agent_remote"}, true, cfg); got != "solo_agent_remote" {
		t.Fatalf("channel = %q, want the model's own channel", got)
	}
	if got := channelFor(remoteModel{}, true, cfg); got != "solo_work_lite" {
		t.Fatalf("channel = %q, want the default", got)
	}
	fallback := Config{Channels: []string{"a"}}
	if got := channelFor(remoteModel{}, false, fallback); got != DefaultFunction {
		t.Fatalf("channel = %q, want the built-in fallback", got)
	}
}

// TestConfigNameFor strips the internal suffixes.
func TestConfigNameFor(t *testing.T) {
	cases := map[string]string{
		"glm-5.2":              "glm-5.2",
		"custom_model_1M__max": "custom_model_1M",
		"glm-5.1__dev":         "glm-5.1",
	}
	for input, want := range cases {
		if got := configNameFor(input); got != want {
			t.Fatalf("configNameFor(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestCatalogCacheRoundTrip exercises the cache without any network call.
func TestCatalogCacheRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	credential := &Credential{AccessToken: "token", UID: "uid-cache-test"}
	if _, ok := cachedCatalog(cfg, credential); ok {
		t.Fatal("nothing should be cached yet")
	}
	storeCatalog(cfg, credential, []remoteModel{{ID: "glm-5.2", Name: "GLM-5.2"}})
	entry, ok := cachedCatalog(cfg, credential)
	if !ok || len(entry.models) != 1 {
		t.Fatalf("cached entry = %#v ok=%v", entry, ok)
	}
	if _, ok := entry.byID["glm-5.2"]; !ok {
		t.Fatalf("index = %#v", entry.byID)
	}
	invalidateCatalog(cfg, credential)
	if _, ok := cachedCatalog(cfg, credential); ok {
		t.Fatal("invalidateCatalog must drop the entry")
	}
}

// TestHandleModelRegisterAndStaticAreOfflineAndIdentical: both report the static
// catalog and neither touches the network (nil host).
func TestHandleModelRegisterAndStaticAreOfflineAndIdentical(t *testing.T) {
	registration, errRegister := handleModelRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("model.register: %v", errRegister)
	}
	registered, ok := registration.(pluginapi.ModelRegistrationResponse)
	if !ok {
		t.Fatalf("unexpected type %T", registration)
	}
	if registered.Provider != ProviderKey || len(registered.Models) == 0 {
		t.Fatalf("registration = %#v", registered)
	}

	static, errStatic := handleModelStatic(nil, nil)
	if errStatic != nil {
		t.Fatalf("model.static: %v", errStatic)
	}
	staticResponse, ok := static.(pluginapi.ModelResponse)
	if !ok {
		t.Fatalf("unexpected type %T", static)
	}
	if staticResponse.Provider != ProviderKey || len(staticResponse.Models) == 0 {
		t.Fatalf("static response = %#v", staticResponse)
	}
}

// TestHandleModelForAuthWithoutCredentialReturnsEmpty locks the documented
// behaviour: no credential yields an empty list, never an error.
func TestHandleModelForAuthWithoutCredentialReturnsEmpty(t *testing.T) {
	raw, _ := json.Marshal(pluginapi.AuthModelRequest{StorageJSON: []byte(`{"access_token":""}`)})
	value, errHandler := handleModelForAuth(nil, raw)
	if errHandler != nil {
		t.Fatalf("model.for_auth: %v", errHandler)
	}
	response, ok := value.(pluginapi.ModelResponse)
	if !ok {
		t.Fatalf("unexpected type %T", value)
	}
	if len(response.Models) != 0 {
		t.Fatalf("models = %#v, want an empty list", response.Models)
	}
}

// TestHandleModelForAuthWithDiscoveryOffReportsStatic: with discovery disabled no
// network call happens, so the static catalog is reported even with a nil host.
func TestHandleModelForAuthWithDiscoveryOffReportsStatic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DiscoverModels = false
	setSettings(cfg)
	defer setSettings(DefaultConfig())

	raw, _ := json.Marshal(pluginapi.AuthModelRequest{StorageJSON: []byte(`{"access_token":"t","uid":"u"}`)})
	value, errHandler := handleModelForAuth(nil, raw)
	if errHandler != nil {
		t.Fatalf("model.for_auth: %v", errHandler)
	}
	response := value.(pluginapi.ModelResponse)
	if len(response.Models) == 0 {
		t.Fatal("discovery disabled must still report the static catalog")
	}
}

// floatPtr and boolPtr build pointers for the flag-heavy fixtures.
func floatPtr(value float64) *float64 { return &value }

func boolPtr(value bool) *bool { return &value }
