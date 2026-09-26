package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The header matrix, the QR poll truth table and the refresh merge rules. These
// are the three places a faithful port usually drifts, and every deviation is
// invisible until a user hits it.

// TestAuthEndpointsSendOnlyContentType pins `postJson`: the auth family carries
// NO Accept, NO Authorization and NO X-Org-Code (`raccoon-oauth.ts:83-96`).
func TestAuthEndpointsSendOnlyContentType(t *testing.T) {
	fake := newFakeHost()
	credential := sampleCredential(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, RefreshPath) {
			return httpResponse(http.StatusOK, `{"code":0,"data":{"access_token":"`+fakeJWT(t, time.Now().Add(3*time.Hour))+`"}}`), nil
		}
		return httpResponse(http.StatusOK, `{"code":0,"data":{"status":"pending"}}`), nil
	}
	fake.install(t)

	pollQRCodeOnce(testHost(), DefaultConfig(), strings.Repeat("a", 32))
	if _, errRefresh := refreshCredential(testHost(), DefaultConfig(), credential); errRefresh != nil {
		t.Fatalf("refresh: %v", errRefresh)
	}

	calls := append(fake.callsFor(QRLoginCodePath), fake.callsFor(RefreshPath)...)
	if len(calls) != 2 {
		t.Fatalf("recorded %d auth calls, want 2", len(calls))
	}
	for _, call := range calls {
		if len(call.Headers) != 1 {
			t.Errorf("%s sent %d headers (%v), want only Content-Type", call.URL, len(call.Headers), call.Headers)
		}
		if got := call.Headers.Get("Content-Type"); got != "application/json" {
			t.Errorf("%s Content-Type = %q", call.URL, got)
		}
	}
}

// TestHeaderMatrixPerEndpoint pins the per-endpoint table from the spec.
func TestHeaderMatrixPerEndpoint(t *testing.T) {
	fake := newFakeHost()
	credential := sampleCredential(t)
	// A PERSONAL account has no office identity, and the header must still be
	// sent — as an empty string.
	credential.OfficeIdentity = ""
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, ChatCompletionsPath):
			return httpResponse(http.StatusOK, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
		case strings.Contains(request.URL, ModelCatalogPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"categories":[]}}`), nil
		case strings.Contains(request.URL, UserInfoPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"id":"u1"}}`), nil
		default:
			return httpResponse(http.StatusOK, `{"code":0,"data":{"available_points":7}}`), nil
		}
	}
	fake.install(t)

	host := testHost()
	if _, errInfo := fetchUserInfo(host, DefaultConfig(), credential); errInfo != nil {
		t.Fatalf("user_info: %v", errInfo)
	}
	if _, errBalance := fetchBalance(host, credential, DefaultConfig()); errBalance != nil {
		t.Fatalf("balance: %v", errBalance)
	}
	if _, errDo := performInfer(host, executorRequest(t, credential, `{"model":"sn-kimi-k3","messages":[{"role":"user","content":"hi"}]}`), credential); errDo != nil {
		t.Fatalf("chat: %v", errDo)
	}
	discoverModels(host, credential, DefaultConfig())

	cases := []struct {
		name        string
		match       string
		accept      string
		contentType string
		platform    string
		version     string
		authorized  bool
	}{
		{name: "user_info", match: UserInfoPath, accept: "application/json", contentType: "application/json", authorized: true},
		{name: "model_catalog", match: ModelCatalogPath, accept: "application/json", contentType: "", authorized: true},
		{name: "chat", match: ChatCompletionsPath, accept: "text/event-stream", contentType: "application/json", platform: ClientPlatform, authorized: true},
		{name: "credits", match: PointsBalancePath, accept: "application/json", contentType: "application/json", platform: ClientPlatform, version: ClientVersion, authorized: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			calls := fake.callsFor(testCase.match)
			if len(calls) == 0 {
				t.Fatalf("no call recorded for %s", testCase.match)
			}
			headers := calls[len(calls)-1].Headers
			if got := headers.Get("Accept"); got != testCase.accept {
				t.Errorf("Accept = %q, want %q", got, testCase.accept)
			}
			// Content-Type is compared through the map because its ABSENCE is
			// the assertion for the catalogue.
			if got := headers.Get("Content-Type"); got != testCase.contentType {
				t.Errorf("Content-Type = %q, want %q", got, testCase.contentType)
			}
			if got := headers.Get("X-Client-Platform"); got != testCase.platform {
				t.Errorf("X-Client-Platform = %q, want %q", got, testCase.platform)
			}
			if got := headers.Get("X-Client-Version"); got != testCase.version {
				t.Errorf("X-Client-Version = %q, want %q", got, testCase.version)
			}
			if got := headers.Get("Authorization"); (got != "") != testCase.authorized {
				t.Errorf("Authorization = %q, want authorized=%v", got, testCase.authorized)
			}
			if got := headers.Get("X-Raccoon-Language"); got != Language {
				t.Errorf("X-Raccoon-Language = %q, want %q", got, Language)
			}
			// ⚠️ X-Org-Code must be PRESENT even when empty: the reference
			// always sets it and personal accounts have no office identity.
			if _, present := headers["X-Org-Code"]; !present {
				t.Errorf("X-Org-Code is absent; it must be sent even when empty")
			}
			if got := headers.Get("X-Org-Code"); got != "" {
				t.Errorf("X-Org-Code = %q, want the empty office identity", got)
			}
		})
	}
	// A non-empty office identity is passed through verbatim (`raccoon.ts:280`).
	organisation := sampleCredential(t)
	organisation.OfficeIdentity = "org-42"
	organisation.Nickname = "OrgUser"
	organisation.UserID = "user-org"
	organisation.Phone = "13700137000"
	fake.requests = nil
	if _, errBalance := fetchBalance(host, organisation, DefaultConfig()); errBalance != nil {
		t.Fatalf("organisation balance: %v", errBalance)
	}
	orgCalls := fake.callsFor(PointsBalancePath)
	if len(orgCalls) != 1 {
		t.Fatalf("recorded %d organisation calls, want 1", len(orgCalls))
	}
	if got := orgCalls[0].Headers.Get("X-Org-Code"); got != "org-42" {
		t.Errorf("X-Org-Code = %q, want org-42", got)
	}

	// `userAgent` and `device_id` are dead configuration and must never appear.
	for _, call := range fake.requests {
		if value := call.Headers.Get("User-Agent"); value != "" {
			t.Errorf("%s sent User-Agent %q; the reference never sends one", call.URL, value)
		}
		if value := call.Headers.Get("X-Client-Device-ID"); value != "" {
			t.Errorf("%s sent X-Client-Device-ID %q; it is never populated", call.URL, value)
		}
	}
}

