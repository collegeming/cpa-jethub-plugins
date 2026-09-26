package main

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider identity and every upstream endpoint used by the adapter.
//
// Jet-Hub registers two provider ids for ByteDance TRAE (`trae` for the CN site
// and `trae-intl` for the international site, trae-product.ts:304-373) because
// CPA has exactly one provider key per plugin. This port keeps the single key
// `trae` and exposes the region as a configuration field instead; the field
// selects the endpoint set below. Credentials are *not* interchangeable between
// the two regions, which is why the credential records the `api_host` it was
// minted against and refresh prefers it (trae-auth.ts:325).
const (
	// ProviderKey is the provider key returned by auth.identifier and used for
	// every model this plugin registers.
	ProviderKey = "trae"
	// DisplayName is the human-readable plugin name.
	DisplayName = "TRAE (字节)"

	// RegionCN / RegionINTL are the accepted values of the `region` setting.
	RegionCN   = "trae"
	RegionINTL = "trae-intl"

	// Path constants, verbatim from trae.ts:44-65 (no trailing slash).
	ChatPath          = "/api/agent/v3/llm_utils_chat"              // trae.ts:44
	ModelsPath        = "/api/ide/v1/get_detail_param"              // trae.ts:46 (single channel, unused for catalogs)
	BatchModelsPath   = "/api/ide/v1/batch_get_detail_param"        // trae.ts:53 (multi channel, the real CN IDE endpoint)
	ExchangePath      = "/cloudide/api/v3/trae/oauth/ExchangeToken" // trae.ts:55
	UserInfoPath      = "/cloudide/api/v3/trae/GetUserInfo"         // trae.ts:57
	CheckinStatusPath = "/trae/api/v2/ug/checkin_credits/status"    // trae.ts:59
	CheckinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"     // trae.ts:61
	EntUsagePath      = "/trae/api/v2/pay/ide_user_ent_usage"       // trae.ts:63
	CallbackPath      = "/authorize"                                // trae.ts:65

	// DefaultCallbackPort is the port the TRAE login portal is documented to call
	// back on (trae-oauth.ts:40). internal/jethub/oauthcb allocates ephemeral
	// ports, so this is passed as its MinPort floor and the real bound port is
	// echoed into `auth_callback_url` — the portal calls back whatever URL the
	// login request carried (trae-oauth.ts:508-535).
	DefaultCallbackPort = 18080

	// Timeouts. Both mirror trae.ts:68-70.
	DefaultRequestTimeoutMS = 30000
	DefaultLoginTimeoutMS   = 10 * 60 * 1000

	// SOLO body constants (trae.ts:1258, :1265).
	DefaultConfigName = "glm-5.2"
	// Function solo_work_lite is the default channel. The adapter always sends
	// the channel the model was listed under; this is only the fallback
	// (trae.ts:1265, trae-adapter.ts:557-560).
	DefaultFunction = "solo_work_lite"

	// Max-mode constants (trae.ts:1275-1286).
	MaxContextTokens = 1000000
	MaxPromptTokens  = 936000
	MaxOutputTokens  = 64000
	MaxModeType      = 1

	// DefaultMaxCompletionTokens clamps a single response (trae.ts:1196).
	DefaultMaxCompletionTokens = 64000
	// DefaultMaxHistoryChars is the request-body character budget; upstream
	// silently ends the stream above roughly 500K characters (trae.ts:1311-1315).
	DefaultMaxHistoryChars = 480000

	// DefaultModelCacheTTLMS bounds how long a discovered catalog is reused.
	DefaultModelCacheTTLMS = 30000

	// DefaultChannels is the channel list pulled by the batch catalog call.
	// Order matters: it is the priority order (trae-product.ts:258-268).
	DefaultChannels = "solo_agent,solo_work_lite,solo_agent_remote"
)

// fallbackModel is one entry of the product-level fallback catalog
// (trae-product.ts:210-243, ported from the Go trae2api staticModels table).
type fallbackModel struct {
	ID            string
	Name          string
	ContextWindow int64
	// Hidden models are upstream-internal entries that must not appear in the
	// catalog even when the remote listing is unavailable
	// (trae-product.ts:38-44, trae-adapter.ts:656-660).
	Hidden bool
}

