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
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the self-healing contract of the Raccoon plugin: the status
// page, the quota route and the executor renew an expired credential themselves
// instead of waiting for a host timer that every restart resets.
//
// Everything is offline. The fake host answers both the renewal and the points
// endpoint, and the renewal rotates the access token — which is also how the
// tests prove the refreshed credential is what the read used: the points call
// carries the new bearer token.
//
// The positive path cannot be produced live without waiting for a token to
// expire; a live credential is normally valid, which is the negative case
// TestPageDoesNotRenewAValidCredential pins here.

const (
	raccoonLiveName  = "raccoon-live.json"
	raccoonLiveIndex = "idx-1"
	raccoonPoints    = `{"code":0,"data":{"available_points":120,"pools":[{"name":"默认","available_points":120}]}}`
)

// raccoonFreshnessFixture wraps the plugin's scriptable host and counts the
// renewal calls.
type raccoonFreshnessFixture struct {
	host *fakeHost
	mu   sync.Mutex
	// refreshCalls counts the renewal requests; refreshErr scripts a transport
	// failure and refreshStatus/refreshBody the HTTP answer.
	refreshCalls  int
	refreshStatus int
	refreshBody   string
	refreshErr    error
}

// newRaccoonFreshnessFixture wires one account and clears the process-wide
// freshness state, so a test never inherits another test's cooldown.
func newRaccoonFreshnessFixture(t *testing.T, stored json.RawMessage) (*raccoonFreshnessFixture, *abiboot.Host) {
	t.Helper()
	withRaccoonSettings(t, DefaultConfig())
	credentialRefresher = newCredentialRefresher()
	t.Cleanup(func() { credentialRefresher = newCredentialRefresher() })

	fixture := &raccoonFreshnessFixture{host: newFakeHost()}
	fixture.host.files = []pluginapi.HostAuthFileEntry{{
		ID:        raccoonLiveName,
		AuthIndex: raccoonLiveIndex,
		Name:      raccoonLiveName,
		Provider:  ProviderKey,
		Type:      ProviderKey,
		Status:    "active",
	}}
	fixture.host.auths[raccoonLiveIndex] = stored
	fixture.host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		switch {
		case strings.HasSuffix(request.URL, RefreshPath):
			fixture.refreshCalls++
			if fixture.refreshErr != nil {
				return nil, fixture.refreshErr
			}
			status := fixture.refreshStatus
			if status == 0 {
				status = http.StatusOK
			}
			return raccoonHTTPResponse(status, fixture.refreshBody), nil
		case strings.HasSuffix(request.URL, PointsBalancePath):
			return raccoonHTTPResponse(http.StatusOK, raccoonPoints), nil
		case strings.HasSuffix(request.URL, ChatCompletionsPath):
			return raccoonHTTPResponse(http.StatusOK, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), nil
		}
		return nil, fmt.Errorf("unexpected upstream call %s", request.URL)
	}
	fixture.host.install(t)
	return fixture, testHost()
}

func (f *raccoonFreshnessFixture) counts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

func (f *raccoonFreshnessFixture) answerWith(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshStatus, f.refreshBody, f.refreshErr = http.StatusOK, body, nil
}

func (f *raccoonFreshnessFixture) fail(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshStatus, f.refreshBody, f.refreshErr = status, body, nil
}

func (f *raccoonFreshnessFixture) failTransport() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshStatus, f.refreshErr = 0, errFakeTransport
}

func raccoonHTTPResponse(status int, body string) *pluginapi.HTTPResponse {
	return &pluginapi.HTTPResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(body),
	}
}

// raccoonJWT builds an unsigned JWT carrying the given `exp`, which is what the
// credential's expiry fallback reads.
func raccoonJWT(expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"exp": expiry.Unix()})
	claims := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + claims + ".signature"
}

// raccoonCredential builds a renewable credential. expiresAtMS = 0 means "no
// stored expiry", which is how the JWT `exp` fallback is exercised.
func raccoonCredential(accessToken string, expiresAtMS int64) json.RawMessage {
	expiresAt := ""
	if expiresAtMS > 0 {
		expiresAt = fmt.Sprintf("%d", expiresAtMS)
	}
	raw, _ := json.Marshal(&Credential{
		Type:         ProviderKey,
		AccessToken:  accessToken,
		RefreshToken: "refresh-token",
		ExpiresAt:    expiresAt,
		UserID:       "uid-1",
	})
	return raw
}

// raccoonRefreshAnswer is a renewal answer carrying a new access token.
func raccoonRefreshAnswer(token string) string {
	return fmt.Sprintf(`{"code":0,"data":{"access_token":%q,"refresh_token":"refresh-2"}}`, token)
}

func withRaccoonSettings(t *testing.T, cfg Config) {
	t.Helper()
	previous := settings()
	setSettings(cfg)
	t.Cleanup(func() { setSettings(previous) })
}

