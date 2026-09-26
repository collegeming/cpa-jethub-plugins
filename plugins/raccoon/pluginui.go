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
// The host exposes every route as a Menu-less resource on
// `/v0/resource/plugins/<id>/<path>` and CPAMP renders it in a same-origin
// iframe with the host theme injected as CSS custom properties. Two consequences
// shape everything here:
//
//   - the resource mount is dispatched as GET ONLY, so every action is a link
//     carrying a query string — never a form, and never JavaScript. The QR page
//     advances itself with `<meta http-equiv="refresh">`, one poll per load;
//   - markup only consumes the host's CSS variables, so both light and dark
//     themes work with no frontend build.

// smsLoginNotice is the honest, one-line statement about the missing SMS path.
//
// The Aliyun `captcha_param` every `send_sms` requires can only be minted by
// Aliyun's browser JS after a human solves a slider (`raccoon-login-page.ts:417,
// 505-524`, spec §2.3). There is no headless path, so the plugin does not ship a
// button that could only ever fail.
const smsLoginNotice = "手机号验证码登录需要人机验证（阿里云滑块），只有浏览器里由人手动完成才能取得凭据，" +
	"本插件无法做到，因此只提供微信扫码登录。"

// pluguiPage wraps body fragments in the themed document shell.
func pluguiPage(heading string, body ...template.HTML) pluginapi.ManagementResponse {
	return plugui.HTML(heading, body...)
}

// renderStatusPage renders the account overview, the catalogue and one points
// card per account.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	body := []template.HTML{plugui.Card("插件设置", plugui.Fields(
		plugui.Field{Label: "推理端点", Value: APIBase + ChatCompletionsPath},
		plugui.Field{Label: "模型目录", Value: catalogueModeText(cfg)},
		plugui.Field{Label: "兜底模型数", Value: itoaInt(len(fallbackCatalogue))},
		plugui.Field{Label: "登录页", Value: loginResourcePath},
		plugui.Field{Label: "登录方式", Value: "仅微信扫码（" + smsLoginNotice + "）"},
		plugui.Field{Label: "会话续期", Value: "支持：POST " + RefreshPath + "，到期前 " +
			itoaInt(cfg.refreshWindow()) + " 秒提前续期；401 / code 200003 视为登录态终止"},
		plugui.Field{Label: "每日签到", Value: "没有：每日积分由服务端自动发放，没有接口可调用；" +
			"唯一可领取的是一次性桌面端登录奖励（" + LoginPointsGrantPath + "）"},
	))}

	accounts := raccoonAccounts(h)
	if len(accounts) == 0 {
		body = append(body, plugui.Card("尚未添加账号",
			plugui.Notice("warning", "当前实例还没有 Raccoon 账号。登录只能微信扫码：点下面的按钮直接打开二维码页，"+
				"用微信扫码并在手机上确认即可，不需要手机号。"),
			plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
		))
		return pluguiPage("Raccoon", body...)
	}

	entry, found := selectAccount(h, request)
	if !found {
		body = append(body, plugui.Card("账号不存在",
			plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。")))
		return pluguiPage("Raccoon", body...)
	}

	// One sweep, one card per account: each card carries that account's own
	// points, never the selected account's repeated.
	quotas := collectAccountQuotas(h, accounts, cfg)
	for _, quota := range quotas {
		body = append(body, renderQuotaCard(quota, quota.Entry.AuthIndex == entry.AuthIndex))
	}
	body = append(body, renderAccountList(accounts, entry.AuthIndex))

	if credential, _, errCredential := credentialOf(h, entry); errCredential == nil {
		body = append(body, catalogueCard(h, credential, cfg))
	}
	return pluguiPage("Raccoon", body...)
}

// renderQuotaCard renders one account: its identity, its OWN point pools and its
// own actions.
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
			plugui.Field{Label: "组织标识", Value: emptyText(quota.Credential.OfficeIdentity)},
			plugui.Field{Label: "有效期至", Value: formatExpiry(quota.Credential.Expiry())},
			plugui.Field{Label: "可自动续期", Value: refreshableText(quota.Credential)},
		)
		switch {
		case quota.PointsErr != nil:
			fields = append(fields, plugui.Field{Label: "积分", Value: "查询失败：" + quota.PointsErr.Error()})
		case quota.PointsNone:
			fields = append(fields, plugui.Field{Label: "积分", Value: "服务端未返回 available_points 字段，无法给出积分数字（不显示 0）"})
		default:
			pools := quota.Snapshot.pools()
			for _, pool := range pools {
				fields = append(fields, plugui.Field{Label: pool.Name, Value: trimAmount(pool.Amount)})
			}
		}
	}
	title := "积分 · " + entry.Name
	if current {
		title += "（当前）"
	}
	return plugui.Card(title, plugui.Fields(fields...),
		plugui.Action{Label: "一次性登录奖励", Path: "reward", Query: accountQuery(entry), Kind: "primary"},
		plugui.Action{Label: "重新登录", Path: "login", Query: accountQuery(entry)},
	)
}

