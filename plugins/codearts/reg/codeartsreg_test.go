package reg

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeChain scripts the whole upstream chain: SMS pool verbs, the registerui
// registration calls, the thirdLogin ticket redemption, the OAuth2 authorize
// grant and the STS exchange. SMS responses are stage-gated so a message only
// "arrives" after send_verify_code was invoked.
type fakeChain struct {
	mu             sync.Mutex
	calls          []string
	phone          string
	saves          []saveCall
	failsAt        string
	sentVerify     bool
	registerBody   string
	orderBody      string
	activated      bool
	confirmedURL   string
	generateBody   string
	released       []string
	identityUsed   bool
	accountInfoBody string
	quickLoginBody  string
}

type saveCall struct {
	Name string
	JSON map[string]any
}

func (f *fakeChain) install(t *testing.T) {
	t.Helper()
	f.phone = "13900001111"
	previousTransport := transportFor
	transportFor = func(*abiboot.Host) sessionTransport {
		return sessionTransport{do: f.serve, cookie: func(_, _ string) string { return "cftk-test" }}
	}
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return envelopeOK(map[string]any{"files": []any{
				map[string]any{"auth_index": "a1", "name": "codearts-old.json", "type": "codearts"},
			}})
		case pluginabi.MethodHostAuthGet:
			return envelopeOK(pluginapi.HostAuthGetResponse{
				Name: "codearts-old.json",
				JSON: json.RawMessage(`{"type":"codearts","invite_code":"INV-POOL-CODE-0001"}`),
			})
		case pluginabi.MethodHostAuthSave:
			var req pluginapi.HostAuthSaveRequest
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
			var decoded map[string]any
			if err := json.Unmarshal(req.JSON, &decoded); err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.saves = append(f.saves, saveCall{Name: req.Name, JSON: decoded})
			f.mu.Unlock()
			return envelopeOK(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auths/" + req.Name})
		case pluginabi.MethodHostLog:
			return envelopeOK(map[string]any{})
		}
		return nil, fmt.Errorf("unexpected host call: %s", method)
	})
	t.Cleanup(func() {
		transportFor = previousTransport
		abiboot.ClearHostCaller()
	})
}

func respond(status int, headers map[string][]string, body string) (*pluginapi.HTTPResponse, error) {
	return &pluginapi.HTTPResponse{StatusCode: status, Headers: headers, Body: []byte(body)}, nil
}

func jsonBody(document string) (*pluginapi.HTTPResponse, error) {
	return respond(http.StatusOK, map[string][]string{"Content-Type": {"application/json"}}, document)
}

