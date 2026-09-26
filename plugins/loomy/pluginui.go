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
			plugui.Notice("warning", "当前实例还没有 Loomy 账号。登录以微信扫码为主："+
				"点下面的按钮直接打开二维码页，用微信扫码确认即可，不需要手机号；"+
				"同一页也提供手机验证码登录作为备用路径。"),
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

	// One sweep, one card per account: each card carries that account's own
	// points, never the selected account's repeated.
	quotas := collectAccountQuotas(h, accounts, cfg)
	for _, quota := range quotas {
		body = append(body, renderQuotaCard(quota, quota.Entry.AuthIndex == entry.AuthIndex))
	}
	body = append(body, renderAccountList(accounts, entry.AuthIndex))

	// The catalogue is a property of the selected account's credential, so it
	// stays a single card for the selected one.
	if credential, errCredential := credentialOf(h, entry); errCredential == nil {
		body = append(body, catalogueCard(h, credential, cfg))
	}
	return pluguiPage("Loomy", body...)
}

// renderQuotaCard renders one account: its identity, its OWN point pools and its
// own actions.
//
// The card title names the account, so ten cards stay tellable apart, and every
// number shown belongs to the account in the title. The two pools are displayed
// separately (an explicit user requirement, `README.md:1471-1474`), and a failed
// read is stated as such — the card never falls back to 0 (trap #11).
func renderQuotaCard(quota accountQuota, current bool) template.HTML {
	entry := quota.Entry
	fields := []plugui.Field{{Label: "状态", Value: statusText(entry)}}
	if entry.AuthIndex != "" {
		fields = append(fields, plugui.Field{Label: "索引", Value: entry.AuthIndex})
	}
	if quota.CredentialErr != nil {
		fields = append(fields, plugui.Field{Label: "凭据", Value: "无法读取：" + quota.CredentialErr.Error()})
	} else {
		fields = append(fields,
			plugui.Field{Label: "手机号", Value: quota.Credential.maskedPhone()},
			plugui.Field{Label: "用户 ID", Value: emptyText(quota.Credential.UserID)},
			plugui.Field{Label: "有效期至", Value: formatExpiry(quota.Credential.Expiry())},
			plugui.Field{Label: "可自动续期", Value: "否（无 refresh_token，过期只能重新登录）"},
		)
		switch {
		case quota.PointsErr != nil:
			fields = append(fields, plugui.Field{Label: "积分", Value: "查询失败：" + quota.PointsErr.Error()})
		case quota.PointsNone:
			fields = append(fields, plugui.Field{Label: "积分", Value: "服务端未返回 balance 字段，无法给出积分数字（不显示 0）"})
		default:
			snapshot := quota.Snapshot
			fields = append(fields,
				plugui.Field{Label: "永久积分", Value: trimAmount(snapshot.Balance)},
				plugui.Field{Label: "每日赠送", Value: trimAmount(snapshot.DailyBalance)},
				plugui.Field{Label: "可用合计", Value: trimAmount(snapshot.Available)},
			)
			if snapshot.DailyQuota != nil {
				fields = append(fields, plugui.Field{Label: "每日额度", Value: trimAmount(*snapshot.DailyQuota) + "（" + dailyQuotaDescription + "）"})
			}
			if snapshot.DailyConsumed != nil {
				fields = append(fields, plugui.Field{Label: "今日已用", Value: trimAmount(*snapshot.DailyConsumed)})
			}
			if snapshot.DailyCycleDate != "" {
				fields = append(fields, plugui.Field{Label: "额度日期", Value: snapshot.DailyCycleDate})
			}
		}
	}
	title := "积分 · " + entry.Name
	if current {
		title += "（当前）"
	}
	return plugui.Card(title, plugui.Fields(fields...),
		plugui.Action{Label: "刷新每日额度", Path: "checkin", Query: accountQuery(entry), Kind: "primary"},
		plugui.Action{Label: "一次性任务", Path: "onboarding", Query: accountQuery(entry)},
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

// truthyParam reports whether a query value means "yes".
func truthyParam(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// loginPhoneFor resolves the number an SMS action works on: an explicit link
// value wins, then the configured default, then the number the user typed on the
// page's own keypad. The last step is what removes the old requirement to edit
// plugin settings before an SMS login.
func loginPhoneFor(cfg Config, session *loginSession, override string) string {
	if phone := cfg.defaultPhone(override); validPhone(phone) {
		return phone
	}
	if session == nil {
		return ""
	}
	if draft := normalizePhone(session.phoneDraftValue()); validPhone(draft) {
		return draft
	}
	return ""
}

// keypadActions renders the 0-9 links a form-free page uses to collect a number.
// An input field would need a form, which the resource mount drops.
func keypadActions(state, action, extra string) []plugui.Action {
	actions := make([]plugui.Action, 0, 10)
	for _, digit := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "0"} {
		query := "action=" + action + "&state=" + state + "&digit=" + digit
		if extra != "" {
			query += "&" + extra
		}
		actions = append(actions, plugui.Action{Label: digit, Query: query})
	}
	return actions
}

// renderLoginPage drives both login paths.
//
// The page's own route IS the primary path: `/login` with no action is the
// WeChat QR page, and `action=qr` spells that out. It shows a live QR code and
// walks the scan → confirm → (bind a phone) → credential state machine with one
// long-poll iteration per page load. The SMS flow stays available on the same
// page as the clearly secondary alternative and needs no configuration: its
// number is typed with the same digit links, and every control is a query-string
// link, never a form.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	action := strings.ToLower(strings.TrimSpace(request.Query.Get("action")))

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
	case "", "qr":
		return renderWechatQRPage(h, cfg, request, session, created)

	case "sms":
		return loginSMSPage(cfg, request, session, created)

	case bindActionSend:
		resolved := phone
		if resolved == "" {
			resolved = session.phoneDraftValue()
		}
		if errSend := sendWechatBindCode(h, cfg, session, resolved, time.Now()); errSend != nil {
			return wechatBindPage(cfg, session, plugui.Notice("danger", "绑定验证码发送失败："+errSend.Error()))
		}
		return wechatBindPage(cfg, session, plugui.Notice("success", "绑定验证码已发送到 "+session.wechatStateValue().BindPhone))

	case bindActionDigit:
		session.appendBindDigit(request.Query.Get("digit"))
		return wechatBindPage(cfg, session, "")

	case bindActionClear:
		session.clearBindDigits()
		return wechatBindPage(cfg, session, "")

	case bindActionVerify:
		credential, errVerify := verifyWechatBindCode(h, cfg, session, request.Query.Get("code"), time.Now())
		if errVerify != nil {
			return wechatBindPage(cfg, session, plugui.Notice("danger", "绑定失败："+errVerify.Error()))
		}
		return loginSuccessPage(session, credential, "微信扫码 + 绑定手机号")

	case bindActionRetry:
		// Retry the exchange from whatever the session still holds: the rcode if
		// bind/auth already succeeded, otherwise the one-time code.
		current := session.wechatStateValue()
		var outcome wechatOutcome
		if strings.TrimSpace(current.RCode) == "" {
			outcome = completeWechatCodeExchange(h, cfg, session, time.Now())
		} else {
			outcome = resumeWechatLogin(h, cfg, session, time.Now())
		}
		return wechatOutcomePage(cfg, request, session, outcome)

	case "send":
		resolved := loginPhoneFor(cfg, session, request.Query.Get("phone"))
		if errSend := sendLoginCode(h, cfg, session, resolved, time.Now()); errSend != nil {
			return loginFailedPage(state, emptyText(resolved), "验证码发送失败", errSend.Error())
		}
		return loginCodePage(cfg, session, resolved, created)

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
		return loginSuccessPage(session, credential, "手机验证码")

	case phoneActionDigit:
		session.appendPhoneDigit(request.Query.Get("digit"))
		return loginSMSPage(cfg, request, session, created)

	case phoneActionClear:
		session.clearPhoneDraft()
		return loginSMSPage(cfg, request, session, created)
	}
	// An unknown action is not an error page: the primary path is the QR page, so
	// that is where a stray link lands.
	return renderWechatQRPage(h, cfg, request, session, created)
}

