package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// remoteModels renders the bare-array `/models` body the plugin parses.
func remoteModels(ids ...string) string {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, `{"id":"`+id+`","type":"chat","name":"`+id+`","context_length":1048576}`)
	}
	return "[" + strings.Join(entries, ",") + "]"
}

// catalogueHost installs a host with one account whose `/models` answer is
// controlled by the test.
func catalogueHost(t *testing.T, body string) *fakeHost {
	t.Helper()
	fake := newFakeHost()
	credential := sampleCredential(t)
	fake.auths["idx-1"] = mustStorage(t, credential)
	fake.files = []pluginapi.HostAuthFileEntry{
		{AuthIndex: "idx-1", Name: "loomy-13800138000.json", Provider: ProviderKey, Type: ProviderKey},
	}
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, ModelsPath) {
			return httpResponse(200, body), nil
		}
		return httpResponse(404, `{"code":"100001","desc":"unexpected"}`), nil
	}
	fake.install(t)
	cfg := DefaultConfig()
	cfg.DiscoverModels = true
	withSettings(t, cfg)
	return fake
}

// mustStorage encodes a credential the way the host stores it.
func mustStorage(t *testing.T, credential *Credential) []byte {
	t.Helper()
	encoded, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode credential: %v", errEncode)
	}
	return encoded
}

// TestCatalogueRefreshReportsUpstreamFailure pins the rule the shared package
// states: when the vendor never answered, the refresh must report a failure and
// must NOT publish. discoverModels returns nil on every failure so the CALLER
// falls back to the bundled table (`catalogueForAuth`), and that fallback must
// never be reported as a successful refresh.
func TestCatalogueRefreshReportsUpstreamFailure(t *testing.T) {
	fake := catalogueHost(t, "")
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, ModelsPath) {
			return nil, errFakeTransport
		}
		return httpResponse(404, `{}`), nil
	}

	outcome, errRefresh := catalogueRefresh(testHost(), settings())
	if errRefresh == nil {
		t.Fatalf("a failed refresh reported success: %+v", outcome)
	}
	if outcome.Models != 0 || outcome.Changed {
		t.Fatalf("a failed refresh reported an outcome: %+v", outcome)
	}
	if len(fake.callsFor(ModelsPath)) == 0 {
		t.Fatal("the refresh never contacted /models: the failure was not an upstream failure")
	}
	if peekDiscoveredModels() != nil {
		t.Fatal("a failed refresh left something in the cache, so the next request would serve it")
	}

	page := catalogueRefreshPage(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/loomy/status", Query: url.Values{"action": {"refresh-catalog"}},
	})
	if !strings.Contains(string(page.Body), "刷新失败") {
		t.Fatalf("a failed refresh did not say so: %s", string(page.Body))
	}
	if len(fake.savedNames()) != 0 {
		t.Fatalf("a failed refresh published to the host: %v", fake.savedNames())
	}
}

// TestCatalogueRefreshComputesChanged pins the before/after comparison: the
// first successful fetch moves the catalogue (changed), an identical fetch does
// not, and a different listing does.
func TestCatalogueRefreshComputesChanged(t *testing.T) {
	fake := catalogueHost(t, remoteModels("MiniMax-M3"))

	outcome, errRefresh := catalogueRefresh(testHost(), settings())
	if errRefresh != nil {
		t.Fatalf("first refresh: %v", errRefresh)
	}
	if outcome.Models != 1 || !outcome.Changed {
		t.Fatalf("first refresh = %+v, want 1 model marked changed", outcome)
	}

	outcome, errRefresh = catalogueRefresh(testHost(), settings())
	if errRefresh != nil {
		t.Fatalf("second refresh: %v", errRefresh)
	}
	if outcome.Models != 1 || outcome.Changed {
		t.Fatalf("identical refresh = %+v, want no change", outcome)
	}

	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, ModelsPath) {
			return httpResponse(200, remoteModels("MiniMax-M3", "MiniMax-M3.1")), nil
		}
		return httpResponse(404, `{}`), nil
	}
	outcome, errRefresh = catalogueRefresh(testHost(), settings())
	if errRefresh != nil {
		t.Fatalf("third refresh: %v", errRefresh)
	}
	if outcome.Models != 2 || !outcome.Changed {
		t.Fatalf("grown refresh = %+v, want 2 models marked changed", outcome)
	}
	// The refresh function itself never publishes: an automatic tick and the
	// manual button call the same function, and only the manual path adds the
	// AuthName that makes catalog.Run rewrite an auth file.
	if len(fake.savedNames()) != 0 {
		t.Fatalf("the refresh function published on its own: %v", fake.savedNames())
	}
}

// TestCatalogueRefreshDoesNotReportTheFallbackAsFresh pins the trap directly:
// after a failed fetch the serving path still answers with the bundled table (a
// client request must not lose its model list), and the refresh must still be an
// error rather than "the vendor sent the fallback".
func TestCatalogueRefreshDoesNotReportTheFallbackAsFresh(t *testing.T) {
	fake := catalogueHost(t, "")
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"100002","desc":"缺少 token"}`), nil
	}
	if _, errRefresh := catalogueRefresh(testHost(), settings()); errRefresh == nil {
		t.Fatal("a rejected session was reported as a successful refresh")
	}
	// The serving path still falls back, which is exactly why the refresh cannot
	// treat that fallback as evidence.
	value, errAuth := handleModelForAuth(testHost(), authModelPayload(t, sampleCredential(t)))
	if errAuth != nil {
		t.Fatalf("model.for_auth: %v", errAuth)
	}
	response := decodeResult[pluginapi.ModelResponse](t, value)
	if len(response.Models) != len(fallbackCatalogue) {
		t.Fatalf("models = %d, want the %d bundled entries", len(response.Models), len(fallbackCatalogue))
	}
}

// TestCatalogueRefreshFailsCleanlyWithoutAnAccount covers the other honest
// failure: a refresh with nothing to refresh must say so, not report success.
func TestCatalogueRefreshFailsCleanlyWithoutAnAccount(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	if _, errRefresh := catalogueRefresh(testHost(), settings()); errRefresh == nil {
		t.Fatal("a refresh without an account reported success")
	}
}

// TestCatalogueRefreshRefusesWhenDiscoveryIsOff pins that a disabled discovery
// is not silently refreshed from the bundled table.
func TestCatalogueRefreshRefusesWhenDiscoveryIsOff(t *testing.T) {
	catalogueHost(t, remoteModels("MiniMax-M3"))
	cfg := settings()
	cfg.DiscoverModels = false
	if _, errRefresh := catalogueRefresh(testHost(), cfg); errRefresh == nil {
		t.Fatal("a refresh with discovery disabled reported success")
	}
}
