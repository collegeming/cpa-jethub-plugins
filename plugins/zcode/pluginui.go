package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"slices"
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

	body := []template.HTML{plugui.Card("插件设置", plugui.Group(plugui.Fields(
		plugui.Field{Label: "推理端点", Value: Origin + MessagesPath},
		plugui.Field{Label: "协议", Value: "Anthropic Messages（流式 SSE）—— 该通道没有 OpenAI 形态的端点"},
		plugui.Field{Label: "客户端版本", Value: cfg.AppVersion},
		plugui.Field{Label: "来源标识", Value: "X-Platform=" + cfg.Platform + "，X-Os-Category=" + cfg.OSGroup +
			"，X-Client-Language=" + cfg.ClientLanguage + "，X-Client-Timezone=" + cfg.ClientTimezone},
		plugui.Field{Label: "身份块", Value: fmt.Sprintf("cliPrefix %d 字符 + stable %d 字符（共 %d），"+
			"拆成独立文本块按序下发；调用方 system 追加在最后",
			len(officialCLIPrefix), len(officialStableSections()),
			len(officialCLIPrefix)+len(officialStableSections()))},
		plugui.Field{Label: "模型目录", Value: catalogueSourceText(cfg)},
		plugui.Field{Label: "线上目录缓存", Value: catalogueCacheText()},
		plugui.Field{Label: "后台自动刷新", Value: autoRefreshText(cfg)},
		plugui.Field{Label: "会话续期", Value: "不支持：JWT 没有 exp，服务端也没有续期端点；" +
			"auth.refresh 只做有效性探测，失效需重新登录"},
		plugui.Field{Label: "每日签到", Value: "不支持：领取端点强制要求阿里云验证码（3007），" +
			"本插件不产出验证码，因此与 cline 一样不提供签到入口；额度请在官方客户端或网页领取"},
		plugui.Field{Label: "captcha", Value: "只被领取端点要求，推理实测不需要；" +
			"本插件不提供领取入口，因此不涉及"},
	)),
		plugui.Action{Label: "刷新目录", Query: "action=refresh-catalog", Kind: "primary"},
	)}

	if len(accounts) == 0 {
		body = append(body, plugui.Card("尚未添加账号",
			plugui.Notice("warning", "当前实例还没有 ZCode 账号。点下面的按钮打开登录页："+
				"先取得授权 URL，在浏览器里完成授权，再由轮询把凭据收回来。"+
				"若本机已装官方 ZCode 客户端并登录过，也可以在登录页用「导入官方客户端凭据」直接采用它的登录态。"),
			plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
		))
		// The catalogue card is rendered even without an account: the built-in
		// table is what this channel would publish, and hiding the model list
		// behind a login would leave the page unable to answer its own question.
		body = append(body, renderCatalogueCard(h, request, cfg))
		return pluguiPage("ZCode（智谱）", body...)
	}

	selected, found := selectAccount(h, request)
	if !found {
		body = append(body, plugui.Card("账号不存在",
			plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。")))
		body = append(body, renderCatalogueCard(h, request, cfg))
		return pluguiPage("ZCode（智谱）", body...)
	}

	// One sweep, one card per account: each card carries that account's own
	// balance, never the selected account's repeated.
	statuses := collectAccountStatuses(h, accounts, cfg)
	for _, status := range statuses {
		body = append(body, renderAccountCard(status, status.Entry.AuthIndex == selected.AuthIndex))
	}
	body = append(body, renderAccountList(accounts, selected.AuthIndex))
	body = append(body, renderCatalogueCard(h, request, cfg))
	return pluguiPage("ZCode（智谱）", body...)
}

// renderAccountList renders the switcher across accounts.
//
// It is rendered for a single account as well, because it carries 新建账号 —
// the only way to add a SECOND account from this page. Every other provider in
// this repository does the same; its absence here left the add-account flow that
// the login page already implements (`plugui.IsAddAccountRequest`) reachable
// only by hand-editing the URL.
func renderAccountList(accounts []pluginapi.HostAuthFileEntry, current string) template.HTML {
	fields := make([]plugui.Field, 0, len(accounts))
	for _, entry := range accounts {
		marker := ""
		if entry.AuthIndex == current {
			marker = "（当前）"
		}
		fields = append(fields, plugui.Field{Label: entry.Name + marker, Value: entryStatusText(entry)})
	}
	title := "全部账号"
	if len(accounts) > 1 {
		title += "（在地址后追加 ?auth_index=<索引> 可切换）"
	}
	return plugui.Card(title, plugui.Fields(fields...),
		plugui.Action{Label: "新建账号", Path: "login", Query: plugui.AddAccountQuery},
	)
}

// renderAccountCard renders one account: its identity, its OWN token balance and
// its own actions.
func renderAccountCard(status accountStatus, current bool) template.HTML {
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
	}

	actions := []plugui.Action{
		// ZCode credentials are static — the JWT carries no expiry and the server
		// exposes no refresh endpoint — so a dead credential can ONLY be replaced
		// by logging in again. Without this link the page offers no route back to
		// the login flow, which is what makes a stale credential a dead end.
		{Label: "重新登录", Path: "login", Query: "auth_index=" + url.QueryEscape(entry.AuthIndex)},
	}
	title := "账号 · " + entry.Name
	if current {
		// The same marker the account list uses, so a reader can tell which
		// account the page's own links (重新登录) will act on.
		title += "（当前）"
	}
	return plugui.Card(title, plugui.Fields(fields...), actions...)
}

