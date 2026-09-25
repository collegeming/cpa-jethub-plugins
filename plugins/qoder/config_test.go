package main

import "testing"

// TestConfigFromYAMLHandlesFlowStyle is the regression guard for a silent
// failure: the host hands the instance subtree back in whichever style the user
// wrote it, and a block-only line scanner ignores flow style entirely, leaving
// every key at its default with no error — including `region` and `wasm_path`,
// which decide the inference path.
func TestConfigFromYAMLHandlesFlowStyle(t *testing.T) {
	cfg := ConfigFromYAML([]byte(`{enabled: true, region: qoder-cn, wasm_path: /opt/qoder.wasm, max_tokens: 4096}`))
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.Region != RegionCN {
		t.Errorf("Region = %q, want %q", cfg.Region, RegionCN)
	}
	if cfg.WASMPath != "/opt/qoder.wasm" {
		t.Errorf("WASMPath = %q, want /opt/qoder.wasm", cfg.WASMPath)
	}
	if cfg.DefaultMaxTokens != 4096 {
		t.Errorf("DefaultMaxTokens = %d, want 4096", cfg.DefaultMaxTokens)
	}
}

// TestConfigFromYAMLFlowAndBlockAgree pins that the two document styles produce
// the same settings.
func TestConfigFromYAMLFlowAndBlockAgree(t *testing.T) {
	flow := ConfigFromYAML([]byte(`{region: qoder-cn, wasm_path: "/w.wasm", max_tokens: 8192, poll_max_failures: 3}`))
	block := ConfigFromYAML([]byte("region: qoder-cn\nwasm_path: /w.wasm\nmax_tokens: 8192\npoll_max_failures: 3\n"))
	if flow.Region != block.Region || flow.WASMPath != block.WASMPath ||
		flow.DefaultMaxTokens != block.DefaultMaxTokens || flow.PollMaxFailures != block.PollMaxFailures {
		t.Fatalf("flow and block documents disagree:\n flow  = %#v\n block = %#v", flow, block)
	}
}

// TestConfigDefaultsArePreservedWhenYAMLIsPartial mirrors the host's own runtime
// document: `enabled: false, priority: 0`.
func TestConfigDefaultsArePreservedWhenYAMLIsPartial(t *testing.T) {
	cfg := ConfigFromYAML([]byte("enabled: false\npriority: 0\n"))
	if cfg.Enabled {
		t.Fatal("Enabled = true, want the explicit false from the document")
	}
	if cfg.Region != RegionGlobal {
		t.Fatalf("Region = %q, want the %q default", cfg.Region, RegionGlobal)
	}
	if cfg.ClientVersion != DefaultClientVersion {
		t.Fatalf("ClientVersion = %q, want %q", cfg.ClientVersion, DefaultClientVersion)
	}
	if cfg.FirstTokenTimeoutMS != FirstTokenTimeoutMS || cfg.ChunkTimeoutMS != ChunkTimeoutMS {
		t.Fatalf("timeouts were lost: %#v", cfg)
	}
	if cfg.PollIntervalMS != PollIntervalMS || cfg.PollMaxFailures != PollMaxFailures ||
		cfg.LoginTimeoutMS != LoginTimeoutMS {
		t.Fatalf("device-code budgets were lost: %#v", cfg)
	}
}

// TestConfigCoercesYAML12Strings covers the YAML 1.2 trap: `no`/`off`/`yes`/`on`
// are plain strings there, so a typed decode into bool would fail and discard the
// whole document.
func TestConfigCoercesYAML12Strings(t *testing.T) {
	cfg := ConfigFromYAML([]byte("enabled: no\nregion: QODER-CN\nmax_tokens: \"2048\"\n"))
	if cfg.Enabled {
		t.Fatal("Enabled = true, want false from \"no\"")
	}
	if cfg.Region != RegionCN {
		t.Errorf("Region = %q, want %q (case-insensitive)", cfg.Region, RegionCN)
	}
	if cfg.DefaultMaxTokens != 2048 {
		t.Errorf("DefaultMaxTokens = %d, want 2048 from a quoted number", cfg.DefaultMaxTokens)
	}

	off := ConfigFromYAML([]byte("enabled: off\n"))
	if off.Enabled {
		t.Error("Enabled = true, want false from \"off\"")
	}
}

