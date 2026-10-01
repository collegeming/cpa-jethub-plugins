package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Management-page tests.
//
// The host mounts a GET resource route under `/v0/resource/plugins/<id>/<path>` and
// dispatches it as GET ONLY. Two consequences shape the pages and are asserted here:
//
//   - every action is a LINK carrying a query string; a form would never reach the
//     plugin from inside the manager's iframe;
//   - a resource route that carries a `Menu` becomes a sidebar entry, and the
//     repository spends its single entry on the `hub` plugin. This plugin therefore
//     declares none — see zz_menucheck_test.go, which guards the count.
//
// Escape discipline is also asserted: account labels and upstream error text end up
// in the markup, and they come from outside.

// TestManagementDispatchesTheDeclaredRoutes covers the router.
func TestManagementDispatchesTheDeclaredRoutes(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, BalancePath):
			return httpResponse(http.StatusOK, balanceBody(1000, 2000)), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
		default:
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		}
	}
	storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())

	cases := []struct {
		path        string
		wantContain string
		wantType    string
	}{
		{"/status", "ZCode", "text/html"},
		{"/login", "登录", "text/html"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			response := callManagement(t, testHost(), managementRequest(
				http.MethodGet, "/v0/resource/plugins/zcode"+tc.path, url.Values{}, nil))
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			if got := response.Headers.Get("Content-Type"); !strings.Contains(got, tc.wantType) {
				t.Fatalf("content type = %q, want %s", got, tc.wantType)
			}
			if !strings.Contains(string(response.Body), tc.wantContain) {
				t.Fatalf("body does not contain %q", tc.wantContain)
			}
		})
	}

	// An unknown route is a 404 rather than a panic.
	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/nope", url.Values{}, nil))
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", response.StatusCode)
	}
}

// TestPagesUseLinksNotForms covers the GET-only dispatch rule.
func TestPagesUseLinksNotForms(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, BalancePath):
			return httpResponse(http.StatusOK, balanceBody(1000, 2000)), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
		default:
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		}
	}
	storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())

	for _, path := range []string{"/status", "/login"} {
		t.Run(path, func(t *testing.T) {
			response := callManagement(t, testHost(), managementRequest(
				http.MethodGet, "/v0/resource/plugins/zcode"+path, url.Values{}, nil))
			body := string(response.Body)
			if strings.Contains(strings.ToLower(body), "<form") {
				t.Error("the page contains a form, which the GET-only resource mount would never deliver")
			}
			if strings.Contains(body, "POST") {
				t.Error("the page mentions POST")
			}
			// No JavaScript either: the manager renders the page in a sandboxed
			// iframe and the repository's helper has no script support.
			if strings.Contains(strings.ToLower(body), "<script") {
				t.Error("the page contains a script tag")
			}
		})
	}
}

func TestLoginPageStartsAFlowOnlyOnAnExplicitAction(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	inits := 0
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(request.URL, OAuthCLIInitPath) {
			inits++
			return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
				"code": 0,
				"data": map[string]any{
					"flow_id": "f1", "authorize_url": "https://bigmodel.cn/login",
					"poll_interval_sec": float64(2),
				},
			})), nil
		}
		return httpResponse(http.StatusOK, `{"code":0}`), nil
	}

	// A plain load renders the steps but starts nothing.
	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/login", url.Values{}, nil))
	if inits != 0 {
		t.Fatalf("a plain page load started %d authorization flows", inits)
	}
	if !strings.Contains(string(response.Body), "开始授权") {
		t.Error("the page offers no way to start the flow")
	}

	// The explicit action renders the URL.
	response = callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/login", url.Values{"action": {"start"}}, nil))
	if inits != 1 {
		t.Fatalf("the explicit action started %d flows, want 1", inits)
	}
	if !strings.Contains(string(response.Body), "https://bigmodel.cn/login") {
		t.Error("the rendered page does not show the authorization URL")
	}
}

