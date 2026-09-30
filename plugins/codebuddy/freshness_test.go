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
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file pins the self-healing contract of the CodeBuddy plugin: the status
// page, the quota route and the executor renew an expired credential themselves
// instead of waiting for a host timer that every restart resets. The same source
// tree builds codebuddy / codebuddy-intl / workbuddy-cn / workbuddy, so this one
// wiring covers every variant.
//
// Everything is offline. The fake host answers both the renewal and the billing
// endpoints, and the renewal rotates the access token — which is also how the
// tests prove the refreshed credential is what the read used: the billing call
// carries the new bearer token.
//
// The positive path cannot be produced live without waiting for a token to
// expire; a live credential is normally valid, which is the negative case
// TestPageDoesNotRenewAValidCredential pins here.

const (
	codebuddyLiveName  = "codebuddy-live.json"
	codebuddyLiveIndex = "idx-1"
	codebuddyBalance   = `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"CodeBuddy个人体验版",` +
		`"CapacityUnit":"credit","Status":0,"CycleCapacityRemain":247.87,"CycleCapacitySize":1000,"CycleCapacityUsed":752.13}]}}}}`
	codebuddyCheckin = `{"code":0,"data":{"active":true,"today_checked_in":false,"streak_days":0,"daily_credit":100,"activity_name":"每日签到"}}`
)

// codebuddyFreshnessFixture is the scripted host.
type codebuddyFreshnessFixture struct {
	mu sync.Mutex
	// refreshCalls counts renewal requests; refreshStatus/refreshBody script the
	// answer and refreshErr a transport failure.
	refreshCalls  int
	refreshStatus int
	refreshBody   string
	refreshErr    error
	// billingTokens records the Authorization header of each balance read.
	billingTokens []string
	// chatTokens records the Authorization header of each inference call.
	chatTokens []string
	// chatBody overrides the streamed chat answer; empty means the default frame.
	chatBody string
	// saved records what was written back through host.auth.save.
	saved map[string]json.RawMessage
}

// newCodebuddyFreshnessFixture wires one account and clears the process-wide
// freshness state, so a test never inherits another test's cooldown.
func newCodebuddyFreshnessFixture(t *testing.T, expiresAtMS int64) (*codebuddyFreshnessFixture, *abiboot.Host) {
	t.Helper()
	withCodebuddySettings(t, Config{Product: ProductCodeBuddy})
	credentialRefresher = newCredentialRefresher()
	t.Cleanup(func() { credentialRefresher = newCredentialRefresher() })

	fixture := &codebuddyFreshnessFixture{saved: map[string]json.RawMessage{}}
	stored := codebuddyCredential("old-token", expiresAtMS)
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return abiboot.OK(map[string]any{"files": []map[string]any{{
				"name": codebuddyLiveName, "id": codebuddyLiveName, "auth_index": codebuddyLiveIndex,
				"provider": ProviderKey, "type": ProviderKey, "status": "active",
			}}})
		case pluginabi.MethodHostAuthGet:
			return abiboot.OK(map[string]any{"auth_index": codebuddyLiveIndex, "name": codebuddyLiveName, "json": stored})
		case pluginabi.MethodHostAuthSave:
			var payload struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			if errDecode := json.Unmarshal(request, &payload); errDecode != nil {
				return nil, errDecode
			}
			fixture.mu.Lock()
			fixture.saved[payload.Name] = payload.JSON
			fixture.mu.Unlock()
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
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			switch {
			case strings.HasSuffix(payload.URL, AuthRefreshPath):
				fixture.refreshCalls++
				if fixture.refreshErr != nil {
					return nil, fixture.refreshErr
				}
				status := fixture.refreshStatus
				if status == 0 {
					status = http.StatusOK
				}
				return codebuddyHTTPResponse(status, fixture.refreshBody)
			case strings.HasSuffix(payload.URL, UserResourcePath):
				fixture.billingTokens = append(fixture.billingTokens, payload.Headers.Get("Authorization"))
				return codebuddyHTTPResponse(http.StatusOK, codebuddyBalance)
			case strings.HasSuffix(payload.URL, CheckinActivityStatusPath):
				return codebuddyHTTPResponse(http.StatusOK, codebuddyCheckin)
			case strings.Contains(payload.URL, "/v2/chat/"):
				fixture.chatTokens = append(fixture.chatTokens, payload.Headers.Get("Authorization"))
				body := fixture.chatBody
				if body == "" {
					body = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
				}
				return codebuddyHTTPResponse(http.StatusOK, body)
			}
			return nil, fmt.Errorf("unexpected upstream call %s", payload.URL)
		case pluginabi.MethodHostLog:
			return abiboot.OK(map[string]any{})
		}
		return nil, fmt.Errorf("unexpected host method %s", method)
	})
	t.Cleanup(abiboot.ClearHostCaller)
	return fixture, abiboot.NewHost(json.RawMessage(`{"host_callback_id":"codebuddy-freshness"}`))
}

