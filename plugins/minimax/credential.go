package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Credential is the JSON persisted for one MiniMax Code account. Field names
// match the reference's `MinimaxCredential` (`minimax.ts:MinimaxCredential`) so
// auth material stays interchangeable between the two implementations.
type Credential struct {
	// AccessToken is the OAuth bearer token.
	//
	// ⚠️ The FIELD NAME is mandatory: for every provider other than `codearts`
	// the account pool reads `access_token` as the account identity. The value
	// is `mmoat_`-prefixed and about 60 characters — NOT a JWT, so nothing can
	// be decoded out of it (`minimax.ts:decodeJwtExpMs` and the measured shape
	// recorded there: 60 characters, zero dots).
	AccessToken string `json:"access_token"`
	// RefreshToken is `mmort_`-prefixed and also not a JWT. MiniMax DOES run a
	// refresh grant, so a credential carrying one is refreshable
	// (`minimax-oauth.ts:buildMinimaxRefreshBody`).
	RefreshToken string `json:"refresh_token,omitempty"`
	// TokenType is always `Bearer`; the grant is rejected unless it is.
	TokenType string `json:"token_type,omitempty"`
	// ExpiresAt is a MILLISECOND epoch timestamp, as a STRING.
	//
	// ⚠️ It is computed from the grant's `expires_in` and is the ONLY usable
	// expiry source: there is no JWT to decode. A grant that omits `expires_in`
	// is rejected outright rather than stored without an expiry, because a
	// credential with no expiry is never renewed and silently dies.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Scope is the space-separated scope string the grant returned. It always
	// contains `agent.default` (the grant is rejected otherwise).
	Scope string `json:"scope,omitempty"`
	// AccountID is the JWT `sub`, when the token happens to be a JWT. For real
	// MiniMax credentials this is always empty — recorded only so a future
	// upstream switch to JWTs needs no schema change.
	AccountID string `json:"account_id,omitempty"`
	// Nickname is a display label. The OAuth flow returns none, so it stays
	// empty unless an operator sets one.
	Nickname string `json:"nickname,omitempty"`
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
// (`isMinimaxRefreshable`, `minimax.ts`).
func (c *Credential) Refreshable() bool {
	return c != nil && strings.TrimSpace(c.RefreshToken) != ""
}

// ExpiresAtMS parses `expires_at`.
//
// ⚠️ ONLY a pure decimal string is accepted, and a value at or below the
// millisecond/second boundary is read as SECONDS and scaled
// (`minimax.ts:minimaxCredentialExpiresAtMs`). Two consequences the reference
// spells out and this port keeps:
//
//   - `Number(' 123 ')` silently becomes 123 and `Number(”)` becomes 0 in
//     JavaScript, so a lenient parse would turn an empty or padded field into
//     a bogus instant. A non-numeric value is therefore treated as ABSENT.
//   - a seconds value read as milliseconds lands in 1970, which reads as
//     "always expired" and triggers a pointless renewal on every single use.
//
// A missing or unusable value counts as ABSENT (0), which is a different thing
// from "expired at the epoch".
func (c *Credential) ExpiresAtMS() int64 {
	if c == nil {
		return 0
	}
	text := strings.TrimSpace(c.ExpiresAt)
	if text == "" || !isDecimalDigits(text) {
		return 0
	}
	value, errParse := strconv.ParseInt(text, 10, 64)
	if errParse != nil || value <= 0 {
		return 0
	}
	// Values beyond 1e12 are already milliseconds; anything smaller is seconds.
	if value <= 1_000_000_000_000 {
		value *= 1000
	}
	return value
}

// isDecimalDigits reports whether a string is a non-empty run of ASCII digits.
func isDecimalDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Expiry resolves the credential's expiry.
//
// `expires_at` is authoritative; the JWT `exp` is only a compatibility
// fallback for a token that happens to be a JWT (real MiniMax tokens are not).
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
// ⚠️ No expiry at all means NOT expired (`isMinimaxExpired`,
// `minimax.ts`): the server stays the only authority, and blocking the user on
// a guess is worse than trying a possibly stale token.
func (c *Credential) Expired(now time.Time) bool {
	expiry := c.Expiry()
	if expiry.IsZero() {
		return false
	}
	return !now.Before(expiry)
}

// NeedsRefresh reports whether the credential is inside the pre-refresh window.
// A credential with no resolvable expiry never reports a need: there is nothing
// to count down to.
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
		return nil, abiboot.Errorf("encode_credential", "encode MiniMax credential: %v", errMarshal)
	}
	return json.RawMessage(raw), nil
}

// ParseCredential decodes and validates an auth-file payload.
//
// The source is lenient on purpose: a JSON parse failure or a non-string
// `access_token` means "not one of ours" rather than a thrown error
// (`parseMinimaxCredential`, `minimax-auth.ts`), and every caller treats the
// error that way.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, credentialError("invalid_credential", "空的 MiniMax 凭据")
	}
	var credential Credential
	if errUnmarshal := json.Unmarshal(raw, &credential); errUnmarshal != nil {
		return nil, credentialError("invalid_credential", "解析 MiniMax 凭据失败：%v", errUnmarshal)
	}
	if credential.Session() == "" {
		return nil, credentialError("invalid_credential", "MiniMax 凭据缺少 access_token 字符串")
	}
	return &credential, nil
}

// jwtExpiryMS decodes a JWT payload and returns its `exp` in milliseconds.
//
// No signature verification: this is a local clock, not an authorisation
// decision, and the value is only ever used to decide WHEN to ask the server
// for a new token. It exists for compatibility only — a real MiniMax
// access_token has zero dots and never reaches the decode
// (`minimax.ts:decodeJwtExpMs`).
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
	millis := seconds * 1000
	// A wildly large `exp` overflows to +Inf, which downstream would read as
	// "never expires" — an invented instant. Refuse it instead.
	if millis <= 0 || millis > 1e15 {
		return 0
	}
	return int64(millis)
}

// jwtSubject decodes a JWT payload's `sub`, or "".
func jwtSubject(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if errDecode != nil {
		payload, errDecode = base64.URLEncoding.DecodeString(parts[1])
		if errDecode != nil {
			return ""
		}
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(claims.Sub)
}

// displayLabel is the account label the manager shows.
//
// The OAuth flow returns no nickname and a real token is not a JWT, so the
// label usually falls back to a short token prefix. That prefix ROTATES on
// every renewal — it is a display label only and must never be used to name a
// file (see `defaultAuthFileName`).
func (c *Credential) displayLabel() string {
	if c == nil {
		return ProviderKey
	}
	if nickname := strings.TrimSpace(c.Nickname); nickname != "" {
		return nickname
	}
	if accountID := strings.TrimSpace(c.AccountID); accountID != "" {
		return accountID
	}
	if prefix := tokenPrefix(c.Session()); prefix != "" {
		return "MiniMax " + prefix
	}
	return ProviderKey
}

// tokenPrefix is the first eight characters of a bearer token, for display.
func tokenPrefix(token string) string {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) <= 8 {
		return trimmed
	}
	return trimmed[:8]
}

// maskedToken renders a token for a screenshot-able page.
func (c *Credential) maskedToken() string {
	if c == nil {
		return "未登录"
	}
	trimmed := strings.TrimSpace(c.Session())
	switch {
	case trimmed == "":
		return "未登录"
	case len(trimmed) <= 12:
		return trimmed
	default:
		return trimmed[:8] + "…" + trimmed[len(trimmed)-4:]
	}
}