// TestStatusJSONReportsEveryAccountWithItsOwnBalance covers the machine-readable
// view.
//
// ⚠ Each entry carries its OWN balance. Repeating one account's numbers across a list
// is the defect that made a multi-account sweep useless.
func TestStatusJSONReportsEveryAccountWithItsOwnBalance(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)

	first := sampleCredential()
	first.UserID = "user-first"
	first.AccountLabel = "first account"
	first.ZCodeJWT = "jwt-for-the-first-account"
	second := sampleCredential()
	second.UserID = "user-second"
	second.AccountLabel = "second account"
	second.ZCodeJWT = "jwt-for-the-second-account"
	storedCredential(t, fake, "auth-1", "zcode-1.json", first)
	storedCredential(t, fake, "auth-2", "zcode-2.json", second)

	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, BalancePath):
			// Answer differently per credential so a shared number would be visible.
			if strings.Contains(request.Headers.Get("Authorization"), first.ZCodeJWT) {
				return httpResponse(http.StatusOK, balanceBody(111, 1000)), nil
			}
			return httpResponse(http.StatusOK, balanceBody(222, 2000)), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
		default:
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		}
	}

	response := callManagement(t, testHost(), jsonManagementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if payload["account_count"] != float64(2) {
		t.Fatalf("account_count = %v, want 2", payload["account_count"])
	}
	accounts, _ := payload["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts length = %d", len(accounts))
	}
	remaining := map[string]float64{}
	for _, raw := range accounts {
		entry, _ := raw.(map[string]any)
		label, _ := entry["label"].(string)
		value, _ := entry["remaining_units"].(float64)
		remaining[label] = value
		// The unit travels with the number.
		if entry["unit_type"] != "token" {
			t.Errorf("account %q unit_type = %v, want token", label, entry["unit_type"])
		}
		if entry["remaining_tokens"] == "" {
			t.Errorf("account %q has no rendered token magnitude", label)
		}
	}
	if remaining["first account"] != 111 || remaining["second account"] != 222 {
		t.Fatalf("per-account balances = %v, want 111 and 222", remaining)
	}

	// The identity rule is stated in the payload, because a caller correlating
	// accounts needs to know which field is the identity.
	for _, raw := range accounts {
		entry, _ := raw.(map[string]any)
		note, _ := entry["identity_note"].(string)
		if !strings.Contains(note, "user_id") || !strings.Contains(note, "device_mid") {
			t.Errorf("the identity note does not explain the distinction: %q", note)
		}
	}
}

// TestStatusJSONReportsABrokenCredentialWithoutZeroingIt covers the honesty rule.
//
// A credential that cannot be read is reported as an error and NOT as a zero: a
// zeroed balance reads as "used up", which is the opposite of "unknown".
func TestStatusJSONReportsABrokenCredentialWithoutZeroingIt(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.files = append(fake.files, pluginapi.HostAuthFileEntry{
		Provider: ProviderKey, AuthIndex: "auth-broken", Name: "broken.json", Status: "active",
	})
	fake.auths["auth-broken"] = []byte(`{"nonsense":true}`)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatal("a request was issued for an unreadable credential")
		return nil, nil
	}

	response := callManagement(t, testHost(), jsonManagementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}))
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	accounts, _ := payload["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts length = %d", len(accounts))
	}
	entry, _ := accounts[0].(map[string]any)
	if entry["credential_error"] == nil || entry["credential_error"] == "" {
		t.Fatal("the unreadable credential was not reported as an error")
	}
	for _, forbidden := range []string{"remaining_units", "total_units", "balance"} {
		if _, present := entry[forbidden]; present {
			t.Errorf("the entry carries %q, which would present an unknown balance as a real one", forbidden)
		}
	}
}

// TestStatusJSONStatesTheCaptchaAndIdentityDecisions covers the transparency fields.
//
// Both are counter-intuitive enough that a reader will otherwise assume the plugin is
// broken: it never mints a captcha, and the identity block's structure is what opens
// the gate.
func TestStatusJSONStatesTheCaptchaAndIdentityDecisions(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	response := callManagement(t, testHost(), jsonManagementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}))
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}

	captcha, _ := payload["captcha"].(map[string]any)
	if captcha["used_for_inference"] != false {
		t.Errorf("captcha.used_for_inference = %v, want false", captcha["used_for_inference"])
	}
	captchaNote, _ := captcha["note"].(string)
	for _, want := range []string{"200", "claim"} {
		if !strings.Contains(captchaNote, want) {
			t.Errorf("the captcha note does not mention %q: %s", want, captchaNote)
		}
	}

	identity, _ := payload["identity_block"].(map[string]any)
	if identity["total_chars"] == nil {
		t.Fatal("the identity block does not report its size")
	}
	penalty, _ := identity["penalty"].(string)
	if !strings.Contains(penalty, "冷却") {
		t.Errorf("the identity block does not warn about the penalty: %q", penalty)
	}
	note, _ := identity["note"].(string)
	if !strings.Contains(note, "独立文本块") {
		t.Errorf("the note does not explain the structural requirement: %q", note)
	}

	// The refresh contract is stated as unsupported rather than left ambiguous.
	if payload["refreshable"] != false {
		t.Errorf("refreshable = %v, want false", payload["refreshable"])
	}
	if !strings.Contains(payload["refresh_note"].(string), "没有 exp") {
		t.Errorf("the refresh note does not give the reason: %v", payload["refresh_note"])
	}
}

