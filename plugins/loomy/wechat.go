package main

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// WeChat QR login — the HTTP half, ported from `src/loomy-wechat.ts`.
//
// The reference's own comment says why the popup server of
// `src/loomy-wechat-login.ts` is not needed here: the WeChat `code` never comes
// from the callback page (it 404s by design, trap #22) but from a long poll, and
// a CPA plugin IS a web page served by the host. The QR is therefore rendered
// inline by `pluginui.go` and polled from the page load itself:
//
//	GET open.weixin.qq.com/connect/qrconnect?…   -> HTML with an embedded uuid
//	GET open.weixin.qq.com/connect/qrcode/<uuid> -> the QR image (JPEG, ~47 KB)
//	GET long.open.weixin.qq.com/connect/l/qrconnect?uuid=…[&last=…]&_=<ts>
//	                                             -> wx_errcode / wx_code state
//
// Every call goes through `hostRequest` (`host.http.do`): the plugin never opens
// a socket of its own, so the host keeps owning proxy, TLS and request logging.

const (
	// WechatAppID is the open-platform "website application" AppID of the Loomy
	// desktop client (`loomy-wechat.ts:33`).
	WechatAppID = "wx18d60be432287cf8"
	// WechatRedirectURI must be EXACTLY this official address: WeChat validates
	// the domain against a whitelist, and any other value (a local callback in
	// particular) answers an 872-byte "redirect_uri 参数错误" page instead of the
	// 42 KB authorize page (`loomy-wechat.ts:36-42`, trap #22). The page itself
	// 404s — nothing in this flow ever loads it, it is only a placeholder.
	WechatRedirectURI = "https://loomy.xunfei.cn/oauth/wechat/callback"

	// WechatAuthorizeURL serves the HTML that embeds the QR uuid.
	WechatAuthorizeURL = "https://open.weixin.qq.com/connect/qrconnect"
	// WechatQRCodeBase is the QR image endpoint; the uuid is appended raw.
	WechatQRCodeBase = "https://open.weixin.qq.com/connect/qrcode/"
	// WechatLongPollURL is the long-poll endpoint.
	WechatLongPollURL = "https://long.open.weixin.qq.com/connect/l/qrconnect"

	// WechatScope is the website-application login scope.
	WechatScope = "snsapi_login"

	// WechatUserAgent is sent on every WeChat call: the platform may reject an
	// empty or crawler UA (`loomy-wechat.ts:100-101`).
	WechatUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	// WechatReferer is the Referer of the authorize-page and QR-image calls; the
	// long poll uses the authorize URL itself instead (`loomy-wechat.ts:172,206,268`).
	WechatReferer = "https://open.weixin.qq.com/"

	// WechatPageTimeoutMS bounds the authorize-page and QR-image calls
	// (`loomy-wechat.ts:173,207`), WechatPollTimeoutMS the long poll
	// (`loomy-wechat.ts:46,269`). They are the reference's stated budget; the host
	// owns the actual transport timeout, and WeChat's long poll answers by itself
	// after about 25 s, which is what bounds one page load here.
	WechatPageTimeoutMS = 30_000
	WechatPollTimeoutMS = 40_000

	// wechatMinimumImageBytes rejects an error page dressed as an image
	// (`loomy-wechat.ts:217`).
	wechatMinimumImageBytes = 200
)

// Long-poll status values, with the semantics taken from WeChat's own embedded
// JavaScript (`switch(window.wx_errcode)`) rather than guessed
// (`loomy-wechat.ts:48-85`):
//
//	408 waiting   nothing scanned yet (the normal answer)
//	404 scanned   scanned, waiting for the phone confirmation — KEEP POLLING
//	405 confirmed the one-time code is in THIS frame
//	403 cancelled the user refused on the phone
//	402 expired   the QR must be re-fetched
//	—   error     transport failure; not fatal, the caller retries
//
// ⚠️ Trap #23: reversing 404 and 405 is a real, shipped defect — after the user
// confirms, WeChat answers 405 with `wx_code`, and an implementation that waits
// for a "404 carrying a code" waits forever, leaving the account pool empty.
const (
	wechatWaiting   = "waiting"
	wechatScanned   = "scanned"
	wechatConfirmed = "confirmed"
	wechatCancelled = "cancelled"
	wechatExpired   = "expired"
	wechatError     = "error"
)

