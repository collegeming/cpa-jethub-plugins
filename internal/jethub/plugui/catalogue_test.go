package plugui

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The card exists to answer "which models does this channel offer?", so it must
// show the provider's own names. ID is the name this deployment routes by, and a
// plugin may rename upstream ids; showing that rename as if it were the
// provider's name would misreport the catalogue.
func TestCatalogueCardShowsTheProviderNativeName(t *testing.T) {
	card := string(CatalogueCard(ModelCatalogue{
		Source: "两个签名接口",
		Entries: []ModelEntry{
			// codearts renames this one; the provider's own spelling is the label.
			{Native: "deepseek-v4.1-flash", ID: "DeepSeek-V4.1-Flash"},
		},
	}))
	if !strings.Contains(card, "deepseek-v4.1-flash") {
		t.Errorf("the provider's own name is missing:\n%s", card)
	}
	// The routing name is still reachable, marked as such rather than presented
	// as the provider's name.
	if !strings.Contains(card, "DeepSeek-V4.1-Flash") {
		t.Errorf("the routing name is missing:\n%s", card)
	}
	if !strings.Contains(card, "请求用名") {
		t.Errorf("the routing name is not marked as one:\n%s", card)
	}
}

// When the two names agree, the row must not repeat itself.
func TestCatalogueCardOmitsTheRoutedNameWhenItMatches(t *testing.T) {
	card := string(CatalogueCard(ModelCatalogue{
		Entries: []ModelEntry{{Native: "GLM-5.3-Flash", ID: "GLM-5.3-Flash"}},
	}))
	if strings.Contains(card, "请求用名") {
		t.Errorf("an identical routing name was repeated:\n%s", card)
	}
	if strings.Count(card, "GLM-5.3-Flash") != 1 {
		t.Errorf("the name appears %d times, want 1:\n%s", strings.Count(card, "GLM-5.3-Flash"), card)
	}
}

// A short list needs no filter: one more control to skip past costs the reader
// more than it saves.
func TestCatalogueCardOmitsTheFilterForAShortList(t *testing.T) {
	entries := make([]ModelEntry, filterThreshold)
	for index := range entries {
		entries[index] = ModelEntry{Native: "model-" + string(rune('a'+index))}
	}
	card := string(CatalogueCard(ModelCatalogue{Entries: entries}))
	if strings.Contains(card, "cat-filter-input") {
		t.Errorf("a %d-entry list rendered a filter:\n%s", filterThreshold, card)
	}

	entries = append(entries, ModelEntry{Native: "one-more"})
	card = string(CatalogueCard(ModelCatalogue{Entries: entries}))
	if !strings.Contains(card, "cat-filter-input") {
		t.Errorf("a %d-entry list rendered no filter:\n%s", len(entries), card)
	}
}

// Every model must still be visible without JavaScript: the filter narrows an
// already-rendered list, it does not fetch one. A row hidden in the markup would
// be invisible on a host that refuses the script.
func TestCatalogueCardRendersEveryRowWithoutScripting(t *testing.T) {
	entries := make([]ModelEntry, 0, filterThreshold+3)
	for index := 0; index < filterThreshold+3; index++ {
		entries = append(entries, ModelEntry{Native: "model-" + string(rune('a'+index))})
	}
	card := string(CatalogueCard(ModelCatalogue{Entries: entries}))
	for _, entry := range entries {
		if !strings.Contains(card, entry.Native) {
			t.Errorf("%q is missing from the rendered table:\n%s", entry.Native, card)
		}
	}
	body := card[strings.Index(card, "<tbody>"):strings.Index(card, "</tbody>")]
	for _, row := range strings.Split(body, "<tr ")[1:] {
		if strings.Contains(row, " hidden") {
			t.Error("a row was rendered hidden, so it would stay invisible without the script")
		}
	}
}

// One row per model: the same routing id must not be listed twice.
func TestCatalogueCardDeduplicatesByRoutingID(t *testing.T) {
	card := string(CatalogueCard(ModelCatalogue{
		Source: "内置表",
		Entries: []ModelEntry{
			{Native: "GLM-5.3-Flash", ID: "GLM-5.3-Flash"},
			{Native: "GLM-5.3-Flash", ID: "GLM-5.3-Flash"},
		},
	}))
	if count := strings.Count(card, ">GLM-5.3-Flash<"); count != 1 {
		t.Errorf("the model is listed %d times, want 1:\n%s", count, card)
	}
	if !strings.Contains(card, "<dd>1</dd>") {
		t.Errorf("the count does not reflect the deduplicated list:\n%s", card)
	}
}

