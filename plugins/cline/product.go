package main

import (
	"net/url"
	"time"
)

// Cline product constants, ported from `src/cline-product.ts`.
//
// Citations are `path:line` relative to the Jet-Hub TypeScript repository, as
// collected in the porting specification (`/tmp/cline-spec.md`, §1). Every host,
// header and timeout below was read from the TypeScript source; nothing here is
// invented, and the one value that has no source evidence (the balance divisor)
// is called out in place.

// Provider identity.
const (
	// ProviderKey is the stable provider identifier written into CPA auth files.
	ProviderKey = "cline"
	// DisplayName is the human-readable name shown by management clients.
	DisplayName = "Cline"
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
)

// Hosts. Only two are ever contacted: api.cline.bot and api.workos.com
// (spec §1 "Every host contacted").
const (
	// APIBase serves every `/api/v1/...` call: register, refresh, chat,
	// models, recommended-models and balance (`cline-product.ts:253`).
	//
	// Two hosts/paths the source mentions have no runtime role and therefore no
	// constant here: `appBase` (`cline-product.ts:254`), which nothing reads
	// because the login URL comes from WorkOS, and `/api/v1/users/me`
	// (`cline-product.ts:289`), which only the read-only e2e probe calls
	// (`tests/e2e/cline-probe.e2e.spec.ts:65`).
	APIBase = "https://api.cline.bot"
	// WorkOSBase serves exactly the two device-authorization calls.
	WorkOSBase = "https://api.workos.com"
	// WorkOSClientID is the public device-flow client id
	// (`cline-product.ts:256`), self-verified there against the JWT `client_id`
	// claim.
	WorkOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"
	// TokenPrefix is the literal prefix every Cline access token carries. It is
	// load-bearing: `Bearer workos:<jwt>` answers 200 while `Bearer <jwt>`
	// answers 401 with a misleading "update Cline" body (`cline.ts:166-176`,
	// `cline-product.ts:106-111`). The only place the prefix is stripped is JWT
	// payload decoding in the official client, which this plugin never does.
	TokenPrefix = "workos:"
)

// Endpoint paths (`cline-product.ts:281-295`).
const (
	// DeviceAuthorizationPath is mounted on WorkOSBase.
	DeviceAuthorizationPath = "/user_management/authorize/device"
	// DeviceAuthenticatePath is mounted on WorkOSBase.
	DeviceAuthenticatePath = "/user_management/authenticate"
	// RegisterPath exchanges WorkOS tokens for Cline tokens, on APIBase.
	RegisterPath = "/api/v1/auth/register"
	// RefreshPath renews Cline tokens, on APIBase.
	RefreshPath = "/api/v1/auth/refresh"
	// ChatPath is the SSE chat endpoint, on APIBase.
	ChatPath = "/api/v1/chat/completions"
	// ModelsPath lists the full catalogue, on APIBase. It requires a credential.
	ModelsPath = "/api/v1/models"
	// RecommendedModelsPath is the anonymous curated catalogue, on APIBase.
	RecommendedModelsPath = "/api/v1/ai/cline/recommended-models"
)

// balancePath builds `/api/v1/users/{account_id}/balance` (`cline-credits.ts:181`).
//
// The `{id}` segment is the Cline account id (`usr-…`), never the JWT `sub`
// (`user_…`): passing the subject answers `400 {"error":"Invalid request
// format"}` (`cline-credits.ts:15-19`).
func balancePath(accountID string) string {
	return "/api/v1/users/" + url.PathEscape(accountID) + "/balance"
}

// clientHeaderPairs is `DEFAULT_CLINE_REQUEST_HEADERS` (`cline-product.ts:257-262`).
//
// They are sent on inference, model list, register, refresh and balance — but
// NOT on the two WorkOS calls (`cline-oauth.ts:185`, `:245`). Note
// `X-IS-MULTIROOT` is the literal string "false", not a boolean, and that the
// TypeScript sets no User-Agent anywhere in the Cline code path.
var clientHeaderPairs = [][2]string{
	{"HTTP-Referer", "https://cline.bot"},
	{"X-Title", "Cline"},
	{"X-IS-MULTIROOT", "false"},
	{"X-CLIENT-TYPE", "cline-sdk"},
}