// serve is the scripted transport. The URL alone decides the response, in the
// same order the orchestration issues them.
func (f *fakeChain) serve(_, rawURL string, _ http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, rawURL)
	f.mu.Unlock()
	if f.failsAt != "" && strings.Contains(rawURL, f.failsAt) {
		return jsonBody(`{"error_code":"MOCK.FAIL","error_msg":"scripted failure"}`)
	}
	parsed, _ := url.Parse(rawURL)
	verb := parsed.Query().Get("code")

	switch {
	case strings.Contains(rawURL, "d1jiema") && verb == "leftAmount":
		return respond(http.StatusOK, nil, "余额 42")
	case strings.Contains(rawURL, "d1jiema") && verb == "getPhone":
		return respond(http.StatusOK, nil, "手机号"+f.phone)
	case strings.Contains(rawURL, "d1jiema") && verb == "getMsg":
		// A message exists only after send_verify_code was invoked; both sender
		// keywords then return the registration SMS.
		f.mu.Lock()
		regSent := f.sentVerify
		f.mu.Unlock()
		if !regSent {
			return respond(http.StatusOK, nil, "[尚未收到短信]")
		}
		return respond(http.StatusOK, nil, "【华为云】您的注册验证码为123456，请查收")
	case strings.Contains(rawURL, "d1jiema") && verb == "release":
		f.mu.Lock()
		f.released = append(f.released, parsed.Query().Get("phone"))
		f.mu.Unlock()
		return respond(http.StatusOK, nil, "ok")
	case strings.Contains(rawURL, "d1jiema"):
		return nil, fmt.Errorf("unhandled pool verb: %s", rawURL)
	case strings.Contains(rawURL, "/account/check_identity"):
		if f.identityUsed {
			return jsonBody(`{"loginId":"sess-abc","canUse":"true","identityUsed":"true","hwidFlag":"false"}`)
		}
		return jsonBody(`{"loginId":"sess-abc","canUse":"true","identityUsed":"false"}`)
	case strings.Contains(rawURL, "/account/get-account-info"):
		f.mu.Lock()
		f.accountInfoBody = string(body)
		f.mu.Unlock()
		return jsonBody(`{"error_code":"CBC.0000","account_infos":[{"customer_id":"cid-77","customer_name":"hw-existing-1","last_login_time":1700000000},{"customer_id":"cid-88","customer_name":"hw-older","last_login_time":1600000000}]}`)
	case strings.Contains(rawURL, "/customer/ticket/quick_login"):
		f.mu.Lock()
		f.quickLoginBody = string(body)
		f.mu.Unlock()
		return jsonBody(`{"error_code":"CBC.0000","ticket":"QL-TICKET-1","status":"ok"}`)
	case strings.Contains(rawURL, "auth.huaweicloud.com/authui/casLogin"):
		return respond(http.StatusFound, map[string][]string{
			"Location":   {"https://auth.huaweicloud.com/authui/login?service=https%3A%2F%2Faccount.huaweicloud.com%2Fusercenter%2F"},
			"Set-Cookie": {"SSOTGC=tgc-login; Path=/; Domain=huaweicloud.com"},
		}, "")
	case strings.Contains(rawURL, "/util/geetest/init"):
		return jsonBody(`{"error_code":"CBC.0000","sessionId":"gt-sess"}`)
	case strings.Contains(rawURL, "/util/send_verify_code"):
		f.mu.Lock()
		f.sentVerify = true
		f.mu.Unlock()
		return jsonBody(`{"error_code":"CBC.0000","rec_id":"r-1"}`)
	case strings.Contains(rawURL, "/account/portal/register/quick"):
		f.mu.Lock()
		f.registerBody = string(body)
		f.mu.Unlock()
		return jsonBody(`{"error_code":null,"account_info":{"domain_id":"d-1","domain_name":"hw-mock-1","ticket":"T-1","user_id":"u-1"}}`)
	case strings.Contains(rawURL, "auth.huaweicloud.com/authui/thirdLogin"):
		return respond(http.StatusFound, map[string][]string{
			"Location":   {"https://auth.huaweicloud.com/authui/login?service=https%3A%2F%2Faccount.huaweicloud.com%2Fusercenter%2F"},
			"Set-Cookie": {"SSOTGC=tgc-1; Path=/; Domain=huaweicloud.com"},
		}, "")
	case strings.Contains(rawURL, "auth.huaweicloud.com/authui/login"):
		// Mirror the real CAS bounce: redirect back onto the service URL with
		// an ST ticket attached, planting that domain's session on the way.
		service := parsed.Query().Get("service")
		if service == "" {
			service = "https://account.huaweicloud.com/usercenter/"
		}
		sep := "?"
		if strings.Contains(service, "?") {
			sep = "&"
		}
		return respond(http.StatusFound, map[string][]string{
			"Location": {service + sep + "ticket=ST-1"},
		}, "")
	case strings.Contains(rawURL, "account.huaweicloud.com/usercenter"):
		return respond(http.StatusOK, map[string][]string{"Content-Type": {"text/html"}}, "<html>usercenter</html>")
	case strings.Contains(rawURL, "auth.huaweicloud.com/authui/v1/oauth2/authorize"):
		// Echo the request's state so the plugin's state check exercises the
		// real comparison, and hand the code back on the loopback callback.
		return respond(http.StatusFound, map[string][]string{
			"Location": {"http://127.0.0.1:18080/oauth/callback?code=AUTHCODE9&state=" + parsed.Query().Get("state")},
		}, "")
	case strings.Contains(rawURL, "/portal/snap-manager/v1/package/info"):
		f.mu.Lock()
		activated := f.activated
		f.mu.Unlock()
		if activated {
			return jsonBody(`{"enable":true,"spec_code":"codearts.agent.individual.trial","status":"normal"}`)
		}
		return jsonBody(`{"enable":false,"resource_id":null}`)
	case strings.Contains(rawURL, "/portal/DevCloudConsole/v5/orders/subscribe"):
		f.mu.Lock()
		f.activated = true
		f.orderBody = string(body)
		f.mu.Unlock()
		return jsonBody(`{"status":"success","result":[{"order_id":"CS1"}]}`)
	case strings.Contains(rawURL, "devcloud.cn-north-4.huaweicloud.com/chat/home") ||
		strings.Contains(rawURL, "devcloud.cn-north-4.huaweicloud.com/chat?"):
		// The workbench landing page: a static SPA shell that 200s for anyone.
		return respond(http.StatusOK, nil, "<html>chat workbench</html>")
	case strings.Contains(rawURL, "/chat/PromptCenterService/v1/ops/delivery"):
		// The welfare panel's campaign list: the invite campaign is present
		// but carries no minted code, so the job must call invite/generate.
		return jsonBody(`{"code":0,"data":{"campaigns":[{"campaignId":"INVITE-CAMP-7","name":"invite user reward"},{"campaignId":"34","name":"daily sign-in"}]}}`)
	case strings.Contains(rawURL, "/chat/PromptCenterService/v1/ops/confirm"):
		return jsonBody(`{"code":0,"message":"enrolled"}`)
	case strings.Contains(rawURL, "/chat/PromptCenterService/v1/ops/invite/confirm-by-code"):
		f.mu.Lock()
		f.confirmedURL = rawURL
		f.mu.Unlock()
		return jsonBody(`{"code":0,"message":"ok"}`)
	case strings.Contains(rawURL, "/chat/PromptCenterService/v1/ops/invite/generate"):
		f.mu.Lock()
		f.generateBody = string(body)
		f.mu.Unlock()
		return jsonBody(`{"code":0,"data":{"inviteCode":"INV-NEW-MINTED-9999"}}`)
	case strings.Contains(rawURL, "sts.cn-north-4"):
		return jsonBody(`{"credentials":{"access_key_id":"HSTANEWKEY000000000","secret_access_key":"SK","security_token":"ST","expiration":"2026-11-01T00:00:00Z"},"refresh_token":"rt-jwt"}`)
	}
	return nil, fmt.Errorf("unexpected URL: %s", rawURL)
}

