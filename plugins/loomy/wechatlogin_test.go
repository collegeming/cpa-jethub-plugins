package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The QR login is driven from the plugin's own page, one long poll per load, so
// these tests walk the page exactly like a browser (meta refresh included) and
// count what each load sent upstream. Nothing here touches the network.

// wechatTransport scripts every endpoint the QR flow can reach.
type wechatTransport struct {
	uuid string
	// polls are the long-poll bodies, consumed in order; the last one repeats.
	polls []string
	// pollCount is how many long polls have been served.
	pollCount int
	// qrFetches counts authorize-page downloads (one per QR).
	qrFetches int
	// accounts answers the account host; nil means "any 000000 empty data".
	accounts func(path string, request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error)
}

func (w *wechatTransport) do(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
	switch {
	case strings.Contains(request.URL, "open.weixin.qq.com/connect/qrconnect"):
		w.qrFetches++
		return httpResponse(200, wechatAuthPage(w.uuid)), nil
	case strings.Contains(request.URL, "open.weixin.qq.com/connect/qrcode/"):
		return &pluginapi.HTTPResponse{StatusCode: 200, Body: jpegBytes()}, nil
	case strings.Contains(request.URL, "long.open.weixin.qq.com/connect/l/qrconnect"):
		body := ""
		if len(w.polls) > 0 {
			index := w.pollCount
			if index >= len(w.polls) {
				index = len(w.polls) - 1
			}
			body = w.polls[index]
		}
		w.pollCount++
		return httpResponse(200, body), nil
	case strings.Contains(request.URL, AccountBase):
		if w.accounts == nil {
			return httpResponse(200, `{"code":"000000","data":{}}`), nil
		}
		return w.accounts(accountPathOf(request.URL), request)
	}
	return nil, errFakeHostRoute
}

// accountPathOf strips the account host off a URL.
func accountPathOf(rawURL string) string {
	return strings.TrimPrefix(rawURL, AccountBase)
}

// accountCallParam decodes the `param` object of a recorded account request.
func accountCallParam[T any](t *testing.T, request abiboot.HTTPDoRequest) T {
	t.Helper()
	var envelope struct {
		Param json.RawMessage `json:"param"`
	}
	if errUnmarshal := json.Unmarshal(request.Body, &envelope); errUnmarshal != nil {
		t.Fatalf("account request is not the standard envelope: %v", errUnmarshal)
	}
	var out T
	if errUnmarshal := json.Unmarshal(envelope.Param, &out); errUnmarshal != nil {
		t.Fatalf("decode account param: %v", errUnmarshal)
	}
	return out
}

// onlyAccountCall returns the single recorded call for one account path.
func onlyAccountCall(t *testing.T, fake *fakeHost, path string) abiboot.HTTPDoRequest {
	t.Helper()
	calls := fake.callsFor(AccountBase + path)
	if len(calls) != 1 {
		t.Fatalf("issued %d calls to %s, want 1", len(calls), path)
	}
	return calls[0]
}

// qrPage drives one page load of the QR page.
func qrPage(t *testing.T, h *abiboot.Host, state string, extra url.Values) string {
	t.Helper()
	query := url.Values{"action": {"qr"}}
	if state != "" {
		query.Set("state", state)
	}
	for key, values := range extra {
		query[key] = values
	}
	return renderPage(t, h, resourceLoginPath, query)
}

