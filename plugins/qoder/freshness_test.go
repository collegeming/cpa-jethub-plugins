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

// This file pins the self-healing contract of the Qoder plugin: the status page,
// the quota route and the executor renew an expired credential themselves
// instead of waiting for a host timer that every restart resets.
//
// Everything is offline: the fake host transport answers both the renewal and
// the usage endpoint, and the renewal response carries a NEW access token, which
// is also how the tests prove the refreshed credential is what the read used —
// the Authorization header of the usage call names the token it was sent with.
//
// The positive path cannot be produced live without waiting for a token to
// expire; the live instance's credentials are currently valid, which is the
// negative case TestStatusPageDoesNotRenewAValidCredential pins here.

// qoderFreshnessFixture is the scripted vendor for these tests: one host
// transport that answers the auth callbacks AND both upstream endpoints.
type qoderFreshnessFixture struct {
	// host is the installed transport, so a test can inspect what was written
	// back through host.auth.save.
	host *fakeHost

	mu sync.Mutex
	// refreshCalls counts the renewal requests.
	refreshCalls int
	// usageTokens records the bearer token of each usage read.
	usageTokens []string
	// refreshStatus/refreshBody replace the renewal answer when set.
	refreshStatus int
	refreshBody   string
	// transportFailure makes the renewal fail the way host.http.do does when
	// the network is down.
	transportFailure bool
	// usageBody is the balance answer.
	usageBody string
}

// newQoderFreshnessFixture wires one account whose credential expires at
// `expiry`, with a fresh freshness state (the real one is process-wide, so a
// test must not inherit another test's cooldown).
func newQoderFreshnessFixture(t *testing.T, token string, expiry time.Time) (*qoderFreshnessFixture, *abiboot.Host) {
	t.Helper()
	withSettings(t, DefaultConfig())
	credentialRefresher = newCredentialRefresher()
	t.Cleanup(func() { credentialRefresher = newCredentialRefresher() })

	fixture := &qoderFreshnessFixture{usageBody: usageBodyFixture()}
	transport := newFakeHost()
	transport.files = []pluginapi.HostAuthFileEntry{{
		ID:        "qoder-live.json",
		AuthIndex: "idx-1",
		Name:      "qoder-live.json",
		Provider:  ProviderKey,
		Type:      ProviderKey,
		Status:    "active",
	}}
	transport.auths["idx-1"] = qoderCredential(token, expiry)
	transport.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		switch {
		case strings.Contains(request.URL, RefreshPath):
			fixture.refreshCalls++
			if fixture.transportFailure {
				return nil, fmt.Errorf("dial tcp 1.2.3.4:443: connect: network is unreachable")
			}
			status := fixture.refreshStatus
			if status == 0 {
				status = http.StatusOK
			}
			return httpResponse(status, fixture.refreshBody), nil
		case strings.Contains(request.URL, UsagePath):
			fixture.usageTokens = append(fixture.usageTokens, request.Headers.Get("Authorization"))
			return httpResponse(http.StatusOK, fixture.usageBody), nil
		}
		return nil, fmt.Errorf("unexpected upstream call %s", request.URL)
	}
	transport.install(t)
	fixture.host = transport
	return fixture, testHost()
}

func (f *qoderFreshnessFixture) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls, append([]string(nil), f.usageTokens...)
}

// savedCredential returns what the fixture wrote back through host.auth.save.
func (f *qoderFreshnessFixture) savedCredential(t *testing.T) (string, json.RawMessage) {
	t.Helper()
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	for name, storage := range f.host.saved {
		return name, storage
	}
	return "", nil
}

// refreshAnswer is a renewal response carrying a new access token.
func qoderRefreshAnswer(token string, expiry time.Time) string {
	return fmt.Sprintf(`{"device_token":%q,"refresh_token":"refresh-token","expires_at":%d,"user_id":"uid-1"}`,
		token, expiry.UnixMilli())
}

// qoderCredential builds a renewable credential with the given token and expiry.
func qoderCredential(token string, expiry time.Time) json.RawMessage {
	raw, _ := json.Marshal(&Credential{
		Type:         ProviderKey,
		AccessToken:  token,
		RefreshToken: "refresh-token",
		ExpireTime:   expiry.UnixMilli(),
		MachineID:    "machine-1",
		UID:          "uid-1",
		Nickname:     "live-user",
	})
	return raw
}

// usageBodyFixture is a usable balance answer.
func usageBodyFixture() string {
	return `{"displayMode":"qoder","qoderUsage":{"userType":"personal_standard",` +
		`"userQuota":{"total":0,"used":0,"remaining":0,"unit":"credits"},` +
		`"addOnQuota":{"total":100,"used":40,"remaining":60,"unit":"credits"},` +
		`"expiresAt":253402214400000}}`
}

