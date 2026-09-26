package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestIsFreeModel is the union rule of `cline-models.ts:85-94`. Free and paid
// models are DIFFERENT ids, so nothing here may fuzzy-match.
func TestIsFreeModel(t *testing.T) {
	remoteFree := map[string]struct{}{"cline-free/from-server": {}}
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"remote free list is authoritative", "cline-free/from-server", true},
		{"free suffix", "owner/model:free", true},
		{"free namespace prefix", "cline-free/anything", true},
		{"static table entry", "stealth/space-bunny-alpha", true},
		{"paid id with the same model name", "deepseek/deepseek-v4.1-flash", false},
		{"free substring is not a prefix", "owner/cline-free/model", false},
		{"unknown id", "anthropic/claude-x", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isFreeModel(testCase.id, remoteFree); got != testCase.want {
				t.Fatalf("isFreeModel(%q) = %v, want %v", testCase.id, got, testCase.want)
			}
		})
	}
}

// TestMergeCatalogueOrderAndMetadata pins the four-step merge order and the
// metadata precedence of `cline-models.ts:192-235`.
func TestMergeCatalogueOrderAndMetadata(t *testing.T) {
	curated := parseCatalogue([]byte(`{
	  "free": [
	    {"id":"cline-free/deepseek-v4.1-flash","name":"Remote Name","description":"remote free description"},
	    {"id":"other/model:free"},
	    {"id":""}
	  ],
	  "recommended": [{"id":"deepseek/deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash"}],
	  "clinePass": [{"id":"cline-pass/model","name":"Pass Model"}]
	}`))
	remoteIDs := parseRemoteModelIDs([]byte(`{"data":[{"id":"anthropic/claude-x"},{"id":"cline-free/deepseek-v4.1-flash"}]}`))

	models := mergeCatalogue(curated, remoteIDs)
	wantOrder := []string{
		"cline-free/deepseek-v4.1-flash",
		"other/model:free",
		"stealth/space-bunny-alpha",
		"cline-free/mimo-v2.6-flash",
		"cline-free/gemini-3.8-flash",
		"cline-free/muse-spark-1.3-contributor",
		"deepseek/deepseek-v4.1-flash",
		"cline-pass/model",
		"anthropic/claude-x",
	}
	if len(models) != len(wantOrder) {
		t.Fatalf("merged %d models, want %d: %+v", len(models), len(wantOrder), idsOf(models))
	}
	for index, want := range wantOrder {
		if models[index].ID != want {
			t.Fatalf("position %d = %q, want %q (order: %v)", index, models[index].ID, want, idsOf(models))
		}
	}

	byID := map[string]pluginapi.ModelInfo{}
	for _, model := range models {
		byID[model.ID] = model
	}

	deepseek := byID["cline-free/deepseek-v4.1-flash"]
	// The static table supplies context window / output limit / image support,
	// and its NAME wins over the remote one.
	if deepseek.Name != "DeepSeek V4.1 Flash" {
		t.Errorf("name = %q, want the static table name", deepseek.Name)
	}
	if deepseek.DisplayName != "DeepSeek V4.1 Flash · 免费" {
		t.Errorf("display name = %q, want the free marker in the name", deepseek.DisplayName)
	}
	if deepseek.ContextLength != 1_048_576 || deepseek.MaxCompletionTokens != 131_072 {
		t.Errorf("static metadata missing: %+v", deepseek)
	}
	// The remote description wins over the static table's generic one.
	if deepseek.Description != "remote free description" {
		t.Errorf("description = %q", deepseek.Description)
	}
	if len(deepseek.SupportedInputModalities) != 2 {
		t.Errorf("image modality missing: %v", deepseek.SupportedInputModalities)
	}

	gemini := byID["cline-free/gemini-3.8-flash"]
	if gemini.MaxCompletionTokens != 65_536 {
		t.Errorf("gemini clamp = %d, want 65536", gemini.MaxCompletionTokens)
	}

	remotelyFree := byID["other/model:free"]
	if !strings.HasSuffix(remotelyFree.DisplayName, "· 免费") {
		t.Errorf("the :free suffix must mark the model free: %q", remotelyFree.DisplayName)
	}
	if remotelyFree.ContextLength != 0 || remotelyFree.MaxCompletionTokens != 0 {
		t.Errorf("an unknown model must not get invented limits: %+v", remotelyFree)
	}

	paid := byID["deepseek/deepseek-v4.1-flash"]
	if strings.Contains(paid.DisplayName, "免费") {
		t.Errorf("the metered twin must not be marked free: %q", paid.DisplayName)
	}
	if paid.Name != "DeepSeek V4.1 Flash" {
		t.Errorf("remote name = %q", paid.Name)
	}

	pass := byID["cline-pass/model"]
	if strings.Contains(pass.DisplayName, "免费") {
		t.Errorf("clinePass is a subscription, not a free tier: %q", pass.DisplayName)
	}
	if !strings.Contains(pass.Description, "订阅") {
		t.Errorf("clinePass description = %q", pass.Description)
	}

	if derived := byID["anthropic/claude-x"].Name; derived != "Claude X" {
		t.Errorf("derived name = %q, want %q", derived, "Claude X")
	}

	// Reasoning levels are declared for every model (`cline-adapter.ts:341-350`).
	for _, model := range models {
		if model.Thinking == nil || len(model.Thinking.Levels) != 5 {
			t.Fatalf("model %s has no five-level thinking table: %+v", model.ID, model.Thinking)
		}
		if !model.Thinking.ZeroAllowed {
			t.Errorf("model %s must allow disabling reasoning (`none`)", model.ID)
		}
	}
}

