package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Management API and CPAMP resource routes.
//
// Three mounts, all decided by the host:
//
//   - a GET route carrying a `Menu` is registered under
//     `/v0/resource/plugins/<id>/<path>` AND becomes its own sidebar entry in
//     CPA-Manager-Plus. The repository's single entry belongs to the hub plugin,
//     which is why NO route below carries a Menu.
//   - a ResourceRoute is mounted under the same prefix but is listed in the
//     sidebar only when it carries a Menu. Leaving Menu empty keeps the page
//     browser-reachable — the hub's channel overview and this plugin's status
//     page link to them — without adding a nav item.
//   - any other route is registered under `/v0/management/<path>`, a GLOBAL
//     namespace shared with every other plugin. This plugin declares none:
//     everything a script needs is `?format=json` on the resource routes.
//
// The resource mount is dispatched as GET ONLY, which is why every action on
// every page is a link carrying a query string and never a form.

// Resource paths, referenced by the pages and by the hub.
const (
	statusResourcePath  = "/status"
	loginResourcePath   = "/login"
	checkinResourcePath = "/checkin"
)

// handleManagementRegister declares the entries management clients show.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{},
		Resources: []pluginapi.ResourceRoute{
			{Path: statusResourcePath, Description: "账号、额度（token 计量）、模型目录与设置概览（由 hub 的渠道总览链接进入）"},
			{Path: loginResourcePath, Description: "ZCode 设备授权登录：先取授权 URL，再由轮询收尾；不监听本地端口"},
			{Path: checkinResourcePath, Description: "补齐活跃上报 → 查询可领额度 → 领取；ZCode 的额度按自然日由服务端结算"},
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
	case statusResourcePath:
		if wantsJSON(request) {
			return statusJSON(h, request), nil
		}
		return renderStatusPage(h, request), nil

	case loginResourcePath:
		if wantsJSON(request) {
			return loginJSON(h, request), nil
		}
		return renderLoginPage(h, request), nil

	case checkinResourcePath:
		if wantsJSON(request) {
			return checkinJSON(h, request), nil
		}
		return renderCheckinPage(h, request), nil
	}
	return jsonManagementResponse(http.StatusNotFound, map[string]any{
		"error": "unknown ZCode management route",
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

// zcodeAccounts returns the host's ZCode credentials, in host order.
func zcodeAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
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

// selectAccount resolves which credential a page acts on. Without an explicit
// selector the first account is used, so the page works with no parameters.
func selectAccount(h *abiboot.Host, request pluginapi.ManagementRequest) (pluginapi.HostAuthFileEntry, bool) {
	wanted := strings.TrimSpace(request.Query.Get("auth_index"))
	if wanted == "" {
		wanted = strings.TrimSpace(request.Query.Get("auth_id"))
	}
	for _, entry := range zcodeAccounts(h) {
		if wanted == "" {
			return entry, true
		}
		if entry.AuthIndex == wanted || entry.ID == wanted || entry.Name == wanted {
			return entry, true
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

// credentialOf loads one account's credential.
//
// There is no renewal to consider: ZCode credentials are static and carry no
// local expiry, which is why the reference's `isZcodeExpired` returns false
// unconditionally.
func credentialOf(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) (*Credential, error) {
	if h == nil || strings.TrimSpace(entry.AuthIndex) == "" {
		return nil, statusError(false, "missing_auth", http.StatusBadRequest,
			"账号 %s 缺少运行时索引", entry.Name)
	}
	auth, errGet := h.GetAuth(entry.AuthIndex)
	if errGet != nil {
		return nil, errGet
	}
	return ParseCredential(auth.JSON)
}

// accountStatus is one account's row in the status payload.
//
// A failed read is reported as an error on the entry and never as a zero: a
// zeroed token count reads as "used up", which is the opposite of "unknown".
type accountStatus struct {
	Entry      pluginapi.HostAuthFileEntry
	Credential *Credential
	Error      string
	Balance    *balanceResult
	BalanceErr string
	Checkin    *checkinStatus
	CheckinErr string
}

// collectAccountStatuses reads every account's balance once.
func collectAccountStatuses(h *abiboot.Host, accounts []pluginapi.HostAuthFileEntry, cfg Config) []accountStatus {
	out := make([]accountStatus, 0, len(accounts))
	for _, entry := range accounts {
		status := accountStatus{Entry: entry}
		credential, errCredential := credentialOf(h, entry)
		if errCredential != nil {
			status.Error = errCredential.Error()
			out = append(out, status)
			continue
		}
		status.Credential = credential
		balance, errBalance := fetchBalance(h, credential, cfg)
		if errBalance != nil {
			status.BalanceErr = errBalance.Error()
		} else {
			status.Balance = balance
		}
		checkin, errCheckin := fetchCheckinStatus(h, credential, cfg)
		if errCheckin != nil {
			status.CheckinErr = errCheckin.Error()
		} else {
			status.Checkin = &checkin
		}
		out = append(out, status)
	}
	return out
}

// statusJSON is the machine-readable status payload behind `?format=json`.
//
// The document reports EVERY account this plugin owns: `accounts` is an array,
// one entry per account in host order, each with that account's OWN balance.
// `account_count` carries the number, and the selected account's fields stay at
// the top level for consumers that read them there.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	accounts := zcodeAccounts(h)
	_, found := selectAccount(h, request)
	statuses := collectAccountStatuses(h, accounts, cfg)

	rows := make([]map[string]any, 0, len(statuses))
	for _, status := range statuses {
		rows = append(rows, accountStatusJSON(status))
	}

	body := map[string]any{
		"provider":        ProviderKey,
		"display_name":    DisplayName,
		"account_count":   len(accounts),
		"accounts":        rows,
		"selected_found":  found,
		"model_count":     len(currentCatalogue()),
		"models":          catalogueIDs(currentCatalogue()),
		"endpoint":        Origin + MessagesPath,
		"protocol":        "anthropic-messages",
		"inference_paths": []string{MessagesPath},
		"login_page":      loginResourcePath,
		"checkin_page":    checkinResourcePath,
		"refreshable":     false,
		"refresh_note": "ZCode 凭据是静态的：JWT 里没有 exp，服务端也没有续期端点，" +
			"auth.refresh 只做有效性探测（billing/balance）。失效只能重新登录",
		"daily_checkin": true,
		"checkin_note": "每日额度按自然日由服务端结算；领取前必须先补 app_launch / app_daily_active 活跃上报，" +
			"否则 preview 恒为空",
		"captcha": map[string]any{
			"used_for_inference": false,
			"note": "实测：身份块正确时，不带任何 captcha 头也返回 200；带一个故意伪造的 captcha 参数同样 200。" +
				"官方客户端自身的产物里，captcha 头只剩 billing/claim 一处，且其请求模型带 " +
				"reason:\"model-request\"|\"captcha-retry\"，即 captcha 已变成被动重试而非前置条件。" +
				"本插件因此不产出 captcha，claim 也按无 captcha 实现",
		},
		"identity_block": map[string]any{
			"cli_prefix_chars": len(officialCLIPrefix),
			"stable_chars":     len(officialStableSections()),
			"total_chars":      len(officialCLIPrefix) + len(officialStableSections()),
			"note": "system 必须是「多个独立文本块」，且顺序为 cliPrefix → stable → # Environment → 调用方 system。" +
				"同样的字符数拼成单块仍会得到 3012",
			"penalty": "3012 有账号冷却惩罚：30 分钟；24 小时内第 3 次起 24 小时；第 5 次停用账号。" +
				"本插件从不自动重试 3012",
		},
		"settings": map[string]any{
			"app_version":              cfg.AppVersion,
			"platform":                 cfg.Platform,
			"os_category":              cfg.OSGroup,
			"model_prefix":             cfg.ModelPrefix,
			"discover_models":          cfg.DiscoverModels,
			"default_max_tokens":       cfg.DefaultMaxTokens,
			"import_client_credential": cfg.ImportClientCredential,
		},
	}
	if len(statuses) > 0 {
		body["selected"] = accountStatusJSON(statuses[0])
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// accountStatusJSON renders one account row.
func accountStatusJSON(status accountStatus) map[string]any {
	row := map[string]any{
		"name":       status.Entry.Name,
		"auth_index": status.Entry.AuthIndex,
		"status":     status.Entry.Status,
		"disabled":   status.Entry.Disabled,
	}
	if status.Error != "" {
		row["credential_error"] = status.Error
		return row
	}
	row["label"] = status.Credential.displayLabel()
	row["user_id"] = status.Credential.UserID
	row["source"] = status.Credential.Source
	row["app_version"] = status.Credential.AppVersion
	// ⚠ `device_mid` is reported as a DEVICE id and is explicitly not an account
	// identifier: the plugin regenerates it on every login.
	row["device_mid"] = status.Credential.DeviceMid
	row["identity_note"] = "user_id 才是账号标识；device_mid 每次登录都会变，不要用它去重"
	if status.BalanceErr != "" {
		row["balance_error"] = status.BalanceErr
	}
	if status.Balance != nil {
		balance := status.Balance
		row["enterprise"] = balance.Enterprise
		// Tokens, not credits: the unit field is upstream's own.
		row["unit_type"] = balanceUnit(balance)
		row["remaining_units"] = balance.Remaining
		row["total_units"] = balance.Total
		row["remaining_tokens"] = formatTokenMagnitude(balance.Remaining)
		row["total_tokens"] = formatTokenMagnitude(balance.Total)
		row["plan_name"] = balance.PlanName
		if balance.ExpiresAt > 0 {
			row["expires_at"] = unixSecondsToRFC3339(balance.ExpiresAt)
		}
		buckets := make([]map[string]any, 0, len(balance.Buckets))
		for _, bucket := range balance.Buckets {
			buckets = append(buckets, map[string]any{
				"plan_id":         bucket.PlanID,
				"show_name":       bucket.ShowName,
				"unit_type":       bucket.UnitType,
				"meter":           bucket.Meter,
				"total_units":     bucket.Total,
				"used_units":      bucket.Used,
				"remaining_units": bucket.Remaining,
				"available_units": bucket.Available,
				"expires_at":      unixSecondsToRFC3339(bucket.ExpiresAt),
			})
		}
		row["buckets"] = buckets
	}
	if status.CheckinErr != "" {
		row["checkin_error"] = status.CheckinErr
	}
	if status.Checkin != nil {
		row["today_checked_in"] = status.Checkin.TodayCheckedIn
		row["claimable_plans"] = len(status.Checkin.Claimable)
		row["checkin_note"] = status.Checkin.Note
	}
	return row
}

// balanceUnit renders the unit label from the bucket's own field.
func balanceUnit(balance *balanceResult) string {
	if balance == nil || len(balance.Buckets) == 0 || balance.Buckets[0].UnitType == "" {
		return "token"
	}
	return balance.Buckets[0].UnitType
}

// catalogueIDs lists a catalogue's model ids.
func catalogueIDs(catalogue []fallbackModel) []string {
	out := make([]string, 0, len(catalogue))
	for _, model := range catalogue {
		out = append(out, model.ID)
	}
	return out
}

// loginJSON is the machine-readable view of the login page.
//
// A GET resource route cannot start a flow on its own — doing so on every page
// load would mint an authorization request per refresh — so the JSON view reports
// the flow's shape and the caller starts one explicitly through `auth.login.start`
// or the page's action link.
func loginJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	payload := map[string]any{
		"provider":            ProviderKey,
		"init_endpoint":       Origin + OAuthCLIInitPath,
		"poll_endpoint":       Origin + OAuthCLIPollPrefix + "<flow_id>",
		"login_provider":      loginProvider,
		"callback_port":       nil,
		"uses_local_callback": false,
		"hint":                "auth.login.start 立即返回 authorize_url，auth.login.poll 每次只做一次上游轮询并回报 pending/success/failure",
		"device_mid_note": "X-Device-Mid 是硬需求（缺失 → 400 code 3001），但它的值不被服务端校验；" +
			"本插件在登录时自行生成并随凭据持久化",
		"poll_interval_ms": cfg.PollIntervalMS,
	}
	if wantsJSON(request) && strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "start") {
		// An explicit action may start a flow, because the caller asked for it.
		value, errStart := handleAuthLoginStart(h, nil)
		if errStart != nil {
			payload["start_error"] = errStart.Error()
			return jsonManagementResponse(http.StatusOK, payload)
		}
		started, ok := value.(pluginapi.AuthLoginStartResponse)
		if ok {
			payload["authorize_url"] = started.URL
			payload["state"] = started.State
			payload["expires_at"] = jsonTime(started.ExpiresAt)
		}
	}
	return jsonManagementResponse(http.StatusOK, payload)
}

// checkinJSON is the machine-readable view of the check-in page.
//
// `?action=claim` performs the claim, because a resource route is GET-only and an
// explicit action link is the only way a user can trigger it. A plain page load
// never claims anything.
func checkinJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"provider": ProviderKey,
			"error":    "没有可用的 ZCode 账号，请先登录",
		})
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"provider": ProviderKey,
			"account":  entry.Name,
			"error":    errCredential.Error(),
		})
	}

	payload := map[string]any{"provider": ProviderKey, "account": entry.Name}
	claimAttempted := strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "claim")
	var outcomes []claimOutcome
	if claimAttempted {
		claimed, errClaim := claimDaily(h, credential, cfg)
		outcomes = claimed
		if errClaim != nil {
			payload["claim_error"] = errClaim.Error()
		} else {
			rows := make([]map[string]any, 0, len(outcomes))
			for _, outcome := range outcomes {
				rows = append(rows, map[string]any{
					"plan_id":         outcome.PlanID,
					"ok":              outcome.OK,
					"code":            outcome.Code,
					"already_claimed": outcome.AlreadyClaimed,
					"http_status":     outcome.HTTPStatus,
					"message":         outcome.Message,
				})
			}
			payload["outcomes"] = rows
		}
	}
	// The hub renders a check-in row from the TOP-LEVEL `message` and nothing
	// else (`plugins/hub/orchestrator.go`, interpretCheckinJSON). Without one,
	// a failed claim reaches the user as the bare word "failed" while the
	// actionable sentence sits unread in `outcomes[].message` — which is exactly
	// how a 3007 captcha rejection looked like a mystery for a whole run.
	if claimAttempted {
		if summary := checkinSummary(outcomes, payload["claim_error"]); summary != "" {
			payload["message"] = summary
		}
	}

	status, errStatus := fetchCheckinStatus(h, credential, cfg)
	if errStatus != nil {
		payload["status_error"] = errStatus.Error()
		// The claim result is known even when the read-back failed, so the
		// verdict is still published rather than withheld.
		payload["status"] = checkinStatusWord(claimAttempted, outcomes, false, 0)
		return jsonManagementResponse(http.StatusOK, payload)
	}
	payload["active"] = status.Active
	payload["today_checked_in"] = status.TodayCheckedIn
	payload["activity_name"] = status.ActivityName
	payload["claimable_plans"] = len(status.Claimable)
	payload["note"] = status.Note
	payload["status"] = checkinStatusWord(claimAttempted, outcomes, status.TodayCheckedIn, len(status.Claimable))
	return jsonManagementResponse(http.StatusOK, payload)
}

