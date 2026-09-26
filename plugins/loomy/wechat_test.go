package main

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The WeChat half is pure protocol: every branch below is one line of
// `loomy-wechat.ts`, and the 404/405 direction is the defect the reference
// documents as a user-visible hang (trap #23).

// wechatAuthPage builds the authorize-page HTML WeChat answers. It embeds the
// uuid twice, exactly like the real page: once in the <img> (main path) and once
// in the devtool poll URL (fallback).
func wechatAuthPage(uuid string) string {
	return `<!DOCTYPE html><html><head><title>微信登录</title></head><body>` +
		`<img class="js_qrcode_img" src="/connect/qrcode/` + uuid + `"/>` +
		`<script>var fordevtool = "https://long.open.weixin.qq.com/connect/l/qrconnect?uuid=` + uuid + `";</script>` +
		`</body></html>`
}

// wechatPollBody builds a long-poll response body in WeChat's own shape.
func wechatPollBody(errcode string, code string) string {
	body := "window.wx_errcode = " + errcode + ";"
	if code != "" {
		body += "window.wx_code = '" + code + "';"
	}
	return body
}

// jpegBytes is a body that passes the image sniffing: the JPEG magic bytes plus
// enough padding to clear the 200-byte floor.
func jpegBytes() []byte {
	raw := make([]byte, 512)
	copy(raw, []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'})
	for index := 10; index < len(raw); index++ {
		raw[index] = byte(index % 251)
	}
	return raw
}

// A credential is not involved in any of these calls, but the transport helper
// takes the settings object.
func wechatTestConfig() Config { return DefaultConfig() }

// The authorize URL must carry the exact whitelisted redirect_uri: WeChat
// validates the domain, and any other value answers an error page instead of the
// page that embeds the uuid (trap #22).
func TestWechatAuthURLKeepsTheOfficialRedirect(t *testing.T) {
	authURL := buildWechatAuthURL("0123456789abcdef0123456789abcdef")
	parsed, errParse := url.Parse(authURL)
	if errParse != nil {
		t.Fatalf("auth url does not parse: %v", errParse)
	}
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != WechatAuthorizeURL {
		t.Fatalf("authorize endpoint = %q, want %q", parsed.Scheme+"://"+parsed.Host+parsed.Path, WechatAuthorizeURL)
	}
	if parsed.Fragment != "wechat_redirect" {
		t.Fatalf("fragment = %q, want wechat_redirect", parsed.Fragment)
	}
	query := parsed.Query()
	if query.Get("appid") != WechatAppID {
		t.Fatalf("appid = %q, want %q", query.Get("appid"), WechatAppID)
	}
	if query.Get("redirect_uri") != WechatRedirectURI {
		t.Fatalf("redirect_uri = %q, want the whitelisted %q", query.Get("redirect_uri"), WechatRedirectURI)
	}
	if query.Get("response_type") != "code" || query.Get("scope") != "snsapi_login" {
		t.Fatalf("query = %v, want response_type=code and scope=snsapi_login", query)
	}
	if query.Get("state") != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("state = %q, want it echoed through", query.Get("state"))
	}
	if !strings.Contains(authURL, "redirect_uri=https%3A%2F%2Floomy.xunfei.cn%2Foauth%2Fwechat%2Fcallback") {
		t.Fatalf("redirect_uri must be percent-encoded: %s", authURL)
	}
}

// The uuid comes from the embedded <img>, and from the devtool poll URL when the
// image is gone — both without running any JavaScript.
func TestExtractWechatUUIDUsesBothEmbeddedPaths(t *testing.T) {
	if got := extractWechatUUID(wechatAuthPage("001ZDsw64Vu7ll2E")); got != "001ZDsw64Vu7ll2E" {
		t.Fatalf("uuid = %q, want the <img> value", got)
	}
	fallback := `<script>var fordevtool = "https://long.open.weixin.qq.com/connect/l/qrconnect?uuid=4Y0N_jyVQg==";</script>`
	if got := extractWechatUUID(fallback); got != "4Y0N_jyVQg==" {
		t.Fatalf("uuid = %q, want the devtool fallback including base64 padding", got)
	}
	// A page whose <img> holds a foreign value still falls back to the poll URL.
	mixed := `<img src="/connect/qrcode/short"/>` + fallback
	if got := extractWechatUUID(mixed); got != "4Y0N_jyVQg==" {
		t.Fatalf("uuid = %q, want the fallback after a rejected candidate", got)
	}
}

