package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/cpa-jethub/plugins/internal/abiboot"
)

// Credential is the JSON persisted as the CPA auth file for one CodeArts
// account. The field names match Jet-Hub's `CodeArtsCredential` so auth files
// remain interchangeable between the two implementations.
type Credential struct {
	Type            string `json:"type,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	// ExpiresAt is an ISO-8601 timestamp describing when the STS credentials die.
	ExpiresAt string `json:"expires_at"`
	DomainID  string `json:"domain_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	UserName  string `json:"user_name,omitempty"`
	// RefreshToken, CodeVerifier and DpopPrivateKeyJWK are present only for
	// accounts created through the IAM OAuth flow.
	RefreshToken      string                 `json:"refresh_token,omitempty"`
	CodeVerifier      string                 `json:"code_verifier,omitempty"`
	DpopPrivateKeyJWK *DpopPrivateJWK        `json:"dpop_private_key_jwk,omitempty"`
	ModelRateLimits   map[string]any         `json:"model_rate_limits,omitempty"`
	Extra             map[string]interface{} `json:"-"`
}

// DpopPrivateJWK is the persisted ES256 (P-256) key used to build DPoP proofs.
type DpopPrivateJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

// ParseCredential decodes and validates an auth-file payload.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, abiboot.Errorf("invalid_credential", "empty CodeArts credential")
	}
	var credential Credential
	if err := json.Unmarshal(raw, &credential); err != nil {
		return nil, abiboot.Errorf("invalid_credential", "decode CodeArts credential: %v", err)
	}
	if strings.TrimSpace(credential.AccessKeyID) == "" || strings.TrimSpace(credential.SecretAccessKey) == "" {
		return nil, abiboot.Errorf("invalid_credential", "CodeArts credential is missing access_key_id/secret_access_key")
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
		return nil, abiboot.Errorf("encode_credential", "encode CodeArts credential: %v", err)
	}
	return json.RawMessage(raw), nil
}

// Expiry reports when the STS credentials expire. Unparseable timestamps fall
// back to 24 hours from now, matching Jet-Hub's account-pool bookkeeping.
func (c *Credential) Expiry() time.Time {
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(c.ExpiresAt)); err == nil {
		return parsed
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05Z", strings.TrimSpace(c.ExpiresAt)); err == nil {
		return parsed
	}
	return time.Now().Add(24 * time.Hour)
}

// Expired reports whether the STS credentials are past (or within skew of) their
// expiry.
func (c *Credential) Expired(skew time.Duration) bool {
	return time.Now().Add(skew).After(c.Expiry())
}

// Refreshable reports whether the credential can be renewed without a new login.
func (c *Credential) Refreshable() bool {
	return strings.TrimSpace(c.RefreshToken) != "" && strings.TrimSpace(c.CodeVerifier) != "" && c.DpopPrivateKeyJWK != nil
}
