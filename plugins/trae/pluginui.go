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
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
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
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return nil, authrefresh.Result{}, abiboot.Errorf("missing_auth", "账号 %s 缺少运行时索引", entry.Name)
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
		if credential, _, errCredential := credentialOf(h, entry); errCredential == nil {
			invalidateCatalog(cfg, credential)
			body = append(body, plugui.Notice("success", "已清除模型目录缓存，重新拉取。"))
		}
	}

	accountFields := []plugui.Field{}
	warnings := []template.HTML{}
	catalogFields := []plugui.Field{
		{Label: "区域", Value: cfg.Region},
		{Label: "配置通道", Value: strings.Join(cfg.Channels, ", ")},
		{Label: "默认通道", Value: cfg.DefaultChannel},
	}
	credential, freshness, errCredential := credentialOf(h, entry)
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
		// The renewal this page view just performed is stated, so a figure that
		// only exists because the credential was renewed is never a silent
		// surprise — and a renewal that failed is never hidden.
		switch {
		case freshness.Err != nil:
			accountFields = append(accountFields, plugui.Field{Label: "自动续期", Value: "失败：" + freshness.Err.Error()})
		case freshness.Refreshed:
			accountFields = append(accountFields, plugui.Field{Label: "自动续期", Value: "刚刚已自动续期"})
		}
		if credential.Region != "" && credential.Region != cfg.Region {
			warnings = append(warnings, plugui.Notice("warning",
				fmt.Sprintf("该凭据来自 %s，但当前插件区域配置为 %s；两侧端点与登录态互不相通，请改配置或重新登录。",
					credential.Region, cfg.Region)))
		}
		if credential.Expired() && freshness.Err == nil {
			// The page renews on the spot, so "wait for the auto-refresh" is only
			// still true when the renewal itself has not failed; when it has, the
			// field above says exactly that.
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
	}

	body = append(body, warnings...)
	// The selected account keeps its identity card (the actions that are not
	// per-account live here), and every account gets its own credit card.
	if len(accountFields) > 0 {
		body = append(body, plugui.Card("账号 · "+entry.Name+"（当前）", plugui.Fields(accountFields...),
			plugui.Action{Label: "签到", Query: "action=checkin&" + accountQuery(entry), Kind: "primary"},
			plugui.Action{Label: "刷新模型目录", Query: "action=refresh"},
			plugui.Action{Label: "重新登录", Path: "login", Query: accountQuery(entry)},
		))
	} else {
		body = append(body, plugui.Card("账号 · "+entry.Name+"（当前）",
			plugui.Notice("danger", "凭据无法读取："+errCredential.Error())))
	}
	body = append(body, plugui.Card("通道与模型", plugui.Fields(catalogFields...)))

	// One sweep, one credit card per account: each card carries that account's
	// own figures, never the selected account's repeated.
	quotas := collectAccountQuotas(h, accounts, cfg)
	for _, quota := range quotas {
		body = append(body, renderQuotaCard(quota, quota.Entry.AuthIndex == entry.AuthIndex))
	}
	body = append(body, renderAccountList(accounts, entry.AuthIndex))
	return plugui.HTML("TRAE 状态", body...)
}

// renderQuotaCard renders one account's own credits and check-in state, with the
// two per-account actions.
//
// The card title names the account, so ten cards stay tellable apart, and every
// number shown belongs to the account in the title. A failed read is stated as
// such — the card never falls back to 0.
func renderQuotaCard(quota accountQuota, current bool) template.HTML {
	entry := quota.Entry
	fields := []plugui.Field{{Label: "状态", Value: statusText(entry)}}
	if entry.AuthIndex != "" {
		fields = append(fields, plugui.Field{Label: "索引", Value: entry.AuthIndex})
	}
	switch {
	case quota.CredentialErr != nil:
		fields = append(fields, plugui.Field{Label: "凭据", Value: "无法读取：" + quota.CredentialErr.Error()})
	default:
		switch {
		case quota.BalanceErr != nil:
			fields = append(fields, plugui.Field{Label: "积分余额", Value: "查询失败：" + quota.BalanceErr.Error()})
		case quota.Balance != nil:
			fields = append(fields, plugui.Field{Label: "剩余积分", Value: fmt.Sprintf("%.0f", quota.Balance.Total)})
			for _, pack := range quota.Balance.Packages {
				fields = append(fields,
					plugui.Field{Label: pack.Name, Value: fmt.Sprintf("剩余 %.0f / 共 %.0f（已用 %.0f）", pack.Remaining, pack.Total, pack.Used)})
			}
		}
		switch {
		case quota.CheckinErr != nil:
			fields = append(fields, plugui.Field{Label: "签到状态", Value: "查询失败：" + quota.CheckinErr.Error()})
		case quota.Checkin != nil:
			fields = append(fields,
				plugui.Field{Label: "今日已签到", Value: yesNo(quota.Checkin.CheckedIn)},
				plugui.Field{Label: "签到奖励", Value: fmt.Sprintf("%d", quota.Checkin.Credits)},
				plugui.Field{Label: "连续签到", Value: fmt.Sprintf("%d 天", quota.Checkin.StreakDays)},
			)
		}
	}
	title := "积分与签到 · " + entry.Name
	if current {
		title += "（当前）"
	}
	return plugui.Card(title, plugui.Fields(fields...),
		plugui.Action{Label: "签到", Query: "action=checkin&" + accountQuery(entry), Kind: "primary"},
		plugui.Action{Label: "重新登录", Path: "login", Query: accountQuery(entry)},
	)
}

// accountQuery names one account in a link. It is empty when the host gave the
// entry no runtime index, so a link never carries a dangling selector.
func accountQuery(entry pluginapi.HostAuthFileEntry) string {
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return ""
	}
	return "auth_index=" + entry.AuthIndex
}

