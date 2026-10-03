package main

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// errTransport simulates an unreachable vendor endpoint.
var errTransport = errors.New("fake transport failure")

// catalogueHost installs a host with one account whose `GET /models` answer is
// controlled by the test.
func catalogueHost(t *testing.T, modelsBody string) *fakeHost {
	t.Helper()
	host := newFakeHost()
	host.auths["idx-1"] = []byte(`{"access_token":"mmoat_test","token_type":"Bearer"}`)
	host.files = []pluginapi.HostAuthFileEntry{
		{AuthIndex: "idx-1", Name: "minimax-work.json", Provider: ProviderKey, Type: ProviderKey},
	}
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, ModelsPath) {
			return httpResponse(200, modelsBody), nil
		}
		return httpResponse(404, `{"message":"unexpected"}`), nil
	}
	host.install(t)
	resetDiscoveredModels()
	t.Cleanup(resetDiscoveredModels)
	withSettings(t, DefaultConfig())
	return host
}

// modelsBody builds the measured `GET /models` envelope around a set of ids.
func modelsBody(ids ...string) string {
	models := make([]string, 0, len(ids))
	for _, id := range ids {
		models = append(models, `"`+id+`":{"name":"`+id+`","context_window_options":[512000]}`)
	}
	return `{"providers":[{"providerId":"minimax","config":{"models":{` +
		strings.Join(models, ",") + `},"model_order":[` + quoteJoin(ids) + `]}}]}`
}

// quoteJoin renders ids as a JSON string array.
func quoteJoin(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, `"`+id+`"`)
	}
	return strings.Join(out, ",")
}

// requestedCatalogue reports whether the host saw a call to the catalogue
// endpoint, proving the failure came from upstream rather than from a guard that
// never dialled at all.
func requestedCatalogue(host *fakeHost) bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	for _, request := range host.requests {
		if strings.Contains(request.URL, ModelsPath) {
			return true
		}
	}
	return false
}

// TestCatalogueRefreshReportsUpstreamFailure pins the rule the shared package
// states: when the vendor never answered, the refresh must report a failure and
// must NOT publish. discoverModels returns nil on every failure so the CALLER
// falls back to the bundled table, and that fallback must never be reported as a
// successful refresh.
func TestCatalogueRefreshReportsUpstreamFailure(t *testing.T) {
	host := catalogueHost(t, "")
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, ModelsPath) {
			return nil, errTransport
		}
		return httpResponse(404, `{}`), nil
	}

	outcome, errRefresh := catalogueRefresh(testHost(), DefaultConfig())
	if errRefresh == nil {
		t.Fatalf("a failed refresh reported success: %+v", outcome)
	}
	if outcome.Models != 0 || outcome.Changed {
		t.Fatalf("a failed refresh reported an outcome: %+v", outcome)
	}
	if len(host.calls) == 0 {
		t.Fatal("the fake host was never called")
	}
	if !requestedCatalogue(host) {
		t.Fatal("the refresh never contacted the catalogue endpoint: the failure was not an upstream failure")
	}
	if peekDiscoveredModels() != nil {
		t.Fatal("a failed refresh left something in the cache, so the next request would serve it")
	}
	// Nothing is published when the refresh failed: the manual page and the JSON
	// route both go through catalog.Run, which stops before Publish on error.
	page := catalogueRefreshPage(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/minimax/status", Query: url.Values{"action": {"refresh-catalog"}},
	})
	if !strings.Contains(string(page.Body), "刷新失败") {
		t.Fatalf("a failed refresh did not say so: %s", string(page.Body))
	}
	if len(host.saved) != 0 {
		t.Fatalf("a failed refresh published to the host: %v", host.saved)
	}
}

// TestCatalogueRefreshComputesChanged pins the before/after comparison: the
// first successful fetch moves the catalogue (changed), an identical fetch does
// not, and a different listing does.
func TestCatalogueRefreshComputesChanged(t *testing.T) {
	host := catalogueHost(t, modelsBody("MiniMax-M3"))

	outcome, errRefresh := catalogueRefresh(testHost(), DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("first refresh: %v", errRefresh)
	}
	if outcome.Models != 1 || !outcome.Changed {
		t.Fatalf("first refresh = %+v, want 1 model marked changed", outcome)
	}

	outcome, errRefresh = catalogueRefresh(testHost(), DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("second refresh: %v", errRefresh)
	}
	if outcome.Models != 1 || outcome.Changed {
		t.Fatalf("identical refresh = %+v, want no change", outcome)
	}

	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, ModelsPath) {
			return httpResponse(200, modelsBody("MiniMax-M3", "MiniMax-M3.1-Flash-Preview")), nil
		}
		return httpResponse(404, `{}`), nil
	}
	outcome, errRefresh = catalogueRefresh(testHost(), DefaultConfig())
	if errRefresh != nil {
		t.Fatalf("third refresh: %v", errRefresh)
	}
	if outcome.Models != 2 || !outcome.Changed {
		t.Fatalf("grown refresh = %+v, want 2 models marked changed", outcome)
	}
	// The refresh function itself never publishes: an automatic tick and the
	// manual button call the same function, and only the manual path adds the
	// AuthName that makes catalog.Run rewrite an auth file.
	if len(host.saved) != 0 {
		t.Fatalf("the refresh function published on its own: %v", host.saved)
	}
}

// TestCatalogueRefreshDoesNotReportTheFallbackAsFresh pins the trap directly:
// after a failed refresh the served catalogue is the bundled table, and the
// refresh must still be an error rather than "the vendor sent the fallback".
func TestCatalogueRefreshDoesNotReportTheFallbackAsFresh(t *testing.T) {
	host := catalogueHost(t, modelsBody("MiniMax-M3"))
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(500, `{"message":"boom"}`), nil
	}
	if _, errRefresh := catalogueRefresh(testHost(), DefaultConfig()); errRefresh == nil {
		t.Fatal("an HTTP 500 refresh reported success")
	}
	// The serving path still answers with the bundled table, which is correct for
	// a client request — that is exactly why the refresh cannot use it as proof.
	served := catalogueEntries(testHost(), &Credential{AccessToken: "mmoat_test"}, DefaultConfig())
	if len(served) == 0 {
		t.Fatal("the serving path lost its fallback")
	}
}

// TestCatalogueRefreshFailsCleanlyWithoutAnAccount covers the other honest
// failure: a refresh with nothing to refresh must say so, not report success.
func TestCatalogueRefreshFailsCleanlyWithoutAnAccount(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	resetDiscoveredModels()
	t.Cleanup(resetDiscoveredModels)
	if _, errRefresh := catalogueRefresh(testHost(), DefaultConfig()); errRefresh == nil {
		t.Fatal("a refresh without an account reported success")
	}
}

// TestCatalogueRefreshRefusesWhenDiscoveryIsOff pins that a disabled discovery
// is not silently refreshed from the bundled table.
func TestCatalogueRefreshRefusesWhenDiscoveryIsOff(t *testing.T) {
	catalogueHost(t, modelsBody("MiniMax-M3"))
	cfg := DefaultConfig()
	cfg.DiscoverModels = false
	if _, errRefresh := catalogueRefresh(testHost(), cfg); errRefresh == nil {
		t.Fatal("a refresh with discovery disabled reported success")
	}
}
