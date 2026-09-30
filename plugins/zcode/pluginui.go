package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file renders the management pages a user sees inside CPA-Manager-Plus.
//
// The host exposes every route as a Menu-less resource on
// `/v0/resource/plugins/<id>/<path>` and CPAMP renders it in a same-origin
// iframe with the host theme injected as CSS custom properties. Two consequences
// shape everything here:
//
//   - the resource mount is dispatched as GET ONLY, so every action is a link
//     carrying a query string — never a form, and never JavaScript;
//   - markup only consumes the host's CSS variables, so both light and dark
//     themes work with no frontend build.
//
// The login page is the one place that needs a two-step flow, and a GET-only
// route cannot hold one: the page therefore renders the URL from an explicit
// `?action=start` link, and the user copies it into a browser. That is the same
// shape the reference's front end has (it calls `auth.login.start` and opens the
// returned URL itself), and it keeps every step reachable from a link.

// pluguiPage wraps body fragments in the themed document shell.
func pluguiPage(heading string, body ...template.HTML) pluginapi.ManagementResponse {
	return plugui.HTML(heading, body...)
}

// renderStatusPage renders account cards, the catalogue and the settings summary.
//
// Every value that comes from upstream or from a credential is rendered through
// plugui's escaping helpers, so a hostile account label cannot inject markup.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	accounts := zcodeAccounts(h)

	body := []template.HTML{plugui.Card("插件设置", plugui.Fields(
		plugui.Field{Label: "推理端点", Value: Origin + MessagesPath},
		plugui.Field{Label: "协议", Value: "Anthropic Messages（流式 SSE）—— 该通道没有 OpenAI 形态的端点"},
		plugui.Field{Label: "客户端版本", Value: cfg.AppVersion},
		plugui.Field{Label: "来源标识", Value: "X-Platform=" + cfg.Platform + "，X-Os-Category=" + cfg.OSGroup +
			"，X-Client-Language=" + cfg.ClientLanguage + "，X-Client-Timezone=" + cfg.ClientTimezone},
		plugui.Field{Label: "身份块", Value: fmt.Sprintf("cliPrefix %d 字符 + stable %d 字符（共 %d），"+
			"拆成独立文本块按序下发；调用方 system 追加在最后",
			len(officialCLIPrefix), len(officialStableSections()),
			len(officialCLIPrefix)+len(officialStableSections()))},
		plugui.Field{Label: "会话续期", Value: "不支持：JWT 没有 exp，服务端也没有续期端点；" +
			"auth.refresh 只做有效性探测，失效需重新登录"},
		plugui.Field{Label: "每日签到", Value: "支持：先补 app_launch / app_daily_active 活跃上报，再查 preview，再领取"},
		plugui.Field{Label: "captcha", Value: "不产出：实测身份块正确时不带 captcha 头也返回 200；" +
			"官方客户端自身也只在 billing/claim 一处还带它，且是「被动重试」语义"},
	))}

	if len(accounts) == 0 {
		body = append(body, plugui.Card("尚未添加账号",
			plugui.Notice("warning", "当前实例还没有 ZCode 账号。点下面的按钮打开登录页："+
				"先取得授权 URL，在浏览器里完成授权，再由轮询把凭据收回来。"+
				"若本机已装官方 ZCode 客户端并登录过，也可以在登录页用「导入官方客户端凭据」直接采用它的登录态。"),
			plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
		))
		return pluguiPage("ZCode（智谱）", body...)
	}

	_, found := selectAccount(h, request)
	if !found {
		body = append(body, plugui.Card("账号不存在",
			plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。")))
		return pluguiPage("ZCode（智谱）", body...)
	}

	// One sweep, one card per account: each card carries that account's own
	// balance, never the selected account's repeated.
	statuses := collectAccountStatuses(h, accounts, cfg)
	for _, status := range statuses {
		body = append(body, renderAccountCard(status))
	}
	body = append(body, renderCatalogueCard())
	return pluguiPage("ZCode（智谱）", body...)
}

// renderAccountCard renders one account: its identity, its OWN token balance and
// its own actions.
func renderAccountCard(status accountStatus) template.HTML {
	entry := status.Entry
	fields := []plugui.Field{{Label: "状态", Value: entryStatusText(entry)}}
	if entry.AuthIndex != "" {
		fields = append(fields, plugui.Field{Label: "索引", Value: entry.AuthIndex})
	}
	if status.Error != "" {
		fields = append(fields, plugui.Field{Label: "凭据", Value: "无法读取：" + status.Error})
	} else {
		credential := status.Credential
		fields = append(fields,
			plugui.Field{Label: "账号", Value: credential.displayLabel()},
			plugui.Field{Label: "user_id（账号标识）", Value: emptyText(credential.UserID)},
			plugui.Field{Label: "来源", Value: sourceText(credential.Source)},
			plugui.Field{Label: "客户端版本", Value: emptyText(credential.AppVersion)},
		)
		// ⚠ device_mid is shown, but labelled as a device id: it is regenerated on
		// every login, so presenting it as an account identifier would invite the
		// exact mistake the reference records.
		fields = append(fields, plugui.Field{
			Label: "device_mid（设备标识，非账号标识）",
			Value: shortMid(credential.DeviceMid) + "…（每次登录都会重新生成）",
		})
		switch {
		case status.BalanceErr != "":
			fields = append(fields, plugui.Field{Label: "额度", Value: "查询失败：" + status.BalanceErr})
		case status.Balance == nil:
			fields = append(fields, plugui.Field{Label: "额度", Value: "未查询"})
		case status.Balance.Enterprise:
			fields = append(fields, plugui.Field{Label: "额度",
				Value: "企业版账号：服务端不下发额度数字（显示 0 会被误读成「已用光」，故如实说明）"})
		default:
			balance := status.Balance
			fields = append(fields,
				// Rendered in TOKENS: upstream's own `unit_type` is `token`, and a
				// bare nine-digit number reads like credits.
				plugui.Field{Label: "剩余额度", Value: formatTokenMagnitude(balance.Remaining) + " " +
					balanceUnit(balance) + fmt.Sprintf("（%d）", balance.Remaining)},
				plugui.Field{Label: "总额度", Value: formatTokenMagnitude(balance.Total) + " " +
					balanceUnit(balance) + fmt.Sprintf("（%d）", balance.Total)},
			)
			if balance.PlanName != "" {
				fields = append(fields, plugui.Field{Label: "套餐", Value: balance.PlanName})
			}
			for _, bucket := range balance.Buckets {
				name := bucket.ShowName
				if name == "" {
					name = bucket.PlanID
				}
				if name == "" {
					name = "额度桶"
				}
				fields = append(fields, plugui.Field{
					Label: "· " + name,
					Value: fmt.Sprintf("剩余 %s / 共 %s %s（meter=%s，用量 %s）",
						formatTokenMagnitude(bucket.Remaining), formatTokenMagnitude(bucket.Total),
						emptyText(bucket.UnitType), emptyText(bucket.Meter),
						formatTokenMagnitude(bucket.Used)),
				})
			}
			if balance.ExpiresAt > 0 {
				fields = append(fields, plugui.Field{Label: "最早到期", Value: unixSecondsToRFC3339(balance.ExpiresAt)})
			}
			// The raw device id is available in the JSON view for anyone who needs
			// to correlate it; the page keeps the truncated form above.
			_ = credential
		}
		switch {
		case status.CheckinErr != "":
			fields = append(fields, plugui.Field{Label: "今日领取", Value: "查询失败：" + status.CheckinErr})
		case status.Checkin != nil && status.Checkin.TodayCheckedIn:
			fields = append(fields, plugui.Field{Label: "今日领取",
				Value: "暂无可领额度（" + emptyText(status.Checkin.Note) + "）"})
		case status.Checkin != nil:
			fields = append(fields, plugui.Field{Label: "今日领取",
				Value: fmt.Sprintf("有 %d 项可领", len(status.Checkin.Claimable))})
		}
	}

	actions := []plugui.Action{
		{Label: "签到页", Path: "checkin", Query: "auth_index=" + url.QueryEscape(entry.AuthIndex)},
	}
	return plugui.Card("账号 · "+entry.Name, plugui.Fields(fields...), actions...)
}

// renderCatalogueCard lists the published catalogue.
func renderCatalogueCard() template.HTML {
	catalogue := currentCatalogue()
	fields := make([]plugui.Field, 0, len(catalogue)+2)
	for _, model := range catalogue {
		levels := "无档位"
		if len(model.ReasoningLevels) > 0 {
			levels = strings.Join(model.ReasoningLevels, " / ")
		}
		vision := "不支持图片"
		if model.SupportsImage {
			vision = "支持图片"
		}
		fields = append(fields, plugui.Field{
			Label: model.ID,
			Value: fmt.Sprintf("上下文 %s、最大输出 %s、思考档位 %s、%s",
				formatTokenMagnitude(int64(model.ContextWindow)),
				formatTokenMagnitude(int64(model.MaxOutputTokens)), levels, vision),
		})
	}
	fields = append(fields,
		plugui.Field{Label: "上游模型池", Value: "目录里还有 GLM-5-Turbo 与 GLM-5.2，但它们在 Start Plan 权益下返回空响应" +
			"（实测 0/3 正确，而 GLM-5.3 是 3/3），因此不对外暴露"},
		plugui.Field{Label: "命名说明", Value: "GLM-5.3 与 GLM-5.3-Flash 已被其它渠道发布；" +
			"CPA 会把同名模型合并成一个条目、由多个凭据共同供给，因此本插件是给已有模型 id 增加容量，而不是新建模型"},
	)
	return plugui.Card("模型目录", plugui.Fields(fields...))
}

// renderLoginPage renders the device-authorization steps.
//
// The flow is deliberately two-step and the page never blocks: `?action=start`
// mints an authorization URL on demand (a page load must not, or every refresh
// would create a flow), and the polling itself is driven by `auth.login.poll`
// from the management client.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	body := make([]template.HTML, 0, 4)
	if plugui.IsAddAccountRequest(request) {
		body = append(body, plugui.Notice("", plugui.AddAccountNotice))
	}

	action := strings.ToLower(strings.TrimSpace(request.Query.Get("action")))
	switch action {
	case "start":
		value, errStart := handleAuthLoginStart(h, nil)
		if errStart != nil {
			body = append(body, plugui.Card("授权初始化失败",
				plugui.Notice("danger", errStart.Error())))
			break
		}
		started, ok := value.(pluginapi.AuthLoginStartResponse)
		if !ok || started.URL == "" {
			body = append(body, plugui.Card("授权初始化失败",
				plugui.Notice("danger", "授权服务没有返回可用的 URL")))
			break
		}
		body = append(body, plugui.Card("① 在浏览器中打开这个地址完成授权",
			plugui.Group(
				plugui.Fields(
					plugui.Field{Label: "授权地址", Value: started.URL},
					plugui.Field{Label: "会话状态", Value: started.State},
					plugui.Field{Label: "有效期至", Value: jsonTime(started.ExpiresAt)},
					plugui.Field{Label: "轮询间隔", Value: fmt.Sprintf("%d 毫秒（服务端下发，缺省用配置值 %d）",
						pollIntervalFromMetadata(started.Metadata, cfg.PollIntervalMS), cfg.PollIntervalMS)},
				),
				plugui.Notice("", "授权在服务端完成：本插件不监听任何本地端口，浏览器停在授权成功页即可。"+
					"授权完成后由管理客户端调用 auth.login.poll（state="+started.State+"）收取凭据。"),
			),
			plugui.Action{Label: "重新发起", Query: "action=start"},
		))

	case "import":
		body = append(body, renderImportCard(h, cfg))

	default:
		body = append(body, plugui.Card("ZCode 登录（设备授权流）", plugui.Group(
			plugui.Notice("", "① 点「开始授权」取得授权 URL；② 在浏览器里打开它完成授权；"+
				"③ 由 auth.login.poll 轮询收取凭据。全过程不需要本地回调端口。"),
			plugui.Fields(
				plugui.Field{Label: "初始化端点", Value: Origin + OAuthCLIInitPath},
				plugui.Field{Label: "轮询端点", Value: Origin + OAuthCLIPollPrefix + "<flow_id>"},
				plugui.Field{Label: "登录方式", Value: "服务端中介的设备授权（provider=" + loginProvider + "）"},
				plugui.Field{Label: "本地回调端口", Value: "无 —— 授权结果由服务端保存，客户端轮询取回"},
				plugui.Field{Label: "X-Device-Mid", Value: "硬需求：缺失时上游回 400 code 3001；" +
					"但它的值不被服务端校验，因此由本插件在登录时自行生成并随凭据持久化"},
			),
		),
			plugui.Action{Label: "开始授权", Query: "action=start", Kind: "primary"},
			plugui.Action{Label: "导入官方客户端凭据", Query: "action=import"},
		))
	}
	body = append(body, plugui.Card("登录后的凭据", plugui.Fields(
		plugui.Field{Label: "凭据内容", Value: "zcode_jwt、device_mid、user_id、bigmodel_access_token、app_version、source"},
		plugui.Field{Label: "账号标识", Value: "user_id（服务端下发，稳定）—— 不要用 device_mid 判重，" +
			"它每次登录都会变"},
	)))
	return pluguiPage("ZCode 登录", body...)
}

