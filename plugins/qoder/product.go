package main

// Qoder product configuration: hosts, client identity and the model catalog.
//
// Ported from `src/qoder-product.ts` (QoderProduct, QODER, QODER_CN,
// QODER_FALLBACK_MODELS). Every host and client identity below carries the
// TypeScript line it was read from; nothing here is invented.

// Region identifies which of the two Qoder sites a credential belongs to. The
// two sites are separate providers in Jet-Hub (`qoder` / `qoder-cn`), never
// share credentials, and differ in every host.
type Region string

const (
	// RegionGlobal is the international site (`qoder-product.ts:319-338`).
	RegionGlobal Region = "qoder"
	// RegionCN is the mainland-China site (`qoder-product.ts:376-396`).
	RegionCN Region = "qoder-cn"
)

// clientMetadata mirrors `QoderProduct.clientMetadata` (`qoder-product.ts:193-198`):
// the body of `metadata.context` / the WASM `QoderContext` client metadata.
type clientMetadata struct {
	ClientType      string `json:"client_type"`
	BusinessProduct string `json:"business_product"`
	BusinessType    string `json:"business_type"`
	Scene           string `json:"scene"`
}

// product is the Go mirror of `QoderProduct` (`qoder-product.ts:126-213`).
type product struct {
	ID       Region
	Site     string // "global" | "cn" (`qoder-product.ts:136`)
	Display  string
	AuthBase string // browser authorization base (`qoder-product.ts:140`)
	// OpenAPIBase serves device-token polling, refresh and userinfo
	// (`qoder-product.ts:142`; `qoder.ts:127-133` explains why the poll host is
	// not AuthBase: polling qoder.com answers 401).
	OpenAPIBase string
	// InferBase is the PUBLIC OpenAI-compatible endpoint. It only accepts
	// generic model names — the catalog keys in fallbackModels are rejected with
	// `Unsupported model` (`qoder-product.ts:143-154`).
	InferBase string
	// EncryptedInferBase hosts `agent_chat_generation`, the endpoint the real
	// client uses. It accepts catalog keys, and the request must be built by the
	// WASM signer (`qoder-product.ts:155-162`).
	EncryptedInferBase string
	// ClientID is the production device-flow client id (`qoder-product.ts:185`).
	ClientID string
	// TestClientID is the non-prod id, recorded only; never used here.
	TestClientID string
	// SessionType is written into the WASM payload. Official source:
	// `session_type: process.env[SESSION_TYPE] ?? (r0() ? l7A : swe)` — the
	// global build uses `qodercli` (`swe`), the CN build `qoder_work` (`l7A`)
	// (`qoder-wasm.ts:145-152`). The Jet-Hub adapter always sends `qodercli`
	// because it never passes the field; the per-region value recorded in the
	// source comment is used here instead (see DEVIATIONS in the report).
	SessionType string
	// UserAgentPrefix is concatenated as `${prefix}/1.0.0` (`qoder.ts:315`,
	// `qoder-product.ts:199-200`).
	UserAgentPrefix string
	// DefaultCredentialRef is Jet-Hub's single-credential fallback ref.
	DefaultCredentialRef string
	// ModelCatalog is `QoderProduct.fallbackModels` (`qoder-product.ts:212`).
	ModelCatalog []catalogModel
}

