package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Catalogue rules. Four of them are traps in the reference: `name` is the id,
// `visible:false` is filtered, entries dedupe by id first-wins, and a non-positive
// `max_tokens` must never be advertised.

// TestFallbackDisplayNamesMatchReference pins the bundled table's rendered names
// VERBATIM against `raccoon-product.ts:99-144`. The price lives in the name and
// `x1` is shown on purpose (trap #8).
func TestFallbackDisplayNamesMatchReference(t *testing.T) {
	expected := []struct {
		id   string
		name string
	}{
		{"sn-sensenova-6-8-flash", "SenseNova-6.8-Flash · 免费"},
		{"sn-sensenova-6-8-flash-lite", "SenseNova-6.8-Flash-Lite · 免费"},
		{"sn-glm-5-3", "GLM-5-3 · x0.75"},
		{"sn-kimi-k3", "Kimi-K3 · x1"},
		{"sn-glm-5-3-flash", "GLM-5-3-Flash · x0.2→x0.1"},
		{"sn-deepseek-v4-1-flash", "DeepSeek-V4.1-Flash · x0.25"},
	}
	entries := fallbackModels()
	if len(entries) != len(expected) {
		t.Fatalf("fallback table has %d entries, want %d", len(entries), len(expected))
	}
	for index, want := range expected {
		if entries[index].ID != want.id {
			t.Errorf("entry %d id = %q, want %q (order must not be re-sorted)", index, entries[index].ID, want.id)
		}
		if got := entries[index].displayName(); got != want.name {
			t.Errorf("entry %d display name = %q, want %q", index, got, want.name)
		}
		// The ID is the wire id, verbatim: it is what a request must carry.
		if entries[index].ID != entries[index].info(time.Now()).ID {
			t.Errorf("entry %d publishes a different id than it sends upstream", index)
		}
		// `info()` publishes the priced name in BOTH name fields, so any client
		// that reads either one shows the multiplier.
		info := entries[index].info(time.Now())
		if info.Name != want.name || info.DisplayName != want.name {
			t.Errorf("entry %d info names = %q / %q, want %q", index, info.Name, info.DisplayName, want.name)
		}
		if strings.Contains(info.Description, "x") || strings.Contains(info.Description, "免费") {
			t.Errorf("entry %d description carries the price: %q", index, info.Description)
		}
	}
}

// TestDisplayNameMultiplierFormatting pins the suffix rules
// (`raccoonDisplayName`, `raccoon.ts:219-262`).
func TestDisplayNameMultiplierFormatting(t *testing.T) {
	cases := []struct {
		description string
		effective   *float64
		base        *float64
		want        string
	}{
		{description: "Kimi-K3", effective: numberRef(1), base: numberRef(1), want: "Kimi-K3 · x1"},
		{description: "GLM-5-3", effective: numberRef(0.75), base: numberRef(0.75), want: "GLM-5-3 · x0.75"},
		{description: "Flash", effective: numberRef(0.1), base: numberRef(0.2), want: "Flash · x0.2→x0.1"},
		{description: "Free", effective: numberRef(0), base: numberRef(0.5), want: "Free · 免费"},
		{description: "Plain", effective: nil, base: nil, want: "Plain"},
		{description: "Negative", effective: numberRef(-1), base: nil, want: "Negative"},
		{description: "Rounded", effective: numberRef(0.123456), base: nil, want: "Rounded · x0.1235"},
		{description: "BaseOnly", effective: numberRef(0.5), base: numberRef(0.5), want: "BaseOnly · x0.5"},
		{description: "", effective: nil, base: nil, want: "sn-x"},
	}
	for _, testCase := range cases {
		entry := catalogueEntry{ID: "sn-x", Description: testCase.description, EffectiveMultiplier: testCase.effective, BaseMultiplier: testCase.base}
		if got := entry.displayName(); got != testCase.want {
			t.Errorf("display name = %q, want %q", got, testCase.want)
		}
	}
	// No multiplier at all must not leave a dangling separator.
	if got := (catalogueEntry{ID: "sn-y"}).displayName(); got != "sn-y" {
		t.Errorf("display name = %q, want the bare id", got)
	}
}

