package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Config tests.
//
// The host hands the plugin the instance subtree in whichever YAML style the user
// wrote, with values of whatever scalar type YAML produced. Three properties are
// therefore required, and each has a test below:
//
//   - block AND flow style both work;
//   - `yes` / `no` / `on` / `off` work, even though yaml.v3 is YAML 1.2 and would
//     reject them for a typed `bool` field;
//   - one unusable value costs only its own default, never the whole document.

// TestConfigFromYAMLAcceptsBlockAndFlowStyle is the property that matters most for
// a real deployment.
//
// ⚠ A block-only line scanner silently ignores a flow-style document and falls back
// to EVERY default without reporting anything — a configuration mistake with no
// symptom until a request behaves unexpectedly.
func TestConfigFromYAMLAcceptsBlockAndFlowStyle(t *testing.T) {
	block := []byte(`
enabled: true
model_prefix: false
discover_models: false
max_tokens: 1234
app_version: "9.8.7"
`)
	flow := []byte(`{enabled: true, model_prefix: false, discover_models: false, max_tokens: 1234, app_version: "9.8.7"}`)

	for _, tc := range []struct {
		name     string
		document []byte
	}{
		{"block style", block},
		{"flow style", flow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ConfigFromYAML(tc.document)
			if !cfg.Enabled {
				t.Error("enabled = false")
			}
			if cfg.ModelPrefix {
				t.Error("model_prefix = true, want the configured false")
			}
			if cfg.DiscoverModels {
				t.Error("discover_models = true, want the configured false")
			}
			if cfg.DefaultMaxTokens != 1234 {
				t.Errorf("max_tokens = %d, want 1234", cfg.DefaultMaxTokens)
			}
			if cfg.AppVersion != "9.8.7" {
				t.Errorf("app_version = %q", cfg.AppVersion)
			}
		})
	}
}

// TestConfigFromYAMLAcceptsTheSpellingsUsersWrite covers the coercion.
func TestConfigFromYAMLAcceptsTheSpellingsUsersWrite(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"true", "true", true},
		{"yes", "yes", true},
		{"on", "on", true},
		{"one", "1", true},
		{"false", "false", false},
		{"no", "no", false},
		{"off", "off", false},
		{"zero", "0", false},
		{"YES upper case", "YES", true},
		{"with surrounding space", "  false  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ConfigFromYAML([]byte("enabled: " + tc.value))
			if cfg.Enabled != tc.want {
				t.Fatalf("enabled = %v for %q, want %v", cfg.Enabled, tc.value, tc.want)
			}
		})
	}
}

// TestConfigFromYAMLKeepsOtherValuesWhenOneIsUnusable is the "one bad value costs
// one default" rule.
func TestConfigFromYAMLKeepsOtherValuesWhenOneIsUnusable(t *testing.T) {
	document := []byte(`
enabled: true
max_tokens: not-a-number
app_version: "1.2.3"
poll_max_failures: also-not-a-number
`)
	cfg := ConfigFromYAML(document)
	if cfg.DefaultMaxTokens != DefaultDefaultMaxTokens {
		t.Errorf("an unusable max_tokens did not fall back: %d", cfg.DefaultMaxTokens)
	}
	if cfg.PollMaxFailures != DefaultPollMaxFailures {
		t.Errorf("an unusable poll_max_failures did not fall back: %d", cfg.PollMaxFailures)
	}
	// The values that WERE usable survive.
	if !cfg.Enabled {
		t.Error("the usable enabled value was lost")
	}
	if cfg.AppVersion != "1.2.3" {
		t.Errorf("app_version = %q, want the usable value", cfg.AppVersion)
	}
}

// TestConfigFromYAMLOnAnUnparseableDocumentFallsBackEntirely covers the last
// resort: a document that is not YAML at all.
func TestConfigFromYAMLOnAnUnparseableDocumentFallsBackEntirely(t *testing.T) {
	for _, document := range [][]byte{
		[]byte("\t- not: valid: yaml: at all"),
		[]byte("{{{"),
	} {
		cfg := ConfigFromYAML(document)
		want := DefaultConfig()
		if cfg != want {
			t.Fatalf("an unparseable document produced %#v, want the defaults", cfg)
		}
	}
}

