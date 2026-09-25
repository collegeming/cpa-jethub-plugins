package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// jwtWithExp builds an unsigned JWT carrying only an `exp` claim.
func jwtWithExp(exp int64) string {
	payload, _ := json.Marshal(map[string]any{"exp": exp})
	segment := base64.RawURLEncoding.EncodeToString(payload)
	return "header." + segment + ".signature"
}

// TestCredentialExpiry covers every accepted `expires_at` shape plus the JWT
// fallback (trae.ts:139-156).
func TestCredentialExpiry(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		credential Credential
		wantMS     int64
		wantOK     bool
	}{
		{name: "millisecond string", credential: Credential{ExpiresAt: "1786847930141"}, wantMS: 1786847930141, wantOK: true},
		{name: "second string", credential: Credential{ExpiresAt: "1786847930"}, wantMS: 1786847930000, wantOK: true},
		{name: "iso timestamp", credential: Credential{ExpiresAt: "2030-01-02T03:04:05Z"}, wantMS: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli(), wantOK: true},
		{name: "jwt fallback", credential: Credential{AccessToken: jwtWithExp(now.Add(time.Hour).Unix())}, wantMS: now.Add(time.Hour).Unix() * 1000, wantOK: true},
		{name: "nothing parseable", credential: Credential{ExpiresAt: "soon", AccessToken: "not-a-jwt"}, wantOK: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := testCase.credential.ExpiresAtMS()
			if ok != testCase.wantOK {
				t.Fatalf("ok = %v, want %v (value %d)", ok, testCase.wantOK, got)
			}
			if !ok {
				return
			}
			if testCase.name == "jwt fallback" {
				if got/1000 != testCase.wantMS/1000 {
					t.Fatalf("expiry = %d, want about %d", got, testCase.wantMS)
				}
				return
			}
			if got != testCase.wantMS {
				t.Fatalf("expiry = %d, want %d", got, testCase.wantMS)
			}
		})
	}

	// An unparseable expiry is never treated as expired.
	if (&Credential{AccessToken: "not-a-jwt"}).Expired() {
		t.Fatal("an unparseable expiry must not count as expired")
	}
	if !(&Credential{ExpiresAt: "1000"}).Expired() {
		t.Fatal("a past expiry must count as expired")
	}
	if (&Credential{ExpiresAt: "99999999999999"}).Expired() {
		t.Fatal("a future expiry must not count as expired")
	}
}

// TestCredentialParseAndEncode covers the persisted shape.
func TestCredentialParseAndEncode(t *testing.T) {
	if _, errParse := ParseCredential(nil); errParse == nil {
		t.Fatal("an empty payload must be rejected")
	}
	if _, errParse := ParseCredential([]byte("not json")); errParse == nil {
		t.Fatal("a non-JSON payload must be rejected")
	}
	if _, errParse := ParseCredential([]byte(`{"refresh_token":"r"}`)); errParse == nil {
		t.Fatal("a payload without access_token must be rejected")
	}

	credential := &Credential{AccessToken: "tok", RefreshToken: "r", UID: "u", MachineID: "m", DeviceID: "d"}
	encoded, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	if !strings.Contains(string(encoded), `"type":"trae"`) {
		t.Fatalf("encoded credential must declare its provider type: %s", encoded)
	}
	parsed, errParse := ParseCredential(encoded)
	if errParse != nil {
		t.Fatalf("round trip: %v", errParse)
	}
	if parsed.AccessToken != "tok" || parsed.MachineID != "m" || parsed.DeviceID != "d" {
		t.Fatalf("round trip = %#v", parsed)
	}
	if !parsed.Refreshable() {
		t.Fatal("a credential with a refresh token is refreshable")
	}
	if (&Credential{AccessToken: "tok"}).Refreshable() {
		t.Fatal("a credential without a refresh token is not refreshable")
	}
}