// TestLoginJSONReportsNoLocalCallback covers the login payload.
func TestLoginJSONReportsNoLocalCallback(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	response := callManagement(t, testHost(), jsonManagementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/login", url.Values{}))
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if payload["uses_local_callback"] != false {
		t.Errorf("uses_local_callback = %v, want false", payload["uses_local_callback"])
	}
	if payload["callback_port"] != nil {
		t.Errorf("callback_port = %v, want null", payload["callback_port"])
	}
	if !strings.Contains(payload["device_mid_note"].(string), "3001") {
		t.Errorf("the device-id note does not cite the failure it avoids: %v", payload["device_mid_note"])
	}
}

func TestStatusPageEscapesUpstreamText(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	hostile := sampleCredential()
	hostile.AccountLabel = `<script>alert("xss")</script>`
	hostile.UserID = `"><img src=x onerror=alert(1)>`
	storedCredential(t, fake, "auth-1", "zcode-1.json", hostile)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, BalancePath):
			// A hostile upstream message too.
			return httpResponse(http.StatusBadGateway, `{"msg":"<script>alert('upstream')</script>"}`), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
		default:
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		}
	}

	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}, nil))
	body := string(response.Body)
	// The payload must not survive as MARKUP. The distinction matters: the words
	// themselves may appear as escaped TEXT (and should — dropping them would hide a
	// real diagnostic), but no tag or attribute may be formed from them.
	for _, raw := range []string{
		"<script>alert",            // a live script element
		`"><img src=x onerror=`,    // an attribute break-out
		"<img src=x onerror=alert", // a live image element
	} {
		if strings.Contains(body, raw) {
			t.Fatalf("a hostile value reached the markup as %q", raw)
		}
	}
	// The content is still SHOWN, just escaped.
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the hostile label was dropped rather than escaped")
	}
}

// TestStatusPageRendersTheConfigurationSummary covers what the page tells the user.
func TestStatusPageRendersTheConfigurationSummary(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}, nil))
	body := string(response.Body)

	// The endpoint and protocol are shown, because this channel is Anthropic-shaped
	// and a user comparing it to the OpenAI-compatible siblings needs to know.
	if !strings.Contains(body, MessagesPath) {
		t.Error("the page does not show the inference endpoint")
	}
	if !strings.Contains(body, "Anthropic") {
		t.Error("the page does not name the protocol")
	}
	// The two counter-intuitive decisions are explained on the page itself.
	if !strings.Contains(body, "captcha") {
		t.Error("the page does not explain the captcha decision")
	}
	if !strings.Contains(body, "身份块") {
		t.Error("the page does not explain the identity block")
	}
	// The static-credential contract.
	if !strings.Contains(body, "不支持") {
		t.Error("the page does not state that renewal is unsupported")
	}
	// An empty instance offers the way forward.
	if !strings.Contains(body, "去登录") {
		t.Error("an instance with no account offers no login link")
	}
}

// TestStatusPageListsTheVerifiedCatalogueOnly covers the catalogue card.
func TestStatusPageListsTheVerifiedCatalogueOnly(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, BalancePath):
			return httpResponse(http.StatusOK, balanceBody(1000, 2000)), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
		default:
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		}
	}
	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}, nil))
	body := string(response.Body)

	for _, id := range []string{"GLM-5.3-Flash", "GLM-5.3"} {
		if !strings.Contains(body, id) {
			t.Errorf("the catalogue does not list %s", id)
		}
	}
	// The unusable models are NAMED as excluded rather than silently absent, so a
	// user who saw them in the official client understands why.
	if !strings.Contains(body, "GLM-5-Turbo") || !strings.Contains(body, "返回空响应") {
		t.Error("the page does not explain why two upstream models are hidden")
	}
	// The naming collision is explained rather than worked around.
	if !strings.Contains(body, "合并") {
		t.Error("the page does not explain the same-name model merge")
	}
	// The reasoning levels are shown.
	if !strings.Contains(body, "low") || !strings.Contains(body, "max") {
		t.Error("the catalogue does not show the reasoning levels")
	}
}

// TestLoginPageHonoursTheAddAccountQuery covers the second-account affordance.
func TestLoginPageHonoursTheAddAccountQuery(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/login", url.Values{"add": {"1"}}, nil))
	if !strings.Contains(string(response.Body), "新增账号") {
		t.Error("the add-account page does not say it is adding an account")
	}
	// Without the query the notice is absent.
	response = callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/login", url.Values{}, nil))
	if strings.Contains(string(response.Body), "新增账号") {
		t.Error("the notice appears without the add query")
	}
}

// TestImportCardRespectsTheSetting covers the optional import path's gate.
func TestImportCardRespectsTheSetting(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	isolateClientStore(t)

	// Off by default: the page explains the setting instead of importing.
	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/login", url.Values{"action": {"import"}}, nil))
	if !strings.Contains(string(response.Body), "import_client_credential") {
		t.Error("the disabled import does not name the setting that enables it")
	}

	// On, but with no client store present: it explains the failure.
	cfg := settings()
	cfg.ImportClientCredential = true
	withSettings(t, cfg)
	response = callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/login", url.Values{"action": {"import"}}, nil))
	body := string(response.Body)
	if !strings.Contains(body, "credentials.json") {
		t.Error("the failed import does not name the file it looked for")
	}
}

