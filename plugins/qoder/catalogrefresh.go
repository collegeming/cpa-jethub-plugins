package main

import (
	"net/http"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/catalog"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file wires Qoder into the shared catalogue-refresh mechanism
// (`internal/jethub/catalog`). It is deliberately a PARTIAL wiring, and the
// reason is a property of this provider rather than a shortcut.
//
// ⚠️ Qoder has no online model catalogue. The catalogue a Qoder instance
// publishes is a compile-time table (`qoderModelCatalog` / `qoderCNModelCatalog`
// in product.go, ported from `QODER_FALLBACK_MODELS`), selected by the region
// and the credential's own region, and extended only by the operator's
// `public_models` setting. `handleModelForAuth` therefore performs **no network
// call**.
//
// ⚠️ It is NOT that this plugin lacks a WASM signer: it ships one, and it works
// (`wasm.go`, embedded artifact, used for the encrypted inference path). The
// blocker is narrower and was pinned down by loading the real artifact with
// wazero rather than by reading a comment:
//
//   - the signer's only request-building entry point is
//     `qodercontext_prepareInferRequest`, and the module hardwires the path it
//     signs to `…/algo/api/v2/service/pro/sse/agent_chat_generation
//     ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1`. There is no
//     parameter that redirects it to a listing path;
//   - the artifact also exports a generic `qodercontext_prepareRequest`, but it
//     emits **no signature**: probed across every argument layout, it returns the
//     common Cosy-* identity headers and never `Authorization: Bearer COSY…`,
//     `Cosy-Key` or `Cosy-Date`. (A caller-supplied Authorization in its
//     "extra headers" JSON is echoed back verbatim, which is not a signature.)
//     So the vendor's listing endpoint (`/api/v2/model/list`, which the same
//     sources record as signature-gated) cannot be signed with this artifact.
//
// The reference agrees, and its newest revision confirms it: `listModels` still
// serves `fallbackModels`, and the one addition there — `decryptModelCatalog`,
// which reads `~/.qoder/.models/{uid}/catalog-v6` through `model_cache_decrypt` —
// is documented as offline cross-checking only ("线上模型列表仍走兜底表"). That
// path is unusable here anyway: it needs the operator's own Qoder IDE cache
// directory, which a hosted CPA deployment does not have.
//
// Two consequences follow. There is no layer ① to refresh — no cache to drop and
// no discovery function to re-run — so TWO pieces the other channels carry are
// deliberately absent here: the `model_refresh_ms` setting and the background
// scheduler behind it. A timer whose every tick reported "no change" would teach
// an operator that the feature works while nothing was ever fetched — the exact
// failure this package's `Outcome`/`Describe` exists to prevent.
//
// Layer ②, on the other hand, is still real and still worth reaching.
// `GET /v1/models` serves the host's registry, and re-registering this provider is
// what makes that registry match what `model.for_auth` would answer *right now* —
// which can differ from what it answered at startup when `public_models`,
// `region` or the credential's own region moved.
//
// So this provider gets the manual publish (the `刷新目录` button and the
// `?format=json` route), and no automatic refresh. The manual path is the only
// one that rewrites an auth file, and it does so only because an operator asked.

// qoderCatalogueRefresh reports the catalogue this instance would publish for the
// selected account, so `catalog.Run` can publish it.
//
// It is named `...Refresh` because that is what `catalog.Refresh` means — "the
// work that produces the current catalogue" — not because it fetches anything:
// re-running this provider's own catalogue path IS calling `staticModelInfos`,
// the same function `handleModelRegister`, `handleModelStatic` and
// `handleModelForAuth` all serve from (`pluginui.go:44`, `management.go:197`).
// Inventing a fetch here would break the one property the shared package
// guarantees: that a refresh can never return something a client request would
// not.
//
// `Changed` is reported as false, and that is a statement of fact rather than a
// placeholder: this provider replaces nothing, so there is no previous listing
// for the new one to differ from. The operator-visible value of this path is the
// PUBLISH, which `catalog.Result.Describe` states separately.
func qoderCatalogueRefresh(h *abiboot.Host, request pluginapi.ManagementRequest) (catalog.Outcome, error) {
	cfg := settings()
	return qoderCatalogueOutcome(staticModelInfos(cfg, qoderCatalogueRegion(h, request)))
}

// qoderCatalogueRegion resolves which region's table this request publishes for,
// through the same account-credential path `handleModelForAuth` uses. It is
// split out so the refresh page's "来源" row and the catalogue it just published
// are derived from one resolution and cannot show different regions.
func qoderCatalogueRegion(h *abiboot.Host, request pluginapi.ManagementRequest) Region {
	cfg := settings()
	region := activeRegion()
	if entry, found := selectAccount(h, request); found {
		if credential, _, errCredential := credentialOf(h, entry); errCredential == nil {
			region = credential.regionOr(cfg.Region)
		}
	}
	return region
}

// qoderCatalogueOutcome turns a resolved listing into the outcome the shared
// package reports.
//
// An empty listing is a hard failure, not a refresh: publishing it would offer a
// client nothing to route to. The guard is split out so it can be exercised —
// the product tables are non-empty constants, so no configuration can reach it
// through the handler.
func qoderCatalogueOutcome(models []pluginapi.ModelInfo) (catalog.Outcome, error) {
	if len(models) == 0 {
		return catalog.Outcome{}, abiboot.Errorf("qoder_catalogue_empty",
			"当前区域与配置下没有任何可发布的模型，目录未发布")
	}
	// Changed is false as a statement of fact: this provider replaces no listing
	// (no cache, no fetch), so there is nothing a new catalogue could differ from.
	return catalog.Outcome{Models: len(models), Changed: false}, nil
}

// catalogueRefreshPage republishes the catalogue and reports what happened.
func catalogueRefreshPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	authName := ""
	if found {
		authName = entry.Name
	}
	result := catalog.Run(catalog.Request{
		Host:     h,
		Provider: ProviderKey,
		AuthName: authName,
		Refresh:  func() (catalog.Outcome, error) { return qoderCatalogueRefresh(h, request) },
	})
	// For this provider the publish is the whole point of the button, so a
	// refresh that could not publish is a warning and the message says why —
	// a green "目录无变化" over an un-published catalogue would read as success.
	kind := "success"
	notice := result.Describe()
	switch {
	case result.Err != nil:
		kind = "danger"
	case result.PublishErr != nil:
		kind = "warning"
	case !result.Published:
		kind = "warning"
		notice += "；未发布：没有可用的账号文件，/v1/models 不会重新注册"
	}
	return pluguiPage("Qoder", plugui.Card("刷新模型目录",
		plugui.Group(
			plugui.Notice(kind, notice),
			plugui.Fields(
				plugui.Field{Label: "目录来源", Value: catalogueSourceText(qoderCatalogueRegion(h, request))},
				plugui.Field{Label: "发布到宿主", Value: publishText(result)},
			),
		),
		plugui.Action{Label: "返回状态", Path: "status", Kind: "primary"},
	))
}

