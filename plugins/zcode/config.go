package main

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider identity. One provider key: the free-quota channel is single-region
// (`https://zcode.z.ai`) and every credential is issued by the same flow.
const (
	// ProviderKey is the stable provider identifier written into CPA auth files.
	ProviderKey = "zcode"
	// DisplayName is the human-readable name shown by management clients.
	DisplayName = "ZCode (智谱)"
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
)

// Upstream endpoints. Every one of them is measured; see the file comments in
// upstream.go for which headers each requires.
const (
	// Origin is the ZCode platform origin (`ZCODE_ORIGIN`).
	Origin = "https://zcode.z.ai"
	// MessagesPath is the ONLY inference path that exists. The OpenAI-shaped
	// `/v1/chat/completions` is not mounted under `zcode-plan` and answers
	// `404 page not found` (`zcode-anthropic.ts:1-20`).
	MessagesPath = "/api/v1/zcode-plan/anthropic/v1/messages"
	// BalancePath needs `Authorization` AND `X-Device-Mid`.
	BalancePath = "/api/v1/zcode-plan/billing/balance"
	// PreviewPath needs only `X-Device-Mid`.
	PreviewPath = "/api/v1/zcode-plan/billing/preview"
	// ClaimPath needs `Authorization`, `X-Device-Mid` and an Aliyun captcha
	// header. It is the ONE endpoint where that header appears, and the server
	// ENFORCES it: calling without one returns
	// `400 {"code":3007,"msg":"captcha verify failed"}` every time (measured
	// 2026-10-01). This plugin does not mint captchas, so a one-click check-in
	// for this provider cannot succeed; it reports 3007 plainly rather than
	// failing silently. Inference is unaffected — it needs no captcha.
	ClaimPath = "/api/v1/zcode-plan/billing/claim"
	// EventReportPath needs only `X-Device-Mid`.
	EventReportPath = "/api/v1/event/report"
	// ClientConfigsPath publishes the model pool (context window, output cap and
	// the reasoning levels the client may pick).
	ClientConfigsPath = "/api/v1/client/configs"
	// OAuthCLIInitPath starts the server-mediated device authorization flow.
	OAuthCLIInitPath = "/api/v1/oauth/cli/init"
	// OAuthCLIPollPrefix is the poll endpoint; append the flow id.
	OAuthCLIPollPrefix = "/api/v1/oauth/cli/poll/"
)

