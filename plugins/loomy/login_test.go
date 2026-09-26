package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const resourceLoginPath = "/v0/resource/plugins/loomy/login"

// loginStartPayload builds an auth.login.start request the way the host sends it,
// including the management callback base URL.
func loginStartPayload(t *testing.T, baseURL string, metadata map[string]any) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(pluginapi.AuthLoginStartRequest{
		Provider: ProviderKey,
		BaseURL:  baseURL,
		Metadata: metadata,
	})
	if errMarshal != nil {
		t.Fatalf("marshal login start: %v", errMarshal)
	}
	return raw
}

// startLogin drives auth.login.start and returns the response.
func startLogin(t *testing.T, h *abiboot.Host, baseURL string, metadata map[string]any) pluginapi.AuthLoginStartResponse {
	t.Helper()
	value, errStart := handleAuthLoginStart(h, loginStartPayload(t, baseURL, metadata))
	if errStart != nil {
		t.Fatalf("auth.login.start: %v", errStart)
	}
	return decodeResult[pluginapi.AuthLoginStartResponse](t, value)
}

// pollLogin drives auth.login.poll for one state.
func pollLogin(t *testing.T, h *abiboot.Host, state string) pluginapi.AuthLoginPollResponse {
	t.Helper()
	raw, errMarshal := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: ProviderKey, State: state})
	if errMarshal != nil {
		t.Fatalf("marshal poll: %v", errMarshal)
	}
	value, errPoll := handleAuthLoginPoll(h, raw)
	if errPoll != nil {
		t.Fatalf("auth.login.poll: %v", errPoll)
	}
	return decodeResult[pluginapi.AuthLoginPollResponse](t, value)
}

// start must return a URL and a state immediately and must not touch WeChat or
// the account host: the returned URL opens the plugin's own page, and THAT page
// load fetches and renders the QR code. Doing the fetch here would block the
// host's own request for as long as WeChat takes and would still show nothing.
func TestLoginStartReturnsURLWithoutNetwork(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatalf("auth.login.start must not call WeChat or the account host (%s)", request.URL)
		return nil, nil
	}
	fake.install(t)

	response := startLogin(t, testHost(), "http://127.0.0.1:8317/v0/management/oauth-callback", map[string]any{"phone": "13800138000"})
	if response.Provider != ProviderKey {
		t.Fatalf("provider = %q, want %q", response.Provider, ProviderKey)
	}
	if response.State == "" {
		t.Fatal("state must not be empty: the host rejects an empty state")
	}
	// The host validates the state against [A-Za-z0-9-_.]; anything else is
	// rejected before the plugin is ever polled.
	for _, character := range response.State {
		valid := (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.'
		if !valid {
			t.Fatalf("state %q contains %q, which the host's ValidateOAuthState rejects", response.State, character)
		}
	}
	// The entry point leads straight to the QR page: parsing the URL must yield
	// action=qr, so the very first page load shows a scannable code.
	wantURL := "http://127.0.0.1:8317" + loginResourcePath + "?action=" + loginQRAction + "&state=" + response.State
	if response.URL != wantURL {
		t.Fatalf("url = %q, want %q", response.URL, wantURL)
	}
	if response.ExpiresAt.IsZero() {
		t.Fatal("a login session must carry an expiry")
	}
	if response.Metadata["flow"] != "wechat" {
		t.Fatalf("metadata = %#v, want flow=wechat", response.Metadata)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("issued %d requests, want 0", len(fake.requests))
	}
}

// Without a usable base URL the relative resource path is returned, which the
// manager resolves against the API base.
func TestLoginStartWithoutBaseURLStaysRelative(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	response := startLogin(t, testHost(), "", nil)
	if !strings.HasPrefix(response.URL, loginResourcePath+"?") {
		t.Fatalf("url = %q, want the relative resource path", response.URL)
	}
	if !strings.Contains(response.URL, "action="+loginQRAction) || !strings.Contains(response.URL, "state="+response.State) {
		t.Fatalf("url = %q, want the QR action and the state", response.URL)
	}
}

