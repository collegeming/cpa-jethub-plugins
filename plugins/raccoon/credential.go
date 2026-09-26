package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Credential is the JSON persisted for one Raccoon Work account. Field names
// match the reference's `RaccoonCredential` (`raccoon.ts:71-106`) so auth
// material stays interchangeable between the two implementations.
type Credential struct {
	// AccessToken is the JWT the vendor issues.
	//
	// ⚠️ The FIELD NAME is mandatory: for every provider other than `codearts`
	// the account pool reads `access_token` as the account identity
	// (`account-pool.ts:417-429`, `raccoon.ts:73-77`). It ROTATES on every
	// refresh and whenever the vendor's own client restarts, so nothing durable
	// may be keyed on it (trap #16).
	AccessToken string `json:"access_token"`
	// RefreshToken may legitimately be EMPTY: the vendor is not required to
	// return one, and a QR login can produce `refresh_token: ""`
	// (`raccoon-oauth.ts:241`). An empty value means "not refreshable".
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt is the JWT `exp` in MILLISECONDS, as a STRING
	// (`String(exp*1000)`, `raccoon-oauth.ts:245`). It is omitted when the JWT
	// does not decode; the JWT itself is then the only expiry source.
	ExpiresAt string `json:"expires_at,omitempty"`
	// OfficeIdentity is `personal` or an organisation code. It becomes the
	// `X-Org-Code` header and is OMITTED when empty (`raccoon.ts:246-251`); the
	// header is still sent, as an empty string.
	OfficeIdentity string `json:"office_identity,omitempty"`
	// UserID is `user_info.id`, backfilled when missing (`raccoon-auth.ts:230`).
	UserID string `json:"user_id,omitempty"`
	// Nickname is the server's `user_info.name`. It is an AUTO-GENERATED
	// default (`RaccoonAva`) and WeChat returns no nickname, so it collides
	// across accounts (trap #18) — display only.
	Nickname string `json:"nickname,omitempty"`
	// Phone is the bound mobile number, added specifically to tell accounts
	// apart (`raccoon.ts:98-103`).
	Phone string `json:"phone,omitempty"`
	// Type is the CPA auth file discriminator.
	Type string `json:"type,omitempty"`
}

// Session is the bearer token every authenticated endpoint expects.
func (c *Credential) Session() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.AccessToken)
}

// Refreshable reports whether the credential carries a usable refresh token
// (`isRaccoonRefreshable`, `raccoon.ts:164-172`).
func (c *Credential) Refreshable() bool {
	return c != nil && strings.TrimSpace(c.RefreshToken) != ""
}

// ExpiresAtMS parses `expires_at`. A missing, empty, non-numeric or
// non-positive value counts as ABSENT, which is a different thing from
// "expired at the epoch" (`raccoon.ts:142-150`).
func (c *Credential) ExpiresAtMS() int64 {
	if c == nil {
		return 0
	}
	text := strings.TrimSpace(c.ExpiresAt)
	if text == "" {
		return 0
	}
	value, errParse := strconv.ParseInt(text, 10, 64)
	if errParse != nil || value <= 0 {
		return 0
	}
	return value
}

// Expiry resolves the credential's expiry: `expires_at` first, then the JWT's
// `exp` decoded locally without signature verification.
//
// ⚠️ The JWT fallback is LOAD-BEARING, not cosmetic: reading only `expires_at`
// makes expiry permanently false for old or hand-imported credentials, so a
// refresh never fires and the credential expires silently
// (`raccoon.ts:131-150`, trap #17).
func (c *Credential) Expiry() time.Time {
	if value := c.ExpiresAtMS(); value > 0 {
		return time.UnixMilli(value)
	}
	if value := jwtExpiryMS(c.Session()); value > 0 {
		return time.UnixMilli(value)
	}
	return time.Time{}
}

// Expired reports whether the credential is past its expiry.
//
// ⚠️ No expiry at all means NOT expired (`raccoon.ts:152-162`): the server stays
// the only authority, and blocking the user on a guess is worse than trying a
// possibly stale token.
func (c *Credential) Expired(now time.Time) bool {
	expiry := c.Expiry()
	if expiry.IsZero() {
		return false
	}
	return !now.Before(expiry)
}

// NeedsRefresh reports whether the credential is inside the pre-refresh window.
//
// This is the port's DELIBERATE divergence from the reference, where the 300 s
// constant is dead code and refresh only fires once the token has already
// expired (trap #12). A credential with no resolvable expiry never reports a
// need: there is nothing to count down to.
func (c *Credential) NeedsRefresh(now time.Time, window time.Duration) bool {
	expiry := c.Expiry()
	if expiry.IsZero() {
		return false
	}
	if window < 0 {
		window = 0
	}
	return !now.Before(expiry.Add(-window))
}

