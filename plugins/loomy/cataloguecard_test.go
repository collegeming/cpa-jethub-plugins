package main

import (
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
)

// TestCatalogueCardReplacesTheHandwrittenOne pins the structural requirement:
// this plugin must have exactly ONE 模型目录 card. The hand-written card was
// replaced rather than kept beside the shared one, because two cards answering
// "which models does this channel offer" would leave a reader unsure which to
// believe.
func TestCatalogueCardReplacesTheHandwrittenOne(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	withAccount(t, fake)
	withSettings(t, DefaultConfig())

	body := renderPage(t, testHost(), "/status", nil)
	if got := strings.Count(body, "模型目录</h2>"); got != 1 {
		t.Fatalf("the status page renders %d 模型目录 cards, want exactly 1:\n%s", got, truncate(body, 3000))
	}
	if !strings.Contains(body, "action=refresh-catalog") {
		t.Error("the catalogue card lost its 刷新目录 action")
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestCatalogueCardListsTheModels pins the point of the card: it must carry rows,
// not only provenance. The bundled table answers when the cache is cold, and the
// rows are the vendor's own names.
func TestCatalogueCardListsTheModels(t *testing.T) {
	withSettings(t, DefaultConfig())
	resetDiscoveredModels()
	t.Cleanup(resetDiscoveredModels)

	card := string(catalogueCard(DefaultConfig()))
	if !strings.Contains(card, "模型目录") {
		t.Fatalf("no 模型目录 card was rendered:\n%s", card)
	}
	for _, name := range []string{"MiniMax M3", "Qwen 3.8 Max", "DeepSeek V4 Flash 0731"} {
		if !strings.Contains(card, name) {
			t.Errorf("the catalogue card does not list %q:\n%s", name, card)
		}
	}
	if !strings.Contains(card, "请求用名") {
		t.Errorf("the card does not report the routing id beside the vendor name:\n%s", card)
	}
}

// TestCatalogueCardShowsTheVendorNameNotTheStrippedOne is the naming guard, and
// it is subtle for this provider in a way worth pinning.
//
// `modelDescriptor.Name` is the vendor's raw wire name, multiplier included
// (models.go:99-101), and models.go:19-23 records that the price lives in that
// name and nowhere else. `ModelInfo.Name`, by contrast, is `bareName()`
// (models.go:153) — `splitLoomyRate(m.Name)`, a LOCAL normalisation that strips
// the multiplier. Sourcing Native from `ModelInfo.Name` would therefore display a
// string the vendor never published.
//
// `ModelInfo.ID` is the wire id (`MiniMax-M3`), which requests route by; it
// belongs in the row as 请求用名, never as the label.
func TestCatalogueCardShowsTheVendorNameNotTheStrippedOne(t *testing.T) {
	entries := []modelDescriptor{
		{ID: "MiniMax-M3", Name: "MiniMax M3 （x4.0）", ContextLength: 1_048_576},
		{ID: "qwen-3.8-max", Name: "Qwen 3.8 Max (x12.0)", ContextLength: 1_000_000},
		{ID: "GLM-5.3-Flash", Name: "GLM 5.3 Flash(x0.8)", ContextLength: 1_048_576},
		{ID: "spark-x", Name: "Spark X2.5", ContextLength: 1_048_576},
	}
	rows := catalogueModelEntries(entries)
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}

	want := []struct {
		native string
		id     string
	}{
		// The vendor's own three multiplier spellings survive verbatim.
		{native: "MiniMax M3 （x4.0）", id: "MiniMax-M3"},
		{native: "Qwen 3.8 Max (x12.0)", id: "qwen-3.8-max"},
		{native: "GLM 5.3 Flash(x0.8)", id: "GLM-5.3-Flash"},
		// A name with no multiplier stays as upstream sent it, and the routed id
		// is still reported because it differs.
		{native: "Spark X2.5", id: "spark-x"},
	}
	for index, expected := range want {
		if rows[index].Native != expected.native {
			t.Errorf("rows[%d].Native = %q, want the vendor's own name %q",
				index, rows[index].Native, expected.native)
		}
		if rows[index].ID != expected.id {
			t.Errorf("rows[%d].ID = %q, want the routing id %q", index, rows[index].ID, expected.id)
		}
	}

	card := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: rows}))
	for _, expected := range want {
		if !strings.Contains(card, `<span class="mono">`+expected.native+`</span>`) {
			t.Errorf("the card does not label a row with the vendor name %q:\n%s", expected.native, card)
		}
	}
	// The STRIPPED spelling must never be the main label: upstream never
	// published it.
	for _, stripped := range []string{"MiniMax M3", "Qwen 3.8 Max", "GLM 5.3 Flash"} {
		if strings.Contains(card, `<span class="mono">`+stripped+`</span>`) {
			t.Errorf("the card uses the locally stripped name %q as the main label:\n%s", stripped, card)
		}
	}
}

// TestCatalogueCardReadsTheCacheWithoutCallingTheVendor pins that a page load is
// served from this plugin's own cache or the bundled table and NEVER issues an
// upstream call. `activeCatalogue` would fetch on a cold cache, which is exactly
// why the card has its own `catalogueForPage` that peeks instead.
//
// Discovery is switched ON explicitly: this plugin ships it OFF
// (config.go:147-150), and the cache branch is unreachable without it — the test
// would otherwise pass by taking the pinned-table path and prove nothing.
func TestCatalogueCardReadsTheCacheWithoutCallingTheVendor(t *testing.T) {
	fake := newFakeHost()
	// No `do` handler: any HTTP call fails the test loudly.
	fake.install(t)
	cfg := DefaultConfig()
	cfg.DiscoverModels = true
	withSettings(t, cfg)
	resetDiscoveredModels()
	t.Cleanup(resetDiscoveredModels)

	if _, source := catalogueForPage(cfg); !strings.Contains(source, "尚未拉取") {
		t.Fatalf("source without a cache = %q, want it to say the listing was never fetched", source)
	}
	if requests := fake.callsFor(ModelsPath); len(requests) != 0 {
		t.Fatalf("a page load issued %d upstream calls; it must be served from the cache", len(requests))
	}

	putCachedModels([]modelDescriptor{{ID: "MiniMax-M3", Name: "MiniMax M3 （x4.0）", Remote: true}})
	entries, source := catalogueForPage(cfg)
	if len(entries) != 1 || entries[0].ID != "MiniMax-M3" {
		t.Fatalf("catalogueForPage with a warm cache = %+v", entries)
	}
	if !strings.Contains(source, "缓存于") {
		t.Fatalf("source with a warm cache = %q, want the cache line", source)
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
// exclusion or alias, and that it lists image models too: `SupportsImage` says a
// model ACCEPTS images and must never be a filter (models.go:105-108).
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
	if requests := fake.callsFor(ModelsPath); len(requests) != 0 {
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
