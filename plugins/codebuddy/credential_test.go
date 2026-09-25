package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeJWT builds an unsigned JWT whose payload carries the given claims. The
// signature segment is a placeholder: nothing in the plugin verifies it.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, errMarshal := json.Marshal(claims)
	if errMarshal != nil {
		t.Fatalf("marshal claims: %v", errMarshal)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestStripControlChars(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"profile\n    offline_access\n    email", "profile offline_access email"},
		{"a\r\nb", "a b"},
		{"  spaced  ", "spaced"},
		{"tab\there", "tab here"},
		{"clean", "clean"},
	}
	for _, test := range tests {
		if got := stripControlChars(test.in); got != test.want {
			t.Errorf("stripControlChars(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestCredentialExpiresAtMs(t *testing.T) {
	jwt := makeJWT(t, map[string]any{"exp": float64(1_800_000_000)})
	tests := []struct {
		name       string
		credential Credential
		want       int64
		wantOK     bool
	}{
		{
			name:       "millisecond timestamp",
			credential: Credential{AccessToken: jwt, ExpiresAt: "1799999999000"},
			want:       1_799_999_999_000,
			wantOK:     true,
		},
		{
			name:       "second timestamp is scaled to milliseconds",
			credential: Credential{AccessToken: jwt, ExpiresAt: "1799999999"},
			want:       1_799_999_999_000,
			wantOK:     true,
		},
		{
			name:       "iso timestamp",
			credential: Credential{AccessToken: jwt, ExpiresAt: "2027-01-15T08:00:00Z"},
			want:       1_800_000_000_000,
			wantOK:     true,
		},
		{
			name:       "missing expires_at falls back to the JWT exp",
			credential: Credential{AccessToken: jwt},
			want:       1_800_000_000_000,
			wantOK:     true,
		},
		{
			name:       "non-JWT token with no expires_at is unknown",
			credential: Credential{AccessToken: "not-a-jwt"},
			wantOK:     false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := credentialExpiresAtMs(&test.credential)
			if ok != test.wantOK {
				t.Fatalf("ok = %v, want %v", ok, test.wantOK)
			}
			if test.wantOK && got != test.want {
				t.Fatalf("expiry = %d, want %d", got, test.want)
			}
		})
	}
}

func TestAbsoluteExpiryUsesJWTIssuedAt(t *testing.T) {
	issuedAt := int64(1_700_000_000)
	token := makeJWT(t, map[string]any{"iat": float64(issuedAt), "exp": float64(issuedAt + 3600)})

	// expiresIn is relative to iat, NOT to wall clock (buddy.ts:263-306).
	record := map[string]any{"expiresIn": float64(3600), "refreshExpiresIn": float64(7200)}
	got := absoluteExpiryMs(record, "expiresAt", "expiresIn", token)
	want := time.Unix(issuedAt+3600, 0).UnixMilli()
	if got != formatInt(want) {
		t.Fatalf("absoluteExpiryMs(expiresIn) = %q, want %d", got, want)
	}
	gotRefresh := absoluteExpiryMs(record, "refreshExpiresAt", "refreshExpiresIn", token)
	wantRefresh := time.Unix(issuedAt+7200, 0).UnixMilli()
	if gotRefresh != formatInt(wantRefresh) {
		t.Fatalf("absoluteExpiryMs(refreshExpiresIn) = %q, want %d", gotRefresh, wantRefresh)
	}
}

func formatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestParseTokenDataAndBuildCredential(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"iat":      float64(1_700_000_000),
		"sub":      "user-from-jwt",
		"nickname": "昵称\n 来自JWT",
	})
	parsed := parseTokenData(map[string]any{
		"accessToken":      token,
		"refreshToken":     "refresh-1",
		"expiresIn":        float64(100),
		"refreshExpiresIn": float64(200),
		"scope":            "profile\n    offline_access",
		"domain":           "copilot.tencent.com",
	})
	if parsed.AccessToken != token || parsed.RefreshToken != "refresh-1" {
		t.Fatalf("token parse = %+v", parsed)
	}
	if parsed.TokenType != "Bearer" {
		t.Fatalf("token type = %q, want the Bearer default", parsed.TokenType)
	}
	if parsed.Scope != "profile offline_access" {
		t.Fatalf("scope = %q, want control characters collapsed", parsed.Scope)
	}

	// login/account often omits the nickname; it must come from the JWT.
	account := parseAccountData(map[string]any{"uid": "uid-1", "type": "enterprise"})
	credential := buildCredential(parsed, account, ProductDefault())
	if credential.Nickname != "昵称 来自JWT" {
		t.Errorf("nickname = %q, want the JWT nickname", credential.Nickname)
	}
	if credential.UserID != "uid-1" {
		t.Errorf("user id = %q, want the account uid", credential.UserID)
	}
	if credential.AccountType != "enterprise" {
		t.Errorf("account type = %q", credential.AccountType)
	}
	if credential.Product != ProductCodeBuddy {
		t.Errorf("product = %q, want %q", credential.Product, ProductCodeBuddy)
	}
	if !credential.Refreshable() {
		t.Error("credential should be refreshable")
	}
	// The fixed 2023 iat makes this credential long expired; a token issued now
	// with a 100s lifetime must not be.
	fresh := parseTokenData(map[string]any{
		"accessToken": makeJWT(t, map[string]any{"iat": float64(time.Now().Unix())}),
		"expiresIn":   float64(100),
	})
	if buildCredential(fresh, account, ProductDefault()).Expired(time.Minute) {
		t.Error("a credential issued now with a 100s lifetime should not be expired")
	}

	// uid missing: the JWT subject is the fallback.
	fallback := buildCredential(parsed, parseAccountData(map[string]any{}), ProductDefault())
	if fallback.UserID != "user-from-jwt" {
		t.Errorf("user id = %q, want the JWT subject", fallback.UserID)
	}
	if fallback.AccountType != "personal" {
		t.Errorf("account type = %q, want the personal default", fallback.AccountType)
	}
}

