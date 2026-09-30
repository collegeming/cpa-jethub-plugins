package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestStatusPageOffersBothLoginRoutes pins the two login affordances every
// provider in this repository carries, and which this one was missing:
//
//   - 新建账号, the only way to add a SECOND account from the page. The login
//     page already implements the add-account branch
//     (`plugui.IsAddAccountRequest` → AddAccountNotice), so without the link
//     that code was unreachable from the UI.
//   - 重新登录, the only way to replace a credential. ZCode's is static — the JWT
//     has no expiry and the server exposes no refresh endpoint — so a dead
//     credential has no other route back, which is what makes the missing link a
//     dead end rather than a cosmetic gap.
func TestStatusPageOffersBothLoginRoutes(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, BalancePath):
			return httpResponse(http.StatusOK, balanceBody(1000, 2000)), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
		default:
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		}
	}
	storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())

	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/status", url.Values{}, nil))
	page := string(response.Body)
	if !strings.Contains(page, "login?add=1") {
		t.Fatalf("新建账号 is not reachable from the status page:\n%s", truncate(page, 800))
	}
	if !strings.Contains(page, "重新登录") {
		t.Fatalf("the account card offers no way to replace a dead credential:\n%s", truncate(page, 800))
	}
	// The switcher must name the account it will act on, so 重新登录 cannot be
	// mistaken for a page-wide action.
	if !strings.Contains(page, "auth_index=auth-1") {
		t.Fatalf("the login link does not name the account:\n%s", truncate(page, 800))
	}
}

// TestLoginPageExplainsTheAddAccountFlow keeps the other end of that link
// honest: a page opened with `add=1` has to say what it is doing, because the
// flow itself is identical to a first login.
func TestLoginPageExplainsTheAddAccountFlow(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())

	// Parsed from the shared constant rather than hand-written, so a change to
	// its spelling cannot silently desynchronise this test from the link the
	// status page renders.
	query, errQuery := url.ParseQuery(plugui.AddAccountQuery)
	if errQuery != nil {
		t.Fatalf("AddAccountQuery is not a query string: %v", errQuery)
	}
	response := callManagement(t, testHost(), managementRequest(
		http.MethodGet, "/v0/resource/plugins/zcode/login", query, nil))
	if !strings.Contains(string(response.Body), plugui.AddAccountNotice) {
		t.Fatal("the login page does not explain that this run adds an account")
	}
}