func (f *codebuddyFreshnessFixture) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls, append([]string(nil), f.billingTokens...)
}

func (f *codebuddyFreshnessFixture) answerWith(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshStatus, f.refreshBody, f.refreshErr = http.StatusOK, body, nil
}

func (f *codebuddyFreshnessFixture) fail(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshStatus, f.refreshBody, f.refreshErr = status, body, nil
}

func (f *codebuddyFreshnessFixture) failTransport(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshStatus, f.refreshErr = 0, err
}

func (f *codebuddyFreshnessFixture) savedFiles() map[string]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]json.RawMessage, len(f.saved))
	for name, storage := range f.saved {
		out[name] = storage
	}
	return out
}

// codebuddyHTTPResponse wraps a canned upstream answer in the host envelope the
// plugin decodes.
func codebuddyHTTPResponse(status int, body string) ([]byte, error) {
	return abiboot.OK(map[string]any{"StatusCode": status, "Body": []byte(body)})
}

// codebuddyCredential builds a renewable credential expiring at `expiresAtMS`.
func codebuddyCredential(accessToken string, expiresAtMS int64) json.RawMessage {
	raw, _ := json.Marshal(&Credential{
		Type:         ProviderKey,
		AccessToken:  accessToken,
		RefreshToken: "refresh-token",
		ExpiresAt:    fmt.Sprintf("%d", expiresAtMS),
		UserID:       "uid-1",
		Nickname:     "live-user",
		Product:      string(ProductCodeBuddy),
	})
	return raw
}

// codebuddyRefreshAnswer is a renewal answer carrying a new access token.
func codebuddyRefreshAnswer(token string, expiresAtMS int64) string {
	return fmt.Sprintf(`{"code":0,"data":{"accessToken":%q,"refreshToken":"refresh-2","expiresAt":"%d"}}`, token, expiresAtMS)
}

func withCodebuddySettings(t *testing.T, cfg Config) {
	t.Helper()
	previous := settings()
	setSettings(cfg)
	t.Cleanup(func() { setSettings(previous) })
}

// TestPageRenewsAnExpiredCredential is the positive path: the expired credential
// is renewed exactly once before the read, the refreshed bytes are written back
// to the SAME auth file, and the read carries the NEW token.
func TestPageRenewsAnExpiredCredential(t *testing.T) {
	fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.answerWith(codebuddyRefreshAnswer("new-token", time.Now().Add(12*time.Hour).UnixMilli()))

	quotas := collectAccountQuotas(host, codebuddyAccounts(host))
	if len(quotas) != 1 {
		t.Fatalf("quotas = %d, want 1", len(quotas))
	}
	if quotas[0].CredentialErr != nil {
		t.Fatalf("credential error = %v", quotas[0].CredentialErr)
	}
	if !quotas[0].Refresh.Refreshed {
		t.Fatalf("refresh outcome = %+v, want a renewal", quotas[0].Refresh)
	}

	refreshCalls, billingTokens := fixture.counts()
	if refreshCalls != 1 {
		t.Fatalf("renewal calls = %d, want exactly 1", refreshCalls)
	}
	if len(billingTokens) != 1 || !strings.Contains(billingTokens[0], "new-token") {
		t.Fatalf("billing tokens = %#v, want the refreshed token", billingTokens)
	}

	saved := fixture.savedFiles()
	if len(saved) != 1 {
		t.Fatalf("saved files = %d, want exactly 1", len(saved))
	}
	storage, ok := saved[codebuddyLiveName]
	if !ok {
		t.Fatalf("saved names = %#v, want %q", saved, codebuddyLiveName)
	}
	var stored Credential
	if errDecode := json.Unmarshal(storage, &stored); errDecode != nil {
		t.Fatalf("decode saved credential: %v", errDecode)
	}
	if stored.AccessToken != "new-token" {
		t.Fatalf("saved access token = %q, want the renewed one", stored.AccessToken)
	}
	if stored.Type != ProviderKey {
		t.Fatalf("saved type = %q, want the provider marker", stored.Type)
	}
}

