package main

import "testing"

// TestRefreshKeepsTheCredentialsOwnAuthFileName pins Bug A: a token refresh must
// update the credential the host asked us to renew, never mint a second file.
//
// The refreshed credential carries a new access token, and a credential without
// a label or an account id derives its name from the first eight characters of
// that token — i.e. the derived name points at a different file on every
// refresh. Only the name the host hands over keeps the credential in place.
func TestRefreshKeepsTheCredentialsOwnAuthFileName(t *testing.T) {
	existing := "cline-user@example.com.json"
	refreshed := &Credential{Type: ProviderKey, AccessToken: "workos:new-access-token", RefreshToken: "r", ExpireTime: 1799999999000}
	if derived := defaultAuthFileName(&Credential{AccessToken: "workos:new-access-token"}); derived == existing {
		t.Fatalf("the derived name reproduced %q; the test no longer covers the bug", derived)
	}
	if got := authNameForHost(existing, "", "", refreshed); got != existing {
		t.Fatalf("authNameForHost = %q, want the refreshed credential's own file %q", got, existing)
	}
	auth, errAuth := authDataFor(refreshed, authNameForHost(existing, "", "", refreshed))
	if errAuth != nil {
		t.Fatalf("authDataFor: %v", errAuth)
	}
	if auth.FileName != existing || auth.ID != existing {
		t.Fatalf("AuthData = {ID: %q, FileName: %q}, want both %q", auth.ID, auth.FileName, existing)
	}
}

// A host that hands over the record's attributes instead of an id still pins the
// file: `path`/`source` name the file the credential was read from.
func TestRefreshFallsBackToThePathAttribute(t *testing.T) {
	refreshed := &Credential{Type: ProviderKey, AccessToken: "workos:tok"}
	got := authNameForHost("", "/root/.cli-proxy-api/cline-user@example.com.json", "", refreshed)
	if got != "cline-user@example.com.json" {
		t.Fatalf("authNameForHost = %q, want the file named by the path attribute", got)
	}
}

// The inverse guarantee: a brand-new login (the host knows no name yet) still
// derives one, and two accounts still land in two files.
func TestNewLoginStillDerivesDistinctNames(t *testing.T) {
	first := authNameForHost("", "", "", &Credential{Type: ProviderKey, Email: "alice@example.com"})
	second := authNameForHost("", "", "", &Credential{Type: ProviderKey, Email: "bob@example.com"})
	if first == second {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", first)
	}
}