// Two DIFFERENT models that the provider labels identically must both be kept.
//
// Measured on Cline's live catalogue: `qwen/qwen3.8-27b` and
// `qwen/qwen3.8-27b:free` both derive the display name "Qwen3.8 27b", and 12 such
// pairs existed. Deduplicating on the label silently deleted 12 real models.
func TestCatalogueCardKeepsDistinctModelsWithTheSameLabel(t *testing.T) {
	card := string(CatalogueCard(ModelCatalogue{
		Source: "线上目录",
		Entries: []ModelEntry{
			{Native: "Qwen3.8 27b", ID: "qwen/qwen3.8-27b"},
			{Native: "Qwen3.8 27b", ID: "qwen/qwen3.8-27b:free"},
		},
	}))
	if count := strings.Count(card, ">Qwen3.8 27b<"); count != 2 {
		t.Errorf("the label appears %d times, want 2 — a distinct model was dropped:\n%s", count, card)
	}
	if !strings.Contains(card, "<dd>2</dd>") {
		t.Errorf("the count should be 2:\n%s", card)
	}
	// Both routing names stay reachable, and each is distinguishable.
	for _, id := range []string{"qwen/qwen3.8-27b", "qwen/qwen3.8-27b:free"} {
		if !strings.Contains(card, id) {
			t.Errorf("routing id %q is missing:\n%s", id, card)
		}
	}
}

// An empty catalogue must say so rather than render an empty table.
func TestCatalogueCardStatesAnEmptyCatalogue(t *testing.T) {
	card := string(CatalogueCard(ModelCatalogue{EmptyNotice: "暂无模型"}))
	if !strings.Contains(card, "暂无模型") {
		t.Errorf("the empty notice is missing:\n%s", card)
	}
	if strings.Contains(card, "<table") {
		t.Errorf("an empty catalogue rendered a table:\n%s", card)
	}
}

// The filter must match on the routing name too, or a reader who knows the model
// by the name they type into a request would find nothing.
func TestCatalogueSearchTextCoversBothNames(t *testing.T) {
	card := string(CatalogueCard(ModelCatalogue{
		Entries: []ModelEntry{{Native: "deepseek-v4.1-flash", ID: "DeepSeek-V4.1-Flash"}},
	}))
	// data-search carries both spellings, lower-cased.
	if !strings.Contains(card, `data-search="deepseek-v4.1-flash deepseek-v4.1-flash"`) {
		t.Errorf("the search haystack is missing a spelling:\n%s", card)
	}
	renamed := string(CatalogueCard(ModelCatalogue{
		Entries: []ModelEntry{{Native: "glm5.3-flash", ID: "GLM-5.3-Flash"}},
	}))
	if !strings.Contains(renamed, "glm5.3-flash glm-5.3-flash") {
		t.Errorf("the search haystack does not carry both spellings:\n%s", renamed)
	}
}

// Escape-hatch: a model name is upstream data and must never become markup.
func TestCatalogueCardEscapesNames(t *testing.T) {
	card := string(CatalogueCard(ModelCatalogue{
		Entries: []ModelEntry{{Native: `<img src=x onerror=alert(1)>`}},
	}))
	if strings.Contains(card, "<img src=x") {
		t.Errorf("a model name was rendered as markup:\n%s", card)
	}
	if !strings.Contains(card, "&lt;img") {
		t.Errorf("the name was not escaped:\n%s", card)
	}
}

// ModelEntriesFromInfo reads Name as the displayed value and falls back to ID,
// so a plugin that never sets Name still lists its models.
func TestModelEntriesFromInfoPrefersNameAndFallsBackToID(t *testing.T) {
	entries := ModelEntriesFromInfo([]pluginapi.ModelInfo{
		{ID: "DeepSeek-V4.1-Flash", Name: "deepseek-v4.1-flash"},
		{ID: "GLM-5.3-Flash"},
	}, func(info pluginapi.ModelInfo) string { return "ctx " + info.ID })
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Native != "deepseek-v4.1-flash" || entries[0].ID != "DeepSeek-V4.1-Flash" {
		t.Errorf("entry 0 = %+v, want the provider name displayed and the id routed", entries[0])
	}
	if entries[1].Native != "GLM-5.3-Flash" {
		t.Errorf("entry 1 native = %q, want the ID fallback", entries[1].Native)
	}
	if entries[0].Detail != "ctx DeepSeek-V4.1-Flash" {
		t.Errorf("detail = %q, want the callback's value", entries[0].Detail)
	}
}