// catalogueRefreshJSON is the machine-readable form of the manual refresh.
//
// `refetch` is reported explicitly and is always false: a script reading this
// document must be able to tell "the vendor was asked and had nothing new" from
// "this provider has no vendor endpoint to ask". The second is the truth here.
func catalogueRefreshJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	entry, found := selectAccount(h, request)
	authName := ""
	if found {
		authName = entry.Name
	}
	result := catalog.Run(catalog.Request{
		Host:     h,
		Provider: ProviderKey,
		AuthName: authName,
		Refresh:  func() (catalog.Outcome, error) { return qoderCatalogueRefresh(h, request) },
	})
	body := map[string]any{
		"status":      "ok",
		"models":      result.Models,
		"changed":     result.Changed,
		"published":   result.Published,
		"duration_ms": result.Duration.Milliseconds(),
		// This provider's catalogue is built in; nothing was fetched. Stated as
		// data, because a caller cannot infer it from the other fields.
		"refetch": false,
		"source":  "builtin",
		"note":    "Qoder 的模型目录是内置静态表，没有上游目录端点可拉取；本操作让宿主重新注册当前目录（GET /v1/models 约 1 秒后生效）",
	}
	switch {
	case result.Err != nil:
		body["status"] = "error"
		body["error"] = result.Err.Error()
	case result.PublishErr != nil:
		body["status"] = "warning"
		body["publish_error"] = result.PublishErr.Error()
	case !result.Published:
		// Unlike the other channels, an un-published refresh here achieved
		// nothing at all: there is no cache that was still refreshed. Reporting
		// "ok" would be a plain falsehood, so it is a warning with the reason.
		body["status"] = "warning"
		body["publish_skipped"] = "没有可用的账号文件，未通知宿主重新注册；请先登录一个账号"
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// publishText describes whether the catalogue reached the host registry.
func publishText(result catalog.Result) string {
	switch {
	case result.Published:
		return "已通知宿主重新注册，/v1/models 约 1 秒后生效"
	case result.PublishErr != nil:
		return "未发布（" + result.PublishErr.Error() + "）：/v1/models 要等宿主下次重新注册"
	default:
		return "未发布：没有可用的账号文件；请先登录一个账号"
	}
}

// catalogueSourceText states where the published catalogue comes from, so the
// refresh page cannot be read as evidence that something was fetched.
//
// The count is taken from the same `staticModelInfos` the refresh just published,
// for the region it published — not from the bare product table — so the row
// agrees with the model figure in the notice above it even when `public_models`
// or a region override is in play.
func catalogueSourceText(region Region) string {
	cfg := settings()
	builtin := len(productByID(string(region)).ModelCatalog)
	text := "内置静态表（无上游目录端点）：" + string(region) + " " + itoaInt(builtin) + " 个模型"
	if total := len(staticModelInfos(cfg, region)); total != builtin {
		text += "，实际发布 " + itoaInt(total) + " 个（含 public_models 声明的公开端点模型名）"
	}
	return text
}

// autoRefreshText states why there is no background refresh for this provider.
//
// It exists so the status page can answer the question the operator will ask
// after seeing the other channels' 「后台自动刷新」 row, instead of leaving the
// absence to be guessed at.
func autoRefreshText() string {
	return "不适用：Qoder 的目录是内置静态表，没有上游端点可拉取，也没有缓存需要定时刷新；" +
		"需要让 /v1/models 立即反映当前配置时，用「刷新目录」按钮（它会通知宿主重新注册）"
}
