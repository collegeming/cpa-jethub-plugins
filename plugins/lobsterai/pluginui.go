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
//   - the host dispatches those routes as GET only, so every action is a
//     `plugui.Action` link carrying a query string, never a form submission;
//   - markup only consumes the host's CSS variables, so there is no frontend
//     build and both light and dark themes work for free.
//
// Because the page drives `auth.login.poll` itself, it is also responsible for
// persisting the resulting credential (see pollLoginForManagement) — the host
// only saves what the normal auth.login.poll reply returns.

// maxModelsOnPage bounds how many model rows the status page renders.
const maxModelsOnPage = 10

// wantsJSON reports whether the caller asked for machine-readable output.
//
// The defaults follow what each mount is for:
//   - the resource mount that CPAMP embeds is loaded by a browser (Accept
//     mentions text/html) or by curl, whose `*/*` accepts HTML too, so HTML is
//     the default there;
//   - a caller that accepts only machine types (application/json), a non-GET
//     request, or an explicit `?format=json` always gets JSON.
//
// `?format=html` forces the page for the opposite case.
func wantsJSON(request pluginapi.ManagementRequest) bool {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("format"))) {
	case "json":
		return true
	case "html":
		return false
	}
	if request.Method != "" && !strings.EqualFold(request.Method, http.MethodGet) {
		return true
	}
	accept := strings.ToLower(strings.TrimSpace(request.Headers.Get("Accept")))
	if accept == "" {
		return false
	}
	// `*/*` accepts HTML, so it is a page navigation as far as this plugin is
	// concerned; only an Accept that excludes HTML means "give me data".
	if strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*") {
		return false
	}
	return true
}

// lobsteraiAccounts returns the host's LobsterAI credentials, in host order.
func lobsteraiAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
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

