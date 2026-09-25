package main

import (
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
	if cn.SessionType != "qoder_work" || global.SessionType != "qodercli" {
		t.Fatalf("session types drifted: global=%q cn=%q", global.SessionType, cn.SessionType)
	}
	if cn.ClientID != global.ClientID {
		t.Fatal("both sites share the device-flow client id")
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
	// Both sites publish the same table (`qoder-product.ts:337`, `:395`).
	if len(productByID(string(RegionCN)).ModelCatalog) != len(want) {
		t.Fatal("the CN site must publish the same catalog table")
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
	if flash.contextSize != 180_000 {
		t.Errorf("qfmodel context = %d, want 180000", flash.contextSize)
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
