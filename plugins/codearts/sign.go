package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Huawei's Snap-Access gateway authenticates requests with the SDK-HMAC-SHA256
// scheme. Note that this is *not* AWS SigV4 and not the Huawei variant that
// carries a `Credential=` scope: there is no region, no service and no signing
// key derivation chain. The HMAC key is the secret access key itself.
const (
	SignAlgorithm  = "SDK-HMAC-SHA256"
	SignDateFormat = "20060102T150405Z"
)

// Sha256Hex returns the lowercase hex SHA-256 digest of data.
func Sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HmacSha256Hex returns the lowercase hex HMAC-SHA256 of data under key.
func HmacSha256Hex(key, data []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignRequest signs req in place. extraHeaders are folded into the canonical
// request and the SignedHeaders list, and are emitted on the wire with their
// exact given casing, because server-side verification recomputes them.
func SignRequest(req *http.Request, body []byte, ak, sk, securityToken string, now time.Time, extraHeaders map[string]string) {
	method := strings.ToUpper(req.Method)

	uri := req.URL.Path
	if uri == "" {
		uri = "/"
	}
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	// Preserve the original query encoding and ordering; only drop the '?'.
	query := req.URL.RawQuery

	dateStamp := now.UTC().Format(SignDateFormat)
	payloadHash := Sha256Hex(body)

	headers := map[string]string{
		"host":                 req.URL.Host,
		"x-sdk-date":           dateStamp,
		"x-sdk-content-sha256": payloadHash,
		"x-security-token":     securityToken,
	}
	for key, value := range extraHeaders {
		headers[key] = value
	}
	if method != http.MethodGet {
		headers["content-type"] = "application/json"
	}

	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	headerLines := make([]string, 0, len(keys))
	for _, key := range keys {
		headerLines = append(headerLines, key+":"+headers[key])
	}
	signedHeaders := strings.Join(keys, ";")

	// Seven fields joined by '\n'; the fifth is deliberately an empty line.
	canonicalRequest := strings.Join([]string{
		method,
		uri,
		query,
		strings.Join(headerLines, "\n"),
		"",
		signedHeaders,
		payloadHash,
	}, "\n")

	canonicalHash := Sha256Hex([]byte(canonicalRequest))
	stringToSign := SignAlgorithm + "\n" + dateStamp + "\n" + canonicalHash
	signature := HmacSha256Hex([]byte(sk), []byte(stringToSign))

	authorization := SignAlgorithm +
		" Access=" + ak +
		",SignedHeaders=" + signedHeaders +
		",Signature=" + signature

	req.Header.Set("X-Sdk-Date", dateStamp)
	req.Header.Set("X-Sdk-Content-Sha256", payloadHash)
	req.Header.Set("X-Security-Token", securityToken)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range extraHeaders {
		// Assign through the map so the on-wire casing matches the signed name
		// instead of being canonically reformatted by Header.Set.
		req.Header[key] = []string{value}
	}
	req.Header.Set("Authorization", authorization)
}

// UnsignedHeaders returns the headers the Snap-Access gateway expects but which
// must NOT participate in the signature. Signing them breaks verification with
// APIG.0301.
func UnsignedHeaders() map[string]string {
	return map[string]string{
		"Agent-Type": "PromptCenter",
		"X-Language": "zh-cn",
	}
}
