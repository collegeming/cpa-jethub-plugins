package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file renders the management pages a user sees inside CPA-Manager-Plus.
//
// The host exposes every management route carrying a Menu on
// `/v0/resource/plugins/trae/<path>` and CPAMP renders it in a same-origin iframe
// with the host theme injected as CSS custom properties. Two consequences shape
// everything here: those routes are dispatched as GET only, so every action is a
// link with a query string, and the markup only consumes host CSS variables, so
// there is no frontend build and both themes work.

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

// traeAccounts returns the host's TRAE credentials, in host order.
func traeAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
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
	for _, entry := range traeAccounts(h) {
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
		return nil, abiboot.Errorf("missing_auth", "账号 %s 缺少运行时索引", entry.Name)
	}
	auth, errGet := h.GetAuth(entry.AuthIndex)
	if errGet != nil {
		return nil, errGet
	}
	return ParseCredential(auth.JSON)
}

// catalogSummary describes the account's catalog for the status page.
type catalogSummary struct {
	Discovered  bool
	ModelCount  int
	ChannelHits map[string]int
	ImageCount  int
	MaxModeHits int
	Error       string
}

// summariseCatalog collects the catalog counters shown on the status page.
func summariseCatalog(h *abiboot.Host, credential *Credential, cfg Config) catalogSummary {
	summary := catalogSummary{ChannelHits: map[string]int{}}
	if !cfg.DiscoverModels {
		for _, info := range staticModelInfos(cfg) {
			_ = info
			summary.ModelCount++
		}
		return summary
	}
	entry, errCatalog := catalogFor(h, credential, cfg)
	if errCatalog != nil {
		summary.Error = errCatalog.Error()
		summary.ModelCount = len(staticModelInfos(cfg))
		return summary
	}
	summary.Discovered = true
	for _, model := range entry.models {
		if !isModelCallable(model) {
			continue
		}
		summary.ModelCount++
		if model.Channel != "" {
			summary.ChannelHits[model.Channel]++
		}
		if modelSupportsImage(model) {
			summary.ImageCount++
		}
		if maxModeFor(model, cfg) {
			summary.MaxModeHits++
		}
	}
	return summary
}

