package main

import (
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestCatalogueCardListsTheModels pins the point of the card: it answers "which
// models does this channel offer", so it must carry rows. The page previously
// reported only counters (可调用模型数 / 各通道模型数), which answer "how many" and
// never "which"; the counters stay, and the list is added below them.
func TestCatalogueCardListsTheModels(t *testing.T) {
	accountHost(t, nil)
	body := string(renderStatusPage(abiboot.NewHost(nil), pluginapi.ManagementRequest{}).Body)

	if !strings.Contains(body, "模型目录") {
		t.Fatalf("the status page has no 模型目录 card:\n%s", body)
	}
	// The fixture's batch answer declares these two callable models under
	// `solo_agent_remote` (models_test.go:15-80); the counters beside the card
	// report the same figure.
	for _, want := range []string{"glm-5.1", "image-only"} {
		if !strings.Contains(body, want) {
			t.Errorf("the catalogue card does not list %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "可调用模型数</dt><dd>2</dd>") {
		t.Errorf("the model counter and the listed rows disagree:\n%s", body)
	}
	if !strings.Contains(body, "action=refresh-catalog") {
		t.Error("the catalogue card lost its 刷新目录 action")
	}
	if strings.Contains(body, "<form") {
		t.Fatal("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestCatalogueCardShowsTheUpstreamName is the naming guard.
//
// `modelInfoForRemote` sets `Name: model.ID` (models.go:606), and `model.ID` is
// the vendor's own `config_name` (`parseTraeConfigEntry`, models.go:299-301) —
// the string the chat endpoint is addressed with (`configNameFor`, models.go:848).
// This plugin renames nothing, so `ModelInfo.ID` carries the same value and the
// card must NOT grow a 请求用名 row claiming a rename that does not exist.
//
// The vendor's separate human label (`display_config.display_name`, read into
// `remoteModel.Name`, models.go:303-312) is not upstream's identifier; it is
// reported as context, never as the main label.
func TestCatalogueCardShowsTheUpstreamName(t *testing.T) {
	cfg := DefaultConfig()
	models := []remoteModel{
		{ID: "glm-5.2", Name: "GLM-5.2", Channel: "solo_agent_remote", ContextWindow: 200_000, MaxOutputTokens: 32_000},
		{ID: "DeepSeek-V4-Flash-Official", Name: "DeepSeek V4 Flash Official", Channel: "solo_agent", ContextWindow: 200_000},
	}
	infos := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, modelInfoForRemote(model, cfg))
	}

	rows := catalogueModelEntries(models, infos)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Native != "glm-5.2" {
		t.Errorf("rows[0].Native = %q, want the vendor's config_name %q", rows[0].Native, "glm-5.2")
	}
	if rows[0].ID != "glm-5.2" {
		t.Errorf("rows[0].ID = %q, want %q: this channel renames nothing", rows[0].ID, "glm-5.2")
	}

	card := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: rows}))
	if strings.Contains(card, "请求用名") {
		t.Fatalf("an unrenamed model claims a routed name:\n%s", card)
	}
	if !strings.Contains(card, `<span class="mono">glm-5.2</span>`) {
		t.Fatalf("the card does not label the row with the vendor's config_name:\n%s", card)
	}
	// The human label belongs in the detail, not in the label position.
	if strings.Contains(card, `<span class="mono">GLM-5.2</span>`) {
		t.Fatalf("the card uses the human label as the main label:\n%s", card)
	}
	if !strings.Contains(card, "上游标签 GLM-5.2") {
		t.Errorf("the card does not report the vendor's human label:\n%s", card)
	}
	if !strings.Contains(card, "通道 solo_agent_remote") {
		t.Errorf("the card does not report the channel the model was listed under:\n%s", card)
	}
}

// TestCatalogueCardReadsTheCacheWithoutCallingTheVendor pins that a page load is
// served from this plugin's own cache or the bundled table and NEVER issues an
// upstream call. `catalogFor` — the path a client request uses — would call
// `batch_get_detail_param` on a cold cache, so the card peeks instead.
//
// The credential carries a UID of its own: `catalogKey` scopes the cache by
// `region|uid` (models.go:723-725), so reusing the shared fixture's `uid-1` would
// leave a stored listing behind for whichever test runs next under that account.
func TestCatalogueCardReadsTheCacheWithoutCallingTheVendor(t *testing.T) {
	requests := []string{}
	accountHost(t, &requests)
	cfg := DefaultConfig()
	credential := &Credential{Type: ProviderKey, AccessToken: "token-catalogue-card", UID: "uid-catalogue-card"}
	invalidateCatalog(cfg, credential)
	t.Cleanup(func() { invalidateCatalog(cfg, credential) })

	if _, _, source := catalogueForPage(cfg, credential); !strings.Contains(source, "尚未拉取") {
		t.Fatalf("source without a cache = %q, want it to say the listing was never fetched", source)
	}
	if len(requests) != 0 {
		t.Fatalf("a page load issued %d upstream calls; it must be served from the cache: %#v", len(requests), requests)
	}

	storeCatalog(cfg, credential, []remoteModel{{ID: "glm-5.2", Name: "GLM-5.2", Channel: "solo_agent_remote"}})
	_, _, source := catalogueForPage(cfg, credential)
	if !strings.Contains(source, "缓存于") {
		t.Fatalf("source with a warm cache = %q, want the cache line", source)
	}
	if len(requests) != 0 {
		t.Fatalf("reading the cache issued an upstream call: %#v", requests)
	}
}

// TestCatalogueCardExcludesNothingAPublisherWouldNot pins that the card lists
// exactly what `model.for_auth` would publish — no extra exclusion or alias of
// its own. `oauth-excluded-models` and `oauth-model-alias` belong to the host.
func TestCatalogueCardExcludesNothingAPublisherWouldNot(t *testing.T) {
	cfg := DefaultConfig()
	models := []remoteModel{
		{ID: "glm-5.2", Name: "GLM-5.2", Channel: "solo_agent_remote"},
		{ID: "solo-hidden", Name: "Solo Hidden", Channel: "solo_agent_remote"},
	}
	// One entry `isModelCallable` rejects: it must be absent from BOTH lists, so
	// the card cannot advertise a model a request would be refused for.
	models = append(models, remoteModel{
		ID: "dropped", Name: "Dropped", Channel: "solo_agent_remote",
		IsEnabled: boolPtr(false),
	})

	infos := make([]pluginapi.ModelInfo, 0, len(models))
	published := make([]remoteModel, 0, len(models))
	for _, model := range models {
		if !isModelCallable(model) {
			continue
		}
		published = append(published, model)
		infos = append(infos, modelInfoForRemote(model, cfg))
	}
	rows := catalogueModelEntries(published, infos)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the disabled model must not be published)", len(rows))
	}
	card := string(plugui.CatalogueCard(plugui.ModelCatalogue{Entries: rows}))
	if strings.Contains(card, "dropped") {
		t.Fatalf("the card advertised a model upstream disabled:\n%s", card)
	}
	// The static fallback path must be listed in full as well.
	statics := staticModelInfos(cfg)
	staticRows := catalogueModelEntries(nil, statics)
	if len(staticRows) != len(statics) {
		t.Fatalf("static rows = %d, want one per published model (%d)", len(staticRows), len(statics))
	}
}

