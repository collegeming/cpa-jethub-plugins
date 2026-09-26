package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The credential round trip keeps the field names Jet-Hub uses, and in
// particular `access_token`, which is the account-pool identity field for every
// provider except `codearts` (trap #20).
func TestCredentialRoundTripKeepsIdentityField(t *testing.T) {
	credential := buildCredential("0123456789abcdef0123456789abcdef", "123456789012345678", "13800138000", "", DefaultConfig(), time.Now())
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	if !strings.Contains(string(storage), `"access_token":"0123456789abcdef0123456789abcdef"`) {
		t.Fatalf("storage = %s, want the access_token field name", storage)
	}
	if !strings.Contains(string(storage), `"userid":"123456789012345678"`) {
		t.Fatalf("storage = %s, want the userid field name", storage)
	}

	parsed, errParse := ParseCredential(storage)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if parsed.Session() != "0123456789abcdef0123456789abcdef" || parsed.UserID != "123456789012345678" {
		t.Fatalf("parsed = %#v, want the same session and user id", parsed)
	}
	if parsed.Type != ProviderKey {
		t.Fatalf("type = %q, want %q", parsed.Type, ProviderKey)
	}
}

// `expires_at` must be written as a STRING millisecond timestamp (trap #19).
func TestBuildCredentialStoresExpiresAtAsString(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	credential := buildCredential("session", "userid", "13800138000", "", cfg, now)
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	if !strings.Contains(string(storage), `"expires_at":"`) {
		t.Fatalf("storage = %s, want expires_at to be a quoted string", storage)
	}
	want := now.Add(time.Duration(SessionTTLSeconds) * time.Second).UnixMilli()
	if got := credential.ExpiresAtMS(); got != want {
		t.Fatalf("expires_at = %d, want now + %ds = %d", got, SessionTTLSeconds, want)
	}
}

// The host rewrites a numeric-looking string into a JSON number, so the decoder
// must coerce it back — otherwise the provider publishes no models at all.
func TestCredentialAcceptsNumericExpiresAt(t *testing.T) {
	storage := []byte(`{"access_token":"0123456789abcdef0123456789abcdef","userid":"1","phone":"13800138000","expires_at":1790000000000}`)
	credential, errParse := ParseCredential(storage)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if got := credential.ExpiresAtMS(); got != 1790000000000 {
		t.Fatalf("expires_at = %d, want the coerced 1790000000000", got)
	}
	if credential.Expiry().IsZero() {
		t.Fatal("a coerced numeric timestamp must still produce an expiry time")
	}
}

// Absent / unusable `expires_at` counts as ABSENT: it must not expire the
// credential, because the server's 100002 is the only authority (trap #17).
func TestExpiredTreatsMissingExpiryAsValid(t *testing.T) {
	cases := map[string]string{
		"missing":     ``,
		"empty":       `"expires_at":""`,
		"zero":        `"expires_at":"0"`,
		"negative":    `"expires_at":"-5"`,
		"non-numeric": `"expires_at":"soon"`,
		"object":      `"expires_at":{"at":1}`,
	}
	for label, extra := range cases {
		body := `{"access_token":"s"`
		if extra != "" {
			body += "," + extra
		}
		body += "}"
		credential, errParse := ParseCredential([]byte(body))
		if errParse != nil {
			t.Fatalf("%s: parse: %v", label, errParse)
		}
		if credential.ExpiresAtMS() != 0 {
			t.Errorf("%s: expires_at = %d, want 0 (absent)", label, credential.ExpiresAtMS())
		}
		if credential.Expired(time.Now()) {
			t.Errorf("%s: a credential without a usable expiry must not be reported expired", label)
		}
	}
}

// A real expiry does expire the credential locally.
func TestExpiredUsesLocalTimestamp(t *testing.T) {
	now := time.Now()
	past := buildCredential("s", "u", "13800138000", "", DefaultConfig(), now.Add(-2*time.Hour))
	// Force the expiry into the past: the builder always writes now+TTL.
	past.ExpiresAt = "1000"
	if !past.Expired(now) {
		t.Fatal("a past expiry must be reported expired")
	}
	future := buildCredential("s", "u", "13800138000", "", DefaultConfig(), now)
	if future.Expired(now) {
		t.Fatal("a fresh credential must not be reported expired")
	}
}

