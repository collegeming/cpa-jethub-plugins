package reg

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// smsPool is the d1jiema-compatible receive-SMS API. One GET per call, code
// plus token plus verb-specific params, plain text body. An `ERROR:` prefix
// in the body is the protocol's failure signal.
type smsPool struct {
	doer     doer
	baseURL  string
	token    string
	keyword  string
	cardType string
}

var smsPhonePattern = regexp.MustCompile(`1\d{10}`)

func newSMSPool(d doer, cfg Config) *smsPool {
	return &smsPool{
		doer:     d,
		baseURL:  strings.TrimRight(cfg.SMSBaseURL, "/"),
		token:    cfg.SMSToken,
		keyword:  cfg.SMSKeyword,
		cardType: cfg.SMSCardType,
	}
}

// call issues one pool verb. Errors from HTTP status, the ERROR: protocol
// marker and empty bodies all surface as Go errors.
func (s *smsPool) call(code string, params map[string]string) (string, error) {
	if s.token == "" {
		return "", fmt.Errorf("接码 Token 未配置")
	}
	query := url.Values{"code": {code}, "token": {s.token}}
	for key, value := range params {
		if value != "" {
			query.Set(key, value)
		}
	}
	response, errDo := s.doer(http.MethodGet, s.baseURL+"?"+query.Encode(), nil, nil)
	if errDo != nil {
		return "", fmt.Errorf("接码接口请求失败: %v", errDo)
	}
	text := strings.TrimSpace(string(response.Body))
	if response.StatusCode >= 400 {
		return "", fmt.Errorf("接码接口 HTTP %d: %s", response.StatusCode, truncate(text, 200))
	}
	if strings.HasPrefix(strings.ToUpper(text), "ERROR:") {
		return "", fmt.Errorf("%s", truncate(text, 200))
	}
	if text == "" {
		return "", fmt.Errorf("接码接口返回空")
	}
	return text, nil
}

// leftAmount returns the pool balance display string.
func (s *smsPool) leftAmount() (string, error) {
	return s.call("leftAmount", nil)
}

// getPhone draws one number matching the keyword.
func (s *smsPool) getPhone() (string, error) {
	text, errCall := s.call("getPhone", map[string]string{
		"keyWord":  s.keyword,
		"cardType": s.cardType,
	})
	if errCall != nil {
		return "", errCall
	}
	phone := smsPhonePattern.FindString(text)
	if phone == "" {
		return "", fmt.Errorf("取号失败: %s", truncate(text, 200))
	}
	return phone, nil
}

// getMsg fetches the latest SMS for the phone under one keyword.
func (s *smsPool) getMsg(phone, keyword string) (string, error) {
	return s.call("getMsg", map[string]string{
		"phone":   phone,
		"keyWord": keyword,
	})
}

// release returns the number to the pool so its cost is refunded.
func (s *smsPool) release(phone string) {
	_, _ = s.call("release", map[string]string{"phone": phone})
}

// smsKeywords are the sender keywords Huawei texts can arrive under —
// registration SMSes sign 【华为云】, login SMSes sign 【华为】.
var smsKeywords = []string{"华为云", "华为"}

// smsCodePattern pulls the verification digits out of a message body.
var smsCodePatterns = []*regexp.Regexp{
	regexp.MustCompile(`验证码[^\d]{0,8}(\d{4,8})`),
	regexp.MustCompile(`(\d{6})`),
	regexp.MustCompile(`(\d{4,8})`),
}

func extractSMSCode(text string) string {
	for _, pattern := range smsCodePatterns {
		if match := pattern.FindStringSubmatch(text); match != nil {
			return match[1]
		}
	}
	return ""
}

// waitSMS polls the pool until a new code arrives that differs from the
// snapshot taken before sending. Returns the code and the full message.
func (s *smsPool) waitSMS(phone string, waitMS int, oldMsg, oldCode string, log func(string)) (string, string, error) {
	deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
	var last string
	for time.Now().Before(deadline) {
		for _, keyword := range smsKeywords {
			text, errMsg := s.getMsg(phone, keyword)
			if errMsg != nil {
				continue
			}
			last = text
			if strings.Contains(text, "尚未收到") {
				continue
			}
			if oldMsg != "" && strings.TrimSpace(text) == strings.TrimSpace(oldMsg) {
				continue
			}
			code := extractSMSCode(text)
			if code == "" || (oldCode != "" && code == oldCode) {
				continue
			}
			return code, text, nil
		}
		remain := int(time.Until(deadline) / time.Second)
		if remain > 0 && log != nil {
			log(fmt.Sprintf("尚未收到短信，剩余 %ds", remain))
		}
		time.Sleep(3 * time.Second)
	}
	return "", last, fmt.Errorf("等待短信超时: %s", truncate(last, 200))
}

// snapshotMsg reads whatever SMS state exists before sending, so an old
// message does not get mistaken for the fresh code.
func (s *smsPool) snapshotMsg(phone string) (string, string) {
	for _, keyword := range smsKeywords {
		text, errMsg := s.getMsg(phone, keyword)
		if errMsg != nil || strings.Contains(text, "尚未收到") {
			continue
		}
		return text, extractSMSCode(text)
	}
	return "", ""
}