// selectAccount resolves which credential a page should act on. Without an
// explicit selector the first account is used, so the page works with no
// parameters at all.
func selectAccount(h *abiboot.Host, request pluginapi.ManagementRequest) (pluginapi.HostAuthFileEntry, bool) {
	wanted := strings.TrimSpace(request.Query.Get("auth_index"))
	if wanted == "" {
		wanted = strings.TrimSpace(request.Query.Get("auth_id"))
	}
	for _, entry := range lobsteraiAccounts(h) {
		if wanted == "" {
			return entry, true
		}
		if entry.AuthIndex == wanted || entry.ID == wanted || entry.Name == wanted {
			return entry, true
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

// credentialOf loads and parses the credential of one account.
func credentialOf(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) (*Credential, error) {
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return nil, abiboot.Errorf("missing_auth", "账号 %s 缺少运行时索引", entry.Name)
	}
	auth, errGet := h.GetAuth(entry.AuthIndex)
	if errGet != nil {
		return nil, errGet
	}
	return ParseCredential(auth.JSON)
}

// renderStatusPage renders the account overview: accounts, credential validity,
// the model catalog with its remote parameters, and the credit balance.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	accounts := lobsteraiAccounts(h)
	if len(accounts) == 0 {
		return plugui.HTML("LobsterAI (有道)",
			plugui.Card("尚未添加账号",
				plugui.Notice("warning", "当前实例还没有 LobsterAI 账号。先在浏览器完成一次授权即可。"),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
			),
		)
	}

	entry, found := selectAccount(h, request)
	if !found {
		return plugui.HTML("LobsterAI (有道)",
			plugui.Card("账号不存在", plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。"),
				plugui.Action{Label: "返回默认账号", Path: "status"}))
	}

	body := make([]template.HTML, 0, 5)
	cfg := settings()

	// One sweep, one card per account: each card carries that account's own
	// credits and activity slot, never the selected account's repeated.
	quotas := collectAccountQuotas(h, accounts, cfg, resolveClientVersion(h, cfg))
	for _, quota := range quotas {
		body = append(body, renderQuotaCard(quota, quota.Entry.AuthIndex == entry.AuthIndex))
	}
	body = append(body, renderModelCard())
	body = append(body, renderAccountList(accounts, entry.AuthIndex))
	return plugui.HTML("LobsterAI (有道)", body...)
}

// renderQuotaCard renders one account: its identity, its OWN credits and its own
// activity state, with the two per-account actions.
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
			plugui.Field{Label: "有效期至", Value: formatCredentialExpiry(quota.Credential)},
			plugui.Field{Label: "可自动续期", Value: yesNo(quota.Credential.Refreshable())},
		)
		if quota.Credential.Nickname != "" {
			fields = append(fields, plugui.Field{Label: "昵称", Value: quota.Credential.Nickname})
		}
		fields = append(fields,
			plugui.Field{Label: "uid", Value: orPlaceholder(quota.Credential.UID)},
			plugui.Field{Label: "安装 uuid", Value: orPlaceholder(quota.Credential.UUID)},
			plugui.Field{Label: "first_keyfrom", Value: orPlaceholder(quota.Credential.FirstKeyfrom)},
			plugui.Field{Label: "latest_keyfrom", Value: orPlaceholder(quota.Credential.LatestKeyfrom)},
		)
		switch {
		case quota.BalanceErr != nil:
			// Unknown, not zero: the read failed, so no number is shown.
			fields = append(fields, plugui.Field{Label: "积分", Value: "查询失败：" + quota.BalanceErr.Error()})
		case quota.Balance != nil:
			fields = append(fields,
				plugui.Field{Label: "剩余积分", Value: fmt.Sprintf("%.2f", quota.Balance.Total)},
				plugui.Field{Label: "积分包", Value: fmt.Sprintf("%d 个", len(quota.Balance.Packages))},
			)
			if quota.Balance.ExpiredTotal > 0 {
				fields = append(fields, plugui.Field{Label: "已失效积分", Value: fmt.Sprintf("%.2f", quota.Balance.ExpiredTotal)})
			}
		}
		switch {
		case quota.CheckinDisabled:
			fields = append(fields, plugui.Field{Label: "签到", Value: "已在配置中关闭"})
		case quota.ActivityErr != nil:
			fields = append(fields, plugui.Field{Label: "今日签到", Value: "查询失败：" + quota.ActivityErr.Error()})
		case quota.Activity != nil:
			fields = append(fields, plugui.Field{Label: "今日签到", Value: slotText(quota.Activity)})
		}
	}
	title := "积分 · " + entry.Name
	if current {
		title += "（当前）"
	}
	return plugui.Card(title, plugui.Fields(fields...),
		plugui.Action{Label: "签到", Path: "checkin", Query: accountQuery(entry), Kind: "primary"},
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

// renderModelCard lists the catalog with the remote model parameters this
// adapter consumes. The thinking levels show the product-side names while the
// wire values are what actually travels (openclawLevel).
func renderModelCard() template.HTML {
	catalog := currentCatalog(time.Now())
	fields := make([]plugui.Field, 0, maxModelsOnPage+2)
	fields = append(fields, plugui.Field{Label: "模型数量", Value: fmt.Sprintf("%d", len(catalog))})
	fields = append(fields, plugui.Field{Label: "数据来源", Value: catalogSource()})
	for index, model := range catalog {
		if index >= maxModelsOnPage {
			fields = append(fields, plugui.Field{
				Label: "…",
				Value: fmt.Sprintf("其余 %d 个模型省略", len(catalog)-maxModelsOnPage),
			})
			break
		}
		fields = append(fields, plugui.Field{Label: displayNameFor(model), Value: modelParameterSummary(model)})
	}
	return plugui.Card("模型与远端参数", plugui.Fields(fields...))
}

// catalogSource reports whether the advertised catalog is the discovered one or
// the static fallback.
func catalogSource() string {
	if cached, _ := discoveredModels.get(modelCacheTTL(settings()), time.Now()); len(cached) > 0 {
		return "远端 /api/models/available（已缓存）"
	}
	return "内置兜底列表（未发现远端目录）"
}

// modelParameterSummary renders the parameters consumed for one model.
func modelParameterSummary(model remoteModel) string {
	parts := make([]string, 0, 5)
	if model.ContextWindow != nil {
		parts = append(parts, fmt.Sprintf("上下文 %d", *model.ContextWindow))
	}
	if model.MaxTokens != nil {
		parts = append(parts, fmt.Sprintf("单次输出上限 %d", *model.MaxTokens))
	}
	if model.Thinking != nil {
		pairs := make([]string, 0, len(model.Thinking.Options))
		for _, option := range model.Thinking.Options {
			pairs = append(pairs, option.Level+"→"+option.OpenclawLevel)
		}
		parts = append(parts, "思考档位(展示→wire) "+strings.Join(pairs, ", "))
	}
	if model.SupportsImage != nil {
		parts = append(parts, "图片 "+yesNo(*model.SupportsImage))
	}
	if model.CostMultiplier != nil {
		if *model.CostMultiplier == 0 {
			parts = append(parts, "免费")
		} else {
			parts = append(parts, "倍率 x"+fmt.Sprintf("%g", *model.CostMultiplier))
		}
	}
	if len(parts) == 0 {
		return "远端未下发参数（仅 id/name）"
	}
	return strings.Join(parts, "；")
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

// renderLoginPage renders the two-step browser login. The first request hands
// the user an authorization URL; later requests poll for the result. Nothing
// ever blocks waiting for the user to finish.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "start", "login":
		return startLoginPage()
	case "poll":
		return pollLoginPage(h, request)
	}
	notices := []template.HTML{}
	if plugui.IsAddAccountRequest(request) {
		notices = append(notices, plugui.Notice("", plugui.AddAccountNotice))
	}
	notices = append(notices, plugui.Notice("", "点击下面的按钮获取授权链接。LobsterAI 使用本地回调 + authCode 换 token："+
		"授权完成后浏览器会跳到本机 127.0.0.1 的回调地址，再回到本页检查结果。"))
	return plugui.HTML("LobsterAI 登录",
		plugui.Card("浏览器登录",
			plugui.Group(notices...),
			plugui.Action{Label: "开始登录", Query: "action=start", Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// startLoginPage begins a login session and shows the authorization URL.
func startLoginPage() pluginapi.ManagementResponse {
	session, errStart := startLoginSession(time.Now())
	if errStart != nil {
		return plugui.HTML("LobsterAI 登录",
			plugui.Card("无法发起登录", plugui.Notice("danger", errStart.Error()),
				plugui.Action{Label: "重试", Query: "action=start", Kind: "primary"}))
	}
	return plugui.HTML("LobsterAI 登录",
		plugui.Card("在浏览器中完成授权",
			plugui.Group(
				plugui.Notice("", fmt.Sprintf("已在本地端口 %d 监听回调，有效期 %s。请在浏览器中打开下面的授权链接，完成后点击「检查登录结果」。",
					session.Port, LoginTimeout)),
				plugui.Fields(
					plugui.Field{Label: "授权链接", Value: session.LoginURL()},
					plugui.Field{Label: "回调地址", Value: session.RedirectURI()},
					plugui.Field{Label: "state", Value: session.State},
				),
			),
			plugui.Action{Label: "打开授权链接", Path: session.LoginURL(), Kind: "primary"},
			plugui.Action{Label: "检查登录结果", Query: "action=poll&state=" + session.State},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// pollLoginPage polls a login session, persisting the credential on success.
func pollLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	state := strings.TrimSpace(request.Query.Get("state"))
	response, errPoll := pollLoginForManagement(h, state)
	if errPoll != nil {
		return plugui.HTML("LobsterAI 登录",
			plugui.Card("检查失败", plugui.Notice("danger", errPoll.Error()),
				plugui.Action{Label: "重新开始", Query: "action=start", Kind: "primary"}))
	}

	switch response.Status {
	case pluginapi.AuthLoginStatusSuccess:
		message := response.Message
		if message == "" {
			message = "登录成功，账号已保存。"
		}
		return plugui.HTML("LobsterAI 登录",
			plugui.Card("登录成功",
				plugui.Group(
					plugui.Notice("success", message),
					plugui.Fields(plugui.Field{Label: "凭据文件", Value: orPlaceholder(response.Auth.FileName)}),
				),
				plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"}))
	case pluginapi.AuthLoginStatusPending, "":
		return plugui.HTML("LobsterAI 登录",
			plugui.Card("等待授权",
				plugui.Notice("", "还没有收到授权回调。请先在浏览器里完成授权，然后再次检查。"),
				plugui.Action{Label: "再次检查", Query: "action=poll&state=" + state, Kind: "primary"},
				plugui.Action{Label: "重新开始", Query: "action=start"},
			))
	default:
		message := response.Message
		if message == "" {
			message = "登录失败。"
		}
		return plugui.HTML("LobsterAI 登录",
			plugui.Card("登录失败", plugui.Notice("danger", message),
				plugui.Action{Label: "重新开始", Query: "action=start", Kind: "primary"}))
	}
}

// loginStartResponse serves `/lobsterai/login/start` for scripts.
func loginStartResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if !wantsJSON(request) {
		return startLoginPage()
	}
	value, errStart := handleAuthLoginStart(h, json.RawMessage(`{}`))
	if errStart != nil {
		return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": errStart.Error()})
	}
	response, ok := value.(pluginapi.AuthLoginStartResponse)
	if !ok {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]any{"error": "unexpected login start response"})
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"provider":     response.Provider,
		"url":          response.URL,
		"state":        response.State,
		"expires_at":   response.ExpiresAt,
		"metadata":     response.Metadata,
		"next":         "轮询 POST /v0/management/" + ProviderKey + "/login/poll {\"State\":\"<state>\"}",
		"poll_timeout": LoginTimeout.String(),
	})
}

// loginPollResponse serves `/lobsterai/login/poll` for scripts and persists the
// credential on success.
func loginPollResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	state := strings.TrimSpace(request.Query.Get("state"))
	if state == "" && len(request.Body) > 0 {
		var body struct {
			State string `json:"State"`
		}
		if errUnmarshal := json.Unmarshal(request.Body, &body); errUnmarshal == nil {
			state = strings.TrimSpace(body.State)
		}
	}
	if state == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "缺少 state 参数"})
	}
	response, errPoll := pollLoginForManagement(h, state)
	if errPoll != nil {
		return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": errPoll.Error()})
	}
	body := map[string]any{
		"status":  response.Status,
		"message": response.Message,
	}
	if response.Status == pluginapi.AuthLoginStatusSuccess {
		body["auth_file"] = response.Auth.FileName
		body["label"] = response.Auth.Label
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// pollLoginForManagement reuses the auth.login.poll handler and then persists
// the credential itself.
//
// On the normal login path the host saves the credential the poll returns, but
// this page drives the poll directly, so saving is this function's job.
func pollLoginForManagement(h *abiboot.Host, state string) (pluginapi.AuthLoginPollResponse, error) {
	var empty pluginapi.AuthLoginPollResponse
	if state == "" {
		return empty, abiboot.Errorf("missing_state", "缺少 state 参数，请重新发起登录")
	}
	raw, errMarshal := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: ProviderKey, State: state})
	if errMarshal != nil {
		return empty, abiboot.Errorf("encode_poll", "encode poll request: %v", errMarshal)
	}
	value, errPoll := handleAuthLoginPoll(h, raw)
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
			name = ProviderKey + "-" + time.Now().Format("20060102150405")
		}
	}
	if h == nil {
		return empty, abiboot.Errorf("save_auth", "host transport 不可用，无法保存凭据")
	}
	if _, errSave := h.SaveAuth(name, response.Auth.StorageJSON); errSave != nil {
		return empty, abiboot.Errorf("save_auth", "保存凭据失败：%v", errSave)
	}
	response.Auth.FileName = name
	forgetLoginSession(state)
	return response, nil
}

