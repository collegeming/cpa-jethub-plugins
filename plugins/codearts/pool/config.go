package pool

import (
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// Pool identity inside the merged codearts plugin. The pool no longer ships
// its own artifact, so ProviderKey only names its POST management route.
const (
	// ProviderKey prefixes the pool's POST route in the shared /v0/management
	// namespace.
	ProviderKey = "codearts"
	// DisplayName is the single sidebar label for the combined ops page
	// (pool + registrar). CPA-Manager-Plus renders one nav item per Menu.
	DisplayName = "CodeArts 运维"
)

// Defaults for the pool manager.
const (
	// DefaultHostBaseURL is the loopback address of the CPA process this plugin
	// runs inside. 8317 is the CPA default HTTP port. It cannot be
	// auto-discovered: HostConfigSummary carries no listen port.
	DefaultHostBaseURL = "http://127.0.0.1:8317"
	// DefaultScanIntervalMS is how often the pool is reconciled. Five minutes
	// is frequent enough to pull an exhausted account before many users hit
	// it, and cheap enough to leave on: one loopback GET per tick.
	DefaultScanIntervalMS = 300_000
	// disabledByCredit is the marker the scanner writes next to `disabled` so a
	// later scan can tell "the pool pulled this credential" apart from an
	// operator who disabled it on purpose. Only the marker is resurrected.
	disabledByCredit = "codeartspool:credit"
	// disabledByManual marks a credential disabled through this plugin's own
	// page, which the scanner must not resurrect either.
	disabledByManual = "codeartspool:manual"
)

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
type Config struct {
	// Enabled is the write switch, mirrored from the host's own
	// `plugins.configs.<id>.enabled` value. When false the scanner reads and
	// reports but never rewrites an auth file.
	Enabled bool
	// HostBaseURL is the origin of the CPA process hosting this plugin.
	HostBaseURL string
	// ScanIntervalMS is the background scan period in milliseconds; <= 0
	// disables the scheduler (the page and the scan route still work).
	ScanIntervalMS int
	// MinRemaining is the credit floor: an account reporting
	// `remaining <= MinRemaining` counts as exhausted.
	MinRemaining float64
	// AutoDisable lets the scanner write `disabled: true` on an exhausted
	// credential.
	AutoDisable bool
	// AutoEnable lets the scanner clear a pool-set `disabled` once the account
	// reports credits again. Manual disables (marker absent or foreign) are
	// never touched.
	AutoEnable bool
	// AutoCheckin lets the sweep claim each account's daily-login grant before
	// the balance decision runs: a claim that lands credit can revive an
	// exhausted credential in the same pass it would otherwise have stayed
	// parked. The claim is an upstream write, so it follows the Enabled switch.
	AutoCheckin bool
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		HostBaseURL:    DefaultHostBaseURL,
		ScanIntervalMS: DefaultScanIntervalMS,
		MinRemaining:   0,
		AutoDisable:    true,
		AutoEnable:     true,
		AutoCheckin:    true,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
// Values are coerced individually so one unusable entry costs only its own
// default, the same convention every sibling plugin follows.
func ConfigFromYAML(document []byte) Config {
	cfg := DefaultConfig()
	if len(document) == 0 {
		return cfg
	}
	var raw map[string]any
	if errUnmarshal := yaml.Unmarshal(document, &raw); errUnmarshal != nil {
		return DefaultConfig()
	}
	return ConfigFromMap(raw, "")
}

// ConfigFromMap decodes pool settings from an already-parsed config map,
// reading each key with the given prefix. The merged codearts plugin keeps
// provider, pool and registrar settings in one YAML block, so the pool keys
// arrive as `pool_enabled`, `pool_scan_interval_ms`, and so on.
func ConfigFromMap(raw map[string]any, prefix string) Config {
	cfg := DefaultConfig()
	cfg.Enabled = coerceBool(raw[prefix+"enabled"], cfg.Enabled)
	cfg.HostBaseURL = coerceString(raw[prefix+"host_base_url"], cfg.HostBaseURL)
	cfg.ScanIntervalMS = coerceInt(raw[prefix+"scan_interval_ms"], cfg.ScanIntervalMS)
	cfg.MinRemaining = coerceFloat(raw[prefix+"min_remaining"], cfg.MinRemaining)
	cfg.AutoDisable = coerceBool(raw[prefix+"auto_disable"], cfg.AutoDisable)
	cfg.AutoEnable = coerceBool(raw[prefix+"auto_enable"], cfg.AutoEnable)
	cfg.AutoCheckin = coerceBool(raw[prefix+"auto_checkin"], cfg.AutoCheckin)
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
		{Name: "enabled", Type: "boolean",
			Description: "写开关（yaml 键 pool_enabled）：为 false 时巡检只读不写"},
		{Name: "host_base_url", Type: "string",
			Description: "本 CPA 进程的访问地址（默认 http://127.0.0.1:8317）。必须手动填写：宿主传给插件的 HostConfigSummary 不含 HTTP 端口"},
		{Name: "scan_interval_ms", Type: "integer",
			Description: "后台巡检周期，毫秒（默认 300000，0 关闭自动巡检；页面上的「立即巡检」不受影响）"},
		{Name: "min_remaining", Type: "string",
			Description: "余额下限：remaining ≤ 该值即判定积分耗尽并停用账号（默认 0）"},
		{Name: "auto_disable", Type: "boolean",
			Description: "自动停用积分耗尽的账号（写 disabled:true 到凭据文件，宿主立即将该账号移出轮换）"},
		{Name: "auto_enable", Type: "boolean",
			Description: "账号恢复余额后自动重新启用——只解除本插件写入的停用标记，手工停用不受影响"},
		{Name: "auto_checkin", Type: "boolean",
			Description: "巡检时自动领取每个可签到账号的每日积分（含被本插件停用的账号——签到回血是复活路径）。受 enabled 总开关约束"},
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

// HostConfigFields returns the settings description in the host's type. The
// merged plugin prefixes each name with "pool_" before appending them to its
// own field list.
func HostConfigFields() []pluginapi.ConfigField {
	fields := ConfigFields()
	out := make([]pluginapi.ConfigField, 0, len(fields))
	for _, field := range fields {
		out = append(out, pluginapi.ConfigField{
			Name:        field.Name,
			Type:        pluginapi.ConfigFieldType(field.Type),
			EnumValues:  field.EnumValues,
			Description: field.Description,
		})
	}
	return out
}
