package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The HTML half of the management surface. Every page is reached through a
// resource route, which the host dispatches as GET only, so each page action is
// a link carrying its whole intent in the query string: no forms, no JavaScript.

// renderStatusPage is the channel's landing page, linked from the hub overview.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	accounts := atomcodeAccounts(h)
	if len(accounts) == 0 {
		return plugui.HTML("AtomCode 状态",
			plugui.Card("尚未登录",
				plugui.Group(
					plugui.Notice("warning", "还没有 AtomGit 账号。AtomCode 的模型只能用 AtomGit 账号的 OAuth 令牌调用。"),
					plugui.Fields(configRows(cfg)...),
				),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
			),
		)
	}

	selected, found := selectAccount(h, request)
	if !found {
		return plugui.HTML("AtomCode 状态",
			plugui.Card("账号不存在", plugui.Notice("danger", "指定的 auth_index 不存在"),
				plugui.Action{Label: "返回", Path: "status"}))
	}

	cards := []template.HTML{}
	cards = append(cards, plugui.Card("全部账号",
		plugui.Group(
			plugui.Notice("", "同一账号可重复登录以刷新凭据；不同 AtomGit 账号会各自保存为独立的凭据文件。"),
			renderAccountList(h, accounts, selected),
		),
		plugui.Action{Label: "新建账号", Path: "login", Query: plugui.AddAccountQuery, Kind: "primary"},
		plugui.Action{Label: "刷新", Path: "status"},
	))

	credential, fresh, errCredential := credentialOf(h, selected)
	if errCredential != nil {
		cards = append(cards, plugui.Card("凭据不可用",
			plugui.Group(
				plugui.Notice("danger", "读取账号凭据失败："+errCredential.Error()),
			),
			plugui.Action{Label: "重新登录", Path: "login"},
			plugui.Action{Label: "新建账号", Path: "login", Query: plugui.AddAccountQuery},
		))
		return plugui.HTML("AtomCode 状态", cards...)
	}

	cards = append(cards, plugui.Card("当前账号",
		plugui.Group(
			renderRefreshNotice(fresh),
			plugui.Fields(
				plugui.Field{Label: "账号", Value: credential.Label()},
				plugui.Field{Label: "AtomGit ID", Value: credential.AccountID()},
				plugui.Field{Label: "用户名", Value: credential.User.Username},
				plugui.Field{Label: "凭据文件", Value: selected.Name},
				plugui.Field{Label: "有效期至", Value: formatExpiry(credential)},
				plugui.Field{Label: "可续期", Value: boolLabel(credential.Refreshable())},
			),
		),
		plugui.Action{Label: "重新登录", Path: "login", Query: "auth_index=" + selected.AuthIndex},
		plugui.Action{Label: "领取 / 用量", Path: "checkin", Query: "auth_index=" + selected.AuthIndex, Kind: "primary"},
	))

	cards = append(cards, planCard(h, cfg, credential, selected)...)
	cards = append(cards, modelCard(h, cfg, credential))
	cards = append(cards, plugui.Card("接入参数",
		plugui.Fields(configRows(cfg)...)))
	return plugui.HTML("AtomCode 状态", cards...)
}

// renderAccountList renders one row per auth file, marking the selected one.
func renderAccountList(h *abiboot.Host, accounts []pluginapi.HostAuthFileEntry, selected pluginapi.HostAuthFileEntry) template.HTML {
	fragments := make([]template.HTML, 0, len(accounts))
	for _, entry := range accounts {
		isCurrent := entry.AuthIndex == selected.AuthIndex && entry.Name == selected.Name
		credential, _, errCredential := credentialOf(h, entry)
		title := entry.Name
		rows := []plugui.Field{{Label: "凭据文件", Value: entry.Name}}
		if errCredential == nil {
			title = credential.Label()
			rows = append(rows,
				plugui.Field{Label: "AtomGit ID", Value: credential.AccountID()},
				plugui.Field{Label: "有效期至", Value: formatExpiry(credential)},
			)
		} else {
			rows = append(rows, plugui.Field{Label: "状态", Value: "凭据不可读：" + errCredential.Error()})
		}
		query := "auth_index=" + entry.AuthIndex
		actions := []plugui.Action{
			plugui.Action{Label: "重新登录", Path: "login", Query: query},
			plugui.Action{Label: "领取 / 用量", Path: "checkin", Query: query},
		}
		if !isCurrent {
			actions = append([]plugui.Action{{Label: "切到此账号", Path: "status", Query: query, Kind: "primary"}}, actions...)
		}
		badge := template.HTML("")
		if isCurrent {
			badge = plugui.Badge("success", "当前")
		}
		fragments = append(fragments, plugui.Card(title,
			plugui.Group(badge, plugui.Fields(rows...)), actions...))
	}
	return plugui.Group(fragments...)
}

