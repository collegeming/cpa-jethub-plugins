package main

import (
	"reflect"
	"testing"
)

// TestConfigDefaults pins the defaults that matter: Max mode is on, discovery is
// on, the region is the CN site and the output clamp is 64K.
func TestConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Enabled || !cfg.DiscoverModels || !cfg.MaxMode || cfg.RotateMachineID {
		t.Fatalf("defaults = %#v", cfg)
	}
	if cfg.Region != RegionCN {
		t.Fatalf("region = %q, want %q", cfg.Region, RegionCN)
	}
	if cfg.MaxCompletionTokens != DefaultMaxCompletionTokens || cfg.MaxHistoryChars != DefaultMaxHistoryChars {
		t.Fatalf("numeric defaults = %#v", cfg)
	}
	if cfg.DefaultChannel != DefaultFunction || cfg.CallbackPort != DefaultCallbackPort {
		t.Fatalf("channel/port defaults = %#v", cfg)
	}
}

// TestConfigFromYAMLStyles is the regression guard for the shared parsing defect:
// the host returns the instance subtree in the style the user wrote it, so a
// flow-style document must be decoded exactly like its block-style twin.
func TestConfigFromYAMLStyles(t *testing.T) {
	block := []byte("enabled: true\ndiscover_models: false\nregion: trae-intl\nmax_mode: false\n" +
		"max_completion_tokens: 32000\nchannels: \"a,b\"\n")
	// Caret: inside a flow mapping a comma separates entries, so a comma-separated
	// scalar has to be quoted there. A flow *sequence* is the natural spelling.
	flow := []byte("{enabled: true, discover_models: false, region: trae-intl, max_mode: false, " +
		"max_completion_tokens: 32000, channels: [a, b]}\n")

	fromBlock := ConfigFromYAML(block)
	fromFlow := ConfigFromYAML(flow)
	if !reflect.DeepEqual(fromBlock, fromFlow) {
		t.Fatalf("flow and block documents disagree:\n block = %#v\n flow  = %#v", fromBlock, fromFlow)
	}
	if fromFlow.DiscoverModels {
		t.Fatal("discover_models: false must reach the config")
	}
	if fromFlow.Region != RegionINTL {
		t.Fatalf("region = %q, want %q", fromFlow.Region, RegionINTL)
	}
	if fromFlow.MaxMode {
		t.Fatal("max_mode: false must reach the config")
	}
	if fromFlow.MaxCompletionTokens != 32000 {
		t.Fatalf("max_completion_tokens = %d, want 32000", fromFlow.MaxCompletionTokens)
	}
	if len(fromFlow.Channels) != 2 || fromFlow.Channels[0] != "a" || fromFlow.Channels[1] != "b" {
		t.Fatalf("channels = %#v", fromFlow.Channels)
	}
}

// TestConfigFromYAMLCoercions covers the value spellings a typed YAML decode
// would either reject (and thereby discard the whole document) or misinterpret.
func TestConfigFromYAMLCoercions(t *testing.T) {
	document := []byte("discover_models: 'no'\nmax_mode: off\nrotate_machine_id: 1\n" +
		"max_completion_tokens: '32000'\ncallback_port: 19090\nregion: BOGUS\n")
	cfg := ConfigFromYAML(document)
	if cfg.DiscoverModels {
		t.Fatal("quoted 'no' must be read as false")
	}
	if cfg.MaxMode {
		t.Fatal("off must be read as false")
	}
	if !cfg.RotateMachineID {
		t.Fatal("1 must be read as true")
	}
	if cfg.MaxCompletionTokens != 32000 {
		t.Fatalf("quoted number = %d, want 32000", cfg.MaxCompletionTokens)
	}
	if cfg.CallbackPort != 19090 {
		t.Fatalf("callback_port = %d", cfg.CallbackPort)
	}
	if cfg.Region != RegionCN {
		t.Fatalf("an unknown region must fall back to the default, got %q", cfg.Region)
	}
}

// TestConfigFromYAMLSequenceList accepts a YAML sequence as well as a
// comma-separated string.
func TestConfigFromYAMLSequenceList(t *testing.T) {
	document := []byte("channels:\n  - solo_agent\n  - solo_work_lite\nmax_mode_models: []\n")
	cfg := ConfigFromYAML(document)
	if len(cfg.Channels) != 2 || cfg.Channels[1] != "solo_work_lite" {
		t.Fatalf("channels = %#v", cfg.Channels)
	}
	// An empty list means "not configured", so the default channel list stays.
	if len(cfg.Channels) == 0 {
		t.Fatal("list parsing failed")
	}
	if cfg.MaxModeModels != nil {
		t.Fatalf("an empty max_mode_models list must mean unset, got %#v", cfg.MaxModeModels)
	}
}