// Page actions. They are query-string values on the one /login resource route;
// the bind trio is prefixed so a page can never confuse the SMS keypad with the
// phone-binding keypad.
const (
	// phoneActionDigit / phoneActionClear collect the phone number itself, for
	// the SMS path, so no plugin setting has to be edited to log in.
	phoneActionDigit = "phone"
	phoneActionClear = "phoneclear"
	// bindAction* drive the phone-binding sub-step a WeChat account without a
	// bound number needs (`loomy-oauth.ts:249-295`).
	bindActionSend   = "bindsend"
	bindActionVerify = "bindverify"
	bindActionDigit  = "binddigit"
	bindActionClear  = "bindclear"
	bindActionRetry  = "bindretry"
)

// addAccountNotice is the caveat a login page shows when it was reached through
// 新建账号.
//
// Loomy identifies an account by its phone number or, for a WeChat login without
// one, by the user id (`defaultAuthFileName`), so a second account needs a second
// identity — a second WeChat account, or another number.
func addAccountNotice(request pluginapi.ManagementRequest) template.HTML {
	if !plugui.IsAddAccountRequest(request) {
		return ""
	}
	return plugui.Group(
		plugui.Notice("", plugui.AddAccountNotice),
		plugui.Notice("", "新增账号要有另一个身份：用另一个微信号扫码即可；走短信路径时才需要另一个手机号，"+
			"号码可以直接在本页用数字链接输入，不需要改插件设置。"),
	)
}