// refreshableText states whether the account can renew itself.
func refreshableText(credential *Credential) string {
	if credential != nil && credential.Refreshable() {
		return "是（refresh_token 有效；续期后 access_token 会轮换）"
	}
	return "否（凭据没有 refresh_token，过期只能重新微信扫码）"
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
		label = "实时目录（GET " + ModelCatalogPath + "，失败时回退到内置目录）"
	}
	entries := activeCatalogue(h, credential, cfg)
	fields := []plugui.Field{
		{Label: "来源", Value: label},
		{Label: "数量", Value: itoaInt(len(entries))},
	}
	now := time.Now()
	for _, entry := range entries {
		info := entry.info(now)
		modalities := strings.Join(info.SupportedInputModalities, "+")
		fields = append(fields, plugui.Field{
			Label: info.ID,
			Value: fmt.Sprintf("%s（上下文 %s，最大输出 %s，输入 %s）",
				entry.displayName(), trimAmount(float64(info.ContextLength)),
				trimAmount(float64(info.MaxCompletionTokens)), modalities),
		})
	}
	return plugui.Card("模型目录（价格显示在名称里，x1 也会显示）", plugui.Fields(fields...))
}

// renderAccountList renders the switcher across accounts.
//
// It is rendered for a single account as well, because it carries 新建账号 — the
// only way to add a SECOND account from this page.
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

// loginActionSession resolves the session a login-page action applies to and
// reports whether it was just created.
//
// A page reached from the status page has no `state` yet, so a session is
// created on demand. That keeps the page usable without ever leaving the
// GET-only mount. A freshly created session renders WITHOUT polling: its code
// has not been shown to anyone yet.
func loginActionSession(request pluginapi.ManagementRequest) (*loginSession, bool, error) {
	if requested := strings.TrimSpace(request.Query.Get("state")); requested != "" {
		if session, found := lookupLoginSession(requested); found {
			return session, false, nil
		}
	}
	if recent := lastLoginState(); recent != "" {
		if session, found := lookupLoginSession(recent); found {
			return session, false, nil
		}
	}
	session, errStart := startLoginSession(settings())
	return session, true, errStart
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

// renderLoginPage drives the WeChat QR flow.
//
// The page's own route IS the flow: `/login` (or `?action=qr`) shows a live QR
// code and advances it with one poll per page load. `?action=qr&fresh=1` mints a
// new code without polling.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	action := strings.ToLower(strings.TrimSpace(request.Query.Get("action")))
	state := strings.TrimSpace(request.Query.Get("state"))
	addAccount := plugui.IsAddAccountRequest(request)

	var session *loginSession
	var errStart error
	created := false
	if action == "start" {
		session, errStart = startLoginSession(cfg)
		created = errStart == nil
	} else if state != "" {
		if found, ok := lookupLoginSession(state); ok {
			session = found
		}
	}
	if session == nil && errStart == nil {
		session, created, errStart = loginActionSession(request)
	}
	if errStart != nil || session == nil {
		message := "无法创建登录会话"
		if errStart != nil {
			message = errStart.Error()
		}
		return pluguiPage("Raccoon 微信登录", plugui.Card("登录不可用",
			plugui.Notice("danger", message),
			plugui.Action{Label: "返回状态", Path: "status", Kind: "primary"},
		))
	}

	// One poll per page load, except on the load that minted the session and on
	// the explicit "换一张二维码" action.
	// A session that already produced a credential renders its result: polling
	// the consumed code again would be a wasted vendor call.
	if status, _, credential, done := session.snapshot(); done && status == pluginapi.AuthLoginStatusSuccess && credential != nil {
		return loginSuccessPage(session, credential, addAccount)
	}
	outcome := advanceQR(h, cfg, session, created || truthyParam(request.Query.Get("fresh")))
	current := session.qrStateValue()
	stateValue := session.stateValue()

	if outcome.Stage == "done" {
		return loginSuccessPage(session, outcome.Credential, addAccount)
	}

	fields := []plugui.Field{
		{Label: "登录会话", Value: stateValue},
		{Label: "扫码状态", Value: qrStageText(outcome.Stage)},
		{Label: "二维码内容", Value: current.Content},
		{Label: "刷新方式", Value: "meta refresh 每 " + itoaInt(outcome.RefreshSeconds) + " 秒重新加载本页；" +
			"每次加载只做一次轮询，本页没有表单也没有脚本"},
	}
	if current.Rotations > 0 {
		fields = append(fields, plugui.Field{Label: "换码次数", Value: itoaInt(current.Rotations) + "（取消后会自动换新码）"})
	}
	if current.ExpiredAt != "" {
		fields = append(fields, plugui.Field{Label: "服务端过期时间", Value: current.ExpiredAt})
	}
	if current.Detail != "" {
		fields = append(fields, plugui.Field{Label: "最近一次轮询", Value: current.Detail})
	}
	body := []template.HTML{}
	if addAccount {
		body = append(body, plugui.Notice("", plugui.AddAccountNotice))
	}
	body = append(body,
		plugui.Notice(toneForStage(outcome.Stage), outcome.Message),
		plugui.Image(current.Image, "微信扫码登录二维码", 220),
		plugui.Fields(fields...),
		plugui.Notice("", smsLoginNotice+" 二维码由本插件本地生成（code 为本地随机 32 位十六进制），"+
			"扫码后由服务端把该 code 置为成功；插件不会读取任何厂商客户端文件。"),
	)
	card := plugui.Card("微信扫码登录（推荐，不需要手机号）", plugui.Group(body...),
		plugui.Action{Label: "换一张二维码", Query: "action=qr&fresh=1&state=" + stateValue},
		plugui.Action{Label: "返回状态", Path: "status"},
	)
	if outcome.RefreshSeconds > 0 {
		return plugui.HTMLWithRefresh(outcome.RefreshSeconds, "Raccoon 微信登录", card)
	}
	return pluguiPage("Raccoon 微信登录", card)
}