// renderAccountList renders the switcher across accounts.
//
// It is rendered for a single account as well, because it carries 新建账号 —
// the only way to add a SECOND account from this page.
func renderAccountList(accounts []pluginapi.HostAuthFileEntry, current string) template.HTML {
	fields := make([]plugui.Field, 0, len(accounts))
	for _, entry := range accounts {
		marker := ""
		if entry.AuthIndex == current {
			marker = "（当前）"
		}
		fields = append(fields, plugui.Field{Label: entry.Name + marker, Value: statusText(entry)})
	}
	title := "全部账号"
	if len(accounts) > 1 {
		title += "（在地址后追加 ?auth_index=<索引> 可切换）"
	}
	return plugui.Card(title, plugui.Fields(fields...),
		plugui.Action{Label: "新建账号", Path: "login", Query: plugui.AddAccountQuery},
	)
}

// renderCheckinOutcome performs the daily check-in and renders its result.
func renderCheckinOutcome(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) template.HTML {
	credential, freshness, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return plugui.Notice("danger", "无法读取凭据："+errCredential.Error())
	}
	// A check-in signs with the same credential the page reads with: an expired
	// one that could not be renewed would only produce an upstream rejection.
	if freshness.Expired && freshness.Err != nil {
		return plugui.Notice("danger", "凭据已过期且自动续期失败："+freshness.Err.Error())
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
//
// The document reports EVERY account this plugin owns: `accounts` carries one
// entry per account, in host order, each with that account's own credits and
// check-in state. The selected account's figures stay at the top level
// (`credits`, `daily_checkin`) for the consumers that read them there, and its
// entry additionally carries the model catalog, which is a property of the
// credential and costs one upstream call per account.
//
// A failed read is reported as an error field on the entry that failed and never
// as a zero: `credits` is simply absent when it could not be read.
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

	quotas := collectAccountQuotas(h, accounts, cfg)
	entries := quotaListJSON(quotas)

	current, okCurrent := quotaOf(quotas, entry)
	if !okCurrent {
		// Unreachable while accounts and quotas come from the same listing.
		body["accounts"] = entries
		body["error"] = "无法定位该账号的额度记录"
		return jsonManagementResponse(http.StatusOK, body)
	}

	// The selected account's entry carries the catalog: it is what the page's
	// 通道与模型 card describes, and computing it for every account would issue
	// one extra upstream request per account for a field the others never showed.
	selectedEntry := map[string]any{}
	for index, quota := range quotas {
		if quota.Entry.AuthIndex == entry.AuthIndex && quota.Entry.Name == entry.Name {
			selectedEntry = entries[index]
			break
		}
	}
	if current.CredentialErr == nil {
		summary := summariseCatalog(h, current.Credential, cfg)
		selectedEntry["models"] = summary.ModelCount
		selectedEntry["image_models"] = summary.ImageCount
		selectedEntry["max_mode_models"] = summary.MaxModeHits
		selectedEntry["channels"] = summary.ChannelHits
		if summary.Error != "" {
			selectedEntry["catalog_error"] = summary.Error
		}
	}
	body["accounts"] = entries

	body["auth_index"] = entry.AuthIndex
	body["name"] = entry.Name
	body["status"] = statusText(entry)
	if current.Refresh.Refreshed {
		body["refreshed"] = true
	}
	if current.Refresh.Err != nil {
		body["refresh_error"] = current.Refresh.Err.Error()
	}
	if current.CredentialErr != nil {
		body["error"] = current.CredentialErr.Error()
		return jsonManagementResponse(http.StatusOK, body)
	}
	if current.Balance != nil {
		body["credits"] = current.Balance.Total
	}
	if current.BalanceErr != nil {
		body["credit_error"] = current.BalanceErr.Error()
	}
	if current.Checkin != nil {
		body["daily_checkin"] = map[string]any{
			"checked_in":  current.Checkin.CheckedIn,
			"credits":     current.Checkin.Credits,
			"streak_days": current.Checkin.StreakDays,
		}
	}
	if current.CheckinErr != nil {
		body["checkin_error"] = current.CheckinErr.Error()
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
	credential, freshness, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errCredential.Error()})
		}
		return plugui.HTML("TRAE 签到",
			plugui.Card("签到失败", plugui.Notice("danger", errCredential.Error()),
				plugui.Action{Label: "返回状态", Path: "status"}))
	}
	if freshness.Expired && freshness.Err != nil {
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadGateway,
				map[string]any{"error": "凭据已过期且自动续期失败：" + freshness.Err.Error()})
		}
		return plugui.HTML("TRAE 签到",
			plugui.Card("签到失败", plugui.Notice("danger", "凭据已过期且自动续期失败："+freshness.Err.Error()),
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
	notices := []template.HTML{}
	if plugui.IsAddAccountRequest(request) {
		notices = append(notices, plugui.Notice("", plugui.AddAccountNotice))
	}
	notices = append(notices, plugui.Notice("", "点击下面的按钮获取授权链接，在浏览器里完成授权后回到本页检查结果。"))
	return plugui.HTML("TRAE 登录",
		plugui.Card("浏览器登录",
			plugui.Group(append(notices,
				plugui.Fields(
					plugui.Field{Label: "区域", Value: cfg.Region + "（" + product.Site + "）"},
					plugui.Field{Label: "登录门户", Value: product.ConsoleHost + "/authorization"},
					plugui.Field{Label: "回调地址", Value: fmt.Sprintf("http://127.0.0.1:<实际端口>%s", CallbackPath)},
					plugui.Field{Label: "首选端口", Value: fmt.Sprintf("%d", cfg.CallbackPort)},
				),
			)...),
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
