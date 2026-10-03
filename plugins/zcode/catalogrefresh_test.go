package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogueHost installs a host with one account whose `client/configs` answer
// is controlled by the test.
func catalogueHost(t *testing.T, handler func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error)) *fakeHost {
	t.Helper()
	fake := newFakeHost()
	fake.install(t)
	storedCredential(t, fake, "idx-1", "zcode-user.json", sampleCredential())
	fake.do = handler
	withSettings(t, DefaultConfig())
	return fake
}

// configsBody renders the `client/configs` envelope around the given model ids.
func configsBody(ids ...string) string {
	return configsBodyWith(ids, 1_000_000, 128_000)
}

// configsBodyWith renders the same envelope with explicit context window and
// output cap, so a test can make the remote metadata differ from the built-in
// table — which is exactly what the discovery exists to correct.
func configsBodyWith(ids []string, contextWindow, maxOutput int) string {
	models := make([]string, 0, len(ids))
	for _, id := range ids {
		models = append(models, `"`+id+`":{"modelId":"`+id+`","contextWindow":`+
			strconv.Itoa(contextWindow)+`,"maxCompletionTokens":`+strconv.Itoa(maxOutput)+`}`)
	}
	return `{"code":0,"data":{"builtinModels":{` + strings.Join(models, ",") + `}}}`
}

// TestCatalogueRefreshReportsUpstreamFailure pins the rule the shared package
// states: when the vendor never answered, the refresh must report a failure and
// must NOT publish. refreshRemoteCatalogue returns the fetch failure; the
// built-in table that a client request would then serve is not evidence the
// vendor answered.
func TestCatalogueRefreshReportsUpstreamFailure(t *testing.T) {
	fake := catalogueHost(t, func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, ClientConfigsPath) {
			return nil, errFakeTransport
		}
		return httpResponse(404, `{}`), nil
	})

	outcome, errRefresh := catalogueRefresh(testHost(), settings())
	if errRefresh == nil {
		t.Fatalf("a failed refresh reported success: %+v", outcome)
	}
	if outcome.Models != 0 || outcome.Changed {
		t.Fatalf("a failed refresh reported an outcome: %+v", outcome)
	}
	if len(fake.callsFor(ClientConfigsPath)) == 0 {
		t.Fatal("the refresh never contacted client/configs: the failure was not an upstream failure")
	}
	if peekDiscoveredModels() != nil {
		t.Fatal("a failed refresh left something in the cache, so the next request would serve it")
	}
	if names := fake.savedNames(); len(names) != 0 {
		t.Fatalf("a failed refresh published to the host: %v", names)
	}
}

// TestCatalogueRefreshKeepsThePreviousListingOnFailure pins that a failed
// refresh is not destructive: a client request keeps whatever the last
// successful fetch produced.
func TestCatalogueRefreshKeepsThePreviousListingOnFailure(t *testing.T) {
	fake := catalogueHost(t, func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, configsBody("GLM-5.3-Flash", "GLM-5.3")), nil
	})
	if _, errRefresh := catalogueRefresh(testHost(), settings()); errRefresh != nil {
		t.Fatalf("first refresh: %v", errRefresh)
	}
	before := catalogueIDSet(currentCatalogue())

	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	if _, errRefresh := catalogueRefresh(testHost(), settings()); errRefresh == nil {
		t.Fatal("a failed refresh reported success")
	}
	if after := catalogueIDSet(currentCatalogue()); !sameIDSet(before, after) {
		t.Fatalf("catalogue after a failed refresh = %v, want the previous %v", after, before)
	}
}

