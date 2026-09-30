package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Model-catalogue tests.
//
// Two facts drive everything here:
//
//   - the exposed models are a MEASURED whitelist. The upstream pool lists four,
//     but two of them answer with an empty body under the Start Plan entitlement
//     (0/3 correct against 3/3 for GLM-5.3), so publishing them would hand the
//     user a model that produces silence;
//   - the capability fields come from upstream, never from inference. A recorded
//     defect came from guessing `200_000` / `32_768` where upstream publishes
//     `1_000_000` / `128_000`, and from marking both models as vision-capable when
//     upstream's `capabilities` object is EMPTY on GLM-5.3.

// TestFallbackCatalogueExposesOnlyVerifiedModels pins the whitelist and the
// upstream capability values.
func TestFallbackCatalogueExposesOnlyVerifiedModels(t *testing.T) {
	catalogue := currentCatalogue()
	if len(catalogue) != 2 {
		t.Fatalf("catalogue size = %d, want 2", len(catalogue))
	}
	ids := catalogueIDs(catalogue)
	for _, forbidden := range []string{"GLM-5-Turbo", "GLM-5.2"} {
		for _, id := range ids {
			if id == forbidden {
				t.Errorf("%s is published although it answers with an empty response under this entitlement", forbidden)
			}
		}
	}
	for _, expected := range []string{"GLM-5.3", "GLM-5.3-Flash"} {
		found := false
		for _, id := range ids {
			if id == expected {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not published although it was measured to work", expected)
		}
	}
}

// TestFallbackCatalogueCapabilitiesMatchUpstream pins the per-model values,
// including the vision asymmetry.
func TestFallbackCatalogueCapabilitiesMatchUpstream(t *testing.T) {
	cases := []struct {
		id              string
		contextWindow   int
		maxOutputTokens int
		supportsImage   bool
		levels          []string
	}{
		{"GLM-5.3-Flash", 1_000_000, 128_000, true, []string{"low", "high", "max"}},
		// ⚠ Upstream's `capabilities` is an EMPTY OBJECT for GLM-5.3, so it has no
		// vision. Marking it true because "the family should match" was the defect.
		{"GLM-5.3", 1_000_000, 128_000, false, []string{"low", "high", "max"}},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			model, ok := catalogueModel(fallbackCatalogue, tc.id)
			if !ok {
				t.Fatalf("%s is not in the fallback catalogue", tc.id)
			}
			if model.ContextWindow != tc.contextWindow {
				t.Errorf("context window = %d, want %d", model.ContextWindow, tc.contextWindow)
			}
			if model.MaxOutputTokens != tc.maxOutputTokens {
				t.Errorf("max output = %d, want %d", model.MaxOutputTokens, tc.maxOutputTokens)
			}
			if model.SupportsImage != tc.supportsImage {
				t.Errorf("supportsImage = %v, want %v", model.SupportsImage, tc.supportsImage)
			}
			if strings.Join(model.ReasoningLevels, ",") != strings.Join(tc.levels, ",") {
				t.Errorf("levels = %v, want %v", model.ReasoningLevels, tc.levels)
			}
		})
	}
}

// TestModelInfoDeclaresThinkingLevels covers the host descriptor.
//
// The level list is the ONLY reason the model's reasoning selector appears in a
// client, so it has to be published. The host type has no default-effort field,
// which is why `DefaultReasoningLevel` is carried for the request path and the
// management page instead.
func TestModelInfoDeclaresThinkingLevels(t *testing.T) {
	model, _ := catalogueModel(fallbackCatalogue, "GLM-5.3-Flash")
	info := modelInfoFor(model, "", time.Now())

	if info.Thinking == nil {
		t.Fatal("Thinking is nil, so no reasoning selector would render for this model")
	}
	if strings.Join(info.Thinking.Levels, ",") != "low,high,max" {
		t.Fatalf("levels = %v, want low,high,max in display order", info.Thinking.Levels)
	}
	// The upstream default is `max`; the host type cannot carry it, so the model
	// table must — recording the limitation here keeps it from being "fixed" by
	// inventing a field on ModelInfo.
	if model.DefaultReasoningLevel != "max" {
		t.Errorf("default level = %q, want max (carried in the table, since ModelInfo has no such field)",
			model.DefaultReasoningLevel)
	}
	if info.ContextLength != 1_000_000 || info.MaxCompletionTokens != 128_000 {
		t.Errorf("window/output = %d/%d, want 1000000/128000", info.ContextLength, info.MaxCompletionTokens)
	}
	if info.ID != "GLM-5.3-Flash" || info.Name != "GLM-5.3-Flash" {
		t.Errorf("id/name = %q/%q", info.ID, info.Name)
	}
	if info.OwnedBy != ProviderKey {
		t.Errorf("owned_by = %q, want %q", info.OwnedBy, ProviderKey)
	}
}

