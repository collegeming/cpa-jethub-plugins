package main

// 本文件承载 auth.identifier / auth.parse / auth.refresh 三个方法，以及把
// 凭据投影为宿主 AuthData 的逻辑（对应 Jet-Hub `buddy-auth.ts` 中「凭据解析 +
// 到期续期」的部分，DSH service 本体不复用：CPA 的 abiboot 分派取代了它）。

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// refreshLead is how long before expiry a credential should be renewed.
// Mirrors `internal/jethub/credits.RefreshLeadMS` and Jet-Hub's scheduler.
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
	label := credential.Nickname
	if label == "" {
		label = credential.UserID
	}
	if label == "" {
		label = credential.Product
	}
	prefix := credential.UserID
	if prefix == "" {
		prefix = credential.AccessToken
	}
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}

	metadata := map[string]any{
		"product":      credential.Product,
		"account_type": credential.AccountType,
		"nickname":     credential.Nickname,
		"user_id":      credential.UserID,
		"refreshable":  credential.Refreshable(),
	}
	if expiry := credential.Expiry(); !expiry.IsZero() {
		metadata["expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	if credential.EnterpriseID != "" {
		metadata["enterprise_id"] = credential.EnterpriseID
	}

	auth := pluginapi.AuthData{
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
			"product":     credential.Product,
			"refreshable": boolString(credential.Refreshable()),
		},
	}
	if expiry := credential.Expiry(); !expiry.IsZero() {
		auth.NextRefreshAfter = expiry.Add(-refreshLead)
	}
	return auth, nil
}

// defaultAuthFileName derives a stable auth-file name for an account.
func defaultAuthFileName(credential *Credential) string {
	identity := credential.Nickname
	if identity == "" {
		identity = credential.UserID
	}
	if identity == "" {
		identity = credential.Product
	}
	if identity == "" {
		identity = "account"
	}
	identity = unsafeFileNameChars.ReplaceAllString(strings.TrimSpace(identity), "-")
	identity = strings.Trim(identity, "-")
	if identity == "" {
		identity = "account"
	}
	if len(identity) > 48 {
		identity = identity[:48]
	}
	suffix, errSuffix := randomHex(4)
	if errSuffix != nil {
		suffix = "0000"
	}
	return ProviderKey + "-" + identity + "-" + suffix + ".json"
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// truncate shortens a string for an error message.
func truncate(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
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
	// Jet-Hub auth files do not carry the product; default it so the status
	// page and the executor agree on an endpoint.
	if credential.Product == "" {
		credential.Product = settings().Product
	}
	auth, errAuth := authDataFor(credential, request.FileName)
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

// handleAuthRefresh renews a credential through the X-Refresh-Token endpoint.
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
		return nil, abiboot.Errorf("not_refreshable", "该 CodeBuddy 账号缺少 refresh_token，请重新登录")
	}
	product := productForCredential(credential)
	refreshed, errRefresh := refreshCredential(h, credential, product)
	if errRefresh != nil {
		if errors.Is(errRefresh, errRefreshTokenExpired) || errRefresh == errRefreshTokenExpired {
			return nil, abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized,
				"CodeBuddy refresh_token 已失效，请重新登录")
		}
		return nil, errRefresh
	}
	auth, errAuth := authDataFor(refreshed, request.AuthID)
	if errAuth != nil {
		return nil, errAuth
	}
	next := time.Time{}
	if expiry := refreshed.Expiry(); !expiry.IsZero() {
		next = expiry.Add(-refreshLead)
	}
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: next}, nil
}
