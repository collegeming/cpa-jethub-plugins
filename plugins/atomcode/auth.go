package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authfile"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Credential lifecycle: host-facing AuthData conversion, auth.parse,
// auth.login.start / poll and auth.refresh.
//
// The reference's own surface is `atomcode login` (`crates/atomcode-auth/src/oauth.rs:596`)
// plus the `auth.toml` it writes; the split here follows the CPA ABI, which
// drives the browser gesture from the management page instead of a terminal.

// unsafeFileNameChars matches the characters that must not appear in an auth
// file name.
var unsafeFileNameChars = regexp.MustCompile(`[^A-Za-z0-9._@-]+`)

// authNameForHost resolves the auth file name the host already uses for this
// credential: the name it supplies on `auth.parse` (the file it read the
// credential from) or on `auth.refresh` (the auth record id), and failing both,
// the `path`/`source` attribute naming that file.
//
// Deriving a name is the brand-new-login case only. A credential that arrived
// under a host-assigned name must keep it: an AtomGit account that was imported
// by hand would otherwise be renamed on the next refresh, which is how one
// account ends up with several auth files.
func authNameForHost(incoming, path, source string, credential *Credential) string {
	return authfile.Name(func() string { return defaultAuthFileName(credential) }, incoming, path, source)
}

// defaultAuthFileName derives a stable auth-file name for an account.
//
// The AtomGit display name is frequently Chinese, and sanitising it yields
// nothing usable, so the ladder is display name -> login handle -> account id.
// Every AtomGit account has the last two and both are ASCII, which is what keeps
// the name stable across logins of the same account.
func defaultAuthFileName(credential *Credential) string {
	for _, candidate := range []string{credential.User.Name, credential.User.Username, credential.User.ID} {
		identity := sanitizeFileIdentity(candidate)
		if identity == "" {
			continue
		}
		if len(identity) > 64 {
			identity = identity[:64]
		}
		return ProviderKey + "-" + identity + ".json"
	}
	return ProviderKey + "-account.json"
}

// sanitizeFileIdentity keeps the characters that are safe in an auth file name.
func sanitizeFileIdentity(value string) string {
	identity := unsafeFileNameChars.ReplaceAllString(strings.TrimSpace(value), "-")
	return strings.Trim(identity, "-")
}

// boolString renders a boolean for attribute maps.
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
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
	label := credential.Label()
	prefix := ""
	if settings().ModelPrefix {
		prefix = modelPrefixFor(credential)
		if strings.HasSuffix(prefix, "/") {
			prefix = strings.TrimSuffix(prefix, "/")
		}
	}
	metadata := map[string]any{
		"account_id":  credential.AccountID(),
		"username":    credential.User.Username,
		"refreshable": credential.Refreshable(),
	}
	if expiresAt, ok := credential.ExpiresAtMS(); ok {
		metadata["expires_at"] = expiresAt
	}
	nextRefresh := time.Time{}
	if expiry := credential.Expiry(); !expiry.IsZero() {
		nextRefresh = expiry.Add(-refreshLeadFor(settings()))
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
			"username":    credential.User.Username,
		},
		NextRefreshAfter: nextRefresh,
	}, nil
}

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

// handleAuthLoginStart opens a broker sign-in and returns the browser URL. It
// never blocks: the browser gesture must happen while the user agent still
// considers it user-initiated.
func handleAuthLoginStart(h *abiboot.Host, _ json.RawMessage) (any, error) {
	session, errStart := startLoginSession(h, settings())
	if errStart != nil {
		return nil, errStart
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       session.LoginURL,
		State:     session.State,
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"broker": settings().BrokerBase,
			"hint":   "在浏览器打开 URL，用 AtomGit 账号登录并授权后，调用 auth.login.poll",
		},
	}, nil
}

// handleAuthLoginPoll advances an in-flight sign-in.
//
// One poll spans two broker calls: `/auth/check` asks whether the browser
// gesture finished, and once it has, `/auth/token` exchanges the state for the
// credential. Doing the exchange here — in the caller's invocation rather than a
// background goroutine — keeps the whole flow on one host callback identity, the
// same split plugins/codearts/loginserver.go documents.
func handleAuthLoginPoll(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthLoginPollRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	session, found := lookupLoginSession(request.State)
	if !found {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已超时，请重新发起登录",
		}, nil
	}

	if status, message, credential, finished := session.snapshot(); finished {
		defer forgetLoginSession(request.State)
		if status == pluginapi.AuthLoginStatusSuccess && credential != nil {
			auth, errAuth := authDataFor(credential, "")
			if errAuth != nil {
				return nil, errAuth
			}
			return pluginapi.AuthLoginPollResponse{Status: status, Message: message, Auth: auth}, nil
		}
		return pluginapi.AuthLoginPollResponse{Status: status, Message: message}, nil
	}

	authorized, errCheck := brokerCheck(h, settings(), request.State)
	if errCheck != nil {
		// A failed poll is not a failed login: the user may simply not have
		// finished. Report pending and let the page poll again, which is what
		// the reference's poller does (oauth.rs:494-515).
		if h != nil {
			h.Log("debug", "AtomCode 登录状态查询失败，继续等待", map[string]any{"error": errCheck.Error()})
		}
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待浏览器完成登录",
		}, nil
	}
	if !authorized {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待浏览器完成登录",
		}, nil
	}

	credential, errExchange := brokerExchange(h, settings(), request.State)
	if errExchange != nil {
		message := errExchange.Error()
		session.fail(message)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
	}
	label := credential.Label()
	session.finish(credential, "登录成功："+label)
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		return nil, errAuth
	}
	forgetLoginSession(request.State)
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功：" + label,
		Auth:    auth,
	}, nil
}

// handleAuthRefresh renews a credential through the broker
// (`crates/atomcode-auth/src/oauth.rs:901-975`).
//
// The refresh ROTATES the credential: the broker returns a new access token AND
// a new refresh token, and the access token it replaces stops working at once.
// The rotated value is therefore written back through the returned AuthData —
// dropping it would leave the account unusable until the next manual login.
//
// Failure handling follows the reference's three-way split: transport failures
// stay retryable, a 401/403 or a 2xx without a token is terminal, and everything
// else is surfaced with its status.
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
		return nil, abiboot.HTTPError("not_refreshable", http.StatusUnauthorized,
			"该 AtomCode 账号没有 refresh_token，请重新登录")
	}

	response, errRefresh := brokerRefresh(h, settings(), credential.RefreshToken)
	if errRefresh != nil {
		return nil, errRefresh
	}
	refreshed := applyRefresh(credential, *response, time.Now())
	auth, errAuth := authDataFor(refreshed,
		authNameForHost(request.AuthID, request.Attributes["path"], request.Attributes["source"], refreshed))
	if errAuth != nil {
		return nil, errAuth
	}
	nextRefresh := time.Time{}
	if expiry := refreshed.Expiry(); !expiry.IsZero() {
		nextRefresh = expiry.Add(-refreshLeadFor(settings()))
	}
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: nextRefresh}, nil
}
