package main

import (
	"encoding/json"
	"testing"
)

// CPA decides which provider an auth file belongs to by reading the TOP-LEVEL
// `type` field of the credential JSON (CLIProxyAPI v7.3.17
// internal/pluginhost/auth_callbacks.go:184 for the auth-files listing used by
// the status pages, and :324 for the auth load). A file without it is reported
// as "unknown" — or claimed by whichever other plugin parses it first, which is
// exactly how a CodeBuddy credential ended up attributed to the Cline plugin —
// so a successful login looks like "尚未添加账号" on the plugin's own page.
//
// These tests pin both halves of the contract:
//
//  1. the credential handed to `host.auth.save` carries `type` == ProviderKey;
//  2. the field survives auth.parse -> auth.refresh, i.e. re-encoding a parsed
//     credential keeps it and repairs a credential written without it.

func TestAuthDataCarriesProviderType(t *testing.T) {
	auth, errAuth := authDataFor(providerTypeCredential(), "")
	if errAuth != nil {
		t.Fatalf("authDataFor: %v", errAuth)
	}
	if auth.Provider != ProviderKey {
		t.Fatalf("AuthData.Provider = %q, want %q", auth.Provider, ProviderKey)
	}
	assertTopLevelType(t, auth.StorageJSON)

	parsed, errParse := ParseCredential(auth.StorageJSON)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if parsed.Type != ProviderKey {
		t.Fatalf("auth.parse round trip: type = %q, want %q", parsed.Type, ProviderKey)
	}

	// auth.refresh re-encodes the credential it parsed; the discriminator must
	// not be dropped on the way back out.
	reencoded, errEncode := parsed.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	assertTopLevelType(t, reencoded)
}

// TestCredentialTypeFollowsInjectedProviderKey is the variant guard.
//
// One source tree builds FOUR plugins (codebuddy / codebuddy-intl /
// workbuddy-cn / workbuddy); scripts/build.sh injects the identity with
// `-ldflags -X main.ProviderKey=...`. If the auth file were stamped with a
// literal, all four artifacts would write "codebuddy" and the host would hand
// every WorkBuddy credential back to the CodeBuddy plugin. Overwriting
// ProviderKey with a value no literal could contain proves the emitted value
// comes from the injected var.
func TestCredentialTypeFollowsInjectedProviderKey(t *testing.T) {
	original := ProviderKey
	ProviderKey = "variant-probe-not-a-real-key"
	t.Cleanup(func() { ProviderKey = original })

	fresh := buildCredential(
		buddyToken{AccessToken: "buddy-access-token"},
		buddyAccount{UID: "user-1", Nickname: "tester"},
		ProductDefault(),
	)
	if fresh.Type != ProviderKey {
		t.Fatalf("buildCredential type = %q, want the injected %q", fresh.Type, ProviderKey)
	}
	encoded, errEncode := fresh.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	assertTopLevelType(t, encoded)

	auth, errAuth := authDataFor(providerTypeCredential(), "")
	if errAuth != nil {
		t.Fatalf("authDataFor: %v", errAuth)
	}
	if auth.Provider != ProviderKey {
		t.Fatalf("AuthData.Provider = %q, want the injected %q", auth.Provider, ProviderKey)
	}
	assertTopLevelType(t, auth.StorageJSON)
}

// TestEncodeRepairsMissingProviderType covers the files already on disk: an auth
// file written before the field existed, or imported from Jet-Hub, has no
// `type`. The next save must add it instead of preserving the broken state.
func TestEncodeRepairsMissingProviderType(t *testing.T) {
	legacy := []byte(`{"access_token":"buddy-access-token","refresh_token":"buddy-refresh-token","expires_at":"1799999999000","domain":"codebuddy.cn","user_id":"user-1","product":"codebuddy"}`)
	parsed, errParse := ParseCredential(legacy)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	encoded, errEncode := parsed.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	assertTopLevelType(t, encoded)
}

// providerTypeCredential is the smallest credential ParseCredential accepts.
func providerTypeCredential() *Credential {
	return &Credential{
		AccessToken:  "buddy-access-token",
		RefreshToken: "buddy-refresh-token",
		ExpiresAt:    "1799999999000",
		Domain:       "codebuddy.cn",
		UserID:       "user-1",
		Nickname:     "tester",
		Product:      ProductCodeBuddy,
	}
}

// TestProviderTypeSurvivesTimestampCoercion exercises the coercing decoder. The
// host re-serialises a numeric-looking timestamp as a JSON number, so
// UnmarshalJSON rewrites the whole document before decoding it; the
// discriminator has to come out of that rewrite intact.
func TestProviderTypeSurvivesTimestampCoercion(t *testing.T) {
	raw := []byte(`{"type":"` + ProviderKey + `","access_token":"buddy-access-token","refresh_token":"buddy-refresh-token","expires_at":1799999999000}`)
	parsed, errParse := ParseCredential(raw)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if parsed.Type != ProviderKey {
		t.Fatalf("type after coercion = %q, want %q", parsed.Type, ProviderKey)
	}
	if parsed.ExpiresAt != "1799999999000" {
		t.Fatalf("coercion did not rewrite expires_at: %q", parsed.ExpiresAt)
	}
	encoded, errEncode := parsed.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	assertTopLevelType(t, encoded)
}

// assertTopLevelType fails unless raw is a JSON object whose top-level `type` is
// exactly the provider key this artifact registered.
func assertTopLevelType(t *testing.T, raw []byte) {
	t.Helper()
	var record map[string]any
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		t.Fatalf("decode credential %s: %v", raw, errUnmarshal)
	}
	value, present := record["type"]
	if !present {
		t.Fatalf("credential %s has no top-level type field", raw)
	}
	if text, _ := value.(string); text != ProviderKey {
		t.Fatalf("credential type = %v (%T), want %q", value, value, ProviderKey)
	}
}