// qrStageText renders a stage for humans.
func qrStageText(stage string) string {
	switch stage {
	case qrStatusPending:
		return "等待扫码"
	case qrStatusLogging:
		return "已扫码，等待手机确认"
	case qrStatusCanceled:
		return "已取消（已自动换新码）"
	case "done":
		return "已完成"
	case "failed", "error":
		return "失败"
	default:
		return emptyText(stage)
	}
}

// toneForStage maps a stage onto a notice tone.
func toneForStage(stage string) string {
	switch stage {
	case "done":
		return "success"
	case "failed", "error":
		return "danger"
	case qrStatusCanceled:
		return "warning"
	default:
		return ""
	}
}

// loginSuccessPage reports a stored credential. It never auto-refreshes: the
// login is over.
func loginSuccessPage(session *loginSession, credential *Credential, addAccount bool) pluginapi.ManagementResponse {
	fileName := ""
	if session != nil {
		fileName = session.storedFileName()
	}
	if fileName == "" && credential != nil {
		fileName = defaultAuthFileName(credential)
	}
	fields := []plugui.Field{
		{Label: "登录方式", Value: "微信扫码"},
		{Label: "凭据文件", Value: emptyText(fileName)},
		{Label: "有效期至", Value: formatExpiry(credential.Expiry())},
		{Label: "一次性登录奖励", Value: "未自动领取：请在「一次性登录奖励」页显式领取（每号一次）"},
	}
	if credential != nil {
		fields = append([]plugui.Field{
			{Label: "手机号", Value: credential.maskedPhone()},
			{Label: "用户 ID", Value: emptyText(credential.UserID)},
		}, fields...)
	}
	body := []template.HTML{plugui.Notice("success", "账号已保存")}
	if addAccount {
		body = append(body, plugui.Notice("", plugui.AddAccountNotice))
	}
	body = append(body, plugui.Fields(fields...))
	return pluguiPage("Raccoon 登录", plugui.Card("登录成功", plugui.Group(body...),
		plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"},
		plugui.Action{Label: "领取一次性登录奖励", Path: "reward", Query: "auth_index=" + sessionAuthIndex(session)},
		plugui.Action{Label: "再登一个账号", Path: "login", Query: plugui.AddAccountQuery},
	))
}

