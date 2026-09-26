package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Credential is the JSON persisted for one Loomy account. Field names match
// Jet-Hub's `LoomyCredential` (`loomy.ts:54-70`) so auth material stays
// interchangeable between the two implementations.
type Credential struct {
	// AccessToken is the iFlytek `session`: 32 lowercase hex characters.
	//
	// ⚠️ The FIELD NAME is mandatory: for every provider other than `codearts`
	// the account pool reads `access_token` as the account identity
	// (`account-pool.ts:369-373`, trap #20). Renaming it makes two different
	// credentials look unidentifiable.
	AccessToken string `json:"access_token"`
	// UserID is the iFlytek user id, an 18-digit numeric string (`loomy.ts:57-58`).
	UserID string `json:"userid"`
	// Phone is the bound mobile number. It may legitimately be EMPTY: a WeChat
	// `bind/skip` login persists an empty phone (`loomy-wechat-login.ts:246`,
	// `loomy.ts:59-60`), so nothing here may key on it.
	Phone string `json:"phone"`
	// Nickname is display-only. Loomy has no nickname API, so the UI falls back
	// to the user id (`loomy.ts:61-62`).
	Nickname string `json:"nickname,omitempty"`
	// ExpiresAt is a MILLISECOND TIMESTAMP AS A STRING, computed locally at login
	// as `now + sessionTTL*1000` (`loomy.ts:63-69`, `loomy-auth.ts:272`). The
	// server never returns an expiry, and a JSON number here would fail the
	// decode on the model and execution paths (see credential_json.go).
	ExpiresAt string `json:"expires_at,omitempty"`
	// Type is the CPA auth file discriminator.
	Type string `json:"type,omitempty"`
}

// phonePattern is the plugin's ONLY phone gate, applied to a trimmed string
// (`jet-hub-rpc.ts:1124-1127`). `ccode` is hard-coded `86`
// (`loomy-oauth.ts:137`).
var phonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)

// nonDigits strips everything a user might type or paste around a number.
var nonDigits = regexp.MustCompile(`[^0-9]+`)

// normalizePhone reduces user input to the 11-digit form the account host wants:
// digits only, with a leading country code removed when it is present.
func normalizePhone(raw string) string {
	digits := nonDigits.ReplaceAllString(strings.TrimSpace(raw), "")
	switch {
	case len(digits) == 13 && strings.HasPrefix(digits, "86"):
		digits = digits[2:]
	case len(digits) == 14 && strings.HasPrefix(digits, "086"):
		digits = digits[3:]
	}
	return digits
}

// validPhone reports whether the number passes the source's gate.
func validPhone(raw string) bool {
	return phonePattern.MatchString(normalizePhone(raw))
}

// Session is the bearer token business endpoints expect.
func (c *Credential) Session() string { return strings.TrimSpace(c.AccessToken) }

// ExpiresAtMS parses `expires_at`.
//
// A non-string, empty, non-numeric or non-positive value counts as ABSENT
// (`loomy.ts:177-182`): "no expiry" and "expired at the epoch" are different
// things, and only the first may keep a credential in use.
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

// Expiry renders the local expiry, or the zero time when it is unknown.
func (c *Credential) Expiry() time.Time {
	value := c.ExpiresAtMS()
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}

// Expired reports whether the credential is locally past its expiry.
//
// ⚠️ A missing `expires_at` means "NOT expired" (`loomy.ts:184-193`, trap #17):
// the server's `100002` is the only authority, so a possibly-stale credential
// must still be tried instead of blocking the user. This matters twice as much
// for this provider because there is no refresh endpoint at all.
func (c *Credential) Expired(now time.Time) bool {
	expiry := c.ExpiresAtMS()
	if expiry <= 0 {
		return false
	}
	return !now.Before(time.UnixMilli(expiry))
}

// Encode serialises the credential for storage in the CPA auth file.
func (c *Credential) Encode() (json.RawMessage, error) {
	if c.Type == "" {
		c.Type = ProviderKey
	}
	raw, errMarshal := json.Marshal(c)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_credential", "encode Loomy credential: %v", errMarshal)
	}
	return json.RawMessage(raw), nil
}

// ParseCredential decodes and validates an auth-file payload.
//
// The source accepts any JSON object whose `access_token` is a string
// (`loomy-auth.ts:99-108`); everything else is not one of ours.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, credentialError("invalid_credential", "空的 Loomy 凭据")
	}
	var credential Credential
	if errUnmarshal := json.Unmarshal(raw, &credential); errUnmarshal != nil {
		return nil, credentialError("invalid_credential", "解析 Loomy 凭据失败：%v", errUnmarshal)
	}
	if credential.Session() == "" {
		return nil, credentialError("invalid_credential", "Loomy 凭据缺少 access_token")
	}
	return &credential, nil
}

// buildCredential assembles the credential a successful login produces
// (`loomy-auth.ts:266-274`).
//
// `expires_at` is computed LOCALLY from the configured session TTL: the account
// host answers with `session`/`userid` only and never an expiry.
func buildCredential(session, userID, phone, nickname string, cfg Config, now time.Time) *Credential {
	expiresAt := now.Add(time.Duration(cfg.sessionTTL()) * time.Second).UnixMilli()
	return &Credential{
		AccessToken: strings.TrimSpace(session),
		UserID:      strings.TrimSpace(userID),
		Phone:       normalizePhone(phone),
		Nickname:    strings.TrimSpace(nickname),
		ExpiresAt:   strconv.FormatInt(expiresAt, 10),
		Type:        ProviderKey,
	}
}

// displayLabel is the account label shown by the manager. Loomy has no nickname
// API, so the phone (when bound) or the user id is the best identity available.
func (c *Credential) displayLabel() string {
	if c == nil {
		return ProviderKey
	}
	if phone := normalizePhone(c.Phone); phone != "" {
		return phone
	}
	if userID := strings.TrimSpace(c.UserID); userID != "" {
		return userID
	}
	if session := c.Session(); session != "" {
		if len(session) > 8 {
			return session[:8]
		}
		return session
	}
	return ProviderKey
}

// maskedPhone renders a phone number for management pages without publishing the
// full number in a screenshot-able page title.
func (c *Credential) maskedPhone() string {
	phone := normalizePhone(c.Phone)
	if len(phone) != 11 {
		if phone == "" {
			return "未绑定"
		}
		return phone
	}
	return phone[:3] + "****" + phone[7:]
}
