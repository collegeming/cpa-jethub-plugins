package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Management API and CPAMP resource routes.
//
// Two mounts with different rules, both determined by the host
// (`internal/pluginhost/management.go`, README "挂载规则"):
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/<id>/<path>`, the path management clients embed in an
//     iframe, and that mount is dispatched as GET ONLY. The repository's ONE
//     Menu belongs to the hub plugin, so this plugin declares none: its pages
//     are ResourceRoutes, reached from the hub's channel overview and from each
//     other;
//   - every other route is registered under `/v0/management/<path>`, a GLOBAL
//     namespace shared with all other plugins and with the host's own endpoints.
//     A collision is skipped with a warning, so those paths carry the provider
//     prefix.
//
// Consequently every interactive control on a page is a link with a query
// string, never a form POST.
//
// handleManagementRegister declares the entries management clients show.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			// Script-facing routes: no Menu, therefore management-API only, and
			// namespaced by the provider key.
			{Method: http.MethodGet, Path: "/" + ProviderKey + "/status",
				Description: "账号与积分状态（JSON）"},
			{Method: http.MethodGet, Path: "/" + ProviderKey + "/checkin",
				Description: "执行每日领取（JSON）"},
		},
		// Browser-reachable pages that must NOT become sidebar entries: a
		// ResourceRoute only shows in the manager nav when it carries a Menu.
		// Login lives on the manager's own OAuth page too (it discovers plugins
		// declaring the auth-provider capability).
		Resources: []pluginapi.ResourceRoute{
			{Path: "/status", Description: "账号、推理通道、模型数量与积分余额（由 hub 的渠道总览链接进入）"},
			{Path: "/login", Description: "设备码 (PKCE) 登录 Qoder 账号（由状态页或 OAuth 登录页进入）"},
			{Path: "/checkin", Description: "领取 Qoder 每日积分（由状态页进入）"},
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
		if wantsJSON(request) {
			return statusJSON(h, request), nil
		}
		return renderStatusPage(h, request), nil

	case "/login":
		if wantsJSON(request) {
			return loginJSON(request), nil
		}
		return renderLoginPage(h, request), nil

	case "/checkin":
		return checkinResponse(h, request), nil
	}

	return jsonManagementResponse(http.StatusNotFound, map[string]any{
		"error": "unknown Qoder management route",
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

// qoderAccounts returns the host's Qoder credentials, in host order.
func qoderAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
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
	for _, entry := range qoderAccounts(h) {
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

// statusJSON is the machine-readable status payload.
//
// The document reports EVERY account this plugin owns: `accounts` is an array,
// one entry per account in host order, each with that account's own quota and
// check-in state. `account_count` keeps the number the field used to carry, and
// the selected account's fields (`account`, `credits`, `daily_checkin`, …) stay
// at the top level for the consumers that read them there.
//
// A failed read is reported as an error field on the entry that failed and never
// as a zero: `credits` is simply absent when it could not be read.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	accounts := qoderAccounts(h)
	body := map[string]any{
		"provider":        ProviderKey,
		"region":          string(activeRegion()),
		"infer_path":      string(activeInferPath(cfg)),
		"wasm_configured": strings.TrimSpace(cfg.WASMPath) != "",
		"wasm_path":       cfg.WASMPath,
		"model_count":     len(staticModelInfos(cfg, activeRegion())),
		"account_count":   len(accounts),
		"accounts":        []map[string]any{},
	}
	if signer, errSigner := signerFor(cfg.WASMPath); errSigner != nil && strings.TrimSpace(cfg.WASMPath) != "" {
		body["wasm_error"] = errSigner.Error()
	} else if errSigner == nil && signer != nil {
		body["wasm_loaded"] = true
	}

	entry, found := selectAccount(h, request)
	if !found {
		body["account"] = nil
		return jsonManagementResponse(http.StatusOK, body)
	}

	quotas := collectAccountQuotas(h, accounts, cfg)
	body["accounts"] = quotaListJSON(quotas)

	current, okCurrent := quotaOf(quotas, entry)
	if !okCurrent {
		// Unreachable while accounts and quotas come from the same listing.
		body["account"] = nil
		body["error"] = "无法定位该账号的额度记录"
		return jsonManagementResponse(http.StatusOK, body)
	}

	account := map[string]any{
		"auth_index": entry.AuthIndex,
		"name":       entry.Name,
		"status":     statusText(entry),
	}
	if label := strings.TrimSpace(entry.Label); label != "" {
		account["label"] = label
	}
	if current.CredentialErr != nil {
		account["error"] = current.CredentialErr.Error()
		body["account"] = account
		return jsonManagementResponse(http.StatusOK, body)
	}
	account["region"] = string(current.Credential.regionOr(activeRegion()))
	account["expires_at"] = jsonTime(current.Credential.ExpiresAt())
	account["refreshable"] = current.Credential.Refreshable()
	account["has_uid"] = strings.TrimSpace(current.Credential.UID) != ""
	body["account"] = account

	switch {
	case current.BalanceErr != nil:
		// The document is still served: the other accounts' figures and the
		// account count are exactly what the hub needs, and this account's
		// balance is reported as unknown rather than as 0.
		body["credit_error"] = current.BalanceErr.Error()
	case current.BalanceNone:
		body["credit_note"] = "企业版账号不下发额度数字"
	default:
		body["credits"] = creditsJSON(current.Balance)
	}
	if current.Campaigns != nil {
		body["daily_checkin"] = map[string]any{
			"claimable": len(claimableCampaigns(current.Campaigns)) > 0,
			"show":      current.Campaigns.ShowCampaign,
			"campaigns": len(current.Campaigns.Campaigns),
		}
	}
	if current.CampaignsErr != nil {
		body["activity_error"] = current.CampaignsErr.Error()
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// loginJSON describes the login route for scripts.
func loginJSON(request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"region": string(activeRegion()),
		"action": strings.TrimSpace(request.Query.Get("action")),
		"hint": "GET ?action=start 获取授权链接；GET ?action=poll&state=<state> 轮询结果。" +
			"设备码流程不需要本地回调端口",
	})
}

// checkinResponse serves the check-in route in both representations.
func checkinResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
		}
		return pluguiPage("Qoder 签到", checkinFailed("指定的账号不存在"))
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		if wantsJSON(request) {
			return jsonManagementResponse(statusOf(errCredential, http.StatusBadRequest),
				map[string]any{"error": errCredential.Error()})
		}
		return pluguiPage("Qoder 签到", checkinFailed(errCredential.Error()))
	}
	outcome, errClaim := claimDailyCheckin(h, credential, settings())
	if errClaim != nil {
		// The failure's own status travels to the caller: a dead credential must
		// be a 401, not a generic 502.
		if wantsJSON(request) {
			return jsonManagementResponse(statusOf(errClaim, http.StatusBadGateway),
				map[string]any{"error": errClaim.Error()})
		}
		return pluguiPage("Qoder 签到", checkinFailed(errClaim.Error()))
	}
	if wantsJSON(request) {
		body := map[string]any{
			"status":  outcome.Status,
			"message": outcome.Message,
			"amount":  outcome.Amount,
		}
		return jsonManagementResponse(http.StatusOK, body)
	}
	return pluguiPage("Qoder 签到", renderCheckinCard(outcome))
}
