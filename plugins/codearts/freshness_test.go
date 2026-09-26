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

// This file pins the self-healing contract of the CodeArts plugin: the status
// page and the executor renew an expired credential themselves instead of
// waiting for a host timer that every restart resets.
//
// Everything is offline. The "vendor" is a fake host transport whose STS answer
// carries a NEW access key id, which is also how the tests prove the refreshed
// credential is what the balance read ends up using: the signed Authorization
// header names the access key it was signed with.
//
// The two live CodeArts credentials are currently valid, so
// TestStatusPageDoesNotRenewAValidCredential is the negative case the deployed
// instance is in; the positive path cannot be produced live without waiting for
// a token to expire, which is why it is proven here.

// freshnessHost is the fake host transport for the renewal tests.
type freshnessHost struct {
	mu sync.Mutex
	// stsCalls counts the renewal requests, keyed by nothing: one credential.
	stsCalls int
	// balanceCalls records the Authorization header of each balance read.
	balanceCalls []string
	// saved records what was written back through host.auth.save.
	saved []savedAuthFile
	// stsStatus/stsBody replace the STS answer when stsStatus is non-zero.
	stsStatus int
	stsBody   string
	// transportFailure makes the STS call fail the way host.http.do does when
	// the network is down.
	transportFailure bool
	// account is the credential the host hands over.
	account pluginapi.HostAuthFileEntry
	// credential is the stored credential JSON.
	credential json.RawMessage
}

type savedAuthFile struct {
	Name string          `json:"name"`
	JSON json.RawMessage `json:"json"`
}

// install registers the fake transport.
func (f *freshnessHost) install(t *testing.T) *abiboot.Host {
	t.Helper()
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case pluginabi.MethodHostAuthList:
			return abiboot.OK(map[string]any{"files": []map[string]any{{
				"name":       f.account.Name,
				"id":         f.account.ID,
				"auth_index": f.account.AuthIndex,
				"provider":   ProviderKey,
				"type":       ProviderKey,
				"status":     "active",
			}}})
		case pluginabi.MethodHostAuthGet:
			return abiboot.OK(map[string]any{
				"auth_index": f.account.AuthIndex,
				"name":       f.account.Name,
				"json":       f.credential,
			})
		case pluginabi.MethodHostAuthSave:
			var payload savedAuthFile
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			f.saved = append(f.saved, payload)
			return abiboot.OK(map[string]any{"name": payload.Name, "path": "/auths/" + payload.Name})
		case pluginabi.MethodHostHTTPDo:
			var payload struct {
				URL     string      `json:"url"`
				Headers http.Header `json:"headers"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			switch {
			case payload.URL == STSTokenEndpoint:
				f.stsCalls++
				if f.transportFailure {
					return nil, fmt.Errorf("dial tcp 1.2.3.4:443: connect: network is unreachable")
				}
				status := f.stsStatus
				body := f.stsBody
				if status == 0 {
					status = http.StatusOK
				}
				return abiboot.OK(map[string]any{
					"StatusCode": status,
					"Body":       base64.StdEncoding.EncodeToString([]byte(body)),
				})
			case strings.Contains(payload.URL, PackageInfoPath):
				f.balanceCalls = append(f.balanceCalls, payload.Headers.Get("Authorization"))
				return abiboot.OK(map[string]any{
					"StatusCode": http.StatusOK,
					"Body": base64.StdEncoding.EncodeToString([]byte(
						`{"package":{"is_credit_package":true},"metrics":[{"name":"usageTotalPackageCredit","package_credit_amount":100,"package_credit_used":40,"package_credit_remain":60}]}`)),
				})
			case strings.Contains(payload.URL, OpsDeliveryPath):
				return abiboot.OK(map[string]any{
					"StatusCode": http.StatusOK,
					"Body":       base64.StdEncoding.EncodeToString([]byte(`{"code":0,"message":"ok","data":{"items":[]}}`)),
				})
			}
			return nil, fmt.Errorf("unexpected upstream call %s", payload.URL)
		case pluginabi.MethodHostLog:
			return abiboot.OK(map[string]any{})
		}
		return nil, fmt.Errorf("unexpected host method %s", method)
	})
	t.Cleanup(abiboot.ClearHostCaller)
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"freshness-callback"}`))
}

func (f *freshnessHost) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stsCalls, len(f.balanceCalls)
}

func (f *freshnessHost) savedFiles() []savedAuthFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]savedAuthFile(nil), f.saved...)
}

