package main

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider identity. The plugin ID is derived by CPA from the artifact file
// name (`hub.so` / `hub-v<version>.so`), NOT from this constant, so the build
// script must name the artifact after ProviderKey.
const (
	// ProviderKey is the plugin id CPA derives from the artifact name.
	ProviderKey = "hub"
	// DisplayName is the human-readable name shown by management clients.
	DisplayName = "Jet Hub"
	// MenuLabel is the sidebar entry, and the ONLY Menu any plugin in this
	// repository declares: the manager's nav is flat and does not group entries
	// by plugin, so every provider page lives in the resource list instead
	// (README "挂载规则"). It names the whole hub — the channel overview and the
	// one-click check-in are the same page — rather than the check-in action.
	MenuLabel = "Jet Hub"
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
)

// Defaults for the settings that must be supplied by the user.
const (
	// DefaultHostBaseURL is the loopback address of the CPA process this plugin
	// runs inside. 8317 is the CPA default HTTP port.
	//
	// It CANNOT be auto-discovered: the host hands a plugin only a
	// pluginapi.HostConfigSummary (AuthDir, ProxyURL, excluded models, ...) and
	// nothing in it carries the HTTP listen port, so the plugin has no way to
	// learn where its own host is listening. Hence this is the one setting the
	// user has to get right; every target URL is built from it.
	DefaultHostBaseURL = "http://127.0.0.1:8317"
	// DefaultTimeoutMS bounds ONE request to the host's resource routes. It is
	// enforced plugin-side (see boundedGet): the host's `host.http.do` callback
	// has no timeout field, so a per-request deadline is the only way a stuck
	// loopback call cannot hang the whole run.
	DefaultTimeoutMS = 30_000
)

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
type Config struct {
	// Enabled is the write switch, mirrored from the host's own
	// `plugins.configs.<id>.enabled` value: when the host delivers config_yaml it
	// FORCES this key to its own flag (`internal/pluginhost/config.go`,
	// ensureMappingScalar), so it is true whenever this plugin is running. It is
	// honoured anyway, because claiming is the one destructive action the hub
	// performs: a run on a disabled instance refuses to write and says so.
	Enabled bool
	// HostBaseURL is the origin of the CPA process hosting this plugin, for
	// example http://127.0.0.1:8317 or https://cpa.example.com.
	HostBaseURL string
	// TimeoutMS bounds one request to a provider's resource route.
	TimeoutMS int
	// Providers narrows the run to the listed provider ids. Empty means every
	// target in the catalogue, including the ones that report 不支持.
	Providers []string
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:     true,
		HostBaseURL: DefaultHostBaseURL,
		TimeoutMS:   DefaultTimeoutMS,
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
//
// The document is first decoded into a generic mapping and every key is then
// coerced individually. Three properties follow, all of which matter in practice:
//
//   - block AND flow style are both accepted, because the host hands the instance
//     subtree back in whichever style the user wrote it. A block-only line
//     scanner silently ignores `{enabled: true, host_base_url: "..."}` and falls
//     back to every default without reporting anything — which here would point
//     the whole run at the wrong host;
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
	cfg.HostBaseURL = coerceString(raw["host_base_url"], cfg.HostBaseURL)
	cfg.TimeoutMS = coerceInt(raw["timeout_ms"], cfg.TimeoutMS)
	cfg.Providers = coerceList(raw["providers"])
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
		{Name: "enabled", Type: "boolean",
			Description: "签到写开关，与宿主的 plugins.configs.hub.enabled 同一个值（宿主下发配置时会强制写入该键）。为 false 时一键签到不执行任何写操作，只报告已关闭"},
		{Name: "host_base_url", Type: "string",
			Description: "本 CPA 进程的访问地址（默认 http://127.0.0.1:8317）。必须手动填写：宿主传给插件的 HostConfigSummary 只有 AuthDir/ProxyURL 等字段，不含 HTTP 端口，插件无法自动发现自己的宿主地址"},
		{Name: "timeout_ms", Type: "integer",
			Description: "单次调用 provider 资源路由的超时，毫秒（默认 30000，插件侧计时）"},
		{Name: "providers", Type: "string",
			Description: "参与一键签到的 provider id，逗号分隔（如 qoder,trae,loomy）。留空表示目录内全部（codearts,codebuddy,codebuddy-intl,workbuddy-cn,workbuddy,qoder,trae,lobsterai,loomy,cline），其中 cline 会如实显示为不支持"},
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

// selectedProviders returns the configured provider filter, lower-cased and
// de-duplicated; an empty list means "no filter".
func selectedProviders(cfg Config) []string {
	out := make([]string, 0, len(cfg.Providers))
	seen := map[string]bool{}
	for _, raw := range cfg.Providers {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