// TestCatalogueRefreshComputesChanged pins the before/after comparison.
//
// ⚠️ The id set this plugin can publish is pinned to the two verified ids by
// `restrictToVerified`, so an id-only comparison could never report the change
// that matters here: what a successful fetch corrects is the METADATA. These
// cases therefore vary the remote context window / output cap, which is the
// difference models.go records between the built-in table and upstream.
func TestCatalogueRefreshComputesChanged(t *testing.T) {
	fake := catalogueHost(t, func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, configsBody("GLM-5.3-Flash", "GLM-5.3")), nil
	})

	// Cold start against metadata that differs from the built-in table: this is a
	// real change, and it is what must reach /v1/models on the first tick.
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, configsBodyWith([]string{"GLM-5.3-Flash", "GLM-5.3"}, 2_000_000, 256_000)), nil
	}
	outcome, errRefresh := catalogueRefresh(testHost(), settings())
	if errRefresh != nil {
		t.Fatalf("first refresh: %v", errRefresh)
	}
	if outcome.Models != len(fallbackCatalogue) || !outcome.Changed {
		t.Fatalf("first refresh = %+v, want %d models marked changed (upstream metadata differs)",
			outcome, len(fallbackCatalogue))
	}

	// The same listing again: nothing moved.
	outcome, errRefresh = catalogueRefresh(testHost(), settings())
	if errRefresh != nil {
		t.Fatalf("second refresh: %v", errRefresh)
	}
	if outcome.Models != len(fallbackCatalogue) || outcome.Changed {
		t.Fatalf("identical refresh = %+v, want no change", outcome)
	}

	// A listing that drops one verified id is a real change.
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, configsBodyWith([]string{"GLM-5.3-Flash"}, 2_000_000, 256_000)), nil
	}
	outcome, errRefresh = catalogueRefresh(testHost(), settings())
	if errRefresh != nil {
		t.Fatalf("third refresh: %v", errRefresh)
	}
	if outcome.Models != 1 || !outcome.Changed {
		t.Fatalf("shrunk refresh = %+v, want 1 model marked changed", outcome)
	}
	// The refresh function itself never publishes: an automatic tick and the
	// manual button call the same function, and only the manual path adds the
	// AuthName that makes catalog.Run rewrite an auth file.
	if len(fake.savedNames()) != 0 {
		t.Fatalf("the refresh function published on its own: %v", fake.savedNames())
	}
}

// TestCatalogueRefreshReportsNoChangeForAnIdenticalListing pins the other side:
// when upstream returns exactly what the built-in table already carries, the
// refresh is honest about having moved nothing — which is what keeps the
// automatic path from rewriting an auth file on every tick.
func TestCatalogueRefreshReportsNoChangeForAnIdenticalListing(t *testing.T) {
	catalogueHost(t, func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		// The built-in values, so the fetch confirms them rather than changing them.
		return httpResponse(200, configsBody("GLM-5.3-Flash")), nil
	})
	// Seed a first successful fetch, then repeat it.
	if _, errSeed := catalogueRefresh(testHost(), settings()); errSeed != nil {
		t.Fatalf("seed refresh: %v", errSeed)
	}
	outcome, errRefresh := catalogueRefresh(testHost(), settings())
	if errRefresh != nil {
		t.Fatalf("repeat refresh: %v", errRefresh)
	}
	if outcome.Changed {
		t.Fatalf("an identical listing was reported as changed: %+v", outcome)
	}
}

// TestCatalogueRefreshFailsCleanlyWithoutAnAccount covers the other honest
// failure: a refresh with nothing to refresh must say so, not report success.
func TestCatalogueRefreshFailsCleanlyWithoutAnAccount(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	withSettings(t, DefaultConfig())
	if _, errRefresh := catalogueRefresh(testHost(), settings()); errRefresh == nil {
		t.Fatal("a refresh without an account reported success")
	}
}

// TestCatalogueRefreshRefusesWhenDiscoveryIsOff pins that a disabled discovery
// is not silently refreshed from the built-in table.
func TestCatalogueRefreshRefusesWhenDiscoveryIsOff(t *testing.T) {
	catalogueHost(t, func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, configsBody("GLM-5.3")), nil
	})
	cfg := settings()
	cfg.DiscoverModels = false
	if _, errRefresh := catalogueRefresh(testHost(), cfg); errRefresh == nil {
		t.Fatal("a refresh with discovery disabled reported success")
	}
}