// statusJSON is the machine-readable form of the status page, served when the
// caller asks for `?format=json` or does not accept HTML.
//
// The document reports EVERY account this plugin owns: `accounts` carries one
// entry per account, in host order, each with that account's own credit and
// activity state — the same keys `selected` publishes, lifted to the entry
// level. The selected account's fields stay under `selected` for the consumers
// that read them there.
//
// A failed read is reported as an error field on the entry that failed and never
// as a zero: `credit` is simply absent when it could not be read.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	accounts := lobsteraiAccounts(h)
	cfg := settings()
	version, source := versionResolver.resolve(h, cfg, time.Now())
	body := map[string]any{
		"provider":       ProviderKey,
		"account_count":  len(accounts),
		"accounts":       []map[string]any{},
		"client_version": map[string]any{"version": version, "source": source},
		"config": map[string]any{
			"discover_models": cfg.DiscoverModels,
			"daily_checkin":   cfg.DailyCheckin,
			"max_tokens":      cfg.DefaultMaxTokens,
		},
		"models": summariseModels(currentCatalog(time.Now())),
	}

	entry, found := selectAccount(h, request)
	if !found {
		body["selected"] = nil
		return jsonManagementResponse(http.StatusOK, body)
	}

	quotas := collectAccountQuotas(h, accounts, cfg, version)
	body["accounts"] = quotaListJSON(quotas)

	current, okCurrent := quotaOf(quotas, entry)
	if !okCurrent {
		// Unreachable while accounts and quotas come from the same listing.
		body["selected"] = nil
		body["error"] = "无法定位该账号的积分记录"
		return jsonManagementResponse(http.StatusOK, body)
	}

	selected := map[string]any{
		"auth_index": entry.AuthIndex,
		"name":       entry.Name,
		"status":     statusText(entry),
	}
	if label := strings.TrimSpace(entry.Label); label != "" {
		selected["label"] = label
	}
	body["selected"] = selected
	if current.CredentialErr != nil {
		selected["error"] = current.CredentialErr.Error()
		return jsonManagementResponse(http.StatusOK, body)
	}
	selected["uid"] = current.Credential.UID
	selected["nickname"] = current.Credential.Nickname
	selected["refreshable"] = current.Credential.Refreshable()
	if expiry, ok := current.Credential.Expiry(); ok {
		selected["expires_at"] = expiry.UTC().Format(time.RFC3339)
		selected["expired"] = current.Credential.Expired(0)
	}
	switch {
	case current.BalanceErr != nil:
		// The document is still served: the other accounts' figures and the
		// account count are exactly what the hub needs, and this account's
		// credits are reported as unknown rather than as 0.
		selected["credit_error"] = current.BalanceErr.Error()
	case current.Balance != nil:
		selected["credit"] = map[string]any{
			"total":         current.Balance.Total,
			"expired_total": current.Balance.ExpiredTotal,
			"packages":      current.Balance.Packages,
		}
	}
	switch {
	case current.CheckinDisabled:
		selected["checkin_state"] = "disabled"
	case current.ActivityErr != nil:
		selected["activity_error"] = current.ActivityErr.Error()
	case current.Activity != nil:
		selected["activity"] = map[string]any{
			"slot_state":    current.Activity.SlotState,
			"activity_code": current.Activity.ActivityCode,
		}
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// summariseModels renders the catalog for JSON clients, including every remote
// parameter this adapter consumes.
func summariseModels(models []remoteModel) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, model := range models {
		entry := map[string]any{
			"id":           model.ID,
			"name":         model.Name,
			"display_name": displayNameFor(model),
		}
		if model.ContextWindow != nil {
			entry["context_window"] = *model.ContextWindow
		}
		if model.MaxTokens != nil {
			entry["max_tokens"] = *model.MaxTokens
		}
		if model.SupportsImage != nil {
			entry["supports_image"] = *model.SupportsImage
		}
		if model.CostMultiplier != nil {
			entry["cost_multiplier"] = *model.CostMultiplier
		}
		if model.Thinking != nil {
			options := make([]map[string]string, 0, len(model.Thinking.Options))
			for _, option := range model.Thinking.Options {
				options = append(options, map[string]string{
					"level":         option.Level,
					"openclawLevel": option.OpenclawLevel,
					"display":       effortDisplayNames[option.Level],
				})
			}
			entry["thinking"] = map[string]any{"options": options, "default_level": model.Thinking.DefaultLevel}
		}
		out = append(out, entry)
	}
	return out
}

