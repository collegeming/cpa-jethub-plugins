package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authfile"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Auth-provider surface: identity, auth-file parsing, the two-step device-code
// login and credential refresh.

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

// handleAuthLoginStart begins a WorkOS device-code login and returns at once.
//
// It never blocks on the user: the response carries the verification URL and the
// user code, and `auth.login.poll` advances the flow. The TypeScript splits the
// same flow into a URL-returning call and a later result only to satisfy browser
// popup rules (`cline-oauth.ts:48-57`); in CPA the split is the ABI contract, and
// the requirement is stronger — a login that blocks would hold the management
// request open for up to five minutes.
func handleAuthLoginStart(h *abiboot.Host, raw json.RawMessage) (any, error) {
	cfg := settings()
	// The request is decoded only to reject a mismatched provider early; the
	// device flow itself takes no caller input.
	if request, errDecode := abiboot.Decode[pluginapi.AuthLoginStartRequest](raw); errDecode == nil {
		if request.Provider != "" && request.Provider != ProviderKey {
			return nil, statusError(false, "provider_mismatch", 400,
				"该登录入口属于 %s，无法为 %s 发起登录", ProviderKey, request.Provider)
		}
	}
	session, errStart := startLoginSession(transportFor(h), cfg)
	if errStart != nil {
		return nil, errStart
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       session.LoginURL(),
		State:     session.State,
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"user_code":     session.Device.UserCode,
			"interval_ms":   session.Device.IntervalMS,
			"expires_in_ms": session.Device.ExpiresInMS,
			"hint": "在浏览器中打开 URL（已带 user_code）完成授权后，由 auth.login.poll 轮询取回凭据；" +
				"WorkOS 设备码流程不需要本地回调端口",
		},
	}, nil
}

// handleAuthLoginPoll advances one in-flight login by at most one upstream call.
//
// The TypeScript loop sleeps between attempts (`cline-oauth.ts:233-307`); a host
// invocation must not block for that long, so the interval is enforced against
// the previous attempt and the call returns "pending" instead. Every other rule
// survives, including the immediate first poll.
func handleAuthLoginPoll(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthLoginPollRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	state := strings.TrimSpace(request.State)
	session, found := lookupLoginSession(state)
	if !found {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已超时，请重新发起登录",
		}, nil
	}
	return advanceLoginSession(transportFor(h), session, state, settings())
}

// handleAuthRefresh renews a Cline credential through `/api/v1/auth/refresh`.
func handleAuthRefresh(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthRefreshRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errParse := ParseCredential(request.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}
	refreshed, errRefresh := refreshCredential(transportFor(h), credential)
	if errRefresh != nil {
		return nil, errRefresh
	}
	auth, errAuth := authDataFor(refreshed, authNameForHost(request.AuthID, request.Attributes["path"], request.Attributes["source"], refreshed))
	if errAuth != nil {
		return nil, errAuth
	}
	next := time.Time{}
	if refreshed.ExpireTime > 0 {
		next = refreshed.ExpiresAt().Add(-refreshLead)
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
// down to the first eight characters of the access token when the credential
// carries neither a label nor an account id, and that token rotates on every
// refresh — a refresh that derived a name would leave the file the host asked
// us to renew behind and write a second one.
func authNameForHost(incoming, path, source string, credential *Credential) string {
	return authfile.Name(func() string { return defaultAuthFileName(credential) }, incoming, path, source)
}

// authDataFor converts a credential into the host-facing AuthData record.
func authDataFor(credential *Credential, fileName string) (pluginapi.AuthData, error) {
	// Normalise the stored token through the prefix helper before writing it
	// back. The helper only ever ADDS the prefix, so a credential that already
	// carries it is untouched — and anything the host persists from here on is
	// in the form the API accepts.
	credential.AccessToken = credential.bearerValue()
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		return pluginapi.AuthData{}, errEncode
	}
	if fileName == "" {
		fileName = defaultAuthFileName(credential)
	}
	label := credential.Label()
	if label == "" {
		label = ProviderKey
	}
	// The host uses Prefix as this auth's model prefix; every plugin in this
	// repository derives it from the account identifier.
	prefix := strings.TrimSpace(credential.AccountID)
	if prefix == "" {
		prefix = ProviderKey
	}
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	metadata := map[string]any{
		"account_id":  credential.AccountID,
		"email":       credential.Email,
		"refreshable": credential.Refreshable(),
		// Recorded because a credential whose token lost the prefix is the one
		// failure mode that looks like an expired token upstream.
		"token_prefix": TokenPrefix,
	}
	if credential.ExpireTime > 0 {
		metadata["expires_at"] = credential.ExpiresAt().UTC().Format(time.RFC3339)
	}
	nextRefresh := time.Time{}
	if credential.ExpireTime > 0 {
		nextRefresh = credential.ExpiresAt().Add(-refreshLead)
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

// defaultAuthFileName derives a stable auth-file name for one account.
func defaultAuthFileName(credential *Credential) string {
	identity := credential.Label()
	if identity == "" {
		token := strings.TrimPrefix(credential.bearerValue(), TokenPrefix)
		if len(token) > 8 {
			token = token[:8]
		}
		identity = token
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
