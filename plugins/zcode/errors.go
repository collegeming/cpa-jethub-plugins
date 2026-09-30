package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Upstream failure classification.
//
// The host reads `http_status` off the failure envelope (it becomes
// `rpcError.statusCode`, which `clienterror.HTTPStatusFromError` and
// `IsRequestFault` consume) to decide whether a failure is the REQUEST's fault —
// which drives credential rotation and the status code returned to the client.
// The envelope `code` is only consulted for a couple of dispatch cases, so a
// failure that carries no status is effectively unclassified.
//
// The mapping used throughout this plugin:
//
//	credential missing / unparseable / dead                        401
//	malformed request, unusable model                              400
//	quota exhausted (`1005` / `1113`)                              402
//	concurrency limited (`3009`), other rate limits                429
//	risk control (`3012`) — NOT an auth failure, see below         403
//	captcha failure (`3007`) — retryable                           429
//	upstream 5xx, transport failure                                502
//	upstream timeout                                               504
//	plugin-side misconfiguration                                   500
//
// TWO codes deserve a warning rather than a table entry:
//
//   - `3012` (`request has been blocked due to unusual activity`) is the
//     risk-control gate. It is raised when the request body's identity block is
//     absent or malformed. It carries an ACCOUNT PENALTY — 30 minutes, then 24
//     hours from the third occurrence within 24 hours, and disablement on the
//     fifth — so it must NEVER be retried automatically and must never be shown
//     as "your credential expired". The reference maps it to a permission error
//     for exactly this reason (`zcode-adapter.ts:httpErrorCodeForZcode`).
//   - `3007` (captcha verification failed) is retryable in principle, but the
//     plugin does not mint captchas: the header was measured to be unnecessary
//     for inference, so a `3007` here means the server changed its mind. It is
//     surfaced as a retryable rate limit with a message that says so.

// Upstream business codes.
const (
	// codeParameterError is `{"code":3001,"msg":"parameter error"}`. It is what a
	// missing `X-Device-Mid` produces, which makes it a client-shape bug rather
	// than a credential problem.
	codeParameterError = 3001
	// codeCaptchaFailed is the Aliyun captcha rejection.
	codeCaptchaFailed = 3007
	// codeConcurrencyLimit is `model concurrency limit exceeded`. It shares HTTP
	// 429 with the quota code, so only the body distinguishes them.
	codeConcurrencyLimit = 3009
	// codeRiskControl is `request has been blocked due to unusual activity`.
	codeRiskControl = 3012
	// codeQuotaExceeded is `exceed quota limit`.
	codeQuotaExceeded = 1005
	// codeInsufficientBalance is the ultra/coding-plan "余额不足或无可用资源包".
	codeInsufficientBalance = 1113
	// codeAlreadyClaimed is the idempotent claim success. It is NOT an error:
	// treating it as one makes a scheduled check-in report a false failure.
	codeAlreadyClaimed = 1003
	// codePlanGone / codePlanEnded / codeNotEligible / codePlanSoldOut are the
	// documented claim rejections (`zcode-upstream.ts:claimZcodePlan`).
	codePlanGone     = 1001
	codePlanEnded    = 1002
	codeNotEligible  = 1004
	codePlanSoldOut  = 1005
	codeClaimSuccess = 0
)

// statusError builds a classified failure, keeping the retry hint for hosts that
// still look at it.
func statusError(retryable bool, code string, status int, format string, args ...any) *abiboot.EnvelopeError {
	return &abiboot.EnvelopeError{
		Code:       code,
		Message:    fmt.Sprintf(format, args...),
		Retryable:  retryable,
		HTTPStatus: status,
	}
}

// credentialError marks a credential as unusable so the host can rotate it.
func credentialError(code, format string, args ...any) *abiboot.EnvelopeError {
	return statusError(false, code, http.StatusUnauthorized, format, args...)
}

// transportError marks a network or upstream failure.
func transportError(code, format string, args ...any) *abiboot.EnvelopeError {
	return statusError(true, code, http.StatusBadGateway, format, args...)
}

// requestError marks the caller's request as unusable.
func requestError(code, format string, args ...any) *abiboot.EnvelopeError {
	return statusError(false, code, http.StatusBadRequest, format, args...)
}

// statusOf reads the HTTP status a failure carries, falling back to the given
// value when the error is unclassified or not a plugin failure.
func statusOf(err error, fallback int) int {
	var envelope *abiboot.EnvelopeError
	if errors.As(err, &envelope) && envelope != nil && envelope.HTTPStatus != 0 {
		return envelope.HTTPStatus
	}
	return fallback
}