// Poll reports `pending` until the credential is stored, and the whole SMS flow
// runs through GET links on the plugin's own page.
func TestSMSLoginFlowThroughManagementPage(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, SendMsgCodePath):
			return httpResponse(200, `{"code":"000000","data":{"msgid":"M-1"}}`), nil
		case strings.Contains(request.URL, CheckCodePath):
			return httpResponse(200, `{"code":"000000","data":{"session":"0123456789abcdef0123456789abcdef","userid":"123456789012345678"}}`), nil
		case strings.Contains(request.URL, PointsFirstLoginPath):
			return httpResponse(200, `{"code":"000000","data":{"alreadyProcessed":false,"permanentBalance":5000,"dailyBalance":5000,"dailyQuota":5000,"dailyConsumed":0}}`), nil
		}
		return nil, errFakeHostRoute
	}
	fake.install(t)

	h := testHost()
	started := startLogin(t, h, "http://127.0.0.1:8317/v0/management/oauth-callback", nil)

	if pending := pollLogin(t, h, started.State); pending.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("poll status = %q, want pending before the credential exists", pending.Status)
	}

	// ?action=send&phone=… (a link, never a form).
	send := callManagement(t, h, managementRequest("GET", resourceLoginPath,
		url.Values{"action": {"send"}, "state": {started.State}, "phone": {"13800138000"}}, nil))
	if send.StatusCode != 200 || !strings.Contains(string(send.Body), "输入短信验证码") {
		t.Fatalf("send page status = %d, body = %s", send.StatusCode, truncate(string(send.Body), 200))
	}
	if calls := fake.callsFor(SendMsgCodePath); len(calls) != 1 {
		t.Fatalf("issued %d sendMsgCode calls, want 1", len(calls))
	}

	// The keypad accumulates the code, because an input field would need a form.
	for _, digit := range []string{"1", "2", "3", "4", "5", "6"} {
		callManagement(t, h, managementRequest("GET", resourceLoginPath,
			url.Values{"action": {"code"}, "state": {started.State}, "digit": {digit}}, nil))
	}
	if session, found := lookupLoginSession(started.State); !found || session.accumulatedCode() != "123456" {
		t.Fatalf("accumulated code = %q, want 123456", session.accumulatedCode())
	}

	// ?action=verify&… — without an explicit code the keypad buffer is used.
	verified := callManagement(t, h, managementRequest("GET", resourceLoginPath,
		url.Values{"action": {"verify"}, "state": {started.State}}, nil))
	if !strings.Contains(string(verified.Body), "登录成功") {
		t.Fatalf("verify page = %s, want a success page", truncate(string(verified.Body), 300))
	}

	// The credential is stored, and the name is deterministic.
	names := fake.savedNames()
	if len(names) != 1 {
		t.Fatalf("saved %d auth files (%v), want exactly 1", len(names), names)
	}
	if !strings.HasPrefix(names[0], ProviderKey+"-") {
		t.Fatalf("saved name = %q, want a loomy- prefix", names[0])
	}

	// The daily grant is initialised best-effort after login.
	if calls := fake.callsFor(PointsFirstLoginPath); len(calls) != 1 {
		t.Fatalf("issued %d first-login calls, want 1", len(calls))
	}

	// Poll now reports success and hands the SAME file name back to the host.
	success := pollLogin(t, h, started.State)
	if success.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll status = %q, want success", success.Status)
	}
	if success.Auth.FileName != names[0] {
		t.Fatalf("poll file name = %q, want the saved %q (so a host-side save overwrites it)", success.Auth.FileName, names[0])
	}
	parsed, errParse := ParseCredential(success.Auth.StorageJSON)
	if errParse != nil {
		t.Fatalf("parse returned credential: %v", errParse)
	}
	if parsed.Session() != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("session = %q, want the account host session", parsed.Session())
	}
	// The credential expires 14 days out, as declared.
	wantExpiry := time.Now().Add(time.Duration(SessionTTLSeconds) * time.Second).UnixMilli()
	if delta := parsed.ExpiresAtMS() - wantExpiry; delta > 5000 || delta < -5000 {
		t.Fatalf("expires_at = %d, want about %d (now + %ds)", parsed.ExpiresAtMS(), wantExpiry, SessionTTLSeconds)
	}

	// The session is dropped once reported, so a second poll cannot replay it.
	if again := pollLogin(t, h, started.State); again.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("second poll status = %q, want an error for a forgotten session", again.Status)
	}
}

// A bare `?action=verify` link must use the number the code was sent to, not the
// configured default: otherwise a typo in the settings verifies against the wrong
// account.
func TestVerifyPrefersTheSessionPhone(t *testing.T) {
	fake := newFakeHost()
	var checked checkCodeParam
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, SendMsgCodePath):
			return httpResponse(200, `{"code":"000000","data":{"msgid":"M-1"}}`), nil
		case strings.Contains(request.URL, CheckCodePath):
			var envelope struct {
				Param checkCodeParam `json:"param"`
			}
			if errUnmarshal := json.Unmarshal(request.Body, &envelope); errUnmarshal != nil {
				return nil, errUnmarshal
			}
			checked = envelope.Param
			return httpResponse(200, `{"code":"000000","data":{"session":"0123456789abcdef0123456789abcdef","userid":"1"}}`), nil
		}
		return httpResponse(200, `{"code":"000000","data":{}}`), nil
	}
	fake.install(t)

	cfg := DefaultConfig()
	cfg.Phone = "13800138000"
	withSettings(t, cfg)
	h := testHost()
	session := startLoginSession(cfg, "13900139000")

	callManagement(t, h, managementRequest("GET", resourceLoginPath,
		url.Values{"action": {"send"}, "state": {session.stateValue()}, "phone": {"13900139000"}}, nil))
	verified := callManagement(t, h, managementRequest("GET", resourceLoginPath,
		url.Values{"action": {"verify"}, "state": {session.stateValue()}, "code": {"123456"}}, nil))
	if !strings.Contains(string(verified.Body), "登录成功") {
		t.Fatalf("verify page = %s, want success", truncate(string(verified.Body), 300))
	}
	if checked.Phone != "13900139000" {
		t.Fatalf("checkCode phone = %q, want the session number 13900139000", checked.Phone)
	}
}

