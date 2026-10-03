package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestCatalogueCardListsTheModels pins the point of the card: it answers "which
// models does this channel offer", so it must actually carry rows. A card that
// only reports provenance is the defect this pins.
func TestCatalogueCardListsTheModels(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	discoveredModels.reset()
	t.Cleanup(discoveredModels.reset)

	body := string(renderCatalogueCard(DefaultConfig()))
	if !strings.Contains(body, "模型目录") {
		t.Fatalf("the status page has no 模型目录 card:\n%s", body)
	}
	for _, want := range []string{"GLM-5.2", "deepseek-v4-flash", "deepseek-v4-pro"} {
		if !strings.Contains(body, want) {
			t.Errorf("the catalogue card does not list %q:\n%s", want, body)
		}
	}
	// The refresh control survives the rewrite, and stays a GET link.
	if !strings.Contains(body, "action=refresh-catalog") {
		t.Error("the catalogue card lost its 刷新目录 action")
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestCatalogueCardShowsTheUpstreamNameNotTheRenamedID is the naming guard.
//
// `publicModelNames` (models.go:128) renames two CodeArts-native ids so this
// deployment publishes a canonical spelling: `glm-5.3-flash` → `GLM-5.3-Flash`
// and `deepseek-v4.1-flash` → `DeepSeek-V4.1-Flash`. Those results are the
// ROUTING ids, not the vendor's own names, so the card must label the row with
// the native spelling and report the renamed one as 「请求用名」 — never the other
// way round.
//
// The evidence for which field holds which name is `modelInfoFor`
// (models.go:184,189,195): `ID: publicModelID(id)` and `Name: id`, so `Name` is the
// native id as the vendor and the discovery endpoint spell it
// (`recordModel` normalises the raw `model_id` and keeps the raw `model_name`,
// models.go:366-381).
func TestCatalogueCardShowsTheUpstreamNameNotTheRenamedID(t *testing.T) {
	entries := catalogueModelEntries([]pluginapi.ModelInfo{
		modelInfoFor(BenefitModel, BenefitModel),
	})
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	native, routed := BenefitModel, publicModelID(BenefitModel)
	if entry.Native != native {
		t.Fatalf("Native = %q, want the upstream name %q", entry.Native, native)
	}
	if entry.ID != routed {
		t.Fatalf("ID = %q, want the routing id %q", entry.ID, routed)
	}

	body := string(renderCatalogueCard(DefaultConfig()))
	// The two spellings must both be present, and only the native one may be
	// the label. The card renders `请求用名 <routed>` in its own span, so the
	// routing id appears after that marker.
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
	entries := catalogueModelEntries([]pluginapi.ModelInfo{
		modelInfoFor("deepseek-v4-pro", "deepseek-v4-pro"),
	})
	if len(entries) != 1 || entries[0].Native != entries[0].ID {
		t.Fatalf("entries = %+v, want one row whose two names agree", entries)
	}
	card := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: entries}))
	if strings.Contains(card, "请求用名") {
		t.Fatalf("an unrenamed model claims a routed name:\n%s", card)
	}
}

// TestCatalogueCardPrefersTheCacheAndTouchesNoNetwork pins that a page load is
// served from the plugin's own cache: the remote listing when one was fetched,
// the built-in table otherwise. A page view must never issue an upstream call.
//
// The host transport is installed and records every non-listing call, so "no
// fetch happened" is an observation rather than an assumption drawn from the
// absence of a host.
func TestCatalogueCardPrefersTheCacheAndTouchesNoNetwork(t *testing.T) {
	previous := settings()
	t.Cleanup(func() { setSettings(previous) })
	t.Cleanup(discoveredModels.reset)

	dialed := 0
	abiboot.SetHostCaller(func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return abiboot.OK(map[string]any{"files": []pluginapi.HostAuthFileEntry{}})
		}
		dialed++
		return nil, fmt.Errorf("the page load reached the host: %s", method)
	})
	t.Cleanup(abiboot.ClearHostCaller)
	host := abiboot.NewHost(json.RawMessage(`{"host_callback_id":"catalogue-card"}`))

	discoveredModels.reset()
	if _, source := catalogueForPage(DefaultConfig()); !strings.Contains(source, "尚未拉取") {
		t.Fatalf("source without a cache = %q, want it to say the listing was never fetched", source)
	}
	// The whole status page, not just the card helper: a page load is the thing
	// that must stay offline.
	renderStatusPage(host, pluginapi.ManagementRequest{Query: map[string][]string{}})
	if dialed != 0 {
		t.Fatalf("the status page reached a data endpoint %d times while rendering the catalogue", dialed)
	}

	discoveredModels.put([]pluginapi.ModelInfo{modelInfoFor("glm-5.3-flash", "glm-5.3-flash")})
	models, source := catalogueForPage(DefaultConfig())
	if len(models) != 1 || models[0].ID != "GLM-5.3-Flash" {
		t.Fatalf("catalogueForPage with a warm cache = %+v", models)
	}
	if !strings.Contains(source, "线上目录缓存") {
		t.Fatalf("source with a warm cache = %q, want the cache line", source)
	}
}

// TestCatalogueCardReportsEveryModelWithoutFiltering pins that the card applies
// no exclusion or alias of its own: `oauth-excluded-models` and
// `oauth-model-alias` belong to the host, and rows must not vanish because a
// second mechanism was reimplemented here.
func TestCatalogueCardReportsEveryModelWithoutFiltering(t *testing.T) {
	statics := staticModelInfos()
	entries := catalogueModelEntries(statics)
	if len(entries) != len(statics) {
		t.Fatalf("entries = %d, want one per published model (%d)", len(entries), len(statics))
	}
	// The card de-duplicates by routing id only, so the count must not shrink
	// for any pair of models that merely share a label.
	body := string(renderCatalogueCard(DefaultConfig()))
	count := strings.Count(body, `<tr data-search=`)
	if count != len(statics) {
		t.Fatalf("the card rendered %d rows for %d published models:\n%s", count, len(statics), body)
	}
}
