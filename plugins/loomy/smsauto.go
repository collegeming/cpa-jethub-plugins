package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Auto SMS login via d1jiema: getPhone → sendMsgCode → wait SMS → checkCode → save.
//
// Driven from the login resource page (`?action=auto` confirms, `?action=autorun`
// executes) because resource routes are GET-only. A POST management route
// `/loomy/sms/auto` is also registered for scripts that used to call
// `/v0/management/plugins/loomy-provider/sms/auto`.

const (
	autoActionConfirm = "auto"
	autoActionRun     = "autorun"
)

func loginAutoConfirmPage(cfg Config, request pluginapi.ManagementRequest, session *loginSession) pluginapi.ManagementResponse {
	state := session.stateValue()
	token, keyword, base := resolvedSMSConfig(cfg)
	tokenOK := "已配置"
	if token == "" {
		tokenOK = "未配置（请先填 sms_token）"
	}
	body := []template.HTML{
		addAccountNotice(request),
		plugui.Notice("warning", "自动接码会从接码平台取一个新号并发短信，会产生接码费用。确认后开始取号 → 发码 → 收码 → 登录。"),
		plugui.Card("自动接码（d1jiema）", plugui.Group(
			plugui.Fields(
				plugui.Field{Label: "接码平台", Value: base},
				plugui.Field{Label: "短信抬头关键词", Value: keyword},
				plugui.Field{Label: "sms_token", Value: tokenOK},
				plugui.Field{Label: "最长等待", Value: fmt.Sprintf("%d 秒", cfg.smsWaitMS()/1000)},
				plugui.Field{Label: "脚本用法", Value: "GET ?action=autorun&state=" + state + " 或 POST /v0/management/loomy/sms/auto"},
			),
		),
			plugui.Action{Label: "确认自动接码登录", Query: "action=" + autoActionRun + "&state=" + state, Kind: "primary"},
			plugui.Action{Label: "改用手动短信", Query: "action=sms&state=" + state},
			plugui.Action{Label: "返回状态", Path: "status"},
		),
	}
	return pluguiPage("Loomy 自动接码登录", body...)
}

func loginAutoRunPage(h *abiboot.Host, cfg Config, session *loginSession) pluginapi.ManagementResponse {
	credential, phone, errRun := runAutoSMSLogin(h, cfg, session, time.Now())
	if errRun != nil {
		return pluguiPage("Loomy 自动接码失败",
			plugui.Notice("danger", errRun.Error()),
			plugui.Card("自动接码", plugui.Group(
				plugui.Fields(
					plugui.Field{Label: "手机号", Value: emptyText(maskPhone(phone))},
					plugui.Field{Label: "state", Value: session.stateValue()},
				),
			),
				plugui.Action{Label: "再试一次", Query: "action=" + autoActionConfirm + "&state=" + session.stateValue(), Kind: "primary"},
				plugui.Action{Label: "改用手动短信", Query: "action=sms&state=" + session.stateValue()},
				plugui.Action{Label: "返回状态", Path: "status"},
			),
		)
	}
	return loginSuccessPage(session, credential, "自动接码（"+maskPhone(phone)+"）")
}

// runAutoSMSLogin performs the full d1jiema auto login and stores the credential.
// On failure it may still return the phone that was drawn (for display / release).
func runAutoSMSLogin(h *abiboot.Host, cfg Config, session *loginSession, now time.Time) (*Credential, string, error) {
	if session == nil {
		return nil, "", statusError(false, "missing_state", http.StatusBadRequest, "缺少 state 参数，请重新开始登录")
	}
	pool := newSMSPool(cfg)
	if strings.TrimSpace(pool.token) == "" {
		return nil, "", statusError(false, "missing_sms_token", http.StatusBadRequest,
			"接码 token 未配置：在 plugins.configs.loomy-provider.sms_token 或环境变量 LOOMY_SMS_TOKEN 中设置")
	}

	phone, errPhone := pool.getPhone()
	if errPhone != nil {
		return nil, "", statusError(false, "sms_get_phone", http.StatusBadGateway, "%v", errPhone)
	}

	msgID, errSend := sendMsgCode(h, cfg, phone, now)
	if errSend != nil {
		pool.release(phone)
		return nil, phone, errSend
	}
	session.notePhone(phone)
	session.noteMsgID(msgID)

	code, errWait := waitForSMSCode(pool, phone, cfg.smsWaitMS())
	if errWait != nil {
		pool.release(phone)
		return nil, phone, statusError(false, "sms_timeout", http.StatusGatewayTimeout,
			"接码超时，没有收到验证码（%s）：%v", maskPhone(phone), errWait)
	}

	sessionToken, userID, errCheck := checkCode(h, cfg, phone, code, msgID, now)
	if errCheck != nil {
		return nil, phone, errCheck
	}
	credential := buildCredential(sessionToken, userID, phone, "", cfg, now)
	if errStore := storeLoginCredential(h, cfg, session, credential, "自动接码登录成功，账号已保存"); errStore != nil {
		return nil, phone, errStore
	}
	return credential, phone, nil
}

func waitForSMSCode(pool *smsPool, phone string, waitMS int) (string, error) {
	deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
	var last string
	for time.Now().Before(deadline) {
		text, pending, errMsg := pool.getMsg(phone)
		if errMsg != nil {
			return "", errMsg
		}
		last = text
		if !pending {
			if code := extractSMSCode(text); code != "" {
				return code, nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		sleep := 3 * time.Second
		if remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
	return "", fmt.Errorf("等待短信超时: %s", truncate(last, 120))
}

// handleSMSAutoJSON serves POST /v0/management/loomy/sms/auto for scripts.
func handleSMSAutoJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	state := strings.TrimSpace(request.Query.Get("state"))
	if state == "" && len(request.Body) > 0 {
		var body struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(request.Body, &body)
		state = strings.TrimSpace(body.State)
	}
	var session *loginSession
	var ok bool
	if state != "" {
		session, ok = lookupLoginSession(state)
	}
	if !ok || session == nil {
		session = startLoginSession(cfg, "")
	}
	credential, phone, errRun := runAutoSMSLogin(h, cfg, session, time.Now())
	if errRun != nil {
		return jsonManagementResponse(statusOf(errRun, http.StatusBadGateway), map[string]any{
			"ok":           false,
			"error":        errRun.Error(),
			"phone_masked": maskPhone(phone),
			"state":        session.stateValue(),
		})
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":           true,
		"label":        credential.displayLabel(),
		"phone_masked": maskPhone(phone),
		"state":        session.stateValue(),
		"auth_file":    session.fileNameValue(),
	})
}

func (s *loginSession) fileNameValue() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fileName
}
