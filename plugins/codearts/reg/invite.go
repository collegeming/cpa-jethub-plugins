package reg

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// CodeArts chat-workbench host. The ops endpoints mount under the SPA's
// configured basePath (/chat — $N() = service_cf3_config.basePath || "/chat",
// so oi+"/ops/..." resolves to /chat/PromptCenterService/v1/ops/...). The chat
// SPA itself is a static shell that never server-side redirects, so the
// devcloud-domain session is planted by bouncing authui/login?service= onto
// the invite landing URL — the same CAS ST dance every domain uses.
const (
	chatBase    = "https://devcloud.cn-north-4.huaweicloud.com"
	chatHomeURL = chatBase + "/chat/home"
	chatOpsBase = chatBase + "/chat/PromptCenterService/v1"
)

// inviteCodePattern matches the INV- codes Huawei issues for the referral
// campaign. Codes are ~140 chars; 10 keeps the pattern permissive.
var inviteCodePattern = regexp.MustCompile(`INV-[A-Za-z0-9_-]{10,}`)

// collectInviteCodes reads the `invite_code` field out of every CodeArts
// credential the host tracks. These are the referral codes previous runs
// minted — the pool the next registration draws from.
func collectInviteCodes(h *abiboot.Host) []string {
	entries, errList := h.ListAuth()
	if errList != nil {
		return nil
	}
	seen := map[string]bool{}
	codes := []string{}
	for _, entry := range entries {
		if entry.Type != "codearts" && entry.Provider != "codearts" {
			continue
		}
		fresh := &abiboot.Host{}
		response, errGet := fresh.GetAuth(entry.AuthIndex)
		if errGet != nil {
			continue
		}
		var probe struct {
			InviteCode string `json:"invite_code"`
		}
		if errDecode := json.Unmarshal(response.JSON, &probe); errDecode != nil {
			continue
		}
		code := strings.TrimSpace(probe.InviteCode)
		if code != "" && !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	return codes
}

// pickInviteCode chooses which referral code the new account binds to: an
// explicitly configured code wins; otherwise a random one from the pool.
func pickInviteCode(h *abiboot.Host, cfg Config) string {
	if cfg.InviteCode != "" {
		return cfg.InviteCode
	}
	codes := collectInviteCodes(h)
	if len(codes) == 0 {
		return ""
	}
	index, errRand := rand.Int(rand.Reader, big.NewInt(int64(len(codes))))
	if errRand != nil {
		return codes[0]
	}
	return codes[index.Int64()]
}

// chatHeaders mirrors the workbench's XHR layer: the xs base headers, the
// PromptCenter agent tag, and the cftk CSRF header mirrored off the
// devclouddevuibjtcftk session cookie — POSTs without it draw
// {"error":"csrf validate fail."}.
func chatHeaders(t sessionTransport) http.Header {
	return http.Header{
		"User-Agent":                    {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
		"Accept":                        {"application/json, text/plain, */*"},
		"Content-Type":                  {"application/json"},
		"x-requested-with":              {"XMLHttpRequest"},
		"x-codearts-doer-scenario-type": {"web"},
		"Agent-Type":                    {"PromptCenter"},
		"X-Language":                    {"zh-cn"},
		"Cftk":                          {t.cookie(chatBase, "cftk")},
		"Referer":                       {chatHomeURL},
		"Origin":                        {chatBase},
	}
}

// chatLandingURL is the CAS service the session bounce lands on. It stays the
// plain workbench home: the referral code binds via ops/invite/confirm-by-code
// instead of the invitecode URL param — the param'd landing (chat?source=...
// &invitecode=...) draws a WAF 403 and takes the session planting down with it.
func chatLandingURL() string {
	return chatHomeURL
}

// confirmInvite binds the fresh account to somebody's referral code — the
// workbench's `invitecode` URL param lands exactly here. The SPA retries on
// 500/429 with 1s/2s/4s backoff; we mirror that.
func confirmInvite(t sessionTransport, code string) error {
	raw := chatOpsBase + "/ops/invite/confirm-by-code?inviteCode=" + url.QueryEscape(code)
	var lastErr error
	for _, delay := range []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second} {
		if delay > 0 {
			time.Sleep(delay)
		}
		response, errDo := t.do(http.MethodPost, raw, chatHeaders(t), nil)
		if errDo != nil {
			lastErr = errDo
			continue
		}
		if response.StatusCode == 500 || response.StatusCode == 429 {
			lastErr = fmt.Errorf("HTTP %d", response.StatusCode)
			continue
		}
		if response.StatusCode != 200 {
			return fmt.Errorf("HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 160))
		}
		var probe struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		}
		if errDecode := json.Unmarshal(response.Body, &probe); errDecode != nil {
			if len(response.Body) == 0 {
				return fmt.Errorf("HTTP 200 空响应（会话未建立）")
			}
			return nil
		}
		if numeric, ok := probe.Code.(float64); ok && numeric != 0 && numeric != 200 {
			return fmt.Errorf("code %v: %s", numeric, probe.Message)
		}
		return nil
	}
	return lastErr
}

// confirmCampaign posts ops/confirm for a campaign id — the SPA's enrollment
// verb for welfare activities. Best effort; failures surface in the caller.
func confirmCampaign(t sessionTransport, campaignID string) error {
	raw, errMarshal := json.Marshal(map[string]any{"campaignId": campaignID})
	if errMarshal != nil {
		return errMarshal
	}
	response, errDo := t.do(http.MethodPost, chatOpsBase+"/ops/confirm", chatHeaders(t), raw)
	if errDo != nil {
		return errDo
	}
	if response.StatusCode != 200 || len(response.Body) == 0 {
		return fmt.Errorf("ops/confirm HTTP %d len=%d", response.StatusCode, len(response.Body))
	}
	var probe struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if errDecode := json.Unmarshal(response.Body, &probe); errDecode == nil {
		if numeric, ok := probe.Code.(float64); ok && numeric != 0 && numeric != 200 {
			return fmt.Errorf("code %v: %s", numeric, probe.Message)
		}
	}
	return nil
}

// fetchOpsDelivery returns the welfare-panel campaign list. Its payload is how
// the SPA discovers the invite campaign id; the account's own minted code may
// already sit inside it.
func fetchOpsDelivery(t sessionTransport) ([]byte, error) {
	response, errDo := t.do(http.MethodGet, chatOpsBase+"/ops/delivery?channel=WEB", chatHeaders(t), nil)
	if errDo != nil {
		return nil, errDo
	}
	if response.StatusCode != 200 || len(response.Body) == 0 {
		return nil, fmt.Errorf("ops/delivery HTTP %d len=%d（会话未建立）", response.StatusCode, len(response.Body))
	}
	return response.Body, nil
}

// inviteCampaignID digs the invite campaign's id out of the delivery payload:
// the object whose JSON mentions invite/邀请 wins, and its id/campaignId field
// is what ops/invite/generate expects.
func inviteCampaignID(delivery []byte) string {
	var probe struct {
		Data any `json:"data"`
	}
	if errDecode := json.Unmarshal(delivery, &probe); errDecode != nil {
		return ""
	}
	var found string
	var walk func(node any)
	walk = func(node any) {
		if found != "" {
			return
		}
		switch typed := node.(type) {
		case map[string]any:
			raw, _ := json.Marshal(typed)
			lower := strings.ToLower(string(raw))
			if strings.Contains(lower, "invite") || strings.Contains(lower, "邀请") {
				for _, key := range []string{"campaignId", "campaign_id", "id"} {
					switch v := typed[key].(type) {
					case float64:
						found = fmt.Sprintf("%v", int64(v))
						return
					case string:
						if v != "" {
							found = v
							return
						}
					}
				}
			}
			for _, v := range typed {
				walk(v)
			}
		case []any:
			for _, v := range typed {
				walk(v)
			}
		}
	}
	walk(probe.Data)
	return found
}

// fetchMyInviteCode mints this account's own referral code. The delivery
// payload is checked first — the code may already be inside the invite
// campaign's data — then ops/invite/generate is tried with the discovered
// campaignId. The code itself is matched out of whatever JSON comes back.
func fetchMyInviteCode(t sessionTransport, delivery []byte) (string, error) {
	if match := inviteCodePattern.FindString(string(delivery)); match != "" {
		return match, nil
	}
	campaignID := inviteCampaignID(delivery)
	if campaignID == "" {
		return "", fmt.Errorf("ops/delivery 中未找到邀请活动 ID")
	}
	raw, errMarshal := json.Marshal(map[string]any{
		"campaignId": campaignID,
		"channel":    "WEB",
	})
	if errMarshal != nil {
		return "", errMarshal
	}
	response, errDo := t.do(http.MethodPost, chatOpsBase+"/ops/invite/generate", chatHeaders(t), raw)
	if errDo != nil {
		return "", errDo
	}
	if match := inviteCodePattern.FindString(string(response.Body)); match != "" {
		return match, nil
	}
	return "", fmt.Errorf("invite/generate HTTP %d 未返回邀请码: %s", response.StatusCode, truncate(string(response.Body), 160))
}

// runInviteChain executes the referral half inside the live SSO session:
// bounces the authui session onto the devcloud chat domain through the CAS
// service= hop (planting its session cookies), binds one pool code as inviter,
// then mints and returns this account's own code for the next run. Every step
// is best effort — referral bookkeeping must never sink a registration.
func runInviteChain(t sessionTransport, host *abiboot.Host, cfg Config, log func(string)) string {
	code := pickInviteCode(host, cfg)

	// The chat SPA is a static 200 for everyone — riding it directly plants
	// nothing. The devcloud session only exists after authui issues an ST for
	// it, so the ride starts on authui/login?service=<landing URL>.
	gate := AuthUIBase + "/authui/login?service=" + url.QueryEscape(chatLandingURL())
	rideRedirects(t.do, gate, log)

	// The tcftk CSRF cookie is issued lazily — make sure it exists before the
	// first POST or the call burns a csrf rejection.
	if t.cookie(chatBase, "cftk") == "" {
		_, _ = t.do(http.MethodGet, chatHomeURL, chatHeaders(t), nil)
	}

	delivery, errDelivery := fetchOpsDelivery(t)
	if errDelivery != nil {
		log("获取活动列表失败: " + errDelivery.Error())
		return ""
	}

	// The invitee may have to join the referral campaign before a bind takes:
	// ops/confirm enrolls the account in the campaign the SPA surfaced.
	if campaignID := inviteCampaignID(delivery); campaignID != "" {
		if errJoin := confirmCampaign(t, campaignID); errJoin != nil {
			log("加入邀请活动未确认（不影响绑定）: " + errJoin.Error())
		}
	}

	if code != "" {
		if errConfirm := confirmInvite(t, code); errConfirm != nil {
			log("邀请码绑定失败（不影响注册）: " + errConfirm.Error())
		} else {
			log("已绑定池内邀请码 " + truncate(code, 18) + "...")
		}
	} else {
		log("池内暂无可用邀请码，跳过绑定")
	}

	myCode, errMine := fetchMyInviteCode(t, delivery)
	if errMine != nil {
		log("生成本号邀请码失败: " + errMine.Error())
		return ""
	}
	log("本号邀请码: " + truncate(myCode, 18) + "...")
	return myCode
}