// loginAlternativeCard is the SMS path as it appears on the QR page: clearly
// secondary, plainly worded, and always one click from a working entry point.
//
// It deliberately does NOT carry the digit keypad. That keypad lives on the SMS
// page (`?action=sms`), which does not poll; a keypad on a page that reloads
// itself every couple of seconds would make typing a number a race.
func loginAlternativeCard(cfg Config, request pluginapi.ManagementRequest, session *loginSession) template.HTML {
	state := session.stateValue()
	phone := loginPhoneFor(cfg, session, request.Query.Get("phone"))
	fields := []plugui.Field{
		{Label: "说明", Value: "备用路径：需要一个 11 位大陆手机号，验证码用数字链接输入"},
		{Label: "短信接口", Value: AccountBase + SendMsgCodePath},
		{Label: "验证码有效期", Value: itoaInt(cfg.smsCodeTTL()) + " 秒（向服务端声明）"},
	}
	actions := []plugui.Action{}
	if phone != "" {
		fields = append(fields, plugui.Field{Label: "将发送到", Value: phone})
		actions = append(actions, plugui.Action{Label: "发送验证码", Query: "action=send&state=" + state + "&phone=" + phone})
	}
	entry := "输入手机号并发送验证码"
	if phone != "" {
		entry = "改用手机验证码登录"
	}
	actions = append(actions, plugui.Action{Label: entry, Query: "action=sms&state=" + state})
	return plugui.Card("手机验证码登录（备用）", plugui.Group(
		addAccountNotice(request),
		plugui.Fields(fields...),
	), actions...)
}

// loginSMSPage is the SMS path's own page, reachable from the QR page and from a
// failure page. It never polls: the user is typing.
func loginSMSPage(cfg Config, request pluginapi.ManagementRequest, session *loginSession, created bool) pluginapi.ManagementResponse {
	notice := ""
	if created {
		notice = "已建立新的登录会话（state=" + session.stateValue() + "）。"
	}
	body := []template.HTML{}
	if notice != "" {
		body = append(body, plugui.Notice("", notice))
	}
	body = append(body, loginSMSCard(cfg, request, session))
	return pluguiPage("Loomy 手机验证码登录", body...)
}

