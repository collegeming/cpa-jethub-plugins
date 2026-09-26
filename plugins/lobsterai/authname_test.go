package main

import "testing"

// TestRefreshKeepsTheCredentialsOwnAuthFileName pins Bug A: a token refresh must
// update the credential the host asked us to renew, never mint a second file.
//
// A credential without a uid, a user id and a nickname derives the shared
// constant "account"; if a refresh used that name it would collect every such
// account into one file. Only the name the host hands over keeps each credential
// where it is.
func TestRefreshKeepsTheCredentialsOwnAuthFileName(t *testing.T) {
	existing := "lobsterai-101989.json"
	refreshed := &Credential{Type: ProviderKey, AccessToken: "new-token", RefreshToken: "r"}
	if derived := defaultAuthFileName(refreshed); derived == existing {
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
	refreshed := &Credential{Type: ProviderKey, AccessToken: "tok", UID: "101989"}
	got := authNameForHost("", "/root/.cli-proxy-api/lobsterai-101989.json", "", refreshed)
	if got != "lobsterai-101989.json" {
		t.Fatalf("authNameForHost = %q, want the file named by the path attribute", got)
	}
}

// The inverse guarantee: a brand-new login (the host knows no name yet) still
// derives one, and two accounts still land in two files.
func TestNewLoginStillDerivesDistinctNames(t *testing.T) {
	first := authNameForHost("", "", "", &Credential{Type: ProviderKey, UID: "101989"})
	second := authNameForHost("", "", "", &Credential{Type: ProviderKey, UID: "202989"})
	if first == second {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", first)
	}
}
