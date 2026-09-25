package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNormalizeRate(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"prefix form", "x0.29", "x0.29"},
		{"suffix form", "0.50x", "x0.50"},
		{"unit suffix", "x0.03 credits", "x0.03"},
		{"uppercase", "X1.62", "x1.62"},
		{"empty", "", ""},
		{"non string", float64(3), ""},
		{"unparseable", "free", ""},
		{"zero placeholder", "0x", "x0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeRate(test.in); got != test.want {
				t.Fatalf("normalizeRate(%v) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestFormatCreditsRate(t *testing.T) {
	tests := []struct {
		rate       string
		discounted string
		want       string
	}{
		{"x0.29", "x0.17", "x0.29→x0.17"},
		{"x0.29", "", "x0.29"},
		{"", "免费", "免费"},
		{"", "", ""},
	}
	for _, test := range tests {
		if got := formatCreditsRate(test.rate, test.discounted); got != test.want {
			t.Errorf("formatCreditsRate(%q, %q) = %q, want %q", test.rate, test.discounted, got, test.want)
		}
	}
}

func TestParseHHMM(t *testing.T) {
	tests := []struct {
		in     any
		want   int
		wantOK bool
	}{
		{"23:00", 23 * 60, true},
		{"7:50", 7*60 + 50, true},
		{"07:50", 7*60 + 50, true},
		{"24:00", 0, false},
		{"12:60", 0, false},
		{"noon", 0, false},
		{float64(12), 0, false},
		{"12", 0, false},
	}
	for _, test := range tests {
		got, ok := parseHHMM(test.in)
		if ok != test.wantOK || (ok && got != test.want) {
			t.Errorf("parseHHMM(%v) = (%d, %v), want (%d, %v)", test.in, got, ok, test.want, test.wantOK)
		}
	}
}

func shanghai(t *testing.T, hour, minute int) time.Time {
	t.Helper()
	location, errLoad := time.LoadLocation("Asia/Shanghai")
	if errLoad != nil {
		t.Fatalf("load Asia/Shanghai: %v", errLoad)
	}
	return time.Date(2026, 9, 14, hour, minute, 0, 0, location)
}

func TestPromotionActiveNow(t *testing.T) {
	// The measured glm-5.2 pair: night 23:00–7:50 with the discount, day
	// 7:50–23:00 with only a badge (buddy.ts:557-563).
	night := map[string]any{
		"schedule": map[string]any{
			"daily":    []any{map[string]any{"start": "23:00", "end": "7:50"}},
			"timezone": "Asia/Shanghai",
		},
	}
	tests := []struct {
		name string
		item map[string]any
		now  time.Time
		want bool
	}{
		{"inside the cross-midnight window (late)", night, shanghai(t, 23, 30), true},
		{"inside the cross-midnight window (early)", night, shanghai(t, 3, 0), true},
		{"outside the cross-midnight window", night, shanghai(t, 12, 0), false},
		{"boundary start is inclusive", night, shanghai(t, 23, 0), true},
		{"boundary end is exclusive", night, shanghai(t, 7, 50), false},
		{"no schedule means always active", map[string]any{}, shanghai(t, 12, 0), true},
		{
			"expired validity window",
			map[string]any{"schedule": map[string]any{"validUntil": "2020-01-01T00:00:00Z"}},
			shanghai(t, 12, 0),
			false,
		},
		{
			"future validity window",
			map[string]any{"schedule": map[string]any{"validFrom": "2030-01-01T00:00:00Z"}},
			shanghai(t, 12, 0),
			false,
		},
		{
			"unknown timezone does not kill the promotion",
			map[string]any{"schedule": map[string]any{
				"daily":    []any{map[string]any{"start": "00:00", "end": "00:01"}},
				"timezone": "Not/AZone",
			}},
			shanghai(t, 12, 0),
			true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := promotionActiveNow(test.item, test.now); got != test.want {
				t.Fatalf("promotionActiveNow = %v, want %v", got, test.want)
			}
		})
	}
}