// TestPageDoesNotRenewAValidCredential is the negative case a live instance is
// normally in: a credential with hours left must produce no renewal call.
func TestPageDoesNotRenewAValidCredential(t *testing.T) {
	fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(6*time.Hour).UnixMilli())

	quotas := collectAccountQuotas(host, codebuddyAccounts(host))
	if len(quotas) != 1 || quotas[0].Refresh.Refreshed || quotas[0].Refresh.Err != nil {
		t.Fatalf("quotas = %+v, want one untouched account", quotas)
	}
	refreshCalls, billingTokens := fixture.counts()
	if refreshCalls != 0 {
		t.Fatalf("renewal calls = %d, want 0 for a valid credential", refreshCalls)
	}
	if len(billingTokens) != 1 || !strings.Contains(billingTokens[0], "old-token") {
		t.Fatalf("billing tokens = %#v, want one read with the stored token", billingTokens)
	}
	if saved := fixture.savedFiles(); len(saved) != 0 {
		t.Fatalf("saved files = %d, want nothing written back", len(saved))
	}
}

// TestPageRenewsOnceForConcurrentCallers pins the single-flight.
func TestPageRenewsOnceForConcurrentCallers(t *testing.T) {
	fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.answerWith(codebuddyRefreshAnswer("new-token", time.Now().Add(12*time.Hour).UnixMilli()))

	var wg sync.WaitGroup
	for index := 0; index < 6; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = collectAccountQuotas(host, codebuddyAccounts(host))
		}()
	}
	wg.Wait()

	if got, _ := fixture.counts(); got != 1 {
		t.Fatalf("renewal calls = %d, want 1 (single-flight)", got)
	}
}

// TestPageSurfacesARenewalFailureAndBacksOff pins the failure contract: a
// transport failure is reported as a renewal failure — never as an expired
// credential — and a second load inside the cooldown does not retry.
func TestPageSurfacesARenewalFailureAndBacksOff(t *testing.T) {
	fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(-time.Minute).UnixMilli())
	fixture.failTransport(fmt.Errorf("dial tcp 1.2.3.4:443: connect: network is unreachable"))

	quotas := collectAccountQuotas(host, codebuddyAccounts(host))
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

	_ = collectAccountQuotas(host, codebuddyAccounts(host))
	if got, _ := fixture.counts(); got != 1 {
		t.Fatalf("renewal calls inside the cooldown = %d, want 1", got)
	}
}

// TestPageMarksATerminalRenewalDeadWithoutRetrying pins the terminal half.
func TestPageMarksATerminalRenewalDeadWithoutRetrying(t *testing.T) {
	fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.fail(http.StatusUnauthorized, `{"code":401,"msg":"token expired"}`)

	quotas := collectAccountQuotas(host, codebuddyAccounts(host))
	if len(quotas) != 1 || quotas[0].Refresh.Err == nil {
		t.Fatalf("quotas = %+v, want a recorded terminal failure", quotas)
	}
	if !authrefresh.IsTerminal(quotas[0].Refresh.Err) {
		t.Fatalf("renewal error = %v, want a terminal classification", quotas[0].Refresh.Err)
	}
	for load := 0; load < 3; load++ {
		_ = collectAccountQuotas(host, codebuddyAccounts(host))
	}
	if got, _ := fixture.counts(); got != 1 {
		t.Fatalf("renewal calls for a dead credential = %d, want 1", got)
	}
}

