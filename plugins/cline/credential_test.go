package main

import (
	"encoding/json"
	"testing"
	"time"
)

// TestClineBearerValueIdempotence is the single highest-risk rule of this
// provider (spec risk 8): `Bearer workos:<jwt>` answers 200 while the same token
// without the prefix answers 401 with a body that blames the client version.
// The prefix is therefore prepended IFF absent, never stripped and never doubled.
func TestClineBearerValueIdempotence(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"already prefixed", "workos:eyJhbGciOi", "workos:eyJhbGciOi"},
		{"bare token", "eyJhbGciOi", "workos:eyJhbGciOi"},
		{"surrounding whitespace", "  workos:eyJhbGciOi  ", "workos:eyJhbGciOi"},
		{"bare token with whitespace", "\teyJhbGciOi\n", "workos:eyJhbGciOi"},
		{"empty", "", ""},
		{"only whitespace", "   ", ""},
		{"prefix case matters", "WorkOS:eyJhbGciOi", "workos:WorkOS:eyJhbGciOi"},
		{"prefix without a token", TokenPrefix, TokenPrefix},
		{"idempotent twice", clineBearerValue(clineBearerValue("abc")), "workos:abc"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := clineBearerValue(testCase.input); got != testCase.want {
				t.Fatalf("clineBearerValue(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
		})
	}
}

// TestCredentialBearerValueNeverStripsThePrefix guards the stored form too: a
// credential loaded from disk must not have its prefix normalised away.
func TestCredentialBearerValueNeverStripsThePrefix(t *testing.T) {
	credential := &Credential{AccessToken: "workos:eyJ"}
	if got := credential.bearerValue(); got != "workos:eyJ" {
		t.Fatalf("bearerValue = %q", got)
	}
	credential = &Credential{AccessToken: "eyJ"}
	if got := credential.bearerValue(); got != "workos:eyJ" {
		t.Fatalf("a bare token must gain the prefix, got %q", got)
	}
}

// TestParseTokenEnvelope covers the tolerant parser of `cline.ts:121-157`.
func TestParseTokenEnvelope(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, tokens clineTokens)
	}{
		{
			name: "measured register envelope",
			body: `{"success":true,"data":{"accessToken":"workos:eyJ","refreshToken":"tmgEeM","expiresAt":"2026-09-25T05:23:47.000Z","tokenType":"Bearer","userInfo":{"clineUserId":"usr-01M3","email":"a@b.c","firstName":"Ada","lastName":"Lovelace"}}}`,
			check: func(t *testing.T, tokens clineTokens) {
				if tokens.AccessToken != "workos:eyJ" || tokens.RefreshToken != "tmgEeM" {
					t.Fatalf("tokens = %+v", tokens)
				}
				if tokens.AccountID != "usr-01M3" || tokens.Email != "a@b.c" {
					t.Fatalf("identity = %+v", tokens)
				}
				if tokens.Nickname != "Ada Lovelace" {
					t.Fatalf("nickname = %q", tokens.Nickname)
				}
				want := time.Date(2026, 9, 25, 5, 23, 47, 0, time.UTC).UnixMilli()
				if tokens.ExpireTime != want {
					t.Fatalf("expire_time = %d, want %d", tokens.ExpireTime, want)
				}
			},
		},
		{
			name: "top level payload without data",
			body: `{"access_token":"workos:eyJ","refresh_token":"r","expire_time":1768000000000,"account_id":"usr-2","email":"x@y.z"}`,
			check: func(t *testing.T, tokens clineTokens) {
				if tokens.AccessToken != "workos:eyJ" || tokens.RefreshToken != "r" {
					t.Fatalf("tokens = %+v", tokens)
				}
				if tokens.AccountID != "usr-2" || tokens.ExpireTime != 1_768_000_000_000 {
					t.Fatalf("payload = %+v", tokens)
				}
			},
		},
		{
			name: "second-precision epoch is scaled",
			body: `{"data":{"accessToken":"t","refreshToken":"r","expiresAt":1768000000}}`,
			check: func(t *testing.T, tokens clineTokens) {
				if tokens.ExpireTime != 1_768_000_000_000 {
					t.Fatalf("expire_time = %d", tokens.ExpireTime)
				}
			},
		},
		{
			name: "unparseable expiry is zero, never a fabricated epoch",
			body: `{"data":{"accessToken":"t","refreshToken":"r","expiresAt":"soon"}}`,
			check: func(t *testing.T, tokens clineTokens) {
				if tokens.ExpireTime != 0 {
					t.Fatalf("expire_time = %d, want 0", tokens.ExpireTime)
				}
			},
		},
		{
			name: "account id falls back to the top level",
			body: `{"data":{"accessToken":"t","refreshToken":"r","userInfo":{"email":"u@v.w"},"accountId":"usr-3"}}`,
			check: func(t *testing.T, tokens clineTokens) {
				if tokens.AccountID != "usr-3" || tokens.Email != "u@v.w" {
					t.Fatalf("payload = %+v", tokens)
				}
			},
		},
		{
			name: "junk",
			body: `<html>502</html>`,
			check: func(t *testing.T, tokens clineTokens) {
				if tokens.AccessToken != "" || tokens.RefreshToken != "" {
					t.Fatalf("junk must parse to empty: %+v", tokens)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.check(t, parseTokenEnvelope([]byte(testCase.body)))
		})
	}
}