// renderStatusPage renders the account overview, the catalog summary and the
// credit/check-in state. A `?action=checkin` link performs the check-in first.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	product := productFor(cfg.Region)
	accounts := traeAccounts(h)
	if len(accounts) == 0 {
		return plugui.HTML("TRAE 状态",
			plugui.Card("尚未添加账号",
				plugui.Group(
					plugui.Notice("warning", "当前实例还没有 TRAE 账号。请先完成一次浏览器登录授权。"),
					plugui.Fields(
						plugui.Field{Label: "区域", Value: cfg.Region + "（" + product.Site + "）"},
						plugui.Field{Label: "对话端点", Value: product.AgentHost},
					),
				),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
			),
		)
	}

	entry, found := selectAccount(h, request)
	if !found {
		return plugui.HTML("TRAE 状态",
			plugui.Card("账号不存在", plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。"),
				plugui.Action{Label: "返回第一个账号", Path: "status", Kind: "primary"}))
	}

	body := make([]template.HTML, 0, 5)
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "checkin") {
		body = append(body, renderCheckinOutcome(h, entry))
	}
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "refresh") {
		if credential, errCredential := credentialOf(h, entry); errCredential == nil {
			invalidateCatalog(cfg, credential)
			body = append(body, plugui.Notice("success", "已清除模型目录缓存，重新拉取。"))
		}
	}

	accountFields := []plugui.Field{
		{Label: "名称", Value: entry.Name},
		{Label: "状态", Value: statusText(entry)},
	}
	if entry.AuthIndex != "" {
		accountFields = append(accountFields, plugui.Field{Label: "索引", Value: entry.AuthIndex})
	}
	catalogFields := []plugui.Field{
		{Label: "区域", Value: cfg.Region},
		{Label: "配置通道", Value: strings.Join(cfg.Channels, ", ")},
		{Label: "默认通道", Value: cfg.DefaultChannel},
	}
	creditFields := []plugui.Field{}
	warnings := []template.HTML{}

	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		catalogFields = append(catalogFields, plugui.Field{Label: "凭据", Value: "无法读取：" + errCredential.Error()})
	} else {
		accountFields = append(accountFields,
			plugui.Field{Label: "用户", Value: emptyFallback(credential.Nickname, credential.UID)},
			plugui.Field{Label: "uid", Value: credential.UID},
			plugui.Field{Label: "凭据有效期", Value: formatExpiry(credential.Expiry())},
			plugui.Field{Label: "可自动续期", Value: yesNo(credential.Refreshable())},
			plugui.Field{Label: "machine_id", Value: shorten(credential.MachineID)},
			plugui.Field{Label: "device_id", Value: shorten(credential.DeviceID)},
		)
		if credential.Region != "" && credential.Region != cfg.Region {
			warnings = append(warnings, plugui.Notice("warning",
				fmt.Sprintf("该凭据来自 %s，但当前插件区域配置为 %s；两侧端点与登录态互不相通，请改配置或重新登录。",
					credential.Region, cfg.Region)))
		}
		if credential.Expired() {
			warnings = append(warnings, plugui.Notice("danger", "凭据已过期，请重新登录或等待自动续期。"))
		}
		if !credential.Refreshable() {
			warnings = append(warnings, plugui.Notice("warning", "该凭据没有 refresh_token，无法静默续期。"))
		}

		if summary := summariseCatalog(h, credential, cfg); summary.Error != "" {
			catalogFields = append(catalogFields,
				plugui.Field{Label: "模型目录", Value: "远端不可用，使用内置兜底列表（" + summary.Error + "）"},
				plugui.Field{Label: "兜底模型数", Value: fmt.Sprintf("%d", summary.ModelCount)},
			)
		} else {
			catalogFields = append(catalogFields,
				plugui.Field{Label: "可调用模型数", Value: fmt.Sprintf("%d", summary.ModelCount)},
				plugui.Field{Label: "支持图片的模型数", Value: fmt.Sprintf("%d", summary.ImageCount)},
				plugui.Field{Label: "可启用 Max 模式的模型数", Value: fmt.Sprintf("%d", summary.MaxModeHits)},
			)
			if len(summary.ChannelHits) > 0 {
				catalogFields = append(catalogFields, plugui.Field{Label: "各通道模型数", Value: formatChannelHits(summary.ChannelHits)})
			}
			if !summary.Discovered {
				catalogFields = append(catalogFields, plugui.Field{Label: "目录来源", Value: "内置兜底列表（discover_models 已关闭）"})
			}
		}

		if balance, errBalance := fetchCreditBalance(h, credential, cfg); errBalance != nil {
			creditFields = append(creditFields, plugui.Field{Label: "积分余额", Value: "查询失败：" + errBalance.Error()})
		} else {
			creditFields = append(creditFields, plugui.Field{Label: "剩余积分", Value: fmt.Sprintf("%.0f", balance.Total)})
			for _, pack := range balance.Packages {
				creditFields = append(creditFields,
					plugui.Field{Label: pack.Name, Value: fmt.Sprintf("剩余 %.0f / 共 %.0f（已用 %.0f）", pack.Remaining, pack.Total, pack.Used)})
			}
		}
		if status, errStatus := fetchCheckinStatus(h, credential, cfg); errStatus != nil {
			creditFields = append(creditFields, plugui.Field{Label: "签到状态", Value: "查询失败：" + errStatus.Error()})
		} else {
			creditFields = append(creditFields,
				plugui.Field{Label: "今日已签到", Value: yesNo(status.CheckedIn)},
				plugui.Field{Label: "签到奖励", Value: fmt.Sprintf("%d", status.Credits)},
				plugui.Field{Label: "连续签到", Value: fmt.Sprintf("%d 天", status.StreakDays)},
			)
		}
	}

	body = append(body, warnings...)
	body = append(body, plugui.Card("账号", plugui.Fields(accountFields...),
		plugui.Action{Label: "签到", Query: "action=checkin", Kind: "primary"},
		plugui.Action{Label: "刷新模型目录", Query: "action=refresh"},
		plugui.Action{Label: "重新登录", Path: "login"},
	))
	body = append(body, plugui.Card("通道与模型", plugui.Fields(catalogFields...)))
	if len(creditFields) > 0 {
		body = append(body, plugui.Card("积分与签到", plugui.Fields(creditFields...)))
	}
	if switcher := renderAccountList(accounts, entry.AuthIndex); switcher != "" {
		body = append(body, switcher)
	}
	return plugui.HTML("TRAE 状态", body...)
}

// renderAccountList renders the switcher across accounts.
func renderAccountList(accounts []pluginapi.HostAuthFileEntry, current string) template.HTML {
	if len(accounts) < 2 {
		return ""
	}
	fields := make([]plugui.Field, 0, len(accounts))
	for _, entry := range accounts {
		marker := ""
		if entry.AuthIndex == current {
			marker = "（当前）"
		}
		fields = append(fields, plugui.Field{Label: entry.Name + marker, Value: statusText(entry)})
	}
	return plugui.Card("全部账号（在地址后追加 ?auth_index=<索引> 可切换）", plugui.Fields(fields...))
}