// TestConfigFromYAMLMalformedFallsBack: an unparsable document keeps the defaults
// instead of failing the registration.
func TestConfigFromYAMLMalformedFallsBack(t *testing.T) {
	cfg := ConfigFromYAML([]byte("\tnot: [valid: yaml"))
	if !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatalf("malformed document must fall back to defaults, got %#v", cfg)
	}
	empty := ConfigFromYAML(nil)
	if !reflect.DeepEqual(empty, DefaultConfig()) {
		t.Fatalf("empty document must keep defaults, got %#v", empty)
	}
}

// TestConfigFromYAMLTrailingCommentAndOneBadValue: a trailing comment is normal
// YAML, and one unusable value only costs its own field.
func TestConfigFromYAMLTrailingCommentAndOneBadValue(t *testing.T) {
	document := []byte("discover_models: true # keep discovery\nmax_completion_tokens: not-a-number\nregion: trae\n")
	cfg := ConfigFromYAML(document)
	if !cfg.DiscoverModels {
		t.Fatal("discover_models with a trailing comment must be honoured")
	}
	if cfg.MaxCompletionTokens != DefaultMaxCompletionTokens {
		t.Fatalf("a bad number must fall back to its own default, got %d", cfg.MaxCompletionTokens)
	}
}

// TestConfigFieldsAreComplete guards the settings the management UI renders.
func TestConfigFieldsAreComplete(t *testing.T) {
	fields := ConfigFields()
	if len(fields) < 10 {
		t.Fatalf("expected the full settings surface, got %d fields", len(fields))
	}
	seen := map[string]bool{}
	for _, field := range fields {
		if field.Name == "" || field.Description == "" || field.Type == "" {
			t.Fatalf("incomplete field %#v", field)
		}
		seen[field.Name] = true
	}
	for _, required := range []string{"region", "discover_models", "channels", "max_mode", "max_history_chars"} {
		if !seen[required] {
			t.Fatalf("config field %q is missing", required)
		}
	}
}

// TestSplitList covers the comma-separated list parser.
func TestSplitList(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{raw: "", want: nil},
		{raw: " , ", want: nil},
		{raw: "a", want: []string{"a"}},
		{raw: "a, b ,c", want: []string{"a", "b", "c"}},
	}
	for _, testCase := range cases {
		got := splitList(testCase.raw)
		if len(got) != len(testCase.want) {
			t.Fatalf("splitList(%q) = %#v, want %#v", testCase.raw, got, testCase.want)
		}
		for index := range got {
			if got[index] != testCase.want[index] {
				t.Fatalf("splitList(%q) = %#v, want %#v", testCase.raw, got, testCase.want)
			}
		}
	}
}

// TestProductForRegion maps the region setting onto the endpoint sets, including
// the never-invent rule for the INTL identity fields.
func TestProductForRegion(t *testing.T) {
	cn := productFor(RegionCN)
	intl := productFor(RegionINTL)
	if cn.AgentHost != "https://trae-api-cn.mchost.guru" || cn.UGHost != "https://api.trae.cn" {
		t.Fatalf("CN hosts = %#v", cn)
	}
	if intl.AgentHost != "https://api5-normal-alisg.mchost.guru" || intl.UGHost != "https://api.trae.ai" {
		t.Fatalf("INTL hosts = %#v", intl)
	}
	if intl.ConsoleHost != "https://www.trae.ai" || intl.OAuthHost != "https://api.trae.ai" {
		t.Fatalf("INTL console/oauth = %#v", intl)
	}
	// The identity fields are shared on purpose: Jet-Hub refused to invent an
	// INTL SOLO identity (trae-product.ts:341-347).
	if cn.ClientID != intl.ClientID || cn.IDEVersion != intl.IDEVersion {
		t.Fatalf("identity fields must match: cn=%#v intl=%#v", cn, intl)
	}
	if productFor("nonsense").ID != RegionCN {
		t.Fatal("an unknown region must fall back to the CN product")
	}
	if productFor("TRAE-INTL").ID != RegionINTL {
		t.Fatal("region matching must be case-insensitive")
	}
}
