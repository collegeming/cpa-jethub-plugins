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
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the self-healing contract of the LobsterAI plugin: the page
// sweep, the quota route and the executor renew an expired credential
// themselves instead of waiting for a host timer that every restart resets.
//
// Everything is offline. The fake host answers both the renewal and the profile
// endpoint, and the renewal rotates the access token — which is also how the
// tests prove the refreshed credential is what the read used: the profile call
// carries the new bearer token.
//
// The positive path cannot be produced live without waiting for a token to
// expire; a live credential is normally valid, which is the negative case
// TestPageSweepDoesNotRenewAValidCredential pins here.

const (
	lobsterLiveName      = "lobsterai-live.json"
	lobsterLiveIndex     = "idx-1"
	lobsterProfileAnswer = `{"code":0,"data":{"creditItems":[{"type":"积分包","creditsRemaining":42.5}]}}`
)

// refreshAnswer is a renewal response carrying a new access token.
func lobsterRefreshAnswer(token string, expiresInSeconds float64) string {
	return fmt.Sprintf(`{"code":0,"data":{"accessToken":%q,"refreshToken":"refresh-2","expiresIn":%v}}`,
		token, expiresInSeconds)
}

// lobsterCredential builds a renewable credential expiring at `expiresAt`
// (milliseconds as a string, the shape the provider stores).
func lobsterCredential(expiresAt string) json.RawMessage {
	raw, _ := json.Marshal(&Credential{
		Type:         ProviderKey,
		AccessToken:  "old-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    expiresAt,
		UID:          "uid-1",
		Nickname:     "live-user",
	})
	return raw
}

// newLobsterFreshnessFixture wires one account expiring at `expiresAt` and the
// given renewal route, with a fresh process-wide freshness state so a test never
// inherits another test's cooldown.
func newLobsterFreshnessFixture(t *testing.T, expiresAt string, renewal httpRoute) (*fakeHost, *abiboot.Host) {
	t.Helper()
	fake := newFakeHost().
		withFiles(pluginapi.HostAuthFileEntry{
			ID:        lobsterLiveName,
			AuthIndex: lobsterLiveIndex,
			Name:      lobsterLiveName,
			Provider:  ProviderKey,
			Type:      ProviderKey,
			Status:    "active",
		}).
		withAuthJSON(lobsterLiveIndex, string(lobsterCredential(expiresAt))).
		on(httpRoute{Method: http.MethodGet, Match: ProfileSummaryPath, Body: lobsterProfileAnswer})
	if renewal.Match != "" {
		fake.on(renewal)
	}

	credentialRefresher = newCredentialRefresher()
	t.Cleanup(func() { credentialRefresher = newCredentialRefresher() })
	return fake, installFakeHost(t, fake)
}