// renderCatalogueCard lists the catalogue this deployment publishes.
//
// The rows are built from the same descriptors `model.for_auth` reports, so the
// page cannot disagree with the published catalogue. `modelInfos` fills `Name`
// from the PROVIDER's own name (`fallbackModel.Name`, upstream's
// `builtinModels[].name`) and `ID` from the routed id — the shared card renders
// the former as the label and shows the latter as 「请求用名」 only when they
// differ, which on a healthy catalogue they do not.
//
// The listing is cache-or-built-in and never a fetch: a page load must not call
// upstream. The source line says which of the two is on screen.
func renderCatalogueCard(h *abiboot.Host, request pluginapi.ManagementRequest, cfg Config) template.HTML {
	catalogue := currentCatalogue()
	// The prefix is the selected account's, so a prefixed deployment shows the
	// name a request must actually carry. Without a usable account the listing is
	// the unprefixed one, which is also what `model.register` publishes.
	prefix := ""
	if entry, found := selectAccount(h, request); found {
		if credential, errCredential := credentialOf(h, entry); errCredential == nil {
			prefix = modelPrefixFor(credential)
		}
	}
	return plugui.Group(
		plugui.CatalogueCard(plugui.ModelCatalogue{
			Source:      catalogueSourceText(cfg),
			Entries:     catalogueModelEntries(catalogue, prefix),
			EmptyNotice: "暂无模型：目录尚未拉取，且内置表为空。",
			Actions: []plugui.Action{
				{Label: "刷新目录", Query: "action=refresh-catalog", Kind: "primary"},
			},
		}),
		plugui.Card("目录说明", plugui.Fields(
			plugui.Field{Label: "上游模型池", Value: "目录里还有 GLM-5-Turbo 与 GLM-5.2，但它们在 Start Plan 权益下返回空响应" +
				"（实测 0/3 正确，而 GLM-5.3 是 3/3），因此不对外暴露"},
			plugui.Field{Label: "命名说明", Value: "GLM-5.3 与 GLM-5.3-Flash 已被其它渠道发布；" +
				"CPA 会把同名模型合并成一个条目、由多个凭据共同供给，因此本插件是给已有模型 id 增加容量，而不是新建模型"},
		)),
	)
}

// catalogueModelEntries maps the served catalogue onto the shared card's rows.
//
// The rows are built from `modelInfos`, the very descriptors this plugin hands
// the host for `model.for_auth`, so the page and the published catalogue cannot
// disagree — including the account prefix, which is part of the name a request
// must carry. `ModelEntriesFromInfo` reads `Name` as the displayed value and `ID`
// as the routing name; `modelInfoFor` fills `Name` from the PROVIDER's own name
// (upstream's `builtinModels[].name`, see parseBuiltinModels) rather than copying
// the routed id, so the label is what ZCode calls the model.
//
// The per-model metadata this card always published (window, output cap, effort
// ladder, vision) is kept as each row's detail.
func catalogueModelEntries(catalogue []fallbackModel, prefix string) []plugui.ModelEntry {
	return plugui.ModelEntriesFromInfo(modelInfos(catalogue, prefix), func(info pluginapi.ModelInfo) string {
		levels := "无档位"
		if info.Thinking != nil && len(info.Thinking.Levels) > 0 {
			levels = strings.Join(info.Thinking.Levels, " / ")
		}
		vision := "不支持图片"
		if slices.Contains(info.SupportedInputModalities, "image") {
			vision = "支持图片"
		}
		return fmt.Sprintf("上下文 %s、最大输出 %s、思考档位 %s、%s",
			formatTokenMagnitude(info.ContextLength),
			formatTokenMagnitude(info.MaxCompletionTokens), levels, vision)
	})
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