func TestCredentialEncodeRoundTripSanitizes(t *testing.T) {
	credential := Credential{
		AccessToken:  "token",
		RefreshToken: "refresh",
		Nickname:     "line1\nline2",
		Product:      ProductWorkBuddy,
	}
	raw, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	parsed, errParse := ParseCredential(raw)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if parsed.Nickname != "line1 line2" {
		t.Fatalf("nickname = %q, want the control characters removed", parsed.Nickname)
	}
	if parsed.Product != ProductWorkBuddy {
		t.Fatalf("product = %q", parsed.Product)
	}

	if _, errEmpty := ParseCredential(nil); errEmpty == nil {
		t.Error("empty credential must be rejected")
	}
	if _, errMissing := ParseCredential([]byte(`{"refresh_token":"x"}`)); errMissing == nil {
		t.Error("credential without access_token must be rejected")
	}
}

func TestCredentialHeadersFollowTheProduct(t *testing.T) {
	workbuddy, _ := productByConfigValue(ProductWorkBuddy)
	// A legacy credential whose domain points at the China endpoint must not
	// drag X-Domain back there (AGENTS.md: X-Domain 必须跟随产品).
	credential := &Credential{
		AccessToken:  "token",
		Domain:       "copilot.tencent.com",
		EnterpriseID: "ent-1",
	}
	headers := credentialDomain(credential, workbuddy)
	if headers != "www.workbuddy.ai" {
		t.Fatalf("X-Domain = %q, want the product domain", headers)
	}
	requestHeaders := credentialRequestHeaders(credential)
	if requestHeaders[HeaderEnterpriseID] != "ent-1" || requestHeaders[HeaderTenantID] != "ent-1" {
		t.Fatalf("enterprise headers = %+v", requestHeaders)
	}
	authHeaders := credentialAuthHeaders(credential)
	if authHeaders["Authorization"] != "Bearer token" {
		t.Fatalf("authorization = %q", authHeaders["Authorization"])
	}

	// No product domain (never the case for the four shipped products, but the
	// fallback chain must still prefer the credential over the constant).
	empty := productConfig{}
	if got := credentialDomain(credential, empty); got != "copilot.tencent.com" {
		t.Fatalf("fallback domain = %q", got)
	}
}

func TestProductForCredential(t *testing.T) {
	if got := productForCredential(&Credential{Product: ProductWorkBuddyCN}); got.ConfigValue != ProductWorkBuddyCN {
		t.Errorf("stored product wins: got %q", got.ConfigValue)
	}
	// Jet-Hub auth files carry the provider id instead.
	if got := productForCredential(&Credential{Product: "buddy-intl"}); got.ConfigValue != ProductCodeBuddyIntl {
		t.Errorf("provider id alias: got %q", got.ConfigValue)
	}
	previous := settings()
	setSettings(Config{Product: ProductWorkBuddy})
	defer setSettings(previous)
	if got := productForCredential(&Credential{}); got.ConfigValue != ProductWorkBuddy {
		t.Errorf("configured product fallback: got %q", got.ConfigValue)
	}
}

func TestRandomUUIDAndPromptCacheKey(t *testing.T) {
	first := randomUUIDv4()
	second := randomUUIDv4()
	if len(first) != 36 {
		t.Fatalf("uuid length = %d, want 36: %q", len(first), first)
	}
	if first == second {
		t.Fatal("two UUIDs must differ")
	}
	if key := newPromptCacheKey(); len(key) != 32 || strings.ContainsRune(key, '-') {
		t.Fatalf("prompt cache key = %q, want a dash-free UUID", key)
	}
	if _, errHex := randomHex(8); errHex != nil {
		t.Fatalf("randomHex: %v", errHex)
	}
}

func TestDecorateLoginURL(t *testing.T) {
	workbuddy, _ := productByConfigValue(ProductWorkBuddy)
	decorated := decorateLoginURL("https://www.workbuddy.ai/login/?platform=workbuddy-ai&state=abc", workbuddy)
	for _, fragment := range []string{"version=5.5.2", "loginSessionId=", "state=abc"} {
		if !strings.Contains(decorated, fragment) {
			t.Errorf("decorated URL %q is missing %q", decorated, fragment)
		}
	}

	codebuddy, _ := productByConfigValue(ProductCodeBuddy)
	original := "https://www.codebuddy.cn/login/?platform=ide&state=abc"
	// CodeBuddy's server-issued URL must not be rebuilt (buddy-oauth.ts:525-538).
	if got := decorateLoginURL(original, codebuddy); got != original {
		t.Errorf("CodeBuddy URL = %q, want it unchanged", got)
	}
}