// The character/length rule is enforced on the candidate, not on the surrounding
// page: an unusable page yields an empty string.
func TestExtractWechatUUIDRejectsUnusableValues(t *testing.T) {
	cases := map[string]string{
		"empty page":       "",
		"no uuid at all":   `<html><body>redirect_uri 参数错误</body></html>`,
		"too short":        `<img class="js_qrcode_img" src="/connect/qrcode/abc"/>`,
		"illegal chars":    `<img class="js_qrcode_img" src="/connect/qrcode/abc%20def"/>`,
		"html, not a uuid": `<img class="js_qrcode_img" src="/connect/qrcode/&lt;script&gt;"/>`,
	}
	for name, page := range cases {
		if got := extractWechatUUID(page); got != "" {
			t.Errorf("%s: uuid = %q, want empty", name, got)
		}
	}
	// 64 characters is still accepted; 65 is not.
	long := strings.Repeat("a", 64)
	if got := extractWechatUUID(wechatAuthPage(long)); got != long {
		t.Errorf("64-character uuid = %q, want it accepted", got)
	}
	tooLong := strings.Repeat("a", 65)
	if got := extractWechatUUID(wechatAuthPage(tooLong)); got != "" {
		t.Errorf("65-character uuid = %q, want it rejected", got)
	}
}

// The status map, with the 404/405 direction pinned: 405 is CONFIRMED and the
// code is in that frame; 404 only means "scanned, keep waiting".
func TestWechatPollStatusMap(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  string
		code    string
		errcode string
	}{
		{"408 waits", wechatPollBody("408", ""), wechatWaiting, "", "408"},
		{"404 is scanned, not confirmed", wechatPollBody("404", ""), wechatScanned, "", "404"},
		{"405 confirms and carries the code", wechatPollBody("405", "CODE-1"), wechatConfirmed, "CODE-1", "405"},
		{"405 without a code keeps polling", wechatPollBody("405", ""), wechatScanned, "", "405"},
		{"403 cancels", wechatPollBody("403", ""), wechatCancelled, "", "403"},
		{"402 expires", wechatPollBody("402", ""), wechatExpired, "", "402"},
		{"unknown values wait", wechatPollBody("999", "CODE-1"), wechatWaiting, "", "999"},
		{"an unparsable body waits", "<html>error</html>", wechatWaiting, "", ""},
	}
	for _, testCase := range cases {
		result := parseWechatPollBody(testCase.body)
		if result.Status != testCase.status {
			t.Errorf("%s: status = %q, want %q", testCase.name, result.Status, testCase.status)
		}
		if result.Code != testCase.code {
			t.Errorf("%s: code = %q, want %q", testCase.name, result.Code, testCase.code)
		}
		if result.Errcode != testCase.errcode {
			t.Errorf("%s: errcode = %q, want %q", testCase.name, result.Errcode, testCase.errcode)
		}
	}
}

// The long poll sends back the previous errcode as `last` and a cache buster, so
// WeChat can answer a state change immediately instead of holding the request.
func TestWechatPollSendsLastAndCacheBuster(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, wechatPollBody("404", "")), nil
	}
	fake.install(t)

	now := time.UnixMilli(1_760_000_000_000)
	result := pollWechatOnce(testHost(), wechatTestConfig(), "001ZDsw64Vu7ll2E", "408", now)
	if result.Status != wechatScanned {
		t.Fatalf("status = %q, want scanned", result.Status)
	}
	calls := fake.callsFor("l/qrconnect")
	if len(calls) != 1 {
		t.Fatalf("issued %d long polls, want 1", len(calls))
	}
	parsed, errParse := url.Parse(calls[0].URL)
	if errParse != nil {
		t.Fatalf("poll url does not parse: %v", errParse)
	}
	query := parsed.Query()
	if query.Get("uuid") != "001ZDsw64Vu7ll2E" {
		t.Fatalf("uuid = %q, want the QR uuid", query.Get("uuid"))
	}
	if query.Get("last") != "408" {
		t.Fatalf("last = %q, want the previous errcode", query.Get("last"))
	}
	if query.Get("_") != "1760000000000" {
		t.Fatalf("_ = %q, want the cache buster", query.Get("_"))
	}
	// The poll carries the browser headers this endpoint expects.
	if agent := calls[0].Headers.Get("User-Agent"); !strings.Contains(agent, "Chrome/120") {
		t.Fatalf("User-Agent = %q, want the Chrome UA", agent)
	}
	if referer := calls[0].Headers.Get("Referer"); referer != buildWechatAuthURL("") {
		t.Fatalf("Referer = %q, want the authorize URL", referer)
	}
}

