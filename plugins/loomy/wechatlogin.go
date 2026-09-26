package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// The WeChat QR login flow, ported from `src/loomy-wechat-login.ts` with the one
// structural difference this host forces: there is no loopback popup server and
// no browser window to open. The authorize page's uuid, the QR image and the
// long poll are all plain HTTP (`wechat.go`), a resource route can render the QR
// inline, and the page walks the state machine by reloading itself.
//
//	page load with no QR      -> fetch uuid + image, render, reload in 1 s
//	page load with a QR       -> ONE long poll, then re-render (or move on)
//	405 + wx_code             -> bind/auth -> bind/skip (bind=1) or phone binding
//	bind=0                    -> the keypad page collects phone + SMS code
//	credential stored         -> no refresh at all: the flow is over
//
// Nothing here loops: one page load is exactly one poll iteration. The long poll
// itself is what bounds a load (WeChat holds it for about 25 s and the documented
// budget is 40 s, `loomy-wechat.ts:46`).

// WeChat page refresh intervals, in seconds.
//
// A page load that has just done a long poll reloads itself: quickly when the
// poll came back immediately (the state is moving — the user is scanning or
// confirming, and the next frame should show it), and more slowly when the poll
// held the connection (nobody is at the QR; there is nothing to be fast about).
// The long poll paces the flow either way, so neither value can hammer WeChat.
const (
	wechatRefreshFast      = 1
	wechatRefreshSlow      = 3
	wechatImmediateSeconds = 2 * time.Second
)

// QR page stages. The first five are the long-poll statuses themselves; the rest
// are this plugin's own states.
const (
	// stageWechatBind asks for a phone number and its SMS code.
	stageWechatBind = "bind"
	// stageWechatFailed is a terminal failure with a way back.
	stageWechatFailed = "failed"
	// stageWechatDone means the credential is stored.
	stageWechatDone = "done"
)

// wechatOutcome is what the page should render after one flow step.
type wechatOutcome struct {
	// Stage is one of the constants above.
	Stage string
	// Message is the notice the page shows.
	Message string
	// RefreshSeconds is the meta-refresh interval; 0 means "do not reload".
	RefreshSeconds int
	// Credential is set once the login succeeded.
	Credential *Credential
	// FileName is the auth file the credential was stored under.
	FileName string
}

// wechatRefreshAfter picks the reload interval for a poll that took elapsed.
func wechatRefreshAfter(elapsed time.Duration) int {
	if elapsed < wechatImmediateSeconds {
		return wechatRefreshFast
	}
	return wechatRefreshSlow
}

// startWechatQR fetches a fresh QR (uuid + image) and installs it in the session.
//
// The image is fetched once per uuid and cached as a `data:` URL, so re-rendering
// the page on every poll costs no WeChat call at all — the only call a refresh
// makes is the long poll itself.
func startWechatQR(h *abiboot.Host, cfg Config, session *loginSession, _ time.Time) error {
	uuid, errUUID := fetchWechatUUID(h, cfg, session.stateValue())
	if errUUID != nil {
		return errUUID
	}
	image, errImage := fetchWechatQRImage(h, cfg, uuid)
	if errImage != nil {
		return errImage
	}
	session.updateWechat(func(w *wechatState) {
		// A new QR invalidates everything the old one produced: the code, the
		// rcode and the binding context all belonged to that scan.
		*w = wechatState{UUID: uuid, Image: image, Stage: wechatWaiting}
	})
	return nil
}