// pollIntervalFromMetadata reads the interval the start response reported.
func pollIntervalFromMetadata(metadata map[string]any, fallback int) int {
	if metadata == nil {
		return fallback
	}
	if value, ok := nonNegativeInt(metadata["poll_interval_ms"]); ok && value > 0 {
		return value
	}
	return fallback
}

// renderImportCard performs the optional client-credential adoption.
//
// The import writes a new credential ONLY when the caller asked for it: importing
// on page load would silently overwrite a plugin login with a possibly stale
// client store, which is the "I just logged in and it says expired" failure the
// reference warns about.
func renderImportCard(h *abiboot.Host, cfg Config) template.HTML {
	if !cfg.ImportClientCredential {
		return plugui.Card("导入官方客户端凭据",
			plugui.Notice("warning", "配置项 import_client_credential 当前是关闭的。"+
				"打开它之后，本页才能读取官方客户端的 ~/.zcode/v2/credentials.json。"))
	}
	credential, errImport := importClientCredential(cfg.ClientDataDir)
	if errImport != nil {
		return plugui.Card("导入官方客户端凭据",
			plugui.Notice("danger", errImport.Error()))
	}
	auth, errAuth := authDataFor(credential, defaultAuthFileName(credential))
	if errAuth != nil {
		return plugui.Card("导入官方客户端凭据", plugui.Notice("danger", errAuth.Error()))
	}
	if h == nil {
		return plugui.Card("导入官方客户端凭据",
			plugui.Notice("danger", "插件未通过宿主调用，无法保存凭据。"))
	}
	saved, errSave := h.SaveAuth(auth.FileName, auth.StorageJSON)
	if errSave != nil {
		return plugui.Card("导入官方客户端凭据",
			plugui.Notice("danger", "保存凭据失败："+errSave.Error()))
	}
	path := auth.FileName
	if saved != nil && saved.Path != "" {
		path = saved.Path
	}
	return plugui.Card("导入成功", plugui.Group(
		plugui.Fields(
			plugui.Field{Label: "凭据文件", Value: path},
			plugui.Field{Label: "账号", Value: credential.displayLabel()},
			plugui.Field{Label: "来源", Value: "ide（解密自官方客户端）"},
		),
		plugui.Notice("success", "已采用官方客户端的登录态。它通常是长期有效的；"+
			"若官方客户端后来重新登录，本插件手里的这份不会自动跟着变，可以再次导入，或改用插件内登录。"),
	))
}

