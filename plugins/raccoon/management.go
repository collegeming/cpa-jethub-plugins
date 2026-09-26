package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Management API and CPAMP resource routes.
//
// Three mounts, all decided by the host:
//
//   - a GET route carrying a Menu is registered under
//     `/v0/resource/plugins/<id>/<path>` AND becomes its own sidebar entry in
//     CPA-Manager-Plus. The repository's single entry belongs to the hub plugin,
//     which is why NO route below carries a Menu.
//   - a ResourceRoute is mounted under the same prefix but is listed in the
//     sidebar only when it carries a Menu. Leaving Menu empty keeps the page
//     browser-reachable — the hub's channel overview and this plugin's status
//     page link to them — without adding a nav item.
//   - any other route is registered under `/v0/management/<path>`, a GLOBAL
//     namespace shared with every other plugin. This plugin deliberately
//     declares none: everything a script needs is `?format=json` on the resource
//     routes.
//
// The resource mount is dispatched as GET ONLY, which is why every action on
// every page is a link carrying a query string and never a form.
//
// handleManagementRegister declares the entries management clients show.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/status", Description: "账号与积分余额、模型目录、登录/续期设置（由 hub 的渠道总览链接进入）"},
			{Path: "/login", Description: "微信扫码登录 Raccoon 账号（auth.login.start 直接打开二维码页）；" +
				"手机验证码登录需要人机验证，本插件不提供"},
			{Path: "/reward", Description: "领取一次性的桌面端登录奖励（由状态页进入；这不是每日签到）"},
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

	case "/reward":
		if wantsJSON(request) {
			return rewardJSON(h, request), nil
		}
		return renderRewardPage(h, request), nil
	}
	return jsonManagementResponse(http.StatusNotFound, map[string]any{
		"error": "unknown Raccoon management route",
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

// raccoonAccounts returns the host's Raccoon credentials, in host order.
func raccoonAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
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
	for _, entry := range raccoonAccounts(h) {
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
	if h == nil || strings.TrimSpace(entry.AuthIndex) == "" {
		return nil, statusError(false, "missing_auth", http.StatusBadRequest, "账号 %s 缺少运行时索引", entry.Name)
	}
	auth, errGet := h.GetAuth(entry.AuthIndex)
	if errGet != nil {
		return nil, errGet
	}
	credential, errParse := ParseCredential(auth.JSON)
	if errParse != nil {
		return nil, errParse
	}
	return credential, nil
}

// statusJSON is the machine-readable status payload behind `?format=json`.
//
// The document reports EVERY account this plugin owns: `accounts` is an array,
// one entry per account in host order, each with that account's OWN point pools.
// `account_count` keeps the number the field used to carry, and the selected
// account's fields (`account`, `points`, …) stay at the top level for consumers
// that read them there.
//
// A failed read is reported as an error field on the entry that failed and never
// as a zero: `points` is simply absent when it could not be read.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	accounts := raccoonAccounts(h)
	body := map[string]any{
		"provider":               ProviderKey,
		"discover_models":        cfg.DiscoverModels,
		"model_count":            len(fallbackCatalogue),
		"account_count":          len(accounts),
		"accounts":               []map[string]any{},
		"login_page":             loginResourcePath,
		"login_method":           "wechat-qr",
		"sms_login":              false,
		"sms_login_note":         smsLoginNotice,
		"refreshable":            true,
		"refresh_path":           RefreshPath,
		"refresh_window_seconds": cfg.refreshWindow(),
		// There is no daily check-in for this provider; the one-off login reward
		// is a separate, explicit action and is never part of a sweep.
		"daily_checkin":  false,
		"one_off_reward": LoginPointsGrantPath,
		"reward_page":    "/v0/resource/plugins/" + ProviderKey + "/reward",
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
		body["error"] = "无法定位该账号的积分记录"
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
	body["account"] = account
	if current.CredentialErr != nil {
		account["error"] = current.CredentialErr.Error()
		return jsonManagementResponse(http.StatusOK, body)
	}
	body["model_count"] = len(activeCatalogue(h, current.Credential, cfg))
	account["phone"] = current.Credential.maskedPhone()
	account["userid"] = current.Credential.UserID
	account["office"] = current.Credential.OfficeIdentity
	account["refreshable"] = current.Credential.Refreshable()
	account["expired"] = current.Credential.Expired(time.Now())
	account["needs_refresh"] = current.Credential.NeedsRefresh(time.Now(), refreshWindow(cfg))
	if expiry := current.Credential.Expiry(); !expiry.IsZero() {
		account["expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	switch {
	case current.PointsErr != nil:
		// The document is still served: the other accounts' figures and the
		// account count are exactly what the hub needs, and this account's points
		// are reported as unknown rather than as 0.
		body["points_error"] = current.PointsErr.Error()
	case current.PointsNone:
		body["points_note"] = "服务端未返回 available_points 字段"
	default:
		body["points"] = pointsJSON(current.Snapshot)
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// loginJSON documents the login resource route for scripts.
func loginJSON(request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	state := strings.TrimSpace(request.Query.Get("state"))
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"action": strings.TrimSpace(request.Query.Get("action")),
		"state":  state,
		"primary": "微信扫码：GET ?action=qr&state=… 返回二维码页（data: URL PNG + meta refresh），" +
			"每次加载做一次轮询；服务端状态 pending / logging / canceled / success，" +
			"canceled 会自动换新码；success 且带 access_token 时保存凭据",
		"sms_login":  false,
		"sms_note":   smsLoginNotice,
		"qr_payload": "https://xiaohuanxiong.com/login/mp?code=<32 位小写十六进制>&appname=商汤小浣熊官网（144 字节，二维码版本 8 / 纠错级别 M）",
		"no_forms":   "resource 路由只派发 GET：所有动作都是查询串链接，页面没有表单也没有脚本",
		"poll":       "轮询接口 POST " + QRLoginCodePath + "（只带 Content-Type）；网络错误、未知状态、无 token 的 success 都按 pending 处理",
		"credential": "凭据以 access_token 作为账号池身份字段（不落在任何持久索引上，因为续期会轮换它）",
	})
}

// rewardJSON runs the ONE-OFF desktop login reward for scripts.
func rewardJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "未找到 Raccoon 账号"})
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(statusOf(errCredential, http.StatusBadRequest),
			map[string]any{"error": errCredential.Error()})
	}
	cfg := settings()
	if !strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "grant") {
		status := fetchRewardStatus(h, credential, cfg)
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"action":     "confirm",
			"one_off":    true,
			"daily":      false,
			"claimed":    status.Claimed,
			"points":     status.Points,
			"biz_type":   LoginRewardBizType,
			"event_name": LoginRewardEventName,
			"note":       status.Note,
			"hint":       "追加 &action=grant 才会真正调用 POST " + LoginPointsGrantPath + "（写接口，每号一次）",
		})
	}
	outcome := grantLoginReward(h, credential, cfg)
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"kind":    outcome.Kind,
		"code":    outcome.Code,
		"message": outcome.Message,
		"credit":  outcome.Credit,
		"one_off": true,
		// The server's `granted` flag is the idempotency key; a repeat is
		// reported as already-claimed, never as claimed again.
		"idempotent": "服务端 granted 标记",
	})
}

// jsonManagementResponse renders a management API reply.
func jsonManagementResponse(status int, body any) pluginapi.ManagementResponse {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		encoded = []byte(`{"error":"failed to encode response"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       encoded,
	}
}
