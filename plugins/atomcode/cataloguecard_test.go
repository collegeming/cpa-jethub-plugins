package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The shared 模型目录 card exists to answer "which models does this channel
// offer?", and it must answer with the PROVIDER's own model names. This plugin
// renames three gateway ids onto this deployment's vocabulary
// (`canonicalModelNames`: `glm5.3-flash` → `GLM-5.3-Flash`), and the rename is a
// ROUTING name: the gateway answers to `glm5.3-flash`, which is what
// `models-v2` returns as `display_model_name`. The card must therefore display
// `glm5.3-flash` and offer `GLM-5.3-Flash` beside it as 「请求用名」.
//
// The upstream spelling is taken from a real captured payload shape
// (`browseHost`): `display_model_name`. See models.go:44-48 for the field.

// TestModelInfoNameIsTheGatewayNativeId pins the FIELD the page and any other
// reader of `ModelInfo.Name` receives.
//
// `Name` is the provider-native name. Publishing the canonical name there made
// `Name` a copy of the rename, so nothing downstream — the card included — could
// tell what the gateway actually calls the model.
func TestModelInfoNameIsTheGatewayNativeId(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, codingPlanModelsPath) {
			return httpResponse(200, `[{"display_model_name":"glm5.3-flash","context_window":512000,`+
				`"supports_vision":true,"plan_available":true,"reasoning_effort_levels":["low","high"]}]`), nil
		}
		if strings.Contains(request.URL, codingPlanStatusPath) {
			return httpResponse(200, `{"codingplan_free":{"plan_name":"CodingPlan Lite-体验版"}}`), nil
		}
		return httpResponse(404, `{"message":"unexpected"}`), nil
	}
	host.install(t)

	cfg := settings()
	infos := modelInfos(staticModelEntries(testHost(), cfg, sampleCredential(7*24*3600)), cfg, nil)
	if len(infos) != 1 {
		t.Fatalf("model count = %d, want 1", len(infos))
	}
	info := infos[0]
	// ID is what requests route by: the canonical name this deployment publishes.
	if info.ID != "GLM-5.3-Flash" {
		t.Fatalf("routing id = %q, want the canonical GLM-5.3-Flash", info.ID)
	}
	// Name is what the GATEWAY calls it, and it is not the rename.
	if info.Name != "glm5.3-flash" {
		t.Fatalf("Name = %q, want the gateway's own id glm5.3-flash (not the canonical rename)", info.Name)
	}
	if strings.EqualFold(info.Name, info.ID) {
		t.Fatalf("Name (%q) duplicates ID (%q), so Native cannot be distinguished from the routed name",
			info.Name, info.ID)
	}
}

// TestCatalogueCardShowsTheGatewayNameNotTheRename is the card-level assertion:
// the row's Native must be the upstream id, and the canonical rename must appear
// only as the routed name.
func TestCatalogueCardShowsTheGatewayNameNotTheRename(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, codingPlanModelsPath) {
			return httpResponse(200, `[{"display_model_name":"glm5.3-flash","context_window":512000,`+
				`"supports_vision":true,"plan_available":true,"reasoning_effort_levels":["low","high"]},`+
				`{"display_model_name":"qwen3.8-27b","context_window":262144,"plan_available":true}]`), nil
		}
		if strings.Contains(request.URL, codingPlanStatusPath) {
			return httpResponse(200, `{"codingplan_free":{"plan_name":"CodingPlan Lite-体验版"}}`), nil
		}
		return httpResponse(404, `{"message":"unexpected"}`), nil
	}
	host.install(t)
	// The page reads the credential from the host, so the account has to exist
	// there; the SAME credential seeds the cache, so both sides address one key.
	credential := sampleCredential(7 * 24 * 3600)
	host.auths["idx-1"] = mustJSON(t, credential)
	host.files = []pluginapi.HostAuthFileEntry{
		{ID: "atomcode-qq.json", AuthIndex: "idx-1", Name: "atomcode-qq.json",
			Provider: ProviderKey, Type: ProviderKey, Label: "黎明文铮"},
	}
	// Seed the cache, then render the page from the CACHE (the page never fetches).
	if len(staticModelEntries(testHost(), settings(), credential)) == 0 {
		t.Fatal("the catalogue is empty, so the card would prove nothing")
	}
	fetchesBefore := len(host.callsFor(codingPlanModelsPath))

	page := bodyOf(renderStatusPage(testHost(), pluginapi.ManagementRequest{
		Path:    "/v0/resource/plugins/atomcode/status",
		Query:   url.Values{},
		Headers: map[string][]string{"Accept": {"text/html"}},
	}))

	// The gateway's own spelling is the label.
	if !strings.Contains(page, "glm5.3-flash") {
		t.Errorf("the card does not list the gateway's own id glm5.3-flash:\n%s", truncateBody(page, 2000))
	}
	if !strings.Contains(page, "qwen3.8-27b") {
		t.Errorf("the card does not list the gateway's own id qwen3.8-27b:\n%s", truncateBody(page, 2000))
	}
	// The canonical rename is still reachable, and marked as the REQUEST name
	// rather than presented as the provider's name.
	if !strings.Contains(page, "GLM-5.3-Flash") {
		t.Errorf("the card hides the routing name GLM-5.3-Flash:\n%s", truncateBody(page, 2000))
	}
	if !strings.Contains(page, "请求用名") {
		t.Errorf("the rename is not marked as a routing name:\n%s", truncateBody(page, 2000))
	}
	// The per-model metadata this card has always published must survive the move
	// to the shared component.
	for _, want := range []string{"512000 ctx", "思考级别 low/high", "支持图片", "262144 ctx"} {
		if !strings.Contains(page, want) {
			t.Errorf("the model detail %q was lost:\n%s", want, truncateBody(page, 2000))
		}
	}
	// Rendering the page is not a reason to call the vendor.
	if after := len(host.callsFor(codingPlanModelsPath)); after != fetchesBefore {
		t.Errorf("rendering the status page issued %d models-v2 calls, want 0", after-fetchesBefore)
	}
}

