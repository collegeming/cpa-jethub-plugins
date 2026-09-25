package main

import (
	"strings"
	"testing"
)

// TestConfigDefaultsArePreservedWhenYAMLIsPartial guards the opt-out defaults:
// a partial document must not silently disable features that default to on.
func TestConfigDefaultsArePreservedWhenYAMLIsPartial(t *testing.T) {
	cfg := ConfigFromYAML([]byte("enabled: true\n"))
	if !cfg.Enabled || !cfg.DiscoverModels || !cfg.DailyCheckin {
		t.Fatalf("opt-out defaults were lost: %#v", cfg)
	}
	if cfg.DefaultMaxTokens != 0 {
		t.Fatalf("DefaultMaxTokens = %d, want 0 (no invented default)", cfg.DefaultMaxTokens)
	}
	if cfg.ClientVersionOverride != "" {
		t.Fatalf("ClientVersionOverride = %q, want empty", cfg.ClientVersionOverride)
	}
	if cfg.ModelCacheTTLMS != DefaultConfig().ModelCacheTTLMS {
		t.Fatalf("ModelCacheTTLMS = %d", cfg.ModelCacheTTLMS)
	}
}

// TestConfigFromYAMLHandlesFlowStyle is the regression guard for a silent
// failure: the host hands the instance subtree back in the style the user wrote
// it, so a block-only line scanner would ignore a flow-style mapping entirely
// and leave every key at its default with no error.
func TestConfigFromYAMLHandlesFlowStyle(t *testing.T) {
	cfg := ConfigFromYAML([]byte("{enabled: true, discover_models: false, daily_checkin: false, client_version: 2026.9.4, max_tokens: 4096}"))
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.DiscoverModels {
		t.Error("DiscoverModels = true, want false (flow style must be honoured)")
	}
	if cfg.DailyCheckin {
		t.Error("DailyCheckin = true, want false")
	}
	if cfg.ClientVersionOverride != "2026.9.4" {
		t.Errorf("ClientVersionOverride = %q, want 2026.9.4", cfg.ClientVersionOverride)
	}
	if cfg.DefaultMaxTokens != 4096 {
		t.Errorf("DefaultMaxTokens = %d, want 4096", cfg.DefaultMaxTokens)
	}
}

// TestConfigFromYAMLFlowAndBlockAgree pins that the two styles are equivalent.
func TestConfigFromYAMLFlowAndBlockAgree(t *testing.T) {
	flow := ConfigFromYAML([]byte("{discover_models: false, daily_checkin: false, max_tokens: 8192, model_cache_ttl_ms: 1000}"))
	block := ConfigFromYAML([]byte("discover_models: false\ndaily_checkin: false\nmax_tokens: 8192\nmodel_cache_ttl_ms: 1000\n"))
	if flow != block {
		t.Fatalf("flow and block documents disagree:\n flow  = %#v\n block = %#v", flow, block)
	}
}

// TestConfigFromYAMLReadsFlatScalars covers the documented spellings, including
// YAML 1.2 string spellings of booleans and a quoted number.
func TestConfigFromYAMLReadsFlatScalars(t *testing.T) {
	document := []byte(`
# LobsterAI instance settings
enabled: true
discover_models: no
daily_checkin: off
client_version: "2026.9.4"
max_tokens: 4096
model_cache_ttl_ms: "60000"
request_timeout_ms: 15000
`)
	cfg := ConfigFromYAML(document)
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.DiscoverModels {
		t.Error("DiscoverModels = true, want false (\"no\")")
	}
	if cfg.DailyCheckin {
		t.Error("DailyCheckin = true, want false (\"off\")")
	}
	if cfg.ClientVersionOverride != "2026.9.4" {
		t.Errorf("ClientVersionOverride = %q, want 2026.9.4", cfg.ClientVersionOverride)
	}
	if cfg.DefaultMaxTokens != 4096 {
		t.Errorf("DefaultMaxTokens = %d, want 4096", cfg.DefaultMaxTokens)
	}
	if cfg.ModelCacheTTLMS != 60000 {
		t.Errorf("ModelCacheTTLMS = %d, want 60000 (quoted value)", cfg.ModelCacheTTLMS)
	}
	if cfg.RequestTimeoutMS != 15000 {
		t.Errorf("RequestTimeoutMS = %d, want 15000", cfg.RequestTimeoutMS)
	}
}

// TestConfigFromYAMLQuotedAndCommentedValues covers scalars that a naive
// comma/comment split would mangle.
func TestConfigFromYAMLQuotedAndCommentedValues(t *testing.T) {
	cfg := ConfigFromYAML([]byte("client_version: \"2026.1.1\"   # inline comment\nmax_tokens: 2048 # trailing\n"))
	if cfg.ClientVersionOverride != "2026.1.1" {
		t.Errorf("ClientVersionOverride = %q, want 2026.1.1", cfg.ClientVersionOverride)
	}
	if cfg.DefaultMaxTokens != 2048 {
		t.Errorf("DefaultMaxTokens = %d, want 2048", cfg.DefaultMaxTokens)
	}
}

