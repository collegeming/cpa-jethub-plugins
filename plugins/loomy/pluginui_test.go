package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// renderPage drives one management page request and returns the HTML.
func renderPage(t *testing.T, h *abiboot.Host, path string, query url.Values) string {
	t.Helper()
	response := callManagement(t, h, managementRequest("GET", path, query, nil))
	if response.StatusCode != 200 {
		t.Fatalf("%s status = %d, want 200", path, response.StatusCode)
	}
	body := string(response.Body)
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("%s did not render a document: %s", path, truncate(body, 200))
	}
	return body
}

// The resource mount is GET-only, so no page may contain a form. This is the
// regression guard for the whole form-free design (README "挂载规则").
func TestNoPageContainsAForm(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"balance":15000,"dailyBalance":4992,"availableBalance":19992}}`), nil
	}
	fake.install(t)
	withAccount(t, fake)
	h := testHost()

	pages := map[string]url.Values{
		"/status":     nil,
		"/login":      nil,
		"/checkin":    nil,
		"/onboarding": nil,
	}
	for path, query := range pages {
		body := renderPage(t, h, path, query)
		if strings.Contains(strings.ToLower(body), "<form") {
			t.Errorf("%s must not contain a form: the resource mount drops POSTs", path)
		}
		if strings.Contains(strings.ToLower(body), "<script") {
			t.Errorf("%s must not need JavaScript", path)
		}
	}
}

// The login entry page explains the missing phone instead of offering a field,
// and the configured phone becomes a send link.
func TestLoginPageWithoutAndWithPhone(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	h := testHost()

	without := renderPage(t, h, "/login", nil)
	if !strings.Contains(without, "没有输入框") {
		t.Fatalf("page = %s, want the no-input explanation", truncate(without, 300))
	}
	if strings.Contains(without, "action=send") {
		t.Fatal("without a phone there is nothing to send to")
	}

	withSettings(t, Config{Enabled: true, Phone: "13800138000"})
	configured := renderPage(t, h, "/login", nil)
	if !strings.Contains(configured, "action=send") || !strings.Contains(configured, "phone=13800138000") {
		t.Fatalf("page = %s, want a send link carrying the configured phone", truncate(configured, 400))
	}

	// ?phone= overrides the configured default.
	override := renderPage(t, h, "/login", url.Values{"phone": {"13900139000"}})
	if !strings.Contains(override, "phone=13900139000") {
		t.Fatalf("page = %s, want the override phone in the link", truncate(override, 400))
	}
}

// `?action=start` mints the session, so every link on the page carries the same
// state from the very first step.
func TestLoginStartActionMintsASession(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	body := renderPage(t, testHost(), "/login", url.Values{"action": {"start"}, "phone": {"13800138000"}})
	state := lastLoginState()
	if state == "" {
		t.Fatal("?action=start must create a login session")
	}
	if !strings.Contains(body, "action=send") || !strings.Contains(body, "state="+state) {
		t.Fatalf("page = %s, want a send link carrying state=%s", truncate(body, 400), state)
	}
	if !strings.Contains(body, "phone=13800138000") {
		t.Fatalf("page = %s, want the phone in the link", truncate(body, 400))
	}
}

// After a code has been requested the page offers a keypad of links plus an
// explicit `?action=verify&…&code=…` shape for scripts.
func TestLoginCodePageOffersKeypadAndScriptLink(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"msgid":"M-1"}}`), nil
	}
	fake.install(t)
	h := testHost()
	session := startLoginSession(DefaultConfig(), "13800138000")

	body := renderPage(t, h, "/login", url.Values{
		"action": {"send"}, "state": {session.stateValue()}, "phone": {"13800138000"},
	})
	for _, digit := range []string{"1", "5", "9", "0"} {
		if !strings.Contains(body, "action=code") || !strings.Contains(body, "digit="+digit) {
			t.Fatalf("page = %s, want a keypad link for digit %s", truncate(body, 600), digit)
		}
	}
	if !strings.Contains(body, "action=verify") {
		t.Fatalf("page = %s, want the verify link", truncate(body, 400))
	}
	if !strings.Contains(body, "code=123456") {
		t.Fatalf("page = %s, want the documented script shape", truncate(body, 400))
	}
	if !strings.Contains(body, "13800138000") {
		t.Fatal("the page must show the number the code went to")
	}
	// The msgid reached the session, which is what makes the verify meaningful.
	if !session.hasMsgID() {
		t.Fatal("the send action must record the msgid")
	}
}