// envelopeOK wraps a host result in the ABI envelope.
func envelopeOK(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(abiboot.Envelope{OK: true, Result: raw})
}

// awaitJob polls the job snapshot until the run reports done.
func awaitJob(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap := job.snapshot()
		if snap["done"] == true {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestJobRunsFullChain drives one registration end to end: the saved auth
// file must carry the OAuth credential shape the codearts provider parses —
// STS triple, refresh token, code verifier and the private DPoP JWK.
func TestJobRunsFullChain(t *testing.T) {
	fake := &fakeChain{}
	fake.install(t)

	cfg := DefaultConfig()
	cfg.SMSToken = "tok"
	setSettings(cfg)
	t.Cleanup(func() { setSettings(DefaultConfig()) })

	if err := job.start(); err != nil {
		t.Fatalf("job.start: %v", err)
	}
	snap := awaitJob(t)
	if snap["succeeded"] != true {
		t.Fatalf("job failed: %v", snap["error"])
	}

	if len(fake.saves) != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1", len(fake.saves))
	}
	saved := fake.saves[0]
	if saved.Name != "codearts-"+fake.phone+".json" {
		t.Fatalf("saved file = %q", saved.Name)
	}
	cred := saved.JSON
	if cred["type"] != "codearts" || cred["access_key_id"] != "HSTANEWKEY000000000" {
		t.Fatalf("credential shape wrong: %v", cred)
	}
	if cred["refresh_token"] != "rt-jwt" || cred["code_verifier"] == "" {
		t.Fatalf("refresh material missing: %v", cred)
	}
	if cred["domain_id"] != "d-1" || cred["user_name"] != "hw-mock-1" {
		t.Fatalf("account identity fields missing: %v", cred)
	}
	jwk, ok := cred["dpop_private_key_jwk"].(map[string]any)
	if !ok || jwk["d"] == nil || jwk["crv"] != "P-256" {
		t.Fatalf("dpop JWK missing or malformed: %v", cred["dpop_private_key_jwk"])
	}

	// The SMS code must land in the snake_case register body as
	// verification_code, and the ride must have consumed the register ticket.
	if !strings.Contains(fake.registerBody, `"verification_code":"123456"`) {
		t.Fatalf("register body missing reg code: %s", fake.registerBody)
	}
	if !strings.Contains(fake.registerBody, `"country_code":"CN"`) {
		t.Fatalf("register body missing region code: %s", fake.registerBody)
	}
	joined := strings.Join(fake.calls, "\n")
	if !strings.Contains(joined, "authui/thirdLogin") || !strings.Contains(joined, "ticket=T-1") {
		t.Fatalf("register ticket was not redeemed:\n%s", joined)
	}
	if !strings.Contains(fake.orderBody, portalProductID) || !strings.Contains(fake.orderBody, portalSpecCode) {
		t.Fatalf("free-tier order missing or malformed: %s", fake.orderBody)
	}

	// Referral chain: the pool member's code must be bound, and this
	// account's freshly minted code must land in the credential file.
	if !strings.Contains(fake.confirmedURL, "inviteCode=INV-POOL-CODE-0001") {
		t.Fatalf("pool invite code was not confirmed: %s", fake.confirmedURL)
	}
	if !strings.Contains(fake.generateBody, `"campaignId":"INVITE-CAMP-7"`) {
		t.Fatalf("invite/generate used wrong campaign: %s", fake.generateBody)
	}
	if cred["invite_code"] != "INV-NEW-MINTED-9999" {
		t.Fatalf("minted invite code missing from credential: %v", cred["invite_code"])
	}
}

// TestJobGuardRefusesWhenDisabled pins the write switch: enabled=false must
// never start a registration.
func TestJobGuardRefusesWhenDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = false
	cfg.SMSToken = "tok"
	setSettings(cfg)
	t.Cleanup(func() { setSettings(DefaultConfig()) })
	if err := job.start(); err == nil {
		t.Fatal("job.start should refuse when enabled=false")
	}
}