// TestModelInfoModalitiesFollowVision covers the modality publication, which has
// to agree with the vision flag: a client decides whether to send an image based
// on this list.
func TestModelInfoModalitiesFollowVision(t *testing.T) {
	flash, _ := catalogueModel(fallbackCatalogue, "GLM-5.3-Flash")
	pro, _ := catalogueModel(fallbackCatalogue, "GLM-5.3")

	flashInfo := modelInfoFor(flash, "", time.Now())
	if strings.Join(flashInfo.SupportedInputModalities, ",") != "text,image" {
		t.Errorf("Flash modalities = %v, want text,image", flashInfo.SupportedInputModalities)
	}
	proInfo := modelInfoFor(pro, "", time.Now())
	if strings.Join(proInfo.SupportedInputModalities, ",") != "text" {
		t.Errorf("GLM-5.3 modalities = %v, want text only", proInfo.SupportedInputModalities)
	}
}

// TestModelPrefixIsPerAccount covers the `<account>/<model>` id shape.
//
// A static catalogue carries NO prefix, because there is no account behind it to
// serve those ids.
func TestModelPrefixIsPerAccount(t *testing.T) {
	model, _ := catalogueModel(fallbackCatalogue, "GLM-5.3")
	credential := sampleCredential()

	info := modelInfoFor(model, modelPrefixFor(credential), time.Now())
	// The prefix is the first eight characters of the STABLE account identity.
	if info.ID != "user-123/GLM-5.3" {
		t.Fatalf("prefixed id = %q, want user-123/GLM-5.3", info.ID)
	}
	if info.Name != "GLM-5.3" {
		t.Errorf("Name = %q, want the bare model name", info.Name)
	}

	// With the setting off there is no prefix at all.
	withSettings(t, Config{ModelPrefix: false})
	if got := modelPrefixFor(credential); got != "" {
		t.Fatalf("prefix = %q, want empty when model_prefix is off", got)
	}

	// A credential without a user id has no stable prefix either.
	bare := &Credential{ZCodeJWT: "jwt", DeviceMid: "mid"}
	if got := modelPrefixFor(bare); got != "" {
		t.Fatalf("prefix = %q, want empty for a credential with no user id", got)
	}
}

// TestStaticCatalogueNeverGetsAPrefix guards the rule that a static listing
// publishes unprefixed ids only.
func TestStaticCatalogueNeverGetsAPrefix(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	value, errRegister := handleModelRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("model.register: %v", errRegister)
	}
	response := decodeResult[pluginapi.ModelRegistrationResponse](t, value)
	for _, model := range response.Models {
		if strings.Contains(model.ID, "/") {
			t.Errorf("static model id %q carries a prefix", model.ID)
		}
	}
}

// TestModelRegisterDoesNotTouchTheNetwork is an explicit assertion: enumeration is
// static by design, and the reference documents that listing models must never
// require a call.
func TestModelRegisterDoesNotTouchTheNetwork(t *testing.T) {
	fake := newFakeHost()
	fake.install(t) // fake.do is nil: any outbound call fails the test.

	for _, call := range []struct {
		name    string
		handler func(*abiboot.Host, json.RawMessage) (any, error)
	}{
		{"model.register", handleModelRegister},
		{"model.static", handleModelStatic},
	} {
		if _, errHandler := call.handler(nil, nil); errHandler != nil {
			t.Fatalf("%s: %v", call.name, errHandler)
		}
	}
	if calls := fake.callsFor("zcode.z.ai"); len(calls) != 0 {
		t.Fatalf("the static catalogue issued %d network calls", len(calls))
	}
}