// TestConfigFromYAMLOnAnEmptyDocumentIsTheDefaults covers the absent-subtree case.
func TestConfigFromYAMLOnAnEmptyDocumentIsTheDefaults(t *testing.T) {
	for _, document := range [][]byte{nil, {}, []byte("   \n")} {
		cfg := ConfigFromYAML(document)
		want := DefaultConfig()
		if cfg != want {
			t.Fatalf("document %q produced %#v, want the defaults", document, cfg)
		}
	}
}

// TestConfigFromYAMLRestrictsChoicesToKnownValues covers the enum coercion.
func TestConfigFromYAMLRestrictsChoicesToKnownValues(t *testing.T) {
	cases := []struct {
		name         string
		document     string
		wantPlatform string
		wantOSGroup  string
	}{
		{"known values are accepted", "platform: linux\nos_category: linux", "linux", "linux"},
		{"upper case is normalised", "platform: WIN32\nos_category: WINDOWS", "win32", "windows"},
		{"an unknown value keeps the default", "platform: plan9\nos_category: beos", DefaultPlatform, DefaultOSCategory},
		{"an empty value keeps the default", "platform: ''\nos_category: ''", DefaultPlatform, DefaultOSCategory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ConfigFromYAML([]byte(tc.document))
			if cfg.Platform != tc.wantPlatform {
				t.Errorf("platform = %q, want %q", cfg.Platform, tc.wantPlatform)
			}
			if cfg.OSGroup != tc.wantOSGroup {
				t.Errorf("os_category = %q, want %q", cfg.OSGroup, tc.wantOSGroup)
			}
		})
	}
}