// TestWechatQRLoginFlowThroughManagementPage walks the whole bind=1 path: the
// entry point returned by auth.login.start renders a real QR, three page loads
// perform three long polls, and the 405 frame turns into a stored credential.
func TestWechatQRLoginFlowThroughManagementPage(t *testing.T) {
	transport := &wechatTransport{
		uuid: "001ZDsw64Vu7ll2E",
		polls: []string{
			wechatPollBody("408", ""),
			wechatPollBody("404", ""),
			wechatPollBody("405", "WX-CODE-1"),
		},
		accounts: func(path string, request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			switch path {
			case BindAuthPath:
				return httpResponse(200, `{"code":"000000","data":{"bind":1,"rcode":"R-1","nickname":"微信昵称"}}`), nil
			case BindSkipPath:
				return httpResponse(200, `{"code":"000000","data":{"session":"0123456789abcdef0123456789abcdef","userid":"123456789012345678"}}`), nil
			case PointsFirstLoginPath:
				return httpResponse(200, `{"code":"000000","data":{"alreadyProcessed":false,"dailyQuota":5000,"dailyBalance":5000}}`), nil
			}
			return nil, errFakeHostRoute
		},
	}
	fake := newFakeHost()
	fake.do = transport.do
	fake.install(t)
	h := testHost()

	// Part 3: the URL auth.login.start hands back lands on the QR page itself.
	started := startLogin(t, h, "http://127.0.0.1:8317/v0/management/oauth-callback", nil)
	parsed, errParse := url.Parse(started.URL)
	if errParse != nil {
		t.Fatalf("start url does not parse: %v", errParse)
	}
	if parsed.Query().Get("action") != loginQRAction {
		t.Fatalf("start url = %q, want it to open the QR page directly", started.URL)
	}
	state := started.State

	// Load 1: the QR is fetched and rendered, and NOT polled yet — polling here
	// would delay the visible code by a whole long-poll window.
	first := qrPage(t, h, state, nil)
	if !strings.Contains(first, "data:image/jpeg;base64,") {
		t.Fatalf("page has no QR image: %s", truncate(first, 400))
	}
	if !strings.Contains(first, `<meta http-equiv="refresh" content="1">`) {
		t.Fatalf("page must reload itself for the first poll: %s", truncate(first, 600))
	}
	if strings.Contains(strings.ToLower(first), "<form") || strings.Contains(strings.ToLower(first), "<script") {
		t.Fatalf("the QR page must have neither a form nor a script: %s", truncate(first, 600))
	}
	if transport.pollCount != 0 {
		t.Fatalf("the QR-fetching load issued %d long polls, want 0", transport.pollCount)
	}
	// The image is cached as a data: URL: re-rendering it costs no WeChat call.
	if transport.qrFetches != 1 {
		t.Fatalf("authorize-page fetches = %d, want 1", transport.qrFetches)
	}
	// The QR uuid itself must never appear in the page as a link (the page needs
	// no external fetch at all).
	if strings.Contains(first, "open.weixin.qq.com/connect/qrcode/") {
		t.Fatal("the page must embed the QR, not link to WeChat")
	}

	// Loads 2 and 3: one long poll each — 408 waits, 404 is scanned.
	second := qrPage(t, h, state, nil)
	if transport.pollCount != 1 {
		t.Fatalf("long polls after two loads = %d, want 1", transport.pollCount)
	}
	if !strings.Contains(second, "等待微信扫码") || !strings.Contains(second, "data:image/jpeg;base64,") {
		t.Fatalf("waiting page = %s", truncate(second, 400))
	}
	third := qrPage(t, h, state, nil)
	if transport.pollCount != 2 {
		t.Fatalf("long polls after three loads = %d, want 2", transport.pollCount)
	}
	if !strings.Contains(third, "已扫码") {
		t.Fatalf("404 must render as scanned: %s", truncate(third, 400))
	}
	if transport.qrFetches != 1 {
		t.Fatalf("a re-render re-fetched the QR (%d authorize calls): the data URL must be cached", transport.qrFetches)
	}

	// Load 4: the 405 frame carries the code, so the exchange runs.
	fourth := qrPage(t, h, state, nil)
	if transport.pollCount != 3 {
		t.Fatalf("long polls after four loads = %d, want 3", transport.pollCount)
	}
	if !strings.Contains(fourth, "登录成功") {
		t.Fatalf("confirmed page = %s, want a success page", truncate(fourth, 600))
	}
	if strings.Contains(fourth, "http-equiv=\"refresh\"") {
		t.Fatal("the success page must not reload itself: the flow is over")
	}

	// The WeChat code is single-use and goes to bind/auth exactly once, in the
	// documented parameter shape; everything after it uses the rcode.
	authParam := accountCallParam[bindAuthParam](t, onlyAccountCall(t, fake, BindAuthPath))
	if authParam.Type != "wx" || authParam.TCode.Code != "WX-CODE-1" {
		t.Fatalf("bind/auth param = %#v, want {tcode:{code:WX-CODE-1},type:wx}", authParam)
	}
	skipParam := accountCallParam[bindSkipParam](t, onlyAccountCall(t, fake, BindSkipPath))
	if skipParam.RCode != "R-1" || skipParam.Expire != SessionTTLSeconds {
		t.Fatalf("bind/skip param = %#v, want the rcode and the declared session TTL", skipParam)
	}

	// The credential is stored under a deterministic name, with the quirks of the
	// bind=1 path: an EMPTY phone and the WeChat nickname.
	names := fake.savedNames()
	if len(names) != 1 {
		t.Fatalf("saved %d auth files (%v), want exactly 1", len(names), names)
	}
	if !strings.HasPrefix(names[0], ProviderKey+"-") {
		t.Fatalf("saved name = %q, want a loomy- prefix", names[0])
	}
	credential, errParse := ParseCredential(fake.saved[names[0]])
	if errParse != nil {
		t.Fatalf("parse stored credential: %v", errParse)
	}
	if credential.Session() != "0123456789abcdef0123456789abcdef" || credential.UserID != "123456789012345678" {
		t.Fatalf("credential = %#v, want the session and user id from bind/skip", credential)
	}
	if credential.Phone != "" {
		t.Fatalf("phone = %q, want empty: bind/skip never returns one", credential.Phone)
	}
	if credential.Nickname != "微信昵称" {
		t.Fatalf("nickname = %q, want the WeChat nickname", credential.Nickname)
	}
	if credential.Type != ProviderKey {
		t.Fatalf("type = %q, want the provider key", credential.Type)
	}
	wantExpiry := time.Now().Add(time.Duration(SessionTTLSeconds) * time.Second).UnixMilli()
	if delta := credential.ExpiresAtMS() - wantExpiry; delta > 5000 || delta < -5000 {
		t.Fatalf("expires_at = %d, want about %d", credential.ExpiresAtMS(), wantExpiry)
	}
	if calls := fake.callsFor(PointsFirstLoginPath); len(calls) != 1 {
		t.Fatalf("issued %d first-login calls, want 1 (best effort after login)", len(calls))
	}

	// Part 3: poll reports pending until the credential exists — it exists now, so
	// success, with the SAME file name the page saved.
	success := pollLogin(t, h, state)
	if success.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll status = %q, want success", success.Status)
	}
	if success.Auth.FileName != names[0] {
		t.Fatalf("poll file name = %q, want the saved %q", success.Auth.FileName, names[0])
	}
}

