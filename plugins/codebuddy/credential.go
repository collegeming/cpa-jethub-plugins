package main

// 本文件是 Jet-Hub `src/buddy.ts:88-379` 的 Go 移植：凭据结构、JWT 解析、
// 过期时间归一化、令牌/账户响应解析与请求头构造。
//
// 关键约定（全部来自 TS，逐条标注源位置）：
//   - 端点不返回绝对 `expiresAt`，只返回相对的 `expiresIn`/`refreshExpiresIn`，
//     换算基准是 access_token 这个 JWT 的 `iat`（不是当前时刻）。buddy.ts:263-306。
//   - 昵称只在 access_token 的 JWT 声明里，`login/account` 的 nickname 常为空。
//     buddy.ts:343-366。
//   - `scope` 等字段可能带 CR/LF，必须剔除控制字符，否则会破坏 auth 文件。
//     buddy.ts:229-249。

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Credential is the JSON persisted as the CPA auth file for one CodeBuddy-family
// account. Field names match Jet-Hub's `BuddyCredential` (buddy.ts:97-120) so
// auth material remains interchangeable between the two implementations.
type Credential struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresAt        string `json:"expires_at,omitempty"`
	RefreshExpiresAt string `json:"refresh_expires_at,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	Scope            string `json:"scope,omitempty"`
	Domain           string `json:"domain,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	Nickname         string `json:"nickname,omitempty"`
	EnterpriseID     string `json:"enterprise_id,omitempty"`
	AccountType      string `json:"account_type,omitempty"`

	// Product is NOT part of Jet-Hub's BuddyCredential. It records which of the
	// four products this account was created against, so that (a) the executor
	// talks to the right endpoint even after the `product` setting changes and
	// (b) the status page can show it. Auth files imported from Jet-Hub lack
	// the field; those fall back to the configured product.
	Product string `json:"product,omitempty"`
}

// buddyToken mirrors `BuddyToken` (buddy.ts:123-131).
type buddyToken struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        string
	RefreshExpiresAt string
	TokenType        string
	Scope            string
	Domain           string
}

// buddyAccount mirrors `BuddyAccount` (buddy.ts:134-139).
type buddyAccount struct {
	UID           string
	Nickname      string
	EnterpriseID  string
	AccountType   string
	AccountStatus string
}

// ParseCredential decodes and validates an auth-file payload.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, abiboot.Errorf("invalid_credential", "empty CodeBuddy credential")
	}
	var credential Credential
	if err := json.Unmarshal(raw, &credential); err != nil {
		return nil, abiboot.Errorf("invalid_credential", "decode CodeBuddy credential: %v", err)
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return nil, abiboot.Errorf("invalid_credential", "CodeBuddy credential is missing access_token")
	}
	return &credential, nil
}

// Encode serialises the credential for storage in the CPA auth file.
func (c *Credential) Encode() (json.RawMessage, error) {
	c.sanitize()
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, abiboot.Errorf("encode_credential", "encode CodeBuddy credential: %v", err)
	}
	return json.RawMessage(raw), nil
}

// sanitize strips the control characters that would corrupt the auth file.
// buddy.ts:229-249 (readStringField → stripControlChars).
func (c *Credential) sanitize() {
	c.AccessToken = stripControlChars(c.AccessToken)
	c.RefreshToken = stripControlChars(c.RefreshToken)
	c.ExpiresAt = stripControlChars(c.ExpiresAt)
	c.RefreshExpiresAt = stripControlChars(c.RefreshExpiresAt)
	c.TokenType = stripControlChars(c.TokenType)
	c.Scope = stripControlChars(c.Scope)
	c.Domain = stripControlChars(c.Domain)
	c.UserID = stripControlChars(c.UserID)
	c.Nickname = stripControlChars(c.Nickname)
	c.EnterpriseID = stripControlChars(c.EnterpriseID)
	c.AccountType = stripControlChars(c.AccountType)
	c.Product = stripControlChars(c.Product)
}