// The status page carries the 新建账号 affordance: a SECOND Loomy account must be
// reachable from the page itself instead of only through the manager's OAuth
// page. The link stays a GET navigation into this plugin's own login route —
// 新建账号 adds a link, not a route.
func TestStatusPageOffersAddAccount(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"balance":15000,"dailyBalance":4992,"availableBalance":19992}}`), nil
	}
	fake.install(t)
	withAccount(t, fake)

	body := renderPage(t, testHost(), "/status", nil)
	if !strings.Contains(body, "新建账号") {
		t.Fatalf("the status page offers no way to add a second account:\n%s", truncate(body, 600))
	}
	if !strings.Contains(body, `href="login?add=1"`) {
		t.Fatalf("新建账号 must be a GET link into the login route:\n%s", truncate(body, 600))
	}
}

// The add-account flow says which account it will touch. Loomy identifies an
// account by its phone number, so "another account" means "another number", and
// without that line the flow would re-save the configured account instead.
func TestLoginPageExplainsAddingAnAccount(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	h := testHost()

	plain := renderPage(t, h, "/login", nil)
	if strings.Contains(plain, "新增账号") {
		t.Fatalf("the ordinary login page must not claim to add an account:\n%s", truncate(plain, 400))
	}
	adding := renderPage(t, h, "/login", url.Values{"add": {"1"}})
	if !strings.Contains(adding, "已有账号的凭据不受影响") {
		t.Fatalf("the add-account login page must state that the existing account survives:\n%s", truncate(adding, 600))
	}
	if !strings.Contains(adding, "换一个手机号") {
		t.Fatalf("the add-account login page must say that a second number is required:\n%s", truncate(adding, 600))
	}
}

// The guarantee 新建账号 relies on: the auth file name is derived from the
// account's own phone (or user id when there is no phone), so a second login
// writes a NEW file and leaves the first credential alone. The host saves by
// exactly this name (it feeds AuthData.FileName), and no code path in this plugin
// deletes a credential.
func TestSecondAccountGetsItsOwnCredentialFile(t *testing.T) {
	first := defaultAuthFileName(&Credential{AccessToken: "a", UserID: "100000000000000001", Phone: "13800138000"})
	second := defaultAuthFileName(&Credential{AccessToken: "b", UserID: "100000000000000002", Phone: "13900139000"})
	if first == second {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", first)
	}
	// A WeChat login with no bound phone still gets a name of its own: the
	// fallback is the user id, never a constant.
	wechatA := defaultAuthFileName(&Credential{AccessToken: "c", UserID: "100000000000000003"})
	wechatB := defaultAuthFileName(&Credential{AccessToken: "d", UserID: "100000000000000004"})
	if wechatA == wechatB {
		t.Fatalf("two phone-less accounts share the file name %q", wechatA)
	}
}

// A failed balance read is shown as a failure and never as a zero (trap #11).
func TestStatusPageNeverRendersAFailedBalanceAsZero(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(500, "gateway exploded"), nil
	}
	fake.install(t)
	withAccount(t, fake)
	body := renderPage(t, testHost(), "/status", nil)
	if !strings.Contains(body, "查询失败") {
		t.Fatalf("page = %s, want the failure notice", truncate(body, 500))
	}
	if strings.Contains(body, "永久积分</dt><dd>0") {
		t.Fatal("a failed read must not be rendered as a zero balance")
	}
}

// A response without `balance` says so instead of inventing numbers.
func TestStatusPageReportsMissingBalance(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"dailyBalance":10}}`), nil
	}
	fake.install(t)
	withAccount(t, fake)
	body := renderPage(t, testHost(), "/status", nil)
	if !strings.Contains(body, "无法给出积分数字") {
		t.Fatalf("page = %s, want the unknown-balance wording", truncate(body, 500))
	}
}

