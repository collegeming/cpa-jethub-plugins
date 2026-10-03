package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The shared 模型目录 card answers "which models does this channel offer?", so it
// must list the PROVIDER's own names. For ZCode those come from
// `GET /api/v1/client/configs`:
//
//	builtinModels[].modelId  → the id the wire carries (parseBuiltinModels, models.go:345)
//	builtinModels[].name     → the provider's own model name (models.go:351)
//
// `parseBuiltinModels` keeps the two apart, and `fallbackModel` carries both, so
// the card's Native can be the provider's name rather than a copy of the id.
// Measured against the live endpoint: `modelId` and `name` are identical for both
// exposed entries (`GLM-5.3`/`GLM-5.3`, `GLM-5.3-Flash`/`GLM-5.3-Flash`), which
// is why the two fields agree on a healthy catalogue and are still kept separate.

// TestCatalogueModelEntriesUseTheProviderName pins the mapping from the served
// catalogue onto the card's rows.
func TestCatalogueModelEntriesUseTheProviderName(t *testing.T) {
	// An entry whose provider name differs from its routed id is the case that
	// distinguishes the two fields; a fixture where they coincide could not.
	catalogue := []fallbackModel{{
		ID:              "GLM-5.3-Flash",
		Name:            "glm-5.3-flash",
		ContextWindow:   1_000_000,
		MaxOutputTokens: 128_000,
		SupportsImage:   true,
		ReasoningLevels: []string{"low", "high", "max"},
	}}
	rows := catalogueModelEntries(catalogue, "")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Native != "glm-5.3-flash" {
		t.Fatalf("Native = %q, want the provider's own name glm-5.3-flash", rows[0].Native)
	}
	if rows[0].ID != "GLM-5.3-Flash" {
		t.Fatalf("ID = %q, want the routed id GLM-5.3-Flash", rows[0].ID)
	}
	// The metadata this card always published must survive. Magnitudes are
	// rendered in their abbreviated form (`formatTokenMagnitude`).
	for _, want := range []string{"1M", "128K", "low / high / max", "支持图片"} {
		if !strings.Contains(rows[0].Detail, want) {
			t.Errorf("detail %q is missing %q", rows[0].Detail, want)
		}
	}
}

// TestCatalogueModelEntriesCarryTheAccountPrefix pins that the routing name the
// card shows is the one a request must actually carry. With `model_prefix` on (the
// default) the host registers `<account>/<model>`, so a card showing the bare id
// would name a request the router does not recognise.
func TestCatalogueModelEntriesCarryTheAccountPrefix(t *testing.T) {
	catalogue := []fallbackModel{{ID: "GLM-5.3", Name: "GLM-5.3"}}
	rows := catalogueModelEntries(catalogue, "user-123")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].ID != "user-123/GLM-5.3" {
		t.Fatalf("ID = %q, want the prefixed routing id user-123/GLM-5.3", rows[0].ID)
	}
	// The provider's name stays unprefixed: the prefix is this deployment's, not
	// something the vendor names.
	if rows[0].Native != "GLM-5.3" {
		t.Fatalf("Native = %q, want the bare provider name GLM-5.3", rows[0].Native)
	}
}

// TestCatalogueModelEntriesFallBackToTheID guards the degenerate entry: a blank
// Native is DROPPED by the shared card, so a missing provider name must not
// silently delete a model from the list.
func TestCatalogueModelEntriesFallBackToTheID(t *testing.T) {
	rows := catalogueModelEntries([]fallbackModel{{ID: "GLM-5.3"}}, "")
	if len(rows) != 1 || rows[0].Native != "GLM-5.3" {
		t.Fatalf("rows = %+v, want one row labelled with the id", rows)
	}
	if rows[0].ID != "GLM-5.3" {
		t.Fatalf("ID = %q, want GLM-5.3", rows[0].ID)
	}
}

