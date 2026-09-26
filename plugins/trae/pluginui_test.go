package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The management pages are HTML first: CPA-Manager-Plus embeds the resource route
// in an iframe, so a page has to render without any upstream reachability, and
// every action must be a GET link with a query string.

// fakeHost installs a host transport so the pages can be exercised in-process,
// with no network. Every reply is a well-formed host envelope.
func fakeHost(t *testing.T, handler func(method string, request []byte) (any, error)) {
	t.Helper()
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		result, errHandler := handler(method, request)
		if errHandler != nil {
			return json.Marshal(abiboot.Envelope{
				OK:    false,
				Error: &abiboot.EnvelopeError{Code: "stub", Message: errHandler.Error()},
			})
		}
		return abiboot.OK(result)
	})
	t.Cleanup(abiboot.ClearHostCaller)
}

// credentialJSON is a usable TRAE credential for the fake host.
func credentialJSON(t *testing.T) json.RawMessage {
	t.Helper()
	encoded, errMarshal := json.Marshal(&Credential{
		Type:         ProviderKey,
		AccessToken:  "tok",
		RefreshToken: "refresh",
		ExpiresAt:    "99999999999999",
		UID:          "uid-1",
		Nickname:     "alice",
		MachineID:    "0123456789abcdef0123456789abcdef",
		DeviceID:     "fedcba9876543210fedcba9876543210",
		Region:       RegionCN,
	})
	if errMarshal != nil {
		t.Fatalf("marshal credential: %v", errMarshal)
	}
	return encoded
}

// accountHost answers credential enumeration plus the three upstream endpoints the
// status page touches, so the page renders fully offline.
func accountHost(t *testing.T, requests *[]string) {
	t.Helper()
	credential := credentialJSON(t)
	fakeHost(t, func(method string, request []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return map[string]any{"files": []map[string]any{{
				"name":       "trae-alice.json",
				"auth_index": "idx-1",
				"provider":   ProviderKey,
				"type":       ProviderKey,
				"status":     "active",
			}}}, nil
		case pluginabi.MethodHostAuthGet:
			return map[string]any{"auth_index": "idx-1", "name": "trae-alice.json", "json": credential}, nil
		case pluginabi.MethodHostHTTPDo:
			var payload struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(request, &payload)
			if requests != nil {
				*requests = append(*requests, payload.URL)
			}
			switch {
			case strings.Contains(payload.URL, BatchModelsPath):
				return map[string]any{
					"StatusCode": 200,
					"Body":       base64.StdEncoding.EncodeToString(batchBody()),
				}, nil
			case strings.Contains(payload.URL, CheckinStatusPath):
				body, _ := json.Marshal(map[string]any{"code": 0, "checked_in": true, "credits": 150, "streak_days": 3, "enable": true})
				return map[string]any{"StatusCode": 200, "Body": base64.StdEncoding.EncodeToString(body)}, nil
			case strings.Contains(payload.URL, EntUsagePath):
				body, _ := json.Marshal(map[string]any{
					"user_entitlement_pack_list": []any{map[string]any{
						"entitlement_base_info": map[string]any{
							"name":  "签到奖励",
							"quota": map[string]any{"credits_limit": 200},
						},
						"usage": map[string]any{"credits_amount": 50},
					}},
				})
				return map[string]any{"StatusCode": 200, "Body": base64.StdEncoding.EncodeToString(body)}, nil
			case strings.Contains(payload.URL, CheckinClaimPath):
				body, _ := json.Marshal(map[string]any{"code": 0, "message": "success"})
				return map[string]any{"StatusCode": 200, "Body": base64.StdEncoding.EncodeToString(body)}, nil
			default:
				return map[string]any{"StatusCode": 404, "Body": ""}, nil
			}
		case pluginabi.MethodHostLog:
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
}

// TestManagementRouteAndWantsJSON pins the last-segment routing rule and the two
// response representations.
func TestManagementRouteAndWantsJSON(t *testing.T) {
	cases := map[string]string{
		"/v0/resource/plugins/trae/status":  "/status",
		"/v0/resource/plugins/trae/status/": "/status",
		"/v0/management/trae/checkin":       "/checkin",
		"/v0/management/trae/login":         "/login",
		"/status":                           "/status",
	}
	for path, want := range cases {
		if got := managementRoute(path); got != want {
			t.Fatalf("managementRoute(%q) = %q, want %q", path, got, want)
		}
	}

	jsonRequest := pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}}
	if !wantsJSON(jsonRequest) {
		t.Fatal("?format=json must select JSON")
	}
	htmlRequest := pluginapi.ManagementRequest{Headers: http.Header{"Accept": []string{"text/html,application/xhtml+xml"}}}
	if wantsJSON(htmlRequest) {
		t.Fatal("a browser Accept header must select HTML")
	}
	curlRequest := pluginapi.ManagementRequest{Headers: http.Header{"Accept": []string{"*/*"}}}
	if !wantsJSON(curlRequest) {
		t.Fatal("a non-HTML Accept header must select JSON")
	}
	if wantsJSON(pluginapi.ManagementRequest{}) {
		t.Fatal("a missing Accept header must select HTML")
	}
}

