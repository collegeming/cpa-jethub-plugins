package reg

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ExtraPageCards, when set by the parent plugin, prepends more cards to this
// page (the pool section) so /reg shows the same combined ops surface as /pool.
var ExtraPageCards func(h *abiboot.Host) []template.HTML

// PageCards renders the registrar section without wrapping a full document.
func PageCards() []template.HTML {
	return regCards(settings())
}

// IsJobRunning reports whether a registration job is in flight (drives the
// combined ops page auto-refresh).
func IsJobRunning() bool {
	snap := job.snapshot()
	return snap["running"] == true
}

// regPage serves the resource route: HTML by default, JSON on ?format=json,
// and the action query strings for the operator buttons.
func regPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()

	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "register":
		if errStart := job.start(); errStart != nil {
			if wantsJSON(request) {
				return jsonManagementResponse(http.StatusConflict, map[string]any{"error": errStart.Error()})
			}
			return plugui.HTML(DisplayName, plugui.Card("无法发起注册", plugui.Notice("danger", errStart.Error()),
				plugui.Action{Label: "返回", Path: "reg"}))
		}
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusOK, map[string]any{"started": true})
		}
	case "balance":
		amount, errAmount := newSMSPool(transportFor(h).do, cfg).leftAmount()
		if errAmount != nil {
			amount = "查询失败: " + errAmount.Error()
		}
		if wantsJSON(request) {
			return jsonManagementResponse(http.StatusOK, map[string]any{"balance": amount})
		}
	}
	if wantsJSON(request) {
		return jsonManagementResponse(http.StatusOK, job.snapshot())
	}
	return renderRegPage(h, cfg)
}

// renderRegPage renders the combined ops page from the /reg resource: pool
// cards (when ExtraPageCards is wired) plus the registrar section.
func renderRegPage(h *abiboot.Host, cfg Config) pluginapi.ManagementResponse {
	var fragments []template.HTML
	if ExtraPageCards != nil {
		fragments = append(fragments, ExtraPageCards(h)...)
	}
	fragments = append(fragments, regCards(cfg)...)
	if IsJobRunning() {
		return plugui.HTMLWithRefresh(4, DisplayName, fragments...)
	}
	return plugui.HTML(DisplayName, fragments...)
}

// regCards builds the registrar section: config, job status, and log.
func regCards(cfg Config) []template.HTML {
	snap := job.snapshot()

	guardText := "开启"
	if !cfg.Enabled {
		guardText = "enabled=false：只读，不发起注册"
	}
	tokenText := "未配置（注册前必填）"
	if cfg.SMSToken != "" {
		tokenText = "已配置"
	}
	var fragments []template.HTML
	fragments = append(fragments, plugui.Card("注册配置", plugui.Group(
		plugui.Fields(
			plugui.Field{Label: "总开关", Value: guardText},
			plugui.Field{Label: "接码 Token", Value: tokenText},
			plugui.Field{Label: "接码平台", Value: cfg.SMSBaseURL},
			plugui.Field{Label: "发信关键词", Value: cfg.SMSKeyword + "（收码同时尝试 华为）"},
			plugui.Field{Label: "短信等待", Value: fmt.Sprintf("%d ms", cfg.SMSWaitMS)},
			plugui.Field{Label: "邀请码", Value: orDefault(cfg.InviteCode, "随机绑定池内邀请码")},
			plugui.Field{Label: "凭证命名", Value: cfg.NamePrefix + "-<手机号>.json"},
		),
	), plugui.Action{Label: "注册新账号", Path: "reg", Query: "action=register", Kind: "primary"},
		plugui.Action{Label: "查接码余额", Path: "reg", Query: "action=balance&format=json"},
		plugui.Action{Label: "刷新进度", Path: "reg"}))

	statusTone := "info"
	statusText := "空闲"
	if snap["running"] == true {
		statusTone = "warning"
		statusText = "进行中（发起于 " + snap["started"].(string) + "）"
	} else if snap["done"] == true {
		if snap["succeeded"] == true {
			statusTone = "success"
			statusText = "成功"
		} else {
			statusTone = "danger"
			statusText = "失败"
		}
	}
	fragments = append(fragments, plugui.Card("任务状态", plugui.Group(
		plugui.Notice(statusTone, statusText),
		plugui.Fields(
			plugui.Field{Label: "手机号", Value: orDefault(snap["phone"].(string), "—")},
			plugui.Field{Label: "凭证文件", Value: orDefault(snap["file"].(string), "—")},
			plugui.Field{Label: "AK 前缀", Value: orDefault(snap["ak_prefix"].(string), "—")},
			plugui.Field{Label: "错误", Value: orDefault(snap["error"].(string), "—")},
		),
	)))

	if lines, ok := snap["lines"].([]string); ok && len(lines) > 0 {
		var rows strings.Builder
		rows.WriteString(`<pre class="log" style="white-space:pre-wrap;font-size:12px;line-height:1.6">`)
		for _, line := range lines {
			rows.WriteString(template.HTMLEscapeString(line) + "\n")
		}
		rows.WriteString(`</pre>`)
		fragments = append(fragments, plugui.Card("执行日志", template.HTML(rows.String())))
	}
	return fragments
}

// orDefault renders fallback when value is empty.
func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// wantsJSON reports whether the requester asked for the machine-readable form.
func wantsJSON(request pluginapi.ManagementRequest) bool {
	if strings.EqualFold(strings.TrimSpace(request.Query.Get("format")), "json") {
		return true
	}
	return strings.Contains(strings.ToLower(request.Headers.Get("Accept")), "application/json")
}