// A transport failure is a status, not an exception: one hiccup must not end the
// login (`loomy-wechat.ts:272-280`).
func TestWechatPollTransportFailureIsNotFatal(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	fake.install(t)

	result := pollWechatOnce(testHost(), wechatTestConfig(), "uuid-uuid", "", time.Now())
	if result.Status != wechatError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if result.Detail == "" {
		t.Fatal("an error status must explain itself")
	}
	if result.Code != "" {
		t.Fatalf("code = %q, want empty: a failed poll must never look like a success", result.Code)
	}
}

// The QR image is a JPEG in practice; PNG and GIF are accepted too, and anything
// that is not an image — or is too small to be one — is refused rather than
// rendered as a broken picture (trap #24).
func TestFetchWechatQRImageSniffsMagicBytes(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		want    string
		mime    string
		refused bool
	}{
		{"jpeg", jpegBytes(), "data:image/jpeg;base64,", "image/jpeg", false},
		{"png", append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, 260)...), "data:image/png;base64,", "image/png", false},
		{"gif", append([]byte("GIF89a"), make([]byte, 260)...), "data:image/gif;base64,", "image/gif", false},
		{"html error page", []byte(strings.Repeat("<html>redirect_uri 参数错误</html>", 20)), "", "", true},
		{"too small", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00}, "", "", true},
	}
	for _, testCase := range cases {
		fake := newFakeHost()
		body := testCase.body
		fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return &pluginapi.HTTPResponse{StatusCode: 200, Body: body}, nil
		}
		fake.install(t)

		dataURL, errImage := fetchWechatQRImage(testHost(), wechatTestConfig(), "001ZDsw64Vu7ll2E")
		if testCase.refused {
			if errImage == nil {
				t.Errorf("%s: accepted a body that is not a usable QR image", testCase.name)
			}
			continue
		}
		if errImage != nil {
			t.Errorf("%s: %v", testCase.name, errImage)
			continue
		}
		if !strings.HasPrefix(dataURL, testCase.want) {
			t.Errorf("%s: data url starts with %q, want %q", testCase.name, truncate(dataURL, 40), testCase.want)
		}
	}
}

// A page without a uuid is an error, never an empty uuid to poll forever.
func TestFetchWechatUUIDRequiresAUuid(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `<html><body>redirect_uri 参数错误</body></html>`), nil
	}
	fake.install(t)

	if _, errUUID := fetchWechatUUID(testHost(), wechatTestConfig(), "state"); errUUID == nil {
		t.Fatal("a page without a uuid must fail loudly")
	}
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, wechatAuthPage("001ZDsw64Vu7ll2E")), nil
	}
	uuid, errUUID := fetchWechatUUID(testHost(), wechatTestConfig(), "state")
	if errUUID != nil {
		t.Fatalf("fetchWechatUUID: %v", errUUID)
	}
	if uuid != "001ZDsw64Vu7ll2E" {
		t.Fatalf("uuid = %q, want the embedded value", uuid)
	}
	// Both WeChat calls carry the browser headers the platform expects.
	for _, call := range fake.callsFor("open.weixin.qq.com") {
		if agent := call.Headers.Get("User-Agent"); !strings.Contains(agent, "Chrome/120") {
			t.Fatalf("User-Agent = %q, want the Chrome UA", agent)
		}
		if referer := call.Headers.Get("Referer"); referer == "" {
			t.Fatal("every WeChat call must send a Referer")
		}
	}
}
