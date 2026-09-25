package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Credential is the JSON persisted as the CPA auth file for one TRAE account.
// Field names match Jet-Hub's `TraeCredential` (trae.ts:89-127) so auth material
// stays interchangeable between the two implementations.
type Credential struct {
	// Type is the provider key; it tells the host which plugin owns this file.
	Type string `json:"type,omitempty"`
	// AccessToken is sent as `Authorization: Cloud-IDE-JWT <token>`.
	AccessToken string `json:"access_token"`
	// RefreshToken is rotated by ExchangeToken; a refresh must write the new
	// value back (trae.ts:20-21, docs/agents/trae.md:45).
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt is a **millisecond** timestamp held as a string, matching the
	// Jet-Hub storage convention (trae.ts:95-101).
	ExpiresAt string `json:"expires_at,omitempty"`
	// UID is the account identity used for pooling and check-in derivation.
	UID string `json:"uid"`
	// Nickname is display-only.
	Nickname string `json:"nickname,omitempty"`
	// MachineID is a 32-hex device fingerprint. It is generated at login and
	// must never be regenerated on refresh (trae.ts:106-113).
	MachineID string `json:"machine_id"`
	// DeviceID is a 32-hex check-in device number; it must differ per account
	// (trae.ts:114-120).
	DeviceID string `json:"device_id"`
	// Domain is informational (`trae.cn`).
	Domain string `json:"domain,omitempty"`
	// APIHost pins the OAuth host this credential was minted against so a
	// refresh never crosses regions (trae-auth.ts:325).
	APIHost string `json:"api_host,omitempty"`
	// Region records which site produced the credential ("trae"/"trae-intl").
	Region string `json:"region,omitempty"`
	// EnterpriseID comes from the callback's `TenantID` field (trae-oauth.ts:288).
	EnterpriseID string `json:"enterprise_id,omitempty"`
}

// ParseCredential decodes and validates an auth-file payload.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, abiboot.Errorf("invalid_credential", "空的 TRAE 凭据")
	}
	var credential Credential
	if err := json.Unmarshal(raw, &credential); err != nil {
		return nil, abiboot.Errorf("invalid_credential", "解析 TRAE 凭据失败: %v", err)
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return nil, abiboot.Errorf("invalid_credential", "TRAE 凭据缺少 access_token")
	}
	return &credential, nil
}

// Encode serialises the credential for storage in the CPA auth file.
func (c *Credential) Encode() (json.RawMessage, error) {
	if c.Type == "" {
		c.Type = ProviderKey
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, abiboot.Errorf("encode_credential", "序列化 TRAE 凭据失败: %v", err)
	}
	return json.RawMessage(raw), nil
}

// Refreshable reports whether the credential can be renewed without a new login.
func (c *Credential) Refreshable() bool {
	return strings.TrimSpace(c.RefreshToken) != ""
}

// ExpiresAtMS resolves the credential expiry in Unix milliseconds
// (trae.ts:139-150):
//
//  1. `expires_at` parsed as a millisecond timestamp, a second timestamp or an
//     ISO-8601 string;
//  2. otherwise the `exp` claim of the access token.
//
// The second return value is false when neither source yields a value, in which
// case the credential is *not* treated as expired (trae.ts:152-156).
func (c *Credential) ExpiresAtMS() (int64, bool) {
	if raw := strings.TrimSpace(c.ExpiresAt); raw != "" {
		if isDigits(raw) {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err == nil {
				// Values above 1e12 are already milliseconds.
				if value > 1_000_000_000_000 {
					return value, true
				}
				return value * 1000, true
			}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, raw); err == nil {
				return parsed.UnixMilli(), true
			}
		}
	}
	if exp, ok := jwtExpiresAtMS(c.AccessToken); ok {
		return exp, true
	}
	return 0, false
}

// Expired reports whether the credential is past its expiry. An unparseable
// expiry is not treated as expired.
func (c *Credential) Expired() bool {
	expiresAt, ok := c.ExpiresAtMS()
	if !ok {
		return false
	}
	return time.Now().UnixMilli() >= expiresAt
}

// Expiry renders the credential expiry as a time, for refresh scheduling and
// display. Unparseable expiries return the zero time.
func (c *Credential) Expiry() time.Time {
	expiresAt, ok := c.ExpiresAtMS()
	if !ok {
		return time.Time{}
	}
	return time.UnixMilli(expiresAt)
}

// jwtExpiresAtMS reads the `exp` claim of a JWT and converts it to milliseconds
// (buddy.ts:168-178). The signature is deliberately not verified: the value is
// only used for display and refresh scheduling.
func jwtExpiresAtMS(token string) (int64, bool) {
	if token == "" {
		return 0, false
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some tokens pad the segment; retry with the standard URL alphabet.
		decoded, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return 0, false
		}
	}
	var payload struct {
		Exp *float64 `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &payload); err != nil || payload.Exp == nil {
		return 0, false
	}
	return int64(*payload.Exp * 1000), true
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
