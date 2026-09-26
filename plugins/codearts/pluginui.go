package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
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

// wantsJSON reports whether the caller asked for machine-readable output.
func wantsJSON(request pluginapi.ManagementRequest) bool {
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("format")), "json") {
		return true
	}
	// A page navigation sends an Accept header mentioning text/html; anything
	// else is treated as a script or a client that wants data.
	accept := strings.ToLower(request.Headers.Get("Accept"))
	return accept != "" && !strings.Contains(accept, "text/html")
}

// codeartsAccounts returns the host's CodeArts credentials, in host order.
func codeartsAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
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

// accountQuery is the query string that names an account on a page link. It is
// empty when the host gave the entry no runtime index, so a link never carries a
// dangling selector.
func accountQuery(entry pluginapi.HostAuthFileEntry) string {
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return ""
	}
	return "auth_index=" + entry.AuthIndex
}

// selectAccount resolves which credential a page should act on. Without an
// explicit selector the first account is used, so the page works with no
// parameters at all.
func selectAccount(h *abiboot.Host, request pluginapi.ManagementRequest) (pluginapi.HostAuthFileEntry, bool) {
	wanted := strings.TrimSpace(request.Query.Get("auth_index"))
	if wanted == "" {
		wanted = strings.TrimSpace(request.Query.Get("auth_id"))
	}
	for _, entry := range codeartsAccounts(h) {
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

// renderStatusPage renders the account overview, optionally after performing a
// check-in.
//
// There is ONE card per account, in the same order as the account list, and each
// card carries that account's own figures — never the selected account's numbers
// repeated. Both the upstream reads and the cards are per account, so a channel
// with several accounts shows several balances, which is what the reference
// panel does.
func renderStatusPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	accounts := codeartsAccounts(h)
	if len(accounts) == 0 {
		return plugui.HTML("CodeArts Agent",
			plugui.Card("尚未添加账号",
				plugui.Notice("warning", "当前实例还没有 CodeArts 账号。先在浏览器完成一次登录授权即可。"),
				plugui.Action{Label: "去登录", Path: "login", Kind: "primary"},
			),
		)
	}

	entry, found := selectAccount(h, request)
	if !found {
		return plugui.HTML("CodeArts Agent",
			plugui.Card("账号不存在", plugui.Notice("danger", "指定的 auth_index 不在本插件的账号列表里。")),
		)
	}

	quotas := collectAccountQuotas(h, accounts)

	body := make([]template.HTML, 0, len(quotas)+2)
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("action")), "checkin") {
		body = append(body, renderCheckinOutcome(h, entry))
	}
	for _, quota := range quotas {
		body = append(body, renderQuotaCard(quota, quota.Entry.AuthIndex == entry.AuthIndex))
	}
	// 新建账号 stays reachable even for a single account, which is why the
	// account list card is rendered whenever there is at least one account.
	body = append(body, renderAccountList(accounts, entry.AuthIndex))
	return plugui.HTML("CodeArts Agent", body...)
}

// renderQuotaCard renders one account: its identity, its OWN quota and its OWN
// check-in state, with the two per-account actions.
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
			plugui.Field{Label: "有效期至", Value: formatExpiry(quota.Credential.Expiry())},
			plugui.Field{Label: "可自动续期", Value: yesNo(quota.Credential.Refreshable())},
		)
		if quota.Credential.UserName != "" {
			fields = append(fields, plugui.Field{Label: "用户", Value: quota.Credential.UserName})
		}
		switch {
		case quota.BalanceErr != nil:
			fields = append(fields, plugui.Field{Label: "额度", Value: "查询失败：" + quota.BalanceErr.Error()})
		case quota.Balance != nil:
			fields = append(fields,
				plugui.Field{Label: "计费方式", Value: creditPackageText(quota.Balance.IsCreditPackage)},
				plugui.Field{Label: "剩余额度", Value: formatCredit(quota.Balance.Remaining)},
				plugui.Field{Label: "已用额度", Value: formatCredit(quota.Balance.Used)},
			)
			switch {
			case quota.ActivityErr != nil:
				fields = append(fields, plugui.Field{Label: "今日签到", Value: "查询失败：" + quota.ActivityErr.Error()})
			case quota.Activity != nil:
				fields = append(fields, plugui.Field{Label: "今日签到", Value: checkinText(quota.Activity)})
			}
		}
	}
	title := "额度 · " + entry.Name
	if current {
		title += "（当前）"
	}
	return plugui.Card(title, plugui.Fields(fields...),
		plugui.Action{Label: "签到", Query: "action=checkin&" + accountQuery(entry), Kind: "primary"},
		// 重新登录 names the account it re-authorises, so the new credential
		// replaces that account's file instead of adding a second entry.
		plugui.Action{Label: "重新登录", Path: "login", Query: accountQuery(entry)},
	)
}