// TestJobReleasesNumberOnFailure pins pool hygiene: a number whose
// registration aborts is released back to the SMS pool.
func TestJobReleasesNumberOnFailure(t *testing.T) {
	fake := &fakeChain{failsAt: "check_identity"}
	fake.install(t)
	cfg := DefaultConfig()
	cfg.SMSToken = "tok"
	setSettings(cfg)
	t.Cleanup(func() { setSettings(DefaultConfig()) })

	if err := job.start(); err != nil {
		t.Fatalf("job.start: %v", err)
	}
	awaitJob(t)
	if len(fake.released) != maxPhoneDraws {
		t.Fatalf("released = %v, want %d entries", fake.released, maxPhoneDraws)
	}
	for _, phone := range fake.released {
		if phone != fake.phone {
			t.Fatalf("released phone %q != drawn %q", phone, fake.phone)
		}
	}
}

func TestExtractSMSCode(t *testing.T) {
	for input, want := range map[string]string{
		"【华为云】您的验证码为123456，请及时输入": "123456",
		"[华为] code: 998877":         "998877",
		"random text 4321 trailing": "4321",
		"[尚未收到短信]":                  "",
	} {
		if got := extractSMSCode(input); got != want {
			t.Fatalf("extractSMSCode(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestDirectTransportKeepsRedirectsAndCookies pins the two transport
// properties the CAS chain depends on: redirects are surfaced (not followed
// internally) and Set-Cookie values persist across calls through the standard
// jar.
func TestDirectTransportKeepsRedirectsAndCookies(t *testing.T) {
	var sawCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hop":
			http.SetCookie(w, &http.Cookie{Name: "SSOTGC", Value: "tgc-1"})
			w.Header().Set("Location", "/next")
			w.WriteHeader(http.StatusFound)
		case "/next":
			sawCookie = r.Header.Get("Cookie")
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)

	d := directTransport().do
	response, errDo := d(http.MethodGet, server.URL+"/hop", nil, nil)
	if errDo != nil {
		t.Fatalf("hop: %v", errDo)
	}
	if response.StatusCode != http.StatusFound {
		t.Fatalf("redirect was followed internally: HTTP %d", response.StatusCode)
	}
	if response.Headers.Get("Location") != "/next" {
		t.Fatalf("Location not surfaced: %v", response.Headers)
	}
	if _, errDo := d(http.MethodGet, server.URL+"/next", nil, nil); errDo != nil {
		t.Fatalf("next: %v", errDo)
	}
	if !strings.Contains(sawCookie, "SSOTGC=tgc-1") {
		t.Fatalf("cookie was not replayed by the jar: %q", sawCookie)
	}
}

// TestJobClaimsAlreadyRegisteredPhone drives the shared registerui login path
// used when check_identity reports identityUsed: SMS → get-account-info →
// customer/ticket/quick_login → casLogin → same OAuth/STS half as register.
func TestJobClaimsAlreadyRegisteredPhone(t *testing.T) {
	fake := &fakeChain{identityUsed: true}
	fake.install(t)

	cfg := DefaultConfig()
	cfg.SMSToken = "tok"
	setSettings(cfg)
	t.Cleanup(func() { setSettings(DefaultConfig()) })

	if err := job.start(); err != nil {
		t.Fatalf("job.start: %v", err)
	}
	snap := awaitJob(t)
	if snap["succeeded"] != true {
		t.Fatalf("job failed: %v lines=%v", snap["error"], snap["lines"])
	}

	if len(fake.saves) != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1", len(fake.saves))
	}
	cred := fake.saves[0].JSON
	if cred["type"] != "codearts" || cred["access_key_id"] != "HSTANEWKEY000000000" {
		t.Fatalf("credential shape wrong: %v", cred)
	}
	if cred["user_name"] != "hw-existing-1" {
		t.Fatalf("expected picked account name, got %v", cred["user_name"])
	}
	if !strings.Contains(fake.accountInfoBody, `"verification_code":"123456"`) {
		t.Fatalf("get-account-info missing SMS code: %s", fake.accountInfoBody)
	}
	if !strings.Contains(fake.quickLoginBody, `"customer_id":"cid-77"`) {
		t.Fatalf("quick_login did not pick latest account: %s", fake.quickLoginBody)
	}
	joined := strings.Join(fake.calls, "\n")
	if strings.Contains(joined, "/account/portal/register/quick") {
		t.Fatalf("used phone should not hit register/quick:\n%s", joined)
	}
	if !strings.Contains(joined, "authui/casLogin") || !strings.Contains(joined, "ticket=QL-TICKET-1") {
		t.Fatalf("quick_login ticket was not redeemed via casLogin:\n%s", joined)
	}
	if strings.Contains(joined, "authui/thirdLogin") {
		t.Fatalf("login ticket must not use thirdLogin:\n%s", joined)
	}
}

// TestPickAccountPrefersLatestLogin covers the SelectAccount defaulting rule.
func TestPickAccountPrefersLatestLogin(t *testing.T) {
	picked, ok := pickAccount([]existingAccount{
		{CustomerID: "a", LastLogin: 1},
		{CustomerID: "b", LastLogin: 9},
		{CustomerID: "c", LastLogin: 3},
	})
	if !ok || picked.CustomerID != "b" {
		t.Fatalf("picked %#v ok=%v", picked, ok)
	}
	if _, ok := pickAccount(nil); ok {
		t.Fatal("empty list should not pick")
	}
}
