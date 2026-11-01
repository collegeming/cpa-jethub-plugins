package reg

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
)

// huaweiRegister drives the registerui REST chain that the browser sign-up
// SPA uses: check_identity for availability, geetest/init for the captcha
// session, send_verify_code, then portal_register with the SMS code.
// Request shapes mirror the live SPA bundle (Ro/sendSmsCode classes).
type huaweiRegister struct {
	doer doer
}

// registerHeaders mirrors the SPA's request layer (jo.setHeaders): the
// X-REG-SITE-TYPE site tag is mandatory — the backend rejects requests whose
// phone country code does not match the declared site with
// "CountryCode must match areaCode".
var registerHeaders = http.Header{
	"Accept":          []string{"application/json, text/plain, */*"},
	"Content-Type":    []string{"application/json;charset=UTF-8"},
	"Origin":          []string{"https://reg.huaweicloud.com"},
	"Referer":         []string{"https://reg.huaweicloud.com/registerui/cn/index.html"},
	"User-Agent":      []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
	"Pragma":          []string{"no-store"},
	"Cache-Control":   []string{"no-store"},
	"X-Reg-Site-Type": []string{"CN"},
	"X-Language":      []string{"zh-cn"},
}

// clientInfo mirrors getRiskInfo(): devId is the cbc-sid cookie value and
// hwMeta the fingerprint element value — both legitimately empty strings when
// the browser SDK has not produced them yet.
func clientInfo() map[string]string {
	return map[string]string{"devId": "", "hwMeta": ""}
}

// regError is a failed Huawei API envelope carrying the upstream message.
type regError struct{ Stage string }

func (e *regError) Error() string { return e.Stage }

// post issues one JSON call against the registerui service and returns the
// decoded envelope. `error_code` is the protocol's failure marker — HTTP is
// 200 for failures too, except validation rejects which are HTTP 400 with the
// same envelope shape.
func (r *huaweiRegister) post(path string, body map[string]any) (map[string]any, error) {
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal %s: %v", path, errMarshal)
	}
	fullURL := RegAPIBase + path
	response, errDo := r.doer(http.MethodPost, fullURL, registerHeaders.Clone(), raw)
	if errDo != nil {
		return nil, fmt.Errorf("POST %s: %v", path, errDo)
	}
	var envelope map[string]any
	if errDecode := json.Unmarshal(response.Body, &envelope); errDecode != nil {
		return nil, fmt.Errorf("POST %s HTTP %d: %s", path, response.StatusCode, truncate(string(response.Body), 200))
	}
	if code, _ := envelope["error_code"].(string); code != "" && code != "CBC.0000" {
		msg, _ := envelope["error_msg"].(string)
		return nil, &regError{Stage: fmt.Sprintf("%s 失败: %s %s", path, code, truncate(msg, 160))}
	}
	return envelope, nil
}

// identityResult is the SPA's check_identity outcome. identityUsed alone is
// not a hard stop: Huawei's new-portal register treats USED (can still SMS /
// login or continue-register) differently from FULL_USED (canUse=false).
// onlyHwc mirrors the SPA: identityUsed && !hwidFlag — those numbers share
// the registerui login ticket path (get-account-info + customer/ticket/quick_login).
type identityResult struct {
	LoginID      string
	CanUse       bool
	IdentityUsed bool
	HWIDFlag     bool
	OnlyHWC      bool
	Reason       string
}

func flagTrue(envelope map[string]any, key string) bool {
	value, _ := envelope[key].(string)
	return value == "true"
}

// checkIdentity probes phone availability. Verified against the live API:
// identityValue is "<dial>-<number>" and areaCode carries the site region.
// Callers branch on IdentityUsed / CanUse — this method does not abort on USED.
func (r *huaweiRegister) checkIdentity(phone string) (*identityResult, error) {
	envelope, errPost := r.post("/account/check_identity", map[string]any{
		"identityType":  "MOBILE_PHONE",
		"identityValue": "0086-" + phone,
		"scenceType":    "new_portal_register",
		"areaCode":      "CN",
		"clientInfo":    clientInfo(),
	})
	if errPost != nil {
		return nil, errPost
	}
	used := flagTrue(envelope, "identityUsed")
	hwid := flagTrue(envelope, "hwidFlag")
	result := &identityResult{
		LoginID:      stringField(envelope, "loginId"),
		CanUse:       flagTrue(envelope, "canUse"),
		IdentityUsed: used,
		HWIDFlag:     hwid,
		OnlyHWC:      used && !hwid,
		Reason:       stringField(envelope, "reason"),
	}
	return result, nil
}

