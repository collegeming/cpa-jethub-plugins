package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The golden vector from the porting spec §3.5. The expected Content-MD5 and
// signature were computed with Node from the TypeScript algorithm, so matching
// them proves the Go port reproduces the wire format bit for bit.
const (
	goldenBody  = `{"base":{"appid":"GM3LOOMY","modelid":"Web","version":"1.0.0","devid":"web","ua":"Loomy|Desktop|Electron|macOS","traceid":"0123456789abcdef0123456789abcdef"},"param":{"ccode":"86","phone":"13800138000","expire":300}}`
	goldenNonce = "11111111-2222-3333-4444-555555555555"
	goldenDate  = "Mon, 06 Oct 2025 12:00:00 GMT"
	goldenMD5   = "tTPnQR6rYm7mt4L/ybmRug=="
	goldenSig   = "tQv47ymAPUlyIPpX0uyDqxeevsk="
	goldenSign  = "POST\n/login/phone/sendMsgCode\n\n" + goldenMD5 + "\napplication/json\n" + goldenDate + "\n" + goldenNonce + "\n\n"
)

// goldenTime is the Date of the golden vector, built in UTC so the formatter
// cannot silently depend on the local zone.
func goldenTime() time.Time {
	return time.Date(2025, 10, 6, 12, 0, 0, 0, time.UTC)
}

func goldenInput() signInput {
	return signInput{
		Method:      http.MethodPost,
		Path:        "/login/phone/sendMsgCode",
		Body:        []byte(goldenBody),
		ContentType: accountContentType,
		Date:        goldenTime(),
		Nonce:       goldenNonce,
	}
}

// TestSigningStringGoldenVector is the decisive test: the spec's §3.5 vector
// must come out byte-identical, including the empty query line and the two
// trailing newlines.
func TestSigningStringGoldenVector(t *testing.T) {
	got := signingString(goldenInput())
	if got != goldenSign {
		t.Fatalf("signing string mismatch\n got: %q\nwant: %q", got, goldenSign)
	}
	if len(got) != 141 {
		t.Fatalf("signing string length = %d, want the documented 141", len(got))
	}
	if md5Value := contentMD5([]byte(goldenBody)); md5Value != goldenMD5 {
		t.Fatalf("Content-MD5 = %q, want %q", md5Value, goldenMD5)
	}
	if signature := signatureOf(AccountAccessKeySecret, got); signature != goldenSig {
		t.Fatalf("signature = %q, want %q", signature, goldenSig)
	}
}

// TestSignRequestGoldenHeaders pins the header set produced for the vector: the
// `account` prefix (not `Bearer`), the signed Date string verbatim, and the
// Content-MD5 of the exact body bytes.
func TestSignRequestGoldenHeaders(t *testing.T) {
	header := signRequest(AccountAccessKeyID, AccountAccessKeySecret, goldenInput())

	const wantAuth = "account 2thryby66wxi53sk:" + goldenSig
	if got := header.Get("Authorization"); got != wantAuth {
		t.Fatalf("Authorization = %q, want %q", got, wantAuth)
	}
	if got := header.Get("Date"); got != goldenDate {
		t.Fatalf("Date = %q, want the exact signed string %q", got, goldenDate)
	}
	if got := header.Get("Nonce"); got != goldenNonce {
		t.Fatalf("Nonce = %q, want %q", got, goldenNonce)
	}
	if got := header.Get("Content-MD5"); got != goldenMD5 {
		t.Fatalf("Content-MD5 = %q, want %q", got, goldenMD5)
	}
	if got := header.Get("Content-Type"); got != accountContentType {
		t.Fatalf("Content-Type = %q, want %q", got, accountContentType)
	}
	// The account host has no Accept header in the signed set.
	if got := header.Get("Accept"); got != "" {
		t.Fatalf("Accept = %q, want no Accept header on account calls", got)
	}
}

// TestSigningStringEndsWithTwoNewlines guards porting trap #5: segments 8 and 9
// are always empty, so the string ends with "\n\n" and must never be trimmed.
func TestSigningStringEndsWithTwoNewlines(t *testing.T) {
	signing := signingString(goldenInput())
	if !strings.HasSuffix(signing, "\n\n") {
		t.Fatalf("signing string must end with two newlines, got %q", signing)
	}
	if strings.HasSuffix(signing, "\n\n\n") {
		t.Fatalf("signing string has more than two trailing newlines: %q", signing)
	}
	if segments := strings.Split(signing, "\n"); len(segments) != 9 {
		t.Fatalf("signing string has %d segments, want 9", len(segments))
	} else if segments[7] != "" || segments[8] != "" {
		t.Fatalf("segments 8/9 = %q/%q, want both empty", segments[7], segments[8])
	}
}

// TestContentMD5EmptyBody pins porting trap #3: an empty body contributes an
// empty segment AND omits the header — it is not the md5 of "".
func TestContentMD5EmptyBody(t *testing.T) {
	if got := contentMD5(nil); got != "" {
		t.Fatalf("contentMD5(nil) = %q, want the empty string", got)
	}
	if got := contentMD5([]byte{}); got != "" {
		t.Fatalf("contentMD5(empty) = %q, want the empty string", got)
	}
	// The literal md5 of "" base64 ("1B2M2Y8AsgTpgAmY7PhCfg==") must NOT appear.
	if base64OfEmpty := "1B2M2Y8AsgTpgAmY7PhCfg=="; contentMD5([]byte("")) == base64OfEmpty {
		t.Fatal("empty body must not be signed as the md5 of the empty string")
	}

	input := signInput{Method: http.MethodPost, Path: "/login/phone/sendMsgCode", ContentType: accountContentType, Date: goldenTime(), Nonce: goldenNonce}
	signing := signingString(input)
	segments := strings.Split(signing, "\n")
	if len(segments) != 9 || segments[3] != "" {
		t.Fatalf("empty body must produce an empty Content-MD5 segment, got %q", signing)
	}
	header := signRequest("AK", "SK", input)
	if _, present := header["Content-Md5"]; present {
		t.Fatal("Content-MD5 header must be omitted for an empty body")
	}
}

