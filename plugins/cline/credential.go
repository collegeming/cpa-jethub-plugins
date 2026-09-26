package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Credential is the JSON persisted as the CPA auth file for one Cline account.
//
// Field names match Jet-Hub's `ClineCredential` (`cline.ts:32-50`) so auth
// material stays interchangeable between the two implementations. Note what is
// NOT here: no machine id, no device fingerprint, no HMAC key. The whole Cline
// provider is a plain HTTPS + `Bearer workos:<token>` job (spec §9).
type Credential struct {
	// AccessToken is the Cline access token, `workos:` prefix included. The
	// prefix is load-bearing and must never be stripped (`cline.ts:178-182`).
	AccessToken string `json:"access_token"`
	// RefreshToken is the Cline refresh token. Its absence makes the credential
	// terminal — only a new login can fix it (`cline.ts:240-242`).
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpireTime is the access-token expiry as a millisecond epoch
	// (`cline.ts:38`). Absent ⇒ treated as non-expired and left to a server 401.
	ExpireTime int64 `json:"expire_time,omitempty"`
	// AccountID is the Cline account id (`usr-…`, from `userInfo.clineUserId`).
	// Optional for inference, but the balance endpoint cannot be called without
	// it (`cline-credits.ts:177-180`).
	AccountID string `json:"account_id,omitempty"`
	// Email is display-only (`cline.ts:47`).
	Email string `json:"email,omitempty"`
	// Nickname is the display name. The source resolves
	// `email ?? displayName ?? accountId` at render time (`cline.ts:193-195`);
	// the raw first/last name pair is kept here.
	Nickname string `json:"nickname,omitempty"`
	// Type is the CPA auth file discriminator.
	Type string `json:"type,omitempty"`
}

// clineTokens is the parsed `{success, data:{...}}` envelope returned by both
// `/api/v1/auth/register` and `/api/v1/auth/refresh` (`cline.ts:121-157`).
type clineTokens struct {
	AccessToken  string
	RefreshToken string
	ExpireTime   int64
	AccountID    string
	Email        string
	Nickname     string
}

// clineBearerValue is `clineBearerValue` (`cline.ts:178-182`).
//
// The prefix is prepended IFF absent — idempotent, not forced. This is the
// single highest-risk behavioural detail of the provider: the same credential
// answers 200 as `Bearer workos:eyJ…` and 401 as `Bearer eyJ…`, with a body that
// blames the client version (spec risk 8).
func clineBearerValue(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, TokenPrefix) {
		return trimmed
	}
	return TokenPrefix + trimmed
}

// bearerValue is the Authorization value for every authenticated request. The
// returned string still has to be prefixed with "Bearer ".
func (c *Credential) bearerValue() string {
	return clineBearerValue(c.AccessToken)
}

// Refreshable reports whether the credential can be renewed
// (`isClineRefreshable`, `cline.ts:271-273`): only the presence of a refresh
// token matters, independently of expiry.
func (c *Credential) Refreshable() bool { return strings.TrimSpace(c.RefreshToken) != "" }

// ExpiresAt is `clineCredentialExpiresAtMs` (`cline.ts:261-263`). A zero time
// means "no expiry information", not "expired in 1970".
func (c *Credential) ExpiresAt() time.Time {
	if c.ExpireTime <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.ExpireTime)
}

// Expired is `isClineExpired` (`cline.ts:276-279`). A credential without an
// expiry is conservatively treated as valid; the server's 401 decides.
func (c *Credential) Expired(skew time.Duration) bool {
	if c.ExpireTime <= 0 {
		return false
	}
	return !time.Now().Add(skew).Before(c.ExpiresAt())
}

// Label is the display name resolution of `cline.ts:193-195`.
func (c *Credential) Label() string {
	for _, candidate := range []string{c.Nickname, c.Email, c.AccountID} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// buildCredential is `buildClineCredential` (`cline.ts:185-204`): it normalises
// the token through the prefix helper and nothing else.
func buildCredential(tokens clineTokens) *Credential {
	return &Credential{
		AccessToken:  clineBearerValue(tokens.AccessToken),
		RefreshToken: strings.TrimSpace(tokens.RefreshToken),
		ExpireTime:   tokens.ExpireTime,
		AccountID:    strings.TrimSpace(tokens.AccountID),
		Email:        strings.TrimSpace(tokens.Email),
		Nickname:     strings.TrimSpace(tokens.Nickname),
		Type:         ProviderKey,
	}
}

// applyRefresh is `applyClineRefresh` (`cline.ts:213-227`).
//
// account_id / email / nickname are NOT part of the refresh response and must
// survive: dropping them breaks the balance endpoint (which needs account_id)
// and the display label.
func (c *Credential) applyRefresh(tokens clineTokens) *Credential {
	next := buildCredential(tokens)
	if next.AccessToken == "" {
		next.AccessToken = c.AccessToken
	}
	if next.RefreshToken == "" {
		next.RefreshToken = c.RefreshToken
	}
	if next.ExpireTime == 0 {
		next.ExpireTime = c.ExpireTime
	}
	if next.AccountID == "" {
		next.AccountID = c.AccountID
	}
	if next.Email == "" {
		next.Email = c.Email
	}
	if next.Nickname == "" {
		next.Nickname = c.Nickname
	}
	return next
}

// Encode serialises the credential for storage in the CPA auth file.
func (c *Credential) Encode() (json.RawMessage, error) {
	if c.Type == "" {
		c.Type = ProviderKey
	}
	raw, errMarshal := json.Marshal(c)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_credential", "encode Cline credential: %v", errMarshal)
	}
	return json.RawMessage(raw), nil
}

