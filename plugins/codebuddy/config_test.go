package main

import "testing"

func TestConfigFromYAML(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     func(Config) bool
		describe string
	}{
		{
			name:     "empty document keeps defaults",
			document: "",
			want: func(c Config) bool {
				return c.Enabled && c.Product == ProductCodeBuddy && c.DiscoverModels &&
					c.PromptCacheKey && c.ThinkingEnabled && c.CheckinEnabled &&
					c.FirstTokenTimeoutMS == 120000 && c.DefaultMaxTokens == 0
			},
			describe: "defaults",
		},
		{
			name:     "product enum value",
			document: "product: workbuddy\n",
			want:     func(c Config) bool { return c.Product == ProductWorkBuddy },
			describe: "product=workbuddy",
		},
		{
			name:     "jet-hub provider id is accepted as a product alias",
			document: "product: buddy-intl\n",
			want:     func(c Config) bool { return c.Product == ProductCodeBuddyIntl },
			describe: "product alias",
		},
		{
			name:     "unknown product falls back to the default",
			document: "product: nope\n",
			want:     func(c Config) bool { return c.Product == ProductCodeBuddy },
			describe: "unknown product",
		},
		{
			name: "booleans and integers",
			document: "enabled: false\npriority: 7\ndiscover_models: false\nprompt_cache_key: false\n" +
				"thinking_enabled: false\ncheckin_enabled: false\nmax_tokens: 4096\n" +
				"first_token_timeout_ms: 1000\nchunk_timeout_ms: 2000\nmodel_cache_ttl_ms: 3000\n",
			want: func(c Config) bool {
				return !c.Enabled && c.Priority == 7 && !c.DiscoverModels && !c.PromptCacheKey &&
					!c.ThinkingEnabled && !c.CheckinEnabled && c.DefaultMaxTokens == 4096 &&
					c.FirstTokenTimeoutMS == 1000 && c.ChunkTimeoutMS == 2000 && c.ModelCacheTTLMS == 3000
			},
			describe: "scalars",
		},
		{
			name:     "quoted values and trailing comments",
			document: "product: \"codebuddy-intl\" # 国际版\nmax_tokens: 1234 # 上限\n",
			want:     func(c Config) bool { return c.Product == ProductCodeBuddyIntl && c.DefaultMaxTokens == 1234 },
			describe: "quoting",
		},
		{
			name:     "nested mappings are ignored",
			document: "plugins:\n  codebuddy:\n    enabled: false\nproduct: workbuddy-cn\n",
			want:     func(c Config) bool { return c.Enabled && c.Product == ProductWorkBuddyCN },
			describe: "nesting",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ConfigFromYAML([]byte(test.document))
			if !test.want(got) {
				t.Fatalf("%s: ConfigFromYAML(%q) = %+v", test.describe, test.document, got)
			}
		})
	}
}

// flowAndBlock describes the same settings twice so the two styles can be
// compared field by field.
const (
	blockStyleConfig = "" +
		"enabled: true\n" +
		"product: workbuddy\n" +
		"discover_models: false\n" +
		"prompt_cache_key: false\n" +
		"thinking_enabled: false\n" +
		"checkin_enabled: false\n" +
		"max_tokens: 4096\n" +
		"first_token_timeout_ms: 1000\n" +
		"chunk_timeout_ms: 2000\n" +
		"model_cache_ttl_ms: 3000\n"
	flowStyleConfig = "{enabled: true, product: workbuddy, discover_models: false, " +
		"prompt_cache_key: false, thinking_enabled: false, checkin_enabled: false, " +
		"max_tokens: 4096, first_token_timeout_ms: 1000, chunk_timeout_ms: 2000, model_cache_ttl_ms: 3000}"
)

