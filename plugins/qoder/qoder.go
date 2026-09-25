package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Paths, all from `qoder.ts:22-31`.
const (
	// DeviceSelectPath is mounted on AuthBase: the browser authorization page.
	DeviceSelectPath = "/device/selectAccounts"
	// PollPath is mounted on OpenAPIBase. `qoder.ts:120-126` records the
	// measurement behind that choice: the same path on `qoder.com` answers 401
	// while `openapi.qoder.sh` answers 404 (= no session yet, keep polling).
	PollPath = "/api/v1/deviceToken/poll"
	// RefreshPath is mounted on OpenAPIBase (`qoder.ts:27`).
	RefreshPath = "/api/v1/deviceToken/refresh"
	// UserInfoPath is mounted on OpenAPIBase (`qoder.ts:29`).
	UserInfoPath = "/api/v1/userinfo"
	// PublicChatPath is mounted on InferBase (`qoder.ts:31`). It is the public
	// OpenAI-compatible endpoint; it does NOT recognise the catalog keys.
	PublicChatPath = "/model/v1/chat/completions"
)

// pkceAlphabet is the 66-character RFC 7636 unreserved set, matching the
// alphabet `Y_a()` draws from upstream (`qoder.ts:33-39`).
const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// pkcePair is one verifier/challenge pair (`QoderPkce`, `qoder.ts:41-47`).
type pkcePair struct {
	Verifier  string
	Challenge string
}

// newPKCE generates a verifier of 43..128 characters and its challenge.
//
// The challenge is `base64url(sha256(verifier))` WITHOUT padding: keeping the
// `=` makes the server reject the request (`qoder.ts:49-55`, `:63-67`).
func newPKCE() (*pkcePair, error) {
	length := 43 + randomBelow(86)
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return nil, abiboot.Errorf("pkce_random", "生成 PKCE 随机串失败：%v", err)
	}
	var verifier strings.Builder
	verifier.Grow(length)
	for _, value := range raw {
		verifier.WriteByte(pkceAlphabet[int(value)%len(pkceAlphabet)])
	}
	digest := sha256.Sum256([]byte(verifier.String()))
	return &pkcePair{
		Verifier:  verifier.String(),
		Challenge: base64.RawURLEncoding.EncodeToString(digest[:]),
	}, nil
}

// randomBelow returns a uniform random integer in [0, limit).
func randomBelow(limit int) int {
	if limit <= 1 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0
	}
	return int(value.Int64())
}

// randomUUID returns a lower-case RFC 4122 v4 UUID, the Go equivalent of the
// `randomUUID()` calls in `qoder.ts:91-93` and `qoder-wasm.ts:501-502`.
func randomUUID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing is unrecoverable; fall back to a timestamp so the
		// caller still gets a syntactically valid identifier.
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// deviceSession is one device-code login session (`QoderDeviceSession`,
// `qoder.ts:80-86`). MachineId is generated here and persisted with the
// credential; it is NOT a hardware fingerprint — see `qoder.ts:71-78` for why
// the upstream SMBIOS-based fingerprint is deliberately not reproduced.
type deviceSession struct {
	PKCE      *pkcePair
	Nonce     string
	MachineID string
}

// newDeviceSession builds a fresh session, reusing a machine id when the caller
// already has one (`createQoderDeviceSession`, `qoder.ts:89-95`).
func newDeviceSession(machineID string) (*deviceSession, error) {
	pkce, errPKCE := newPKCE()
	if errPKCE != nil {
		return nil, errPKCE
	}
	if strings.TrimSpace(machineID) == "" {
		machineID = randomUUID()
	}
	return &deviceSession{PKCE: pkce, Nonce: randomUUID(), MachineID: machineID}, nil
}

// authURL is `buildQoderAuthUrl` (`qoder.ts:109-118`).
//
// ⚠️ `client_id` must be the PRODUCTION id. Upstream is
// `client_id: i ? J_a : G_a` where the fourth argument is `isProd()`, so prod
// takes `J_a` (`qoder-product.ts:164-184`). Using the non-prod id makes the
// server reject the authorization callback with "参数无效" (a real reported bug).
func (s *deviceSession) authURL(p *product) string {
	query := url.Values{}
	query.Set("challenge", s.PKCE.Challenge)
	query.Set("challenge_method", "S256")
	query.Set("nonce", s.Nonce)
	query.Set("machine_id", s.MachineID)
	query.Set("client_id", p.ClientID)
	return p.AuthBase + DeviceSelectPath + "?" + query.Encode()
}

// pollURL is `buildQoderPollUrl` (`qoder.ts:127-134`); it hangs off OpenAPIBase.
func (s *deviceSession) pollURL(p *product) string {
	query := url.Values{}
	query.Set("nonce", s.Nonce)
	query.Set("verifier", s.PKCE.Verifier)
	query.Set("challenge_method", "S256")
	return p.OpenAPIBase + PollPath + "?" + query.Encode()
}

