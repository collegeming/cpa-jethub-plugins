package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Management API and CPAMP resource routes.
//
// Two mounts with different rules, both determined by the host
// (`internal/pluginhost/management.go`, README "挂载规则"):
//
//   - a GET route carrying a Menu is registered ONLY under
//     `/v0/resource/plugins/<id>/<path>` AND becomes its own sidebar entry in
//     CPA-Manager-Plus. The manager renders one nav item per menu route and does
//     not group them by plugin, so this plugin declares exactly ONE — the
//     channel overview — and every provider plugin in this repository declares
//     none, which is what turns ten sidebar entries into one.
//   - a ResourceRoute is mounted under the same prefix but is listed in the
//     sidebar only when it carries a Menu. Leaving Menu empty keeps the page
//     browser-reachable — the overview links to it — without adding a nav item.
//   - any other route would land in the GLOBAL `/v0/management/<path>`
//     namespace. This plugin declares none: it owns no credentials and every
//     machine caller can use `?format=json` on the resource routes instead,
//     which needs no management key.
//
// The resource mount is dispatched as GET ONLY, which is why the one write this
// plugin performs is a query string (`?action=checkin`) and never a form.

// handleManagementRegister declares the entries management clients show.
func handleManagementRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			// The single Menu route. Its label is what the sidebar shows, so it
			// names the hub rather than the plugin id or the check-in action:
			// the same page is the channel overview and the one-click entry.
			{Method: http.MethodGet, Path: "/status", Menu: MenuLabel,
				Description: "跨 provider 渠道总览与一键签到：状态、账号数与上次结果"},
		},
		Resources: []pluginapi.ResourceRoute{
			// Script-facing mirror of `?action=checkin`, for callers that want
			// JSON without a status page in the way. Empty Menu: no nav item.
			{Path: "/checkin", Description: "执行一键签到（GET /checkin?action=checkin，写操作；?format=json 返回聚合结果）"},
		},
	}, nil
}

// handleManagementHandle dispatches the routes declared above.
//
// The request path is the FULL incoming path and differs per mount, so only its
// last segment identifies the route.
func handleManagementHandle(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ManagementRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}

	switch managementRoute(request.Path) {
	case "/status":
		return statusResponse(h, request), nil
	case "/checkin":
		return checkinRouteResponse(h, request), nil
	}

	return jsonManagementResponse(http.StatusNotFound, map[string]any{
		"error": "unknown hub management route",
		"path":  request.Path,
		"hint":  "GET /status（目标与上次结果）；GET /status?action=checkin 或 GET /checkin 执行签到",
	}), nil
}

// managementRoute reduces the request path to the route the plugin registered.
func managementRoute(path string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return "/" + trimmed
}

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

// actionOf returns the normalised `action` query parameter.
func actionOf(request pluginapi.ManagementRequest) string {
	return strings.ToLower(strings.TrimSpace(request.Query.Get("action")))
}

// statusResponse serves the single menu route: the channel overview, or the one
// click that claims.
//
// The write is gated on an EXPLICIT `action=checkin`. Everything else — a plain
// page load, `?format=json`, a refresh — renders the overview and performs no
// write at all: the only requests it makes are each provider's own read-only
// `status?format=json` (see channelReports). That is what makes the page safe to
// embed in an iframe.
func statusResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch actionOf(request) {
	case "checkin", "claim":
		return checkinResponse(h, request)
	case "":
		// fall through to the read-only overview
	default:
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{
				"error":  "未知的 action",
				"action": actionOf(request),
				"hint":   "只有 action=checkin 会执行签到；不带 action 时本路由只读",
			})
		}
		return renderErrorPage("未知的 action",
			"只有 action=checkin 会执行签到；不带 action 时本路由只读。")
	}

	cfg := settings()
	reports := channelReports(h, cfg)
	if wantsJSON(request) {
		return jsonManagementResponse(http.StatusOK, statusDocument(cfg, reports))
	}
	return renderStatusPage(reports, cfg, lastRun())
}

// checkinRouteResponse serves the script-facing resource route.
//
// It obeys the same opt-in rule as the status route: a bare GET is NOT a write.
// Without `action=checkin` it answers with a confirmation hint instead of
// claiming, so a stray link or a prefetcher cannot check anybody in.
func checkinRouteResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch actionOf(request) {
	case "checkin", "claim":
		return checkinResponse(h, request)
	}
	if wantsJSON(request) {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"action":   "confirm",
			"claiming": false,
			"hint":     "追加 &action=checkin 才会真正执行签到（写操作）；不带 action 时本路由只读",
			// No channel reports here on purpose: this route only has to answer
			// "did I claim?" so an accidental link stays cheap. The overview
			// lives on /status.
			"status": statusDocument(settings(), nil),
		})
	}
	return renderConfirmPage()
}