// Timeouts and limits (spec §1 "Timeouts / limits").
//
// The per-request timeouts of the TypeScript (30 s for the device authorization,
// the token poll, register and refresh — `cline-oauth.ts:77`; 20 s for each
// catalogue call — `cline-models.ts:48`; 30 s for the balance —
// `cline-credits.ts:63`) are deliberately NOT represented here. The CPA host
// transport is synchronous and owns the deadline, so a constant in this package
// would have no reference point; the values are recorded in this comment instead.
const (
	// DeviceCodeTTLMS is the device-code lifetime used when the server omits or
	// malforms `expires_in` (`cline-oauth.ts:80`, `:209`).
	DeviceCodeTTLMS = 300_000
	// DevicePollIntervalMS is the poll interval used when the server omits or
	// malforms `interval` (`cline-oauth.ts:83`, `:210`).
	DevicePollIntervalMS = 5_000
	// PollIntervalFloorMS is the lower bound applied to the negotiated interval
	// (`cline-oauth.ts:237`).
	PollIntervalFloorMS = 1_000
	// PollMaxFailures is the consecutive transport-failure budget while polling
	// (`cline-oauth.ts:86`).
	PollMaxFailures = 5
	// MaxOutputTokensCeiling clamps `max_tokens` (`cline-adapter.ts:72`). The
	// value is the largest catalogue entry, not a documented API limit
	// (spec risk 6).
	MaxOutputTokensCeiling = 943_718
	// balanceDivisorRaw converts the raw balance into the displayed unit
	// (`cline-credits.ts:79`). ⚠️ It has NO source evidence: `balance: 500000`
	// is guessed to be micro-USD (spec §7.1, risk 1). It is one named constant
	// so a real account can confirm or correct it in exactly one place, and it
	// is exposed as a configuration field for the same reason.
	balanceDivisorRaw = 100_000
)

// refreshLead is how long before expiry the host is asked to renew the
// credential. It matches the Jet-Hub scheduler's 1 h lead (`refresh.ts:2`) and
// the refresh deadline every other plugin in this repository reports.
const refreshLead = time.Hour

// reasoningLevels are the five thinking levels declared for EVERY model
// (`cline-adapter.ts:341-350`, table at `cline-product.ts:230-236`). The ids are
// the wire `reasoning_effort` values and are sent verbatim; `xhigh` was measured
// as indistinguishable from `high` and is deliberately absent
// (`cline-product.ts:215-225`).
var reasoningLevels = []string{"none", "low", "medium", "high", "max"}

// fallbackModel is one entry of the static catalogue table
// (`cline-product.ts:134-192`). Context window, output limit and image support
// are NOT published by any endpoint (spec risk 11), so they exist only here.
type fallbackModel struct {
	// ID is the exact model id, namespace included. Free and paid models are
	// different ids: `cline-free/deepseek-v4.1-flash` is not
	// `deepseek/deepseek-v4.1-flash` and no fuzzy matching is allowed
	// (`cline-product.ts:120-125`).
	ID string
	// Name is the display name.
	Name string
	// ContextWindow is the input+output context length.
	ContextWindow int64
	// MaxTokens is the advertised completion limit. `gemini-3.8-flash` is
	// deliberately 65 536: vertex rejects 131 072 with "supported range is from
	// 1 (inclusive) to 65537 (exclusive)" (`cline-product.ts:164-180`).
	MaxTokens int64
	// SupportsImage reports vision support.
	SupportsImage bool
	// IsFree mirrors the table's `isFree` flag. All five entries are free, and
	// the flag is kept because free status is a marketing state that moves.
	IsFree bool
}

// clineFallbackModels is `CLINE_FALLBACK_MODELS` (`cline-product.ts:134-192`), a
// snapshot measured at collection time. The table is already known to be missing
// and pre-dating entries (spec risk 3), which is exactly why the remote `free`
// array stays authoritative and this table only supplies metadata.
var clineFallbackModels = []fallbackModel{
	{ID: "stealth/space-bunny-alpha", Name: "Space Bunny Alpha", ContextWindow: 1_000_000, MaxTokens: 524_288, SupportsImage: true, IsFree: true},
	{ID: "cline-free/mimo-v2.6-flash", Name: "MiMo-V2.6 Flash", ContextWindow: 1_048_576, MaxTokens: 131_072, SupportsImage: true, IsFree: true},
	{ID: "cline-free/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", ContextWindow: 1_048_576, MaxTokens: 131_072, SupportsImage: true, IsFree: true},
	{ID: "cline-free/gemini-3.8-flash", Name: "Gemini 3.8 Flash", ContextWindow: 1_048_576, MaxTokens: 65_536, SupportsImage: true, IsFree: true},
	{ID: "cline-free/muse-spark-1.3-contributor", Name: "Muse Spark 1.3 Contributor", ContextWindow: 1_048_576, MaxTokens: 943_718, SupportsImage: true, IsFree: true},
}

// fallbackModelFor looks an id up in the static table.
func fallbackModelFor(id string) (fallbackModel, bool) {
	for _, model := range clineFallbackModels {
		if model.ID == id {
			return model, true
		}
	}
	return fallbackModel{}, false
}