// advanceWechatQR moves the QR flow by exactly one step and reports what to show.
func advanceWechatQR(h *abiboot.Host, cfg Config, session *loginSession, now time.Time) wechatOutcome {
	current := session.wechatStateValue()
	if strings.TrimSpace(current.UUID) == "" {
		// No QR yet: get one and render it NOW. Polling on this same load would
		// delay the QR by a whole long poll, so the poll waits for the reload.
		if errStart := startWechatQR(h, cfg, session, now); errStart != nil {
			return wechatOutcome{
				Stage:          wechatError,
				Message:        "获取微信二维码失败：" + errStart.Error(),
				RefreshSeconds: wechatRefreshSlow,
			}
		}
		return wechatOutcome{
			Stage:          wechatWaiting,
			Message:        "二维码已生成，请用微信扫描（页面会自动刷新）",
			RefreshSeconds: wechatRefreshFast,
		}
	}

	started := time.Now()
	result := pollWechatOnce(h, cfg, current.UUID, current.Last, now)
	refresh := wechatRefreshAfter(time.Since(started))
	session.updateWechat(func(w *wechatState) {
		w.Stage = result.Status
		w.Detail = result.Detail
		// `last` is what lets WeChat answer a state change immediately instead of
		// holding the next poll for its full window.
		w.Last = result.Errcode
	})

	switch result.Status {
	case wechatWaiting:
		// 408 and every unknown code: still nothing to do but wait.
		return wechatOutcome{
			Stage:          wechatWaiting,
			Message:        "等待微信扫码…（本页每次加载只做一次长轮询）",
			RefreshSeconds: refresh,
		}

	case wechatScanned:
		// ⚠️ 404 = scanned, the phone still has to confirm: keep polling. This is
		// the status a reversed 404/405 mapping hangs on forever (trap #23).
		return wechatOutcome{
			Stage:          wechatScanned,
			Message:        "已扫码，请在手机上点「确认登录」…",
			RefreshSeconds: refresh,
		}

	case wechatCancelled:
		return wechatOutcome{
			Stage:   wechatCancelled,
			Message: "你在手机上取消了本次登录。可以重新获取二维码再试一次。",
		}

	case wechatExpired:
		if errStart := startWechatQR(h, cfg, session, now); errStart != nil {
			return wechatOutcome{
				Stage:          wechatError,
				Message:        "二维码已过期，重新获取也失败了：" + errStart.Error(),
				RefreshSeconds: wechatRefreshSlow,
			}
		}
		return wechatOutcome{
			Stage:          wechatExpired,
			Message:        "二维码已过期，已自动换了一张，请重新扫描",
			RefreshSeconds: wechatRefreshFast,
		}

	case wechatConfirmed:
		// ⚠️ 405 carries the one-time code in THIS frame. The code is single-use,
		// so it stops being usable the moment bind/auth accepts it; everything
		// after that runs on the `rcode` (trap in §2.4).
		session.updateWechat(func(w *wechatState) {
			w.Code = result.Code
			w.Stage = wechatConfirmed
		})
		return completeWechatCodeExchange(h, cfg, session, now)

	default:
		// A long-poll hiccup must not end the login: the page simply tries again
		// (`loomy-wechat.ts:272-280`).
		detail := result.Detail
		if detail == "" {
			detail = "网络异常"
		}
		return wechatOutcome{
			Stage:          wechatError,
			Message:        "长轮询失败：" + detail + "（稍后自动重试）",
			RefreshSeconds: wechatRefreshSlow,
		}
	}
}

// completeWechatCodeExchange trades the one-time WeChat code for a binding
// context, then continues on the `rcode` it returns.
func completeWechatCodeExchange(h *abiboot.Host, cfg Config, session *loginSession, now time.Time) wechatOutcome {
	current := session.wechatStateValue()
	if strings.TrimSpace(current.Code) == "" {
		return wechatOutcome{Stage: stageWechatFailed, Message: "微信授权码为空，请重新扫码"}
	}
	binding, errAuth := bindThirdAccountAuth(h, cfg, current.Code, now)
	if errAuth != nil {
		return wechatOutcome{
			Stage:   stageWechatFailed,
			Message: "换取讯飞会话失败（bind/auth）：" + errAuth.Error(),
		}
	}
	session.updateWechat(func(w *wechatState) {
		w.RCode = binding.RCode
		w.Bind = binding.Bind
		w.Nickname = binding.Nickname
		// The code has served its single purpose; later steps must not resend it.
		w.Code = ""
	})
	return resumeWechatLogin(h, cfg, session, now)
}

// resumeWechatLogin continues from the binding context, which is also where a
// retry after a transient failure re-enters: `bind === 1` skips the phone step,
// anything else (including a missing `bind`) asks for a phone number.
func resumeWechatLogin(h *abiboot.Host, cfg Config, session *loginSession, now time.Time) wechatOutcome {
	current := session.wechatStateValue()
	if strings.TrimSpace(current.RCode) == "" {
		return wechatOutcome{
			Stage:   stageWechatFailed,
			Message: "登录会话里没有 rcode（可能已过期），请重新扫码",
		}
	}
	if current.Bind != 1 {
		// ⚠️ `bind` is 1 ONLY when the server said the number 1; everything else
		// goes through the phone binding. The conservative direction is on
		// purpose: skipping it would produce a credential with no recoverable
		// identity (`loomy-oauth.ts:230-232`).
		return wechatOutcome{
			Stage:   stageWechatBind,
			Message: "这个微信号还没有绑定讯飞手机号，需要用短信验证码绑定一次（只会发生一次）",
		}
	}
	login, errSkip := bindSkip(h, cfg, current.RCode, now)
	if errSkip != nil {
		return wechatOutcome{
			Stage:   stageWechatFailed,
			Message: "微信登录失败（bind/skip）：" + errSkip.Error(),
		}
	}
	// ⚠️ The bind=1 path persists an EMPTY phone on purpose
	// (`loomy-wechat-login.ts:246`): 讯飞 knows the number but never sends it, and
	// inventing one would be worse than showing "未绑定". The WeChat nickname is
	// display-only.
	credential := buildCredential(login.Session, login.UserID, "", current.Nickname, cfg, now)
	message := "登录成功，账号已保存（微信扫码）"
	if errStore := storeLoginCredential(h, cfg, session, credential, message); errStore != nil {
		return wechatOutcome{Stage: stageWechatFailed, Message: errStore.Error()}
	}
	return wechatOutcome{
		Stage:      stageWechatDone,
		Message:    message,
		Credential: credential,
		FileName:   session.storedFileName(),
	}
}