// sessionAuthIndex names the freshly written account in a link.
func sessionAuthIndex(session *loginSession) string {
	if session == nil {
		return ""
	}
	// The host addresses credentials by name (the auth file), which is what the
	// session already holds; an index is assigned by the host on its next load.
	return session.storedFileName()
}

// renderRewardPage serves the ONE-OFF desktop login reward.
//
// The status read is read-only (the page may be opened freely); `?action=grant`
// performs the write. It is deliberately NOT part of any "claim everything"
// path: the reward is per account and per lifetime, and the harness's other
// providers do have daily check-ins — mixing them would misreport this one.
func renderRewardPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return pluguiPage("Raccoon 一次性登录奖励",
			plugui.Card("没有可用账号", plugui.Notice("warning", "请先微信扫码登录一个 Raccoon 账号。"),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"}))
	}
	credential, freshness, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return pluguiPage("Raccoon 一次性登录奖励",
			plugui.Card("无法读取凭据", plugui.Notice("danger", errCredential.Error()),
				plugui.Action{Label: "重新登录", Path: "login", Kind: "primary"}))
	}
	if freshness.Expired && freshness.Err != nil {
		return pluguiPage("Raccoon 一次性登录奖励",
			plugui.Card("凭据已过期", plugui.Notice("danger", "自动续期失败："+freshness.Err.Error()),
				plugui.Action{Label: "重新登录", Path: "login", Kind: "primary"}))
	}
	cfg := settings()
	selector := accountQuery(entry)

	var notice template.HTML
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "grant") {
		outcome := grantLoginReward(h, credential, cfg)
		tone := "success"
		switch outcome.Kind {
		case "already-claimed":
			tone = ""
		case "claimed":
			tone = "success"
		default:
			tone = "danger"
		}
		notice = plugui.Notice(tone, outcome.Message)
	}

	status := fetchRewardStatus(h, credential, cfg)
	state := "未领取"
	if status.Claimed {
		state = "已领取"
	}
	fields := []plugui.Field{
		{Label: "账号", Value: entry.Name},
		{Label: "奖励类型", Value: "一次性桌面端登录奖励（每号一次，不是每日签到）"},
		{Label: "状态", Value: state + "（来源：账单 " + LoginRewardBizType + " + " + LoginRewardEventName + "）"},
		{Label: "金额", Value: trimAmount(float64(status.Points)) + " 积分"},
		{Label: "接口", Value: "POST " + LoginPointsGrantPath + "（必须带 X-Client-Platform: " + ClientPlatform + "）"},
		{Label: "幂等性", Value: "由服务端 granted 标记决定：重复调用返回 HTTP 200 + granted=false，" +
			"插件据此显示「已领取」而不是「领取成功」"},
	}
	if status.Note != "" {
		fields = append(fields, plugui.Field{Label: "账单读取", Value: status.Note})
	}
	body := []template.HTML{}
	if notice != "" {
		body = append(body, notice)
	}
	body = append(body,
		plugui.Fields(fields...),
		plugui.Notice("warning", "Raccoon 没有每日签到：每日 300 积分由服务端自动发放，没有可调用的接口，"+
			"因此这里不提供、也不会参与任何「一键领取全部」动作。"),
	)
	actions := []plugui.Action{
		{Label: "领取登录奖励", Query: "action=grant" + selector, Kind: "primary"},
		{Label: "刷新状态", Query: selector},
		{Label: "返回状态", Path: "status"},
	}
	return pluguiPage("Raccoon 一次性登录奖励", plugui.Card("桌面端登录奖励（一次性）", plugui.Group(body...), actions...))
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
		return "实时拉取 GET " + ModelCatalogPath + "（超时 " + itoaInt(cfg.catalogueTimeout()) + " ms；失败时回退到内置目录）"
	}
	return "内置兜底目录（未开启 discover_models，不访问网络）"
}

// formatExpiry renders a local expiry with the remaining time.
func formatExpiry(expiry time.Time) string {
	if expiry.IsZero() {
		return "未知（凭据未带 expires_at，JWT 也无法解析，以服务端 401 为准）"
	}
	remaining := time.Until(expiry)
	if remaining <= 0 {
		return expiry.Local().Format("2006-01-02 15:04") + "（本地推算已过期，实际以服务端为准）"
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