// Credential is the JSON persisted as the CPA auth file for one Qoder account.
// Field names match Jet-Hub's `QoderCredential` (`qoder.ts:144-164`) so auth
// material stays interchangeable between the two implementations.
type Credential struct {
	// SecurityOAuthToken and AccessToken are written with the same value:
	// upstream reads `security_oauth_token ?? access_token` (`qoder.ts:137-143`).
	SecurityOAuthToken string `json:"security_oauth_token"`
	AccessToken        string `json:"access_token"`
	RefreshToken       string `json:"refresh_token,omitempty"`
	// ExpireTime is the access-token expiry as a millisecond timestamp
	// (`qoder.ts:148-149`).
	ExpireTime int64 `json:"expire_time,omitempty"`
	// RefreshTokenExpireTime is the refresh-token expiry, milliseconds.
	RefreshTokenExpireTime int64 `json:"refresh_token_expire_time,omitempty"`
	// MachineID is generated by this plugin and must be persisted: the refresh
	// body and the encrypted signature both use it (`qoder.ts:152-153`).
	MachineID string `json:"machine_id"`
	// UID is the device-token `user_id`. The encrypted path REQUIRES it:
	// `generate_runtime_auth_fields` derives `encrypt_user_info` from it, and
	// without it the signature is invalid (`qoder.ts:154-161`,
	// `qoder-adapter.ts:75-88`, `:352-362`).
	UID string `json:"uid,omitempty"`
	// Nickname is the display name (`qoder.ts:162-163`).
	Nickname string `json:"nickname,omitempty"`
	// Region records which site issued the credential. Jet-Hub splits the two
	// sites into two providers; this plugin keeps them in one provider, so the
	// site travels with the credential.
	Region string `json:"region,omitempty"`
	// Type is the CPA auth file discriminator.
	Type string `json:"type,omitempty"`
}

// tokenPayload is `QoderTokenPayload` (`qoder.ts:166-176`).
type tokenPayload struct {
	AccessToken           string
	RefreshToken          string
	ExpiresAt             int64
	RefreshTokenExpiresAt int64
	UID                   string
	UserName              string
}

// parseTokenPayload is `parseQoderTokenPayload` (`qoder.ts:211-235`).
//
// The login response uses `token` while the refresh response uses
// `device_token`, so both are accepted. Garbage in yields an empty access token
// rather than an error; the caller decides that the login failed.
func parseTokenPayload(value any) tokenPayload {
	source, ok := value.(map[string]any)
	if !ok {
		return tokenPayload{}
	}
	return tokenPayload{
		AccessToken:           readStringField(source, "token", "device_token", "access_token"),
		RefreshToken:          readStringField(source, "refresh_token", "refreshToken"),
		ExpiresAt:             readTimestampField(source, "expires_at", "expiresAt"),
		RefreshTokenExpiresAt: readTimestampField(source, "refresh_token_expires_at", "refreshTokenExpiresAt"),
		// The device-token response carries user_id / user_name
		// (`qoder.ts:222-226`); uid is required by the encrypted path.
		UID:      readStringField(source, "user_id", "userId"),
		UserName: readStringField(source, "user_name", "userName"),
	}
}

// parseTokenPayloadJSON is the []byte convenience wrapper.
func parseTokenPayloadJSON(body []byte) tokenPayload {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return tokenPayload{}
	}
	return parseTokenPayload(decoded)
}

