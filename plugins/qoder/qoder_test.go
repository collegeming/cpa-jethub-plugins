package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPKCEChallengeIsUnpaddedBase64URL pins the one detail that makes the server
// reject the whole flow: the challenge is `base64url(sha256(verifier))` with the
// padding stripped (`qoder.ts:45`, `:63-67`).
func TestPKCEChallengeIsUnpaddedBase64URL(t *testing.T) {
	pair, errPKCE := newPKCE()
	if errPKCE != nil {
		t.Fatalf("newPKCE: %v", errPKCE)
	}
	if len(pair.Verifier) < 43 || len(pair.Verifier) > 128 {
		t.Fatalf("verifier length = %d, want 43..128", len(pair.Verifier))
	}
	for _, symbol := range pair.Verifier {
		if !strings.ContainsRune(pkceAlphabet, symbol) {
			t.Fatalf("verifier contains %q, which is outside the RFC 7636 unreserved set", symbol)
		}
	}
	digest := sha256.Sum256([]byte(pair.Verifier))
	want := base64.RawURLEncoding.EncodeToString(digest[:])
	if pair.Challenge != want {
		t.Fatalf("challenge = %q, want %q", pair.Challenge, want)
	}
	if strings.ContainsAny(pair.Challenge, "+/=") {
		t.Fatalf("challenge %q contains padding or non-URL-safe characters", pair.Challenge)
	}
}

// TestAuthURLUsesTheProductionClientID guards the reported defect where the
// non-prod client id was used and the authorization callback was rejected with
// "参数无效" (`qoder-product.ts:164-184`).
func TestAuthURLUsesTheProductionClientID(t *testing.T) {
	session, errDevice := newDeviceSession("machine-1")
	if errDevice != nil {
		t.Fatalf("newDeviceSession: %v", errDevice)
	}
	global := productByID(string(RegionGlobal))
	parsed, errParse := url.Parse(session.authURL(global))
	if errParse != nil {
		t.Fatalf("parse auth url: %v", errParse)
	}
	if parsed.Host != "qoder.com" || parsed.Path != DeviceSelectPath {
		t.Fatalf("auth url endpoints = %s%s, want qoder.com%s", parsed.Host, parsed.Path, DeviceSelectPath)
	}
	query := parsed.Query()
	if got := query.Get("client_id"); got != global.ClientID {
		t.Fatalf("client_id = %q, want the production id %q", got, global.ClientID)
	}
	if got := query.Get("client_id"); got == global.TestClientID {
		t.Fatal("client_id is the non-prod id; the callback would be rejected")
	}
	if query.Get("challenge_method") != "S256" || query.Get("challenge") == "" ||
		query.Get("nonce") == "" || query.Get("machine_id") != "machine-1" {
		t.Fatalf("auth url query is incomplete: %v", query)
	}

	// The CN site uses its own auth host.
	cn := productByID(string(RegionCN))
	cnURL, _ := url.Parse(session.authURL(cn))
	if cnURL.Host != "qoder.cn" {
		t.Fatalf("CN auth host = %q, want qoder.cn", cnURL.Host)
	}
}

// TestPollURLUsesTheOpenAPIHost is the documented trap: polling `qoder.com`
// returns 401 while `openapi.qoder.sh` returns 404 for "no session yet"
// (`qoder.ts:120-134`).
func TestPollURLUsesTheOpenAPIHost(t *testing.T) {
	session, _ := newDeviceSession("machine-2")
	global := productByID(string(RegionGlobal))
	parsed, errParse := url.Parse(session.pollURL(global))
	if errParse != nil {
		t.Fatalf("parse poll url: %v", errParse)
	}
	if parsed.Host != "openapi.qoder.sh" {
		t.Fatalf("poll host = %q, want openapi.qoder.sh (polling qoder.com returns 401)", parsed.Host)
	}
	if parsed.Path != PollPath {
		t.Fatalf("poll path = %q, want %q", parsed.Path, PollPath)
	}
	query := parsed.Query()
	if query.Get("verifier") != session.PKCE.Verifier || query.Get("nonce") != session.Nonce ||
		query.Get("challenge_method") != "S256" {
		t.Fatalf("poll query is incomplete: %v", query)
	}
}

// TestDeviceSessionKeepsTheGivenMachineID checks that a re-login reuses the
// persisted device identity instead of minting a new one.
func TestDeviceSessionKeepsTheGivenMachineID(t *testing.T) {
	session, errDevice := newDeviceSession("persisted")
	if errDevice != nil {
		t.Fatalf("newDeviceSession: %v", errDevice)
	}
	if session.MachineID != "persisted" {
		t.Fatalf("machine id = %q, want the persisted one", session.MachineID)
	}
	fresh, _ := newDeviceSession("")
	if fresh.MachineID == "" || fresh.MachineID == session.MachineID {
		t.Fatalf("a session without a machine id must mint a fresh one, got %q", fresh.MachineID)
	}
}

