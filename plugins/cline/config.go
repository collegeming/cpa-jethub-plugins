package main

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
type Config struct {
	Enabled  bool
	Priority int

	// ModelDiscovery enables the two catalogue endpoints in `model.for_auth`.
	// When false only the static fallback table is published, which avoids two
	// upstream round trips per listing.
	ModelDiscovery bool
	// ModelCacheTTLMS bounds how long a fetched catalogue is reused. The remote
	// `free` array is server-side marketing state that the source re-fetches
	// every time (`README.md:1269-1270`), so the default is short (10 min) and 0
	// disables caching entirely.
	ModelCacheTTLMS int
	// DefaultMaxTokens is applied when a request omits `max_tokens`; 0 sends
	// none, which is what the TypeScript does (`cline-adapter.ts:434-435`).
	DefaultMaxTokens int
	// MaxOutputTokens is the clamp ceiling for `max_tokens`
	// (`cline-adapter.ts:72`, 943 718).
	MaxOutputTokens int
	// DefaultReasoningEffort is sent as `reasoning_effort` when the client did
	// not ask for one. The TypeScript product defaults to `high`
	// (`cline-product.ts:247`), but CPA has its own thinking configuration, so
	// the plugin defaults to "" (= omit the field) and never overrides an
	// explicit client choice. Set it to one of reasoningLevels to reproduce the
	// TypeScript default.
	DefaultReasoningEffort string
	// LoginTimeoutMS bounds the whole device-code wait (`cline-oauth.ts:80`).
	LoginTimeoutMS int
	// PollIntervalMS is the fallback poll interval used when the server does not
	// publish `interval` (`cline-oauth.ts:83`). `slow_down` still adds 1 s per
	// occurrence on top of whatever is negotiated.
	PollIntervalMS int
	// PollMaxFailures is the consecutive transport-failure budget
	// (`cline-oauth.ts:86`).
	PollMaxFailures int
	// BalanceDivisor converts the raw balance into the displayed unit. See
	// balanceDivisorRaw for why it is configurable.
	BalanceDivisor float64
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:                true,
		ModelDiscovery:         true,
		ModelCacheTTLMS:        600_000,
		DefaultMaxTokens:       0,
		MaxOutputTokens:        MaxOutputTokensCeiling,
		DefaultReasoningEffort: "",
		LoginTimeoutMS:         DeviceCodeTTLMS,
		PollIntervalMS:         DevicePollIntervalMS,
		PollMaxFailures:        PollMaxFailures,
		BalanceDivisor:         balanceDivisorRaw,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is first decoded into a generic mapping and every key is then
// coerced individually. Three properties follow, all of which matter in practice:
//
//   - block AND flow style are both accepted, because the host hands the instance
//     subtree back in whichever style the user wrote it. A block-only line
//     scanner silently ignores `{model_discovery: false, login_timeout_ms: 60000}`
//     and falls back to every default without reporting anything;
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
	cfg.ModelDiscovery = coerceBool(raw["model_discovery"], cfg.ModelDiscovery)
	cfg.ModelCacheTTLMS = coerceInt(raw["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	cfg.DefaultMaxTokens = coerceInt(raw["max_tokens"], cfg.DefaultMaxTokens)
	cfg.MaxOutputTokens = coerceInt(raw["max_output_tokens"], cfg.MaxOutputTokens)
	cfg.DefaultReasoningEffort = coerceReasoningEffort(raw["reasoning_effort"], cfg.DefaultReasoningEffort)
	cfg.LoginTimeoutMS = coerceInt(raw["login_timeout_ms"], cfg.LoginTimeoutMS)
	cfg.PollIntervalMS = coerceInt(raw["poll_interval_ms"], cfg.PollIntervalMS)
	cfg.PollMaxFailures = coerceInt(raw["poll_max_failures"], cfg.PollMaxFailures)
	cfg.BalanceDivisor = coerceFloat(raw["balance_divisor"], cfg.BalanceDivisor)
	return cfg
}

// coerceReasoningEffort accepts any non-empty scalar. The wire field is passed
// through verbatim with no whitelist because upstream silently ignores unknown
// values (`cline-adapter.ts:437-446`), so validating here would only invent a
// failure the server does not have.
func coerceReasoningEffort(value any, fallback string) string {
	if text := coerceString(value, ""); text != "" {
		return text
	}
	return fallback
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

// coerceFloat accepts a number or a numeric string.
func coerceFloat(value any, fallback float64) float64 {
	switch typed := value.(type) {
	case nil:
		return fallback
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case float64:
		return typed
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
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
		{Name: "model_discovery", Type: "boolean",
			Description: "是否在 model.for_auth 拉取线上目录：/api/v1/ai/cline/recommended-models（匿名，免费名单的唯一权威来源）与 /api/v1/models（需要凭据）。关闭后只发布静态表"},
		{Name: "model_cache_ttl_ms", Type: "integer",
			Description: "线上目录缓存时长，毫秒（默认 600000）。免费名单是服务端营销状态，随时会变，0 表示每次都重新拉取"},
		{Name: "max_tokens", Type: "integer",
			Description: "请求未带 max_tokens 时的默认值，0 表示不发送（与原始实现一致）"},
		{Name: "max_output_tokens", Type: "integer",
			Description: "max_tokens 的上限钳制值（默认 943718）。上游对超限值直接报错，gemini-3.8-flash 的目录值另有 65536 的限制"},
		{Name: "reasoning_effort", Type: "string",
			Description: "客户端未指定 reasoning_effort 时补发的思考级别：none/low/medium/high/max，留空则不发送（原始客户端默认 high，但 CPA 有自己的思考配置，插件不覆盖显式选择）"},
		{Name: "login_timeout_ms", Type: "integer",
			Description: "设备码登录等待总超时，毫秒（默认 300000，即 5 分钟；服务端下发 expires_in 时以服务端为准）"},
		{Name: "poll_interval_ms", Type: "integer",
			Description: "设备码轮询间隔，毫秒（默认 5000；服务端下发 interval 时以服务端为准，slow_down 每次在此基础上累加 1 秒）"},
		{Name: "poll_max_failures", Type: "integer",
			Description: "轮询期间连续网络失败上限（默认 5，收到任何 HTTP 响应即清零）"},
		{Name: "balance_divisor", Type: "number",
			Description: "余额换算除数（默认 100000，即把 raw 当作微美元）。⚠️ 这个刻度在上游没有任何文档依据，只有真实账号能确认；填写错误只会让余额显示不准"},
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
