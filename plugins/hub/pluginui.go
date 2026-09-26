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
// the deliverable of this plugin IS one aggregated table, so this file owns a
// small table renderer of its own. It goes through html/template like plugui
// does, which keeps every provider-supplied string (account labels, upstream
// messages) escaped.

// hubTableStyle is the only extra CSS this plugin needs. It consumes the same
// host variables plugui consumes, with neutral fallbacks, so it follows the
// panel theme.
const hubTableStyle = `<style>
table.hub { width: 100%; border-collapse: collapse; font-size: 13px; }
table.hub th, table.hub td { text-align: left; padding: 6px 10px 6px 0; border-bottom: 1px solid var(--app-border, var(--border-color, #d8dee4)); vertical-align: top; }
table.hub th { color: var(--text-secondary, #59636e); font-weight: 600; white-space: nowrap; }
table.hub tr:last-child td { border-bottom: none; }
table.hub td.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; color: var(--text-secondary, #59636e); }
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

// renderStatusPage renders the ONE menu page: the configured targets, their last
// known account counts, and the previous run's table when there is one.
//
// Nothing here performs a request. The page is a report of what the last
// `?action=checkin` run observed; that is why the counters say "上次" instead of
// claiming a live value.
func renderStatusPage(cfg Config, last *runResult) pluginapi.ManagementResponse {
	// The table style travels once per page: plugui owns the document shell and
	// has no table helper, so the only CSS this plugin adds is emitted here.
	body := []template.HTML{template.HTML(hubTableStyle)}

	intro := "本页汇总所有 provider 的每日签到入口。签到是写操作，" +
		"只有显式点击下面的「一键签到」按钮（即 ?action=checkin）才会执行；" +
		"打开本页不会向任何 provider 发起签到请求。"
	if !cfg.Enabled {
		intro += " 当前配置 enabled=false，一键签到已关闭。"
	}
	body = append(body, plugui.Card("一键签到",
		plugui.Notice(introTone(cfg), intro),
		plugui.Action{Label: "一键签到", Query: "action=checkin", Kind: "primary"},
		plugui.Action{Label: "JSON", Query: "format=json"},
	))

	body = append(body, renderTargetsCard(cfg, last))

	if last != nil {
		body = append(body,
			plugui.Card(fmt.Sprintf("上次运行结果（%s，耗时 %s）",
				last.FinishedAt.Local().Format("2006-01-02 15:04:05"),
				last.duration().Truncate(time.Millisecond)),
				plugui.Group(runSummaryNotice(last), renderResultTable(last))))
	}
	return plugui.HTML(MenuLabel, body...)
}

// renderTargetsCard renders the target table: what will be driven, how many
// accounts each provider had last time, and which request the driver will send.
func renderTargetsCard(cfg Config, last *runResult) template.HTML {
	counts := map[string]int{}
	ranAt := ""
	if last != nil {
		counts = last.accountCounts()
		ranAt = last.FinishedAt.Local().Format("01-02 15:04")
	}
	notes := map[string]string{}
	if last != nil {
		for _, entry := range last.Providers {
			if entry.Error != "" {
				notes[entry.Target.ID] = entry.Error
			}
		}
	}

	rows := make([][]template.HTML, 0, len(selectTargets(cfg)))
	for _, entry := range selectTargets(cfg) {
		support := plugui.Badge("success", "支持")
		if !entry.supportsCheckin() {
			support = plugui.Badge("", "不支持")
		}
		count := cell("—（尚未运行）")
		if value, ok := counts[entry.ID]; ok {
			count = cell(fmt.Sprintf("%d", value))
		}
		ran := cell("—")
		if ranAt != "" {
			ran = cell(ranAt)
		}
		request := monoCell(entry.checkinDescription())
		if entry.Note != "" {
			request = plugui.Group(request, template.HTML("<br>"),
				cell(entry.Note))
		}
		if note, ok := notes[entry.ID]; ok {
			request = plugui.Group(request, template.HTML("<br>"),
				plugui.Notice("warning", note))
		}
		rows = append(rows, []template.HTML{
			cell(fmt.Sprintf("%s（%s）", entry.Label, entry.ID)),
			support,
			count,
			ran,
			request,
		})
	}
	return plugui.Card("签到目标",
		renderTable([]string{"提供方", "签到能力", "上次账号数", "上次运行", "实际请求 / 备注"}, rows))
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
