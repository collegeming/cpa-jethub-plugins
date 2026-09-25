package main

import (
	"testing"
	"time"
)

// TestConfigDefaultsArePreservedWhenYAMLIsPartial mirrors the guarantee Jet-Hub
// makes: `enabled: false, priority: 0` is the host's default runtime document,
// and a partial document must not silently drop the opt-out defaults.
func TestConfigDefaultsArePreservedWhenYAMLIsPartial(t *testing.T) {
	cfg := ConfigFromYAML([]byte("enabled: false\npriority: 0\n"))
	if cfg.Enabled {
		t.Fatal("Enabled = true, want the explicit false from the document")
	}
	if !cfg.DiscoverModels || !cfg.BenefitModels || !cfg.ToolStream || !cfg.PromptCacheKey {
		t.Fatalf("opt-out defaults were lost: %#v", cfg)
	}
	if cfg.Flow != LoginFlowOAuth {
		t.Fatalf("Flow = %q, want the %q default", cfg.Flow, LoginFlowOAuth)
	}
	if cfg.DefaultMaxTokens != 65536 {
		t.Fatalf("DefaultMaxTokens = %d, want 65536", cfg.DefaultMaxTokens)
	}
}

func TestConfigFromYAMLReadsFlatScalars(t *testing.T) {
	document := []byte(`
# CodeArts instance settings
enabled: true
priority: 7
flow: ticket
discover_models: no
benefit_models: off
max_tokens: 4096
first_token_timeout_ms: 120000
chunk_timeout_ms: "180000"
model_cache_ttl_ms: 60000
`)
	cfg := ConfigFromYAML(document)
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.Priority != 7 {
		t.Errorf("Priority = %d, want 7", cfg.Priority)
	}
	if cfg.Flow != LoginFlowTicket {
		t.Errorf("Flow = %q, want %q", cfg.Flow, LoginFlowTicket)
	}
	if cfg.DiscoverModels {
		t.Error("DiscoverModels = true, want false (\"no\")")
	}
	if cfg.BenefitModels {
		t.Error("BenefitModels = true, want false (\"off\")")
	}
	if cfg.DefaultMaxTokens != 4096 {
		t.Errorf("DefaultMaxTokens = %d, want 4096", cfg.DefaultMaxTokens)
	}
	if cfg.FirstTokenTimeoutMS != 120000 {
		t.Errorf("FirstTokenTimeoutMS = %d, want 120000", cfg.FirstTokenTimeoutMS)
	}
	if cfg.ChunkTimeoutMS != 180000 {
		t.Errorf("ChunkTimeoutMS = %d, want 180000 (quoted value)", cfg.ChunkTimeoutMS)
	}
	if cfg.ModelCacheTTLMS != 60000 {
		t.Errorf("ModelCacheTTLMS = %d, want 60000", cfg.ModelCacheTTLMS)
	}
}

func TestConfigFromYAMLIgnoresNestedMappings(t *testing.T) {
	document := []byte(`
enabled: true
extra:
  nested: value
  deeper:
    - item
max_tokens: 8192
`)
	cfg := ConfigFromYAML(document)
	if cfg.DefaultMaxTokens != 8192 {
		t.Fatalf("DefaultMaxTokens = %d, want 8192 (nested keys must not shadow flat ones)", cfg.DefaultMaxTokens)
	}
}

func TestConfigRejectsUnknownFlowValue(t *testing.T) {
	cfg := ConfigFromYAML([]byte("flow: telepathy\n"))
	if cfg.Flow != LoginFlowOAuth {
		t.Fatalf("Flow = %q, want the %q fallback", cfg.Flow, LoginFlowOAuth)
	}
}

func TestParseFlatYAMLHandlesCommentsAndQuotes(t *testing.T) {
	values := parseFlatYAML([]byte(`
flow: "oauth"   # inline comment after a quoted value
priority: 3 # unquoted trailing comment
# whole-line comment
empty:
`))
	if values["flow"] != "oauth" {
		t.Errorf("flow = %q, want oauth", values["flow"])
	}
	if values["priority"] != "3" {
		t.Errorf("priority = %q, want 3", values["priority"])
	}
	if _, present := values["empty"]; present {
		t.Error("a key with no scalar value must be skipped (nested block header)")
	}
}

func TestCredentialExpiryFallbackAndRefreshability(t *testing.T) {
	parsed := &Credential{ExpiresAt: "2026-09-25T12:00:00Z"}
	if parsed.Expiry().IsZero() {
		t.Fatal("Expiry() returned the zero time for a valid RFC3339 stamp")
	}
	if parsed.Refreshable() {
		t.Fatal("a credential without refresh_token/code_verifier/DPoP key must not be refreshable")
	}

	unparseable := &Credential{ExpiresAt: "not-a-timestamp"}
	if got := unparseable.Expiry(); got.Before(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("unparseable expiry fell back to %v, want ~24h from now", got)
	}
}

func TestParseCredentialRejectsMissingKeys(t *testing.T) {
	if _, err := ParseCredential([]byte(`{"access_key_id":"AK"}`)); err == nil {
		t.Fatal("ParseCredential accepted a credential without secret_access_key")
	}
	if _, err := ParseCredential([]byte(`{"access_key_id":"AK","secret_access_key":"SK"}`)); err != nil {
		t.Fatalf("ParseCredential rejected a complete credential: %v", err)
	}
	if _, err := ParseCredential(nil); err == nil {
		t.Fatal("ParseCredential accepted an empty payload")
	}
}