// existingAccount is one Huawei Cloud account bound to a phone (get-account-info).
type existingAccount struct {
	CustomerID   string
	CustomerName string
	LastLogin    float64
}

// getAccountInfoList lists accounts for a phone after the register SMS code is
// verified. Mirrors Account.getAccountInfoList: locateType "6" (PHONE_NUMBER),
// areacode "0086", and the geetest sessionId carried as sso_ticket.
func (r *huaweiRegister) getAccountInfoList(sessionID, phone, code string) ([]existingAccount, error) {
	envelope, errPost := r.post("/account/get-account-info", map[string]any{
		"locate_type":       "6",
		"locate_id":         phone,
		"areacode":          "0086",
		"sso_ticket":        sessionID,
		"verification_code": code,
		"offset":            0,
		"limit":             200,
		"client_info":       map[string]string{"dev_id": "", "hw_meta": ""},
	})
	if errPost != nil {
		return nil, errPost
	}
	if nested, _ := envelope["error_code"].(string); nested != "" && nested != "CBC.0000" {
		msg, _ := envelope["error_msg"].(string)
		return nil, fmt.Errorf("查询账号失败: %s %s", nested, truncate(msg, 120))
	}
	rawList, _ := envelope["account_infos"].([]any)
	out := make([]existingAccount, 0, len(rawList))
	for _, item := range rawList {
		raw, _ := item.(map[string]any)
		if raw == nil {
			continue
		}
		acc := existingAccount{
			CustomerID:   stringField(raw, "customer_id"),
			CustomerName: stringField(raw, "customer_name"),
		}
		switch typed := raw["last_login_time"].(type) {
		case float64:
			acc.LastLogin = typed
		case int64:
			acc.LastLogin = float64(typed)
		}
		if acc.CustomerID != "" {
			out = append(out, acc)
		}
	}
	return out, nil
}

// pickAccount chooses the account the SPA would preselect: most recent login,
// else the first entry.
func pickAccount(accounts []existingAccount) (existingAccount, bool) {
	if len(accounts) == 0 {
		return existingAccount{}, false
	}
	best := accounts[0]
	for _, acc := range accounts[1:] {
		if acc.LastLogin > best.LastLogin {
			best = acc
		}
	}
	return best, true
}

// quickLoginTicket mints the shared registerui login ticket (biz_type=
// quick_login). Body shape matches Mi: customer_id + session_id +
// verification_code. The SPA redeems this ticket at authui/casLogin.
func (r *huaweiRegister) quickLoginTicket(sessionID, customerID, code string) (string, error) {
	envelope, errPost := r.post("/customer/ticket/quick_login", map[string]any{
		"customer_id":       customerID,
		"session_id":        sessionID,
		"verification_code": code,
	})
	if errPost != nil {
		return "", errPost
	}
	ticket := stringField(envelope, "ticket")
	if ticket == "" {
		return "", fmt.Errorf("quick_login 未返回 ticket: %s", truncate(fmt.Sprint(envelope), 200))
	}
	return ticket, nil
}

// geetestInit warms the captcha session; send_verify_code binds to its
// sessionId. The page tolerates init failure ("自动跳过验证"), so a failure
// here yields an empty session rather than aborting the run.
func (r *huaweiRegister) geetestInit() string {
	envelope, errPost := r.post("/util/geetest/init", map[string]any{
		"clientType": "web",
	})
	if errPost != nil {
		return ""
	}
	sessionID, _ := envelope["sessionId"].(string)
	return sessionID
}

// checkIdentityRisk is the risk-control call the SPA makes after geetest/init
// and before send_verify_code: check_identity with identityType CAPTCHA_CODE
// bound to the geetest sessionId. Verified live — omitting it makes
// send_verify_code answer "CBC.0150 operation illegal".
func (r *huaweiRegister) checkIdentityRisk(sessionID, phone string) error {
	_, errPost := r.post("/account/check_identity", map[string]any{
		"sessionId":     sessionID,
		"identityValue": "0086-" + phone,
		"areaCode":      "CN",
		"identityType":  "CAPTCHA_CODE",
		"scenceType":    "new_portal_register",
		"clientInfo":    clientInfo(),
	})
	return errPost
}