// Defaults measured from the reference.
const (
	// DefaultAppVersion is `ZCODE_APP_VERSION_FALLBACK` (`zcode.ts:392`).
	DefaultAppVersion = "3.14.3"
	// DefaultPlatform is the `X-Platform` value the official client sends on
	// Windows (`zcode-upstream.ts:79`).
	DefaultPlatform = "win32"
	// DefaultOSCategory is the `X-Os-Category` value paired with DefaultPlatform.
	DefaultOSCategory = "windows"
	// DefaultClientLanguage is `X-Client-Language` (`zcode-upstream.ts:76`).
	DefaultClientLanguage = "zh-CN"
	// DefaultClientTimezone is `X-Client-Timezone` (`zcode-upstream.ts:77`).
	DefaultClientTimezone = "Asia/Shanghai"
	// DefaultRequestTimeoutMS bounds one upstream inference call. The reference
	// uses 180s because the free channel's single-request latency is 3–30s with a
	// long tail (`zcode-product.ts` `requestTimeoutMs`).
	DefaultRequestTimeoutMS = 180_000
	// DefaultLoginTimeoutMS is the whole device-authorization budget.
	DefaultLoginTimeoutMS = 300_000
	// DefaultPollIntervalMS is the fallback when the init response omits
	// `poll_interval_sec` (`zcode-login.ts`).
	DefaultPollIntervalMS = 2_000
	// DefaultPollMaxFailures is the consecutive transport-failure budget of one
	// login poll.
	DefaultPollMaxFailures = 5
	// DefaultConcurrencyRetryMax is how many times a `3009` concurrency rejection
	// is retried inside the adapter (`zcode-product.ts:concurrencyRetryMax`).
	DefaultConcurrencyRetryMax = 2
	// DefaultConcurrencyRetryBaseMS is the LINEAR backoff base; 1500ms because a
	// 900ms retry still hit the window (`zcode-product.ts`).
	DefaultConcurrencyRetryBaseMS = 1_500
	// DefaultDefaultMaxTokens is sent when the caller omits `max_tokens`. The
	// reference uses 8192 (`zcode-adapter.ts`).
	DefaultDefaultMaxTokens = 8_192
)

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
type Config struct {
	Enabled  bool
	Priority int

	// AppVersion is written into `X-ZCode-App-Version`, `User-Agent` and the
	// activity reports. `detectInstalledAppVersion` may replace the default.
	AppVersion string
	// Platform is `X-Platform`; OSGroup is `X-Os-Category`.
	Platform string
	OSGroup  string
	// ClientLanguage / ClientTimezone are the `X-Client-*` pair.
	ClientLanguage string
	ClientTimezone string

	// ModelPrefix exposes `<账号>/<模型>` ids; off publishes the bare ids.
	ModelPrefix bool
	// DiscoverModels fetches `client/configs` for the authoritative context
	// window, output cap and reasoning levels. Failure falls back to the static
	// table, so this is safe to leave on.
	DiscoverModels bool
	// ToolCacheBreakpoint puts one prompt-caching breakpoint on the LAST tool.
	ToolCacheBreakpoint bool
	// DefaultMaxTokens is applied when the request omits `max_tokens`; 0 sends none.
	DefaultMaxTokens int

	// ConcurrencyRetryMax is how many times a `3009` concurrency rejection is
	// retried; ConcurrencyRetryBaseMS is the linear backoff base.
	ConcurrencyRetryMax    int
	ConcurrencyRetryBaseMS int
	// RequestTimeoutMS bounds one inference call.
	RequestTimeoutMS int
	// LoginTimeoutMS / PollIntervalMS / PollMaxFailures drive the login poll.
	LoginTimeoutMS  int
	PollIntervalMS  int
	PollMaxFailures int

	// ImportClientCredential enables the optional adoption path: decrypt the
	// official client's `~/.zcode/v2/credentials.json`.
	ImportClientCredential bool
	// ClientDataDir overrides the data root the import path looks under. Empty
	// means "the home directory", which is the official default.
	ClientDataDir string
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:                true,
		ModelPrefix:            true,
		AppVersion:             DefaultAppVersion,
		Platform:               DefaultPlatform,
		OSGroup:                DefaultOSCategory,
		ClientLanguage:         DefaultClientLanguage,
		ClientTimezone:         DefaultClientTimezone,
		DiscoverModels:         true,
		ToolCacheBreakpoint:    true,
		DefaultMaxTokens:       DefaultDefaultMaxTokens,
		ConcurrencyRetryMax:    DefaultConcurrencyRetryMax,
		ConcurrencyRetryBaseMS: DefaultConcurrencyRetryBaseMS,
		RequestTimeoutMS:       DefaultRequestTimeoutMS,
		LoginTimeoutMS:         DefaultLoginTimeoutMS,
		PollIntervalMS:         DefaultPollIntervalMS,
		PollMaxFailures:        DefaultPollMaxFailures,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is decoded into a generic mapping first and every key is then
// coerced on its own. Three properties follow, and all three matter in practice:
//
//   - block AND flow style both work, because the host hands the instance
//     subtree back in whichever style the user wrote it — a block-only line
//     scanner silently ignores `{enabled: true, model_prefix: false}` and falls
//     back to every default without reporting anything;
//   - values YAML 1.2 reads as strings but a user reasonably writes as booleans
//     (`yes`/`no`/`on`/`off`) still work: yaml.v3 is YAML 1.2, so a typed decode
//     into `bool` would fail on `no` and discard the whole document;
//   - one unusable value costs only its own default, never the whole file.
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
	cfg.AppVersion = coerceString(raw["app_version"], cfg.AppVersion)
	cfg.Platform = coerceChoice(raw["platform"], cfg.Platform, []string{"win32", "darwin", "linux"})
	cfg.OSGroup = coerceChoice(raw["os_category"], cfg.OSGroup, []string{"windows", "macos", "linux"})
	cfg.ClientLanguage = coerceString(raw["client_language"], cfg.ClientLanguage)
	cfg.ClientTimezone = coerceString(raw["client_timezone"], cfg.ClientTimezone)
	cfg.ModelPrefix = coerceBool(raw["model_prefix"], cfg.ModelPrefix)
	cfg.DiscoverModels = coerceBool(raw["discover_models"], cfg.DiscoverModels)
	cfg.ToolCacheBreakpoint = coerceBool(raw["tool_cache_breakpoint"], cfg.ToolCacheBreakpoint)
	cfg.DefaultMaxTokens = coerceInt(raw["max_tokens"], cfg.DefaultMaxTokens)
	cfg.ConcurrencyRetryMax = coerceInt(raw["concurrency_retry_max"], cfg.ConcurrencyRetryMax)
	cfg.ConcurrencyRetryBaseMS = coerceInt(raw["concurrency_retry_base_ms"], cfg.ConcurrencyRetryBaseMS)
	cfg.RequestTimeoutMS = coerceInt(raw["request_timeout_ms"], cfg.RequestTimeoutMS)
	cfg.LoginTimeoutMS = coerceInt(raw["login_timeout_ms"], cfg.LoginTimeoutMS)
	cfg.PollIntervalMS = coerceInt(raw["poll_interval_ms"], cfg.PollIntervalMS)
	cfg.PollMaxFailures = coerceInt(raw["poll_max_failures"], cfg.PollMaxFailures)
	cfg.ImportClientCredential = coerceBool(raw["import_client_credential"], cfg.ImportClientCredential)
	cfg.ClientDataDir = coerceString(raw["client_data_dir"], cfg.ClientDataDir)
	return cfg
}

