package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pageRequest builds a browser-shaped management request.
func pageRequest(path string, query url.Values) pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    path,
		Headers: http.Header{"Accept": []string{"text/html,application/xhtml+xml"}},
		Query:   query,
	}
}

// pageOf extracts the HTML body of a management response.
func pageOf(t *testing.T, value any, errHandler error) string {
	t.Helper()
	if errHandler != nil {
		t.Fatalf("management handler: %v", errHandler)
	}
	response, okResponse := value.(pluginapi.ManagementResponse)
	if !okResponse {
		t.Fatalf("unexpected reply %T", value)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Headers.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q", got)
	}
	return string(response.Body)
}

// TestRenderStatusPageWithoutAccounts pins the empty state, which is what a
// fresh install shows.
func TestRenderStatusPageWithoutAccounts(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	page := pageOf(t, renderStatusPage(nil, pageRequest("/status", nil)), nil)
	for _, fragment := range []string{"尚未添加账号", "WorkOS 设备码", APIBase, WorkOSBase, "模型目录", "思考级别", reasoningLevels[4]} {
		if !strings.Contains(page, fragment) {
			t.Errorf("page is missing %q", fragment)
		}
	}
	// The login action must be a query-string link, never a form: the resource
	// mount is dispatched as GET only.
	if !strings.Contains(page, `href="login"`) {
		t.Errorf("the login action must be a relative link:\n%s", page)
	}
	if strings.Contains(page, "<form") {
		t.Errorf("resource pages must not use forms:\n%s", page)
	}
}

// TestRenderLoginPageDefault pins the first screen of the device flow.
func TestRenderLoginPageDefault(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	page := pageOf(t, renderLoginPage(nil, pageRequest("/login", nil)), nil)
	for _, fragment := range []string{"WorkOS 设备码登录", "开始登录", "action=start", "不需要本地回调端口", DeviceAuthorizationPath} {
		if !strings.Contains(page, fragment) {
			t.Errorf("page is missing %q", fragment)
		}
	}
}

// TestRenderLoginPageStart pins the authorization screen: the URL and the user
// code both have to be visible, because the bare verification URI does not carry
// the code.
func TestRenderLoginPageStart(t *testing.T) {
	resetLoginState(t)
	withTestSettings(t, DefaultConfig())
	fake := &fakeTransport{steps: []fakeStep{{response: jsonResponse(200,
		`{"device_code":"dev-1","user_code":"ABCD-1234","verification_uri":"https://workos.test/device","verification_uri_complete":"https://workos.test/device?user_code=ABCD-1234","expires_in":300,"interval":5}`)}}}
	installFakeTransport(t, fake)

	page := pageOf(t, renderLoginPage(nil, pageRequest("/login", url.Values{"action": []string{"start"}})), nil)
	for _, fragment := range []string{
		"https://workos.test/device?user_code=ABCD-1234",
		"ABCD-1234",
		"检查授权结果",
		"5 秒",
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("page is missing %q:\n%s", fragment, page)
		}
	}
	// The poll link has to carry the state the plugin will look the session up by.
	if !strings.Contains(page, "action=poll&amp;state=") {
		t.Errorf("the poll link must carry the state:\n%s", page)
	}
}

// TestRenderLoginPageStartWithoutTransport pins the failure page, which is the
// path a user hits when the host transport is unavailable.
func TestRenderLoginPageStartWithoutTransport(t *testing.T) {
	resetLoginState(t)
	withTestSettings(t, DefaultConfig())
	fake := &fakeTransport{steps: []fakeStep{{err: errTestTransport}}}
	installFakeTransport(t, fake)
	page := pageOf(t, renderLoginPage(nil, pageRequest("/login", url.Values{"action": []string{"start"}})), nil)
	if !strings.Contains(page, "无法发起登录") {
		t.Errorf("page = %s", page)
	}
}

// TestRenderLoginPagePollUnknownState pins the "restart the flow" page.
func TestRenderLoginPagePollUnknownState(t *testing.T) {
	resetLoginState(t)
	withTestSettings(t, DefaultConfig())
	page := pageOf(t, renderLoginPage(nil, pageRequest("/login", url.Values{
		"action": []string{"poll"},
		"state":  []string{"gone"},
	})), nil)
	if !strings.Contains(page, "登录会话不存在或已超时") {
		t.Errorf("page = %s", page)
	}
}