// product is the per-region endpoint and identity configuration. It mirrors
// Jet-Hub's `TraeProduct` (trae-product.ts:51-159); every host below carries the
// `file:line` it was read from.
type product struct {
	ID                      string
	Site                    string
	DisplayName             string
	AgentHost               string // chat + model catalog
	UGHost                  string // check-in / credit balance
	OAuthHost               string // ExchangeToken / GetUserInfo
	ConsoleHost             string // browser login portal
	ClientID                string
	AppID                   string
	IDEVersion              string
	IDECode                 string
	DeviceBrand             string
	OSVersion               string
	UserAgent               string
	PluginVersion           string
	DefaultCredentialRef    string
	FallbackMaxOutputTokens int64
	Fallback                []fallbackModel
}

// productFor selects the endpoint set for a `region` setting value. Unknown
// values fall back to the CN site, which is the default.
func productFor(region string) product {
	if strings.EqualFold(strings.TrimSpace(region), RegionINTL) {
		return productINTL
	}
	return productCN
}

// productCN is the domestic site (trae-product.ts:162-168, :304-328).
var productCN = product{
	ID:                      RegionCN,
	Site:                    "cn",
	DisplayName:             "TRAE (字节)",
	AgentHost:               "https://trae-api-cn.mchost.guru", // trae-product.ts:162
	UGHost:                  "https://api.trae.cn",             // trae-product.ts:164
	OAuthHost:               "https://api.trae.com.cn",         // trae-product.ts:166
	ConsoleHost:             "https://www.trae.cn",             // trae-product.ts:168
	ClientID:                "en1oxy7wnw8j9n",                  // trae-product.ts:312
	AppID:                   "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8",
	IDEVersion:              "0.1.52",            // trae-product.ts:314
	IDECode:                 "20260811",          // trae-product.ts:315
	DeviceBrand:             "Apple",             // trae-product.ts:316
	OSVersion:               "macOS 15.7.4",      // trae-product.ts:317
	UserAgent:               "Trae/0.1.52",       // trae-product.ts:326
	PluginVersion:           "2.3.62834",         // trae-product.ts:324
	DefaultCredentialRef:    "TRAE_ACCESS_TOKEN", // trae-product.ts:325
	FallbackMaxOutputTokens: 32000,               // trae-product.ts:320
	Fallback:                fallbackModels(),
}

// productINTL is the international site (trae-product.ts:196-202, :349-373).
//
// The identity fields (client id / app id / IDE version / plugin version) are
// deliberately the same values as the CN product: Jet-Hub could not verify an
// INTL SOLO identity and refused to invent one (trae-product.ts:341-347). Only
// the hosts differ.
var productINTL = product{
	ID:                      RegionINTL,
	Site:                    "global",
	DisplayName:             "TRAE (国际版)",
	AgentHost:               "https://api5-normal-alisg.mchost.guru", // trae-product.ts:196
	UGHost:                  "https://api.trae.ai",                   // trae-product.ts:198
	OAuthHost:               "https://api.trae.ai",                   // trae-product.ts:200
	ConsoleHost:             "https://www.trae.ai",                   // trae-product.ts:202
	ClientID:                "en1oxy7wnw8j9n",                        // trae-product.ts:357
	AppID:                   "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8",  // trae-product.ts:358
	IDEVersion:              "0.1.52",                                // trae-product.ts:359
	IDECode:                 "20260811",                              // trae-product.ts:360
	DeviceBrand:             "Apple",                                 // trae-product.ts:361
	OSVersion:               "macOS 15.7.4",                          // trae-product.ts:362
	UserAgent:               "Trae/0.1.52",                           // trae-product.ts:371
	PluginVersion:           "2.3.62834",                             // trae-product.ts:369
	DefaultCredentialRef:    "TRAE_INTL_ACCESS_TOKEN",                // trae-product.ts:370
	FallbackMaxOutputTokens: 32000,                                   // trae-product.ts:365
	Fallback:                fallbackModels(),
}