// planCard renders the plan state and the rolling call window.
func planCard(h *abiboot.Host, cfg Config, credential *Credential, entry pluginapi.HostAuthFileEntry) []template.HTML {
	status, errStatus := fetchStatus(h, cfg, credential)
	if errStatus != nil {
		return []template.HTML{plugui.Card("套餐状态",
			plugui.Group(plugui.Notice("danger", "读取套餐状态失败："+errStatus.Error())),
			plugui.Action{Label: "重试", Path: "status"},
			plugui.Action{Label: "去领取", Path: "checkin", Query: "auth_index=" + entry.AuthIndex, Kind: "primary"},
		)}
	}
	rows := []plugui.Field{}
	if plan := status.CodingPlanFree; plan != nil {
		rows = append(rows,
			plugui.Field{Label: "套餐", Value: plan.PlanName},
			plugui.Field{Label: "套餐档位", Value: plan.PlanType},
			plugui.Field{Label: "领取日期", Value: blankAs(plan.ClaimedAt, "未领取")},
			plugui.Field{Label: "到期日期", Value: blankAs(plan.ExpiresAt, "未领取")},
			plugui.Field{Label: "剩余天数", Value: optionalInt(plan.Remaining, "未知")},
		)
	} else {
		rows = append(rows, plugui.Field{Label: "套餐", Value: "尚未领取免费套餐"})
	}
	for _, window := range status.quotaWindows() {
		rows = append(rows, plugui.Field{Label: "调用额度", Value: quotaSummary(window)})
		break
	}
	active := plugui.Badge("warning", "未生效")
	if status.active() {
		active = plugui.Badge("success", "生效中")
	}
	notice := plugui.Notice("", "免费套餐按「调用次数 / 滚动窗口」计量，不是 token 额度；窗口结束后自动重置。")
	if !status.active() {
		notice = plugui.Notice("warning", "套餐尚未生效：网关会以 403 user has no codingplan 拒绝所有请求，请先执行一次领取。")
	}
	return []template.HTML{plugui.Card("套餐状态",
		plugui.Group(active, notice, plugui.Fields(rows...)),
		plugui.Action{Label: "去领取", Path: "checkin", Query: "auth_index=" + entry.AuthIndex, Kind: "primary"},
	)}
}

// modelCard renders the serving catalogue and says where it came from.
func modelCard(h *abiboot.Host, cfg Config, credential *Credential) template.HTML {
	source := modelSource(h, cfg, credential)
	entries := staticModelEntries(h, cfg, credential)
	rows := make([]plugui.Field, 0, len(entries))
	extra := 0
	for _, entry := range entries {
		detail := ""
		if entry.ContextWindow != nil {
			detail = itoa(entry.effectiveContextWindow()) + " ctx"
		}
		if entry.acceptsImages() {
			detail += " · 支持图片"
		}
		if len(entry.ReasoningEffortLevels) > 0 {
			detail += " · 思考级别 " + strings.Join(entry.ReasoningEffortLevels, "/")
		}
		if entry.FromConfig {
			extra++
			if detail == "" {
				detail = "服务端目录未下发"
			}
			detail = "补充模型 · " + detail
		}
		if detail == "" {
			detail = "服务端未声明窗口"
		}
		rows = append(rows, plugui.Field{Label: entry.DisplayModelName, Value: detail})
	}
	notice := plugui.Notice("", "模型目录来自服务端 models-v2（来源："+modelSourceLabel(source)+"）。")
	if extra > 0 {
		notice = plugui.Notice("warning",
			"其中 "+itoa(extra)+" 个是 extra_models 补充的模型：网关能调用，但服务端目录已不下发，"+
				"因此不受套餐目录背书——随时可能被上游撤下，请以实际调用结果为准。")
	}
	return plugui.Card("可用模型",
		plugui.Group(notice,
			plugui.Fields(
				plugui.Field{Label: "线上目录缓存", Value: catalogueCacheText()},
				plugui.Field{Label: "后台自动刷新", Value: autoRefreshText(cfg)},
			),
			plugui.Fields(rows...),
		),
		plugui.Action{Label: "刷新目录", Query: "action=refresh-catalog", Kind: "primary"},
	)
}

