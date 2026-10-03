package main

import (
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
)

// TestCatalogueCardListsTheModels pins the point of the shared card: it answers
// "which models does this channel offer", so it must actually carry rows. A card
// that only reports provenance is the defect this replaces.
//
// The listing is read from the cache and the bundled table, so no host is
// installed: any transport call would fail the test loudly.
func TestCatalogueCardListsTheModels(t *testing.T) {
	withSettings(t, DefaultConfig())
	resetDiscoveredModels()
	t.Cleanup(resetDiscoveredModels)

	card := string(catalogueCard(DefaultConfig()))
	if !strings.Contains(card, "模型目录") {
		t.Fatalf("the status page has no 模型目录 card:\n%s", card)
	}
	for _, id := range []string{"sn-kimi-k3", "sn-glm-5-3", "sn-deepseek-v4-1-flash"} {
		if !strings.Contains(card, id) {
			t.Errorf("the catalogue card does not list %q:\n%s", id, card)
		}
	}
	if !strings.Contains(card, "action=refresh-catalog") {
		t.Error("the catalogue card lost its 刷新目录 action")
	}
	if strings.Contains(card, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestCatalogueCardShowsTheVendorNameAndKeepsThePrice is the naming guard plus
// the price guard, which pull in opposite directions here and must both hold.
//
// The vendor's own catalogue publishes the model under `name` — which
// `normaliseCatalogueEntry` records as the MODEL ID rather than a display name
// (models.go:267-268) — and this plugin renames nothing (models.go:19-23). So
// Native must be that id, and the card must NOT invent a 「请求用名」 row: the two
// names are the same string.
//
// The price, by contrast, must survive: `catalogueEntry.info` puts the
// multiplier into `ModelInfo.Name` (models.go:134), and using that as Native
// would dress this plugin's own annotation up as the vendor's name. `x1` is
// stated explicitly because omitting it makes a 1x model indistinguishable from
// one whose multiplier failed to load (models.go:89-91).
func TestCatalogueCardShowsTheVendorNameAndKeepsThePrice(t *testing.T) {
	entries := []catalogueEntry{
		{ID: "sn-kimi-k3", Description: "Kimi-K3", ContextWindow: 1_000_000, MaxTokens: 100_000,
			EffectiveMultiplier: numberRef(1), BaseMultiplier: numberRef(1)},
		{ID: "sn-sensenova-6-8-flash", Description: "SenseNova-6.8-Flash", ContextWindow: 256_000, MaxTokens: 63_999,
			EffectiveMultiplier: numberRef(0), BaseMultiplier: numberRef(0.5)},
	}
	rows := catalogueModelEntries(entries)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for index, want := range []string{"sn-kimi-k3", "sn-sensenova-6-8-flash"} {
		if rows[index].Native != want {
			t.Errorf("rows[%d].Native = %q, want the vendor's own id %q", index, rows[index].Native, want)
		}
		if rows[index].ID != want {
			t.Errorf("rows[%d].ID = %q, want %q: this channel renames nothing", index, rows[index].ID, want)
		}
	}

	card := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: rows}))
	if strings.Contains(card, "请求用名") {
		t.Fatalf("an unrenamed model claims a routed name:\n%s", card)
	}
	if !strings.Contains(card, `<span class="mono">sn-kimi-k3</span>`) {
		t.Fatalf("the card does not label the row with the vendor id:\n%s", card)
	}
	for _, want := range []string{"Kimi-K3 · x1", "SenseNova-6.8-Flash · 免费"} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lost the price figure %q:\n%s", want, card)
		}
	}
	// The priced spelling must NOT be the main label: that string is this
	// plugin's rendering, not the vendor's name.
	if strings.Contains(card, `<span class="mono">Kimi-K3 · x1</span>`) {
		t.Fatalf("the card uses the locally priced name as the main label:\n%s", card)
	}
}

