package main

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// Credential and expiry behaviour. The JWT fallback and the deliberate
// pre-refresh window are the two rules most likely to be "simplified" away, so
// both are pinned here.

// TestExpiryPrefersExpiresAtThenJWT pins the resolution order
// (`raccoon.ts:142-150`).
func TestExpiryPrefersExpiresAtThenJWT(t *testing.T) {
	explicit := time.Now().Add(90 * time.Minute).Truncate(time.Millisecond)
	jwtExpiry := time.Now().Add(3 * time.Hour).Truncate(time.Millisecond)

	withBoth := &Credential{AccessToken: fakeJWT(t, jwtExpiry), ExpiresAt: strconv.FormatInt(explicit.UnixMilli(), 10)}
	if got := withBoth.Expiry(); !got.Equal(explicit) {
		t.Errorf("expiry = %v, want the explicit expires_at %v", got, explicit)
	}

	// An unusable expires_at must fall through to the JWT, which is what makes
	// refresh fire for old or hand-imported credentials (trap #17). The JWT `exp`
	// has second precision, so the comparison is on unix seconds.
	for _, broken := range []string{"", "0", "-5", "not-a-number"} {
		fallback := &Credential{AccessToken: fakeJWT(t, jwtExpiry), ExpiresAt: broken}
		got := fallback.Expiry()
		if got.Unix() != jwtExpiry.Unix() {
			t.Errorf("expires_at=%q: expiry = %v, want the JWT's %v", broken, got, jwtExpiry)
		}
	}
}

// TestExpiryUnknownIsNotExpired pins "no expiry means try it anyway"
// (`raccoon.ts:152-162`).
func TestExpiryUnknownIsNotExpired(t *testing.T) {
	credential := &Credential{AccessToken: "not-a-jwt"}
	if !credential.Expiry().IsZero() {
		t.Fatalf("expiry = %v, want the zero time", credential.Expiry())
	}
	if credential.Expired(time.Now()) {
		t.Error("a credential with no resolvable expiry must NOT be reported expired")
	}
	if credential.NeedsRefresh(time.Now(), 5*time.Minute) {
		t.Error("a credential with no resolvable expiry must NOT report a refresh need")
	}
}

// TestNeedsRefreshImplementsThePreWindow pins the deliberate 300 s behaviour: the
// reference declares the constant but never uses it (trap #12), so this port is
// the only place it takes effect.
func TestNeedsRefreshImplementsThePreWindow(t *testing.T) {
	now := time.Now()
	window := time.Duration(RefreshWindowSeconds) * time.Second

	inside := &Credential{AccessToken: fakeJWT(t, now.Add(4*time.Minute))}
	if !inside.NeedsRefresh(now, window) {
		t.Errorf("a credential expiring in 4 minutes must need a refresh inside the %s window", window)
	}
	outside := &Credential{AccessToken: fakeJWT(t, now.Add(6*time.Minute))}
	if outside.NeedsRefresh(now, window) {
		t.Error("a credential expiring in 6 minutes must NOT need a refresh yet")
	}
	if outside.Expired(now) {
		t.Error("a credential expiring in 6 minutes is not expired")
	}
	// The window never fires on an expiry that only just passed: Expired and
	// NeedsRefresh answer different questions.
	past := &Credential{AccessToken: fakeJWT(t, now.Add(-time.Minute))}
	if !past.Expired(now) || !past.NeedsRefresh(now, window) {
		t.Error("an expired credential must be both expired and in need of a refresh")
	}
}

// TestJWTExpiryRejectsGarbage keeps a malformed token from producing a bogus
// timestamp.
func TestJWTExpiryRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"only-one-part",
		"two.parts",
		"a.!!!not-base64!!!.c",
		"a." + encodeSegment(`{"exp":"soon"}`) + ".c",
		"a." + encodeSegment(`{"exp":-1}`) + ".c",
		"a." + encodeSegment(`{"exp":0}`) + ".c",
	}
	for _, token := range cases {
		if got := jwtExpiryMS(token); got != 0 {
			t.Errorf("jwtExpiryMS(%q) = %d, want 0", token, got)
		}
	}
}

// encodeSegment base64url-encodes a JWT payload segment.
func encodeSegment(payload string) string {
	return base64URLEncode([]byte(payload))
}

