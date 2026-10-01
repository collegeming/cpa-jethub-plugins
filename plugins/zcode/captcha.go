package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// The claim endpoint is the ONE place ZCode still demands an Aliyun captcha,
// and the demand is enforced: without a token the server answers
//
//	400 {"code":3007,"msg":"captcha verify failed"}
//
// with no challenge in the body to solve. The token has to be minted by
// Aliyun's own frontend SDK in a browser, which is why this plugin cannot
// produce one unattended — and why a one-click check-in for this provider can
// never succeed on its own.
//
// What makes a HUMAN-assisted claim possible is that the parameters are not
// secret: the server publishes them itself, in the same `client/configs`
// document the official client reads (`data.configs.captcha`). A page can
// therefore render the real widget, hand the resulting token back, and let the
// plugin attach it to the claim.
//
// Measured 2026-10-01 against the live service:
//
//	GET /api/v1/client/configs?platform=unknown
//	-> {"data":{"configs":{"captcha":{"enabled":true,"prefix":"no8xfe",
//	    "region":"cn","sceneId":"11xygtvd","skip_model_request":true}}}}
//
// The `platform=unknown` query is required: `win32`, `linux` and an absent
// parameter all answer `400 code 3001` (the same parameter gate inference has).

// captchaConfig is the subset of `data.configs.captcha` this plugin needs.
type captchaConfig struct {
	// Enabled reports whether the server wants a captcha at all.
	Enabled bool `json:"enabled"`
	// Prefix is the Aliyun captcha prefix (`no8xfe` today).
	Prefix string `json:"prefix"`
	// Region is sent back alongside the token as
	// `X-Aliyun-Captcha-Verify-Region`.
	Region string `json:"region"`
	// SceneID selects the Aliyun captcha scene (`11xygtvd` today).
	SceneID string `json:"sceneId"`
}

// usable reports whether the parameters are complete enough to render a widget.
// `enabled:false` still renders nothing: the server would not want a token.
func (c captchaConfig) usable() bool {
	return c.Enabled && c.SceneID != "" && c.Prefix != "" && c.Region != ""
}

// captchaToken is one solved challenge on its way to the claim request.
type captchaToken struct {
	// VerifyParam is what Aliyun's callback hands the page.
	VerifyParam string
	// Region rides along when the server sent one.
	Region string
}

// captchaHeaders renders the two headers the official client attaches. The
// names come from the client's own bundle
// (`~/.zcode/server/zcode-server.cjs`):
//
//	"X-Aliyun-Captcha-Verify-Param": request.captchaVerifyParam,
//	...request.captchaRegion ? {"X-Aliyun-Captcha-Verify-Region": ...} : {},
func (t captchaToken) headers() map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(t.VerifyParam) == "" {
		return out
	}
	out["X-Aliyun-Captcha-Verify-Param"] = t.VerifyParam
	if region := strings.TrimSpace(t.Region); region != "" {
		out["X-Aliyun-Captcha-Verify-Region"] = region
	}
	return out
}

// fetchCaptchaConfig reads the server-published captcha parameters.
//
// A failure is not fatal: it only means the page cannot offer the widget, and
// the caller falls back to reporting the 3007 in words.
func fetchCaptchaConfig(h *abiboot.Host, credential *Credential, cfg Config) (captchaConfig, error) {
	endpoint := Origin + CaptchaConfigPath + "?" + CaptchaPlatformQuery
	headers := buildHeaders(credential, cfg, headerOptions{Authorization: true, Accept: "application/json"})
	response, errDo := hostRequest(h, http.MethodGet, endpoint, headers, nil)
	if errDo != nil {
		return captchaConfig{}, errDo
	}
	envelope := parseUpstreamEnvelope(response.Body, response.StatusCode)
	if envelope.Code.present && envelope.Code.value != 0 {
		return captchaConfig{}, abiboot.Errorf("captcha_config",
			"读取验证码配置失败（code %d）", envelope.Code.value)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return captchaConfig{}, abiboot.Errorf("captcha_config",
			"读取验证码配置失败（HTTP %d）", response.StatusCode)
	}
	var decoded struct {
		Data struct {
			Configs struct {
				Captcha captchaConfig `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(envelope.Data, &decoded.Data); errUnmarshal != nil {
		return captchaConfig{}, abiboot.Errorf("captcha_config", "解析验证码配置失败：%v", errUnmarshal)
	}
	return decoded.Data.Configs.Captcha, nil
}
