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

// TestConfigFromYAMLHandlesFlowStyle is the regression guard for a silent
// failure: the host hands the instance subtree back in the style the user wrote
// it, and a block-only line scanner ignored flow style entirely, leaving every
// key at its default with no error.
func TestConfigFromYAMLHandlesFlowStyle(t *testing.T) {
	cfg := ConfigFromYAML([]byte("{enabled: true, discover_models: false, flow: ticket, max_tokens: 4096}"))
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.DiscoverModels {
		t.Error("DiscoverModels = true, want false (flow style must be honoured)")
	}
	if cfg.Flow != LoginFlowTicket {
		t.Errorf("Flow = %q, want %q", cfg.Flow, LoginFlowTicket)
	}
	if cfg.DefaultMaxTokens != 4096 {
		t.Errorf("DefaultMaxTokens = %d, want 4096", cfg.DefaultMaxTokens)
	}
}

// TestConfigFromYAMLFlowAndBlockAgree pins that the two styles are equivalent.
func TestConfigFromYAMLFlowAndBlockAgree(t *testing.T) {
	flow := ConfigFromYAML([]byte("{flow: ticket, discover_models: false, max_tokens: 8192, tool_stream: false}"))
	block := ConfigFromYAML([]byte("flow: ticket\ndiscover_models: false\nmax_tokens: 8192\ntool_stream: false\n"))
	if flow.Flow != block.Flow || flow.DiscoverModels != block.DiscoverModels ||
		flow.DefaultMaxTokens != block.DefaultMaxTokens || flow.ToolStream != block.ToolStream {
		t.Fatalf("flow and block documents disagree:\n flow  = %#v\n block = %#v", flow, block)
	}
}

// TestConfigFromYAMLQuotedAndCommentedValues covers scalars that a naive
// comma/comment split would mangle.
func TestConfigFromYAMLQuotedAndCommentedValues(t *testing.T) {
	cfg := ConfigFromYAML([]byte("flow: \"oauth\"   # inline comment\nmax_tokens: 2048 # trailing\n"))
	if cfg.Flow != LoginFlowOAuth {
		t.Errorf("Flow = %q, want %q", cfg.Flow, LoginFlowOAuth)
	}
	if cfg.DefaultMaxTokens != 2048 {
		t.Errorf("DefaultMaxTokens = %d, want 2048", cfg.DefaultMaxTokens)
	}
}

// TestConfigFromYAMLMalformedKeepsDefaults ensures a broken document does not
// half-apply on top of values that did parse.
func TestConfigFromYAMLMalformedKeepsDefaults(t *testing.T) {
	cfg := ConfigFromYAML([]byte("flow: ticket\n  bad indent: [unclosed\n"))
	if cfg.Flow != LoginFlowOAuth {
		t.Fatalf("Flow = %q, want the default after a malformed document", cfg.Flow)
	}
	if cfg.DefaultMaxTokens != 65536 {
		t.Fatalf("DefaultMaxTokens = %d, want the default 65536", cfg.DefaultMaxTokens)
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