// TestManagementRegistrationMountRules guards the three mounts and the sidebar
// budget: NO route of this plugin may carry a Menu, because CPAMP renders one
// sidebar entry per menu route and does not group them by plugin, and the
// repository spends its single entry on the hub's "Jet Hub" page — which links to
// these pages. The status, login and check-in pages are ResourceRoutes with an
// empty Menu: still reachable in the browser, but not a nav item. Everything else
// lands in the global management namespace and must carry the plugin prefix.
func TestManagementRegistrationMountRules(t *testing.T) {
	value, errRegister := handleManagementRegister(nil, nil)
	if errRegister != nil {
		t.Fatalf("management.register: %v", errRegister)
	}
	response := value.(pluginapi.ManagementRegistrationResponse)

	for _, route := range response.Routes {
		if route.Menu != "" {
			t.Fatalf("management route %s carries menu %q, want none", route.Path, route.Menu)
		}
		if !strings.HasPrefix(route.Path, "/"+ProviderKey+"/") {
			t.Fatalf("non-resource route %q must be prefixed with the plugin key", route.Path)
		}
	}

	pages := map[string]string{}
	for _, route := range response.Resources {
		if route.Menu != "" {
			t.Fatalf("resource route %q carries a menu and would add a sidebar entry", route.Path)
		}
		pages[route.Path] = route.Description
	}
	for _, required := range []string{"/status", "/login", "/checkin"} {
		if pages[required] == "" {
			t.Fatalf("resource route %q is missing: %v", required, pages)
		}
	}
}

// TestStatusPageWithoutAccounts: the page must render even when nothing is
// configured, and it must offer the login route.
func TestStatusPageWithoutAccounts(t *testing.T) {
	requests := []string{}
	fakeHost(t, func(method string, _ []byte) (any, error) {
		if method == pluginabi.MethodHostAuthList {
			return map[string]any{"files": []any{}}, nil
		}
		return map[string]any{}, nil
	})
	response := renderStatusPage(abiboot.NewHost(nil), pluginapi.ManagementRequest{})
	if response.StatusCode != 200 {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Headers.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q", got)
	}
	body := string(response.Body)
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("body is not a document: %s", body)
	}
	if !strings.Contains(body, "尚未添加账号") {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, `href="login"`) {
		t.Fatalf("the login action link is missing: %s", body)
	}
	if len(requests) != 0 {
		t.Fatalf("no upstream request may be made without an account: %#v", requests)
	}
}

