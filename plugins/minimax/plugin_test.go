package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestCredentialRoundTripAndExpiry(t *testing.T) {
	tests := []struct {
		name       string
		expiresAt  string
		wantMillis int64
	}{
		{"millisecond string", "1700000000001", 1_700_000_000_001},
		{"second string", "1700000000", 1_700_000_000_000},
		{"empty absent", "", 0},
		{"padded string rejected", " 1700000000 ", 1_700_000_000_000}, // field is trimmed by ExpiresAtMS
		{"scientific notation rejected", "1.7e12", 0},
		{"junk rejected", "1700ms", 0},
		{"zero absent", "0", 0},
		{"negative absent", "-1", 0},
		// RFC3339 spellings. The credential CPA actually held for this provider
		// carried exactly the first of these, and while it was rejected as
		// "absent" the account could never renew: a 3600-second token sat dead
		// for an hour while every local signal reported it healthy.
		{"rfc3339 observed on disk", "2026-09-30T17:48:18Z", 1_790_790_498_000},
		{"rfc3339 nano", "2026-09-30T17:48:18.698Z", 1_790_790_498_698},
		{"rfc3339 with offset", "2026-10-01T01:48:18+08:00", 1_790_790_498_000},
		{"naive spelling parsed as UTC", "2026-09-30 17:48:18", 1_790_790_498_000},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			credential := &Credential{AccessToken: "mmoat_x", ExpiresAt: testCase.expiresAt}
			if got := credential.ExpiresAtMS(); got != testCase.wantMillis {
				t.Errorf("ExpiresAtMS(%q) = %d, want %d", testCase.expiresAt, got, testCase.wantMillis)
			}
		})
	}
}

func TestCredentialJSONAcceptsNumericExpiresAt(t *testing.T) {
	credential, errParse := ParseCredential([]byte(`{
		"access_token":"mmoat_x","refresh_token":"mmort_y","expires_at":1700000000001
	}`))
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if credential.ExpiresAt != "1700000000001" || credential.ExpiresAtMS() != 1_700_000_000_001 {
		t.Errorf("expires_at = %q / %d", credential.ExpiresAt, credential.ExpiresAtMS())
	}
}

func TestCredentialExpiryAndRefreshability(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	credential := &Credential{
		AccessToken: "mmoat_x", RefreshToken: "mmort_y",
		ExpiresAt: "1700000300000",
	}
	if !credential.Refreshable() {
		t.Error("credential with a refresh token is not refreshable")
	}
	if credential.Expired(now) {
		t.Error("credential is expired before its expiry")
	}
	if !credential.NeedsRefresh(now, 5*time.Minute) {
		t.Error("credential exactly 5 minutes from expiry should need refresh")
	}
	if !credential.Expired(now.Add(5 * time.Minute)) {
		t.Error("credential is not expired at its expiry")
	}
	withoutExpiry := &Credential{AccessToken: "mmoat_x"}
	if withoutExpiry.Expired(now) || withoutExpiry.NeedsRefresh(now, time.Hour) {
		t.Error("credential with no expiry was guessed to be expired/due")
	}
}

func TestAuthDataKeepsHostFileNameOnRefresh(t *testing.T) {
	credential := &Credential{
		AccessToken: "mmoat_new_token", RefreshToken: "mmort_y",
		ExpiresAt: "1700003600000", Type: ProviderKey,
	}
	name := authNameForHost("minimax-existing.json", "", "", credential)
	if name != "minimax-existing.json" {
		t.Fatalf("authNameForHost = %q, want host-owned name", name)
	}
	auth, errAuth := authDataFor(credential, name)
	if errAuth != nil {
		t.Fatal(errAuth)
	}
	if auth.FileName != "minimax-existing.json" || auth.ID != "minimax-existing.json" {
		t.Errorf("auth names = (%q,%q), want existing name", auth.FileName, auth.ID)
	}
	if auth.Attributes["refreshable"] != "true" {
		t.Errorf("refreshable attr = %q", auth.Attributes["refreshable"])
	}
	if len(auth.StorageJSON) == 0 {
		t.Error("auth storage is empty")
	}
}

