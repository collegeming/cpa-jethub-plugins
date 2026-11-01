package pool

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ExtraPageCards, when set by the parent plugin, appends more cards to the
// ops page (the registrar section). RefreshSeconds, when > 0, makes the page
// auto-refresh so a running registration job stays visible from /pool.
var (
	ExtraPageCards func(h *abiboot.Host) []template.HTML
	RefreshSeconds func() int
)

// PageCards renders the pool section cards (config, last scan, account table)
// without wrapping them in a full HTML document. The parent combines these
// with the registrar cards on the single ops page.
func PageCards(h *abiboot.Host) []template.HTML {
	return poolCards(h, settings(), lastScanOutcome())
}

// poolPage serves the resource route: HTML by default, JSON on ?format=json,
// and the action query strings for the operator buttons.
func poolPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()

	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "scan":
		outcome := runScan(h, cfg, "manual")
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusOK, outcome)
		}
		return renderPoolPage(h, cfg, &outcome)
	case "enable", "disable":
		return accountAction(h, cfg, request)
	}
	if wantsJSON(request) {
		return poolJSON(h, cfg)
	}
	return renderPoolPage(h, cfg, lastScanOutcome())
}

// accountAction applies the page's per-account buttons. A manual disable is
// marked `codeartspool:manual` so the auto-enable sweep never resurrects it; a
// manual enable clears whatever marker is there, including a manual one.
func accountAction(h *abiboot.Host, cfg Config, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	name := strings.TrimSpace(request.Query.Get("name"))
	action := strings.ToLower(strings.TrimSpace(request.Query.Get("action")))
	if name == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "name is required"})
	}
	entry, found := findAuthEntry(h, name)
	if !found {
		return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "凭据不存在：" + name})
	}
	var err error
	if action == "enable" {
		err = enableAccount(h, entry)
	} else {
		err = writeDisabled(h, entry.Name, true, disabledByManual)
	}
	if err != nil {
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return plugui.HTML(DisplayName, plugui.Card("操作失败", plugui.Notice("danger", err.Error()),
			plugui.Action{Label: "返回", Path: "pool"}))
	}
	h.Log("info", fmt.Sprintf("CodeArts 账号 %s 由页面操作%s", name, map[string]string{"enable": "启用", "disable": "停用"}[action]),
		map[string]any{"auth": name, "action": action})
	if wantsJSON(request) {
		return jsonManagementResponse(http.StatusOK, map[string]any{"ok": true, "name": name, "action": action})
	}
	return renderPoolPage(h, cfg, lastScanOutcome())
}

