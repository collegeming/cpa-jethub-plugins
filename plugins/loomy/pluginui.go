package main

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file renders the management pages a user sees inside CPA-Manager-Plus.
//
// The host exposes the Menu route on `/v0/resource/plugins/<id>/<path>` and
// CPAMP renders it in a same-origin iframe with the host theme injected as CSS
// custom properties. Two consequences shape everything here:
//
//   - the resource mount is dispatched as GET ONLY, so every action is a link
//     carrying a query string — never a form, and never JavaScript. That is why
//     the SMS code is entered on a keypad of links: an input field would need a
//     form, and the host would drop the submission;
//   - markup only consumes the host's CSS variables, so both light and dark
//     themes work with no frontend build.

// pluguiPage wraps body fragments in the themed document shell.
func pluguiPage(heading string, body ...template.HTML) pluginapi.ManagementResponse {
	return plugui.HTML(heading, body...)
}

// renderStatusPage renders the account overview, the model catalogue and the two
// point pools.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	body := []template.HTML{plugui.Card("插件设置", plugui.Fields(
		plugui.Field{Label: "推理端点", Value: APIBase + ChatCompletionsPath},
		plugui.Field{Label: "账号端点", Value: AccountBase},
		plugui.Field{Label: "模型目录", Value: catalogueModeText(cfg)},
		plugui.Field{Label: "模型数量", Value: itoaInt(len(fallbackCatalogue))},
		plugui.Field{Label: "登录页", Value: loginResourcePath},
		plugui.Field{Label: "会话续期", Value: "不支持：Loomy 没有 refresh 端点，" +
			"auth.refresh 只对 " + PointsRecordsPath + " 做一次有效性探测"},
	))}

	accounts := loomyAccounts(h)
	if len(accounts) == 0 {
		body = append(body, plugui.Card("尚未添加账号",
			plugui.Notice("warning", "当前实例还没有 Loomy 账号。Loomy 只有手机验证码登录："+
				"点下面的按钮进入登录页，先用链接发送验证码，再用键盘链接输入验证码。"),
			plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
		))
		return pluguiPage("Loomy", body...)
	}

	entry, found := selectAccount(h, request)
	if !found {
		body = append(body, plugui.Card("账号不存在",
			plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。")))
		return pluguiPage("Loomy", body...)
	}

	accountFields := []plugui.Field{
		{Label: "名称", Value: entry.Name},
		{Label: "状态", Value: statusText(entry)},
	}
	if entry.AuthIndex != "" {
		accountFields = append(accountFields, plugui.Field{Label: "索引", Value: entry.AuthIndex})
	}

	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		accountFields = append(accountFields, plugui.Field{Label: "凭据", Value: "无法读取：" + errCredential.Error()})
		body = append(body, plugui.Card("账号", plugui.Fields(accountFields...),
			plugui.Action{Label: "重新登录", Path: "login"},
			plugui.Action{Label: "新建账号", Path: "login", Query: plugui.AddAccountQuery}))
		return pluguiPage("Loomy", body...)
	}
	accountFields = append(accountFields,
		plugui.Field{Label: "手机号", Value: credential.maskedPhone()},
		plugui.Field{Label: "用户 ID", Value: emptyText(credential.UserID)},
		plugui.Field{Label: "有效期至", Value: formatExpiry(credential.Expiry())},
		plugui.Field{Label: "可自动续期", Value: "否（无 refresh_token，过期只能重新登录）"},
	)
	body = append(body, plugui.Card("账号", plugui.Fields(accountFields...),
		plugui.Action{Label: "重新登录", Path: "login"},
		plugui.Action{Label: "新建账号", Path: "login", Query: plugui.AddAccountQuery},
		plugui.Action{Label: "刷新每日额度", Path: "checkin"},
		plugui.Action{Label: "一次性任务", Path: "onboarding"},
	))

	body = append(body, pointsCard(h, credential, cfg))
	body = append(body, catalogueCard(h, credential, cfg))

	if switcher := renderAccountList(accounts, entry.AuthIndex); switcher != "" {
		body = append(body, switcher)
	}
	return pluguiPage("Loomy", body...)
}