// renewableCredential builds a credential that can be renewed, with the given
// expiry and access key id.
func renewableCredential(t *testing.T, accessKey string, expiry time.Time) json.RawMessage {
	t.Helper()
	pair, errPair := newDpopKeyPair()
	if errPair != nil {
		t.Fatalf("generate DPoP key: %v", errPair)
	}
	jwk := pair.privateJWK()
	raw, errMarshal := json.Marshal(&Credential{
		Type:              ProviderKey,
		AccessKeyID:       accessKey,
		SecretAccessKey:   "secret-" + accessKey,
		SecurityToken:     "token-" + accessKey,
		ExpiresAt:         expiry.UTC().Format(time.RFC3339),
		UserName:          "live-user",
		RefreshToken:      "refresh-token",
		CodeVerifier:      "code-verifier",
		DpopPrivateKeyJWK: &jwk,
	})
	if errMarshal != nil {
		t.Fatalf("encode credential: %v", errMarshal)
	}
	return raw
}

// stsSuccessBody is the vendor's answer to a renewal.
func stsSuccessBody(accessKey string, expiry time.Time) string {
	return fmt.Sprintf(
		`{"credentials":{"access_key_id":%q,"secret_access_key":%q,"security_token":%q,"expiration":%q}}`,
		accessKey, "secret-"+accessKey, "token-"+accessKey, expiry.UTC().Format("2006-01-02T15:04:05Z"))
}

// newFreshnessFixture wires one account whose credential expires at `expiry`.
//
// The freshness state is rebuilt for every test: it is process-wide on purpose
// (that is what makes the cooldown and the single-flight work), so one test
// must never inherit another's clock.
func newFreshnessFixture(t *testing.T, accessKey string, expiry time.Time) (*freshnessHost, *abiboot.Host) {
	t.Helper()
	credentialRefresher = newCredentialRefresher()
	t.Cleanup(func() { credentialRefresher = newCredentialRefresher() })
	host := &freshnessHost{
		account: pluginapi.HostAuthFileEntry{
			ID:        "codearts-live.json",
			AuthIndex: "idx-1",
			Name:      "codearts-live.json",
			Provider:  ProviderKey,
			Type:      ProviderKey,
			Status:    "active",
		},
	}
	host.credential = renewableCredential(t, accessKey, expiry)
	return host, host.install(t)
}

// freshnessStatusRequest is a page view of the status document.
func freshnessStatusRequest() pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}}
}

// TestStatusPageRenewsAnExpiredCredential is the positive path: the expired
// credential is renewed exactly once before the balance is read, the refreshed
// bytes are written back to the SAME auth file, and the balance read is signed
// with the NEW access key — proof that the refreshed credential is what got
// used, not merely what got stored.
func TestStatusPageRenewsAnExpiredCredential(t *testing.T) {
	host, handle := newFreshnessFixture(t, "HSTAOLDKEY", time.Now().Add(-time.Hour))
	host.stsBody = stsSuccessBody("HSTANEWKEY", time.Now().Add(12*time.Hour))

	document := decodeDocument(t, statusJSON(handle, freshnessStatusRequest()))

	stsCalls, balanceCalls := host.counts()
	if stsCalls != 1 {
		t.Fatalf("STS renewal calls = %d, want exactly 1", stsCalls)
	}
	if balanceCalls != 1 {
		t.Fatalf("balance reads = %d, want 1", balanceCalls)
	}
	if got := document["refreshed"]; got != true {
		t.Errorf("document refreshed = %#v, want true", got)
	}
	if got := document["remaining"]; got != 60.0 {
		t.Errorf("document remaining = %#v, want the balance the renewed credential read", got)
	}

	saved := host.savedFiles()
	if len(saved) != 1 {
		t.Fatalf("saved files = %d, want exactly 1", len(saved))
	}
	if saved[0].Name != "codearts-live.json" {
		t.Fatalf("saved name = %q, want the auth file the host knows", saved[0].Name)
	}
	var stored Credential
	if errDecode := json.Unmarshal(saved[0].JSON, &stored); errDecode != nil {
		t.Fatalf("decode saved credential: %v", errDecode)
	}
	if stored.AccessKeyID != "HSTANEWKEY" {
		t.Fatalf("saved access key = %q, want the renewed one", stored.AccessKeyID)
	}
	if stored.Type != ProviderKey {
		t.Fatalf("saved type = %q, want the provider marker %q", stored.Type, ProviderKey)
	}

	host.mu.Lock()
	signedWith := append([]string(nil), host.balanceCalls...)
	host.mu.Unlock()
	if !strings.Contains(signedWith[0], "HSTANEWKEY") {
		t.Fatalf("balance read was signed with %q, want the refreshed access key", signedWith[0])
	}
}