// TestExecutorRenewsBeforeSigning pins the second trigger point.
func TestExecutorRenewsBeforeSigning(t *testing.T) {
	fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.answerWith(codebuddyRefreshAnswer("new-token", time.Now().Add(12*time.Hour).UnixMilli()))

	request := pluginapi.ExecutorRequest{
		AuthID:       codebuddyLiveName,
		AuthProvider: ProviderKey,
		Model:        "deepseek-v4.1-flash",
		StorageJSON:  codebuddyCredential("old-token", time.Now().Add(-time.Hour).UnixMilli()),
		Payload:      json.RawMessage(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`),
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	_, errExecute := handleExecutorExecute(host, raw)
	if errExecute != nil {
		t.Fatalf("handleExecutorExecute: %v", errExecute)
	}
	if got, _ := fixture.counts(); got != 1 {
		t.Fatalf("renewal calls = %d, want 1", got)
	}
	if len(fixture.chatTokens) != 1 || !strings.Contains(fixture.chatTokens[0], "new-token") {
		t.Fatalf("chat tokens = %#v, want the refreshed token", fixture.chatTokens)
	}
}

// TestExecutorRefusesADeadExpiredCredential pins that the executor reports the
// renewal failure instead of signing a doomed request.
func TestExecutorRefusesADeadExpiredCredential(t *testing.T) {
	fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(-time.Hour).UnixMilli())
	fixture.fail(http.StatusUnauthorized, `{"code":401,"msg":"token expired"}`)

	request := pluginapi.ExecutorRequest{
		AuthID:       codebuddyLiveName,
		AuthProvider: ProviderKey,
		Model:        "deepseek-v4.1-flash",
		StorageJSON:  codebuddyCredential("old-token", time.Now().Add(-time.Hour).UnixMilli()),
		Payload:      json.RawMessage(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`),
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	_, errExecute := handleExecutorExecute(host, raw)
	if errExecute == nil {
		t.Fatal("executor accepted an expired credential after a terminal renewal failure")
	}
	if !authrefresh.IsTerminal(errExecute) {
		t.Fatalf("executor error = %v, want a terminal classification", errExecute)
	}
}

// TestCredentialExpiryLeavesAnUndatableCredentialAlone pins that a credential
// with no parsable expiry is never renewed.
func TestCredentialExpiryLeavesAnUndatableCredentialAlone(t *testing.T) {
	if _, ok := credentialExpiry(json.RawMessage(`{"access_token":"tok","refresh_token":"r"}`)); ok {
		t.Fatal("credentialExpiry invented an expiry for an undatable credential")
	}
	if credentialRefreshable(json.RawMessage(`{"access_token":"tok"}`)) {
		t.Fatal("credentialRefreshable accepted a credential without a refresh token")
	}
}

// TestExecutorStreamStripsEmptyToolCalls is the end-to-end guard for the
// CodeBuddy wire convention that broke reasoning rendering downstream: every
// frame carries `"tool_calls":[]`, and the AI SDK's openai-compatible provider
// ends the active reasoning segment on any non-null `tool_calls`. One segment
// per token is what a client renders as one word per row.
//
// The frames below reproduce the real shape captured from the live channel.
func TestExecutorStreamStripsEmptyToolCalls(t *testing.T) {
	fixture, host := newCodebuddyFreshnessFixture(t, time.Now().Add(time.Hour).UnixMilli())
	fixture.chatBody = "data: " + `{"id":"cmb-1","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1790561792,"choices":[{"index":0,"delta":{"content":"","reasoning_content":"我们","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}` + "\n\n" +
		"data: " + `{"id":"cmb-1","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1790561792,"choices":[{"index":0,"delta":{"content":"","reasoning_content":"需要","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}` + "\n\n" +
		"data: " + `{"id":"cmb-1","choices":[{"index":0,"delta":{"content":"hi","reasoning_content":""},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	request := pluginapi.ExecutorRequest{
		AuthID:       codebuddyLiveName,
		AuthProvider: ProviderKey,
		Model:        "deepseek-v4.1-flash",
		StorageJSON:  codebuddyCredential("token", time.Now().Add(time.Hour).UnixMilli()),
		Payload:      json.RawMessage(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`),
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	value, errStream := handleExecutorExecuteStream(host, raw)
	if errStream != nil {
		t.Fatalf("handleExecutorExecuteStream: %v", errStream)
	}
	response := value.(executorStreamResponse)
	if len(response.Chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(response.Chunks))
	}
	reasoning := strings.Builder{}
	for index, chunk := range response.Chunks {
		payload := string(chunk.Payload)
		if strings.HasPrefix(payload, "data:") || strings.Contains(payload, "[DONE]") {
			t.Fatalf("chunk %d must stay bare: %q", index, payload)
		}
		if strings.Contains(payload, `"tool_calls"`) {
			t.Fatalf("chunk %d still carries tool_calls: %s", index, payload)
		}
		// A round trip through map[string]any would rewrite `created` into
		// scientific notation; the payload must be spliced, not re-marshalled.
		if index < 2 && !strings.Contains(payload, `"created":1790561792`) {
			t.Errorf("chunk %d lost its original number formatting: %s", index, payload)
		}
		var decoded struct {
			Choices []struct {
				Delta struct {
					ReasoningContent string `json:"reasoning_content"`
					Content          string `json:"content"`
					Refusal          string `json:"refusal"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if errUnmarshal := json.Unmarshal([]byte(payload), &decoded); errUnmarshal != nil {
			t.Fatalf("chunk %d is not JSON: %v (%s)", index, errUnmarshal, payload)
		}
		if index < 2 {
			reasoning.WriteString(decoded.Choices[0].Delta.ReasoningContent)
			if decoded.Choices[0].Delta.Content != "" {
				t.Errorf("chunk %d content changed: %q", index, decoded.Choices[0].Delta.Content)
			}
		}
	}
	if got := reasoning.String(); got != "我们需要" {
		t.Fatalf("reasoning = %q, want %q", got, "我们需要")
	}
}