// TestPageRenewsAnExpiredCredential is the positive path: the expired credential
// is renewed exactly once before the read, the refreshed bytes are written back
// to the SAME auth file, and the points read carries the NEW token.
func TestPageRenewsAnExpiredCredential(t *testing.T) {
	fixture, host := newRaccoonFreshnessFixture(t,
		raccoonCredential("old-token", time.Now().Add(-time.Hour).UnixMilli()))
	fixture.answerWith(raccoonRefreshAnswer(raccoonJWT(time.Now().Add(12 * time.Hour))))

	quotas := collectAccountQuotas(host, raccoonAccounts(host), settings())
	if len(quotas) != 1 {
		t.Fatalf("quotas = %d, want 1", len(quotas))
	}
	if quotas[0].CredentialErr != nil {
		t.Fatalf("credential error = %v", quotas[0].CredentialErr)
	}
	if !quotas[0].Refresh.Refreshed {
		t.Fatalf("refresh outcome = %+v, want a renewal", quotas[0].Refresh)
	}
	if fixture.counts() != 1 {
		t.Fatalf("renewal calls = %d, want exactly 1", fixture.counts())
	}

	reads := fixture.requestsFor(PointsBalancePath)
	if len(reads) != 1 {
		t.Fatalf("points reads = %d, want 1", len(reads))
	}
	if got := bearerOf(reads[0]); got == "" || strings.Contains(got, "old-token") {
		t.Fatalf("points read sent %q, want the refreshed token", got)
	}

	fixture.host.mu.Lock()
	saved := make(map[string][]byte, len(fixture.host.saved))
	for name, storage := range fixture.host.saved {
		saved[name] = storage
	}
	fixture.host.mu.Unlock()
	if len(saved) != 1 {
		t.Fatalf("saved files = %d, want exactly 1", len(saved))
	}
	storage, ok := saved[raccoonLiveName]
	if !ok {
		t.Fatalf("saved names = %#v, want %q", saved, raccoonLiveName)
	}
	var stored Credential
	if errDecode := json.Unmarshal(storage, &stored); errDecode != nil {
		t.Fatalf("decode saved credential: %v", errDecode)
	}
	if stored.AccessToken == "old-token" || stored.AccessToken == "" {
		t.Fatalf("saved access token = %q, want the renewed one", stored.AccessToken)
	}
	if stored.Type != ProviderKey {
		t.Fatalf("saved type = %q, want the provider marker", stored.Type)
	}
}

// TestPageDoesNotRenewAValidCredential is the negative case a live instance is
// normally in: a credential with hours left must produce no renewal call.
func TestPageDoesNotRenewAValidCredential(t *testing.T) {
	fixture, host := newRaccoonFreshnessFixture(t,
		raccoonCredential("valid-token", time.Now().Add(6*time.Hour).UnixMilli()))

	quotas := collectAccountQuotas(host, raccoonAccounts(host), settings())
	if len(quotas) != 1 || quotas[0].Refresh.Refreshed || quotas[0].Refresh.Err != nil {
		t.Fatalf("quotas = %+v, want one untouched account", quotas)
	}
	if fixture.counts() != 0 {
		t.Fatalf("renewal calls = %d, want 0 for a valid credential", fixture.counts())
	}
	reads := fixture.requestsFor(PointsBalancePath)
	if len(reads) != 1 || !strings.Contains(bearerOf(reads[0]), "valid-token") {
		t.Fatalf("points reads = %+v, want one read with the stored token", reads)
	}
	if saved := fixture.savedCount(); saved != 0 {
		t.Fatalf("saved files = %d, want nothing written back", saved)
	}
}

// TestPageRenewsOnceForConcurrentCallers pins the single-flight.
func TestPageRenewsOnceForConcurrentCallers(t *testing.T) {
	fixture, host := newRaccoonFreshnessFixture(t,
		raccoonCredential("old-token", time.Now().Add(-time.Hour).UnixMilli()))
	fixture.answerWith(raccoonRefreshAnswer(raccoonJWT(time.Now().Add(12 * time.Hour))))

	var wg sync.WaitGroup
	for index := 0; index < 6; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = collectAccountQuotas(host, raccoonAccounts(host), settings())
		}()
	}
	wg.Wait()

	if got := fixture.counts(); got != 1 {
		t.Fatalf("renewal calls = %d, want 1 (single-flight)", got)
	}
}

// TestPageSurfacesARenewalFailureAndBacksOff pins the failure contract: a
// transport failure is reported as a renewal failure — never as an expired
// credential — and a second load inside the cooldown does not retry.
func TestPageSurfacesARenewalFailureAndBacksOff(t *testing.T) {
	fixture, host := newRaccoonFreshnessFixture(t,
		raccoonCredential("old-token", time.Now().Add(-time.Minute).UnixMilli()))
	fixture.failTransport()

	quotas := collectAccountQuotas(host, raccoonAccounts(host), settings())
	if len(quotas) != 1 || quotas[0].Refresh.Err == nil {
		t.Fatalf("quotas = %+v, want a recorded renewal failure", quotas)
	}
	message := quotas[0].Refresh.Err.Error()
	if strings.Contains(message, "已过期，请重新登录") {
		t.Fatalf("a transport failure was reported as a dead credential: %q", message)
	}
	if quotas[0].Credential == nil {
		t.Fatal("the credential was dropped over a renewal failure")
	}

	_ = collectAccountQuotas(host, raccoonAccounts(host), settings())
	if got := fixture.counts(); got != 1 {
		t.Fatalf("renewal calls inside the cooldown = %d, want 1", got)
	}
}