// loginSMSCard renders the alternative path: a 11-digit number, then an SMS code,
// both entered with digit links. A configured or `?phone=` number is offered
// directly, but nothing forces the user to touch plugin settings.
func loginSMSCard(cfg Config, request pluginapi.ManagementRequest, session *loginSession) template.HTML {
	state := session.stateValue()
	phone := cfg.defaultPhone(request.Query.Get("phone"))
	if phone == "" {
		phone = normalizePhone(session.phoneDraftValue())
	}
	fields := []plugui.Field{
		{Label: "说明", Value: "备用路径：需要一个 11 位大陆手机号，短信验证码用数字链接输入"},
		{Label: "短信接口", Value: AccountBase + SendMsgCodePath},
		{Label: "验证码有效期", Value: itoaInt(cfg.smsCodeTTL()) + " 秒（向服务端声明）"},
	}
	actions := []plugui.Action{}
	if !validPhone(phone) {
		draft := session.phoneDraftValue()
		shown := draft
		if shown == "" {
			shown = "（尚未输入）"
		}
		fields = append(fields,
			plugui.Field{Label: "待输入号码", Value: shown},
			plugui.Field{Label: "输入方式", Value: "本页只有 GET 链接、没有输入框：点下面的数字键输入 11 位号码，" +
				"也可以直接在链接后追加 ?phone=13800138000"},
		)
		actions = append(actions, keypadActions(state, phoneActionDigit, "")...)
		if draft != "" {
			actions = append(actions, plugui.Action{Label: "清空号码", Query: "action=" + phoneActionClear + "&state=" + state})
		}
		if validPhone(draft) {
			actions = append([]plugui.Action{{
				Label: "发送验证码", Kind: "primary",
				Query: "action=send&state=" + state + "&phone=" + normalizePhone(draft),
			}}, actions...)
		}
		actions = append(actions, plugui.Action{Label: "微信扫码登录", Query: "action=qr&state=" + state, Kind: "primary"})
		return plugui.Card("手机验证码登录", plugui.Group(plugui.Fields(fields...), noticeForDraft(draft)), actions...)
	}
	fields = append(fields, plugui.Field{Label: "将发送到", Value: phone})
	actions = append(actions,
		plugui.Action{Label: "发送验证码", Query: "action=send&state=" + state + "&phone=" + phone, Kind: "primary"},
		plugui.Action{Label: "换一个号码", Query: "action=" + phoneActionClear + "&state=" + state},
		plugui.Action{Label: "微信扫码登录", Query: "action=qr&state=" + state},
	)
	return plugui.Card("手机验证码登录", plugui.Group(plugui.Fields(fields...)), actions...)
}

// noticeForDraft explains the state of the phone keypad without ever telling the
// user to go and edit the plugin configuration.
func noticeForDraft(draft string) template.HTML {
	switch {
	case draft == "":
		return plugui.Notice("", "先用下面的数字键输入手机号；输满 11 位后会出现「发送验证码」。")
	case len(draft) < 11:
		return plugui.Notice("", "已输入 "+itoaInt(len(draft))+" 位，还需要 "+itoaInt(11-len(draft))+" 位。")
	default:
		return plugui.Notice("", "号码已输满，点「发送验证码」继续。")
	}
}

// renderWechatQRPage is the primary login page: it shows the QR obtained from
// WeChat and performs ONE long-poll iteration per load.
func renderWechatQRPage(h *abiboot.Host, cfg Config, request pluginapi.ManagementRequest, session *loginSession, created bool) pluginapi.ManagementResponse {
	if truthyParam(request.Query.Get("fresh")) {
		// 换一张 / 重新开始: drop the old QR, its code and its binding context.
		session.updateWechat(func(w *wechatState) { *w = wechatState{} })
	}
	outcome := advanceWechatQR(h, cfg, session, time.Now())
	if created && outcome.Stage != stageWechatDone {
		outcome.Message = "已建立新的登录会话（state=" + session.stateValue() + "）。" + outcome.Message
	}
	return wechatOutcomePage(cfg, request, session, outcome)
}

// wechatOutcomePage renders whatever the flow decided.
func wechatOutcomePage(cfg Config, request pluginapi.ManagementRequest, session *loginSession, outcome wechatOutcome) pluginapi.ManagementResponse {
	switch outcome.Stage {
	case stageWechatDone:
		return loginSuccessPage(session, outcome.Credential, "微信扫码")
	case stageWechatBind:
		return wechatBindPage(cfg, session, plugui.Notice("", outcome.Message))
	case stageWechatFailed:
		return wechatFailedPage(session, outcome.Message)
	default:
		return wechatQRCardPage(cfg, request, session, outcome)
	}
}