// TestCatalogueCardReadsTheCacheWithoutCallingTheVendor pins that a page load is
// served from this plugin's own cache or the bundled table, and NEVER issues an
// upstream call. `activeCatalogue` would fetch on a cold cache, which is why the
// card's own `catalogueForPage` peeks instead.
func TestCatalogueCardReadsTheCacheWithoutCallingTheVendor(t *testing.T) {
	fake := newFakeHost()
	// No `do` handler: any HTTP call fails the test.
	fake.install(t)
	withSettings(t, DefaultConfig())
	resetDiscoveredModels()
	t.Cleanup(resetDiscoveredModels)

	cfg := DefaultConfig()
	if _, source := catalogueForPage(cfg); !strings.Contains(source, "尚未拉取") {
		t.Fatalf("source without a cache = %q, want it to say the listing was never fetched", source)
	}
	if requests := fake.callsFor(ModelCatalogPath); len(requests) != 0 {
		t.Fatalf("a page load issued %d upstream calls; it must be served from the cache", len(requests))
	}

	putCachedModels([]catalogueEntry{{ID: "sn-kimi-k3", Description: "Kimi-K3", Remote: true}})
	entries, source := catalogueForPage(cfg)
	if len(entries) != 1 || entries[0].ID != "sn-kimi-k3" {
		t.Fatalf("catalogueForPage with a warm cache = %+v", entries)
	}
	if !strings.Contains(source, "缓存于") {
		t.Fatalf("source with a warm cache = %q, want the cache line", source)
	}
	card := string(catalogueCard(cfg))
	if !strings.Contains(card, "sn-kimi-k3") {
		t.Fatalf("the card does not render the cached listing:\n%s", card)
	}
}

// TestCatalogueCardReportsDisabledDiscovery pins the honest source line when the
// operator pinned the catalogue to the bundled table.
func TestCatalogueCardReportsDisabledDiscovery(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DiscoverModels = false
	entries, source := catalogueForPage(cfg)
	if len(entries) != len(fallbackCatalogue) {
		t.Fatalf("entries = %d, want the bundled table (%d)", len(entries), len(fallbackCatalogue))
	}
	if !strings.Contains(source, "discover_models 已关闭") {
		t.Fatalf("source = %q, want it to name the disabled discovery", source)
	}
}

// TestCatalogueCardAppliesNoFilteringOfItsOwn pins that the card reimplements no
// model exclusion or alias: `oauth-excluded-models` and `oauth-model-alias`
// belong to the host, and a second mechanism here would silently hide real models.
func TestCatalogueCardAppliesNoFilteringOfItsOwn(t *testing.T) {
	withSettings(t, DefaultConfig())
	resetDiscoveredModels()
	t.Cleanup(resetDiscoveredModels)

	entries := fallbackModels()
	rows := catalogueModelEntries(entries)
	if len(rows) != len(entries) {
		t.Fatalf("rows = %d, want one per published model (%d)", len(rows), len(entries))
	}
	card := string(catalogueCard(DefaultConfig()))
	if got := strings.Count(card, "<tr data-search="); got != len(entries) {
		t.Fatalf("the card rendered %d rows for %d published models:\n%s", got, len(entries), card)
	}
}

// TestCatalogueCardRendersWithoutAnAccount pins the first-run experience: the
// card must answer "which models does this channel offer" before any credential
// exists, from the bundled table, without fetching anything.
func TestCatalogueCardRendersWithoutAnAccount(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	withSettings(t, DefaultConfig())
	resetDiscoveredModels()
	t.Cleanup(resetDiscoveredModels)

	page := renderPage(t, testHost(), "/status", nil)
	if !strings.Contains(page, "尚未添加账号") {
		t.Fatalf("the empty state is missing:\n%s", truncate(page, 2000))
	}
	if !strings.Contains(page, "模型目录") {
		t.Fatalf("the catalogue card is missing when no account is configured:\n%s", truncate(page, 2000))
	}
	if got := strings.Count(page, "<tr data-search="); got != len(fallbackCatalogue) {
		t.Fatalf("the card rendered %d rows, want the bundled table (%d):\n%s",
			got, len(fallbackCatalogue), truncate(page, 2000))
	}
	if requests := fake.callsFor(ModelCatalogPath); len(requests) != 0 {
		t.Fatalf("rendering without an account issued %d upstream calls: %#v", len(requests), requests)
	}
}

// TestCatalogueCardEmptyStateSaysSo pins that a catalogue with nothing to list
// renders the notice instead of an empty table.
func TestCatalogueCardEmptyStateSaysSo(t *testing.T) {
	card := plugui.CatalogueCard(plugui.ModelCatalogue{
		Source:      "内置兜底表",
		EmptyNotice: "暂无模型：内置兜底表为空。",
	})
	if strings.Contains(string(card), "<table") {
		t.Fatalf("an empty catalogue rendered a table:\n%s", card)
	}
	if !strings.Contains(string(card), "暂无模型") {
		t.Fatalf("an empty catalogue rendered no notice:\n%s", card)
	}
}
