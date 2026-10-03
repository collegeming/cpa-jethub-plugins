package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
)

// TestCatalogueCardListsTheModels pins the point of the card: it answers "which
// models does this channel offer", so it must actually carry rows. The card it
// replaced was titled 模型目录 but rendered one field pair per model with the id
// as a Field LABEL and no table; the reader had to reconstruct the list.
func TestCatalogueCardListsTheModels(t *testing.T) {
	withSettings(t, DefaultConfig())

	body := string(renderModelCard(settings(), fallbackModels()))
	if !strings.Contains(body, "模型目录") {
		t.Fatalf("the status page has no 模型目录 card:\n%s", body)
	}
	for _, want := range []string{"MiniMax-M3.1-Flash-Preview", "MiniMax-M3", "MiniMax-M2.7"} {
		if !strings.Contains(body, want) {
			t.Errorf("the catalogue card does not list %q:\n%s", want, body)
		}
	}
	if rows := strings.Count(body, `<tr data-search=`); rows != len(fallbackCatalogue) {
		t.Fatalf("the card rendered %d rows for %d bundled models:\n%s", rows, len(fallbackCatalogue), body)
	}
	if !strings.Contains(body, "action=refresh-catalog") {
		t.Error("the catalogue card lost its 刷新目录 action")
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestCatalogueCardLabelsRowsWithTheUpstreamID is the naming guard.
//
// This plugin has NO rename table: `modelInfoFor` (models.go:338,344) sets
// `ID: entry.ID` and `Name: entry.Name`, and the two are different KINDS of fact
// rather than two spellings of one — `ID` is the long id the inference endpoint
// expects (`MiniMax-M3.1-Flash-Preview`, the key in the vendor's `models`
// object, models.go:194) while `Name` is the SHORT label the official IDE shows
// (`M3.1-Flash-Preview`, product.go:136-140).
//
// So the card's label must be the upstream ID, and the short display name must
// NOT be promoted to the label — a reader copying the friendly spelling into a
// request would get a model the endpoint does not know. It appears as the row's
// detail instead.
func TestCatalogueCardLabelsRowsWithTheUpstreamID(t *testing.T) {
	entries := fallbackModels()
	rows := catalogueModelEntries(entries)
	if len(rows) != len(entries) {
		t.Fatalf("rows = %d, want one per catalogue entry (%d)", len(rows), len(entries))
	}
	for index, row := range rows {
		entry := entries[index]
		if row.Native != entry.ID {
			t.Errorf("row %d Native = %q, want the upstream long id %q", index, row.Native, entry.ID)
		}
		if row.ID != entry.ID {
			t.Errorf("row %d ID = %q, want %q (no rename exists in this plugin)", index, row.ID, entry.ID)
		}
		if !strings.Contains(row.Detail, entry.Name) {
			t.Errorf("row %d Detail = %q, want the short display name %q", index, row.Detail, entry.Name)
		}
	}

	// The short name must never be the label. `MiniMax-M3.1-Flash-Preview`
	// carries the short name `M3.1-Flash-Preview`, so the label check is exact:
	// the shorter spelling must appear only inside the detail span.
	body := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: rows}))
	if !strings.Contains(body, `<span class="mono">MiniMax-M3.1-Flash-Preview</span>`) {
		t.Fatalf("the card does not label the row with the upstream long id:\n%s", body)
	}
	if strings.Contains(body, `<span class="mono">M3.1-Flash-Preview</span>`) {
		t.Fatalf("the card labelled a row with the short display name:\n%s", body)
	}
}

// TestCatalogueCardReportsNoRoutedNameBecauseNothingIsRenamed pins the honest
// absence: with no rename table there is no second spelling to report, so the
// card must not invent a 「请求用名」 line.
func TestCatalogueCardReportsNoRoutedNameBecauseNothingIsRenamed(t *testing.T) {
	rows := catalogueModelEntries(fallbackModels())
	for _, row := range rows {
		if row.Native != row.ID {
			t.Fatalf("row %+v claims a routed name although this plugin renames nothing", row)
		}
	}
	body := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: rows}))
	if strings.Contains(body, "请求用名") {
		t.Fatalf("the card claims a routed name for an unrenamed catalogue:\n%s", body)
	}
}

