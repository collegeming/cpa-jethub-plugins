package main

// 本文件实现 management.register / management.handle，即 CPA-Manager-Plus 里
// 用户直接看到的页面（账号状态 / 浏览器登录 / 每日签到）。
//
// 宿主的两条挂载规则（实测，也是本文件结构的原因）：
//   - **GET + Menu** 的路由只挂在 `/v0/resource/plugins/<id>/<path>`，也就是
//     CPAMP iframe 实际加载的地址；该挂载**只按 GET 派发**，所以页面上的每个
//     操作都是带查询参数的链接（plugui.Action 现在渲染成 <a href="?…">）。
//   - **其余**路由只挂在 `/v0/management/<path>`，那是**全局命名空间**，必须
//     自带插件前缀（`/codebuddy/...`），否则与别的插件冲突会被宿主静默跳过。
//
// 侧边栏只有一条（hub 的 "Jet Hub"），所以本插件的页面全部注册为无 Menu 的
// ResourceRoute：`/status` 由 hub 的渠道总览链接进入，`/login`、`/checkin`
// 由状态页进入。脚本/API 仍走自命名空间的 GET/POST 路由，两种表示共用同一批
// 处理函数（`?format=json` 或非 HTML Accept 时输出 JSON）。

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

// managementRoute reduces the incoming path to the registered route.
//
// The host passes the full path, which differs between the two mounts
// (`/v0/management/codebuddy/status` vs
// `/v0/resource/plugins/codebuddy/status`), so only the last segment identifies
// the route in both cases.
func managementRoute(path string) string {
	trimmed := strings.TrimSpace(path)
	// The host passes r.URL.Path (no query), but tolerate a full request URI so
	// a stray "?format=json" can never turn a valid route into a 404.
	if index := strings.IndexAny(trimmed, "?#"); index >= 0 {
		trimmed = trimmed[:index]
	}
	trimmed = strings.TrimSuffix(trimmed, "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return "/" + trimmed
}

// wantsJSON reports whether the caller asked for machine-readable output.
//
// `?format=json` always wins; a browser Accept header always means the page.
// When the client accepts anything (`*/*`, or no Accept at all) the **mount**
// decides: `/v0/resource/plugins/<id>/...` is the address a management UI embeds
// in an iframe, so it renders the page, while `/v0/management/<path>` is the
// script/API namespace and answers JSON. That keeps `curl <resource>/status`
// showing the page without special-casing curl.
func wantsJSON(request pluginapi.ManagementRequest) bool {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("format"))) {
	case "json":
		return true
	case "html":
		return false
	}
	accept := strings.ToLower(request.Headers.Get("Accept"))
	if strings.Contains(accept, "text/html") {
		return false
	}
	if accept == "" || strings.Contains(accept, "*/*") {
		return !isResourceMount(request.Path)
	}
	return true
}

// isResourceMount reports whether the request arrived on the browser-navigable
// resource namespace.
func isResourceMount(path string) bool {
	return strings.Contains(path, "/resource/plugins/")
}