// The bind=0 path: 讯飞 has no phone for this WeChat account, so the page asks
// for one with its keypad and completes the login through bind/sendMsg +
// bind/checkCode. The number the SERVER returns wins.
func TestWechatLoginBindsAPhoneWhenTheAccountHasNone(t *testing.T) {
	transport := &wechatTransport{
		uuid:  "4Y0N_jyVQg==",
		polls: []string{wechatPollBody("405", "WX-CODE-2")},
		accounts: func(path string, request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			switch path {
			case BindAuthPath:
				return httpResponse(200, `{"code":"000000","data":{"bind":0,"rcode":"R-2","nickname":"小明"}}`), nil
			case BindSendMsgPath:
				return httpResponse(200, `{"code":"000000","data":{"msgid":"M-9"}}`), nil
			case BindCheckCodePath:
				return httpResponse(200, `{"code":"000000","data":{"session":"fedcba9876543210fedcba9876543210","userid":"876543210987654321","phone":"13800138000"}}`), nil
			case PointsFirstLoginPath:
				return httpResponse(200, `{"code":"000000","data":{"alreadyProcessed":true}}`), nil
			}
			return nil, errFakeHostRoute
		},
	}
	fake := newFakeHost()
	fake.do = transport.do
	fake.install(t)
	h := testHost()
	state := startLoginSession(DefaultConfig(), "").stateValue()

	// Load 1 fetches the QR, load 2 polls, gets 405 and stops on the bind page.
	qrPage(t, h, state, nil)
	bind := qrPage(t, h, state, nil)
	if !strings.Contains(bind, "绑定手机号") {
		t.Fatalf("page = %s, want the phone-binding step", truncate(bind, 600))
	}
	if !strings.Contains(bind, "action="+phoneActionDigit) || !strings.Contains(bind, "action="+bindActionSend) {
		t.Fatalf("page = %s, want the phone keypad and the send link", truncate(bind, 800))
	}
	if strings.Contains(bind, "http-equiv=\"refresh\"") {
		t.Fatal("the binding step waits for the user: it must not poll itself")
	}
	if calls := fake.callsFor(BindSkipPath); len(calls) != 0 {
		t.Fatalf("bind=0 must not call bind/skip (%d calls)", len(calls))
	}
	if names := fake.savedNames(); len(names) != 0 {
		t.Fatalf("saved %v before the phone was bound, want nothing", names)
	}

	// ?action=bindsend&phone=… carries the rcode, not the spent WeChat code.
	renderPage(t, h, resourceLoginPath, url.Values{
		"action": {bindActionSend}, "state": {state}, "phone": {"13800138000"},
	})
	sendParam := accountCallParam[bindSendMsgParam](t, onlyAccountCall(t, fake, BindSendMsgPath))
	if sendParam.RCode != "R-2" || sendParam.Phone != "13800138000" ||
		sendParam.CCode != accountCountryCode || sendParam.Expire != SMSCodeTTLSeconds {
		t.Fatalf("bind/sendMsg param = %#v", sendParam)
	}

	// The code is entered with the same digit links the SMS path uses.
	codePage := renderPage(t, h, resourceLoginPath, url.Values{"action": {bindActionSend}, "state": {state}, "phone": {"13800138000"}})
	if !strings.Contains(codePage, "已输入") || !strings.Contains(codePage, bindActionVerify) {
		t.Fatalf("code page = %s, want the keypad state and the verify link", truncate(codePage, 800))
	}
	for _, digit := range []string{"1", "2", "3", "4", "5", "6"} {
		renderPage(t, h, resourceLoginPath, url.Values{"action": {bindActionDigit}, "state": {state}, "digit": {digit}})
	}
	if session, found := lookupLoginSession(state); !found || session.wechatStateValue().BindDigits != "123456" {
		t.Fatalf("accumulated binding code = %q, want 123456", session.wechatStateValue().BindDigits)
	}

	// Submitting stores the credential, with the server's phone and the WeChat
	// nickname.
	verified := renderPage(t, h, resourceLoginPath, url.Values{"action": {bindActionVerify}, "state": {state}})
	if !strings.Contains(verified, "登录成功") {
		t.Fatalf("verify page = %s, want success", truncate(verified, 600))
	}
	checkParam := accountCallParam[bindCheckCodeParam](t, onlyAccountCall(t, fake, BindCheckCodePath))
	if checkParam.RCode != "R-2" || checkParam.MCode != "123456" || checkParam.MsgID != "M-9" ||
		checkParam.Expire != SessionTTLSeconds {
		t.Fatalf("bind/checkCode param = %#v", checkParam)
	}
	names := fake.savedNames()
	if len(names) != 1 {
		t.Fatalf("saved %v, want exactly one credential", names)
	}
	credential, errParse := ParseCredential(fake.saved[names[0]])
	if errParse != nil {
		t.Fatalf("parse stored credential: %v", errParse)
	}
	if credential.Phone != "13800138000" {
		t.Fatalf("phone = %q, want the number the server returned", credential.Phone)
	}
	if credential.Nickname != "小明" {
		t.Fatalf("nickname = %q, want the WeChat nickname", credential.Nickname)
	}
	if success := pollLogin(t, h, state); success.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll status = %q, want success", success.Status)
	}
}

