package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file PINS the Loomy exception to the credential-freshness scheme the
// other providers got.
//
// Loomy has no refresh endpoint and its credentials carry no refresh token
// (`loomy.ts:195-205`), so there is nothing to renew: `auth.refresh` is a
// validity PROBE. Forcing a renewal here would either invent a protocol that
// does not exist or hammer the probe endpoint on every request, so the plugin
// deliberately does NOT wire the shared refresher, and these tests make that a
// checkable fact rather than a comment:
//
//   - an expired credential triggers NO renewal and NO write-back;
//   - the page still says it cannot renew, in the existing wording;
//   - the executor still does not probe or renew before inference — the server's
//     100002 stays the only authority.
//
// A future change that "helpfully" wires a refresher here has to delete these
// tests first, which is the point.

const (
	loomyLiveName  = "loomy-live.json"
	loomyLiveIndex = "idx-1"
	// loomyExpiredCredential is a REAL credential shape (`expires_at` in
	// milliseconds, in the past) whose session the local clock says is over.
	loomyPointsAnswer = `{"code":0,"data":{"balance":120,"daily_balance":30,"available":150}}`
)

// expiredLoomyCredential is a stored credential that expired an hour ago.
func expiredLoomyCredential(t *testing.T) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(&Credential{
		Type:        ProviderKey,
		AccessToken: "stale-token",
		Phone:       "13800138000",
		UserID:      "uid-1",
		ExpiresAt:   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	if errMarshal != nil {
		t.Fatalf("marshal credential: %v", errMarshal)
	}
	return raw
}

// loomyFreshnessHost wires one expired account and a points answer. Any request
// the tests do not expect fails the host transport.
func loomyFreshnessHost(t *testing.T) *fakeHost {
	t.Helper()
	host := newFakeHost()
	host.files = []pluginapi.HostAuthFileEntry{{
		ID:        loomyLiveName,
		AuthIndex: loomyLiveIndex,
		Name:      loomyLiveName,
		Provider:  ProviderKey,
		Type:      ProviderKey,
		Status:    "active",
	}}
	host.auths[loomyLiveIndex] = expiredLoomyCredential(t)
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if !strings.Contains(request.URL, PointsRecordsPath) {
			return nil, errFakeTransport
		}
		return httpResponse(200, loomyPointsAnswer), nil
	}
	host.install(t)
	return host
}

// TestExpiredCredentialIsNotRenewed is the core pin: with no renewal path
// whatsoever, an expired Loomy credential must produce the page's own read and
// NOTHING else — no renewal request, no write-back.
func TestExpiredCredentialIsNotRenewed(t *testing.T) {
	host := loomyFreshnessHost(t)

	quotas := collectAccountQuotas(testHost(), loomyAccounts(testHost()), settings())
	if len(quotas) != 1 {
		t.Fatalf("quotas = %d, want 1", len(quotas))
	}
	if quotas[0].CredentialErr != nil {
		t.Fatalf("credential error = %v", quotas[0].CredentialErr)
	}
	if quotas[0].Credential == nil {
		t.Fatal("the credential was dropped")
	}

	// Every recorded call must be the read-only points endpoint. A renewal —
	// or anything resembling one — would show up here as another URL.
	for _, request := range host.allCalls() {
		if !strings.Contains(request.URL, PointsRecordsPath) {
			t.Fatalf("an expired credential provoked a call to %s; Loomy has no renewal path", request.URL)
		}
	}
	if names := host.savedNames(); len(names) != 0 {
		t.Fatalf("saved names = %#v, want nothing written back: Loomy cannot renew", names)
	}
}

// allCalls returns every recorded outbound request.
func (f *fakeHost) allCalls() []abiboot.HTTPDoRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]abiboot.HTTPDoRequest(nil), f.requests...)
}

// loomyExecutorRequest builds an executor request carrying the expired
// credential.
func loomyExecutorRequest(t *testing.T) (json.RawMessage, error) {
	t.Helper()
	storage, errEncode := (&Credential{
		Type:        ProviderKey,
		AccessToken: "stale-token",
		Phone:       "13800138000",
		UserID:      "uid-1",
		ExpiresAt:   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}).Encode()
	if errEncode != nil {
		return nil, errEncode
	}
	return json.Marshal(pluginapi.ExecutorRequest{
		AuthID:       loomyLiveName,
		AuthProvider: ProviderKey,
		Model:        "loomy-chat",
		Format:       "chat-completions",
		SourceFormat: "chat-completions",
		Stream:       true,
		Payload:      []byte(`{"model":"loomy-chat","messages":[{"role":"user","content":"hi"}]}`),
		StorageJSON:  storage,
	})
}

// TestPageKeepsTheHonestNoRefreshWording pins the wording the user sees: the
// page must keep saying the credential cannot be renewed, and must never claim a
// renewal that did not happen.
func TestPageKeepsTheHonestNoRefreshWording(t *testing.T) {
	loomyFreshnessHost(t)

	response := statusJSON(testHost(), pluginapi.ManagementRequest{
		Query: map[string][]string{"format": {"json"}},
	})
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v", errDecode)
	}
	if got := document["refreshable"]; got != false {
		t.Fatalf("refreshable = %#v, want false", got)
	}
	for _, key := range []string{"refreshed", "refresh_error"} {
		if _, ok := document[key]; ok {
			t.Fatalf("document claims %q; Loomy cannot renew", key)
		}
	}

	page := renderStatusPage(testHost(), pluginapi.ManagementRequest{
		Query: map[string][]string{"auth_index": {loomyLiveIndex}},
	})
	body := string(page.Body)
	if !strings.Contains(body, "否（无 refresh_token，过期只能重新登录）") {
		t.Fatalf("the card lost its honest no-refresh wording:\n%s", body)
	}
}

// TestExecutorDoesNotProbeOrRenewBeforeInference pins the executor's half: the
// inference path signs with the credential it was given. Loomy's server (100002)
// is the only authority on whether a session is really dead, and there is no
// renewal to attempt.
func TestExecutorDoesNotProbeOrRenewBeforeInference(t *testing.T) {
	host := loomyFreshnessHost(t)
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, ChatCompletionsPath):
			return httpResponse(200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), nil
		case strings.Contains(request.URL, PointsRecordsPath):
			return httpResponse(200, loomyPointsAnswer), nil
		}
		return nil, errFakeTransport
	}

	request, errRequest := loomyExecutorRequest(t)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	if _, errHandler := handleExecutorExecute(testHost(), request); errHandler != nil {
		t.Fatalf("handleExecutorExecute: %v", errHandler)
	}

	chats := host.callsFor(ChatCompletionsPath)
	if len(chats) != 1 {
		t.Fatalf("chat calls = %d, want 1", len(chats))
	}
	if got := chats[0].Headers.Get("Authorization"); !strings.Contains(got, "stale-token") {
		t.Fatalf("chat call sent %q, want the stored credential untouched", got)
	}
	if names := host.savedNames(); len(names) != 0 {
		t.Fatalf("saved names = %#v, want nothing written back", names)
	}
}