// renderCheckinOutcome performs the daily check-in and renders its result.
func renderCheckinOutcome(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) template.HTML {
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return plugui.Notice("danger", "无法读取凭据："+errCredential.Error())
	}
	outcome, errClaim := claimDailyCheckin(h, credential, settings())
	if errClaim != nil {
		return plugui.Notice("danger", "签到失败："+errClaim.Error())
	}
	return checkinNotice(outcome)
}

// checkinNotice renders one check-in outcome. The state comes from the response
// body, never from the HTTP status: a repeated claim also answers success
// upstream, so only the body tells the truth.
func checkinNotice(outcome *traeClaimOutcome) template.HTML {
	message := outcome.Message
	if outcome.Credit > 0 {
		message = fmt.Sprintf("%s，本次获得 %d 积分", message, outcome.Credit)
	}
	switch outcome.Status {
	case "claimed":
		return plugui.Notice("success", message)
	case "already-claimed":
		return plugui.Notice("", message)
	case "inactive":
		return plugui.Notice("warning", message)
	default:
		return plugui.Notice("danger", message)
	}
}

// formatChannelHits renders the per-channel model counts deterministically.
func formatChannelHits(hits map[string]int) string {
	channels := make([]string, 0, len(hits))
	for channel := range hits {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	parts := make([]string, 0, len(channels))
	for _, channel := range channels {
		parts = append(parts, fmt.Sprintf("%s=%d", channel, hits[channel]))
	}
	return strings.Join(parts, ", ")
}

// statusJSON is the machine-readable form of the status page.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	product := productFor(cfg.Region)
	accounts := traeAccounts(h)
	body := map[string]any{
		"region":        cfg.Region,
		"site":          product.Site,
		"agent_host":    product.AgentHost,
		"ug_host":       product.UGHost,
		"oauth_host":    product.OAuthHost,
		"channels":      cfg.Channels,
		"max_mode":      cfg.MaxMode,
		"account_count": len(accounts),
	}
	if len(accounts) == 0 {
		body["accounts"] = []any{}
		return jsonManagementResponse(http.StatusOK, body)
	}
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errCredential.Error()})
	}
	summary := summariseCatalog(h, credential, cfg)
	account := map[string]any{
		"auth_index":      entry.AuthIndex,
		"name":            entry.Name,
		"uid":             credential.UID,
		"nickname":        credential.Nickname,
		"region":          credential.Region,
		"refreshable":     credential.Refreshable(),
		"expired":         credential.Expired(),
		"models":          summary.ModelCount,
		"image_models":    summary.ImageCount,
		"max_mode_models": summary.MaxModeHits,
		"channels":        summary.ChannelHits,
	}
	if expiresAt, ok := credential.ExpiresAtMS(); ok {
		account["expires_at_ms"] = expiresAt
	}
	if summary.Error != "" {
		account["catalog_error"] = summary.Error
	}
	body["accounts"] = []any{account}
	if balance, errBalance := fetchCreditBalance(h, credential, cfg); errBalance == nil {
		body["credits"] = balance.Total
	}
	if status, errStatus := fetchCheckinStatus(h, credential, cfg); errStatus == nil {
		body["daily_checkin"] = map[string]any{
			"checked_in":  status.CheckedIn,
			"credits":     status.Credits,
			"streak_days": status.StreakDays,
		}
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// checkinResponse serves the check-in route in both representations.
func checkinResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	entry, found := selectAccount(h, request)
	if !found {
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
		}
		return plugui.HTML("TRAE 签到",
			plugui.Card("签到失败", plugui.Notice("danger", "指定的账号不存在"),
				plugui.Action{Label: "返回状态", Path: "status"}))
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errCredential.Error()})
		}
		return plugui.HTML("TRAE 签到",
			plugui.Card("签到失败", plugui.Notice("danger", errCredential.Error()),
				plugui.Action{Label: "返回状态", Path: "status"}))
	}

	status, errStatus := fetchCheckinStatus(h, credential, cfg)
	var balance *traeCreditBalance
	if fetched, errBalance := fetchCreditBalance(h, credential, cfg); errBalance == nil {
		balance = fetched
	}

	// The link performs the claim; a plain visit only reports the state.
	var outcome *traeClaimOutcome
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "claim") {
		claimed, errClaim := claimDailyCheckin(h, credential, cfg)
		if errClaim != nil {
			if wantsJSON(request) {
				return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": errClaim.Error()})
			}
			return plugui.HTML("TRAE 签到",
				plugui.Card("签到失败", plugui.Notice("danger", errClaim.Error()),
					plugui.Action{Label: "返回状态", Path: "status"}))
		}
		outcome = claimed
		status, errStatus = fetchCheckinStatus(h, credential, cfg)
	}

	if wantsJSON(request) {
		body := map[string]any{
			"auth_index": entry.AuthIndex,
			"name":       entry.Name,
		}
		if errStatus != nil {
			body["status_error"] = errStatus.Error()
		} else if status != nil {
			body["checked_in"] = status.CheckedIn
			body["credits"] = status.Credits
			body["streak_days"] = status.StreakDays
			body["active"] = status.Active
		}
		if balance != nil {
			body["credit_remaining"] = balance.Total
		}
		if outcome != nil {
			body["claim"] = map[string]any{
				"status":       outcome.Status,
				"message":      outcome.Message,
				"credit":       outcome.Credit,
				"streak_days":  outcome.StreakDays,
				"error_type":   outcome.ErrorType,
				"cooldown_sec": outcome.CooldownSec,
			}
		}
		return jsonManagementResponse(http.StatusOK, body)
	}

	fields := []plugui.Field{{Label: "账号", Value: entry.Name}}
	if errStatus != nil {
		fields = append(fields, plugui.Field{Label: "签到状态", Value: "查询失败：" + errStatus.Error()})
	} else if status != nil {
		fields = append(fields,
			plugui.Field{Label: "活动可用", Value: yesNo(status.Active)},
			plugui.Field{Label: "今日已签到", Value: yesNo(status.CheckedIn)},
			plugui.Field{Label: "签到奖励", Value: fmt.Sprintf("%d", status.Credits)},
			plugui.Field{Label: "连续签到", Value: fmt.Sprintf("%d 天", status.StreakDays)},
		)
	}
	if balance != nil {
		fields = append(fields, plugui.Field{Label: "剩余积分", Value: fmt.Sprintf("%.0f", balance.Total)})
	}

	body := []template.HTML{}
	if outcome != nil {
		body = append(body, checkinNotice(outcome))
	}
	body = append(body, plugui.Fields(fields...))
	actions := []plugui.Action{
		{Label: "领取今日积分", Query: "action=claim", Kind: "primary"},
		{Label: "刷新", Path: "checkin"},
		{Label: "返回状态", Path: "status"},
	}
	return plugui.HTML("TRAE 签到", plugui.Card("签到与积分", plugui.Group(body...), actions...))
}

