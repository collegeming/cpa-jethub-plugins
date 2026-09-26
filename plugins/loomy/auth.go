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

// refreshLead is how long before the local expiry a probe is scheduled. It only
// decides WHEN the host asks; this provider has no way to extend a session.
const refreshLead = 24 * time.Hour

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
		// Not one of ours: let the host try other providers.
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	auth, errAuth := authDataFor(credential, authNameForHost(request.FileName, request.Path, "", credential))
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

// handleAuthRefresh PROBES the credential; it does not renew anything.
//
// ⚠️ Loomy has NO refresh endpoint and the credential carries no refresh token
// at all (`loomy.ts:195-205`, `loomy-auth.ts:4-8`). `isLoomyRefreshable()`
// returns `false` unconditionally and is described as an honest marker rather
// than an oversight (`loomy.ts:203-205`). What the reference does instead is a
// validity probe against the cheapest read-only endpoint, so that is exactly what
// this handler does:
//
//   - probe says the session works -> return the SAME credential and schedule the
//     next probe;
//   - probe says `100002` -> the credential is dead: fail with 401 so the host
//     retires it and the user is asked to log in again;
//   - transport failure or any other business failure -> a retryable 502, never
//     an expiry (`loomy-auth.ts:350-369`, trap #10).
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
	if errProbe := probeCredential(h, credential, cfg); errProbe != nil {
		return nil, errProbe
	}
	auth, errAuth := authDataFor(credential, authNameForHost(request.AuthID, request.Attributes["path"], request.Attributes["source"], credential))
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: nextProbeAfter(credential, time.Now()),
	}, nil
}

// nextProbeAfter decides when the host should probe again.
//
// A credential with no local expiry gets no schedule at all: there is nothing to
// count down to, and the server stays the only authority. A locally expired
// credential is still probed on the reference's 30-minute health cadence
// (`index.ts:490`) instead of immediately, because the local clock is not
// evidence — the source deliberately keeps using a possibly-stale credential
// (`loomy.ts:184-193`).
func nextProbeAfter(credential *Credential, now time.Time) time.Time {
	expiry := credential.Expiry()
	if expiry.IsZero() {
		return time.Time{}
	}
	if candidate := expiry.Add(-refreshLead); candidate.After(now) {
		return candidate
	}
	return now.Add(time.Duration(CredentialHealthIntervalMS) * time.Millisecond)
}

// authNameForHost resolves the auth file name the host already uses for this
// credential: the name it supplies on `auth.parse` (the file it read the
// credential from) or on the probe the host calls `auth.refresh` (the auth
// record id, which for a file-backed credential is that same file name), and
// failing both, the `path`/`source` attribute naming that file.
//
// The probe returns the credential unchanged, so a derived name would be
// harmless today — but Loomy's name comes from the phone number, and a
// credential whose phone and user id are both missing would fall back to a
// constant ("account") shared by every such account. Keeping the host's name
// means the probe can never move a credential into another file.
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
		"userid":     credential.UserID,
		"phone":      credential.Phone,
		"expires_at": credential.ExpiresAtMS(),
		// There is no refresh token to rotate; the flag keeps the manager from
		// offering a "renew" affordance that cannot work.
		"refreshable": false,
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
		// host reads it in Auth.AuthKind() and would classify the credential as an
		// API key, which silently disables the manager's model alias and
		// excluded-model rules for this provider.
		Attributes: map[string]string{
			"account":     label,
			"credential":  ProviderKey,
			"userid":      credential.UserID,
			"refreshable": boolString(false),
		},
		NextRefreshAfter: nextProbeAfter(credential, time.Now()),
	}, nil
}

// defaultAuthFileName derives a stable auth-file name for an account.
//
// It is deterministic on purpose: the management page saves the credential when
// the code verifies and `auth.login.poll` returns the same FileName, so a host
// that saves the poll result again overwrites that file instead of creating a
// second credential for the same account.
func defaultAuthFileName(credential *Credential) string {
	identity := credential.displayLabel()
	identity = unsafeFileNameChars.ReplaceAllString(strings.TrimSpace(identity), "-")
	identity = strings.Trim(identity, "-")
	if identity == "" {
		identity = "account"
	}
	if len(identity) > 48 {
		identity = identity[:48]
	}
	return ProviderKey + "-" + identity + ".json"
}

// credentialAdvice is appended to every dead-session answer: there is no refresh
// token to rotate, so the only remedy is a fresh login (WeChat scan, or SMS).
func credentialAdvice() string {
	return "（Loomy 没有 refresh_token，只能用微信扫码或手机验证码重新登录）"
}
