package main

import (
	"testing"
)

// TestConfigDefaults pins the documented defaults.
func TestConfigDefaults(t *testing.T) {
	cfg := ConfigFromYAML(nil)
	if !cfg.Enabled || !cfg.ModelDiscovery {
		t.Fatalf("enabled/model_discovery must default to true: %+v", cfg)
	}
	if cfg.LoginTimeoutMS != DeviceCodeTTLMS {
		t.Errorf("login timeout = %d, want %d", cfg.LoginTimeoutMS, DeviceCodeTTLMS)
	}
	if cfg.PollIntervalMS != DevicePollIntervalMS {
		t.Errorf("poll interval = %d, want %d", cfg.PollIntervalMS, DevicePollIntervalMS)
	}
	if cfg.PollMaxFailures != PollMaxFailures {
		t.Errorf("poll failures = %d, want %d", cfg.PollMaxFailures, PollMaxFailures)
	}
	if cfg.MaxOutputTokens != MaxOutputTokensCeiling {
		t.Errorf("max output tokens = %d, want %d", cfg.MaxOutputTokens, MaxOutputTokensCeiling)
	}
	if cfg.BalanceDivisor != balanceDivisorRaw {
		t.Errorf("balance divisor = %v, want %v", cfg.BalanceDivisor, float64(balanceDivisorRaw))
	}
	if cfg.DefaultReasoningEffort != "" || cfg.DefaultMaxTokens != 0 {
		t.Errorf("the request-shaping defaults must stay opt-in: %+v", cfg)
	}
}

// TestConfigFromYAMLStyles covers the two YAML styles a user can write and the
// scalar spellings YAML 1.2 sees as strings.
func TestConfigFromYAMLStyles(t *testing.T) {
	block := `
enabled: no
priority: 7
model_discovery: off
model_cache_ttl_ms: 0
max_tokens: "4096"
max_output_tokens: 100000
reasoning_effort: LOW
login_timeout_ms: 60000
poll_interval_ms: 1000
poll_max_failures: 2
balance_divisor: 200000
`
	flow := `{enabled: false, priority: 7, model_discovery: false, model_cache_ttl_ms: 0, max_tokens: 4096, max_output_tokens: 100000, reasoning_effort: LOW, login_timeout_ms: 60000, poll_interval_ms: 1000, poll_max_failures: 2, balance_divisor: 200000}`

	for name, document := range map[string]string{"block": block, "flow": flow} {
		t.Run(name, func(t *testing.T) {
			cfg := ConfigFromYAML([]byte(document))
			if cfg.Enabled {
				t.Error("`no`/`false` must disable the plugin")
			}
			if cfg.Priority != 7 {
				t.Errorf("priority = %d", cfg.Priority)
			}
			if cfg.ModelDiscovery {
				t.Error("`off`/`false` must disable model discovery")
			}
			if cfg.ModelCacheTTLMS != 0 {
				t.Errorf("model_cache_ttl_ms = %d, want 0", cfg.ModelCacheTTLMS)
			}
			if cfg.DefaultMaxTokens != 4096 {
				t.Errorf("max_tokens = %d, want 4096 (quoted numbers must work)", cfg.DefaultMaxTokens)
			}
			if cfg.MaxOutputTokens != 100_000 {
				t.Errorf("max_output_tokens = %d", cfg.MaxOutputTokens)
			}
			if cfg.DefaultReasoningEffort != "LOW" {
				t.Errorf("reasoning_effort = %q", cfg.DefaultReasoningEffort)
			}
			if cfg.LoginTimeoutMS != 60_000 || cfg.PollIntervalMS != 1_000 || cfg.PollMaxFailures != 2 {
				t.Errorf("login knobs = %+v", cfg)
			}
			if cfg.BalanceDivisor != 200_000 {
				t.Errorf("balance_divisor = %v", cfg.BalanceDivisor)
			}
		})
	}
}