// pointsCard renders the two point pools.
//
// The pools are displayed separately by explicit user requirement
// (`README.md:1471-1474`); a failed read is shown as a failure and never as 0
// (trap #11).
func pointsCard(h *abiboot.Host, credential *Credential, cfg Config) template.HTML {
	snapshot, errPoints := fetchPoints(h, credential, cfg)
	switch {
	case errPoints != nil:
		return plugui.Card("积分", plugui.Notice("danger", "查询失败："+errPoints.Error()))
	case snapshot == nil:
		return plugui.Card("积分", plugui.Notice("warning", "服务端未返回 balance 字段，无法给出积分数字（不显示 0）"))
	}
	fields := []plugui.Field{
		{Label: "永久积分", Value: trimAmount(snapshot.Balance)},
		{Label: "每日赠送", Value: trimAmount(snapshot.DailyBalance)},
		{Label: "可用合计", Value: trimAmount(snapshot.Available)},
	}
	if snapshot.DailyQuota != nil {
		fields = append(fields, plugui.Field{Label: "每日额度", Value: trimAmount(*snapshot.DailyQuota) + "（" + dailyQuotaDescription + "）"})
	}
	if snapshot.DailyConsumed != nil {
		fields = append(fields, plugui.Field{Label: "今日已用", Value: trimAmount(*snapshot.DailyConsumed)})
	}
	if snapshot.DailyCycleDate != "" {
		fields = append(fields, plugui.Field{Label: "额度日期", Value: snapshot.DailyCycleDate})
	}
	return plugui.Card("积分", plugui.Fields(fields...),
		plugui.Action{Label: "刷新每日额度", Path: "checkin"})
}

// catalogueCard renders the model catalogue currently in use.
func catalogueCard(h *abiboot.Host, credential *Credential, cfg Config) template.HTML {
	label := "内置兜底目录"
	if cfg.DiscoverModels {
		label = "实时目录（GET /models，失败时回退到内置目录）"
	}
	entries := activeCatalogue(h, credential, cfg)
	fields := []plugui.Field{
		{Label: "来源", Value: label},
		{Label: "数量", Value: itoaInt(len(entries))},
	}
	now := time.Now()
	infos := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, entry.info(now))
	}
	for _, info := range infos {
		modalities := strings.Join(info.SupportedInputModalities, "+")
		fields = append(fields, plugui.Field{
			Label: info.ID,
			Value: fmt.Sprintf("%s（上下文 %s，输入 %s）", info.DisplayName, trimAmount(float64(info.ContextLength)), modalities),
		})
	}
	return plugui.Card("模型目录", plugui.Fields(fields...))
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

// loginActionSession resolves the session a login-page action applies to.
//
// A page reached from the status page has no `state` yet, and a script may call
// `?action=send&phone=…` directly, so a session is created on demand. That keeps
// the page usable without ever leaving the GET-only mount.
func loginActionSession(request pluginapi.ManagementRequest) (*loginSession, bool) {
	if requested := strings.TrimSpace(request.Query.Get("state")); requested != "" {
		if session, found := lookupLoginSession(requested); found {
			return session, false
		}
	}
	if recent := lastLoginState(); recent != "" {
		if session, found := lookupLoginSession(recent); found {
			return session, false
		}
	}
	return startLoginSession(settings(), request.Query.Get("phone")), true
}