// findAuthEntry resolves a credential file name to its host entry.
func findAuthEntry(h *abiboot.Host, name string) (pluginapi.HostAuthFileEntry, bool) {
	entries, errList := h.ListAuth()
	if errList != nil {
		return pluginapi.HostAuthFileEntry{}, false
	}
	for _, entry := range entries {
		if entry.Name == name && (entry.Type == "codearts" || entry.Provider == "codearts") {
			return entry, true
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

// poolJSON is the machine-readable pool document: the last scan plus the
// live account list and the scanner's effective configuration.
func poolJSON(h *abiboot.Host, cfg Config) pluginapi.ManagementResponse {
	outcome := lastScanOutcome()
	accounts, errList := liveAccountRows(h, outcome)
	body := map[string]any{
		"config": map[string]any{
			"enabled":          cfg.Enabled,
			"scan_interval_ms": cfg.ScanIntervalMS,
			"min_remaining":    cfg.MinRemaining,
			"auto_disable":     cfg.AutoDisable,
			"auto_enable":      cfg.AutoEnable,
			"auto_checkin":     cfg.AutoCheckin,
		},
		"accounts":  accounts,
		"last_scan": outcome,
	}
	if errList != nil {
		body["accounts_error"] = errList.Error()
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// liveAccountRows builds one row per codearts credential the host reports
// right now, then overlays whatever the remembered sweep measured for each
// account. A page load never calls upstream: rows carry name/label/status/
// disabled from the auth file and leave the measured columns empty, so the
// full roster is always visible — including accounts added since the last
// sweep and the whole roster after a restart wiped the remembered outcome.
func liveAccountRows(h *abiboot.Host, outcome *scanOutcome) ([]accountReport, error) {
	entries, err := h.ListAuth()
	if err != nil {
		return nil, err
	}
	scanned := map[string]accountReport{}
	if outcome != nil {
		for _, account := range outcome.Accounts {
			scanned[account.Name] = account
		}
	}
	var rows []accountReport
	for _, entry := range entries {
		if entry.Type != "codearts" && entry.Provider != "codearts" {
			continue
		}
		row := accountReport{
			Name:      entry.Name,
			AuthIndex: entry.AuthIndex,
			Label:     firstNonEmpty(entry.Label, entry.Email, entry.Account),
			Status:    statusOf(entry),
			Disabled:  entry.Disabled,
		}
		if prev, found := scanned[entry.Name]; found {
			row.Remaining, row.Used, row.Total = prev.Remaining, prev.Used, prev.Total
			row.Checkin, row.CheckinResult = prev.Checkin, prev.CheckinResult
			row.Action, row.Detail = prev.Action, prev.Detail
			if row.Label == "" {
				row.Label = prev.Label
			}
		} else if outcome != nil {
			row.Detail = "未在上次巡检中"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// renderPoolPage renders the combined ops page: pool cards plus any ExtraPageCards
// the parent wired in (registrar). A running registration job triggers refresh.
func renderPoolPage(h *abiboot.Host, cfg Config, outcome *scanOutcome) pluginapi.ManagementResponse {
	fragments := poolCards(h, cfg, outcome)
	if ExtraPageCards != nil {
		fragments = append(fragments, ExtraPageCards(h)...)
	}
	if RefreshSeconds != nil {
		if sec := RefreshSeconds(); sec > 0 {
			return plugui.HTMLWithRefresh(sec, DisplayName, fragments...)
		}
	}
	return plugui.HTML(DisplayName, fragments...)
}

// poolCards builds the pool section: configuration, last sweep result, and the
// live account table. A plain page load is read-only and never triggers a sweep.
func poolCards(h *abiboot.Host, cfg Config, outcome *scanOutcome) []template.HTML {
	writeGuard := cfg.Enabled && (cfg.AutoDisable || cfg.AutoEnable)
	guardText := "开启"
	if !cfg.Enabled {
		guardText = "enabled=false：巡检只读不写"
	} else if !writeGuard {
		guardText = "auto_disable/auto_enable 均已关闭：巡检只读不写"
	}
	interval := time.Duration(cfg.ScanIntervalMS) * time.Millisecond
	intervalText := "已关闭（scan_interval_ms = 0）"
	if cfg.ScanIntervalMS > 0 {
		intervalText = "每 " + interval.String()
	}

	configCard := plugui.Card("巡检配置", plugui.Group(
		plugui.Fields(
			plugui.Field{Label: "写开关", Value: guardText},
			plugui.Field{Label: "自动巡检", Value: intervalText},
			plugui.Field{Label: "余额下限", Value: fmt.Sprintf("remaining ≤ %g 判定耗尽", cfg.MinRemaining)},
			plugui.Field{Label: "自动签到", Value: map[bool]string{true: "开启（领取到积分可同轮复活停用账号）", false: "关闭"}[cfg.AutoCheckin]},
		),
	), plugui.Action{Label: "立即巡检", Path: "pool", Query: "action=scan", Kind: "primary"})

	var fragments []template.HTML
	fragments = append(fragments, configCard)

	if outcome == nil {
		fragments = append(fragments, plugui.Card("巡检结果", plugui.Notice("info", "尚未巡检。点「立即巡检」跑第一轮，或等自动巡检到点。")))
	} else {
		summaryTone := "success"
		summary := fmt.Sprintf("%s 完成：停用 %d 个，恢复 %d 个，共 %d 个账号",
			outcome.StartedAt.Local().Format("15:04:05"), outcome.DisabledNow, outcome.EnabledNow, len(outcome.Accounts))
		if outcome.Error != "" {
			summaryTone = "danger"
			summary = outcome.Error
		} else if outcome.DisabledNow > 0 {
			summaryTone = "warning"
		}
		fragments = append(fragments, plugui.Card("巡检结果", plugui.Group(
			plugui.Notice(summaryTone, summary),
			plugui.Fields(plugui.Field{Label: "触发方式", Value: outcome.Reason},
				plugui.Field{Label: "耗时", Value: fmt.Sprintf("%d ms", outcome.DurationMS)}),
		)))
	}

	accounts, errList := liveAccountRows(h, outcome)
	switch {
	case errList != nil:
		fragments = append(fragments, plugui.Card("账号", plugui.Notice("danger", "读取凭据列表失败："+errList.Error())))
		return fragments
	case len(accounts) == 0:
		fragments = append(fragments, plugui.Card("账号", plugui.Notice("info", "没有 codearts 类型的凭据。")))
		return fragments
	}
	var rows strings.Builder
	rows.WriteString(`<table class="data"><thead><tr>` +
		`<th>账号</th><th>额度（剩余/已用/总额）</th><th>签到</th><th>状态</th><th>本次动作</th><th>操作</th>` +
		`</tr></thead><tbody>`)
	for _, account := range accounts {
		name := template.HTMLEscapeString(account.Name)
		if account.Label != "" {
			name += `<br><small>` + template.HTMLEscapeString(account.Label) + `</small>`
		}
		actionLabel := "停用"
		actionQuery := "action=disable&name=" + template.URLQueryEscaper(account.Name)
		if account.Disabled {
			actionLabel = "启用"
			actionQuery = "action=enable&name=" + template.URLQueryEscaper(account.Name)
		}
		detail := account.Action
		if detail == "" {
			detail = "—"
		}
		if account.Detail != "" {
			detail += " · " + account.Detail
		}
		checkin := account.Checkin
		if account.CheckinResult != "" {
			if checkin != "" {
				checkin = account.CheckinResult + "（" + checkin + "）"
			} else {
				checkin = account.CheckinResult
			}
		}
		if checkin == "" {
			checkin = "—"
		}
		rows.WriteString(fmt.Sprintf(
			"<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td><a class=\"btn\" href=\"pool?%s\">%s</a></td></tr>",
			name, template.HTMLEscapeString(quotaText(account)), template.HTMLEscapeString(checkin),
			template.HTMLEscapeString(account.Status), template.HTMLEscapeString(detail), actionQuery, actionLabel))
	}
	rows.WriteString(`</tbody></table>`)
	fragments = append(fragments, plugui.Card("账号", template.HTML(rows.String())))
	return fragments
}

// quotaText renders the account's credit triple. A figure the provider did not
// publish shows as 未知 rather than 0 — zero is a statement, unknown is not.
func quotaText(account accountReport) string {
	part := func(value *float64) string {
		if value == nil {
			return "未知"
		}
		return fmt.Sprintf("%.2f", *value)
	}
	return part(account.Remaining) + " / " + part(account.Used) + " / " + part(account.Total)
}

// wantsJSON reports whether the requester asked for the machine-readable form.
func wantsJSON(request pluginapi.ManagementRequest) bool {
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("format")), "json") {
		return true
	}
	return strings.Contains(strings.ToLower(request.Headers.Get("Accept")), "application/json")
}