// TestStatusPageDoesNotRenewAValidCredential is the negative case — the one the
// deployed instance is in right now: a credential with hours left must produce
// NO renewal call at all.
func TestStatusPageDoesNotRenewAValidCredential(t *testing.T) {
	host, handle := newFreshnessFixture(t, "HSTAVALIDKEY", time.Now().Add(6*time.Hour))

	document := decodeDocument(t, statusJSON(handle, freshnessStatusRequest()))

	stsCalls, balanceCalls := host.counts()
	if stsCalls != 0 {
		t.Fatalf("STS renewal calls = %d, want 0 for a valid credential", stsCalls)
	}
	if balanceCalls != 1 {
		t.Fatalf("balance reads = %d, want 1", balanceCalls)
	}
	if _, ok := document["refreshed"]; ok {
		t.Error("document claims a renewal that never happened")
	}
	if got := host.savedFiles(); len(got) != 0 {
		t.Fatalf("saved files = %d, want 0", len(got))
	}
}

// TestStatusPageRenewsOnceForConcurrentCallers pins the single-flight: two page
// loads and an inference request arriving together must cost the vendor one
// renewal.
func TestStatusPageRenewsOnceForConcurrentCallers(t *testing.T) {
	host, handle := newFreshnessFixture(t, "HSTAOLDKEY", time.Now().Add(-time.Hour))
	host.stsBody = stsSuccessBody("HSTANEWKEY", time.Now().Add(12*time.Hour))

	const callers = 6
	var wg sync.WaitGroup
	for index := 0; index < callers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = statusJSON(handle, freshnessStatusRequest())
		}()
	}
	wg.Wait()

	stsCalls, _ := host.counts()
	if stsCalls != 1 {
		t.Fatalf("STS renewal calls = %d, want 1 (single-flight)", stsCalls)
	}
	if saved := host.savedFiles(); len(saved) != 1 {
		t.Fatalf("saved files = %d, want 1", len(saved))
	}
}

// TestStatusPageSurfacesARenewalFailureAndBacksOff pins the failure contract: a
// transport failure is reported as a renewal failure (never as an expired
// credential), the page still reads the balance with the credential it has, and
// a second load inside the cooldown does not call the vendor again.
func TestStatusPageSurfacesARenewalFailureAndBacksOff(t *testing.T) {
	host, handle := newFreshnessFixture(t, "HSTAOLDKEY", time.Now().Add(-time.Minute))
	host.transportFailure = true

	document := decodeDocument(t, statusJSON(handle, freshnessStatusRequest()))

	refreshError, _ := document["refresh_error"].(string)
	if refreshError == "" {
		t.Fatalf("document has no refresh_error: %#v", document)
	}
	if strings.Contains(refreshError, "已失效") || strings.Contains(refreshError, "expired") {
		t.Fatalf("a transport failure was reported as an expired credential: %q", refreshError)
	}
	if !strings.Contains(refreshError, "network is unreachable") {
		t.Fatalf("refresh_error = %q, want the transport reason", refreshError)
	}
	if _, ok := document["refreshed"]; ok {
		t.Error("document claims a renewal that failed")
	}

	// The credential is still read, and the balance is reported: the token is
	// only a minute past its expiry, and the upstream is the authority on it.
	stsCalls, balanceCalls := host.counts()
	if stsCalls != 1 || balanceCalls != 1 {
		t.Fatalf("sts=%d balance=%d, want one attempt each", stsCalls, balanceCalls)
	}

	// Second load, inside the cooldown: the failure is reported again and the
	// vendor is left alone. This is what "must not spin" means.
	second := decodeDocument(t, statusJSON(handle, freshnessStatusRequest()))
	if got, _ := second["refresh_error"].(string); got == "" {
		t.Fatal("the second load lost the recorded renewal failure")
	}
	stsCalls, _ = host.counts()
	if stsCalls != 1 {
		t.Fatalf("STS renewal calls inside the cooldown = %d, want 1", stsCalls)
	}
}

// TestStatusPageMarksATerminalRenewalDeadWithoutRetrying pins the terminal half:
// a 401-style renewal failure stops the attempts and keeps saying why, so a
// credential that needs a new login is not hammered on every page load.
func TestStatusPageMarksATerminalRenewalDeadWithoutRetrying(t *testing.T) {
	host, handle := newFreshnessFixture(t, "HSTAOLDKEY", time.Now().Add(-time.Hour))
	host.stsStatus = http.StatusBadRequest
	host.stsBody = `{"error":"invalid_grant","error_code":"ExpiredRefreshToken","error_msg":"refresh token expired"}`

	document := decodeDocument(t, statusJSON(handle, freshnessStatusRequest()))
	refreshError, _ := document["refresh_error"].(string)
	if !strings.Contains(refreshError, "重新登录") {
		t.Fatalf("refresh_error = %q, want the terminal re-login message", refreshError)
	}
	// No figure is claimed as renewed: whatever the vendor answers next belongs
	// to a credential the provider has already declared dead, and the user has
	// to be told to log in again rather than shown a renewal that never was.
	if got, ok := document["refreshed"]; ok {
		t.Fatalf("document claims a renewal: %#v", got)
	}

	for load := 0; load < 3; load++ {
		_ = statusJSON(handle, freshnessStatusRequest())
	}
	if stsCalls, _ := host.counts(); stsCalls != 1 {
		t.Fatalf("STS renewal calls for a dead credential = %d, want 1", stsCalls)
	}
}

