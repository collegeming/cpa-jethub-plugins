package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Management API and CPAMP resource routes.
//
// Two mounts with different rules, both determined by the host:
//
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/<id>/<path>`, the path management clients embed in
//     an iframe, and that mount is dispatched as GET ONLY. The repository's ONE
//     Menu belongs to the hub plugin, so this plugin declares none: its pages
//     are ResourceRoutes, reached from the hub's channel overview and from each
//     other;
//   - every other route is registered under `/v0/management/<path>`, a GLOBAL
//     namespace shared with all other plugins and with the host's own
//     endpoints. A collision is skipped with a warning, so those paths carry the
//     provider prefix.
//
// Consequently every interactive control on a page is a link with a query
// string, never a form POST.

// handleManagementRegister declares the entries management clients show.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			// Script-facing routes: no Menu, therefore management-API only, and
			// namespaced by the provider key.
			{Method: http.MethodGet, Path: "/" + ProviderKey + "/status",
				Description: "账号、签到与积分状态（JSON）"},
			{Method: http.MethodGet, Path: "/" + ProviderKey + "/checkin",
				Description: "执行每日签到（JSON）"},
		},
		// Browser-reachable pages that must NOT become sidebar entries: a
		// ResourceRoute only shows in the manager nav when it carries a Menu.
		Resources: []pluginapi.ResourceRoute{
			{Path: "/status", Description: "账号、模型目录、推理协议与积分余额（由 hub 的渠道总览链接进入）"},
			{Path: "/login", Description: "MiniMax Code 设备码 (PKCE) 登录（由状态页或 OAuth 登录页进入）；无需本地回调端口"},
			{Path: "/checkin", Description: "领取 MiniMax 每日积分（由状态页进入）"},
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
			return loginJSON(h, request), nil
		}
		return renderLoginPage(h, request), nil

	case "/checkin":
		return checkinResponse(h, request), nil
	}
	return jsonManagementResponse(http.StatusNotFound, map[string]any{
		"error": "unknown MiniMax management route",
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

// minimaxAccounts returns the host's MiniMax credentials, in host order.
func minimaxAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
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
	for _, entry := range minimaxAccounts(h) {
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
// failed comes back inside the result (`Result.Err`) instead: it does not stop
// the caller from showing the credential's own fields.
func credentialOf(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) (*Credential, authrefresh.Result, error) {
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return nil, authrefresh.Result{}, statusError(false, "missing_auth",
			http.StatusBadRequest, "账号 %s 缺少运行时索引", entry.Name)
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

// accountState is one account's gathered state, for the JSON document and the
// cards.
type accountState struct {
	Entry         pluginapi.HostAuthFileEntry
	Credential    *Credential
	CredentialErr error
	Refresh       authrefresh.Result
	Balance       *creditBalance
	BalanceErr    error
	Signin        signinStatus
	SigninErr     error
}

// collectAccountStates reads every account's credits and check-in state, in
// host order, one account at a time.
//
// Sequential on purpose: each account costs upstream requests, and one
// account's failure never stops the sweep nor hides another's figures.
func collectAccountStates(h *abiboot.Host, accounts []pluginapi.HostAuthFileEntry, cfg Config) []accountState {
	out := make([]accountState, 0, len(accounts))
	for _, entry := range accounts {
		state := accountState{Entry: entry}
		credential, freshness, errCredential := credentialOf(h, entry)
		if errCredential != nil {
			state.CredentialErr = errCredential
			out = append(out, state)
			continue
		}
		state.Credential = credential
		state.Refresh = freshness
		if balance, errBalance := fetchCreditBalance(h, credential, cfg); errBalance != nil {
			state.BalanceErr = errBalance
		} else {
			state.Balance = balance
		}
		if signin, errSignin := fetchSigninStatus(h, credential, cfg); errSignin != nil {
			state.SigninErr = errSignin
		} else {
			state.Signin = signin
		}
		out = append(out, state)
	}
	return out
}

// stateOf picks one account's state out of a collected list.
func stateOf(states []accountState, entry pluginapi.HostAuthFileEntry) (accountState, bool) {
	for _, state := range states {
		if state.Entry.AuthIndex == entry.AuthIndex && state.Entry.Name == entry.Name {
			return state, true
		}
	}
	return accountState{}, false
}

// statusJSON is the machine-readable status payload.
//
// The document reports EVERY account this plugin owns: `accounts` is an array,
// one entry per account in host order, each with that account's own credits and
// check-in state. `account_count` keeps the number the field used to carry, and
// the selected account's fields stay at the top level for consumers that read
// them there.
//
// A failed read is reported as an error field on the entry that failed and never
// as a zero: `credits` is simply absent when it could not be read.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	accounts := minimaxAccounts(h)
	body := map[string]any{
		"provider":           ProviderKey,
		"region":             Region,
		"build_env":          BuildEnv,
		"infer_path":         InferPath,
		"protocol":           "anthropic-messages",
		"discover_models":    cfg.DiscoverModels,
		"model_count":        len(staticCatalogueEntries(cfg)),
		"account_count":      len(accounts),
		"accounts":           []map[string]any{},
		"timezone_id":        cfg.timezoneID(),
		"login_verified":     LoginVerified,
		"inference_verified": InferenceVerified,
		"refresh_verified":   RefreshVerified,
	}
	if errNote := catalogueLastError(); errNote != "" {
		body["catalogue_error"] = errNote
	}

	entry, found := selectAccount(h, request)
	if !found {
		body["account"] = nil
		return jsonManagementResponse(http.StatusOK, body)
	}

	states := collectAccountStates(h, accounts, cfg)
	items := make([]map[string]any, 0, len(states))
	for _, state := range states {
		items = append(items, accountJSON(state))
	}
	body["accounts"] = items

	current, okCurrent := stateOf(states, entry)
	if !okCurrent {
		// Unreachable while accounts and states come from the same listing.
		body["account"] = nil
		body["error"] = "无法定位该账号的状态记录"
		return jsonManagementResponse(http.StatusOK, body)
	}
	selected := accountJSON(current)
	body["account"] = selected
	for key, value := range selected {
		if key == "auth_index" || key == "name" || key == "status" {
			continue
		}
		body[key] = value
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// accountJSON renders one account's own figures.
//
// A failed read emits the reason and NO number, so a consumer can tell "the
// balance is zero" from "the balance could not be read".
func accountJSON(state accountState) map[string]any {
	item := map[string]any{
		"auth_index": state.Entry.AuthIndex,
		"name":       state.Entry.Name,
		"status":     statusText(state.Entry),
	}
	if label := strings.TrimSpace(state.Entry.Label); label != "" {
		item["label"] = label
	}
	if state.CredentialErr != nil {
		item["error"] = state.CredentialErr.Error()
		return item
	}
	credential := state.Credential
	item["token_masked"] = credential.maskedToken()
	item["expired"] = credential.Expired(nowTime())
	item["needs_refresh"] = credential.NeedsRefresh(nowTime(), refreshWindow(settings()))
	item["refreshable"] = credential.Refreshable()
	if expiry := credential.Expiry(); !expiry.IsZero() {
		item["expires_at"] = jsonTime(expiry)
	}
	if state.Refresh.Refreshed {
		item["refreshed"] = true
	}
	if state.Refresh.Err != nil {
		item["refresh_error"] = state.Refresh.Err.Error()
	}
	switch {
	case state.BalanceErr != nil:
		item["credit_error"] = state.BalanceErr.Error()
	case state.Balance != nil:
		item["credits"] = map[string]any{
			"total": state.Balance.Total,
			// The record count is published under its OWN name so no consumer
			// can mistake it for the balance — the mistake that once shipped.
			"records": state.Balance.Rows,
		}
	}
	switch {
	case state.SigninErr != nil:
		item["signin_error"] = state.SigninErr.Error()
	default:
		item["daily_checkin"] = map[string]any{
			"active":           state.Signin.Active,
			"today_checked_in": state.Signin.TodayCheckedIn,
			"streak_days":      state.Signin.StreakDays,
			"daily_credit":     state.Signin.DailyCredit,
			"today_credit":     state.Signin.TodayCredit,
			"claimable":        state.Signin.ClaimableExists,
		}
	}
	return item
}

// loginJSON describes the login route for scripts.
func loginJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body := map[string]any{
		"action": strings.TrimSpace(request.Query.Get("action")),
		"flow":   "device-code",
		"hint": "GET ?action=start 获取授权链接（含 user_code）；GET ?action=poll&state=<state> 轮询结果。" +
			"设备码流程不需要本地回调端口",
		"login_verified": LoginVerified,
	}
	if session, ok := currentLoginSession(); ok {
		body["state"] = session.stateValue()
		body["url"] = session.loginURL()
		body["user_code"] = session.Auth.UserCode
		body["poll_interval_seconds"] = session.currentIntervalSeconds()
		body["expires_at"] = jsonTime(session.ExpiresAt)
	}
	// A page or a script can start a flow from here, which keeps the login page
	// a pure GET with no form and no script.
	switch strings.TrimSpace(request.Query.Get("action")) {
	case "start":
		started, errStart := startLoginSession(h, settings())
		if errStart != nil {
			body["error"] = errStart.Error()
			return jsonManagementResponse(statusOf(errStart, http.StatusBadGateway), body)
		}
		body["state"] = started.stateValue()
		body["url"] = started.loginURL()
		body["user_code"] = started.Auth.UserCode
		body["poll_interval_seconds"] = started.currentIntervalSeconds()
		body["expires_at"] = jsonTime(started.ExpiresAt)
	case "poll":
		wanted := strings.TrimSpace(request.Query.Get("state"))
		session, found := lookupLoginSession(wanted)
		if !found {
			body["status"] = string(pluginapi.AuthLoginStatusError)
			body["message"] = "登录会话不存在或已超时，请重新发起登录"
			return jsonManagementResponse(http.StatusOK, body)
		}
		status, message, credential, done := session.snapshot()
		if !done {
			// One upstream attempt, subject to the same interval the ABI poll
			// honours; the page reloads itself and asks again.
			if !session.due() {
				body["status"] = string(pluginapi.AuthLoginStatusPending)
				body["message"] = "等待用户完成授权"
				return jsonManagementResponse(http.StatusOK, body)
			}
			session.markAttempt()
			decision, decisionMessage, polled, errPoll := pollDeviceToken(h, settings(), session.Auth)
			switch decision {
			case pollSuccess:
				session.resetFailures()
				session.setCredential(polled, firstNonEmpty(decisionMessage, "登录成功"))
				status, message, credential, done = session.snapshot()
			case pollPending:
				session.resetFailures()
				body["status"] = string(pluginapi.AuthLoginStatusPending)
				body["message"] = firstNonEmpty(decisionMessage, "等待用户完成授权")
				return jsonManagementResponse(http.StatusOK, body)
			case pollSlowDown:
				session.slowDown()
				body["status"] = string(pluginapi.AuthLoginStatusPending)
				body["message"] = "服务端要求放慢轮询，已把间隔调整为 " + itoaInt(session.currentIntervalSeconds()) + " 秒"
				return jsonManagementResponse(http.StatusOK, body)
			default:
				detail := firstNonEmpty(decisionMessage, "MiniMax 授权失败")
				if errPoll != nil {
					detail = errPoll.Error()
				}
				session.fail(detail)
				forgetLoginSession(wanted)
				body["status"] = string(pluginapi.AuthLoginStatusError)
				body["message"] = detail
				return jsonManagementResponse(http.StatusOK, body)
			}
		}
		if status == pluginapi.AuthLoginStatusSuccess && credential != nil {
			auth, errAuth := authDataFor(credential, "")
			if errAuth != nil {
				body["error"] = errAuth.Error()
				return jsonManagementResponse(http.StatusInternalServerError, body)
			}
			if saved, errSave := h.SaveAuth(auth.FileName, auth.StorageJSON); errSave != nil {
				body["error"] = "保存凭据失败：" + errSave.Error()
				return jsonManagementResponse(http.StatusBadGateway, body)
			} else {
				body["saved"] = saved.Name
			}
			forgetLoginSession(wanted)
			body["status"] = string(pluginapi.AuthLoginStatusSuccess)
			body["message"] = firstNonEmpty(message, "登录成功")
			body["account"] = credential.displayLabel()
			return jsonManagementResponse(http.StatusOK, body)
		}
		body["status"] = string(status)
		body["message"] = message
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// checkinResponse serves the check-in route in both representations.
func checkinResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
		}
		return pluguiPage("MiniMax 签到", checkinFailed("指定的账号不存在"))
	}
	credential, freshness, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		if wantsJSON(request) {
			return jsonManagementResponse(statusOf(errCredential, http.StatusBadRequest),
				map[string]any{"error": errCredential.Error()})
		}
		return pluguiPage("MiniMax 签到", checkinFailed(errCredential.Error()))
	}
	// A check-in signs with the same credential the page reads with: an expired
	// one that could not be renewed would only produce an upstream rejection.
	if freshness.Expired && freshness.Err != nil {
		message := "凭据已过期且自动续期失败：" + freshness.Err.Error()
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": message})
		}
		return pluguiPage("MiniMax 签到", checkinFailed(message))
	}
	outcome, errClaim := claimDailyCheckin(h, credential, settings())
	if errClaim != nil {
		// The failure's own status travels to the caller: a dead credential must
		// be a 401, not a generic 502.
		if wantsJSON(request) {
			return jsonManagementResponse(statusOf(errClaim, http.StatusBadGateway),
				map[string]any{"error": errClaim.Error()})
		}
		return pluguiPage("MiniMax 签到", checkinFailed(errClaim.Error()))
	}
	if wantsJSON(request) {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"status":  outcome.Status,
			"message": outcome.Message,
			"amount":  outcome.Amount,
		})
	}
	return pluguiPage("MiniMax 签到", renderCheckinCard(outcome))
}

// statusText renders a credential entry's state for the pages.
func statusText(entry pluginapi.HostAuthFileEntry) string {
	switch {
	case entry.Disabled:
		return "已禁用"
	case entry.Unavailable:
		return "不可用"
	case strings.TrimSpace(entry.Status) != "":
		return entry.Status
	default:
		return "正常"
	}
}
