package main

import (
	"encoding/json"
	"strings"
)

// Credential is the ZCode account record stored in a CPA auth file.
//
// The field names are the reference's own (`ZcodeCredential`,
// `zcode.ts:180-260`) so a credential written by either implementation reads
// back here.
//
// Two members deserve their own paragraph:
//
//   - `user_id` is the ONLY stable account identity. It is issued by the server
//     and does not change when the same account logs in again, which is what
//     makes it the de-duplication key.
//   - `device_mid` is NOT an identity. Under the plugin's own login it is
//     generated locally (a fresh random UUID per login) because the server
//     accepts any value as long as one is present; using it to decide "is this
//     the same account" therefore splits one account into many.
type Credential struct {
	// ZCodeJWT is the inference bearer. Its payload carries no `exp` claim
	// (measured: `{user_id, token_version, sub, iat}`), so it is treated as
	// long-lived and refreshable only by logging in again.
	ZCodeJWT string `json:"zcode_jwt"`
	// DeviceMid is the `X-Device-Mid` value. REQUIRED: without it every
	// `billing/*` call answers `400 {"code":3001,"msg":"parameter error"}`.
	DeviceMid string `json:"device_mid"`
	// UserID is the server-issued account identity (`user.user_id`). Optional
	// because credentials written before it was recorded do not carry it;
	// de-duplication must tolerate its absence rather than fail.
	UserID string `json:"user_id,omitempty"`
	// BigModelAccessToken is the secondary identity returned by the login poll
	// (`bigmodel.access_token`).
	BigModelAccessToken string `json:"bigmodel_access_token,omitempty"`
	// BigModelRefreshToken is present only when the server chooses to issue one.
	BigModelRefreshToken string `json:"bigmodel_refresh_token,omitempty"`
	// CodingPlanKeyZai / CodingPlanKeyBigmodel are the ultra-channel api keys.
	// They are not needed by the free-quota channel; they are carried so an
	// imported official credential is not silently truncated.
	CodingPlanKeyZai      string `json:"coding_plan_key_zai,omitempty"`
	CodingPlanKeyBigmodel string `json:"coding_plan_key_bigmodel,omitempty"`
	// AccountLabel is the display label (masked phone number, user name or a
	// device-code fragment).
	AccountLabel string `json:"account_label,omitempty"`
	// AppVersion rides `X-ZCode-App-Version`.
	AppVersion string `json:"app_version,omitempty"`
	// Source records where the credential came from: `plugin` (this plugin's own
	// login) or `ide` (decrypted from the official client's credential store).
	// Display and diagnostics only — it takes no part in authentication.
	Source string `json:"source,omitempty"`
}

// ParseCredential decodes an auth file.
//
// A record is accepted when it carries the two fields the upstream actually
// requires — the JWT and a device id — which is the reference's own usability
// test (`isUsableZcodeCredential`, `zcode.ts:429-437`). Everything else is
// optional: a missing coding-plan key only removes the ultra channel, and a
// missing `user_id` only disables de-duplication.
func ParseCredential(raw []byte) (*Credential, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, credentialError("missing_credential", "ZCode 凭据为空：请重新登录该账号")
	}
	var credential Credential
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &credential); errUnmarshal != nil {
		return nil, credentialError("invalid_credential", "解析 ZCode 凭据失败：%v", errUnmarshal)
	}
	credential.normalize()
	if !credential.Usable() {
		return nil, credentialError("incomplete_credential",
			"ZCode 凭据缺少 %s：请重新登录该账号", credential.missingField())
	}
	return &credential, nil
}

// normalize trims every member so a value pasted with stray whitespace does not
// become a different account (the auth file is edited by hand in practice).
func (c *Credential) normalize() {
	if c == nil {
		return
	}
	c.ZCodeJWT = strings.TrimSpace(c.ZCodeJWT)
	c.DeviceMid = strings.TrimSpace(c.DeviceMid)
	c.UserID = strings.TrimSpace(c.UserID)
	c.BigModelAccessToken = strings.TrimSpace(c.BigModelAccessToken)
	c.BigModelRefreshToken = strings.TrimSpace(c.BigModelRefreshToken)
	c.CodingPlanKeyZai = strings.TrimSpace(c.CodingPlanKeyZai)
	c.CodingPlanKeyBigmodel = strings.TrimSpace(c.CodingPlanKeyBigmodel)
	c.AccountLabel = strings.TrimSpace(c.AccountLabel)
	c.AppVersion = strings.TrimSpace(c.AppVersion)
	c.Source = strings.TrimSpace(c.Source)
}

// Usable reports whether the credential carries both hard requirements.
func (c *Credential) Usable() bool {
	return c != nil && c.ZCodeJWT != "" && c.DeviceMid != ""
}

// missingField names the first absent hard requirement, for the error message.
func (c *Credential) missingField() string {
	switch {
	case c == nil || c.ZCodeJWT == "":
		return "zcode_jwt"
	case c.DeviceMid == "":
		return "device_mid"
	default:
		return "未知字段"
	}
}

// Encode serialises the credential back into the auth file.
func (c *Credential) Encode() (json.RawMessage, error) {
	encoded, errMarshal := json.Marshal(c)
	if errMarshal != nil {
		return nil, credentialError("encode_credential", "序列化 ZCode 凭据失败：%v", errMarshal)
	}
	return encoded, nil
}

// Bearer renders the Authorization header value.
func (c *Credential) Bearer() string { return "Bearer " + c.ZCodeJWT }

// appVersionOrDefault resolves the version this credential rides on, falling
// back to the configured default so a credential imported without one still
// produces a well-formed header set.
func (c *Credential) appVersionOrDefault(cfg Config) string {
	if c != nil && c.AppVersion != "" {
		return c.AppVersion
	}
	return cfg.AppVersion
}

// displayLabel is the human-facing account name. It never falls back to
// `device_mid` — that value changes on every login and would make one account
// look like several — so the fallbacks walk to the user id and finally to a
// neutral string.
func (c *Credential) displayLabel() string {
	if c == nil {
		return "ZCode 账号"
	}
	for _, candidate := range []string{c.AccountLabel, c.UserID, maskedJWT(c.ZCodeJWT)} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return "ZCode 账号"
}

// maskedJWT renders a short, non-reversible fragment of the token for a label.
func maskedJWT(token string) string {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) <= 8 {
		return trimmed
	}
	return "…" + trimmed[len(trimmed)-8:]
}

// identityKey is the stable de-duplication key for an account.
//
// It is `user_id` when the credential carries one, and empty otherwise — the
// caller must then SKIP de-duplication rather than fall back to `device_mid`,
// which would place two logins of one account in two separate buckets.
func (c *Credential) identityKey() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.UserID)
}
