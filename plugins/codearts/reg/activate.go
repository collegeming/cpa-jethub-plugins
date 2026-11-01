package reg

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Portal endpoints behind the CodeArts free-tier activation. Verified live:
// package/info reports {"enable":false} on a fresh account; the subscribe
// order turns it into {"enable":true,"spec_code":"codearts.agent.individual.trial"}.
const (
	portalBase       = "https://codearts.huaweicloud.com"
	packageInfoURL   = portalBase + "/portal/snap-manager/v1/package/info"
	packageOrderURL  = portalBase + "/portal/DevCloudConsole/v5/orders/subscribe"
	portalProductID  = "OFFI1298576195589636096"
	portalSpecCode   = "codearts.agent.individual.trial"
	portalPluginName = "snap_AIIDE"
	portalPluginVer  = "5.2.0"
	packagePollLimit = 20
	packagePollEvery = 2 * time.Second
)

// plantPortalSession rides the /portal/authorize CAS gate — the consent page
// itself is useless headlessly, but its redirect chain plants the
// codearts-domain session cookies that the /portal/* REST APIs demand. The
// codearts-domain gate bounces through authui/login like every other domain.
func plantPortalSession(d doer, challenge string, port int, log func(string)) {
	raw := fmt.Sprintf("%s/portal/authorize?theme=2&locale=zh-cn"+
		"&uri_scheme=codearts-agent&client_id=%s&port=%d"+
		"&code_challenge=%s&code_challenge_method=SHA-256"+
		"&ticket_id=x&plugin-name=%s&plugin-version=%s",
		portalBase, OAuthClientID, port, challenge, portalPluginName, portalPluginVer)
	rideRedirects(d, raw, log)
}

// portalHeaders mirrors the authorize page's fetchRequest: XHR marker, JSON,
// and the cftk header which echoes the portal session cookie.
func portalHeaders(t sessionTransport) http.Header {
	return http.Header{
		"User-Agent":       {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
		"Accept":           {"application/json, text/plain, */*"},
		"Content-Type":     {"application/json"},
		"X-Requested-With": {"XMLHttpRequest"},
		"Cftk":             {t.cookie(portalBase, "cftk")},
	}
}

// packageEnabled reports whether the free tier is already active.
func packageEnabled(t sessionTransport) (bool, error) {
	response, errDo := t.do(http.MethodGet, packageInfoURL, portalHeaders(t), nil)
	if errDo != nil {
		return false, errDo
	}
	var info struct {
		Enable bool `json:"enable"`
	}
	if errDecode := json.Unmarshal(response.Body, &info); errDecode != nil {
		return false, fmt.Errorf("package/info HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 160))
	}
	return info.Enable, nil
}

// orderBody mirrors buyFreeactivation() in the portal's packageService.js: a
// single free-trial subscription for the individual plan.
func orderBody() ([]byte, error) {
	return json.Marshal(map[string]any{
		"is_agree_policy": "yes",
		"order_infos": []map[string]any{{
			"is_auto_pay":      "yes",
			"subscription_num": 1,
			"offering_info": map[string]any{
				"product_id":    portalProductID,
				"period_num":    1,
				"period_type":   2,
				"is_auto_renew": "yes",
				"product_info": map[string]any{
					"cloud_service_type":       "hws.service.type.devcloud",
					"resource_type":            "hws.resource.type.codearts.snap",
					"resource_spec_code":       portalSpecCode,
					"resource_size":            1,
					"resource_size_measure_id": 14,
				},
			},
		}},
	})
}

// activatePackage purchases the free trial when the account lacks it, then
// polls until the portal reports enable=true. DEV.CS.70142 means the package
// already exists server-side — still treated as success after the poll.
//
// The portal's CSRF token (a *cftk-named cookie mirrored in the cftk header)
// is issued lazily by the first codearts page hit, so the flow warms the
// domain first and retries the order once on a csrf rejection.
func activatePackage(t sessionTransport, log func(string)) error {
	warmPortal := func() {
		_, _ = t.do(http.MethodGet, portalBase+"/portal/", http.Header{
			"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
			"Accept":     {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		}, nil)
	}
	warmPortal()

	enabled, errInfo := packageEnabled(t)
	switch {
	case errInfo == nil && enabled:
		log("套餐已激活")
		return nil
	case errInfo != nil:
		log("套餐查询失败，尝试直接订购: " + errInfo.Error())
	}
	if t.cookie(portalBase, "cftk") == "" {
		log("cftk 令牌未就位")
	}

	raw, errBody := orderBody()
	if errBody != nil {
		return errBody
	}

	var response *pluginapi.HTTPResponse
	var result struct {
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			warmPortal()
		}
		headers := portalHeaders(t)
		headers.Set("X-Target-Services", "codearts-console-iam5")
		headers.Set("Origin", portalBase)
		headers.Set("Referer", portalBase+"/portal/")
		response, errBody = t.do(http.MethodPost, packageOrderURL, headers, raw)
		if errBody != nil {
			return fmt.Errorf("套餐订购请求失败: %v", errBody)
		}
		result.Status, result.Error = "", nil
		_ = json.Unmarshal(response.Body, &result)
		if !strings.Contains(string(response.Body), "csrf") {
			break
		}
	}
	if result.Status != "success" && (result.Error == nil || result.Error.Code != "DEV.CS.70142") {
		return fmt.Errorf("套餐订购失败 (HTTP %d): %s", response.StatusCode, truncate(string(response.Body), 200))
	}
	if result.Status == "success" {
		log("免费套餐已下单，等待生效...")
	} else {
		log("套餐已存在（DEV.CS.70142），等待生效...")
	}

	for attempt := 0; attempt < packagePollLimit; attempt++ {
		time.Sleep(packagePollEvery)
		enabled, errInfo = packageEnabled(t)
		if errInfo == nil && enabled {
			log("套餐生效")
			return nil
		}
	}
	return fmt.Errorf("套餐生效等待超时")
}

// isPortalAccessError recognizes the "no access" marker the quota API returns
// for an unactivated account (TM.00001005) — kept for callers that want to
// decide whether activation is the right remedy.
func isPortalAccessError(body string) bool {
	return strings.Contains(body, "TM.00001005")
}