// TestRenderLoginPagePollPending pins the waiting page and its retry link.
func TestRenderLoginPagePollPending(t *testing.T) {
	resetLoginState(t)
	withTestSettings(t, DefaultConfig())
	fake := &fakeTransport{steps: []fakeStep{
		{response: jsonResponse(200, `{"device_code":"d","user_code":"u","verification_uri":"https://w/d"}`)},
		{response: jsonResponse(400, `{"error":"authorization_pending"}`)},
	}}
	installFakeTransport(t, fake)
	session, errStart := startLoginSession(fake.do, settings())
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}
	page := pageOf(t, renderLoginPage(nil, pageRequest("/login", url.Values{
		"action": []string{"poll"},
		"state":  []string{session.State},
	})), nil)
	for _, fragment := range []string{"等待授权", "再次检查"} {
		if !strings.Contains(page, fragment) {
			t.Errorf("page is missing %q:\n%s", fragment, page)
		}
	}
}

// TestRenderLoginPageEscapesUpstreamText pins that untrusted upstream text is
// HTML-escaped rather than injected into the page.
func TestRenderLoginPageEscapesUpstreamText(t *testing.T) {
	resetLoginState(t)
	withTestSettings(t, DefaultConfig())
	fake := &fakeTransport{steps: []fakeStep{
		{response: jsonResponse(200, `{"device_code":"d","user_code":"u","verification_uri":"https://w/d"}`)},
		{response: jsonResponse(400, `{"error":"access_denied","error_description":"<script>alert(1)</script>"}`)},
	}}
	installFakeTransport(t, fake)
	session, errStart := startLoginSession(fake.do, settings())
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}
	page := pageOf(t, renderLoginPage(nil, pageRequest("/login", url.Values{
		"action": []string{"poll"},
		"state":  []string{session.State},
	})), nil)
	if strings.Contains(page, "<script>") {
		t.Fatalf("the upstream message was injected verbatim:\n%s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("the upstream message must be escaped and shown:\n%s", page)
	}
}

// TestRefreshPageWithoutAccounts pins the page-driven refresh failure path: it
// must not touch the network when the account cannot be resolved.
func TestRefreshPageWithoutAccounts(t *testing.T) {
	withTestSettings(t, DefaultConfig())
	fake := &fakeTransport{}
	installFakeTransport(t, fake)
	page := pageOf(t, refreshPage(nil, pageRequest("/status", url.Values{"action": []string{"refresh"}})), nil)
	if !strings.Contains(page, "指定的账号不存在") {
		t.Errorf("page = %s", page)
	}
	if fake.callCount() != 0 {
		t.Errorf("no request may be issued: %d", fake.callCount())
	}
}

// TestDisplayHelpers pins the small renderers.
func TestDisplayHelpers(t *testing.T) {
	if got := trimNumber(5); got != "5" {
		t.Errorf("trimNumber(5) = %q", got)
	}
	if got := trimNumber(0.25); got != "0.25" {
		t.Errorf("trimNumber(0.25) = %q", got)
	}
	if got := trimNumber(0); got != "0" {
		t.Errorf("trimNumber(0) = %q", got)
	}
	if got := yesNo(true); got != "是" {
		t.Errorf("yesNo(true) = %q", got)
	}
	if got := yesNo(false); got != "否" {
		t.Errorf("yesNo(false) = %q", got)
	}
	if got := valueOr("", "—"); got != "—" {
		t.Errorf("valueOr = %q", got)
	}
	if got := tokenPrefixText(&Credential{AccessToken: "workos:x"}); !strings.Contains(got, "正常") {
		t.Errorf("tokenPrefixText = %q", got)
	}
	if got := tokenPrefixText(&Credential{AccessToken: "x"}); !strings.Contains(got, "缺失") {
		t.Errorf("tokenPrefixText = %q", got)
	}
	if got := formatExpiry(time.Time{}); got != "未知（以服务端 401 为准）" {
		t.Errorf("formatExpiry(zero) = %q", got)
	}
	if got := formatExpiry(time.Now().Add(-time.Hour)); !strings.Contains(got, "已过期") {
		t.Errorf("formatExpiry(past) = %q", got)
	}
	if got := catalogueText(DefaultConfig()); !strings.Contains(got, "缓存") {
		t.Errorf("catalogueText = %q", got)
	}
	if got := catalogueText(Config{ModelDiscovery: false}); !strings.Contains(got, "仅静态表") {
		t.Errorf("catalogueText = %q", got)
	}
	if got := maxOutputTokens(Config{}); got != MaxOutputTokensCeiling {
		t.Errorf("maxOutputTokens = %d", got)
	}
	if got := catalogueCacheText(); !strings.Contains(got, "尚未拉取") {
		t.Errorf("catalogueCacheText = %q", got)
	}
	discoveredModels.put([]pluginapi.ModelInfo{{ID: "cline-free/x"}})
	defer discoveredModels.reset()
	if got := catalogueCacheText(); !strings.Contains(got, "1 条") {
		t.Errorf("catalogueCacheText = %q", got)
	}
}
