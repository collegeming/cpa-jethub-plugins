package main

import (
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider identity. This plugin owns exactly one provider key; MiniMax Code
// has a single region (`cn`) and a single client identity, so there is no
// product selector and no region setting here.
const (
	// ProviderKey is the stable provider identifier written into CPA auth files.
	ProviderKey = "minimax"
	// DisplayName is the human-readable name shown by management clients.
	DisplayName = "MiniMax Code"
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
)

// Live-verification status of the two remote paths, as a single source of truth.
//
// These are developer assertions, not runtime probes: they record what has
// actually been exercised against the real service, so an operator can tell
// "never tried" from "known to work" without reading the git log.
//
// The reference implementation could not verify its own login flow — every one
// of its probes read the desktop client's token instead — so this was carried
// here as unverified until it was exercised for real:
//
//   - LoginVerified: a device-code authorization completed against
//     account.minimax.cn and the resulting `mmoat_`/`mmort_` credential was
//     stored and then accepted by the inference endpoint. Verified 2026-10-01.
//
//   - InferenceVerified: independently re-confirmed here — MiniMax-M2.7 and
//     MiniMax-M3.1-Flash-Preview both answered HTTP 200 through CPA with
//     content, reasoning and usage.
//
//   - RefreshVerified: NOW OBSERVED, and only after the reason it was missing
//     got fixed. Two separate things were true and both had to be:
//
//     (a) the grant itself works — POST /oauth2/token with
//     `grant_type=refresh_token` returns a new `mmoat_`/`mmort_` pair and
//     `expires_in: 3600`;
//     (b) it never ran, because the stored `expires_at` was RFC3339 while the
//     reader accepted only digit strings, and an unreadable expiry counts as
//     ABSENT — which this provider deliberately treats as "not expired". The
//     credential therefore looked permanently healthy and died with a 401.
//
//     Verified 2026-10-01 by placing the credential 120 s from expiry (inside
//     the 300 s window) and issuing one inference call through CPA: the call
//     returned 200 AND the stored access token changed, i.e. renewal ran before
//     the request. A follow-up call with the new token ~1 h out did NOT rotate
//     it again, so renewal is demand-driven rather than per-request.
const (
	// LoginVerified reports that the device-code login is proven against the
	// live service.
	LoginVerified = true
	// InferenceVerified reports that streaming inference is proven against the
	// live service.
	InferenceVerified = true
	// RefreshVerified reports that token renewal is proven against the live
	// service. Verified 2026-10-01; see the note above for the exact evidence.
	RefreshVerified = true
)

// Timeouts and polling budgets, mirroring the reference constants
// (`minimax-product.ts:MINIMAX_REQUEST_TIMEOUT_MS` = 30 s and
// `MINIMAX_OAUTH_TIMEOUT_MS` = 20 s).
const (
	// RequestTimeoutMS is the single-request timeout for catalogue, sign-in and
	// credit calls.
	RequestTimeoutMS = 30_000
	// OAuthTimeoutMS bounds one OAuth call; it is deliberately shorter than a
	// business call so the user is not left waiting on a stalled login.
	OAuthTimeoutMS = 20_000
	// LoginTimeoutMS is the whole device-code wait. The reference derives it
	// from the device-code grant's own `expires_in`; the value here is the
	// budget used when the server does not publish one.
	LoginTimeoutMS = 300_000
	// PollIntervalSeconds is the default device-code poll interval. The grant
	// carries one (`interval`), and it is in SECONDS; 5 is the reference's
	// default when the server omits it (`minimax-oauth.ts:parse...`).
	PollIntervalSeconds = 5
	// SlowDownIncrementSeconds is what a `slow_down` answer adds to the poll
	// interval (`minimax-oauth.ts`, both the 200/status and the 400/error form).
	SlowDownIncrementSeconds = 5
	// PollMaxFailures is the consecutive transport-failure budget before an
	// interactive login is abandoned. The reference has no such budget (it
	// polls until the device code expires); a CPA login is driven by repeated
	// host invocations, so a bound is needed or the flow never ends.
	PollMaxFailures = 5
)

// DefaultEffortLevels is the effort ladder MiniMax publishes for
// `MiniMax-M3.1-Flash-Preview` (remote `effort_options`). It is only used by
// the FALLBACK catalogue: the remote catalogue always carries the real list.
var DefaultEffortLevels = []string{"default", "low", "medium", "high", "xhigh", "max"}

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
type Config struct {
	// Enabled is the host's own switch, mirrored for diagnostics.
	Enabled bool
	// Priority orders credentials in the host's scheduler.
	Priority int

	// DiscoverModels enables the live model catalogue. It is on by default:
	// the reference prefers the remote catalogue and treats its bundled table
	// as a degraded mode only (`minimax-auth.ts:fetchModelsWith`). The vendor's
	// desktop client ships a 3-entry static table, but this provider's measured
	// fallback deliberately carries all four exposed ids so M3.1 does not
	// disappear during a catalogue outage.
	DiscoverModels bool
	// ModelPrefix 是否把账号标识作为模型前缀暴露（<账号>/<模型>）。关闭后模型列表只显示模型本身的名字。
	ModelPrefix bool
	// ModelCacheTTLMS bounds how long a discovered catalogue is reused. The
	// reference records a remote `ttlSeconds: 300` and deliberately does NOT
	// copy it (it pulls the catalogue on demand); the default here is the same
	// 5 minutes the server advertises.
	ModelCacheTTLMS int
	// ModelRefreshMS is how often the catalogue is refetched in the background,
	// so the cache does not depend on a client request to stay fresh. 0 disables
	// the background refresh, which is the default: it costs one vendor round
	// trip per interval, and the page's 刷新目录 button covers the manual case.
	ModelRefreshMS int

	// RequestTimeoutMS bounds catalogue, sign-in and credit calls.
	RequestTimeoutMS int
	// CatalogueTimeoutMS bounds the catalogue call.
	CatalogueTimeoutMS int
	// OAuthTimeoutMS bounds one OAuth (device-code / token / refresh) call.
	OAuthTimeoutMS int
	// LoginTimeoutMS bounds one interactive device-code login session.
	LoginTimeoutMS int
	// PollIntervalSeconds is the fallback device-code poll interval, used only
	// when the device-code grant does not publish one.
	PollIntervalSeconds int
	// RefreshWindowSeconds is the pre-refresh window; see the constant comment.
	RefreshWindowSeconds int
	// DefaultMaxTokens is applied when a request omits `max_tokens`; 0 omits it.
	DefaultMaxTokens int

	// TimezoneID is the IANA zone reported to the sign-in endpoints. The
	// reference resolves it from the host (`Intl.DateTimeFormat()
	// .resolvedOptions().timeZone || "UTC"`, `minimax-credits.ts`); a Go plugin
	// has no such host API, so an empty value is reported as "UTC" unless the
	// operator sets this. The server settles the daily window on the zone the
	// client reports, so getting it wrong shifts the claim window.
	TimezoneID string
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:              true,
		DiscoverModels:       true,
		ModelPrefix:          true,
		ModelCacheTTLMS:      ModelCacheTTLMS,
		ModelRefreshMS:       0,
		RequestTimeoutMS:     RequestTimeoutMS,
		CatalogueTimeoutMS:   RequestTimeoutMS,
		OAuthTimeoutMS:       OAuthTimeoutMS,
		LoginTimeoutMS:       LoginTimeoutMS,
		PollIntervalSeconds:  PollIntervalSeconds,
		RefreshWindowSeconds: RefreshWindowSeconds,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is decoded into a generic mapping first and every key is then
// coerced on its own, for the three reasons the sibling plugins document:
// block and flow style must both work, values YAML 1.2 calls strings but a user
// writes as booleans (`yes`/`no`, a quoted number) must still work, and one
// unusable value costs only its own default instead of the whole file.
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
	cfg.DiscoverModels = coerceBool(raw["discover_models"], cfg.DiscoverModels)
	cfg.ModelPrefix = coerceBool(raw["model_prefix"], cfg.ModelPrefix)
	cfg.ModelCacheTTLMS = coerceInt(raw["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	cfg.ModelRefreshMS = coerceInt(raw["model_refresh_ms"], cfg.ModelRefreshMS)
	cfg.RequestTimeoutMS = coerceInt(raw["request_timeout_ms"], cfg.RequestTimeoutMS)
	cfg.CatalogueTimeoutMS = coerceInt(raw["catalogue_timeout_ms"], cfg.CatalogueTimeoutMS)
	cfg.OAuthTimeoutMS = coerceInt(raw["oauth_timeout_ms"], cfg.OAuthTimeoutMS)
	cfg.LoginTimeoutMS = coerceInt(raw["login_timeout_ms"], cfg.LoginTimeoutMS)
	cfg.PollIntervalSeconds = coerceInt(raw["poll_interval_seconds"], cfg.PollIntervalSeconds)
	cfg.RefreshWindowSeconds = coerceInt(raw["refresh_window_seconds"], cfg.RefreshWindowSeconds)
	cfg.DefaultMaxTokens = coerceInt(raw["max_tokens"], cfg.DefaultMaxTokens)
	cfg.TimezoneID = coerceString(raw["timezone_id"], cfg.TimezoneID)
	return cfg
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

// coerceString accepts a string; a non-string keeps the fallback.
func coerceString(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return fallback
}

// ConfigFields describes the settings the CPA management UI renders.
func ConfigFields() []configField {
	return []configField{
		{Name: "discover_models", Type: "boolean",
			Description: "是否用账号会话实时拉取远端模型目录 GET /mavis/api/v1/models（默认开启）。" +
				"关闭时只用内置的 4 个实测模型；远端拉取失败时也会静默回退，不报错"},
		{Name: "model_prefix", Type: "boolean", Description: "是否把账号标识作为模型前缀暴露（<账号>/<模型>）。关闭后模型列表只显示模型本身的名字"},
		{Name: "model_cache_ttl_ms", Type: "integer", Description: "远端模型目录的缓存时长，毫秒（默认 300000 = 5 分钟，与远端下发的 ttlSeconds 一致）"},
		{Name: "model_refresh_ms", Type: "integer",
			Description: "后台自动刷新目录的间隔，毫秒（默认 0 = 关闭）。开启后每隔该时长重拉一次远端目录；" +
				"只有在目录确实变化时才写凭据文件通知宿主重新注册（目录不变则不写，因此稳定期零写入）；" +
				"无论是否变化都可用状态页的「刷新目录」按钮手动刷新并强制通知宿主"},
		{Name: "request_timeout_ms", Type: "integer", Description: "签到 / 积分接口的单次请求超时，毫秒（默认 30000）"},
		{Name: "catalogue_timeout_ms", Type: "integer", Description: "模型目录接口的单次请求超时，毫秒（默认 30000）"},
		{Name: "oauth_timeout_ms", Type: "integer", Description: "单次 OAuth 调用（设备码 / 令牌 / 续期）的超时，毫秒（默认 20000，比业务请求短）"},
		{Name: "login_timeout_ms", Type: "integer", Description: "单次设备码登录会话的存活时长，毫秒（默认 300000 = 5 分钟）"},
		{Name: "poll_interval_seconds", Type: "integer",
			Description: "设备码轮询间隔的兜底值，秒（默认 5）。⚠️ 单位是秒；服务端下发的 interval 优先，它也是秒"},
		{Name: "refresh_window_seconds", Type: "integer",
			Description: "令牌到期前多久提前续期，秒（默认 300）。凭据的 expires_at 由令牌响应的 expires_in 自算（access_token 不是 JWT，解不出过期时间）"},
		{Name: "max_tokens", Type: "integer", Description: "请求未带 max_tokens 时使用的默认值；0 表示不发该字段（默认 0，交给模型自己的上限）"},
		{Name: "timezone_id", Type: "string",
			Description: "上报给签到接口的 IANA 时区名（如 Asia/Shanghai）。留空按 UTC 上报。" +
				"⚠️ 该值必须作为 query 参数下发；服务端按客户端上报的时区结算每日窗口"},
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

// oauthTimeout is the configured OAuth-request timeout.
func (c Config) oauthTimeout() int {
	if c.OAuthTimeoutMS > 0 {
		return c.OAuthTimeoutMS
	}
	return OAuthTimeoutMS
}

// requestTimeout is the configured single-request timeout for catalogue,
// sign-in and credit calls.
func (c Config) requestTimeout() int {
	if c.RequestTimeoutMS > 0 {
		return c.RequestTimeoutMS
	}
	return RequestTimeoutMS
}

// catalogueTimeout is the configured catalogue timeout.
func (c Config) catalogueTimeout() int {
	if c.CatalogueTimeoutMS > 0 {
		return c.CatalogueTimeoutMS
	}
	return RequestTimeoutMS
}

// loginSessionTTL is the configured interactive-login lifetime.
func (c Config) loginSessionTTL() int {
	if c.LoginTimeoutMS > 0 {
		return c.LoginTimeoutMS
	}
	return LoginTimeoutMS
}

// pollIntervalSeconds is the configured fallback poll interval.
func (c Config) pollIntervalSeconds() int {
	if c.PollIntervalSeconds > 0 {
		return c.PollIntervalSeconds
	}
	return PollIntervalSeconds
}

// refreshWindow is the configured pre-refresh window.
func (c Config) refreshWindow() int {
	if c.RefreshWindowSeconds >= 0 {
		return c.RefreshWindowSeconds
	}
	return RefreshWindowSeconds
}

// modelCacheTTL is the configured discovered-catalogue lifetime.
func (c Config) modelCacheTTL() int {
	if c.ModelCacheTTLMS > 0 {
		return c.ModelCacheTTLMS
	}
	return ModelCacheTTLMS
}

// timezoneID is the zone reported to the sign-in endpoints.
//
// The reference reads the host zone and falls back to `UTC`
// (`resolveMinimaxTimezoneId`, `minimax-credits.ts`). A plugin has no host zone
// API, so the configured value wins and an unset one is reported as `UTC` —
// never invented from the process clock, which would silently change the
// server-side claim window.
func (c Config) timezoneID() string {
	if zone := strings.TrimSpace(c.TimezoneID); zone != "" {
		return zone
	}
	return "UTC"
}

// nowTime is the plugin's single clock read, kept in one place so tests can
// reason about time-dependent behaviour.
func nowTime() time.Time { return time.Now() }

// refreshWindow converts the configured pre-refresh window to a duration.
func refreshWindow(cfg Config) time.Duration {
	return time.Duration(cfg.refreshWindow()) * time.Second
}