// sendVerifyCode triggers the registration SMS. Field names mirror the SPA's
// sendSmsCode payload verbatim (camelCase, scenceType upper snake).
func (r *huaweiRegister) sendVerifyCode(sessionID, phone string) error {
	_, errPost := r.post("/util/send_verify_code", map[string]any{
		"sessionId":   sessionID,
		"type":        1,
		"countryCode": "0086",
		"mobilePhone": phone,
		"captchaType": "GRAPHIC",
		"scenceType":  "NEW_PORTAL_REGISTER",
		"areaCode":    "CN",
		"clientInfo":  clientInfo(),
	})
	return errPost
}

// agreementVersions are the privacy/customer agreement IDs the CN site accepts;
// batchQueryAgreement returns the same pair so they are stable across releases.
var agreementVersions = []map[string]string{
	{"agt_type": "PRIVACY_STATEMENT", "agt_id": "a2591e21dda54bcf922e7d29055b6abf"},
	{"agt_type": "CUSTOMER_AGREEMENT", "agt_id": "fc3c0f12c6434700a75dbcaacce3e28c"},
}

// accountInfo is the created account block returned inside the register
// response. Ticket is the auto-login ticket redeemable at authui/thirdLogin.
// Password is the generated console password — persisted alongside the
// credential so the account can be re-authenticated after its refresh token
// dies without needing the SMS number again.
type accountInfo struct {
	DomainID   string `json:"domain_id"`
	DomainName string `json:"domain_name"`
	Ticket     string `json:"ticket"`
	UserID     string `json:"user_id"`
	Password   string `json:"-"`
	// LoginViaCAS is set when Ticket came from customer/ticket/quick_login —
	// those redeem at authui/casLogin rather than authui/thirdLogin.
	LoginViaCAS bool `json:"-"`
}

// registerQuick submits the SMS code and creates the account. Verified live:
// the working endpoint is /account/portal/register/quick with snake_case
// fields — country_code carries the region code "CN" (not the dial code), and
// reg_account uses "<dial>-<number>". The CN SPA sends no invite field on this
// path; referral binding happens post-registration via ops/invite/confirm.
// The browser-style /account/portal_register endpoint answers 401 without
// extra session material, so it is not used.
func (r *huaweiRegister) registerQuick(sessionID, phone, code string) (*accountInfo, error) {
	password := randomPassword()
	body := map[string]any{
		"session_id":        sessionID,
		"country_code":      "CN",
		"reg_type":          0,
		"reg_account":       "0086-" + phone,
		"verification_code": code,
		"password":          password,
		"allow_notify":      1,
		"agt_vers":          agreementVersions,
		"client_info":       map[string]string{"dev_id": "", "hw_meta": ""},
	}
	envelope, errPost := r.post("/account/portal/register/quick", body)
	if errPost != nil {
		return nil, errPost
	}
	raw, _ := envelope["account_info"].(map[string]any)
	if len(raw) == 0 {
		return nil, fmt.Errorf("注册响应缺少 account_info: %s", truncate(fmt.Sprint(envelope), 200))
	}
	info := &accountInfo{
		DomainID:   stringField(raw, "domain_id"),
		DomainName: stringField(raw, "domain_name"),
		Ticket:     stringField(raw, "ticket"),
		UserID:     stringField(raw, "user_id"),
		Password:   password,
	}
	return info, nil
}

// stringField pulls a string out of a loosely-typed decoded object.
func stringField(raw map[string]any, key string) string {
	value, _ := raw[key].(string)
	return value
}

// randomPassword mints a compliant secret (8-32 chars, upper+lower+digit
// classes present) for the backend-generated account.
func randomPassword() string {
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const lower = "abcdefghijkmnpqrstuvwxyz"
	const digits = "23456789"
	all := upper + lower + digits
	out := make([]byte, 0, 16)
	pick := func(set string) byte {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		return set[n.Int64()]
	}
	out = append(out, pick(upper), pick(lower), pick(digits))
	for len(out) < 16 {
		out = append(out, pick(all))
	}
	return string(out)
}

// truncate bounds s for inclusion in logs and error strings.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
