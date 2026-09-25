package main

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider identity and every upstream endpoint used by the adapter. All hosts
// embed the cn-north-4 region; there is no region in the signing scope.
const (
	ProviderKey = "codearts"
	DisplayName = "CodeArts Agent"

	// Snap-Access gateway: chat completions plus credit/sign-in operations.
	SnapEngineBase = "https://snap-access.cn-north-4.myhuaweicloud.com"
	ChatAPIPath    = "/api/v2/chat/completions"
	// SnapModelBuiltinURL lists the regular (non-benefit) models.
	SnapModelBuiltinURL = SnapEngineBase + "/v1/model/builtin"
	// OpenGWGatewayConfigURL lists the benefit models.
	OpenGWGatewayConfigURL = "https://opengw.developer.huaweicloud.com/api/v1/gateway/config"
	// PackageInfoPath reports whether the account is a credit package.
	PackageInfoPath = "/snap-manager/v1/statistics/plugin"
	// Daily check-in operations.
	OpsDeliveryPath = "/v1/ops/delivery"
	OpsClaimPath    = "/v1/ops/claim"
	OpsConfirmPath  = "/v1/ops/confirm"
	OpsChannel      = "IDE"
	DailyLoginType  = "USER_LOGIN"

	// STS: OAuth2 token endpoint for both authorization_code and refresh_token.
	STSTokenEndpoint = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	// Portal: browser entry point for the IAM OAuth flow.
	PortalAuthorizeBase = "https://codearts.huaweicloud.com/portal/authorize"
	PortalLoginBase     = "https://codearts.huaweicloud.com/portal/login"
	OAuthClientID       = "codearts-agent"
	OAuthRedirectPath   = "/oauth/callback"
	OAuthTheme          = "2"
	OAuthLocale         = "zh-cn"
	LoginPluginName     = "snap_AIIDE"
	LoginPluginVersion  = "5.2.0"

	// Legacy ticket flow.
	LegacyCredentialEndpoint = SnapEngineBase + "/snap-manager/v1/login/ticket"
	LegacyLoginBase          = "https://devcloud.cn-north-4.huaweicloud.com/doer/redirect"
	LegacyAuthBase           = "https://auth.huaweicloud.com/authui/login.html"
	LegacyCallbackPath       = "/authentication"
	LegacyPluginName         = "snap_jetbrains"
	LegacyPluginVersion      = "26.3.3"

	// BenefitModel must be called with the signed extra header maas_type=benefit.
	BenefitModel = "glm-5.3-flash"

	// LoginFlowOAuth and LoginFlowTicket select the interactive sign-in method.
	LoginFlowOAuth  = "oauth"
	LoginFlowTicket = "ticket"
)

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree the host passes in plugin.register / plugin.reconfigure.
type Config struct {
	Enabled  bool
	Priority int `yaml:"priority"`

	// Flow selects the sign-in method: "oauth" (default) or "ticket".
	Flow string
	// DiscoverModels enables the two signed model-listing endpoints. When false
	// the static fallback list is used, which avoids an upstream round trip.
	DiscoverModels bool
	// DefaultMaxTokens is applied when a request omits max_tokens.
	DefaultMaxTokens int
	// BenefitModels routes the known benefit model through maas_type=benefit.
	BenefitModels bool
	// ToolStream enables the streaming tool-call mode used by deepseek models.
	ToolStream bool
	// PromptCacheKey attaches a stable prompt cache key to chat requests.
	PromptCacheKey bool
	// FirstTokenTimeoutMS bounds the wait for the first upstream SSE token.
	FirstTokenTimeoutMS int
	// ChunkTimeoutMS bounds the wait between two upstream SSE chunks.
	ChunkTimeoutMS int
	// ModelCacheTTLMS controls how long a discovered model list is reused.
	ModelCacheTTLMS int
	// MaxAccountsPerAuth is reserved for the shared account pool; 0 means
	// unlimited.
	MaxAccountsPerAuth int
}

// DefaultConfig returns the settings used when the user provides nothing.
// Booleans that default to true are deliberately expressed as opt-out so a
// partial YAML document does not silently disable them.
func DefaultConfig() Config {
	return Config{
		Enabled:             true,
		Flow:                LoginFlowOAuth,
		DiscoverModels:      true,
		DefaultMaxTokens:    65536,
		BenefitModels:       true,
		ToolStream:          true,
		PromptCacheKey:      true,
		FirstTokenTimeoutMS: 300000,
		ChunkTimeoutMS:      600000,
		ModelCacheTTLMS:     2 * 60 * 60 * 1000,
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
//     writes as booleans (`yes`/`no`/`on`/`off`, or a quoted number) still work;
//   - one unusable value costs only its own default instead of discarding the
//     whole document, which is what a typed decode would do.
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
	cfg.Flow = coerceFlow(raw["flow"], cfg.Flow)
	cfg.DiscoverModels = coerceBool(raw["discover_models"], cfg.DiscoverModels)
	cfg.BenefitModels = coerceBool(raw["benefit_models"], cfg.BenefitModels)
	cfg.ToolStream = coerceBool(raw["tool_stream"], cfg.ToolStream)
	cfg.PromptCacheKey = coerceBool(raw["prompt_cache_key"], cfg.PromptCacheKey)
	cfg.DefaultMaxTokens = coerceInt(raw["max_tokens"], cfg.DefaultMaxTokens)
	cfg.FirstTokenTimeoutMS = coerceInt(raw["first_token_timeout_ms"], cfg.FirstTokenTimeoutMS)
	cfg.ChunkTimeoutMS = coerceInt(raw["chunk_timeout_ms"], cfg.ChunkTimeoutMS)
	cfg.ModelCacheTTLMS = coerceInt(raw["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	cfg.MaxAccountsPerAuth = coerceInt(raw["max_accounts_per_auth"], cfg.MaxAccountsPerAuth)
	return cfg
}

// coerceFlow accepts only the two known sign-in flows.
func coerceFlow(value any, fallback string) string {
	switch strings.ToLower(coerceString(value, "")) {
	case LoginFlowOAuth:
		return LoginFlowOAuth
	case LoginFlowTicket:
		return LoginFlowTicket
	default:
		return fallback
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

// ConfigFields describes the settings the CPA management UI renders for this
// plugin.
func ConfigFields() []configField {
	return []configField{
		{Name: "flow", Type: "enum", EnumValues: []string{LoginFlowOAuth, LoginFlowTicket}, Description: "登录方式：oauth=浏览器 PKCE 授权（默认），ticket=旧版票据轮询"},
		{Name: "discover_models", Type: "boolean", Description: "是否调用远端接口动态发现模型列表，关闭则使用内置列表"},
		{Name: "benefit_models", Type: "boolean", Description: "对 glm-5.3-flash 使用 maas_type=benefit 签名头（权益模型）"},
		{Name: "tool_stream", Type: "boolean", Description: "启用 deepseek 系列的工具调用流式模式（DSML）"},
		{Name: "prompt_cache_key", Type: "boolean", Description: "为请求附加稳定的 prompt_cache_key 以命中提示缓存"},
		{Name: "max_tokens", Type: "integer", Description: "请求未指定 max_tokens 时使用的默认值（默认 65536）"},
		{Name: "first_token_timeout_ms", Type: "integer", Description: "等待上游首个 SSE 分片的超时，毫秒"},
		{Name: "chunk_timeout_ms", Type: "integer", Description: "两个上游 SSE 分片之间的超时，毫秒"},
		{Name: "model_cache_ttl_ms", Type: "integer", Description: "动态模型列表缓存时长，毫秒"},
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
