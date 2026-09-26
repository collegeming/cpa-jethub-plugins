package main

import "testing"

// TestProbeKeepsTheCredentialsOwnAuthFileName pins Bug A: the host's probe
// ("auth.refresh") must return the record it asked about, never a record that
// names another file.
//
// Loomy has no refresh token — the probe returns the SAME credential — so this
// test also documents the one case where a derived name would be harmless today
// and wrong the moment the credential loses its phone and user id (the fallback
// would be the shared constant "account").
func TestProbeKeepsTheCredentialsOwnAuthFileName(t *testing.T) {
	existing := "loomy-13800138000.json"
	credential := &Credential{Type: ProviderKey, AccessToken: "tok", UserID: "100000000000000001", Phone: "13800138000"}
	if got := authNameForHost(existing, "", "", credential); got != existing {
		t.Fatalf("authNameForHost = %q, want the credential's own file %q", got, existing)
	}
	auth, errAuth := authDataFor(credential, authNameForHost(existing, "", "", credential))
	if errAuth != nil {
		t.Fatalf("authDataFor: %v", errAuth)
	}
	if auth.FileName != existing || auth.ID != existing {
		t.Fatalf("AuthData = {ID: %q, FileName: %q}, want both %q", auth.ID, auth.FileName, existing)
	}
}

// A host that hands over the record's attributes instead of an id still pins the
// file: `path`/`source` name the file the credential was read from.
func TestProbeFallsBackToThePathAttribute(t *testing.T) {
	credential := &Credential{Type: ProviderKey, AccessToken: "tok", UserID: "1", Phone: "13800138000"}
	got := authNameForHost("", "/root/.cli-proxy-api/loomy-13800138000.json", "", credential)
	if got != "loomy-13800138000.json" {
		t.Fatalf("authNameForHost = %q, want the file named by the path attribute", got)
	}
}

// The inverse guarantee: a brand-new login (the host knows no name yet) still
// derives one, and two accounts still land in two files.
func TestNewLoginStillDerivesDistinctNames(t *testing.T) {
	first := authNameForHost("", "", "", &Credential{Type: ProviderKey, AccessToken: "a", UserID: "100000000000000001", Phone: "13800138000"})
	second := authNameForHost("", "", "", &Credential{Type: ProviderKey, AccessToken: "b", UserID: "100000000000000002", Phone: "13900139000"})
	if first == second {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", first)
	}
}