// ParseCredential decodes and validates an auth-file payload.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, credentialError("invalid_credential", "Cline 凭据为空")
	}
	var credential Credential
	if errUnmarshal := json.Unmarshal(raw, &credential); errUnmarshal != nil {
		return nil, credentialError("invalid_credential", "解码 Cline 凭据失败：%v", errUnmarshal)
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return nil, credentialError("invalid_credential", "Cline 凭据缺少 access_token")
	}
	return &credential, nil
}

// parseTokenEnvelope is `parseClineTokenResponse` (`cline.ts:121-157`).
//
// The parser is deliberately tolerant: it uses the `data` object when there is
// one and the top level otherwise, accepts camelCase and snake_case spellings,
// and reads the user block from `userInfo` or the top level. `success` is NOT
// required — only a non-empty token is (the official client's stricter
// `success && data.accessToken` check is described in a comment at
// `cline.ts:115-116` and deliberately not reproduced).
func parseTokenEnvelope(body []byte) clineTokens {
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		return clineTokens{}
	}
	payload := decoded
	if data, okData := decoded["data"].(map[string]any); okData {
		payload = data
	}
	tokens := clineTokens{
		AccessToken:  readStringField(payload, "accessToken", "access_token"),
		RefreshToken: readStringField(payload, "refreshToken", "refresh_token"),
		ExpireTime:   parseExpireTime(firstField(payload, "expiresAt", "expires_at", "expire_time")),
		AccountID:    readStringField(payload, "accountId", "account_id"),
		Email:        readStringField(payload, "email"),
	}
	if user, okUser := payload["userInfo"].(map[string]any); okUser {
		if account := readStringField(user, "clineUserId", "accountId", "account_id"); account != "" {
			tokens.AccountID = account
		}
		if email := readStringField(user, "email"); email != "" {
			tokens.Email = email
		}
		tokens.Nickname = strings.TrimSpace(
			strings.TrimSpace(readStringField(user, "firstName")) + " " + strings.TrimSpace(readStringField(user, "lastName")))
	}
	return tokens
}

// parseExpireTime is `toEpochMs` (`cline.ts:89-99`). A number below 1e12 is a
// second-precision epoch, anything at or above it is already milliseconds; a
// string goes through the date parser. Unparseable input yields 0, never a
// fabricated epoch.
func parseExpireTime(value any) int64 {
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			if typed < 1e12 {
				return int64(typed * 1000)
			}
			return int64(typed)
		}
	case json.Number:
		if parsed, errNumber := typed.Float64(); errNumber == nil && parsed > 0 {
			if parsed < 1e12 {
				return int64(parsed * 1000)
			}
			return int64(parsed)
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0
		}
		// A numeric string is an epoch spelled defensively; the host normally
		// rewrites those into numbers (see credential_json.go), so this only
		// covers hand-written auth files.
		if parsed, errNumber := strconv.ParseFloat(trimmed, 64); errNumber == nil {
			return parseExpireTime(parsed)
		}
		for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05"} {
			if parsed, errParse := time.Parse(layout, trimmed); errParse == nil {
				return parsed.UnixMilli()
			}
		}
	}
	return 0
}

// firstField returns the first present key's value, so parseExpireTime can see
// whichever spelling the response used.
func firstField(source map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, present := source[key]; present {
			return value
		}
	}
	return nil
}

// readStringField returns the first non-empty trimmed string among keys.
func readStringField(source map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, okValue := source[key].(string); okValue {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// readNumberField reads a JSON number that may also arrive as a numeric string,
// which is what the balance endpoint is documented to send
// (`cline-credits.ts:92-100`).
func readNumberField(source map[string]any, key string) (float64, bool) {
	switch typed := source[key].(type) {
	case float64:
		return typed, true
	case json.Number:
		if parsed, errNumber := typed.Float64(); errNumber == nil {
			return parsed, true
		}
	case string:
		if parsed, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64); errParse == nil {
			return parsed, true
		}
	}
	return 0, false
}

// sanitizeFileName strips anything that cannot appear in an auth file name.
func sanitizeFileName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.' || r == '_' || r == '@' || r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}
