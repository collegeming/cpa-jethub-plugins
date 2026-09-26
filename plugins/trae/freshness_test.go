package main

import (
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

// This file pins the self-healing contract of the TRAE plugin: the page sweep,
// the quota route and the executor renew an expired credential themselves
// instead of waiting for a host timer that every restart resets.
//
// Everything is offline. The fake host answers both the renewal and the credits
// endpoint, and the renewal rotates the access token — which is also how the
// tests prove the refreshed credential is what the read used: the credits call
// carries `Authorization: Cloud-IDE-JWT <token>`.
//
// The positive path cannot be produced live without waiting for a token to
// expire; a live credential is normally valid, which is the negative case
// TestPageSweepDoesNotRenewAValidCredential pins here.

// traeFreshnessFixture is the scripted vendor.
type traeFreshnessFixture struct {
	mu sync.Mutex
	// refreshCalls counts ExchangeToken requests.
	refreshCalls int
	// usageTokens records the Authorization header of each credits read.
	usageTokens []string
	// refreshStatus/refreshBody replace the renewal answer when set.
	refreshStatus int
	refreshBody   string
	// transportFailure makes the renewal fail the way host.http.do does when
	// the network is down.
	transportFailure bool
	// saved records host.auth.save payloads.
	saved map[string]json.RawMessage
}

// install registers the fake transport.
func (f *traeFreshnessFixture) install(t *testing.T, stored json.RawMessage) *abiboot.Host {
	t.Helper()
	f.saved = map[string]json.RawMessage{}
	fakeHost(t, func(method string, request []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return map[string]any{"files": []map[string]any{{
				"name":       "trae-live.json",
				"auth_index": "idx-1",
				"provider":   ProviderKey,
				"type":       ProviderKey,
				"status":     "active",
			}}}, nil
		case pluginabi.MethodHostAuthGet:
			return map[string]any{"auth_index": "idx-1", "name": "trae-live.json", "json": stored}, nil
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
			return map[string]any{"name": payload.Name, "path": "/auths/" + payload.Name}, nil
		case pluginabi.MethodHostHTTPDo:
			var payload struct {
				URL     string      `json:"url"`
				Headers http.Header `json:"headers"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			switch {
			case strings.HasSuffix(payload.URL, ExchangePath):
				f.refreshCalls++
				if f.transportFailure {
					return nil, fmt.Errorf("dial tcp 1.2.3.4:443: connect: network is unreachable")
				}
				status := f.refreshStatus
				if status == 0 {
					status = http.StatusOK
				}
				return traeHTTPResponse(status, f.refreshBody), nil
			case strings.HasSuffix(payload.URL, EntUsagePath):
				f.usageTokens = append(f.usageTokens, payload.Headers.Get("Authorization"))
				return traeHTTPResponse(http.StatusOK, traeUsageBody), nil
			case strings.HasSuffix(payload.URL, CheckinStatusPath):
				return traeHTTPResponse(http.StatusOK, `{"checked_in":false,"credits":0}`), nil
			}
			return nil, fmt.Errorf("unexpected upstream call %s", payload.URL)
		case pluginabi.MethodHostLog:
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("unexpected host method %s", method)
	})
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"trae-freshness"}`))
}

func (f *traeFreshnessFixture) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls, append([]string(nil), f.usageTokens...)
}

func (f *traeFreshnessFixture) savedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.saved))
	for name := range f.saved {
		out = append(out, name)
	}
	return out
}

func (f *traeFreshnessFixture) savedCredential(t *testing.T, name string) Credential {
	t.Helper()
	f.mu.Lock()
	storage := f.saved[name]
	f.mu.Unlock()
	var credential Credential
	if errDecode := json.Unmarshal(storage, &credential); errDecode != nil {
		t.Fatalf("decode saved credential: %v", errDecode)
	}
	return credential
}

// traeHTTPResponse builds a canned host HTTP response.
func traeHTTPResponse(status int, body string) *pluginapi.HTTPResponse {
	return &pluginapi.HTTPResponse{
		StatusCode: status,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(body),
	}
}

const traeUsageBody = `{"user_entitlement_pack_list":[{"entitlement_base_info":{"name":"包","quota":{"credits_limit":100}},` +
	`"usage":{"credits_amount":40}}]}`

// traeCredential builds a renewable credential expiring at `expiryMS`.
func traeCredential(expiryMS int64) json.RawMessage {
	raw, _ := json.Marshal(&Credential{
		Type:         ProviderKey,
		AccessToken:  "old-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    ExpiresAtValue(fmt.Sprintf("%d", expiryMS)),
		UID:          "uid-1",
		Nickname:     "live-user",
		MachineID:    "0123456789abcdef0123456789abcdef",
		DeviceID:     "fedcba9876543210fedcba9876543210",
		Region:       RegionCN,
	})
	return raw
}

// refreshAnswer is an ExchangeToken answer carrying a new access token.
func traeRefreshAnswer(token string, expiryMS int64) string {
	return fmt.Sprintf(`{"Result":{"Token":%q,"RefreshToken":"refresh-2","TokenExpireAt":%d}}`, token, expiryMS)
}