func TestConfigFromYAMLFlowStyle(t *testing.T) {
	// The host returns the instance subtree in the style the user wrote it.
	// A line-oriented scanner reads zero keys from this and silently falls back
	// to every default, which is the defect this test locks down.
	flow := ConfigFromYAML([]byte(flowStyleConfig))

	if flow.Product != ProductWorkBuddy {
		t.Errorf("product = %q, want %q", flow.Product, ProductWorkBuddy)
	}
	if flow.DiscoverModels {
		t.Error("discover_models: false was ignored")
	}
	if flow.PromptCacheKey || flow.ThinkingEnabled || flow.CheckinEnabled {
		t.Errorf("flow booleans ignored: %+v", flow)
	}
	if flow.DefaultMaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096", flow.DefaultMaxTokens)
	}
	if flow.FirstTokenTimeoutMS != 1000 || flow.ChunkTimeoutMS != 2000 || flow.ModelCacheTTLMS != 3000 {
		t.Errorf("flow integers ignored: %+v", flow)
	}

	// The nested form the user actually writes in config.yaml, where the host
	// hands over the inner subtree.
	nested := ConfigFromYAML([]byte("{enabled: true, discover_models: false}"))
	if !nested.Enabled || nested.DiscoverModels {
		t.Errorf("nested flow form ignored: %+v", nested)
	}
}

func TestConfigFromYAMLFlowAndBlockAgree(t *testing.T) {
	block := ConfigFromYAML([]byte(blockStyleConfig))
	flow := ConfigFromYAML([]byte(flowStyleConfig))
	if block != flow {
		t.Fatalf("block = %+v\nflow  = %+v", block, flow)
	}
	if block == DefaultConfig() {
		t.Fatal("the fixture must differ from the defaults, otherwise the comparison proves nothing")
	}
}

func TestConfigFromYAMLLenientScalars(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     func(Config) bool
		describe string
	}{
		{
			name:     "yaml 1.2 keeps no/off as strings; the coercer accepts them",
			document: "enabled: no\ndiscover_models: off\nprompt_cache_key: 'no'\n",
			want:     func(c Config) bool { return !c.Enabled && !c.DiscoverModels && !c.PromptCacheKey },
			describe: "no/off",
		},
		{
			name:     "on/yes/1",
			document: "enabled: on\ndiscover_models: yes\ncheckin_enabled: 1\n",
			want:     func(c Config) bool { return c.Enabled && c.DiscoverModels && c.CheckinEnabled },
			describe: "on/yes/1",
		},
		{
			name:     "quoted numbers",
			document: "max_tokens: \"1234\"\nchunk_timeout_ms: '500'\n",
			want:     func(c Config) bool { return c.DefaultMaxTokens == 1234 && c.ChunkTimeoutMS == 500 },
			describe: "quoted numbers",
		},
		{
			name:     "trailing comments",
			document: "product: codebuddy-intl # 国际版\nmax_tokens: 777 # 上限\n",
			want:     func(c Config) bool { return c.Product == ProductCodeBuddyIntl && c.DefaultMaxTokens == 777 },
			describe: "comments",
		},
		{
			name:     "one bad value costs only its own default",
			document: "product: workbuddy-cn\nmax_tokens: not-a-number\nenabled: [1, 2]\n",
			want: func(c Config) bool {
				return c.Product == ProductWorkBuddyCN && c.DefaultMaxTokens == 0 && c.Enabled
			},
			describe: "partial fallback",
		},
		{
			name:     "unknown product keeps the fallback",
			document: "product: nope\n",
			want:     func(c Config) bool { return c.Product == ProductCodeBuddy },
			describe: "unknown product",
		},
		{
			name:     "malformed documents fall back to the defaults",
			document: "product: [unclosed\n",
			want:     func(c Config) bool { return c == DefaultConfig() },
			describe: "malformed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ConfigFromYAML([]byte(test.document))
			if !test.want(got) {
				t.Fatalf("%s: ConfigFromYAML(%q) = %+v", test.describe, test.document, got)
			}
		})
	}

	if got := ConfigFromYAML(nil); got != DefaultConfig() {
		t.Errorf("nil document = %+v, want the defaults", got)
	}
}

