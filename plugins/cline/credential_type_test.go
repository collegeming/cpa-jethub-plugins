package main

import (
	"encoding/json"
	"testing"
	"time"
)

// CPA decides which provider an auth file belongs to by reading the TOP-LEVEL
// `type` field of the credential JSON (CLIProxyAPI v7.3.17
// internal/pluginhost/auth_callbacks.go:184 for the auth-files listing used by
// the status pages, and :324 for the auth load). A file without it is reported
// as "unknown" — or claimed by whichever other plugin parses it first, which is
// how a CodeBuddy credential was once counted as a Cline account — so a
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
	legacy := []byte(`{"access_token":"workos:cline-access-token","refresh_token":"cline-refresh-token","expire_time":1799999999000,"account_id":"account-1","email":"tester@example.com"}`)
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
		AccessToken:  "workos:cline-access-token",
		RefreshToken: "cline-refresh-token",
		ExpireTime:   time.Now().Add(24 * time.Hour).UnixMilli(),
		AccountID:    "account-1",
		Email:        "tester@example.com",
		Nickname:     "tester",
	}
}

// TestProviderTypeSurvivesTimestampCoercion exercises the coercing decoder. A
// third-party or hand-written file can carry `expire_time` as a QUOTED number,
// which UnmarshalJSON rewrites by re-encoding the whole document; the
// discriminator has to come out of that rewrite intact.
func TestProviderTypeSurvivesTimestampCoercion(t *testing.T) {
	raw := []byte(`{"type":"` + ProviderKey + `","access_token":"workos:cline-access-token","refresh_token":"cline-refresh-token","expire_time":"1799999999000","account_id":"account-1"}`)
	parsed, errParse := ParseCredential(raw)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if parsed.Type != ProviderKey {
		t.Fatalf("type after coercion = %q, want %q", parsed.Type, ProviderKey)
	}
	if parsed.ExpireTime != 1799999999000 {
		t.Fatalf("coercion did not rewrite expire_time: %d", parsed.ExpireTime)
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
