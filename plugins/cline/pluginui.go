package main

import (
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

// pluguiNoticeCard renders a single card holding one message and its actions.
func pluguiNoticeCard(title, tone, message string, actions ...plugui.Action) template.HTML {
	return plugui.Card(title, plugui.Notice(tone, message), actions...)
}

// renderStatusPage renders the account overview, the token diagnostics, the
// balance and the catalogue size.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	body := []template.HTML{plugui.Card("接口与目录", plugui.Fields(
		plugui.Field{Label: "接口基址", Value: APIBase},
		plugui.Field{Label: "登录服务", Value: WorkOSBase},
		plugui.Field{Label: "工作流", Value: "WorkOS 设备码（无本地回调端口）"},
		plugui.Field{Label: "模型目录", Value: catalogueText(cfg)},
		plugui.Field{Label: "线上目录缓存", Value: catalogueCacheText()},
		plugui.Field{Label: "思考级别", Value: strings.Join(reasoningLevels, " / ") + "（默认不发送，由客户端或宿主决定）"},
		plugui.Field{Label: "max_tokens 上限", Value: itoaInt(maxOutputTokens(cfg))},
	))}

	accounts := clineAccounts(h)
	if len(accounts) == 0 {
		body = append(body, plugui.Card("尚未添加账号",
			plugui.Notice("warning", "当前实例还没有 Cline 账号。WorkOS 设备码登录不需要本地回调端口，"+
				"点下面的按钮获取授权链接即可。"),
			plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
		))
		return pluguiPage("Cline", body...)
	}

	entry, found := selectAccount(h, request)
	if !found {
		body = append(body, plugui.Card("账号不存在",
			plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。")))
		return pluguiPage("Cline", body...)
	}

	accountFields := []plugui.Field{
		{Label: "名称", Value: entry.Name},
		{Label: "状态", Value: statusText(entry)},
	}
	if entry.AuthIndex != "" {
		accountFields = append(accountFields, plugui.Field{Label: "索引", Value: entry.AuthIndex})
	}
	balanceFields := []plugui.Field{}

	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		accountFields = append(accountFields, plugui.Field{Label: "凭据", Value: "无法读取：" + errCredential.Error()})
	} else {
		accountFields = append(accountFields,
			plugui.Field{Label: "账号 ID", Value: valueOr(credential.AccountID, "未知（余额查询需要它）")},
			plugui.Field{Label: "邮箱", Value: valueOr(credential.Email, "—")},
			plugui.Field{Label: "有效期至", Value: formatExpiry(credential.ExpiresAt())},
			plugui.Field{Label: "可自动续期", Value: yesNo(credential.Refreshable())},
			plugui.Field{Label: "令牌前缀", Value: tokenPrefixText(credential)},
		)
		balance, errBalance := fetchBalance(transportFor(h), credential, cfg)
		switch {
		case errBalance != nil:
			balanceFields = append(balanceFields, plugui.Field{Label: "余额", Value: "查询失败：" + errBalance.Error()})
		case balance == nil:
			balanceFields = append(balanceFields, plugui.Field{Label: "余额", Value: "上游没有返回余额"})
		default:
			balanceFields = append(balanceFields,
				plugui.Field{Label: "Cline 账户余额", Value: fmt.Sprintf("%s %s", trimNumber(balance.Total), balance.Unit)},
				// The raw number is shown on purpose: the scale is a guess, and
				// this is what a real account needs in order to confirm it.
				plugui.Field{Label: "原始值", Value: trimNumber(balance.Raw) + "（换算除数 " + trimNumber(cfg.BalanceDivisor) + "，刻度无上游依据）"},
			)
		}
	}

	body = append(body, plugui.Card("账号", plugui.Fields(accountFields...),
		plugui.Action{Label: "刷新令牌", Query: "action=refresh", Kind: "primary"},
		plugui.Action{Label: "重新登录", Path: "login"},
		plugui.Action{Label: "新建账号", Path: "login", Query: plugui.AddAccountQuery},
	))
	if len(balanceFields) > 0 {
		body = append(body, plugui.Card("余额", plugui.Fields(balanceFields...)))
	}
	if switcher := renderAccountList(accounts, entry.AuthIndex); switcher != "" {
		body = append(body, switcher)
	}
	return pluguiPage("Cline", body...)
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

// renderLoginPage renders the two-step WorkOS device-code login.
//
// Step one hands the user an authorization URL immediately; nothing here ever
// blocks waiting for the browser. The URL WorkOS returns already carries the
// user code, which is why the page shows the code separately as well: a user who
// opens the bare verification URI has to type it in.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "login", "start":
		return startLoginPage(h)
	case "poll":
		return pollLoginPage(h, request)
	}
	notices := []template.HTML{}
	if plugui.IsAddAccountRequest(request) {
		notices = append(notices, plugui.Notice("", plugui.AddAccountNotice))
	}
	notices = append(notices, plugui.Notice("", "点击下面的按钮获取设备码授权链接。在浏览器里完成授权后回到本页点击「检查授权结果」即可，"+
		"整个过程不需要本地回调端口。"))
	return pluguiPage("Cline 登录",
		plugui.Card("WorkOS 设备码登录",
			plugui.Group(append(notices,
				plugui.Fields(
					plugui.Field{Label: "授权接口", Value: WorkOSBase + DeviceAuthorizationPath},
					plugui.Field{Label: "轮询接口", Value: WorkOSBase + DeviceAuthenticatePath},
					plugui.Field{Label: "换取凭据", Value: APIBase + RegisterPath},
				),
			)...),
			plugui.Action{Label: "开始登录", Query: "action=start", Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// startLoginPage begins a device-code session and shows the authorization URL.
func startLoginPage(h *abiboot.Host) pluginapi.ManagementResponse {
	session, errStart := startLoginSession(transportFor(h), settings())
	if errStart != nil {
		return pluguiPage("Cline 登录",
			plugui.Card("无法发起登录", plugui.Notice("danger", errStart.Error()),
				plugui.Action{Label: "重试", Query: "action=start", Kind: "primary"}))
	}
	return pluguiPage("Cline 登录",
		plugui.Card("在浏览器中完成授权",
			plugui.Group(
				plugui.Notice("", "请复制下面的链接到浏览器打开并完成授权（WorkOS 登录页）。"+
					"链接已带 user_code；如果只打开了 verification_uri，请手动输入下面的设备码。"),
				plugui.Fields(
					plugui.Field{Label: "授权链接", Value: session.LoginURL()},
					plugui.Field{Label: "设备码", Value: session.Device.UserCode},
					plugui.Field{Label: "有效期", Value: formatExpiry(session.ExpiresAt)},
					plugui.Field{Label: "轮询间隔", Value: itoaInt(session.Device.IntervalMS/1000) + " 秒"},
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
		return pluguiPage("Cline 登录",
			plugui.Card("检查失败", plugui.Notice("danger", errPoll.Error()),
				plugui.Action{Label: "重新登录", Query: "action=start", Kind: "primary"}))
	}
	switch response.Status {
	case pluginapi.AuthLoginStatusSuccess:
		message := response.Message
		if message == "" {
			message = "登录成功，账号已保存。"
		}
		return pluguiPage("Cline 登录",
			plugui.Card("登录成功", plugui.Notice("success", message),
				plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"}))
	case pluginapi.AuthLoginStatusPending, "":
		return pluguiPage("Cline 登录",
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
		return pluguiPage("Cline 登录",
			plugui.Card("登录失败", plugui.Notice("danger", message),
				plugui.Action{Label: "重新登录", Query: "action=start", Kind: "primary"}))
	}
}

// pollLoginForManagement drives the shared poll and persists the credential
// itself.
//
// On the normal login path the host saves the credential the poll returns, but
// this page drives the poll directly, so saving is this function's job.
func pollLoginForManagement(h *abiboot.Host, state string) (pluginapi.AuthLoginPollResponse, error) {
	var empty pluginapi.AuthLoginPollResponse
	if state == "" {
		return empty, statusError(false, "missing_state", http.StatusBadRequest, "缺少 state 参数，请重新发起登录")
	}
	session, found := lookupLoginSession(state)
	if !found {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已超时，请重新发起登录",
		}, nil
	}
	response, errPoll := advanceLoginSession(transportFor(h), session, state, settings())
	if errPoll != nil {
		return empty, errPoll
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

// lastLoginState returns the most recently started PENDING session, so a poll
// link that lost its state parameter still works while a completed or expired
// session never gets resumed by accident.
func lastLoginState() string {
	loginMu.Lock()
	defer loginMu.Unlock()
	newest := ""
	var newestAt time.Time
	for state, session := range loginSessions {
		if session.expired() || session.snapshotDone() {
			continue
		}
		if session.CreatedAt.After(newestAt) {
			newest = state
			newestAt = session.CreatedAt
		}
	}
	return newest
}

// catalogueText describes the catalogue sources actually in use.
func catalogueText(cfg Config) string {
	if !cfg.ModelDiscovery {
		return "仅静态表（model_discovery 已关闭）"
	}
	if cfg.ModelCacheTTLMS > 0 {
		return fmt.Sprintf("线上目录 + 静态表，缓存 %d 秒", cfg.ModelCacheTTLMS/1000)
	}
	return "线上目录 + 静态表，不缓存"
}

// catalogueCacheText reports what the catalogue cache currently holds.
func catalogueCacheText() string {
	cached, fetchedAt := discoveredModels.peek()
	if len(cached) == 0 {
		return "尚未拉取（model.for_auth 时获取）"
	}
	return fmt.Sprintf("%d 条，获取于 %s", len(cached), fetchedAt.Local().Format("2006-01-02 15:04"))
}

// maxOutputTokens resolves the effective clamp ceiling.
func maxOutputTokens(cfg Config) int {
	if cfg.MaxOutputTokens <= 0 {
		return MaxOutputTokensCeiling
	}
	return cfg.MaxOutputTokens
}

// tokenPrefixText reports whether the stored token still carries the prefix.
func tokenPrefixText(credential *Credential) string {
	if strings.HasPrefix(strings.TrimSpace(credential.AccessToken), TokenPrefix) {
		return TokenPrefix + "…（正常）"
	}
	return "缺失（上游会返回 401，请重新登录）"
}

// formatExpiry renders an expiry with the remaining time.
func formatExpiry(expiry time.Time) string {
	if expiry.IsZero() {
		return "未知（以服务端 401 为准）"
	}
	return formatRemainingValue(expiry)
}

// formatRemainingValue renders an absolute time plus the remaining duration.
func formatRemainingValue(deadline time.Time) string {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return deadline.Local().Format("2006-01-02 15:04") + "（已过期）"
	}
	return fmt.Sprintf("%s（剩余 %s）", deadline.Local().Format("2006-01-02 15:04"), remaining.Truncate(time.Minute))
}

// valueOr renders a value or a placeholder when it is empty.
func valueOr(value, placeholder string) string {
	if strings.TrimSpace(value) == "" {
		return placeholder
	}
	return value
}

// yesNo renders a boolean for display.
func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// trimNumber renders a number without trailing zeros.
func trimNumber(value float64) string {
	text := fmt.Sprintf("%.2f", value)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "" || text == "-" {
		return "0"
	}
	return text
}
