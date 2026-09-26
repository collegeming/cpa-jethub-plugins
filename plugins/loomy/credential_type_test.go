package main

import (
	"encoding/json"
	"testing"
)

// CPA decides which provider an auth file belongs to by reading the TOP-LEVEL
// `type` field of the credential JSON (CLIProxyAPI v7.3.17
// internal/pluginhost/auth_callbacks.go:184 for the auth-files listing used by
// the status pages, and :324 for the auth load). A file without it is reported
// as "unknown" — or claimed by whichever other plugin parses it first — so a
// successful login looks like the plugin never received the account.
//
// These tests pin both halves of the contract:
//
//  1. the credential handed to `host.auth.save` carries `type` == ProviderKey;
//  2. the field survives auth.parse -> auth.refresh, i.e. re-encoding a parsed
//     credential keeps it and repairs a credential written without it.

func TestAuthDataCarriesProviderType(t *testing.T) {
	auth, errAuth := authDataFor(sampleCredential(t), "")
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

// TestEncodeRepairsMissingProviderType covers the files already on disk: an auth
// file written before the field existed, or imported from Jet-Hub, has no
// `type`. The next save must add it instead of preserving the broken state.
func TestEncodeRepairsMissingProviderType(t *testing.T) {
	legacy := []byte(`{"access_token":"0123456789abcdef0123456789abcdef","userid":"123456789012345678","phone":"13800138000"}`)
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

// TestProviderTypeSurvivesTimestampCoercion exercises the coercing decoder. The
// host re-serialises a numeric-looking timestamp as a JSON number, and Loomy
// additionally DROPS an unusable expires_at; both rewrites re-encode the whole
// document, so the discriminator has to survive them.
func TestProviderTypeSurvivesTimestampCoercion(t *testing.T) {
	numeric := []byte(`{"type":"` + ProviderKey + `","access_token":"0123456789abcdef0123456789abcdef","userid":"123456789012345678","expires_at":1799999999000}`)
	parsed, errParse := ParseCredential(numeric)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if parsed.Type != ProviderKey {
		t.Fatalf("type after coercion = %q, want %q", parsed.Type, ProviderKey)
	}
	if parsed.ExpiresAt != "1799999999000" {
		t.Fatalf("coercion did not rewrite expires_at: %q", parsed.ExpiresAt)
	}

	dropped := []byte(`{"type":"` + ProviderKey + `","access_token":"0123456789abcdef0123456789abcdef","userid":"123456789012345678","expires_at":true}`)
	parsedDropped, errParseDropped := ParseCredential(dropped)
	if errParseDropped != nil {
		t.Fatalf("ParseCredential (dropped field): %v", errParseDropped)
	}
	if parsedDropped.Type != ProviderKey {
		t.Fatalf("type after drop = %q, want %q", parsedDropped.Type, ProviderKey)
	}
	if parsedDropped.ExpiresAt != "" {
		t.Fatalf("a non-string expires_at should be dropped, got %q", parsedDropped.ExpiresAt)
	}

	encoded, errEncode := parsedDropped.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	assertTopLevelType(t, encoded)
}

// assertTopLevelType fails unless raw is a JSON object whose top-level `type` is
// exactly the provider key this plugin registered.
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