// renderLoginPage drives the SMS login.
//
// Every control is a link: `?action=start`, `?action=send&phone=…`,
// `?action=code&digit=…` and `?action=verify&phone=…&code=…`. Nothing here blocks
// waiting for the user, and nothing sends a form.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	action := strings.ToLower(strings.TrimSpace(request.Query.Get("action")))
	if action == "" {
		return loginIntroPage(cfg, request, nil)
	}

	var session *loginSession
	created := false
	if action == "start" {
		// `start` mints the session explicitly so every link on the page carries
		// the same state from the very first step.
		session = startLoginSession(cfg, cfg.defaultPhone(request.Query.Get("phone")))
		created = true
	} else {
		session, created = loginActionSession(request)
	}
	state := session.stateValue()
	phone := cfg.defaultPhone(request.Query.Get("phone"))

	switch action {
	case "start":
		return loginIntroPage(cfg, request, session)

	case "send":
		errSend := sendLoginCode(h, cfg, session, phone, time.Now())
		if errSend != nil {
			return loginFailedPage(state, phone, "验证码发送失败", errSend.Error())
		}
		return loginCodePage(cfg, session, phone, created)

	case "code", "digit":
		if !session.hasMsgID() {
			return loginFailedPage(state, phone, "请先发送验证码", "还没有发送验证码，msgid 为空。")
		}
		session.appendDigit(request.Query.Get("digit"))
		return loginCodePage(cfg, session, phone, created)

	case "clear":
		session.clearDigits()
		return loginCodePage(cfg, session, phone, created)

	case "verify":
		// Only an explicit `phone=` on the link is passed through; otherwise the
		// session's own number is used.
		credential, errVerify := verifyLoginCode(h, cfg, session, request.Query.Get("phone"), request.Query.Get("code"), time.Now())
		if errVerify != nil {
			return loginFailedPage(state, phone, "验证码校验失败", errVerify.Error())
		}
		return pluguiPage("Loomy 登录", plugui.Card("登录成功",
			plugui.Group(
				plugui.Notice("success", "账号已保存："+defaultAuthFileName(credential)),
				plugui.Fields(
					plugui.Field{Label: "手机号", Value: credential.maskedPhone()},
					plugui.Field{Label: "用户 ID", Value: emptyText(credential.UserID)},
					plugui.Field{Label: "有效期至", Value: formatExpiry(credential.Expiry())},
					plugui.Field{Label: "每日额度", Value: "已尝试初始化（写接口 /points/first-login，失败不影响登录）"},
				),
			),
			plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"},
			plugui.Action{Label: "再登一个账号", Query: "action=start"},
		))
	}
	return loginIntroPage(cfg, request, session)
}

// addAccountNotice is the two-line caveat the SMS login page shows when it was
// reached through 新建账号.
//
// Loomy identifies an account by its phone number (`defaultAuthFileName` uses
// `displayLabel`, i.e. the full number), so "another account" only means
// "another number": with the configured phone the flow would re-save the SAME
// account instead of adding one.
func addAccountNotice(request pluginapi.ManagementRequest) template.HTML {
	if !plugui.IsAddAccountRequest(request) {
		return ""
	}
	return plugui.Group(
		plugui.Notice("", plugui.AddAccountNotice),
		plugui.Notice("", "新增账号要换一个手机号：改插件设置里的 phone，或在链接后追加 ?phone=13xxxxxxxxx。"),
	)
}

