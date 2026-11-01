package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// d1jiema (接码) — same platform as autoclaw / codeartsreg on this host.
const (
	defaultSMSAPIBase = "https://api.d1jiema.com/zc/data.php"
	defaultSMSKeyword = "讯飞"
	defaultSMSWaitMS  = 90_000
)

var (
	smsHTTPClient = &http.Client{Timeout: 15 * time.Second}

	smsCodeAfterWord  = regexp.MustCompile(`(?i)(?:验证码|校验码|code)\D{0,12}(\d{4,8})`)
	smsStandaloneCode = regexp.MustCompile(`(?:^|[^\d])(\d{6})(?:[^\d]|$)`)
)

// smsPool talks to a d1jiema-compatible receive-SMS API.
type smsPool struct {
	baseURL string
	token   string
	keyword string
}

func newSMSPool(cfg Config) *smsPool {
	token, keyword, base := resolvedSMSConfig(cfg)
	return &smsPool{baseURL: base, token: token, keyword: keyword}
}

func resolvedSMSConfig(cfg Config) (token, keyword, base string) {
	token = strings.TrimSpace(cfg.SMSToken)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("LOOMY_SMS_TOKEN"))
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv("SMS_TOKEN"))
	}
	keyword = strings.TrimSpace(cfg.SMSKeyword)
	if keyword == "" {
		keyword = defaultSMSKeyword
	}
	base = strings.TrimRight(strings.TrimSpace(cfg.SMSAPIBase), "/")
	if base == "" {
		base = defaultSMSAPIBase
	}
	return token, keyword, base
}

func (cfg Config) smsWaitMS() int {
	if cfg.SMSWaitMS > 0 {
		return cfg.SMSWaitMS
	}
	return defaultSMSWaitMS
}

func (s *smsPool) call(code string, params map[string]string) (string, error) {
	if strings.TrimSpace(s.token) == "" {
		return "", fmt.Errorf("接码 token 未配置：在 plugins.configs.loomy-provider.sms_token 或环境变量 LOOMY_SMS_TOKEN 中设置")
	}
	query := url.Values{"code": {code}, "token": {s.token}}
	for key, value := range params {
		if value != "" {
			query.Set(key, value)
		}
	}
	req, errReq := http.NewRequest(http.MethodGet, s.baseURL+"?"+query.Encode(), nil)
	if errReq != nil {
		return "", fmt.Errorf("接码请求无法发出")
	}
	resp, errDo := smsHTTPClient.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("接码平台没有应答: %v", errDo)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := strings.TrimSpace(string(raw))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("接码平台返回 HTTP %d", resp.StatusCode)
	}
	if strings.HasPrefix(strings.ToUpper(text), "ERROR:") {
		msg := strings.TrimSpace(strings.TrimPrefix(text, "ERROR:"))
		msg = strings.TrimSpace(strings.TrimPrefix(msg, "error:"))
		msg = strings.ReplaceAll(msg, s.token, "[REDACTED]")
		return "", fmt.Errorf("接码：%s", msg)
	}
	if text == "" {
		return "", fmt.Errorf("接码接口返回空")
	}
	return text, nil
}

func (s *smsPool) getPhone() (string, error) {
	text, errCall := s.call("getPhone", map[string]string{"keyWord": s.keyword})
	if errCall != nil {
		return "", errCall
	}
	phone := smsPhoneDigits(text)
	if phone == "" {
		return "", fmt.Errorf("接码平台返回的不是手机号: %s", truncate(text, 80))
	}
	return phone, nil
}

func (s *smsPool) getMsg(phone string) (text string, pending bool, err error) {
	text, err = s.call("getMsg", map[string]string{
		"phone":   phone,
		"keyWord": s.keyword,
	})
	if err != nil {
		return "", false, err
	}
	if strings.Contains(text, "尚未收到") {
		return text, true, nil
	}
	return text, false, nil
}

func (s *smsPool) release(phone string) {
	_, _ = s.call("release", map[string]string{"phone": phone})
}

func extractSMSCode(message string) string {
	if match := smsCodeAfterWord.FindStringSubmatch(message); len(match) == 2 {
		return match[1]
	}
	if match := smsStandaloneCode.FindStringSubmatch(message); len(match) == 2 {
		return match[1]
	}
	return ""
}

func smsPhoneDigits(raw string) string {
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	number := digits.String()
	if strings.HasPrefix(number, "86") && len(number) == 13 {
		number = number[2:]
	}
	if !phonePattern.MatchString(number) {
		return ""
	}
	return number
}

func maskPhone(phone string) string {
	phone = normalizePhone(phone)
	if len(phone) < 7 {
		return phone
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}
