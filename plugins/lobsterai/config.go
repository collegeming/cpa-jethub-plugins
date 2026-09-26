package main

import (
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider identity and every upstream endpoint used by the adapter.
//
// LobsterAI (有道 LobsterAI) is deliberately NOT part of the Tencent CodeBuddy
// family: it has its own login flow, header set, renewal payload, check-in flow
// and version source, and it must not reuse the BuddyProduct type. There is no
// apiDomain / productCode / attributionName / userAgentByModelFamily /
// appendSessionParams here because none of them exist for this provider.
// Ported from jethub-src/src/lobsterai-product.ts:421-487 and
// jethub-src/src/lobsterai.ts:29-60.
const (
	// ProviderKey is the stable provider identifier written into CPA auth files.
	ProviderKey = "lobsterai"
	// DisplayName is the human-readable name shown by management clients.
	DisplayName = "LobsterAI (有道)"
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
	// Logo is the plugin display asset shown by management clients.
	Logo = "https://lobsterai.youdao.com/favicon.ico"

	// APIBase is the upstream API host (lobsterai-product.ts:421).
	APIBase = "https://lobsterai-server.youdao.com"
	// PortalBase is the browser login portal host (lobsterai-product.ts:435).
	PortalBase = "https://lobsterai.youdao.com"
	// ClientVersionAPI reports the current desktop client version. It is a
	// third-party host and its response is NOT the business envelope: the
	// payload lives at data.value (lobsterai-product.ts:443).
	ClientVersionAPI = "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update"
	// FallbackClientVersion is used when the update endpoint cannot be reached.
	FallbackClientVersion = "2026.9.4"
	// UserAgent is copied verbatim from the reference Go client
	// (lobsterai-product.ts:487). It intentionally does not track the real
	// version; only X-LobsterAI-Client-Version carries the dynamic truth.
	UserAgent = "LobsterAI/0.1.0"
	// ClientCapabilities declares the capabilities the client supports. Both are
	// load-bearing (lobsterai-product.ts:474): kimi-k3-agentic-v1 is the model
	// list admission condition, thinking-level-control-v1 is the precondition
	// for the "off" reasoning level.
	ClientCapabilities = "kimi-k3-agentic-v1,thinking-level-control-v1"

	// ExchangePath trades the authorization code for tokens (lobsterai.ts:32).
	ExchangePath = "/api/auth/exchange"
	// RefreshPath renews a credential (lobsterai.ts:34).
	RefreshPath = "/api/auth/refresh"
	// ModelsPath lists the available models (lobsterai.ts:36).
	ModelsPath = "/api/models/available"
	// ChatPath is the OpenAI-compatible chat endpoint; SSE only
	// (lobsterai.ts:45).
	ChatPath = "/api/proxy/v1/chat/completions"
	// CallbackPath is the loopback login callback (lobsterai.ts:47).
	CallbackPath = "/auth/callback"

	// ActivitySlotPath queries the daily activity slot
	// (lobsterai-credits.ts:207).
	ActivitySlotPath = "/api/client-activities/slot"
	// ActivityContextPath queries one activity's context; the activity code is
	// appended (lobsterai-credits.ts:209).
	ActivityContextPath = "/api/client-activities"
	// ProfileSummaryPath reports the credit balance
	// (lobsterai-credits.ts:211).
	ProfileSummaryPath = "/api/user/profile-summary"

	// SlotPlacement / SlotContainerAPIVersion / SlotPlatform are the three fixed
	// slot query parameters observed from the real client
	// (lobsterai-credits.ts:221-223). platform=win32 impersonates the desktop
	// client form and is independent of this plugin's actual host OS.
	SlotPlacement           = "desktop_sidebar"
	SlotContainerAPIVersion = "2"
	SlotPlatform            = "win32"
)

// Timeouts and limits.
const (
	// RequestTimeoutMS bounds control-plane requests (lobsterai.ts:50).
	RequestTimeoutMS = 30_000
	// LoginTimeout bounds the whole interactive sign-in
	// (lobsterai.ts:52, ten minutes).
	LoginTimeout = 10 * time.Minute
	// VersionCacheTTL caches the resolved client version (lobsterai.ts:60).
	VersionCacheTTL = 12 * time.Hour
	// MinCallbackPort accepts every ephemeral port. The shared oauthcb listener
	// defaults to 10000 because the CodeArts portal requires it; LobsterAI binds
	// port 0 with no lower bound (lobsterai-oauth.ts:345-358), so 1 keeps the
	// same "any free port" behaviour.
	MinCallbackPort = 1
)

// Config is the per-instance plugin configuration decoded from the config_yaml
// subtree the host passes in plugin.register / plugin.reconfigure.
type Config struct {
	Enabled bool
	// DiscoverModels enables GET /api/models/available. When false the local
	// fallback catalog is used and no upstream round trip happens.
	DiscoverModels bool
	// ClientVersionOverride pins the client version used by the exchange,
	// refresh, models and check-in requests. Empty means "resolve dynamically".
	ClientVersionOverride string
	// DefaultMaxTokens is applied when the caller did not supply max_tokens and
	// the remote model metadata declares none. Zero leaves the field absent
	// rather than inventing a value.
	DefaultMaxTokens int
	// ModelCacheTTLMS controls how long a discovered model list is reused.
	ModelCacheTTLMS int
	// RequestTimeoutMS is advertised for operators; the host transport owns the
	// real timeout, so this is currently informational only.
	RequestTimeoutMS int
	// DailyCheckin enables the management check-in routes.
	DailyCheckin bool

	// CallbackPort pins the loopback callback listener to an exact port. The
	// LobsterAI portal takes a full `redirect_uri` and echoes `state`, so the
	// port is ours to choose. Zero (the default) keeps the reference behaviour:
	// any free ephemeral port.
	//
	// A container deployment MUST set this, because an ephemeral port cannot be
	// published in advance and the container's 127.0.0.1 is not the browser's.
	CallbackPort int
	// CallbackBindHost is the local address the listener binds. Defaults to
	// 127.0.0.1; a container deployment must use 0.0.0.0 so the published port
	// reaches it.
	CallbackBindHost string
	// CallbackPublicHost and CallbackPublicPort describe the address the BROWSER
	// dials. Defaults to 127.0.0.1 and the bound port, which is correct for a
	// published container port.
	CallbackPublicHost string
	CallbackPublicPort int
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:          true,
		DiscoverModels:   true,
		DefaultMaxTokens: 0,
		ModelCacheTTLMS:  2 * 60 * 60 * 1000,
		RequestTimeoutMS: RequestTimeoutMS,
		DailyCheckin:     true,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is first decoded into a generic mapping and each key is then
// coerced individually. Three properties follow, all of which matter in
// practice:
//
//   - block and flow style are both accepted, because the host hands the
//     instance subtree back in whichever style the user wrote it;
//   - values that a YAML 1.2 decoder sees as strings but a user reasonably
//     writes as booleans (`yes`/`no`/`on`/`off`, or a quoted number) still work,
//     because yaml.v3 follows YAML 1.2 where those spellings are strings;
//   - one unusable value costs only its own default instead of discarding the
//     whole document, which is what a typed decode into a struct would do.
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
	cfg.DiscoverModels = coerceBool(raw["discover_models"], cfg.DiscoverModels)
	cfg.DailyCheckin = coerceBool(raw["daily_checkin"], cfg.DailyCheckin)
	cfg.ClientVersionOverride = coerceString(raw["client_version"], cfg.ClientVersionOverride)
	cfg.DefaultMaxTokens = coerceInt(raw["max_tokens"], cfg.DefaultMaxTokens)
	cfg.ModelCacheTTLMS = coerceInt(raw["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	cfg.RequestTimeoutMS = coerceInt(raw["request_timeout_ms"], cfg.RequestTimeoutMS)
	cfg.CallbackPort = coerceInt(raw["callback_port"], cfg.CallbackPort)
	cfg.CallbackBindHost = coerceString(raw["callback_bind_host"], cfg.CallbackBindHost)
	cfg.CallbackPublicHost = coerceString(raw["callback_public_host"], cfg.CallbackPublicHost)
	cfg.CallbackPublicPort = coerceInt(raw["callback_public_port"], cfg.CallbackPublicPort)
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
		{Name: "discover_models", Type: "boolean", Description: "是否调用 /api/models/available 动态发现模型（含远端模型参数与思考档位），关闭则使用内置兜底列表"},
		{Name: "client_version", Type: "string", Description: "固定客户端版本号（如 2026.9.4）；留空表示从有道更新接口动态获取，失败时回退 2026.9.4"},
		{Name: "max_tokens", Type: "integer", Description: "请求未指定 max_tokens 且远端模型未声明 maxTokens 时使用的默认值（0 = 不下发该字段）"},
		{Name: "model_cache_ttl_ms", Type: "integer", Description: "动态模型列表缓存时长，毫秒"},
		{Name: "daily_checkin", Type: "boolean", Description: "启用每日签到（/api/client-activities 领取积分）与状态页签到入口"},
		{Name: "callback_port", Type: "integer", Description: "登录回调固定端口（容器部署必填，并需在 compose 中发布同名端口；0=随机端口，仅本机部署可用）"},
		{Name: "callback_bind_host", Type: "string", Description: "回调监听绑定的本机地址（容器部署填 0.0.0.0，默认 127.0.0.1）"},
		{Name: "callback_public_host", Type: "string", Description: "浏览器访问回调时使用的主机名（默认 127.0.0.1）"},
		{Name: "callback_public_port", Type: "integer", Description: "浏览器访问回调时使用的端口（默认与 callback_port 相同，仅当宿主机映射端口不同时填写）"},
	}
}