// A verify without a msgid is rejected locally: the server would answer with a
// confusing error instead of "please send a code first"
// (`jet-hub-rpc.ts:1139-1185`).
func TestVerifyWithoutMsgIDIsRejectedLocally(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatalf("no request may be sent before a code was requested (%s)", request.URL)
		return nil, nil
	}
	fake.install(t)
	session := startLoginSession(DefaultConfig(), "13800138000")
	_, errVerify := verifyLoginCode(testHost(), DefaultConfig(), session, "13800138000", "123456", time.Now())
	if errVerify == nil || !strings.Contains(errVerify.Error(), "请先发送验证码") {
		t.Fatalf("error = %v, want 请先发送验证码", errVerify)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("issued %d requests, want 0", len(fake.requests))
	}
}

// A failed verify keeps the pending entry so the user can retry
// (`jet-hub-rpc.ts:1174-1183`).
func TestVerifyFailureKeepsTheSession(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, SendMsgCodePath) {
			return httpResponse(200, `{"code":"000000","data":{"msgid":"M-1"}}`), nil
		}
		return httpResponse(200, `{"code":"020002","desc":"验证码错误"}`), nil
	}
	fake.install(t)

	session := startLoginSession(DefaultConfig(), "13800138000")
	if errSend := sendLoginCode(testHost(), DefaultConfig(), session, "13800138000", time.Now()); errSend != nil {
		t.Fatalf("sendLoginCode: %v", errSend)
	}
	_, errVerify := verifyLoginCode(testHost(), DefaultConfig(), session, "13800138000", "000000", time.Now())
	if errVerify == nil || !strings.Contains(errVerify.Error(), "验证码错误") {
		t.Fatalf("error = %v, want the server desc", errVerify)
	}
	if !session.hasMsgID() {
		t.Fatal("a failed verify must keep the msgid so the user can retry")
	}
	if _, found := lookupLoginSession(session.stateValue()); !found {
		t.Fatal("a failed verify must keep the session alive")
	}
}

// An expired session is invisible to every entry point.
func TestExpiredLoginSessionIsDropped(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	session := startLoginSession(DefaultConfig(), "13800138000")
	state := session.stateValue()
	session.mu.Lock()
	session.ExpiresAt = time.Now().Add(-time.Second)
	session.mu.Unlock()
	if _, found := lookupLoginSession(state); found {
		t.Fatal("an expired session must not be found")
	}
	if response := pollLogin(t, testHost(), state); response.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("poll status = %q, want error for an expired session", response.Status)
	}
}

// A phone number typed on a link is validated before any request.
func TestSendLoginCodeValidatesPhone(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatal("an invalid phone must not reach the account host")
		return nil, nil
	}
	fake.install(t)
	session := startLoginSession(DefaultConfig(), "")
	errSend := sendLoginCode(testHost(), DefaultConfig(), session, "+1 555 0100", time.Now())
	if errSend == nil || statusOf(errSend, 0) != 400 {
		t.Fatalf("error = %v, want a 400 phone failure", errSend)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("issued %d requests, want 0", len(fake.requests))
	}
}

// The configured phone is the default the page offers, and the login session
// lifetime follows the setting.
func TestLoginSessionUsesConfiguredLifetimeAndPhone(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	cfg := DefaultConfig()
	cfg.Phone = "13900139000"
	cfg.LoginTimeoutMS = 60_000
	withSettings(t, cfg)

	started := startLogin(t, testHost(), "", nil)
	if started.Metadata["phone"] != "13900139000" {
		t.Fatalf("metadata phone = %v, want the configured default", started.Metadata["phone"])
	}
	session, found := lookupLoginSession(started.State)
	if !found {
		t.Fatal("the session must exist right after start")
	}
	remaining := time.Until(session.ExpiresAt)
	if remaining > time.Minute+time.Second || remaining < 50*time.Second {
		t.Fatalf("session lifetime = %s, want about 60s", remaining)
	}
}

// errFakeHostRoute is returned when a test forgot to script a route.
var errFakeHostRoute = fmt.Errorf("no scripted fake route")
