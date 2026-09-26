package main

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider identity. One plugin instance owns exactly one provider key.
const (
	// ProviderKey is the stable provider identifier written into CPA auth files
	// and matched by the account pool (`loomy-adapter.ts:45`,
	// `loomy-product.ts:89`).
	ProviderKey = "loomy"
	// DisplayName is the human-readable name shown by management clients
	// (`loomy-product.ts:90`).
	DisplayName = "Loomy (讯飞)"
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
	// Logo stays empty: the Loomy desktop client publishes no favicon, and an
	// invented URL would render as a broken image in the manager.
	// The icon the Loomy site itself links. An empty logo renders nothing in
	// the panel, which is what every plugin looked like before this was set.
	Logo = "https://loomy.xunfei.cn/icon.png?5f7e7b8be5d4c76b"
)

// Hosts and paths. Both are PRODUCTION addresses and the source explicitly
// forbids switching to a test environment (`README.md:1386`).
const (
	// APIBase serves models, points, onboarding and chat completions
	// (`loomy.ts:30`).
	APIBase = "https://loomyad.xunfei.cn/api/v1"

	// ModelsPath is the catalogue listing (`index.ts:463-481`).
	ModelsPath = "/models"
	// ChatCompletionsPath is the OpenAI-compatible streaming endpoint
	// (`loomy-adapter.ts:331-337`).
	ChatCompletionsPath = "/chat/completions"
	// PointsRecordsPath is the read-only balance endpoint (`loomy-credits.ts:128-132`).
	PointsRecordsPath = "/points/records"
	// PointsFirstLoginPath is the WRITE endpoint that initialises the daily pool
	// (`loomy-credits.ts:213-215`). Never call it from a read path.
	PointsFirstLoginPath = "/points/first-login"
	// OnboardingTasksPath lists the one-off task state (`loomy-onboarding.ts:167-169`).
	OnboardingTasksPath = "/onboarding/tasks"
	// OnboardingCompletePath completes one task (`loomy-onboarding.ts:193-195`).
	OnboardingCompletePath = "/onboarding/tasks/complete"

	// SendMsgCodePath requests an SMS code (`loomy-oauth.ts:13`).
	SendMsgCodePath = "/login/phone/sendMsgCode"
	// CheckCodePath exchanges the SMS code for a session (`loomy-oauth.ts:14`).
	CheckCodePath = "/login/phone/checkCode"
)

// iFlytek CAccount client envelope constants, sent in the `base` object of every
// account request (`loomy-oauth.ts:55-71`).
const (
	// AccountAppID is `base.appid` (`loomy-product.ts:96`, `loomy-oauth.ts:61`).
	AccountAppID = "GM3LOOMY"
	// AccountModelID is `base.modelid` (`loomy-oauth.ts:62`).
	AccountModelID = "Web"
	// AccountClientVersion is `base.version` (`loomy-oauth.ts:63`).
	AccountClientVersion = "1.0.0"
	// AccountDeviceID is `base.devid` (`loomy-oauth.ts:64`).
	AccountDeviceID = "web"
	// AccountUA is `base.ua`. It is hard-coded to macOS even on Windows and must
	// NOT be "fixed": the Windows client sends the same value
	// (`loomy-oauth.ts:65-66`, trap #25).
	AccountUA = "Loomy|Desktop|Electron|macOS"
)

