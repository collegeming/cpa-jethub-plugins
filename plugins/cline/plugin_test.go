package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestMethodSurface pins the complete method surface every provider plugin in
// this repository implements. A silently dropped route is invisible until a
// client hits it, so it is asserted explicitly.
func TestMethodSurface(t *testing.T) {
	expected := []string{
		pluginabi.MethodAuthIdentifier,
		pluginabi.MethodAuthParse,
		pluginabi.MethodAuthLoginStart,
		pluginabi.MethodAuthLoginPoll,
		pluginabi.MethodAuthRefresh,
		pluginabi.MethodModelRegister,
		pluginabi.MethodModelStatic,
		pluginabi.MethodModelForAuth,
		pluginabi.MethodExecutorIdentifier,
		pluginabi.MethodExecutorExecute,
		pluginabi.MethodExecutorExecuteStream,
		pluginabi.MethodExecutorCountTokens,
		pluginabi.MethodRequestTranslate,
		pluginabi.MethodResponseTranslate,
		pluginabi.MethodQuotaIdentifier,
		pluginabi.MethodQuotaDescribe,
		pluginabi.MethodQuotaFetch,
		pluginabi.MethodQuotaReset,
		pluginabi.MethodManagementRegister,
		pluginabi.MethodManagementHandle,
	}
	routes := Plugin().Routes()
	for _, method := range expected {
		if _, present := routes[method]; !present {
			t.Errorf("method %s is not registered", method)
		}
	}
	if len(routes) != len(expected) {
		t.Errorf("route count = %d, want %d", len(routes), len(expected))
	}
}

// TestRegistrationDeclaresTheProvider pins identity and capabilities.
func TestRegistrationDeclaresTheProvider(t *testing.T) {
	registration := Plugin().Registration()
	if registration.Metadata.Name != DisplayName {
		t.Errorf("name = %q", registration.Metadata.Name)
	}
	if registration.Metadata.Version != Version {
		t.Errorf("version = %q", registration.Metadata.Version)
	}
	if registration.Metadata.GitHubRepository != Repository {
		t.Errorf("repository = %q", registration.Metadata.GitHubRepository)
	}
	if len(registration.Metadata.ConfigFields) != len(ConfigFields()) {
		t.Errorf("config fields = %d, want %d", len(registration.Metadata.ConfigFields), len(ConfigFields()))
	}
	capabilities := registration.Capabilities
	if !capabilities.AuthProvider || !capabilities.ModelProvider || !capabilities.ModelRegistrar ||
		!capabilities.Executor || !capabilities.QuotaProvider || !capabilities.ManagementAPI ||
		!capabilities.RequestTranslator || !capabilities.ResponseTranslator {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Errorf("scope = %q", capabilities.ExecutorModelScope)
	}
	for _, formats := range [][]string{capabilities.ExecutorInputFormats, capabilities.ExecutorOutputFormats} {
		if len(formats) != 1 || formats[0] != "chat-completions" {
			t.Errorf("formats = %v", formats)
		}
	}
	if registration.SchemaVersion == 0 {
		t.Error("the schema version must be stamped by the bootstrap")
	}
}

