package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
//     namespace shared with every other plugin. This plugin declared none for a
//     long time — everything a script needed was `?format=json` on the resource
//     routes — and it now declares exactly one: `/<id>/catalog`. The catalogue
//     refresh is the one action that writes an auth file (that write is what
//     makes the host re-register the provider's models), so it gets an
//     authenticated, explicitly named entry point instead of being reachable
//     only as a side effect of a status-page query string.
//
// The resource mount is dispatched as GET ONLY, which is why every action on
// every page is a link carrying a query string and never a form.
//
// handleManagementRegister declares the entries management clients show.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/" + ProviderKey + "/catalog",
				Description: "刷新模型目录并发布到宿主（JSON；等价于状态页的「刷新目录」按钮）"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/status", Description: "账号与积分余额、模型目录、登录/续期设置（由 hub 的渠道总览链接进入）"},
			{Path: "/login", Description: "微信扫码登录 Raccoon 账号（auth.login.start 直接打开二维码页）；" +
				"手机验证码登录需要人机验证，本插件不提供"},
			{Path: "/reward", Description: "领取一次性的桌面端登录奖励（由状态页进入；这不是每日签到）"},
			{Path: "/checkin", Description: "一键签到的入口：本渠道没有每日签到，这里只处理一次性登录奖励（action=claim 才写）"},
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
		// The manual refresh is an explicit action on the status route: the
		// resource mount is dispatched as GET only, so the button is a link and
		// `?format=json` selects the machine-readable form. It is the only path
		// that publishes (rewrites an auth file), and it runs only because the
		// operator asked for it.
		if strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "refresh-catalog") {
			if wantsJSON(request) {
				return catalogueRefreshJSON(h, request), nil
			}
			return catalogueRefreshPage(h, request), nil
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

	case "/reward":
		if wantsJSON(request) {
			return rewardJSON(h, request), nil
		}
		return renderRewardPage(h, request), nil

	case "/checkin":
		if wantsJSON(request) {
			return checkinJSON(h, request), nil
		}
		return renderCheckinPage(h, request), nil

	case "/catalog":
		return catalogueRefreshJSON(h, request), nil
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

// credentialOf loads one account's credential and makes sure it is still valid
// before a page uses it: an expired credential — or one inside the renewal lead
// window — is renewed through this provider's own `auth.refresh` handler and
// written back to the auth file the host knows.
//
// The returned error covers a credential that could not be READ. A renewal that
// failed comes back inside the result (Result.Err) instead: it does not stop the
// caller from showing the credential's own fields, and — while the credential is
// still inside its validity — does not stop the upstream read either.
func credentialOf(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) (*Credential, authrefresh.Result, error) {
	if h == nil || strings.TrimSpace(entry.AuthIndex) == "" {
		return nil, authrefresh.Result{}, statusError(false, "missing_auth", http.StatusBadRequest, "账号 %s 缺少运行时索引", entry.Name)
	}
	auth, errGet := h.GetAuth(entry.AuthIndex)
	if errGet != nil {
		return nil, authrefresh.Result{}, errGet
	}
	credential, errParse := ParseCredential(auth.JSON)
	if errParse != nil {
		return nil, authrefresh.Result{}, errParse
	}
	result, _ := credentialRefresher.Ensure(h, authrefresh.Request{
		Name:        authNameForHost(entry.Name, entry.Path, entry.Source, credential),
		StorageJSON: auth.JSON,
		Attributes:  map[string]string{"path": entry.Path, "source": entry.Source},
	})
	if len(result.Storage) > 0 {
		if renewed, errRenewed := ParseCredential(result.Storage); errRenewed == nil {
			credential = renewed
		}
	}
	return credential, result, nil
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
	if current.Refresh.Refreshed {
		account["refreshed"] = true
		body["refreshed"] = true
	}
	if current.Refresh.Err != nil {
		account["refresh_error"] = current.Refresh.Err.Error()
		body["refresh_error"] = current.Refresh.Err.Error()
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
	credential, freshness, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(statusOf(errCredential, http.StatusBadRequest),
			map[string]any{"error": errCredential.Error()})
	}
	if freshness.Expired && freshness.Err != nil {
		return jsonManagementResponse(http.StatusBadGateway,
			map[string]any{"error": "凭据已过期且自动续期失败：" + freshness.Err.Error()})
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

// checkinJSON is the hub's one-click entry for this provider.
//
// ⚠️ Raccoon has NO daily check-in: the daily 300 points are granted by the
// server on its own and no endpoint claims them (`raccoon-credits.ts:12-15`,
// trap #26). The only claimable item is the ONE-OFF desktop login reward — and
// it is included here on purpose, because a sweep that silently walks past 3000
// unclaimed points is not doing the job the button promises.
//
// Including it is safe because the write is gated twice:
//
//   - the reward's own state is read FIRST, so an account that already holds it
//     reports 已领取 without issuing a write at all;
//   - the server's `granted` flag is the second gate: a duplicate answers
//     `granted:false`, which maps to already-claimed rather than to a second
//     "claimed" (`raccoon-credits.ts:197-205`).
//
// The route still writes only for an explicit `action=claim`, matching every
// sibling provider: a page load or a monitor poll must never grant anything.
func checkinJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	entry, found := selectAccount(h, request)
	if !found {
		// Distinguish "no account at all" from "the selector did not match".
		// Both used to answer "没有可用账号", which sends a caller who passed a
		// stale auth_index looking for a credential that is right there.
		status, message := "no-account", "该 provider 下没有可用账号"
		if selector := strings.TrimSpace(request.Query.Get("auth_index")); selector != "" {
			status, message = "unknown-account", "指定的 auth_index 不存在："+selector
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"provider": ProviderKey,
			"status":   status,
			"message":  message,
		})
	}
	credential, _, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"provider": ProviderKey,
			"account":  entry.Name,
			"status":   "failed",
			"message":  errCredential.Error(),
		})
	}

	status := fetchRewardStatus(h, credential, cfg)
	payload := map[string]any{
		"provider":      ProviderKey,
		"account":       entry.Name,
		"one_off":       true,
		"reward_points": status.Points,
	}

	if !isClaimRequest(request) {
		payload["status"] = "needs-action"
		payload["message"] = "本渠道没有每日签到（每日额度由服务端自动发放）；" +
			"该接口只在 action=claim 时领取一次性桌面端登录奖励，本次仅返回状态"
		payload["reward_claimed"] = status.Claimed
		if status.Note != "" {
			payload["reward_note"] = status.Note
		}
		return jsonManagementResponse(http.StatusOK, payload)
	}

	if status.Claimed {
		payload["status"] = "already-claimed"
		payload["message"] = "一次性桌面端登录奖励已领取（每号一次）"
		payload["reward_claimed"] = true
		if status.Note != "" {
			payload["reward_note"] = status.Note
		}
		return jsonManagementResponse(http.StatusOK, payload)
	}

	outcome := grantLoginReward(h, credential, cfg)
	payload["status"] = outcome.Kind
	payload["message"] = outcome.Message
	payload["reward_claimed"] = outcome.Kind != "claimed"
	if outcome.Credit > 0 {
		payload["reward_points"] = outcome.Credit
	}
	if outcome.Code != 0 {
		payload["code"] = outcome.Code
	}
	return jsonManagementResponse(http.StatusOK, payload)
}