// fallbackModels returns the 32-entry static catalog (trae-product.ts:210-243).
// Values are estimates: the remote `model_detail_list[].max_tokens` and
// `context_window_tokens.dev` are authoritative whenever discovery succeeds.
func fallbackModels() []fallbackModel {
	return []fallbackModel{
		{ID: "DeepSeek-V4-Flash-Official", Name: "DeepSeek V4 Flash Official", ContextWindow: 200000},
		{ID: "Doubao-Seed-2.1-Pro", Name: "Doubao Seed 2.1 Pro", ContextWindow: 200000},
		{ID: "seed-code-pro-0430", Name: "Seed Code Pro 0430", ContextWindow: 200000},
		{ID: "Doubao-Seed-2.1-Turbo", Name: "Doubao Seed 2.1 Turbo", ContextWindow: 200000},
		{ID: "Doubao-Seed-2.0-Code", Name: "Doubao Seed 2.0 Code", ContextWindow: 200000},
		{ID: "browser_use_subagent", Name: "Browser Use Subagent", ContextWindow: 200000, Hidden: true},
		{ID: "glm-5.2", Name: "GLM-5.2", ContextWindow: 200000},
		{ID: "glm-5-turbo", Name: "GLM-5 Turbo", ContextWindow: 200000},
		{ID: "glm-5", Name: "GLM-5", ContextWindow: 200000},
		{ID: "DeepSeek-V4-Pro", Name: "DeepSeek V4 Pro", ContextWindow: 200000},
		{ID: "DeepSeek-V4-Flash", Name: "DeepSeek V4 Flash", ContextWindow: 200000},
		{ID: "kimi-k3", Name: "Kimi K3", ContextWindow: 200000},
		{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", ContextWindow: 200000},
		{ID: "kimi-k2.6", Name: "Kimi K2.6", ContextWindow: 200000},
		{ID: "minimax-m3", Name: "MiniMax M3", ContextWindow: 200000},
		{ID: "qwen-3.7-plus", Name: "Qwen 3.7 Plus", ContextWindow: 200000},
		{ID: "sagitta", Name: "Sagitta", ContextWindow: 200000},
		{ID: "aquila", Name: "Aquila", ContextWindow: 200000},
		{ID: "custom_model_gemini", Name: "Custom Gemini", ContextWindow: 200000},
		{ID: "custom_model_placeholder", Name: "Custom Placeholder", ContextWindow: 200000},
		{ID: "custom_model_1M_text", Name: "Custom 1M Text", ContextWindow: 200000},
		{ID: "custom_model_1M", Name: "Custom 1M", ContextWindow: 200000},
		{ID: "custom_model_kimi", Name: "Custom Kimi", ContextWindow: 200000},
		{ID: "custom_model_claude", Name: "Custom Claude", ContextWindow: 200000},
		{ID: "custom_model_gpt-5", Name: "Custom GPT-5", ContextWindow: 200000},
		{ID: "custom_model_no-fc", Name: "Custom No-FC", ContextWindow: 200000},
		{ID: "custom_model_deepseek_chat", Name: "Custom DeepSeek Chat", ContextWindow: 200000},
		{ID: "custom_model_deepseek_reasoner", Name: "Custom DeepSeek Reasoner", ContextWindow: 200000},
		{ID: "custom_model_deepseek_v4", Name: "Custom DeepSeek V4", ContextWindow: 200000},
		{ID: "explore_sub_agent_v13", Name: "Explore Sub Agent V13", ContextWindow: 200000, Hidden: true},
		{ID: "explore_sub_agent_v2", Name: "Explore Sub Agent V2", ContextWindow: 200000, Hidden: true},
		{ID: "summary", Name: "Summary", ContextWindow: 200000, Hidden: true},
	}
}

// Config is the per-instance plugin configuration decoded from the `config_yaml`
// subtree the host passes in plugin.register / plugin.reconfigure.
type Config struct {
	Enabled  bool
	Priority int

	// Region selects the endpoint set: "trae" (CN, default) or "trae-intl".
	Region string
	// DiscoverModels enables the remote catalog call. When false the static
	// fallback list is used.
	DiscoverModels bool
	// Channels is the list of SOLO channels requested from
	// batch_get_detail_param. Order is the priority order.
	Channels []string
	// DefaultChannel is the `function` sent when the catalog does not know which
	// channel lists a model.
	DefaultChannel string
	// MaxMode enables the 1M-context Max session fields. It only ever applies to
	// models whose remote config marks `max_mode === true`.
	MaxMode bool
	// MaxModeModels optionally restricts Max mode to a list of model ids. Empty
	// or containing "*" means every eligible model.
	MaxModeModels []string
	// MaxCompletionTokens clamps a single response; 0 disables the clamp.
	MaxCompletionTokens int
	// MaxHistoryChars is the request-body character budget.
	MaxHistoryChars int
	// RotateMachineID opts into machine-fingerprint rotation. Off by default:
	// the device identity is supposed to be stable for the life of a credential.
	RotateMachineID bool
	// ModelCacheTTLMS is how long a discovered catalog is reused.
	ModelCacheTTLMS int
	// CallbackPort pins the loopback callback listener to an exact port. The
	// TRAE portal echoes `auth_callback_url` verbatim, so the port is ours to
	// choose.
	//
	// Zero (the default) keeps the reference behaviour: prefer 18080 and fall
	// back to any free port at or above it. An explicit value pins the listener
	// and fails if the port is taken — which is what a container deployment
	// needs, because the port must match the published mapping and a silent
	// fallback would hand the browser an unroutable URL.
	CallbackPort int
	// CallbackBindHost is the local address the listener binds. Defaults to
	// 127.0.0.1; a container deployment must use 0.0.0.0 so the published port
	// reaches it.
	CallbackBindHost string
	// CallbackPublicHost and CallbackPublicPort describe the address the
	// BROWSER dials. Defaults to 127.0.0.1 and the bound port, which is correct
	// for a published container port.
	CallbackPublicHost string
	CallbackPublicPort int
	// LoginTimeoutMS bounds an interactive sign-in.
	LoginTimeoutMS int
	// RequestTimeoutMS bounds the control-plane requests (chat streams are not
	// affected because the executor buffers one host HTTP call).
	RequestTimeoutMS int
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:             true,
		Region:              RegionCN,
		DiscoverModels:      true,
		Channels:            splitList(DefaultChannels),
		DefaultChannel:      DefaultFunction,
		MaxMode:             true,
		MaxCompletionTokens: DefaultMaxCompletionTokens,
		MaxHistoryChars:     DefaultMaxHistoryChars,
		RotateMachineID:     false,
		ModelCacheTTLMS:     DefaultModelCacheTTLMS,
		CallbackPort:        0,
		LoginTimeoutMS:      DefaultLoginTimeoutMS,
		RequestTimeoutMS:    DefaultRequestTimeoutMS,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is decoded into a generic mapping first and every key is coerced
// individually. Three properties follow, all of which matter in practice:
//
//   - block and flow style are both accepted. The host hands the instance subtree
//     back in whichever style the user wrote it, and a line-oriented scanner
//     silently ignores `{enabled: true, ...}` entirely, leaving every value at its
//     default with no error at all.
//   - Values a YAML 1.2 decoder reports as strings but a user reasonably writes
//     as booleans (`yes`/`no`/`on`/`off`) or numbers (quoted) still work. A typed
//     decode would fail on them and discard the whole document.
//   - One unusable value costs only its own default, never the whole document.
func ConfigFromYAML(document []byte) Config {
	cfg := DefaultConfig()
	if len(document) == 0 {
		return cfg
	}
	var raw map[string]any
	if errUnmarshal := yaml.Unmarshal(document, &raw); errUnmarshal != nil {
		return DefaultConfig()
	}
	if raw == nil {
		return DefaultConfig()
	}

	cfg.Enabled = coerceBool(raw["enabled"], cfg.Enabled)
	cfg.Priority = coerceInt(raw["priority"], cfg.Priority)
	cfg.Region = coerceRegion(raw["region"], cfg.Region)
	cfg.DiscoverModels = coerceBool(raw["discover_models"], cfg.DiscoverModels)
	if channels := coerceList(raw["channels"]); len(channels) > 0 {
		cfg.Channels = channels
	}
	cfg.DefaultChannel = coerceString(raw["default_channel"], cfg.DefaultChannel)
	cfg.MaxMode = coerceBool(raw["max_mode"], cfg.MaxMode)
	cfg.MaxModeModels = coerceList(raw["max_mode_models"])
	cfg.MaxCompletionTokens = coerceInt(raw["max_completion_tokens"], cfg.MaxCompletionTokens)
	cfg.MaxHistoryChars = coerceInt(raw["max_history_chars"], cfg.MaxHistoryChars)
	cfg.RotateMachineID = coerceBool(raw["rotate_machine_id"], cfg.RotateMachineID)
	cfg.ModelCacheTTLMS = coerceInt(raw["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	cfg.CallbackPort = coerceInt(raw["callback_port"], cfg.CallbackPort)
	cfg.CallbackBindHost = coerceString(raw["callback_bind_host"], cfg.CallbackBindHost)
	cfg.CallbackPublicHost = coerceString(raw["callback_public_host"], cfg.CallbackPublicHost)
	cfg.CallbackPublicPort = coerceInt(raw["callback_public_port"], cfg.CallbackPublicPort)
	cfg.LoginTimeoutMS = coerceInt(raw["login_timeout_ms"], cfg.LoginTimeoutMS)
	cfg.RequestTimeoutMS = coerceInt(raw["request_timeout_ms"], cfg.RequestTimeoutMS)
	return cfg
}

// coerceRegion accepts only the two known sites.
func coerceRegion(value any, fallback string) string {
	switch strings.ToLower(coerceString(value, "")) {
	case RegionCN:
		return RegionCN
	case RegionINTL:
		return RegionINTL
	default:
		return fallback
	}
}

// coerceBool accepts a real boolean, a number, or the string spellings users
// write in configuration files. YAML 1.2 decodes `no`/`off`/`yes`/`on` as
// strings, which is exactly why this coercion exists.
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

// coerceList accepts a comma-separated string or a YAML sequence. An empty
// result means "not configured", which callers treat as "keep the default".
func coerceList(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return splitList(typed)
	case []any:
		out := []string{}
		for _, item := range typed {
			if text := coerceString(item, ""); text != "" {
				out = append(out, text)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []string:
		if len(typed) == 0 {
			return nil
		}
		return typed
	default:
		return nil
	}
}

// ConfigFields describes the settings the CPA management UI renders.
func ConfigFields() []configField {
	return []configField{
		{Name: "region", Type: "enum", EnumValues: []string{RegionCN, RegionINTL},
			Description: "区域站点：trae=国内（trae.cn，默认），trae-intl=国际（trae.ai）。两侧端点与登录态互不相通"},
		{Name: "discover_models", Type: "boolean",
			Description: "是否调用 batch_get_detail_param 动态发现模型目录（含通道归属）；关闭则使用内置兜底列表"},
		{Name: "channels", Type: "string",
			Description: "要拉取的 SOLO 通道，逗号分隔，顺序即优先级（默认 solo_agent,solo_work_lite,solo_agent_remote）"},
		{Name: "default_channel", Type: "string",
			Description: "远端目录未标明通道时使用的 function（默认 solo_work_lite）"},
		{Name: "max_mode", Type: "boolean",
			Description: "启用 Max 模式（1M 上下文，默认开）。仅对远端 display_config.max_mode=true 的模型生效"},
		{Name: "max_mode_models", Type: "string",
			Description: "Max 模式白名单，逗号分隔；留空或含 * 表示全部合格模型"},
		{Name: "max_completion_tokens", Type: "integer",
			Description: "单次响应输出上限收敛值（默认 64000，0 表示不收敛）"},
		{Name: "max_history_chars", Type: "integer",
			Description: "请求体历史字符预算（默认 480000；上游超过约 500K 会静默断流）"},
		{Name: "rotate_machine_id", Type: "boolean",
			Description: "每 4 次请求轮换 machine_id（默认关）。设备身份漂移可能触发重新登录，仅在集中 401/风控时启用"},
		{Name: "model_cache_ttl_ms", Type: "integer",
			Description: "远端模型目录缓存时长，毫秒（默认 30000）"},
		{Name: "callback_port", Type: "integer",
			Description: "登录回调端口：留空=优先 18080 并自动改用其它空闲端口（仅本机部署可用）；容器部署填固定端口并在 compose 中发布同名端口"},
		{Name: "callback_bind_host", Type: "string",
			Description: "回调监听绑定的本机地址（容器部署填 0.0.0.0，默认 127.0.0.1）"},
		{Name: "callback_public_host", Type: "string",
			Description: "浏览器访问回调时使用的主机名（默认 127.0.0.1）"},
		{Name: "callback_public_port", Type: "integer",
			Description: "浏览器访问回调时使用的端口（默认与 callback_port 相同，仅当宿主机映射端口不同时填写）"},
		{Name: "login_timeout_ms", Type: "integer",
			Description: "交互式登录的等待上限，毫秒（默认 600000）"},
		{Name: "request_timeout_ms", Type: "integer",
			Description: "控制面（模型目录/签到/token 交换）请求超时，毫秒（默认 30000）"},
	}
}

// configField mirrors pluginapi.ConfigField; the plugin owns its own type so the
// ABI layer stays free of provider specifics.
type configField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}

// splitList parses a comma-separated list, trimming blanks.
func splitList(value string) []string {
	out := []string{}
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