// Encode serialises the credential for storage in the CPA auth file.
func (c *Credential) Encode() (json.RawMessage, error) {
	if c.Type == "" {
		c.Type = ProviderKey
	}
	raw, errMarshal := json.Marshal(c)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_credential", "encode Raccoon credential: %v", errMarshal)
	}
	return json.RawMessage(raw), nil
}

// ParseCredential decodes and validates an auth-file payload.
//
// The source is lenient on purpose: a JSON parse failure or a non-string
// `access_token` means "not one of ours" rather than a thrown error
// (`raccoon-auth.ts:106-116`), and every caller treats the error that way.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, credentialError("invalid_credential", "空的 Raccoon 凭据")
	}
	var credential Credential
	if errUnmarshal := json.Unmarshal(raw, &credential); errUnmarshal != nil {
		return nil, credentialError("invalid_credential", "解析 Raccoon 凭据失败：%v", errUnmarshal)
	}
	if credential.Session() == "" {
		return nil, credentialError("invalid_credential", "Raccoon 凭据缺少 access_token 字符串")
	}
	return &credential, nil
}

// buildCredential assembles the credential a successful login produces
// (`credentialFromEnvelope`, `raccoon-oauth.ts:238-253`).
//
// `expires_at` comes from the JWT and is OMITTED when it does not decode; no
// local TTL is invented, because the vendor's tokens carry a real `exp`
// (observed lifetime `exp - nbf` ≈ 3 hours, `raccoon.ts:48-51`).
func buildCredential(accessToken, refreshToken, officeIdentity string) *Credential {
	credential := &Credential{
		AccessToken:    strings.TrimSpace(accessToken),
		RefreshToken:   strings.TrimSpace(refreshToken),
		OfficeIdentity: strings.TrimSpace(officeIdentity),
		Type:           ProviderKey,
	}
	if expiry := jwtExpiryMS(credential.AccessToken); expiry > 0 {
		credential.ExpiresAt = strconv.FormatInt(expiry, 10)
	}
	return credential
}

// jwtExpiryMS decodes a JWT payload and returns its `exp` in milliseconds.
//
// No signature verification: this is a local clock, not an authorisation
// decision, and the value is only ever used to decide WHEN to ask the server
// for a new token (`decodeJwtExpMs`, `raccoon.ts:115-129`).
func jwtExpiryMS(token string) int64 {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return 0
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if errDecode != nil {
		// Some issuers pad the segment; retry with padding before giving up.
		payload, errDecode = base64.URLEncoding.DecodeString(parts[1])
		if errDecode != nil {
			return 0
		}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if errUnmarshal := decoder.Decode(&claims); errUnmarshal != nil {
		return 0
	}
	seconds, errParse := strconv.ParseFloat(strings.TrimSpace(claims.Exp.String()), 64)
	if errParse != nil || seconds <= 0 {
		return 0
	}
	return int64(seconds * 1000)
}

// displayLabel is the account label the manager shows.
//
// The server's `name` is auto-generated and collides across accounts, so the
// phone's last four digits disambiguate it and the suffix is skipped when it is
// already present (`buildRaccoonNickname`, `jet-hub-rpc.ts:242-261`, trap #18).
func (c *Credential) displayLabel() string {
	if c == nil {
		return ProviderKey
	}
	nickname := strings.TrimSpace(c.Nickname)
	last4 := last4Digits(c.Phone)
	if nickname == "" {
		if userID := strings.TrimSpace(c.UserID); userID != "" {
			return userID
		}
		if last4 != "" {
			return "Raccoon " + last4
		}
		return ProviderKey
	}
	if last4 == "" {
		return nickname
	}
	if strings.Contains(nickname, "("+last4+")") {
		return nickname
	}
	return nickname + " (" + last4 + ")"
}

// maskedPhone renders a phone number for management pages without publishing the
// full number in a screenshot-able page.
func (c *Credential) maskedPhone() string {
	if c == nil {
		return "未绑定"
	}
	phone := strings.TrimSpace(c.Phone)
	if len(phone) != 11 {
		if phone == "" {
			return "未绑定"
		}
		return phone
	}
	return phone[:3] + "****" + phone[7:]
}

// last4Digits returns the last four characters of a phone number, or "".
func last4Digits(phone string) string {
	trimmed := strings.TrimSpace(phone)
	if len(trimmed) < 4 {
		return ""
	}
	return trimmed[len(trimmed)-4:]
}
