package main

// 本文件是 Jet-Hub `src/buddy.ts`（常量）与 `src/product.ts`（产品表）的 Go 移植。
//
// 端口映射（每行都标注了 TypeScript 源位置）：
//   - buddy.ts:20-86   端点、路径、轮询参数、错误码、请求头名、UA、产品码
//   - product.ts:74-144 BuddyProduct 字段
//   - product.ts:168-254 / 283-355 两个产品的兜底模型目录（见 models.go）
//   - product.ts:256-272 / 369-396 / 408-422 / 432-451 四个产品配置
//
// 与 TS 的一处结构性差异：TS 把「provider id」同时当作路由名与账号归属字段，
// 因此有 buddy / buddy-intl / workbuddy-cn / workbuddy 四个 provider。CPA 的插件
// 只能注册**一个** provider key（本插件为 `codebuddy`），所以产品改由配置字段
// `product` 选择，四个产品各自仍保留自己的 endpoint / apiDomain / UA /
// X-Product-Code / 兜底模型表 —— 与 TS 的差异全部收敛在本文件的产品表里。

import (
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/brandicons"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Plugin identity.
//
// These are vars, not consts, so one source tree can be built into a separate
// plugin per product family: CPA derives a plugin's id from its file name
// (`<id>-v<version>.so`) and lets one plugin register exactly one provider key,
// so "CodeBuddy 国内版 and WorkBuddy 国际版 both available at once" means several
// .so files. scripts/build.sh passes the identity with
// `-ldflags -X main.ProviderKey=... -X main.DefaultProduct=...`.
var (
	// ProviderKey is the CPA provider key this build registers. It names the
	// auth files, the model prefix and the executor route, so two builds that
	// shared it would collide.
	ProviderKey = "codebuddy"
	// DisplayName is the human-readable name shown by the host.
	DisplayName = "CodeBuddy"
	// DefaultProduct is the Jet-Hub product this build serves when the user
	// configures nothing. Overriding it is what makes a variant "the
	// international one" out of the box rather than by configuration.
	DefaultProduct = ProductCodeBuddy
)

const (
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
)

// ── API 端点常量（buddy.ts:20-39）──

const (
	// APIEndpoint is the default (China) API base. buddy.ts:20.
	APIEndpoint = "https://copilot.tencent.com"
	// PrefixPath is the plugin path prefix. buddy.ts:22.
	PrefixPath = "/plugin"
	// PlatformIDE is the auth/state platform for CodeBuddy. buddy.ts:24.
	PlatformIDE = "ide"
	// WebsiteHome is the China login website home. buddy.ts:26.
	WebsiteHome = "https://www.codebuddy.cn"

	// AuthStatePath is POST /v2/plugin/auth/state?platform=<platform>. buddy.ts:29.
	AuthStatePath = "/v2/plugin/auth/state"
	// AuthTokenPath is GET /v2/plugin/auth/token?state=<state>. buddy.ts:31.
	AuthTokenPath = "/v2/plugin/auth/token"
	// LoginAccountPath is GET /v2/plugin/login/account?state=<state>. buddy.ts:33.
	LoginAccountPath = "/v2/plugin/login/account"
	// AuthRefreshPath is POST /v2/plugin/auth/token/refresh. buddy.ts:35.
	AuthRefreshPath = "/v2/plugin/auth/token/refresh"
	// AccountsPath is GET /v2/plugin/accounts. buddy.ts:37. Declared by the TS
	// but not consumed by any flow this plugin ports (kept for completeness).
	AccountsPath = "/v2/plugin/accounts"
	// ConfigPath is GET /v3/config. buddy.ts:39.
	ConfigPath = "/v3/config"
)

// ── 轮询参数（buddy.ts:41-50）──

const (
	// LoginTimeoutMS bounds the whole interactive login. buddy.ts:44.
	LoginTimeoutMS = 5 * 60 * 1000
	// PollIntervalMS is the upstream polling cadence. buddy.ts:46.
	PollIntervalMS = 1000
	// StateRequestTimeoutMS bounds POST auth/state. buddy.ts:48.
	StateRequestTimeoutMS = 5000
	// RequestTimeoutMS bounds the remaining control-plane calls. buddy.ts:50.
	RequestTimeoutMS = 60000
)

// ── 错误码（buddy.ts:52-57）──

const (
	// CodeTokenNotReady means "keep polling for the token". buddy.ts:55.
	CodeTokenNotReady = 11217
	// CodeAccountNotReady means "keep polling for the account". buddy.ts:57.
	CodeAccountNotReady = 12151
)

// ── 请求头名（buddy.ts:59-71）──

const (
	HeaderDomain            = "X-Domain"
	HeaderEnterpriseID      = "X-Enterprise-Id"
	HeaderTenantID          = "X-Tenant-Id"
	HeaderNoAuthorization   = "X-No-Authorization"
	HeaderNoUserID          = "X-No-User-Id"
	HeaderNoEnterpriseID    = "X-No-Enterprise-Id"
	HeaderNoDepartmentInfo  = "X-No-Department-Info"
	HeaderRefreshToken      = "X-Refresh-Token"
	HeaderAuthRefreshSource = "X-Auth-Refresh-Source"
	HeaderProduct           = "X-Product"
	HeaderProductCode       = "X-Product-Code"
)

// ── 身份标识（buddy.ts:73-86）──

const (
	// BuddyUserAgent is the CodeBuddy IDE UA. buddy.ts:77.
	BuddyUserAgent = "CodeBuddyIDE/1.106.1"
	// BuddyProductCode is the X-Product-Code value for CodeBuddy. buddy.ts:79.
	BuddyProductCode = "codebuddy"
	// DeploymentType is the X-Product value used by the control plane.
	// buddy.ts:81 (`BUDDY_DEPLOYMENT_TYPE = 'SaaS'`).
	DeploymentType = "SaaS"
	// AuthRefreshSource is the X-Auth-Refresh-Source value. buddy.ts:83.
	AuthRefreshSource = "ide-main"
	// APIDomain is the bare host used as the X-Domain default. buddy.ts:86.
	APIDomain = "copilot.tencent.com"
)

// ── 产品配置（product.ts）──

// userAgentRule is one "model id prefix → User-Agent" override.
// product.ts:54-59 (`BuddyUserAgentRule`).
type userAgentRule struct {
	Match string
	UA    string
}

// productConfig is the Go port of `BuddyProduct` (product.ts:74-144).
type productConfig struct {
	// ID is the Jet-Hub provider id (product.ts:76). CPA folds all four into
	// the single `codebuddy` key, but the field is kept so auth-file metadata
	// and logs stay comparable with Jet-Hub.
	ID string
	// ConfigValue is the value of the CPA `product` setting that selects this
	// product (see productByConfigValue).
	ConfigValue string
	// Platform is the auth/state `platform` query parameter. product.ts:78.
	Platform string
	// Endpoint is the API base for /v2/plugin/*, /v3/config and chat.
	// product.ts:83.
	Endpoint string
	// APIDomain is the X-Domain value (bare host, no scheme). product.ts:88.
	APIDomain string
	// DisplayName is the management-facing label. product.ts:90.
	DisplayName string
	// ProductCode is the X-Product-Code value. product.ts:92.
	ProductCode string
	// UserAgent is the default UA; product.ts:99.
	UserAgent string
	// UserAgentByModelFamily overrides UserAgent per model id prefix.
	// product.ts:111.
	UserAgentByModelFamily []userAgentRule
	// AttributionName is the value shared by X-IDE-Name / X-IDE-Type /
	// X-Product. product.ts:118.
	AttributionName string
	// ClientVersion is the X-IDE-Version value. product.ts:120.
	ClientVersion string
	// CLIVersion is the third UA segment. product.ts:122.
	CLIVersion string
	// DefaultCredentialRef is Jet-Hub's fallback credential ref. product.ts:124.
	DefaultCredentialRef string
	// AppendSessionParams appends version + loginSessionId to the login URL.
	// product.ts:141.
	AppendSessionParams bool
	// PluginVersion is appended as `version` when AppendSessionParams is set.
	// product.ts:143.
	PluginVersion string
	// FallbackModels is the built-in catalog (product.ts:136). See models.go.
	FallbackModels []fallbackModel
	// LogoURL is the product's own mark, taken from the icon the vendor site
	// links. The obvious `<domain>/favicon.ico` is a 404 on every Tencent
	// product domain, which is why the icon never rendered.
	LogoURL string
}

// Config value names for the `product` setting. These are the four names the
// task and the management UI use; they are NOT the Jet-Hub provider ids.
const (
	ProductCodeBuddy     = "codebuddy"
	ProductCodeBuddyIntl = "codebuddy-intl"
	ProductWorkBuddyCN   = "workbuddy-cn"
	ProductWorkBuddy     = "workbuddy"
)

// Products is the Go port of `ALL_PRODUCTS` (product.ts:465-470), keyed by the
// CPA config value. Order matters: it is the order of the config enum and of
// the product picker.
var Products = []productConfig{
	{
		ID:          "buddy",
		LogoURL:     brandicons.CodeBuddy,
		ConfigValue: ProductCodeBuddy,
		Platform:    "ide",
		Endpoint:    APIEndpoint,
		APIDomain:   APIDomain,
		DisplayName: "CodeBuddy (国内版)",
		ProductCode: BuddyProductCode,
		UserAgent:   BuddyUserAgent,
		// 中国版只有一条产品线，无按模型分档。product.ts:265.
		UserAgentByModelFamily: nil,
		AttributionName:        "CodeBuddy",
		ClientVersion:          "1.106.1",
		CLIVersion:             "2.137.1",
		DefaultCredentialRef:   "BUDDY_ACCESS_TOKEN",
		AppendSessionParams:    false,
		FallbackModels:         codeBuddyFallbackModels,
	},
	{
		ID:          "buddy-intl",
		LogoURL:     brandicons.CodeBuddy,
		ConfigValue: ProductCodeBuddyIntl,
		Platform:    "ide",
		Endpoint:    "https://www.codebuddy.ai",
		APIDomain:   "www.codebuddy.ai",
		DisplayName: "CodeBuddy (国际版)",
		ProductCode: BuddyProductCode,
		UserAgent:   BuddyUserAgent,
		// product.ts:415-421 未定义 userAgentByModelFamily。
		AttributionName:      "CodeBuddy",
		ClientVersion:        "1.106.1",
		CLIVersion:           "2.137.1",
		DefaultCredentialRef: "BUDDY_INTL_ACCESS_TOKEN",
		AppendSessionParams:  false,
		// product.ts:421 复用 WorkBuddy 的兜底表。
		FallbackModels: workBuddyFallbackModels,
	},
	{
		ID:          "workbuddy-cn",
		LogoURL:     brandicons.WorkBuddy,
		ConfigValue: ProductWorkBuddyCN,
		Platform:    "workbuddy",
		Endpoint:    APIEndpoint,
		APIDomain:   APIDomain,
		DisplayName: "WorkBuddy (国内版)",
		ProductCode: "workbuddy",
		UserAgent:   BuddyUserAgent,
		// product.ts:439 未定义 userAgentByModelFamily。
		AttributionName:      "WorkBuddy",
		ClientVersion:        "5.5.4",
		CLIVersion:           "5.5.4",
		DefaultCredentialRef: "WORKBUDDY_CN_ACCESS_TOKEN",
		AppendSessionParams:  true,
		PluginVersion:        "5.5.4",
		// product.ts:450 复用 CodeBuddy 的兜底表。
		FallbackModels: codeBuddyFallbackModels,
	},
	{
		ID:          "workbuddy",
		LogoURL:     brandicons.WorkBuddy,
		ConfigValue: ProductWorkBuddy,
		Platform:    "workbuddy-ai",
		Endpoint:    "https://www.workbuddy.ai",
		APIDomain:   "www.workbuddy.ai",
		DisplayName: "WorkBuddy (国际版)",
		ProductCode: "workbuddy",
		UserAgent:   workBuddyUAIntl,
		// product.ts:378-388：国际版模型线用国际版形态，国内系模型用国内形态。
		UserAgentByModelFamily: []userAgentRule{
			{Match: "gpt-", UA: workBuddyUAIntl},
			{Match: "gemini-", UA: workBuddyUAIntl},
			{Match: "claude-", UA: workBuddyUAIntl},
			{Match: "glm-", UA: workBuddyUACN},
			{Match: "hy", UA: workBuddyUACN},
			{Match: "kimi-", UA: workBuddyUACN},
			{Match: "minimax-", UA: workBuddyUACN},
		},
		AttributionName:      "WorkBuddy",
		ClientVersion:        "5.5.2",
		CLIVersion:           "5.5.2",
		DefaultCredentialRef: "WORKBUDDY_ACCESS_TOKEN",
		AppendSessionParams:  true,
		PluginVersion:        "5.5.2",
		FallbackModels:       workBuddyFallbackModels,
	},
}

// WorkBuddy UA 分档常量，product.ts:70-71。
const (
	workBuddyUAIntl = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	workBuddyUACN   = "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"
)

// ProductIDs lists the config enum values in table order.
func ProductIDs() []string {
	out := make([]string, 0, len(Products))
	for _, product := range Products {
		out = append(out, product.ConfigValue)
	}
	return out
}

// ProductDefault returns the product this build serves when nothing is
// configured. It is Products[0] unless the build pinned another one.
//
// It deliberately does not go through productByConfigValue: that function falls
// back to this one, so delegating would recurse forever on a bad pin.
func ProductDefault() productConfig {
	wanted := strings.ToLower(strings.TrimSpace(DefaultProduct))
	for _, product := range Products {
		if product.ConfigValue == wanted || product.ID == wanted {
			return product
		}
	}
	return Products[0]
}

// productByConfigValue resolves the `product` setting. Unknown values fall back
// to the default product so a typo can never leave the plugin endpoint-less.
func productByConfigValue(value string) (productConfig, bool) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, product := range Products {
		if product.ConfigValue == normalized {
			return product, true
		}
		// Tolerate the Jet-Hub provider ids too: a user migrating auth files
		// may naturally type `buddy` or `workbuddy-cn`.
		if product.ID == normalized {
			return product, true
		}
	}
	return ProductDefault(), false
}

