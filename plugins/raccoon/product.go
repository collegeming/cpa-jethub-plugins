package main

// Provider identity. One plugin instance owns exactly one provider key.
const (
	// ProviderKey is the stable provider identifier written into CPA auth files
	// and matched by the account pool (`raccoon-product.ts:148`).
	ProviderKey = "raccoon"
	// DisplayName is the human-readable name management clients show. It is
	// deliberately the SHORT form: `Raccoon Work (商汤)` wraps in the provider
	// tab, which is why the reference uses `Raccoon (商汤)`
	// (`raccoon-product.ts:149-153`).
	DisplayName = "Raccoon (商汤)"
	// Version is the plugin release version.
	Version = "0.1.0"
	// Author identifies the plugin author organization.
	Author = "cpa-jethub"
	// Repository is the public source location of this plugin.
	Repository = "https://github.com/collegeming/cpa-jethub-plugins"
	// Logo is a locally drawn badge, not vendor artwork: 商汤 publishes no icon
	// this repository may embed, and an invented URL would render as a broken
	// image. It follows the same badge shape the Loomy entry uses.
	Logo = `data:image/svg+xml,%3Csvg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"%3E%3Crect width="24" height="24" rx="6" fill="%230f766e"/%3E%3Ctext x="12" y="17.5" font-size="14" font-family="sans-serif" font-weight="700" fill="white" text-anchor="middle"%3E%E6%B5%A3%3C/text%3E%3C/svg%3E`
)

// Hosts and paths. `https://xiaohuanxiong.com` is the PRODUCTION base; the
// reference declares no test environment (`raccoon.ts:25-34`).
const (
	// APIBase serves every endpoint this plugin uses (`raccoon.ts:25`).
	APIBase = "https://xiaohuanxiong.com"

	// AuthPrefix is the account/authorisation prefix (`raccoon.ts:28`).
	AuthPrefix = "/api/web/auth/v1"
	// LLMPrefix is the inference and catalogue prefix (`raccoon.ts:30`).
	LLMPrefix = "/api/web/llm/v2"
	// PointsPrefix is the points prefix (`raccoon.ts:32`).
	PointsPrefix = "/api/web/points/v1"
	// DesktopPrefix is the desktop-client prefix that owns the one-off login
	// reward (`raccoon.ts:34`).
	DesktopPrefix = "/api/web/desktop/v1"

	// QRLoginCodePath polls a scanned QR code for its login result
	// (`raccoon-oauth.ts:147-151`). The code is generated LOCALLY; this endpoint
	// only reports what the scan produced.
	QRLoginCodePath = AuthPrefix + "/login_with_qrcode_code"
	// RefreshPath exchanges a refresh token for a new access token
	// (`raccoon-oauth.ts:295-297`).
	RefreshPath = AuthPrefix + "/refresh"
	// UserInfoPath returns the account profile (`raccoon-oauth.ts:336-339`).
	UserInfoPath = AuthPrefix + "/user_info"

	// ModelCatalogPath lists the models the account may call
	// (`raccoon-auth.ts:392`).
	ModelCatalogPath = LLMPrefix + "/model_catalog"
	// ChatCompletionsPath is the OpenAI-compatible SSE endpoint
	// (`raccoon-adapter.ts:310`).
	ChatCompletionsPath = LLMPrefix + "/chat/completions"

	// PointsBalancePath reports the point pools (`raccoon-credits.ts:135`).
	PointsBalancePath = PointsPrefix + "/balance"
	// PointsBillsPath is the bill history the one-off reward status is derived
	// from, because the server publishes no reward-status endpoint
	// (`raccoon-credits.ts:228-234`).
	PointsBillsPath = PointsPrefix + "/bills"
	// PointsBillsQuery is the paging the reference uses (`raccoon-credits.ts:234`).
	PointsBillsQuery = "?paging.limit=50&paging.offset=0"
	// LoginPointsGrantPath grants the one-off desktop login reward
	// (`raccoon-credits.ts:182`). It is NOT a daily check-in: the server grants
	// the daily 300 by itself and there is no endpoint for it
	// (`raccoon-credits.ts:12-15`).
	LoginPointsGrantPath = DesktopPrefix + "/login/points/grant"

	// QRLoginPageBase is the PUBLIC page the QR image points at. We never fetch
	// it: the scanning WeChat client opens it (`raccoon-oauth.ts:121-130`).
	QRLoginPageBase = APIBase + "/login/mp"
	// QRLoginAppName is the `appname` parameter, in the exact spelling the
	// reference sends (`raccoon-oauth.ts:130`).
	QRLoginAppName = "商汤小浣熊官网"
)