// TestQRPollStatusTruthTable pins the full mapping from `raccoon-oauth.ts:152-179`.
func TestQRPollStatusTruthTable(t *testing.T) {
	jwt := fakeJWT(t, time.Now().Add(3*time.Hour))
	cases := []struct {
		name       string
		status     int
		body       string
		transport  bool
		wantStatus string
		wantToken  bool
	}{
		{name: "pending", status: 200, body: `{"code":0,"data":{"status":"pending"}}`, wantStatus: qrStatusPending},
		// The live server really does answer the literal string `pending`, so a
		// known status must never be described as unknown.
		{name: "logging keeps expired_at", status: 200, body: `{"code":0,"data":{"status":"logging","expired_at":"2026-09-26 22:00:00"}}`, wantStatus: qrStatusLogging},
		{name: "logging", status: 200, body: `{"code":0,"data":{"status":"logging","expired_at":"2026-09-26 22:00:00"}}`, wantStatus: qrStatusLogging},
		{name: "canceled", status: 200, body: `{"code":0,"data":{"status":"canceled"}}`, wantStatus: qrStatusCanceled},
		{name: "success", status: 200, body: `{"code":0,"data":{"status":"success","access_token":"` + jwt + `","refresh_token":"rt"}}`, wantStatus: qrStatusSuccess, wantToken: true},
		{name: "success without a token", status: 200, body: `{"code":0,"data":{"status":"success"}}`, wantStatus: qrStatusPending},
		{name: "success with an empty token", status: 200, body: `{"code":0,"data":{"status":"success","access_token":""}}`, wantStatus: qrStatusPending},
		{name: "unknown status", status: 200, body: `{"code":0,"data":{"status":"teleporting"}}`, wantStatus: qrStatusPending},
		{name: "missing status", status: 200, body: `{"code":0,"data":{}}`, wantStatus: qrStatusPending},
		{name: "non-string status", status: 200, body: `{"code":0,"data":{"status":7}}`, wantStatus: qrStatusPending},
		{name: "business failure", status: 200, body: `{"code":100002,"message":"params invalid"}`, wantStatus: qrStatusPending},
		{name: "missing data", status: 200, body: `{"code":0}`, wantStatus: qrStatusPending},
		{name: "gateway error", status: 502, body: `{"message":"bad gateway"}`, wantStatus: qrStatusPending},
		{name: "network error", transport: true, wantStatus: qrStatusPending},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				if testCase.transport {
					return nil, errFakeTransport
				}
				return httpResponse(testCase.status, testCase.body), nil
			}
			fake.install(t)

			outcome := pollQRCodeOnce(testHost(), DefaultConfig(), strings.Repeat("b", 32))
			if outcome.Status != testCase.wantStatus {
				t.Fatalf("status = %q, want %q (detail %q)", outcome.Status, testCase.wantStatus, outcome.Detail)
			}
			if (outcome.Credential != nil) != testCase.wantToken {
				t.Fatalf("credential present = %v, want %v", outcome.Credential != nil, testCase.wantToken)
			}
			if testCase.wantStatus == qrStatusPending && !testCase.transport &&
				strings.Contains(testCase.body, `"status":"pending"`) &&
				strings.Contains(outcome.Detail, "未知状态") {
				t.Errorf("a literal `pending` status was described as unknown: %q", outcome.Detail)
			}
			if strings.Contains(testCase.body, `"status":"logging"`) && outcome.ExpiredAt != "2026-09-26 22:00:00" {
				t.Errorf("expired_at = %q, want the server value carried through", outcome.ExpiredAt)
			}
			if testCase.wantToken {
				if outcome.Credential.RefreshToken != "rt" {
					t.Errorf("refresh_token = %q, want rt", outcome.Credential.RefreshToken)
				}
				if outcome.Credential.ExpiresAt == "" {
					t.Error("expires_at must be derived from the JWT when it decodes")
				}
			}
		})
	}
}