// checkinResponse serves the check-in route in both representations. The
// outcome always comes from the response body, never from an HTTP status: a
// repeated claim also succeeds upstream, so only the body tells the truth.
func checkinResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	if !cfg.DailyCheckin {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"status": "inactive", "message": "签到已在插件配置中关闭（daily_checkin=false）",
		})
	}
	entry, found := selectAccount(h, request)
	if !found {
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
		}
		return plugui.HTML("LobsterAI 签到",
			plugui.Card("签到失败", plugui.Notice("danger", "指定的账号不存在"),
				plugui.Action{Label: "返回状态", Path: "status"}))
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errCredential.Error()})
		}
		return plugui.HTML("LobsterAI 签到",
			plugui.Card("签到失败", plugui.Notice("danger", errCredential.Error()),
				plugui.Action{Label: "返回状态", Path: "status"}))
	}

	clientVersion := resolveClientVersion(h, cfg)
	outcome := claimDailyCheckin(h, credential, clientVersion, time.Now())
	if wantsJSON(request) {
		body := map[string]any{
			"status": outcome.Kind,
			// The literal response-body verdict, so a caller can see the claim
			// was idempotent rather than a fresh grant.
			"message":          outcome.Message,
			"idempotent_keyed": true,
			"credit_granted":   outcome.CreditGranted,
			"http_status":      http.StatusOK,
		}
		if outcome.CreditGranted {
			body["credit"] = outcome.Credit
		}
		if outcome.DelayedMessage != "" {
			body["server_message"] = outcome.DelayedMessage
		}
		if balance, errBalance := fetchCreditBalance(h, credential); errBalance == nil {
			body["remaining"] = balance.Total
		}
		return jsonManagementResponse(http.StatusOK, body)
	}

	fields := []plugui.Field{
		{Label: "账号", Value: entry.Name},
		{Label: "判定依据", Value: "响应体（kind=" + outcome.Kind + "）"},
		{Label: "服务端消息", Value: orPlaceholder(outcome.Message)},
	}
	if outcome.DelayedMessage != "" {
		fields = append(fields, plugui.Field{Label: "活动消息", Value: outcome.DelayedMessage})
	}
	if outcome.CreditGranted {
		fields = append(fields, plugui.Field{Label: "本次获得积分", Value: fmt.Sprintf("%.2f", outcome.Credit)})
	} else {
		fields = append(fields, plugui.Field{Label: "本次获得积分", Value: "响应体未返回积分字段"})
	}
	if balance, errBalance := fetchCreditBalance(h, credential); errBalance == nil {
		fields = append(fields, plugui.Field{Label: "当前剩余积分", Value: fmt.Sprintf("%.2f", balance.Total)})
	} else {
		fields = append(fields, plugui.Field{Label: "当前剩余积分", Value: "查询失败：" + errBalance.Error()})
	}

	return plugui.HTML("LobsterAI 签到",
		plugui.Card("签到结果",
			plugui.Group(
				checkinNotice(outcome),
				plugui.Notice("", "签到是客户端幂等的：请求携带 idempotencyKey，且签到前先查 state.claimedToday 与 actions。"+
					"重复签到同样会返回成功，因此本页按响应体判定结果，而不是看 HTTP 状态码。"),
				plugui.Fields(fields...),
			),
			plugui.Action{Label: "再次签到", Query: "action=checkin"},
			plugui.Action{Label: "返回状态", Path: "status", Kind: "primary"},
		),
	)
}

