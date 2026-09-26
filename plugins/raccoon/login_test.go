package main

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/qr"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The QR login page. What matters here cannot be seen in a unit of logic: the
// page must carry a scannable image, must advance itself WITHOUT a form or a
// script (the resource mount is GET only), and must be honest about the missing
// SMS path.

// loginCodePattern is the code shape the reference asserts: 32 lowercase hex
// characters (`raccoon-oauth.ts:111-118`).
var loginCodePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// renderPage invokes the management dispatcher as a BROWSER would: an HTML
// Accept header, no `format=json`.
func renderPage(t *testing.T, h *abiboot.Host, path string, query map[string][]string) string {
	t.Helper()
	response := callManagement(t, h, managementRequest(http.MethodGet, path, url.Values(query), nil))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned HTTP %d", path, response.StatusCode)
	}
	return string(response.Body)
}

// TestQRContentIsTheExact144ByteLoginURL pins the payload byte-for-byte: 144
// bytes forces version 8 at level M, and a changed URL shape would need a
// different symbol (`raccoon-oauth.ts:127-130`, spec §2.4.2).
func TestQRContentIsTheExact144ByteLoginURL(t *testing.T) {
	code := "0123456789abcdef0123456789abcdef"
	content := qrContentFor(code)
	want := "https://xiaohuanxiong.com/login/mp?code=0123456789abcdef0123456789abcdef&appname=%E5%95%86%E6%B1%A4%E5%B0%8F%E6%B5%A3%E7%86%8A%E5%AE%98%E7%BD%91"
	if content != want {
		t.Fatalf("QR content = %q, want %q", content, want)
	}
	if len(content) != 144 {
		t.Fatalf("QR content is %d bytes, want 144", len(content))
	}
	// The parameter order is `code` then `appname`: url.Values.Encode() would
	// sort them and produce a different string.
	if !strings.HasPrefix(content, QRLoginPageBase+"?code=") {
		t.Errorf("QR content does not start with the code parameter: %q", content)
	}
	symbol, errEncode := qr.Encode(content)
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	if symbol.Version != 8 {
		t.Errorf("QR version = %d, want 8", symbol.Version)
	}
}

// TestGeneratedCodesAre32LowercaseHex pins the code shape.
func TestGeneratedCodesAre32LowercaseHex(t *testing.T) {
	seen := map[string]struct{}{}
	for index := 0; index < 32; index++ {
		code, errCode := generateLoginCode()
		if errCode != nil {
			t.Fatalf("generate: %v", errCode)
		}
		if !loginCodePattern.MatchString(code) {
			t.Fatalf("code = %q, want 32 lowercase hex characters", code)
		}
		if _, duplicate := seen[code]; duplicate {
			t.Fatalf("duplicate code %q", code)
		}
		seen[code] = struct{}{}
	}
	// The login session state has its own shape the host validates.
	state := newLoginState()
	if !loginCodePattern.MatchString(state) {
		t.Fatalf("state = %q, want 32 lowercase hex characters", state)
	}
}

