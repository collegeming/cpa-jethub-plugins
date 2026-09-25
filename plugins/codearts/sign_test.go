package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The empty-body SHA-256 is a fixed constant quoted in the porting spec; it is
// the payload hash every GET must produce.
const emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestSha256HexMatchesKnownVectors(t *testing.T) {
	if got := Sha256Hex(nil); got != emptyBodySHA256 {
		t.Fatalf("Sha256Hex(nil) = %q, want the documented empty digest %q", got, emptyBodySHA256)
	}
	if got := Sha256Hex([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("Sha256Hex(\"abc\") = %q, want the FIPS 180-4 vector", got)
	}
}

func TestHmacSha256HexMatchesRFC4231Case2(t *testing.T) {
	// RFC 4231 test case 2: key "Jefe", data "what do ya want for nothing?".
	got := HmacSha256Hex([]byte("Jefe"), []byte("what do ya want for nothing?"))
	const want = "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
	if got != want {
		t.Fatalf("HmacSha256Hex = %q, want RFC 4231 case 2 %q", got, want)
	}
}

// TestSignRequestCanonicalForm pins the exact seven-line canonical request from
// the porting spec, including the forced trailing slash on the path, the empty
// query line, and the deliberately empty fifth line. The expected signature is
// derived independently here so a regression in SignRequest is caught.
func TestSignRequestCanonicalForm(t *testing.T) {
	const (
		ak    = "AKIDEXAMPLE"
		sk    = "SECRETKEYEXAMPLE"
		token = "SECURITYTOKENEXAMPLE"
		body  = `{"model":"GLM-5.2"}`
	)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	dateStamp := "20260925T120000Z"

	request, errRequest := http.NewRequest(http.MethodPost,
		"https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions", nil)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}

	SignRequest(request, []byte(body), ak, sk, token, now, map[string]string{"maas_type": "benefit"})

	payloadHash := Sha256Hex([]byte(body))
	signedHeaders := strings.Join([]string{
		"content-type",
		"host",
		"maas_type",
		"x-sdk-content-sha256",
		"x-sdk-date",
		"x-security-token",
	}, ";")
	headerLines := strings.Join([]string{
		"content-type:application/json",
		"host:snap-access.cn-north-4.myhuaweicloud.com",
		"maas_type:benefit",
		"x-sdk-content-sha256:" + payloadHash,
		"x-sdk-date:" + dateStamp,
		"x-security-token:" + token,
	}, "\n")
	// Seven fields joined by '\n'; the fifth field is intentionally empty.
	canonicalRequest := strings.Join([]string{
		http.MethodPost,
		"/api/v2/chat/completions/",
		"",
		headerLines,
		"",
		signedHeaders,
		payloadHash,
	}, "\n")

	wantSignature := HmacSha256Hex([]byte(sk), []byte(SignAlgorithm+"\n"+dateStamp+"\n"+Sha256Hex([]byte(canonicalRequest))))
	wantAuthorization := SignAlgorithm + " Access=" + ak + ",SignedHeaders=" + signedHeaders + ",Signature=" + wantSignature

	if got := request.Header.Get("Authorization"); got != wantAuthorization {
		t.Fatalf("Authorization mismatch\n got: %s\nwant: %s", got, wantAuthorization)
	}
	if got := request.Header.Get("X-Sdk-Date"); got != dateStamp {
		t.Fatalf("X-Sdk-Date = %q, want %q", got, dateStamp)
	}
	if got := request.Header.Get("X-Sdk-Content-Sha256"); got != payloadHash {
		t.Fatalf("X-Sdk-Content-Sha256 = %q, want %q", got, payloadHash)
	}
	if got := request.Header.Get("X-Security-Token"); got != token {
		t.Fatalf("X-Security-Token = %q, want %q", got, token)
	}
	// The signed extra header must keep its exact lower-case on-wire casing.
	if values := request.Header["maas_type"]; len(values) != 1 || values[0] != "benefit" {
		t.Fatalf("maas_type header = %#v, want value \"benefit\" under the exact lower-case key", values)
	}
	// The signing key is the SK directly: no derivation chain, no region scope.
	if strings.Contains(wantAuthorization, "Credential=") {
		t.Fatal("Authorization must not carry a Credential= scope")
	}
}

// TestSignRequestGetOmitsContentTypeAndQuestionMark guards the two GET-specific
// rules: content-type is only signed for non-GET, and the query is signed in its
// original form without a leading '?'.
func TestSignRequestGetOmitsContentTypeAndQuestionMark(t *testing.T) {
	request, errRequest := http.NewRequest(http.MethodGet,
		"https://snap-access.cn-north-4.myhuaweicloud.com/v1/ops/delivery?channel=IDE&b=2", nil)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	SignRequest(request, nil, "AK", "SK", "ST", time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC), nil)

	authorization := request.Header.Get("Authorization")
	if strings.Contains(authorization, "content-type") {
		t.Fatalf("GET signature must not cover content-type: %s", authorization)
	}
	for _, want := range []string{"host", "x-sdk-content-sha256", "x-sdk-date", "x-security-token"} {
		if !strings.Contains(authorization, want) {
			t.Fatalf("GET signature is missing %q: %s", want, authorization)
		}
	}
	if values := request.Header["X-Sdk-Content-Sha256"]; len(values) != 1 || values[0] != emptyBodySHA256 {
		t.Fatalf("GET payload hash = %#v, want the empty-body digest", values)
	}
}

// TestUnsignedHeadersAreNotSigned is the regression guard for APIG.0301: the
// attribution headers must never enter the signed set.
func TestUnsignedHeadersAreNotSigned(t *testing.T) {
	unsigned := UnsignedHeaders()
	if unsigned["Agent-Type"] != "PromptCenter" || unsigned["X-Language"] != "zh-cn" {
		t.Fatalf("UnsignedHeaders() = %#v, want Agent-Type/X-Language", unsigned)
	}

	request, _ := http.NewRequest(http.MethodGet, "https://snap-access.cn-north-4.myhuaweicloud.com/v1/model/builtin", nil)
	SignRequest(request, nil, "AK", "SK", "ST", time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC), nil)
	authorization := request.Header.Get("Authorization")

	// Adding them after signing (as the adapter does) must leave the signature intact.
	before := authorization
	for key, value := range unsigned {
		request.Header[key] = []string{value}
	}
	if request.Header.Get("Authorization") != before {
		t.Fatal("attaching unsigned headers must not change the signature")
	}
	lowered := strings.ToLower(authorization)
	if strings.Contains(lowered, "agent-type") || strings.Contains(lowered, "x-language") {
		t.Fatalf("Agent-Type/X-Language must not be in SignedHeaders: %s", authorization)
	}
}

func TestNormalizeModelIDStripsDateSuffix(t *testing.T) {
	cases := map[string]string{
		"deepseek-v4-flash-0731": "deepseek-v4-flash",
		"GLM-5.2":                "GLM-5.2",
		"glm-5.3-flash":          "glm-5.3-flash",
		"openpangu-2.0-pro":      "openpangu-2.0-pro",
		"model-12":               "model-12",
		"model-12345":            "model-12345",
	}
	for input, want := range cases {
		if got := normalizeModelID(input); got != want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsVisionModel(t *testing.T) {
	for _, id := range []string{"GLM-5.2-VL", "glm-vl-1.0", "foo-VL-bar"} {
		if !isVisionModel(id) {
			t.Errorf("isVisionModel(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"GLM-5.2", "deepseek-v4-pro", "openpangu-2.0-flash"} {
		if isVisionModel(id) {
			t.Errorf("isVisionModel(%q) = true, want false", id)
		}
	}
}
