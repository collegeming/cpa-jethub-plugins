package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the self-healing contract of the Cline plugin: the status page,
// the quota route and the executor renew an expired credential themselves
// instead of waiting for a host timer that every restart resets.
//
// Everything is offline. One fake host transport answers the auth callbacks AND
// the upstream HTTP calls, so the tests see exactly which credential each read
// was sent with.
//
// The positive path cannot be produced live without waiting for a token to
// expire; a live credential is normally valid, which is the negative case
// TestPageDoesNotRenewAValidCredential pins here.

const (
	clineLiveName  = "cline-live.json"
	clineLiveIndex = "idx-1"
)

// clineFreshnessFixture is the scripted host.
type clineFreshnessFixture struct {
	mu sync.Mutex
	// refreshStatus/refreshBody script the renewal answer.
	refreshStatus int
	refreshBody   string
	// refreshErr makes the renewal fail at the transport level.
	refreshErr error
	// renewalCalls counts the renewal requests.
	renewalCalls int
	// chatTokens records the Authorization header of each chat call.
	chatTokens []string
	// balanceTokens records the Authorization header of each balance call.
	balanceTokens []string
	// saved records host.auth.save payloads.
	saved map[string]json.RawMessage
}

// install registers the fake transport.
func (f *clineFreshnessFixture) install(t *testing.T, stored json.RawMessage) *abiboot.Host {
	t.Helper()
	f.saved = map[string]json.RawMessage{}
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return abiboot.OK(map[string]any{"files": []map[string]any{{
				"name": clineLiveName, "id": clineLiveName, "auth_index": clineLiveIndex,
				"provider": ProviderKey, "type": ProviderKey, "status": "active",
			}}})
		case pluginabi.MethodHostAuthGet:
			return abiboot.OK(map[string]any{"auth_index": clineLiveIndex, "name": clineLiveName, "json": stored})
		case pluginabi.MethodHostAuthSave:
			var payload struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.mu.Lock()
			f.saved[payload.Name] = payload.JSON
			f.mu.Unlock()
			return abiboot.OK(map[string]any{"name": payload.Name, "path": "/auths/" + payload.Name})
		case pluginabi.MethodHostHTTPDo:
			var payload struct {
				Method  string      `json:"method"`
				URL     string      `json:"url"`
				Headers http.Header `json:"headers"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			switch {
			case strings.HasSuffix(payload.URL, RefreshPath):
				f.renewalCalls++
				if f.refreshErr != nil {
					return nil, f.refreshErr
				}
				status := f.refreshStatus
				if status == 0 {
					status = http.StatusOK
				}
				return clineHTTPResponse(status, f.refreshBody)
			case strings.Contains(payload.URL, "/api/v1/users/"):
				f.balanceTokens = append(f.balanceTokens, payload.Headers.Get("Authorization"))
				return clineHTTPResponse(http.StatusOK, `{"data":{"balance":425}}`)
			case strings.HasSuffix(payload.URL, ChatPath):
				f.chatTokens = append(f.chatTokens, payload.Headers.Get("Authorization"))
				return clineHTTPResponse(http.StatusOK, chatStreamBody)
			}
			return nil, fmt.Errorf("unexpected upstream call %s", payload.URL)
		case pluginabi.MethodHostLog:
			return abiboot.OK(map[string]any{})
		}
		return nil, fmt.Errorf("unexpected host method %s", method)
	})
	t.Cleanup(abiboot.ClearHostCaller)
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"cline-freshness"}`))
}

func (f *clineFreshnessFixture) counts() (int, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewalCalls, append([]string(nil), f.balanceTokens...), append([]string(nil), f.chatTokens...)
}

// clineHTTPResponse wraps a canned upstream answer in the host envelope the
// plugin decodes. Body travels base64-encoded because it is a []byte field.
func clineHTTPResponse(status int, body string) ([]byte, error) {
	return abiboot.OK(map[string]any{
		"StatusCode": status,
		"Headers":    http.Header{"Content-Type": []string{"application/json"}},
		"Body":       base64.StdEncoding.EncodeToString([]byte(body)),
	})
}

// clineCredential builds a renewable credential expiring at `expiresAtMS`.
func clineCredential(accessToken string, expiresAtMS int64) json.RawMessage {
	raw, _ := json.Marshal(&Credential{
		Type:         ProviderKey,
		AccessToken:  accessToken,
		RefreshToken: "refresh-token",
		ExpireTime:   expiresAtMS,
		AccountID:    "acct-1",
		Email:        "live@example.com",
	})
	return raw
}