// TestCatalogueCardKeepsTheThinkingMatrix pins that replacing the card lost no
// information: the thinking behaviour and the effort ladder — the two things a
// picker cannot show — are still stated, now on the model's own row.
func TestCatalogueCardKeepsTheThinkingMatrix(t *testing.T) {
	rows := catalogueModelEntries(fallbackModels())
	byID := map[string]plugui.ModelEntry{}
	for _, row := range rows {
		byID[row.Native] = row
	}

	// M3.1 is forced-on AND hard-rejects `disabled`.
	m31 := byID["MiniMax-M3.1-Flash-Preview"]
	if !strings.Contains(m31.Detail, "强制开启") || !strings.Contains(m31.Detail, "adaptive") {
		t.Errorf("M3.1 detail = %q, want the forced-on/adaptive rule", m31.Detail)
	}
	if !strings.Contains(m31.Detail, "上下文（context_window_options 最大档）") {
		t.Errorf("M3.1 detail = %q, want the window's provenance stated", m31.Detail)
	}
	// M3 is switchable and offers no effort tiers.
	m3 := byID["MiniMax-M3"]
	if !strings.Contains(m3.Detail, "可开关") {
		t.Errorf("M3 detail = %q, want the switchable rule", m3.Detail)
	}
	if !strings.Contains(m3.Detail, "无档位") {
		t.Errorf("M3 detail = %q, want it to say the model publishes no effort tiers", m3.Detail)
	}
	// M2.7 is forced-on but only silently ignores `disabled` — a DIFFERENT rule
	// from the hard rejection, and conflating them would tell the user a request
	// is refused when it is merely ignored.
	m27 := byID["MiniMax-M2.7"]
	if !strings.Contains(m27.Detail, "静默忽略") {
		t.Errorf("M2.7 detail = %q, want the silently-ignored rule", m27.Detail)
	}
	if strings.Contains(m27.Detail, "硬拒") {
		t.Errorf("M2.7 detail = %q, but this model does not hard-reject disabled thinking", m27.Detail)
	}
}

// TestCatalogueCardRendersFromTheCacheOrTheBundledTable pins that the card
// reports what this deployment currently HAS, never a fresh fetch: the page-load
// path takes the cache or the bundled table.
//
// The assertion is on the recorded outbound requests rather than on the absence
// of a host, so "the page did not fetch" is observed instead of assumed.
func TestCatalogueCardRendersFromTheCacheOrTheBundledTable(t *testing.T) {
	withSettings(t, DefaultConfig())
	resetDiscoveredModels()
	t.Cleanup(resetDiscoveredModels)

	// staticCatalogueEntries is the page's source, and it never dials.
	if got := staticCatalogueEntries(settings()); len(got) != len(fallbackCatalogue) {
		t.Fatalf("staticCatalogueEntries without a cache = %d models, want the bundled %d", len(got), len(fallbackCatalogue))
	}
	putCachedModels([]ModelCatalogEntry{{ID: "MiniMax-M9", Name: "M9"}})
	got := staticCatalogueEntries(settings())
	if len(got) != 1 || got[0].ID != "MiniMax-M9" {
		t.Fatalf("staticCatalogueEntries with a warm cache = %+v", got)
	}

	// The whole status page, not just the helper: a page load is the thing that
	// must stay offline.
	host := catalogueHost(t, modelsBody("MiniMax-M3"))
	if requestedCatalogue(host) {
		t.Fatal("building the page's catalogue reached the upstream endpoint")
	}
	dispatchPage(t, "/v0/resource/plugins/minimax/status", url.Values{})
	if requestedCatalogue(host) {
		t.Fatal("the status page fetched the model catalogue while rendering")
	}
}

// TestCatalogueCardEmptyStateSaysSo pins that a catalogue with nothing to list
// renders the notice instead of an empty table.
func TestCatalogueCardEmptyStateSaysSo(t *testing.T) {
	card := plugui.CatalogueCard(plugui.ModelCatalogue{
		Source:      "内置兜底表",
		EmptyNotice: "暂无模型：远端目录尚未拉取，且内置兜底表为空。",
	})
	if strings.Contains(string(card), "<table") {
		t.Fatalf("an empty catalogue rendered a table:\n%s", card)
	}
	if !strings.Contains(string(card), "暂无模型") {
		t.Fatalf("an empty catalogue gave no notice:\n%s", card)
	}
}

// TestStatusPageCarriesOneModelCard pins that the shared card is the page's only
// model LISTING: the old hand-written one must be replaced, not kept beside it.
//
// The protocol card above legitimately keeps a `模型目录` metadata FIELD naming
// where the catalogue comes from, so the count that matters is the card
// headings, not every occurrence of the phrase.
func TestStatusPageCarriesOneModelCard(t *testing.T) {
	withSettings(t, DefaultConfig())
	catalogueHost(t, modelsBody("MiniMax-M3"))
	body := dispatchPage(t, "/v0/resource/plugins/minimax/status", url.Values{})
	if got := strings.Count(body, "<h2>模型目录</h2>"); got != 1 {
		t.Fatalf("the page renders %d 模型目录 cards, want exactly 1:\n%s", got, body)
	}
	if !strings.Contains(body, `<span class="mono">MiniMax-M3</span>`) {
		t.Fatalf("the page's catalogue card does not list the bundled models:\n%s", body)
	}
}
