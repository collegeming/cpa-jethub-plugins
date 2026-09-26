package main

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file renders the pages a user sees inside CPA-Manager-Plus.
//
// The host exposes the single Menu route on
// `/v0/resource/plugins/<id>/<path>` and CPAMP renders it in a same-origin
// iframe with the host theme injected as CSS custom properties. Two consequences
// shape everything here:
//
//   - the host dispatches those routes as GET only, so every action is a link
//     carrying a query string rather than a form submission. There is no <form>
//     anywhere in this package, and a test asserts it;
//   - markup only consumes the host's CSS variables, so there is no frontend
//     build and both light and dark themes work for free.
//
// plugui renders cards, fields, notices and badges but has no table helper, and
// the deliverable of this plugin IS two aggregated tables — the channel overview
// and the previous run's results — so this file owns a small table renderer of
// its own. It goes through html/template like plugui does, which keeps every
// provider-supplied string (account labels, upstream messages, status text)
// escaped.

// hubTableStyle is the only extra CSS this plugin needs. It consumes the same
// host variables plugui consumes, with neutral fallbacks, so it follows the
// panel theme.
const hubTableStyle = `<style>
table.hub { width: 100%; border-collapse: collapse; font-size: 13px; }
table.hub th, table.hub td { text-align: left; padding: 6px 10px 6px 0; border-bottom: 1px solid var(--app-border, var(--border-color, #d8dee4)); vertical-align: top; }
table.hub th { color: var(--text-secondary, #59636e); font-weight: 600; white-space: nowrap; }
table.hub tr:last-child td { border-bottom: none; }
table.hub td.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; color: var(--text-secondary, #59636e); }
.channel { display: flex; align-items: center; gap: 8px; }
.channel img { width: 20px; height: 20px; border-radius: 4px; flex: none; }
.channel .channel-body { display: flex; flex-direction: column; }
.channel .channel-line { display: flex; align-items: center; gap: 6px; }
.channel .channel-name { font-weight: 600; white-space: nowrap; }
.channel .channel-id { color: var(--text-secondary, #59636e); font-size: 12px; }
</style>`

// tableTemplate renders a plain HTML table. Cells arrive as template.HTML
// because some of them embed a badge; every string that reaches a cell is
// escaped by the helper that builds it.
var tableTemplate = template.Must(template.New("hub-table").Parse(
	`<table class="hub"><thead><tr>{{range .Head}}<th>{{.}}</th>{{end}}</tr></thead><tbody>` +
		`{{range .Rows}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>{{end}}` +
		`</tbody></table>`))

// renderTable renders the aggregated tables.
func renderTable(head []string, rows [][]template.HTML) template.HTML {
	var buffer bytes.Buffer
	if errExecute := tableTemplate.Execute(&buffer, struct {
		Head []string
		Rows [][]template.HTML
	}{Head: head, Rows: rows}); errExecute != nil {
		return template.HTML(template.HTMLEscapeString(buffer.String()))
	}
	return template.HTML(buffer.String())
}

// cell renders an escaped text cell.
func cell(value string) template.HTML {
	return template.HTML(template.HTMLEscapeString(value))
}

// monoCell renders an escaped monospace cell, used for request URLs and
// credential indexes.
func monoCell(value string) template.HTML {
	return template.HTML(`<span class="mono">` + template.HTMLEscapeString(value) + `</span>`)
}

// renderStatusPage renders the ONE menu page. It is the channel overview: one
// row per provider, plus the one-click check-in and the previous run's table.
//
// The rows arrive as reports because reading them is the caller's job
// (`channelReports`), which is what keeps this function a pure renderer: it is
// the reason the page can be tested with scripted reports and no transport, and
// the reason a page load cannot accidentally claim anything.
func renderStatusPage(reports []channelReport, cfg Config, last *runResult) pluginapi.ManagementResponse {
	// The table style travels once per page: plugui owns the document shell and
	// has no table helper, so the only CSS this plugin adds is emitted here.
	body := []template.HTML{template.HTML(hubTableStyle)}

	intro := "本页汇总每个渠道的账号与状态。状态文字取自各 provider 自己的 " +
		"status?format=json 文档，只报告它确实返回的字段；读取状态是只读操作，" +
		"打开本页不会向任何 provider 发起签到。"
	if !cfg.Enabled {
		intro += " 当前配置 enabled=false，一键签到已关闭。"
	}
	body = append(body,
		plugui.Card("渠道总览", renderChannelTable(reports)),
		plugui.Card("一键签到",
			plugui.Notice(introTone(cfg), intro+
				"签到是写操作，只有显式点击下面的「一键签到」按钮（即 ?action=checkin）才会执行。"),
			plugui.Action{Label: "一键签到", Query: "action=checkin", Kind: "primary"},
			plugui.Action{Label: "JSON", Query: "format=json"},
		))

	if last != nil {
		body = append(body,
			plugui.Card(fmt.Sprintf("上次运行结果（%s，耗时 %s）",
				last.FinishedAt.Local().Format("2006-01-02 15:04:05"),
				last.duration().Truncate(time.Millisecond)),
				plugui.Group(runSummaryNotice(last), renderResultTable(last))))
	}
	return plugui.HTML(MenuLabel, body...)
}