// newTraeFreshnessFixture wires one account expiring at `expiryMS` and clears
// the process-wide freshness state, so a test never inherits another's cooldown.
func newTraeFreshnessFixture(t *testing.T, expiryMS int64) (*traeFreshnessFixture, *abiboot.Host) {
	t.Helper()
	withTraeSettings(t, DefaultConfig())
	credentialRefresher = newCredentialRefresher()
	t.Cleanup(func() { credentialRefresher = newCredentialRefresher() })

	fixture := &traeFreshnessFixture{}
	return fixture, fixture.install(t, traeCredential(expiryMS))
}

// withTraeSettings swaps the process settings for one test.
func withTraeSettings(t *testing.T, cfg Config) {
	t.Helper()
	previous := settings()
	setSettings(cfg)
	t.Cleanup(func() { setSettings(previous) })
}

// TestPageSweepRenewsAnExpiredCredential is the positive path: the expired
// credential is renewed exactly once before the read, the refreshed bytes are
// written back to the SAME auth file, and the read carries the NEW token.
func TestPageSweepRenewsAnExpiredCredential(t *testing.T) {
	fixture, host := newTraeFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.refreshBody = traeRefreshAnswer("new-token", time.Now().Add(12*time.Hour).UnixMilli())

	quotas := collectAccountQuotas(host, traeAccounts(host), settings())
	if len(quotas) != 1 {
		t.Fatalf("quotas = %d, want 1", len(quotas))
	}
	if quotas[0].CredentialErr != nil {
		t.Fatalf("credential error = %v", quotas[0].CredentialErr)
	}
	if !quotas[0].Refresh.Refreshed {
		t.Fatalf("refresh outcome = %+v, want a renewal", quotas[0].Refresh)
	}

	refreshCalls, usageTokens := fixture.counts()
	if refreshCalls != 1 {
		t.Fatalf("renewal calls = %d, want exactly 1", refreshCalls)
	}
	if len(usageTokens) != 1 || usageTokens[0] != "Cloud-IDE-JWT new-token" {
		t.Fatalf("usage tokens = %#v, want the refreshed token", usageTokens)
	}

	names := fixture.savedNames()
	if len(names) != 1 || names[0] != "trae-live.json" {
		t.Fatalf("saved names = %#v, want the auth file the host knows", names)
	}
	stored := fixture.savedCredential(t, "trae-live.json")
	if stored.AccessToken != "new-token" {
		t.Fatalf("saved access token = %q, want the renewed one", stored.AccessToken)
	}
	if stored.Type != ProviderKey {
		t.Fatalf("saved type = %q, want the provider marker", stored.Type)
	}
}

// TestPageSweepDoesNotRenewAValidCredential is the negative case a live instance
// is normally in: a credential with hours left must produce no renewal call.
func TestPageSweepDoesNotRenewAValidCredential(t *testing.T) {
	fixture, host := newTraeFreshnessFixture(t, time.Now().Add(6*time.Hour).UnixMilli())

	quotas := collectAccountQuotas(host, traeAccounts(host), settings())
	if len(quotas) != 1 || quotas[0].Refresh.Refreshed {
		t.Fatalf("quotas = %+v, want one unrenewed account", quotas)
	}
	refreshCalls, usageTokens := fixture.counts()
	if refreshCalls != 0 {
		t.Fatalf("renewal calls = %d, want 0 for a valid credential", refreshCalls)
	}
	if len(usageTokens) != 1 || usageTokens[0] != "Cloud-IDE-JWT old-token" {
		t.Fatalf("usage tokens = %#v, want one read with the stored token", usageTokens)
	}
	if names := fixture.savedNames(); len(names) != 0 {
		t.Fatalf("saved names = %#v, want nothing written back", names)
	}
}

// TestPageSweepRenewsOnceForConcurrentCallers pins the single-flight.
func TestPageSweepRenewsOnceForConcurrentCallers(t *testing.T) {
	fixture, host := newTraeFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.refreshBody = traeRefreshAnswer("new-token", time.Now().Add(12*time.Hour).UnixMilli())

	var wg sync.WaitGroup
	for index := 0; index < 6; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = collectAccountQuotas(host, traeAccounts(host), settings())
		}()
	}
	wg.Wait()

	if refreshCalls, _ := fixture.counts(); refreshCalls != 1 {
		t.Fatalf("renewal calls = %d, want 1 (single-flight)", refreshCalls)
	}
}

