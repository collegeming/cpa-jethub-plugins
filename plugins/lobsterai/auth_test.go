package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// jwtWithExp builds an unsigned JWT-shaped token carrying one exp claim.
func jwtWithExp(exp int64) string {
	payload, _ := json.Marshal(map[string]any{"exp": exp, "sub": "user"})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestParseCredential(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "valid", raw: `{"access_token":"a","refresh_token":"r"}`},
		{name: "empty access token", raw: `{"access_token":"  ","refresh_token":"r"}`, wantErr: true},
		{name: "missing access token", raw: `{"refresh_token":"r"}`, wantErr: true},
		{name: "empty payload", raw: ``, wantErr: true},
		{name: "not json", raw: `{"access_token":`, wantErr: true},
		{name: "camelCase is not our shape", raw: `{"accessToken":"a"}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credential, errParse := ParseCredential([]byte(test.raw))
			if test.wantErr {
				if errParse == nil {
					t.Fatalf("ParseCredential(%s) succeeded unexpectedly: %+v", test.raw, credential)
				}
				return
			}
			if errParse != nil {
				t.Fatalf("ParseCredential(%s) failed: %v", test.raw, errParse)
			}
		})
	}
}

func TestCredentialEncodeRoundTrip(t *testing.T) {
	original := &Credential{
		AccessToken:   "access",
		RefreshToken:  "refresh",
		UID:           "uid-1",
		UserID:        "yid-1",
		Nickname:      "nick",
		UUID:          "uuid-1",
		FirstKeyfrom:  "1000",
		LatestKeyfrom: "2000",
	}
	encoded, errEncode := original.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	// snake_case field names are part of the interchange contract with Jet-Hub.
	for _, key := range []string{"access_token", "refresh_token", "first_keyfrom", "latest_keyfrom", "user_id"} {
		if !jsonHasKey(encoded, key) {
			t.Fatalf("encoded credential %s is missing %s", encoded, key)
		}
	}
	parsed, errParse := ParseCredential(encoded)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if parsed.UUID != original.UUID || parsed.LatestKeyfrom != original.LatestKeyfrom || parsed.UserID != original.UserID {
		t.Fatalf("round trip lost identity fields: %+v", parsed)
	}
	if parsed.Type != ProviderKey {
		t.Fatalf("Type = %q, want %q", parsed.Type, ProviderKey)
	}
}

// jsonHasKey reports whether the raw JSON object carries a non-null key.
func jsonHasKey(raw []byte, key string) bool {
	var record map[string]any
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return false
	}
	value, present := record[key]
	return present && value != nil
}

func TestCredentialExpiry(t *testing.T) {
	now := time.Now()
	msTimestamp := now.Add(2 * time.Hour).UnixMilli()
	secondsTimestamp := now.Add(2 * time.Hour).Unix()
	exp := now.Add(3 * time.Hour).Unix()

	tests := []struct {
		name       string
		credential Credential
		wantOK     bool
		check      func(time.Time) bool
	}{
		{
			name:       "millisecond timestamp",
			credential: Credential{ExpiresAt: itoa64(msTimestamp)},
			wantOK:     true,
			check:      func(got time.Time) bool { return got.UnixMilli() == msTimestamp },
		},
		{
			name:       "second timestamp",
			credential: Credential{ExpiresAt: itoa64(secondsTimestamp)},
			wantOK:     true,
			check:      func(got time.Time) bool { return got.Unix() == secondsTimestamp },
		},
		{
			name:       "iso timestamp",
			credential: Credential{ExpiresAt: "2030-01-02T03:04:05Z"},
			wantOK:     true,
			check:      func(got time.Time) bool { return got.UTC().Year() == 2030 },
		},
		{
			name:       "jwt fallback",
			credential: Credential{AccessToken: jwtWithExp(exp)},
			wantOK:     true,
			check:      func(got time.Time) bool { return got.Unix() == exp },
		},
		{
			name:       "garbage with no jwt is unknown",
			credential: Credential{ExpiresAt: "not-a-date", AccessToken: "opaque"},
			wantOK:     false,
		},
		{
			name:       "empty is unknown",
			credential: Credential{},
			wantOK:     false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := test.credential.Expiry()
			if ok != test.wantOK {
				t.Fatalf("Expiry() ok = %v, want %v (got %v)", ok, test.wantOK, got)
			}
			if ok && test.check != nil && !test.check(got) {
				t.Fatalf("Expiry() = %v, predicate failed", got)
			}
		})
	}
}

func TestCredentialExpiredNeverTreatsUnknownAsExpired(t *testing.T) {
	unknown := &Credential{AccessToken: "opaque"}
	if unknown.Expired(0) {
		t.Fatal("an unknown expiry must not be treated as expired")
	}
	past := &Credential{ExpiresAt: itoa64(time.Now().Add(-time.Hour).UnixMilli())}
	if !past.Expired(0) {
		t.Fatal("a past expiry must be expired")
	}
	future := &Credential{ExpiresAt: itoa64(time.Now().Add(time.Hour).UnixMilli())}
	if future.Expired(0) {
		t.Fatal("an expiry an hour away must not be expired without skew")
	}
	if future.Expired(30 * time.Minute) {
		t.Fatal("an expiry an hour away must not be expired with a 30 minute skew")
	}
	if !future.Expired(2 * time.Hour) {
		t.Fatal("skew must pull a future expiry into the expired window")
	}
}

func itoa64(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestResolveUIDFallbackChain(t *testing.T) {
	token := "access-token-value"
	sum := sha256.Sum256([]byte(token))
	wantHash := hex.EncodeToString(sum[:])[:16]

	tests := []struct {
		name    string
		payload tokenPayload
		want    string
	}{
		{name: "user.id wins", payload: tokenPayload{UserID: "id", AccountUserID: "uid", YID: "yid", AccessToken: token}, want: "id"},
		{name: "user.userId second", payload: tokenPayload{AccountUserID: "uid", YID: "yid", AccessToken: token}, want: "uid"},
		{name: "user.yid third", payload: tokenPayload{YID: "yid", AccessToken: token}, want: "yid"},
		{name: "hash fallback", payload: tokenPayload{AccessToken: token}, want: wantHash},
		{name: "blank fields are skipped", payload: tokenPayload{UserID: "  ", AccountUserID: "", YID: "yid", AccessToken: token}, want: "yid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveUID(test.payload); got != test.want {
				t.Fatalf("resolveUID = %q, want %q", got, test.want)
			}
		})
	}
	if len(wantHash) != 16 {
		t.Fatalf("hash fallback must be 16 hex chars, got %d", len(wantHash))
	}
}

func TestBuildCredential(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	session := loginSessionState{UUID: "uuid-1", FirstKeyfrom: "111", LatestKeyfrom: "222"}

	t.Run("expiresIn is measured from now", func(t *testing.T) {
		credential := buildCredential(tokenPayload{
			AccessToken: "a", RefreshToken: "r", HasExpiresIn: true, ExpiresIn: 3600,
		}, session, now)
		want := now.Add(time.Hour).UnixMilli()
		if credential.ExpiresAt != itoa64(want) {
			t.Fatalf("ExpiresAt = %s, want %d", credential.ExpiresAt, want)
		}
		if credential.UUID != "uuid-1" || credential.FirstKeyfrom != "111" || credential.LatestKeyfrom != "222" {
			t.Fatalf("identity fields lost: %+v", credential)
		}
	})

	t.Run("jwt exp fallback", func(t *testing.T) {
		exp := now.Add(2 * time.Hour).Unix()
		credential := buildCredential(tokenPayload{AccessToken: jwtWithExp(exp)}, session, now)
		if credential.ExpiresAt != itoa64(exp*1000) {
			t.Fatalf("ExpiresAt = %s, want %d", credential.ExpiresAt, exp*1000)
		}
	})

	t.Run("no expiry information stays empty", func(t *testing.T) {
		credential := buildCredential(tokenPayload{AccessToken: "opaque"}, session, now)
		if credential.ExpiresAt != "" {
			t.Fatalf("ExpiresAt = %q, want empty", credential.ExpiresAt)
		}
	})

	t.Run("user_id preference", func(t *testing.T) {
		withAccount := buildCredential(tokenPayload{AccessToken: "a", AccountUserID: "acct", YID: "yid"}, session, now)
		if withAccount.UserID != "acct" {
			t.Fatalf("UserID = %q, want acct", withAccount.UserID)
		}
		withYID := buildCredential(tokenPayload{AccessToken: "a", YID: "yid"}, session, now)
		if withYID.UserID != "yid" {
			t.Fatalf("UserID = %q, want yid", withYID.UserID)
		}
	})
}

func TestApplyRefreshPreservesIdentity(t *testing.T) {
	original := &Credential{
		AccessToken:   "old-access",
		RefreshToken:  "old-refresh",
		ExpiresAt:     "1000",
		UID:           "uid",
		UserID:        "yid",
		Nickname:      "nick",
		UUID:          "uuid",
		FirstKeyfrom:  "111",
		LatestKeyfrom: "222",
	}
	now := time.Unix(1_800_000_000, 0)
	refreshed := applyRefresh(original, tokenPayload{
		AccessToken: "new-access",
		// The refresh response carries no new refreshToken.
		HasExpiresIn: true,
		ExpiresIn:    7200,
	}, now)

	if refreshed.AccessToken != "new-access" {
		t.Fatalf("AccessToken = %q", refreshed.AccessToken)
	}
	if refreshed.RefreshToken != "old-refresh" {
		t.Fatalf("RefreshToken = %q, want the previous value", refreshed.RefreshToken)
	}
	if refreshed.UUID != "uuid" || refreshed.FirstKeyfrom != "111" || refreshed.LatestKeyfrom != "222" ||
		refreshed.UID != "uid" || refreshed.UserID != "yid" || refreshed.Nickname != "nick" {
		t.Fatalf("identity fields changed: %+v", refreshed)
	}
	if refreshed.ExpiresAt != itoa64(now.Add(2*time.Hour).UnixMilli()) {
		t.Fatalf("ExpiresAt = %s", refreshed.ExpiresAt)
	}
	// The original must not be mutated in place.
	if original.AccessToken != "old-access" {
		t.Fatal("applyRefresh mutated the previous credential")
	}
}

func TestApplyRefreshKeepsPreviousExpiryWhenUnknown(t *testing.T) {
	original := &Credential{AccessToken: "old", RefreshToken: "r", ExpiresAt: "12345"}
	refreshed := applyRefresh(original, tokenPayload{AccessToken: "opaque"}, time.Now())
	if refreshed.ExpiresAt != "12345" {
		t.Fatalf("ExpiresAt = %q, want the previous value", refreshed.ExpiresAt)
	}
}

func TestKeyfromAndRefreshBody(t *testing.T) {
	credential := &Credential{
		RefreshToken:  "refresh-token",
		UserID:        "yid",
		UUID:          "uuid",
		FirstKeyfrom:  "111",
		LatestKeyfrom: "222",
	}
	body := keyfromBody(credential, "2026.9.4")
	if body["firstKeyfrom"] != "111" || body["latestKeyfrom"] != "222" || body["version"] != "2026.9.4" {
		t.Fatalf("keyfromBody = %v", body)
	}
	if body["uuid"] != "uuid" || body["userId"] != "yid" {
		t.Fatalf("keyfromBody dropped identity: %v", body)
	}
	if _, present := body["refreshToken"]; present {
		t.Fatal("keyfromBody must not carry refreshToken")
	}

	withoutOptionals := keyfromBody(&Credential{FirstKeyfrom: "1", LatestKeyfrom: "2"}, "v")
	if _, present := withoutOptionals["uuid"]; present {
		t.Fatal("an absent uuid must omit the key, not send an empty string")
	}
	if _, present := withoutOptionals["userId"]; present {
		t.Fatal("an absent userId must omit the key")
	}

	refresh := refreshBody(credential, "2026.9.4")
	if refresh["refreshToken"] != "refresh-token" {
		t.Fatalf("refreshBody = %v", refresh)
	}
	if len(refresh) != len(body)+1 {
		t.Fatalf("refreshBody must be keyfrom + refreshToken only: %v", refresh)
	}
}

func TestDefaultAuthFileName(t *testing.T) {
	tests := []struct {
		name       string
		credential Credential
		want       string
	}{
		{name: "uid", credential: Credential{UID: "abc123"}, want: "lobsterai-abc123.json"},
		{name: "user id fallback", credential: Credential{UserID: "yid"}, want: "lobsterai-yid.json"},
		{name: "empty", credential: Credential{}, want: "lobsterai-account.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := defaultAuthFileName(&test.credential); got != test.want {
				t.Fatalf("defaultAuthFileName = %q, want %q", got, test.want)
			}
		})
	}

	// Unsafe characters are replaced instead of leaking into a path.
	sanitised := defaultAuthFileName(&Credential{Nickname: "昵称 带/斜杠"})
	if strings.ContainsAny(sanitised, "/ ") {
		t.Fatalf("defaultAuthFileName left unsafe characters: %q", sanitised)
	}
	if !strings.HasPrefix(sanitised, ProviderKey+"-") || !strings.HasSuffix(sanitised, ".json") {
		t.Fatalf("defaultAuthFileName = %q", sanitised)
	}
}

func TestAuthDataFor(t *testing.T) {
	credential := &Credential{
		AccessToken:   "a",
		RefreshToken:  "r",
		UID:           "uid-123",
		Nickname:      "nick",
		ExpiresAt:     itoa64(time.Now().Add(2 * time.Hour).UnixMilli()),
		LatestKeyfrom: "222",
	}
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		t.Fatalf("authDataFor: %v", errAuth)
	}
	if auth.Provider != ProviderKey {
		t.Fatalf("Provider = %q", auth.Provider)
	}
	if auth.FileName != "lobsterai-uid-123.json" {
		t.Fatalf("FileName = %q", auth.FileName)
	}
	if auth.Label != "nick" {
		t.Fatalf("Label = %q", auth.Label)
	}
	if auth.Attributes["refreshable"] != "true" {
		t.Fatalf("Attributes = %v", auth.Attributes)
	}
	if auth.NextRefreshAfter.IsZero() {
		t.Fatal("NextRefreshAfter must be derived from the expiry")
	}
	if auth.Metadata["latest_keyfrom"] != "222" {
		t.Fatalf("Metadata = %v", auth.Metadata)
	}

	// A credential without a parseable expiry must not claim a refresh time.
	opaque := &Credential{AccessToken: "opaque"}
	auth, errAuth = authDataFor(opaque, "explicit.json")
	if errAuth != nil {
		t.Fatalf("authDataFor(opaque): %v", errAuth)
	}
	if !auth.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero for an unknown expiry", auth.NextRefreshAfter)
	}
	if auth.FileName != "explicit.json" {
		t.Fatalf("FileName = %q, want the explicit name", auth.FileName)
	}
}

func TestHandleAuthParse(t *testing.T) {
	ours := mustJSON(t, pluginapi.AuthParseRequest{
		Provider: ProviderKey,
		FileName: "lobsterai-uid.json",
		RawJSON:  []byte(`{"access_token":"a","uid":"uid"}`),
	})
	value, errParse := handleAuthParse(nil, ours)
	if errParse != nil {
		t.Fatalf("handleAuthParse: %v", errParse)
	}
	response, ok := value.(pluginapi.AuthParseResponse)
	if !ok || !response.Handled {
		t.Fatalf("handleAuthParse = %#v", value)
	}
	if response.Auth.Provider != ProviderKey {
		t.Fatalf("Provider = %q", response.Auth.Provider)
	}

	foreign := mustJSON(t, pluginapi.AuthParseRequest{
		Provider: "codearts",
		RawJSON:  []byte(`{"access_token":"a","uid":"uid"}`),
	})
	value, errParse = handleAuthParse(nil, foreign)
	if errParse != nil {
		t.Fatalf("handleAuthParse(foreign): %v", errParse)
	}
	if response := value.(pluginapi.AuthParseResponse); response.Handled {
		t.Fatal("a foreign provider key must not be claimed")
	}

	unknown := mustJSON(t, pluginapi.AuthParseRequest{RawJSON: []byte(`{"hello":"world"}`)})
	value, errParse = handleAuthParse(nil, unknown)
	if errParse != nil {
		t.Fatalf("handleAuthParse(unknown): %v", errParse)
	}
	if response := value.(pluginapi.AuthParseResponse); response.Handled {
		t.Fatal("an unrecognised payload must be handed back to the host")
	}
}

func TestHandleAuthIdentifier(t *testing.T) {
	value, errIdentifier := handleAuthIdentifier(nil, nil)
	if errIdentifier != nil {
		t.Fatalf("handleAuthIdentifier: %v", errIdentifier)
	}
	encoded := mustJSON(t, value)
	if string(encoded) != `{"identifier":"lobsterai"}` {
		t.Fatalf("auth.identifier = %s", encoded)
	}
}

func TestHandleAuthRefreshSuccess(t *testing.T) {
	fake := newFakeHost().on(httpRoute{
		Method: http.MethodPost,
		Match:  RefreshPath,
		Body:   `{"code":0,"msg":"OK","data":{"accessToken":"new-token","refreshToken":"new-refresh","expiresIn":3600}}`,
	})
	host := installFakeHost(t, fake)

	stored := &Credential{
		AccessToken: "old", RefreshToken: "old-refresh", UID: "uid", UserID: "yid",
		UUID: "uuid", FirstKeyfrom: "111", LatestKeyfrom: "222",
	}
	raw := mustJSON(t, pluginapi.AuthRefreshRequest{AuthID: "lobsterai-uid.json", StorageJSON: mustJSON(t, stored)})
	value, errRefresh := handleAuthRefresh(host, raw)
	if errRefresh != nil {
		t.Fatalf("handleAuthRefresh: %v", errRefresh)
	}
	response := value.(pluginapi.AuthRefreshResponse)
	refreshed, errParse := ParseCredential(response.Auth.StorageJSON)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if refreshed.AccessToken != "new-token" || refreshed.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed = %+v", refreshed)
	}
	if refreshed.UUID != "uuid" || refreshed.LatestKeyfrom != "222" || refreshed.UID != "uid" {
		t.Fatalf("identity fields lost: %+v", refreshed)
	}

	requests := fake.requestsFor(RefreshPath)
	if len(requests) != 1 {
		t.Fatalf("refresh requests = %d", len(requests))
	}
	if got := requests[0].Headers.Get("Authorization"); got != "" {
		t.Fatalf("refresh must not send Authorization, got %q", got)
	}
	body := map[string]any{}
	if errUnmarshal := json.Unmarshal(requests[0].Body, &body); errUnmarshal != nil {
		t.Fatalf("decode refresh body: %v", errUnmarshal)
	}
	for _, key := range []string{"refreshToken", "firstKeyfrom", "latestKeyfrom", "version", "uuid", "userId"} {
		if _, present := body[key]; !present {
			t.Fatalf("refresh body is missing %s: %v", key, body)
		}
	}
}

func TestHandleAuthRefreshTerminal(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantStatus int
	}{
		{
			name:       "session dead business code",
			status:     http.StatusOK,
			body:       `{"code":40101,"msg":"refresh token was rejected"}`,
			wantCode:   "refresh_token_expired",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "http 401",
			status:     http.StatusUnauthorized,
			body:       `{"code":0,"msg":"unauthorized","data":{}}`,
			wantCode:   "refresh_token_expired",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "code 0 without access token",
			status:     http.StatusOK,
			body:       `{"code":0,"msg":"OK","data":{"refreshToken":"r"}}`,
			wantCode:   "refresh_token_expired",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "retryable server failure",
			status:     http.StatusInternalServerError,
			body:       `{"code":500,"msg":"boom"}`,
			wantCode:   "refresh_failed",
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeHost().on(httpRoute{Method: http.MethodPost, Match: RefreshPath, Status: test.status, Body: test.body})
			host := installFakeHost(t, fake)
			raw := mustJSON(t, pluginapi.AuthRefreshRequest{
				StorageJSON: mustJSON(t, &Credential{AccessToken: "old", RefreshToken: "r"}),
			})
			_, errRefresh := handleAuthRefresh(host, raw)
			if errRefresh == nil {
				t.Fatal("expected a refresh failure")
			}
			envelopeError, ok := errRefresh.(*abiboot.EnvelopeError)
			if !ok {
				t.Fatalf("error type = %T", errRefresh)
			}
			if envelopeError.Code != test.wantCode || envelopeError.HTTPStatus != test.wantStatus {
				t.Fatalf("error = %+v, want %s/%d", envelopeError, test.wantCode, test.wantStatus)
			}
		})
	}
}

func TestHandleAuthRefreshNotRefreshable(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)
	raw := mustJSON(t, pluginapi.AuthRefreshRequest{StorageJSON: mustJSON(t, &Credential{AccessToken: "a"})})
	_, errRefresh := handleAuthRefresh(host, raw)
	if errRefresh == nil {
		t.Fatal("a credential without refresh_token must not be refreshable")
	}
	if len(fake.requests) != 0 {
		t.Fatal("no upstream call may happen without a refresh token")
	}
}
