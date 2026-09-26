package main

import (
	"encoding/json"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestRefreshKeepsTheCredentialsOwnAuthFileName pins Bug A: a token refresh must
// update the credential the host asked us to renew, never mint a second file.
//
// The derived name ends in a random suffix, so it can NEVER reproduce the file
// the credential already lives in: only the name the host hands over can.
func TestRefreshKeepsTheCredentialsOwnAuthFileName(t *testing.T) {
	refreshed := &Credential{
		Type:        ProviderKey,
		AccessToken: "new-access-token",
		UserID:      "u-1",
		Nickname:    "黎明文铮",
		Product:     ProductCodeBuddy,
		ExpiresAt:   "2026-09-26T13:42:47.131Z",
	}
	existing := "codebuddy-account-39cf135e.json"
	if derived := defaultAuthFileName(refreshed); derived == existing {
		t.Fatalf("the derived name reproduced %q; the test no longer covers the bug", derived)
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
	refreshed := &Credential{Type: ProviderKey, AccessToken: "tok", UserID: "u-1", Nickname: "alice"}
	request := pluginapi.AuthRefreshRequest{
		Attributes: map[string]string{
			"path":   "/root/.cli-proxy-api/codebuddy-alice.json",
			"source": "/root/.cli-proxy-api/codebuddy-alice.json",
		},
	}
	name := authNameForHost("", request.Attributes["path"], request.Attributes["source"], refreshed)
	if name != "codebuddy-alice.json" {
		t.Fatalf("authNameForHost = %q, want the file named by the path attribute", name)
	}
}

// A brand-new login (the host knows no name yet) still derives one, and two
// accounts still land in two files.
func TestNewLoginStillDerivesDistinctNames(t *testing.T) {
	first := authNameForHost("", "", "", &Credential{Type: ProviderKey, UserID: "u-1", Nickname: "alice"})
	second := authNameForHost("", "", "", &Credential{Type: ProviderKey, UserID: "u-2", Nickname: "bob"})
	if first == second {
		t.Fatalf("two accounts share the file name %q: a second login would overwrite the first account", first)
	}
}

// A runtime auth index is not a file name; the plugin must not turn it into one.
func TestRefreshRejectsARuntimeAuthIndex(t *testing.T) {
	refreshed := &Credential{Type: ProviderKey, AccessToken: "tok", UserID: "u-1", Nickname: "alice"}
	name := authNameForHost("9c1db01c89adaa0a", "", "", refreshed)
	if name != defaultAuthFileName(refreshed) {
		t.Fatalf("authNameForHost = %q, want the derived name for a non-file id", name)
	}
}

// TestExistingAuthFileNameMatchesTheAccount is what keeps a re-login from adding
// a second entry beside a credential whose file name no derivation can
// reproduce: the account's own user id finds the file the host already keeps.
func TestExistingAuthFileNameMatchesTheAccount(t *testing.T) {
	legacy := &Credential{Type: ProviderKey, AccessToken: "old", UserID: "u-legacy", Nickname: "黎明文铮"}
	raw, errEncode := legacy.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	fresh := &Credential{Type: ProviderKey, AccessToken: "new", UserID: "u-legacy", Nickname: "黎明文铮"}

	installAuthHost(t, map[string][]byte{"idx-legacy": raw},
		[]pluginapi.HostAuthFileEntry{{AuthIndex: "idx-legacy", Name: "codebuddy-account-39cf135e.json", Provider: ProviderKey}})
	host := abiboot.NewHost(json.RawMessage(`{"host_callback_id":"test-callback"}`))
	if got := existingAuthFileName(host, fresh); got != "codebuddy-account-39cf135e.json" {
		t.Fatalf("existingAuthFileName = %q, want the legacy file of the same account", got)
	}
	// A different account must not be folded into that file.
	other := &Credential{Type: ProviderKey, AccessToken: "new", UserID: "u-other"}
	if got := existingAuthFileName(host, other); got != "" {
		t.Fatalf("existingAuthFileName = %q for another account, want no match", got)
	}
}

// installAuthHost answers host.auth.list / host.auth.get from fixed data, the
// two callbacks existingAuthFileName walks. The transport is restored when the
// test ends.
func installAuthHost(t *testing.T, auths map[string][]byte, files []pluginapi.HostAuthFileEntry) {
	t.Helper()
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return abiboot.OK(map[string]any{"files": files})
		case pluginabi.MethodHostAuthGet:
			var payload pluginapi.HostAuthGetRequest
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			raw, ok := auths[payload.AuthIndex]
			if !ok {
				return nil, abiboot.Errorf("auth_not_found", "no auth %s", payload.AuthIndex)
			}
			return abiboot.OK(pluginapi.HostAuthGetResponse{AuthIndex: payload.AuthIndex, JSON: raw})
		default:
			return nil, abiboot.Errorf("unexpected_host_method", "unexpected host method %s", method)
		}
	})
	t.Cleanup(abiboot.ClearHostCaller)
}

// TestLoginPollKeepsTheFileTheAccountAlreadyHas covers the manager-driven login,
// whose poll result the host saves itself: the record's FileName decides the
// file, so returning a derived name for an account the host already holds would
// add a second entry (the live file was named by the old random scheme).
func TestLoginPollKeepsTheFileTheAccountAlreadyHas(t *testing.T) {
	legacy := &Credential{Type: ProviderKey, AccessToken: "old", UserID: "u-legacy", Nickname: "黎明文铮"}
	raw, errEncode := legacy.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	installAuthHost(t, map[string][]byte{"idx-legacy": raw},
		[]pluginapi.HostAuthFileEntry{{AuthIndex: "idx-legacy", Name: "codebuddy-account-39cf135e.json", Provider: ProviderKey}})
	host := abiboot.NewHost(json.RawMessage(`{"host_callback_id":"test-callback"}`))

	auth, errAuth := loginPollAuth(host, &Credential{Type: ProviderKey, AccessToken: "new", UserID: "u-legacy", Nickname: "黎明文铮"})
	if errAuth != nil {
		t.Fatalf("loginPollAuth: %v", errAuth)
	}
	if auth.FileName != "codebuddy-account-39cf135e.json" {
		t.Fatalf("loginPollAuth.FileName = %q, want the file the account already has", auth.FileName)
	}

	// An account the host does not know is a new account: it gets its own file.
	fresh, errFresh := loginPollAuth(host, &Credential{Type: ProviderKey, AccessToken: "new", UserID: "u-new", Nickname: "alice"})
	if errFresh != nil {
		t.Fatalf("loginPollAuth: %v", errFresh)
	}
	if fresh.FileName != "codebuddy-alice.json" {
		t.Fatalf("loginPollAuth.FileName = %q, want a derived name for a new account", fresh.FileName)
	}
}
