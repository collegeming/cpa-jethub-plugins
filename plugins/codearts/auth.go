package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// refreshLead is how long before expiry a credential should be renewed.
const refreshLead = time.Hour

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
	label := credential.UserName
	if label == "" {
		label = credential.AccessKeyID
	}
	prefix := credential.AccessKeyID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return pluginapi.AuthData{
		Provider:    ProviderKey,
		ID:          fileName,
		FileName:    fileName,
		Label:       label,
		Prefix:      prefix,
		StorageJSON: storage,
		Metadata: map[string]any{
			"expires_at": credential.ExpiresAt,
			"flow":       flowOf(credential),
		},
		// NOTE: do not use the name "api_key" here. It is a reserved host
		// attribute: the host reads it in Auth.AuthKind() and classifies the
		// credential as an API key, which makes OAuthModelAliasChannel() return
		// "" so the manager's model aliases and excluded-model rules are
		// SILENTLY ignored for this provider.
		Attributes: map[string]string{
			"access_key_id": credential.AccessKeyID,
			"account":       label,
			"credential":    ProviderKey,
			"refreshable":   boolString(credential.Refreshable()),
		},
		NextRefreshAfter: credential.Expiry().Add(-refreshLead),
	}, nil
}

// defaultAuthFileName derives a stable auth-file name for an account.
func defaultAuthFileName(credential *Credential) string {
	identity := credential.UserName
	if identity == "" {
		identity = credential.DomainID
	}
	if identity == "" {
		identity = credential.AccessKeyID
	}
	identity = unsafeFileNameChars.ReplaceAllString(strings.TrimSpace(identity), "-")
	identity = strings.Trim(identity, "-")
	if identity == "" {
		identity = "account"
	}
	if len(identity) > 64 {
		identity = identity[:64]
	}
	return ProviderKey + "-" + identity + ".json"
}

func flowOf(credential *Credential) string {
	if credential.Refreshable() {
		return LoginFlowOAuth
	}
	return LoginFlowTicket
}

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
	credential, errParse := ParseCredential(request.RawJSON)
	if errParse != nil {
		// Not one of ours: let the host try other providers.
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if request.Provider != "" && request.Provider != ProviderKey {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	auth, errAuth := authDataFor(credential, request.FileName)
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

// handleAuthLoginStart opens the loopback callback listener and returns the
// browser URL for the interactive sign-in.
func handleAuthLoginStart(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	flow := settings().Flow
	if flow != LoginFlowTicket {
		flow = LoginFlowOAuth
	}
	session, errStart := startLoginSession(flow)
	if errStart != nil {
		return nil, errStart
	}
	metadata := map[string]any{
		"flow":         session.Flow,
		"port":         session.Port,
		"redirect_uri": session.RedirectURI(),
	}
	if session.Flow == LoginFlowTicket {
		metadata["ticket_id"] = session.TicketID
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       session.LoginURL(),
		State:     session.State,
		ExpiresAt: session.ExpiresAt,
		Metadata:  metadata,
	}, nil
}

// handleAuthLoginPoll advances an in-flight sign-in. Network work happens here
// rather than in the callback goroutine so it can use this invocation's host
// callback identity.
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

	switch session.Flow {
	case LoginFlowOAuth:
		if code := session.pendingCode(); code != "" {
			token, errExchange := exchangeAuthorizationCode(h, code, session.PKCE.Verifier, session.RedirectURI(), session.KeyPair)
			if errExchange != nil {
				message := errExchange.Error()
				session.fail(message)
				return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
			}
			credential := credentialFromToken(token, session.PKCE.Verifier, session.KeyPair)
			session.finish(credential, "登录成功")
			auth, errAuth := authDataFor(credential, "")
			if errAuth != nil {
				return nil, errAuth
			}
			forgetLoginSession(request.State)
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "登录成功", Auth: auth}, nil
		}
	case LoginFlowTicket:
		if secret := session.pendingSecret(); secret != "" {
			credential, errPoll := pollLegacyCredential(h, session.TicketID, secret)
			if errPoll != nil {
				message := errPoll.Error()
				session.fail(message)
				return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
			}
			if credential != nil {
				session.finish(credential, "登录成功")
				auth, errAuth := authDataFor(credential, "")
				if errAuth != nil {
					return nil, errAuth
				}
				forgetLoginSession(request.State)
				return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "登录成功", Auth: auth}, nil
			}
		}
	}

	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: "等待浏览器完成登录",
	}, nil
}