// TestLoginPageRendersAScannableQRWithoutFormsOrScripts is the core page
// contract.
func TestLoginPageRendersAScannableQRWithoutFormsOrScripts(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"code":0,"data":{"status":"pending"}}`), nil
	}
	fake.install(t)
	withSettings(t, DefaultConfig())

	page := renderPage(t, testHost(), "/login", nil)
	if !strings.Contains(page, "data:image/png;base64,") {
		t.Fatalf("the login page carries no base64 PNG QR image:\n%s", truncate(page, 1200))
	}
	if !strings.Contains(page, `<meta http-equiv="refresh"`) {
		t.Error("the page does not refresh itself; the flow would never advance")
	}
	for _, forbidden := range []string{"<form", "<input", "<script", "onclick", "javascript:"} {
		if strings.Contains(strings.ToLower(page), forbidden) {
			t.Errorf("the page contains %q; the resource mount is GET only and must stay form- and script-free", forbidden)
		}
	}
	// The QR payload is the exact login URL, and the code inside it is the
	// session's own code.
	content := qrContentFor(loginCodeFromPage(t, page))
	if !strings.Contains(page, template.HTMLEscapeString(content)) && !strings.Contains(page, content) {
		t.Errorf("the page does not show the QR payload %q", content)
	}
	// Honesty about the missing SMS path, in one line.
	if !strings.Contains(page, "人机验证") || !strings.Contains(page, "微信扫码") {
		t.Errorf("the page does not explain that only WeChat scanning is available:\n%s", truncate(page, 1200))
	}
	if strings.Contains(page, "发送验证码") || strings.Contains(page, "action=sms") {
		t.Error("the page offers an SMS control; that path is deliberately not shipped")
	}
}

// loginCodeFromPage extracts the 32-hex code out of the rendered QR URL.
func loginCodeFromPage(t *testing.T, page string) string {
	t.Helper()
	match := regexp.MustCompile(`code=([0-9a-f]{32})`).FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("no 32-hex login code found in the page:\n%s", truncate(page, 1200))
	}
	if !loginCodePattern.MatchString(match[1]) {
		t.Fatalf("extracted code %q has the wrong shape", match[1])
	}
	return match[1]
}

// TestLoginPageShowsEachVendorStatus pins the page half of the status truth
// table.
func TestLoginPageShowsEachVendorStatus(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantText  string
		wantRotat bool
	}{
		{name: "pending", body: `{"code":0,"data":{"status":"pending"}}`, wantText: "等待微信扫码"},
		{name: "logging", body: `{"code":0,"data":{"status":"logging","expired_at":"2026-09-26 22:00:00"}}`, wantText: "已扫码"},
		{name: "canceled", body: `{"code":0,"data":{"status":"canceled"}}`, wantText: "已自动换了一张新二维码", wantRotat: true},
		{name: "unknown", body: `{"code":0,"data":{"status":"teleporting"}}`, wantText: "等待微信扫码"},
		{name: "network failure", body: "", wantText: "等待微信扫码"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				if testCase.body == "" {
					return nil, errFakeTransport
				}
				return httpResponse(http.StatusOK, testCase.body), nil
			}
			fake.install(t)
			withSettings(t, DefaultConfig())

			// First load mints the code and does NOT poll.
			first := renderPage(t, testHost(), "/login", nil)
			if len(fake.callsFor(QRLoginCodePath)) != 0 {
				t.Fatal("the load that minted the code must not poll")
			}
			firstCode := loginCodeFromPage(t, first)

			// The meta-refresh load polls exactly once.
			second := renderPage(t, testHost(), "/login", map[string][]string{"state": {stateFromPage(t, first)}})
			if got := len(fake.callsFor(QRLoginCodePath)); got != 1 {
				t.Fatalf("the refreshing load made %d polls, want exactly 1", got)
			}
			if !strings.Contains(second, testCase.wantText) {
				t.Errorf("page does not show %q:\n%s", testCase.wantText, truncate(second, 1500))
			}
			secondCode := loginCodeFromPage(t, second)
			if testCase.wantRotat && secondCode == firstCode {
				t.Error("canceled must regenerate the code")
			}
			if !testCase.wantRotat && secondCode != firstCode {
				t.Error("the code must stay stable while the scan is pending")
			}
		})
	}
}

// stateFromPage extracts the 32-hex session state the page links carry.
func stateFromPage(t *testing.T, page string) string {
	t.Helper()
	match := regexp.MustCompile(`state=([0-9a-f]{32})`).FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("no session state found in the page:\n%s", truncate(page, 1200))
	}
	return match[1]
}

// TestSuccessfulScanStoresTheCredentialAndStopsRefreshing covers the whole happy
// path the plugin can reach without a real WeChat account: a scripted `success`
// frame.
func TestSuccessfulScanStoresTheCredentialAndStopsRefreshing(t *testing.T) {
	fake := newFakeHost()
	jwt := fakeJWT(t, time.Now().Add(3*time.Hour))
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, UserInfoPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"id":"user-1","name":"RaccoonAva","phone":"13800138000","office_identity":"personal"}}`), nil
		default:
			return httpResponse(http.StatusOK,
				`{"code":0,"data":{"status":"success","access_token":"`+jwt+`","refresh_token":"refresh-1"}}`), nil
		}
	}
	fake.install(t)
	withSettings(t, DefaultConfig())

	first := renderPage(t, testHost(), "/login", nil)
	state := stateFromPage(t, first)
	second := renderPage(t, testHost(), "/login", map[string][]string{"state": {state}})

	if !strings.Contains(second, "登录成功") {
		t.Fatalf("the success page was not rendered:\n%s", truncate(second, 1500))
	}
	if strings.Contains(second, `<meta http-equiv="refresh"`) {
		t.Error("a finished login must not keep refreshing")
	}
	names := fake.savedNames()
	if len(names) != 1 {
		t.Fatalf("saved %v, want exactly one auth file", names)
	}
	if names[0] != "raccoon-user-1.json" {
		t.Errorf("auth file name = %q, want the user id based name", names[0])
	}
	var stored Credential
	if errUnmarshal := json.Unmarshal(fake.saved[names[0]], &stored); errUnmarshal != nil {
		t.Fatalf("decode stored credential: %v", errUnmarshal)
	}
	if stored.AccessToken != jwt || stored.RefreshToken != "refresh-1" {
		t.Errorf("stored credential = %+v", stored)
	}
	if stored.Type != ProviderKey {
		t.Errorf("stored type = %q, want the provider marker", stored.Type)
	}
	if stored.Nickname != "RaccoonAva" || stored.Phone != "13800138000" || stored.UserID != "user-1" {
		t.Errorf("the profile was not merged into the credential: %+v", stored)
	}
	// The one-off login reward must NOT be claimed automatically.
	if calls := fake.callsFor(LoginPointsGrantPath); len(calls) != 0 {
		t.Errorf("login claimed the one-off reward automatically (%d calls); it must be an explicit action", len(calls))
	}

	// `auth.login.poll` now reports success with the stored credential.
	value, errPoll := handleAuthLoginPoll(testHost(), mustJSON(t, pluginapi.AuthLoginPollRequest{State: state}))
	if errPoll != nil {
		t.Fatalf("poll: %v", errPoll)
	}
	poll := value.(pluginapi.AuthLoginPollResponse)
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll status = %q, want success", poll.Status)
	}
	if poll.Auth.FileName != "raccoon-user-1.json" {
		t.Errorf("poll Auth.FileName = %q", poll.Auth.FileName)
	}
}