// TestEscapedPathRules covers the four path rules of `loomy-sign.ts:65-69`.
func TestEscapedPathRules(t *testing.T) {
	cases := map[string]string{
		"/login/phone/sendMsgCode": "/login/phone/sendMsgCode",
		"login/phone/sendMsgCode":  "/login/phone/sendMsgCode",
		"/points/records/":         "/points/records",
		"/":                        "/",
		"":                         "/",
		"//":                       "/",
		"/a b/c+d":                 "/a%20b/c%2Bd",
		"/a/b/":                    "/a/b",
	}
	for input, want := range cases {
		if got := escapedPath(input); got != want {
			t.Errorf("escapedPath(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestEscapeComponentMatchesRFC3986Unreserved pins the escaping alphabet: the
// unreserved set survives, everything else is uppercase percent-encoded.
func TestEscapeComponentMatchesRFC3986Unreserved(t *testing.T) {
	if got := escapeComponent("AZaz09-_.~"); got != "AZaz09-_.~" {
		t.Fatalf("unreserved characters must pass through unchanged, got %q", got)
	}
	cases := map[string]string{
		"a b":      "a%20b",
		"a+b":      "a%2Bb",
		"a/b":      "a%2Fb",
		"a=b":      "a%3Db",
		"a&b":      "a%26b",
		"a!b":      "a%21b", // explicitly escaped on top of encodeURIComponent
		"a'b":      "a%27b",
		"a(b)":     "a%28b%29",
		"a*b":      "a%2Ab",
		"讯飞":       "%E8%AE%AF%E9%A3%9E",
		"100%":     "100%25",
		"a\u007fb": "a%7Fb",
	}
	for input, want := range cases {
		if got := escapeComponent(input); got != want {
			t.Errorf("escapeComponent(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestEscapedQueryKeepsInsertionOrder: the source joins an ordered list and
// never sorts, so the port must not either.
func TestEscapedQueryKeepsInsertionOrder(t *testing.T) {
	if got := escapedQuery(nil); got != "" {
		t.Fatalf("escapedQuery(nil) = %q, want the empty segment", got)
	}
	params := []queryParam{{Key: "pageSize", Value: "1"}, {Key: "pageNo", Value: "1"}, {Key: "recordType", Value: "all"}}
	want := "pageSize=1&pageNo=1&recordType=all"
	if got := escapedQuery(params); got != want {
		t.Fatalf("escapedQuery = %q, want the insertion order %q", got, want)
	}
	// A query-less path still yields the empty segment in the signing string,
	// hence the "\n\n" right after the path.
	signing := signingString(signInput{Method: http.MethodGet, Path: "/points/records", Date: goldenTime(), Nonce: goldenNonce})
	if !strings.HasPrefix(signing, "GET\n/points/records\n\n") {
		t.Fatalf("query-less signing string = %q, want an empty query line", signing)
	}
}

// TestDateIsFormattedWithGMT pins porting trap #9: time.RFC1123 would print
// `UTC` for a UTC time and break verification.
func TestDateIsFormattedWithGMT(t *testing.T) {
	rendered := signDate(goldenTime())
	if rendered != goldenDate {
		t.Fatalf("signDate = %q, want %q", rendered, goldenDate)
	}
	if strings.Contains(rendered, "UTC") {
		t.Fatal("the Date segment must print GMT, not UTC")
	}
	if rfc1123 := goldenTime().Format(time.RFC1123); rfc1123 == rendered {
		t.Fatal("time.RFC1123 must not produce the signing date")
	}
	// A zero time means "now", which must still format as GMT.
	if now := signDate(time.Time{}); !strings.HasSuffix(now, " GMT") {
		t.Fatalf("signDate(zero) = %q, want a GMT-suffixed date", now)
	}
}

// TestSignRequestUsesConfiguredKeys: the embedded AK/SK can be overridden by the
// settings, which is the documented mitigation for an upstream rotation.
func TestSignRequestUsesConfiguredKeys(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AccountAK = "custom-ak"
	cfg.AccountSK = "custom-sk"
	header := accountHeaders(cfg, goldenInput())
	wantSignature := signatureOf("custom-sk", signingString(goldenInput()))
	if got := header.Get("Authorization"); got != "account custom-ak:"+wantSignature {
		t.Fatalf("Authorization = %q, want the configured key pair to sign", got)
	}
	if got := DefaultConfig().accountAK(); got != AccountAccessKeyID {
		t.Fatalf("default AK = %q, want the embedded %q", got, AccountAccessKeyID)
	}
	if got := DefaultConfig().accountSK(); got != AccountAccessKeySecret {
		t.Fatalf("default SK = %q, want the embedded value", got)
	}
}

// TestSignRequestPathIsSignedNotURL: the signature covers the escaped PATH, so a
// change to the body alone must change the signature too.
func TestSignRequestBodyIsCovered(t *testing.T) {
	first := signRequest("AK", "SK", goldenInput())
	second := goldenInput()
	second.Body = []byte(goldenBody + " ")
	if first.Get("Authorization") == signRequest("AK", "SK", second).Get("Authorization") {
		t.Fatal("a changed body must change the signature")
	}
}