// TestParseCatalogueRules walks the whole per-model mapping at once.
func TestParseCatalogueRules(t *testing.T) {
	body := `{"code":0,"data":{"categories":[
		{"type":"image","models":[{"name":"image-model"}]},
		{"type":"chat","models":[
			{"name":" sn-glm-5-3 ","description":"GLM-5-3","visible":true,
			 "params":{"context_window":1000000,"max_tokens":100000},
			 "tags":["vision","fast"],
			 "billing_effective_multiplier":0.75,"billing_multiplier":0.75,
			 "billing_status":"discount","billing_status_note":"promo"},
			{"name":"dup","description":"first","params":{"max_tokens":10}},
			{"name":"dup","description":"second","params":{"max_tokens":20}},
			{"name":"hidden","visible":false},
			{"name":"","description":"no id"},
			{"name":"weird","params":{"max_tokens":0,"context_window":-3},"tags":["image-understanding"],"billing_status":"bogus"},
			{"name":"ctx-top","context_window":5000,"max_tokens":7},
			{"name":"ctx-float","params":{"context_window":12.5,"max_tokens":"9"}}
		]}
	]}}`
	entries := parseCatalogue([]byte(body))
	byID := map[string]catalogueEntry{}
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	if len(entries) != 5 {
		t.Fatalf("parsed %d entries (%v), want 5: the hidden and id-less models must be dropped", len(entries), byID)
	}
	if _, present := byID["image-model"]; present {
		t.Error("a non-chat category must be ignored")
	}
	first := byID["sn-glm-5-3"]
	if first.Description != "GLM-5-3" || first.ContextWindow != 1_000_000 || first.MaxTokens != 100_000 {
		t.Errorf("first entry = %+v", first)
	}
	if !first.SupportsImage {
		t.Error("a `vision` tag must mark the model as image-capable")
	}
	if first.EffectiveMultiplier == nil || *first.EffectiveMultiplier != 0.75 ||
		first.BaseMultiplier == nil || *first.BaseMultiplier != 0.75 || first.Status != "discount" || first.StatusNote != "promo" {
		t.Errorf("billing fields = %+v", first)
	}
	// ⚠️ `name` is the id; the description is the label. Swapping them sends a
	// model that does not exist (trap #6).
	if first.ID != "sn-glm-5-3" {
		t.Errorf("id = %q, want the trimmed wire name", first.ID)
	}
	dup := byID["dup"]
	if dup.Description != "first" || dup.MaxTokens != 10 {
		t.Errorf("duplicate resolution = %+v, want the FIRST occurrence", dup)
	}
	weird := byID["weird"]
	if weird.MaxTokens != 0 || weird.ContextWindow != 0 {
		t.Errorf("non-positive numbers must be dropped, got %+v", weird)
	}
	if !weird.SupportsImage {
		t.Error("an `image-understanding` tag must mark the model as image-capable")
	}
	if weird.Status != "normal" {
		t.Errorf("status = %q, want normal for an unknown billing_status", weird.Status)
	}
	if got := weird.displayName(); got != "weird" {
		t.Errorf("display name = %q, want no separator when the multiplier is unknown", got)
	}
	ctxTop := byID["ctx-top"]
	if ctxTop.ContextWindow != 5000 || ctxTop.MaxTokens != 7 {
		t.Errorf("top-level numeric fields = %+v", ctxTop)
	}
	if got := byID["ctx-float"].MaxTokens; got != 9 {
		t.Errorf("numeric-string max_tokens = %d, want 9", got)
	}
	// A fractional context window is not a safe integer and must be dropped.
	if got := byID["ctx-float"].ContextWindow; got != 0 {
		t.Errorf("fractional context_window = %d, want it dropped", got)
	}
}

