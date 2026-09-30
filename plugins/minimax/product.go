package main

import "strings"

// MiniMax Code (中国版) product configuration: hosts, OAuth constants, endpoint
// paths and the fallback model catalogue.
//
// Ported from `src/minimax-product.ts` (MINIMAX, MINIMAX_*_PATH). Every host,
// client id and scope below carries the fact it was read from; nothing here is
// invented.

const (
	// AccountHost serves the OAuth device-code and token endpoints
	// (`minimax-product.ts:MINIMAX.accountHost`).
	AccountHost = "https://account.minimax.cn" // APIHost serves the model catalogue, inference, sign-in and credit
	// endpoints (`minimax-product.ts:MINIMAX.apiHost`).
	APIHost = "https://agent.minimax.cn"

	// Region and BuildEnv are the catalogue query parameters
	// (`minimax-product.ts:MINIMAX.region` / `.buildEnv`).
	Region   = "cn"
	BuildEnv = "prod"

	// ClientID is the vendor's public OAuth client id, taken from the official
	// desktop client's own auth.json and its asar bundle
	// (`minimax-product.ts:MINIMAX.clientId`).
	ClientID = "mcode-public"
	// Audience is `MCODE_OAUTH_AUDIENCE` from the client's contracts module.
	Audience = "agent-backend"
	// Scope is the OAuth scope. ⚠️ It is LOAD-BEARING on two paths: the
	// device-code request sends it, and the token grant is REJECTED as a whole
	// when the answer's own `scope` does not contain it.
	Scope = "agent.default"

	// DefaultCredentialRef is the reference's single-credential fallback ref.
	DefaultCredentialRef = "MINIMAX_ACCESS_TOKEN"
)

// Logo is a locally drawn badge, not vendor artwork: MiniMax publishes no icon
// this repository may redistribute, and the shared `internal/jethub/brandicons`
// package has no mark for this provider and must not be modified by this
// plugin. It follows the same fallback the raccoon plugin uses — a coloured
// tile with the brand's first character — so the management UI shows a stable,
// recognisable entry without shipping third-party assets.
const Logo = `data:image/svg+xml,%3Csvg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"%3E%3Crect width="24" height="24" rx="6" fill="%23e0342b"/%3E%3Ctext x="12" y="17.5" font-size="14" font-family="sans-serif" font-weight="700" fill="white" text-anchor="middle"%3EM%3C/text%3E%3C/svg%3E`

// Endpoint paths, all from `minimax-product.ts`.
const (
	// DeviceCodePath issues a device code + user code pair.
	DeviceCodePath = "/oauth2/device/code"
	// TokenPath serves BOTH the device-code grant and the refresh grant.
	TokenPath = "/oauth2/token"
	// RevokePath revokes a token. Recorded for completeness; this plugin never
	// calls it (a CPA credential is removed from the auth directory instead).
	RevokePath = "/oauth2/revoke"
	// ModelsPath is the remote model catalogue; it requires `?region=&buildEnv=`.
	ModelsPath = "/mavis/api/v1/models"
	// InferPath is the Anthropic Messages inference endpoint.
	InferPath = "/mavis/api/v1/llm/v1/messages"
	// SigninStatusPath reports the 7-day check-in panel. ⚠️ It requires
	// `?timezone_id=<IANA>`.
	SigninStatusPath = "/minimax-cloud/api/v1/signin/status"
	// SigninClaimPath claims the day's reward. ⚠️ It requires the same query
	// parameter, and its body is `{}`.
	SigninClaimPath = "/minimax-cloud/api/v1/signin/claim"
	// CreditDetailsPath reports the credit ledger. It takes NO query parameter
	// and — unlike the two sign-in paths — its payload is FLAT (no `data`).
	CreditDetailsPath = "/minimax-cloud/api/v1/credit/details"
)

// Model cache and refresh-window defaults.
const (
	// ModelCacheTTLMS is the remote catalogue's cache lifetime. The remote
	// snapshot advertises `ttlSeconds: 300`; the reference records that value
	// without copying it (it fetches on demand). Five minutes is the same
	// number, and it is short enough that a catalogue change is picked up
	// inside a session.
	ModelCacheTTLMS = 300_000
	// RefreshWindowSeconds is how long before its expiry a credential is
	// renewed. It matches the lead the plugin reports to the host through
	// `NextRefreshAfter`, so the plugin and the host agree on "due".
	RefreshWindowSeconds = 300
)

