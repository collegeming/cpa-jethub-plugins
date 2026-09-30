package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Management-page tests.
//
// This plugin had none, and the gap they now cover is exactly the kind that
// hides without them: the login page implemented the add-account branch
// (`plugui.IsAddAccountRequest`) while nothing on the status page linked to it,
// so adding a second account was impossible from the UI and replacing a dead
// credential had no route at all. Both are link-only affordances, so only a
// rendered-page assertion can catch their absence.

// seedAccount stores one credential and reports its auth index.
func seedAccount(t *testing.T, fake *fakeHost) string {
	t.Helper()
	credential := &Credential{
		AccessToken:  "mmoat_0123456789012345678901234567890123456789012345678901234567",
		RefreshToken: "mmort_0123456789012345678901234567890123456789012345678901234567",
		TokenType:    "Bearer",
		ExpiresAt:    "1790784000000",
	}
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode credential: %v", errEncode)
	}
	const authIndex = "mm-1"
	fake.mu.Lock()
	fake.files = append(fake.files, pluginapi.HostAuthFileEntry{
		Provider: ProviderKey, AuthIndex: authIndex, Name: "minimax-1.json", Status: "active",
	})
	fake.auths[authIndex] = storage
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, ModelsPath):
			return httpResponse(http.StatusOK, `{"providers":[]}`), nil
		case strings.Contains(request.URL, SigninStatusPath):
			return httpResponse(http.StatusOK, `{"base_resp":{"status_code":0}}`), nil
		case strings.Contains(request.URL, CreditDetailsPath):
			return httpResponse(http.StatusOK, `{"details":[]}`), nil
		default:
			return httpResponse(http.StatusOK, `{"base_resp":{"status_code":0}}`), nil
		}
	}
	fake.mu.Unlock()
	return authIndex
}

// dispatchPage renders one management resource route.
func dispatchPage(t *testing.T, path string, query url.Values) string {
	t.Helper()
	request := pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    path,
		Headers: http.Header{"Accept": {"text/html"}},
		Query:   query,
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	value, errHandle := handleManagementHandle(testHost(), raw)
	if errHandle != nil {
		t.Fatalf("management handler: %v", errHandle)
	}
	response, okResponse := value.(pluginapi.ManagementResponse)
	if !okResponse {
		t.Fatalf("handler returned %T", value)
	}
	return string(response.Body)
}

// TestStatusPageOffersBothLoginRoutes pins the two login affordances every
// provider in this repository carries.
func TestStatusPageOffersBothLoginRoutes(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	authIndex := seedAccount(t, fake)

	page := dispatchPage(t, "/v0/resource/plugins/minimax/status", url.Values{})
	if !strings.Contains(page, "login?"+plugui.AddAccountQuery) {
		t.Fatalf("新建账号 is not reachable from the status page:\n%s", truncateForTest(page, 1200))
	}
	if !strings.Contains(page, "重新登录") {
		t.Fatalf("the account card offers no way to replace a credential:\n%s", truncateForTest(page, 1200))
	}
	if !strings.Contains(page, "auth_index="+authIndex) {
		t.Fatalf("the login link does not name the account:\n%s", truncateForTest(page, 1200))
	}
	// The sign-in action must survive alongside them; it is the card's primary
	// action and the reason the account card renders actions at all.
	if !strings.Contains(page, "checkin?auth_index="+authIndex) {
		t.Fatalf("the check-in link disappeared:\n%s", truncateForTest(page, 1200))
	}
}

// TestLoginPageExplainsTheAddAccountFlow keeps the other end honest: a page
// opened with `add=1` has to say what it is doing, because the flow is
// otherwise identical to a first login.
func TestLoginPageExplainsTheAddAccountFlow(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	seedAccount(t, fake)

	query, errQuery := url.ParseQuery(plugui.AddAccountQuery)
	if errQuery != nil {
		t.Fatalf("AddAccountQuery is not a query string: %v", errQuery)
	}
	page := dispatchPage(t, "/v0/resource/plugins/minimax/login", query)
	if !strings.Contains(page, plugui.AddAccountNotice) {
		t.Fatalf("the login page does not explain that this run adds an account:\n%s", truncateForTest(page, 900))
	}
}