// clineRefreshAnswer is a renewal answer carrying a new access token. The expiry
// key is the one the provider's parser reads (`expiresAt`).
func clineRefreshAnswer(token string, expiresAtMS int64) string {
	return fmt.Sprintf(`{"data":{"accessToken":%q,"refreshToken":"refresh-2","expiresAt":%d}}`, token, expiresAtMS)
}

// newClineFreshnessFixture wires one account and clears the process-wide
// freshness state, so a test never inherits another test's cooldown.
func newClineFreshnessFixture(t *testing.T, stored json.RawMessage) (*clineFreshnessFixture, *abiboot.Host) {
	t.Helper()
	withTestSettings(t, DefaultConfig())
	credentialRefresher = newCredentialRefresher()
	t.Cleanup(func() { credentialRefresher = newCredentialRefresher() })

	fixture := &clineFreshnessFixture{}
	return fixture, fixture.install(t, stored)
}

// TestPageRenewsAnExpiredCredential is the positive path: the expired credential
// is renewed exactly once before the read, the refreshed bytes are written back
// to the SAME auth file, and the balance read carries the NEW token.
func TestPageRenewsAnExpiredCredential(t *testing.T) {
	fixture, host := newClineFreshnessFixture(t, clineCredential("workos:old", time.Now().Add(-time.Hour).UnixMilli()))
	fixture.refreshBody = clineRefreshAnswer("workos:new", time.Now().Add(12*time.Hour).UnixMilli())

	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}})
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v", errDecode)
	}
	account, _ := document["account"].(map[string]any)
	if got := account["refreshed"]; got != true {
		t.Fatalf("account refreshed = %#v, want true (document %#v)", got, document)
	}

	renewals, balanceTokens, _ := fixture.counts()
	if renewals != 1 {
		t.Fatalf("renewal calls = %d, want exactly 1", renewals)
	}
	if len(balanceTokens) != 1 || balanceTokens[0] != "Bearer workos:new" {
		t.Fatalf("balance tokens = %#v, want the refreshed token", balanceTokens)
	}

	fixture.mu.Lock()
	saved := make(map[string]json.RawMessage, len(fixture.saved))
	for name, storage := range fixture.saved {
		saved[name] = storage
	}
	fixture.mu.Unlock()
	if len(saved) != 1 {
		t.Fatalf("saved files = %d, want exactly 1", len(saved))
	}
	storage, ok := saved[clineLiveName]
	if !ok {
		t.Fatalf("saved names = %#v, want %q", saved, clineLiveName)
	}
	var stored Credential
	if errDecode := json.Unmarshal(storage, &stored); errDecode != nil {
		t.Fatalf("decode saved credential: %v", errDecode)
	}
	if stored.AccessToken != "workos:new" {
		t.Fatalf("saved access token = %q, want the renewed one", stored.AccessToken)
	}
	if stored.Type != ProviderKey {
		t.Fatalf("saved type = %q, want the provider marker", stored.Type)
	}
}

// TestPageDoesNotRenewAValidCredential is the negative case a live instance is
// normally in: a credential with hours left must produce no renewal call.
func TestPageDoesNotRenewAValidCredential(t *testing.T) {
	fixture, host := newClineFreshnessFixture(t, clineCredential("workos:valid", time.Now().Add(6*time.Hour).UnixMilli()))

	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}})
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v", errDecode)
	}

	renewals, balanceTokens, _ := fixture.counts()
	if renewals != 0 {
		t.Fatalf("renewal calls = %d, want 0 for a valid credential", renewals)
	}
	if len(balanceTokens) != 1 || balanceTokens[0] != "Bearer workos:valid" {
		t.Fatalf("balance tokens = %#v, want one read with the stored token", balanceTokens)
	}
	if account, _ := document["account"].(map[string]any); account != nil {
		if _, ok := account["refreshed"]; ok {
			t.Error("document claims a renewal that never happened")
		}
	}
}

// TestPageSurfacesARenewalFailureAndBacksOff pins the failure contract: a
// transport failure is reported as a renewal failure — never as an expired
// credential — and a second load inside the cooldown does not retry.
func TestPageSurfacesARenewalFailureAndBacksOff(t *testing.T) {
	fixture, host := newClineFreshnessFixture(t, clineCredential("workos:old", time.Now().Add(-time.Minute).UnixMilli()))
	fixture.refreshErr = fmt.Errorf("dial tcp 1.2.3.4:443: connect: network is unreachable")

	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}})
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v", errDecode)
	}
	account, _ := document["account"].(map[string]any)
	message, _ := account["refresh_error"].(string)
	if message == "" {
		t.Fatalf("account has no refresh_error: %#v", document)
	}
	if strings.Contains(message, "已失效") {
		t.Fatalf("a transport failure was reported as a dead credential: %q", message)
	}

	if renewals, _, _ := fixture.counts(); renewals != 1 {
		t.Fatalf("renewal calls = %d, want 1", renewals)
	}
	_ = statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}})
	if renewals, _, _ := fixture.counts(); renewals != 1 {
		t.Fatalf("renewal calls inside the cooldown = %d, want 1", renewals)
	}
}