// Thinking-mode markers from the remote catalogue's `thinking_config.mode`
// (`minimax-product.ts:MinimaxFallbackModel.thinkingMode`).
const (
	// thinkingForcedOn means the server thinks regardless of what the request
	// asks for. Two measured variants exist and they behave DIFFERENTLY:
	// `MiniMax-M2.7*` SILENTLY IGNORES `thinking.type="disabled"`, while
	// `MiniMax-M3.1-Flash-Preview` answers a HARD HTTP 400.
	thinkingForcedOn = "forced_on"
	// thinkingSwitchable means the user really can turn thinking off
	// (`MiniMax-M3`: omitting `thinking` produces 0 thinking blocks, `adaptive`
	// produces them).
	thinkingSwitchable = "switchable"
)

// adaptiveOnlyPrefix identifies the models that MUST be sent
// `thinking:{type:"adaptive"}`.
//
// ⚠️ It is a PREFIX, and the prefix must be `MiniMax-M3.1` — not `MiniMax-M3`.
// The measured 400 for `MiniMax-M3.1-Flash-Preview` is quoted verbatim below;
// `MiniMax-M3` accepts `disabled` and answers 200, so a `MiniMax-M3` prefix
// would strip that model's "turn thinking off" control for no reason.
//
//	{"type":"error","error":{"type":"invalid_request_error","message":
//	 "invalid params, model \"MiniMax-M3.1-Flash-Preview\" requires adaptive
//	  thinking; thinking.type=\"disabled\" (including reasoning.effort=none)
//	  is not allowed (2013)"}}
//
// This is why `rewriteAdaptiveThinking` exists at all: the CPA host builds the
// Anthropic payload from the client's OpenAI request, and its own translator
// derives `thinking.type` from `reasoning_effort` (see
// `internal/translator/claude/openai/chat-completions/claude_openai_request.go`,
// the `case "none": thinking.type = "disabled"` branch). Whatever this plugin
// DECLARES in `Thinking.Levels` is not enforced by the host on that path, so a
// payload carrying `disabled` for an M3.1 model reaches the vendor and hard
// fails. The rewrite below is therefore mandatory, not cosmetic.
const adaptiveOnlyPrefix = "MiniMax-M3.1"

// requiresAdaptiveThinking reports whether a model id falls under the prefix
// that cannot be sent `thinking.type="disabled"`.
func requiresAdaptiveThinking(model string) bool {
	return strings.HasPrefix(strings.TrimSpace(model), adaptiveOnlyPrefix)
}

// ModelCatalogEntry is one model as this plugin models it. It is the single
// shape shared by the fallback table and the remote catalogue; the two paths
// share the OUTPUT shape only (the fallback entries are hand-written literals,
// already expressed in the documented units).
type ModelCatalogEntry struct {
	// ID is the long model id sent to the inference endpoint
	// (`MiniMax-M3.1-Flash-Preview`).
	ID string
	// Name is the short display name the official IDE shows
	// (`M3.1-Flash-Preview`). It is a DIFFERENT field from ID upstream: the
	// catalogue's `models` object is keyed by the long id while each value
	// carries the short `name`, and both must be preserved.
	Name string
	// ContextWindow is the context window in tokens.
	//
	// ⚠️ It is the MAXIMUM tier of `context_window_options`, NOT
	// `limit.context`. For `MiniMax-M3.1-Flash-Preview` the two disagree:
	// `limit.context` is 512000 while the options are [512000, 1000000]. The
	// official client offers the max tier, so 1M is the honest number; filling
	// in 512K would make the host compact far earlier than the vendor's own
	// client does. The host has a single `ContextLength` field, so the max tier
	// is what is published.
	ContextWindow int64
	// MaxTokens is the per-response output cap (`limit.output`). 0 means the
	// catalogue did not publish a usable one.
	MaxTokens int64
	// SupportsImage mirrors `modalities.input` containing `image`.
	SupportsImage bool
	// EffortOptions is the remote `effort_options` list. ⚠️ ONLY
	// `MiniMax-M3.1-Flash-Preview` publishes one; the other three publish no
	// such field, and inventing tiers for them would send requests the server
	// does not accept.
	EffortOptions []string
	// DefaultEffort is the remote `default_effort`. It is dropped unless it
	// falls inside EffortOptions.
	DefaultEffort string
	// ThinkingMode is the remote `thinking_config.mode`; empty means the
	// catalogue did not say, which is treated as UNKNOWN rather than guessed.
	ThinkingMode string
}