func TestParsePromotions(t *testing.T) {
	record := map[string]any{
		"modelPromotions": []any{
			map[string]any{
				"kind": "discount", "enabled": true, "priority": float64(100),
				"discount": map[string]any{"discountedCredits": "0.50x", "factor": 0.5},
				"schedule": map[string]any{
					"daily":    []any{map[string]any{"start": "23:00", "end": "7:50"}},
					"timezone": "Asia/Shanghai",
				},
				"modelIds": []any{"glm-5.2"},
			},
			map[string]any{
				"kind": "discount", "enabled": true, "priority": float64(10),
				"discount": map[string]any{"discountedCredits": "0.10x"},
				"modelIds": []any{"glm-5.2"},
			},
			map[string]any{
				"kind": "discount", "enabled": false, "priority": float64(999),
				"discount": map[string]any{"discountedCredits": "0.01x"},
				"modelIds": []any{"glm-5.2"},
			},
			map[string]any{
				"kind": "discount", "enabled": true,
				"discount": map[string]any{"discountedCredits": "0x", "factor": float64(0)},
				"modelIds": []any{"no-window-free"},
			},
			map[string]any{
				"kind": "discount", "enabled": true,
				"discount": map[string]any{"discountedCredits": "0x", "factor": float64(0)},
				"schedule": map[string]any{"daily": []any{map[string]any{"start": "23:00", "end": "7:50"}}},
				"modelIds": []any{"windowed-free"},
			},
			map[string]any{
				"kind": "discount", "enabled": true,
				"discount": map[string]any{"discountedCredits": "0x"},
				"modelIds": []any{"placeholder"},
			},
		},
	}

	night := parsePromotions(record, shanghai(t, 23, 30))
	if night["glm-5.2"] != "x0.50" {
		t.Errorf("night glm-5.2 = %q, want x0.50", night["glm-5.2"])
	}
	if night["windowed-free"] != "免费" {
		t.Errorf("windowed factor 0 = %q, want 免费", night["windowed-free"])
	}
	if _, present := night["no-window-free"]; present {
		t.Error("a windowless factor 0 must be treated as an ended placeholder")
	}
	if _, present := night["placeholder"]; present {
		t.Error("a bare 0x must be skipped")
	}

	// Outside the window the higher-priority promotion no longer applies, so
	// the always-on low-priority one wins.
	noon := parsePromotions(record, shanghai(t, 12, 0))
	if noon["glm-5.2"] != "x0.10" {
		t.Errorf("noon glm-5.2 = %q, want the low-priority x0.10", noon["glm-5.2"])
	}
}

