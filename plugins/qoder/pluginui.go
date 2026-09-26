package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file renders the management pages a user sees inside CPA-Manager-Plus.
//
// This plugin declares no Menu route, so its pages are mounted on
// `/v0/resource/plugins/<id>/<path>` as Menu-less ResourceRoutes and CPAMP renders
// them in a same-origin iframe with the host theme injected as CSS custom
// properties. Two consequences shape everything here:
//
//   - the host dispatches those routes as GET only, so every action is a link
//     carrying a query string rather than a form submission;
//   - markup only consumes the host's CSS variables, so there is no frontend
//     build and both light and dark themes work for free.

// pluguiPage wraps body fragments in the themed document shell.
func pluguiPage(heading string, body ...template.HTML) pluginapi.ManagementResponse {
	return plugui.HTML(heading, body...)
}

// renderStatusPage renders the account overview, the active inference path and
// the credit balance.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	path := activeInferPath(cfg)

	// The channel card answers the question this plugin is most often
	// misconfigured about: which endpoint the requests actually use.
	channelFields := []plugui.Field{
		{Label: "区域", Value: regionText(activeRegion())},
		{Label: "推理通道", Value: inferPathText(path)},
		{Label: "模型数量", Value: itoaInt(len(staticModelInfos(cfg, activeRegion())))},
	}
	switch {
	case path == pathPublic:
		channelFields = append(channelFields, plugui.Field{
			Label: "说明", Value: "未配置 wasm_path，只走公开 OpenAI 兼容端点；目录 key 在该端点会被判 Unsupported model",
		})
	case strings.TrimSpace(cfg.WASMPath) == "":
		channelFields = append(channelFields, plugui.Field{Label: "说明", Value: "未配置 wasm_path"})
	default:
		if _, errSigner := signerFor(cfg.WASMPath); errSigner != nil {
			channelFields = append(channelFields,
				plugui.Field{Label: "WASM 路径", Value: cfg.WASMPath},
				plugui.Field{Label: "WASM 状态", Value: "加载失败：" + errSigner.Error()},
			)
		} else {
			channelFields = append(channelFields,
				plugui.Field{Label: "WASM 路径", Value: cfg.WASMPath},
				plugui.Field{Label: "WASM 状态", Value: "已加载，签名由 WASM 生成并原样透传"},
			)
		}
	}
	channelFields = append(channelFields,
		plugui.Field{Label: "客户端版本", Value: clientVersion(cfg)},
		plugui.Field{Label: "会话类型", Value: sessionType(cfg, productByID(string(activeRegion())))},
	)

	body := []template.HTML{plugui.Card("推理通道", plugui.Fields(channelFields...))}

	accounts := qoderAccounts(h)
	if len(accounts) == 0 {
		body = append(body, plugui.Card("尚未添加账号",
			plugui.Notice("warning", "当前实例还没有 Qoder 账号。设备码登录不需要本地回调端口，点下面的按钮获取授权链接即可。"),
			plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
		))
		return pluguiPage("Qoder", body...)
	}

	entry, found := selectAccount(h, request)
	if !found {
		body = append(body, plugui.Card("账号不存在",
			plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。")))
		return pluguiPage("Qoder", body...)
	}

	// One sweep, one card per account: each card carries that account's own
	// figures, never the selected account's repeated.
	quotas := collectAccountQuotas(h, accounts, cfg)
	for _, quota := range quotas {
		body = append(body, renderQuotaCard(quota, quota.Entry.AuthIndex == entry.AuthIndex))
	}
	body = append(body, renderAccountList(accounts, entry.AuthIndex))
	return pluguiPage("Qoder", body...)
}

// renderQuotaCard renders one account: its identity, its OWN credits and its OWN
// check-in state, with the two per-account actions.
//
// The card title names the account, so ten cards stay tellable apart, and every
// number shown belongs to the account in the title. An unreadable credential or
// balance is stated as such — the card never falls back to 0.
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
		fields = append(fields,
			plugui.Field{Label: "区域", Value: regionText(quota.Credential.regionOr(activeRegion()))},
			plugui.Field{Label: "有效期至", Value: formatExpiry(quota.Credential.ExpiresAt())},
			plugui.Field{Label: "可自动续期", Value: yesNo(quota.Credential.Refreshable())},
			plugui.Field{Label: "含 uid（加密推理必需）", Value: yesNo(strings.TrimSpace(quota.Credential.UID) != "")},
		)
		if quota.Credential.Nickname != "" {
			fields = append(fields, plugui.Field{Label: "用户", Value: quota.Credential.Nickname})
		}
		switch {
		case quota.BalanceErr != nil:
			// Unknown, not zero: the read failed, so no number is shown.
			fields = append(fields, plugui.Field{Label: "额度", Value: "查询失败：" + quota.BalanceErr.Error()})
		case quota.BalanceNone:
			fields = append(fields, plugui.Field{Label: "额度", Value: "企业版账号不下发额度数字"})
		default:
			for _, pkg := range quota.Balance.Packages {
				label := pkg.Name
				if strings.TrimSpace(label) == "" {
					label = "额度包"
				}
				fields = append(fields, plugui.Field{
					Label: label,
					Value: fmt.Sprintf("%s / %s %s", trimNumber(pkg.Remaining), trimNumber(pkg.Total), pkg.Unit),
				})
			}
			fields = append(fields, plugui.Field{Label: "剩余合计", Value: trimNumber(quota.Balance.Total) + " credits"})
		}
		switch {
		case quota.CampaignsErr != nil:
			fields = append(fields, plugui.Field{Label: "今日签到", Value: "查询失败：" + quota.CampaignsErr.Error()})
		case quota.Campaigns != nil:
			fields = append(fields, plugui.Field{Label: "今日签到", Value: checkinText(quota.Campaigns)})
		}
	}
	title := "额度 · " + entry.Name
	if current {
		title += "（当前）"
	}
	return plugui.Card(title, plugui.Fields(fields...),
		plugui.Action{Label: "领取每日积分", Path: "checkin", Query: accountQuery(entry), Kind: "primary"},
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

// renderLoginPage renders the two-step device-code login.
//
// Step one hands the user an authorization URL immediately; nothing here ever
// blocks waiting for the browser. That is not a style choice: a popup or a
// navigation started after a blocking call loses the transient activation window
// (`qoder-oauth.ts:151-159`).
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "login", "start":
		return startLoginPage()
	case "poll":
		return pollLoginPage(h, request)
	}

	region := activeRegion()
	note := "点击下面的按钮获取设备码授权链接。在浏览器里完成授权后回到本页点击「检查授权结果」即可，" +
		"整个过程不需要本地回调端口。"
	if strings.TrimSpace(settings().WASMPath) == "" {
		note += "（当前未配置 wasm_path，登录后仅能调用公开端点认识的通用模型名。）"
	}
	notices := []template.HTML{}
	if plugui.IsAddAccountRequest(request) {
		notices = append(notices, plugui.Notice("", plugui.AddAccountNotice))
	}
	notices = append(notices, plugui.Notice("", note))
	return pluguiPage("Qoder 登录",
		plugui.Card("设备码登录",
			plugui.Group(append(notices,
				plugui.Fields(
					plugui.Field{Label: "区域", Value: regionText(region)},
					plugui.Field{Label: "授权页", Value: productByID(string(region)).AuthBase + DeviceSelectPath},
					plugui.Field{Label: "轮询地址", Value: productByID(string(region)).OpenAPIBase + PollPath},
				),
			)...),
			plugui.Action{Label: "开始登录", Query: "action=start", Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// startLoginPage begins a device-code session and shows the authorization URL.
func startLoginPage() pluginapi.ManagementResponse {
	session, errStart := startLoginSession(activeRegion())
	if errStart != nil {
		return pluguiPage("Qoder 登录",
			plugui.Card("无法发起登录", plugui.Notice("danger", errStart.Error()),
				plugui.Action{Label: "重试", Query: "action=start", Kind: "primary"}))
	}
	return pluguiPage("Qoder 登录",
		plugui.Card("在浏览器中完成授权",
			plugui.Group(
				plugui.Notice("", "请复制下面的链接到浏览器打开并完成授权（GitHub / 账号登录）。"+
					"授权完成后回到本页点击「检查授权结果」。"),
				plugui.Fields(
					plugui.Field{Label: "授权链接", Value: session.LoginURL()},
					plugui.Field{Label: "区域", Value: regionText(session.Region)},
					plugui.Field{Label: "有效期", Value: "5 分钟"},
					plugui.Field{Label: "设备标识", Value: session.Device.MachineID},
				),
			),
			plugui.Action{Label: "检查授权结果", Query: "action=poll&state=" + session.State, Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// pollLoginPage advances a login session and persists the credential on success.
func pollLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	state := strings.TrimSpace(request.Query.Get("state"))
	if state == "" {
		state = lastLoginState()
	}
	response, errPoll := pollLoginForManagement(h, state)
	if errPoll != nil {
		if wantsJSON(request) {
			return jsonManagementResponse(statusOf(errPoll, http.StatusBadGateway),
				map[string]any{"error": errPoll.Error()})
		}
		return pluguiPage("Qoder 登录",
			plugui.Card("检查失败", plugui.Notice("danger", errPoll.Error()),
				plugui.Action{Label: "重新登录", Query: "action=start", Kind: "primary"}))
	}
	switch response.Status {
	case pluginapi.AuthLoginStatusSuccess:
		message := response.Message
		if message == "" {
			message = "登录成功，账号已保存。"
		}
		return pluguiPage("Qoder 登录",
			plugui.Card("登录成功", plugui.Notice("success", message),
				plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"}))
	case pluginapi.AuthLoginStatusPending, "":
		return pluguiPage("Qoder 登录",
			plugui.Card("等待授权",
				plugui.Notice("", "还没有拿到令牌，请确认已在浏览器里完成授权，然后再次检查。"),
				plugui.Action{Label: "再次检查", Query: "action=poll&state=" + state, Kind: "primary"},
				plugui.Action{Label: "重新开始", Query: "action=start"},
			))
	default:
		message := response.Message
		if message == "" {
			message = "登录失败。"
		}
		return pluguiPage("Qoder 登录",
			plugui.Card("登录失败", plugui.Notice("danger", message),
				plugui.Action{Label: "重新登录", Query: "action=start", Kind: "primary"}))
	}
}

// pollLoginForManagement reuses the auth.login.poll handler and then persists the
// credential itself.
//
// On the normal login path the host saves the credential the poll returns, but
// this page drives the poll directly, so saving is this function's job.
func pollLoginForManagement(h *abiboot.Host, state string) (pluginapi.AuthLoginPollResponse, error) {
	var empty pluginapi.AuthLoginPollResponse
	if state == "" {
		return empty, statusError(false, "missing_state", http.StatusBadRequest, "缺少 state 参数，请重新发起登录")
	}
	raw, errMarshal := json.Marshal(pluginapi.AuthLoginPollRequest{State: state})
	if errMarshal != nil {
		return empty, statusError(false, "encode_poll", http.StatusInternalServerError, "encode poll request: %v", errMarshal)
	}
	value, errPoll := handleAuthLoginPoll(h, raw)
	if errPoll != nil {
		return empty, errPoll
	}
	response, ok := value.(pluginapi.AuthLoginPollResponse)
	if !ok {
		return empty, statusError(false, "unexpected_poll", http.StatusInternalServerError, "poll handler returned %T", value)
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
		return empty, transportError("save_auth", "保存凭据失败：%v", errSave)
	}
	forgetLoginSession(state)
	return response, nil
}

// renderCheckinCard renders the check-in outcome.
func renderCheckinCard(outcome claimOutcome) template.HTML {
	message := outcome.Message
	if outcome.Amount > 0 {
		message = fmt.Sprintf("%s，获得 %s 积分", message, trimNumber(outcome.Amount))
	}
	// The status comes from the RESPONSE BODY, never from the HTTP status: a
	// repeat claim also answers 200 (`qoder-credits.ts:389-396`).
	var tone string
	switch outcome.Status {
	case "claimed":
		tone = "success"
	case "already-claimed":
		tone = ""
	case "inactive":
		tone = "warning"
	default:
		tone = "danger"
	}
	return plugui.Card("领取结果", plugui.Notice(tone, message),
		plugui.Action{Label: "返回状态", Path: "status", Kind: "primary"})
}

// checkinFailed renders a failed check-in page.
func checkinFailed(message string) template.HTML {
	return plugui.Card("领取失败", plugui.Notice("danger", message),
		plugui.Action{Label: "返回状态", Path: "status"})
}

// lastLoginState returns the most recently started PENDING session, so a poll
// link that lost its state parameter still works while a completed or expired
// session never gets resumed by accident.
func lastLoginState() string {
	loginMu.Lock()
	defer loginMu.Unlock()
	newest := ""
	var newestAt time.Time
	for state, session := range loginSessions {
		if session.expired() {
			continue
		}
		if _, _, done := session.snapshot(); done {
			continue
		}
		if session.CreatedAt.After(newestAt) {
			newest = state
			newestAt = session.CreatedAt
		}
	}
	return newest
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

// regionText renders a region for display.
func regionText(region Region) string {
	switch normalizeRegion(string(region)) {
	case RegionCN:
		return "国内版 (qoder-cn)"
	default:
		return "国际版 (qoder)"
	}
}

// inferPathText renders the active inference path.
func inferPathText(path inferPath) string {
	if path == pathEncrypted {
		return "加密端点 agent_chat_generation（目录 key，WASM 签名头原样透传）"
	}
	return "公开端点 /model/v1/chat/completions（通用模型名）"
}

// checkinText renders the campaign state.
func checkinText(parsed *campaigns) string {
	switch {
	case len(claimableCampaigns(parsed)) > 0:
		return "可领取"
	case parsed.ShowCampaign:
		return "今日已领取或不可领取"
	default:
		// `showCampaign:false, campaigns:[]` is what the server returns after the
		// daily benefit was taken; "none" and "already taken" are
		// indistinguishable there (`qoder-credits.ts:250-256`).
		return "今日已领取（活动每日 10:00 UTC+8 刷新）"
	}
}

// formatExpiry renders an expiry with the remaining time.
func formatExpiry(expiry time.Time) string {
	if expiry.IsZero() {
		return "未知（以服务端 401 为准）"
	}
	remaining := time.Until(expiry)
	if remaining <= 0 {
		return expiry.Local().Format("2006-01-02 15:04") + "（已过期）"
	}
	return fmt.Sprintf("%s（剩余 %s）", expiry.Local().Format("2006-01-02 15:04"), remaining.Truncate(time.Minute))
}

// yesNo renders a boolean for display.
func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// trimNumber renders a credit amount without trailing zeros.
func trimNumber(value float64) string {
	return formatFactor(value)
}