// TestPageMarksATerminalRenewalDeadWithoutRetrying pins the terminal half.
func TestPageMarksATerminalRenewalDeadWithoutRetrying(t *testing.T) {
	fixture, host := newClineFreshnessFixture(t, clineCredential("workos:old", time.Now().Add(-time.Hour).UnixMilli()))
	fixture.refreshStatus = http.StatusUnauthorized
	fixture.refreshBody = `{"error":"Unauthorized"}`

	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}})
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v", errDecode)
	}
	account, _ := document["account"].(map[string]any)
	message, _ := account["refresh_error"].(string)
	if !strings.Contains(message, "重新登录") {
		t.Fatalf("refresh_error = %q, want the terminal re-login message", message)
	}
	for load := 0; load < 3; load++ {
		_ = statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}})
	}
	if renewals, _, _ := fixture.counts(); renewals != 1 {
		t.Fatalf("renewal calls for a dead credential = %d, want 1", renewals)
	}
}

// TestExecutorRenewsBeforeSigning pins the second trigger point: the inference
// path renews the credential before it signs anything.
func TestExecutorRenewsBeforeSigning(t *testing.T) {
	fixture, host := newClineFreshnessFixture(t, clineCredential("workos:old", time.Now().Add(-time.Hour).UnixMilli()))
	fixture.refreshBody = clineRefreshAnswer("workos:new", time.Now().Add(12*time.Hour).UnixMilli())

	request, errRequest := executorRequest(expiredClineCredential(), `{"messages":[{"role":"user","content":"hi"}]}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	request.AuthID = clineLiveName

	if _, errHandler := handleExecutorExecute(host, marshalRequest(t, request)); errHandler != nil {
		t.Fatalf("handleExecutorExecute: %v", errHandler)
	}
	renewals, _, chatTokens := fixture.counts()
	if renewals != 1 {
		t.Fatalf("renewal calls = %d, want 1", renewals)
	}
	if len(chatTokens) != 1 || chatTokens[0] != "Bearer workos:new" {
		t.Fatalf("chat tokens = %#v, want the refreshed token", chatTokens)
	}
}

// TestExecutorRefusesADeadExpiredCredential pins that the executor reports the
// renewal failure instead of signing a doomed request.
func TestExecutorRefusesADeadExpiredCredential(t *testing.T) {
	fixture, host := newClineFreshnessFixture(t, clineCredential("workos:old", time.Now().Add(-time.Hour).UnixMilli()))
	fixture.refreshStatus = http.StatusUnauthorized
	fixture.refreshBody = `{"error":"Unauthorized"}`

	request, errRequest := executorRequest(expiredClineCredential(), `{"messages":[{"role":"user","content":"hi"}]}`)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	request.AuthID = clineLiveName

	_, errHandler := handleExecutorExecute(host, marshalRequest(t, request))
	if errHandler == nil {
		t.Fatal("executor accepted an expired credential after a terminal renewal failure")
	}
	if !authrefresh.IsTerminal(errHandler) {
		t.Fatalf("executor error = %v, want a terminal classification", errHandler)
	}
}

// expiredClineCredential is the credential an executor request carries.
func expiredClineCredential() *Credential {
	return &Credential{
		Type:         ProviderKey,
		AccessToken:  "workos:old",
		RefreshToken: "refresh-token",
		ExpireTime:   time.Now().Add(-time.Hour).UnixMilli(),
		AccountID:    "acct-1",
		Email:        "live@example.com",
	}
}

// TestCredentialExpiryLeavesAnUndatableCredentialAlone pins that a credential
// with no expiry is never renewed: the server's 401 decides.
func TestCredentialExpiryLeavesAnUndatableCredentialAlone(t *testing.T) {
	if _, ok := credentialExpiry(json.RawMessage(`{"access_token":"workos:x","refresh_token":"r"}`)); ok {
		t.Fatal("credentialExpiry invented an expiry for an undatable credential")
	}
	if credentialRefreshable(json.RawMessage(`{"access_token":"workos:x"}`)) {
		t.Fatal("credentialRefreshable accepted a credential without a refresh token")
	}
}