func qoderStatusRequest() pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}}
}

// TestStatusPageRenewsAnExpiredCredential is the positive path: the credential
// is renewed exactly once before the read, the refreshed bytes are written back
// to the SAME auth file, and the read is sent with the NEW token.
func TestStatusPageRenewsAnExpiredCredential(t *testing.T) {
	vendor, host := newQoderFreshnessFixture(t, "old-token", time.Now().Add(-time.Hour))
	vendor.refreshBody = qoderRefreshAnswer("new-token", time.Now().Add(12*time.Hour))

	document := decodeQoderDocument(t, statusJSON(host, qoderStatusRequest()))

	refreshCalls, usageTokens := vendor.counts()
	if refreshCalls != 1 {
		t.Fatalf("renewal calls = %d, want exactly 1", refreshCalls)
	}
	if len(usageTokens) != 1 {
		t.Fatalf("usage reads = %d, want 1", len(usageTokens))
	}
	if usageTokens[0] != "Bearer new-token" {
		t.Fatalf("usage read sent %q, want the refreshed token", usageTokens[0])
	}
	if got := document["refreshed"]; got != true {
		t.Fatalf("document refreshed = %#v, want true", got)
	}

	name, storage := vendor.savedCredential(t)
	if name != "qoder-live.json" {
		t.Fatalf("saved name = %q, want the auth file the host knows", name)
	}
	if storage == nil {
		t.Fatal("nothing was written back through host.auth.save")
	}
	stored := decodeQoderCredential(t, storage)
	if stored.AccessToken != "new-token" {
		t.Fatalf("saved access token = %q, want the renewed one", stored.AccessToken)
	}
	if stored.Type != ProviderKey {
		t.Fatalf("saved type = %q, want the provider marker", stored.Type)
	}
}

// TestStatusPageDoesNotRenewAValidCredential is the negative case the live
// instance is in: a credential with hours left must produce no renewal call.
func TestStatusPageDoesNotRenewAValidCredential(t *testing.T) {
	vendor, host := newQoderFreshnessFixture(t, "valid-token", time.Now().Add(6*time.Hour))

	document := decodeQoderDocument(t, statusJSON(host, qoderStatusRequest()))

	refreshCalls, usageTokens := vendor.counts()
	if refreshCalls != 0 {
		t.Fatalf("renewal calls = %d, want 0 for a valid credential", refreshCalls)
	}
	if len(usageTokens) != 1 || usageTokens[0] != "Bearer valid-token" {
		t.Fatalf("usage tokens = %#v, want one read with the stored token", usageTokens)
	}
	if _, ok := document["refreshed"]; ok {
		t.Error("document claims a renewal that never happened")
	}
}

// TestStatusPageRenewsOnceForConcurrentCallers pins the single-flight.
func TestStatusPageRenewsOnceForConcurrentCallers(t *testing.T) {
	vendor, host := newQoderFreshnessFixture(t, "old-token", time.Now().Add(-time.Hour))
	vendor.refreshBody = qoderRefreshAnswer("new-token", time.Now().Add(12*time.Hour))

	var wg sync.WaitGroup
	for index := 0; index < 6; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = statusJSON(host, qoderStatusRequest())
		}()
	}
	wg.Wait()

	if refreshCalls, _ := vendor.counts(); refreshCalls != 1 {
		t.Fatalf("renewal calls = %d, want 1 (single-flight)", refreshCalls)
	}
}

// TestStatusPageSurfacesARenewalFailureAndBacksOff pins the failure contract: a
// transport failure is reported as a renewal failure — never as an expired
// credential — and a second load inside the cooldown does not call the vendor
// again.
func TestStatusPageSurfacesARenewalFailureAndBacksOff(t *testing.T) {
	vendor, host := newQoderFreshnessFixture(t, "old-token", time.Now().Add(-time.Minute))
	vendor.transportFailure = true

	document := decodeQoderDocument(t, statusJSON(host, qoderStatusRequest()))
	refreshError, _ := document["refresh_error"].(string)
	if refreshError == "" {
		t.Fatalf("document has no refresh_error: %#v", document)
	}
	if strings.Contains(refreshError, "已失效") {
		t.Fatalf("a transport failure was reported as a dead credential: %q", refreshError)
	}
	if !strings.Contains(refreshError, "network is unreachable") {
		t.Fatalf("refresh_error = %q, want the transport reason", refreshError)
	}

	if refreshCalls, _ := vendor.counts(); refreshCalls != 1 {
		t.Fatalf("renewal calls = %d, want 1", refreshCalls)
	}
	second := decodeQoderDocument(t, statusJSON(host, qoderStatusRequest()))
	if got, _ := second["refresh_error"].(string); got == "" {
		t.Fatal("the second load lost the recorded renewal failure")
	}
	if refreshCalls, _ := vendor.counts(); refreshCalls != 1 {
		t.Fatalf("renewal calls inside the cooldown = %d, want 1", refreshCalls)
	}
}