// formatCredit renders a credit amount the way the provider reports it: two
// decimals, like the balance endpoint and the reference panel do.
func formatCredit(value float64) string {
	return fmt.Sprintf("%.2f", value)
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

// renderCheckinOutcome performs the daily check-in and renders its result.
func renderCheckinOutcome(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) template.HTML {
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return plugui.Notice("danger", "无法读取凭据："+errCredential.Error())
	}
	outcome, errClaim := claimDaily(h, credential)
	if errClaim != nil {
		return plugui.Notice("danger", "签到失败："+errClaim.Error())
	}
	return checkinNotice(outcome)
}

// checkinNotice renders one check-in outcome. The status comes from the response
// body, never from an HTTP status code: a repeated claim also succeeds upstream,
// so only the body tells the truth.
func checkinNotice(outcome *checkinOutcome) template.HTML {
	message := outcome.Message
	if outcome.Amount > 0 {
		message = fmt.Sprintf("%s，获得 %.2f 额度", message, outcome.Amount)
	}
	switch outcome.Status {
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

// statusJSON is the machine-readable form of the status page, served when the
// caller asks for `?format=json` or does not accept HTML.
//
// The document reports EVERY account this plugin owns: `accounts` carries one
// entry per account, in host order, each with that account's own quota and
// check-in state. The selected account's fields stay at the top level, so a
// consumer that only ever read the flat document (the hub's channel overview,
// CPAMP) keeps working unchanged.
//
// A failed read is reported as an error field on the entry that failed and never
// as a zero: remaining/used/total are simply absent when they could not be read.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	accounts := codeartsAccounts(h)
	if len(accounts) == 0 {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"account": nil, "accounts": []map[string]any{}, "account_count": 0,
		})
	}
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
	}

	quotas := collectAccountQuotas(h, accounts)
	body := map[string]any{
		"account_count": len(accounts),
		"accounts":      quotaListJSON(quotas),
		"auth_index":    entry.AuthIndex,
		"name":          entry.Name,
		"status":        statusText(entry),
	}
	if label := strings.TrimSpace(entry.Label); label != "" {
		body["label"] = label
	}

	current, okCurrent := quotaOf(quotas, entry)
	if !okCurrent {
		// Unreachable while accounts and quotas come from the same listing.
		body["error"] = "无法定位该账号的额度记录"
		return jsonManagementResponse(http.StatusOK, body)
	}
	switch {
	case current.CredentialErr != nil:
		body["error"] = current.CredentialErr.Error()
	case current.BalanceErr != nil:
		// The document is still served: the other accounts' figures and the
		// account count are exactly what the hub needs, and this account's
		// balance is reported as unknown rather than as 0.
		body["credit_error"] = current.BalanceErr.Error()
	default:
		body["credit_package"] = current.Balance.IsCreditPackage
		body["remaining"] = current.Balance.Remaining
		body["used"] = current.Balance.Used
		body["total"] = current.Balance.Total
	}
	if current.Activity != nil {
		body["daily_checkin"] = map[string]any{
			"campaign_id": current.Activity.CampaignID,
			"claimable":   current.Activity.Claimable,
			"status":      current.Activity.Status,
		}
	}
	if current.ActivityErr != nil {
		body["activity_error"] = current.ActivityErr.Error()
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// checkinJSON serves the script/API check-in route.
//
// This route is deliberately JSON-only: the interactive check-in lives on the
// status page as `GET /status?action=checkin`, and a machine caller should not
// have to guess a representation.
func checkinJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
	}
	credential, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errCredential.Error()})
	}
	outcome, errClaim := claimDaily(h, credential)
	if errClaim != nil {
		return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": errClaim.Error()})
	}
	body := map[string]any{
		"status":  outcome.Status,
		"message": outcome.Message,
		"amount":  outcome.Amount,
	}
	if outcome.Balance != nil {
		body["remaining"] = outcome.Balance.Remaining
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// renderLoginPage renders the two-step browser login: the first request hands
// the user an authorization URL and later requests poll for the result. Nothing
// ever blocks waiting for the user to finish.
func renderLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "login", "start":
		return startLoginPage(h, request)
	case "poll":
		return pollLoginPage(h, request)
	}

	note := "点击下面的按钮获取授权链接，在浏览器里完成授权后回到本页检查结果。"
	if settings().Flow == LoginFlowTicket {
		note = "当前配置为旧版票据登录流程（flow=ticket）。点击按钮获取登录链接。"
	}
	notices := []template.HTML{}
	if plugui.IsAddAccountRequest(request) {
		notices = append(notices, plugui.Notice("", plugui.AddAccountNotice))
	}
	notices = append(notices, plugui.Notice("", note))
	return plugui.HTML("CodeArts 登录",
		plugui.Card("浏览器登录",
			plugui.Group(notices...),
			plugui.Action{Label: "获取授权链接", Query: "action=login", Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// startLoginPage begins a login session and shows the authorization URL.
//
// The account a re-login targets, when there is one, rides along the poll link:
// that page has to know which file the finished credential belongs in.
func startLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	target := loginTargetName(h, request)
	session, errStart := startLoginSession(settings().Flow, target)
	if errStart != nil {
		return plugui.HTML("CodeArts 登录",
			plugui.Card("无法发起登录", plugui.Notice("danger", errStart.Error()),
				plugui.Action{Label: "重试", Query: "action=login", Kind: "primary"}))
	}
	pollQuery := "action=poll&state=" + session.State
	if target != "" {
		pollQuery += "&target=" + url.QueryEscape(target)
	}
	return plugui.HTML("CodeArts 登录",
		plugui.Card("在浏览器中完成授权",
			plugui.Group(
				plugui.Notice("", fmt.Sprintf("已在本地端口 %d 监听回调。请在浏览器中打开下面的链接完成授权，然后点击「检查登录结果」。", session.Port)),
				plugui.Fields(
					plugui.Field{Label: "授权链接", Value: session.LoginURL()},
					plugui.Field{Label: "回调地址", Value: session.RedirectURI()},
					plugui.Field{Label: "有效期", Value: "5 分钟"},
				),
			),
			plugui.Action{Label: "检查登录结果", Query: pollQuery, Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// loginTargetName resolves the auth file a re-login has to update, or "" when
// this page is adding an account.
//
// The page names the account as an auth_index (the status page's 重新登录 link)
// or as a target file name (the login page's poll link). Either way the value is
// only accepted when it is one of THIS plugin's own accounts: the page must
// never be able to point a save at an unrelated credential's file.
func loginTargetName(h *abiboot.Host, request pluginapi.ManagementRequest) string {
	if plugui.IsAddAccountRequest(request) {
		return ""
	}
	wanted := strings.TrimSpace(request.Query.Get("target"))
	if wanted == "" {
		wanted = strings.TrimSpace(request.Query.Get("auth_index"))
	}
	if wanted == "" {
		wanted = strings.TrimSpace(request.Query.Get("auth_id"))
	}
	if wanted == "" {
		return ""
	}
	for _, entry := range codeartsAccounts(h) {
		if entry.Name == wanted || entry.ID == wanted || entry.AuthIndex == wanted {
			return strings.TrimSpace(entry.Name)
		}
	}
	return ""
}

// loginTargetNameFromMetadata is loginTargetName for the manager-driven login,
// which forwards its query string as metadata instead of as a page request.
func loginTargetNameFromMetadata(h *abiboot.Host, metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	query := url.Values{}
	for _, key := range []string{"target", "auth_index", "auth_id", "name", "file_name"} {
		value, _ := metadata[key].(string)
		if strings.TrimSpace(value) != "" {
			query.Set(key, strings.TrimSpace(value))
		}
	}
	if len(query) == 0 {
		return ""
	}
	return loginTargetName(h, pluginapi.ManagementRequest{Query: query})
}

// pollLoginPage polls a login session, persisting the credential on success.
func pollLoginPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	state := strings.TrimSpace(request.Query.Get("state"))
	response, errPoll := pollLoginForManagement(h, state, loginTargetName(h, request))
	if errPoll != nil {
		return plugui.HTML("CodeArts 登录",
			plugui.Card("检查失败", plugui.Notice("danger", errPoll.Error()),
				plugui.Action{Label: "重新登录", Query: "action=login", Kind: "primary"}))
	}

	switch response.Status {
	case pluginapi.AuthLoginStatusSuccess:
		message := response.Message
		if message == "" {
			message = "登录成功，账号已保存。"
		}
		return plugui.HTML("CodeArts 登录",
			plugui.Card("登录成功", plugui.Notice("success", message),
				plugui.Action{Label: "查看状态", Path: "status", Kind: "primary"}))
	case pluginapi.AuthLoginStatusPending, "":
		return plugui.HTML("CodeArts 登录",
			plugui.Card("等待授权",
				plugui.Notice("", "还没有收到授权回调。请先在浏览器里完成授权，然后再次检查。"),
				plugui.Action{Label: "再次检查", Query: "action=poll&state=" + state, Kind: "primary"},
				plugui.Action{Label: "重新开始", Query: "action=login"},
			))
	default:
		message := response.Message
		if message == "" {
			message = "登录失败。"
		}
		return plugui.HTML("CodeArts 登录",
			plugui.Card("登录失败", plugui.Notice("danger", message),
				plugui.Action{Label: "重新登录", Query: "action=login", Kind: "primary"}))
	}
}

// pollLoginForManagement reuses the auth.login.poll handler and then persists the
// credential itself.
//
// On the normal login path the host saves the credential the poll returns, but
// this page drives the poll directly, so saving is this function's job.
func pollLoginForManagement(h *abiboot.Host, state string, target string) (pluginapi.AuthLoginPollResponse, error) {
	var empty pluginapi.AuthLoginPollResponse
	if state == "" {
		return empty, abiboot.Errorf("missing_state", "缺少 state 参数，请重新发起登录")
	}
	poll := pluginapi.AuthLoginPollRequest{State: state}
	if strings.TrimSpace(target) != "" {
		poll.Metadata = map[string]any{"target": strings.TrimSpace(target)}
	}
	raw, errMarshal := json.Marshal(poll)
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
	if _, errSave := h.SaveAuth(name, response.Auth.StorageJSON); errSave != nil {
		return empty, abiboot.Errorf("save_auth", "保存凭据失败：%v", errSave)
	}
	forgetLoginSession(state)
	return response, nil
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

func creditPackageText(isCreditPackage bool) string {
	if isCreditPackage {
		return "额度包（可签到）"
	}
	return "Token 计费（无签到）"
}

func checkinText(activity *dailyActivity) string {
	if activity.Claimable {
		return "可领取"
	}
	if claimedStatuses[activity.Status] {
		return "今日已领取"
	}
	if activity.Status != "" {
		return "不可领取（" + activity.Status + "）"
	}
	return "不可领取"
}
