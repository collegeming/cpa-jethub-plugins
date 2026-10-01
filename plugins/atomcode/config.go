package main

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProviderKey is the provider key this plugin owns. The host derives the auth
// provider id, the management namespace and the resource mount from it, so the
// built library file must be named `atomcode.so` (plugins/atomcode/main.go).
const ProviderKey = "atomcode"

// PluginVersion is the adapter version.
const PluginVersion = "0.1.0"

// OfficialClientVersion is the AtomCode release whose wire contract this adapter
// mirrors. Kept for documentation and for the reference citations in this
// package; it is deliberately NOT sent as a User-Agent (see AdapterUserAgent).
const OfficialClientVersion = "5.2.0"

// OfficialUserAgent is what the official client identifies itself as
// (`crates/atomcode-auth/src/oauth.rs:24`: `concat!("atomcode/", CARGO_PKG_VERSION)`).
//
// ⚠️ This adapter must NEVER send it. The gateway enforces the closed-source
// request signature *by User-Agent*: any `atomcode/<version>` identity gets
//
//	403 {"detail":{"code":"ATOMCODE_SIG_MISSING","message":"请升级到最新版 AtomCode 后再试。"}}
//
// while any other identity is served normally. Measured 2026-10-01 against
// `POST https://api-ai.gitcode.com/v1/chat/completions` with one unchanged valid
// bearer token:
//
//	(no User-Agent)               -> HTTP 200, real completion
//	curl/8.5.0                    -> HTTP 200, real completion
//	python-urllib/3.11            -> HTTP 200, real completion
//	cpa-jethub-atomcode/0.1.0     -> HTTP 200, real completion
//	atomcode/5.2.0                -> HTTP 403 ATOMCODE_SIG_MISSING
//	atomcode/5.1.0                -> HTTP 403 ATOMCODE_SIG_MISSING
//
// The constant exists so the trap is documented in one place rather than
// rediscovered from a 403.
const OfficialUserAgent = "atomcode/" + OfficialClientVersion

// AdapterUserAgent identifies this adapter honestly on every request it makes.
//
// Claiming to be the official client is both untrue and self-defeating: it is
// exactly what arms the signature gate above.
const AdapterUserAgent = "cpa-jethub-atomcode/" + PluginVersion

// Hosted endpoints. Each has a settings override for the same reason the
// reference keeps them in one module (`crates/atomcode-config/src/endpoints.rs:1-33`):
// retargeting a deployment must not mean editing call sites.
const (
	// DefaultBrokerBase is the OAuth/auth broker, `acs.atomgit.com`
	// (`crates/atomcode-config/src/endpoints.rs:90`).
	DefaultBrokerBase = "https://acs.atomgit.com"
	// DefaultCodingPlanAPIBase serves the CodingPlan REST API
	// (`crates/atomcode-config/src/endpoints.rs:91`).
	DefaultCodingPlanAPIBase = "https://api.gitcode.com/api/v5"

	// DefaultGatewayBase is the OpenAI-compatible chat gateway this adapter
	// talks to.
	//
	// ⚠️ This is deliberately NOT the reference's default
	// (`https://llm-api.atomgit.com/v1`, `endpoints.rs:92`). The reference
	// reaches that host only from an official build, because it demands the
	// CLOSED-SOURCE request signature (`atomcode-codingplan-crypto::sign_v1`,
	// overlaid by `scripts/build-official.sh`; the open-source stub at
	// `crates/atomcode-codingplan-crypto/src/lib.rs:15` is `unreachable!`).
	// Without it the gateway answers
	// `403 {"detail":{"code":"ATOMCODE_SIG_MISSING"}}`.
	//
	// `api-ai.gitcode.com` is the sibling gateway host the same reference
	// recognises as first-party (`crates/atomcode-auth/src/gateway_crypto.rs:140-142`
	// lists it beside the other two). It serves the same models from the same
	// OAuth bearer WITHOUT a signature, provided the request does not claim to
	// be the official client — see AdapterUserAgent for the measured table.
	DefaultGatewayBase = "https://api-ai.gitcode.com/v1"
)

// Broker paths, relative to the broker base (`crates/atomcode-auth/src/oauth.rs:38-52`).
const (
	brokerLoginPath   = "/auth/login"
	brokerCheckPath   = "/auth/check"
	brokerTokenPath   = "/auth/token"
	brokerRefreshPath = "/oauth/refresh"
)