// TestPageSweepSurfacesARenewalFailureAndBacksOff pins the failure contract: a
// transport failure is reported as a renewal failure — never as an expired
// credential — and a second load inside the cooldown does not retry.
func TestPageSweepSurfacesARenewalFailureAndBacksOff(t *testing.T) {
	fixture, host := newTraeFreshnessFixture(t, time.Now().Add(-time.Minute).UnixMilli())
	fixture.transportFailure = true

	quotas := collectAccountQuotas(host, traeAccounts(host), settings())
	if len(quotas) != 1 || quotas[0].Refresh.Err == nil {
		t.Fatalf("quotas = %+v, want a recorded renewal failure", quotas)
	}
	message := quotas[0].Refresh.Err.Error()
	if strings.Contains(message, "已失效") {
		t.Fatalf("a transport failure was reported as a dead credential: %q", message)
	}
	if !strings.Contains(message, "network is unreachable") {
		t.Fatalf("renewal error = %q, want the transport reason", message)
	}
	if quotas[0].Credential == nil {
		t.Fatal("the credential was dropped over a renewal failure")
	}

	_ = collectAccountQuotas(host, traeAccounts(host), settings())
	if refreshCalls, _ := fixture.counts(); refreshCalls != 1 {
		t.Fatalf("renewal calls inside the cooldown = %d, want 1", refreshCalls)
	}
}

// TestPageSweepMarksATerminalRenewalDeadWithoutRetrying pins the terminal half.
func TestPageSweepMarksATerminalRenewalDeadWithoutRetrying(t *testing.T) {
	fixture, host := newTraeFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.refreshStatus = http.StatusUnauthorized
	fixture.refreshBody = `{"error":"invalid_grant"}`

	quotas := collectAccountQuotas(host, traeAccounts(host), settings())
	if len(quotas) != 1 || quotas[0].Refresh.Err == nil {
		t.Fatalf("quotas = %+v, want a recorded terminal failure", quotas)
	}
	if !authrefresh.IsTerminal(quotas[0].Refresh.Err) {
		t.Fatalf("renewal error = %v, want a terminal classification", quotas[0].Refresh.Err)
	}
	for load := 0; load < 3; load++ {
		_ = collectAccountQuotas(host, traeAccounts(host), settings())
	}
	if refreshCalls, _ := fixture.counts(); refreshCalls != 1 {
		t.Fatalf("renewal calls for a dead credential = %d, want 1", refreshCalls)
	}
}

// TestStatusDocumentReportsTheRenewal pins that the page's JSON says what the
// freshness check did, instead of silently publishing figures from a credential
// the user did not know was renewed.
func TestStatusDocumentReportsTheRenewal(t *testing.T) {
	fixture, host := newTraeFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.refreshBody = traeRefreshAnswer("new-token", time.Now().Add(12*time.Hour).UnixMilli())

	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}})
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v", errDecode)
	}
	if got := document["refreshed"]; got != true {
		t.Fatalf("document refreshed = %#v, want true", got)
	}
}

// TestExecutorRenewsBeforeSigning pins the second trigger point.
func TestExecutorRenewsBeforeSigning(t *testing.T) {
	fixture, host := newTraeFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.refreshBody = traeRefreshAnswer("new-token", time.Now().Add(12*time.Hour).UnixMilli())

	credential, errCredential := executorCredential(host, pluginapi.ExecutorRequest{
		AuthID:       "trae-live.json",
		AuthProvider: ProviderKey,
		StorageJSON:  traeCredential(time.Now().Add(-time.Hour).UnixMilli()),
	})
	if errCredential != nil {
		t.Fatalf("executorCredential: %v", errCredential)
	}
	if credential.AccessToken != "new-token" {
		t.Fatalf("executor token = %q, want the refreshed one", credential.AccessToken)
	}
	if refreshCalls, _ := fixture.counts(); refreshCalls != 1 {
		t.Fatalf("renewal calls = %d, want 1", refreshCalls)
	}
}

// TestExecutorRefusesADeadExpiredCredential pins that the executor reports the
// renewal failure instead of signing a doomed request.
func TestExecutorRefusesADeadExpiredCredential(t *testing.T) {
	fixture, host := newTraeFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.refreshStatus = http.StatusUnauthorized
	fixture.refreshBody = `{"error":"invalid_grant"}`

	_, errCredential := executorCredential(host, pluginapi.ExecutorRequest{
		AuthID:       "trae-live.json",
		AuthProvider: ProviderKey,
		StorageJSON:  traeCredential(time.Now().Add(-time.Hour).UnixMilli()),
	})
	if errCredential == nil {
		t.Fatal("executor accepted an expired credential after a terminal renewal failure")
	}
	if !authrefresh.IsTerminal(errCredential) {
		t.Fatalf("executor error = %v, want a terminal classification", errCredential)
	}
}

// TestCredentialExpiryLeavesAnUndatableCredentialAlone pins that a credential
// with neither a stored expiry nor a readable `exp` claim is never renewed.
func TestCredentialExpiryLeavesAnUndatableCredentialAlone(t *testing.T) {
	if _, ok := credentialExpiry(json.RawMessage(`{"access_token":"tok","refresh_token":"r"}`)); ok {
		t.Fatal("credentialExpiry invented an expiry for an undatable credential")
	}
	if credentialRefreshable(json.RawMessage(`{"access_token":"tok"}`)) {
		t.Fatal("credentialRefreshable accepted a credential without a refresh token")
	}
}
