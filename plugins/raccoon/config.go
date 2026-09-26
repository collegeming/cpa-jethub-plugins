package main

import (
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
//
// There is deliberately no `product`-style setting here: this provider has one
// product, one host and one client fingerprint.
type Config struct {
	// Enabled is the host's own switch, mirrored for diagnostics.
	Enabled bool
	// Priority orders credentials in the host's scheduler.
	Priority int

	// DiscoverModels enables the live `GET /model_catalog` catalogue. It is on
	// by default: the reference prefers the remote catalogue and treats its
	// bundled table as a degraded mode only (`raccoon-auth.ts:380-410`).
	DiscoverModels bool
	// ModelCacheTTLMS bounds how long a discovered catalogue is reused.
	ModelCacheTTLMS int

	// RequestTimeoutMS bounds auth, user_info and credits calls.
	RequestTimeoutMS int
	// CatalogueTimeoutMS bounds the catalogue call.
	CatalogueTimeoutMS int
	// LoginTimeoutMS bounds one interactive QR login session.
	LoginTimeoutMS int
	// RefreshWindowSeconds is the pre-refresh window; see the constant comment.
	RefreshWindowSeconds int
	// QRPollIntervalSeconds is the QR page's meta-refresh cadence.
	QRPollIntervalSeconds int
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:               true,
		DiscoverModels:        true,
		ModelCacheTTLMS:       ModelCacheTTLMS,
		RequestTimeoutMS:      RequestTimeoutMS,
		CatalogueTimeoutMS:    CatalogueTimeoutMS,
		LoginTimeoutMS:        LoginTimeoutMS,
		RefreshWindowSeconds:  RefreshWindowSeconds,
		QRPollIntervalSeconds: QRPollIntervalSeconds,
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
	cfg.ModelCacheTTLMS = coerceInt(raw["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	cfg.RequestTimeoutMS = coerceInt(raw["request_timeout_ms"], cfg.RequestTimeoutMS)
	cfg.CatalogueTimeoutMS = coerceInt(raw["catalogue_timeout_ms"], cfg.CatalogueTimeoutMS)
	cfg.LoginTimeoutMS = coerceInt(raw["login_timeout_ms"], cfg.LoginTimeoutMS)
	cfg.RefreshWindowSeconds = coerceInt(raw["refresh_window_seconds"], cfg.RefreshWindowSeconds)
	cfg.QRPollIntervalSeconds = coerceInt(raw["qr_poll_interval_seconds"], cfg.QRPollIntervalSeconds)
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

// ConfigFields describes the settings the CPA management UI renders.
func ConfigFields() []configField {
	return []configField{
		{Name: "discover_models", Type: "boolean",
			Description: "是否用账号会话实时拉取 GET /model_catalog（默认开启）。关闭时只使用内置的 6 个兜底模型；" +
				"远端目录拉取失败时也会静默回退到兜底表，不会报错"},
		{Name: "model_cache_ttl_ms", Type: "integer", Description: "实时模型目录的缓存时长，毫秒（默认 2 小时）"},
		{Name: "request_timeout_ms", Type: "integer",
			Description: "auth / user_info / 积分接口的单次请求超时，毫秒（默认 60000）"},
		{Name: "catalogue_timeout_ms", Type: "integer",
			Description: "模型目录接口的单次请求超时，毫秒（默认 20000，比其它接口短——与参考实现一致）"},
		{Name: "login_timeout_ms", Type: "integer", Description: "单次交互式扫码登录会话的存活时长，毫秒（默认 300000 = 5 分钟）"},
		{Name: "refresh_window_seconds", Type: "integer",
			Description: "令牌到期前多久提前续期，秒（默认 300）。参考实现里该常量是死代码（只在 exp 已过才续期），" +
				"这里按声明的语义真正实现：提前 5 分钟续期，避免懒调度下把已过期令牌发出去"},
		{Name: "qr_poll_interval_seconds", Type: "integer",
			Description: "登录页二维码的 meta refresh 间隔，秒（默认 2）。每次页面加载只做一次轮询，页面没有脚本也没有表单"},
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

// requestTimeout is the configured single-request timeout for auth/user_info/
// credits calls.
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
	return CatalogueTimeoutMS
}

// loginSessionTTL is the configured interactive-login lifetime.
func (c Config) loginSessionTTL() int {
	if c.LoginTimeoutMS > 0 {
		return c.LoginTimeoutMS
	}
	return LoginTimeoutMS
}

// refreshWindow is the configured pre-refresh window.
func (c Config) refreshWindow() int {
	if c.RefreshWindowSeconds >= 0 {
		return c.RefreshWindowSeconds
	}
	return RefreshWindowSeconds
}

// qrPollSeconds is the configured QR page refresh interval.
func (c Config) qrPollSeconds() int {
	if c.QRPollIntervalSeconds > 0 {
		return c.QRPollIntervalSeconds
	}
	return QRPollIntervalSeconds
}

// modelCacheTTL is the configured discovered-catalogue lifetime.
func (c Config) modelCacheTTL() int {
	if c.ModelCacheTTLMS > 0 {
		return c.ModelCacheTTLMS
	}
	return ModelCacheTTLMS
}

// nowTime is the plugin's single clock read, kept in one place so tests can
// reason about time-dependent behaviour.
func nowTime() time.Time { return time.Now() }

// refreshWindow converts the configured pre-refresh window to a duration.
func refreshWindow(cfg Config) time.Duration {
	return time.Duration(cfg.refreshWindow()) * time.Second
}