// TestAuthLoginStartHandsBackTheQRPage pins the two-step contract.
func TestAuthLoginStartHandsBackTheQRPage(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	withSettings(t, DefaultConfig())

	value, errStart := handleAuthLoginStart(testHost(), mustJSON(t, pluginapi.AuthLoginStartRequest{
		Provider: ProviderKey,
		BaseURL:  "http://127.0.0.1:8317",
	}))
	if errStart != nil {
		t.Fatalf("login.start: %v", errStart)
	}
	start := value.(pluginapi.AuthLoginStartResponse)
	want := "http://127.0.0.1:8317" + loginResourcePath + "?action=qr&state=" + start.State
	if start.URL != want {
		t.Errorf("URL = %q, want %q", start.URL, want)
	}
	if len(fake.requests) != 0 {
		t.Error("login.start must not talk to the vendor: the page does that")
	}
	if !start.ExpiresAt.After(time.Now()) {
		t.Error("login.start must return a future expiry")
	}
	// With no usable base URL the relative resource path is returned.
	value, errStart = handleAuthLoginStart(testHost(), mustJSON(t, pluginapi.AuthLoginStartRequest{}))
	if errStart != nil {
		t.Fatalf("login.start: %v", errStart)
	}
	if relative := value.(pluginapi.AuthLoginStartResponse).URL; !strings.HasPrefix(relative, loginResourcePath+"?") {
		t.Errorf("URL = %q, want the relative resource path", relative)
	}
}