// productByID resolves a Jet-Hub provider id stored inside an auth file.
func productByID(id string) (productConfig, bool) {
	normalized := strings.ToLower(strings.TrimSpace(id))
	for _, product := range Products {
		if product.ID == normalized || product.ConfigValue == normalized {
			return product, true
		}
	}
	return productConfig{}, false
}

// resolveUserAgent is the Go port of `resolveUserAgent` (product.ts:487-492):
// first matching prefix wins, otherwise the product default.
func resolveUserAgent(product productConfig, model string) string {
	for _, rule := range product.UserAgentByModelFamily {
		if strings.HasPrefix(model, rule.Match) {
			return rule.UA
		}
	}
	if product.UserAgent != "" {
		return product.UserAgent
	}
	return BuddyUserAgent
}

// urlFor joins a product endpoint with one of the `/v2/plugin/*` paths.
func (p productConfig) urlFor(path string) string {
	return strings.TrimRight(p.Endpoint, "/") + path
}

// ── 插件配置 ──

// Config is the per-instance plugin configuration decoded from the `config_yaml`
// subtree the host passes in plugin.register / plugin.reconfigure.
type Config struct {
	Enabled  bool
	Priority int

	// Product selects the CodeBuddy-family endpoint. Empty or unknown values
	// resolve to `codebuddy`. One plugin instance serves exactly ONE product;
	// accounts created under a different product keep working because the
	// product id is persisted in the auth file (see Credential.Product).
	Product string
	// DiscoverModels enables GET /v3/config (and the scoped enterprise model
	// endpoint) for the per-account catalog. When false the built-in fallback
	// table is used and no model request is made.
	DiscoverModels bool
	// DefaultMaxTokens is applied when neither the request nor the remote model
	// metadata declares an output cap. 0 means "do not send max_tokens".
	DefaultMaxTokens int
	// PromptCacheKey attaches a stable prompt_cache_key to chat requests
	// (buddy-adapter.ts:998).
	PromptCacheKey bool
	// ThinkingEnabled injects `thinking:{type:"enabled"}` for deepseek models
	// (buddy-adapter.ts:1027).
	ThinkingEnabled bool
	// FirstTokenTimeoutMS / ChunkTimeoutMS bound upstream SSE idleness.
	FirstTokenTimeoutMS int
	ChunkTimeoutMS      int
	// ModelCacheTTLMS controls how long a discovered model list is reused.
	ModelCacheTTLMS int
	// CheckinEnabled allows the daily check-in route to call upstream.
	CheckinEnabled bool
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:             true,
		Product:             ProductDefault().ConfigValue,
		DiscoverModels:      true,
		DefaultMaxTokens:    0,
		PromptCacheKey:      true,
		ThinkingEnabled:     true,
		FirstTokenTimeoutMS: 120000,
		ChunkTimeoutMS:      120000,
		ModelCacheTTLMS:     2 * 60 * 60 * 1000,
		CheckinEnabled:      true,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is first decoded into a generic mapping and each key is then
// coerced individually. Three properties follow, and all three matter in
// practice:
//
//   - **block and flow style are both accepted.** The host hands the instance
//     subtree back in whichever style the user wrote it, so
//     `codebuddy: {enabled: true, product: workbuddy}` must work as well as the
//     multi-line form. A hand-written line scanner reads zero keys from the flow
//     form and silently falls back to every default.
//   - values that a YAML 1.2 decoder sees as strings but a user reasonably
//     writes as booleans (`yes`/`no`/`on`/`off`) or as a quoted number still
//     work. Decoding straight into a `bool` field would fail on those, and a
//     failed decode discards the whole document.
//   - one unusable value costs only its own default: each key is coerced on its
//     own, so a typo in one setting cannot reset the others.
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
	cfg.Product = coerceProduct(raw["product"], cfg.Product)
	cfg.DiscoverModels = coerceBool(raw["discover_models"], cfg.DiscoverModels)
	cfg.PromptCacheKey = coerceBool(raw["prompt_cache_key"], cfg.PromptCacheKey)
	cfg.ThinkingEnabled = coerceBool(raw["thinking_enabled"], cfg.ThinkingEnabled)
	cfg.CheckinEnabled = coerceBool(raw["checkin_enabled"], cfg.CheckinEnabled)
	cfg.DefaultMaxTokens = coerceInt(raw["max_tokens"], cfg.DefaultMaxTokens)
	cfg.FirstTokenTimeoutMS = coerceInt(raw["first_token_timeout_ms"], cfg.FirstTokenTimeoutMS)
	cfg.ChunkTimeoutMS = coerceInt(raw["chunk_timeout_ms"], cfg.ChunkTimeoutMS)
	cfg.ModelCacheTTLMS = coerceInt(raw["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	return cfg
}

// coerceProduct accepts only the four known products (including their Jet-Hub
// provider-id spellings) and keeps the fallback otherwise.
func coerceProduct(value any, fallback string) string {
	text := coerceString(value, "")
	if text == "" {
		return fallback
	}
	if product, ok := productByConfigValue(text); ok {
		return product.ConfigValue
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

// normalizeProductValue maps a user-supplied product name to a config value.
func normalizeProductValue(value string) string {
	product, _ := productByConfigValue(value)
	return product.ConfigValue
}

// ConfigFields describes the settings the CPA management UI renders.
// configField mirrors pluginapi.ConfigField; the plugin package owns its own
// type so the ABI layer stays free of provider specifics (same as codearts).
func ConfigFields() []configField {
	return []configField{
		{
			Name: "product", Type: "enum", EnumValues: ProductIDs(),
			Description: "产品族：codebuddy=腾讯 CodeBuddy 国内版（copilot.tencent.com）、" +
				"codebuddy-intl=国际版（www.codebuddy.ai）、" +
				"workbuddy-cn=WorkBuddy 国内版（copilot.tencent.com）、" +
				"workbuddy=WorkBuddy 国际版（www.workbuddy.ai）。" +
				"每个插件实例只服务一个产品；已登录账号的产品记在凭据里，改本项不影响旧账号",
		},
		{Name: "discover_models", Type: "boolean", Description: "是否调用远端接口（/v3/config 与企业模型端点）动态发现模型列表，关闭则使用内置兜底表"},
		{Name: "prompt_cache_key", Type: "boolean", Description: "为请求附加稳定的 prompt_cache_key 以命中提示缓存（实测可显著降低计费）"},
		{Name: "thinking_enabled", Type: "boolean", Description: "对 deepseek 系模型注入 thinking:{type:'enabled'}（reasoning_effort 才是真正的思考开关）"},
		{Name: "checkin_enabled", Type: "boolean", Description: "允许管理页/额度接口调用每日签到（仅国内版 CodeBuddy 支持）"},
		{Name: "max_tokens", Type: "integer", Description: "请求、远端元数据与兜底表都未给出时的默认输出上限；0 表示不下发该字段"},
		{Name: "first_token_timeout_ms", Type: "integer", Description: "等待上游首个 SSE 分片的超时，毫秒"},
		{Name: "chunk_timeout_ms", Type: "integer", Description: "两个上游 SSE 分片之间的超时，毫秒"},
		{Name: "model_cache_ttl_ms", Type: "integer", Description: "动态模型列表缓存时长，毫秒"},
	}
}

// configField mirrors pluginapi.ConfigField without importing it here.
type configField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}