// checkinResponse performs the one-click run and renders its result.
func checkinResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	if !cfg.Enabled {
		body := map[string]any{
			"enabled": false,
			"error":   "插件配置里 enabled=false，已拒绝执行签到（写操作被关闭）",
		}
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusOK, body)
		}
		return renderDisabledPage()
	}

	result := newRunner(h, cfg).run()
	rememberLastRun(result)
	if wantsJSON(request) {
		return jsonManagementResponse(http.StatusOK, runDocument(result))
	}
	return renderResultPage(result)
}

// statusDocument is the machine-readable form of the overview page.
//
// Each entry reports only observed facts: which state the probe reached, the
// account counts, the provider's own one-line status, and the last run's numbers
// when there was one. `reports` is nil when the caller did not probe (the
// /checkin confirmation), in which case the document simply carries no channel
// list instead of inventing an empty one.
//
// The icons the page shows are deliberately absent: they are data URLs of tens
// of kilobytes each, they exist to be painted, and a machine caller that wants
// one can read it from the plugin registration or the page itself.
func statusDocument(cfg Config, reports []channelReport) map[string]any {
	last := lastRun()
	counts := map[string]int{}
	ranAt := ""
	if last != nil {
		counts = last.accountCounts()
		ranAt = jsonTime(last.FinishedAt)
	}

	channels := make([]map[string]any, 0, len(reports))
	for _, report := range reports {
		document := map[string]any{
			"provider":  report.Target.ID,
			"label":     report.Target.Label,
			"supported": report.Target.supportsCheckin(),
			"request":   report.Target.checkinDescription(),
			"state":     string(report.State),
			"installed": report.State != channelMissing,
			"accounts":  report.Accounts,
			"status":    report.Status,
			"url":       report.URL(),
		}
		if report.Reported >= 0 {
			document["reported_accounts"] = report.Reported
		} else {
			document["reported_accounts"] = nil
		}
		if report.Error != "" {
			document["error"] = report.Error
		}
		if report.Target.Note != "" {
			document["note"] = report.Target.Note
		}
		// account_details is the provider's own per-account list: identity plus
		// the figures that provider published for that account. `accounts` above
		// stays the host ledger's COUNT (scripts read it as a number), so the two
		// are separate keys on purpose. A provider whose document carries no
		// per-account list reports an empty list rather than a fabricated row.
		details := make([]map[string]any, 0, len(report.AccountDetail))
		for _, account := range report.AccountDetail {
			entry := map[string]any{
				"auth_index": account.AuthIndex,
				"name":       account.Name,
				"figures":    account.Figures,
				"status":     strings.Join(account.Figures, " · "),
			}
			if account.Label != "" {
				entry["label"] = account.Label
			}
			details = append(details, entry)
		}
		document["account_details"] = details
		if count, ok := counts[report.Target.ID]; ok {
			document["last_account_count"] = count
			document["last_run_at"] = ranAt
		} else {
			document["last_account_count"] = nil
		}
		channels = append(channels, document)
	}

	providers := selectedProviders(cfg)
	if providers == nil {
		providers = []string{}
	}
	body := map[string]any{
		"plugin":        ProviderKey,
		"route":         "/status",
		"action":        "",
		"claiming":      false,
		"enabled":       cfg.Enabled,
		"host_base_url": cfg.HostBaseURL,
		"timeout_ms":    cfg.TimeoutMS,
		"providers":     providers,
		"checkin_url":   "?action=checkin",
		"last_run":      nil,
	}
	if reports != nil {
		body["channels"] = channels
		// `targets` is the same list under the name the first release used, so a
		// script written against it keeps working. One slice, one provider list.
		body["targets"] = channels
	}
	if last != nil {
		body["last_run"] = runDocument(last)
	}
	return body
}

// runDocument is the machine-readable aggregated result of one run.
func runDocument(result *runResult) map[string]any {
	rows := make([]map[string]any, 0)
	for _, item := range result.rows() {
		rows = append(rows, map[string]any{
			"provider":        item.Provider,
			"provider_label":  item.ProviderLabel,
			"auth_index":      item.AuthIndex,
			"account":         item.Account,
			"result":          string(item.Result),
			"result_text":     item.ResultText,
			"provider_status": item.ProviderStatus,
			"message":         item.Message,
			"request":         item.Request,
			"http_status":     item.HTTPStatus,
		})
	}
	providers := make([]map[string]any, 0, len(result.Providers))
	for _, entry := range result.Providers {
		providers = append(providers, map[string]any{
			"provider":          entry.Target.ID,
			"label":             entry.Target.Label,
			"supported":         entry.Target.supportsCheckin(),
			"accounts":          entry.Accounts,
			"reported_accounts": entry.Reported,
			"error":             entry.Error,
		})
	}
	return map[string]any{
		"action":      "checkin",
		"claiming":    true,
		"base_url":    result.BaseURL,
		"started_at":  jsonTime(result.StartedAt),
		"finished_at": jsonTime(result.FinishedAt),
		"duration_ms": result.duration().Milliseconds(),
		"summary":     result.summary(),
		"rows":        rows,
		"providers":   providers,
	}
}
