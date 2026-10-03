package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Credential is the JSON persisted as the CPA auth file for one AtomGit
// account. The field names are the reference's own `AuthInfo` shape
// (`crates/atomcode-auth/src/oauth.rs:143-158`), so a credential stays
// interchangeable with what the official client writes to `auth.toml`.
type Credential struct {
	// Type is the provider key; it tells the host which plugin owns this file.
	Type string `json:"type,omitempty"`

	// AccessToken is sent as `Authorization: Bearer <token>` on every gateway
	// and CodingPlan call. It is an OPAQUE token, not a JWT: the AtomGit broker
	// issues a short random string, so the expiry can only come from
	// `created_at` + `expires_in` (oauth.rs:143-158).
	AccessToken string `json:"access_token"`
	// RefreshToken is rotated on every successful refresh: the broker returns a
	// new access token AND a new refresh token, and the previous access token
	// stops working immediately (oauth.rs:944-960). A refresh that does not
	// persist the new pair strands the account until the user logs in again.
	RefreshToken string `json:"refresh_token,omitempty"`
	// TokenType is the broker's `token_type`, always "Bearer" in practice.
	TokenType string `json:"token_type,omitempty"`
	// ExpiresIn is the token lifetime in seconds as the broker reported it
	// (604800 = 7 days, measured 2026-10-01).
	ExpiresIn int64 `json:"expires_in,omitempty"`
	// CreatedAt is the Unix second at which the token was issued
	// (oauth.rs:535-541). Together with ExpiresIn it is the only expiry source.
	CreatedAt int64 `json:"created_at,omitempty"`
	// User is the AtomGit identity the token belongs to.
	User UserInfo `json:"user"`
}

// UserInfo is the AtomGit account identity (`crates/atomcode-auth/src/oauth.rs:160-168`).
type UserInfo struct {
	// ID is the stable account identity used for the auth file name, the
	// account label and request routing.
	ID string `json:"id"`
	// Username is the login handle, e.g. `qq_23240873`.
	Username string `json:"username,omitempty"`
	// Name is the display name (often Chinese).
	Name string `json:"name,omitempty"`
	// Email is the noreply address the broker reports.
	Email string `json:"email,omitempty"`
	// AvatarURL is the account avatar.
	AvatarURL string `json:"avatar_url,omitempty"`
}

// ParseCredential decodes and validates an auth-file payload.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, abiboot.Errorf("invalid_credential", "空的 AtomCode 凭据")
	}
	var credential Credential
	if err := json.Unmarshal(raw, &credential); err != nil {
		return nil, abiboot.Errorf("invalid_credential", "解析 AtomCode 凭据失败: %v", err)
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return nil, abiboot.Errorf("invalid_credential", "AtomCode 凭据缺少 access_token")
	}
	return &credential, nil
}

// Encode serialises the credential for storage in the CPA auth file.
func (c *Credential) Encode() (json.RawMessage, error) {
	if c.Type == "" {
		c.Type = ProviderKey
	}
	raw, errMarshal := json.Marshal(c)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_credential", "序列化 AtomCode 凭据失败: %v", errMarshal)
	}
	return json.RawMessage(raw), nil
}

// Refreshable reports whether the credential can be renewed without a new login.
func (c *Credential) Refreshable() bool {
	return strings.TrimSpace(c.RefreshToken) != ""
}

// ExpiresAtMS resolves the credential expiry in Unix milliseconds.
//
// There is no JWT to fall back to (the token is opaque), so the only source is
// the issuance record the broker returned. The second return value is false when
// the record is incomplete, in which case the credential is *not* treated as
// expired: a host that imported a token by hand should still be able to use it,
// and the gateway's own 401 is the authority.
func (c *Credential) ExpiresAtMS() (int64, bool) {
	if c.ExpiresIn <= 0 {
		return 0, false
	}
	created := c.CreatedAt
	if created <= 0 {
		// A credential without an issuance time cannot be aged. Treat it as
		// fresh rather than as 1970-expired, which would trigger an endless
		// refresh loop against a token that may well be valid.
		return 0, false
	}
	return (created + c.ExpiresIn) * 1000, true
}

// Expiry renders the credential expiry as a time, for refresh scheduling and
// display. An incomplete record returns the zero time.
func (c *Credential) Expiry() time.Time {
	expiresAt, ok := c.ExpiresAtMS()
	if !ok {
		return time.Time{}
	}
	return time.UnixMilli(expiresAt)
}

// Expired reports whether the credential is past its expiry. An unknown expiry
// is never "expired".
func (c *Credential) Expired() bool {
	expiresAt, ok := c.ExpiresAtMS()
	if !ok {
		return false
	}
	return time.Now().UnixMilli() >= expiresAt
}

// Label is the human-readable account name shown in the management pages.
func (c *Credential) Label() string {
	for _, candidate := range []string{c.User.Name, c.User.Username, c.User.ID} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return "AtomGit 账号"
}

// AccountID is the stable identity used for prefixes and file names.
//
// A nil receiver yields "": the status page renders the catalogue card with no
// account bound (so the page still answers which models the channel serves), and
// a missing credential is exactly the "no identity" case rather than a fault.
func (c *Credential) AccountID() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.User.ID)
}