// TestParseCredentialAcceptsHostNumberRewrite covers the boundary coercion: the
// host rewrites numeric-looking strings into JSON numbers, and a hand-written
// auth file can do the opposite for the timestamp.
func TestParseCredentialAcceptsHostNumberRewrite(t *testing.T) {
	// expire_time as a quoted number and the opaque tokens as bare numbers.
	raw := []byte(`{"access_token":1234567890,"refresh_token":987654321,"expire_time":"1768000000000","account_id":"usr-1"}`)
	credential, errParse := ParseCredential(raw)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if credential.AccessToken != "1234567890" {
		t.Errorf("access_token = %q", credential.AccessToken)
	}
	if credential.RefreshToken != "987654321" {
		t.Errorf("refresh_token = %q", credential.RefreshToken)
	}
	if credential.ExpireTime != 1_768_000_000_000 {
		t.Errorf("expire_time = %d", credential.ExpireTime)
	}
	if got := credential.bearerValue(); got != "workos:1234567890" {
		t.Errorf("bearerValue = %q", got)
	}
}

// TestParseCredentialRejectsUnusableInput pins the two rejection cases.
func TestParseCredentialRejectsUnusableInput(t *testing.T) {
	if _, errParse := ParseCredential(nil); errParse == nil {
		t.Error("an empty payload must be rejected")
	}
	if _, errParse := ParseCredential([]byte(`{"refresh_token":"r"}`)); errParse == nil {
		t.Error("a credential without an access token must be rejected")
	}
	if _, errParse := ParseCredential([]byte(`not json`)); errParse == nil {
		t.Error("an undecodable payload must be rejected")
	}
}

// TestCredentialExpiryRules pins `isClineExpired` (`cline.ts:276-279`): an absent
// expiry means "no information", which is treated as usable and left to a server
// 401.
func TestCredentialExpiryRules(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		expireTime int64
		wantExpiry bool
	}{
		{"absent expiry is usable", 0, false},
		{"future expiry is usable", now.Add(time.Hour).UnixMilli(), false},
		{"past expiry is expired", now.Add(-time.Minute).UnixMilli(), true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			credential := &Credential{AccessToken: "workos:t", ExpireTime: testCase.expireTime}
			if got := credential.Expired(0); got != testCase.wantExpiry {
				t.Fatalf("Expired = %v, want %v", got, testCase.wantExpiry)
			}
			if testCase.expireTime == 0 && !credential.ExpiresAt().IsZero() {
				t.Error("a zero expiry must render as the zero time")
			}
		})
	}
}