// fallbackCatalogue is the bundled table, a 2026-09-28 snapshot of
// `GET /mavis/api/v1/models?region=cn&buildEnv=prod`, in the remote
// `model_order`.
//
// ⚠️ It contains all FOUR exposed model ids. The vendor's desktop client's own
// built-in static table has only three (M3 / M2.7-highspeed / M2.7), but the
// reference provider deliberately adds the remote-only
// `MiniMax-M3.1-Flash-Preview` to its fallback table so a temporary catalogue
// failure cannot make the model currently in use disappear.
//
// ⚠️ Context windows follow the max-tier rule documented on ContextWindow:
// 1M for M3.1 and M3, 200K for the M2.7 pair. `limit.context` for M3.1 and M3
// is 512000 but their options reach 1M.
var fallbackCatalogue = []ModelCatalogEntry{
	{
		ID:            "MiniMax-M3.1-Flash-Preview",
		Name:          "M3.1-Flash-Preview",
		ContextWindow: 1_000_000,
		MaxTokens:     128_000,
		SupportsImage: true,
		EffortOptions: append([]string(nil), DefaultEffortLevels...),
		DefaultEffort: "default",
		ThinkingMode:  thinkingForcedOn,
	},
	{
		ID:            "MiniMax-M3",
		Name:          "M3",
		ContextWindow: 1_000_000,
		MaxTokens:     128_000,
		SupportsImage: true,
		// ⚠️ `switchable`: omitting `thinking` means NO thinking (measured 0
		// blocks), `adaptive` means thinking. It publishes no effort tiers, so
		// the only control is the on/off pair this plugin appends.
		ThinkingMode: thinkingSwitchable,
	},
	{
		ID:            "MiniMax-M2.7-highspeed",
		Name:          "M2.7-highspeed",
		ContextWindow: 200_000,
		MaxTokens:     128_000,
		SupportsImage: false,
		// ⚠️ `forced_on` and SILENTLY IGNORED when disabled: the server still
		// produces thinking blocks. No level list, no on/off pair — offering
		// one would tell the user thinking is off while it is not.
		ThinkingMode: thinkingForcedOn,
	},
	{
		ID:            "MiniMax-M2.7",
		Name:          "M2.7",
		ContextWindow: 200_000,
		MaxTokens:     128_000,
		SupportsImage: false,
		ThinkingMode:  thinkingForcedOn,
	},
}

// fallbackModels returns a copy of the bundled table.
func fallbackModels() []ModelCatalogEntry {
	out := make([]ModelCatalogEntry, len(fallbackCatalogue))
	copy(out, fallbackCatalogue)
	return out
}

// catalogueEntryFor looks a model up by its long id.
func catalogueEntryFor(entries []ModelCatalogEntry, id string) (ModelCatalogEntry, bool) {
	trimmed := strings.TrimSpace(id)
	for _, entry := range entries {
		if entry.ID == trimmed {
			return entry, true
		}
	}
	return ModelCatalogEntry{}, false
}

// reasoningEffortsFor renders the thinking controls a model offers.
//
// Three cases, all read off the catalogue rather than guessed:
//
//   - `effort_options` present (only M3.1) ⇒ that list, in catalogue order;
//   - `thinking_config.mode == "switchable"` (M3) ⇒ the on/off pair. The
//     vendor's own client spells these `on` / `off`; `off` is exposed under the
//     repository-wide name `none` (Qoder's "关闭思考" uses `none` too), and `on`
//     is added as well because M3 defaults to NOT thinking — offering only
//     `none` would let the user turn thinking off but never on.
//   - `forced_on` ⇒ NOTHING. M3.1 hard-fails on `disabled` (HTTP 400, business
//     code 2013) and M2.7 silently ignores it, which is worse than no option:
//     the user would believe thinking was disabled while it was not.
func reasoningEffortsFor(entry ModelCatalogEntry) []string {
	efforts := append([]string(nil), entry.EffortOptions...)
	if entry.ThinkingMode != thinkingSwitchable {
		return efforts
	}
	if !containsString(efforts, "on") {
		efforts = append(efforts, "on")
	}
	if !containsString(efforts, "none") {
		efforts = append(efforts, "none")
	}
	return efforts
}

// containsString reports membership without pulling in slices for one call.
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