// BrokerProvider is the `provider` query parameter `start_login` sends
// (`crates/atomcode-auth/src/oauth.rs`, `attempt_login`: `.query(&[("provider","atomgit")])`).
const BrokerProvider = "atomgit"

// CodingPlan REST paths, relative to the CodingPlan API base
// (`crates/atomcode-codingplan/src/client.rs:213,254,287,318`).
const (
	codingPlanClaimPath  = "/coding-plan/claim-v2"
	codingPlanModelsPath = "/coding-plan/models-v2"
	codingPlanStatusPath = "/coding-plan/status-v2"
	codingPlanUsagePath  = "/coding-plan/usage"
)

// GatewayPath is the chat-completions route appended to the gateway base
// (`crates/atomcode-auth/src/gateway_crypto.rs:148-157`
// `canonical_chat_completions_path`).
const GatewayPath = "/chat/completions"

// Timeouts and windows. Every value mirrors the reference's own budget
// (`crates/atomcode-auth/src/oauth.rs` builds both clients with a 5s connect
// timeout and a 10s total timeout).
const (
	// RequestTimeoutMS bounds auth, plan and usage calls.
	RequestTimeoutMS = 20_000
	// CatalogueTimeoutMS bounds the model-catalogue call.
	CatalogueTimeoutMS = 15_000
	// LoginTimeoutMS bounds one interactive browser sign-in.
	LoginTimeoutMS = 300_000
	// ModelCacheTTLMS bounds how long a discovered catalogue is reused.
	ModelCacheTTLMS = 2 * 60 * 60 * 1000
	// RefreshWindowSeconds is how long before expiry a credential is renewed.
	//
	// The reference refreshes only once `created_at + expires_in` is in the
	// past (`crates/atomcode-auth/src/oauth.rs:1050-1060`), which under a lazy
	// scheduler would ship an already-dead token. Refreshing early is strictly
	// safer here for a second reason: refresh ROTATES the refresh token, so a
	// refresh that happens *after* the access token died still succeeds while a
	// request made with the dead token does not.
	RefreshWindowSeconds = 3600
	// PlanTypeAuto asks the adapter to resolve the plan tier from status-v2.
	PlanTypeAuto = "auto"
)

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
type Config struct {
	// Enabled is the host's own switch, mirrored for diagnostics.
	Enabled bool
	// Priority orders credentials in the host's scheduler.
	Priority int

	// BrokerBase overrides the OAuth broker host.
	BrokerBase string
	// CodingPlanAPIBase overrides the CodingPlan REST host.
	CodingPlanAPIBase string
	// GatewayBase overrides the chat-completions gateway.
	//
	// Pointing this at `https://llm-api.atomgit.com/v1` will NOT work: that host
	// requires the closed-source signature. See DefaultGatewayBase.
	GatewayBase string

	// DiscoverModels enables the live `models-v2` catalogue. On by default:
	// the reference registers providers from the server payload and keeps no
	// bundled table at all (`crates/atomcode-codingplan/src/setup.rs:924`).
	DiscoverModels bool
	// ExtraModels are model ids served by the gateway but NOT advertised by
	// `models-v2` for this account.
	//
	// The two lists genuinely differ, and the difference is worth exposing
	// deliberately rather than discovering by accident. Measured 2026-10-01 on
	// one `CodingPlan Lite-体验版` account with `POST /chat/completions`:
	//
	//	deepseek-flash                200, real answer   (absent from models-v2)
	//	Qwen/Qwen3-32B                200, real answer   (absent from models-v2)
	//	Qwen/Qwen2-VL-72B             200, real answer   (absent from models-v2)
	//	Qwen/Qwen3-4B-Instruct-2507   200 whose CONTENT is 三方请求失败: 502 …
	//	no-such-model-xyz             200 whose CONTENT is 参数错误
	//
	// So "reachable" is not "advertised", and it is certainly not "works": one
	// of those ids fails inside the gateway while still reporting HTTP 200.
	// That is why this is an operator list and not a bundled one — only the
	// person holding the account can decide an unadvertised model is worth
	// offering, and the plugin must not guess.
	//
	// Ids here are added ON TOP of the discovered catalogue, never instead of
	// it, and duplicates are dropped.
	ExtraModels []string
	// ModelPrefix exposes the account id as a model prefix (`<account>/<model>`).
	ModelPrefix bool
	// ModelCacheTTLMS bounds how long a discovered catalogue is reused.
	ModelCacheTTLMS int

	// PlanType selects the tier queried on `models-v2`: auto, Max, Pro or Lite.
	PlanType string

	// RequestTimeoutMS bounds auth, plan and usage calls.
	RequestTimeoutMS int
	// CatalogueTimeoutMS bounds the catalogue call.
	CatalogueTimeoutMS int
	// LoginTimeoutMS bounds one interactive sign-in.
	LoginTimeoutMS int
	// RefreshWindowSeconds is the pre-refresh window.
	RefreshWindowSeconds int
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:              true,
		BrokerBase:           DefaultBrokerBase,
		CodingPlanAPIBase:    DefaultCodingPlanAPIBase,
		GatewayBase:          DefaultGatewayBase,
		DiscoverModels:       true,
		ExtraModels:          nil,
		ModelPrefix:          true,
		ModelCacheTTLMS:      ModelCacheTTLMS,
		PlanType:             PlanTypeAuto,
		RequestTimeoutMS:     RequestTimeoutMS,
		CatalogueTimeoutMS:   CatalogueTimeoutMS,
		LoginTimeoutMS:       LoginTimeoutMS,
		RefreshWindowSeconds: RefreshWindowSeconds,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is decoded into a generic mapping first and every key is coerced
// on its own, for the three reasons the sibling plugins document: block and flow
// style must both work, values YAML 1.2 calls strings but a user means as numbers
// must still land, and one unrecognised key must not discard the rest.
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
	cfg.BrokerBase = coerceURL(raw["broker_base"], cfg.BrokerBase, DefaultBrokerBase)
	cfg.CodingPlanAPIBase = coerceURL(raw["codingplan_api_base"], cfg.CodingPlanAPIBase, DefaultCodingPlanAPIBase)
	cfg.GatewayBase = coerceURL(raw["gateway_base"], cfg.GatewayBase, DefaultGatewayBase)
	cfg.DiscoverModels = coerceBool(raw["discover_models"], cfg.DiscoverModels)
	cfg.ExtraModels = coerceStringList(raw["extra_models"], cfg.ExtraModels)
	cfg.ModelPrefix = coerceBool(raw["model_prefix"], cfg.ModelPrefix)
	cfg.ModelCacheTTLMS = coerceInt(raw["model_cache_ttl_ms"], cfg.ModelCacheTTLMS)
	cfg.PlanType = coercePlanType(raw["plan_type"], cfg.PlanType)
	cfg.RequestTimeoutMS = coerceInt(raw["request_timeout_ms"], cfg.RequestTimeoutMS)
	cfg.CatalogueTimeoutMS = coerceInt(raw["catalogue_timeout_ms"], cfg.CatalogueTimeoutMS)
	cfg.LoginTimeoutMS = coerceInt(raw["login_timeout_ms"], cfg.LoginTimeoutMS)
	cfg.RefreshWindowSeconds = coerceInt(raw["refresh_window_seconds"], cfg.RefreshWindowSeconds)
	return cfg
}