// renderLoginPage renders the two-step browser login: the first request hands the
// user an authorization URL, later requests poll for the result. Nothing blocks
// waiting for the user to finish (the browser gesture would expire).
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "start", "login":
		return startLoginPage()
	case "poll":
		return pollLoginPage(h, request)
	}
	cfg := settings()
	product := productFor(cfg.Region)
	return plugui.HTML("TRAE 登录",
		plugui.Card("浏览器登录",
			plugui.Group(
				plugui.Notice("", "点击下面的按钮获取授权链接，在浏览器里完成授权后回到本页检查结果。"),
				plugui.Fields(
					plugui.Field{Label: "区域", Value: cfg.Region + "（" + product.Site + "）"},
					plugui.Field{Label: "登录门户", Value: product.ConsoleHost + "/authorization"},
					plugui.Field{Label: "回调地址", Value: fmt.Sprintf("http://127.0.0.1:<实际端口>%s", CallbackPath)},
					plugui.Field{Label: "首选端口", Value: fmt.Sprintf("%d", cfg.CallbackPort)},
				),
			),
			plugui.Action{Label: "开始登录", Query: "action=start", Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// startLoginPage begins a login session and shows the authorization URL.
func startLoginPage() pluginapi.ManagementResponse {
	cfg := settings()
	session, errStart := startLoginSession(cfg)
	if errStart != nil {
		return plugui.HTML("TRAE 登录",
			plugui.Card("无法发起登录", plugui.Notice("danger", errStart.Error()),
				plugui.Action{Label: "重试", Query: "action=start", Kind: "primary"},
				plugui.Action{Label: "返回状态", Path: "status"}))
	}
	ttl := time.Until(session.ExpiresAt).Truncate(time.Second)
	return plugui.HTML("TRAE 登录",
		plugui.Card("在浏览器中完成授权",
			plugui.Group(
				plugui.Notice("", fmt.Sprintf("已在本地端口 %d 监听回调。复制下面的授权链接到浏览器打开，完成授权后点击「检查登录结果」。", session.Port)),
				plugui.Fields(
					plugui.Field{Label: "授权链接", Value: session.LoginURL},
					plugui.Field{Label: "回调地址", Value: session.RedirectURI()},
					plugui.Field{Label: "login_trace_id", Value: machineTraceID(session.MachineID, session.DeviceID)},
					plugui.Field{Label: "有效期", Value: ttl.String()},
				),
			),
			plugui.Action{Label: "检查登录结果", Query: "action=poll&state=" + session.State, Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// pollLoginPage polls a login session, persisting the credential on success.
func pollLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	state := strings.TrimSpace(request.Query.Get("state"))
	response, errPoll := pollLoginForManagement(h, state)
	if errPoll != nil {
		return plugui.HTML("TRAE 登录",
			plugui.Card("检查失败", plugui.Notice("danger", errPoll.Error()),
				plugui.Action{Label: "重新登录", Query: "action=start", Kind: "primary"}))
	}

	switch response.Status {
	case pluginapi.AuthLoginStatusSuccess:
		message := response.Message
		if message == "" {
			message = "登录成功，账号已保存。"
		}
		return plugui.HTML("TRAE 登录",
			plugui.Card("登录成功", plugui.Notice("success", message),
				plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"}))
	case pluginapi.AuthLoginStatusPending, "":
		return plugui.HTML("TRAE 登录",
			plugui.Card("等待授权",
				plugui.Notice("", "还没有收到授权回调。请先在浏览器里完成授权，然后再次检查。"),
				plugui.Action{Label: "再次检查", Query: "action=poll&state=" + state, Kind: "primary"},
				plugui.Action{Label: "重新开始", Query: "action=start"}))
	default:
		message := response.Message
		if message == "" {
			message = "登录失败。"
		}
		return plugui.HTML("TRAE 登录",
			plugui.Card("登录失败", plugui.Notice("danger", message),
				plugui.Action{Label: "重新登录", Query: "action=start", Kind: "primary"},
				plugui.Action{Label: "返回状态", Path: "status"}))
	}
}

// pollLoginForManagement reuses the auth.login.poll handler and then persists the
// credential itself: driving the poll from this page bypasses the host's own
// save.
func pollLoginForManagement(h *abiboot.Host, state string) (pluginapi.AuthLoginPollResponse, error) {
	var empty pluginapi.AuthLoginPollResponse
	if state == "" {
		return empty, abiboot.Errorf("missing_state", "缺少 state 参数，请重新发起登录")
	}
	rawRequest, errMarshal := json.Marshal(pluginapi.AuthLoginPollRequest{State: state})
	if errMarshal != nil {
		return empty, abiboot.Errorf("encode_poll", "encode poll request: %v", errMarshal)
	}
	value, errPoll := handleAuthLoginPoll(h, rawRequest)
	if errPoll != nil {
		return empty, errPoll
	}
	response, ok := value.(pluginapi.AuthLoginPollResponse)
	if !ok {
		return empty, abiboot.Errorf("unexpected_poll", "poll handler returned %T", value)
	}
	if response.Status != pluginapi.AuthLoginStatusSuccess || len(response.Auth.StorageJSON) == 0 {
		return response, nil
	}
	name := strings.TrimSpace(response.Auth.FileName)
	if name == "" {
		if credential, errParse := ParseCredential(response.Auth.StorageJSON); errParse == nil {
			name = defaultAuthFileName(credential)
		} else {
			name = ProviderKey + "-" + time.Now().Format("20060102150405") + ".json"
		}
	}
	if _, errSave := h.SaveAuth(name, response.Auth.StorageJSON); errSave != nil {
		return empty, abiboot.Errorf("save_auth", "保存凭据失败：%v", errSave)
	}
	forgetLoginSession(state)
	return response, nil
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

// formatExpiry renders an expiry with its remaining time.
func formatExpiry(expiry time.Time) string {
	if expiry.IsZero() {
		return "未知（凭据未声明过期时间）"
	}
	remaining := time.Until(expiry)
	if remaining <= 0 {
		return expiry.Local().Format("2006-01-02 15:04") + "（已过期）"
	}
	return fmt.Sprintf("%s（剩余 %s）", expiry.Local().Format("2006-01-02 15:04"), remaining.Truncate(time.Minute))
}

// yesNo renders a boolean in Chinese.
func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// emptyFallback returns the first non-empty value.
func emptyFallback(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "-"
}

// shorten abbreviates a device fingerprint for display while keeping it
// recognisable.
func shorten(value string) string {
	if value == "" {
		return "-"
	}
	if len(value) <= 12 {
		return value
	}
	return value[:6] + "…" + value[len(value)-6:]
}