// TestConfigureAppliesSettings pins the lifecycle hook.
func TestConfigureAppliesSettings(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	concrete, okConcrete := Plugin().(*plugin)
	if !okConcrete {
		t.Fatalf("unexpected plugin type %T", Plugin())
	}
	if errConfigure := concrete.Configure([]byte("model_discovery: false\nmax_tokens: 1234\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}
	if settings().ModelDiscovery {
		t.Error("the configured value was not applied")
	}
	if settings().DefaultMaxTokens != 1234 {
		t.Errorf("max_tokens = %d", settings().DefaultMaxTokens)
	}
}

// TestManagementRouteTable pins the two hard rules of the host's mounting
// scheme: exactly ONE route with a Menu, and every Menu-less route namespaced by
// the provider key.
func TestManagementRouteTable(t *testing.T) {
	value, errRegister := handleManagementRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("handleManagementRegister: %v", errRegister)
	}
	registration, okRegistration := value.(pluginapi.ManagementRegistrationResponse)
	if !okRegistration {
		t.Fatalf("unexpected reply %T", value)
	}
	menus := 0
	for _, route := range registration.Routes {
		if route.Menu != "" {
			menus++
			if route.Method != http.MethodGet {
				t.Errorf("a menu route must be GET (the resource mount is GET-only): %s %s", route.Method, route.Path)
			}
			if route.Path != "/status" {
				t.Errorf("the single menu route must be the status page, got %q", route.Path)
			}
		} else if !strings.HasPrefix(route.Path, "/"+ProviderKey+"/") {
			t.Errorf("a Menu-less management route must be provider-prefixed: %q", route.Path)
		}
		if route.Description == "" {
			t.Errorf("route %q has no description", route.Path)
		}
	}
	if menus != 1 {
		t.Fatalf("menu routes = %d, want exactly 1", menus)
	}
	if len(registration.Resources) == 0 {
		t.Fatal("the login page must be reachable as a resource route")
	}
	for _, resource := range registration.Resources {
		if resource.Menu != "" {
			t.Errorf("a resource route must not add a sidebar entry: %q", resource.Path)
		}
		if resource.Path != "/login" {
			t.Errorf("unexpected resource route %q", resource.Path)
		}
	}
}

// TestManagementRouteNormalisation pins that the last path segment identifies the
// route on both mounts.
func TestManagementRouteNormalisation(t *testing.T) {
	cases := map[string]string{
		"/v0/resource/plugins/cline/status": "/status",
		"/v0/management/cline/refresh":      "/refresh",
		"/v0/management/cline/refresh/":     "/refresh",
		"/status":                           "/status",
		"":                                  "/",
	}
	for input, want := range cases {
		if got := managementRoute(input); got != want {
			t.Errorf("managementRoute(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestWantsJSON pins the negotiation rule the pages rely on.
func TestWantsJSON(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		query   url.Values
		want    bool
	}{
		{"browser navigation", http.Header{"Accept": []string{"text/html,application/xhtml+xml"}}, nil, false},
		{"explicit format", http.Header{"Accept": []string{"text/html"}}, url.Values{"format": []string{"json"}}, true},
		{"script without accept", nil, nil, false},
		{"json client", http.Header{"Accept": []string{"application/json"}}, nil, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := pluginapi.ManagementRequest{Headers: testCase.headers, Query: testCase.query}
			if got := wantsJSON(request); got != testCase.want {
				t.Fatalf("wantsJSON = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestManagementDispatchRejectsUnknownRoutes pins the 404 answer.
func TestManagementDispatchRejectsUnknownRoutes(t *testing.T) {
	value, errHandler := handleManagementHandle(nil, marshalRequest(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/cline/nope",
	}))
	if errHandler != nil {
		t.Fatalf("handleManagementHandle: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.ManagementResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

// TestStatusJSONWithoutAccounts pins that the machine-readable status works with
// no credential and never leaks a token.
func TestStatusJSONWithoutAccounts(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	value, errHandler := handleManagementHandle(nil, marshalRequest(t, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/management/cline/status",
		Headers: http.Header{"Accept": []string{"application/json"}},
	}))
	if errHandler != nil {
		t.Fatalf("handleManagementHandle: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.ManagementResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded["provider"] != ProviderKey || decoded["token_prefix"] != TokenPrefix {
		t.Fatalf("payload = %v", decoded)
	}
	if decoded["account"] != nil {
		t.Errorf("account = %v, want null", decoded["account"])
	}
	if levels, okLevels := decoded["reasoning_levels"].([]any); !okLevels || len(levels) != 5 {
		t.Errorf("reasoning_levels = %v", decoded["reasoning_levels"])
	}
	if strings.Contains(string(response.Body), "access_token") {
		t.Error("the status payload must never carry a token field")
	}
}

// TestStatusJSONReportsTheCatalogueCache pins the diagnostic that tells an
// operator whether the online catalogue was ever fetched (the free list is
// server-side marketing state, so a stale cache matters).
func TestStatusJSONReportsTheCatalogueCache(t *testing.T) {
	resetModelCache(t)
	withTestSettings(t, DefaultConfig())
	discoveredModels.put([]pluginapi.ModelInfo{{ID: "cline-free/x"}})
	response := statusJSON(nil, pageRequest("/status", nil))
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded["cached_model_count"] != float64(1) {
		t.Errorf("cached_model_count = %v", decoded["cached_model_count"])
	}
	if decoded["cached_model_fetched_at"] == "" {
		t.Errorf("cached_model_fetched_at = %v", decoded["cached_model_fetched_at"])
	}
}

// TestRefreshJSONWithoutAccount pins the script-facing refresh failure.
func TestRefreshJSONWithoutAccount(t *testing.T) {
	value, errHandler := handleManagementHandle(nil, marshalRequest(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/cline/refresh",
	}))
	if errHandler != nil {
		t.Fatalf("handleManagementHandle: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.ManagementResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}

// TestStatusText pins the account status rendering.
func TestStatusText(t *testing.T) {
	cases := []struct {
		name  string
		entry pluginapi.HostAuthFileEntry
		want  string
	}{
		{"disabled", pluginapi.HostAuthFileEntry{Disabled: true}, "已停用"},
		{"unavailable", pluginapi.HostAuthFileEntry{Unavailable: true}, "不可用"},
		{"status with a message", pluginapi.HostAuthFileEntry{Status: "error", StatusMessage: "401"}, "error（401）"},
		{"status only", pluginapi.HostAuthFileEntry{Status: "ok"}, "ok"},
		{"default", pluginapi.HostAuthFileEntry{}, "正常"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := statusText(testCase.entry); got != testCase.want {
				t.Fatalf("statusText = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestDefaultAuthFileName pins that auth file names are stable and safe.
func TestDefaultAuthFileName(t *testing.T) {
	cases := []struct {
		name       string
		credential Credential
		want       string
	}{
		{"email", Credential{Email: "a@b.c"}, "cline-a@b.c.json"},
		{"nickname wins", Credential{Nickname: "Ada", Email: "a@b.c"}, "cline-Ada.json"},
		{"account id", Credential{AccountID: "usr-1"}, "cline-usr-1.json"},
		{"token fallback", Credential{AccessToken: "workos:abcdefghijkl"}, "cline-abcdefgh.json"},
		{"nothing", Credential{}, "cline-account.json"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			credential := testCase.credential
			if got := defaultAuthFileName(&credential); got != testCase.want {
				t.Fatalf("defaultAuthFileName = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestAuthDataForNeverStoresABareToken pins the storage round trip and the
// refresh deadline the host schedules from.
func TestAuthDataForNeverStoresABareToken(t *testing.T) {
	credential := &Credential{
		AccessToken:  "eyJ",
		RefreshToken: "r",
		AccountID:    "usr-1",
		Email:        "a@b.c",
	}
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		t.Fatalf("authDataFor: %v", errAuth)
	}
	if auth.Provider != ProviderKey || auth.FileName == "" || auth.Label != "a@b.c" {
		t.Fatalf("auth = %+v", auth)
	}
	if auth.Prefix != "usr-1" {
		t.Errorf("prefix = %q", auth.Prefix)
	}
	if auth.Attributes["refreshable"] != "true" {
		t.Errorf("attributes = %v", auth.Attributes)
	}
	if auth.Metadata["token_prefix"] != TokenPrefix {
		t.Errorf("metadata = %v", auth.Metadata)
	}
	if !auth.NextRefreshAfter.IsZero() {
		t.Errorf("a credential without an expiry must not schedule a refresh: %v", auth.NextRefreshAfter)
	}
	var stored Credential
	if errUnmarshal := json.Unmarshal(auth.StorageJSON, &stored); errUnmarshal != nil {
		t.Fatalf("decode storage: %v", errUnmarshal)
	}
	if stored.AccessToken != "workos:eyJ" {
		t.Fatalf("stored token = %q, want the prefixed form", stored.AccessToken)
	}
}

// TestAuthDataForSchedulesRefreshBeforeExpiry pins the 1 h lead.
func TestAuthDataForSchedulesRefreshBeforeExpiry(t *testing.T) {
	expiry := time.Now().Add(3 * time.Hour)
	credential := &Credential{AccessToken: "workos:t", RefreshToken: "r", ExpireTime: expiry.UnixMilli()}
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		t.Fatalf("authDataFor: %v", errAuth)
	}
	want := expiry.Add(-refreshLead)
	if delta := auth.NextRefreshAfter.Sub(want); delta > time.Second || delta < -time.Second {
		t.Fatalf("NextRefreshAfter = %v, want %v", auth.NextRefreshAfter, want)
	}
}

// TestParseAuthFileRoutesToTheProvider pins auth.parse.
func TestParseAuthFileRoutesToTheProvider(t *testing.T) {
	raw := []byte(`{"access_token":"workos:eyJ","refresh_token":"r","account_id":"usr-1"}`)
	value, errHandler := handleAuthParse(nil, marshalRequest(t, pluginapi.AuthParseRequest{Provider: ProviderKey, RawJSON: raw, FileName: "x.json"}))
	if errHandler != nil {
		t.Fatalf("handleAuthParse: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.AuthParseResponse)
	if !okResponse || !response.Handled {
		t.Fatalf("reply = %T %+v", value, response)
	}
	if response.Auth.Provider != ProviderKey {
		t.Errorf("provider = %q", response.Auth.Provider)
	}

	// Another provider's material is not ours to claim.
	value, errHandler = handleAuthParse(nil, marshalRequest(t, pluginapi.AuthParseRequest{Provider: "qoder", RawJSON: raw}))
	if errHandler != nil {
		t.Fatalf("handleAuthParse: %v", errHandler)
	}
	foreign, okForeign := value.(pluginapi.AuthParseResponse)
	if !okForeign || foreign.Handled {
		t.Fatalf("a foreign provider must not be handled: %+v", foreign)
	}

	// Junk is not ours either.
	value, errHandler = handleAuthParse(nil, marshalRequest(t, pluginapi.AuthParseRequest{RawJSON: []byte(`nope`)}))
	if errHandler != nil {
		t.Fatalf("handleAuthParse: %v", errHandler)
	}
	junk, okJunk := value.(pluginapi.AuthParseResponse)
	if !okJunk || junk.Handled {
		t.Fatalf("junk must not be handled: %+v", junk)
	}
}

// TestQuotaSurface pins the declared quota capability: balance yes, reset no
// (`jet-hub-rpc.ts:1487-1502`).
func TestQuotaSurface(t *testing.T) {
	value, errDescribe := handleQuotaDescribe(nil, nil)
	if errDescribe != nil {
		t.Fatalf("handleQuotaDescribe: %v", errDescribe)
	}
	describe, okDescribe := value.(pluginapi.QuotaDescribeResponse)
	if !okDescribe {
		t.Fatalf("unexpected reply %T", value)
	}
	if len(describe.SupportedProviders) != 1 || describe.SupportedProviders[0] != ProviderKey {
		t.Fatalf("providers = %v", describe.SupportedProviders)
	}
	if describe.SupportsReset {
		t.Error("Cline has no quota reset endpoint")
	}

	value, errReset := handleQuotaReset(nil, nil)
	if errReset != nil {
		t.Fatalf("handleQuotaReset: %v", errReset)
	}
	reset, okReset := value.(pluginapi.QuotaResetResponse)
	if !okReset || reset.Success {
		t.Fatalf("reset = %+v", reset)
	}
	if !strings.Contains(reset.Message, "不支持") {
		t.Errorf("message = %q", reset.Message)
	}

	value, errIdentifier := handleQuotaIdentifier(nil, nil)
	if errIdentifier != nil {
		t.Fatalf("handleQuotaIdentifier: %v", errIdentifier)
	}
	if raw, errMarshal := json.Marshal(value); errMarshal != nil || string(raw) != `{"identifier":"cline"}` {
		t.Fatalf("identifier = %s (%v)", raw, errMarshal)
	}
}

// TestQuotaFetchReportsTheBalance pins the normalized metric.
func TestQuotaFetchReportsTheBalance(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{AccessToken: "workos:eyJ", AccountID: "usr-1"}
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(200,
		`{"data":{"userId":"usr-01M3BCV4FYCGJKAWD3MJG3DBQM","balance":500000},"success":true}`)}}}
	installFakeTransport(t, fake)

	value, errHandler := handleQuotaFetch(nil, marshalRequest(t, pluginapi.QuotaFetchRequest{
		Provider:    ProviderKey,
		StorageJSON: storage,
	}))
	if errHandler != nil {
		t.Fatalf("handleQuotaFetch: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.QuotaFetchResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	if len(response.Summary) != 1 {
		t.Fatalf("summary = %+v", response.Summary)
	}
	metric := response.Summary[0]
	if metric.Key != "balance" || metric.Value != 5 || metric.Currency != "USD" || metric.Format != "currency" {
		t.Fatalf("metric = %+v", metric)
	}
	if fake.call(0).URL != APIBase+balancePath("usr-1") {
		t.Fatalf("url = %q", fake.call(0).URL)
	}
	if got := fake.call(0).Headers.Get("Authorization"); got != "Bearer workos:eyJ" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestQuotaFetchWithoutAccountID pins the explainable failure.
func TestQuotaFetchWithoutAccountID(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	credential := &Credential{AccessToken: "workos:eyJ"}
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	fake := &fakeTransport{}
	installFakeTransport(t, fake)
	_, errHandler := handleQuotaFetch(nil, marshalRequest(t, pluginapi.QuotaFetchRequest{StorageJSON: storage}))
	if errHandler == nil {
		t.Fatal("a credential without an account id cannot query the balance")
	}
	if status := statusOf(errHandler, 0); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if fake.callCount() != 0 {
		t.Errorf("no request may be issued: %d", fake.callCount())
	}
}

// TestParseBalance pins the parsing order of `cline-credits.ts:142-162`,
// including the 401 body that carries no `success` field at all.
func TestParseBalance(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantValue float64
		wantErr   string
	}{
		{"measured body", `{"data":{"userId":"usr-1","balance":500000},"success":true}`, 500000, ""},
		{"numeric string balance", `{"data":{"balance":"250000"},"success":true}`, 250000, ""},
		{"the 401 body has no success field", `{"error":"Unauthorized: Please make sure you're using the latest version of Cline and re-authenticate your Cline account."}`, 0, "Unauthorized"},
		{"success false", `{"data":{"balance":1},"success":false,"error":"nope"}`, 0, "nope"},
		{"missing data", `{"success":true}`, 0, "缺少 data"},
		{"balance of the wrong type", `{"data":{"balance":{"amount":1}},"success":true}`, 0, "不是数字"},
		{"non-json", `<html>502</html>`, 0, "缺少 data"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			value, _, errParse := parseBalance([]byte(testCase.body))
			if testCase.wantErr == "" {
				if errParse != nil {
					t.Fatalf("unexpected error: %v", errParse)
				}
				if value != testCase.wantValue {
					t.Fatalf("value = %v, want %v", value, testCase.wantValue)
				}
				return
			}
			if errParse == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(errParse.Error(), testCase.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", errParse.Error(), testCase.wantErr)
			}
		})
	}
}

// TestFetchBalanceClassification pins the status mapping of a failed lookup: a
// dead credential must stay a 401 rather than becoming a fake 0 balance.
func TestFetchBalanceClassification(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantValue  float64
	}{
		{"ok", 200, `{"data":{"userId":"usr-1","balance":500000},"success":true}`, 200, 5},
		{"unauthorized", 401, `{"error":"Unauthorized: Please make sure you're using the latest version of Cline"}`, 401, 0},
		{"server error", 500, `{"error":"boom"}`, 502, 0},
		{"shape failure", 200, `{"success":true}`, 502, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(testCase.status, testCase.body)}}}
			balance, errBalance := fetchBalance(fake.do, &Credential{AccessToken: "workos:eyJ", AccountID: "usr-1"}, DefaultConfig())
			if testCase.wantStatus == 200 {
				if errBalance != nil {
					t.Fatalf("unexpected error: %v", errBalance)
				}
				if balance.Total != testCase.wantValue || balance.Raw != 500000 || balance.Unit != "USD" {
					t.Fatalf("balance = %+v", balance)
				}
				return
			}
			if errBalance == nil {
				t.Fatal("expected an error")
			}
			if status := statusOf(errBalance, 0); status != testCase.wantStatus {
				t.Fatalf("status = %d, want %d (%v)", status, testCase.wantStatus, errBalance)
			}
		})
	}
}
