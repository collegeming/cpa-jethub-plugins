package main

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credits"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The management pages a user sees inside CPA-Manager-Plus.
//
// This plugin declares no Menu route, so its pages are mounted on
// `/v0/resource/plugins/<id>/<path>` as Menu-less ResourceRoutes and CPAMP
// renders them in a same-origin iframe with the host theme injected as CSS
// custom properties. Two consequences shape everything here:
//
//   - the host dispatches those routes as GET only, so every action is a link
//     carrying a query string rather than a form submission;
//   - markup only consumes the host's CSS variables, so there is no frontend
//     build and both light and dark themes work for free.

// pluguiPage wraps body fragments in the themed document shell.
func pluguiPage(heading string, body ...template.HTML) pluginapi.ManagementResponse {
	return plugui.HTML(heading, body...)
}

// renderStatusPage renders the protocol summary and one card per account.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	entries := staticCatalogueEntries(cfg)

	body := []template.HTML{plugui.Card("推理协议", plugui.Fields(
		plugui.Field{Label: "协议", Value: "Anthropic Messages（本仓库首个该协议族的 provider）"},
		plugui.Field{Label: "端点", Value: APIDisplayHost() + InferPath},
		plugui.Field{Label: "请求头", Value: "仅 Authorization / Content-Type / Accept（实测不需要 anthropic-version）"},
		plugui.Field{Label: "模型数量", Value: itoaInt(len(entries))},
		plugui.Field{Label: "模型目录", Value: catalogueSourceText(cfg)},
		plugui.Field{Label: "签到时区", Value: cfg.timezoneID() + "（必须作为 query 参数下发）"},
	))}

	// ⚠️ The verification state is on the page on purpose. The inference path
	// was measured end to end; the login and refresh round trips were not.
	body = append(body, plugui.Card("验证状态",
		plugui.Notice("warning",
			"推理路径已真机实测（4 个模型全部 HTTP 200，文本 / 思考 / 结构化工具调用均正常）。"+
				"登录与续期的往返**未**在真实服务端验证过：参考实现的登录只跑过单测，历次实机探针读的都是桌面客户端已有的令牌。"+
				"本机也没有 MiniMax 客户端，无法补测。请把登录流程当作「忠实移植」而非「已验证」。"),
	))

	if errNote := catalogueLastError(); errNote != "" {
		body = append(body, plugui.Card("模型目录降级", plugui.Notice("warning", errNote+"（已回退内置兜底表）")))
	}

	accounts := minimaxAccounts(h)
	if len(accounts) == 0 {
		body = append(body, plugui.Card("尚未添加账号",
			plugui.Notice("warning", "当前实例还没有 MiniMax Code 账号。设备码登录不需要本地回调端口，点下面的按钮获取授权链接即可。"),
			plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
		))
		return pluguiPage("MiniMax Code", body...)
	}

	entry, found := selectAccount(h, request)
	if !found {
		body = append(body, plugui.Card("账号不存在",
			plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。")))
		return pluguiPage("MiniMax Code", body...)
	}

	// One sweep, one card per account: each card carries that account's own
	// figures, never the selected account's repeated.
	states := collectAccountStates(h, accounts, cfg)
	for _, state := range states {
		body = append(body, renderAccountCard(state, state.Entry.AuthIndex == entry.AuthIndex))
	}
	body = append(body, renderModelCard(entries))
	body = append(body, renderAccountList(accounts, entry.AuthIndex))
	return pluguiPage("MiniMax Code", body...)
}

// APIDisplayHost renders the API host for the page.
func APIDisplayHost() string { return APIHost }

// catalogueSourceText says where the published catalogue came from.
func catalogueSourceText(cfg Config) string {
	if !cfg.DiscoverModels {
		return "内置兜底表（discover_models 已关闭；包含 4 个实测模型）"
	}
	if cached := cachedModels(time.Duration(cfg.modelCacheTTL()) * time.Millisecond); len(cached) > 0 {
		return "远端目录（缓存有效期 " + itoaInt(cfg.modelCacheTTL()/1000) + " 秒）"
	}
	return "远端目录尚未拉取，当前展示兜底表；打开状态页或发起推理即会拉取"
}

// renderAccountCard renders one account: its identity, its OWN credits and its
// OWN check-in state, with the actions that apply to it.
//
// An unreadable credential or balance is stated as such — the card never falls
// back to 0.
func renderAccountCard(state accountState, current bool) template.HTML {
	entry := state.Entry
	title := "账号 " + entry.Name
	if label := strings.TrimSpace(entry.Label); label != "" {
		title = label
	}
	if current {
		title += "（当前选中）"
	}
	fields := []plugui.Field{{Label: "状态", Value: statusText(entry)}}
	if entry.AuthIndex != "" {
		fields = append(fields, plugui.Field{Label: "索引", Value: entry.AuthIndex})
	}
	if state.CredentialErr != nil {
		fields = append(fields, plugui.Field{Label: "凭据", Value: "无法读取：" + state.CredentialErr.Error()})
		// The notice says to log in again, so it has to carry the way to do it —
		// an instruction with no link is the dead end this page exists to avoid.
		return plugui.Card(title, plugui.Group(plugui.Fields(fields...),
			plugui.Notice("danger", "该账号的凭据无法读取，请重新登录。")),
			plugui.Action{Label: "重新登录", Path: "login", Query: "auth_index=" + entry.AuthIndex, Kind: "primary"})
	}

	credential := state.Credential
	fields = append(fields,
		plugui.Field{Label: "令牌", Value: credential.maskedToken()},
		plugui.Field{Label: "有效期至", Value: formatExpiry(credential.Expiry())},
		plugui.Field{Label: "可自动续期", Value: yesNo(credential.Refreshable())},
	)

	notices := []template.HTML{}
	if state.Refresh.Refreshed {
		notices = append(notices, plugui.Notice("success", "凭据已在本次访问中自动续期。"))
	}
	if state.Refresh.Err != nil {
		notices = append(notices, plugui.Notice("warning", "自动续期失败："+state.Refresh.Err.Error()))
	}

	fields = append(fields, plugui.Field{Label: "积分余额", Value: balanceText(state)})
	fields = append(fields, plugui.Field{Label: "签到", Value: signinText(state)})
	if state.Signin.Active {
		fields = append(fields,
			plugui.Field{Label: "连续签到", Value: itoaInt(state.Signin.StreakDays) + " 天"},
			plugui.Field{Label: "今日额度", Value: trimAmount(state.Signin.DailyCredit) + " 积分"},
		)
	}
	if state.SigninErr != nil {
		notices = append(notices, plugui.Notice("warning", "签到状态读取失败："+state.SigninErr.Error()))
	}
	if current {
		actions := []plugui.Action{
			{Label: "签到", Path: "checkin", Query: "auth_index=" + entry.AuthIndex, Kind: "primary"},
			// The refresh token can be revoked or rotated away, and an account
			// whose credential cannot be read has no other route back.
			{Label: "重新登录", Path: "login", Query: "auth_index=" + entry.AuthIndex},
			{Label: "刷新本页", Kind: ""},
		}
		return plugui.Card(title, plugui.Group(append([]template.HTML{plugui.Fields(fields...)}, notices...)...), actions...)
	}
	return plugui.Card(title, plugui.Group(append([]template.HTML{plugui.Fields(fields...)}, notices...)...))
}

// balanceText renders the credit line.
//
// ⚠️ The balance is the sum of `details[].remaining_amount`; the record count is
// shown SEPARATELY and labelled, because reporting it as the balance is the
// defect this port exists to avoid repeating.
func balanceText(state accountState) string {
	switch {
	case state.BalanceErr != nil:
		return "读取失败：" + state.BalanceErr.Error()
	case state.Balance == nil:
		return "未知"
	case state.Balance.Rows == 0:
		return "0 积分（无积分记录）"
	default:
		return trimAmount(state.Balance.Total) + " 积分（" + itoaInt(state.Balance.Rows) + " 条记录）"
	}
}

// signinText renders the check-in line.
func signinText(state accountState) string {
	switch {
	case state.SigninErr != nil:
		return "读取失败"
	case !state.Signin.Active:
		return "未返回面板"
	case state.Signin.TodayCheckedIn:
		return "今日已签到"
	case state.Signin.ClaimableExists:
		return "今日可领取"
	default:
		return "今日不可领取"
	}
}

// renderModelCard lists the published catalogue with its thinking matrix.
//
// The matrix is shown because it is the one thing about this provider a user
// cannot infer: two models look identical in a picker and behave completely
// differently when "thinking off" is requested.
func renderModelCard(entries []ModelCatalogEntry) template.HTML {
	rows := make([]plugui.Field, 0, len(entries)*3)
	for _, entry := range entries {
		thinking := "未知（目录未声明思考模式）"
		switch entry.ThinkingMode {
		case thinkingSwitchable:
			thinking = "可开关：不传即不思考；开启需显式发 adaptive"
		case thinkingForcedOn:
			if requiresAdaptiveThinking(entry.ID) {
				thinking = "强制开启：必须发 adaptive，传 disabled 会被硬拒（HTTP 400 / 业务码 2013）"
			} else {
				thinking = "强制开启：服务端总是思考，传 disabled 会被静默忽略"
			}
		}
		efforts := "无档位"
		if len(entry.EffortOptions) > 0 {
			efforts = strings.Join(entry.EffortOptions, " / ")
		}
		rows = append(rows,
			plugui.Field{Label: entry.ID, Value: entry.Name + " · " + contextText(entry) + " · " + efforts},
			plugui.Field{Label: "思考", Value: thinking},
		)
	}
	return plugui.Card("模型目录", plugui.Group(
		plugui.Fields(rows...),
		plugui.Notice("", "上下文窗口取远端 context_window_options 的最大档（M3.1 与 M3 为 1M），"+
			"不是 limit.context（M3.1 的 limit.context 只有 512000）。"),
	))
}

// contextText renders one entry's window and image support.
func contextText(entry ModelCatalogEntry) string {
	window := itoa64(entry.ContextWindow)
	if entry.ContextWindow <= 0 {
		window = "窗口未知"
	} else {
		window += " 上下文"
	}
	if entry.SupportsImage {
		return window + " · 支持图片"
	}
	return window + " · 纯文本"
}

// renderAccountList renders the account switcher as links.
func renderAccountList(accounts []pluginapi.HostAuthFileEntry, currentIndex string) template.HTML {
	rows := make([]plugui.Field, 0, len(accounts))
	for _, entry := range accounts {
		marker := ""
		if entry.AuthIndex == currentIndex {
			marker = "（当前）"
		}
		label := entry.Name
		if entry.Label != "" {
			label = entry.Label
		}
		rows = append(rows, plugui.Field{
			Label: label + marker,
			Value: statusText(entry) + " · ?auth_index=" + entry.AuthIndex,
		})
	}
	return plugui.Card("全部账号", plugui.Group(
		plugui.Fields(rows...),
		plugui.Notice("", "换账号：在地址后加 ?auth_index=<索引>；本插件的 pages 由 hub 的渠道总览与彼此之间的链接进入。"),
	),
		// Rendered even for a single account, because this is the only way to add
		// a SECOND one. The login page already understands the parameter
		// (`plugui.IsAddAccountRequest`); without this action that branch was
		// unreachable from the UI, exactly as in the sibling providers.
		plugui.Action{Label: "新建账号", Path: "login", Query: plugui.AddAccountQuery},
	)
}

// renderLoginPage renders the device-code login page.
//
// The page is a pure GET with no form and no script: `?action=start` requests a
// code and renders the authorisation link for the user to open, and
// `?action=poll&state=…` advances the flow by ONE upstream attempt. A completed
// login is saved through the host and reported on the same load.
//
// Every state is rendered through `page`, so the "this login adds an account"
// caveat rides along instead of being lost partway through the flow.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	addAccount := plugui.IsAddAccountRequest(request)
	action := strings.TrimSpace(request.Query.Get("action"))
	extra := addQuery(addAccount)

	page := func(cards ...template.HTML) pluginapi.ManagementResponse {
		body := make([]template.HTML, 0, len(cards)+1)
		if addAccount {
			body = append(body, plugui.Notice("", plugui.AddAccountNotice))
		}
		body = append(body, cards...)
		return pluguiPage("MiniMax Code 登录", body...)
	}

	if action == "" || action == "start" {
		session, errStart := startLoginSession(h, settings())
		if errStart != nil {
			return page(plugui.Card("无法发起登录", plugui.Notice("danger", errStart.Error())))
		}
		return page(plugui.Card("在浏览器中完成授权",
			plugui.Group(
				plugui.Fields(
					plugui.Field{Label: "授权链接", Value: session.loginURL()},
					plugui.Field{Label: "用户码", Value: session.Auth.UserCode},
					plugui.Field{Label: "轮询间隔", Value: itoaInt(session.currentIntervalSeconds()) + " 秒（服务端下发；slow_down 会加 5 秒）"},
					plugui.Field{Label: "有效期至", Value: jsonTime(session.ExpiresAt)},
				),
				plugui.Notice("", "设备码流程不需要本地回调端口：在浏览器打开上面的链接并确认，然后点下面的按钮取回凭据。"),
				// A "login is unverified" warning used to sit here. It is gone
				// because the flow has since completed against the real service
				// (see LoginVerified). What remains is the one real caveat, stated
				// where the user is about to act on it.
				plugui.Notice("warning", "⚠️ 服务端不下发账号标识（访问令牌不是 JWT，也没有 nickname），"+
					"所以每次「新建账号」都会得到一个**新的**凭据文件名 —— 同一账号重复添加会出现多条记录。"+
					"给已有账号换凭据请在状态页用「重新登录」，它带 auth_index，会覆盖同一条。"),
			),
			plugui.Action{Label: "我已授权，取回凭据", Path: "login",
				Query: "action=poll&state=" + session.stateValue() + extra, Kind: "primary"},
			plugui.Action{Label: "换一个授权链接", Path: "login", Query: "action=start" + extra},
			plugui.Action{Label: "返回状态页", Path: "status"},
		))
	}

	// Any other action is a poll: one attempt, then an outcome.
	wanted := strings.TrimSpace(request.Query.Get("state"))
	session, found := lookupLoginSession(wanted)
	if !found {
		return page(plugui.Card("登录会话已失效",
			plugui.Notice("warning", "登录会话不存在或已超时，请重新发起登录。"),
			plugui.Action{Label: "重新发起登录", Path: "login", Query: "action=start" + extra, Kind: "primary"},
			plugui.Action{Label: "返回状态页", Path: "status"},
		))
	}

	if status, message, credential, done := session.snapshot(); done {
		forgetLoginSession(wanted)
		if status == pluginapi.AuthLoginStatusSuccess && credential != nil {
			return page(loginSuccessCard(h, credential))
		}
		return page(plugui.Card("登录未完成",
			plugui.Notice("danger", firstNonEmpty(message, "授权未完成")),
			plugui.Action{Label: "重新发起登录", Path: "login", Query: "action=start" + extra, Kind: "primary"},
			plugui.Action{Label: "返回状态页", Path: "status"},
		))
	}

	if !session.due() {
		return page(waitingCard(session, wanted, extra,
			"距离上次轮询还不到 "+itoaInt(session.currentIntervalSeconds())+" 秒，稍后刷新本页即可。", ""))
	}
	session.markAttempt()
	decision, message, credential, errPoll := pollDeviceToken(h, settings(), session.Auth)
	switch decision {
	case pollSuccess:
		session.setCredential(credential, firstNonEmpty(message, "登录成功"))
		forgetLoginSession(wanted)
		return page(loginSuccessCard(h, credential))
	case pollPending:
		session.resetFailures()
		return page(waitingCard(session, wanted, extra,
			"还没有完成授权。请在浏览器打开下面的链接并确认，然后点按钮再试。", ""))
	case pollSlowDown:
		session.slowDown()
		return page(waitingCard(session, wanted, extra,
			"服务端要求放慢轮询。",
			"间隔已调整为 "+itoaInt(session.currentIntervalSeconds())+" 秒。"))
	}
	detail := firstNonEmpty(message, "MiniMax 授权失败")
	if errPoll != nil {
		detail = errPoll.Error()
	}
	session.fail(detail)
	forgetLoginSession(wanted)
	return page(plugui.Card("登录失败",
		plugui.Notice("danger", detail),
		plugui.Action{Label: "重新发起登录", Path: "login", Query: "action=start" + extra, Kind: "primary"},
		plugui.Action{Label: "返回状态页", Path: "status"},
	))
}