// TestConfigCoercion is the field-by-field coercion table.
func TestConfigCoercion(t *testing.T) {
	cases := []struct {
		name      string
		document  string
		assertion func(t *testing.T, cfg Config)
	}{
		{"bool spellings", "enabled: yes", func(t *testing.T, cfg Config) {
			if !cfg.Enabled {
				t.Error("`yes` must be true")
			}
		}},
		{"numeric bool", "enabled: 0", func(t *testing.T, cfg Config) {
			if cfg.Enabled {
				t.Error("0 must be false")
			}
		}},
		{"unknown bool spelling keeps default", "enabled: maybe", func(t *testing.T, cfg Config) {
			if !cfg.Enabled {
				t.Error("an unusable value must keep the default, not clear it")
			}
		}},
		{"float to int", "poll_interval_ms: 2500.9", func(t *testing.T, cfg Config) {
			if cfg.PollIntervalMS != 2500 {
				t.Errorf("poll_interval_ms = %d", cfg.PollIntervalMS)
			}
		}},
		{"unparsable int", "priority: abc", func(t *testing.T, cfg Config) {
			if cfg.Priority != 0 {
				t.Errorf("priority = %d, want the zero default", cfg.Priority)
			}
		}},
		{"divisor as string", `balance_divisor: "50000"`, func(t *testing.T, cfg Config) {
			if cfg.BalanceDivisor != 50_000 {
				t.Errorf("balance_divisor = %v", cfg.BalanceDivisor)
			}
		}},
		{"effort fallback on blank", `reasoning_effort: "   "`, func(t *testing.T, cfg Config) {
			if cfg.DefaultReasoningEffort != "" {
				t.Errorf("reasoning_effort = %q, want the default", cfg.DefaultReasoningEffort)
			}
		}},
		{"bad value costs only itself", "priority: 3\npoll_interval_ms: nope\nmax_tokens: 12", func(t *testing.T, cfg Config) {
			if cfg.Priority != 3 || cfg.DefaultMaxTokens != 12 {
				t.Errorf("neighbours were discarded: %+v", cfg)
			}
			if cfg.PollIntervalMS != DevicePollIntervalMS {
				t.Errorf("the bad value did not fall back: %d", cfg.PollIntervalMS)
			}
		}},
		{"invalid document", "\tthis: [is: not: yaml", func(t *testing.T, cfg Config) {
			if cfg != DefaultConfig() {
				t.Errorf("an undecodable document must fall back entirely: %+v", cfg)
			}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.assertion(t, ConfigFromYAML([]byte(testCase.document)))
		})
	}
}

// TestConfigFieldNames ties the management-UI field list to the keys
// ConfigFromYAML actually reads, so a renamed field cannot silently stop being
// configurable.
func TestConfigFieldNames(t *testing.T) {
	expected := map[string]bool{
		"model_discovery":    true,
		"model_cache_ttl_ms": true,
		"max_tokens":         true,
		"max_output_tokens":  true,
		"reasoning_effort":   true,
		"login_timeout_ms":   true,
		"poll_interval_ms":   true,
		"poll_max_failures":  true,
		"balance_divisor":    true,
	}
	seen := map[string]bool{}
	for _, field := range ConfigFields() {
		if field.Name == "" {
			t.Fatal("a config field has no name")
		}
		if seen[field.Name] {
			t.Fatalf("duplicate config field %q", field.Name)
		}
		seen[field.Name] = true
		if field.Description == "" {
			t.Errorf("field %q has no Chinese description", field.Name)
		}
		if !expected[field.Name] {
			t.Errorf("field %q is not decoded by ConfigFromYAML", field.Name)
		}
	}
	for name := range expected {
		if !seen[name] {
			t.Errorf("documented key %q is missing from ConfigFields()", name)
		}
	}
}

// TestConfigFieldTypes pins the host-facing types the UI renders.
func TestConfigFieldTypes(t *testing.T) {
	types := map[string]string{}
	for _, field := range ConfigFields() {
		types[field.Name] = field.Type
	}
	for name, want := range map[string]string{
		"model_discovery":    "boolean",
		"model_cache_ttl_ms": "integer",
		"max_tokens":         "integer",
		"max_output_tokens":  "integer",
		"reasoning_effort":   "string",
		"login_timeout_ms":   "integer",
		"poll_interval_ms":   "integer",
		"poll_max_failures":  "integer",
		"balance_divisor":    "number",
	} {
		if types[name] != want {
			t.Errorf("field %q type = %q, want %q", name, types[name], want)
		}
	}
}