// TestWantsJSONDetection covers the content negotiation.
func TestWantsJSONDetection(t *testing.T) {
	cases := []struct {
		name     string
		query    url.Values
		accept   string
		wantJSON bool
	}{
		{"an explicit format parameter", url.Values{"format": {"json"}}, "text/html", true},
		{"an uppercase format parameter", url.Values{"format": {"JSON"}}, "text/html", true},
		{"a JSON accept header", url.Values{}, "application/json", true},
		{"a browser navigation", url.Values{}, "text/html,application/xhtml+xml", false},
		{"no accept header at all", url.Values{}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := managementRequest(http.MethodGet, "/status", tc.query, nil)
			if tc.accept == "" {
				request.Headers.Del("Accept")
			} else {
				request.Headers.Set("Accept", tc.accept)
			}
			if got := wantsJSON(request); got != tc.wantJSON {
				t.Fatalf("wantsJSON = %v, want %v", got, tc.wantJSON)
			}
		})
	}
}

// TestManagementRouteReducesToTheLastSegment covers the router's path handling:
// the request path differs per mount, so only the last segment identifies a route.
func TestManagementRouteReducesToTheLastSegment(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v0/resource/plugins/zcode/status", "/status"},
		{"/v0/resource/plugins/zcode/status/", "/status"},
		{"/status", "/status"},
		{"status", "/status"},
		{"/v0/management/zcode/login", "/login"},
		{"", "/"},
	}
	for _, tc := range cases {
		if got := managementRoute(tc.path); got != tc.want {
			t.Errorf("managementRoute(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestSelectAccountResolvesByEveryIdentifier covers the account selector, which the
// hub's links use.
func TestSelectAccountResolvesByEveryIdentifier(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	entry := storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())

	cases := []struct {
		name  string
		query url.Values
		want  bool
	}{
		{"no selector uses the first account", url.Values{}, true},
		{"by auth index", url.Values{"auth_index": {"auth-1"}}, true},
		{"by auth id", url.Values{"auth_id": {"auth-1"}}, true},
		{"by name", url.Values{"auth_index": {entry.Name}}, true},
		{"an unknown selector finds nothing", url.Values{"auth_index": {"nope"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := managementRequest(http.MethodGet, "/status", tc.query, nil)
			_, found := selectAccount(testHost(), request)
			if found != tc.want {
				t.Fatalf("selectAccount found = %v, want %v", found, tc.want)
			}
		})
	}
}

// TestZcodeAccountsFiltersOtherProviders covers the provider filter: the host's list
// is host-wide.
func TestZcodeAccountsFiltersOtherProviders(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.files = []pluginapi.HostAuthFileEntry{
		{Provider: "qoder", AuthIndex: "q1", Name: "qoder-1.json"},
		{Provider: ProviderKey, AuthIndex: "z1", Name: "zcode-1.json"},
		{Type: ProviderKey, AuthIndex: "z2", Name: "zcode-2.json"},
		{Provider: "trae", AuthIndex: "t1", Name: "trae-1.json"},
	}
	accounts := zcodeAccounts(testHost())
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	for _, entry := range accounts {
		if entry.AuthIndex != "z1" && entry.AuthIndex != "z2" {
			t.Errorf("a foreign credential %q was included", entry.AuthIndex)
		}
	}
}

// TestBrandIconIsADataURL covers the logo: the panel must not need an outbound
// request, and the value has to be something a browser renders.
func TestBrandIconIsADataURL(t *testing.T) {
	if !strings.HasPrefix(logo, "data:image/svg+xml,") {
		t.Fatalf("logo = %.40q, want an inline SVG data URL", logo)
	}
	// ⚠ `http://www.w3.org/2000/svg` is the SVG NAMESPACE, not a fetchable resource —
	// asserting "no http substring" would fail on every valid inline SVG. What must
	// not appear is a reference the browser would actually resolve.
	for _, forbidden := range []string{"href=", "src=", "<image", "url("} {
		if strings.Contains(logo, forbidden) {
			t.Errorf("the logo contains %q, so the panel would make an outbound request", forbidden)
		}
	}
	// The SVG is percent-encoded, which is what makes it safe to put in an attribute.
	if !strings.Contains(logo, "%3Csvg") {
		t.Error("the SVG is not percent-encoded")
	}
	registration := Plugin().Registration()
	if registration.Metadata.Logo != logo {
		t.Error("the registration does not carry the logo")
	}
}
