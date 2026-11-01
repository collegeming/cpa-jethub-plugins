package reg

import (
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// Registrar identity inside the merged codearts plugin. The registrar no
// longer ships its own artifact, so ProviderKey only names its POST
// management route.
const (
	// ProviderKey prefixes the registrar's POST route in the shared
	// /v0/management namespace.
	ProviderKey = "codearts"
	// DisplayName is the page heading. The registrar no longer owns a
	// sidebar Menu; the combined ops page (/pool) does.
	DisplayName = "CodeArts 运维"
)

// Upstream endpoints used by the registration and credential chains.
const (
	// RegAPIBase is the Huawei registerui REST service the sign-up SPA calls.
	RegAPIBase = "https://reg.huaweicloud.com/registerui/rest/cbc/cbccustomerregservice/v1"
	// AuthUIBase is the unified Huawei CAS the portal delegates sign-in to.
	AuthUIBase = "https://auth.huaweicloud.com"
	// ThirdLoginURL redeems the register response's auto-login ticket and
	// plants the auth-domain SSO session (idp=CHNIDP).
	ThirdLoginURL = AuthUIBase + "/authui/thirdLogin"
	// CasLoginURL redeems the shared registerui quick_login ticket the SPA
	// mints for already-registered phones (SelectAccount → 登录).
	CasLoginURL = AuthUIBase + "/authui/casLogin"
	// OAuthAuthorizeURL is the real OAuth2 grant endpoint on the CAS domain:
	// GET with session cookies answers 302 to redirect_uri?code=... — the
	// portal/authorize page is only the consent UI in front of it.
	OAuthAuthorizeURL = AuthUIBase + "/authui/v1/oauth2/authorize"
	// RedeemServiceURL is the CAS service the ticket dance lands on; any
	// huaweicloud.com page works — it only exists to complete the ST hop.
	RedeemServiceURL = "https://account.huaweicloud.com/usercenter/?locale=zh-cn&hws_route_url=registSuccess"
	// STSTokenEndpoint exchanges authorization codes and refresh tokens.
	STSTokenEndpoint = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	// OAuthClientID and the loopback path mirror the codearts plugin.
	OAuthClientID     = "codearts-agent"
	OAuthRedirectPath = "/oauth/callback"

	// DefaultSMSBase is the first-SMS-pool endpoint the desktop tool uses.
	DefaultSMSBase = "https://api.d1jiema.com/zc/data.php"
	// DefaultSMSKeyword is the sender-name keyword Huawei sign-up SMSes carry.
	DefaultSMSKeyword = "华为云"
)

// Config is the per-instance plugin configuration, decoded from the
// `config_yaml` subtree delivered with plugin.register / plugin.reconfigure.
type Config struct {
	// Enabled is the write switch: false renders the page but never starts a
	// registration job.
	Enabled bool
	// SMSBaseURL is the SMS pool HTTP endpoint (d1jiema-compatible API).
	SMSBaseURL string
	// SMSToken is the pool account token.
	SMSToken string
	// SMSKeyword is the SMS sender keyword filter ("华为云", fallback "华为").
	SMSKeyword string
	// SMSCardType narrows the number pool ("全部", region filters, ...).
	SMSCardType string
	// InviteCode pins one INV-... referral code for every new account. Empty
	// means draw randomly from the pool's minted codes at job time.
	InviteCode string
	// SMSWaitMS bounds the wait for one SMS before the job fails.
	SMSWaitMS int
	// NamePrefix is the auth file name stem (`<prefix>-<phone>.json`).
	NamePrefix string
}

// DefaultConfig returns the settings used when the user provides nothing.
func DefaultConfig() Config {
	return Config{
		Enabled:     true,
		SMSBaseURL:  DefaultSMSBase,
		SMSKeyword:  DefaultSMSKeyword,
		SMSCardType: "全部",
		SMSWaitMS:   120_000,
		NamePrefix:  "codearts",
	}
}

// ConfigFromYAML decodes the settings the host supplies for this instance.
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

// ConfigFromMap decodes registrar settings from an already-parsed config map,
// reading each key with the given prefix. The merged codearts plugin keeps
// provider, pool and registrar settings in one YAML block, so the registrar
// keys arrive as `reg_enabled`, `reg_sms_token`, and so on.
func ConfigFromMap(raw map[string]any, prefix string) Config {
	cfg := DefaultConfig()
	cfg.Enabled = coerceBool(raw[prefix+"enabled"], cfg.Enabled)
	cfg.SMSBaseURL = coerceString(raw[prefix+"sms_base_url"], cfg.SMSBaseURL)
	cfg.SMSToken = coerceString(raw[prefix+"sms_token"], cfg.SMSToken)
	cfg.SMSKeyword = coerceString(raw[prefix+"sms_keyword"], cfg.SMSKeyword)
	cfg.SMSCardType = coerceString(raw[prefix+"sms_card_type"], cfg.SMSCardType)
	cfg.InviteCode = coerceString(raw[prefix+"invite_code"], cfg.InviteCode)
	cfg.SMSWaitMS = coerceInt(raw[prefix+"sms_wait_ms"], cfg.SMSWaitMS)
	cfg.NamePrefix = coerceString(raw[prefix+"name_prefix"], cfg.NamePrefix)
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

// ConfigFields describes the settings the CPA management UI renders.
func ConfigFields() []configField {
	return []configField{
		{Name: "enabled", Type: "boolean",
			Description: "总开关：关闭后页面只读，不发起注册任务"},
		{Name: "sms_base_url", Type: "string",
			Description: "接码平台 API 地址（兼容第一接码 data.php 协议，默认 api.d1jiema.com）"},
		{Name: "sms_token", Type: "string",
			Description: "接码平台账号 Token"},
		{Name: "sms_keyword", Type: "string",
			Description: "取号/收码匹配的发信关键词（默认 华为云；收码时会同时尝试 华为）"},
		{Name: "sms_card_type", Type: "string",
			Description: "号码卡类型筛选（默认 全部）"},
		{Name: "invite_code", Type: "string",
			Description: "手动指定邀请码 INV-...；留空时新号随机绑定池中已注册的邀请码"},
		{Name: "sms_wait_ms", Type: "integer",
			Description: "等待单条短信的超时毫秒数（默认 120000）"},
		{Name: "name_prefix", Type: "string",
			Description: "凭证文件名前缀（默认 codearts，生成 <前缀>-<手机号>.json）"},
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
// merged plugin prefixes each name with "reg_" before appending them to its
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