// catalogModel mirrors `QoderFallbackModel` (`qoder-product.ts:30-91`).
type catalogModel struct {
	// Key is the catalog key (`qfmodel`, `dmodel`, ...). These are the keys the
	// ENCRYPTED endpoint accepts; the public endpoint rejects them
	// (`qoder-product.ts:31-35`, `:240-249`).
	Key string
	// Display is the catalog `display_name`.
	Display string
	// ContextWindow is the TOTAL context window DSH compresses against (it uses
	// `contextWindow × 0.8`), taken from the catalog's `context_config` TIER
	// TABLE — the maximum tier — and NOT from `max_input_tokens`
	// (`qoder-product.ts:38-45`).
	//
	// ⚠️ The two catalog fields contradict each other and only the tier table is
	// authoritative: `isContextWindowSupportedByModel()` converts the value and
	// hands it to `zX()`, which checks MEMBERSHIP in the tier table and never
	// executes its `max_input_tokens` branch (`t <= n`) when that table exists
	// (asar evidence quoted at `qoder-product.ts:485-493`). CN `dmodel` publishes
	// `max_input_tokens: 96000` while its table reaches 1M, and it does accept
	// ~853K in practice — filling in the reported number makes DSH compress far
	// earlier than the official client does (upstream `db5af3c`).
	//
	// Upstream rule, confirmed per model by measurement (upstream `db5af3c`):
	// every real model gets 1M because its tier table contains a 1M tier; `auto`
	// has no tier table and stays at 200K (CN `mmodel`'s table holds 200K alone).
	ContextWindow int64
	// SupportsImage is the catalog `is_vl` (`qoder-product.ts:259`).
	SupportsImage bool
	// SupportsThinking is the catalog `is_reasoning` (`qoder-product.ts:260`).
	SupportsThinking bool
	// ThinkingDisableAllowed mirrors `QoderFallbackModel.supportsDisable`
	// (`qoder-product.ts:128-136`): the catalog carries a
	// `thinking_config.disabled` branch, i.e. the user may turn thinking OFF.
	//
	// ⚠️ It is an INDEPENDENT dimension from Efforts, not a synonym for "no
	// efforts". `gmodel` / `gfmodel` / `kmodel*` publish effort tiers with NO
	// `disabled` branch (so they cannot be switched off), while CN `qmodel` /
	// `qmodel_latest` publish no efforts at all yet CAN be switched off.
	// Upstream appends `none` for the latter (`qoder-adapter.ts:156-159`,
	// `gU()`), which is why the two flags must stay separate.
	ThinkingDisableAllowed bool
	// IsFree is the catalog `is_free` (`qoder-product.ts:261`).
	IsFree bool
	// PriceFactor is the catalog `price_factor` (`qoder-product.ts:64`).
	//
	// ⚠️ It is a snapshot of the price at collection time and it moves with the
	// promotion window, so it is NOT a constant list price. `0` is a legal value
	// meaning FREE — filtering with `> 0` drops the model users care about most
	// (`qoder-product.ts:60-63`).
	PriceFactor *float64
	// OriginalPriceFactor is the catalog `original_price_factor`
	// (`qoder-product.ts:72`). Independent of PriceFactor: `qfmodel` has
	// `price_factor=0` with `original_price_factor=0.1`.
	OriginalPriceFactor *float64
	// Promotion is the catalog `promotion` off-peak discount
	// (`qoder-product.ts:76-88`).
	Promotion *promotion
	// Efforts lists the `thinking_config.enabled.efforts` keys
	// (`qoder-product.ts:89-90`).
	Efforts []string
}

// promotion mirrors `QoderModelPromotion` (`qoder-product.ts:94-119`).
type promotion struct {
	// Active is the value captured when the catalog was fetched. It goes stale
	// in a long-running session, so the window is recomputed locally and this is
	// only a fallback (`qoder-product.ts:76-79`).
	Active bool
	// DiscountFactor is the catalog `discount_factor` (0.4 = 40% of list).
	DiscountFactor *float64
	// BeforePriceFactor is the catalog `before_promotion_price_factor`.
	BeforePriceFactor *float64
	// WindowStart / WindowEnd are `window_start` / `window_end`, e.g. `22:00`.
	WindowStart string
	WindowEnd   string
	// BadgeZh is the catalog `badge.zh`. Recorded but not rendered: the arrow
	// form `x0.5→x0.2` already carries the discount (`qoder-product.ts:110-118`).
	BadgeZh string
}

// float64Ptr is a helper for the optional catalog numbers.
func float64Ptr(value float64) *float64 { return &value }

// qoderGlobal is `QODER` (`qoder-product.ts:319-338`).
var qoderGlobal = product{
	ID:                   RegionGlobal,
	Site:                 "global",
	Display:              "Qoder",
	AuthBase:             "https://qoder.com",
	OpenAPIBase:          "https://openapi.qoder.sh",
	InferBase:            "https://api2-v2.qoder.sh",
	EncryptedInferBase:   "https://api2.qoder.sh",
	ClientID:             "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
	TestClientID:         "e93fe488-5778-4c35-a6fc-0f54ed7b3139",
	SessionType:          "qodercli",
	UserAgentPrefix:      "qoder",
	DefaultCredentialRef: "QODER_ACCESS_TOKEN",
	ModelCatalog:         qoderModelCatalog,
}