// TestStatusPageWithAccount renders the full page against the fake host: account
// data, catalog counters and credit state, plus a GET action link.
func TestStatusPageWithAccount(t *testing.T) {
	accountHost(t, nil)
	response := renderStatusPage(abiboot.NewHost(nil), pluginapi.ManagementRequest{})
	body := string(response.Body)
	for _, marker := range []string{
		"账号", "通道与模型", "积分与签到",
		"alice", "uid-1", "trae-alice.json",
		"可调用模型数", "支持图片的模型数", "可启用 Max 模式的模型数",
		"剩余积分", "今日已签到",
		// Both callable models in the fixture are listed by solo_agent_remote.
		`solo_agent_remote=2`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("page is missing %q:\n%s", marker, body)
		}
	}
	// The check-in link is a query-carrying GET: the '=' must survive into the
	// attribute, otherwise the host receives one key with no value.
	if !strings.Contains(body, `href="?action=checkin"`) {
		t.Fatalf("check-in action href is malformed:\n%s", body)
	}
	if !strings.Contains(body, `href="login"`) {
		t.Fatalf("re-login action href is malformed:\n%s", body)
	}
	// A credential must not leak its access token into the page.
	if strings.Contains(body, "tok") {
		t.Fatalf("the page must not print the access token:\n%s", body)
	}
}

// TestStatusJSONWithAccount is the machine-readable twin of the same page.
func TestStatusJSONWithAccount(t *testing.T) {
	accountHost(t, nil)
	response := statusJSON(abiboot.NewHost(nil), pluginapi.ManagementRequest{})
	if response.StatusCode != 200 {
		t.Fatalf("status = %d: %s", response.StatusCode, response.Body)
	}
	if got := response.Headers.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded["region"] != RegionCN || decoded["agent_host"] == "" {
		t.Fatalf("decoded = %#v", decoded)
	}
	if decoded["credits"] != float64(150) {
		t.Fatalf("credits = %#v, want 150 (200 limit - 50 used)", decoded["credits"])
	}
	checkin, _ := decoded["daily_checkin"].(map[string]any)
	if checkin["checked_in"] != true || checkin["credits"] != float64(150) {
		t.Fatalf("daily_checkin = %#v", decoded["daily_checkin"])
	}
	accounts, _ := decoded["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v", decoded["accounts"])
	}
	account := accounts[0].(map[string]any)
	if account["refreshable"] != true || account["name"] != "trae-alice.json" {
		t.Fatalf("account = %#v", account)
	}
	if account["models"] == float64(0) {
		t.Fatalf("model count missing: %#v", account)
	}
}

// TestCheckinPagePerformsClaim drives the claim action and renders the outcome.
func TestCheckinPagePerformsClaim(t *testing.T) {
	accountHost(t, nil)
	request := pluginapi.ManagementRequest{Query: map[string][]string{"action": {"claim"}}}
	response := checkinResponse(abiboot.NewHost(nil), request)
	body := string(response.Body)
	// The fake upstream already reports checked_in, so the pre-check short
	// circuits and the outcome is reported as already claimed — that is exactly
	// the idempotency rule: a repeated claim answers success upstream, so only
	// the status may decide.
	if !strings.Contains(body, "今日已签到") || !strings.Contains(body, "150") {
		t.Fatalf("check-in page = %s", body)
	}
	if !strings.Contains(body, `href="checkin"`) {
		t.Fatalf("refresh action href = %s", body)
	}

	// The JSON form reports the claim outcome.
	jsonResponse := checkinResponse(abiboot.NewHost(nil), pluginapi.ManagementRequest{
		Query:   map[string][]string{"action": {"claim"}, "format": {"json"}},
		Headers: http.Header{"Accept": []string{"application/json"}},
	})
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(jsonResponse.Body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	claim, _ := decoded["claim"].(map[string]any)
	if claim["status"] != "already-claimed" || claim["credit"] != float64(150) {
		t.Fatalf("claim = %#v", decoded["claim"])
	}
	if decoded["checked_in"] != true || decoded["credit_remaining"] != float64(150) {
		t.Fatalf("decoded = %#v", decoded)
	}
}

// TestCheckinPageWithoutAccount: the JSON route reports a 400, the HTML route
// renders a readable page.
func TestCheckinPageWithoutAccount(t *testing.T) {
	fakeHost(t, func(method string, _ []byte) (any, error) {
		if method == pluginabi.MethodHostAuthList {
			return map[string]any{"files": []any{}}, nil
		}
		return map[string]any{}, nil
	})
	jsonResponse := checkinResponse(abiboot.NewHost(nil), pluginapi.ManagementRequest{
		Headers: http.Header{"Accept": []string{"application/json"}},
	})
	if jsonResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", jsonResponse.StatusCode)
	}
	htmlResponse := checkinResponse(abiboot.NewHost(nil), pluginapi.ManagementRequest{})
	if htmlResponse.StatusCode != 200 || !strings.Contains(string(htmlResponse.Body), "签到失败") {
		t.Fatalf("html response = %d %s", htmlResponse.StatusCode, htmlResponse.Body)
	}
}

