package main

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// defaultMaxOutputTokens is the per-response cap advertised to the host. The
// Qoder catalog does not publish an output limit, so a conservative value is
// advertised rather than a fabricated per-model one.
const defaultMaxOutputTokens = 65536

// promotionActiveNow reports whether the off-peak window covers `now`.
//
// Port of `promotionActiveNow` (`qoder-adapter.ts:400-419`). The catalog's
// `active` flag is a snapshot taken when the catalog was fetched and goes stale
// in a long-running session, while the window itself is stable — so the window is
// recomputed locally. The catalog timezone is Asia/Singapore (UTC+8) and the
// window may cross midnight (22:00–08:00).
func promotionActiveNow(promo *promotion, now time.Time) bool {
	if promo == nil {
		return false
	}
	start, okStart := clockMinutes(promo.WindowStart)
	end, okEnd := clockMinutes(promo.WindowEnd)
	if !okStart || !okEnd {
		return promo.Active
	}
	// UTC+8 wall clock; the catalog timezone matches the user's, so no real
	// timezone conversion is needed (`qoder-adapter.ts:413-415`).
	utc8 := now.UTC().Add(8 * time.Hour)
	minutes := utc8.Hour()*60 + utc8.Minute()
	if start <= end {
		return minutes >= start && minutes < end
	}
	return minutes >= start || minutes < end
}

// clockMinutes parses an `HH:MM` window boundary.
func clockMinutes(value string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, false
	}
	hours, errHours := strconv.Atoi(strings.TrimSpace(parts[0]))
	minutes, errMinutes := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errHours != nil || errMinutes != nil {
		return 0, false
	}
	if hours < 0 || hours > 23 || minutes < 0 || minutes > 59 {
		return 0, false
	}
	return hours*60 + minutes, true
}

// modelDisplayName is `qoderDisplayName` (`qoder-adapter.ts:446-466`).
//
// The multiplier is written into the model NAME, not the description: the
// composer's model menu renders only `name` (`qoder-adapter.ts:433-436`).
//
//	Qwen3.8-Flash · 免费
//	Qwen3.8-Max · x0.5→x0.2     (inside the window)
//	Qwen3.8-Max · x0.5          (outside the window, list price)
//	Sonus · x8
func modelDisplayName(model catalogModel, now time.Time) string {
	promo := model.Promotion
	hasPromo := promo != nil && promo.BeforePriceFactor != nil && promo.DiscountFactor != nil
	active := hasPromo && promotionActiveNow(promo, now)

	// Free wins over everything: `0` is a legal price factor and must not be
	// rendered as `x0` (`qoder-adapter.ts:453-454`).
	if model.PriceFactor != nil && *model.PriceFactor == 0 {
		return model.Display + " · 免费"
	}

	if hasPromo && active {
		effective := roundTo(*promo.BeforePriceFactor**promo.DiscountFactor, 4)
		return fmt.Sprintf("%s · x%s→x%s", model.Display,
			formatFactor(*promo.BeforePriceFactor), formatFactor(effective))
	}

	// Outside the window the LIST price applies. Showing the discounted snapshot
	// would make the user expect the discount and be billed the list price
	// (`qoder-adapter.ts:462-465`).
	if hasPromo {
		return fmt.Sprintf("%s · x%s", model.Display, formatFactor(*promo.BeforePriceFactor))
	}
	if model.PriceFactor != nil {
		return fmt.Sprintf("%s · x%s", model.Display, formatFactor(*model.PriceFactor))
	}
	return model.Display
}

