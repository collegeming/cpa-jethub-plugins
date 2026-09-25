package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleAuthLoginStart begins a device-code login and returns immediately.
//
// The flow is PKCE + device-code polling (`qoder-oauth.ts:119-254`): there is no
// local callback listener at all, because upstream uses the custom
// `qoder-app://` redirect and the official CLI polls
// `/api/v1/deviceToken/poll` instead (`qoder-oauth.ts:144-150`).
func handleAuthLoginStart(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	cfg := settings()
	region := activeRegion()
	// Allow a caller to request the other site explicitly.
	if request, errDecode := abiboot.Decode[pluginapi.AuthLoginStartRequest](raw); errDecode == nil {
		if candidate, ok := request.Metadata["region"]; ok {
			if text, isText := candidate.(string); isText && strings.TrimSpace(text) != "" {
				region = normalizeRegion(text)
			}
		}
	}

	session, errStart := startLoginSession(region)
	if errStart != nil {
		return nil, errStart
	}
	timeout := time.Duration(cfg.LoginTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = LoginTimeoutMS * time.Millisecond
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderKey,
		URL:       session.LoginURL(),
		State:     session.State,
		ExpiresAt: session.ExpiresAt,
		Metadata: map[string]any{
			"region":     string(session.Region),
			"machine_id": session.Device.MachineID,
			"hint": "在浏览器中打开 URL 完成授权后，由 auth.login.poll 轮询取回凭据；" +
				"设备码流程无需本地回调端口",
			"poll_interval_ms": cfg.PollIntervalMS,
		},
	}, nil
}

// handleAuthLoginPoll advances one in-flight login by at most one upstream call.
//
// The JavaScript loop sleeps `QODER_POLL_INTERVAL_MS` between attempts
// (`qoder-oauth.ts:269-302`); a host invocation must not block for that long, so
// the interval is enforced against the previous attempt and the call returns
// "pending" instead. Every other rule is kept: 404 means "the user has not
// finished yet" and must keep polling (`qoder-oauth.ts:247-258`), while any other
// non-2xx is a server fault and fails the flow.
func handleAuthLoginPoll(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthLoginPollRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	session, found := lookupLoginSession(strings.TrimSpace(request.State))
	if !found {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已超时，请重新发起登录",
		}, nil
	}

	if status, message, done := session.snapshot(); done {
		defer forgetLoginSession(request.State)
		if status == pluginapi.AuthLoginStatusSuccess && message != "" {
			// A finished success is reported through the stored credential.
			if credential := session.storedCredential(); credential != nil {
				auth, errAuth := authDataFor(credential, "")
				if errAuth != nil {
					return nil, errAuth
				}
				return pluginapi.AuthLoginPollResponse{Status: status, Message: message, Auth: auth}, nil
			}
		}
		if status == "" {
			status = pluginapi.AuthLoginStatusError
		}
		return pluginapi.AuthLoginPollResponse{Status: status, Message: message}, nil
	}

	cfg := settings()
	interval := time.Duration(cfg.PollIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = PollIntervalMS * time.Millisecond
	}
	if !session.due(interval) {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户完成授权",
		}, nil
	}
	session.markAttempt()

	p := productByID(string(session.Region))
	headers := canonicalHeader("Accept", "application/json")
	response, errDo := hostRequest(h, http.MethodGet, session.PollURL(), headers, nil, cfg)
	if errDo != nil {
		return pollFailure(session, request.State, cfg, errDo.Error())
	}

	switch {
	case response.StatusCode == http.StatusNotFound:
		// 404 is the documented "session not ready yet" signal; keep polling.
		session.resetFailures()
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户在浏览器中完成授权",
		}, nil

	case response.StatusCode < 200 || response.StatusCode >= 300:
		message := "登录服务返回异常（HTTP " + strconv.Itoa(response.StatusCode) + "）"
		session.fail(message)
		forgetLoginSession(request.State)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
	}

	session.resetFailures()
	payload := parseTokenPayloadJSON(response.Body)
	if payload.AccessToken == "" {
		// 2xx without a token is still "not ready"; keep polling.
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "登录服务尚未下发令牌，继续等待",
		}, nil
	}

	credential := buildCredential(payload, session.Device.MachineID, p.ID, "")
	auth, errAuth := authDataFor(credential, "")
	if errAuth != nil {
		return nil, errAuth
	}
	session.setCredential(credential, "登录成功")
	session.finish()
	forgetLoginSession(request.State)
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth:    auth,
	}, nil
}

// pollFailure counts a transport failure and fails the flow once the budget is
// exhausted (`qoder-oauth.ts:290-299`).
func pollFailure(session *loginSession, state string, cfg Config, detail string) (any, error) {
	limit := cfg.PollMaxFailures
	if limit <= 0 {
		limit = PollMaxFailures
	}
	if failures := session.noteFailure(); failures >= limit {
		message := "无法连接 Qoder 登录服务（连续 " + strconv.Itoa(failures) + " 次失败）：" + detail
		session.fail(message)
		forgetLoginSession(state)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: message}, nil
	}
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: "轮询失败，稍后重试：" + detail,
	}, nil
}