// TestCatalogueCardRendersWithoutAnAccount pins the first-run experience: the
// card must answer "which models does this channel offer" before any credential
// exists, from the bundled table, without fetching anything.
func TestCatalogueCardRendersWithoutAnAccount(t *testing.T) {
	requests := []string{}
	fakeHost(t, func(method string, _ []byte) (any, error) {
		if method == pluginabi.MethodHostAuthList {
			return map[string]any{"files": []any{}}, nil
		}
		return map[string]any{}, nil
	})
	response := renderStatusPage(abiboot.NewHost(nil), pluginapi.ManagementRequest{})
	body := string(response.Body)
	if !strings.Contains(body, "尚未添加账号") {
		t.Fatalf("the empty state is missing:\n%s", body)
	}
	if !strings.Contains(body, "模型目录") {
		t.Fatalf("the catalogue card is missing when no account is configured:\n%s", body)
	}
	if got := strings.Count(body, "<tr data-search="); got != len(staticModelInfos(settings())) {
		t.Fatalf("the card rendered %d rows, want the bundled table (%d):\n%s",
			got, len(staticModelInfos(settings())), body)
	}
	if len(requests) != 0 {
		t.Fatalf("rendering without an account issued %d upstream calls: %#v", len(requests), requests)
	}
}

// TestCatalogueCardEmptyStateSaysSo pins that a catalogue with nothing to list
// renders the notice instead of an empty table.
func TestCatalogueCardEmptyStateSaysSo(t *testing.T) {
	card := plugui.CatalogueCard(plugui.ModelCatalogue{
		Source:      "内置兜底列表",
		EmptyNotice: "暂无模型：线上目录尚未拉取，且内置兜底列表为空。",
	})
	if strings.Contains(string(card), "<table") {
		t.Fatalf("an empty catalogue rendered a table:\n%s", card)
	}
	if !strings.Contains(string(card), "暂无模型") {
		t.Fatalf("an empty catalogue rendered no notice:\n%s", card)
	}
}