// TestParseCatalogueDiscardsUnusableEntries pins the "non-empty string id" rule.
func TestParseCatalogueDiscardsUnusableEntries(t *testing.T) {
	parsed := parseCatalogue([]byte(`{"free":[{"id":"  keep  "},{"id":""},{"name":"no id"},{"id":42},"junk"],"recommended":null}`))
	if len(parsed.Free) != 1 || parsed.Free[0].ID != "keep" {
		t.Fatalf("parsed = %+v", parsed.Free)
	}
	if len(parsed.Recommended) != 0 || len(parsed.ClinePass) != 0 {
		t.Errorf("missing arrays must parse to empty slices: %+v", parsed)
	}
	if parsed := parseCatalogue([]byte(`<html>502</html>`)); len(parsed.Free)+len(parsed.Recommended)+len(parsed.ClinePass) != 0 {
		t.Errorf("a non-JSON body must parse to an empty catalogue: %+v", parsed)
	}
}

// TestFetchCatalogueSkippedWithoutCredential pins `cline-models.ts:282-284`: with
// no credential the authenticated listing is not even requested.
func TestFetchCatalogueSkippedWithoutCredential(t *testing.T) {
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(200, `{"free":[],"recommended":[],"clinePass":[]}`)}}}
	fetched := fetchCatalogue(fake.do, nil)
	if fake.callCount() != 1 {
		t.Fatalf("requests = %d, want only the curated one", fake.callCount())
	}
	if got := fake.call(0).URL; got != APIBase+RecommendedModelsPath {
		t.Fatalf("url = %q", got)
	}
	if got := fake.call(0).Headers.Get("Authorization"); got != "" {
		t.Errorf("the curated endpoint is anonymous, got %q", got)
	}
	if got := fake.call(0).Headers.Get("X-CLIENT-TYPE"); got != "cline-sdk" {
		t.Errorf("the curated endpoint still carries the client headers, got %q", got)
	}
	if len(fetched.notes) != 0 {
		t.Errorf("notes = %v", fetched.notes)
	}
}

// TestFetchCatalogueUsesThePrefixedBearerToken is the highest-risk rule applied
// to the model listing: a stripped prefix answers 401 upstream.
func TestFetchCatalogueUsesThePrefixedBearerToken(t *testing.T) {
	fake := &fakeTransport{steps: []fakeStep{
		{response: jsonResponse(200, `{"free":[{"id":"cline-free/x"}]}`)},
		{response: jsonResponse(200, `{"data":[{"id":"anthropic/claude-x"}]}`)},
	}}
	fetched := fetchCatalogue(fake.do, &Credential{AccessToken: "eyJ"})
	if fake.callCount() != 2 {
		t.Fatalf("requests = %d, want 2", fake.callCount())
	}
	if got := fake.call(1).URL; got != APIBase+ModelsPath {
		t.Fatalf("url = %q", got)
	}
	if got := fake.call(1).Headers.Get("Authorization"); got != "Bearer workos:eyJ" {
		t.Fatalf("Authorization = %q", got)
	}
	if len(fetched.remoteIDs) != 1 || len(fetched.catalogue.Free) != 1 {
		t.Fatalf("fetch = %+v", fetched)
	}
}

