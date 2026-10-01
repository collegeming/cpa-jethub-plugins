package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestFetchCaptchaConfigReadsTheServerDocument pins the parse of the document
// that makes a human-assisted claim possible at all.
//
// The parameters are not secret — the server publishes them in the same
// `client/configs` document the official client reads. If this parse drifts, the
// widget silently stops appearing and the channel looks permanently stuck on
// 3007 with no way out.
func TestFetchCaptchaConfigReadsTheServerDocument(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, CaptchaConfigPath) {
			// The `platform=unknown` query is required; the live endpoint 400s
			// with `code 3001` without it.
			if !strings.Contains(request.URL, CaptchaPlatformQuery) {
				return httpResponse(http.StatusBadRequest, `{"code":3001}`), nil
			}
			return httpResponse(http.StatusOK, `{"code":0,"data":{"configs":{"captcha":`+
				`{"enabled":true,"prefix":"no8xfe","region":"cn","sceneId":"11xygtvd","skip_model_request":true}}}}`), nil
		}
		return httpResponse(http.StatusOK, `{"code":0}`), nil
	}

	captcha, errFetch := fetchCaptchaConfig(testHost(), sampleCredential(), settings())
	if errFetch != nil {
		t.Fatalf("fetch: %v", errFetch)
	}
	if captcha.SceneID != "11xygtvd" || captcha.Prefix != "no8xfe" || captcha.Region != "cn" {
		t.Fatalf("parsed config = %+v, want the measured parameters", captcha)
	}
	if !captcha.usable() {
		t.Fatal("the measured configuration must be usable")
	}
	// `enabled:false` means the server wants no token, so no widget.
	if (captchaConfig{SceneID: "x", Prefix: "y", Region: "cn"}).usable() {
		t.Fatal("a disabled captcha config must not be offered as usable")
	}
}

// TestCaptchaTokenFromRequest covers the query-string handover. The resource
// mount is GET-only, so the token has to arrive this way — there is no form to
// post it.
func TestCaptchaTokenFromRequest(t *testing.T) {
	if token := captchaTokenFromRequest(pluginapi.ManagementRequest{}); token != nil {
		t.Fatalf("an unsupplied captcha produced %+v, want nil", token)
	}
	token := captchaTokenFromRequest(pluginapi.ManagementRequest{
		Query: map[string][]string{"captcha": {"solved"}, "captcha_region": {"cn"}},
	})
	if token == nil || token.VerifyParam != "solved" || token.Region != "cn" {
		t.Fatalf("token = %+v", token)
	}
}

// TestCheckinPageOffersTheWidgetOnACaptchaDemand is the user-visible contract:
// a 3007 must come with a way to clear it, not just a sentence.
func TestCheckinPageOffersTheWidgetOnACaptchaDemand(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, ClaimPath):
			return httpResponse(http.StatusBadRequest, `{"code":3007,"msg":"captcha verify failed"}`), nil
		case strings.Contains(request.URL, CaptchaConfigPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"configs":{"captcha":`+
				`{"enabled":true,"prefix":"no8xfe","region":"cn","sceneId":"11xygtvd"}}}}`), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[{"plan_id":"p1","priority":1}]}}`), nil
		default:
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		}
	}
	storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())

	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/checkin", map[string][]string{
			"auth_index": {"auth-1"}, "action": {"claim"}, "format": {"json"},
		}, nil))
	_ = response

	// The HTML variant is the one that carries the widget.
	page := checkinPageForTest(t, "auth-1")
	if !strings.Contains(page, "initAliyunCaptcha") {
		t.Fatalf("the check-in page did not render the captcha widget:\n%s", page)
	}
	if !strings.Contains(page, "11xygtvd") {
		t.Fatal("the widget was rendered without the server's scene id")
	}
	if !strings.Contains(page, "aliyunCaptcha/AliyunCaptcha.js") {
		t.Fatal("the widget did not load Aliyun's SDK")
	}

	// A token already in flight must NOT re-offer the widget: that would loop.
	withToken := checkinPageForTestWithQuery(t, map[string][]string{
		"auth_index": {"auth-1"}, "action": {"claim"}, "captcha": {"already-solved"},
	})
	if strings.Contains(withToken, "initAliyunCaptcha") {
		t.Fatal("the widget was re-offered after a token was already supplied")
	}
}

// checkinPageForTest renders the check-in page as a browser would request it.
func checkinPageForTest(t *testing.T, authIndex string) string {
	t.Helper()
	return checkinPageForTestWithQuery(t, map[string][]string{"auth_index": {authIndex}, "action": {"claim"}})
}

// checkinPageForTestWithQuery renders the HTML variant (no `format=json`).
func checkinPageForTestWithQuery(t *testing.T, query map[string][]string) string {
	t.Helper()
	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/checkin", query, nil))
	return string(response.Body)
}

// TestCaptchaWidgetWarnsWhenNothingRenders guards the honest-failure path.
//
// Measured 2026-10-01: from a panel origin the Aliyun SDK loads and
// `initAliyunCaptcha` runs — instance created, no console error — while the
// element stays EMPTY. A page that only shows "正在加载验证码组件…" would then
// leave the user staring at a spinner and a button that can never work, which is
// worse than the sentence it replaced. So the widget has to give up out loud.
func TestCaptchaWidgetWarnsWhenNothingRenders(t *testing.T) {
	page := string(captchaWidget(captchaConfig{
		Enabled: true, SceneID: "11xygtvd", Prefix: "no8xfe", Region: "cn",
	}))
	if !strings.Contains(page, "未能渲染出验证") {
		t.Fatal("the widget has no give-up message for a challenge that never renders")
	}
	if !strings.Contains(page, "官方客户端") {
		t.Fatal("the give-up message must name the fallback")
	}
	// The scene parameters still come from the server document and are escaped.
	escaped := string(captchaWidget(captchaConfig{
		Enabled: true, SceneID: `"><script>alert(1)</script>`, Prefix: "p", Region: "cn",
	}))
	if strings.Contains(escaped, "<script>alert(1)</script>") {
		t.Fatal("a scene id from the server document was injected unescaped")
	}
}
