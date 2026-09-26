package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// accountBody must produce exactly the byte sequence the golden vector signs:
// fixed field order, the hard-coded macOS UA and the 32-hex trace id
// (`loomy-oauth.ts:55-71`).
func TestAccountBodyMatchesGoldenShape(t *testing.T) {
	body, errBody := accountBody(sendMsgCodeParam{CCode: "86", Phone: "13800138000", Expire: 300}, "0123456789abcdef0123456789abcdef")
	if errBody != nil {
		t.Fatalf("accountBody: %v", errBody)
	}
	if string(body) != goldenBody {
		t.Fatalf("account body = %s\nwant %s", body, goldenBody)
	}
	if !strings.Contains(string(body), `"ua":"Loomy|Desktop|Electron|macOS"`) {
		t.Fatal("base.ua is hard-coded to macOS and must not be 'fixed' (trap #25)")
	}
}

// The signed bytes ARE the sent bytes (trap #4): recomputing the signature from
// the recorded request body must reproduce the Authorization header.
func TestAccountCallSignsTheExactBytesItSends(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"msgid":"M-1"}}`), nil
	}
	fake.install(t)

	cfg := DefaultConfig()
	now := time.Date(2025, 10, 6, 12, 0, 0, 0, time.UTC)
	if _, errSend := sendMsgCode(testHost(), cfg, "13800138000", now); errSend != nil {
		t.Fatalf("sendMsgCode: %v", errSend)
	}

	calls := fake.callsFor(SendMsgCodePath)
	if len(calls) != 1 {
		t.Fatalf("issued %d sendMsgCode calls, want 1", len(calls))
	}
	sent := calls[0]
	if sent.Method != "POST" || sent.URL != AccountBase+SendMsgCodePath {
		t.Fatalf("call = %s %s, want POST %s", sent.Method, sent.URL, AccountBase+SendMsgCodePath)
	}

	date := sent.Headers.Get("Date")
	nonce := sent.Headers.Get("Nonce")
	expected := signRequest(AccountAccessKeyID, AccountAccessKeySecret, signInput{
		Method:      "POST",
		Path:        SendMsgCodePath,
		Body:        sent.Body,
		ContentType: accountContentType,
		Date:        mustParseHTTPDate(t, date),
		Nonce:       nonce,
	})
	if got := sent.Headers.Get("Authorization"); got != expected.Get("Authorization") {
		t.Fatalf("Authorization = %q, want the signature over the sent bytes (%q)", got, expected.Get("Authorization"))
	}
	if got := sent.Headers.Get("Content-MD5"); got != expected.Get("Content-MD5") || got == "" {
		t.Fatalf("Content-MD5 = %q, want the md5 of the sent body", got)
	}
	if got := sent.Headers.Get("Content-Type"); got != accountContentType {
		t.Fatalf("Content-Type = %q, want %q", got, accountContentType)
	}
	if got := sent.Headers.Get("Accept"); got != "" {
		t.Fatalf("Accept = %q, want no Accept header on account calls", got)
	}
	if !strings.HasPrefix(sent.Headers.Get("Authorization"), "account ") {
		t.Fatalf("Authorization = %q, want the account prefix, not Bearer", sent.Headers.Get("Authorization"))
	}
	var envelope struct {
		Base struct {
			AppID   string `json:"appid"`
			ModelID string `json:"modelid"`
			TraceID string `json:"traceid"`
		} `json:"base"`
		Param sendMsgCodeParam `json:"param"`
	}
	if errUnmarshal := json.Unmarshal(sent.Body, &envelope); errUnmarshal != nil {
		t.Fatalf("decode sent body: %v", errUnmarshal)
	}
	if envelope.Base.AppID != AccountAppID || envelope.Base.ModelID != AccountModelID {
		t.Fatalf("base = %#v, want the fixed client identity", envelope.Base)
	}
	if len(envelope.Base.TraceID) != 32 {
		t.Fatalf("traceid = %q, want 32 hex characters", envelope.Base.TraceID)
	}
	if envelope.Param.CCode != "86" || envelope.Param.Expire != SMSCodeTTLSeconds {
		t.Fatalf("param = %#v, want ccode 86 and the declared 300s TTL", envelope.Param)
	}
}

// The phone gate runs BEFORE any request, with the source's own pattern.
func TestSendMsgCodeRejectsInvalidPhoneLocally(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatalf("an invalid phone must not reach the account host (%s)", request.URL)
		return nil, nil
	}
	fake.install(t)

	_, errSend := sendMsgCode(testHost(), DefaultConfig(), "12800138000", time.Now())
	if errSend == nil {
		t.Fatal("an invalid phone must fail")
	}
	if statusOf(errSend, 0) != 400 {
		t.Fatalf("status = %d, want 400", statusOf(errSend, 0))
	}
	if len(fake.requests) != 0 {
		t.Fatalf("issued %d requests, want 0", len(fake.requests))
	}
}

// An empty msgid is an upstream contract violation, not a usable code id
// (`loomy-oauth.ts:142-149`).
func TestSendMsgCodeRequiresMsgID(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{}}`), nil
	}
	fake.install(t)
	_, errSend := sendMsgCode(testHost(), DefaultConfig(), "13800138000", time.Now())
	if errSend == nil || !strings.Contains(errSend.Error(), "缺少 msgid") {
		t.Fatalf("error = %v, want the documented 缺少 msgid failure", errSend)
	}
}