// wechatQRCardPage renders the QR plus the notice for the current stage. It
// reloads itself (meta refresh) exactly when the flow is still moving.
func wechatQRCardPage(cfg Config, request pluginapi.ManagementRequest, session *loginSession, outcome wechatOutcome) pluginapi.ManagementResponse {
	current := session.wechatStateValue()
	tone := ""
	switch outcome.Stage {
	case wechatScanned:
		tone = "success"
	case wechatCancelled, wechatError:
		tone = "warning"
	case wechatExpired:
		tone = "warning"
	}
	fields := []plugui.Field{
		{Label: "二维码状态", Value: wechatStageText(outcome.Stage)},
		{Label: "登录会话", Value: session.stateValue() + "（" + itoaInt(cfg.loginSessionTTL()/1000) + " 秒内有效）"},
		{Label: "微信应用", Value: WechatAppID + "（微信开放平台网站应用）"},
		{Label: "回调地址", Value: WechatRedirectURI +
			"（微信域名白名单占位：它本身 404，授权码由长轮询取得，不经过回调页）"},
		{Label: "状态接口", Value: WechatLongPollURL},
	}
	if current.Last != "" {
		fields = append(fields, plugui.Field{Label: "上次返回码", Value: "wx_errcode=" + current.Last})
	}
	if current.Detail != "" {
		fields = append(fields, plugui.Field{Label: "诊断", Value: current.Detail})
	}
	fields = append(fields,
		plugui.Field{Label: "刷新方式", Value: refreshDescription(outcome.RefreshSeconds)},
		plugui.Field{Label: "超时预算", Value: "授权页/二维码 " + itoaInt(WechatPageTimeoutMS/1000) + " 秒、" +
			"长轮询 " + itoaInt(WechatPollTimeoutMS/1000) + " 秒（参考实现的预算；实际传输超时由宿主控制，" +
			"微信长轮询自身约 25 秒返回）"},
		plugui.Field{Label: "状态语义", Value: "408 待扫码 / 404 已扫码继续轮询 / 405 已确认（授权码在这一帧）" +
			"/ 403 已取消 / 402 已过期"},
	)
	actions := []plugui.Action{
		{Label: "换一张二维码", Query: "action=qr&fresh=1&state=" + session.stateValue()},
		{Label: "返回状态", Path: "status"},
	}
	card := plugui.Card("微信扫码登录（推荐，不需要手机号）",
		plugui.Group(
			plugui.Notice(tone, outcome.Message),
			plugui.Image(current.Image, "微信扫码登录二维码", 220),
			plugui.Fields(fields...),
		),
		actions...,
	)
	alternative := loginAlternativeCard(cfg, request, session)
	if outcome.RefreshSeconds > 0 {
		return plugui.HTMLWithRefresh(outcome.RefreshSeconds, "Loomy 微信登录", card, alternative)
	}
	return pluguiPage("Loomy 微信登录", card, alternative)
}

// refreshDescription spells out what the browser is about to do.
func refreshDescription(seconds int) string {
	if seconds <= 0 {
		return "本页不再自动刷新：流程已经结束，需要继续时点下面的按钮"
	}
	return "meta refresh 每 " + itoaInt(seconds) + " 秒重新加载本页；每次加载只做一次长轮询，" +
		"长轮询本身会阻塞到微信返回或超时"
}

// wechatStageText renders a stage for humans.
func wechatStageText(stage string) string {
	switch stage {
	case wechatWaiting:
		return "等待扫码"
	case wechatScanned:
		return "已扫码，等待手机确认"
	case wechatConfirmed:
		return "已确认，正在换取会话"
	case wechatCancelled:
		return "已取消"
	case wechatExpired:
		return "已过期（已自动换新）"
	case wechatError:
		return "长轮询异常（会自动重试）"
	case stageWechatBind:
		return "需要绑定手机号"
	case stageWechatFailed:
		return "失败"
	case stageWechatDone:
		return "已完成"
	default:
		return emptyText(stage)
	}
}