// handleAuthRefresh renews an OAuth-managed credential.
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
		return nil, abiboot.Errorf("not_refreshable", "该 CodeArts 账号缺少 refresh_token，请重新登录")
	}
	keyPair, errKey := keyPairFromStoredJWK(*credential.DpopPrivateKeyJWK)
	if errKey != nil {
		return nil, errKey
	}
	token, errExchange := exchangeRefreshToken(h, credential.RefreshToken, credential.CodeVerifier, keyPair)
	if errExchange != nil {
		if errors.Is(errExchange, ErrRefreshTokenExpired) {
			return nil, abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized,
				"CodeArts refresh_token 已失效，请重新登录")
		}
		return nil, errExchange
	}
	refreshed := credentialFromToken(token, credential.CodeVerifier, keyPair)
	// Fields the STS response does not carry are preserved from the old record.
	refreshed.DomainID = credential.DomainID
	refreshed.UserID = credential.UserID
	refreshed.UserName = credential.UserName
	refreshed.ModelRateLimits = credential.ModelRateLimits

	auth, errAuth := authDataFor(refreshed, request.AuthID)
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: refreshed.Expiry().Add(-refreshLead)}, nil
}

// legacyTicketEnvelope tolerates both documented ticket response shapes, at the
// top level or wrapped in a `data` object.
type legacyTicketEnvelope struct {
	Credential *legacyTicketCredential `json:"credential"`
	Result     *legacyTicketResult     `json:"result"`
	Data       *legacyTicketEnvelope   `json:"data"`
}

type legacyTicketCredential struct {
	Access        string `json:"access"`
	Secret        string `json:"secret"`
	SecurityToken string `json:"securitytoken"`
	ExpiresAt     string `json:"expires_at"`
}

type legacyTicketResult struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SecurityToken   string `json:"securityToken"`
	Expiration      string `json:"expiration"`
}

// pollLegacyCredential performs a single ticket poll. A nil credential with a
// nil error means "still waiting".
func pollLegacyCredential(h *abiboot.Host, ticketID, secret string) (*Credential, error) {
	endpoint, errURL := url.Parse(LegacyCredentialEndpoint)
	if errURL != nil {
		return nil, abiboot.Errorf("ticket_url", "parse ticket endpoint: %v", errURL)
	}
	query := endpoint.Query()
	query.Set("ticket_id", ticketID)
	query.Set("secret", secret)
	endpoint.RawQuery = query.Encode()

	headers := http.Header{
		"Content-Type":   []string{"application/json;charset=UTF-8"},
		"plugin-name":    []string{LegacyPluginName},
		"plugin-version": []string{LegacyPluginVersion},
	}
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodGet,
		URL:     endpoint.String(),
		Headers: headers,
	})
	if errDo != nil {
		return nil, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The endpoint returns 4xx while the user is still authenticating.
		return nil, nil
	}
	return parseLegacyTicketResponse(response.Body), nil
}

// parseLegacyTicketResponse extracts a credential from either accepted shape.
func parseLegacyTicketResponse(body []byte) *Credential {
	var envelope legacyTicketEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	for current := &envelope; current != nil; current = current.Data {
		if current.Credential != nil && current.Credential.Access != "" && current.Credential.Secret != "" {
			return &Credential{
				Type:            ProviderKey,
				AccessKeyID:     current.Credential.Access,
				SecretAccessKey: current.Credential.Secret,
				SecurityToken:   current.Credential.SecurityToken,
				ExpiresAt:       current.Credential.ExpiresAt,
			}
		}
		if current.Result != nil && current.Result.AccessKeyID != "" && current.Result.SecretAccessKey != "" {
			return &Credential{
				Type:            ProviderKey,
				AccessKeyID:     current.Result.AccessKeyID,
				SecretAccessKey: current.Result.SecretAccessKey,
				SecurityToken:   current.Result.SecurityToken,
				ExpiresAt:       current.Result.Expiration,
			}
		}
	}
	return nil
}