// coerceChoice keeps the configured default unless the value names one of the
// accepted spellings (case-insensitive).
func coerceChoice(value any, fallback string, accepted []string) string {
	text := strings.ToLower(coerceString(value, ""))
	for _, candidate := range accepted {
		if text == candidate {
			return candidate
		}
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

// coerceString returns a non-empty trimmed string, else the fallback.
func coerceString(value any, fallback string) string {
	if text, ok := value.(string); ok {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

// configField mirrors pluginapi.ConfigField; the plugin package owns its own
// type so the ABI layer stays free of provider specifics.
type configField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}

// ConfigFields describes the settings the CPA management UI renders.
func ConfigFields() []configField {
	return []configField{
		{Name: "app_version", Type: "string",
			Description: "X-ZCode-App-Version / User-Agent 里的客户端版本（默认 " + DefaultAppVersion + "）；" +
				"配置为空时会尝试从本机官方客户端安装清单探测"},
		{Name: "platform", Type: "enum", EnumValues: []string{"win32", "darwin", "linux"},
			Description: "X-Platform 头。官方在 Windows 上发 win32，改它只影响来源标识，不影响准入"},
		{Name: "os_category", Type: "enum", EnumValues: []string{"windows", "macos", "linux"},
			Description: "X-Os-Category 头，与 platform 配对"},
		{Name: "client_language", Type: "string", Description: "X-Client-Language 头（默认 zh-CN）"},
		{Name: "client_timezone", Type: "string", Description: "X-Client-Timezone 头（默认 Asia/Shanghai）"},
		{Name: "model_prefix", Type: "boolean",
			Description: "是否把账号 ID 作为模型前缀暴露（<账号>/<模型>）。GLM-5.3 与 GLM-5.3-Flash 已被其它渠道发布，" +
				"同名模型在本部署里会合并成一个条目、由多个凭据共同供给，因此改名或加前缀都不是必须的"},
		{Name: "discover_models", Type: "boolean",
			Description: "是否从 GET /api/v1/client/configs 读取权威的上下文窗口、最大输出与思考档位；" +
				"拉取失败自动回退内置表，不影响可用性"},
		{Name: "tool_cache_breakpoint", Type: "boolean",
			Description: "是否在最后一个工具上打 Anthropic prompt caching 断点（前缀式缓存，一个断点即覆盖 system + 全部工具）"},
		{Name: "max_tokens", Type: "integer", Description: "请求未指定 max_tokens 时的默认值，0 表示不发送"},
		{Name: "concurrency_retry_max", Type: "integer",
			Description: "上游 429 code 3009（并发限流）时的重试次数上限（默认 2）。它与额度用尽是两回事：并发限流等一下就能过，额度用尽必须换凭据"},
		{Name: "concurrency_retry_base_ms", Type: "integer", Description: "并发限流重试的线性退避基数，毫秒（默认 1500）"},
		{Name: "request_timeout_ms", Type: "integer", Description: "单次推理请求的超时，毫秒（默认 180000）"},
		{Name: "login_timeout_ms", Type: "integer", Description: "设备授权登录的等待总超时，毫秒（默认 5 分钟）"},
		{Name: "poll_interval_ms", Type: "integer", Description: "登录轮询间隔兜底值，毫秒（服务端给出 poll_interval_sec 时以服务端为准）"},
		{Name: "poll_max_failures", Type: "integer", Description: "登录轮询的连续网络失败上限（默认 5）"},
		{Name: "import_client_credential", Type: "boolean",
			Description: "是否允许从官方客户端的 ~/.zcode/v2/credentials.json 导入登录态（可选路径；" +
				"主路径始终是插件自己的登录）。开启后可在状态页点「导入官方客户端凭据」"},
		{Name: "client_data_dir", Type: "string",
			Description: "官方客户端数据根目录（可选）。留空则依次尝试 ZCODE_DATA_BASE_DIR、家目录、APPDATA、LOCALAPPDATA"},
	}
}