// waitingCard renders the "still authorising" state with the link and a retry.
func waitingCard(session *loginSession, state, extra, message, warning string) template.HTML {
	fragments := []template.HTML{plugui.Notice("", message)}
	if warning != "" {
		fragments = append(fragments, plugui.Notice("warning", warning))
	}
	fragments = append(fragments, plugui.Fields(
		plugui.Field{Label: "授权链接", Value: session.loginURL()},
		plugui.Field{Label: "用户码", Value: session.Auth.UserCode},
		plugui.Field{Label: "当前轮询间隔", Value: itoaInt(session.currentIntervalSeconds()) + " 秒"},
		plugui.Field{Label: "有效期至", Value: jsonTime(session.ExpiresAt)},
	))
	return plugui.Card("等待授权", plugui.Group(fragments...),
		plugui.Action{Label: "再试一次", Path: "login",
			Query: "action=poll&state=" + state + extra, Kind: "primary"},
		plugui.Action{Label: "返回状态页", Path: "status"},
	)
}

// loginSuccessCard saves a completed login and reports where it landed.
//
// The credential is persisted through the host, under the name the host will
// look for: deriving a second name here would leave the host's own record
// untouched and grow a duplicate entry for the same account.
func loginSuccessCard(h *abiboot.Host, credential *Credential) template.HTML {
	if credential == nil {
		return plugui.Card("登录未完成", plugui.Notice("danger", "登录流程没有返回凭据。"))
	}
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		return plugui.Card("凭据编码失败", plugui.Notice("danger", errAuth.Error()))
	}
	saved, errSave := h.SaveAuth(auth.FileName, auth.StorageJSON)
	if errSave != nil {
		return plugui.Card("保存凭据失败", plugui.Notice("danger", errSave.Error()))
	}
	return plugui.Card("登录成功", plugui.Group(
		plugui.Fields(
			plugui.Field{Label: "账号", Value: credential.displayLabel()},
			plugui.Field{Label: "凭据文件", Value: saved.Name},
			plugui.Field{Label: "有效期至", Value: formatExpiry(credential.Expiry())},
		),
		plugui.Notice("success", "凭据已写入 CPA 的 auth 目录。"),
	),
		plugui.Action{Label: "返回状态页", Path: "status", Kind: "primary"},
	)
}