// TestCredentialJSONToleratesNumericExpiresAt covers the host re-serialisation
// trap: a numeric-looking string comes back as a JSON number and must not break
// the decode.
func TestCredentialJSONToleratesNumericExpiresAt(t *testing.T) {
	raw := []byte(`{"access_token":"tok","refresh_token":"r","expires_at":1768000000000,"type":"raccoon"}`)
	credential, errParse := ParseCredential(raw)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if credential.ExpiresAt != "1768000000000" {
		t.Errorf("expires_at = %q, want the numeric value coerced to text", credential.ExpiresAt)
	}
	// A structural value is dropped rather than failing the whole credential —
	// the JWT fallback still resolves the expiry.
	broken := []byte(`{"access_token":"tok","expires_at":{"nested":true}}`)
	credential, errParse = ParseCredential(broken)
	if errParse != nil {
		t.Fatalf("parse with a structural expires_at: %v", errParse)
	}
	if credential.ExpiresAt != "" {
		t.Errorf("expires_at = %q, want it dropped", credential.ExpiresAt)
	}
}

// TestParseCredentialRejectsForeignPayloads pins the lenient "not one of ours"
// contract (`raccoon-auth.ts:106-116`).
func TestParseCredentialRejectsForeignPayloads(t *testing.T) {
	for _, raw := range []string{
		``,
		`not json`,
		`{}`,
		`{"refresh_token":"r"}`,
		`{"access_token":123}`,
		`{"access_token":""}`,
		`[]`,
	} {
		if _, errParse := ParseCredential([]byte(raw)); errParse == nil {
			t.Errorf("ParseCredential(%q) succeeded, want an error", raw)
		}
	}
}

// TestDisplayLabelDisambiguatesWithPhoneLast4 pins trap #18: the server name is
// auto-generated and collides, so the phone suffix is what tells accounts apart,
// and it must not be appended twice.
func TestDisplayLabelDisambiguatesWithPhoneLast4(t *testing.T) {
	credential := &Credential{Nickname: "RaccoonAva", Phone: "13800138000"}
	if got := credential.displayLabel(); got != "RaccoonAva (8000)" {
		t.Errorf("label = %q, want %q", got, "RaccoonAva (8000)")
	}
	// Re-rendering is idempotent (`jet-hub-rpc.ts:255-257`).
	credential.Nickname = credential.displayLabel()
	if got := credential.displayLabel(); got != "RaccoonAva (8000)" {
		t.Errorf("label is not idempotent: %q", got)
	}
	noPhone := &Credential{Nickname: "RaccoonBob"}
	if got := noPhone.displayLabel(); got != "RaccoonBob" {
		t.Errorf("label = %q, want the bare nickname", got)
	}
	onlyPhone := &Credential{Phone: "13800138000"}
	if got := onlyPhone.displayLabel(); got != "Raccoon 8000" {
		t.Errorf("label = %q, want a phone-based fallback", got)
	}
	anonymous := &Credential{}
	if got := anonymous.displayLabel(); got != ProviderKey {
		t.Errorf("label = %q, want %q", got, ProviderKey)
	}
}

// TestBuildCredentialOmitsUndecodableExpiry pins the "omit rather than invent"
// rule (`raccoon-oauth.ts:245,250`).
func TestBuildCredentialOmitsUndecodableExpiry(t *testing.T) {
	expiry := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	credential := buildCredential(fakeJWT(t, expiry), "refresh", "personal")
	if credential.ExpiresAt != strconv.FormatInt(expiry.UnixMilli(), 10) {
		t.Errorf("expires_at = %q, want %d", credential.ExpiresAt, expiry.UnixMilli())
	}
	opaque := buildCredential("opaque-token", "", "")
	if opaque.ExpiresAt != "" {
		t.Errorf("expires_at = %q, want it omitted for an undecodable token", opaque.ExpiresAt)
	}
	if opaque.Refreshable() {
		t.Error("an empty refresh_token must not report refreshable")
	}
	// The auth file name must be derived from something that does NOT rotate.
	credential.UserID = "user-alice"
	named := defaultAuthFileName(credential)
	if named != ProviderKey+"-user-alice.json" {
		t.Errorf("derived file name = %q", named)
	}
	unnamed := buildCredential("opaque-token", "", "")
	unnamed.UserID = ""
	unnamed.Nickname = ""
	if got := defaultAuthFileName(unnamed); got != ProviderKey+"-account.json" {
		t.Errorf("derived file name for an anonymous credential = %q", got)
	}
}

// TestEncodeStampsProviderType pins the credential `type` marker, which is what
// makes the auth file recognisable to `auth.parse` and the account pool.
func TestEncodeStampsProviderType(t *testing.T) {
	credential := &Credential{AccessToken: "tok"}
	raw, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded["type"] != ProviderKey {
		t.Errorf("type = %v, want %q", decoded["type"], ProviderKey)
	}
	if _, present := decoded["refresh_token"]; !present {
		t.Error("refresh_token must be written even when empty: its absence is not the same as an empty string")
	}
}
