package main

import (
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
)

// codeBuddyConfig is the configuration these tests run under: the real defaults,
// so `discover_models` stays on the way an operator's deployment has it. A bare
// `Config{Product: …}` literal would leave DiscoverModels false and quietly test
// the pinned-table branch instead of the cache branch.
func codeBuddyConfig() Config {
	cfg := DefaultConfig()
	cfg.Product = ProductCodeBuddy
	return cfg
}

// productUnderTest is the product whose fallback table the card renders when no
// remote catalog has been fetched.
func productUnderTest(t *testing.T) productConfig {
	t.Helper()
	product, ok := productByConfigValue(ProductCodeBuddy)
	if !ok {
		t.Fatalf("product %q is not declared", ProductCodeBuddy)
	}
	return product
}

// TestCatalogueCardListsTheModels pins the point of the card: it answers "which
// models does this channel offer", so it must actually carry rows. A card that
// only reports provenance is the defect this pins.
func TestCatalogueCardListsTheModels(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(codeBuddyConfig())
	discoveredModels.reset()
	t.Cleanup(discoveredModels.reset)

	body := string(catalogueCard(settings()))
	if !strings.Contains(body, "模型目录") {
		t.Fatalf("the status page has no 模型目录 card:\n%s", body)
	}
	for _, want := range []string{"hy3", "glm-5.3-flash", "minimax-m3"} {
		if !strings.Contains(body, want) {
			t.Errorf("the catalogue card does not list %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "action=refresh-catalog") {
		t.Error("the catalogue card lost its 刷新目录 action")
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestCatalogueCardShowsTheUpstreamNameNotTheRenamedID is the naming guard.
//
// `publicModelIDs` (models.go:840) renames nine CodeBuddy-native ids so this
// deployment publishes a canonical spelling: `glm-5.3-flash` → `GLM-5.3-Flash`,
// `hy3` → `Hy3`, `minimax-m3` → `MiniMax-M3`. Those results are the ROUTING ids,
// not the vendor's own names, so the card must label the row with the native
// spelling and report the renamed one as 「请求用名」 — never the other way round.
//
// The evidence for which field holds which name is `modelInfoFor`
// (models.go:893-899): `ID: publicModelID(model.ID)` and `Name: model.ID`, where
// `model.ID` is the id the vendor's own model endpoint returned
// (`parseModelsFromConfig` sets `entry.ID = id` from the `data.models` key, models.go:562).
func TestCatalogueCardShowsTheUpstreamNameNotTheRenamedID(t *testing.T) {
	product := productUnderTest(t)
	entries := catalogueModelEntries(product, []remoteModel{
		{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", ContextWindow: 1_000_000},
	})
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Native != "glm-5.3-flash" {
		t.Fatalf("Native = %q, want the upstream id %q", entry.Native, "glm-5.3-flash")
	}
	if entry.ID != "GLM-5.3-Flash" {
		t.Fatalf("ID = %q, want the renamed routing id %q", entry.ID, "GLM-5.3-Flash")
	}

	body := string(catalogueCard(settings()))
	native, routed := "glm-5.3-flash", "GLM-5.3-Flash"
	if !strings.Contains(body, "请求用名") {
		t.Fatalf("the card does not report the routed name for a renamed model:\n%s", body)
	}
	if !strings.Contains(body, `<span class="mono">`+native+`</span>`) {
		t.Fatalf("the card does not label the row with the upstream name %q:\n%s", native, body)
	}
	if strings.Contains(body, `<span class="mono">`+routed+`</span><span class="cat-routed">`) {
		t.Fatalf("the card uses the renamed id %q as the main label:\n%s", routed, body)
	}
}

// TestCatalogueCardMarksTheRoutedNameOnlyWhenItDiffers pins the other half of
// the naming rule: an unrenamed model must NOT grow a 「请求用名」 line claiming a
// difference that does not exist.
func TestCatalogueCardMarksTheRoutedNameOnlyWhenItDiffers(t *testing.T) {
	product := productUnderTest(t)
	entries := catalogueModelEntries(product, []remoteModel{
		{ID: "deepseek-v4-pro", Name: "Deepseek-V4-Pro", ContextWindow: 1_000_000},
	})
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Native != entries[0].ID {
		t.Fatalf("row = %+v, want the two names to agree for an id with no rename", entries[0])
	}
	card := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: entries}))
	if strings.Contains(card, "请求用名") {
		t.Fatalf("an unrenamed model claims a routed name:\n%s", card)
	}
}

// TestCatalogueCardPrefersTheCacheAndTouchesNoNetwork pins that a page load is
// served from the plugin's own cache: the fetched remote catalog for the
// configured product when one exists, the fallback table otherwise. A page view
// must never issue an upstream call.
func TestCatalogueCardPrefersTheCacheAndTouchesNoNetwork(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(codeBuddyConfig())
	t.Cleanup(discoveredModels.reset)

	product := productUnderTest(t)
	// No host is installed: any transport call would fail loudly.
	discoveredModels.reset()
	if _, source := catalogueForPage(settings(), product); !strings.Contains(source, "尚未拉取") {
		t.Fatalf("source without a cache = %q, want it to say the listing was never fetched", source)
	}

	discoveredModels.put(cachedCatalogKey(product), []remoteModel{{ID: "glm-5.3", Name: "GLM-5.3"}})
	models, source := catalogueForPage(settings(), product)
	if len(models) != 1 || models[0].ID != "glm-5.3" {
		t.Fatalf("catalogueForPage with a warm cache = %+v", models)
	}
	if !strings.Contains(source, "线上目录缓存") {
		t.Fatalf("source with a warm cache = %q, want the cache line", source)
	}

	// A cache filed under ANOTHER product must not be served under this one:
	// switching products would otherwise advertise the wrong model pool.
	other := workBuddyProduct(t)
	if other.ConfigValue == product.ConfigValue {
		t.Skip("the two products share a config value; the isolation check needs distinct products")
	}
	if models, _ := catalogueForPage(settings(), other); len(models) != len(staticCatalog(other)) {
		t.Fatalf("a foreign product's cache leaked into %q: %d models", other.ConfigValue, len(models))
	}
}

// workBuddyProduct returns a product other than the default one.
func workBuddyProduct(t *testing.T) productConfig {
	t.Helper()
	for _, product := range Products {
		if product.ConfigValue != ProductCodeBuddy {
			return product
		}
	}
	t.Fatal("no second product is declared")
	return productConfig{}
}

// TestCatalogueCardReportsEveryModelWithoutFiltering pins that the card applies
// no exclusion or alias of its own: `oauth-excluded-models` and
// `oauth-model-alias` belong to the host, and rows must not vanish because a
// second mechanism was reimplemented here.
func TestCatalogueCardReportsEveryModelWithoutFiltering(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	setSettings(codeBuddyConfig())
	t.Cleanup(discoveredModels.reset)

	product := productUnderTest(t)
	statics := staticCatalog(product)
	entries := catalogueModelEntries(product, statics)
	if len(entries) != len(statics) {
		t.Fatalf("entries = %d, want one per published model (%d)", len(entries), len(statics))
	}
	// Distinct models that happen to share a display name must all survive: the
	// shared card keys rows by routing id, never by label.
	body := string(catalogueCard(settings()))
	if rows := strings.Count(body, `<tr data-search=`); rows != len(statics) {
		t.Fatalf("the card rendered %d rows for %d published models:\n%s", rows, len(statics), body)
	}
}

// TestCatalogueCardEmptyStateSaysSo pins that a catalogue with nothing to list
// renders the notice instead of an empty table.
func TestCatalogueCardEmptyStateSaysSo(t *testing.T) {
	card := plugui.CatalogueCard(plugui.ModelCatalogue{
		Source:      "内置兜底表",
		EmptyNotice: "暂无模型：厂商模型接口尚未拉取，且内置兜底表为空。",
	})
	if strings.Contains(string(card), "<table") {
		t.Fatalf("an empty catalogue rendered a table:\n%s", card)
	}
	if !strings.Contains(string(card), "暂无模型") {
		t.Fatalf("an empty catalogue gave no notice:\n%s", card)
	}
}

// TestCatalogueCardDetailCarriesTheNumbers pins the right-hand column: the
// context window and output cap the adapter actually consumes, so a reader sees
// which parameters apply instead of having to guess.
func TestCatalogueCardDetailCarriesTheNumbers(t *testing.T) {
	product := productUnderTest(t)
	supportsImages := true
	entries := catalogueModelEntries(product, []remoteModel{
		{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 1_000_000, MaxOutputTokens: 64_000, SupportsImages: &supportsImages},
	})
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	detail := entries[0].Detail
	for _, want := range []string{"上下文 1000000", "输出上限 64000", "支持图片"} {
		if !strings.Contains(detail, want) {
			t.Errorf("Detail = %q, want it to mention %q", detail, want)
		}
	}
	// A model the catalog gave no output cap must not claim one.
	bare := catalogueModelEntries(product, []remoteModel{{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 200_000, MaxOutputTokens: 0}})
	if strings.Contains(bare[0].Detail, "输出上限") {
		t.Errorf("Detail = %q, want no output cap for a model that published none", bare[0].Detail)
	}
}
