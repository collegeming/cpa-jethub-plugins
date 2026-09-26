package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Management API and CPAMP resource routes.
//
// Two mounts with different rules, both determined by the host
// (`internal/pluginhost/management.go`, README "挂载规则"):
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/<id>/<path>`, the path management clients embed in an
//     iframe, and that mount is dispatched as GET ONLY;
//   - every other route is registered under `/v0/management/<path>`, a GLOBAL
//     namespace shared with all other plugins and with the host's own endpoints.
//     A collision is skipped with a warning, so those paths carry the provider
//     prefix.
//
// Consequently every interactive control on a page is a link with a query
// string, never a form POST.

// handleManagementRegister declares the entries management clients show.
//
// Exactly ONE route carries a Menu. The manager renders one sidebar entry per
// menu route and does not group them by plugin, so extra menu routes look like
// duplicates (commit f697edd in this repository). The login page must stay
// browser-reachable without adding a nav item, which is exactly what an empty
// Menu on a ResourceRoute does: `registeredPluginMenus` skips empty-Menu entries
// while the path remains served.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/status", Menu: "Cline",
				Description: "账号、令牌前缀、余额与模型数量"},
			// Script-facing routes: no Menu, therefore management-API only, and
			// namespaced by the provider key.
			{Method: http.MethodGet, Path: "/" + ProviderKey + "/status",
				Description: "账号与余额状态（JSON）"},
			{Method: http.MethodPost, Path: "/" + ProviderKey + "/refresh",
				Description: "续期当前账号的 Cline 令牌（JSON）"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/login", Description: "WorkOS 设备码登录 Cline 账号（由状态页或 OAuth 登录页进入）"},
		},
	}, nil
}

// handleManagementHandle dispatches the routes declared above.
//
// The request path is the FULL incoming path and differs per mount, so only its
// last segment identifies the route.
func handleManagementHandle(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ManagementRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}

	switch managementRoute(request.Path) {
	case "/status":
		if strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "refresh") {
			return refreshPage(h, request), nil
		}
		if wantsJSON(request) {
			return statusJSON(h, request), nil
		}
		return renderStatusPage(h, request), nil

	case "/login":
		if wantsJSON(request) {
			return loginJSON(request), nil
		}
		return renderLoginPage(h, request), nil

	case "/refresh":
		return refreshJSON(h, request), nil
	}

	return jsonManagementResponse(http.StatusNotFound, map[string]any{
		"error": "unknown Cline management route",
		"path":  request.Path,
	}), nil
}

// managementRoute reduces the request path to the route the plugin registered.
func managementRoute(path string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return "/" + trimmed
}

// wantsJSON reports whether the caller asked for machine-readable output.
func wantsJSON(request pluginapi.ManagementRequest) bool {
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("format")), "json") {
		return true
	}
	// A page navigation sends an Accept header mentioning text/html; anything
	// else is treated as a script or a client that wants data.
	accept := strings.ToLower(request.Headers.Get("Accept"))
	return accept != "" && !strings.Contains(accept, "text/html")
}

// clineAccounts returns the host's Cline credentials, in host order.
func clineAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
	if h == nil {
		return nil
	}
	entries, errList := h.ListAuth()
	if errList != nil {
		return nil
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Provider == ProviderKey || entry.Type == ProviderKey {
			out = append(out, entry)
		}
	}
	return out
}

// selectAccount resolves which credential a page should act on. Without an
// explicit selector the first account is used, so the page works with no
// parameters at all.
func selectAccount(h *abiboot.Host, request pluginapi.ManagementRequest) (pluginapi.HostAuthFileEntry, bool) {
	wanted := strings.TrimSpace(request.Query.Get("auth_index"))
	if wanted == "" {
		wanted = strings.TrimSpace(request.Query.Get("auth_id"))
	}
	for _, entry := range clineAccounts(h) {
		if wanted == "" {
			return entry, true
		}
		if entry.AuthIndex == wanted || entry.ID == wanted || entry.Name == wanted {
			return entry, true
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

// credentialOf loads and parses the credential of one account.
func credentialOf(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) (*Credential, error) {
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return nil, statusError(false, "missing_auth", http.StatusBadRequest, "账号 %s 缺少运行时索引", entry.Name)
	}
	auth, errGet := h.GetAuth(entry.AuthIndex)
	if errGet != nil {
		return nil, errGet
	}
	return ParseCredential(auth.JSON)
}

// refreshAccount renews one account and persists the result through the host.
//
// On the normal host-driven path `auth.refresh` returns the new record and the
// host writes it; a page-driven refresh has to save it itself (the same
// arrangement the other device-code plugins use for their browser login page).
func refreshAccount(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) (*Credential, error) {
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return nil, errCredential
	}
	refreshed, errRefresh := refreshCredential(transportFor(h), credential)
	if errRefresh != nil {
		return nil, errRefresh
	}
	storage, errEncode := refreshed.Encode()
	if errEncode != nil {
		return nil, errEncode
	}
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		name = defaultAuthFileName(refreshed)
	}
	if _, errSave := h.SaveAuth(name, storage); errSave != nil {
		return nil, transportError("save_auth", "保存续期后的凭据失败：%v", errSave)
	}
	return refreshed, nil
}

// statusJSON is the machine-readable status payload.
//
// The access token is NEVER included. The `token_prefixed` boolean is the one
// diagnostic that matters here: a credential whose token lost the `workos:`
// prefix answers 401 upstream with a body that blames the client version
// (spec risk 8).
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	body := map[string]any{
		"provider":           ProviderKey,
		"api_base":           APIBase,
		"workos_base":        WorkOSBase,
		"token_prefix":       TokenPrefix,
		"model_discovery":    cfg.ModelDiscovery,
		"static_model_count": len(staticModelInfos()),
		"reasoning_levels":   append([]string(nil), reasoningLevels...),
		"accounts":           len(clineAccounts(h)),
	}
	if cached, fetchedAt := discoveredModels.peek(); len(cached) > 0 {
		body["cached_model_count"] = len(cached)
		body["cached_model_fetched_at"] = jsonTime(fetchedAt)
	}
	entry, found := selectAccount(h, request)
	if !found {
		body["account"] = nil
		return jsonManagementResponse(http.StatusOK, body)
	}
	account := map[string]any{
		"auth_index": entry.AuthIndex,
		"name":       entry.Name,
		"status":     statusText(entry),
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		account["error"] = errCredential.Error()
		body["account"] = account
		return jsonManagementResponse(statusOf(errCredential, http.StatusOK), body)
	}
	account["account_id"] = credential.AccountID
	account["email"] = credential.Email
	account["label"] = credential.Label()
	account["expires_at"] = jsonTime(credential.ExpiresAt())
	account["refreshable"] = credential.Refreshable()
	account["token_prefixed"] = strings.HasPrefix(strings.TrimSpace(credential.AccessToken), TokenPrefix)
	body["account"] = account

	balance, errBalance := fetchBalance(transportFor(h), credential, cfg)
	switch {
	case errBalance != nil:
		body["balance_error"] = errBalance.Error()
	case balance != nil:
		body["balance"] = balanceJSON(balance)
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// loginJSON describes the login route for scripts.
func loginJSON(request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"action": strings.TrimSpace(request.Query.Get("action")),
		"state":  strings.TrimSpace(request.Query.Get("state")),
		"hint": "GET ?action=start 获取授权链接与 user_code；GET ?action=poll&state=<state> 轮询结果并在成功后保存凭据。" +
			"WorkOS 设备码流程不需要本地回调端口",
	})
}

// refreshJSON renews the selected account for scripts.
func refreshJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
	}
	refreshed, errRefresh := refreshAccount(h, entry)
	if errRefresh != nil {
		return jsonManagementResponse(statusOf(errRefresh, http.StatusBadGateway),
			map[string]any{"error": errRefresh.Error()})
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"status":      "ok",
		"account_id":  refreshed.AccountID,
		"expires_at":  jsonTime(refreshed.ExpiresAt()),
		"refreshable": refreshed.Refreshable(),
	})
}

// refreshPage performs a page-driven refresh and renders the outcome.
func refreshPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return pluguiPage("Cline", pluguiNoticeCard("刷新失败", "danger", "指定的账号不存在",
			plugui.Action{Label: "返回状态", Path: "status"}))
	}
	refreshed, errRefresh := refreshAccount(h, entry)
	if errRefresh != nil {
		return pluguiPage("Cline", pluguiNoticeCard("刷新失败", "danger", errRefresh.Error(),
			plugui.Action{Label: "重试", Query: "action=refresh"},
			plugui.Action{Label: "返回状态", Path: "status"}))
	}
	message := "令牌已续期"
	if !refreshed.ExpiresAt().IsZero() {
		message += "，有效期至 " + formatExpiry(refreshed.ExpiresAt())
	}
	return pluguiPage("Cline", pluguiNoticeCard("刷新成功", "success", message,
		plugui.Action{Label: "返回状态", Path: "status", Kind: "primary"}))
}

// statusText summarises a host credential entry.
func statusText(entry pluginapi.HostAuthFileEntry) string {
	switch {
	case entry.Disabled:
		return "已停用"
	case entry.Unavailable:
		return "不可用"
	case strings.TrimSpace(entry.Status) != "":
		if entry.StatusMessage != "" {
			return entry.Status + "（" + entry.StatusMessage + "）"
		}
		return entry.Status
	default:
		return "正常"
	}
}