// coerceURL keeps a base URL usable: trimmed, without a trailing slash, and
// never empty. An empty or whitespace-only value falls back to the default
// rather than producing a relative URL that fails at the first request.
func coerceURL(value any, current, fallback string) string {
	text := current
	if provided, ok := value.(string); ok {
		text = provided
	} else if value != nil {
		text = fallback
	}
	trimmed := strings.TrimRight(strings.TrimSpace(text), "/")
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

// coercePlanType normalises the tier setting. Unrecognised values fall back to
// `auto` rather than being forwarded to the server verbatim: `models-v2`
// computes `plan_available` relative to the requested tier, so a typo would
// silently mark entitlement the account does not have
// (`crates/atomcode-codingplan/src/types.rs:53-70`).
func coercePlanType(value any, fallback string) string {
	provided, ok := value.(string)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(provided)) {
	case "max":
		return planTypeMax
	case "pro":
		return planTypePro
	case "lite":
		return planTypeLite
	case "auto", "":
		return PlanTypeAuto
	default:
		return fallback
	}
}

// coerceStringList reads a list of model ids.
//
// A single string is accepted as a one-element list because that is the shape a
// user reaches for first (`extra_models: deepseek-flash`), and a comma-separated
// string because that is what a shell-minded user writes. Entries are trimmed
// and blanks dropped.
func coerceStringList(value any, fallback []string) []string {
	switch typed := value.(type) {
	case nil:
		return fallback
	case string:
		return splitModelList(typed)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				continue
			}
			out = append(out, splitModelList(text)...)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []string:
		return typed
	default:
		return fallback
	}
}