// loginIntroPage is the entry page before a code has been requested. A session is
// passed in when one already exists, so the send link keeps the state.
func loginIntroPage(cfg Config, request pluginapi.ManagementRequest, session *loginSession) pluginapi.ManagementResponse {
	phone := cfg.defaultPhone(request.Query.Get("phone"))
	stateQuery := ""
	if session != nil {
		stateQuery = "&state=" + session.stateValue()
	}
	fields := []plugui.Field{
		{Label: "登录方式", Value: "手机短信验证码（Loomy 没有扫码回调，也没有 refresh_token）"},
		{Label: "验证码有效期", Value: itoaInt(cfg.smsCodeTTL()) + " 秒（向服务端声明）"},
		{Label: "会话有效期", Value: itoaInt(cfg.sessionTTL()) + " 秒（本地推算 expires_at）"},
		{Label: "短信接口", Value: AccountBase + SendMsgCodePath},
	}
	if phone == "" {
		return pluguiPage("Loomy 登录", plugui.Card("手机验证码登录",
			plugui.Group(
				addAccountNotice(request),
				plugui.Notice("warning", "本页只接受 GET 链接，没有输入框。请在插件设置里填写 phone，"+
					"或在链接上追加 ?phone=13800138000 后再发起登录。"),
				plugui.Fields(fields...),
			),
			plugui.Action{Label: "返回状态", Path: "status"},
		))
	}
	fields = append(fields, plugui.Field{Label: "将发送到", Value: phone})
	return pluguiPage("Loomy 登录", plugui.Card("手机验证码登录",
		plugui.Group(
			addAccountNotice(request),
			plugui.Notice("", "点击「发送验证码」调用 "+SendMsgCodePath+"；收到短信后用下面的数字链接输入验证码，"+
				"再点「提交验证」。整个流程都在这一个页面里完成。"),
			plugui.Fields(fields...),
		),
		plugui.Action{Label: "发送验证码", Query: "action=send&phone=" + phone + stateQuery, Kind: "primary"},
		plugui.Action{Label: "返回状态", Path: "status"},
	))
}

// loginCodePage is the keypad page shown after a code has been requested.
func loginCodePage(cfg Config, session *loginSession, phone string, created bool) pluginapi.ManagementResponse {
	state := session.stateValue()
	entered := session.accumulatedCode()
	shown := entered
	if shown == "" {
		shown = "（尚未输入）"
	}
	notice := "验证码已发送到 " + phone + "。用下面的数字链接依次点击 6 位验证码，然后点「提交验证」。"
	if created {
		notice = "已建立新的登录会话（state=" + state + "）。" + notice
	}
	actions := []plugui.Action{
		{Label: "提交验证", Query: "action=verify&state=" + state + "&phone=" + phone, Kind: "primary"},
		{Label: "重新发送", Query: "action=send&state=" + state + "&phone=" + phone},
		{Label: "清空", Query: "action=clear&state=" + state + "&phone=" + phone},
	}
	for _, digit := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "0"} {
		actions = append(actions, plugui.Action{Label: digit, Query: "action=code&state=" + state + "&phone=" + phone + "&digit=" + digit})
	}
	actions = append(actions, plugui.Action{Label: "返回状态", Path: "status"})
	return pluguiPage("Loomy 登录", plugui.Card("输入短信验证码",
		plugui.Group(
			plugui.Notice("", notice),
			plugui.Fields(
				plugui.Field{Label: "手机号", Value: phone},
				plugui.Field{Label: "已输入", Value: shown},
				plugui.Field{Label: "登录会话", Value: state + "（" + itoaInt(cfg.loginSessionTTL()/1000) + " 秒内有效）"},
				plugui.Field{Label: "脚本用法", Value: "?action=verify&state=" + state + "&phone=" + phone + "&code=123456"},
			),
		),
		actions...,
	))
}

// loginFailedPage renders a failed step with the links needed to continue.
func loginFailedPage(state, phone, title, message string) pluginapi.ManagementResponse {
	actions := []plugui.Action{
		{Label: "重试提交", Query: "action=verify&state=" + state + "&phone=" + phone, Kind: "primary"},
		{Label: "重新发送验证码", Query: "action=send&state=" + state + "&phone=" + phone},
		{Label: "返回状态", Path: "status"},
	}
	return pluguiPage("Loomy 登录", plugui.Card(title,
		plugui.Group(
			plugui.Notice("danger", message),
			plugui.Fields(plugui.Field{Label: "登录会话", Value: state}),
		),
		actions...,
	))
}