// TestParseTokenPayloadAcceptsBothShapes covers the login (`token`) and refresh
// (`device_token`) spellings plus the uid/user_name fields the encrypted path
// depends on (`qoder.ts:211-235`).
func TestParseTokenPayloadAcceptsBothShapes(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, payload tokenPayload)
	}{
		{
			name: "login response with token and user info",
			body: `{"token":"tok","refresh_token":"ref","expires_at":1790000000000,` +
				`"refresh_token_expires_at":1800000000,"user_id":"u-1","user_name":"nick"}`,
			check: func(t *testing.T, payload tokenPayload) {
				if payload.AccessToken != "tok" || payload.RefreshToken != "ref" {
					t.Fatalf("tokens = %q/%q", payload.AccessToken, payload.RefreshToken)
				}
				if payload.ExpiresAt != 1790000000000 {
					t.Fatalf("ExpiresAt = %d, want the millisecond value unchanged", payload.ExpiresAt)
				}
				// A 10-digit value is seconds and must be scaled to milliseconds.
				if payload.RefreshTokenExpiresAt != 1_800_000_000_000 {
					t.Fatalf("RefreshTokenExpiresAt = %d, want seconds scaled to ms", payload.RefreshTokenExpiresAt)
				}
				if payload.UID != "u-1" || payload.UserName != "nick" {
					t.Fatalf("identity = %q/%q", payload.UID, payload.UserName)
				}
			},
		},
		{
			name: "refresh response with device_token",
			body: `{"device_token":"tok2","refreshToken":"ref2","expiresAt":"2026-09-25T12:00:00Z"}`,
			check: func(t *testing.T, payload tokenPayload) {
				if payload.AccessToken != "tok2" || payload.RefreshToken != "ref2" {
					t.Fatalf("camelCase shape not accepted: %#v", payload)
				}
				if payload.ExpiresAt != time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC).UnixMilli() {
					t.Fatalf("ExpiresAt = %d, want the ISO stamp parsed", payload.ExpiresAt)
				}
			},
		},
		{
			name: "garbage yields an empty payload rather than an error",
			body: `[1,2,3]`,
			check: func(t *testing.T, payload tokenPayload) {
				if payload.AccessToken != "" {
					t.Fatalf("AccessToken = %q, want empty", payload.AccessToken)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.check(t, parseTokenPayloadJSON([]byte(testCase.body)))
		})
	}
}

// TestBuildCredentialDoubleWritesTheToken captures the `security_oauth_token ??
// access_token` read order upstream uses (`qoder.ts:137-143`).
func TestBuildCredentialDoubleWritesTheToken(t *testing.T) {
	credential := buildCredential(tokenPayload{
		AccessToken: "tok", RefreshToken: "ref", ExpiresAt: 1234, UID: "u-1", UserName: "nick",
	}, "machine", RegionCN, "")
	if credential.SecurityOAuthToken != "tok" || credential.AccessToken != "tok" {
		t.Fatalf("token was not double-written: %#v", credential)
	}
	if credential.MachineID != "machine" || credential.UID != "u-1" || credential.Nickname != "nick" {
		t.Fatalf("credential lost identity fields: %#v", credential)
	}
	if credential.Region != string(RegionCN) {
		t.Fatalf("region = %q, want %q recorded on the credential", credential.Region, RegionCN)
	}
	if credential.bearerToken() != "tok" {
		t.Fatalf("bearer token = %q", credential.bearerToken())
	}
}

// TestApplyRefreshKeepsFieldsTheResponseOmits is the guard for the failure mode
// where refreshing drops machine_id (breaking the next refresh) or uid (breaking
// the encrypted signature) — `qoder.ts:319-341`.
func TestApplyRefreshKeepsFieldsTheResponseOmits(t *testing.T) {
	original := &Credential{
		SecurityOAuthToken: "old", AccessToken: "old", RefreshToken: "ref-old",
		MachineID: "machine-1", UID: "u-1", Nickname: "nick", Region: string(RegionCN),
	}
	refreshed := original.applyRefresh(tokenPayload{AccessToken: "new"})
	if refreshed.MachineID != "machine-1" {
		t.Fatalf("machine_id = %q, want it preserved", refreshed.MachineID)
	}
	if refreshed.UID != "u-1" {
		t.Fatalf("uid = %q, want it preserved (encrypted inference needs it)", refreshed.UID)
	}
	if refreshed.Nickname != "nick" {
		t.Fatalf("nickname = %q, want it preserved", refreshed.Nickname)
	}
	if refreshed.RefreshToken != "ref-old" {
		t.Fatalf("refresh_token = %q, want the old one kept when the response omits it", refreshed.RefreshToken)
	}
	if refreshed.Region != string(RegionCN) {
		t.Fatalf("region = %q, want it preserved", refreshed.Region)
	}
	if refreshed.bearerToken() != "new" {
		t.Fatalf("access token = %q, want the refreshed value", refreshed.bearerToken())
	}
}