// TestQRPollRequestShape pins the request body and that the code travels
// verbatim.
func TestQRPollRequestShape(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"code":0,"data":{"status":"pending"}}`), nil
	}
	fake.install(t)

	code := "0123456789abcdef0123456789abcdef"
	pollQRCodeOnce(testHost(), DefaultConfig(), code)
	calls := fake.callsFor(QRLoginCodePath)
	if len(calls) != 1 {
		t.Fatalf("recorded %d polls, want 1", len(calls))
	}
	if calls[0].Method != http.MethodPost {
		t.Errorf("method = %q, want POST", calls[0].Method)
	}
	var body map[string]string
	if errUnmarshal := json.Unmarshal(calls[0].Body, &body); errUnmarshal != nil {
		t.Fatalf("decode poll body: %v", errUnmarshal)
	}
	if body["qrcode_code"] != code {
		t.Errorf("qrcode_code = %q, want %q", body["qrcode_code"], code)
	}
}

// TestRefreshMergeSemantics pins trap #14: the old refresh token survives a
// response that omits it, and every other field is preserved.
func TestRefreshMergeSemantics(t *testing.T) {
	expiry := time.Now().Add(3 * time.Hour)
	current := &Credential{
		AccessToken:    fakeJWT(t, time.Now().Add(-time.Minute)),
		RefreshToken:   "refresh-old",
		OfficeIdentity: "personal",
		UserID:         "user-alice",
		Nickname:       "RaccoonAva",
		Phone:          "13800138000",
		Type:           ProviderKey,
	}
	newToken := fakeJWT(t, expiry)

	t.Run("omitted refresh token is kept", func(t *testing.T) {
		fake := newFakeHost()
		fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(http.StatusOK, `{"code":0,"data":{"access_token":"`+newToken+`"}}`), nil
		}
		fake.install(t)
		refreshed, errRefresh := refreshCredential(testHost(), DefaultConfig(), current)
		if errRefresh != nil {
			t.Fatalf("refresh: %v", errRefresh)
		}
		if refreshed.RefreshToken != "refresh-old" {
			t.Errorf("refresh_token = %q, want the old one preserved", refreshed.RefreshToken)
		}
		if refreshed.UserID != "user-alice" || refreshed.Nickname != "RaccoonAva" || refreshed.Phone != "13800138000" {
			t.Errorf("refresh dropped credential fields: %+v", refreshed)
		}
		if refreshed.AccessToken != newToken {
			t.Error("access_token was not replaced")
		}
		if refreshed.ExpiresAt == "" {
			t.Error("expires_at must be recomputed from the new JWT")
		}
		if current.AccessToken == refreshed.AccessToken {
			t.Error("the original credential must not be mutated in place")
		}
	})

	t.Run("rotated refresh token wins", func(t *testing.T) {
		fake := newFakeHost()
		fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(http.StatusOK, `{"code":0,"data":{"access_token":"`+newToken+`","refresh_token":"refresh-new"}}`), nil
		}
		fake.install(t)
		refreshed, errRefresh := refreshCredential(testHost(), DefaultConfig(), current)
		if errRefresh != nil {
			t.Fatalf("refresh: %v", errRefresh)
		}
		if refreshed.RefreshToken != "refresh-new" {
			t.Errorf("refresh_token = %q, want the rotated one", refreshed.RefreshToken)
		}
	})

	t.Run("undecodable token omits expires_at", func(t *testing.T) {
		fake := newFakeHost()
		fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(http.StatusOK, `{"code":0,"data":{"access_token":"opaque"}}`), nil
		}
		fake.install(t)
		refreshed, errRefresh := refreshCredential(testHost(), DefaultConfig(), current)
		if errRefresh != nil {
			t.Fatalf("refresh: %v", errRefresh)
		}
		if refreshed.ExpiresAt != "" {
			t.Errorf("expires_at = %q, want it omitted", refreshed.ExpiresAt)
		}
	})
}

// TestRefreshTerminalFailures pins that 401 and code 200003 are terminal and map
// to a 401 so the host can retire the credential (`raccoon-oauth.ts:299-301`).
func TestRefreshTerminalFailures(t *testing.T) {
	current := sampleCredential(t)
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "http 401", status: http.StatusUnauthorized, body: `{"message":"unauthorized"}`},
		{name: "code 200003", status: http.StatusOK, body: `{"code":200003,"message":"authorization_verify_error"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				return httpResponse(testCase.status, testCase.body), nil
			}
			fake.install(t)
			_, errRefresh := refreshCredential(testHost(), DefaultConfig(), current)
			if errRefresh == nil {
				t.Fatal("a dead refresh token must fail")
			}
			if got := statusOf(errRefresh, 0); got != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (terminal)", got)
			}
			if !strings.Contains(errRefresh.Error(), "重新登录") {
				t.Errorf("message = %q, want it to ask for a new login", errRefresh.Error())
			}
		})
	}
}