// TestAuthLoginPollReportsPendingBeforeTheCredentialExists pins the poll
// semantics: the host's own sign-in flow must not see success early.
func TestAuthLoginPollReportsPendingBeforeTheCredentialExists(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"code":0,"data":{"status":"pending"}}`), nil
	}
	fake.install(t)
	withSettings(t, DefaultConfig())

	value, errStart := handleAuthLoginStart(testHost(), mustJSON(t, pluginapi.AuthLoginStartRequest{}))
	if errStart != nil {
		t.Fatalf("login.start: %v", errStart)
	}
	state := value.(pluginapi.AuthLoginStartResponse).State

	polled, errPoll := handleAuthLoginPoll(testHost(), mustJSON(t, pluginapi.AuthLoginPollRequest{State: state}))
	if errPoll != nil {
		t.Fatalf("poll: %v", errPoll)
	}
	if polled.(pluginapi.AuthLoginPollResponse).Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending", polled.(pluginapi.AuthLoginPollResponse).Status)
	}
	// An unknown state is an explicit error, not a silent pending.
	polled, errPoll = handleAuthLoginPoll(testHost(), mustJSON(t, pluginapi.AuthLoginPollRequest{State: "deadbeef"}))
	if errPoll != nil {
		t.Fatalf("poll: %v", errPoll)
	}
	if polled.(pluginapi.AuthLoginPollResponse).Status != pluginapi.AuthLoginStatusError {
		t.Errorf("status = %q, want error for an unknown session", polled.(pluginapi.AuthLoginPollResponse).Status)
	}
}

// TestLoginSessionExpiresAfterItsBudget pins the 300 s flow budget: once it is
// gone, the page asks for a restart instead of polling a stale code forever.
func TestLoginSessionExpiresAfterItsBudget(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"code":0,"data":{"status":"pending"}}`), nil
	}
	fake.install(t)
	withSettings(t, DefaultConfig())

	session, errStart := startLoginSession(DefaultConfig())
	if errStart != nil {
		t.Fatalf("start: %v", errStart)
	}
	state := session.stateValue()
	// Wind the session's clock back rather than sleeping.
	session.mu.Lock()
	session.ExpiresAt = time.Now().Add(-time.Second)
	session.mu.Unlock()

	if _, found := lookupLoginSession(state); found {
		t.Fatal("an expired session must not be found")
	}
	page := renderPage(t, testHost(), "/login", map[string][]string{"state": {state}})
	if !strings.Contains(page, "data:image/png;base64,") {
		t.Errorf("an expired state must still start a fresh session:\n%s", truncate(page, 1200))
	}
}

// mustJSON marshals a request payload for a direct handler call.
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	return raw
}

// TestFinishedSessionDoesNotPollAgain pins that reloading the page after a
// successful scan renders the result instead of polling the consumed code.
func TestFinishedSessionDoesNotPollAgain(t *testing.T) {
	fake := newFakeHost()
	jwt := fakeJWT(t, time.Now().Add(3*time.Hour))
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, UserInfoPath) {
			return httpResponse(http.StatusOK, `{"code":0,"data":{"id":"user-1","name":"RaccoonAva","phone":"13800138000"}}`), nil
		}
		return httpResponse(http.StatusOK,
			`{"code":0,"data":{"status":"success","access_token":"`+jwt+`","refresh_token":"refresh-1"}}`), nil
	}
	fake.install(t)
	withSettings(t, DefaultConfig())

	first := renderPage(t, testHost(), "/login", nil)
	state := stateFromPage(t, first)
	renderPage(t, testHost(), "/login", map[string][]string{"state": {state}})
	pollsAfterSuccess := len(fake.callsFor(QRLoginCodePath))

	// A manual reload of the same finished session must not poll the vendor.
	reloaded := renderPage(t, testHost(), "/login", map[string][]string{"state": {state}})
	if got := len(fake.callsFor(QRLoginCodePath)); got != pollsAfterSuccess {
		t.Errorf("reloading a finished session polled %d extra time(s)", got-pollsAfterSuccess)
	}
	if !strings.Contains(reloaded, "登录成功") {
		t.Error("a finished session must render its result")
	}
	if saved := fake.savedNames(); len(saved) != 1 {
		t.Errorf("saved %v, want the credential written exactly once", saved)
	}
}
