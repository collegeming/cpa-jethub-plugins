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
// Three mounts, all decided by the host (`internal/pluginhost/management.go`,
// README "挂载规则"):
//
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/<id>/<path>` AND becomes its own sidebar entry in
//     CPA-Manager-Plus. The manager renders one nav item per menu route and does
//     not group them by plugin, so EVERY EXTRA MENU ROUTE LOOKS LIKE A DUPLICATE
//     ENTRY. The repository's single entry belongs to the hub plugin, which is
//     why NO route below carries a Menu.
//   - a ResourceRoute is mounted under the same prefix but is listed in the
//     sidebar only when it carries a Menu. Leaving Menu empty keeps the page
//     browser-reachable — the hub's channel overview and the status page link to
//     it — without adding a nav item.
//   - any other route is registered under `/v0/management/<path>`, a GLOBAL
//     namespace shared with every other plugin. A collision is skipped with a
//     warning, so such paths would need the provider prefix. This plugin
//     deliberately declares none: everything a script needs is `?format=json` on
//     the resource routes.
//
// The resource mount is dispatched as GET ONLY, which is why every action on
// every page is a link carrying a query string and never a form.
//
// handleManagementRegister declares the entries management clients show.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/status", Description: "账号、模型目录与积分余额（由 hub 的渠道总览链接进入）"},
			{Path: "/login", Description: "微信扫码登录 Loomy 账号（auth.login.start 直接打开二维码页）；手机验证码登录作为备用路径在同一页提供"},
			{Path: "/checkin", Description: "初始化 Loomy 每日赠送额度（由状态页进入）"},
			{Path: "/onboarding", Description: "领取 Loomy 一次性新手任务积分（由状态页进入）"},
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
		if wantsJSON(request) {
			return checkinJSON(h, request), nil
		}
		return renderCheckinPage(h, request), nil

	case "/onboarding":
		if wantsJSON(request) {
			return onboardingJSON(h, request), nil
		}
		return renderOnboardingPage(h, request), nil
	}
	return jsonManagementResponse(http.StatusNotFound, map[string]any{
		"error": "unknown Loomy management route",
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

// loomyAccounts returns the host's Loomy credentials, in host order.
func loomyAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
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
	for _, entry := range loomyAccounts(h) {
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
//
// ⚠️ Unlike every other provider in this repository, the credential is NOT
// freshness-checked before use. That is deliberate, not an omission: Loomy has
// no refresh endpoint and its credentials carry no refresh token
// (`loomy.ts:195-205`), so `auth.refresh` is a validity PROBE and there is
// nothing to renew. Forcing one would either invent a protocol that does not
// exist or hammer the probe on every request; the whole point of the shared
// refresher is that a credential with no renewal path is left alone. The honesty
// lives on the page instead — 可自动续期 否（无 refresh_token，过期只能重新登录）
// — and the server's 100002 stays the only authority on whether a session is
// really dead. plugins/loomy/freshness_test.go pins all of that.
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
// one entry per account in host order, each with that account's own point pools.
// `account_count` keeps the number the field used to carry, and the selected
// account's fields (`account`, `points`, …) stay at the top level for the
// consumers that read them there.
//
// A failed read is reported as an error field on the entry that failed and never
// as a zero: `points` is simply absent when it could not be read.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	accounts := loomyAccounts(h)
	body := map[string]any{
		"provider":        ProviderKey,
		"discover_models": cfg.DiscoverModels,
		"model_count":     len(fallbackCatalogue),
		"account_count":   len(accounts),
		"accounts":        []map[string]any{},
		"login_page":      loginResourcePath,
		// Loomy has no refresh_token: `auth.refresh` only probes validity.
		"refreshable": false,
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
	account["phone"] = current.Credential.Phone
	account["userid"] = current.Credential.UserID
	account["expired"] = current.Credential.Expired(time.Now())
	if expiry := current.Credential.Expiry(); !expiry.IsZero() {
		account["expires_at"] = expiry.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	switch {
	case current.PointsErr != nil:
		// The document is still served: the other accounts' figures and the
		// account count are exactly what the hub needs, and this account's
		// points are reported as unknown rather than as 0.
		body["points_error"] = current.PointsErr.Error()
	case current.PointsNone:
		body["points_note"] = "服务端未返回 balance 字段"
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
		"primary": "微信扫码：GET ?action=qr&state=… 返回二维码页（data: URL 图片 + meta refresh），" +
			"每次加载做一次长轮询（408 待扫码 / 404 已扫码 / 405 已确认并携带授权码 / 403 取消 / 402 过期）；" +
			"405 之后自动 bind/auth → bind===1 走 bind/skip，否则用 ?action=bindsend&phone=… 与 " +
			"?action=bindverify&code=… 绑定手机号。凭据存好后 auth.login.poll 由 pending 变为 success",
		"backup": "手机验证码：GET ?action=send&state=…&phone=13800138000 发送验证码；" +
			"?action=code&state=…&digit=5 用数字链接输入；?action=verify&state=…&phone=…&code=123456 提交并保存凭据。" +
			"号码也可以在本页用 ?action=phone&digit=… 输入，不需要改插件设置",
		"phone_gate":  "^1[3-9]\\d{9}$",
		"wechat_app":  WechatAppID,
		"redirect":    WechatRedirectURI,
		"poll_url":    WechatLongPollURL,
		"no_forms":    "resource 路由只派发 GET：所有动作都是查询串链接，页面没有表单也没有脚本",
		"no_refresh":  "Loomy 没有 refresh_token：auth.refresh 只做有效性探测",
		"callback404": "微信回调页是白名单占位、本身 404；授权码由长轮询取得，不经过回调页",
	})
}

// checkinJSON runs the daily-grant initialisation for scripts.
func checkinJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "未找到 Loomy 账号"})
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(statusOf(errCredential, http.StatusBadRequest),
			map[string]any{"error": errCredential.Error()})
	}
	if !strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "claim") {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"action": "confirm",
			"hint":   "追加 &action=claim 才会真正调用 POST /points/first-login（写接口）",
		})
	}
	outcome := claimDailyGrant(h, credential, settings())
	body := map[string]any{
		"status":  outcome.Status,
		"message": outcome.Message,
		"credit":  outcome.Credit,
	}
	if outcome.Snapshot != nil {
		body["balance"] = outcome.Snapshot.Balance
		body["daily_balance"] = outcome.Snapshot.DailyBalance
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// onboardingJSON serves the one-off onboarding tasks for scripts.
func onboardingJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "未找到 Loomy 账号"})
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(statusOf(errCredential, http.StatusBadRequest),
			map[string]any{"error": errCredential.Error()})
	}
	cfg := settings()
	if !strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "claim") {
		state, errState := fetchOnboardingState(h, credential, cfg)
		if errState != nil {
			return jsonManagementResponse(statusOf(errState, http.StatusBadGateway),
				map[string]any{"error": errState.Error()})
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"tasks":   state.Tasks,
			"earned":  state.Earned,
			"total":   state.Total,
			"balance": state.Balance,
			"hint":    "追加 &action=claim 才会提交 POST /onboarding/tasks/complete",
		})
	}
	if key := strings.TrimSpace(request.Query.Get("key")); key != "" {
		already, errComplete := completeOnboardingTask(h, credential, key, cfg)
		if errComplete != nil {
			return jsonManagementResponse(statusOf(errComplete, http.StatusBadGateway),
				map[string]any{"error": errComplete.Error()})
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"key": key, "already_completed": already, "status": "claimed",
		})
	}
	outcome, errOutcome := claimAllOnboarding(h, credential, cfg)
	if errOutcome != nil {
		return jsonManagementResponse(statusOf(errOutcome, http.StatusBadGateway),
			map[string]any{"error": errOutcome.Error(), "earned": outcome.Earned})
	}
	claimed := make([]map[string]any, 0, len(outcome.Claimed))
	for _, item := range outcome.Claimed {
		claimed = append(claimed, map[string]any{"key": item.Key, "points": item.Points})
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"claimed":    claimed,
		"skipped":    outcome.Skipped,
		"earned":     outcome.Earned,
		"total":      outcome.Total,
		"message":    outcome.Message,
		"one_off":    true,
		"idempotent": "重复提交由响应的 alreadyCompleted 判定，重放同样算成功",
	})
}