// TestStatusPageMarksATerminalRenewalDeadWithoutRetrying pins the terminal half.
func TestStatusPageMarksATerminalRenewalDeadWithoutRetrying(t *testing.T) {
	vendor, host := newQoderFreshnessFixture(t, "old-token", time.Now().Add(-time.Hour))
	vendor.refreshStatus = http.StatusUnauthorized
	vendor.refreshBody = `{"error":"invalid_grant"}`

	document := decodeQoderDocument(t, statusJSON(host, qoderStatusRequest()))
	refreshError, _ := document["refresh_error"].(string)
	if !strings.Contains(refreshError, "重新登录") {
		t.Fatalf("refresh_error = %q, want the terminal re-login message", refreshError)
	}
	for load := 0; load < 3; load++ {
		_ = statusJSON(host, qoderStatusRequest())
	}
	if refreshCalls, _ := vendor.counts(); refreshCalls != 1 {
		t.Fatalf("renewal calls for a dead credential = %d, want 1", refreshCalls)
	}
}

// TestExecutorRenewsBeforeSigning pins the second trigger point.
func TestExecutorRenewsBeforeSigning(t *testing.T) {
	vendor, host := newQoderFreshnessFixture(t, "old-token", time.Now().Add(-time.Hour))
	vendor.refreshBody = qoderRefreshAnswer("new-token", time.Now().Add(12*time.Hour))

	_, credential, _, errPrepare := decodeExecutorCall(host, mustMarshalQoder(t, pluginapi.ExecutorRequest{
		AuthID:       "qoder-live.json",
		AuthProvider: ProviderKey,
		Model:        "qwen-flash",
		StorageJSON:  qoderCredential("old-token", time.Now().Add(-time.Hour)),
		Payload:      json.RawMessage(`{"model":"qwen-flash","messages":[]}`),
	}))
	if errPrepare != nil {
		t.Fatalf("decodeExecutorCall: %v", errPrepare)
	}
	if credential.AccessToken != "new-token" {
		t.Fatalf("executor credential token = %q, want the refreshed one", credential.AccessToken)
	}
	if refreshCalls, _ := vendor.counts(); refreshCalls != 1 {
		t.Fatalf("renewal calls = %d, want 1", refreshCalls)
	}
}

// TestExecutorRefusesADeadExpiredCredential pins that an expired credential with
// a terminal renewal failure is not signed into a doomed request.
func TestExecutorRefusesADeadExpiredCredential(t *testing.T) {
	vendor, host := newQoderFreshnessFixture(t, "old-token", time.Now().Add(-time.Hour))
	vendor.refreshStatus = http.StatusUnauthorized
	vendor.refreshBody = `{"error":"invalid_grant"}`

	_, _, _, errPrepare := decodeExecutorCall(host, mustMarshalQoder(t, pluginapi.ExecutorRequest{
		AuthID:       "qoder-live.json",
		AuthProvider: ProviderKey,
		Model:        "qwen-flash",
		StorageJSON:  qoderCredential("old-token", time.Now().Add(-time.Hour)),
		Payload:      json.RawMessage(`{"model":"qwen-flash","messages":[]}`),
	}))
	if errPrepare == nil {
		t.Fatal("executor accepted an expired credential after a terminal renewal failure")
	}
	if !authrefresh.IsTerminal(errPrepare) {
		t.Fatalf("executor error = %v, want a terminal classification", errPrepare)
	}
}

// TestCredentialExpiryLeavesAnUndatableCredentialAlone pins that a credential
// with no expiry is never renewed: the server stays the only authority.
func TestCredentialExpiryLeavesAnUndatableCredentialAlone(t *testing.T) {
	if _, ok := credentialExpiry(json.RawMessage(`{"access_token":"tok","refresh_token":"r"}`)); ok {
		t.Fatal("credentialExpiry invented an expiry for an undatable credential")
	}
	if credentialRefreshable(json.RawMessage(`{"access_token":"tok"}`)) {
		t.Fatal("credentialRefreshable accepted a credential without a refresh token")
	}
}

func decodeQoderDocument(t *testing.T, response pluginapi.ManagementResponse) map[string]any {
	t.Helper()
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v\n%s", errDecode, response.Body)
	}
	return document
}

func decodeQoderCredential(t *testing.T, storage json.RawMessage) Credential {
	t.Helper()
	var credential Credential
	if errDecode := json.Unmarshal(storage, &credential); errDecode != nil {
		t.Fatalf("decode credential: %v", errDecode)
	}
	return credential
}

func mustMarshalQoder(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	return raw
}
