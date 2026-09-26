package main

import (
	"testing"
)

// The YAML decoder accepts block style, flow style, YAML-1.2-hostile booleans
// and quoted numbers, and one unusable value costs only its own default.
func TestConfigFromYAMLBlockAndFlowStyle(t *testing.T) {
	block := ConfigFromYAML([]byte(`
enabled: true
priority: 7
phone: 13800138000
discover_models: yes
model_cache_ttl_ms: 60000
session_ttl_seconds: "3600"
login_timeout_ms: 120000
request_timeout_ms: 45000
sms_code_ttl_seconds: 120
account_ak: my-ak
account_sk: my-sk
`))
	if !block.Enabled || block.Priority != 7 {
		t.Fatalf("block config = %#v, want enabled with priority 7", block)
	}
	if block.Phone != "13800138000" {
		t.Fatalf("phone = %q, want the number rendered as text", block.Phone)
	}
	if !block.DiscoverModels {
		t.Fatal("`yes` must be accepted as true (yaml.v3 is YAML 1.2 and would fail a typed bool decode)")
	}
	if block.ModelCacheTTLMS != 60000 || block.SessionTTLSeconds != 3600 || block.LoginTimeoutMS != 120000 {
		t.Fatalf("numeric settings = %#v, want the quoted number to parse", block)
	}
	if block.AccountAK != "my-ak" || block.AccountSK != "my-sk" {
		t.Fatalf("account keys = %q/%q, want the overrides", block.AccountAK, block.AccountSK)
	}

	flow := ConfigFromYAML([]byte(`{enabled: false, phone: "13900139000", discover_models: "off", request_timeout_ms: 1000}`))
	if flow.Enabled {
		t.Fatal("flow style must be decoded, not ignored")
	}
	if flow.Phone != "13900139000" || flow.DiscoverModels {
		t.Fatalf("flow config = %#v, want the phone set and discovery off", flow)
	}
	if flow.RequestTimeoutMS != 1000 {
		t.Fatalf("request timeout = %d, want 1000", flow.RequestTimeoutMS)
	}
}

// A broken document and unusable individual values fall back to defaults instead
// of discarding everything.
func TestConfigFromYAMLFallbacks(t *testing.T) {
	defaults := DefaultConfig()
	if got := ConfigFromYAML(nil); got != defaults {
		t.Fatalf("empty document = %#v, want the defaults", got)
	}
	if got := ConfigFromYAML([]byte("\t- not: a map")); got != defaults {
		t.Fatal("an undecodable document must fall back to the defaults")
	}
	partial := ConfigFromYAML([]byte("priority: 3\nphone: 13800138000\nsession_ttl_seconds: soon"))
	if partial.Priority != 3 {
		t.Fatalf("priority = %d, want 3", partial.Priority)
	}
	if partial.Phone != "13800138000" {
		t.Fatalf("phone = %q, want the readable value to survive a broken sibling", partial.Phone)
	}
	if partial.SessionTTLSeconds != defaults.SessionTTLSeconds {
		t.Fatalf("session ttl = %d, want the default %d", partial.SessionTTLSeconds, defaults.SessionTTLSeconds)
	}
}

// The accessor helpers always return something usable.
func TestConfigAccessorsFallBack(t *testing.T) {
	empty := Config{}
	if empty.smsCodeTTL() != SMSCodeTTLSeconds ||
		empty.sessionTTL() != SessionTTLSeconds ||
		empty.loginSessionTTL() != LoginTimeoutMS ||
		empty.requestTimeout() != RequestTimeoutMS ||
		empty.modelCacheTTL() != ModelCacheTTLMS {
		t.Fatalf("zero config accessors = %#v, want the documented defaults", empty)
	}
	if empty.accountAK() != AccountAccessKeyID || empty.accountSK() != AccountAccessKeySecret {
		t.Fatal("an unset AK/SK must fall back to the embedded pair")
	}
	override := Config{AccountAK: "  custom  ", AccountSK: "sk"}
	if override.accountAK() != "custom" || override.accountSK() != "sk" {
		t.Fatalf("override keys = %q/%q, want the trimmed settings", override.accountAK(), override.accountSK())
	}
	if got := empty.defaultPhone(" 13900139000 "); got != "13900139000" {
		t.Fatalf("defaultPhone(override) = %q, want the override to win", got)
	}
	if got := (Config{Phone: "+86 138-0013-8000"}).defaultPhone(""); got != "13800138000" {
		t.Fatalf("defaultPhone(configured) = %q, want the normalised configured number", got)
	}
}

// Every live setting is declared to the host with a Chinese description, because
// the manager renders exactly these fields.
func TestConfigFieldsAreDeclared(t *testing.T) {
	fields := ConfigFields()
	want := map[string]string{
		"phone":                "string",
		"discover_models":      "boolean",
		"model_cache_ttl_ms":   "integer",
		"sms_code_ttl_seconds": "integer",
		"session_ttl_seconds":  "integer",
		"login_timeout_ms":     "integer",
		"request_timeout_ms":   "integer",
		"account_ak":           "string",
		"account_sk":           "string",
	}
	seen := map[string]bool{}
	for _, field := range fields {
		expected, declared := want[field.Name]
		if !declared {
			t.Errorf("unexpected config field %q", field.Name)
			continue
		}
		seen[field.Name] = true
		if field.Type != expected {
			t.Errorf("field %q type = %q, want %q", field.Name, field.Type, expected)
		}
		if len([]rune(field.Description)) < 8 {
			t.Errorf("field %q needs a real Chinese description, got %q", field.Name, field.Description)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("config field %q is not declared", name)
		}
	}
	// The host type conversion keeps names, types and descriptions.
	hostFields := configFieldsForHost()
	if len(hostFields) != len(fields) {
		t.Fatalf("host fields = %d, want %d", len(hostFields), len(fields))
	}
	for index, field := range hostFields {
		if field.Name != fields[index].Name || field.Description != fields[index].Description {
			t.Fatalf("host field %d = %#v, want it to mirror %#v", index, field, fields[index])
		}
	}
}