// TestLoginPageRendersStartAction: the page offers a non-blocking start link and
// never waits for the user.
func TestLoginPageRendersStartAction(t *testing.T) {
	response := renderLoginPage(abiboot.NewHost(nil), pluginapi.ManagementRequest{})
	body := string(response.Body)
	if !strings.Contains(body, "开始登录") || !strings.Contains(body, `href="?action=start"`) {
		t.Fatalf("login page = %s", body)
	}
	if !strings.Contains(body, "trae.cn") {
		t.Fatalf("login page must name the portal: %s", body)
	}
}

// TestManagementHandleDispatch covers routing and both representations, using the
// full request path the host passes in.
func TestManagementHandleDispatch(t *testing.T) {
	accountHost(t, nil)
	handler := abiboot.NewHost(nil)

	// The management namespace returns JSON for a script.
	rawJSON, _ := json.Marshal(pluginapi.ManagementRequest{
		Path:    "/v0/management/trae/status",
		Headers: http.Header{"Accept": []string{"application/json"}},
	})
	value, errHandle := handleManagementHandle(handler, rawJSON)
	if errHandle != nil {
		t.Fatalf("management.handle: %v", errHandle)
	}
	if got := value.(pluginapi.ManagementResponse).Headers.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON for a script request", got)
	}

	// The resource namespace returns HTML for a browser.
	rawHTML, _ := json.Marshal(pluginapi.ManagementRequest{
		Path:    "/v0/resource/plugins/trae/status",
		Headers: http.Header{"Accept": []string{"text/html"}},
	})
	value, errHandle = handleManagementHandle(handler, rawHTML)
	if errHandle != nil {
		t.Fatalf("management.handle: %v", errHandle)
	}
	response := value.(pluginapi.ManagementResponse)
	if got := response.Headers.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q, want HTML for a browser request", got)
	}
	if !strings.Contains(string(response.Body), "账号") {
		t.Fatalf("body = %s", response.Body)
	}

	// An unknown route answers 404 rather than panicking.
	rawUnknown, _ := json.Marshal(pluginapi.ManagementRequest{Path: "/v0/management/trae/nope"})
	value, errHandle = handleManagementHandle(handler, rawUnknown)
	if errHandle != nil {
		t.Fatalf("management.handle: %v", errHandle)
	}
	if value.(pluginapi.ManagementResponse).StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", value.(pluginapi.ManagementResponse).StatusCode)
	}
}

// TestPluguiActionQueryContract documents the rendering contract the pages rely
// on: a query-carrying action survives into the href unescaped.
func TestPluguiActionQueryContract(t *testing.T) {
	card := plugui.Card("t", plugui.Fields(plugui.Field{Label: "a", Value: "b"}),
		plugui.Action{Label: "领取今日积分", Query: "action=claim", Kind: "primary"},
		plugui.Action{Label: "去状态页", Path: "status"},
	)
	rendered := string(card)
	if !strings.Contains(rendered, `href="?action=claim"`) {
		t.Fatalf("query action href = %s", rendered)
	}
	if !strings.Contains(rendered, `href="status"`) {
		t.Fatalf("path action href = %s", rendered)
	}
}