// The check-in page only confirms until `action=claim` is followed, and then it
// reports the body-derived status (a repeat answers HTTP 200 as well).
func TestCheckinPageRequiresExplicitClaim(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"alreadyProcessed":true,"permanentBalance":15000,"dailyBalance":5000,"dailyQuota":5000,"dailyConsumed":0}}`), nil
	}
	fake.install(t)
	withAccount(t, fake)
	h := testHost()

	confirm := renderPage(t, h, "/checkin", nil)
	if !strings.Contains(confirm, "立即初始化") {
		t.Fatalf("page = %s, want the confirmation link", truncate(confirm, 400))
	}
	if calls := countPosts(fake, PointsFirstLoginPath); calls != 0 {
		t.Fatalf("the confirm page issued %d writes, want 0", calls)
	}

	claimed := renderPage(t, h, "/checkin", url.Values{"action": {"claim"}})
	if !strings.Contains(claimed, "今日额度已初始化") {
		t.Fatalf("page = %s, want the already-processed wording from the body", truncate(claimed, 400))
	}
	if calls := countPosts(fake, PointsFirstLoginPath); calls != 1 {
		t.Fatalf("issued %d writes, want 1", calls)
	}
}

// The onboarding page lists every task and posts only when asked.
func TestOnboardingPageListsTasks(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.Method == "GET" {
			return httpResponse(200, `{"code":"000000","data":{"tasks":{"first_message":true},"total":10000}}`), nil
		}
		return httpResponse(200, `{"code":"000000","data":{"alreadyCompleted":false,"balance":10000}}`), nil
	}
	fake.install(t)
	withAccount(t, fake)
	h := testHost()

	page := renderPage(t, h, "/onboarding", nil)
	for _, task := range onboardingTasks {
		if !strings.Contains(page, task.Key) || !strings.Contains(page, task.Title) {
			t.Fatalf("page is missing task %s: %s", task.Key, truncate(page, 400))
		}
	}
	if !strings.Contains(page, "已完成") || !strings.Contains(page, "未完成") {
		t.Fatalf("page = %s, want both states rendered", truncate(page, 600))
	}

	claimed := renderPage(t, h, "/onboarding", url.Values{"action": {"claim"}})
	if !strings.Contains(claimed, "积分") {
		t.Fatalf("page = %s, want the claim result", truncate(claimed, 400))
	}
	if posts := countPosts(fake, OnboardingCompletePath); posts != len(onboardingTasks)-1 {
		t.Fatalf("posted %d completions, want %d", posts, len(onboardingTasks)-1)
	}

	single := renderPage(t, h, "/onboarding", url.Values{"action": {"complete"}, "key": {"generate_ppt"}})
	if !strings.Contains(single, "任务已提交") {
		t.Fatalf("page = %s, want the single-task result", truncate(single, 400))
	}
}

// A page with no account tells the user to log in instead of failing.
func TestPagesWithoutAnAccountPointAtLogin(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	h := testHost()
	for _, path := range []string{"/status", "/checkin", "/onboarding"} {
		body := renderPage(t, h, path, nil)
		if !strings.Contains(body, "login") {
			t.Fatalf("%s must link to the login page when there is no account: %s", path, truncate(body, 300))
		}
	}
}

// An unknown auth_index is reported, not silently ignored.
func TestStatusPageRejectsUnknownAuthIndex(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	withAccount(t, fake)
	body := renderPage(t, testHost(), "/status", url.Values{"auth_index": {"missing"}})
	if !strings.Contains(body, "账号不存在") {
		t.Fatalf("page = %s, want the missing-account notice", truncate(body, 400))
	}
}