var (
	// wechatUUIDImagePattern is the main path: the `<img class="js_qrcode_img"
	// src="/connect/qrcode/<uuid>">` the page embeds.
	wechatUUIDImagePattern = regexp.MustCompile(`/connect/qrcode/([A-Za-z0-9_\-=+/]+)`)
	// wechatUUIDPollPattern is the fallback: `var fordevtool =
	// "…/connect/l/qrconnect?uuid=<uuid>"`. Neither path needs JavaScript to run.
	wechatUUIDPollPattern = regexp.MustCompile(`l/qrconnect\?uuid=([A-Za-z0-9_\-=+/]+)`)
	// wechatUUIDPattern is the character/length rule a uuid must satisfy. The
	// floor is 6, not more: WeChat never promised a length, and a tighter rule
	// would fail silently the day the format shifts (`loomy-wechat.ts:103-109`).
	wechatUUIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-=+/]{6,64}$`)

	// wechatErrcodePattern / wechatCodePattern parse the long-poll body
	// (`loomy-wechat.ts:282-283`).
	wechatErrcodePattern = regexp.MustCompile(`wx_errcode\s*=\s*(\d+)`)
	wechatCodePattern    = regexp.MustCompile(`wx_code\s*=\s*'([^']*)'`)
)

// wechatPollResult is one long-poll iteration.
type wechatPollResult struct {
	// Status is one of the status constants above.
	Status string
	// Code is the one-time WeChat code; non-empty only for `confirmed`.
	Code string
	// Errcode is the raw `wx_errcode`, kept for diagnostics and for the next
	// call's `last` parameter.
	Errcode string
	// Detail explains an `error` status to the page.
	Detail string
}

// buildWechatAuthURL renders the authorize page URL, parameters in the
// reference's order, `redirect_uri` percent-encoded
// (`loomy-wechat.ts:116-124`).
func buildWechatAuthURL(state string) string {
	return WechatAuthorizeURL +
		"?appid=" + url.QueryEscape(WechatAppID) +
		"&redirect_uri=" + url.QueryEscape(WechatRedirectURI) +
		"&response_type=code" +
		"&scope=" + url.QueryEscape(WechatScope) +
		"&state=" + url.QueryEscape(state) +
		"#wechat_redirect"
}

// extractWechatUUID pulls the QR uuid out of the authorize page HTML.
//
// The page embeds it directly, so no JavaScript has to run — which is exactly
// what lets this flow live inside a plugin page. Both known shapes are tried and
// each candidate must pass the character/length rule; an unusable page yields an
// empty string, never a partial match (`loomy-wechat.ts:137-152`).
func extractWechatUUID(html string) string {
	if html == "" {
		return ""
	}
	if match := wechatUUIDImagePattern.FindStringSubmatch(html); match != nil {
		if wechatUUIDPattern.MatchString(match[1]) {
			return match[1]
		}
	}
	if match := wechatUUIDPollPattern.FindStringSubmatch(html); match != nil {
		if wechatUUIDPattern.MatchString(match[1]) {
			return match[1]
		}
	}
	return ""
}

// wechatQRImageURL addresses one QR image.
func wechatQRImageURL(uuid string) string {
	return WechatQRCodeBase + url.QueryEscape(uuid)
}

// wechatHeaders is the browser header set every WeChat call carries.
func wechatHeaders(referer string) http.Header {
	headers := http.Header{}
	headers.Set("User-Agent", WechatUserAgent)
	headers.Set("Referer", referer)
	return headers
}