// TestPageMarksATerminalRenewalDeadWithoutRetrying pins the terminal half.
func TestPageMarksATerminalRenewalDeadWithoutRetrying(t *testing.T) {
	fixture, host := newRaccoonFreshnessFixture(t,
		raccoonCredential("old-token", time.Now().Add(-time.Hour).UnixMilli()))
	fixture.fail(http.StatusOK, fmt.Sprintf(`{"code":%d,"message":"session dead"}`, codeAuthorizationVerifyError))

	quotas := collectAccountQuotas(host, raccoonAccounts(host), settings())
	if len(quotas) != 1 || quotas[0].Refresh.Err == nil {
		t.Fatalf("quotas = %+v, want a recorded terminal failure", quotas)
	}
	if !authrefresh.IsTerminal(quotas[0].Refresh.Err) {
		t.Fatalf("renewal error = %v, want a terminal classification", quotas[0].Refresh.Err)
	}
	for load := 0; load < 3; load++ {
		_ = collectAccountQuotas(host, raccoonAccounts(host), settings())
	}
	if got := fixture.counts(); got != 1 {
		t.Fatalf("renewal calls for a dead credential = %d, want 1", got)
	}
}

// TestExecutorRenewsBeforeSigning pins the second trigger point.
func TestExecutorRenewsBeforeSigning(t *testing.T) {
	fixture, host := newRaccoonFreshnessFixture(t,
		raccoonCredential("old-token", time.Now().Add(-time.Hour).UnixMilli()))
	fixture.answerWith(raccoonRefreshAnswer(raccoonJWT(time.Now().Add(12 * time.Hour))))

	request := pluginapi.ExecutorRequest{
		AuthID:       raccoonLiveName,
		AuthProvider: ProviderKey,
		Model:        "raccoon-chat",
		StorageJSON:  raccoonCredential("old-token", time.Now().Add(-time.Hour).UnixMilli()),
		Payload:      json.RawMessage(`{"model":"raccoon-chat","messages":[{"role":"user","content":"hi"}]}`),
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	_, credential, errDecode := decodeExecutorCall(host, raw)
	if errDecode != nil {
		t.Fatalf("decodeExecutorCall: %v", errDecode)
	}
	if credential.AccessToken == "old-token" {
		t.Fatal("the executor kept the expired token")
	}
	if got := fixture.counts(); got != 1 {
		t.Fatalf("renewal calls = %d, want 1", got)
	}
}

// TestDashboardAccountReportsTheRenewal pins that the JSON says what the
// freshness check did.
func TestDashboardAccountReportsTheRenewal(t *testing.T) {
	fixture, host := newRaccoonFreshnessFixture(t,
		raccoonCredential("old-token", time.Now().Add(-time.Hour).UnixMilli()))
	fixture.answerWith(raccoonRefreshAnswer(raccoonJWT(time.Now().Add(12 * time.Hour))))

	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}})
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v", errDecode)
	}
	account, _ := document["account"].(map[string]any)
	if got := account["refreshed"]; got != true {
		t.Fatalf("account refreshed = %#v, want true (document %#v)", got, document)
	}
}

// TestCredentialExpiryUsesTheJWTFallback pins the load-bearing fallback: an
// imported credential with no `expires_at` still has an expiry, and a credential
// with neither source is left alone.
func TestCredentialExpiryUsesTheJWTFallback(t *testing.T) {
	expiry := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	imported := raccoonCredential(raccoonJWT(expiry), 0)
	resolved, ok := credentialExpiry(imported)
	if !ok {
		t.Fatal("credentialExpiry ignored the JWT exp claim")
	}
	if !resolved.Equal(expiry) {
		t.Fatalf("credentialExpiry = %v, want the JWT exp %v", resolved, expiry)
	}

	if _, ok := credentialExpiry(json.RawMessage(`{"access_token":"opaque","refresh_token":"r"}`)); ok {
		t.Fatal("credentialExpiry invented an expiry for a credential without one")
	}
	if credentialRefreshable(json.RawMessage(`{"access_token":"opaque"}`)) {
		t.Fatal("credentialRefreshable accepted a credential without a refresh token")
	}
}

// requestsFor returns every recorded request whose URL ends with suffix.
func (f *raccoonFreshnessFixture) requestsFor(suffix string) []abiboot.HTTPDoRequest {
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	out := make([]abiboot.HTTPDoRequest, 0, 2)
	for _, request := range f.host.requests {
		if strings.HasSuffix(request.URL, suffix) {
			out = append(out, request)
		}
	}
	return out
}

// savedCount is how many credentials were written back.
func (f *raccoonFreshnessFixture) savedCount() int {
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	return len(f.host.saved)
}

// bearerOf extracts the bearer token from a recorded request.
func bearerOf(request abiboot.HTTPDoRequest) string {
	for name, values := range request.Headers {
		if strings.EqualFold(name, "Authorization") && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