// renderCheckinPage renders the daily claim.
func renderCheckinPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	entry, found := selectAccount(h, request)
	if !found {
		return pluguiPage("ZCode 每日额度",
			plugui.Card("尚未添加账号",
				plugui.Notice("warning", "当前实例还没有 ZCode 账号，无法签到。"),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"}))
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return pluguiPage("ZCode 每日额度",
			plugui.Card("凭据不可用", plugui.Notice("danger", errCredential.Error())))
	}

	body := make([]template.HTML, 0, 4)
	action := strings.ToLower(strings.TrimSpace(request.Query.Get("action")))
	if action == "claim" {
		outcomes, errClaim := claimDaily(h, credential, cfg)
		switch {
		case errClaim != nil:
			body = append(body, plugui.Card("领取失败", plugui.Notice("danger", errClaim.Error())))
		default:
			fields := make([]plugui.Field, 0, len(outcomes))
			tone := "success"
			for _, outcome := range outcomes {
				label := outcome.PlanID
				if label == "" {
					label = "每日额度"
				}
				value := "已领取"
				switch {
				case outcome.AlreadyClaimed:
					value = "已经领取过（幂等成功，不是错误）"
				case !outcome.OK:
					value = outcome.Message
					tone = "danger"
				}
				fields = append(fields, plugui.Field{Label: label, Value: value})
			}
			if len(fields) == 0 {
				fields = append(fields, plugui.Field{Label: "结果", Value: "服务端没有返回任何结果"})
			}
			body = append(body, plugui.Card("领取结果",
				plugui.Group(plugui.Notice(tone, "已向服务端提交领取请求。"), plugui.Fields(fields...))))
		}
	}

	status, errStatus := fetchCheckinStatus(h, credential, cfg)
	if errStatus != nil {
		body = append(body, plugui.Card("签到状态",
			plugui.Notice("danger", "查询失败："+errStatus.Error())))
	} else {
		state := "有可领额度"
		if status.TodayCheckedIn {
			state = "暂无可领额度"
		}
		fields := []plugui.Field{
			{Label: "账号", Value: credential.displayLabel()},
			{Label: "活动", Value: status.ActivityName},
			{Label: "状态", Value: state},
			{Label: "可领项", Value: fmt.Sprintf("%d", len(status.Claimable))},
		}
		for _, plan := range status.Claimable {
			fields = append(fields, plugui.Field{
				Label: plan.PlanID,
				Value: fmt.Sprintf("优先级 %d %s", plan.Priority, emptyText(plan.Name)),
			})
		}
		if status.Note != "" {
			fields = append(fields, plugui.Field{Label: "说明", Value: status.Note})
		}
		body = append(body, plugui.Card("签到状态", plugui.Fields(fields...)))
	}

	body = append(body, plugui.Card("关于 ZCode 的签到", plugui.Group(
		plugui.Notice("", "① 服务端不会主动推送活动：必须先补 app_launch 与 app_daily_active 两条活跃上报，"+
			"否则 preview 恒为空 plans:[]（实测：补前为空、补后立刻出现 plan）。本插件在每次查询前都自动补报。"),
		plugui.Notice("warning", "② 额度单位是 token，不是积分。面板按上游下发的 unit_type 显示，"+
			"不会把 1 亿 token 伪装成 1 亿积分。"),
		plugui.Notice("warning", "③ claim 是官方客户端唯一还带 captcha 的端点。本插件按实测结论不产出 captcha："+
			"推理不需要它，领取端点目前也不需要；若服务端将来要求，页面会明确报 3007 而不是静默失败。"),
	)))
	body = append(body, plugui.Card("操作", plugui.Group(plugui.Notice("",
		"领取是不可逆的，因此只由显式点击触发：下面这个链接带 action=claim，刷新页面本身不会领取。")),
		plugui.Action{Label: "立即领取", Query: "action=claim&auth_index=" + url.QueryEscape(entry.AuthIndex), Kind: "primary"},
		plugui.Action{Label: "只看状态", Path: "checkin", Query: "auth_index=" + url.QueryEscape(entry.AuthIndex)},
	))
	return pluguiPage("ZCode 每日额度", body...)
}

// entryStatusText renders one credential entry's host-reported status.
func entryStatusText(entry pluginapi.HostAuthFileEntry) string {
	parts := make([]string, 0, 2)
	if entry.Status != "" {
		parts = append(parts, entry.Status)
	}
	switch {
	case entry.Disabled:
		parts = append(parts, "已停用")
	case entry.Unavailable:
		parts = append(parts, "暂不可用")
	}
	if len(parts) == 0 {
		return "未知"
	}
	return strings.Join(parts, " · ")
}

// sourceText renders where a credential came from.
func sourceText(source string) string {
	switch source {
	case "plugin":
		return "插件内登录"
	case "ide":
		return "官方客户端凭据（解密导入）"
	case "":
		return "未标注（早期的凭据没有这个字段）"
	default:
		return source
	}
}

// emptyText renders a placeholder for an absent optional value.
func emptyText(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

// jsonCompact renders a value for a diagnostic field.
func jsonCompact(value any) string {
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return ""
	}
	return string(encoded)
}