// `bind` is only 1 when the server sent the NUMBER 1; a string "1" goes through
// the phone binding, because skipping it would produce an unrecoverable account.
func TestWechatBindFlagOnlyAcceptsTheNumberOne(t *testing.T) {
	transport := &wechatTransport{
		uuid:  "001ZDsw64Vu7ll2E",
		polls: []string{wechatPollBody("405", "WX-CODE-3")},
		accounts: func(path string, request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			if path == BindAuthPath {
				return httpResponse(200, `{"code":"000000","data":{"bind":"1","rcode":"R-3"}}`), nil
			}
			return nil, errFakeHostRoute
		},
	}
	fake := newFakeHost()
	fake.do = transport.do
	fake.install(t)
	h := testHost()
	state := startLoginSession(DefaultConfig(), "").stateValue()

	qrPage(t, h, state, nil)
	page := qrPage(t, h, state, nil)
	if !strings.Contains(page, "绑定手机号") {
		t.Fatalf("page = %s, want the phone-binding step for a non-numeric bind", truncate(page, 600))
	}
	if calls := fake.callsFor(BindSkipPath); len(calls) != 0 {
		t.Fatal("a string bind value must not skip the phone binding")
	}
}

// A 405 without a code is an anomalous frame: keep polling, never send an empty
// code to bind/auth (trap #23).
func TestWechatConfirmedWithoutCodeKeepsPolling(t *testing.T) {
	transport := &wechatTransport{
		uuid:  "001ZDsw64Vu7ll2E",
		polls: []string{wechatPollBody("405", "")},
	}
	fake := newFakeHost()
	fake.do = transport.do
	fake.install(t)
	h := testHost()
	state := startLoginSession(DefaultConfig(), "").stateValue()

	qrPage(t, h, state, nil)
	page := qrPage(t, h, state, nil)
	if !strings.Contains(page, "已扫码") {
		t.Fatalf("page = %s, want the scanned wording", truncate(page, 500))
	}
	if calls := fake.callsFor(BindAuthPath); len(calls) != 0 {
		t.Fatalf("bind/auth was called %d times with no code", len(calls))
	}
	if !strings.Contains(page, "http-equiv=\"refresh\"") {
		t.Fatal("an unterminated flow must keep polling")
	}
}

