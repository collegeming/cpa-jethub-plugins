package main

import (
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestRefreshKeepsTheCredentialsOwnAuthFileName pins Bug A: a token refresh must
// update the credential the host asked us to renew, never mint a second file.
//
// The refreshed STS credential carries a NEW access key id (the STS rotates it
// on every exchange), and the live CodeArts credentials carry no user name or
// domain id, so `defaultAuthFileName` — the name a brand-new login would use —
// points at a different file than the one being refreshed. The name the host
// hands over has to win.
func TestRefreshKeepsTheCredentialsOwnAuthFileName(t *testing.T) {
	refreshed := &Credential{
		Type:            ProviderKey,
		AccessKeyID:     "HSTAVFVPPZRCVTMS4TVV",
		SecretAccessKey: "secret",
		SecurityToken:   "token",
		ExpiresAt:       "2026-09-26T13:42:47.131Z",
		RefreshToken:    "refresh",
		CodeVerifier:    "verifier",
		DpopPrivateKeyJWK: &DpopPrivateJWK{
			Kty: "EC", Crv: "P-256", X: "x", Y: "y", D: "d",
		},
	}
	existing := "codearts-HSTAANV6KVKBXL9FKENJ.json"
	if derived := defaultAuthFileName(refreshed); derived == existing {
		t.Fatalf("the credential identity did not rotate (%q); the test no longer covers the bug", derived)
	}

	name := authNameForHost(existing, "", "", refreshed)
	if name != existing {
		t.Fatalf("authNameForHost = %q, want the refreshed credential's own file %q", name, existing)
	}
	auth, errAuth := authDataFor(refreshed, name)
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
	refreshed := &Credential{
		Type: ProviderKey, AccessKeyID: "HSTANEWKEY", SecretAccessKey: "secret",
		SecurityToken: "token", ExpiresAt: "2026-09-26T13:42:47.131Z",
	}
	request := pluginapi.AuthRefreshRequest{
		Attributes: map[string]string{
			"path":   "/root/.cli-proxy-api/codearts-HSTAANV6KVKBXL9FKENJ.json",
			"source": "/root/.cli-proxy-api/codearts-HSTAANV6KVKBXL9FKENJ.json",
		},
	}
	name := authNameForHost(request.AuthID, request.Attributes["path"], request.Attributes["source"], refreshed)
	if name != "codearts-HSTAANV6KVKBXL9FKENJ.json" {
		t.Fatalf("authNameForHost = %q, want the file named by the path attribute", name)
	}
}

// The inverse guarantee: a brand-new login (the host knows no name yet) still
// derives one, and two accounts still land in two files.
func TestNewLoginStillDerivesDistinctNames(t *testing.T) {
	first := authNameForHost("", "", "", &Credential{Type: ProviderKey, AccessKeyID: "AKALICE", SecretAccessKey: "s"})
	second := authNameForHost("", "", "", &Credential{Type: ProviderKey, AccessKeyID: "AKBOB", SecretAccessKey: "s"})
	if first == second {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", first)
	}
	if first != "codearts-AKALICE.json" || second != "codearts-AKBOB.json" {
		t.Fatalf("derived names = %q, %q", first, second)
	}
}

// TestLoginTargetNameOnlyAcceptsOwnAccounts covers the account a re-login names.
//
// 重新登录 must land in the account's own file, while 新建账号 (and a bare login
// page) names no account at all and keeps deriving a fresh name. A page that
// names something this plugin does not own — another provider's auth file, a
// stale index — is ignored rather than obeyed: host.auth.save would happily
// write that path.
func TestLoginTargetNameOnlyAcceptsOwnAccounts(t *testing.T) {
	host := stubAuthList(t,
		pluginapi.HostAuthFileEntry{Provider: ProviderKey, AuthIndex: "idx-1", Name: "codearts-HSTAANV6KVKBXL9FKENJ.json"},
		pluginapi.HostAuthFileEntry{Provider: ProviderKey, AuthIndex: "idx-2", Name: "codearts-HSTAVFVPPZRCVTMS4TVV.json"},
	)
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"status page names the account by index", "auth_index=idx-1", "codearts-HSTAANV6KVKBXL9FKENJ.json"},
		{"poll page names it by file", "target=codearts-HSTAVFVPPZRCVTMS4TVV.json", "codearts-HSTAVFVPPZRCVTMS4TVV.json"},
		{"add account never reuses a file", "add=1&auth_index=idx-1", ""},
		{"a bare login page has no target", "", ""},
		{"an unknown index is ignored", "auth_index=idx-9", ""},
		{"another provider's file is ignored", "target=codex-user@example.com.json", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query, errQuery := url.ParseQuery(test.query)
			if errQuery != nil {
				t.Fatalf("ParseQuery(%q): %v", test.query, errQuery)
			}
			request := pluginapi.ManagementRequest{Query: query}
			if got := loginTargetName(host, request); got != test.want {
				t.Fatalf("loginTargetName(%q) = %q, want %q", test.query, got, test.want)
			}
		})
	}
}

// The finished sign-in must be written to the named account's file — that is the
// whole point of naming it — and only a new account derives a name of its own.
func TestPollKeepsTheTargetFileOfARelogin(t *testing.T) {
	refreshed := &Credential{
		Type: ProviderKey, AccessKeyID: "HSTANEWACCESSKEY", SecretAccessKey: "secret",
		SecurityToken: "token", ExpiresAt: "2026-09-26T13:42:47.131Z",
	}
	existing := "codearts-HSTAANV6KVKBXL9FKENJ.json"
	auth, errAuth := loginPollAuth(refreshed, pluginapi.AuthLoginPollRequest{
		Metadata: map[string]any{"target": existing},
	})
	if errAuth != nil {
		t.Fatalf("loginPollAuth: %v", errAuth)
	}
	if auth.FileName != existing || auth.ID != existing {
		t.Fatalf("re-login AuthData = {ID: %q, FileName: %q}, want both %q", auth.ID, auth.FileName, existing)
	}

	fresh, errFresh := loginPollAuth(refreshed, pluginapi.AuthLoginPollRequest{})
	if errFresh != nil {
		t.Fatalf("loginPollAuth: %v", errFresh)
	}
	if fresh.FileName != "codearts-HSTANEWACCESSKEY.json" {
		t.Fatalf("new-account AuthData.FileName = %q, want a freshly derived name", fresh.FileName)
	}
}