// TestDefaultConfigMatchesTheMeasuredValues pins the defaults against the
// reference's published constants.
func TestDefaultConfigMatchesTheMeasuredValues(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"enabled", cfg.Enabled, true},
		{"model_prefix", cfg.ModelPrefix, true},
		{"discover_models", cfg.DiscoverModels, true},
		{"tool_cache_breakpoint", cfg.ToolCacheBreakpoint, true},
		{"app_version", cfg.AppVersion, "3.14.3"},
		{"platform", cfg.Platform, "win32"},
		{"os_category", cfg.OSGroup, "windows"},
		{"client_language", cfg.ClientLanguage, "zh-CN"},
		{"client_timezone", cfg.ClientTimezone, "Asia/Shanghai"},
		{"default max tokens", cfg.DefaultMaxTokens, 8_192},
		{"concurrency_retry_max", cfg.ConcurrencyRetryMax, 2},
		{"concurrency_retry_base_ms", cfg.ConcurrencyRetryBaseMS, 1_500},
		{"request_timeout_ms", cfg.RequestTimeoutMS, 180_000},
		{"login_timeout_ms", cfg.LoginTimeoutMS, 300_000},
		{"poll_max_failures", cfg.PollMaxFailures, 5},
		// The import path is OPTIONAL and therefore off by default: the plugin's own
		// login is the primary path.
		{"import_client_credential", cfg.ImportClientCredential, false},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("default %s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestConfigFieldsDescribeEverySettableKey covers the management-client field list.
//
// A setting that exists but is not described cannot be configured through the UI, and
// a described setting that does not exist is worse: the user sets it and nothing
// happens.
func TestConfigFieldsDescribeEverySettableKey(t *testing.T) {
	fields := ConfigFields()
	if len(fields) == 0 {
		t.Fatal("no configuration fields are described")
	}
	seen := map[string]bool{}
	for _, field := range fields {
		if field.Name == "" {
			t.Error("a field has no name")
		}
		if seen[field.Name] {
			t.Errorf("field %q is described twice", field.Name)
		}
		seen[field.Name] = true
		if field.Description == "" {
			t.Errorf("field %q has no description", field.Name)
		}
		switch field.Type {
		case "string", "integer", "boolean", "enum":
		default:
			t.Errorf("field %q has an unexpected type %q", field.Name, field.Type)
		}
		if field.Type == "enum" && len(field.EnumValues) == 0 {
			t.Errorf("enum field %q lists no values", field.Name)
		}
	}

	// Every described key must actually be read by ConfigFromYAML: a field the
	// parser ignores is a setting the user can change with no effect.
	document := []byte(`
app_version: x
platform: linux
os_category: linux
client_language: x
client_timezone: x
model_prefix: true
discover_models: true
tool_cache_breakpoint: true
max_tokens: 1
concurrency_retry_max: 1
concurrency_retry_base_ms: 1
request_timeout_ms: 1
login_timeout_ms: 1
poll_interval_ms: 1
poll_max_failures: 1
import_client_credential: true
client_data_dir: /tmp
`)
	cfg := ConfigFromYAML(document)
	if cfg.ClientDataDir != "/tmp" {
		t.Errorf("client_data_dir was not read: %q", cfg.ClientDataDir)
	}
	if !cfg.ImportClientCredential {
		t.Error("import_client_credential was not read")
	}
	if cfg.ConcurrencyRetryMax != 1 || cfg.RequestTimeoutMS != 1 {
		t.Error("a numeric setting was not read")
	}

	// `enabled` and `priority` are the two keys the HOST owns: it writes `enabled`
	// into every instance subtree itself, and it reads `priority` to order plugins.
	// The sibling provider plugins therefore do not advertise them either, and this
	// plugin follows that convention — but the PARSER still has to honour them or a
	// host-written value would be dropped.
	hostOwned := ConfigFromYAML([]byte("enabled: false\npriority: 7\n"))
	if hostOwned.Enabled {
		t.Error("the parser ignored the host-owned `enabled` key")
	}
	if hostOwned.Priority != 7 {
		t.Errorf("priority = %d, want 7", hostOwned.Priority)
	}
}

// TestConfigFieldsDoNotAdvertiseTheDeprecatedCaptchaSettings is a guard against
// reintroducing the obsolete machinery as configuration.
func TestConfigFieldsDoNotAdvertiseTheDeprecatedCaptchaSettings(t *testing.T) {
	for _, field := range ConfigFields() {
		lower := strings.ToLower(field.Name)
		if strings.Contains(lower, "captcha") {
			t.Errorf("field %q configures a captcha path; inference was measured to need none", field.Name)
		}
		if strings.Contains(lower, "callback_port") || strings.Contains(lower, "callback_bind") {
			t.Errorf("field %q configures a local callback listener; the device flow has none", field.Name)
		}
		if strings.Contains(lower, "reasoning_effort") {
			t.Errorf("field %q uses the wrong effort spelling; upstream publishes output_config.effort", field.Name)
		}
	}
}

// TestConfigFieldsForHostCarriesTypesAndEnums covers the ABI conversion.
func TestConfigFieldsForHostCarriesTypesAndEnums(t *testing.T) {
	fields := configFieldsForHost()
	if len(fields) != len(ConfigFields()) {
		t.Fatalf("converted %d fields from %d", len(fields), len(ConfigFields()))
	}
	var platformField *pluginapi.ConfigField
	for index := range fields {
		if fields[index].Name == "platform" {
			platformField = &fields[index]
		}
	}
	if platformField == nil {
		t.Fatal("the platform field is missing")
	}
	if platformField.Type != pluginapi.ConfigFieldTypeEnum {
		t.Errorf("platform type = %q, want enum", platformField.Type)
	}
	if !slices.Contains(platformField.EnumValues, "win32") {
		t.Errorf("platform enum values = %v, want win32 among them", platformField.EnumValues)
	}
	if platformField.Description == "" {
		t.Error("the platform field lost its description")
	}
}

// TestConfigureInstallsTheSettings covers the lifecycle hook.
func TestConfigureInstallsTheSettings(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	p := newPlugin()
	if errConfigure := p.Configure([]byte("max_tokens: 777\nmodel_prefix: false\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	cfg := settings()
	if cfg.DefaultMaxTokens != 777 {
		t.Errorf("max_tokens = %d, want 777", cfg.DefaultMaxTokens)
	}
	if cfg.ModelPrefix {
		t.Error("model_prefix = true, want the configured false")
	}
	// A reconfiguration drops the cached catalogue, because the settings may change
	// what it is allowed to be.
	storeDiscoveredModels([]fallbackModel{{ID: "GLM-5.3"}})
	if errConfigure := p.Configure([]byte("max_tokens: 1\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if len(currentCatalogue()) != len(fallbackCatalogue) {
		t.Error("the discovered catalogue survived a reconfiguration")
	}
}

// TestRegistrationDeclaresTheProvidersItImplements covers the capability block.
func TestRegistrationDeclaresTheProvidersItImplements(t *testing.T) {
	registration := Plugin().Registration()
	if registration.SchemaVersion == 0 {
		t.Error("the schema version was not stamped")
	}
	caps := registration.Capabilities
	for name, declared := range map[string]bool{
		"model_registrar":    caps.ModelRegistrar,
		"model_provider":     caps.ModelProvider,
		"auth_provider":      caps.AuthProvider,
		"executor":           caps.Executor,
		"management_api":     caps.ManagementAPI,
		"quota_provider":     caps.QuotaProvider,
		"request_translate":  caps.RequestTranslator,
		"response_translate": caps.ResponseTranslator,
	} {
		if !declared {
			t.Errorf("capability %s is implemented but not declared", name)
		}
	}
	// Capabilities this plugin does NOT implement must stay off: the host calls
	// every one that is declared.
	for name, declared := range map[string]bool{
		"scheduler":                    caps.Scheduler,
		"model_router":                 caps.ModelRouter,
		"frontend_auth_provider":       caps.FrontendAuthProvider,
		"request_interceptor":          caps.RequestInterceptor,
		"request_lifecycle_plugin":     caps.RequestLifecyclePlugin,
		"response_interceptor":         caps.ResponseInterceptor,
		"stream_chunk_interceptor":     caps.StreamChunkInterceptor,
		"web_socket_response_observer": caps.WebSocketResponseObserver,
		"thinking_applier":             caps.ThinkingApplier,
		"usage_plugin":                 caps.UsagePlugin,
		"command_line_plugin":          caps.CommandLinePlugin,
	} {
		if declared {
			t.Errorf("capability %s is declared but not implemented", name)
		}
	}
}

// TestRegistrationAnswersEveryMethodItDeclares covers the route table: a declared
// capability without a route is a method the host calls into a void.
func TestRegistrationAnswersEveryMethodItDeclares(t *testing.T) {
	routes := Plugin().Routes()
	expected := []string{
		"auth.identifier", "auth.parse", "auth.login.start", "auth.login.poll", "auth.refresh",
		"model.register", "model.static", "model.for_auth",
		"executor.identifier", "executor.execute", "executor.execute_stream", "executor.count_tokens",
		"request.translate", "response.translate",
		"quota.identifier", "quota.describe", "quota.fetch", "quota.reset",
		"management.register", "management.handle",
	}
	for _, method := range expected {
		if _, ok := routes[method]; !ok {
			t.Errorf("method %s is not routed", method)
		}
	}
	if len(routes) != len(expected) {
		t.Errorf("route count = %d, want %d: every route should be one of the declared methods",
			len(routes), len(expected))
	}
}

// TestPluginShutdownClearsState covers the lifecycle hook.
func TestPluginShutdownClearsState(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	storeDiscoveredModels([]fallbackModel{{ID: "GLM-5.3"}})
	// The singleton is returned through the abiboot interface, so the hook is
	// reached through its concrete type — which is also what the ABI bootstrap
	// does when it type-asserts for Shutdowner.
	instance := Plugin().(*plugin)
	instance.Quiesce()
	instance.Shutdown()
	if len(currentCatalogue()) != len(fallbackCatalogue) {
		t.Error("the catalogue cache survived a shutdown")
	}
	loginMu.Lock()
	leaked := len(loginSessions)
	loginMu.Unlock()
	if leaked != 0 {
		t.Errorf("%d login sessions survived a shutdown", leaked)
	}
}

// TestVersionAndIdentityConstantsAreCoherent covers the plugin's own identity.
func TestVersionAndIdentityConstantsAreCoherent(t *testing.T) {
	if ProviderKey == "" || DisplayName == "" || Version == "" {
		t.Fatal("a plugin identity constant is empty")
	}
	if !strings.HasPrefix(Repository, "https://") {
		t.Errorf("repository = %q, want an https URL", Repository)
	}
	// The endpoint constants have to compose into real URLs.
	if !strings.HasPrefix(Origin, "https://") {
		t.Errorf("origin = %q", Origin)
	}
	for name, path := range map[string]string{
		"MessagesPath": MessagesPath, "BalancePath": BalancePath, "PreviewPath": PreviewPath,
		"ClaimPath": ClaimPath, "EventReportPath": EventReportPath,
		"ClientConfigsPath": ClientConfigsPath, "OAuthCLIInitPath": OAuthCLIInitPath,
	} {
		if !strings.HasPrefix(path, "/") {
			t.Errorf("%s = %q, want an absolute path", name, path)
		}
	}
	// The inference path is the Anthropic one; the OpenAI-shaped path does not
	// exist on this channel.
	if !strings.Contains(MessagesPath, "/anthropic/") {
		t.Errorf("MessagesPath = %q, want the Anthropic messages endpoint", MessagesPath)
	}
	if strings.Contains(MessagesPath, "chat/completions") {
		t.Errorf("MessagesPath = %q, but that endpoint 404s on this channel", MessagesPath)
	}
}