func TestProductTable(t *testing.T) {
	// Every product config value must also be resolvable by its Jet-Hub id, and
	// the endpoints must match product.ts exactly.
	endpoints := map[string]string{
		ProductCodeBuddy:     "https://copilot.tencent.com",
		ProductCodeBuddyIntl: "https://www.codebuddy.ai",
		ProductWorkBuddyCN:   "https://copilot.tencent.com",
		ProductWorkBuddy:     "https://www.workbuddy.ai",
	}
	domains := map[string]string{
		ProductCodeBuddy:     "copilot.tencent.com",
		ProductCodeBuddyIntl: "www.codebuddy.ai",
		ProductWorkBuddyCN:   "copilot.tencent.com",
		ProductWorkBuddy:     "www.workbuddy.ai",
	}
	platforms := map[string]string{
		ProductCodeBuddy:     "ide",
		ProductCodeBuddyIntl: "ide",
		ProductWorkBuddyCN:   "workbuddy",
		ProductWorkBuddy:     "workbuddy-ai",
	}
	productCodes := map[string]string{
		ProductCodeBuddy:     "codebuddy",
		ProductCodeBuddyIntl: "codebuddy",
		ProductWorkBuddyCN:   "workbuddy",
		ProductWorkBuddy:     "workbuddy",
	}
	appendSession := map[string]bool{
		ProductCodeBuddy:     false,
		ProductCodeBuddyIntl: false,
		ProductWorkBuddyCN:   true,
		ProductWorkBuddy:     true,
	}

	if len(Products) != 4 {
		t.Fatalf("Products has %d entries, want 4", len(Products))
	}
	for _, product := range Products {
		if product.Endpoint != endpoints[product.ConfigValue] {
			t.Errorf("%s endpoint = %q, want %q", product.ConfigValue, product.Endpoint, endpoints[product.ConfigValue])
		}
		if product.APIDomain != domains[product.ConfigValue] {
			t.Errorf("%s apiDomain = %q, want %q", product.ConfigValue, product.APIDomain, domains[product.ConfigValue])
		}
		if product.Platform != platforms[product.ConfigValue] {
			t.Errorf("%s platform = %q, want %q", product.ConfigValue, product.Platform, platforms[product.ConfigValue])
		}
		if product.ProductCode != productCodes[product.ConfigValue] {
			t.Errorf("%s productCode = %q, want %q", product.ConfigValue, product.ProductCode, productCodes[product.ConfigValue])
		}
		if product.AppendSessionParams != appendSession[product.ConfigValue] {
			t.Errorf("%s appendSessionParams = %v", product.ConfigValue, product.AppendSessionParams)
		}
		if len(product.FallbackModels) == 0 {
			t.Errorf("%s has an empty fallback catalog", product.ConfigValue)
		}
		if resolved, ok := productByID(product.ID); !ok || resolved.ConfigValue != product.ConfigValue {
			t.Errorf("productByID(%q) did not resolve to %s", product.ID, product.ConfigValue)
		}
	}
}

func TestResolveUserAgent(t *testing.T) {
	workbuddy, _ := productByConfigValue(ProductWorkBuddy)
	codebuddy, _ := productByConfigValue(ProductCodeBuddy)

	tests := []struct {
		product productConfig
		model   string
		want    string
	}{
		{workbuddy, "gpt-5.6-sol", workBuddyUAIntl},
		{workbuddy, "gemini-3.5-flash", workBuddyUAIntl},
		{workbuddy, "claude-4.5", workBuddyUAIntl},
		{workbuddy, "glm-5.3", workBuddyUACN},
		{workbuddy, "hy4-preview", workBuddyUACN},
		{workbuddy, "kimi-k3", workBuddyUACN},
		{workbuddy, "minimax-m3", workBuddyUACN},
		{workbuddy, "deepseek-v4.1-flash", workBuddyUAIntl},
		{codebuddy, "glm-5.3", BuddyUserAgent},
	}
	for _, test := range tests {
		if got := resolveUserAgent(test.product, test.model); got != test.want {
			t.Errorf("resolveUserAgent(%s, %s) = %q, want %q", test.product.ConfigValue, test.model, got, test.want)
		}
	}
}

func TestConfigFieldsCoverEveryProduct(t *testing.T) {
	fields := ConfigFields()
	var productField *configField
	for index := range fields {
		if fields[index].Name == "product" {
			productField = &fields[index]
		}
	}
	if productField == nil {
		t.Fatal("ConfigFields has no product field")
	}
	if len(productField.EnumValues) != len(ProductIDs()) {
		t.Fatalf("product enum has %d values, want %d", len(productField.EnumValues), len(ProductIDs()))
	}
	for index, value := range ProductIDs() {
		if productField.EnumValues[index] != value {
			t.Errorf("product enum[%d] = %q, want %q", index, productField.EnumValues[index], value)
		}
	}
	if productField.Type != "enum" {
		t.Errorf("product field type = %q, want enum", productField.Type)
	}
}