// TestCredentialExpiresAtAcceptsHostNumber is the regression guard for a real
// defect: the host re-serialises the auth storage it holds and turns a
// numeric-looking `expires_at` into a JSON *number*. A plain string field fails
// to decode, which silently hides every model of the provider ("unknown provider
// for model ..." on the client side, with no error in any log).
func TestCredentialExpiresAtAcceptsHostNumber(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{name: "string as stored by the plugin", body: `{"access_token":"t","expires_at":"1786847930000"}`, want: 1786847930000},
		{name: "number as rewritten by the host", body: `{"access_token":"t","expires_at":1786847930000}`, want: 1786847930000},
		{name: "float from the host", body: `{"access_token":"t","expires_at":1786847930000.0}`, want: 1786847930000},
		{name: "seconds as a number", body: `{"access_token":"t","expires_at":1786847930}`, want: 1786847930000},
		{name: "null", body: `{"access_token":"t","expires_at":null}`},
		{name: "absent", body: `{"access_token":"t"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			credential, errParse := ParseCredential([]byte(testCase.body))
			if errParse != nil {
				t.Fatalf("ParseCredential: %v", errParse)
			}
			got, ok := credential.ExpiresAtMS()
			if testCase.want == 0 {
				if ok {
					t.Fatalf("expiry = %d, want no value", got)
				}
				return
			}
			if !ok || got != testCase.want {
				t.Fatalf("expiry = %d (ok=%v), want %d", got, ok, testCase.want)
			}
		})
	}
}

// TestCredentialEncodeKeepsExpiresAtAString: the stored form stays a string so
// the file remains interchangeable with Jet-Hub's.
func TestCredentialEncodeKeepsExpiresAtAString(t *testing.T) {
	encoded, errEncode := (&Credential{AccessToken: "t", ExpiresAt: ExpiresAtValue("1786847930000")}).Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	if !strings.Contains(string(encoded), `"expires_at":"1786847930000"`) {
		t.Fatalf("encoded credential = %s", encoded)
	}
}

// TestDefaultAuthFileName sanitizes the identity into a safe file name.
func TestDefaultAuthFileName(t *testing.T) {
	cases := []struct {
		name       string
		credential Credential
		want       string
	}{
		// A CJK nickname sanitizes to nothing, so the uid keeps accounts apart.
		{name: "cjk nickname falls back to uid", credential: Credential{Nickname: "张三", UID: "u1"}, want: "trae-u1.json"},
		{name: "ascii nickname", credential: Credential{Nickname: "alice", UID: "u1"}, want: "trae-alice.json"},
		{name: "falls back to uid", credential: Credential{UID: "8847309959"}, want: "trae-8847309959.json"},
		{name: "unsafe characters", credential: Credential{Nickname: "a/b c", UID: "u"}, want: "trae-a-b-c.json"},
		{name: "empty", credential: Credential{}, want: "trae-account.json"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := defaultAuthFileName(&testCase.credential); got != testCase.want {
				t.Fatalf("file name = %q, want %q", got, testCase.want)
			}
		})
	}
	long := Credential{Nickname: strings.Repeat("x", 200), UID: "u"}
	if got := defaultAuthFileName(&long); len(got) > len("trae-")+64+len(".json") {
		t.Fatalf("file name must be truncated: %d chars", len(got))
	}
}

// TestAuthDataFor checks the host-facing record, including that machine_id never
// disappears from the metadata and the refresh lead is applied.
func TestAuthDataFor(t *testing.T) {
	expiry := time.Now().Add(3 * time.Hour)
	credential := &Credential{
		AccessToken:  "tok",
		RefreshToken: "refresh",
		ExpiresAt:    ExpiresAtValue(strconvFormat(expiry.UnixMilli())),
		UID:          "uid-123456789",
		Nickname:     "昵称",
		MachineID:    "machine",
		DeviceID:     "device",
		Region:       RegionCN,
	}
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		t.Fatalf("authDataFor: %v", errAuth)
	}
	if auth.Provider != ProviderKey || auth.FileName != "trae-uid-123456789.json" || auth.ID != auth.FileName {
		t.Fatalf("auth = %#v", auth)
	}
	if auth.Label != "昵称" || auth.Prefix != "uid-1234" {
		t.Fatalf("label/prefix = %#v", auth)
	}
	if auth.Attributes["refreshable"] != "true" || auth.Attributes["region"] != RegionCN {
		t.Fatalf("attributes = %#v", auth.Attributes)
	}
	if auth.Metadata["machine_id"] != "machine" || auth.Metadata["device_id"] != "device" {
		t.Fatalf("metadata = %#v", auth.Metadata)
	}
	wantNext := expiry.Add(-refreshLead)
	if diff := auth.NextRefreshAfter.Sub(wantNext); diff > time.Second || diff < -time.Second {
		t.Fatalf("next refresh = %s, want %s", auth.NextRefreshAfter, wantNext)
	}
	// A credential without an expiry schedules no refresh.
	noExpiry, errNoExpiry := authDataFor(&Credential{AccessToken: "tok", UID: "u"}, "explicit.json")
	if errNoExpiry != nil {
		t.Fatalf("authDataFor: %v", errNoExpiry)
	}
	if !noExpiry.NextRefreshAfter.IsZero() {
		t.Fatalf("next refresh = %s, want zero", noExpiry.NextRefreshAfter)
	}
	if noExpiry.FileName != "explicit.json" {
		t.Fatalf("file name = %q, want the explicit one", noExpiry.FileName)
	}
}

// TestApplyTraeRefresh locks the device-identity rule: refresh rotates the
// tokens but must never regenerate machine_id or device_id.
func TestApplyTraeRefresh(t *testing.T) {
	previous := &Credential{
		AccessToken:  "old",
		RefreshToken: "old-refresh",
		UID:          "uid",
		Nickname:     "nick",
		MachineID:    "machine-id",
		DeviceID:     "device-id",
		Region:       RegionCN,
		EnterpriseID: "tenant",
	}
	refreshed := applyTraeRefresh(previous, traeExchangeResult{
		AccessToken:         "new",
		RefreshToken:        "new-refresh",
		TokenExpireAt:       1786847930,
		TokenExpireDuration: 3600,
	}, 1_000_000)
	if refreshed.AccessToken != "new" || refreshed.RefreshToken != "new-refresh" {
		t.Fatalf("tokens = %#v", refreshed)
	}
	if refreshed.MachineID != previous.MachineID || refreshed.DeviceID != previous.DeviceID {
		t.Fatalf("device identity must be preserved: %#v", refreshed)
	}
	if refreshed.UID != "uid" || refreshed.Nickname != "nick" || refreshed.EnterpriseID != "tenant" {
		t.Fatalf("identity fields must be preserved: %#v", refreshed)
	}
	if refreshed.ExpiresAt != "1786847930000" {
		t.Fatalf("expires_at = %q, want tokenExpireAt converted to milliseconds", refreshed.ExpiresAt)
	}
	// The source credential must not be mutated.
	if previous.AccessToken != "old" || previous.RefreshToken != "old-refresh" {
		t.Fatalf("the previous credential was mutated: %#v", previous)
	}
	// An empty rotated refresh token keeps the previous one.
	kept := applyTraeRefresh(previous, traeExchangeResult{AccessToken: "new", TokenExpireDuration: 60}, 1_000_000)
	if kept.RefreshToken != "old-refresh" {
		t.Fatalf("refresh token = %q, want the previous one", kept.RefreshToken)
	}
	if kept.ExpiresAt != "1060000" {
		t.Fatalf("relative expiry = %q", kept.ExpiresAt)
	}
}

// TestExpiresAtStringPrecedence covers the three expiry sources.
func TestExpiresAtStringPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		exchange traeExchangeResult
		nowMS    int64
		want     string
	}{
		{name: "absolute milliseconds", exchange: traeExchangeResult{TokenExpireAt: 1786847930141}, want: "1786847930141"},
		{name: "absolute seconds", exchange: traeExchangeResult{TokenExpireAt: 1786847930}, want: "1786847930000"},
		{name: "relative duration", exchange: traeExchangeResult{TokenExpireDuration: 120}, nowMS: 1000, want: "121000"},
		{name: "jwt fallback", exchange: traeExchangeResult{AccessToken: jwtWithExp(2000)}, want: "2000000"},
		{name: "nothing", exchange: traeExchangeResult{AccessToken: "opaque"}, want: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := expiresAtString(testCase.exchange, testCase.nowMS); got != testCase.want {
				t.Fatalf("expiresAtString = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestBuildLoginURL checks the 17-parameter URL: every name matters, and the
// callback parameter is `auth_callback_url`.
func TestBuildLoginURL(t *testing.T) {
	product := productFor(RegionCN)
	machineID := "0123456789abcdef0123456789abcdef"
	deviceID := "fedcba9876543210fedcba9876543210"
	callback := "http://127.0.0.1:18080/authorize"
	raw := buildTraeLoginURL(product, machineID, deviceID, callback)

	if !strings.HasPrefix(raw, product.ConsoleHost+"/authorization?") {
		t.Fatalf("url = %q", raw)
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	query := parsed.Query()
	expected := map[string]string{
		"login_version":     "1",
		"auth_from":         "solo",
		"login_channel":     "native_ide",
		"plugin_version":    product.PluginVersion,
		"auth_type":         "local",
		"client_id":         product.ClientID,
		"redirect":          "0",
		"login_trace_id":    machineTraceID(machineID, deviceID),
		"auth_callback_url": callback,
		"machine_id":        machineID,
		"device_id":         deviceID,
		"x_device_id":       deviceID,
		"x_machine_id":      machineID,
		"x_device_brand":    "PC",
		"x_device_type":     "PC",
		"x_os_version":      "1.0",
		"x_app_version":     product.IDEVersion,
		"x_app_type":        "stable",
	}
	if len(query) != len(expected) {
		t.Fatalf("parameter count = %d, want %d: %v", len(query), len(expected), query)
	}
	for key, want := range expected {
		if got := query.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	// The two wrong names from the historical defect must not appear.
	for _, wrong := range []string{"callback_url", "redirect_uri"} {
		if _, present := query[wrong]; present {
			t.Fatalf("%s must not be used; the real name is auth_callback_url", wrong)
		}
	}
	// The reference order is preserved so the URL stays byte-comparable.
	if !strings.HasPrefix(raw, product.ConsoleHost+"/authorization?login_version=1&") {
		t.Fatalf("query order = %q", raw)
	}
}

// TestParseCallbackTokenFlow covers the legacy flow, where the callback hands
// back tokens directly.
func TestParseCallbackTokenFlow(t *testing.T) {
	query := url.Values{
		"refreshToken": {"rt-1"},
		"userInfo":     {`{"UserID":"u1","ScreenName":"Nick","TenantID":"tenant-1"}`},
		"userJwt":      {`{"Token":"access-1","RefreshToken":"rt-from-jwt"}`},
	}
	result := parseTraeCallbackQuery(query)
	if !result.OK {
		t.Fatalf("result = %#v", result)
	}
	if result.Info.RefreshToken != "rt-1" {
		t.Fatalf("refresh token = %q, want the query value", result.Info.RefreshToken)
	}
	if result.Info.UID != "u1" || result.Info.EnterpriseID != "tenant-1" {
		t.Fatalf("info = %#v, want TenantID mapped to the enterprise id", result.Info)
	}
	if result.Info.AccessToken != "" {
		t.Fatal("userJwt.Token is only a fallback when there is no refresh token")
	}

	// Without a query refreshToken the JWT one is used, and the access token
	// falls back to userJwt.Token.
	fallback := parseTraeCallbackQuery(url.Values{
		"userJwt": {`{"Token":"access-2"}`},
	})
	if !fallback.OK || fallback.Info.AccessToken != "access-2" || fallback.Info.RefreshToken != "" {
		t.Fatalf("fallback = %#v", fallback)
	}

	// A double-encoded userInfo is still parsed.
	double := parseTraeCallbackQuery(url.Values{
		"refreshToken": {"rt"},
		"userInfo":     {url.QueryEscape(`{"UserID":"u2"}`)},
	})
	if !double.OK || double.Info.UID != "u2" {
		t.Fatalf("double-encoded userInfo = %#v", double)
	}
}

// TestParseCallbackPKCEFlow: a PKCE callback is a *valid* callback that the port
// does not implement, and the message must say so instead of blaming a missing
// refresh token.
func TestParseCallbackPKCEFlow(t *testing.T) {
	result := parseTraeCallbackQuery(url.Values{"code": {"auth-code-1"}})
	if result.OK {
		t.Fatal("the PKCE flow is not implemented, so the parse must not be OK")
	}
	if !result.AuthCodeFlow {
		t.Fatal("AuthCodeFlow must mark the PKCE shape")
	}
	if !strings.Contains(result.Reason, "PKCE") {
		t.Fatalf("reason = %q", result.Reason)
	}

	info := parseTraeCallbackQuery(url.Values{"authCodeInfo": {`{"code":"nested"}`}})
	if info.AuthCodeFlow && !strings.Contains(info.Reason, "PKCE") {
		t.Fatalf("nested authCodeInfo reason = %q", info.Reason)
	}
	plain := parseTraeCallbackQuery(url.Values{"authCodeInfo": {"plain-code"}})
	if !plain.AuthCodeFlow {
		t.Fatalf("a plain authCodeInfo string must be recognised: %#v", plain)
	}
}

// TestParseCallbackInvalid: an unusable callback has to produce a reason, because
// the poll handler reports it instead of hanging.
func TestParseCallbackInvalid(t *testing.T) {
	result := parseTraeCallbackQuery(url.Values{})
	if result.OK || result.AuthCodeFlow || result.Reason == "" {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Reason, "refreshToken") {
		t.Fatalf("reason = %q", result.Reason)
	}
	malformed := parseTraeCallbackRaw("http://[::1]:namedport/authorize")
	if malformed.OK {
		t.Fatal("a malformed URL must not parse")
	}
}

// TestFixNicknameMojibake covers the three outcomes: a repaired name, a CJK name
// kept as-is, and a placeholder for unrecoverable garbage.
func TestFixNicknameMojibake(t *testing.T) {
	// "张三" encoded as UTF-8 bytes read back as latin-1.
	mojibake := string([]rune{rune(0xE5), rune(0xBC), rune(0xA0), rune(0xE4), rune(0xB8), rune(0x89)})
	if got := fixNicknameMojibake(mojibake, "uid1234"); got != "张三" {
		t.Fatalf("repaired = %q, want 张三", got)
	}
	// A plain ASCII name survives unchanged.
	if got := fixNicknameMojibake("alice", "uid1234"); got != "alice" {
		t.Fatalf("ascii = %q", got)
	}
	// An already-correct CJK name is returned untouched.
	if got := fixNicknameMojibake("张三", "uid1234"); got != "张三" {
		t.Fatalf("cjk = %q", got)
	}
	// Unrecoverable, non-CJK text degrades to the placeholder so mojibake never
	// reaches the credential.
	if got := fixNicknameMojibake("Óû§8847309959", "8847309959"); got != "用户9959" {
		t.Fatalf("placeholder = %q", got)
	}
	if got := fixNicknameMojibake("", "uid"); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

// TestParseExchangeAndUserInfo covers both naming conventions.
func TestParseExchangeAndUserInfo(t *testing.T) {
	exchange, ok := parseTraeExchangeResponse([]byte(`{"Result":{"Token":"a","RefreshToken":"r","TokenExpireAt":1786847930,"TokenExpireDuration":3600,"RefreshExpireAt":1}}`))
	if !ok || exchange.AccessToken != "a" || exchange.RefreshToken != "r" {
		t.Fatalf("exchange = %#v ok=%v", exchange, ok)
	}
	if exchange.TokenExpireAt != 1786847930 || exchange.TokenExpireDuration != 3600 {
		t.Fatalf("exchange expiry = %#v", exchange)
	}
	lower, ok := parseTraeExchangeResponse([]byte(`{"result":{"token":"a2","refreshToken":"r2"}}`))
	if !ok || lower.AccessToken != "a2" || lower.RefreshToken != "r2" {
		t.Fatalf("lowercase exchange = %#v", lower)
	}
	if _, ok := parseTraeExchangeResponse([]byte(`{"Result":{"RefreshToken":"r"}}`)); ok {
		t.Fatal("an exchange response without a token must fail")
	}

	userInfo, ok := parseTraeUserInfoResponse([]byte(`{"Result":{"UserID":8847309959,"ScreenName":"nick","EnterpriseID":"t"}}`))
	if !ok || userInfo.UID != "8847309959" || userInfo.ScreenName != "nick" || userInfo.EnterpriseID != "t" {
		t.Fatalf("user info = %#v ok=%v", userInfo, ok)
	}
	defaultName, ok := parseTraeUserInfoResponse([]byte(`{"result":{"uid":"u"}}`))
	if !ok || defaultName.ScreenName != "u" {
		t.Fatalf("screen name fallback = %#v", defaultName)
	}
	if _, ok := parseTraeUserInfoResponse([]byte(`{"Result":{}}`)); ok {
		t.Fatal("a user info response without a uid must fail")
	}
}

// TestClassifyTraeError locks the ordering rule: 4008 must win over 4011.
func TestClassifyTraeError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   traeErrorKind
	}{
		{name: "plan limit", status: 200, body: `{"code":1005,"msg":"plan limit"}`, want: errHardPlan},
		{name: "quota beats rate limit", status: 400, body: `{"code":4008,"msg":"quota exceeded the quota 4011"}`, want: errQuotaExceeded},
		{name: "rate limit", status: 200, body: `{"code":4011,"msg":"too fast"}`, want: errSoftRate},
		{name: "unauthorized", status: 401, body: `{"msg":"login required"}`, want: errSessionDead},
		{name: "too many requests", status: 429, body: "", want: errSoftRate},
		{name: "not found", status: 404, body: "", want: errNotFound},
		{name: "server", status: 503, body: "", want: errServer},
		{name: "client", status: 400, body: "bad request", want: errClient},
		{name: "success", status: 200, body: "{}", want: errNone},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := classifyTraeError(testCase.status, testCase.body); got != testCase.want {
				t.Fatalf("classify = %q, want %q", got, testCase.want)
			}
		})
	}
	if !isTerminalError(errSessionDead) || isTerminalError(errSoftRate) {
		t.Fatal("only session-dead is terminal")
	}
	if !recordsRateLimit(errQuotaExceeded) || recordsRateLimit(errServer) {
		t.Fatal("rate-limit bookkeeping must exclude transport failures")
	}
}

// TestCheckinClassification covers the check-in cooldown table.
func TestCheckinClassification(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		code         int
		wantType     string
		wantCooldown int
	}{
		{name: "plan limit", status: 200, code: 1005, wantType: "PlanLimit", wantCooldown: 43200},
		{name: "soft rate", status: 429, code: 0, wantType: "SoftRate", wantCooldown: 60},
		{name: "session dead", status: 401, code: 0, wantType: "SessionDead", wantCooldown: -1},
		{name: "not found", status: 404, code: 0, wantType: "NotFound", wantCooldown: 60},
		{name: "server", status: 502, code: 0, wantType: "Server", wantCooldown: 600},
		{name: "client", status: 403, code: 0, wantType: "Client", wantCooldown: 600},
		{name: "business", status: 200, code: 9074, wantType: "BusinessError", wantCooldown: 300},
		{name: "unknown", status: 200, code: 0, wantType: "Unknown", wantCooldown: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gotType, gotCooldown := classifyCheckinError(testCase.status, testCase.code)
			if gotType != testCase.wantType || gotCooldown != testCase.wantCooldown {
				t.Fatalf("classify = (%q, %d), want (%q, %d)", gotType, gotCooldown, testCase.wantType, testCase.wantCooldown)
			}
		})
	}
}

// TestReadClaimCode: the gateway sometimes returns the code as a string, and a
// string "9074" must not read as success.
func TestReadClaimCode(t *testing.T) {
	cases := []struct {
		body map[string]any
		want int
	}{
		{body: map[string]any{"code": float64(0)}, want: 0},
		{body: map[string]any{"code": float64(9074)}, want: 9074},
		{body: map[string]any{"code": "9074"}, want: 9074},
		{body: map[string]any{}, want: 0},
		{body: map[string]any{"code": nil}, want: 0},
		{body: map[string]any{"code": "abc"}, want: -1},
		{body: map[string]any{"code": true}, want: -1},
	}
	for _, testCase := range cases {
		if got := readClaimCode(testCase.body); got != testCase.want {
			t.Fatalf("readClaimCode(%#v) = %d, want %d", testCase.body, got, testCase.want)
		}
	}
}

// TestDecodeResponseBody covers the transfer encodings the check-in headers ask
// for.
func TestDecodeResponseBody(t *testing.T) {
	payload := []byte(`{"code":0,"message":"success"}`)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, errWrite := writer.Write(payload); errWrite != nil {
		t.Fatalf("gzip write: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("gzip close: %v", errClose)
	}

	response := &pluginapi.HTTPResponse{
		StatusCode: 200,
		Headers:    http.Header{"Content-Encoding": []string{"gzip"}},
		Body:       compressed.Bytes(),
	}
	if got := decodeResponseBody(response); string(got) != string(payload) {
		t.Fatalf("decoded = %s, want %s", got, payload)
	}
	plain := &pluginapi.HTTPResponse{Headers: http.Header{}, Body: payload}
	if got := decodeResponseBody(plain); string(got) != string(payload) {
		t.Fatalf("plain = %s", got)
	}
	if got := decodeResponseBody(nil); got != nil {
		t.Fatalf("nil response = %#v", got)
	}
	// A corrupt body is returned as-is so the caller reports the decode failure.
	corrupt := &pluginapi.HTTPResponse{Headers: http.Header{"Content-Encoding": []string{"gzip"}}, Body: []byte("nope")}
	if got := decodeResponseBody(corrupt); string(got) != "nope" {
		t.Fatalf("corrupt = %s", got)
	}
}

// TestErrorDetailAndHTTPCode are the small error renderers.
func TestErrorDetailAndHTTPCode(t *testing.T) {
	if got := errorDetail(`{"code":4001,"message":"bad param"}`); got != "code=4001 bad param" {
		t.Fatalf("errorDetail = %q", got)
	}
	if got := errorDetail("plain text"); got != "plain text" {
		t.Fatalf("errorDetail = %q", got)
	}
	cases := map[int]string{401: "AUTH", 403: "AUTH", 429: "RATE_LIMIT", 400: "INVALID_REQUEST", 500: "SERVER", 418: "HTTP_418"}
	for status, want := range cases {
		if got := httpErrorCode(status); got != want {
			t.Fatalf("httpErrorCode(%d) = %q, want %q", status, got, want)
		}
	}
}

// strconvFormat renders an int64 without importing strconv in the test file.
func strconvFormat(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