// 402 means the QR is dead: the page replaces it automatically instead of asking
// the user to do anything.
func TestWechatExpiredQRIsReplacedAutomatically(t *testing.T) {
	transport := &wechatTransport{
		uuid:  "001ZDsw64Vu7ll2E",
		polls: []string{wechatPollBody("402", "")},
	}
	fake := newFakeHost()
	fake.do = transport.do
	fake.install(t)
	h := testHost()
	state := startLoginSession(DefaultConfig(), "").stateValue()

	qrPage(t, h, state, nil)
	page := qrPage(t, h, state, nil)
	if !strings.Contains(page, "已过期") {
		t.Fatalf("page = %s, want the expiry notice", truncate(page, 500))
	}
	if !strings.Contains(page, "data:image/jpeg;base64,") {
		t.Fatalf("page = %s, want a replacement QR", truncate(page, 500))
	}
	if transport.qrFetches != 2 {
		t.Fatalf("authorize-page fetches = %d, want 2 (a fresh QR)", transport.qrFetches)
	}
	if !strings.Contains(page, "http-equiv=\"refresh\"") {
		t.Fatal("the replaced QR must keep polling")
	}
}

// 403 is the user's own decision: stop polling and offer a way back.
func TestWechatCancelledStopsPolling(t *testing.T) {
	transport := &wechatTransport{
		uuid:  "001ZDsw64Vu7ll2E",
		polls: []string{wechatPollBody("403", "")},
	}
	fake := newFakeHost()
	fake.do = transport.do
	fake.install(t)
	h := testHost()
	state := startLoginSession(DefaultConfig(), "").stateValue()

	qrPage(t, h, state, nil)
	page := qrPage(t, h, state, nil)
	if !strings.Contains(page, "已取消") {
		t.Fatalf("page = %s, want the cancellation notice", truncate(page, 500))
	}
	if strings.Contains(page, "http-equiv=\"refresh\"") {
		t.Fatal("a cancelled QR must not keep polling")
	}
	if !strings.Contains(page, "fresh=1") {
		t.Fatalf("page = %s, want a link that fetches a new QR", truncate(page, 700))
	}
}