// handleManagementRegister declares the status page as a Menu-less resource plus
// the browser pages and JSON endpoints behind it. This plugin contributes NO
// sidebar entry.
//
// Three mounts, all decided by the host:
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/<id>/<path>` AND becomes its own sidebar entry in
//     CPA-Manager-Plus. The manager renders one nav item per menu route and does
//     not group them by plugin, so this repository gives that one entry to the
//     hub plugin and none to any provider: the sidebar is a single "Jet Hub" row
//     that links to `/v0/resource/plugins/<id>/status`.
//   - a ResourceRoute is registered under the same prefix and is listed in the
//     sidebar only when it carries a Menu. Leaving Menu empty keeps the page
//     browser-reachable — the hub and the login page link to it — without adding
//     a nav item.
//   - any other route is registered under `/v0/management/<path>`, a GLOBAL
//     namespace, so its path must be prefixed with the provider key or a
//     collision is skipped with a warning.
//
// Login has no sidebar entry on purpose: the manager's own "OAuth 登录" page
// discovers every plugin that declares the auth-provider capability and drives
// `auth.login.start` / `auth.login.poll` itself, then saves the credential.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	product, _ := productByConfigValue(settings().Product)
	resources := []pluginapi.ResourceRoute{
		{Path: "/status", Description: "账号、凭据有效期与积分余额（由 hub 的渠道总览链接进入）"},
		{Path: "/login", Description: "浏览器登录 CodeBuddy / WorkBuddy 账号（由状态页或 OAuth 登录页进入）"},
	}
	// 只有 CodeBuddy 国内版有签到接口（product.ts:366-367）。
	if supportsCheckin(product) {
		resources = append(resources,
			pluginapi.ResourceRoute{Path: "/checkin", Description: "查询并领取每日积分（由状态页进入）"},
		)
	}
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			// 脚本/API 用：不带 Menu，因此挂在全局管理命名空间下，必须自带前缀。
			{Method: http.MethodGet, Path: "/" + ProviderKey + "/status", Description: "账号状态（JSON）"},
			{Method: http.MethodPost, Path: "/" + ProviderKey + "/checkin", Description: "执行每日签到（JSON）"},
		},
		Resources: resources,
	}, nil
}

// handleManagementHandle dispatches the routes declared above.
func handleManagementHandle(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ManagementRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	route := managementRoute(request.Path)

	switch route {
	case "/status":
		if wantsJSON(request) {
			return statusJSON(h, request), nil
		}
		return renderStatusPage(h, request), nil
	case "/login":
		if wantsJSON(request) {
			return loginJSON(request), nil
		}
		return renderLoginPage(h, request), nil
	case "/checkin":
		if wantsJSON(request) {
			return checkinJSON(h, request), nil
		}
		return renderCheckinPage(h, request), nil
	}
	// The same dispatch serves both mounts: `/v0/resource/plugins/<id>/status`
	// and `/v0/management/codebuddy/status` both reduce to "/status".
	return jsonManagementResponse(http.StatusNotFound, map[string]any{
		"error": "unknown CodeBuddy management route",
		"path":  request.Path,
	}), nil
}

// ── 账号解析 ──

// codebuddyAccounts returns the host's CodeBuddy credentials, in host order.
func codebuddyAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
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

// selectAccount resolves which credential a page acts on. Without an explicit
// selector the first account is used, so the page works with no parameters.
func selectAccount(h *abiboot.Host, request pluginapi.ManagementRequest) (pluginapi.HostAuthFileEntry, bool) {
	wanted := strings.TrimSpace(request.Query.Get("auth_index"))
	if wanted == "" {
		wanted = strings.TrimSpace(request.Query.Get("auth_id"))
	}
	for _, entry := range codebuddyAccounts(h) {
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

// ── 页面 ──

// renderStatusPage renders the account overview, the balance and the product
// description.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	accounts := codebuddyAccounts(h)
	if len(accounts) == 0 {
		return plugui.HTML("CodeBuddy",
			plugui.Card("尚未添加账号",
				plugui.Group(
					plugui.Notice("warning", "当前实例还没有 CodeBuddy / WorkBuddy 账号。先在浏览器完成一次登录授权即可。"),
					plugui.Notice("", "本插件一次只服务一个产品；请先在插件配置里选定 product（codebuddy / codebuddy-intl / workbuddy-cn / workbuddy）。"),
				),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
			),
			productCard(settings().Product, nil),
		)
	}

	entry, found := selectAccount(h, request)
	if !found {
		return plugui.HTML("CodeBuddy",
			plugui.Card("账号不存在", plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。")),
		)
	}

	body := make([]template.HTML, 0, 5)
	accountFields := []plugui.Field{
		{Label: "名称", Value: entry.Name},
		{Label: "状态", Value: statusText(entry)},
	}
	if entry.AuthIndex != "" {
		accountFields = append(accountFields, plugui.Field{Label: "索引", Value: entry.AuthIndex})
	}

	var credential *Credential
	creditFields := []plugui.Field{}
	if parsed, errCredential := credentialOf(h, entry); errCredential != nil {
		creditFields = append(creditFields, plugui.Field{Label: "凭据", Value: "无法读取：" + errCredential.Error()})
	} else {
		credential = parsed
		product := productForCredential(credential)
		accountFields = append(accountFields,
			plugui.Field{Label: "产品", Value: product.DisplayName + "（" + product.ConfigValue + "）"},
			plugui.Field{Label: "端点", Value: product.Endpoint},
			plugui.Field{Label: "有效期至", Value: formatExpiry(credential.Expiry())},
			plugui.Field{Label: "可自动续期", Value: yesNo(credential.Refreshable())},
			plugui.Field{Label: "模型数", Value: fmt.Sprintf("%d", catalogSize(product))},
		)
		if credential.Nickname != "" {
			accountFields = append(accountFields, plugui.Field{Label: "昵称", Value: credential.Nickname})
		}
		if credential.UserID != "" {
			accountFields = append(accountFields, plugui.Field{Label: "用户 ID", Value: credential.UserID})
		}
		if credential.AccountType != "" {
			accountFields = append(accountFields, plugui.Field{Label: "账号类型", Value: credential.AccountType})
		}
	}

	if credential != nil {
		product := productForCredential(credential)
		if balance := fetchCreditBalance(h, credential, product); balance != nil {
			creditFields = append(creditFields,
				plugui.Field{Label: "本周期剩余积分", Value: fmt.Sprintf("%.2f", balance.Total)},
				plugui.Field{Label: "资源包数量", Value: fmt.Sprintf("%d", len(balance.Packages))},
			)
			if balance.ExpiredTotal > 0 {
				creditFields = append(creditFields, plugui.Field{Label: "已失效积分", Value: fmt.Sprintf("%.2f", balance.ExpiredTotal)})
			}
			for index, pkg := range balance.Packages {
				if index >= 8 {
					break
				}
				state := "有效"
				if !pkg.Active {
					state = "已失效"
				}
				creditFields = append(creditFields, plugui.Field{
					Label: pkg.Name,
					Value: fmt.Sprintf("%.2f / %.2f（%s）", pkg.Remaining, pkg.Total, state),
				})
			}
		} else {
			creditFields = append(creditFields, plugui.Field{Label: "积分", Value: "查询失败或上游不可达"})
		}
		if supportsCheckin(product) {
			if status := fetchCheckinStatus(h, credential, product); status != nil {
				creditFields = append(creditFields, plugui.Field{Label: "今日签到", Value: checkinStatusText(status)})
			} else {
				creditFields = append(creditFields, plugui.Field{Label: "今日签到", Value: "查询失败或活动未开启"})
			}
		} else {
			creditFields = append(creditFields, plugui.Field{Label: "今日签到", Value: "该产品无签到接口"})
		}
	}

	actions := []plugui.Action{
		{Label: "重新登录", Path: "login"},
	}
	if credential != nil && supportsCheckin(productForCredential(credential)) {
		actions = append([]plugui.Action{{Label: "领取每日积分", Path: "checkin", Kind: "primary"}}, actions...)
	}
	body = append(body, plugui.Card("账号", plugui.Fields(accountFields...), actions...))
	if len(creditFields) > 0 {
		body = append(body, plugui.Card("积分", plugui.Fields(creditFields...)))
	}
	if switcher := renderAccountList(accounts, entry.AuthIndex); switcher != "" {
		body = append(body, switcher)
	}
	configured, _ := productByConfigValue(settings().Product)
	var accountProduct *productConfig
	if credential != nil {
		selected := productForCredential(credential)
		accountProduct = &selected
	}
	body = append(body, productCard(configured.ConfigValue, accountProduct))
	return plugui.HTML("CodeBuddy", body...)
}

// productCard describes the configured product and, when it differs, the
// product the selected account was created against.
func productCard(configuredValue string, accountProduct *productConfig) template.HTML {
	configured, _ := productByConfigValue(configuredValue)
	fields := []plugui.Field{
		{Label: "插件配置产品", Value: configured.DisplayName + "（" + configured.ConfigValue + "）"},
		{Label: "API 端点", Value: configured.Endpoint},
		{Label: "X-Domain", Value: configured.APIDomain},
		{Label: "X-Product-Code", Value: configured.ProductCode},
		{Label: "User-Agent", Value: configured.UserAgent},
		{Label: "动态发现模型", Value: yesNo(settings().DiscoverModels)},
	}
	body := plugui.Fields(fields...)
	if accountProduct != nil && accountProduct.ConfigValue != configured.ConfigValue {
		body = plugui.Group(body, plugui.Notice("warning",
			"当前账号创建于 "+accountProduct.DisplayName+"（"+accountProduct.ConfigValue+"），与插件配置的产品不同；"+
				"请求仍按账号记录的产品发往 "+accountProduct.Endpoint+"。"))
	}
	return plugui.Card("产品", body)
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

// renderLoginPage renders the two-step browser login.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "login", "start":
		return startLoginPage(h)
	case "poll":
		return pollLoginPage(h, request)
	}
	product, _ := productByConfigValue(settings().Product)
	return plugui.HTML("CodeBuddy 登录",
		plugui.Card("浏览器登录",
			plugui.Group(
				plugui.Notice("", "点击下面的按钮获取授权链接。链接由服务端下发（POST /v2/plugin/auth/state），"+
					"在浏览器里完成授权后回到本页检查结果 —— 本页不会阻塞等待。"),
				plugui.Fields(
					plugui.Field{Label: "当前产品", Value: product.DisplayName + "（" + product.ConfigValue + "）"},
					plugui.Field{Label: "授权平台", Value: product.Platform},
				),
			),
			plugui.Action{Label: "开始登录", Query: "action=login", Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// startLoginPage begins a login session and shows the authorization URL.
func startLoginPage(h *abiboot.Host) pluginapi.ManagementResponse {
	result, errStart := startLogin(h, settings())
	if errStart != nil {
		return plugui.HTML("CodeBuddy 登录",
			plugui.Card("无法发起登录", plugui.Notice("danger", errStart.Error()),
				plugui.Action{Label: "重试", Query: "action=login", Kind: "primary"}))
	}
	body := []template.HTML{}
	if result.Fallback {
		body = append(body, plugui.Notice("warning",
			"上游 auth/state 不可达（"+result.FallbackWhy+"），已给出兜底登录地址。"+
				"该地址与官方客户端打开的一致，但没有服务端下发的 state，无法完成轮询换取令牌；"+
				"请恢复网络后重新点击「开始登录」。"))
	} else {
		body = append(body, plugui.Notice("", "请在浏览器中打开下面的链接完成授权，然后点击「检查登录结果」。"))
	}
	body = append(body, plugui.Fields(
		plugui.Field{Label: "授权链接", Value: result.AuthURL},
		plugui.Field{Label: "产品", Value: result.Session.Product},
		plugui.Field{Label: "state", Value: result.State},
		plugui.Field{Label: "有效期", Value: "5 分钟"},
	))
	return plugui.HTML("CodeBuddy 登录",
		plugui.Card("在浏览器中完成授权", plugui.Group(body...),
			plugui.Action{Label: "检查登录结果", Query: "action=poll&state=" + result.State, Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// pollLoginPage polls a login session, persisting the credential on success.
func pollLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	state := strings.TrimSpace(request.Query.Get("state"))
	response, errPoll := pollLoginForManagement(h, state)
	if errPoll != nil {
		return plugui.HTML("CodeBuddy 登录",
			plugui.Card("检查失败", plugui.Notice("danger", errPoll.Error()),
				plugui.Action{Label: "重新登录", Query: "action=login", Kind: "primary"}))
	}
	switch response.Status {
	case pluginapi.AuthLoginStatusSuccess:
		message := response.Message
		if message == "" {
			message = "登录成功，账号已保存。"
		}
		return plugui.HTML("CodeBuddy 登录",
			plugui.Card("登录成功", plugui.Notice("success", message),
				plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"}))
	case pluginapi.AuthLoginStatusPending, "":
		return plugui.HTML("CodeBuddy 登录",
			plugui.Card("等待授权",
				plugui.Group(
					plugui.Notice("", "还没有拿到令牌或账户信息。请先在浏览器里完成授权，然后再次检查。"),
					plugui.Fields(plugui.Field{Label: "state", Value: state}),
				),
				plugui.Action{Label: "再次检查", Query: "action=poll&state=" + state, Kind: "primary"},
				plugui.Action{Label: "重新开始", Query: "action=login"},
			))
	default:
		message := response.Message
		if message == "" {
			message = "登录失败。"
		}
		return plugui.HTML("CodeBuddy 登录",
			plugui.Card("登录失败", plugui.Notice("danger", message),
				plugui.Action{Label: "重新登录", Query: "action=login", Kind: "primary"}))
	}
}

// pollLoginForManagement reuses the auth.login.poll handler and then persists the
// credential itself: the management page drives the poll directly, so the host
// never sees the login result and will not save it.
func pollLoginForManagement(h *abiboot.Host, state string) (pluginapi.AuthLoginPollResponse, error) {
	var empty pluginapi.AuthLoginPollResponse
	if state == "" {
		return empty, abiboot.Errorf("missing_state", "缺少 state 参数，请重新发起登录")
	}
	raw, errMarshal := json.Marshal(pluginapi.AuthLoginPollRequest{State: state})
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
			name = ProviderKey + "-" + time.Now().Format("20060102150405") + ".json"
		}
	}
	if _, errSave := h.SaveAuth(name, response.Auth.StorageJSON); errSave != nil {
		return empty, abiboot.Errorf("save_auth", "保存凭据失败：%v", errSave)
	}
	forgetLoginSession(state)
	return response, nil
}

// renderCheckinPage renders the daily check-in page, performing the claim when
// the page is opened with ?action=checkin.
func renderCheckinPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return plugui.HTML("CodeBuddy 积分",
			plugui.Card("无法签到", plugui.Notice("danger", "没有可用账号，请先登录。"),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"}))
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return plugui.HTML("CodeBuddy 积分",
			plugui.Card("无法签到", plugui.Notice("danger", errCredential.Error()),
				plugui.Action{Label: "返回状态", Path: "status"}))
	}
	product := productForCredential(credential)
	if !supportsCheckin(product) {
		return plugui.HTML("CodeBuddy 积分",
			plugui.Card("该产品没有签到接口",
				plugui.Group(
					plugui.Notice("warning", product.DisplayName+"（"+product.ConfigValue+"）后端没有 /v2/billing/meter/daily-checkin 接口。"+
						"积分余额仍可在状态页查看。"),
					plugui.Notice("", "签到能力只存在于 CodeBuddy 国内版（product=codebuddy）。"),
				),
				plugui.Action{Label: "返回状态", Path: "status", Kind: "primary"},
			))
	}

	body := []template.HTML{}
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "checkin") {
		body = append(body, checkinNotice(claimDailyCheckin(h, credential, product)))
	}
	fields := []plugui.Field{
		{Label: "账号", Value: entry.Name},
	}
	if status := fetchCheckinStatus(h, credential, product); status != nil {
		fields = append(fields,
			plugui.Field{Label: "活动", Value: status.ActivityName},
			plugui.Field{Label: "今日状态", Value: checkinStatusText(status)},
			plugui.Field{Label: "每日可领", Value: fmt.Sprintf("%.2f", status.DailyCredit)},
			plugui.Field{Label: "连续签到", Value: fmt.Sprintf("%.0f 天", status.StreakDays)},
		)
		if status.EndTime != "" {
			fields = append(fields, plugui.Field{Label: "活动结束", Value: status.EndTime})
		}
	} else {
		fields = append(fields, plugui.Field{Label: "状态", Value: "查询失败或活动未开启"})
	}
	body = append(body, plugui.Card("签到", plugui.Fields(fields...),
		plugui.Action{Label: "领取今日积分", Query: "action=checkin", Kind: "primary"},
		plugui.Action{Label: "返回状态", Path: "status"},
	))
	return plugui.HTML("CodeBuddy 积分", body...)
}

// checkinNotice renders one claim outcome. The outcome comes from the response
// body, never from an HTTP status code: a repeated claim is reported as an
// error by HTTP status but carries the "already claimed" business code.
func checkinNotice(outcome claimOutcome) template.HTML {
	message := outcome.Message
	if outcome.Kind == "claimed" && outcome.Credit > 0 {
		message = fmt.Sprintf("%s，获得 %.2f 积分", message, outcome.Credit)
	}
	switch outcome.Kind {
	case "claimed":
		return plugui.Notice("success", message)
	case "already-claimed":
		return plugui.Notice("", message)
	case "inactive":
		return plugui.Notice("warning", message)
	default:
		return plugui.Notice("danger", message)
	}
}

// checkinStatusText summarises one check-in status record.
func checkinStatusText(status *checkinStatus) string {
	switch {
	case !status.Active:
		return "活动未开启"
	case status.TodayCheckedIn:
		return "今日已签到"
	default:
		return "今日可签到"
	}
}

// ── JSON 表示 ──

// statusJSON is the machine-readable status page.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	accounts := codebuddyAccounts(h)
	configured, _ := productByConfigValue(settings().Product)
	body := map[string]any{
		"product":         configured.ConfigValue,
		"product_name":    configured.DisplayName,
		"endpoint":        configured.Endpoint,
		"discover_models": settings().DiscoverModels,
		"account_count":   len(accounts),
		"accounts":        accountSummaries(accounts),
	}
	if len(accounts) == 0 {
		return jsonManagementResponse(http.StatusOK, body)
	}
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errCredential.Error()})
	}
	product := productForCredential(credential)
	body["auth_index"] = entry.AuthIndex
	body["name"] = entry.Name
	body["status"] = statusText(entry)
	body["account_product"] = product.ConfigValue
	body["model_count"] = catalogSize(product)
	if expiry := credential.Expiry(); !expiry.IsZero() {
		body["expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	body["refreshable"] = credential.Refreshable()
	if balance := fetchCreditBalance(h, credential, product); balance != nil {
		body["credits"] = map[string]any{
			"total":         balance.Total,
			"expired_total": balance.ExpiredTotal,
			"packages":      balance.Packages,
		}
	} else {
		body["credits"] = nil
	}
	if supportsCheckin(product) {
		if status := fetchCheckinStatus(h, credential, product); status != nil {
			body["checkin"] = status
		}
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// accountSummaries renders the account list without touching upstream.
func accountSummaries(accounts []pluginapi.HostAuthFileEntry) []map[string]any {
	out := make([]map[string]any, 0, len(accounts))
	for _, entry := range accounts {
		summary := map[string]any{
			"auth_index": entry.AuthIndex,
			"name":       entry.Name,
			"status":     statusText(entry),
			"disabled":   entry.Disabled,
		}
		if entry.Label != "" {
			summary["label"] = entry.Label
		}
		out = append(out, summary)
	}
	return out
}

// loginJSON describes the login route for scripts.
func loginJSON(request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"product": settings().Product,
		"action":  strings.TrimSpace(request.Query.Get("action")),
		"hint":    "GET ?action=login 获取授权链接；GET ?action=poll&state=<state> 查询结果",
	})
}

// checkinJSON is the script-facing representation of the check-in route.
func checkinJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errCredential.Error()})
	}
	product := productForCredential(credential)
	if !supportsCheckin(product) {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"supported": false,
			"product":   product.ConfigValue,
			"message":   "该产品没有每日签到接口",
		})
	}
	status := fetchCheckinStatus(h, credential, product)
	// 页面用 ?action=checkin 触发领取；脚本可直接 POST 本路由。
	claim := request.Method != http.MethodGet ||
		strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "checkin")
	if !claim {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"supported": true,
			"product":   product.ConfigValue,
			"status":    status,
		})
	}
	outcome := claimDailyCheckin(h, credential, product)
	body := map[string]any{
		"supported": true,
		"product":   product.ConfigValue,
		"status":    status,
		"outcome":   outcome.Kind,
		"message":   outcome.Message,
		"credit":    outcome.Credit,
	}
	if outcome.Kind == "failed" {
		return jsonManagementResponse(http.StatusBadGateway, body)
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// ── 展示辅助 ──

// catalogSize reports how many models the account can use. The cached
// (discovered) catalog wins when it is warm; otherwise the built-in table is
// reported, and no network call is made just to render a page.
func catalogSize(product productConfig) int {
	ttl := time.Duration(settings().ModelCacheTTLMS) * time.Millisecond
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if cached, ok := discoveredModels.get(cachedCatalogKey(product), ttl); ok {
		return len(cached)
	}
	return len(staticCatalog(product))
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

func formatExpiry(expiry time.Time) string {
	if expiry.IsZero() {
		return "未知"
	}
	remaining := time.Until(expiry)
	if remaining <= 0 {
		return expiry.Local().Format("2006-01-02 15:04") + "（已过期）"
	}
	return fmt.Sprintf("%s（剩余 %s）", expiry.Local().Format("2006-01-02 15:04"), remaining.Truncate(time.Minute))
}

func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// jsonManagementResponse renders a management API reply.
func jsonManagementResponse(status int, body any) pluginapi.ManagementResponse {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		encoded = []byte(`{"error":"failed to encode response"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       encoded,
	}
}