// TestCredentialRefreshability pins that only the refresh token matters.
func TestCredentialRefreshability(t *testing.T) {
	if (&Credential{AccessToken: "workos:t"}).Refreshable() {
		t.Error("a credential without a refresh token is not refreshable")
	}
	if !(&Credential{AccessToken: "workos:t", RefreshToken: "r"}).Refreshable() {
		t.Error("a credential with a refresh token is refreshable")
	}
	if (&Credential{AccessToken: "workos:t", RefreshToken: "  "}).Refreshable() {
		t.Error("a whitespace refresh token is not a refresh token")
	}
}

// TestApplyRefreshKeepsIdentityFields is `applyClineRefresh` (`cline.ts:213-227`):
// the refresh answer carries no account id, email or nickname, and losing them
// breaks the balance endpoint and the display label.
func TestApplyRefreshKeepsIdentityFields(t *testing.T) {
	current := &Credential{
		AccessToken:  "workos:old",
		RefreshToken: "old-refresh",
		ExpireTime:   1,
		AccountID:    "usr-1",
		Email:        "a@b.c",
		Nickname:     "Ada",
		Type:         ProviderKey,
	}
	refreshed := current.applyRefresh(clineTokens{
		AccessToken: "workos:new",
		ExpireTime:  1_768_000_000_000,
	})
	if refreshed.AccessToken != "workos:new" {
		t.Errorf("access token not replaced: %q", refreshed.AccessToken)
	}
	if refreshed.RefreshToken != "old-refresh" {
		t.Errorf("refresh token must survive an answer that omits it: %q", refreshed.RefreshToken)
	}
	if refreshed.AccountID != "usr-1" || refreshed.Email != "a@b.c" || refreshed.Nickname != "Ada" {
		t.Errorf("identity fields were dropped: %+v", refreshed)
	}
	if refreshed.ExpireTime != 1_768_000_000_000 {
		t.Errorf("expire_time = %d", refreshed.ExpireTime)
	}

	// A bare token in the refresh answer still gains the prefix.
	bare := current.applyRefresh(clineTokens{AccessToken: "eyJ"})
	if bare.AccessToken != "workos:eyJ" {
		t.Errorf("access token = %q", bare.AccessToken)
	}
}

// TestBuildCredentialNormalisesTokens pins that the login path stores the
// prefixed form, and that the WorkOS refresh token is NOT kept as the Cline one.
func TestBuildCredentialNormalisesTokens(t *testing.T) {
	credential := buildCredential(clineTokens{AccessToken: "eyJ", RefreshToken: "cline-refresh", AccountID: "usr-9"})
	if credential.AccessToken != "workos:eyJ" {
		t.Errorf("access token = %q", credential.AccessToken)
	}
	if credential.RefreshToken != "cline-refresh" {
		t.Errorf("refresh token = %q", credential.RefreshToken)
	}
	if credential.Type != ProviderKey {
		t.Errorf("type = %q", credential.Type)
	}
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(storage, &decoded); errUnmarshal != nil {
		t.Fatalf("decode storage: %v", errUnmarshal)
	}
	if decoded["access_token"] != "workos:eyJ" {
		t.Errorf("stored access_token = %v", decoded["access_token"])
	}
}

// TestCredentialLabel pins the display resolution order of `cline.ts:193-195`.
func TestCredentialLabel(t *testing.T) {
	cases := []struct {
		name       string
		credential Credential
		want       string
	}{
		{"nickname wins", Credential{Nickname: "Ada", Email: "a@b.c", AccountID: "usr-1"}, "Ada"},
		{"email second", Credential{Email: "a@b.c", AccountID: "usr-1"}, "a@b.c"},
		{"account id third", Credential{AccountID: "usr-1"}, "usr-1"},
		{"nothing", Credential{}, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.credential.Label(); got != testCase.want {
				t.Fatalf("Label = %q, want %q", got, testCase.want)
			}
		})
	}
}