// readStringField returns the first non-empty string among keys.
func readStringField(source map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := source[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// readTimestampField parses a millisecond timestamp, tolerating numbers (seconds
// or milliseconds) and ISO strings.
//
// `qoder.ts:187-203`: an unparseable value yields 0, never a fabricated epoch —
// "no expiry" and "expired in 1970" are different things.
func readTimestampField(source map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := source[key].(type) {
		case float64:
			if value > 0 {
				if value < 1e12 {
					return int64(value * 1000)
				}
				return int64(value)
			}
		case json.Number:
			if parsed, err := value.Float64(); err == nil && parsed > 0 {
				if parsed < 1e12 {
					return int64(parsed * 1000)
				}
				return int64(parsed)
			}
		case string:
			if value == "" {
				continue
			}
			if parsed, err := time.Parse(time.RFC3339, value); err == nil {
				return parsed.UnixMilli()
			}
			if parsed, err := time.Parse("2006-01-02T15:04:05Z", value); err == nil {
				return parsed.UnixMilli()
			}
		}
	}
	return 0
}

// buildCredential is `buildQoderCredential` (`qoder.ts:238-258`).
func buildCredential(payload tokenPayload, machineID string, region Region, nickname string) *Credential {
	name := nickname
	if name == "" {
		name = payload.UserName
	}
	return &Credential{
		SecurityOAuthToken:     payload.AccessToken,
		AccessToken:            payload.AccessToken,
		RefreshToken:           payload.RefreshToken,
		ExpireTime:             payload.ExpiresAt,
		RefreshTokenExpireTime: payload.RefreshTokenExpiresAt,
		MachineID:              machineID,
		UID:                    payload.UID,
		Nickname:               name,
		Region:                 string(region),
		Type:                   ProviderKey,
	}
}

// applyRefresh is `applyQoderRefresh` (`qoder.ts:325-341`).
//
// machine_id / uid / nickname are NOT in the refresh response and must survive:
// dropping them breaks the next refresh or the encrypted signature.
func (c *Credential) applyRefresh(payload tokenPayload) *Credential {
	next := buildCredential(payload, c.MachineID, c.regionOr(activeRegion()), c.Nickname)
	if next.RefreshToken == "" {
		next.RefreshToken = c.RefreshToken
	}
	if next.UID == "" {
		next.UID = c.UID
	}
	if next.Nickname == "" {
		next.Nickname = c.Nickname
	}
	return next
}

// bearerToken is `qoderBearerToken` (`qoder.ts:296-300`): the security token
// wins over the access token.
func (c *Credential) bearerToken() string {
	if c.SecurityOAuthToken != "" {
		return c.SecurityOAuthToken
	}
	return c.AccessToken
}

// refreshBody is `qoderRefreshBody` (`qoder.ts:288-293`). Upstream's
// `getMachineIdentityRequestFields` only sends machine fields when both exist,
// and `machine_token` comes from a UMID subsystem this plugin does not have.
func (c *Credential) refreshBody() ([]byte, error) {
	body, err := json.Marshal(map[string]string{
		"refresh_token": c.RefreshToken,
		"machine_id":    c.MachineID,
	})
	if err != nil {
		return nil, abiboot.Errorf("encode_refresh", "encode refresh body: %v", err)
	}
	return body, nil
}

// Refreshable is `isQoderRefreshable` (`qoder.ts:271-273`): what matters is the
// presence of a refresh token, independently of expiry.
func (c *Credential) Refreshable() bool { return strings.TrimSpace(c.RefreshToken) != "" }

// ExpiresAt is `qoderCredentialExpiresAtMs` (`qoder.ts:261-263`).
func (c *Credential) ExpiresAt() time.Time {
	if c.ExpireTime <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.ExpireTime)
}

// Expired is `isQoderExpired` (`qoder.ts:276-279`). A credential with no expiry
// is conservatively treated as valid; the server's 401 decides.
func (c *Credential) Expired(skew time.Duration) bool {
	if c.ExpireTime <= 0 {
		return false
	}
	return !time.Now().Add(skew).Before(c.ExpiresAt())
}

// regionOr falls back to a default region when the credential does not carry one.
func (c *Credential) regionOr(fallback Region) Region {
	if c.Region == "" {
		return fallback
	}
	return normalizeRegion(c.Region)
}

// product returns the site this credential belongs to.
func (c *Credential) product(fallback Region) *product {
	return productByID(string(c.regionOr(fallback)))
}

// Encode serialises the credential for storage in the CPA auth file.
func (c *Credential) Encode() (json.RawMessage, error) {
	if c.Type == "" {
		c.Type = ProviderKey
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, abiboot.Errorf("encode_credential", "encode Qoder credential: %v", err)
	}
	return json.RawMessage(raw), nil
}

// ParseCredential decodes and validates an auth-file payload.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, credentialError("invalid_credential", "empty Qoder credential")
	}
	var credential Credential
	if err := json.Unmarshal(raw, &credential); err != nil {
		return nil, credentialError("invalid_credential", "decode Qoder credential: %v", err)
	}
	if strings.TrimSpace(credential.bearerToken()) == "" {
		return nil, credentialError("invalid_credential", "Qoder credential is missing security_oauth_token/access_token")
	}
	return &credential, nil
}

// chatHeaders is `qoderChatHeaders` (`qoder.ts:303-317`) for the PUBLIC endpoint.
func chatHeaders(c *Credential, p *product, requestID, sessionID string) http.Header {
	return canonicalHeader(
		"Authorization", "Bearer "+c.bearerToken(),
		"Accept", "text/event-stream",
		"Content-Type", "application/json",
		"X-Request-ID", requestID,
		"X-Session-ID", sessionID,
		"User-Agent", p.UserAgentPrefix+"/1.0.0",
	)
}

// canonicalHeader builds a header map through Set.
//
// Building the literal map instead would store the caller's spelling verbatim,
// and a later `Header.Get` (which canonicalises its argument) would then miss the
// entry — the classic "the header was silently dropped" bug. Going through Set
// also keeps the host from emitting two differently-cased copies of one header.
func canonicalHeader(pairs ...string) http.Header {
	header := http.Header{}
	for index := 0; index+1 < len(pairs); index += 2 {
		header.Set(pairs[index], pairs[index+1])
	}
	return header
}