func TestParseModelsFromConfig(t *testing.T) {
	body := map[string]any{
		"data": map[string]any{
			"agents": []any{
				map[string]any{"name": "craft", "models": []any{"glm-5.3", "auto", "nes-autocomplete", "hy4-preview-f"}},
				map[string]any{"name": "ask", "models": []any{"glm-5.2"}},
			},
			"models": []any{
				map[string]any{"id": "glm-5.3", "name": "GLM-5.3", "maxInputTokens": float64(1_000_000),
					"maxOutputTokens": float64(64_000), "supportsImages": true, "credits": "x0.29",
					"reasoning": map[string]any{"supportedEfforts": []any{"low", "high"}, "defaultEffort": "high"}},
				map[string]any{"id": "glm-5.2", "maxInputTokens": float64(1_000_000), "maxOutputTokens": float64(0)},
				map[string]any{"id": "nes-next", "maxOutputTokens": float64(4096)},
				map[string]any{"id": "codewise-rewrite", "maxOutputTokens": float64(4096)},
				map[string]any{"id": "tiny-fim", "maxOutputTokens": float64(200)},
				map[string]any{"id": "hunyuan-image-alpha", "maxOutputTokens": float64(4096), "tags": []any{"text-to-image"}},
				map[string]any{"id": "supports-extra", "maxOutputTokens": float64(4096), "supportsExtra": true},
				map[string]any{"id": "default", "maxOutputTokens": float64(4096)},
			},
			"productFeaturesConfig": map[string]any{
				"ModelTrialBanner": map[string]any{
					"banners": []any{map[string]any{"targetModelId": "hy4-preview"}},
				},
			},
		},
	}

	models := parseModelsFromConfig(body)
	byID := map[string]remoteModel{}
	for _, model := range models {
		byID[model.ID] = model
	}

	// craft models first, in order, with the non-chat ones filtered out.
	order := []string{}
	for _, model := range models {
		order = append(order, model.ID)
	}
	wantPrefix := []string{"glm-5.3", "hy4-preview-f", "glm-5.2"}
	for index, id := range wantPrefix {
		if index >= len(order) || order[index] != id {
			t.Fatalf("model order = %v, want it to start with %v", order, wantPrefix)
		}
	}

	for _, filtered := range []string{"auto", "default", "nes-next", "codewise-rewrite", "tiny-fim", "hunyuan-image-alpha", "supports-extra"} {
		if _, present := byID[filtered]; present {
			t.Errorf("%s must be filtered out of the selectable catalog", filtered)
		}
	}

	glm53 := byID["glm-5.3"]
	if glm53.ContextWindow != 1_000_000 || glm53.MaxOutputTokens != 64_000 {
		t.Errorf("glm-5.3 limits = %+v", glm53)
	}
	if glm53.CreditsRate != "x0.29" {
		t.Errorf("glm-5.3 credits = %q", glm53.CreditsRate)
	}
	if glm53.SupportsImages == nil || !*glm53.SupportsImages {
		t.Error("glm-5.3 should support images")
	}
	if len(glm53.ReasoningEfforts) != 2 || glm53.DefaultReasoningEffort != "high" {
		t.Errorf("glm-5.3 reasoning = %+v", glm53)
	}
	if !glm53.AgentReferenced {
		t.Error("glm-5.3 is referenced by craft and must be marked")
	}

	// maxOutputTokens = 0 is illegal and must stay unset rather than become 0.
	if glm52 := byID["glm-5.2"]; glm52.MaxOutputTokens != 0 || glm52.SupportsImages != nil {
		t.Errorf("glm-5.2 = %+v, want an undeclared max output and image capability", glm52)
	}

	trial := byID["hy4-preview"]
	if trial.Name == "" || !trial.AgentReferenced {
		t.Errorf("trial model = %+v, want a display name and the agent-referenced mark", trial)
	}

	if empty := parseModelsFromConfig(map[string]any{}); len(empty) != 0 {
		t.Errorf("an unusable body must parse to an empty catalog, got %v", empty)
	}
	if empty := parseModelsFromConfig(nil); len(empty) != 0 {
		t.Errorf("nil body must parse to an empty catalog, got %v", empty)
	}
}

func TestReconcileWithFallback(t *testing.T) {
	product, _ := productByConfigValue(ProductCodeBuddy)
	falseValue := false
	remote := []remoteModel{
		// Declared by the fallback table: the remote metadata wins.
		{ID: "glm-5.3", ContextWindow: 900_000, MaxOutputTokens: 12_000, SupportsImages: &falseValue, CreditsRate: "x0.33"},
		// Not declared by the fallback table and not agent-referenced: dropped.
		{ID: "internal-alias"},
		// Not declared but agent-referenced: kept (buddy-adapter.ts:687-696).
		{ID: "hy4-preview-f", Name: "Hy4 preview", AgentReferenced: true},
	}
	reconciled := reconcileWithFallback(remote, product)
	byID := map[string]remoteModel{}
	for _, model := range reconciled {
		byID[model.ID] = model
	}

	if len(reconciled) != len(product.FallbackModels)+1 {
		t.Fatalf("reconciled %d models, want %d", len(reconciled), len(product.FallbackModels)+1)
	}
	glm := byID["glm-5.3"]
	if glm.ContextWindow != 900_000 || glm.MaxOutputTokens != 12_000 {
		t.Errorf("remote metadata must win: %+v", glm)
	}
	if glm.SupportsImages == nil || *glm.SupportsImages {
		t.Errorf("a remote explicit false must survive: %+v", glm.SupportsImages)
	}
	if glm.CreditsRate != "x0.33" {
		t.Errorf("credits rate = %q", glm.CreditsRate)
	}
	if _, present := byID["internal-alias"]; present {
		t.Error("an unlisted, unreferenced model must be dropped")
	}
	if _, present := byID["hy4-preview-f"]; !present {
		t.Error("an agent-referenced model must be kept even when unlisted")
	}
	// The fallback table must be preserved in order at the front.
	if reconciled[0].ID != product.FallbackModels[0].ID {
		t.Errorf("reconciled[0] = %q, want %q", reconciled[0].ID, product.FallbackModels[0].ID)
	}
	// A fallback model missing from the remote keeps its built-in metadata.
	if hy3 := byID["hy3"]; hy3.MaxOutputTokens != 64_000 || hy3.ContextWindow != 192_000 {
		t.Errorf("hy3 fallback metadata = %+v", hy3)
	}

	// A product without a fallback table returns the remote catalog untouched.
	empty := reconcileWithFallback(remote, productConfig{})
	if len(empty) != len(remote) {
		t.Errorf("no fallback table should pass the remote catalog through, got %d", len(empty))
	}
}

