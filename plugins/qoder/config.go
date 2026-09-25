package main

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider identity. The plugin owns one provider key; the region (and the
// credential's own region field) selects which site a request goes to.
const (
	// ProviderKey is the stable provider identifier written into CPA auth files.
	ProviderKey = "qoder"
	// DisplayName is the human-readable name shown by management clients.
	DisplayName = "Qoder"
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
)

// Timeouts and polling budgets. Every value is the one Jet-Hub uses.
const (
	// RequestTimeoutMS is the single-request timeout, `QODER_REQUEST_TIMEOUT_MS`
	// (`qoder.ts:14`).
	RequestTimeoutMS = 30_000
	// LoginTimeoutMS is the whole device-code wait, `QODER_LOGIN_TIMEOUT_MS`
	// (`qoder.ts:16`, upstream `L_a = 3e5`).
	LoginTimeoutMS = 300_000
	// PollIntervalMS is the device-token poll interval, `QODER_POLL_INTERVAL_MS`
	// (`qoder.ts:18`, upstream `Djr = 1e3`).
	PollIntervalMS = 1_000
	// PollMaxFailures is the consecutive network-failure budget,
	// `QODER_POLL_MAX_FAILURES` (`qoder.ts:20`, upstream `H_a = 3`, relaxed to 5).
	PollMaxFailures = 5
	// FirstTokenTimeoutMS bounds the wait for the first upstream SSE frame. Jet-Hub
	// reads `DSH_QODER_SSE_FIRST_TOKEN_TIMEOUT_MS` with a 120s default
	// (`qoder-adapter.ts:58-60`). A half-open SSE connection otherwise hangs the
	// generator forever, which is why the guard exists at all.
	FirstTokenTimeoutMS = 120_000
	// ChunkTimeoutMS bounds the silence between two upstream frames
	// (`qoder-adapter.ts:61-63`).
	ChunkTimeoutMS = 120_000
)

// DefaultClientVersion is `COSY_VERSION` from the WASM glue (`qoder-wasm.ts:66`).
// It affects `Cosy-Version` and the signed payload, so it is configurable.
const DefaultClientVersion = "1.1.49"

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
type Config struct {
	Enabled  bool
	Priority int

	// Region selects the default site: `qoder` (international) or `qoder-cn`.
	// A credential that carries its own region field wins over this value, so
	// the two sites can coexist in one instance without sharing credentials.
	Region Region
	// WASMPath points at a local copy of the Qoder signing WASM. It is NOT
	// bundled with this repository: the binary is a third-party build artifact
	// and redistributing it is the repository owner's decision. When it is empty
	// the plugin serves the public OpenAI-compatible endpoint only.
	WASMPath string
	// ClientVersion is written into `Cosy-Version` and the signed payload.
	ClientVersion string
	// SessionType overrides the per-region `session_type` value.
	SessionType string
	// PublicModels are extra model names for the PUBLIC endpoint, supplied by the
	// user. The public endpoint accepts generic names (`qwen-flash`, ...) that do
	// not appear anywhere in the TypeScript sources, so this plugin must not
	// invent them.
	PublicModels []string
	// DefaultMaxTokens is applied when a request omits max_tokens; 0 sends none.
	DefaultMaxTokens int
	// FirstTokenTimeoutMS / ChunkTimeoutMS bound the upstream SSE stream.
	FirstTokenTimeoutMS int
	ChunkTimeoutMS      int
	// RequestTimeoutMS bounds a single non-streaming upstream call.
	RequestTimeoutMS int
	// LoginTimeoutMS / PollIntervalMS / PollMaxFailures drive the device-code loop.
	LoginTimeoutMS  int
	PollIntervalMS  int
	PollMaxFailures int
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:             true,
		Region:              RegionGlobal,
		ClientVersion:       DefaultClientVersion,
		DefaultMaxTokens:    0,
		FirstTokenTimeoutMS: FirstTokenTimeoutMS,
		ChunkTimeoutMS:      ChunkTimeoutMS,
		RequestTimeoutMS:    RequestTimeoutMS,
		LoginTimeoutMS:      LoginTimeoutMS,
		PollIntervalMS:      PollIntervalMS,
		PollMaxFailures:     PollMaxFailures,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is first decoded into a generic mapping and every key is then
// coerced individually. Three properties follow, all of which matter in practice:
//
//   - block AND flow style are both accepted, because the host hands the instance
//     subtree back in whichever style the user wrote it. A block-only line
//     scanner silently ignores `{enabled: true, region: qoder-cn, ...}` and falls
//     back to every default without reporting anything — which would send
//     requests down the wrong inference path;
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
	cfg.Region = coerceRegion(raw["region"], cfg.Region)
	cfg.WASMPath = coerceString(raw["wasm_path"], cfg.WASMPath)
	cfg.ClientVersion = coerceString(raw["client_version"], cfg.ClientVersion)
	cfg.SessionType = coerceString(raw["session_type"], cfg.SessionType)
	cfg.PublicModels = coerceList(raw["public_models"])
	cfg.DefaultMaxTokens = coerceInt(raw["max_tokens"], cfg.DefaultMaxTokens)
	cfg.FirstTokenTimeoutMS = coerceInt(raw["first_token_timeout_ms"], cfg.FirstTokenTimeoutMS)
	cfg.ChunkTimeoutMS = coerceInt(raw["chunk_timeout_ms"], cfg.ChunkTimeoutMS)
	cfg.RequestTimeoutMS = coerceInt(raw["request_timeout_ms"], cfg.RequestTimeoutMS)
	cfg.LoginTimeoutMS = coerceInt(raw["login_timeout_ms"], cfg.LoginTimeoutMS)
	cfg.PollIntervalMS = coerceInt(raw["poll_interval_ms"], cfg.PollIntervalMS)
	cfg.PollMaxFailures = coerceInt(raw["poll_max_failures"], cfg.PollMaxFailures)
	return cfg
}

// coerceRegion accepts only the two known region ids; anything else keeps the
// configured default. `cn` is accepted as a convenience spelling.
func coerceRegion(value any, fallback Region) Region {
	switch strings.ToLower(coerceString(value, "")) {
	case string(RegionGlobal):
		return RegionGlobal
	case string(RegionCN), "cn", "qoder_cn":
		return RegionCN
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

// coerceList accepts either a YAML sequence or a comma/space separated scalar.
func coerceList(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := coerceString(item, ""); text != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		fields := strings.FieldsFunc(typed, func(r rune) bool {
			return r == ',' || r == ';' || r == '\n' || r == ' ' || r == '\t'
		})
		out := make([]string, 0, len(fields))
		for _, field := range fields {
			if trimmed := strings.TrimSpace(field); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	default:
		return nil
	}
}

// ConfigFields describes the settings the CPA management UI renders.
func ConfigFields() []configField {
	return []configField{
		{Name: "region", Type: "enum", EnumValues: []string{string(RegionGlobal), string(RegionCN)},
			Description: "区域：qoder=国际版（api2-v2.qoder.sh 公开 / api2.qoder.sh 加密），qoder-cn=国内版（两者同为 gateway.qoder.com.cn）。凭据自带 region 时以凭据为准"},
		{Name: "wasm_path", Type: "string",
			Description: "本地 Qoder 签名 WASM 的路径（可选）。留空则只走公开 OpenAI 兼容端点；填写后走 agent_chat_generation 加密端点并原样透传 WASM 生成的签名头"},
		{Name: "client_version", Type: "string", Description: "Cosy-Version 客户端版本（默认 1.1.49，对应 qoder-wasm.ts 的 COSY_VERSION）"},
		{Name: "session_type", Type: "string", Description: "加密请求体的 session_type，留空则按区域取值（国际 qodercli / 国内 qoder_work）"},
		{Name: "public_models", Type: "string", Description: "公开端点额外可用的通用模型名，逗号分隔（如 qwen-flash,qwen-plus）。目录 key 只对加密端点有效，公开端点不认"},
		{Name: "max_tokens", Type: "integer", Description: "请求未指定 max_tokens 时的默认值，0 表示不发送"},
		{Name: "first_token_timeout_ms", Type: "integer", Description: "等待上游首个 SSE 分片的超时，毫秒"},
		{Name: "chunk_timeout_ms", Type: "integer", Description: "两个上游 SSE 分片之间的超时，毫秒"},
		{Name: "request_timeout_ms", Type: "integer", Description: "单次非流式上游请求的超时，毫秒"},
		{Name: "login_timeout_ms", Type: "integer", Description: "设备码登录等待总超时，毫秒（默认 5 分钟）"},
		{Name: "poll_interval_ms", Type: "integer", Description: "设备码轮询间隔，毫秒（默认 1000）"},
		{Name: "poll_max_failures", Type: "integer", Description: "轮询连续网络失败上限（默认 5）"},
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

// normalizeRegion maps a configured region string onto a known region.
func normalizeRegion(value string) Region {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(RegionCN), "cn", "qoder_cn":
		return RegionCN
	default:
		return RegionGlobal
	}
}

// activeRegion is the configured default region.
func activeRegion() Region { return normalizeRegion(string(settings().Region)) }
