package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authfile"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The auth method surface: identifier, parse, and refresh. The interactive
// device-code login lives in `login.go`, driven by the session table in
// `plugin.go`.
//
// ⚠️ VERIFICATION STATUS: unlike the inference path — which was measured end to
// end against the live server (all four models answered HTTP 200 with text,
// thinking and structured tool calls) — the LOGIN and REFRESH round trips below
// have NOT been exercised against the live server by us. The reference's own
// login was only ever unit tested: every live probe read an existing desktop
// client token instead of logging in, and there is no MiniMax client on this
// machine. The implementation follows the source faithfully; do not read it as
// a measured result.

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

// handleAuthRefresh renews a credential through the refresh-token grant.
//
// ⚠️ The refreshed bytes are written into the response, NOT persisted here:
// persistence is the host's job, and the freshness layer performs the same
// renewal through this very handler so the two can never drift.
func handleAuthRefresh(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthRefreshRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errParse := ParseCredential(request.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}
	if !credential.Refreshable() {
		return nil, credentialError("not_refreshable", "该 MiniMax 账号缺少 refresh_token，请重新登录")
	}
	refreshed, errRefresh := refreshCredential(h, settings(), credential)
	if errRefresh != nil {
		return nil, errRefresh
	}
	auth, errAuth := authDataFor(refreshed, authNameForHost(request.AuthID, request.Attributes["path"], request.Attributes["source"], refreshed))
	if errAuth != nil {
		return nil, errAuth
	}
	next := time.Time{}
	if expiry := refreshed.Expiry(); !expiry.IsZero() {
		next = expiry.Add(-refreshWindow(settings()))
	}
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: next}, nil
}

// authNameForHost resolves the auth file name the host already uses for this
// credential: the name it supplies on `auth.parse` (the file it read the
// credential from) or on `auth.refresh` (the auth record id, which for a
// file-backed credential is that same file name), and failing both, the
// `path`/`source` attribute naming that file.
//
// Deriving a name is the brand-new-login case only: the derived identity walks
// down to the first eight characters of the bearer token when the credential
// carries neither a nickname nor an account id, and that token ROTATES on every
// refresh — a refresh that derived a name would leave the file the host asked
// us to renew behind and write a second one.
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
	prefix := ""
	if settings().ModelPrefix {
		prefix = tokenPrefix(credential.Session())
	}
	metadata := map[string]any{}
	if expiry := credential.Expiry(); !expiry.IsZero() {
		metadata["expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	nextRefresh := time.Time{}
	if expiry := credential.Expiry(); !expiry.IsZero() {
		nextRefresh = expiry.Add(-refreshWindow(settings()))
	}
	return pluginapi.AuthData{
		Provider:    ProviderKey,
		ID:          fileName,
		FileName:    fileName,
		Label:       label,
		Prefix:      prefix,
		StorageJSON: storage,
		Metadata:    metadata,
		Attributes: map[string]string{
			"account":     label,
			"credential":  ProviderKey,
			"refreshable": boolString(credential.Refreshable()),
		},
		NextRefreshAfter: nextRefresh,
	}, nil
}

// defaultAuthFileName derives a stable auth-file name for an account.
//
// ⚠️ The token prefix is the LAST resort and it rotates, so a name built from it
// is only ever used for a brand-new login (the host supplies the real name on
// every later call). The reference names its single credential ref
// `MINIMAX_ACCESS_TOKEN`; the file name here is what CPA's auth directory shows,
// so it uses the same identity ladder the label does.
func defaultAuthFileName(credential *Credential) string {
	identity := strings.TrimSpace(credential.Nickname)
	if identity == "" {
		identity = strings.TrimSpace(credential.AccountID)
	}
	if identity == "" {
		identity = tokenPrefix(credential.Session())
	}
	identity = sanitizeFileName(identity)
	if identity == "" {
		identity = "account"
	}
	if len(identity) > 48 {
		identity = identity[:48]
	}
	return ProviderKey + "-" + identity + ".json"
}