// TestCatalogueCardListsTheProviderNames covers the rendered page: the card must
// be present with the catalogue's rows, the 刷新目录 button, and the source line.
func TestCatalogueCardListsTheProviderNames(t *testing.T) {
	fake := catalogueHost(t, func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, ClientConfigsPath) {
			// The real upstream payload shape, including the reasoning ladder —
			// which is what the detail line is supposed to show.
			return httpResponse(200, `{"code":0,"data":{"builtinModels":{`+
				`"0":{"modelId":"GLM-5.3-Flash","name":"GLM-5.3-Flash","contextWindow":1000000,`+
				`"maxCompletionTokens":128000,"capabilities":{"vision":true},`+
				`"reasoning":{"levels":{"low":{},"high":{},"max":{}},"defaultLevel":"max"}},`+
				`"1":{"modelId":"GLM-5.3","name":"GLM-5.3","contextWindow":1000000,`+
				`"maxCompletionTokens":128000,"capabilities":{},`+
				`"reasoning":{"levels":{"low":{},"high":{},"max":{}},"defaultLevel":"max"}}}}}`), nil
		}
		return httpResponse(200, `{"code":0}`), nil
	})
	// Populate the cache through the plugin's own refresh path, so the page has a
	// fetched catalogue to list rather than the built-in table.
	if _, errRefresh := catalogueRefresh(testHost(), settings()); errRefresh != nil {
		t.Fatalf("refresh: %v", errRefresh)
	}
	fetchesBefore := len(fake.callsFor(ClientConfigsPath))

	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}, nil))
	page := string(response.Body)

	// The card itself, listing both models by their own names.
	if !strings.Contains(page, "模型目录") {
		t.Fatalf("the status page has no 模型目录 card:\n%s", page)
	}
	for _, id := range []string{"GLM-5.3-Flash", "GLM-5.3"} {
		if !strings.Contains(page, id) {
			t.Errorf("the card does not list %s", id)
		}
	}
	// The refresh affordance and the background-refresh field are preserved.
	if !strings.Contains(page, "刷新目录") || !strings.Contains(page, "action=refresh-catalog") {
		t.Error("the card does not carry the 刷新目录 button")
	}
	if !strings.Contains(page, "后台自动刷新") {
		t.Error("the page no longer reports the background refresh state")
	}
	// The explanation the old hand-written card carried is kept, so the two
	// hidden upstream models stay explained.
	if !strings.Contains(page, "GLM-5-Turbo") || !strings.Contains(page, "返回空响应") {
		t.Error("the page does not explain why two upstream models are hidden")
	}
	if !strings.Contains(page, "合并") {
		t.Error("the page does not explain the same-name model merge")
	}
	// The metadata the old card spelled out is still there.
	if !strings.Contains(page, "low") || !strings.Contains(page, "max") {
		t.Error("the card does not show the reasoning levels")
	}
	// A page view is not a reason to call the vendor.
	if after := len(fake.callsFor(ClientConfigsPath)); after != fetchesBefore {
		t.Errorf("rendering the status page issued %d client/configs calls, want 0", after-fetchesBefore)
	}
}

// TestCatalogueCardFallsBackToTheBuiltInTable pins the never-fetched case: the
// card lists the built-in table and SAYS so, rather than rendering nothing or
// pretending the remote catalogue answered.
func TestCatalogueCardFallsBackToTheBuiltInTable(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"code":0}`), nil
	}
	withSettings(t, DefaultConfig())

	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}, nil))
	page := string(response.Body)
	if !strings.Contains(page, "内置表") {
		t.Errorf("the card does not say the built-in table is on screen:\n%s", page)
	}
	if !strings.Contains(page, "尚未拉取") {
		t.Errorf("the card does not admit the remote catalogue was never fetched:\n%s", page)
	}
	for _, id := range []string{"GLM-5.3-Flash", "GLM-5.3"} {
		if !strings.Contains(page, id) {
			t.Errorf("the built-in table's %s is missing from the card", id)
		}
	}
	if len(fake.callsFor(ClientConfigsPath)) != 0 {
		t.Error("rendering the page with no cache issued an upstream fetch")
	}
}

// TestCatalogueCardRendersWithoutAnAccount pins the no-account page: the model
// list must still be there (the built-in table is what this channel would serve),
// and building it must not fetch.
func TestCatalogueCardRendersWithoutAnAccount(t *testing.T) {
	fake := newFakeHost()
	// A nil do fails the test on any outbound call: this path must not fetch.
	fake.install(t)
	withSettings(t, DefaultConfig())

	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}, nil))
	page := string(response.Body)
	for _, want := range []string{"模型目录", "GLM-5.3-Flash", "GLM-5.3", "尚未添加账号"} {
		if !strings.Contains(page, want) {
			t.Errorf("the no-account page is missing %q:\n%s", want, truncateText(page, 1500))
		}
	}
	if len(fake.callsFor(ClientConfigsPath)) != 0 {
		t.Error("the no-account page issued an upstream fetch")
	}
}

// truncateText keeps a failure message readable.
func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// TestCatalogueCardRendersUpstreamNamesVerbatim is the escape-hatch check: the
// provider's name is upstream data and must be escaped, not trusted as markup.
func TestCatalogueCardRendersUpstreamNamesVerbatim(t *testing.T) {
	card := string(plugui.CatalogueCard(plugui.ModelCatalogue{
		Source:  "线上目录",
		Entries: catalogueModelEntries([]fallbackModel{{ID: "a", Name: `<img src=x onerror=alert(1)>`}}, ""),
	}))
	if strings.Contains(card, "<img src=x") {
		t.Errorf("an upstream model name was rendered as markup:\n%s", card)
	}
	if !strings.Contains(card, "&lt;img") {
		t.Errorf("the upstream name was not escaped:\n%s", card)
	}
}