// The server `desc` travels back verbatim so the page can distinguish causes
// (`loomy-oauth.ts:119-120`).
func TestAccountErrorsSurfaceTheServerDesc(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"ok":false,"code":"030001","desc":"手机号格式错误"}`), nil
	}
	fake.install(t)
	_, errSend := sendMsgCode(testHost(), DefaultConfig(), "13800138000", time.Now())
	if errSend == nil {
		t.Fatal("a business failure must fail")
	}
	if !strings.Contains(errSend.Error(), "手机号格式错误") {
		t.Fatalf("error = %v, want the server desc", errSend)
	}
	if !strings.Contains(errSend.Error(), "030001") {
		t.Fatalf("error = %v, want the business code", errSend)
	}
}

// A transport failure on the account host is NOT a credential failure
// (trap #10): it must stay retryable so a network hiccup never forces a re-login.
func TestAccountTransportFailureIsNotCredentialExpiry(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	fake.install(t)
	_, errSend := sendMsgCode(testHost(), DefaultConfig(), "13800138000", time.Now())
	if errSend == nil {
		t.Fatal("a transport failure must fail")
	}
	if status := statusOf(errSend, 0); status != 502 {
		t.Fatalf("status = %d, want a retryable 502 rather than a credential 401", status)
	}
}

// checkCode validates session and userid, and declares the SESSION TTL — not the
// SMS TTL — in `expire`.
func TestCheckCodeBuildsSessionRequest(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"session":"0123456789abcdef0123456789abcdef","userid":"123456789012345678"}}`), nil
	}
	fake.install(t)

	cfg := DefaultConfig()
	session, userID, errCheck := checkCode(testHost(), cfg, "13800138000", "654321", "M-1", time.Now())
	if errCheck != nil {
		t.Fatalf("checkCode: %v", errCheck)
	}
	if session != "0123456789abcdef0123456789abcdef" || userID != "123456789012345678" {
		t.Fatalf("session/userid = %q/%q, want the server values", session, userID)
	}
	calls := fake.callsFor(CheckCodePath)
	if len(calls) != 1 {
		t.Fatalf("issued %d checkCode calls, want 1", len(calls))
	}
	var envelope struct {
		Param checkCodeParam `json:"param"`
	}
	if errUnmarshal := json.Unmarshal(calls[0].Body, &envelope); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	if envelope.Param.MCode != "654321" || envelope.Param.MsgID != "M-1" || envelope.Param.Phone != "13800138000" {
		t.Fatalf("param = %#v, want mcode/msgid/phone", envelope.Param)
	}
	if envelope.Param.Expire != cfg.sessionTTL() {
		t.Fatalf("expire = %d, want the session TTL %d", envelope.Param.Expire, cfg.sessionTTL())
	}
}

// Missing session/userid is an upstream contract violation.
func TestCheckCodeRequiresSessionAndUserID(t *testing.T) {
	cases := map[string]string{
		"缺少 session": `{"code":"000000","data":{"userid":"1"}}`,
		"缺少 userid":  `{"code":"000000","data":{"session":"s"}}`,
	}
	for want, body := range cases {
		fake := newFakeHost()
		fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(200, body), nil
		}
		fake.install(t)
		_, _, errCheck := checkCode(testHost(), DefaultConfig(), "13800138000", "654321", "M-1", time.Now())
		if errCheck == nil || !strings.Contains(errCheck.Error(), want) {
			t.Fatalf("error = %v, want %s", errCheck, want)
		}
	}
}

// An empty code never reaches the network.
func TestCheckCodeRejectsEmptyCodeLocally(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		t.Fatal("an empty code must not be submitted")
		return nil, nil
	}
	fake.install(t)
	if _, _, errCheck := checkCode(testHost(), DefaultConfig(), "13800138000", "  ", "M-1", time.Now()); errCheck == nil {
		t.Fatal("an empty code must fail")
	}
	if len(fake.requests) != 0 {
		t.Fatalf("issued %d requests, want 0", len(fake.requests))
	}
}

// mustParseHTTPDate parses a Date header back into a time, failing the test when
// it is malformed.
func mustParseHTTPDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, errParse := time.Parse(http.TimeFormat, value)
	if errParse != nil {
		t.Fatalf("Date header %q: %v", value, errParse)
	}
	return parsed
}