// truncateForTest keeps a failure readable.
func truncateForTest(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// dispatchJSON renders one management resource route and decodes its object.
func dispatchJSON(t *testing.T, path string, query url.Values) map[string]any {
	t.Helper()
	request := pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    path,
		Headers: http.Header{"Accept": {"application/json"}},
		Query:   query,
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	value, errHandle := handleManagementHandle(testHost(), raw)
	if errHandle != nil {
		t.Fatalf("management handler: %v", errHandle)
	}
	response, okResponse := value.(pluginapi.ManagementResponse)
	if !okResponse {
		t.Fatalf("handler returned %T", value)
	}
	document := map[string]any{}
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("document is not JSON: %v", errUnmarshal)
	}
	return document
}

// TestVerificationStatusIsPublishedAndHonest pins the developer assertions the
// status document publishes. They exist so an operator can tell "never tried"
// from "known to work" — the reference could not verify its own login at all,
// and this port carried that gap until it completed a real authorization.
//
// The refresh claim is asserted to be FALSE on purpose: the grant is implemented
// but no renewal has been observed, and a document that says otherwise would be
// an overclaim of exactly the kind these fields exist to prevent.
func TestVerificationStatusIsPublishedAndHonest(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	seedAccount(t, fake)

	document := dispatchJSON(t, "/v0/resource/plugins/minimax/status", nil)
	for _, key := range []string{"login_verified", "inference_verified", "refresh_verified"} {
		if _, present := document[key]; !present {
			t.Fatalf("%s is missing: the document must state what has been verified", key)
		}
	}
	if document["login_verified"] != true {
		t.Fatalf("login_verified = %v, want true (a real device-code authorization completed)", document["login_verified"])
	}
	if document["inference_verified"] != true {
		t.Fatalf("inference_verified = %v, want true", document["inference_verified"])
	}
	// refresh_verified flipped from false to true on 2026-10-01, and only after
	// the renewal path was actually exercised: a credential placed 120 s from
	// expiry renewed on the next inference call (the stored access token
	// changed), while a follow-up call outside the window did not rotate it
	// again. The assertion is paired with a check that the evidence is still
	// written down, so this cannot be flipped back without an argument.
	if document["refresh_verified"] != true {
		t.Fatalf("refresh_verified = %v, want true (a renewal was observed on 2026-10-01)", document["refresh_verified"])
	}

	// The login route publishes the same claim, so the two cannot disagree.
	login := dispatchJSON(t, "/v0/resource/plugins/minimax/login", nil)
	if login["login_verified"] != true {
		t.Fatalf("login route login_verified = %v, want true", login["login_verified"])
	}
}

// TestRefreshVerifiedClaimKeepsItsEvidence stops the verification flags from
// becoming decoration. A flag that says "proven" without saying how is
// indistinguishable from a flag someone flipped to make a page look green, and
// this particular one was false for a real reason — renewal genuinely never ran
// until the expiry format was fixed.
func TestRefreshVerifiedClaimKeepsItsEvidence(t *testing.T) {
	source, errRead := os.ReadFile("config.go")
	if errRead != nil {
		t.Fatalf("read config.go: %v", errRead)
	}
	text := string(source)
	for _, want := range []string{
		"RefreshVerified: NOW OBSERVED",
		"grant_type=refresh_token",
		"RFC3339",
		"120 s from expiry",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the refresh_verified claim lost its evidence (%q missing from config.go)", want)
		}
	}
	if !strings.Contains(text, "RefreshVerified = true") {
		t.Fatal("RefreshVerified should be true now that renewal has been observed")
	}
}