// renderChannelTable renders the overview: one row per provider, in catalogue
// order, each with its mark, its account count, the provider's own one-line
// state and the link to its full page.
func renderChannelTable(reports []channelReport) template.HTML {
	rows := make([][]template.HTML, 0, len(reports))
	for _, report := range reports {
		rows = append(rows, []template.HTML{
			channelCell(report),
			channelAccountsCell(report),
			channelStatusCell(report),
			channelLinkCell(report),
		})
	}
	return renderTable([]string{"渠道", "账号", "状态", "页面"}, rows)
}

// channelLinkCell links to a channel's full page — but only when the host
// actually has that plugin loaded. A plugin that is installed but disabled is
// never mounted, so its resource route answers 404 and the link would be a
// dead end; the row already says 未安装或未启用, so showing no link is the
// honest rendering.
func channelLinkCell(report channelReport) template.HTML {
	if report.State == channelMissing {
		return template.HTML(`<span class="muted">—</span>`)
	}
	return template.HTML(`<a href="` + template.HTMLEscapeString(report.URL()) + `">打开</a>`)
}

// channelCell renders the mark and the name of one channel.
//
// The icon is a compile-time data URL from internal/jethub/brandicons, so
// writing it into the attribute needs no template.URL escape hatch — the value
// is a constant of this program, and escaping it keeps the attribute well formed
// even for the one icon that carries raw quotes in its SVG payload. The label
// beside it is provider data and stays escaped.
func channelCell(report channelReport) template.HTML {
	icon := `<img src="` + template.HTMLEscapeString(report.Target.Icon) + `" alt="" width="20" height="20">`
	name := template.HTML(`<span class="channel-name">` + template.HTMLEscapeString(report.Target.Label) + `</span>`)
	// The one thing the overview knows that a provider document does not say:
	// cline has no check-in endpoint upstream, so it never appears in a run.
	if !report.Target.supportsCheckin() {
		name = plugui.Group(name, plugui.Badge("", "不支持签到"))
	}
	// The plugin id sits on its own line: it is what names this channel in
	// `plugins.configs.<id>` and in the auth files, and long labels made it wrap
	// mid-word when it shared the line.
	body := `<span class="channel-line">` + string(name) + `</span>` +
		`<span class="channel-id mono">` + template.HTMLEscapeString(report.Target.ID) + `</span>`
	return template.HTML(`<span class="channel">` + icon + `<span class="channel-body">` + body + `</span></span>`)
}

// channelAccountsCell renders the account count — the host's credential ledger,
// with the provider's own number next to it whenever the two disagree — and,
// under it, one line per account the provider itself reported.
//
// The per-account lines are the fix for a channel whose several accounts showed
// a single balance: the count alone never said whose numbers those were. A
// provider that reports no figures for an account gets no line for it, and a
// provider whose document carries no per-account list at all is unchanged.
func channelAccountsCell(report channelReport) template.HTML {
	if report.State == channelMissing {
		return cell("—")
	}
	text := fmt.Sprintf("%d", report.Accounts)
	switch {
	case report.Reported < 0:
		// The provider's document carries no count at all; showing one would be
		// an invention, so the ledger stands alone.
	case report.Reported != report.Accounts:
		text += fmt.Sprintf("（provider 报告 %d）", report.Reported)
	}
	lines := []template.HTML{cell(text)}
	rendered := 0
	for position, account := range report.AccountDetail {
		if len(account.Figures) == 0 {
			continue
		}
		if rendered == accountLinesPerChannel {
			lines = append(lines, cell(fmt.Sprintf("…另有 %d 个账号", len(report.AccountDetail)-position)))
			break
		}
		lines = append(lines, cell(account.display(position)+"："+strings.Join(account.Figures, " · ")))
		rendered++
	}
	if len(lines) == 1 {
		return lines[0]
	}
	return plugui.Group(lines[0], template.HTML("<br>"),
		template.HTML(`<span class="muted">`+joinCells(lines[1:])+`</span>`))
}

// joinCells concatenates already-escaped cells with line breaks.
func joinCells(cells []template.HTML) string {
	parts := make([]string, 0, len(cells))
	for _, item := range cells {
		parts = append(parts, string(item))
	}
	return strings.Join(parts, "<br>")
}

// channelStatusCell renders the provider's own one-line state, or the reason
// there is none. Both failure shapes stay inside the row: a channel that is not
// installed is a normal row in this panel, never an error page.
func channelStatusCell(report channelReport) template.HTML {
	switch report.State {
	case channelMissing:
		return plugui.Badge("warning", report.Status)
	case channelUnreadable:
		return plugui.Group(plugui.Badge("danger", report.Status), template.HTML("<br>"), cell(report.Error))
	default:
		return cell(report.Status)
	}
}