// TestParseCatalogueFailureModes pins "empty, never an error".
func TestParseCatalogueFailureModes(t *testing.T) {
	for _, body := range []string{
		``,
		`not json`,
		`[]`,
		`{"code":100002,"data":{"categories":[{"type":"chat","models":[{"name":"m"}]}]}}`,
		`{"code":0}`,
		`{"code":0,"data":{"categories":[]}}`,
		`{"code":0,"data":{"categories":[{"type":"image","models":[{"name":"m"}]}]}}`,
		`{"code":0,"data":{"categories":[{"type":"chat"}]}}`,
	} {
		if entries := parseCatalogue([]byte(body)); len(entries) != 0 {
			t.Errorf("parseCatalogue(%q) = %v, want empty", body, entries)
		}
	}
}

// TestMaxTokensIsNeverNonPositive pins trap #9: DSH aborts the whole turn with
// INVALID_MODEL_MAX_TOKENS, so a remote 0/negative/absent value must be replaced
// by the advertised default instead of passed through.
func TestMaxTokensIsNeverNonPositive(t *testing.T) {
	for _, maxTokens := range []int64{0, -1, -100} {
		info := catalogueEntry{ID: "sn-x", MaxTokens: maxTokens}.info(time.Now())
		if info.MaxCompletionTokens != DefaultMaxOutputTokens || info.OutputTokenLimit != DefaultMaxOutputTokens {
			t.Errorf("maxTokens=%d produced %d/%d, want the default %d",
				maxTokens, info.MaxCompletionTokens, info.OutputTokenLimit, DefaultMaxOutputTokens)
		}
	}
	positive := catalogueEntry{ID: "sn-x", MaxTokens: 12_345}.info(time.Now())
	if positive.MaxCompletionTokens != 12_345 {
		t.Errorf("a positive max_tokens must be preserved, got %d", positive.MaxCompletionTokens)
	}
}

// TestModelHandlersFallBackInsteadOfFailing pins trap #10: a catalogue failure
// must yield the bundled table, never an error.
func TestModelHandlersFallBackInsteadOfFailing(t *testing.T) {
	fake := newFakeHost()
	credential := sampleCredential(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusInternalServerError, `gateway exploded`), nil
	}
	fake.install(t)
	withSettings(t, DefaultConfig())

	value, errForAuth := handleModelForAuth(testHost(), authModelPayload(t, credential))
	if errForAuth != nil {
		t.Fatalf("model.for_auth must not fail on a catalogue error: %v", errForAuth)
	}
	response := value.(pluginapi.ModelResponse)
	if len(response.Models) != len(fallbackCatalogue) {
		t.Fatalf("model.for_auth returned %d models, want the %d bundled ones", len(response.Models), len(fallbackCatalogue))
	}
	// The model ids are the wire ids, which is what a request must carry.
	if response.Models[0].ID != "sn-sensenova-6-8-flash" {
		t.Errorf("first model id = %q", response.Models[0].ID)
	}

	// A credential that does not parse must behave the same way.
	raw, errMarshal := json.Marshal(pluginapi.AuthModelRequest{AuthID: "x", StorageJSON: []byte(`{}`)})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	value, errBad := handleModelForAuth(testHost(), raw)
	if errBad != nil {
		t.Fatalf("model.for_auth with an unparsable credential: %v", errBad)
	}
	if len(value.(pluginapi.ModelResponse).Models) != len(fallbackCatalogue) {
		t.Error("an unparsable credential must still publish the bundled catalogue")
	}
}