// TestConfigFromYAMLIgnoresNestedMappings checks that a nested key of the same
// name cannot shadow the flat one.
func TestConfigFromYAMLIgnoresNestedMappings(t *testing.T) {
	document := []byte(`
enabled: true
extra:
  discover_models: false
  max_tokens: 1
discover_models: false
max_tokens: 8192
`)
	cfg := ConfigFromYAML(document)
	if cfg.DefaultMaxTokens != 8192 {
		t.Fatalf("DefaultMaxTokens = %d, want 8192", cfg.DefaultMaxTokens)
	}
	if cfg.DiscoverModels {
		t.Fatal("DiscoverModels = true, want the flat false")
	}
}

// TestConfigFromYAMLMalformedKeepsDefaults ensures a broken document falls back
// to defaults instead of half-applying.
func TestConfigFromYAMLMalformedKeepsDefaults(t *testing.T) {
	cfg := ConfigFromYAML([]byte("discover_models: false\n  bad indent: [unclosed\n"))
	if cfg.DiscoverModels != DefaultConfig().DiscoverModels {
		t.Fatalf("DiscoverModels = %v, want the default after a malformed document", cfg.DiscoverModels)
	}
}

// TestConfigFromYAMLOneBadValueOnlyCostsItsOwnDefault is the property a typed
// decode cannot provide: an unusable value must not discard the rest.
func TestConfigFromYAMLOneBadValueOnlyCostsItsOwnDefault(t *testing.T) {
	cfg := ConfigFromYAML([]byte("max_tokens: not-a-number\ndiscover_models: false\n"))
	if cfg.DefaultMaxTokens != DefaultConfig().DefaultMaxTokens {
		t.Fatalf("DefaultMaxTokens = %d, want the default", cfg.DefaultMaxTokens)
	}
	if cfg.DiscoverModels {
		t.Fatal("a bad sibling value must not discard discover_models")
	}
}

func TestCoerceHelpers(t *testing.T) {
	if !coerceBool(nil, true) || coerceBool(nil, false) {
		t.Fatal("coerceBool(nil) must return the fallback")
	}
	for _, value := range []any{true, 1, int64(1), 2.5, "yes", "on", "TRUE", " 1 "} {
		if !coerceBool(value, false) {
			t.Fatalf("coerceBool(%#v) = false, want true", value)
		}
	}
	for _, value := range []any{false, 0, int64(0), 0.0, "no", "off", "false", "0"} {
		if coerceBool(value, true) {
			t.Fatalf("coerceBool(%#v) = true, want false", value)
		}
	}
	if !coerceBool("maybe", true) {
		t.Fatal("an unknown boolean spelling must fall back")
	}
	if coerceInt(" 42 ", 0) != 42 || coerceInt(7, 0) != 7 || coerceInt(int64(8), 0) != 8 || coerceInt(9.9, 0) != 9 {
		t.Fatal("coerceInt mishandled a value")
	}
	if coerceInt("nope", 5) != 5 || coerceInt(nil, 5) != 5 {
		t.Fatal("coerceInt must fall back")
	}
	if coerceString(" x ", "fallback") != "x" || coerceString("  ", "fallback") != "fallback" ||
		coerceString(12, "fallback") != "fallback" || coerceString(nil, "fallback") != "fallback" {
		t.Fatal("coerceString mishandled a value")
	}
}

func TestConfigFieldsAreNonEmpty(t *testing.T) {
	fields := ConfigFields()
	if len(fields) == 0 {
		t.Fatal("ConfigFields() is empty; the host would render no settings")
	}
	for _, field := range fields {
		if field.Name == "" || field.Type == "" || field.Description == "" {
			t.Fatalf("incomplete config field: %+v", field)
		}
	}
	if len(configFieldsForHost()) != len(fields) {
		t.Fatal("configFieldsForHost() dropped fields")
	}
	for _, field := range configFieldsForHost() {
		if field.Name == "" {
			t.Fatalf("host config field without a name: %+v", field)
		}
	}
}

func TestProviderConstants(t *testing.T) {
	if ProviderKey != "lobsterai" {
		t.Fatalf("ProviderKey = %q, want lobsterai", ProviderKey)
	}
	// The client capabilities header is a model-list admission condition; both
	// values must stay present.
	for _, capability := range []string{"kimi-k3-agentic-v1", "thinking-level-control-v1"} {
		if !strings.Contains(ClientCapabilities, capability) {
			t.Fatalf("ClientCapabilities %q is missing %q", ClientCapabilities, capability)
		}
	}
	if APIBase != "https://lobsterai-server.youdao.com" || PortalBase != "https://lobsterai.youdao.com" {
		t.Fatalf("unexpected hosts: api=%s portal=%s", APIBase, PortalBase)
	}
	if ExchangePath != "/api/auth/exchange" || RefreshPath != "/api/auth/refresh" ||
		ModelsPath != "/api/models/available" || ChatPath != "/api/proxy/v1/chat/completions" ||
		CallbackPath != "/auth/callback" {
		t.Fatal("endpoint constants drifted from lobsterai.ts")
	}
}