// TestPageSweepRenewsAnExpiredCredential is the positive path: the expired
// credential is renewed exactly once before the read, the refreshed bytes are
// written back to the SAME auth file, and the read carries the NEW token.
func TestPageSweepRenewsAnExpiredCredential(t *testing.T) {
	fake, host := newLobsterFreshnessFixture(t,
		fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixMilli()),
		httpRoute{Method: http.MethodPost, Match: RefreshPath, Body: lobsterRefreshAnswer("new-token", 12*3600)})

	quotas := collectAccountQuotas(host, lobsteraiAccounts(host), settings(), "1.0.0")
	if len(quotas) != 1 {
		t.Fatalf("quotas = %d, want 1", len(quotas))
	}
	if quotas[0].CredentialErr != nil {
		t.Fatalf("credential error = %v", quotas[0].CredentialErr)
	}
	if !quotas[0].Refresh.Refreshed {
		t.Fatalf("refresh outcome = %+v, want a renewal", quotas[0].Refresh)
	}

	if renewals := fake.requestsFor(RefreshPath); len(renewals) != 1 {
		t.Fatalf("renewal calls = %d, want exactly 1", len(renewals))
	}
	reads := fake.requestsFor(ProfileSummaryPath)
	if len(reads) != 1 {
		t.Fatalf("profile reads = %d, want 1", len(reads))
	}
	if got := reads[0].Headers.Get("Authorization"); !strings.Contains(got, "new-token") {
		t.Fatalf("profile read sent %q, want the refreshed token", got)
	}

	saved := fake.savedAuths()
	if len(saved) != 1 {
		t.Fatalf("saved files = %d, want exactly 1", len(saved))
	}
	if saved[0].Name != lobsterLiveName {
		t.Fatalf("saved name = %q, want the auth file the host knows", saved[0].Name)
	}
	var stored Credential
	if errDecode := json.Unmarshal(saved[0].JSON, &stored); errDecode != nil {
		t.Fatalf("decode saved credential: %v", errDecode)
	}
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
	// No renewal route is scripted at all, so any attempt would be visible as a
	// failed request AND as a recorded URL.
	fake, host := newLobsterFreshnessFixture(t, fmt.Sprintf("%d", time.Now().Add(6*time.Hour).UnixMilli()), httpRoute{})

	quotas := collectAccountQuotas(host, lobsteraiAccounts(host), settings(), "1.0.0")
	if len(quotas) != 1 || quotas[0].Refresh.Refreshed || quotas[0].Refresh.Err != nil {
		t.Fatalf("quotas = %+v, want one untouched account", quotas)
	}
	if renewals := fake.requestsFor(RefreshPath); len(renewals) != 0 {
		t.Fatalf("renewal calls = %d, want 0 for a valid credential", len(renewals))
	}
	reads := fake.requestsFor(ProfileSummaryPath)
	if len(reads) != 1 || !strings.Contains(reads[0].Headers.Get("Authorization"), "old-token") {
		t.Fatalf("profile reads = %+v, want one read with the stored token", reads)
	}
	if saved := fake.savedAuths(); len(saved) != 0 {
		t.Fatalf("saved files = %d, want nothing written back", len(saved))
	}
}

// TestPageSweepRenewsOnceForConcurrentCallers pins the single-flight.
func TestPageSweepRenewsOnceForConcurrentCallers(t *testing.T) {
	fake, host := newLobsterFreshnessFixture(t,
		fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixMilli()),
		httpRoute{Method: http.MethodPost, Match: RefreshPath, Body: lobsterRefreshAnswer("new-token", 12*3600)})

	var wg sync.WaitGroup
	for index := 0; index < 6; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = collectAccountQuotas(host, lobsteraiAccounts(host), settings(), "1.0.0")
		}()
	}
	wg.Wait()

	if renewals := fake.requestsFor(RefreshPath); len(renewals) != 1 {
		t.Fatalf("renewal calls = %d, want 1 (single-flight)", len(renewals))
	}
}

// TestPageSweepSurfacesARenewalFailureAndBacksOff pins the failure contract: a
// transport failure is reported as a renewal failure — never as an expired
// credential — and a second load inside the cooldown does not retry.
func TestPageSweepSurfacesARenewalFailureAndBacksOff(t *testing.T) {
	fake, host := newLobsterFreshnessFixture(t,
		fmt.Sprintf("%d", time.Now().Add(-time.Minute).UnixMilli()),
		httpRoute{Method: http.MethodPost, Match: RefreshPath, Err: errFakeTransport})

	quotas := collectAccountQuotas(host, lobsteraiAccounts(host), settings(), "1.0.0")
	if len(quotas) != 1 || quotas[0].Refresh.Err == nil {
		t.Fatalf("quotas = %+v, want a recorded renewal failure", quotas)
	}
	message := quotas[0].Refresh.Err.Error()
	if strings.Contains(message, "已失效") {
		t.Fatalf("a transport failure was reported as a dead credential: %q", message)
	}
	if quotas[0].Credential == nil {
		t.Fatal("the credential was dropped over a renewal failure")
	}

	_ = collectAccountQuotas(host, lobsteraiAccounts(host), settings(), "1.0.0")
	if renewals := fake.requestsFor(RefreshPath); len(renewals) != 1 {
		t.Fatalf("renewal calls inside the cooldown = %d, want 1", len(renewals))
	}
}

