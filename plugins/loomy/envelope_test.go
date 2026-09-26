package main

import (
	"strings"
	"testing"
)

// The envelope rules of `loomy.ts:73-103`. Success is `code == "000000"` and
// nothing else, because business failures arrive with HTTP 200 (trap #1).
func TestEnvelopeSuccessRequiresOkCode(t *testing.T) {
	success, errParse := parseEnvelope([]byte(`{"ok":true,"code":"000000","data":{"balance":15000}}`))
	if errParse != nil {
		t.Fatalf("parse success envelope: %v", errParse)
	}
	if !success.success() {
		t.Fatal(`code "000000" must be the success condition`)
	}
	var data struct {
		Balance float64 `json:"balance"`
	}
	if errDecode := success.decodeData(&data); errDecode != nil {
		t.Fatalf("decode data: %v", errDecode)
	}
	if data.Balance != 15000 {
		t.Fatalf("balance = %v, want 15000", data.Balance)
	}

	// HTTP 200 + a business failure code is a FAILURE.
	expired, errParse := parseEnvelope([]byte(`{"ok":false,"code":"100002","desc":"缺少 token"}`))
	if errParse != nil {
		t.Fatalf("parse failure envelope: %v", errParse)
	}
	if expired.success() {
		t.Fatal(`"100002" must not be treated as success`)
	}
	if !expired.isAuthExpired() {
		t.Fatal(`"100002" must be classified as the terminal auth-expired code`)
	}
	if got := expired.text(); got != "缺少 token" {
		t.Fatalf("message = %q, want the server desc", got)
	}
}

// `desc` wins over `message`, and when both are empty a synthetic message is
// produced instead of an empty error box.
func TestEnvelopeMessagePrecedence(t *testing.T) {
	both, _ := parseEnvelope([]byte(`{"code":"100002","desc":"验证码错误","message":"ignored"}`))
	if got := both.text(); got != "验证码错误" {
		t.Fatalf("text() = %q, want desc to win over message", got)
	}
	onlyMessage, _ := parseEnvelope([]byte(`{"code":"020002","message":"登录态失效"}`))
	if got := onlyMessage.text(); got != "登录态失效" {
		t.Fatalf("text() = %q, want the message fallback", got)
	}
	bare, _ := parseEnvelope([]byte(`{"code":"100001"}`))
	if got := bare.text(); got != "业务错误 100001" {
		t.Fatalf("text() = %q, want the synthetic 业务错误 fallback", got)
	}
}

// The account host is not consistent about scalar typing, so code/desc are read
// through flexText: a numeric code must not throw the whole envelope away.
func TestEnvelopeToleratesNonStringScalars(t *testing.T) {
	parsed, errParse := parseEnvelope([]byte(`{"code":100002,"desc":null,"message":123}`))
	if errParse != nil {
		t.Fatalf("parse envelope: %v", errParse)
	}
	if got := parsed.code(); got != "100002" {
		t.Fatalf("code = %q, want the numeric code rendered as text", got)
	}
	if !parsed.isAuthExpired() {
		t.Fatal("a numeric 100002 must still be recognised as auth-expired")
	}
	if got := parsed.text(); got != "123" {
		t.Fatalf("text() = %q, want the numeric message rendered as text", got)
	}
}

// A non-object payload is an error with the source's own wording, not an empty
// envelope that would read as "no code".
func TestEnvelopeRejectsNonObjectPayload(t *testing.T) {
	for _, body := range []string{`[]`, `"oops"`, `<html>gateway</html>`} {
		if _, errParse := parseEnvelope([]byte(body)); errParse == nil {
			t.Fatalf("parseEnvelope(%q) must fail", body)
		} else if !strings.Contains(errParse.Error(), "响应不是 JSON 对象") {
			t.Fatalf("error = %q, want the documented 响应不是 JSON 对象 wording", errParse)
		}
	}
	if _, errParse := parseEnvelope(nil); errParse == nil {
		t.Fatal("an empty body must fail")
	}
}

// envelopeFailure maps 100002 to a 401 (terminal) and everything else to a
// retryable upstream failure — never the other way round.
func TestEnvelopeFailureClassification(t *testing.T) {
	expired, _ := parseEnvelope([]byte(`{"code":"100002","desc":"缺少 token"}`))
	errExpired := envelopeFailure("探测", expired)
	if status := statusOf(errExpired, 0); status != 401 {
		t.Fatalf("100002 status = %d, want 401", status)
	}
	if strings.Contains(errExpired.Error(), "缺少 token") == false {
		t.Fatalf("error %q must carry the server desc", errExpired)
	}

	unknown, _ := parseEnvelope([]byte(`{"code":"100500","desc":"服务器开小差了"}`))
	errUnknown := envelopeFailure("探测", unknown)
	if status := statusOf(errUnknown, 0); status != 502 {
		t.Fatalf("non-100002 status = %d, want a retryable 502", status)
	}
	if !strings.Contains(errUnknown.Error(), "100500") {
		t.Fatalf("error %q must name the business code", errUnknown)
	}

	badRequest, _ := parseEnvelope([]byte(`{"code":"100001","desc":"未知任务"}`))
	if status := statusOf(envelopeFailure("提交", badRequest), 0); status != 400 {
		t.Fatalf("100001 status = %d, want 400", status)
	}
}

// envelopeErrorText flattens the `error`-style payloads a gateway returns
// instead of the Loomy envelope (`openai-compat.ts:271-292`).
func TestEnvelopeErrorTextFlattensGatewayErrors(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"rate limited"}}`:   "rate limited",
		`{"code":"100002","message":"缺少 token"}`: "缺少 token",
		`{"msg":"bad request"}`:                  "bad request",
		`{"error":{"code":"500"}}`:               "业务错误 500",
	}
	for body, want := range cases {
		if got := envelopeErrorText([]byte(body)); got != want {
			t.Errorf("envelopeErrorText(%s) = %q, want %q", body, got, want)
		}
	}
	if got := envelopeErrorText([]byte("plain text failure")); got != "plain text failure" {
		t.Fatalf("envelopeErrorText = %q, want the raw text fallback", got)
	}
}
