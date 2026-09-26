package main

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

// Account-host request signing, ported from `src/loomy-sign.ts`.
//
// Only the ACCOUNT host (`account.xfinfr.com`) signs. Business and inference
// endpoints (`/models`, `/points/*`, `/onboarding/*`, `/chat/completions`) use
// the user session instead — the AK/SK never reach them (`loomy-product.ts:19-23`).
//
// The scheme is plain HMAC-SHA1 — no WASM, no opaque blob:
//
//	HMAC-SHA1(key = accessKeySecret, message = <9-segment string>) -> base64
//	Authorization: account <accessKeyId>:<signature>
//
// ⚠️ The prefix is `account`, NOT `Bearer` (`loomy-sign.ts:18,140`). Sending
// `Bearer` here makes the account host reject the call, and the failure looks
// like a login problem rather than a header problem.
const (
	// AccountBase is the iFlytek CAccount host (`loomy.ts:33`).
	AccountBase = "https://account.xfinfr.com"

	// AccountAccessKeyID / AccountAccessKeySecret are the client-distributed
	// credentials a Loomy desktop install ships in
	// `C:\Program Files\Loomy\resources\.env.prod`
	// (`loomy-product.ts:93-95`). They are embedded here for the same reason the
	// desktop client embeds them, and they can be ROTATED upstream: if SMS login
	// starts answering `020002`/signature errors while the session-based
	// endpoints keep working, re-extract them from a newer client build or set
	// the `account_ak` / `account_sk` settings.
	AccountAccessKeyID     = "2thryby66wxi53sk"
	AccountAccessKeySecret = "zsak6eadrbawz683wf5r3m2snrwj868r"

	// accountContentType is the Content-Type every account request is signed
	// with (`loomy-sign.ts:100`). There is deliberately no `Accept` header on the
	// account calls (`loomy-sign.ts:139-147`).
	accountContentType = "application/json"

	// accountSignScheme is the Authorization prefix (`loomy-sign.ts:140`).
	accountSignScheme = "account"
)

// queryParam is one query-string pair. The signing string keeps INSERTION order
// — `loomy-sign.ts:77-84` joins an ordered entry list and never sorts it.
type queryParam struct {
	Key   string
	Value string
}

// signInput is everything the 9-segment signing string is built from.
type signInput struct {
	Method string
	// Path is the raw (unescaped) request path, for example
	// `/login/phone/sendMsgCode`.
	Path string
	// Query holds the query pairs in insertion order; nil means "no query".
	// Every Loomy account endpoint is a POST with a body, so this is empty in
	// practice, but the segment must still be produced as an empty line.
	Query []queryParam
	// Body must be the EXACT byte slice that goes on the wire
	// (`loomy-oauth.ts:84-92`, trap #4 in the porting spec): marshal once, sign
	// those bytes, send those bytes.
	Body []byte
	// ContentType is signed as segment 5 and omitted from the string (empty
	// segment) when it is not supplied (`loomy-sign.ts:100`).
	ContentType string
	// Date is signed as segment 6 and must be sent as the very same string —
	// never recomputed (`loomy-sign.ts:134,141`).
	Date time.Time
	// Nonce is signed as segment 7 (`loomy-sign.ts:130`).
	Nonce string
}

// signingString builds the 9-segment canonical string.
//
//	1 METHOD                           5 Content-Type
//	2 escaped path                     6 Date
//	3 escaped query                    7 Nonce
//	4 Content-MD5                      8 SignedHeaders   (always '')
//	                                   9 CanonicalizedHeaders (always '')
//
// Segments 8 and 9 are always empty, so the string ALWAYS ends with two
// newlines — an explicit "do not trim" warning sits at `loomy-sign.ts:14-16`.
// Trimming it (or joining 7 segments) produces a valid-looking signature that the
// server rejects.
func signingString(in signInput) string {
	return strings.Join([]string{
		strings.ToUpper(in.Method),
		escapedPath(in.Path),
		escapedQuery(in.Query),
		contentMD5(in.Body),
		in.ContentType,
		signDate(in.Date),
		in.Nonce,
		"",
		"",
	}, "\n")
}

// signDate renders the Date header/signature segment.
//
// ⚠️ `http.TimeFormat` is `"Mon, 02 Jan 2006 15:04:05 GMT"`. `time.RFC1123` on a
// UTC time prints `UTC` instead of `GMT`, and a signature built over `... UTC`
// never verifies (porting trap #9).
func signDate(date time.Time) string {
	if date.IsZero() {
		date = time.Now()
	}
	return date.UTC().Format(http.TimeFormat)
}