// A long-poll failure is transient: the page says so and keeps polling rather
// than ending the login.
func TestWechatPollErrorKeepsRetrying(t *testing.T) {
	transport := &wechatTransport{uuid: "001ZDsw64Vu7ll2E"}
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, "long.open.weixin.qq.com") {
			return nil, errFakeTransport
		}
		return transport.do(request)
	}
	fake.install(t)
	h := testHost()
	state := startLoginSession(DefaultConfig(), "").stateValue()

	qrPage(t, h, state, nil)
	page := qrPage(t, h, state, nil)
	if !strings.Contains(page, "长轮询失败") {
		t.Fatalf("page = %s, want the poll failure notice", truncate(page, 500))
	}
	if !strings.Contains(page, "http-equiv=\"refresh\"") {
		t.Fatal("a transport failure must retry, not end the login")
	}
}

// A response without an rcode cannot continue: the page fails loudly instead of
// walking into a state machine that can never finish.
func TestWechatBindAuthWithoutRcodeFails(t *testing.T) {
	transport := &wechatTransport{
		uuid:  "001ZDsw64Vu7ll2E",
		polls: []string{wechatPollBody("405", "WX-CODE-4")},
		accounts: func(path string, request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			if path == BindAuthPath {
				return httpResponse(200, `{"code":"000000","data":{"bind":1}}`), nil
			}
			return nil, errFakeHostRoute
		},
	}
	fake := newFakeHost()
	fake.do = transport.do
	fake.install(t)
	h := testHost()
	state := startLoginSession(DefaultConfig(), "").stateValue()

	qrPage(t, h, state, nil)
	page := qrPage(t, h, state, nil)
	if !strings.Contains(page, "rcode") {
		t.Fatalf("page = %s, want the missing-rcode failure", truncate(page, 700))
	}
	if !strings.Contains(page, bindActionRetry) {
		t.Fatalf("page = %s, want a retry link", truncate(page, 700))
	}
	if names := fake.savedNames(); len(names) != 0 {
		t.Fatalf("saved %v after a failed exchange, want nothing", names)
	}
}

// A transport failure while fetching the QR is reported, and the page retries by
// itself instead of showing a dead end.
func TestWechatQRFetchFailureIsRetried(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	fake.install(t)
	h := testHost()
	state := startLoginSession(DefaultConfig(), "").stateValue()

	page := qrPage(t, h, state, nil)
	if !strings.Contains(page, "获取微信二维码失败") {
		t.Fatalf("page = %s, want the fetch failure notice", truncate(page, 500))
	}
	if !strings.Contains(page, "http-equiv=\"refresh\"") {
		t.Fatal("a transient QR failure must retry automatically")
	}
	if strings.Contains(page, "data:image") {
		t.Fatal("a failed fetch must not render a placeholder as if it were a QR")
	}
}

// The QR page is only reachable through GET links: the whole flow must survive
// without a single form or script, which is the mount's hard constraint.
func TestWechatPagesHaveNoFormAndNoScript(t *testing.T) {
	transport := &wechatTransport{
		uuid:  "001ZDsw64Vu7ll2E",
		polls: []string{wechatPollBody("408", "")},
	}
	fake := newFakeHost()
	fake.do = transport.do
	fake.install(t)
	h := testHost()
	state := startLoginSession(DefaultConfig(), "").stateValue()

	for name, page := range map[string]string{
		"home":    renderPage(t, h, resourceLoginPath, nil),
		"qr":      qrPage(t, h, state, nil),
		"polling": qrPage(t, h, state, nil),
		"sms":     renderPage(t, h, resourceLoginPath, url.Values{"action": {"sms"}, "state": {state}}),
	} {
		lower := strings.ToLower(page)
		if strings.Contains(lower, "<form") {
			t.Errorf("%s page contains a form; the resource mount drops POSTs", name)
		}
		if strings.Contains(lower, "<script") {
			t.Errorf("%s page contains a script; the flow must stay script-free", name)
		}
	}
}