// TestHandleModelForAuthUsesTheCredentialPrefix covers the per-auth catalogue.
func TestHandleModelForAuthUsesTheCredentialPrefix(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	storage, errEncode := sampleCredential().Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	raw, errMarshal := json.Marshal(pluginapi.AuthModelRequest{
		AuthID: "zcode-1", AuthProvider: ProviderKey, StorageJSON: storage,
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	value, errHandle := handleModelForAuth(nil, raw)
	if errHandle != nil {
		t.Fatalf("model.for_auth: %v", errHandle)
	}
	response := decodeResult[pluginapi.ModelResponse](t, value)
	if len(response.Models) != 2 {
		t.Fatalf("model count = %d, want 2", len(response.Models))
	}
	for _, model := range response.Models {
		if !strings.HasPrefix(model.ID, "user-123/") {
			t.Errorf("model id %q does not carry the account prefix", model.ID)
		}
	}

	// An unparseable credential still yields the unprefixed catalogue rather than
	// an error: the listing is advisory and hiding it would break routing.
	badRaw, _ := json.Marshal(pluginapi.AuthModelRequest{AuthID: "zcode-1", StorageJSON: []byte(`{"nonsense":true}`)})
	badValue, errBad := handleModelForAuth(nil, badRaw)
	if errBad != nil {
		t.Fatalf("model.for_auth with a broken credential: %v", errBad)
	}
	badResponse := decodeResult[pluginapi.ModelResponse](t, badValue)
	if len(badResponse.Models) != 2 {
		t.Fatalf("unprefixed fallback model count = %d, want 2", len(badResponse.Models))
	}
	for _, model := range badResponse.Models {
		if strings.Contains(model.ID, "/") {
			t.Errorf("fallback model id %q unexpectedly carries a prefix", model.ID)
		}
	}
}

// TestParseBuiltinModelsAcceptsBothShapes covers the measured object shape and the
// array alternative.
//
// ⚠ `builtinModels` is an OBJECT keyed by an index string — measured as
// `{"0": {...}, "1": {...}}`. Testing it with an array check yields the false
// negative "zero models", which is what pushed the reference onto a guessed
// fallback table in the first place.
func TestParseBuiltinModelsAcceptsBothShapes(t *testing.T) {
	entry := map[string]any{
		"modelId":             "GLM-5.3-Flash",
		"contextWindow":       float64(1_000_000),
		"maxCompletionTokens": float64(128_000),
		"capabilities":        map[string]any{"vision": true},
		"reasoning": map[string]any{
			// ⚠ The insertion order upstream actually sends is low, max, high;
			// the IDE renders low, high, max.
			"levels":       map[string]any{"low": map[string]any{}, "max": map[string]any{}, "high": map[string]any{}},
			"defaultLevel": "max",
		},
	}
	cases := []struct {
		name string
		raw  any
	}{
		{"the measured object shape", map[string]any{"0": entry}},
		{"the array alternative", []any{entry}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, errParse := parseBuiltinModels(tc.raw)
			if errParse != nil {
				t.Fatalf("parseBuiltinModels: %v", errParse)
			}
			if len(models) != 1 {
				t.Fatalf("model count = %d, want 1", len(models))
			}
			model := models[0]
			if model.ID != "GLM-5.3-Flash" {
				t.Errorf("id = %q", model.ID)
			}
			if model.ContextWindow != 1_000_000 || model.MaxOutputTokens != 128_000 {
				t.Errorf("window/output = %d/%d", model.ContextWindow, model.MaxOutputTokens)
			}
			if !model.SupportsImage {
				t.Error("vision flag was lost")
			}
			// The display order is normalised, NOT the object's key order.
			if strings.Join(model.ReasoningLevels, ",") != "low,high,max" {
				t.Errorf("levels = %v, want low,high,max (the IDE order)", model.ReasoningLevels)
			}
			if model.DefaultReasoningLevel != "max" {
				t.Errorf("default level = %q", model.DefaultReasoningLevel)
			}
		})
	}
}

// TestParseBuiltinModelsRejectsUnusableShapes covers the failure paths.
func TestParseBuiltinModelsRejectsUnusableShapes(t *testing.T) {
	cases := []struct {
		name    string
		raw     any
		wantErr bool
	}{
		{"neither object nor array", "a string", true},
		{"an object with no usable entries", map[string]any{"0": "not-an-object"}, true},
		{"an empty object", map[string]any{}, true},
		{"an entry with no id is skipped", map[string]any{"0": map[string]any{"name": "nameless"}}, true},
		{"a usable entry", map[string]any{"0": map[string]any{"modelId": "GLM-5.3"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errParse := parseBuiltinModels(tc.raw)
			if tc.wantErr && errParse == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && errParse != nil {
				t.Fatalf("unexpected error: %v", errParse)
			}
		})
	}
}

// TestParseBuiltinModelsAppliesConservativeDefaults covers the absent-field path:
// zero would tell the host the model has no window at all.
func TestParseBuiltinModelsAppliesConservativeDefaults(t *testing.T) {
	models, errParse := parseBuiltinModels(map[string]any{"0": map[string]any{"modelId": "GLM-5.3"}})
	if errParse != nil {
		t.Fatalf("parseBuiltinModels: %v", errParse)
	}
	if models[0].ContextWindow != 200_000 || models[0].MaxOutputTokens != 32_768 {
		t.Fatalf("defaults = %d/%d, want 200000/32768",
			models[0].ContextWindow, models[0].MaxOutputTokens)
	}
	if models[0].SupportsImage {
		t.Error("vision defaulted to true, which would send images to a model that may reject them")
	}
}

// TestOrderReasoningLevelsCoversKnownAndUnknown covers the ordering rule.
func TestOrderReasoningLevelsCoversKnownAndUnknown(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"the measured set is reordered to the IDE order", []string{"low", "max", "high"}, "low,high,max"},
		{"an unknown level is appended rather than dropped", []string{"max", "turbo", "low"}, "low,max,turbo"},
		{"a full set is ordered by the table", []string{"max", "xhigh", "medium", "low", "none", "minimal", "high"}, "none,minimal,low,medium,high,xhigh,max"},
		{"duplicates collapse", []string{"low", "low", "high"}, "low,high"},
		{"an empty input stays empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(orderReasoningLevels(tc.in), ","); got != tc.want {
				t.Fatalf("orderReasoningLevels(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRestrictToVerifiedFiltersTheRemoteCatalogue covers the entitlement filter.
//
// A remote catalogue that names a model outside the measured whitelist must not
// publish it: the whitelist exists precisely because those ids answer with an
// empty body.
func TestRestrictToVerifiedFiltersTheRemoteCatalogue(t *testing.T) {
	remote := []fallbackModel{
		{ID: "GLM-5-Turbo"},
		{ID: "GLM-5.2"},
		{ID: "GLM-5.3", ContextWindow: 1_000_000},
		{ID: "GLM-5.3-Flash", ContextWindow: 1_000_000},
		{ID: "some-new-model"},
	}
	filtered := restrictToVerified(remote)
	if len(filtered) != 2 {
		t.Fatalf("filtered count = %d, want 2", len(filtered))
	}
	for _, model := range filtered {
		if model.ID != "GLM-5.3" && model.ID != "GLM-5.3-Flash" {
			t.Errorf("%s survived the filter", model.ID)
		}
	}

	// A remote catalogue that is entirely unverified falls back rather than
	// publishing nothing.
	fallback := restrictToVerified([]fallbackModel{{ID: "GLM-5-Turbo"}})
	if len(fallback) != len(fallbackCatalogue) {
		t.Fatalf("all-unverified fallback count = %d, want %d", len(fallback), len(fallbackCatalogue))
	}
}

// TestRefreshRemoteCatalogueStoresTheFetchedCatalogue covers the discovery path
// against a scripted response.
func TestRefreshRemoteCatalogueStoresTheFetchedCatalogue(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		body := jsonBody(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"builtinModels": map[string]any{
					"0": map[string]any{
						"modelId": "GLM-5.3-Flash", "contextWindow": 2_000_000,
						"maxCompletionTokens": 256_000,
						"capabilities":        map[string]any{"vision": true},
						"reasoning": map[string]any{
							"levels":       map[string]any{"low": map[string]any{}, "high": map[string]any{}},
							"defaultLevel": "high",
						},
					},
				},
			},
		})
		return httpResponse(http.StatusOK, body), nil
	}

	if errRefresh := refreshRemoteCatalogue(testHost(), sampleCredential(), settings()); errRefresh != nil {
		t.Fatalf("refreshRemoteCatalogue: %v", errRefresh)
	}
	catalogue := currentCatalogue()
	model, ok := catalogueModel(catalogue, "GLM-5.3-Flash")
	if !ok {
		t.Fatal("the discovered model is missing")
	}
	// The remote values win over the built-in table's.
	if model.ContextWindow != 2_000_000 || model.MaxOutputTokens != 256_000 {
		t.Fatalf("window/output = %d/%d, want the remote values", model.ContextWindow, model.MaxOutputTokens)
	}
	if model.DefaultReasoningLevel != "high" {
		t.Errorf("default level = %q, want the remote value", model.DefaultReasoningLevel)
	}

	// The request carried the required headers.
	calls := fake.callsFor(ClientConfigsPath)
	if len(calls) != 1 {
		t.Fatalf("configs calls = %d, want 1", len(calls))
	}
	headers := calls[0].Headers
	if headers.Get("Authorization") == "" {
		t.Error("client/configs was queried without the bearer token, which it requires")
	}
	if headers.Get("X-Device-Mid") == "" {
		t.Error("client/configs was queried without X-Device-Mid, which it requires")
	}
	// ⚠ `platform` must be `unknown`: every other value was measured to return
	// 400 code 3001.
	if !strings.Contains(calls[0].URL, "platform=unknown") {
		t.Errorf("configs URL = %q, want platform=unknown", calls[0].URL)
	}
}

// TestRefreshRemoteCatalogueFallsBackSilently covers the "catalogue failure must
// not break the provider" rule.
func TestRefreshRemoteCatalogueFallsBackSilently(t *testing.T) {
	cases := []struct {
		name     string
		response *pluginapi.HTTPResponse
		errDo    error
	}{
		{"a transport failure", nil, errFakeTransport},
		{"a 500", httpResponse(http.StatusInternalServerError, `{"code":500}`), nil},
		{"a business error with HTTP 200", httpResponse(http.StatusOK, `{"code":3001,"msg":"parameter error"}`), nil},
		{"a body with no models", httpResponse(http.StatusOK, `{"code":0,"data":{}}`), nil},
		{"malformed json", httpResponse(http.StatusOK, `not json`), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.install(t)
			fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				return tc.response, tc.errDo
			}
			// The error is returned to the caller, but the CATALOGUE must stay usable.
			_ = refreshRemoteCatalogue(testHost(), sampleCredential(), settings())
			if len(currentCatalogue()) != len(fallbackCatalogue) {
				t.Fatalf("catalogue size = %d, want the built-in fallback of %d",
					len(currentCatalogue()), len(fallbackCatalogue))
			}
		})
	}
}

// TestModelCatalogIsUnaffectedByAnotherAccountsLogin covers the isolation rule: the
// catalogue is process-wide but a second account must not inherit the first one's
// prefix.
func TestModelCatalogIsUnaffectedByAnotherAccountsLogin(t *testing.T) {
	first := sampleCredential()
	second := sampleCredential()
	second.UserID = "another-user-9999"

	firstPrefix := modelPrefixFor(first)
	secondPrefix := modelPrefixFor(second)
	if firstPrefix == secondPrefix {
		t.Fatalf("both accounts produced the prefix %q", firstPrefix)
	}
	if firstPrefix != "user-123" || secondPrefix != "another-" {
		t.Fatalf("prefixes = %q/%q", firstPrefix, secondPrefix)
	}
}