// TestPageSweepMarksATerminalRenewalDeadWithoutRetrying pins the terminal half.
func TestPageSweepMarksATerminalRenewalDeadWithoutRetrying(t *testing.T) {
	fake, host := newLobsterFreshnessFixture(t,
		fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixMilli()),
		httpRoute{Method: http.MethodPost, Match: RefreshPath, Status: http.StatusUnauthorized,
			Body: `{"code":401,"msg":"unauthorized"}`})

	quotas := collectAccountQuotas(host, lobsteraiAccounts(host), settings(), "1.0.0")
	if len(quotas) != 1 || quotas[0].Refresh.Err == nil {
		t.Fatalf("quotas = %+v, want a recorded terminal failure", quotas)
	}
	if !authrefresh.IsTerminal(quotas[0].Refresh.Err) {
		t.Fatalf("renewal error = %v, want a terminal classification", quotas[0].Refresh.Err)
	}
	for load := 0; load < 3; load++ {
		_ = collectAccountQuotas(host, lobsteraiAccounts(host), settings(), "1.0.0")
	}
	if renewals := fake.requestsFor(RefreshPath); len(renewals) != 1 {
		t.Fatalf("renewal calls for a dead credential = %d, want 1", len(renewals))
	}
}

// TestStatusDocumentReportsTheRenewal pins that the JSON says what the freshness
// check did, instead of silently publishing figures from a renewed credential.
func TestStatusDocumentReportsTheRenewal(t *testing.T) {
	fake, host := newLobsterFreshnessFixture(t,
		fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixMilli()),
		httpRoute{Method: http.MethodPost, Match: RefreshPath, Body: lobsterRefreshAnswer("new-token", 12*3600)})
	_ = fake

	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}})
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v", errDecode)
	}
	accounts, _ := document["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v, want one entry", document["accounts"])
	}
	entry, _ := accounts[0].(map[string]any)
	if got := entry["refreshed"]; got != true {
		t.Fatalf("account refreshed = %#v, want true", got)
	}
}

// TestExecutorRenewsBeforeSigning pins the second trigger point.
func TestExecutorRenewsBeforeSigning(t *testing.T) {
	_, host := newLobsterFreshnessFixture(t,
		fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixMilli()),
		httpRoute{Method: http.MethodPost, Match: RefreshPath, Body: lobsterRefreshAnswer("new-token", 12*3600)})

	credential, errCredential := executorCredential(host, pluginapi.ExecutorRequest{
		AuthID:       lobsterLiveName,
		AuthProvider: ProviderKey,
		StorageJSON:  lobsterCredential(fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixMilli())),
	})
	if errCredential != nil {
		t.Fatalf("executorCredential: %v", errCredential)
	}
	if credential.AccessToken != "new-token" {
		t.Fatalf("executor token = %q, want the refreshed one", credential.AccessToken)
	}
}

// TestExecutorRefusesADeadExpiredCredential pins that the executor reports the
// renewal failure instead of signing a doomed request.
func TestExecutorRefusesADeadExpiredCredential(t *testing.T) {
	_, host := newLobsterFreshnessFixture(t,
		fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixMilli()),
		httpRoute{Method: http.MethodPost, Match: RefreshPath, Status: http.StatusUnauthorized,
			Body: `{"code":401,"msg":"unauthorized"}`})

	_, errCredential := executorCredential(host, pluginapi.ExecutorRequest{
		AuthID:       lobsterLiveName,
		AuthProvider: ProviderKey,
		StorageJSON:  lobsterCredential(fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixMilli())),
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