// checkinStatusWord reduces this page's result to the shared check-in
// vocabulary, which is what the Jet Hub panel reads to label the row
// (`plugins/hub/orchestrator.go`, interpretCheckinJSON → normalizeProviderStatus):
//
//	claimed          a plan was granted by this run
//	already-claimed  nothing was granted because today's grant is already taken
//	inactive         no claim was attempted and nothing is claimable
//	failed           a claim was attempted and nothing succeeded
//
// ⚠ It never upgrades an attempt to `claimed`. A run whose claim errored, or
// that came back with a non-success business code, stays `failed`; the panel
// prints an unrecognised word verbatim rather than assuming success, so a wrong
// word here would surface as a false 签到成功.
//
// `claimOutcome.OK` is true for BOTH a fresh grant and the idempotent 1003, so
// the already-claimed test has to come first to tell them apart.
func checkinStatusWord(attempted bool, outcomes []claimOutcome, todayCheckedIn bool, claimable int) string {
	if attempted {
		granted, already := false, false
		for _, outcome := range outcomes {
			if outcome.AlreadyClaimed {
				already = true
				continue
			}
			if outcome.OK {
				granted = true
			}
		}
		switch {
		case granted:
			return "claimed"
		case already:
			return "already-claimed"
		default:
			return "failed"
		}
	}
	if todayCheckedIn {
		return "already-claimed"
	}
	if claimable > 0 {
		// Claimable but not attempted. "claimable" is not in the panel's shared
		// vocabulary on purpose: the panel only classifies runs it drove, and a
		// word it does not know is printed as-is instead of being mapped onto a
		// verdict this page has not earned.
		return "claimable"
	}
	return "inactive"
}

// checkinSummary turns the per-plan outcomes into the one sentence the hub shows.
//
// Priority is deliberate: a failure outranks a success, because a run where one
// plan was claimed and another was refused is not "done" — the refusal is the
// part the user has to act on.
func checkinSummary(outcomes []claimOutcome, errClaim any) string {
	if errClaim != nil {
		if text, ok := errClaim.(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	if len(outcomes) == 0 {
		return ""
	}
	claimed, already := 0, 0
	firstFailure := ""
	for _, outcome := range outcomes {
		switch {
		case !outcome.OK:
			if firstFailure == "" {
				firstFailure = outcome.Message
			}
		case outcome.AlreadyClaimed:
			already++
		default:
			claimed++
		}
	}
	switch {
	case firstFailure != "":
		return firstFailure
	case claimed > 0:
		return "签到成功：" + strconv.Itoa(claimed) + " 个额度已领取"
	case already > 0:
		return "今日已签到：" + strconv.Itoa(already) + " 个额度无需重复领取"
	default:
		return ""
	}
}