// formatFactor renders a multiplier the way a JavaScript template literal does.
func formatFactor(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// roundTo rounds to the given number of decimals, matching `toFixed(4)`.
func roundTo(value float64, decimals int) float64 {
	scale := math.Pow(10, float64(decimals))
	return math.Round(value*scale) / scale
}

// thinkingLevelsFor is `qoderEffortsFor` (`qoder-adapter.ts:156-159`).
//
// The three rules, all read off the catalog rather than guessed:
//
//  1. `Levels` is the `thinking_config.enabled.efforts` key list, in catalog
//     order (the official client renders object key order too);
//  2. a `thinking_config.disabled` branch (`ThinkingDisableAllowed`) ADDS
//     `none` — the client's `gU()` does exactly
//     `… || e.includes('none') ? e : [...e, 'none']`;
//  3. an entry with neither yields no levels at all, and the caller then does
//     not declare `reasoning` (the UI shows "no reasoning levels for this
//     model", matching the IDE's "unsupported").
//
// ⚠️ The two dimensions are INDEPENDENT, and this function deliberately does
// not consult `SupportsThinking`: CN `qmodel` / `qmodel_latest` publish NO
// effort list but DO publish `disabled`, so "turn thinking off" is the only
// option they offer (upstream `c94c3fa`; the user confirmed "上面两个没有思考
// 档位就是关闭的意思"). Conversely `gmodel` / `gfmodel` / `kmodel*` publish
// effort tiers with no `disabled` branch and must NOT gain `none`.
func thinkingLevelsFor(model catalogModel) []string {
	levels := append([]string(nil), model.Efforts...)
	if model.ThinkingDisableAllowed && !slices.Contains(levels, "none") {
		levels = append(levels, "none")
	}
	return levels
}

// modelInfoFor builds the host-facing descriptor for one catalog entry.
func modelInfoFor(model catalogModel, now time.Time) pluginapi.ModelInfo {
	modalities := []string{"text"}
	if model.SupportsImage {
		modalities = append(modalities, "image")
	}
	info := pluginapi.ModelInfo{
		ID:                         model.Key,
		Object:                     "model",
		Created:                    now.Unix(),
		OwnedBy:                    ProviderKey,
		Type:                       "chat",
		DisplayName:                modelDisplayName(model, now),
		Name:                       model.Display,
		Description:                "Qoder " + model.Display + "（目录 key，需加密推理端点）",
		ContextLength:              model.ContextWindow,
		InputTokenLimit:            model.ContextWindow,
		MaxCompletionTokens:        defaultMaxOutputTokens,
		OutputTokenLimit:           defaultMaxOutputTokens,
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   modalities,
		SupportedOutputModalities:  []string{"text"},
	}
	// Thinking comes straight from the catalog (see thinkingLevelsFor). A model
	// with neither effort tiers nor a `disabled` branch gets NO block at all,
	// which is what the host renders as "no reasoning levels".
	//
	// ⚠️ The host's ThinkingSupport has no default-effort field, so an upstream
	// `defaultEffort` cannot be forwarded; only the level list and the
	// "off is allowed" flag are declared. `plugins/trae/models.go:557-564`
	// records the same host limitation. Min/Max are not published by the
	// catalog either, so they stay unset instead of being guessed.
	if levels := thinkingLevelsFor(model); len(levels) > 0 || model.ThinkingDisableAllowed {
		info.Thinking = &pluginapi.ThinkingSupport{
			Levels:      levels,
			ZeroAllowed: model.ThinkingDisableAllowed,
		}
	}
	return info
}

// staticModelInfos renders the catalog for one region, plus any model names the
// user declared for the public endpoint.
func staticModelInfos(cfg Config, region Region) []pluginapi.ModelInfo {
	p := productByID(string(normalizeRegion(string(region))))
	now := time.Now()
	infos := make([]pluginapi.ModelInfo, 0, len(p.ModelCatalog)+len(cfg.PublicModels))
	for _, model := range p.ModelCatalog {
		infos = append(infos, modelInfoFor(model, now))
	}
	for _, name := range cfg.PublicModels {
		if _, known := catalogModelFor(p, name); known {
			continue
		}
		infos = append(infos, pluginapi.ModelInfo{
			ID:                         name,
			Object:                     "model",
			Created:                    now.Unix(),
			OwnedBy:                    ProviderKey,
			Type:                       "chat",
			DisplayName:                name + " · 公开端点",
			Name:                       name,
			Description:                "用户在 public_models 中声明的公开端点模型名（目录 key 只对加密端点有效）",
			MaxCompletionTokens:        defaultMaxOutputTokens,
			SupportedGenerationMethods: []string{"chat.completions"},
			SupportedInputModalities:   []string{"text"},
			SupportedOutputModalities:  []string{"text"},
			UserDefined:                true,
		})
	}
	return infos
}

// handleModelRegister reports the static catalog. It must not touch the network.
func handleModelRegister(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelRegistrationResponse{
		Provider: ProviderKey,
		Models:   staticModelInfos(settings(), activeRegion()),
	}, nil
}

// handleModelStatic is the model.static variant of the same catalog.
func handleModelStatic(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.ModelResponse{
		Provider: ProviderKey,
		Models:   staticModelInfos(settings(), activeRegion()),
	}, nil
}

// handleModelForAuth reports the catalog for one bound account.
//
// The catalog itself is static: the remote listing endpoint needs the WASM
// signature, so it is never called and the fallback table is authoritative
// (`qoder-adapter.ts:144-150`). The region comes from the credential, so two
// accounts of different sites each get their own view. A credential that cannot
// be parsed still yields the configured region's catalog rather than an error —
// the listing is advisory and hiding it would break routing.
func handleModelForAuth(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthModelRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := settings()
	region := activeRegion()
	if credential, errParse := ParseCredential(request.StorageJSON); errParse == nil {
		region = credential.regionOr(cfg.Region)
	}
	return pluginapi.ModelResponse{Provider: ProviderKey, Models: staticModelInfos(cfg, region)}, nil
}