var (
	controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]+`)
	whitespace   = regexp.MustCompile(`\s{2,}`)
	digitsOnly   = regexp.MustCompile(`^\d+$`)
	numberOnly   = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
)

// stripControlChars removes CR/LF/Tab and collapses runs of whitespace.
// buddy.ts:246-249.
func stripControlChars(value string) string {
	replaced := controlChars.ReplaceAllString(value, " ")
	return strings.TrimSpace(whitespace.ReplaceAllString(replaced, " "))
}

// ── JWT 解析（不验签，仅用于展示与续期调度）──

// jwtPayload decodes the payload segment of a JWT. buddy.ts:168-195.
func jwtPayload(token string) (map[string]any, bool) {
	if token == "" {
		return nil, false
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		// 容错：标准 base64（带填充）也接受。
		if decoded, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil, false
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil, false
	}
	return payload, true
}

// jwtExpiresAtMs reads `exp` (seconds) and converts it to milliseconds.
// buddy.ts:168-178.
func jwtExpiresAtMs(token string) (int64, bool) {
	payload, ok := jwtPayload(token)
	if !ok {
		return 0, false
	}
	return numericClaimMs(payload, "exp")
}

// jwtIssuedAtMs reads `iat` (seconds) and converts it to milliseconds.
// buddy.ts:290-300.
func jwtIssuedAtMs(token string) (int64, bool) {
	payload, ok := jwtPayload(token)
	if !ok {
		return 0, false
	}
	return numericClaimMs(payload, "iat")
}

func numericClaimMs(payload map[string]any, key string) (int64, bool) {
	value, ok := payload[key]
	if !ok {
		return 0, false
	}
	seconds, ok := value.(float64)
	if !ok || seconds != seconds { // NaN guard
		return 0, false
	}
	return int64(seconds * 1000), true
}

// jwtNickname reads the display name from the JWT claims. buddy.ts:184-195.
func jwtNickname(token string) string {
	payload, ok := jwtPayload(token)
	if !ok {
		return ""
	}
	for _, key := range []string{"nickname", "preferred_username", "name"} {
		if value, isString := payload[key].(string); isString {
			return stripControlChars(value)
		}
	}
	return ""
}

// jwtSubject reads `sub` from the JWT claims. buddy.ts:369-379.
func jwtSubject(token string) string {
	payload, ok := jwtPayload(token)
	if !ok {
		return ""
	}
	if value, isString := payload["sub"].(string); isString {
		return stripControlChars(value)
	}
	return ""
}

// ── 过期时间 ──

// credentialExpiresAtMs resolves the absolute expiry in milliseconds:
// numeric/ISO `expires_at` first, then the access-token JWT `exp`.
// buddy.ts:150-162.
func credentialExpiresAtMs(credential *Credential) (int64, bool) {
	if raw := strings.TrimSpace(credential.ExpiresAt); raw != "" {
		if digitsOnly.MatchString(raw) {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err == nil {
				if value > 1_000_000_000_000 {
					return value, true
				}
				return value * 1000, true
			}
		}
		if parsed, ok := parseTimeMs(raw); ok {
			return parsed, true
		}
	}
	return jwtExpiresAtMs(credential.AccessToken)
}

// parseTimeMs parses an ISO-8601-ish timestamp into milliseconds.
func parseTimeMs(value string) (int64, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UnixMilli(), true
		}
	}
	return 0, false
}

// Expiry reports when the credential dies. An unparseable expiry reports the
// zero time, matching Jet-Hub's "unknown expiry is not expired" semantics.
func (c *Credential) Expiry() time.Time {
	if ms, ok := credentialExpiresAtMs(c); ok {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}

// Expired reports whether the credential is past (or within skew of) its
// expiry. An unknown expiry is never treated as expired. buddy.ts:198-201.
func (c *Credential) Expired(skew time.Duration) bool {
	expiry := c.Expiry()
	if expiry.IsZero() {
		return false
	}
	return time.Now().Add(skew).After(expiry)
}

// Refreshable reports whether the credential carries a refresh token.
// buddy.ts:204-206.
func (c *Credential) Refreshable() bool {
	return strings.TrimSpace(c.RefreshToken) != ""
}

// ── 请求头 ──

// credentialRequestHeaders builds X-Domain + User-Agent + optional enterprise
// headers. buddy.ts:209-219.
func credentialRequestHeaders(credential *Credential) map[string]string {
	headers := map[string]string{
		HeaderDomain: credentialDomain(credential, ProductDefault()),
		"User-Agent": BuddyUserAgent,
	}
	if credential.EnterpriseID != "" {
		headers[HeaderEnterpriseID] = credential.EnterpriseID
		headers[HeaderTenantID] = credential.EnterpriseID
	}
	return headers
}

// credentialAuthHeaders adds the Bearer token. buddy.ts:222-227.
func credentialAuthHeaders(credential *Credential) map[string]string {
	headers := credentialRequestHeaders(credential)
	headers["Authorization"] = "Bearer " + credential.AccessToken
	return headers
}

// credentialDomain is the X-Domain value. Jet-Hub's shared helper falls back to
// the credential's own domain (buddy.ts:211); the product-aware call sites in
// `credits.ts:185` and `credits.ts:303` override it with the product domain.
// AGENTS.md ("X-Domain 必须跟随产品") requires the product value, so the
// credential domain is only a last-resort fallback.
func credentialDomain(credential *Credential, product productConfig) string {
	if product.APIDomain != "" {
		return product.APIDomain
	}
	if credential != nil && credential.Domain != "" {
		return credential.Domain
	}
	return APIDomain
}

// ── 令牌 / 账户响应解析 ──

// readStringField reads a string field, tolerating a numeric value.
// buddy.ts:238-243.
func readStringField(record map[string]any, key string) string {
	value, ok := record[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return stripControlChars(typed)
	case float64:
		if typed == typed {
			return strconv.FormatFloat(typed, 'f', -1, 64)
		}
	}
	return ""
}

// readNumberField reads a numeric field, tolerating a numeric string.
// buddy.ts:252-257.
func readNumberField(record map[string]any, key string) (float64, bool) {
	value, ok := record[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		if typed == typed {
			return typed, true
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if numberOnly.MatchString(trimmed) {
			if parsed, err := strconv.ParseFloat(trimmed, 64); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

// absoluteExpiryMs converts a relative `expiresIn` to an absolute millisecond
// timestamp using the access token's `iat` as the base. buddy.ts:263-287.
func absoluteExpiryMs(record map[string]any, absoluteKey, relativeKey, accessToken string) string {
	absolute := readStringField(record, absoluteKey)
	if absolute != "" {
		if digitsOnly.MatchString(absolute) {
			if value, err := strconv.ParseInt(absolute, 10, 64); err == nil {
				if value > 1_000_000_000_000 {
					return strconv.FormatInt(value, 10)
				}
				return strconv.FormatInt(value*1000, 10)
			}
		}
		if parsed, ok := parseTimeMs(absolute); ok {
			return strconv.FormatInt(parsed, 10)
		}
		return absolute
	}
	relativeSeconds, ok := readNumberField(record, relativeKey)
	if !ok {
		// 无相对值：交给 JWT exp 兜底（credentialExpiresAtMs）。
		return ""
	}
	baseMs, hasBase := jwtIssuedAtMs(accessToken)
	if !hasBase {
		baseMs = time.Now().UnixMilli()
	}
	return strconv.FormatInt(baseMs+int64(relativeSeconds*1000), 10)
}

// parseTokenData parses `/v2/plugin/auth/token` (and the refresh response).
// buddy.ts:310-323.
func parseTokenData(data any) buddyToken {
	record, _ := data.(map[string]any)
	if record == nil {
		record = map[string]any{}
	}
	accessToken := readStringField(record, "accessToken")
	tokenType := readStringField(record, "tokenType")
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return buddyToken{
		AccessToken:      accessToken,
		RefreshToken:     readStringField(record, "refreshToken"),
		ExpiresAt:        absoluteExpiryMs(record, "expiresAt", "expiresIn", accessToken),
		RefreshExpiresAt: absoluteExpiryMs(record, "refreshExpiresAt", "refreshExpiresIn", accessToken),
		TokenType:        tokenType,
		Scope:            readStringField(record, "scope"),
		Domain:           readStringField(record, "domain"),
	}
}

// parseAccountData parses `/v2/plugin/login/account`. buddy.ts:332-341.
func parseAccountData(data any) buddyAccount {
	record, _ := data.(map[string]any)
	if record == nil {
		record = map[string]any{}
	}
	accountType := readStringField(record, "type")
	if accountType == "" {
		accountType = "personal"
	}
	return buddyAccount{
		UID:           readStringField(record, "uid"),
		Nickname:      readStringField(record, "nickname"),
		EnterpriseID:  readStringField(record, "enterpriseId"),
		AccountType:   accountType,
		AccountStatus: readStringField(record, "status"),
	}
}

// buildCredential combines token and account data into a storable credential.
// buddy.ts:351-366.
func buildCredential(token buddyToken, account buddyAccount, product productConfig) *Credential {
	nickname := account.Nickname
	if nickname == "" {
		nickname = jwtNickname(token.AccessToken)
	}
	userID := account.UID
	if userID == "" {
		userID = jwtSubject(token.AccessToken)
	}
	credential := &Credential{
		AccessToken:      token.AccessToken,
		RefreshToken:     token.RefreshToken,
		ExpiresAt:        token.ExpiresAt,
		RefreshExpiresAt: token.RefreshExpiresAt,
		TokenType:        token.TokenType,
		Scope:            token.Scope,
		Domain:           token.Domain,
		UserID:           userID,
		Nickname:         nickname,
		EnterpriseID:     account.EnterpriseID,
		AccountType:      account.AccountType,
		Product:          product.ConfigValue,
	}
	credential.sanitize()
	return credential
}
