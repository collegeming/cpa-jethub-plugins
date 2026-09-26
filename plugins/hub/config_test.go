package main

import "testing"

func TestConfigFromYAMLDefaults(t *testing.T) {
	cfg := ConfigFromYAML(nil)
	if !cfg.Enabled {
		t.Fatal("enabled should default to true")
	}
	if cfg.HostBaseURL != DefaultHostBaseURL {
		t.Fatalf("host_base_url = %q, want %q", cfg.HostBaseURL, DefaultHostBaseURL)
	}
	if cfg.TimeoutMS != DefaultTimeoutMS {
		t.Fatalf("timeout_ms = %d, want %d", cfg.TimeoutMS, DefaultTimeoutMS)
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("providers = %v, want empty (meaning all)", cfg.Providers)
	}
	if got := len(selectTargets(cfg)); got != len(targetCatalogue()) {
		t.Fatalf("targets = %d, want the whole catalogue (%d)", got, len(targetCatalogue()))
	}
}

func TestConfigFromYAMLBlockAndFlow(t *testing.T) {
	block := []byte("enabled: false\nhost_base_url: http://127.0.0.1:9000\ntimeout_ms: 1500\nproviders: qoder, trae\n")
	cfg := ConfigFromYAML(block)
	if cfg.Enabled {
		t.Fatal("enabled = true, want false")
	}
	if cfg.HostBaseURL != "http://127.0.0.1:9000" {
		t.Fatalf("host_base_url = %q", cfg.HostBaseURL)
	}
	if cfg.TimeoutMS != 1500 {
		t.Fatalf("timeout_ms = %d", cfg.TimeoutMS)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[0] != "qoder" || cfg.Providers[1] != "trae" {
		t.Fatalf("providers = %v", cfg.Providers)
	}

	// Flow style must decode identically: a block-only scanner would silently
	// ignore this document and leave every default in place.
	flow := []byte(`{enabled: no, host_base_url: "https://cpa.example.com/", timeout_ms: "42", providers: [qoder, loomy]}`)
	cfg = ConfigFromYAML(flow)
	if cfg.Enabled {
		t.Fatal("flow: enabled = true, want false (YAML 1.2 'no' written by a user)")
	}
	if cfg.HostBaseURL != "https://cpa.example.com/" {
		t.Fatalf("flow: host_base_url = %q", cfg.HostBaseURL)
	}
	if cfg.TimeoutMS != 42 {
		t.Fatalf("flow: timeout_ms = %d, want 42 (quoted number)", cfg.TimeoutMS)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[1] != "loomy" {
		t.Fatalf("flow: providers = %v", cfg.Providers)
	}
}

func TestConfigFromYAMLBadValuesKeepDefaults(t *testing.T) {
	broken := []byte("host_base_url: \"\"\ntimeout_ms: not-a-number\nproviders: 17\n")
	cfg := ConfigFromYAML(broken)
	if cfg.HostBaseURL != DefaultHostBaseURL {
		t.Fatalf("host_base_url = %q, want the default", cfg.HostBaseURL)
	}
	if cfg.TimeoutMS != DefaultTimeoutMS {
		t.Fatalf("timeout_ms = %d, want the default", cfg.TimeoutMS)
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("providers = %v, want empty", cfg.Providers)
	}

	if cfg := ConfigFromYAML([]byte("::: not yaml :::")); !cfg.Enabled || cfg.HostBaseURL != DefaultHostBaseURL {
		t.Fatalf("undecodable document should fall back to defaults, got %+v", cfg)
	}
}

func TestSelectTargetsHonoursFilter(t *testing.T) {
	cfg := testConfig("trae", " QODER ", "trae", "cline", "nope")
	selected := selectTargets(cfg)
	if len(selected) != 3 {
		t.Fatalf("selected %d targets, want 3 (trae, qoder, cline): %+v", len(selected), selected)
	}
	want := []string{"qoder", "trae", "cline"}
	for index, entry := range selected {
		if entry.ID != want[index] {
			t.Fatalf("target[%d] = %s, want %s", index, entry.ID, want[index])
		}
	}
}

func TestUnsupportedTargetsAreInTheCatalogue(t *testing.T) {
	found := false
	for _, entry := range targetCatalogue() {
		if entry.ID != "cline" {
			continue
		}
		found = true
		if entry.supportsCheckin() {
			t.Fatal("cline must be reported as unsupported: its upstream has no check-in endpoint")
		}
		if entry.Note == "" {
			t.Fatal("cline needs a note explaining why it is unsupported")
		}
	}
	if !found {
		t.Fatal("cline is missing from the catalogue: unsupported providers must be shown, not omitted")
	}
}

func TestConfigFieldsAreDeclared(t *testing.T) {
	want := map[string]string{
		"enabled":       "boolean",
		"host_base_url": "string",
		"timeout_ms":    "integer",
		"providers":     "string",
	}
	fields := ConfigFields()
	if len(fields) != len(want) {
		t.Fatalf("declared %d fields, want %d", len(fields), len(want))
	}
	for _, field := range fields {
		kind, ok := want[field.Name]
		if !ok {
			t.Fatalf("unexpected config field %q", field.Name)
		}
		if field.Type != kind {
			t.Fatalf("field %s type = %s, want %s", field.Name, field.Type, kind)
		}
		if field.Description == "" {
			t.Fatalf("field %s needs a description", field.Name)
		}
		delete(want, field.Name)
	}
	if len(want) != 0 {
		t.Fatalf("fields missing: %v", want)
	}
}

func TestResourceURL(t *testing.T) {
	got := resourceURL("http://127.0.0.1:8317/", "qoder", "/checkin", map[string][]string{
		"action":     {"checkin"},
		"auth_index": {"q1"},
		"format":     {"json"},
	})
	want := "http://127.0.0.1:8317/v0/resource/plugins/qoder/checkin?action=checkin&auth_index=q1&format=json"
	if got != want {
		t.Fatalf("resourceURL = %s, want %s", got, want)
	}
}
