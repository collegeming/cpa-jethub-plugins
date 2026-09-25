package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// refreshLead is how long before expiry a credential should be renewed.
const refreshLead = time.Hour

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

// handleAuthRefresh renews a device-code credential.
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
		return nil, abiboot.Errorf("not_refreshable", "该 Qoder 账号缺少 refresh_token，请重新登录")
	}
	refreshed, errRefresh := refreshCredential(h, credential, settings())
	if errRefresh != nil {
		return nil, errRefresh
	}
	auth, errAuth := authDataFor(refreshed, request.AuthID)
	if errAuth != nil {
		return nil, errAuth
	}
	next := time.Time{}
	if refreshed.ExpireTime > 0 {
		next = refreshed.ExpiresAt().Add(-refreshLead)
	}
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: next}, nil
}

// refreshCredential performs one `/api/v1/deviceToken/refresh` call.
func refreshCredential(h *abiboot.Host, credential *Credential, cfg Config) (*Credential, error) {
	body, errBody := credential.refreshBody()
	if errBody != nil {
		return nil, errBody
	}
	p := credential.product(cfg.Region)
	headers := canonicalHeader(
		"Content-Type", "application/json",
		"Accept", "application/json",
		"User-Agent", p.UserAgentPrefix+"/1.0.0",
	)
	response, errDo := hostRequest(h, http.MethodPost, p.OpenAPIBase+RefreshPath, headers, body, cfg)
	if errDo != nil {
		return nil, abiboot.RetryableError("refresh_transport", "续期请求失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, abiboot.HTTPError("refresh_token_expired", http.StatusUnauthorized,
				"Qoder refresh_token 已失效，请重新登录")
		}
		return nil, abiboot.Errorf("refresh_failed", "续期返回 HTTP %d：%s",
			response.StatusCode, truncate(string(response.Body), 300))
	}
	payload := parseTokenPayloadJSON(response.Body)
	if payload.AccessToken == "" {
		return nil, abiboot.Errorf("refresh_empty", "续期响应没有访问令牌：%s", truncate(string(response.Body), 300))
	}
	return credential.applyRefresh(payload), nil
}

// authDataFor converts a credential into the host-facing AuthData record.
func authDataFor(credential *Credential, fileName string) (pluginapi.AuthData, error) {
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		return pluginapi.AuthData{}, errEncode
	}
	region := credential.regionOr(activeRegion())
	if credential.Region == "" {
		credential.Region = string(region)
	}
	if fileName == "" {
		fileName = defaultAuthFileName(credential)
	}
	label := credential.Nickname
	if label == "" {
		label = credential.UID
	}
	if label == "" {
		label = string(region)
	}
	prefix := credential.UID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	metadata := map[string]any{"region": string(region)}
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
			"region":      string(region),
			"refreshable": boolString(credential.Refreshable()),
		},
		NextRefreshAfter: nextRefresh,
	}, nil
}

// defaultAuthFileName derives a stable auth-file name for an account. The region
// is part of the name because the two sites never share credentials.
func defaultAuthFileName(credential *Credential) string {
	region := credential.regionOr(activeRegion())
	identity := credential.Nickname
	if identity == "" {
		identity = credential.UID
	}
	if identity == "" {
		token := credential.bearerToken()
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
	return string(region) + "-" + identity + ".json"
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// truncate shortens a diagnostic string so error messages stay readable.
func truncate(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "…"
}
