package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
)

// TestCatalogueCardListsTheModels pins the point of the card: it answers "which
// models does this channel offer", so it must actually carry rows. The card it
// replaced listed at most `maxModelsOnPage` of them and then wrote 「其余 N 个模型
// 省略」 — the defect this pins.
func TestCatalogueCardListsTheModels(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	discoveredModels.reset()
	t.Cleanup(discoveredModels.reset)

	body := string(renderModelCard())
	if !strings.Contains(body, "模型目录") {
		t.Fatalf("the status page has no 模型目录 card:\n%s", body)
	}
	// Every bundled model is listed, not just the first screenful.
	rows := strings.Count(body, `<tr data-search=`)
	if rows != len(fallbackModels) {
		t.Fatalf("the card rendered %d rows for %d bundled models:\n%s", rows, len(fallbackModels), body)
	}
	if strings.Contains(body, "个模型省略") {
		t.Fatalf("the card still truncates its list:\n%s", body)
	}
	for _, want := range []string{"deepseek-v4-flash", "kimi-k2.7-code", "doubao-seed-2-1-pro-260628"} {
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
// `publicModelIDs` (models.go:379) renames six LobsterAI-native ids so this
// deployment publishes a canonical spelling: `glm-5.3-flash` → `GLM-5.3-Flash`,
// `deepseek-flash` → `DeepSeek-V4.1-Flash`, `qwen3.8-flash` →
// `Qwen3.8-Flash-Next`. Those results are the ROUTING ids, not the vendor's own
// names, so the card must label the row with the native spelling and report the
// renamed one as 「请求用名」 — never the other way round.
//
// The evidence for which field holds which name is `modelInfoFor`
// (models.go:414-420): `ID: publicModelID(model.ID)` and `Name: model.ID`, where
// `model.ID` comes straight from the vendor's `/api/models/available`
// (`parseModels` reads `modelId` into `ID`, models.go:256-264).
func TestCatalogueCardShowsTheUpstreamNameNotTheRenamedID(t *testing.T) {
	catalog := []remoteModel{{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash"}}
	entries := catalogueModelEntries(catalog)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Native != "glm-5.3-flash" {
		t.Fatalf("Native = %q, want the upstream id %q", entries[0].Native, "glm-5.3-flash")
	}
	if entries[0].ID != "GLM-5.3-Flash" {
		t.Fatalf("ID = %q, want the renamed routing id %q", entries[0].ID, "GLM-5.3-Flash")
	}

	body := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: entries}))
	if !strings.Contains(body, "请求用名") {
		t.Fatalf("the card does not report the routed name for a renamed model:\n%s", body)
	}
	if !strings.Contains(body, `<span class="mono">glm-5.3-flash</span>`) {
		t.Fatalf("the card does not label the row with the upstream name:\n%s", body)
	}
	if strings.Contains(body, `<span class="mono">GLM-5.3-Flash</span><span class="cat-routed">`) {
		t.Fatalf("the card uses the renamed id as the main label:\n%s", body)
	}
}

// TestCatalogueCardKeepsTheCostMarkerOutOfTheLabel pins the other naming rule
// the old card broke: `displayNameFor` splices the cost marker into the name
// (`GLM-5.3-Flash · x0.5`). The label must be the model's own name, so a model
// whose price changes does not read as a different model.
func TestCatalogueCardKeepsTheCostMarkerOutOfTheLabel(t *testing.T) {
	multiplier := 0.5
	catalog := []remoteModel{{ID: "glm-5.3", Name: "GLM-5.3", CostMultiplier: &multiplier}}
	entries := catalogueModelEntries(catalog)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Native != "glm-5.3" {
		t.Fatalf("Native = %q, want the bare vendor id", entries[0].Native)
	}
	if strings.Contains(entries[0].Native, "x0.5") {
		t.Fatalf("the cost marker leaked into the label: %q", entries[0].Native)
	}
	if !strings.Contains(entries[0].Detail, "倍率 x0.5") {
		t.Fatalf("Detail = %q, want the cost marker reported there instead", entries[0].Detail)
	}
	free := 0.0
	freeEntries := catalogueModelEntries([]remoteModel{{ID: "kimi-k2.6", Name: "kimi-k2.6", CostMultiplier: &free}})
	if !strings.Contains(freeEntries[0].Detail, "免费") {
		t.Fatalf("Detail = %q, want a free model to say so", freeEntries[0].Detail)
	}
}

// TestCatalogueCardDetailCarriesTheRemoteParameters pins that replacing the old
// card lost no information: every parameter it printed still appears, now on the
// row instead of in a separate field.
func TestCatalogueCardDetailCarriesTheRemoteParameters(t *testing.T) {
	window, maxTokens, images := int64(1_000_000), int64(64_000), true
	entries := catalogueModelEntries([]remoteModel{{
		ID: "glm-5.3-flash", Name: "GLM-5.3-Flash",
		ContextWindow: &window, MaxTokens: &maxTokens, SupportsImage: &images,
		Thinking: &thinkingConfig{Options: []thinkingOption{{Level: "High", OpenclawLevel: "high"}}},
	}})
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	detail := entries[0].Detail
	for _, want := range []string{"上下文 1000000", "单次输出上限 64000", "图片 是", "High"} {
		if !strings.Contains(detail, want) {
			t.Errorf("Detail = %q, want it to mention %q", detail, want)
		}
	}
	// A parameter the server never sent must not be invented.
	bare := catalogueModelEntries([]remoteModel{{ID: "kimi-k2.6", Name: "kimi-k2.6"}})
	if strings.Contains(bare[0].Detail, "单次输出上限") {
		t.Errorf("Detail = %q, want no output cap for a model that published none", bare[0].Detail)
	}
	if !strings.Contains(bare[0].Detail, "上下文未知") {
		t.Errorf("Detail = %q, want the unknown window stated as unknown", bare[0].Detail)
	}
}

// TestCatalogueCardPrefersTheCacheAndTouchesNoNetwork pins that a page load is
// served from the plugin's own cache: the fetched remote catalog when one exists,
// the bundled list otherwise. A page view must never issue an upstream call.
func TestCatalogueCardPrefersTheCacheAndTouchesNoNetwork(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	t.Cleanup(discoveredModels.reset)

	// No host is installed: any transport call would fail loudly.
	discoveredModels.reset()
	if _, source := catalogueForPage(settings()); !strings.Contains(source, "内置兜底列表") {
		t.Fatalf("source without a cache = %q, want it to say the bundled list is in use", source)
	}

	discoveredModels.put([]remoteModel{{ID: "glm-5.3"}}, time.Now())
	models, source := catalogueForPage(settings())
	if len(models) != 1 || models[0].ID != "glm-5.3" {
		t.Fatalf("catalogueForPage with a warm cache = %+v", models)
	}
	if !strings.Contains(source, "线上目录缓存") {
		t.Fatalf("source with a warm cache = %q, want the cache line", source)
	}
}

// TestCatalogueCardIsTheOnlyOneOnThePage pins that the hand-written card is
// REPLACED rather than kept alongside the shared one: two cards answering the
// same question is the inconsistency this work removes.
func TestCatalogueCardIsTheOnlyOneOnThePage(t *testing.T) {
	_, host, _ := statusFixture(t)

	body := string(renderStatusPage(host, managementRequest(
		http.MethodGet, "/v0/resource/plugins/lobsterai/status", nil, nil)).Body)
	if got := strings.Count(body, "模型目录"); got != 1 {
		t.Fatalf("the page renders %d 模型目录 cards, want exactly 1:\n%s", got, firstLines(body, 40))
	}
	if strings.Contains(body, "模型与远端参数") {
		t.Fatalf("the hand-written model card is still on the page:\n%s", firstLines(body, 40))
	}
}