// TestConfigRejectsUnknownRegion keeps an unrecognised region from silently
// routing requests to an unknown host.
func TestConfigRejectsUnknownRegion(t *testing.T) {
	cfg := ConfigFromYAML([]byte("region: mars\n"))
	if cfg.Region != RegionGlobal {
		t.Fatalf("Region = %q, want the %q fallback", cfg.Region, RegionGlobal)
	}
	cn := ConfigFromYAML([]byte("region: cn\n"))
	if cn.Region != RegionCN {
		t.Fatalf("Region = %q, want %q for the `cn` spelling", cn.Region, RegionCN)
	}
}

// TestConfigReadsPublicModelsInBothShapes accepts the sequence and the scalar
// spellings, because the host delivers flat scalars.
func TestConfigReadsPublicModelsInBothShapes(t *testing.T) {
	scalar := ConfigFromYAML([]byte("public_models: qwen-flash, qwen-plus\n"))
	if len(scalar.PublicModels) != 2 || scalar.PublicModels[0] != "qwen-flash" {
		t.Fatalf("PublicModels = %v, want two names", scalar.PublicModels)
	}
	sequence := ConfigFromYAML([]byte("public_models:\n  - qwen-flash\n  - qwen-max\n"))
	if len(sequence.PublicModels) != 2 || sequence.PublicModels[1] != "qwen-max" {
		t.Fatalf("PublicModels = %v, want the YAML sequence", sequence.PublicModels)
	}
}

// TestConfigHandlesQuotesAndComments covers scalars a naive line splitter would
// mangle.
func TestConfigHandlesQuotesAndComments(t *testing.T) {
	cfg := ConfigFromYAML([]byte("region: \"qoder-cn\"   # inline comment\nmax_tokens: 2048 # trailing\n"))
	if cfg.Region != RegionCN {
		t.Errorf("Region = %q, want %q", cfg.Region, RegionCN)
	}
	if cfg.DefaultMaxTokens != 2048 {
		t.Errorf("DefaultMaxTokens = %d, want 2048", cfg.DefaultMaxTokens)
	}
}

// TestConfigMalformedKeepsDefaults ensures a broken document does not half-apply.
func TestConfigMalformedKeepsDefaults(t *testing.T) {
	cfg := ConfigFromYAML([]byte("region: qoder-cn\n  bad indent: [unclosed\n"))
	if cfg.Region != RegionGlobal {
		t.Fatalf("Region = %q, want the default after a malformed document", cfg.Region)
	}
}

// TestConfigEmptyDocumentKeepsDefaults keeps a nil subtree safe.
func TestConfigEmptyDocumentKeepsDefaults(t *testing.T) {
	cfg := ConfigFromYAML(nil)
	defaults := DefaultConfig()
	if cfg.Region != defaults.Region || cfg.Enabled != defaults.Enabled ||
		cfg.WASMPath != defaults.WASMPath || cfg.DefaultMaxTokens != defaults.DefaultMaxTokens {
		t.Fatalf("ConfigFromYAML(nil) = %#v, want the defaults", cfg)
	}
}

// TestActiveInferPathFollowsWASMPath is the switch that decides which endpoint a
// request reaches; getting it wrong sends catalog keys to a public endpoint that
// rejects them.
func TestActiveInferPathFollowsWASMPath(t *testing.T) {
	if got := activeInferPath(DefaultConfig()); got != pathPublic {
		t.Fatalf("activeInferPath(default) = %q, want %q", got, pathPublic)
	}
	withWASM := DefaultConfig()
	withWASM.WASMPath = "/opt/qoder-auth-wasm.wasm"
	if got := activeInferPath(withWASM); got != pathEncrypted {
		t.Fatalf("activeInferPath(with wasm) = %q, want %q", got, pathEncrypted)
	}
	blank := DefaultConfig()
	blank.WASMPath = "   "
	if got := activeInferPath(blank); got != pathPublic {
		t.Fatalf("activeInferPath(blank) = %q, want %q", got, pathPublic)
	}
}

// TestConfigFieldsCoverEverySetting keeps the management UI in step with the
// settings the parser reads.
func TestConfigFieldsCoverEverySetting(t *testing.T) {
	names := map[string]bool{}
	for _, field := range ConfigFields() {
		names[field.Name] = true
		if field.Description == "" {
			t.Errorf("config field %q has no description", field.Name)
		}
		if field.Type == "enum" && len(field.EnumValues) == 0 {
			t.Errorf("enum field %q has no values", field.Name)
		}
	}
	for _, required := range []string{
		"region", "wasm_path", "client_version", "session_type", "public_models",
		"max_tokens", "first_token_timeout_ms", "chunk_timeout_ms",
		"request_timeout_ms", "login_timeout_ms", "poll_interval_ms", "poll_max_failures",
	} {
		if !names[required] {
			t.Errorf("config field %q is missing from ConfigFields", required)
		}
	}
}