// renderCheckinPage initialises the daily granted pool.
//
// The call is a WRITE (`POST /points/first-login`), so a bare page load only
// shows a confirmation card; `?action=claim` performs it.
func renderCheckinPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return pluguiPage("Loomy 每日额度",
			plugui.Card("没有可用账号", plugui.Notice("warning", "请先登录一个 Loomy 账号。"),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"}))
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return pluguiPage("Loomy 每日额度",
			plugui.Card("无法读取凭据", plugui.Notice("danger", errCredential.Error()),
				plugui.Action{Label: "重新登录", Path: "login", Kind: "primary"}))
	}
	selector := ""
	if entry.AuthIndex != "" {
		selector = "&auth_index=" + entry.AuthIndex
	}
	if !strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "claim") {
		return pluguiPage("Loomy 每日额度",
			plugui.Card("初始化每日赠送额度",
				plugui.Group(
					plugui.Notice("", "这一步会调用写接口 POST "+PointsFirstLoginPath+"。"+
						"每日赠送额度按自然日刷新且不叠加；重复调用是幂等的，"+
						"服务端用响应里的 alreadyProcessed 表示今天已经初始化过。"),
					plugui.Fields(
						plugui.Field{Label: "账号", Value: entry.Name},
						plugui.Field{Label: "手机号", Value: credential.maskedPhone()},
						plugui.Field{Label: "说明", Value: dailyQuotaDescription},
					),
				),
				plugui.Action{Label: "立即初始化", Query: "action=claim" + selector, Kind: "primary"},
				plugui.Action{Label: "返回状态", Path: "status"},
			))
	}
	outcome := claimDailyGrant(h, credential, settings())
	fields := []plugui.Field{{Label: "账号", Value: entry.Name}}
	if outcome.Credit > 0 {
		fields = append(fields, plugui.Field{Label: "本次额度", Value: trimAmount(outcome.Credit)})
	}
	if outcome.Snapshot != nil {
		fields = append(fields,
			plugui.Field{Label: "永久积分", Value: trimAmount(outcome.Snapshot.Balance)},
			plugui.Field{Label: "每日赠送", Value: trimAmount(outcome.Snapshot.DailyBalance)})
		if outcome.Snapshot.DailyQuota != nil {
			fields = append(fields, plugui.Field{Label: "每日额度", Value: trimAmount(*outcome.Snapshot.DailyQuota)})
		}
	}
	tone := "success"
	switch outcome.Status {
	case "already-claimed":
		// A repeat returns HTTP 200 too, so the status comes from the BODY.
		tone = ""
	case "claimed":
		tone = "success"
	default:
		tone = "danger"
	}
	return pluguiPage("Loomy 每日额度",
		plugui.Card("初始化结果",
			plugui.Group(plugui.Notice(tone, outcome.Message), plugui.Fields(fields...)),
			plugui.Action{Label: "返回状态", Path: "status", Kind: "primary"},
			plugui.Action{Label: "再执行一次", Query: "action=claim" + selector},
		))
}