// upstreamEnvelope is the parsed business envelope every ZCode endpoint shares.
//
// Success is `code == 0` and nothing else: business failures arrive with HTTP
// 200, so a status-code check reads a rejected request as a successful one.
type upstreamEnvelope struct {
	Code    flexCode        `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	// Raw keeps the undecoded body text so a non-JSON answer (an HTML gateway
	// page) can still contribute a reason to an error message.
	Raw []byte `json:"-"`
}

// flexCode decodes a `code` that may arrive as a number or as a numeric string,
// and remembers whether it was present at all. An ABSENT code inherits the HTTP
// status, while a present `0` is success — the two are not interchangeable.
type flexCode struct {
	value   int
	present bool
}

// UnmarshalJSON accepts a number, a numeric string, or null.
func (c *flexCode) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if errUnmarshal := json.Unmarshal(data, &text); errUnmarshal != nil {
			return errUnmarshal
		}
		trimmed = strings.TrimSpace(text)
	}
	parsed, errParse := strconv.Atoi(trimmed)
	if errParse != nil {
		// A non-numeric code is kept as "present but unparsable" by storing a
		// sentinel that can never equal success.
		c.present = true
		c.value = -1
		return nil
	}
	c.value = parsed
	c.present = true
	return nil
}

// parseUpstreamEnvelope decodes a response body into the shared envelope.
//
// A body that is not a JSON object (an HTML gateway page, a bare string) keeps
// the raw text so an error message can still carry a reason, and its code stays
// absent so the HTTP status becomes the code.
func parseUpstreamEnvelope(body []byte, status int) *upstreamEnvelope {
	trimmed := strings.TrimSpace(string(body))
	envelope := &upstreamEnvelope{Raw: []byte(trimmed)}
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		if status >= 400 {
			envelope.Code = flexCode{value: status, present: true}
		}
		return envelope
	}
	if errUnmarshal := json.Unmarshal([]byte(trimmed), envelope); errUnmarshal != nil {
		if status >= 400 {
			envelope.Code = flexCode{value: status, present: true}
		}
		return envelope
	}
	if !envelope.Code.present && status >= 400 {
		envelope.Code = flexCode{value: status, present: true}
	}
	return envelope
}

// message renders the human-readable reason the server sent.
func (e *upstreamEnvelope) message(fallback string) string {
	if e != nil {
		for _, candidate := range []string{e.Msg, e.Message} {
			if trimmed := strings.TrimSpace(candidate); trimmed != "" {
				return trimmed
			}
		}
		if len(e.Raw) > 0 && !strings.HasPrefix(strings.TrimSpace(string(e.Raw)), "{") {
			return truncate(string(e.Raw), 200)
		}
	}
	return fallback
}

// success reports the ONLY success condition: a present code equal to zero.
func (e *upstreamEnvelope) success() bool {
	return e != nil && e.Code.present && e.Code.value == codeClaimSuccess
}

// isQuotaExhausted recognises the two deterministic "no quota left" codes.
//
// ⚠ `3009` must be excluded FIRST. It shares HTTP 429 with `1005`, and treating a
// concurrency rejection as "this account is out of quota for the day" wrongly
// retires a perfectly usable credential.
func isQuotaExhausted(status int, envelope *upstreamEnvelope) bool {
	if isConcurrencyLimited(status, envelope) {
		return false
	}
	if envelope == nil {
		return false
	}
	switch envelope.Code.value {
	case codeQuotaExceeded, codeInsufficientBalance:
		return envelope.Code.present
	}
	// Wording fallback: the numeric codes are the server's to change, so a body
	// that says the same thing in words is still recognised. The patterns must stay
	// NARROW — a generic word such as "quota" also appears in a model's own answer
	// about quotas, and matching that would turn a normal completion into a quota
	// failure.
	if quotaTextPattern.Match(envelope.Raw) {
		return true
	}
	// A gateway that strips the envelope entirely still expresses the same thing
	// through the status, but only 402 says it unambiguously — a bare 429 is far
	// more likely to be a rate limit.
	return status == http.StatusPaymentRequired
}

// quotaTextPattern matches the quota wording used when the numeric code is not one
// we know.
var quotaTextPattern = regexp.MustCompile(
	`(?i)exceed\s+quota\s+limit|quota\s+(?:has\s+been\s+)?exhausted|余额不足`)

// isConcurrencyLimited recognises `3009`, which shares HTTP 429 with the quota
// code and therefore has to be told apart by the body.
func isConcurrencyLimited(status int, envelope *upstreamEnvelope) bool {
	if envelope != nil && envelope.Code.present && envelope.Code.value == codeConcurrencyLimit {
		return true
	}
	if status != http.StatusTooManyRequests || envelope == nil {
		return false
	}
	return concurrencyTextPattern.Match(envelope.Raw)
}

// concurrencyTextPattern is the wording fallback for a server that changes the
// numeric code while keeping the meaning. It must stay NARROW: a generic word
// such as "limit" also appears in quota messages.
var concurrencyTextPattern = regexp.MustCompile(`(?i)concurrency\s+limit`)

// captchaTextPattern matches the captcha wording used when the numeric code is
// not the one we know.
var captchaTextPattern = regexp.MustCompile(`(?i)captcha`)

// riskControlTextPattern matches the risk-control wording.
var riskControlTextPattern = regexp.MustCompile(`(?i)unusual\s+activity|blocked\s+due\s+to`)

// describeUpstreamError renders one upstream failure as an actionable sentence.
//
// The wording matters more here than elsewhere because these codes are not
// interchangeable to the user: `3009` means "wait a moment", `1005` means "this
// account is done for the day", and `3012` means "stop immediately, retrying
// costs you the account".
func describeUpstreamError(status int, envelope *upstreamEnvelope) string {
	code := 0
	if envelope != nil && envelope.Code.present {
		code = envelope.Code.value
	}
	detail := envelope.message("")
	suffix := ""
	if detail != "" {
		suffix = "；上游原文：" + detail
	}
	switch {
	case isConcurrencyLimited(status, envelope):
		return "上游并发限流（3009 model concurrency limit exceeded）——" +
			"该模型的对这个账号的并发窗口已满，稍后重试即可，不要更换账号" + suffix
	case isQuotaExhausted(status, envelope):
		return "额度已用尽（1005/1113）——该账号在这个模型上的免费额度已经用完，" +
			"请等待额度按自然日重置、改用其它模型，或在状态页添加账号" + suffix
	case code == codeRiskControl || riskControlTextPattern.MatchString(bodyText(envelope)):
		return "上游风控拦截（3012 request has been blocked due to unusual activity）。" +
			"⚠ 该错误有账号冷却惩罚（首次 30 分钟；24 小时内第 3 次起 24 小时；第 5 次停用账号），" +
			"因此本插件不会自动重试。若反复出现，请检查身份块是否被中间层改写" + suffix
	case code == codeCaptchaFailed || captchaTextPattern.MatchString(bodyText(envelope)):
		return "上游要求人机验证（3007 captcha verify failed）。" +
			"实测推理通道并不需要 captcha 头，出现它说明上游策略已变；请重试一次，若持续出现请反馈" + suffix
	case code == codeParameterError:
		return "上游拒绝请求参数（3001 parameter error）——" +
			"通常是缺少 X-Device-Mid，或登录流程少了必需参数（该参数的值不校验，但不能缺）" + suffix
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "凭据失效（HTTP " + strconv.Itoa(status) + "）——请在状态页重新登录该账号" + suffix
	case status == http.StatusTooManyRequests:
		return "上游限流（HTTP 429）" + suffix
	case status >= 500:
		return "上游服务异常（HTTP " + strconv.Itoa(status) + "）" + suffix
	default:
		if detail != "" {
			return "HTTP " + strconv.Itoa(status) + "：" + detail
		}
		return "HTTP " + strconv.Itoa(status)
	}
}

// bodyText returns the raw body text of an envelope, for the wording fallbacks.
func bodyText(envelope *upstreamEnvelope) string {
	if envelope == nil {
		return ""
	}
	return string(envelope.Raw)
}

// classifyUpstreamError turns one non-2xx upstream answer into a classified
// failure with an HTTP status the host can act on.
func classifyUpstreamError(status int, body []byte) *abiboot.EnvelopeError {
	envelope := parseUpstreamEnvelope(body, status)
	message := describeUpstreamError(status, envelope)

	switch {
	case isConcurrencyLimited(status, envelope):
		// Retryable, but NOT a credential fault: rotating the account here would
		// retire a usable credential for a server-side window.
		return statusError(true, "concurrency_limited", http.StatusTooManyRequests, "%s", message)
	case isQuotaExhausted(status, envelope):
		// Deterministic. 402 makes the host rotate the credential instead of
		// backing off and retrying the same exhausted one.
		return statusError(false, "quota_exhausted", http.StatusPaymentRequired, "%s", message)
	case envelope.Code.present && envelope.Code.value == codeRiskControl:
		return statusError(false, "risk_control", http.StatusForbidden, "%s", message)
	case envelope.Code.present && envelope.Code.value == codeCaptchaFailed:
		return statusError(true, "captcha_required", http.StatusTooManyRequests, "%s", message)
	case envelope.Code.present && envelope.Code.value == codeParameterError:
		return requestError("parameter_error", "%s", message)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return credentialError("auth", "%s", message)
	case status == http.StatusTooManyRequests:
		return statusError(true, "rate_limited", http.StatusTooManyRequests, "%s", message)
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return statusError(true, "upstream_timeout", http.StatusGatewayTimeout, "%s", message)
	case status >= 500:
		return transportError("upstream_error", "%s", message)
	default:
		return statusError(false, "upstream_rejected", http.StatusBadGateway, "%s", message)
	}
}

// truncate shortens a diagnostic string so error messages stay readable.
func truncate(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "…"
}
