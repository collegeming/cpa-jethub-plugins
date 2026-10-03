package main

import (
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
)

// TestCatalogueCardListsTheBuiltInTable pins the point of the card: it must list
// the models this deployment publishes, not only metadata about them.
//
// Qoder has no upstream directory endpoint (see the header of catalogrefresh.go:
// the vendor's listing path is signature-gated and the shipped WASM artifact
// cannot sign it), so the built-in table IS the catalogue.
func TestCatalogueCardListsTheBuiltInTable(t *testing.T) {
	withSettings(t, DefaultConfig())

	card := string(qoderCatalogueCard(RegionGlobal))
	if !strings.Contains(card, "模型目录") {
		t.Fatalf("no 模型目录 card was rendered:\n%s", card)
	}
	table := productByID(string(RegionGlobal)).ModelCatalog
	if len(table) == 0 {
		t.Fatal("the built-in table is empty; the assertion below would be vacuous")
	}
	for _, model := range table {
		if !strings.Contains(card, model.Display) {
			t.Errorf("the catalogue card does not list %q:\n%s", model.Display, card)
		}
	}
	if got := strings.Count(card, "<tr data-search="); got != len(table) {
		t.Fatalf("the card rendered %d rows for %d built-in models:\n%s", got, len(table), card)
	}
	if !strings.Contains(card, "action=refresh-catalog") {
		t.Error("the catalogue card lost its 刷新目录 action")
	}
	if strings.Contains(card, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestCatalogueCardNeverClaimsAnUpstreamName is the honesty guard specific to
// this provider.
//
// There is no upstream directory to quote, so the card must SAY the listing is
// the built-in table rather than let a reader assume it came from the vendor. The
// Source line is where that statement lives, and it must not contain any wording
// that implies a fetch happened.
func TestCatalogueCardNeverClaimsAnUpstreamName(t *testing.T) {
	withSettings(t, DefaultConfig())

	card := string(qoderCatalogueCard(RegionGlobal))
	if !strings.Contains(card, "内置静态表") {
		t.Fatalf("the card does not state that its listing is the built-in table:\n%s", card)
	}
	if !strings.Contains(card, "无上游目录端点") {
		t.Fatalf("the card does not state that there is no upstream directory endpoint:\n%s", card)
	}
	if !strings.Contains(card, "未拉取任何上游目录") {
		t.Fatalf("the card does not state that nothing was fetched:\n%s", card)
	}
	for _, forbidden := range []string{"上游目录（", "缓存于 ", "线上目录"} {
		if strings.Contains(card, forbidden) {
			t.Errorf("the card claims a fetched upstream listing (%q):\n%s", forbidden, card)
		}
	}
}

// TestCatalogueCardSeparatesTheBuiltInNameFromTheRoutingKey pins which string is
// which, and that the card reports BOTH.
//
// `modelInfoFor` sets `Name` to the table's own `Display` (models.go:153) and
// `ID` to the catalog `Key` (models.go:147) — the key requests are addressed with
// (product.go:70-73). These genuinely differ (`DeepSeek-V4-Pro` vs `dmodel`), so
// the row must carry the display name as its label AND the key as 请求用名: a
// reader needs the key to call the model, and hiding it would make the card
// useless for its stated purpose.
func TestCatalogueCardSeparatesTheBuiltInNameFromTheRoutingKey(t *testing.T) {
	region := RegionGlobal
	infos := staticModelInfos(DefaultConfig(), region)
	rows := qoderCatalogueModelEntries(infos)
	if len(rows) != len(infos) {
		t.Fatalf("rows = %d, want one per published model (%d)", len(rows), len(infos))
	}
	for index, info := range infos {
		if rows[index].Native != info.Name {
			t.Errorf("rows[%d].Native = %q, want the table's model name %q", index, rows[index].Native, info.Name)
		}
		if rows[index].ID != info.ID {
			t.Errorf("rows[%d].ID = %q, want the routing key %q", index, rows[index].ID, info.ID)
		}
	}

	card := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: rows}))
	// Take one model the table renames, so the routed line is required.
	model, known := catalogModelFor(productByID(string(region)), "dmodel")
	if !known {
		t.Fatal("dmodel is not in the built-in table; the assertion below would be vacuous")
	}
	if model.Display == model.Key {
		t.Skipf("the table entry %q has no distinct display name", model.Key)
	}
	if !strings.Contains(card, `<span class="mono">`+model.Display+`</span>`) {
		t.Fatalf("the card does not label the row with the table's own name %q:\n%s", model.Display, card)
	}
	if !strings.Contains(card, "请求用名") || !strings.Contains(card, model.Key) {
		t.Fatalf("the card does not report the routing key %q a request must send:\n%s", model.Key, card)
	}
}