// checkinNotice renders one check-in outcome.
func checkinNotice(outcome claimOutcome) template.HTML {
	message := outcome.Message
	if outcome.Kind == "claimed" && outcome.CreditGranted {
		message = fmt.Sprintf("%s，获得 %.2f 积分", message, outcome.Credit)
	}
	switch outcome.Kind {
	case "claimed":
		return plugui.Notice("success", message)
	case "already-claimed":
		return plugui.Notice("warning", message)
	case "inactive":
		return plugui.Notice("warning", message)
	default:
		return plugui.Notice("danger", message)
	}
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

// formatCredentialExpiry renders the credential validity.
func formatCredentialExpiry(credential *Credential) string {
	expiry, ok := credential.Expiry()
	if !ok {
		return "未知（无法解析 expires_at 与 JWT exp）"
	}
	remaining := time.Until(expiry)
	if remaining <= 0 {
		return expiry.Local().Format("2006-01-02 15:04") + "（已过期）"
	}
	return fmt.Sprintf("%s（剩余 %s）", expiry.Local().Format("2006-01-02 15:04"), remaining.Truncate(time.Minute))
}

// slotText summarises the daily activity slot.
func slotText(slot *activitySlot) string {
	if slot.SlotState != "available" {
		return "无可用活动（slotState=" + orPlaceholder(slot.SlotState) + "）"
	}
	return "可签到（activityCode=" + orPlaceholder(slot.ActivityCode) + "）"
}

func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

func orPlaceholder(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}