// renderCheckinPage renders the same action as a page, so the route is usable
// from the browser as well as from the hub.
func renderCheckinPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	response := checkinJSON(h, request)
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		return plugui.HTML("Raccoon 一键领取",
			plugui.Card("结果", plugui.Notice("danger", "无法解析结果")))
	}
	status, _ := document["status"].(string)
	message, _ := document["message"].(string)
	tone := "success"
	switch status {
	case "failed":
		tone = "danger"
	case "needs-action", "no-account":
		tone = "warning"
	}
	return plugui.HTML("Raccoon 一键领取",
		plugui.Card("结果",
			plugui.Group(
				plugui.Notice(tone, message),
				plugui.Notice("", "本渠道没有每日签到：每日 300 积分由服务端自动发放。"+
					"这里处理的是一次性的桌面端登录奖励（每号一次，服务端幂等）。"),
			),
			plugui.Action{Label: "立即领取", Query: "action=claim&auth_index=" + entryAuthIndex(request), Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		))
}

// isClaimRequest reports whether the caller asked for the grant to run.
func isClaimRequest(request pluginapi.ManagementRequest) bool {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "claim", "grant", "checkin":
		return true
	default:
		return false
	}
}

// entryAuthIndex echoes the selector so the page's own link stays on the same
// account; an empty selector is fine (the first account is used).
func entryAuthIndex(request pluginapi.ManagementRequest) string {
	return strings.TrimSpace(request.Query.Get("auth_index"))
}
