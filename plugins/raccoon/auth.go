package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authfile"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Auth surface: identifier, parse, refresh.
//
// ⚠️ Unlike the sibling provider Loomy, Raccoon DOES have a refresh endpoint
// (`raccoon-oauth.ts:289-319`), so `auth.refresh` renews rather than probes.
// Two rules from the reference are load-bearing here (trap #14): refresh must
// target the account whose credential was handed in (never a "default ref"), and
// the auth file name must follow the host's own name so a refresh can never move
// a credential into a second file.

// unsafeFileNameChars keeps an auth file name inside [A-Za-z0-9._@-].
var unsafeFileNameChars = regexp.MustCompile(`[^A-Za-z0-9._@-]+`)

// handleAuthIdentifier advertises the provider key this plugin owns.
func handleAuthIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleAuthParse recognises an auth file already present in the auth directory.
func handleAuthParse(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthParseRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	if request.Provider != "" && request.Provider != ProviderKey {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	credential, errParse := ParseCredential(request.RawJSON)
	if errParse != nil {
		// Not one of ours: let the host try other providers
		// (`parseRaccoonCredential` returns undefined rather than throwing).
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	auth, errAuth := authDataFor(credential, authNameForHost(request.FileName, request.Path, "", credential))
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

// handleAuthRefresh renews the credential through `POST /auth/v1/refresh`.
//
// The pre-refresh window is deliberate (constant comment): the credential is
// refreshed 300 s before it expires when the host asks, and the returned
// `NextRefreshAfter` is what makes the host ask at that moment.
func handleAuthRefresh(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthRefreshRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errParse := ParseCredential(request.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}
	cfg := settings()
	now := time.Now()

	// No refresh token: there is nothing to renew and nothing to schedule. The
	// credential is returned unchanged so the host keeps using it until the
	// server rejects it — inventing an expiry here would log the user out on a
	// local guess.
	if !credential.Refreshable() {
		auth, errAuth := authDataFor(credential, authNameForHost(request.AuthID, request.Attributes["path"], request.Attributes["source"], credential))
		if errAuth != nil {
			return nil, errAuth
		}
		return pluginapi.AuthRefreshResponse{Auth: auth}, nil
	}

	refreshed, errRefresh := refreshCredential(h, cfg, credential)
	if errRefresh != nil {
		return nil, errRefresh
	}
	auth, errAuth := authDataFor(refreshed, authNameForHost(request.AuthID, request.Attributes["path"], request.Attributes["source"], refreshed))
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: nextRefreshAfter(refreshed, cfg, now),
	}, nil
}

// nextRefreshAfter decides when the host should refresh again.
//
// A credential with no resolvable expiry gets no schedule at all (the zero time):
// there is nothing to count down to and the server stays the only authority. A
// credential already inside the window is refreshed immediately, which is what
// "now" means here.
func nextRefreshAfter(credential *Credential, cfg Config, now time.Time) time.Time {
	expiry := credential.Expiry()
	if expiry.IsZero() {
		return time.Time{}
	}
	candidate := expiry.Add(-time.Duration(cfg.refreshWindow()) * time.Second)
	if candidate.After(now) {
		return candidate
	}
	return now
}

// authNameForHost resolves the auth file name the host already uses for this
// credential: the name it supplies on `auth.parse` (the file it read the
// credential from), the record id it passes to `auth.refresh` (which for a
// file-backed credential IS that file name), or the `path`/`source` attribute
// naming that file.
//
// A refresh that derives its own name would write a SECOND file for the same
// account whenever the derived identity changed — and the identity here is the
// `access_token`, which rotates on every refresh (trap #16). The host's name is
// therefore the only safe answer, and `derive` runs only for a brand-new login.
func authNameForHost(incoming, path, source string, credential *Credential) string {
	return authfile.Name(func() string { return defaultAuthFileName(credential) }, incoming, path, source)
}

// authDataFor converts a credential into the host-facing AuthData record.
func authDataFor(credential *Credential, fileName string) (pluginapi.AuthData, error) {
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		return pluginapi.AuthData{}, errEncode
	}
	if fileName == "" {
		fileName = defaultAuthFileName(credential)
	}
	label := credential.displayLabel()
	prefix := strings.TrimSpace(credential.UserID)
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	metadata := map[string]any{
		"userid":      credential.UserID,
		"phone":       credential.Phone,
		"office":      credential.OfficeIdentity,
		"expires_at":  credential.ExpiresAtMS(),
		"refreshable": credential.Refreshable(),
	}
	if expiry := credential.Expiry(); !expiry.IsZero() {
		metadata["expires_at_rfc3339"] = expiry.UTC().Format(time.RFC3339)
	}
	return pluginapi.AuthData{
		Provider:    ProviderKey,
		ID:          fileName,
		FileName:    fileName,
		Label:       label,
		Prefix:      prefix,
		StorageJSON: storage,
		Metadata:    metadata,
		// NOTE: do not use the reserved host attribute name "api_key" here. The
		// host reads it in Auth.AuthKind() and would classify the credential as
		// an API key, silently disabling alias and excluded-model rules.
		Attributes: map[string]string{
			"account":     label,
			"credential":  ProviderKey,
			"userid":      credential.UserID,
			"refreshable": boolString(credential.Refreshable()),
		},
		NextRefreshAfter: nextRefreshAfter(credential, settings(), time.Now()),
	}, nil
}

// defaultAuthFileName derives a stable auth-file name for a brand-new account.
//
// It is deterministic on purpose: the login page saves the credential under this
// name and `auth.login.poll` returns the same FileName, so a host that saves the
// poll result again overwrites that file instead of creating a duplicate.
//
// The identity is the account's `user_id` first (stable across refreshes), then
// the display label, then a constant. A constant is the weak case: two accounts
// that both fail `GET /user_info` would collide. That is preferred over keying on
// `access_token`, which rotates (trap #16) — and once the host knows a name, the
// `authfile.Name` candidates take over and the derive path is never used again.
func defaultAuthFileName(credential *Credential) string {
	identity := strings.TrimSpace(credential.UserID)
	if identity == "" {
		identity = credential.displayLabel()
	}
	// The display label's own last resort is the provider key; using it here
	// would produce `raccoon-raccoon.json` instead of the honest `account`.
	if identity == ProviderKey {
		identity = ""
	}
	identity = unsafeFileNameChars.ReplaceAllString(identity, "-")
	identity = strings.Trim(identity, "-")
	if identity == "" {
		identity = "account"
	}
	if len(identity) > 48 {
		identity = identity[:48]
	}
	return ProviderKey + "-" + identity + ".json"
}

// credentialAdvice is appended to every dead-session answer: the only remedy is a
// fresh WeChat scan, because the SMS path needs a human captcha this plugin
// cannot mint.
func credentialAdvice() string {
	return "（Raccoon 只能重新微信扫码登录：手机验证码需要人机验证，本插件无法完成）"
}
