package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Credential lifecycle: host-facing AuthData conversion, auth.parse,
// auth.login.start / poll and auth.refresh. Ported from trae-auth.ts:95-319 and
// trae.ts:458-520.

// refreshLead is how long before expiry a credential should be renewed
// (refresh.ts's lead time, as used by every provider in this repository).
const refreshLead = time.Hour

// unsafeFileNameChars matches the characters that must not appear in an auth
// file name.
var unsafeFileNameChars = regexp.MustCompile(`[^A-Za-z0-9._@-]+`)

// authDataFor converts a credential into the host-facing AuthData record.
func authDataFor(credential *Credential, fileName string) (pluginapi.AuthData, error) {
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		return pluginapi.AuthData{}, errEncode
	}
	if fileName == "" {
		fileName = defaultAuthFileName(credential)
	}
	label := credential.Nickname
	if label == "" {
		label = credential.UID
	}
	prefix := credential.UID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	region := credential.Region
	if region == "" {
		region = settings().Region
	}
	metadata := map[string]any{
		"uid":           credential.UID,
		"region":        region,
		"refreshable":   credential.Refreshable(),
		"machine_id":    credential.MachineID,
		"device_id":     credential.DeviceID,
		"enterprise_id": credential.EnterpriseID,
	}
	if expiresAt, ok := credential.ExpiresAtMS(); ok {
		metadata["expires_at"] = expiresAt
	}
	nextRefresh := time.Time{}
	if expiry := credential.Expiry(); !expiry.IsZero() {
		nextRefresh = expiry.Add(-refreshLead)
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
			"region":      region,
			"site":        productFor(region).Site,
		},
		NextRefreshAfter: nextRefresh,
	}, nil
}

// defaultAuthFileName derives a stable auth-file name for an account.
//
// The name is deliberately restricted to ASCII: the host validates auth file
// names, and a Chinese nickname sanitizes down to nothing. Rather than fall back
// to a constant (which would collide between accounts), the uid is used when the
// nickname yields no usable characters.
func defaultAuthFileName(credential *Credential) string {
	identity := sanitizeFileIdentity(credential.Nickname)
	if identity == "" {
		identity = sanitizeFileIdentity(credential.UID)
	}
	if identity == "" {
		identity = "account"
	}
	if len(identity) > 64 {
		identity = identity[:64]
	}
	return ProviderKey + "-" + identity + ".json"
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
	auth, errAuth := authDataFor(credential, request.FileName)
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

// handleAuthLoginStart opens the loopback callback listener and returns the
// browser URL. It never blocks: the browser gesture must happen while the user
// agent still considers it user-initiated (docs/PORTING.md:443).
func handleAuthLoginStart(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	session, errStart := startLoginSession(settings())
	if errStart != nil {
		return nil, errStart
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       session.LoginURL,
		State:     session.State,
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"region":       session.Region,
			"port":         session.Port,
			"redirect_uri": session.RedirectURI(),
			"hint":         "在浏览器打开 URL 完成授权后，调用 auth.login.poll",
		},
	}, nil
}

// handleAuthLoginPoll advances an in-flight sign-in. The token exchange runs
// here, in the caller's invocation, so it uses that invocation's host callback
// identity rather than the callback goroutine's (the same split
// plugins/codearts/loginserver.go documents).
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

	query, hasCallback := session.takeCallback()
	if hasCallback {
		cfg := settings()
		parsed := parseTraeCallbackQuery(query)
		if !parsed.OK {
			// A callback we cannot use must still settle the session: leaving it
			// pending shows "authenticating" forever (docs/agents/trae.md:351-369).
			session.fail("TRAE 登录回调无效：" + parsed.Reason)
			return pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusError,
				Message: "TRAE 登录回调无效：" + parsed.Reason,
			}, nil
		}
		credential, errExchange := exchangeTraeCallback(h, parsed.Info, session, cfg)
		if errExchange != nil {
			message := errExchange.Error()
			session.fail(message)
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
		}
		session.finish(credential, "登录成功："+credential.Nickname)
		auth, errAuth := authDataFor(credential, "")
		if errAuth != nil {
			return nil, errAuth
		}
		forgetLoginSession(request.State)
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "登录成功：" + credential.Nickname,
			Auth:    auth,
		}, nil
	}

	if session.expired() {
		session.expire("登录会话已超时，请重新发起")
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话已超时，请重新发起",
		}, nil
	}
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: "等待浏览器完成登录",
	}, nil
}

// handleAuthRefresh renews a credential through ExchangeToken
// (trae-auth.ts:283-378).
//
// The terminal decision has three independent triggers, and all three are
// required: HTTP 401/403, a `session-dead` classification, and a 2xx JSON
// response that simply carries no token (docs/agents/trae.md:412-420). Transport
// failures and 5xx stay retryable so one flaky hop does not force a re-login.
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
			"该 TRAE 账号没有 refresh_token，请重新登录")
	}
	cfg := settings()
	product := productFor(cfg.Region)
	host := credential.APIHost
	if strings.TrimSpace(host) == "" {
		host = product.OAuthHost
	}

	exchange, status, body, errExchange := requestExchangeToken(h, host, credential.RefreshToken, product, cfg)
	if errExchange != nil {
		// Transport failure: retryable, never terminal.
		return nil, abiboot.RetryableError("transport", "TRAE 续期网络失败：%v", errExchange)
	}
	if exchange == nil {
		kind := classifyTraeError(status, body)
		noTokenInSuccess := status >= 200 && status < 300 && json.Valid([]byte(body))
		if status == http.StatusUnauthorized || status == http.StatusForbidden || isTerminalError(kind) || noTokenInSuccess {
			return nil, abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized,
				"TRAE refresh_token 已失效，请重新登录")
		}
		return nil, abiboot.HTTPError(httpErrorCode(status), status,
			"TRAE 续期失败（HTTP %d）：%s", status, truncate(body, 200))
	}

	refreshed := applyTraeRefresh(credential, *exchange, time.Now().UnixMilli())
	if region := strings.TrimSpace(refreshed.Region); region == "" {
		refreshed.Region = cfg.Region
	}
	auth, errAuth := authDataFor(refreshed, request.AuthID)
	if errAuth != nil {
		return nil, errAuth
	}
	nextRefresh := time.Time{}
	if expiry := refreshed.Expiry(); !expiry.IsZero() {
		nextRefresh = expiry.Add(-refreshLead)
	}
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: nextRefresh}, nil
}