// TestCatalogueCardWithNoCacheStatesTheSnapshot pins the empty/degraded path: no
// fetch, no invented rows, and a stated source rather than a blank card.
func TestCatalogueCardWithNoCacheStatesTheSnapshot(t *testing.T) {
	host := newFakeHost()
	credential := sampleCredential(7 * 24 * 3600)
	host.auths["idx-1"] = mustJSON(t, credential)
	host.files = []pluginapi.HostAuthFileEntry{
		{ID: "atomcode-qq.json", AuthIndex: "idx-1", Name: "atomcode-qq.json",
			Provider: ProviderKey, Type: ProviderKey, Label: "黎明文铮"},
	}
	// A nil do fails the test on any outbound call: this path must not fetch.
	host.install(t)

	cfg := settings()
	entries, source := catalogueForPage(cfg, atomcodeSampleCredential())
	if len(entries) == 0 {
		t.Fatal("the bundled snapshot is empty, so the card would render nothing at all")
	}
	if !strings.Contains(source, "内置快照") {
		t.Errorf("source = %q, want it to state that the bundled snapshot is on screen", source)
	}
	if !strings.Contains(source, "尚未拉取") {
		t.Errorf("source = %q, want it to admit the remote catalogue was never fetched", source)
	}
	// The snapshot's own rows still carry the gateway spelling, so the card is
	// never blank and never mislabelled.
	rows := catalogueModelEntries(entries, cfg, nil)
	if len(rows) == 0 || rows[0].Native != "qwen3.8-27b" {
		t.Fatalf("first row = %+v, want the snapshot's gateway id qwen3.8-27b", rows)
	}
	// The card renders those rows.
	card := string(plugui.CatalogueCard(plugui.ModelCatalogue{
		Source:  source,
		Entries: rows,
	}))
	if !strings.Contains(card, "qwen3.8-27b") || !strings.Contains(card, "Qwen3.8-27B") {
		t.Errorf("the card does not carry both spellings:\n%s", card)
	}
}

// TestCatalogueCardRoutingNameMatchesWhatTheHostRegisters pins that the row's
// routing name is the one requests must carry. With `model_prefix` on (the
// default) the host registers `<account>/<model>`, so a card showing the bare
// canonical name would name a request the router does not recognise.
func TestCatalogueCardRoutingNameMatchesWhatTheHostRegisters(t *testing.T) {
	entries := []modelEntry{{DisplayModelName: "glm5.3-flash", PlanAvailable: true}}

	cfg := settings()
	credential := sampleCredential(7 * 24 * 3600)
	rows := catalogueModelEntries(entries, cfg, credential)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// Native stays the gateway's own spelling, unprefixed.
	if rows[0].Native != "glm5.3-flash" {
		t.Fatalf("Native = %q, want the gateway id glm5.3-flash", rows[0].Native)
	}
	// The routing name is whatever `modelInfos` published, so the page and the
	// host registry cannot disagree.
	published := modelInfos(entries, cfg, credential)
	if rows[0].ID != published[0].ID {
		t.Fatalf("card routing name = %q, host registered %q", rows[0].ID, published[0].ID)
	}
	if cfg.ModelPrefix {
		if !strings.HasPrefix(rows[0].ID, modelPrefixFor(credential)) {
			t.Fatalf("routing name %q does not carry the account prefix %q",
				rows[0].ID, modelPrefixFor(credential))
		}
	}
	// And the canonical rename is what the prefix is attached to.
	if !strings.HasSuffix(rows[0].ID, "GLM-5.3-Flash") {
		t.Fatalf("routing name %q does not end in the canonical GLM-5.3-Flash", rows[0].ID)
	}
}

// TestCatalogueCardRendersWithoutAnAccount pins the no-account page: the model
// list must still be there (the bundled snapshot is what this channel would
// serve), and building it must not dereference the missing credential.
func TestCatalogueCardRendersWithoutAnAccount(t *testing.T) {
	host := newFakeHost()
	// A nil do fails the test on any outbound call: this path must not fetch.
	host.install(t)
	withSettings(t, DefaultConfig())

	page := bodyOf(renderStatusPage(testHost(), pluginapi.ManagementRequest{
		Path:  "/v0/resource/plugins/atomcode/status",
		Query: url.Values{},
	}))
	for _, want := range []string{"模型目录", "qwen3.8-27b", "glm5.3-flash", "尚未登录"} {
		if !strings.Contains(page, want) {
			t.Errorf("the no-account page is missing %q:\n%s", want, truncateBody(page, 1500))
		}
	}
	// The snapshot is what is on screen, and the card says so rather than
	// implying a live fetch happened.
	if !strings.Contains(page, "内置快照") {
		t.Errorf("the no-account card does not name the bundled snapshot as its source:\n%s",
			truncateBody(page, 1500))
	}
}

// TestAccountIDToleratesANilCredential guards the helper the no-account card path
// depends on. `AccountID()` is called to key the catalogue cache, and the status
// page renders that card before any credential exists.
func TestAccountIDToleratesANilCredential(t *testing.T) {
	var credential *Credential
	if got := credential.AccountID(); got != "" {
		t.Fatalf("AccountID() = %q, want empty for a nil credential", got)
	}
	if got := modelPrefixFor(credential); got != "" {
		t.Fatalf("modelPrefixFor(nil) = %q, want empty", got)
	}
}

// truncateBody keeps a failure message readable.
func truncateBody(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