// appendBindDigit adds one keypad press to the binding code, capping it at six
// characters.
func (s *loginSession) appendBindDigit(digit string) {
	trimmed := strings.TrimSpace(digit)
	if trimmed == "" || !isAllDigits(trimmed) {
		return
	}
	s.updateWechat(func(w *wechatState) {
		if len(w.BindDigits) < 6 {
			w.BindDigits += trimmed
		}
	})
}

// clearBindDigits resets the binding-code keypad buffer.
func (s *loginSession) clearBindDigits() {
	s.updateWechat(func(w *wechatState) { w.BindDigits = "" })
}

// sendWechatBindCode requests the SMS code that binds a phone to the WeChat
// account and records the msgid the next step needs.
func sendWechatBindCode(h *abiboot.Host, cfg Config, session *loginSession, phone string, now time.Time) error {
	if session == nil {
		return statusError(false, "missing_state", http.StatusBadRequest, "缺少 state 参数，请重新开始登录")
	}
	current := session.wechatStateValue()
	if strings.TrimSpace(current.RCode) == "" {
		return statusError(false, "missing_rcode", http.StatusBadRequest, "登录会话里没有 rcode，请重新扫码")
	}
	resolved := normalizePhone(phone)
	if !phonePattern.MatchString(resolved) {
		resolved = normalizePhone(session.phoneDraftValue())
	}
	if !phonePattern.MatchString(resolved) {
		return statusError(false, "invalid_phone", 400,
			"手机号格式不正确：%q（需要 11 位大陆手机号）", strings.TrimSpace(phone))
	}
	msgID, errSend := bindSendMsg(h, cfg, current.RCode, resolved, now)
	if errSend != nil {
		return errSend
	}
	session.updateWechat(func(w *wechatState) {
		w.BindPhone = resolved
		w.BindMsgID = msgID
		w.BindDigits = ""
	})
	return nil
}

// verifyWechatBindCode submits the binding code and STORES the credential.
//
// Storing here is what keeps `auth.login.poll` reporting `pending` until the
// credential actually exists, for both login paths alike.
func verifyWechatBindCode(h *abiboot.Host, cfg Config, session *loginSession, code string, now time.Time) (*Credential, error) {
	if session == nil {
		return nil, statusError(false, "missing_state", http.StatusBadRequest, "缺少 state 参数，请重新开始登录")
	}
	current := session.wechatStateValue()
	if strings.TrimSpace(current.RCode) == "" {
		return nil, statusError(false, "missing_rcode", http.StatusBadRequest, "登录会话里没有 rcode，请重新扫码")
	}
	if strings.TrimSpace(current.BindMsgID) == "" {
		return nil, statusError(false, "missing_msgid", http.StatusBadRequest, "请先发送绑定验证码")
	}
	resolved := strings.TrimSpace(code)
	if resolved == "" {
		resolved = current.BindDigits
	}
	if resolved == "" {
		return nil, statusError(false, "missing_code", http.StatusBadRequest, "请填写短信验证码")
	}
	login, errCheck := bindCheckCode(h, cfg, current.RCode, resolved, current.BindMsgID, now)
	if errCheck != nil {
		// A failed submit keeps the rcode and the msgid so the user can retry,
		// exactly like the SMS path (`jet-hub-rpc.ts:1174-1183`).
		return nil, errCheck
	}
	// ⚠️ The number the SERVER returns wins over the one typed on the page
	// (`loomy-wechat-login.ts:281`); the typed number is only a fallback for a
	// response that omits it.
	phone := strings.TrimSpace(login.Phone)
	if phone == "" {
		phone = current.BindPhone
	}
	credential := buildCredential(login.Session, login.UserID, phone, current.Nickname, cfg, now)
	if errStore := storeLoginCredential(h, cfg, session, credential, "登录成功，账号已保存（微信扫码 + 绑定手机号）"); errStore != nil {
		return nil, errStore
	}
	return credential, nil
}