// contentMD5 is segment 4: base64(md5(body)).
//
// ⚠️ An EMPTY body contributes the empty string, not the md5 of "". The header is
// then omitted entirely (`loomy-sign.ts:38-42,145-146`), so "no Content-MD5" and
// "Content-MD5: ”" are different wire shapes and only the first one is correct.
func contentMD5(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	sum := md5.Sum(body)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// signatureOf runs HMAC-SHA1 with the secret access key over the signing string
// and base64-encodes the digest (`loomy-sign.ts:135-137`).
func signatureOf(secret, signing string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(signing))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// signRequest produces the header set for one account call.
//
// Only `Authorization`, `Date`, `Nonce` and `Content-Type` are always present;
// `Content-MD5` is added ONLY for a non-empty body (`loomy-sign.ts:139-147`).
// The Date header carries the exact string that was signed.
func signRequest(accessKeyID, accessKeySecret string, in signInput) http.Header {
	signing := signingString(in)
	signature := signatureOf(accessKeySecret, signing)

	header := http.Header{}
	header.Set("Authorization", accountSignScheme+" "+accessKeyID+":"+signature)
	header.Set("Date", signDate(in.Date))
	header.Set("Nonce", in.Nonce)
	if in.ContentType != "" {
		header.Set("Content-Type", in.ContentType)
	}
	if len(in.Body) > 0 {
		header.Set("Content-MD5", contentMD5(in.Body))
	}
	return header
}

// accountHeaders signs one account call with the instance credentials, falling
// back to the embedded pair when the settings do not override them.
func accountHeaders(cfg Config, in signInput) http.Header {
	return signRequest(cfg.accountAK(), cfg.accountSK(), in)
}

// accountAK resolves the access key id: the setting wins, otherwise the value
// extracted from the desktop client.
func (c Config) accountAK() string {
	if trimmed := strings.TrimSpace(c.AccountAK); trimmed != "" {
		return trimmed
	}
	return AccountAccessKeyID
}

// accountSK resolves the secret access key; see accountAK.
func (c Config) accountSK() string {
	if trimmed := strings.TrimSpace(c.AccountSK); trimmed != "" {
		return trimmed
	}
	return AccountAccessKeySecret
}

// escapeComponent is `encodeURIComponent` plus the explicit escaping of
// `! ' ( ) *` the source adds on top (`loomy-sign.ts:50-57`).
//
// Net effect: everything outside the RFC 3986 unreserved set
// (`A-Za-z0-9-_.~`) is percent-encoded, with UPPERCASE hex digits. Go's
// `url.PathEscape` and `url.QueryEscape` both differ (the latter encodes a space
// as `+`), and a single differing octet invalidates the signature.
func escapeComponent(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		octet := value[index]
		switch {
		case octet >= 'A' && octet <= 'Z',
			octet >= 'a' && octet <= 'z',
			octet >= '0' && octet <= '9',
			octet == '-', octet == '_', octet == '.', octet == '~':
			builder.WriteByte(octet)
		default:
			builder.WriteByte('%')
			builder.WriteByte(hexDigits[octet>>4])
			builder.WriteByte(hexDigits[octet&0x0f])
		}
	}
	return builder.String()
}

// escapedPath is segment 2 (`loomy-sign.ts:65-69`): prepend `/` when missing,
// drop a SINGLE trailing `/` when the path is longer than `/`, escape every
// non-empty segment and rejoin with `/`.
func escapedPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	if len(trimmed) > 1 && strings.HasSuffix(trimmed, "/") {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	segments := strings.Split(trimmed, "/")
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			escaped = append(escaped, "")
			continue
		}
		escaped = append(escaped, escapeComponent(segment))
	}
	return strings.Join(escaped, "/")
}

// escapedQuery is segment 3 (`loomy-sign.ts:77-84`): `key=value` pairs joined by
// `&` in insertion order, both sides escaped.
func escapedQuery(params []queryParam) string {
	if len(params) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(params))
	for _, param := range params {
		pairs = append(pairs, escapeComponent(param.Key)+"="+escapeComponent(param.Value))
	}
	return strings.Join(pairs, "&")
}