// TestFetchCatalogueIndependentFailures pins that either endpoint may fail
// without invalidating the other (`cline-models.ts:246-255`).
func TestFetchCatalogueIndependentFailures(t *testing.T) {
	fake := &fakeTransport{steps: []fakeStep{
		{response: jsonResponse(500, `{"error":"boom"}`)},
		{response: jsonResponse(200, `{"data":[{"id":"anthropic/claude-x"}]}`)},
	}}
	fetched := fetchCatalogue(fake.do, &Credential{AccessToken: "workos:eyJ"})
	if len(fetched.remoteIDs) != 1 {
		t.Fatalf("the working endpoint must still contribute: %+v", fetched)
	}
	if len(fetched.notes) != 1 || !strings.Contains(fetched.notes[0], "推荐目录返回 HTTP 500") {
		t.Fatalf("notes = %v", fetched.notes)
	}
}

// TestExecuteModelCatalogCache pins the TTL semantics: 0 disables the cache.
func TestExecuteModelCatalogCache(t *testing.T) {
	resetModelCache(t)
	fake := &fakeTransport{}
	for index := 0; index < 2; index++ {
		fake.steps = append(fake.steps,
			fakeStep{response: jsonResponse(200, `{"free":[{"id":"cline-free/cached"}]}`)},
			fakeStep{response: jsonResponse(200, `{"data":[{"id":"anthropic/claude-x"}]}`)})
	}
	installFakeTransport(t, fake)
	credential := &Credential{AccessToken: "workos:eyJ"}

	cached := executeModelCatalog(nil, credential, Config{ModelDiscovery: true, ModelCacheTTLMS: 600_000})
	if fake.callCount() != 2 {
		t.Fatalf("first fetch issued %d requests", fake.callCount())
	}
	again := executeModelCatalog(nil, credential, Config{ModelDiscovery: true, ModelCacheTTLMS: 600_000})
	if fake.callCount() != 2 {
		t.Fatalf("the cache was not used: %d requests", fake.callCount())
	}
	if len(again) != len(cached) {
		t.Fatalf("cached listing differs: %d vs %d", len(again), len(cached))
	}

	// TTL 0 re-fetches.
	executeModelCatalog(nil, credential, Config{ModelDiscovery: true, ModelCacheTTLMS: 0})
	if fake.callCount() != 4 {
		t.Fatalf("TTL 0 must re-fetch: %d requests", fake.callCount())
	}

	// Discovery disabled never fetches.
	before := fake.callCount()
	static := executeModelCatalog(nil, credential, Config{ModelDiscovery: false})
	if fake.callCount() != before {
		t.Fatalf("discovery disabled must not fetch: %d requests", fake.callCount())
	}
	if len(static) != len(clineFallbackModels) {
		t.Fatalf("static listing = %d models", len(static))
	}
}

// TestExecuteModelCatalogFallsBackWithoutRemoteData pins that a failed fetch is
// not cached and yields the static table.
func TestExecuteModelCatalogFallsBackWithoutRemoteData(t *testing.T) {
	resetModelCache(t)
	fake := &fakeTransport{steps: []fakeStep{
		{err: errTestTransport},
		{err: errTestTransport},
	}}
	installFakeTransport(t, fake)
	models := executeModelCatalog(nil, &Credential{AccessToken: "workos:eyJ"},
		Config{ModelDiscovery: true, ModelCacheTTLMS: 600_000})
	if len(models) != len(clineFallbackModels) {
		t.Fatalf("models = %d, want the static table", len(models))
	}
	if cached := discoveredModels.get(time.Hour); len(cached) != 0 {
		t.Errorf("a fallback listing must not be cached: %d entries", len(cached))
	}
}