// TestRefreshBodyCarriesOnlyRefreshTokenAndMachineID matches
// `qoder.ts:281-293`: `machine_token` comes from a UMID subsystem this plugin
// does not have, so it is not sent.
func TestRefreshBodyCarriesOnlyRefreshTokenAndMachineID(t *testing.T) {
	credential := &Credential{RefreshToken: "ref", MachineID: "machine"}
	body, errBody := credential.refreshBody()
	if errBody != nil {
		t.Fatalf("refreshBody: %v", errBody)
	}
	var decoded map[string]string
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode refresh body: %v", errUnmarshal)
	}
	if len(decoded) != 2 || decoded["refresh_token"] != "ref" || decoded["machine_id"] != "machine" {
		t.Fatalf("refresh body = %v, want exactly refresh_token and machine_id", decoded)
	}
}

// TestCredentialExpiryAndRefreshability pins the "no expiry is not 1970"
// distinction (`qoder.ts:187-203`, `:276-279`).
func TestCredentialExpiryAndRefreshability(t *testing.T) {
	noExpiry := &Credential{AccessToken: "tok", MachineID: "machine"}
	if noExpiry.Expired(0) {
		t.Fatal("a credential without expire_time must be treated as valid")
	}
	if !noExpiry.ExpiresAt().IsZero() {
		t.Fatalf("ExpiresAt = %v, want the zero time", noExpiry.ExpiresAt())
	}
	if noExpiry.Refreshable() {
		t.Fatal("a credential without refresh_token must not be refreshable")
	}

	expired := &Credential{AccessToken: "tok", ExpireTime: time.Now().Add(-time.Minute).UnixMilli()}
	if !expired.Expired(0) {
		t.Fatal("an expired credential was reported valid")
	}
	valid := &Credential{AccessToken: "tok", ExpireTime: time.Now().Add(time.Hour).UnixMilli()}
	if valid.Expired(time.Minute) {
		t.Fatal("a credential with an hour left was reported expired within the skew")
	}
}

// TestParseCredentialRequiresAToken keeps an unrelated JSON file from being
// claimed by this provider.
func TestParseCredentialRequiresAToken(t *testing.T) {
	if _, errParse := ParseCredential([]byte(`{"machine_id":"m"}`)); errParse == nil {
		t.Fatal("ParseCredential accepted a credential without a token")
	}
	if _, errParse := ParseCredential(nil); errParse == nil {
		t.Fatal("ParseCredential accepted an empty payload")
	}
	if _, errParse := ParseCredential([]byte(`{"access_token":"tok"}`)); errParse != nil {
		t.Fatalf("ParseCredential rejected a usable credential: %v", errParse)
	}
}

// TestDefaultAuthFileNameSeparatesRegions keeps two sites from overwriting each
// other's auth file.
func TestDefaultAuthFileNameSeparatesRegions(t *testing.T) {
	global := &Credential{AccessToken: "tok", UID: "u-1", Region: string(RegionGlobal)}
	cn := &Credential{AccessToken: "tok", UID: "u-1", Region: string(RegionCN)}
	globalName := defaultAuthFileName(global)
	cnName := defaultAuthFileName(cn)
	if globalName == cnName {
		t.Fatalf("both regions produced the same file name %q", globalName)
	}
	if !strings.HasSuffix(globalName, ".json") || !strings.HasPrefix(globalName, "qoder-") {
		t.Fatalf("unexpected file name %q", globalName)
	}
	if !strings.HasPrefix(cnName, "qoder-cn-") {
		t.Fatalf("CN file name = %q, want the qoder-cn- prefix", cnName)
	}
	hostile := &Credential{AccessToken: "tok", Nickname: "../../etc/passwd", Region: string(RegionGlobal)}
	if sanitized := defaultAuthFileName(hostile); strings.Contains(sanitized, "/") {
		t.Fatalf("file name %q still contains a path separator", sanitized)
	}
}

// TestChatHeadersMatchTheAdapter keeps the public endpoint's header set faithful
// (`qoder.ts:303-317`).
func TestChatHeadersMatchTheAdapter(t *testing.T) {
	credential := &Credential{AccessToken: "tok", SecurityOAuthToken: "sec"}
	headers := chatHeaders(credential, productByID(string(RegionGlobal)), "req", "sess")
	if got := headers.Get("Authorization"); got != "Bearer sec" {
		t.Fatalf("Authorization = %q, want the security token preferred", got)
	}
	if headers.Get("Accept") != "text/event-stream" || headers.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected headers: %v", headers)
	}
	if headers.Get("X-Request-ID") != "req" || headers.Get("X-Session-ID") != "sess" {
		t.Fatalf("identity headers missing: %v", headers)
	}
	if headers.Get("User-Agent") != "qoder/1.0.0" {
		t.Fatalf("User-Agent = %q, want qoder/1.0.0", headers.Get("User-Agent"))
	}
}

// TestRandomUUIDShape keeps the identifiers in the shape the payload expects.
func TestRandomUUIDShape(t *testing.T) {
	first, second := randomUUID(), randomUUID()
	if first == second {
		t.Fatal("randomUUID returned the same value twice")
	}
	if len(first) != 36 || first[14] != '4' {
		t.Fatalf("uuid = %q, want an RFC 4122 v4 shape", first)
	}
}