// Timeouts and lifetimes, each the value the TypeScript source uses.
const (
	// RequestTimeoutMS is the business/account request timeout,
	// `LOOMY_REQUEST_TIMEOUT_MS` (`loomy.ts:45`).
	RequestTimeoutMS = 60_000
	// ModelsTimeoutMS is the `/models` fetch timeout (`index.ts:477`).
	ModelsTimeoutMS = 30_000
	// SMSCodeTTLSeconds is the SMS validity DECLARED to the server
	// (`loomy-oauth.ts:30,139`). The server does not echo it back.
	SMSCodeTTLSeconds = 300
	// SessionTTLSeconds is the session lifetime DECLARED to the server
	// (`loomy-oauth.ts:39`, `README.md:1397`). The response carries no expiry, so
	// `expires_at` is computed locally from this value
	// (`loomy-auth.ts:272`).
	SessionTTLSeconds = 1_209_600
	// LoginTimeoutMS bounds one interactive login session. The SMS path has no
	// documented timeout in the source; the WeChat flow's whole-login budget
	// (`LOOMY_WECHAT_LOGIN_TIMEOUT_MS`, `loomy-wechat-login.ts:70`) is reused so a
	// half-finished session cannot sit in memory forever.
	LoginTimeoutMS = 300_000
	// ModelCacheTTLMS bounds how long a discovered catalogue is reused.
	ModelCacheTTLMS = 2 * 60 * 60 * 1000
	// CredentialHealthIntervalMS is the credential probe cadence the reference
	// uses (every 30 min, `index.ts:490`). Surfaced as metadata only; the host
	// owns the schedule.
	CredentialHealthIntervalMS = 30 * 60 * 1000
)

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
type Config struct {
	Enabled  bool
	Priority int

	// Phone is the default mobile number the management login page offers. The
	// page's actions are GET links (the resource mount is GET-only), so the
	// number has to come from somewhere other than a form field; a user who
	// wants another number passes `?phone=` in the link instead.
	Phone string

	// AccountAK / AccountSK optionally override the client-distributed signing
	// credentials. Leave empty to use the embedded pair.
	AccountAK string
	AccountSK string

	// SMSCodeTTLSeconds / SessionTTLSeconds are declared to the account host and
	// the latter also defines the locally computed `expires_at`.
	SMSCodeTTLSeconds int
	SessionTTLSeconds int
	// LoginTimeoutMS bounds one interactive login session.
	LoginTimeoutMS int
	// RequestTimeoutMS bounds a single buffered upstream call.
	RequestTimeoutMS int

	// DiscoverModels enables the live `GET /models` catalogue. It is off by
	// default so `model.*` never touches the network when no account exists
	// (`loomy-adapter.ts:198-200`).
	DiscoverModels bool
	// ModelCacheTTLMS bounds the discovered catalogue lifetime.
	ModelCacheTTLMS int
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:           true,
		SMSCodeTTLSeconds: SMSCodeTTLSeconds,
		SessionTTLSeconds: SessionTTLSeconds,
		LoginTimeoutMS:    LoginTimeoutMS,
		RequestTimeoutMS:  RequestTimeoutMS,
		ModelCacheTTLMS:   ModelCacheTTLMS,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is first decoded into a generic mapping and every key is then
// coerced individually. Three properties follow, all of which matter in practice:
//
//   - block AND flow style are both accepted, because the host hands the instance
//     subtree back in whichever style the user wrote it. A block-only line
//     scanner silently ignores `{discover_models: true, phone: 13800138000}` and
//     falls back to every default without reporting anything;
//   - values YAML 1.2 sees as strings but a user reasonably writes as booleans
//     (`yes`/`no`/`on`/`off`, or a quoted number) still work. yaml.v3 is YAML 1.2,
//     so a typed decode into `bool` would fail on `no` and discard the whole
//     document;
//   - one unusable value costs only its own default instead of the whole file.
func ConfigFromYAML(document []byte) Config {
	cfg := DefaultConfig()
	if len(document) == 0 {
		return cfg
	}
	var raw map[string]any
	if errUnmarshal := yaml.Unmarshal(document, &raw); errUnmarshal != nil {
		return DefaultConfig()
	}

	cfg.Enabled = coerceBool(raw["enabled"], cfg.Enabled)
	cfg.Priority = coerceInt(raw["priority"], cfg.Priority)
	cfg.Phone = coercePhone(raw["phone"], cfg.Phone)
	cfg.AccountAK = coerceString(raw["account_ak"], cfg.AccountAK)
	cfg.AccountSK = coerceString(raw["account_sk"], cfg.AccountSK)
	cfg.SMSCodeTTLSeconds = coerceInt(raw["sms_code_ttl_seconds"], cfg.SMSCodeTTLSeconds)
	cfg.SessionTTLSeconds = coerceInt(raw["session_ttl_seconds"], cfg.SessionTTLSeconds)
	cfg.LoginTimeoutMS = coerceInt(raw["login_timeout_ms"], cfg.LoginTimeoutMS)
	cfg.RequestTimeoutMS = coerceInt(raw["request_timeout_ms"], cfg.RequestTimeoutMS)
	cfg.DiscoverModels = coerceBool(raw["discover_models"], cfg.DiscoverModels)
	cfg.ModelCacheTTLMS = coerceInt(raw["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	return cfg
}

// coercePhone accepts a phone number written as a number or a string. A YAML
// number loses nothing here because the number is stored as text everywhere else
// (the account host wants an 11-digit string).
func coercePhone(value any, fallback string) string {
	switch typed := value.(type) {
	case nil:
		return fallback
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return coerceString(value, fallback)
	}
}

// coerceBool accepts a real boolean, a number, or the string spellings users
// write in configuration files.
func coerceBool(value any, fallback bool) bool {
	switch typed := value.(type) {
	case nil:
		return fallback
	case bool:
		return typed
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "yes", "on", "1":
			return true
		case "false", "no", "off", "0":
			return false
		}
		return fallback
	default:
		return fallback
	}
}

// coerceInt accepts a number or a numeric string, including a quoted one.
func coerceInt(value any, fallback int) int {
	switch typed := value.(type) {
	case nil:
		return fallback
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		parsed, errParse := strconv.Atoi(strings.TrimSpace(typed))
		if errParse != nil {
			return fallback
		}
		return parsed
	default:
		return fallback
	}
}

// coerceString returns a non-empty trimmed string, else the fallback.
func coerceString(value any, fallback string) string {
	if text, ok := value.(string); ok {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

// ConfigFields describes the settings the CPA management UI renders.
func ConfigFields() []configField {
	return []configField{
		{Name: "phone", Type: "string",
			Description: "登录页默认手机号（11 位，如 13800138000）。管理页只以 GET 链接派发，没有表单输入框，因此默认号来自这里；也可以在链接里用 ?phone= 覆盖"},
		{Name: "discover_models", Type: "boolean",
			Description: "是否用账号会话实时拉取 GET /models 目录（默认关闭）。关闭时只使用内置的 8 个兜底模型"},
		{Name: "model_cache_ttl_ms", Type: "integer", Description: "实时模型目录的缓存时长，毫秒（默认 2 小时）"},
		{Name: "sms_code_ttl_seconds", Type: "integer", Description: "向讯飞账号服务声明的短信验证码有效期，秒（默认 300）"},
		{Name: "session_ttl_seconds", Type: "integer",
			Description: "向讯飞账号服务声明的会话有效期，秒（默认 1209600 = 14 天）。服务端不回传过期时间，本地 expires_at 由该值推算"},
		{Name: "login_timeout_ms", Type: "integer", Description: "单次交互式登录会话的存活时长，毫秒（默认 5 分钟）"},
		{Name: "request_timeout_ms", Type: "integer", Description: "单次上游请求超时，毫秒（默认 60000）"},
		{Name: "account_ak", Type: "string",
			Description: "讯飞账号服务签名用的 accessKeyId。留空使用内置值（来自 Loomy 桌面客户端）；上游轮换后可在不改插件的情况下覆盖"},
		{Name: "account_sk", Type: "string",
			Description: "讯飞账号服务签名用的 accessKeySecret。留空使用内置值；仅在 account_ak 同时填写时有意义"},
	}
}

// configField mirrors pluginapi.ConfigField; the plugin package owns its own
// type so the ABI layer stays free of provider specifics.
type configField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}

// loginSessionTTL is the configured interactive-login lifetime.
func (c Config) loginSessionTTL() int {
	if c.LoginTimeoutMS > 0 {
		return c.LoginTimeoutMS
	}
	return LoginTimeoutMS
}

// smsCodeTTL is the SMS validity declared to the account host.
func (c Config) smsCodeTTL() int {
	if c.SMSCodeTTLSeconds > 0 {
		return c.SMSCodeTTLSeconds
	}
	return SMSCodeTTLSeconds
}

// sessionTTL is the session lifetime declared to the account host and used to
// derive the local `expires_at`.
func (c Config) sessionTTL() int {
	if c.SessionTTLSeconds > 0 {
		return c.SessionTTLSeconds
	}
	return SessionTTLSeconds
}

// requestTimeout is the configured single-request timeout.
func (c Config) requestTimeout() int {
	if c.RequestTimeoutMS > 0 {
		return c.RequestTimeoutMS
	}
	return RequestTimeoutMS
}

// modelCacheTTL is the configured discovered-catalogue lifetime.
func (c Config) modelCacheTTL() int {
	if c.ModelCacheTTLMS > 0 {
		return c.ModelCacheTTLMS
	}
	return ModelCacheTTLMS
}

// defaultPhone resolves the phone number a login link should use: the explicit
// query value wins, otherwise the configured default.
func (c Config) defaultPhone(override string) string {
	if trimmed := normalizePhone(override); trimmed != "" {
		return trimmed
	}
	return normalizePhone(c.Phone)
}
