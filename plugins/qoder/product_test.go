package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// ts returns a UTC timestamp for a given UTC+8 wall clock, so window tests read
// the way the catalog describes them.
func ts(hour, minute int) time.Time {
	return time.Date(2026, 9, 21, hour-8, minute, 0, 0, time.UTC)
}

func factor(value float64) *float64 { return &value }

// TestPriceFactorZeroMeansFree is the single most misread field in the catalog:
// `price_factor: 0` is FREE, not "no multiplier", and filtering with `> 0` drops
// the model users care about most (`qoder-product.ts:60-63`).
func TestPriceFactorZeroMeansFree(t *testing.T) {
	model := catalogModel{Key: "qfmodel", Display: "Qwen3.8-Flash", PriceFactor: factor(0)}
	if got := modelDisplayName(model, ts(12, 0)); got != "Qwen3.8-Flash · 免费" {
		t.Fatalf("display name = %q, want the free marker (0 must not render as x0)", got)
	}
	if model.PriceFactor == nil {
		t.Fatal("price factor 0 was treated as absent")
	}
}

// TestDisplayNamePromotionWindow pins the arrow form and the local recomputation
// of the window. The catalog's `active` flag is a snapshot and goes stale, so the
// window is what decides (`qoder-adapter.ts:400-419`, `:446-466`).
func TestDisplayNamePromotionWindow(t *testing.T) {
	model := catalogModel{
		Key: "qmodel_38max", Display: "Qwen3.8-Max", PriceFactor: factor(0.2),
		Promotion: &promotion{
			Active: true, DiscountFactor: factor(0.4), BeforePriceFactor: factor(0.5),
			WindowStart: "22:00", WindowEnd: "08:00",
		},
	}
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{"inside the window after midnight", ts(2, 0), "Qwen3.8-Max · x0.5→x0.2"},
		{"inside the window before midnight", ts(23, 30), "Qwen3.8-Max · x0.5→x0.2"},
		{"outside the window", ts(12, 0), "Qwen3.8-Max · x0.5"},
		{"window start is inclusive", ts(22, 0), "Qwen3.8-Max · x0.5→x0.2"},
		{"window end is exclusive", ts(8, 0), "Qwen3.8-Max · x0.5"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := modelDisplayName(model, testCase.now); got != testCase.want {
				t.Fatalf("modelDisplayName = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestDisplayNameFallsBackToTheSnapshotWhenWindowIsMissing keeps a catalog
// without window fields usable.
func TestDisplayNameFallsBackToTheSnapshotWhenWindowIsMissing(t *testing.T) {
	active := catalogModel{Display: "M", PriceFactor: factor(1),
		Promotion: &promotion{Active: true, DiscountFactor: factor(0.5), BeforePriceFactor: factor(2)}}
	if got := modelDisplayName(active, ts(12, 0)); got != "M · x2→x1" {
		t.Fatalf("display name = %q, want the discounted form from the snapshot", got)
	}
	inactive := catalogModel{Display: "M", PriceFactor: factor(1),
		Promotion: &promotion{Active: false, DiscountFactor: factor(0.5), BeforePriceFactor: factor(2)}}
	if got := modelDisplayName(inactive, ts(12, 0)); got != "M · x2" {
		t.Fatalf("display name = %q, want the list price", got)
	}
}

// TestDisplayNameWithoutPriceLeavesTheNameAlone avoids inventing a multiplier.
func TestDisplayNameWithoutPriceLeavesTheNameAlone(t *testing.T) {
	if got := modelDisplayName(catalogModel{Display: "Mystery"}, ts(12, 0)); got != "Mystery" {
		t.Fatalf("display name = %q, want the bare name", got)
	}
}

// TestPromotionWindowUsesUTC8 pins the timezone: the catalog's timezone is
// Asia/Singapore, so 22:00 is 14:00 UTC.
func TestPromotionWindowUsesUTC8(t *testing.T) {
	promo := &promotion{WindowStart: "22:00", WindowEnd: "08:00", Active: false}
	if !promotionActiveNow(promo, time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)) {
		t.Fatal("14:00 UTC (22:00 UTC+8) must be inside the window")
	}
	if promotionActiveNow(promo, time.Date(2026, 9, 21, 13, 59, 0, 0, time.UTC)) {
		t.Fatal("13:59 UTC (21:59 UTC+8) must be outside the window")
	}
}

// TestRegionSelectionPicksTheRightHosts is the highest-risk configuration: the
// two sites use different hosts, and the CN site uses ONE host for both paths
// while the global site uses two (`qoder-product.ts:319-338`, `:376-396`).
func TestRegionSelectionPicksTheRightHosts(t *testing.T) {
	global := productByID(string(RegionGlobal))
	if global.AuthBase != "https://qoder.com" || global.OpenAPIBase != "https://openapi.qoder.sh" {
		t.Fatalf("global hosts drifted: %#v", global)
	}
	if global.InferBase != "https://api2-v2.qoder.sh" || global.EncryptedInferBase != "https://api2.qoder.sh" {
		t.Fatalf("global inference hosts drifted: %#v", global)
	}
	if global.InferBase == global.EncryptedInferBase {
		t.Fatal("the global site must keep the public and encrypted hosts apart (mixing them 404s)")
	}

	cn := productByID(string(RegionCN))
	if cn.AuthBase != "https://qoder.cn" {
		t.Fatalf("CN authBase = %q, want https://qoder.cn (not qoder.com.cn)", cn.AuthBase)
	}
	if cn.OpenAPIBase != "https://openapi.qoder.com.cn" {
		t.Fatalf("CN openApiBase = %q", cn.OpenAPIBase)
	}
	if cn.InferBase != "https://gateway.qoder.com.cn" || cn.EncryptedInferBase != "https://gateway.qoder.com.cn" {
		t.Fatalf("CN must use one gateway host for both paths: %#v", cn)
	}
	// The inference payload uses the default `qodercli` on BOTH sites;
	// `qoder_work` belongs to the CN client's `--ide-type`, not inference.
	if cn.SessionType != "qodercli" || global.SessionType != "qodercli" {
		t.Fatalf("session types drifted: global=%q cn=%q", global.SessionType, cn.SessionType)
	}
	// The CN device-flow client id differs from the international one (the
	// international ids appear 0 times in the CN client); prod and test share
	// the same value there.
	if cn.ClientID != "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa" {
		t.Fatalf("CN client id = %q, want the CN asar value", cn.ClientID)
	}
}

// TestProductByIDFallsBackToGlobalNeverInvents checks that an unknown region
// cannot select an unknown host.
func TestProductByIDFallsBackToGlobalNeverInvents(t *testing.T) {
	if got := productByID("nope"); got.ID != RegionGlobal {
		t.Fatalf("productByID(nope) = %q, want the global fallback", got.ID)
	}
}

// TestCatalogKeysAreTheMeasuredTable guards the model catalog against accidental
// edits: every key here is one the encrypted endpoint accepts
// (`qoder-product.ts:269-316`).
func TestCatalogKeysAreTheMeasuredTable(t *testing.T) {
	want := []string{
		"auto", "ultimate", "performance", "efficient", "smodel", "cmodel",
		"qmodel_38max", "qfmodel", "qmodel_latest", "qmodel", "kmodel_latest",
		"kmodel", "gmodel", "gfmodel", "dmodel", "dfmodel", "mmodel",
	}
	p := productByID(string(RegionGlobal))
	if len(p.ModelCatalog) != len(want) {
		t.Fatalf("catalog has %d entries, want %d", len(p.ModelCatalog), len(want))
	}
	for index, key := range want {
		if p.ModelCatalog[index].Key != key {
			t.Errorf("catalog[%d] = %q, want %q", index, p.ModelCatalog[index].Key, key)
		}
	}
	// The CN catalog is its own 14-entry table: no ultimate/performance/
	// efficient/smodel/cmodel, plus q37fmodel and gm51model.
	cnWant := []string{
		"auto", "qmodel_38max", "qfmodel", "q37fmodel", "qmodel_latest",
		"qmodel", "kmodel_latest", "kmodel", "gmodel", "gfmodel", "gm51model",
		"dmodel", "dfmodel", "mmodel",
	}
	cn := productByID(string(RegionCN))
	if len(cn.ModelCatalog) != len(cnWant) {
		t.Fatalf("CN catalog has %d entries, want %d", len(cn.ModelCatalog), len(cnWant))
	}
	for index, key := range cnWant {
		if cn.ModelCatalog[index].Key != key {
			t.Errorf("CN catalog[%d] = %q, want %q", index, cn.ModelCatalog[index].Key, key)
		}
	}
}

// TestStaticModelInfosKeepsFreeAndImageCapability checks the two fields whose
// loss causes the confusing failures: a free model shown with a price, or an
// image model advertised as text-only.
func TestStaticModelInfosKeepsFreeAndImageCapability(t *testing.T) {
	infos := staticModelInfos(DefaultConfig(), RegionGlobal)
	byID := map[string]struct {
		display     string
		modalities  []string
		contextSize int64
	}{}
	for _, info := range infos {
		byID[info.ID] = struct {
			display     string
			modalities  []string
			contextSize int64
		}{info.DisplayName, info.SupportedInputModalities, info.ContextLength}
	}
	flash, ok := byID["qfmodel"]
	if !ok {
		t.Fatal("qmodel qfmodel is missing from the catalog")
	}
	if !strings.Contains(flash.display, "免费") {
		t.Errorf("qfmodel display name = %q, want the free marker", flash.display)
	}
	if len(flash.modalities) != 2 || flash.modalities[1] != "image" {
		t.Errorf("qfmodel modalities = %v, want text+image (is_vl=true)", flash.modalities)
	}
	if flash.contextSize != 1_000_000 {
		t.Errorf("qfmodel context = %d, want 1000000 (the tier table's maximum)", flash.contextSize)
	}
}

// TestContextWindowsComeFromTheTierTable is the regression guard for upstream
// `db5af3c`: `ContextWindow` must be the catalog's `context_config` MAXIMUM tier,
// never its `max_input_tokens`. `dmodel` is the reported case — the catalog
// publishes 96000 while the tier table reaches 1M, and the official client only
// honours the latter, so a 96K value makes DSH compress at 76.8K instead of 800K.
func TestContextWindowsComeFromTheTierTable(t *testing.T) {
	cases := []struct {
		region Region
		key    string
		want   int64
	}{
		// The measured case, in BOTH regions: `dmodel` is 1M, not 96K.
		{RegionCN, "dmodel", 1_000_000},
		{RegionGlobal, "dmodel", 1_000_000},
		// `auto` is the one entry with no tier table: it legitimately stays 200K.
		{RegionCN, "auto", 200_000},
		{RegionGlobal, "auto", 200_000},
		// CN `mmodel`'s tier table holds the 200K tier alone.
		{RegionCN, "mmodel", 200_000},
		// Every other real model publishes a 1M tier.
		{RegionCN, "qmodel", 1_000_000},
		{RegionCN, "qmodel_latest", 1_000_000},
		{RegionCN, "qfmodel", 1_000_000},
		{RegionCN, "gmodel", 1_000_000},
		{RegionGlobal, "qfmodel", 1_000_000},
		{RegionGlobal, "kmodel", 1_000_000},
		{RegionGlobal, "mmodel", 1_000_000},
		{RegionGlobal, "smodel", 1_000_000},
		{RegionGlobal, "efficient", 1_000_000},
	}
	for _, testCase := range cases {
		t.Run(string(testCase.region)+"/"+testCase.key, func(t *testing.T) {
			model, known := catalogModelFor(productByID(string(testCase.region)), testCase.key)
			if !known {
				t.Fatalf("%s is missing from the %s catalog", testCase.key, testCase.region)
			}
			if model.ContextWindow != testCase.want {
				t.Fatalf("%s/%s ContextWindow = %d, want %d", testCase.region, testCase.key,
					model.ContextWindow, testCase.want)
			}
		})
	}
}

// TestNoCatalogEntryKeepsAStaleWindow sweeps both tables: no entry may carry one
// of the values the tier-table rule retires (the old 180K placeholder, or
// `dmodel`'s 96K from `max_input_tokens`).
func TestNoCatalogEntryKeepsAStaleWindow(t *testing.T) {
	stale := map[int64]string{
		180_000: "the pre-db5af3c placeholder",
		96_000:  "dmodel's max_input_tokens (the wrong field)",
	}
	for _, p := range allProducts {
		for _, model := range p.ModelCatalog {
			if why, bad := stale[model.ContextWindow]; bad {
				t.Errorf("%s/%s ContextWindow = %d, which is %s",
					p.ID, model.Key, model.ContextWindow, why)
			}
		}
	}
}

// TestModelInfoForReportsTheTableWindow pins the host-facing projection: both
// `ContextLength` and `InputTokenLimit` must equal the (corrected) table value,
// because the compaction threshold is derived from them.
func TestModelInfoForReportsTheTableWindow(t *testing.T) {
	cases := []struct {
		region Region
		key    string
		want   int64
	}{
		{RegionCN, "dmodel", 1_000_000},
		{RegionCN, "auto", 200_000},
		{RegionGlobal, "dmodel", 1_000_000},
		{RegionGlobal, "auto", 200_000},
	}
	for _, testCase := range cases {
		t.Run(string(testCase.region)+"/"+testCase.key, func(t *testing.T) {
			p := productByID(string(testCase.region))
			model, known := catalogModelFor(p, testCase.key)
			if !known {
				t.Fatalf("%s is missing from the %s catalog", testCase.key, testCase.region)
			}
			info := modelInfoFor(model, ts(12, 0))
			if info.ContextLength != testCase.want {
				t.Errorf("ContextLength = %d, want %d", info.ContextLength, testCase.want)
			}
			if info.InputTokenLimit != testCase.want {
				t.Errorf("InputTokenLimit = %d, want %d", info.InputTokenLimit, testCase.want)
			}
		})
	}
}

// TestStaticModelInfosAddsDeclaredPublicModels admits user-declared generic names
// without inventing any.
func TestStaticModelInfosAddsDeclaredPublicModels(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PublicModels = []string{"qwen-flash", "qfmodel"}
	infos := staticModelInfos(cfg, RegionGlobal)
	found := 0
	for _, info := range infos {
		if info.ID == "qwen-flash" {
			found++
			if !info.UserDefined {
				t.Error("a declared public model must be marked user-defined")
			}
		}
	}
	if found != 1 {
		t.Fatalf("declared public model appears %d times, want 1", found)
	}
	if len(infos) != len(qoderModelCatalog)+1 {
		t.Fatalf("model count = %d, want the catalog plus one declared name", len(infos))
	}
}

// TestThinkingLevelsForAppendsOffOnlyWhenDisableIsAllowed is the two-dimension
// rule from upstream `c94c3fa`: effort tiers come from the catalog, and "turn
// thinking off" is a SEPARATE flag that appends `none` (the client's `gU()`).
func TestThinkingLevelsForAppendsOffOnlyWhenDisableIsAllowed(t *testing.T) {
	cases := []struct {
		name  string
		model catalogModel
		want  []string
	}{
		{
			name:  "efforts without a disabled branch gain nothing",
			model: catalogModel{Efforts: []string{"high", "low", "max"}},
			want:  []string{"high", "low", "max"},
		},
		{
			name:  "efforts plus a disabled branch gain none",
			model: catalogModel{Efforts: []string{"high", "max"}, ThinkingDisableAllowed: true},
			want:  []string{"high", "max", "none"},
		},
		{
			name:  "no efforts but a disabled branch is off alone",
			model: catalogModel{ThinkingDisableAllowed: true},
			want:  []string{"none"},
		},
		{
			name:  "neither dimension yields nothing",
			model: catalogModel{},
			want:  nil,
		},
		{
			name:  "an effort list already holding none is not duplicated",
			model: catalogModel{Efforts: []string{"none", "high"}, ThinkingDisableAllowed: true},
			want:  []string{"none", "high"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := thinkingLevelsFor(testCase.model); !slices.Equal(got, testCase.want) {
				t.Fatalf("thinkingLevelsFor = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestThinkingDeclaredForEveryModelThatCanThink pins the host-facing block across
// BOTH regions, including the case upstream `c94c3fa` was reported for: an entry
// with no effort list must still declare the block so "off" stays selectable.
func TestThinkingDeclaredForEveryModelThatCanThink(t *testing.T) {
	cases := []struct {
		region      Region
		key         string
		wantLevels  []string
		wantZero    bool
		wantNilInfo bool
	}{
		// Efforts present, disable allowed.
		{RegionCN, "dmodel", []string{"high", "max", "none"}, true, false},
		{RegionCN, "dfmodel", []string{"high", "max", "low", "none"}, true, false},
		{RegionCN, "gm51model", []string{"high", "max", "none"}, true, false},
		{RegionCN, "qmodel_38max", []string{"xhigh", "low", "medium", "none"}, true, false},
		// Efforts present, disable NOT allowed — `none` must be absent, and
		// ZeroAllowed must stay false (there is no `disabled` branch upstream).
		{RegionCN, "gmodel", []string{"high", "low", "max"}, false, false},
		{RegionCN, "gfmodel", []string{"high", "max"}, false, false},
		{RegionCN, "kmodel_latest", []string{"high", "low", "max"}, false, false},
		// The reported CN case: NO efforts at all, thinking supported, can turn
		// off. Declaring nothing here is the defect.
		{RegionCN, "qmodel", []string{"none"}, true, false},
		{RegionCN, "qmodel_latest", []string{"none"}, true, false},
		// No `thinking_config` at all upstream → no block.
		{RegionCN, "auto", nil, false, true},
		{RegionCN, "q37fmodel", nil, false, true},
		{RegionCN, "mmodel", nil, false, true},
		// International equivalents.
		{RegionGlobal, "dmodel", []string{"high", "max", "none"}, true, false},
		{RegionGlobal, "dfmodel", []string{"high", "max", "low", "none"}, true, false},
		{RegionGlobal, "ultimate", []string{"xhigh", "high", "low", "max", "medium", "none"}, true, false},
		{RegionGlobal, "gmodel", []string{"high", "low", "max"}, false, false},
		{RegionGlobal, "smodel", []string{"xhigh", "high", "low", "max", "medium"}, false, false},
		{RegionGlobal, "kmodel", []string{"high", "low", "max"}, false, false},
		{RegionGlobal, "qmodel", []string{"none"}, true, false},
		{RegionGlobal, "auto", nil, false, true},
		{RegionGlobal, "mmodel", nil, false, true},
	}
	for _, testCase := range cases {
		t.Run(string(testCase.region)+"/"+testCase.key, func(t *testing.T) {
			model, known := catalogModelFor(productByID(string(testCase.region)), testCase.key)
			if !known {
				t.Fatalf("%s is missing from the %s catalog", testCase.key, testCase.region)
			}
			info := modelInfoFor(model, ts(12, 0))
			if testCase.wantNilInfo {
				if info.Thinking != nil {
					t.Fatalf("Thinking = %#v, want none (the catalog publishes no thinking_config)", info.Thinking)
				}
				return
			}
			if info.Thinking == nil {
				t.Fatal("Thinking is nil: the model can think, so the selector would never appear")
			}
			if !slices.Equal(info.Thinking.Levels, testCase.wantLevels) {
				t.Errorf("Levels = %v, want %v", info.Thinking.Levels, testCase.wantLevels)
			}
			if info.Thinking.ZeroAllowed != testCase.wantZero {
				t.Errorf("ZeroAllowed = %v, want %v", info.Thinking.ZeroAllowed, testCase.wantZero)
			}
		})
	}
}

// TestThinkingGmodelCannotBeTurnedOff is the trap quoted at
// `qoder-product.ts:539`: `gmodel` has effort tiers with no `disabled` branch, so
// offering "off" would send a request the model cannot honour.
func TestThinkingGmodelCannotBeTurnedOff(t *testing.T) {
	for _, region := range []Region{RegionCN, RegionGlobal} {
		model, known := catalogModelFor(productByID(string(region)), "gmodel")
		if !known {
			t.Fatalf("gmodel is missing from the %s catalog", region)
		}
		info := modelInfoFor(model, ts(12, 0))
		if info.Thinking == nil {
			t.Fatalf("%s/gmodel declared no thinking support at all", region)
		}
		if info.Thinking.ZeroAllowed {
			t.Errorf("%s/gmodel ZeroAllowed = true, want false (no `disabled` branch upstream)", region)
		}
		if slices.Contains(info.Thinking.Levels, "none") {
			t.Errorf("%s/gmodel Levels = %v, must not offer `none`", region, info.Thinking.Levels)
		}
	}
}