// modelSourceLabel renders a model-source code for people.
func modelSourceLabel(source string) string {
	if source == "server" {
		return "服务端实时目录"
	}
	return "内置快照（服务端目录不可用时使用）"
}

// renderRefreshNotice reports what the pre-flight credential check did.
func renderRefreshNotice(fresh authrefresh.Result) template.HTML {
	switch {
	case fresh.Err != nil:
		return plugui.Notice("warning", "凭据续期失败："+fresh.Err.Error()+"（仍按现有凭据尝试）")
	case fresh.Refreshed:
		return plugui.Notice("success", "凭据已自动续期：续期会同时轮换 access_token 与 refresh_token。")
	case fresh.Expired:
		return plugui.Notice("danger", "凭据已过期，请重新登录。")
	default:
		return plugui.Notice("", "")
	}
}

// renderLoginPage is the sign-in page: 开始登录 -> 检查结果 -> 保存凭据.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "start", "login":
		return startLoginPage(h, request)
	case "poll":
		return pollLoginPage(h, request)
	}
	cfg := settings()
	notices := []template.HTML{}
	if plugui.IsAddAccountRequest(request) {
		notices = append(notices, plugui.Notice("", plugui.AddAccountNotice))
	}
	notices = append(notices, plugui.Notice("",
		"点击下面的按钮获取授权链接，在浏览器里用 AtomGit 账号登录并授权，然后回到本页检查结果。"))
	rows := []plugui.Field{
		{Label: "登录代理", Value: cfg.BrokerBase},
		{Label: "登录方式", Value: "AtomGit 账号（浏览器授权，无需在 CPA 侧填密码）"},
	}
	if authIndex := strings.TrimSpace(request.Query.Get("auth_index")); authIndex != "" {
		rows = append(rows, plugui.Field{Label: "目标账号", Value: authIndex + "（重新登录会覆盖该凭据文件）"})
	}
	return plugui.HTML("AtomCode 登录",
		plugui.Card("浏览器登录",
			plugui.Group(append(notices, plugui.Fields(rows...))...),
			plugui.Action{Label: "开始登录", Query: "action=start" + carryAdd(request), Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// carryAdd keeps the "add an account" intent across the start link.
func carryAdd(request pluginapi.ManagementRequest) string {
	if plugui.IsAddAccountRequest(request) {
		return "&" + plugui.AddAccountQuery
	}
	return ""
}

// startLoginPage begins a sign-in and shows the authorisation URL.
func startLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	session, errStart := startLoginSession(h, cfg)
	if errStart != nil {
		return plugui.HTML("AtomCode 登录",
			plugui.Card("无法发起登录", plugui.Notice("danger", errStart.Error()),
				plugui.Action{Label: "重试", Query: "action=start", Kind: "primary"},
				plugui.Action{Label: "返回状态", Path: "status"}))
	}
	ttl := time.Until(session.ExpiresAt).Truncate(time.Second)
	notices := []template.HTML{}
	if plugui.IsAddAccountRequest(request) {
		notices = append(notices, plugui.Notice("", plugui.AddAccountNotice))
	}
	notices = append(notices, plugui.Notice("",
		"在浏览器打开下面的链接，用 AtomGit 账号登录并完成授权，然后点「检查登录结果」。"))
	return plugui.HTML("AtomCode 登录",
		plugui.Card("在浏览器中完成授权",
			plugui.Group(append(notices,
				plugui.Fields(
					plugui.Field{Label: "授权链接", Value: session.LoginURL},
					plugui.Field{Label: "有效期", Value: ttl.String()},
				),
			)...),
			plugui.Action{Label: "检查登录结果", Query: "action=poll&state=" + session.State, Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// pollLoginPage advances a sign-in and persists the credential on success.
func pollLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	state := strings.TrimSpace(request.Query.Get("state"))
	response, errPoll := pollLoginForManagement(h, state)
	if errPoll != nil {
		return plugui.HTML("AtomCode 登录",
			plugui.Card("检查失败", plugui.Notice("danger", errPoll.Error()),
				plugui.Action{Label: "重新登录", Query: "action=start", Kind: "primary"},
				plugui.Action{Label: "返回状态", Path: "status"}))
	}
	switch response.Status {
	case pluginapi.AuthLoginStatusSuccess:
		message := response.Message
		if message == "" {
			message = "登录成功，账号已保存。"
		}
		return plugui.HTML("AtomCode 登录",
			plugui.Card("登录成功", plugui.Notice("success", message),
				plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"},
				plugui.Action{Label: "新建账号", Path: "login", Query: plugui.AddAccountQuery}))
	case pluginapi.AuthLoginStatusPending, "":
		return plugui.HTML("AtomCode 登录",
			plugui.Card("等待授权",
				plugui.Group(
					plugui.Notice("", "还没有完成授权。请在浏览器里登录并同意授权，然后再次检查。"),
					plugui.Fields(
						plugui.Field{Label: "授权链接", Value: loginURLForState(state)},
					),
				),
				plugui.Action{Label: "再次检查", Query: "action=poll&state=" + state, Kind: "primary"},
				plugui.Action{Label: "返回状态", Path: "status"}))
	default:
		return plugui.HTML("AtomCode 登录",
			plugui.Card("登录失败", plugui.Notice("danger", response.Message),
				plugui.Action{Label: "重新登录", Query: "action=start", Kind: "primary"},
				plugui.Action{Label: "返回状态", Path: "status"}))
	}
}

// loginURLForState re-reads the authorisation URL of a live session so a pending
// page can re-offer the link the user may have lost.
func loginURLForState(state string) string {
	session, found := lookupLoginSession(state)
	if !found {
		return "（登录会话已失效，请重新发起）"
	}
	return session.LoginURL
}

// pollLoginForManagement runs the poll handler and saves a successful credential.
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
	return response, nil
}

// renderCheckinPage shows the plan state, runs the claim cascade and reports the
// 60-day usage.
func renderCheckinPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	accounts := atomcodeAccounts(h)
	if len(accounts) == 0 {
		return plugui.HTML("AtomCode 领取",
			plugui.Card("尚未登录", plugui.Notice("warning", "请先登录一个 AtomGit 账号。"),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"}))
	}
	entry, found := selectAccount(h, request)
	if !found {
		return plugui.HTML("AtomCode 领取",
			plugui.Card("账号不存在", plugui.Notice("danger", "指定的 auth_index 不存在"),
				plugui.Action{Label: "返回", Path: "status"}))
	}
	credential, _, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return plugui.HTML("AtomCode 领取",
			plugui.Card("凭据不可用", plugui.Notice("danger", errCredential.Error()),
				plugui.Action{Label: "重新登录", Path: "login", Query: "auth_index=" + entry.AuthIndex}))
	}

	action := strings.ToLower(strings.TrimSpace(request.Query.Get("action")))
	cards := []template.HTML{}
	if action == "claim" {
		outcome := claimCascade(h, cfg, credential)
		invalidateCatalogue(credential)
		cards = append(cards, plugui.Card("领取结果",
			plugui.Group(claimNotice(outcome), renderTierAttempts(outcome.Attempts)),
			plugui.Action{Label: "刷新状态", Path: "checkin", Query: "auth_index=" + entry.AuthIndex, Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"}))
	} else {
		cards = append(cards, plugui.Card("每日领取",
			plugui.Group(
				plugui.Notice("", "领取会按 Max → Pro → Lite 依次尝试，命中即停止；已持有的档位会被识别为「已领取」。"),
			),
			plugui.Action{Label: "立即领取", Query: "action=claim&auth_index=" + entry.AuthIndex, Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"}))
	}

	status, errStatus := fetchStatus(h, cfg, credential)
	if errStatus != nil {
		cards = append(cards, plugui.Card("套餐状态",
			plugui.Group(plugui.Notice("danger", "读取套餐状态失败："+errStatus.Error()))))
	} else {
		rows := []plugui.Field{}
		if plan := status.CodingPlanFree; plan != nil {
			rows = append(rows,
				plugui.Field{Label: "套餐", Value: plan.PlanName},
				plugui.Field{Label: "档位", Value: plan.PlanType},
				plugui.Field{Label: "领取日期", Value: blankAs(plan.ClaimedAt, "未领取")},
				plugui.Field{Label: "到期日期", Value: blankAs(plan.ExpiresAt, "未领取")},
				plugui.Field{Label: "剩余天数", Value: optionalInt(plan.Remaining, "未知")},
			)
		}
		for _, window := range status.quotaWindows() {
			rows = append(rows, plugui.Field{Label: "调用额度", Value: quotaSummary(window)})
		}
		badge := plugui.Badge("warning", "未生效")
		if status.active() {
			badge = plugui.Badge("success", "生效中")
		}
		cards = append(cards, plugui.Card("套餐状态", plugui.Group(badge, plugui.Fields(rows...))))
	}

	if usage, errUsage := fetchUsage(h, cfg, credential); errUsage != nil {
		cards = append(cards, plugui.Card("用量（近 60 天）",
			plugui.Group(plugui.Notice("danger", "读取用量失败："+errUsage.Error()))))
	} else {
		rows := []plugui.Field{{Label: "合计", Value: usageTotals(usage)}}
		for _, model := range usage.Models {
			rows = append(rows, plugui.Field{
				Label: model,
				Value: itoa64(usage.ModelCounts[model]) + " 次调用 / " + itoa64(usage.ModelTokens[model]) + " tokens",
			})
		}
		cards = append(cards, plugui.Card("用量（近 60 天）", plugui.Fields(rows...)))
	}
	return plugui.HTML("AtomCode 领取", cards...)
}

// claimNotice renders the cascade verdict.
func claimNotice(outcome claimOutcome) template.HTML {
	prefix := ""
	switch outcome.Status {
	case claimClaimed:
		prefix = "领取成功"
	case claimAlreadyHeld:
		prefix = "已领取"
	case claimRefused:
		prefix = "领取被拒绝"
	case claimAuthRequired:
		prefix = "登录态失效"
	default:
		prefix = "领取失败"
	}
	// The server's own message is frequently the same words as the prefix
	// ("领取成功"), so it is only appended when it adds something.
	message := prefix
	if detail := strings.TrimSpace(outcome.Message); detail != "" && detail != prefix {
		message += "：" + detail
	}
	tone := "success"
	switch outcome.Status {
	case claimRefused, claimFailed, claimAuthRequired:
		tone = "danger"
	}
	return plugui.Notice(tone, message)
}

// renderTierAttempts renders the per-tier cascade trail.
func renderTierAttempts(attempts []tierAttempt) template.HTML {
	if len(attempts) == 0 {
		return ""
	}
	rows := make([]plugui.Field, 0, len(attempts))
	for _, attempt := range attempts {
		value := attempt.Outcome
		if attempt.PlanName != "" {
			value += " · " + attempt.PlanName
		}
		if attempt.Message != "" {
			value += " · " + attempt.Message
		}
		rows = append(rows, plugui.Field{Label: attempt.Tier, Value: value})
	}
	return plugui.Fields(rows...)
}

// configRows describes the wiring the adapter is using.
func configRows(cfg Config) []plugui.Field {
	return []plugui.Field{
		{Label: "聊天网关", Value: cfg.GatewayBase},
		{Label: "请求标识", Value: AdapterUserAgent},
		{Label: "套餐接口", Value: cfg.CodingPlanAPIBase},
		{Label: "登录代理", Value: cfg.BrokerBase},
		{Label: "模型目录", Value: boolLabel(cfg.DiscoverModels) + "（档位 " + cfg.PlanType + "）"},
		{Label: "模型前缀", Value: boolLabel(cfg.ModelPrefix)},
	}
}

// formatExpiry renders a credential's expiry for display.
func formatExpiry(credential *Credential) string {
	expiry := credential.Expiry()
	if expiry.IsZero() {
		return "未知（凭据未记录签发时间）"
	}
	remaining := time.Until(expiry)
	if remaining <= 0 {
		return expiry.Format("2006-01-02 15:04") + "（已过期）"
	}
	return expiry.Format("2006-01-02 15:04") + "（剩余 " + remaining.Truncate(time.Minute).String() + "）"
}

// boolLabel renders a boolean for people.
func boolLabel(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// blankAs renders a possibly-empty server string.
func blankAs(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// optionalInt renders an optional server integer.
func optionalInt(value *int, fallback string) string {
	if value == nil {
		return fallback
	}
	return fmt.Sprintf("%d", *value)
}