// TestExecutorRenewsBeforeSigning pins the second trigger point: the inference
// path renews the credential before it signs anything, so an expired token can
// never reach the gateway.
func TestExecutorRenewsBeforeSigning(t *testing.T) {
	host, handle := newFreshnessFixture(t, "HSTAOLDKEY", time.Now().Add(-time.Hour))
	host.stsBody = stsSuccessBody("HSTANEWKEY", time.Now().Add(12*time.Hour))

	request := pluginapi.ExecutorRequest{
		AuthID:       "codearts-live.json",
		AuthProvider: ProviderKey,
		Model:        "glm-5.3-flash",
		StorageJSON:  host.credential,
		Payload:      json.RawMessage(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`),
	}
	// The chat call itself is not the subject here; the fake transport answers
	// the renewal and whatever the executor then does is irrelevant to the
	// assertion below.
	_, _ = handleExecutorExecute(handle, mustMarshal(t, request))

	stsCalls, _ := host.counts()
	if stsCalls != 1 {
		t.Fatalf("STS renewal calls = %d, want the executor to have renewed once", stsCalls)
	}
	saved := host.savedFiles()
	if len(saved) != 1 || saved[0].Name != "codearts-live.json" {
		t.Fatalf("saved = %#v, want the refreshed credential in the host's file", saved)
	}
}

// TestCredentialExpiryIgnoresTheDisplayFallback pins that an undatable
// credential is left alone: Expiry() falls back to "24 hours from now" for
// display, and the freshness check must not treat that as a real expiry.
func TestCredentialExpiryIgnoresTheDisplayFallback(t *testing.T) {
	credential, errParse := ParseCredential([]byte(`{
		"type":"codearts","access_key_id":"HSTAX","secret_access_key":"secret",
		"expires_at":"not-a-timestamp","refresh_token":"refresh","code_verifier":"v",
		"dpop_private_key_jwk":{"kty":"EC","crv":"P-256","x":"x","y":"y","d":"d"}
	}`))
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if _, ok := credential.ParsedExpiry(); ok {
		t.Fatal("ParsedExpiry accepted an unreadable timestamp")
	}
	if credential.Expiry().Before(time.Now().Add(23 * time.Hour)) {
		t.Fatal("Expiry() lost its documented 24-hour display fallback")
	}
	if credential, errParse := ParseCredential([]byte(fmt.Sprintf(`{
		"type":"codearts","access_key_id":"HSTAX","secret_access_key":"secret",
		"expires_at":%q
	}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))); errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	} else if expiry, ok := credential.ParsedExpiry(); !ok || expiry.Before(time.Now()) {
		t.Fatalf("ParsedExpiry = %v/%v, want the credential's own expiry", expiry, ok)
	}
}

// TestExecutorSkipsADeadCredentialHonestly pins that the executor reports the
// renewal failure (not a vendor "token expired") when the credential is expired
// and the renewal was terminal.
func TestExecutorSkipsADeadCredentialHonestly(t *testing.T) {
	host, handle := newFreshnessFixture(t, "HSTAOLDKEY", time.Now().Add(-time.Hour))
	host.stsStatus = http.StatusBadRequest
	host.stsBody = `{"error":"invalid_grant","error_code":"ExpiredRefreshToken","error_msg":"refresh token expired"}`

	request := pluginapi.ExecutorRequest{
		AuthID:       "codearts-live.json",
		AuthProvider: ProviderKey,
		Model:        "glm-5.3-flash",
		StorageJSON:  host.credential,
		Payload:      json.RawMessage(`{"model":"glm-5.3-flash","messages":[]}`),
	}
	_, errExecute := handleExecutorExecute(handle, mustMarshal(t, request))
	if errExecute == nil {
		t.Fatal("executor accepted an expired credential after a terminal renewal failure")
	}
	if !strings.Contains(errExecute.Error(), "重新登录") {
		t.Fatalf("executor error = %q, want the renewal failure", errExecute)
	}
	if !authrefresh.IsTerminal(errExecute) {
		t.Fatalf("executor error = %v, want a terminal classification so the host can retire it", errExecute)
	}
}

func mustMarshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	return raw
}