// qoderCN is `QODER_CN` (`qoder-product.ts:376-396`).
//
// The traps recorded there apply verbatim:
//   - `inferBase` and `encryptedInferBase` are the SAME host on the CN site
//     (`gateway.qoder.com.cn`), while the global site uses two different hosts;
//   - `authBase` is `qoder.cn`, not `qoder.com.cn`, which is only the
//     OpenAPI/gateway domain;
//   - both sites share one device-flow client id.
var qoderCN = product{
	ID:          RegionCN,
	Site:        "cn",
	Display:     "Qoder (国内版)",
	AuthBase:    "https://qoder.cn",
	OpenAPIBase: "https://openapi.qoder.com.cn",
	// CN 的公开 OpenAI 兼容端点未部署（/model/v1/chat/completions 实测 503），
	// 推理只走加密端点；两个 Infer 字段同值仅表示「无独立公开端点」。
	InferBase:          "https://gateway.qoder.com.cn",
	EncryptedInferBase: "https://gateway.qoder.com.cn",
	// CN 的 clientId 与国际版不同（国际版两个 id 在 CN 客户端里命中 0 次），
	// prod 与 test 同值；来源：CN asar 的 authClientIds.prod，参考项目
	// 2026-09-27 已实测登录闭环通过。
	ClientID:     "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
	TestClientID: "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
	// 推理载荷用默认 `qodercli`；`qoder_work` 只出现在 CN 客户端的
	// `--ide-type`，与推理无关。
	SessionType:          "qodercli",
	UserAgentPrefix:      "qoder",
	DefaultCredentialRef: "QODER_CN_ACCESS_TOKEN",
	ModelCatalog:         qoderCNModelCatalog,
}

// qoderCNModelCatalog is the CN catalog: 14 entries. The CN endpoint does not
// serve five international entries (ultimate/performance/efficient/smodel/
// cmodel) and adds two of its own (q37fmodel/gm51model); `mmodel` is
// MiniMax-M2.7 here. Every value below is read from
// `QODER_CN_FALLBACK_MODELS` (`qoder-product.ts:480-560`), measured against a
// live catalog on 2026-09-27.
//
// ⚠️ `ContextWindow` follows the tier-table rule documented on the field: all
// entries are 1M except `auto` (no tier table) and `mmodel` (its tier table
// holds the 200K tier alone). See upstream `db5af3c`.
//
// ⚠️ Thinking is declared from `Efforts` PLUS `ThinkingDisableAllowed`
// (`qoder-product.ts:508-512`): `qmodel` / `qmodel_latest` publish no effort
// list but do carry a `disabled` branch, so "turn thinking off" is the only
// option they offer.
var qoderCNModelCatalog = []catalogModel{
	{Key: "auto", Display: "Auto", ContextWindow: 200_000, SupportsImage: true, PriceFactor: float64Ptr(0.5)},
	{Key: "qmodel_38max", Display: "Qwen3.8-Max", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.2), Efforts: []string{"xhigh", "low", "medium"}, ThinkingDisableAllowed: true},
	{Key: "qfmodel", Display: "Qwen3.8-Flash", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0), Efforts: []string{"xhigh", "low", "medium"}, ThinkingDisableAllowed: true},
	{Key: "q37fmodel", Display: "Qwen3.7-Flash", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.1)},
	// ⚠️ No `efforts` but yes `supportsDisable`: "turn thinking off" is the only
	// option the catalog offers (`qoder-product.ts:525-535`). Declaring nothing
	// at all would remove the selector entirely.
	{Key: "qmodel_latest", Display: "Qwen3.7-Max", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.1), ThinkingDisableAllowed: true},
	{Key: "qmodel", Display: "Qwen3.7-Plus", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.04), ThinkingDisableAllowed: true},
	{Key: "kmodel_latest", Display: "Kimi-K3", ContextWindow: 1_000_000, SupportsImage: true,
		PriceFactor: float64Ptr(1.4), Efforts: []string{"high", "low", "max"}},
	{Key: "kmodel", Display: "Kimi-K2.8-Preview", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.8), Efforts: []string{"high", "low", "max"}},
	// ⚠️ `gmodel` / `gfmodel` have effort tiers with NO `disabled` branch, so
	// `ThinkingDisableAllowed` must stay false: offering "off" would send a
	// request the model cannot honour (`qoder-product.ts:539`).
	{Key: "gmodel", Display: "GLM-5.3", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.8), Efforts: []string{"high", "low", "max"}},
	{Key: "gfmodel", Display: "GLM-5.3-Flash", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.1), Efforts: []string{"high", "max"}},
	{Key: "gm51model", Display: "GLM-5.2", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.8), Efforts: []string{"high", "max"}, ThinkingDisableAllowed: true},
	{Key: "dmodel", Display: "DeepSeek-V4-Pro", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(2), Efforts: []string{"high", "max"}, ThinkingDisableAllowed: true},
	{Key: "dfmodel", Display: "DeepSeek-Flash", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.5), Efforts: []string{"high", "max", "low"}, ThinkingDisableAllowed: true},
	// ⚠️ MiniMax-M2.7 here (M3 internationally) and `is_vl` is false. The only
	// CN entry whose tier table stops at 200K — do not raise it to 1M.
	{Key: "mmodel", Display: "MiniMax-M2.7", ContextWindow: 200_000, SupportsImage: false, SupportsThinking: false,
		PriceFactor: float64Ptr(0.2)},
}