// Client fingerprint constants.
//
// ⚠️ These are independent of the plugin version: they identify the official
// desktop client and are pinned by the vendor's risk control
// (`raccoon-product.ts:159-161`).
const (
	// ClientPlatform is sent on chat and on every credits call. The one-off
	// reward call is REJECTED without it (`raccoon-product.ts:160`,
	// `raccoon-credits.ts:171-173`).
	ClientPlatform = "desktop-windows"
	// ClientVersion is sent on credits calls only — never on chat
	// (`raccoon-product.ts:161`, `raccoon-adapter.ts:297-304`).
	ClientVersion = "v1.0.35"
	// Language is the `X-Raccoon-Language` value (`raccoon.ts:281`).
	Language = "zh"
)

// Timeouts and lifetimes. They are NOT uniform, which is trap #31: a single
// global timeout is wrong somewhere.
const (
	// RequestTimeoutMS bounds auth, user_info and credits calls
	// (`raccoon.ts:55`).
	RequestTimeoutMS = 60_000
	// CatalogueTimeoutMS bounds `GET /model_catalog`, which the reference caps at
	// 20 s rather than 60 (`raccoon-auth.ts:401`).
	CatalogueTimeoutMS = 20_000
	// RefreshWindowSeconds is the pre-refresh window this port implements
	// DELIBERATELY. In the reference the constant is dead code: refresh fires
	// only once `exp <= now` (`raccoon.ts:52,159-162`; trap #12). With ~3-hour
	// tokens and a lazy refresh schedule, pre-refreshing 300 s early is strictly
	// safer and there is no evidence it breaks anything.
	RefreshWindowSeconds = 300
	// QRPollIntervalSeconds is the page's meta-refresh cadence, matching the
	// reference's 2 s poll (`raccoon.ts:57-58`, `raccoon-login-page.ts:421`).
	QRPollIntervalSeconds = 2
	// LoginTimeoutMS bounds one interactive login session, matching the
	// reference's 300 s flow budget (`raccoon.ts:61`).
	LoginTimeoutMS = 300_000
	// ModelCacheTTLMS bounds how long a fetched catalogue is reused.
	ModelCacheTTLMS = 2 * 60 * 60 * 1000
	// DefaultMaxOutputTokens is advertised when a model publishes no usable
	// `max_tokens` (`raccoon-adapter.ts:58-67`).
	DefaultMaxOutputTokens = 65_536
)

// Points constants (`raccoon-credits.ts:38-41`).
const (
	// LoginRewardPoints is the fallback amount reported for the one-off desktop
	// login reward when the response omits it, or when the bill entry carries no
	// positive amount.
	LoginRewardPoints = 3000
	// LoginRewardEventName identifies the one-off reward in the bill history.
	// Matching on `biz_type` alone is wrong: the new-user registration gift
	// shares it (`raccoon-credits.ts:220-226`).
	LoginRewardEventName = "桌面端登录奖励"
	// LoginRewardBizType is the bill type the reward carries.
	LoginRewardBizType = "reward_grant"
)

// QR status values (`raccoon.ts:64-69`). Anything else — including a network
// error, a missing status and a `success` without a token — degrades to pending
// (`raccoon-oauth.ts:152-179`).
const (
	qrStatusPending  = "pending"
	qrStatusLogging  = "logging"
	qrStatusCanceled = "canceled"
	qrStatusSuccess  = "success"
)