// TestRefreshNonTerminalFailuresStayRetryable pins trap #15: a business failure
// that is not 200003 and a transport failure must NOT be classified as an expired
// credential.
func TestRefreshNonTerminalFailuresStayRetryable(t *testing.T) {
	current := sampleCredential(t)
	cases := []struct {
		name      string
		status    int
		body      string
		transport bool
	}{
		{name: "business error", status: http.StatusOK, body: `{"code":100002,"message":"params invalid"}`},
		{name: "missing access_token", status: http.StatusOK, body: `{"code":0,"data":{}}`},
		{name: "gateway 502", status: http.StatusBadGateway, body: `{"message":"bad gateway"}`},
		{name: "transport", transport: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				if testCase.transport {
					return nil, errFakeTransport
				}
				return httpResponse(testCase.status, testCase.body), nil
			}
			fake.install(t)
			_, errRefresh := refreshCredential(testHost(), DefaultConfig(), current)
			if errRefresh == nil {
				t.Fatal("want a failure")
			}
			if got := statusOf(errRefresh, 0); got == http.StatusUnauthorized {
				t.Fatalf("status = 401, want a retryable classification: %v", errRefresh)
			}
		})
	}
}

// TestRefreshWithoutTokenIsTerminalForTheRequest pins that a credential with no
// refresh token says so instead of pretending to renew.
func TestRefreshWithoutTokenIsTerminalForTheRequest(t *testing.T) {
	credential := &Credential{AccessToken: "opaque"}
	if _, errRefresh := refreshCredential(testHost(), DefaultConfig(), credential); errRefresh == nil {
		t.Fatal("a credential without a refresh token cannot be refreshed")
	} else if !strings.Contains(errRefresh.Error(), "refresh_token") {
		t.Errorf("message = %q, want it to name the missing refresh_token", errRefresh.Error())
	}
}

// TestUserInfoFillOnlyMissingFields pins `persistLogin`'s enrichment rule.
func TestUserInfoFillOnlyMissingFields(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"code":0,"data":{"id":"u1","name":"RaccoonAva","phone":"13900139000","office_identity":"org-9"}}`), nil
	}
	fake.install(t)

	partial := &Credential{AccessToken: "tok", UserID: "u-existing", Nickname: ""}
	enrichCredential(testHost(), DefaultConfig(), partial)
	if partial.UserID != "u-existing" {
		t.Errorf("user_id = %q, want the existing value kept", partial.UserID)
	}
	if partial.Nickname != "RaccoonAva" || partial.Phone != "13900139000" || partial.OfficeIdentity != "org-9" {
		t.Errorf("missing fields were not filled: %+v", partial)
	}

	// A failing profile call must never fail a login.
	failing := newFakeHost()
	failing.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	failing.install(t)
	untouched := &Credential{AccessToken: "tok"}
	enrichCredential(testHost(), DefaultConfig(), untouched)
	if untouched.Nickname != "" {
		t.Errorf("a failed enrichment changed the credential: %+v", untouched)
	}
}