// wechatBindPage is the phone-binding sub-step: the WeChat account has no phone
// on the 讯飞 side, so one SMS round binds it. The keypad is the same form-free
// pattern the SMS path uses.
func wechatBindPage(cfg Config, session *loginSession, notice template.HTML) pluginapi.ManagementResponse {
	current := session.wechatStateValue()
	state := session.stateValue()
	fields := []plugui.Field{
		{Label: "微信昵称", Value: emptyText(current.Nickname)},
		{Label: "登录会话", Value: state},
		{Label: "绑定接口", Value: AccountBase + BindSendMsgPath + " / " + BindCheckCodePath},
	}
	actions := []plugui.Action{}
	cardTitle := "绑定手机号并登录"
	switch {
	case strings.TrimSpace(current.BindMsgID) == "":
		draft := session.phoneDraftValue()
		shown := draft
		if shown == "" {
			shown = "（尚未输入）"
		}
		fields = append(fields,
			plugui.Field{Label: "待绑定号码", Value: shown},
			plugui.Field{Label: "输入方式", Value: "点数字键输入 11 位号码（本页没有输入框），也可以直接在链接后追加 ?phone=13800138000"},
		)
		actions = append(actions, keypadActions(state, phoneActionDigit, "")...)
		if draft != "" {
			actions = append(actions, plugui.Action{Label: "清空号码", Query: "action=" + phoneActionClear + "&state=" + state})
		}
		actions = append([]plugui.Action{{
			Label: "发送绑定验证码", Kind: "primary",
			Query: "action=" + bindActionSend + "&state=" + state + "&phone=" + normalizePhone(draft),
		}}, actions...)
	default:
		entered := current.BindDigits
		if entered == "" {
			entered = "（尚未输入）"
		}
		fields = append(fields,
			plugui.Field{Label: "手机号", Value: current.BindPhone},
			plugui.Field{Label: "已输入", Value: entered},
			plugui.Field{Label: "脚本用法", Value: "?action=" + bindActionVerify + "&state=" + state + "&code=123456"},
		)
		actions = append(actions, keypadActions(state, bindActionDigit, "")...)
		actions = append([]plugui.Action{
			{Label: "提交绑定并登录", Kind: "primary", Query: "action=" + bindActionVerify + "&state=" + state},
			{Label: "重新发送", Query: "action=" + bindActionSend + "&state=" + state + "&phone=" + current.BindPhone},
			{Label: "清空", Query: "action=" + bindActionClear + "&state=" + state},
		}, actions...)
	}
	actions = append(actions,
		plugui.Action{Label: "重新扫码", Query: "action=qr&fresh=1&state=" + state},
		plugui.Action{Label: "返回状态", Path: "status"},
	)
	body := []template.HTML{
		plugui.Notice("", "这个微信号在讯飞侧还没有手机号，绑定一次即可（之后的扫码登录不再需要）。"),
	}
	if notice != "" {
		body = append([]template.HTML{notice}, body...)
	}
	body = append(body, plugui.Fields(fields...))
	return pluguiPage("Loomy 绑定手机号", plugui.Card(cardTitle, plugui.Group(body...), actions...))
}

// wechatFailedPage renders a terminal failure with the two ways forward.
func wechatFailedPage(session *loginSession, message string) pluginapi.ManagementResponse {
	state := session.stateValue()
	current := session.wechatStateValue()
	fields := []plugui.Field{{Label: "登录会话", Value: state}}
	if current.RCode != "" {
		fields = append(fields, plugui.Field{Label: "绑定上下文", Value: "已取得 rcode，可以重试，不必重新扫码"})
	}
	return pluguiPage("Loomy 微信登录", plugui.Card("微信登录未完成",
		plugui.Group(
			plugui.Notice("danger", message),
			plugui.Fields(fields...),
		),
		plugui.Action{Label: "重试这一步", Query: "action=" + bindActionRetry + "&state=" + state, Kind: "primary"},
		plugui.Action{Label: "重新获取二维码", Query: "action=qr&fresh=1&state=" + state},
		plugui.Action{Label: "改用手机验证码", Query: "action=sms&state=" + state},
		plugui.Action{Label: "返回状态", Path: "status"},
	))
}

// loginSuccessPage reports a stored credential. It never auto-refreshes: the
// login is over, and `auth.login.poll` may already have consumed the session.
func loginSuccessPage(session *loginSession, credential *Credential, method string) pluginapi.ManagementResponse {
	fileName := ""
	if session != nil {
		fileName = session.storedFileName()
	}
	if fileName == "" && credential != nil {
		fileName = defaultAuthFileName(credential)
	}
	fields := []plugui.Field{
		{Label: "登录方式", Value: method},
		{Label: "凭据文件", Value: emptyText(fileName)},
		{Label: "有效期至", Value: formatExpiry(credential.Expiry())},
		{Label: "每日额度", Value: "已尝试初始化（写接口 /points/first-login，失败不影响登录）"},
	}
	if credential != nil {
		fields = append([]plugui.Field{
			{Label: "手机号", Value: credential.maskedPhone()},
			{Label: "用户 ID", Value: emptyText(credential.UserID)},
		}, fields...)
	}
	notice := "账号已保存"
	if credential != nil && normalizePhone(credential.Phone) == "" {
		notice = "账号已保存。这个账号在讯飞侧没有返回手机号（微信 bind/skip 的正常结果），凭据本身完全可用"
	}
	return pluguiPage("Loomy 登录", plugui.Card("登录成功",
		plugui.Group(
			plugui.Notice("success", notice),
			plugui.Fields(fields...),
		),
		plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"},
		plugui.Action{Label: "再登一个账号", Path: "login", Query: plugui.AddAccountQuery},
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