// fetchWechatUUID downloads the authorize page and extracts the uuid.
//
// A page without a uuid is an ERROR rather than an empty string: polling an empty
// uuid would answer `waiting` forever and the user would never learn why
// (`loomy-wechat.ts:183-186`).
func fetchWechatUUID(h *abiboot.Host, cfg Config, state string) (string, error) {
	response, errDo := hostRequest(h, http.MethodGet, buildWechatAuthURL(state), wechatHeaders(WechatReferer), nil, cfg)
	if errDo != nil {
		return "", transportError("wechat_authorize", "微信授权页拉取失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", transportError("wechat_authorize", "微信授权页返回 HTTP %d", response.StatusCode)
	}
	uuid := extractWechatUUID(string(response.Body))
	if uuid == "" {
		// A wrong redirect_uri answers a short error page (872 bytes measured
		// against 42 KB for the real one), so the size is worth reporting.
		return "", transportError("wechat_uuid", "微信授权页未包含二维码 uuid（页面结构可能已变化；本次响应 %d 字节）",
			len(response.Body))
	}
	return uuid, nil
}

// fetchWechatQRImage downloads the QR image and returns it as a `data:` URL, so
// the browser never has to fetch anything from WeChat itself.
//
// ⚠️ The image is a JPEG, not a PNG: only testing for PNG magic bytes reports a
// perfectly good QR as "not an image" (trap #24, `loomy-wechat.ts:191-196`).
// PNG/JPEG/GIF are all accepted, and a body under 200 bytes is rejected because
// it can only be an error page.
func fetchWechatQRImage(h *abiboot.Host, cfg Config, uuid string) (string, error) {
	response, errDo := hostRequest(h, http.MethodGet, wechatQRImageURL(uuid), wechatHeaders(WechatReferer), nil, cfg)
	if errDo != nil {
		return "", transportError("wechat_qr", "微信二维码下载失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", transportError("wechat_qr", "微信二维码返回 HTTP %d", response.StatusCode)
	}
	mime := detectWechatImageMIME(response.Body)
	if mime == "" || len(response.Body) < wechatMinimumImageBytes {
		return "", transportError("wechat_qr", "微信二维码响应不是图片（%d 字节，可能是错误页）", len(response.Body))
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(response.Body), nil
}

// detectWechatImageMIME sniffs PNG/JPEG/GIF by magic bytes, returning an empty
// string for anything else (`loomy-wechat.ts:224-229`).
func detectWechatImageMIME(raw []byte) string {
	switch {
	case len(raw) >= 3 && raw[0] == 0xff && raw[1] == 0xd8 && raw[2] == 0xff:
		return "image/jpeg"
	case len(raw) >= 4 && raw[0] == 0x89 && raw[1] == 0x50 && raw[2] == 0x4e && raw[3] == 0x47:
		return "image/png"
	case len(raw) >= 3 && raw[0] == 0x47 && raw[1] == 0x49 && raw[2] == 0x46:
		return "image/gif"
	default:
		return ""
	}
}

// pollWechatOnce performs exactly ONE long-poll iteration
// (`loomy-wechat.ts:255-301`).
//
// A transport failure is reported as the `error` status and is deliberately NOT
// thrown: an occasional long-poll hiccup must not end the whole login, the page
// simply polls again.
//
// The call is synchronous and bounded by WeChat itself (about 25 s of hold, 40 s
// documented budget). It is deliberately NOT wrapped in a watchdog goroutine:
// the host dlcloses a plugin's shared object on unload, and a goroutine still
// executing inside it after the call returned would be a crash, not a timeout.
func pollWechatOnce(h *abiboot.Host, cfg Config, uuid, lastErrcode string, now time.Time) wechatPollResult {
	query := "uuid=" + url.QueryEscape(uuid)
	if trimmed := strings.TrimSpace(lastErrcode); trimmed != "" {
		query += "&last=" + url.QueryEscape(trimmed)
	}
	query += "&_=" + strconv.FormatInt(now.UnixMilli(), 10)

	response, errDo := hostRequest(h, http.MethodGet, WechatLongPollURL+"?"+query,
		wechatHeaders(buildWechatAuthURL("")), nil, cfg)
	if errDo != nil {
		return wechatPollResult{Status: wechatError, Detail: errDo.Error()}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return wechatPollResult{Status: wechatError,
			Detail: "长轮询返回 HTTP " + strconv.Itoa(response.StatusCode)}
	}
	return parseWechatPollBody(string(response.Body))
}

// parseWechatPollBody maps one long-poll body onto a status.
//
// The mapping is the reference's, and the 404/405 direction is the whole point
// (trap #23): 405 is the CONFIRMED frame and carries `wx_code`; 404 means
// "scanned, still waiting for the phone". A 405 without a code is an anomalous
// shape and is treated as `scanned` — never as a success with an empty code.
func parseWechatPollBody(body string) wechatPollResult {
	errcode := ""
	if match := wechatErrcodePattern.FindStringSubmatch(body); match != nil {
		errcode = match[1]
	}
	code := ""
	if match := wechatCodePattern.FindStringSubmatch(body); match != nil {
		code = match[1]
	}
	result := wechatPollResult{Errcode: errcode}
	switch errcode {
	case "405":
		if code != "" {
			result.Status = wechatConfirmed
			result.Code = code
			return result
		}
		// A 405 with no code must never be treated as success.
		result.Status = wechatScanned
		return result
	case "404":
		result.Status = wechatScanned
		return result
	case "403":
		result.Status = wechatCancelled
		return result
	case "402":
		result.Status = wechatExpired
		return result
	default:
		// 408 and every unknown value mean "keep waiting": the conservative
		// direction, because guessing success would send an empty code.
		result.Status = wechatWaiting
		return result
	}
}
