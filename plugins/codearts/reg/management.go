package reg

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ManagementRegister declares the registrar's JSON POST endpoint. The HTML
// page itself is registered by the parent as a Menu-less resource (/reg) so
// the sidebar only shows the combined ops entry on /pool.
func ManagementRegister() []pluginapi.ManagementRoute {
	return []pluginapi.ManagementRoute{
		{Method: http.MethodPost, Path: "/" + ProviderKey + "/register", Description: "发起一次自动注册任务（返回 JSON）"},
	}
}

// ManagementHandle dispatches the routes declared above.
func ManagementHandle(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.ManagementRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}

	switch managementRoute(request.Path) {
	case "/reg":
		return regPage(h, request), nil
	case "/register":
		if errStart := job.start(); errStart != nil {
			return jsonManagementResponse(http.StatusConflict, map[string]any{"error": errStart.Error()}), nil
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"started": true}), nil
	}
	return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "unknown route " + request.Path}), nil
}

// managementRoute normalises a management path to the form the switch uses.
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