// renderOnboardingPage serves the one-off onboarding tasks.
//
// The status read is read-only (the page is opened freely); `?action=claim`
// completes everything still open, and `?action=complete&key=…` completes one
// task.
func renderOnboardingPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return pluguiPage("Loomy 一次性任务",
			plugui.Card("没有可用账号", plugui.Notice("warning", "请先登录一个 Loomy 账号。"),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"}))
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return pluguiPage("Loomy 一次性任务",
			plugui.Card("无法读取凭据", plugui.Notice("danger", errCredential.Error()),
				plugui.Action{Label: "重新登录", Path: "login", Kind: "primary"}))
	}
	cfg := settings()
	selector := ""
	if entry.AuthIndex != "" {
		selector = "&auth_index=" + entry.AuthIndex
	}

	action := strings.ToLower(strings.TrimSpace(request.Query.Get("action")))
	var notice template.HTML
	switch action {
	case "claim":
		outcome, errOutcome := claimAllOnboarding(h, credential, cfg)
		if errOutcome != nil {
			notice = plugui.Notice("danger", "领取失败："+errOutcome.Error())
		} else if len(outcome.Claimed) == 0 {
			notice = plugui.Notice("", outcome.Message)
		} else {
			notice = plugui.Notice("success", fmt.Sprintf("%s（本次 +%d，累计 %d/%d）",
				outcome.Message, sumClaimPoints(outcome.Claimed), outcome.Earned, outcome.Total))
		}
	case "complete":
		key := strings.TrimSpace(request.Query.Get("key"))
		already, errComplete := completeOnboardingTask(h, credential, key, cfg)
		switch {
		case errComplete != nil:
			notice = plugui.Notice("danger", "提交失败："+errComplete.Error())
		case already:
			notice = plugui.Notice("", "该任务此前已完成（alreadyCompleted=true，同样算成功）")
		default:
			notice = plugui.Notice("success", "任务已提交："+key)
		}
	}

	state, errState := fetchOnboardingState(h, credential, cfg)
	if errState != nil {
		return pluguiPage("Loomy 一次性任务",
			plugui.Card("任务状态读取失败",
				plugui.Group(notice, plugui.Notice("danger", errState.Error())),
				plugui.Action{Label: "重试", Path: "onboarding", Kind: "primary"},
				plugui.Action{Label: "返回状态", Path: "status"},
			))
	}

	actions := []plugui.Action{{Label: "返回状态", Path: "status"}}
	if len(state.Tasks) > 0 {
		actions = append([]plugui.Action{{Label: "一键完成全部", Query: "action=claim" + selector, Kind: "primary"}}, actions...)
	}
	fields := []plugui.Field{
		{Label: "已获得", Value: fmt.Sprintf("%d / %d", state.Earned, state.Total)},
	}
	if state.Balance != nil {
		fields = append(fields, plugui.Field{Label: "当前永久积分", Value: trimAmount(*state.Balance)})
	}
	taskFields := make([]plugui.Field, 0, len(onboardingTasks))
	for _, task := range onboardingTasks {
		status := "未完成"
		if state.Tasks[task.Key] {
			status = "已完成"
		}
		taskFields = append(taskFields, plugui.Field{
			Label: task.Title + "（" + task.Key + "）",
			Value: fmt.Sprintf("%s，+%d 积分", status, task.Points),
		})
	}
	body := []template.HTML{}
	if notice != "" {
		body = append(body, plugui.Card("执行结果", notice))
	}
	progressNote := plugui.Notice("", "任务完成条件由客户端判定，服务端不校验前提条件；"+
		"重复提交由响应里的 alreadyCompleted 表示，重放同样算成功。这是一次性能力，不参与每日签到。")
	body = append(body,
		plugui.Card("任务进度", plugui.Group(plugui.Fields(fields...), progressNote)),
		plugui.Card("任务清单", plugui.Fields(taskFields...), actions...),
	)
	return pluguiPage("Loomy 一次性任务", body...)
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

// catalogueModeText describes the active catalogue source.
func catalogueModeText(cfg Config) string {
	if cfg.DiscoverModels {
		return "实时拉取 GET /models（使用 token 头；失败时回退到内置目录）"
	}
	return "内置兜底目录（未开启 discover_models，不访问网络）"
}

// formatExpiry renders a local expiry with the remaining time.
func formatExpiry(expiry time.Time) string {
	if expiry.IsZero() {
		return "未知（凭据未带 expires_at，以服务端 100002 为准）"
	}
	remaining := time.Until(expiry)
	if remaining <= 0 {
		return expiry.Local().Format("2006-01-02 15:04") + "（本地推算已过期，实际以服务端探测为准）"
	}
	return fmt.Sprintf("%s（剩余 %s）", expiry.Local().Format("2006-01-02 15:04"), remaining.Truncate(time.Minute))
}

// emptyText renders an empty value readably.
func emptyText(value string) string {
	if strings.TrimSpace(value) == "" {
		return "（空）"
	}
	return value
}
