package main

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
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

// TestEncodeRepairsMissingProviderType covers the files already on disk: an auth
// file written before the field existed, or imported from Jet-Hub, has no
// `type`. The next save must add it instead of preserving the broken state.
func TestEncodeRepairsMissingProviderType(t *testing.T) {
	legacy := []byte(`{"access_token":"trae-access-token","refresh_token":"trae-refresh-token","expires_at":"1799999999000","uid":"user-1","machine_id":"0123456789abcdef0123456789abcdef","device_id":"fedcba9876543210fedcba9876543210"}`)
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
		AccessToken:  "trae-access-token",
		RefreshToken: "trae-refresh-token",
		ExpiresAt:    ExpiresAtValue(strconv.FormatInt(time.Now().Add(24*time.Hour).UnixMilli(), 10)),
		UID:          "user-1",
		Nickname:     "tester",
		MachineID:    "0123456789abcdef0123456789abcdef",
		DeviceID:     "fedcba9876543210fedcba9876543210",
	}
}

// TestProviderTypeSurvivesTimestampCoercion exercises the tolerant timestamp
// decoder. The host re-serialises a numeric-looking `expires_at` as a JSON
// number; the credential must still decode, and the discriminator must survive.
func TestProviderTypeSurvivesTimestampCoercion(t *testing.T) {
	raw := []byte(`{"type":"` + ProviderKey + `","access_token":"trae-access-token","refresh_token":"trae-refresh-token","expires_at":1799999999000,"uid":"user-1","machine_id":"0123456789abcdef0123456789abcdef","device_id":"fedcba9876543210fedcba9876543210"}`)
	parsed, errParse := ParseCredential(raw)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if parsed.Type != ProviderKey {
		t.Fatalf("type after decoding a numeric expiry = %q, want %q", parsed.Type, ProviderKey)
	}
	if parsed.ExpiresAt.String() != "1799999999000" {
		t.Fatalf("expires_at = %q, want the numeric value normalised to \"1799999999000\"", parsed.ExpiresAt)
	}
	encoded, errEncode := parsed.Encode()
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
