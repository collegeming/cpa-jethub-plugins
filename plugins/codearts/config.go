package main

import (
	"strconv"
	"strings"
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
	Priority int

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

// ConfigFromYAML decodes the flat scalar settings the host supplies. Nested
// mappings are ignored because every CodeArts setting is a top-level scalar.
func ConfigFromYAML(document []byte) Config {
	cfg := DefaultConfig()
	values := parseFlatYAML(document)

	if v, ok := values["enabled"]; ok {
		cfg.Enabled = parseBool(v, cfg.Enabled)
	}
	if v, ok := values["priority"]; ok {
		cfg.Priority = parseInt(v, cfg.Priority)
	}
	if v, ok := values["flow"]; ok && v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case LoginFlowOAuth, LoginFlowTicket:
			cfg.Flow = strings.ToLower(strings.TrimSpace(v))
		}
	}
	cfg.DiscoverModels = parseBool(values["discover_models"], cfg.DiscoverModels)
	cfg.BenefitModels = parseBool(values["benefit_models"], cfg.BenefitModels)
	cfg.ToolStream = parseBool(values["tool_stream"], cfg.ToolStream)
	cfg.PromptCacheKey = parseBool(values["prompt_cache_key"], cfg.PromptCacheKey)
	cfg.DefaultMaxTokens = parseInt(values["max_tokens"], cfg.DefaultMaxTokens)
	cfg.FirstTokenTimeoutMS = parseInt(values["first_token_timeout_ms"], cfg.FirstTokenTimeoutMS)
	cfg.ChunkTimeoutMS = parseInt(values["chunk_timeout_ms"], cfg.ChunkTimeoutMS)
	cfg.ModelCacheTTLMS = parseInt(values["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	cfg.MaxAccountsPerAuth = parseInt(values["max_accounts_per_auth"], cfg.MaxAccountsPerAuth)
	return cfg
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

// parseFlatYAML reads `key: value` scalar pairs. Indented lines (nested
// mappings and block sequences) are skipped, as are comments and blank lines.
func parseFlatYAML(document []byte) map[string]string {
	out := map[string]string{}
	for _, rawLine := range strings.Split(string(document), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Only top-level, non-sequence entries are part of the flat setting set.
		if line != trimmed || strings.HasPrefix(trimmed, "- ") {
			continue
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" || value == "|" || value == ">" {
			continue
		}
		out[key] = unquoteYAML(value)
	}
	return out
}

func unquoteYAML(value string) string {
	value = strings.TrimSpace(value)
	// A quoted scalar ends at its closing quote; anything after it is a comment.
	if len(value) >= 2 {
		quote := value[0]
		if quote == '"' || quote == '\'' {
			if end := strings.IndexByte(value[1:], quote); end >= 0 {
				return value[1 : 1+end]
			}
		}
	}
	if idx := strings.Index(value, " #"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return value
}

func parseBool(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	default:
		return fallback
	}
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}