// renderResultPage renders one completed run.
func renderResultPage(result *runResult) pluginapi.ManagementResponse {
	return plugui.HTML(MenuLabel,
		template.HTML(hubTableStyle),
		plugui.Card("一键签到结果",
			plugui.Group(runSummaryNotice(result), renderResultTable(result)),
			plugui.Action{Label: "返回", Path: "status", Kind: "primary"},
			plugui.Action{Label: "再次执行", Query: "action=checkin"},
			plugui.Action{Label: "JSON", Query: "action=checkin&format=json"},
		),
		plugui.Card("说明",
			plugui.Notice("", "结果按各 provider 响应体里的字段判定："+
				"status/outcome 为 claimed 记为已领取，already-claimed 记为今日已签到；"+
				"CodeArts 的签到只能在状态页触发，其判定来自签到前后的 ?format=json 状态。"+
				"provider 自己返回的原文写在消息里，未返回的字段不会被补全。"),
		),
	)
}

// renderDisabledPage explains that claiming is switched off.
func renderDisabledPage() pluginapi.ManagementResponse {
	return plugui.HTML(MenuLabel,
		plugui.Card("一键签到已关闭",
			plugui.Notice("warning", "插件当前为 disabled（plugins.configs.hub.enabled=false），"+
				"本次没有向任何 provider 发起签到请求。启用该插件后重试即可。"),
			plugui.Action{Label: "返回", Path: "status", Kind: "primary"},
		),
	)
}

// renderErrorPage renders a single error card.
func renderErrorPage(heading, message string) pluginapi.ManagementResponse {
	return plugui.HTML(MenuLabel,
		plugui.Card(heading,
			plugui.Notice("danger", message),
			plugui.Action{Label: "返回", Path: "status", Kind: "primary"},
		),
	)
}

// renderConfirmPage is what a bare visit to the script-facing /checkin route
// renders. It is deliberately NOT a claim: the run needs an explicit
// `action=checkin`, exactly like the status route.
func renderConfirmPage() pluginapi.ManagementResponse {
	return plugui.HTML(MenuLabel,
		plugui.Card("尚未执行",
			plugui.Notice("warning", "本路由只有带上 action=checkin 才会真正执行签到（写操作）。"+
				"不带参数时它只返回这份确认信息，不会向任何 provider 发起请求。"),
			plugui.Action{Label: "立即签到", Query: "action=checkin", Kind: "primary"},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	)
}

// renderResultTable renders the aggregated table: provider, account, result.
func renderResultTable(result *runResult) template.HTML {
	rows := make([][]template.HTML, 0)
	for _, item := range result.rows() {
		account := cell("—")
		if strings.TrimSpace(item.Account) != "" {
			account = cell(item.Account)
		}
		if item.AuthIndex != "" {
			account = plugui.Group(cell(item.Account), template.HTML("<br>"), monoCell(item.AuthIndex))
		}
		outcome := plugui.Group(
			plugui.Badge(verdictTone(item.Result), item.ResultText),
		)
		if item.Message != "" {
			outcome = plugui.Group(outcome, template.HTML("<br>"), cell(item.Message))
		}
		if item.ProviderStatus != "" {
			outcome = plugui.Group(outcome, template.HTML("<br>"),
				monoCell("provider 原文："+item.ProviderStatus))
		}
		rows = append(rows, []template.HTML{
			cell(fmt.Sprintf("%s（%s）", item.ProviderLabel, item.Provider)),
			account,
			outcome,
		})
	}
	if len(rows) == 0 {
		rows = append(rows, []template.HTML{cell("—"), cell("—"), cell("本次没有任何目标")})
	}
	return renderTable([]string{"提供方", "账号", "结果"}, rows)
}

// runSummaryNotice summarises one run without inventing anything: every count
// below is a count of rows the run actually produced.
func runSummaryNotice(result *runResult) template.HTML {
	summary := result.summary()
	text := fmt.Sprintf("共 %d 条结果：已领取 %d、今日已签到 %d、不可领取 %d、不支持 %d、未启用或无账号 %d、失败 %d、无法判定 %d。耗时 %s。",
		summary["total"],
		summary[string(kindClaimed)],
		summary[string(kindAlreadyClaimed)],
		summary[string(kindInactive)],
		summary[string(kindUnsupported)],
		summary[string(kindUnavailable)],
		summary[string(kindFailed)],
		summary[string(kindUnknown)],
		result.duration().Truncate(time.Millisecond),
	)
	tone := ""
	switch {
	case summary[string(kindFailed)] > 0:
		tone = "danger"
	case summary[string(kindClaimed)] > 0:
		tone = "success"
	case summary[string(kindAlreadyClaimed)] > 0:
		tone = ""
	default:
		tone = "warning"
	}
	return plugui.Notice(tone, text)
}

// introTone warns when the run would be refused.
func introTone(cfg Config) string {
	if cfg.Enabled {
		return ""
	}
	return "warning"
}
