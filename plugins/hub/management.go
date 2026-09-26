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
//     not group them by plugin, so every extra menu route looks like a duplicate
//     entry. Exactly one route below carries a Menu.
//   - a ResourceRoute is mounted under the same prefix but is listed in the
//     sidebar only when it carries a Menu. Leaving Menu empty keeps the page
//     browser-reachable — the status page links to it — without adding a nav
//     item.
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
			// names the feature rather than the plugin id.
			{Method: http.MethodGet, Path: "/status", Menu: MenuLabel,
				Description: "跨 provider 一键签到：目标、上次账号数与结果"},
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

// statusResponse serves the single menu route.
//
// The write is gated on an EXPLICIT `action=checkin`. Everything else — a plain
// page load, `?format=json`, a refresh — renders the targets and the last run's
// numbers and performs no request to any provider at all. That is what makes the
// page safe to embed in an iframe: opening it can never claim anything.
func statusResponse(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch actionOf(request) {
	case "checkin", "claim":
		return checkinResponse(h, request)
	case "":
		// fall through to the read-only report
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
	if wantsJSON(request) {
		return jsonManagementResponse(http.StatusOK, statusDocument(cfg))
	}
	return renderStatusPage(cfg, lastRun())
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
			"status":   statusDocument(settings()),
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

// statusDocument is the machine-readable form of the status page. It reports
// only observed facts: the configured targets, what each one offers, and the
// numbers of the previous run (null when there was none).
func statusDocument(cfg Config) map[string]any {
	targets := make([]map[string]any, 0, len(selectTargets(cfg)))
	last := lastRun()
	counts := map[string]int{}
	ranAt := ""
	if last != nil {
		counts = last.accountCounts()
		ranAt = jsonTime(last.FinishedAt)
	}
	for _, entry := range selectTargets(cfg) {
		document := map[string]any{
			"provider":  entry.ID,
			"label":     entry.Label,
			"supported": entry.supportsCheckin(),
			"request":   entry.checkinDescription(),
		}
		if entry.Note != "" {
			document["note"] = entry.Note
		}
		if count, ok := counts[entry.ID]; ok {
			document["last_account_count"] = count
			document["last_run_at"] = ranAt
		} else {
			document["last_account_count"] = nil
		}
		targets = append(targets, document)
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
		"targets":       targets,
		"last_run":      nil,
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