// ParseCredential accepts any object with a string access_token and rejects
// everything else (`loomy-auth.ts:99-108`).
func TestParseCredentialValidation(t *testing.T) {
	if _, errParse := ParseCredential(nil); errParse == nil {
		t.Fatal("an empty payload must be rejected")
	}
	if _, errParse := ParseCredential([]byte(`{"userid":"1"}`)); errParse == nil {
		t.Fatal("a credential without access_token must be rejected")
	}
	if _, errParse := ParseCredential([]byte(`{"access_token":"   "}`)); errParse == nil {
		t.Fatal("a blank access_token must be rejected")
	}
	if _, errParse := ParseCredential([]byte(`not json`)); errParse == nil {
		t.Fatal("a non-JSON payload must be rejected")
	}
}

// The phone gate is the source's own pattern applied to a trimmed string
// (`jet-hub-rpc.ts:1124-1127`), and normalisation strips what users paste.
func TestPhoneNormalisationAndGate(t *testing.T) {
	cases := map[string]string{
		"13800138000":       "13800138000",
		" 138 0013 8000 ":   "13800138000",
		"+86 138-0013-8000": "13800138000",
		"8613800138000":     "13800138000",
		"08613800138000":    "13800138000",
		"":                  "",
	}
	for input, want := range cases {
		if got := normalizePhone(input); got != want {
			t.Errorf("normalizePhone(%q) = %q, want %q", input, got, want)
		}
	}
	for _, valid := range []string{"13800138000", "19912345678", "15000000000"} {
		if !validPhone(valid) {
			t.Errorf("validPhone(%q) = false, want true", valid)
		}
	}
	for _, invalid := range []string{"", "12800138000", "1380013800", "138001380000", "010-12345678"} {
		if validPhone(invalid) {
			t.Errorf("validPhone(%q) = true, want false", invalid)
		}
	}
}

// The account label tolerates an empty phone: a WeChat `bind/skip` credential
// legitimately has none (`loomy-wechat-login.ts:246`, trap #23).
func TestDisplayLabelToleratesEmptyPhone(t *testing.T) {
	credential := &Credential{AccessToken: "0123456789abcdef", UserID: "123456789012345678"}
	if got := credential.displayLabel(); got != "123456789012345678" {
		t.Fatalf("displayLabel = %q, want the user id when the phone is empty", got)
	}
	withPhone := &Credential{AccessToken: "s", UserID: "1", Phone: "13800138000"}
	if got := withPhone.displayLabel(); got != "13800138000" {
		t.Fatalf("displayLabel = %q, want the phone when it is bound", got)
	}
	if got := withPhone.maskedPhone(); got != "138****8000" {
		t.Fatalf("maskedPhone = %q, want the masked form", got)
	}
	if got := (&Credential{}).maskedPhone(); got != "未绑定" {
		t.Fatalf("maskedPhone = %q, want 未绑定", got)
	}
}

// The auth file name is deterministic so a host-side re-save overwrites instead
// of duplicating the account.
func TestDefaultAuthFileNameIsDeterministic(t *testing.T) {
	credential := &Credential{AccessToken: "s", UserID: "123", Phone: "13800138000"}
	first := defaultAuthFileName(credential)
	second := defaultAuthFileName(credential)
	if first != second || !strings.HasSuffix(first, ".json") || !strings.HasPrefix(first, ProviderKey+"-") {
		t.Fatalf("file name = %q / %q, want a stable loomy-*.json", first, second)
	}
	weird := &Credential{AccessToken: "s", UserID: "../../etc/passwd"}
	if got := defaultAuthFileName(weird); strings.ContainsAny(got, `/\`) {
		t.Fatalf("file name = %q, want path separators stripped", got)
	}
}

// authDataFor exposes the identity the host and the account pool need, and never
// the reserved `api_key` attribute.
func TestAuthDataForShape(t *testing.T) {
	credential := sampleCredential(t)
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		t.Fatalf("authDataFor: %v", errAuth)
	}
	if auth.Provider != ProviderKey || auth.FileName == "" || auth.ID != auth.FileName {
		t.Fatalf("auth = %#v, want a named Loomy record", auth)
	}
	if _, reserved := auth.Attributes["api_key"]; reserved {
		t.Fatal(`the "api_key" attribute is reserved by the host and must not be used`)
	}
	if auth.Attributes["refreshable"] != "false" {
		t.Fatalf("refreshable = %q, want false: there is no refresh token", auth.Attributes["refreshable"])
	}
	var decoded Credential
	if errUnmarshal := json.Unmarshal(auth.StorageJSON, &decoded); errUnmarshal != nil {
		t.Fatalf("storage json: %v", errUnmarshal)
	}
	if decoded.Session() != credential.Session() {
		t.Fatalf("storage session = %q, want %q", decoded.Session(), credential.Session())
	}
	// A local expiry exists, so a probe is scheduled before it.
	if auth.NextRefreshAfter.IsZero() {
		t.Fatal("a credential with a local expiry must carry a next-probe time")
	}
}