func TestPositiveMaxTokens(t *testing.T) {
	tests := []struct {
		in     int64
		want   int
		wantOK bool
	}{
		{128_000, 128_000, true},
		{0, 0, false},
		{-1, 0, false},
	}
	for _, test := range tests {
		got, ok := positiveMaxTokens(test.in)
		if ok != test.wantOK || (ok && got != test.want) {
			t.Errorf("positiveMaxTokens(%d) = (%d, %v), want (%d, %v)", test.in, got, ok, test.want, test.wantOK)
		}
	}
}

func TestVariantDisambiguation(t *testing.T) {
	all := []remoteModel{
		{ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash"},
		{ID: "deepseek-v4.1-flash-sg", Name: "Deepseek-V4.1-Flash"},
		{ID: "hy3", Name: "Hy3"},
		{ID: "hy3-x", Name: "Hy3"},
		{ID: "unique", Name: "Unique"},
	}
	tests := []struct {
		id   string
		want string
	}{
		// The base id of a colliding group keeps the bare name; only the
		// variants get a marker (buddy-adapter.ts:1437-1443).
		{"deepseek-v4.1-flash", ""},
		{"deepseek-v4.1-flash-sg", "SG"},
		{"hy3", ""},
		{"hy3-x", "X"},
		{"unique", ""},
	}
	for _, test := range tests {
		for _, model := range all {
			if model.ID != test.id {
				continue
			}
			if got := variantLabelFor(model, all); got != test.want {
				t.Errorf("variantLabelFor(%s) = %q, want %q", test.id, got, test.want)
			}
		}
	}
	if got := commonPrefix([]string{"hy4-preview", "hy4-preview-f", "hy4-preview-x"}); got != "hy4-preview" {
		t.Errorf("commonPrefix = %q", got)
	}
	if got := commonPrefix(nil); got != "" {
		t.Errorf("commonPrefix(nil) = %q", got)
	}

	glm := remoteModel{ID: "glm-5.3", Name: "GLM-5.3", CreditsRate: "x0.79", DiscountedCreditsRate: "x0.50"}
	if got := displayNameForModelWithSuffix(glm, all); got != "GLM-5.3 · x0.79→x0.50" {
		t.Errorf("display name = %q", got)
	}
	if got := displayNameForModel("kimi-k2.6"); got != "Kimi K2.6" {
		t.Errorf("static display name = %q", got)
	}
	if got := displayNameForModel("brand-new-model"); got != "brand-new-model" {
		t.Errorf("unknown display name = %q, want the id", got)
	}
}

func TestModelInfoForCarriesMaxTokensAndThinking(t *testing.T) {
	product, _ := productByConfigValue(ProductCodeBuddy)
	catalog := staticCatalog(product)
	infos := modelInfosFromCatalog(product, catalog)
	if len(infos) != len(catalog) {
		t.Fatalf("infos = %d, want %d", len(infos), len(catalog))
	}
	byID := map[string]bool{}
	for _, info := range infos {
		byID[info.ID] = true
		if info.ID == "deepseek-v4.1-flash" {
			if info.MaxCompletionTokens != 128_000 {
				t.Errorf("defaultMaxTokens = %d, want the 128000 measured value", info.MaxCompletionTokens)
			}
			if info.ContextLength != 1_000_000 {
				t.Errorf("context length = %d", info.ContextLength)
			}
			if len(info.SupportedInputModalities) != 2 {
				t.Errorf("modalities = %v, want text+image", info.SupportedInputModalities)
			}
			if info.Thinking == nil || len(info.Thinking.Levels) == 0 {
				t.Errorf("thinking levels missing: %+v", info.Thinking)
			}
		}
	}
	if !byID["glm-5.1"] {
		t.Error("the static catalog must contain glm-5.1")
	}

	// A model the remote declared as text-only must be advertised as such, and
	// the glm-5.1 override must still win over a remote false.
	notImages := false
	override := remoteModel{ID: "glm-5.1", SupportsImages: &notImages}
	if got := inputModalitiesFor(override); len(got) != 2 {
		t.Errorf("glm-5.1 modalities = %v, want the image override to win", got)
	}
	plain := remoteModel{ID: "text-only", SupportsImages: &notImages}
	if got := inputModalitiesFor(plain); len(got) != 1 {
		t.Errorf("text-only modalities = %v", got)
	}
}

func TestMergeRemoteModelsKeepsPrimaryMetadata(t *testing.T) {
	primary := []remoteModel{{ID: "a", MaxOutputTokens: 100}, {ID: "b", MaxOutputTokens: 200}}
	extra := []remoteModel{{ID: "b", MaxOutputTokens: 999}, {ID: "c", MaxOutputTokens: 300}}
	merged := mergeRemoteModels(primary, extra)
	if len(merged) != 3 {
		t.Fatalf("merged = %d models, want 3", len(merged))
	}
	if merged[1].MaxOutputTokens != 200 {
		t.Errorf("primary metadata must win: %+v", merged[1])
	}
	if merged[2].ID != "c" {
		t.Errorf("extra-only models go last: %+v", merged)
	}

	withPromotions := applyPromotions(merged, map[string]string{"c": "免费"})
	if withPromotions[2].DiscountedCreditsRate != "免费" {
		t.Errorf("promotion not applied: %+v", withPromotions[2])
	}
}

func TestEnterpriseScopeIsPersonal(t *testing.T) {
	if enterpriseModelsScope != "personal" {
		t.Fatalf("enterpriseModelsScope = %q, want the literal from buddy-oauth.ts:473", enterpriseModelsScope)
	}
	// Sanity check that the scoped URL is built from the product endpoint.
	product, _ := productByConfigValue(ProductCodeBuddy)
	rawURL := product.Endpoint + "/console/enterprises/" + enterpriseModelsScope + "/models"
	if rawURL != "https://copilot.tencent.com/console/enterprises/personal/models" {
		t.Fatalf("scoped URL = %q", rawURL)
	}
}

func TestModelCacheKeyIsPerProduct(t *testing.T) {
	codebuddy, _ := productByConfigValue(ProductCodeBuddy)
	workbuddy, _ := productByConfigValue(ProductWorkBuddy)
	if cachedCatalogKey(codebuddy) == cachedCatalogKey(workbuddy) {
		t.Fatal("the model pool differs per product, so the cache key must too")
	}
	discoveredModels.reset()
	discoveredModels.put(cachedCatalogKey(codebuddy), []remoteModel{{ID: "only-cn"}})
	if _, ok := discoveredModels.get(cachedCatalogKey(workbuddy), time.Hour); ok {
		t.Fatal("a warm CodeBuddy cache must not answer for WorkBuddy")
	}
	if cached, ok := discoveredModels.get(cachedCatalogKey(codebuddy), time.Hour); !ok || cached[0].ID != "only-cn" {
		t.Fatal("the CodeBuddy cache should still be warm")
	}
	if _, ok := discoveredModels.get(cachedCatalogKey(codebuddy), 0); ok {
		// A zero TTL falls back to two hours inside the callers, but the cache
		// itself treats it as "expired"; make sure that is at least not a panic.
		t.Log("zero TTL reported a miss")
	}
	discoveredModels.reset()
}

func TestParseModelsFromConfigAcceptsJSONRoundTrip(t *testing.T) {
	// The remote body arrives as bytes; make sure a decoded-with-encoding/json
	// body (float64 numbers) is what the parser expects.
	raw := []byte(`{"data":{"agents":[{"name":"craft","models":["hy3"]}],` +
		`"models":[{"id":"hy3","maxInputTokens":192000,"maxOutputTokens":64000}]}}`)
	var decoded any
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	models := parseModelsFromConfig(decoded)
	if len(models) != 1 || models[0].ID != "hy3" {
		t.Fatalf("models = %+v", models)
	}
	if models[0].MaxOutputTokens != 64_000 {
		t.Fatalf("max output = %d", models[0].MaxOutputTokens)
	}
}