// TestModelStaticHidesTheProviderWithoutAnAccount pins `listModels()`: no
// credential means an empty list, and it must never be an error.
func TestModelStaticHidesTheProviderWithoutAnAccount(t *testing.T) {
	empty := newFakeHost()
	empty.install(t)
	value, errStatic := handleModelStatic(testHost(), nil)
	if errStatic != nil {
		t.Fatalf("model.static must not fail: %v", errStatic)
	}
	if models := value.(pluginapi.ModelResponse).Models; len(models) != 0 {
		t.Fatalf("model.static returned %d models with no account, want none", len(models))
	}

	withAccount := newFakeHost()
	storedCredential(t, withAccount, "idx-1", "raccoon-user-1.json", sampleCredential(t))
	withAccount.install(t)
	value, errStatic = handleModelStatic(testHost(), nil)
	if errStatic != nil {
		t.Fatalf("model.static: %v", errStatic)
	}
	models := value.(pluginapi.ModelResponse).Models
	if len(models) != len(fallbackCatalogue) {
		t.Fatalf("model.static returned %d models with an account, want %d", len(models), len(fallbackCatalogue))
	}
	// A foreign credential must not make this provider advertise models.
	other := newFakeHost()
	other.files = append(other.files, pluginapi.HostAuthFileEntry{Provider: "loomy", AuthIndex: "x", Name: "loomy-1.json"})
	other.install(t)
	value, _ = handleModelStatic(testHost(), nil)
	if models := value.(pluginapi.ModelResponse).Models; len(models) != 0 {
		t.Errorf("another provider's credential unlocked %d models", len(models))
	}

	// model.register stays unconditional: it is development-time metadata.
	registered, errRegister := handleModelRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("model.register: %v", errRegister)
	}
	if len(registered.(pluginapi.ModelRegistrationResponse).Models) != len(fallbackCatalogue) {
		t.Error("model.register must keep publishing the bundled table")
	}
}

// TestCatalogueUsesTheShorterTimeout pins trap #31: the catalogue call is capped
// at 20 s, not the 60 s the other endpoints use.
func TestCatalogueUsesTheShorterTimeout(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.catalogueTimeout() != CatalogueTimeoutMS {
		t.Errorf("catalogue timeout = %d, want %d", cfg.catalogueTimeout(), CatalogueTimeoutMS)
	}
	if cfg.catalogueTimeout() >= cfg.requestTimeout() {
		t.Errorf("the catalogue timeout (%d) must be SHORTER than the request timeout (%d)",
			cfg.catalogueTimeout(), cfg.requestTimeout())
	}
	// A configured value overrides the default, and a zero falls back to it.
	custom := ConfigFromYAML([]byte("catalogue_timeout_ms: 5000\nrequest_timeout_ms: 90000\n"))
	if custom.catalogueTimeout() != 5000 || custom.requestTimeout() != 90_000 {
		t.Errorf("configured timeouts = %d / %d", custom.catalogueTimeout(), custom.requestTimeout())
	}
	zeroed := ConfigFromYAML([]byte("catalogue_timeout_ms: 0\n"))
	if zeroed.catalogueTimeout() != CatalogueTimeoutMS {
		t.Errorf("a zero catalogue timeout must fall back to the default, got %d", zeroed.catalogueTimeout())
	}
}

// TestConfigFromYAMLCoercesPerKey pins the shared settings contract: flow style,
// quoted numbers and boolean spellings all work, and one bad value costs only
// itself.
func TestConfigFromYAMLCoercesPerKey(t *testing.T) {
	cfg := ConfigFromYAML([]byte(`{discover_models: "no", model_cache_ttl_ms: "60000", priority: 3, login_timeout_ms: 1000}`))
	if cfg.DiscoverModels {
		t.Error(`discover_models: "no" must be read as false`)
	}
	if cfg.ModelCacheTTLMS != 60_000 || cfg.Priority != 3 || cfg.LoginTimeoutMS != 1000 {
		t.Errorf("coerced config = %+v", cfg)
	}
	// An unusable value keeps its default instead of discarding the document.
	broken := ConfigFromYAML([]byte("login_timeout_ms: soon\nqr_poll_interval_seconds: 4\n"))
	if broken.LoginTimeoutMS != LoginTimeoutMS || broken.QRPollIntervalSeconds != 4 {
		t.Errorf("partial config = %+v", broken)
	}
	if got := ConfigFromYAML(nil); got != DefaultConfig() {
		t.Errorf("an empty document must produce the defaults, got %+v", got)
	}
	// The refresh window is honoured, including an explicit zero.
	if got := ConfigFromYAML([]byte("refresh_window_seconds: 0\n")).refreshWindow(); got != 0 {
		t.Errorf("refresh_window_seconds: 0 → %d, want 0", got)
	}
	if got := DefaultConfig().refreshWindow(); got != RefreshWindowSeconds {
		t.Errorf("default refresh window = %d, want %d", got, RefreshWindowSeconds)
	}
}