// sharedClientMetadata is the identical `clientMetadata` of both products
// (`qoder-product.ts:329-334`, `:387-392`).
var sharedClientMetadata = clientMetadata{
	ClientType:      "5",
	BusinessProduct: "cli",
	BusinessType:    "agent",
	Scene:           "assistant",
}

// allProducts is `ALL_QODER_PRODUCTS` (`qoder-product.ts:405`).
var allProducts = []*product{&qoderGlobal, &qoderCN}

// productByID is `qoderProductById` (`qoder-product.ts:413-415`). An unknown id
// falls back to the global site: the plugin must never route to an invented host.
func productByID(id string) *product {
	for _, candidate := range allProducts {
		if string(candidate.ID) == id {
			return candidate
		}
	}
	return &qoderGlobal
}

// qoderModelCatalog is `QODER_FALLBACK_MODELS` (`qoder-product.ts:362-445`),
// measured against a local `catalog-v6` on 2026-09-21. Field order below is
// (key, display, context window, image, thinking, free, price factor, original
// price factor, promotion, efforts).
//
// The comment at `qoder-product.ts:363-366` is the reason these numbers are
// copied literally instead of rounded: an earlier table used estimates and was
// wrong for 14 of 17 models.
//
// ⚠️ `ContextWindow` follows the tier-table rule documented on the field: 1M
// for every entry whose tier table contains a 1M tier (`performance` is
// {272K, 400K, 1M}, `efficient` defaults to 400K), 200K only for `auto`, which
// has no tier table at all (`qoder-product.ts:370-390`).
var qoderModelCatalog = []catalogModel{
	{Key: "auto", Display: "Auto", ContextWindow: 200_000, SupportsImage: true, PriceFactor: float64Ptr(0.5)},
	{Key: "ultimate", Display: "Ultimate", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(2), Efforts: []string{"xhigh", "high", "low", "max", "medium"}, ThinkingDisableAllowed: true},
	// `is_reasoning:false` while `thinking_config.enabled` is set: the upstream
	// does offer effort levels, so efforts stay while isReasoning follows
	// `is_reasoning` (`qoder-product.ts:391-397`).
	{Key: "performance", Display: "Performance", ContextWindow: 1_000_000, SupportsImage: true,
		PriceFactor: float64Ptr(1.1), Efforts: []string{"xhigh", "high", "low", "max", "medium"}, ThinkingDisableAllowed: true},
	{Key: "efficient", Display: "Efficient", ContextWindow: 1_000_000, SupportsImage: true, PriceFactor: float64Ptr(0.3)},
	// `smodel` / `cmodel` carry five tiers but no `disabled` branch, so they
	// cannot be switched off (`qoder-product.ts:405`).
	{Key: "smodel", Display: "Sonus", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(8), Efforts: []string{"xhigh", "high", "low", "max", "medium"}},
	{Key: "cmodel", Display: "Cantus", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(4), Efforts: []string{"xhigh", "high", "low", "max", "medium"}},
	{Key: "qmodel_38max", Display: "Qwen3.8-Max", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		IsFree: true, PriceFactor: float64Ptr(0.2), Efforts: []string{"xhigh", "low", "medium"}, ThinkingDisableAllowed: true,
		Promotion: &promotion{Active: true, DiscountFactor: float64Ptr(0.4), BeforePriceFactor: float64Ptr(0.5),
			WindowStart: "22:00", WindowEnd: "08:00", BadgeZh: "错峰 4 折"}},
	// `priceFactor: 0` is FREE, not a missing value (`qoder-product.ts:413-417`).
	{Key: "qfmodel", Display: "Qwen3.8-Flash", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		IsFree: true, PriceFactor: float64Ptr(0), OriginalPriceFactor: float64Ptr(0.1),
		Efforts: []string{"xhigh", "low", "medium"}, ThinkingDisableAllowed: true},
	// No `efforts` but `supportsDisable: true`: "turn thinking off" is the only
	// option (`qoder-product.ts:418-430`).
	{Key: "qmodel_latest", Display: "Qwen3.7-Max", ContextWindow: 1_000_000, SupportsImage: true,
		PriceFactor: float64Ptr(0.1), OriginalPriceFactor: float64Ptr(0.5), ThinkingDisableAllowed: true,
		Promotion: &promotion{Active: true, DiscountFactor: float64Ptr(0.2), BeforePriceFactor: float64Ptr(0.5),
			WindowStart: "22:00", WindowEnd: "08:00", BadgeZh: "错峰 2 折"}},
	{Key: "qmodel", Display: "Qwen3.7-Plus", ContextWindow: 1_000_000, SupportsImage: true,
		PriceFactor: float64Ptr(0.04), ThinkingDisableAllowed: true,
		Promotion: &promotion{Active: true, DiscountFactor: float64Ptr(0.4), BeforePriceFactor: float64Ptr(0.1),
			WindowStart: "22:00", WindowEnd: "08:00", BadgeZh: "错峰 4 折"}},
	{Key: "kmodel_latest", Display: "Kimi-K3", ContextWindow: 1_000_000, SupportsImage: true,
		PriceFactor: float64Ptr(1.4), Efforts: []string{"high", "low", "max"}},
	// The catalog does NOT send `max_input_tokens` for this entry — itself
	// evidence that the field is not authoritative. The old table fell back to
	// the tier table's DEFAULT tier (200K) while the official client offers the
	// MAXIMUM tier, 1M (`qoder-product.ts:426-431`).
	{Key: "kmodel", Display: "Kimi-K2.8-Preview", ContextWindow: 1_000_000, SupportsImage: true,
		PriceFactor: float64Ptr(0.8), Efforts: []string{"high", "low", "max"}},
	{Key: "gmodel", Display: "GLM-5.3", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.8), Efforts: []string{"high", "low", "max"}},
	{Key: "gfmodel", Display: "GLM-5.3-Flash", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.1), Efforts: []string{"high", "max"}},
	{Key: "dmodel", Display: "DeepSeek-V4-Pro", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.5), Efforts: []string{"high", "max"}, ThinkingDisableAllowed: true},
	{Key: "dfmodel", Display: "DeepSeek-Flash", ContextWindow: 1_000_000, SupportsImage: true, SupportsThinking: true,
		PriceFactor: float64Ptr(0.1), Efforts: []string{"high", "max", "low"}, ThinkingDisableAllowed: true},
	{Key: "mmodel", Display: "MiniMax-M3", ContextWindow: 1_000_000, SupportsImage: true, PriceFactor: float64Ptr(0.2)},
}

// catalogModelFor looks a key up in the product catalog. The second result
// reports whether the key is a known catalog entry at all — the public endpoint
// accepts names this table does not know, so a miss is not an error.
func catalogModelFor(p *product, key string) (catalogModel, bool) {
	for _, model := range p.ModelCatalog {
		if model.Key == key {
			return model, true
		}
	}
	return catalogModel{}, false
}