// TestCatalogueCardListsOperatorDeclaredModels pins that `public_models` entries
// are listed and marked as operator-declared rather than passed off as table or
// vendor data.
func TestCatalogueCardListsOperatorDeclaredModels(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PublicModels = []string{"my-custom-model"}
	withSettings(t, cfg)

	card := string(qoderCatalogueCard(RegionGlobal))
	if !strings.Contains(card, "my-custom-model") {
		t.Fatalf("the card does not list the operator-declared model:\n%s", card)
	}
	if !strings.Contains(card, "public_models 声明") {
		t.Fatalf("the card presents an operator-declared name without saying so:\n%s", card)
	}
	// It must be counted in the published total, not silently dropped.
	if !strings.Contains(card, "public_models 声明的公开端点模型名") {
		t.Fatalf("the source line does not account for the operator-declared model:\n%s", card)
	}
}

// TestCatalogueCardAppliesNoFilteringOfItsOwn pins that the card reimplements no
// model exclusion or alias: those live in the host's `oauth-excluded-models` and
// `oauth-model-alias`, and a second mechanism here would hide real models.
func TestCatalogueCardAppliesNoFilteringOfItsOwn(t *testing.T) {
	withSettings(t, DefaultConfig())

	region := RegionGlobal
	table := productByID(string(region)).ModelCatalog
	card := string(qoderCatalogueCard(region))
	for _, model := range table {
		if !strings.Contains(card, model.Key) {
			t.Errorf("the card hid the built-in model %q:\n%s", model.Key, card)
		}
	}
}

// TestCatalogueCardEmptyStateSaysSo pins that a catalogue with nothing to list
// renders the notice instead of an empty table.
func TestCatalogueCardEmptyStateSaysSo(t *testing.T) {
	card := plugui.CatalogueCard(plugui.ModelCatalogue{
		Source:      "内置静态表（无上游目录端点）",
		EmptyNotice: "暂无模型：内置静态表为空，且 public_models 未声明任何模型名。",
	})
	if strings.Contains(string(card), "<table") {
		t.Fatalf("an empty catalogue rendered a table:\n%s", card)
	}
	if !strings.Contains(string(card), "暂无模型") {
		t.Fatalf("an empty catalogue rendered no notice:\n%s", card)
	}
}

// TestCatalogueCardRendersWithoutAnAccount pins the first-run experience: the
// card must answer "which models does this channel offer" before any credential
// exists, because its listing is compiled in rather than fetched.
func TestCatalogueCardRendersWithoutAnAccount(t *testing.T) {
	newFakeHost().install(t)
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest("/status", nil, "text/html"))
	page := string(response.Body)
	if !strings.Contains(page, "尚未添加账号") {
		t.Fatalf("the empty state is missing:\n%s", page)
	}
	if !strings.Contains(page, "模型目录") {
		t.Fatalf("the catalogue card is missing when no account is configured:\n%s", page)
	}
	if got := strings.Count(page, "<tr data-search="); got != len(qoderModelCatalog) {
		t.Fatalf("the card rendered %d rows, want the built-in table (%d):\n%s",
			got, len(qoderModelCatalog), page)
	}
}

// TestCatalogueCardMakesNoNetworkCall pins that rendering the card issues no
// upstream request at all. Qoder's model metadata is compiled in, and the card
// must not turn a page view into the first network activity this provider makes.
func TestCatalogueCardMakesNoNetworkCall(t *testing.T) {
	newFakeHost().install(t)
	withSettings(t, DefaultConfig())

	// `newFakeHost` installs a transport that fails the test on any call, so a
	// successful render is itself the assertion.
	if card := string(qoderCatalogueCard(RegionGlobal)); !strings.Contains(card, "模型目录") {
		t.Fatalf("the card did not render without a network:\n%s", card)
	}
}