// addQuery appends the add=1 marker when the login is for a new account.
func addQuery(addAccount bool) string {
	if addAccount {
		return "&" + plugui.AddAccountQuery
	}
	return ""
}

// renderCheckinCard renders the check-in outcome.
func renderCheckinCard(outcome credits.Outcome) template.HTML {
	tone := "danger"
	switch outcome.Status {
	case credits.StatusClaimed:
		tone = "success"
	case credits.StatusAlreadyClaimed, credits.StatusInactive:
		tone = "warning"
	}
	fields := []plugui.Field{
		{Label: "结果", Value: outcome.Status},
		{Label: "说明", Value: outcome.Message},
	}
	if outcome.Status == credits.StatusClaimed {
		fields = append(fields, plugui.Field{Label: "本次领取", Value: trimAmount(outcome.Amount) + " 积分"})
	}
	return plugui.Card("签到结果", plugui.Group(
		plugui.Notice(tone, outcome.Message),
		plugui.Fields(fields...),
		plugui.Notice("", "⚠️ 幂等由服务端的 claim_result 判定（1=本次领取、2=已领过），HTTP 状态码区分不出这两者。"+
			"额度取 points，bonus_points 已含在其中、不得相加。"),
	), plugui.Action{Label: "返回状态页", Path: "status", Kind: "primary"})
}

// checkinFailed renders a failed check-in.
func checkinFailed(message string) template.HTML {
	return plugui.Group(
		plugui.Card("签到失败", plugui.Notice("danger", message)),
		plugui.Card("返回", plugui.Notice("", "回到状态页可以查看账号与积分详情。"),
			plugui.Action{Label: "返回状态页", Path: "status", Kind: "primary"}),
	)
}

// formatExpiry renders an expiry instant for a page.
func formatExpiry(value time.Time) string {
	if value.IsZero() {
		return "未知（凭据未携带可解析的过期时间）"
	}
	return value.UTC().Format(time.RFC3339) + "（UTC）"
}

// yesNo renders a boolean for a page.
func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// itoa64 renders an int64 without pulling strconv into page code.
func itoa64(value int64) string { return fmt.Sprintf("%d", value) }