func TestRegistrationCapabilitiesAndRoutes(t *testing.T) {
	p := newPlugin()
	registration := p.Registration()
	if registration.Metadata.Name != DisplayName || registration.Metadata.Version != Version {
		t.Errorf("metadata = %+v", registration.Metadata)
	}
	capabilities := registration.Capabilities
	if !capabilities.AuthProvider || !capabilities.ModelProvider || !capabilities.ModelRegistrar ||
		!capabilities.Executor || !capabilities.QuotaProvider || !capabilities.ManagementAPI {
		t.Errorf("capabilities missing: %+v", capabilities)
	}
	if !reflect.DeepEqual(capabilities.ExecutorInputFormats, []string{"anthropic"}) ||
		!reflect.DeepEqual(capabilities.ExecutorOutputFormats, []string{"anthropic"}) {
		t.Errorf("formats = in:%v out:%v", capabilities.ExecutorInputFormats, capabilities.ExecutorOutputFormats)
	}
	// Full method surface pinned to the method list the porting guide requires.
	wantRoutes := []string{
		"auth.identifier", "auth.parse", "auth.login.start", "auth.login.poll", "auth.refresh",
		"model.register", "model.static", "model.for_auth",
		"executor.identifier", "executor.execute", "executor.execute_stream", "executor.count_tokens",
		"request.translate", "response.translate",
		"quota.identifier", "quota.describe", "quota.fetch", "quota.reset",
		"management.register", "management.handle",
	}
	for _, route := range wantRoutes {
		if _, ok := p.Routes()[route]; !ok {
			t.Errorf("route %s not registered", route)
		}
	}
}

func TestManagementRegistersNoSidebarMenu(t *testing.T) {
	value, err := handleManagementRegister(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := value.(pluginapi.ManagementRegistrationResponse)
	for _, route := range response.Routes {
		if route.Menu != "" {
			t.Errorf("management route %s carries menu %q; the hub owns the only sidebar entry", route.Path, route.Menu)
		}
	}
	wantResources := []string{"/status", "/login", "/checkin"}
	gotResources := make([]string, 0, len(response.Resources))
	for _, resource := range response.Resources {
		if resource.Menu != "" {
			t.Errorf("resource %s carries menu %q", resource.Path, resource.Menu)
		}
		gotResources = append(gotResources, resource.Path)
	}
	if !reflect.DeepEqual(gotResources, wantResources) {
		t.Errorf("resources = %v, want %v", gotResources, wantResources)
	}
}

func TestConfigFromYAML(t *testing.T) {
	cfg := ConfigFromYAML([]byte(`{
		enabled: no, priority: "7", discover_models: off, model_prefix: false,
		model_cache_ttl_ms: "1234", request_timeout_ms: 100,
		catalogue_timeout_ms: "200", oauth_timeout_ms: 300, login_timeout_ms: 400,
		poll_interval_seconds: "9", refresh_window_seconds: 10,
		max_tokens: "2048", timezone_id: Asia/Shanghai
	}`))
	if cfg.Enabled || cfg.DiscoverModels || cfg.ModelPrefix {
		t.Errorf("boolean config = %+v", cfg)
	}
	if cfg.Priority != 7 || cfg.ModelCacheTTLMS != 1234 || cfg.RequestTimeoutMS != 100 ||
		cfg.CatalogueTimeoutMS != 200 || cfg.OAuthTimeoutMS != 300 || cfg.LoginTimeoutMS != 400 ||
		cfg.PollIntervalSeconds != 9 || cfg.RefreshWindowSeconds != 10 || cfg.DefaultMaxTokens != 2048 {
		t.Errorf("numeric config = %+v", cfg)
	}
	if cfg.timezoneID() != "Asia/Shanghai" {
		t.Errorf("timezone = %q", cfg.timezoneID())
	}
	if got := ConfigFromYAML(nil).timezoneID(); got != "UTC" {
		t.Errorf("default timezone = %q, want UTC", got)
	}
}

func TestParseCredentialRejectsMissingAccessToken(t *testing.T) {
	tests := []string{
		``, `not-json`, `{}`, `{"access_token":""}`, `{"access_token":123}`,
	}
	for _, raw := range tests {
		if credential, errParse := ParseCredential([]byte(raw)); errParse == nil {
			t.Errorf("ParseCredential(%q) = %+v, want error", raw, credential)
		}
	}
}

func TestCredentialEncodeSetsType(t *testing.T) {
	credential := &Credential{AccessToken: "mmoat_x"}
	raw, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatal(errEncode)
	}
	if credential.Type != ProviderKey {
		t.Errorf("Type = %q, want %q", credential.Type, ProviderKey)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["type"] != ProviderKey || stored["access_token"] != "mmoat_x" {
		t.Errorf("stored = %#v", stored)
	}
}

func TestDefaultAuthFileNameIsSanitized(t *testing.T) {
	credential := &Credential{AccessToken: "mmoat_1234567890", Nickname: "测试 / 账号"}
	name := defaultAuthFileName(credential)
	if !strings.HasPrefix(name, "minimax-") || !strings.HasSuffix(name, ".json") {
		t.Errorf("name = %q", name)
	}
	if strings.ContainsAny(name, `/\\ `) {
		t.Errorf("name contains a path separator or space: %q", name)
	}
}