// TestStaticModelInfos pins the five-entry snapshot.
func TestStaticModelInfos(t *testing.T) {
	models := staticModelInfos()
	if len(models) != 5 {
		t.Fatalf("static catalogue = %d models, want 5", len(models))
	}
	expected := map[string]int64{
		"stealth/space-bunny-alpha":             524_288,
		"cline-free/mimo-v2.6-flash":            131_072,
		"cline-free/deepseek-v4.1-flash":        131_072,
		"cline-free/gemini-3.8-flash":           65_536,
		"cline-free/muse-spark-1.3-contributor": 943_718,
	}
	for _, model := range models {
		want, known := expected[model.ID]
		if !known {
			t.Fatalf("unexpected static model %q", model.ID)
		}
		if model.MaxCompletionTokens != want {
			t.Errorf("%s max tokens = %d, want %d", model.ID, model.MaxCompletionTokens, want)
		}
		if !strings.HasSuffix(model.DisplayName, "· 免费") {
			t.Errorf("%s is not marked free: %q", model.ID, model.DisplayName)
		}
		if model.OwnedBy != ProviderKey || model.Type != "chat" {
			t.Errorf("%s metadata = %+v", model.ID, model)
		}
	}
}

// TestDerivedModelName is `deriveModelName` (`cline-models.ts:112-118`).
func TestDerivedModelName(t *testing.T) {
	cases := map[string]string{
		"anthropic/claude-sonnet-4":  "Claude Sonnet 4",
		"cline-free/mimo-v2.6-flash": "Mimo V2.6 Flash",
		"owner/model:free":           "Model",
		"plain":                      "Plain",
		"owner/nested/model-x":       "Model X",
	}
	for id, want := range cases {
		if got := derivedModelName(id); got != want {
			t.Errorf("derivedModelName(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestModelForAuthFallsBackOnBadCredential pins that a listing request never
// fails the host call: routing must keep working while a credential is repaired.
func TestModelForAuthFallsBackOnBadCredential(t *testing.T) {
	resetModelCache(t)
	withTestSettings(t, DefaultConfig())
	value, errHandler := handleModelForAuth(nil, []byte(`{"storage_json":"bm90IGpzb24="}`))
	if errHandler != nil {
		t.Fatalf("handleModelForAuth: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.ModelResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	if response.Provider != ProviderKey || len(response.Models) != len(clineFallbackModels) {
		t.Fatalf("response = %+v", response)
	}
}

// TestModelRegisterIsOffline pins that model.register never touches the network.
func TestModelRegisterIsOffline(t *testing.T) {
	fake := &fakeTransport{}
	installFakeTransport(t, fake)
	value, errHandler := handleModelRegister(nil, nil)
	if errHandler != nil {
		t.Fatalf("handleModelRegister: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.ModelRegistrationResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	if fake.callCount() != 0 {
		t.Errorf("model.register issued %d requests", fake.callCount())
	}
	if len(response.Models) != len(clineFallbackModels) {
		t.Errorf("models = %d", len(response.Models))
	}

	value, errHandler = handleModelStatic(nil, nil)
	if errHandler != nil {
		t.Fatalf("handleModelStatic: %v", errHandler)
	}
	static, okStatic := value.(pluginapi.ModelResponse)
	if !okStatic || len(static.Models) != len(clineFallbackModels) {
		t.Errorf("model.static reply = %T %+v", value, static)
	}
	if fake.callCount() != 0 {
		t.Errorf("model.static issued %d requests", fake.callCount())
	}
}

// TestCatalogueHeadersAreAnonymous pins that the curated request never carries a
// credential.
func TestCatalogueHeadersAreAnonymous(t *testing.T) {
	header := catalogueHeaders()
	if got := header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q", got)
	}
	if got := header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
	if got := header.Get("X-IS-MULTIROOT"); got != "false" {
		t.Errorf("X-IS-MULTIROOT = %q, want the literal string", got)
	}
}

// idsOf renders the merged order for failure messages.
func idsOf(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return out
}

// resetModelCache clears the catalogue cache for one test.
func resetModelCache(t *testing.T) {
	t.Helper()
	discoveredModels.reset()
	t.Cleanup(discoveredModels.reset)
}

var errTestTransport = &testTransportError{"fakeTransport: transport failure"}

type testTransportError struct{ message string }

func (e *testTransportError) Error() string { return e.message }

// guard against an accidental real HTTP client in this package.
var _ = http.MethodGet