// splitModelList splits on commas and drops blanks.
func splitModelList(text string) []string {
	parts := strings.Split(text, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// coerceBool accepts every spelling of a boolean a YAML document can produce.
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

// coerceInt accepts the numeric spellings a YAML document can produce.
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

// configField describes one settings control the host renders.
type configField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}

// ConfigFields returns the settings the host shows for this plugin.
func ConfigFields() []configField {
	return []configField{
		{Name: "gateway_base", Type: "string",
			Description: "聊天网关地址（默认 https://api-ai.gitcode.com/v1）。官方默认的 llm-api.atomgit.com 要求闭源请求签名，" +
				"无签名会返回 403 ATOMCODE_SIG_MISSING，因此不要改回该地址"},
		{Name: "broker_base", Type: "string", Description: "AtomGit OAuth 代理地址（默认 https://acs.atomgit.com），一般无需修改"},
		{Name: "codingplan_api_base", Type: "string",
			Description: "CodingPlan REST 地址（默认 https://api.gitcode.com/api/v5），负责模型目录、套餐状态与领取"},
		{Name: "discover_models", Type: "boolean",
			Description: "是否用账号会话实时拉取 models-v2 模型目录（默认开启）。关闭后只使用内置兜底表；" +
				"远端目录拉取失败时也会静默回退，不会报错"},
		{Name: "extra_models", Type: "string",
			Description: "补充模型：网关能调用、但服务端 models-v2 目录已不下发的模型 id（逗号分隔，也可写成 YAML 列表）。" +
				"这些模型会加在实时目录之上。注意「能调用」不等于「可用」——实测 deepseek-flash 返回正常内容，" +
				"而 Qwen/Qwen3-4B-Instruct-2507 会以 200 返回「三方请求失败: 502」，所以请只填自己验证过的模型"},
		{Name: "model_prefix", Type: "boolean", Description: "是否把账号 ID 作为模型前缀暴露（<账号>/<模型>）。关闭后模型列表只显示模型本身的名字"},
		{Name: "model_cache_ttl_ms", Type: "integer", Description: "实时模型目录的缓存时长，毫秒（默认 2 小时）"},
		{Name: "plan_type", Type: "string", EnumValues: []string{"auto", "Max", "Pro", "Lite"},
			Description: "查询 models-v2 时使用的套餐档位（默认 auto：先读 status-v2 的实际档位，读不到才按 Max 查）。" +
				"models-v2 的 plan_available 是相对请求档位算的，档位填高会把不可用的模型标成可用"},
		{Name: "request_timeout_ms", Type: "integer", Description: "auth / 套餐状态 / 用量接口的单次请求超时，毫秒（默认 20000）"},
		{Name: "catalogue_timeout_ms", Type: "integer", Description: "模型目录接口的单次请求超时，毫秒（默认 15000）"},
		{Name: "login_timeout_ms", Type: "integer", Description: "单次浏览器登录会话的存活时长，毫秒（默认 300000 = 5 分钟）"},
		{Name: "refresh_window_seconds", Type: "integer",
			Description: "访问令牌到期前多久提前续期，秒（默认 3600）。续期会轮换 refresh_token，且会让旧 access_token 立即失效，因此提前续期比过期后再续更安全"},
	}
}

// brokerLoginURL resolves the sign-in entry point.
func (c Config) brokerLoginURL() string {
	return c.BrokerBase + brokerLoginPath
}

// brokerCheckURL resolves the poll endpoint.
func (c Config) brokerCheckURL() string {
	return c.BrokerBase + brokerCheckPath
}

// brokerTokenURL resolves the code-for-token endpoint.
func (c Config) brokerTokenURL() string {
	return c.BrokerBase + brokerTokenPath
}

// brokerRefreshURL resolves the refresh endpoint.
func (c Config) brokerRefreshURL() string {
	return c.BrokerBase + brokerRefreshPath
}

// codingPlanURL resolves one CodingPlan REST route.
func (c Config) codingPlanURL(path string) string {
	return c.CodingPlanAPIBase + path
}

// gatewayChatURL resolves the chat-completions route.
func (c Config) gatewayChatURL() string {
	return c.GatewayBase + GatewayPath
}
