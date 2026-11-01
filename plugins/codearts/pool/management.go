package pool

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ManagementRegister declares the ONE sidebar ops page (/pool) plus the scan
// endpoint behind it. The registrar's controls live on the same page; /reg is
// kept as a Menu-less resource by the parent plugin.
func ManagementRegister() []pluginapi.ManagementRoute {
	return []pluginapi.ManagementRoute{
		{Method: http.MethodGet, Path: "/pool", Menu: DisplayName,
			Description: "CodeArts 运维：账号池巡检与自动注册"},
		{Method: http.MethodPost, Path: "/" + ProviderKey + "/scan", Description: "立即巡检账号池并执行停用/恢复（返回 JSON）"},
	}
}

// ManagementHandle dispatches the routes declared above. Resource routes
// arrive as GET only, so every action lives in the query string.
func ManagementHandle(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ManagementRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}

	switch managementRoute(request.Path) {
	case "/pool":
		return poolPage(h, request), nil
	case "/scan":
		outcome := runScan(h, settings(), "api")
		return jsonManagementResponse(http.StatusOK, outcome), nil
	}
	return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "unknown route " + request.Path}), nil
}

// managementRoute normalises a management path to the form the switch uses:
// the host may deliver the full mount (`/codeartspool/pool`) or the relative
// form (`/pool`).
func managementRoute(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "/"
	}
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return "/" + trimmed
}

// jsonManagementResponse wraps a document as a JSON management response.
func jsonManagementResponse(status int, document any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(document)
	if errMarshal != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusInternalServerError, Body: []byte(`{"error":"encode"}`)}
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       raw,
	}
}
