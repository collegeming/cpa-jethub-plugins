package main

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/catalog"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogueScheduler refreshes this plugin's catalogue in the background. It is
// started from Configure and stopped from Quiesce/Shutdown, so a config reload
// cannot accumulate loops.
var catalogueScheduler = catalog.NewScheduler(0)

// catalogueRefresh drops the cached catalogue and refetches it for the first
// usable account.
//
// ⚠️ The data source, verified before wiring this in: the catalogue is the
// `builtinModels` pool of `GET /api/v1/client/configs` (`ClientConfigsPath`,
// `refreshRemoteCatalogue` in models.go), NOT the optional official-client
// credential import. The import (`clientcredential.go`) only adopts a LOGIN
// state; it yields no model list. `refreshRemoteCatalogue` already returns an
// error on every real failure and caches only on success, so it is the plugin's
// own discovery function and is used as-is.
//
// ⚠️ One deviation from the sibling plugins, stated rather than hidden:
// `refreshRemoteCatalogue` currently has NO production caller — `model.for_auth`
// serves `currentCatalogue()` without ever fetching it — so this refresh path is
// the first production trigger of the remote fetch. That is deliberate: wiring a
// network call into the model listing was out of scope, and a refresh that
// populates the very cache `model.for_auth` reads keeps the two consistent
// (both go through `currentCatalogue()`), with the built-in table remaining the
// answer until a refresh has succeeded.
//
// The previous listing is captured before the reset so the caller can report
// whether the catalogue actually moved: a manual refresh whose button merely
// responded is not evidence the catalogue changed.
//
// ⚠️ Changed is computed over the whole entry, not just the id set. This plugin
// pins the published id set to the two measured ids (`restrictToVerified`), so
// the ids are the SAME before and after a first successful fetch while the
// metadata is exactly what moved — `client/configs` is authoritative for the
// context window, the output cap and the reasoning ladder, and models.go records
// that the built-in table's numbers differ from upstream's. An id-only
// comparison would therefore report "no change" on the first successful fetch,
// leave `GET /v1/models` on the built-in metadata, and defeat the point of the
// automatic refresh.
func catalogueRefresh(h *abiboot.Host, cfg Config) (catalog.Outcome, error) {
	if !cfg.DiscoverModels {
		return catalog.Outcome{}, abiboot.Errorf("zcode_catalogue_disabled",
			"线上目录发现已关闭（discover_models=false），目录固定为内置表，无可刷新内容")
	}
	previousDiscovered := peekDiscoveredModels()
	previous := catalogueFingerprint(currentCatalogue())

	resetDiscoveredModels()

	entries := zcodeAccounts(h)
	if len(entries) == 0 {
		restoreDiscoveredModels(previousDiscovered)
		return catalog.Outcome{}, abiboot.Errorf("zcode_no_account",
			"没有可用来拉取目录的 ZCode 账号")
	}
	credential, errCredential := credentialOf(h, entries[0])
	if errCredential != nil {
		restoreDiscoveredModels(previousDiscovered)
		return catalog.Outcome{}, errCredential
	}
	if errRefresh := refreshRemoteCatalogue(h, credential, cfg); errRefresh != nil {
		// The remote fetch failed. The built-in table is what a client request
		// would serve, but reporting that as a successful refresh would tell the
		// operator the vendor confirmed a catalogue it never answered with.
		restoreDiscoveredModels(previousDiscovered)
		return catalog.Outcome{}, errRefresh
	}
	if len(peekDiscoveredModels()) == 0 {
		restoreDiscoveredModels(previousDiscovered)
		return catalog.Outcome{}, abiboot.Errorf("zcode_catalogue_degraded",
			"上游 %s 没有返回可用目录，目录未更新（仍沿用上一次结果）", ClientConfigsPath)
	}

	catalogue := currentCatalogue()
	return catalog.Outcome{
		Models:  len(catalogue),
		Changed: !sameFingerprint(previous, catalogueFingerprint(catalogue)),
	}, nil
}

// peekDiscoveredModels returns a copy of the cached remote catalogue without
// touching the network, so a refresh can report what it replaced and restore it
// on failure.
func peekDiscoveredModels() []fallbackModel {
	catalogueMu.RLock()
	defer catalogueMu.RUnlock()
	if len(discoveredCatalog) == 0 {
		return nil
	}
	out := make([]fallbackModel, len(discoveredCatalog))
	copy(out, discoveredCatalog)
	return out
}

// restoreDiscoveredModels puts a previously cached catalogue back after a failed
// refresh. It mirrors resetDiscoveredModels exactly, so an empty snapshot simply
// leaves the cache empty.
func restoreDiscoveredModels(models []fallbackModel) {
	catalogueMu.Lock()
	discoveredCatalog = models
	catalogueMu.Unlock()
}

// catalogueFingerprint renders every published field of a listing, sorted by id,
// so two catalogues can be compared for real change.
//
// The ids alone are not enough here (see catalogueRefresh): the published id set
// is pinned to the measured pair, and the metadata is what a successful fetch
// updates.
func catalogueFingerprint(catalogue []fallbackModel) []string {
	out := make([]string, 0, len(catalogue))
	for _, model := range catalogue {
		out = append(out, strings.Join([]string{
			strings.TrimSpace(model.ID),
			strings.TrimSpace(model.Name),
			strconv.Itoa(model.ContextWindow),
			strconv.Itoa(model.MaxOutputTokens),
			strconv.FormatBool(model.SupportsImage),
			strings.Join(model.ReasoningLevels, "/"),
			strings.TrimSpace(model.DefaultReasoningLevel),
		}, "\x1f"))
	}
	sort.Strings(out)
	return out
}

// sameFingerprint reports whether two catalogue fingerprints are equal.
func sameFingerprint(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// catalogueIDSet collects the sorted ids of a listing.
func catalogueIDSet(catalogue []fallbackModel) []string {
	ids := make([]string, 0, len(catalogue))
	for _, model := range catalogue {
		ids = append(ids, strings.TrimSpace(model.ID))
	}
	sort.Strings(ids)
	return ids
}

// sameIDSet reports whether two id sets are equal.
func sameIDSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// startCatalogueScheduler (re)configures the background refresh from settings.
//
// The automatic path publishes on change and only on change: the Request names
// no AuthName, so catalog.Run publishes when — and only when — the catalogue
// actually moved. That is what keeps `GET /v1/models` following the vendor
// without a timer rewriting an auth file the host is also writing when it renews
// a token. The manual button is the unconditional path, because an operator
// pressing it expects the host to re-register either way.
func startCatalogueScheduler(cfg Config) {
	catalogueScheduler.SetInterval(time.Duration(cfg.ModelRefreshMS) * time.Millisecond)
	if !catalogueScheduler.Enabled() {
		return
	}
	// A tick carries no host payload, so the refresh builds its own handle: the
	// transport is the plugin's, not the invocation's, and it reaches the host
	// for as long as the plugin is loaded. The same handle reports the outcome,
	// so an automatic refresh is visible in the host log.
	host := &abiboot.Host{}
	catalogueScheduler.Start(catalog.Request{
		Host:     host,
		Provider: ProviderKey,
		// A tick publishes only when the catalogue actually moved, so a stable
		// catalogue costs no auth-file writes while `GET /v1/models` still
		// follows the vendor on its own.
		PublishOnChange: true,
		Refresh:         func() (catalog.Outcome, error) { return catalogueRefresh(host, settings()) },
	})
}

// stopCatalogueScheduler ends the background loop at shutdown.
func stopCatalogueScheduler() { catalogueScheduler.Stop() }

// catalogueRefreshPage refreshes the model catalogue and publishes it.
//
// This is the manual counterpart to the background scheduler. It is the only
// path that rewrites an auth file: the write is what makes the host re-register
// this provider's models, and it happens only because an operator asked for it,
// never on a timer.
func catalogueRefreshPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	entry, found := selectAccount(h, request)
	authName := ""
	if found {
		authName = entry.Name
	}
	result := catalog.Run(catalog.Request{
		Host:     h,
		Provider: ProviderKey,
		AuthName: authName,
		Refresh:  func() (catalog.Outcome, error) { return catalogueRefresh(h, cfg) },
	})
	kind := "success"
	if result.Err != nil {
		kind = "danger"
	} else if result.PublishErr != nil {
		kind = "warning"
	}
	return pluguiPage("ZCode（智谱）", plugui.Card("刷新模型目录",
		plugui.Group(
			plugui.Notice(kind, result.Describe()),
			plugui.Fields(
				plugui.Field{Label: "线上目录缓存", Value: catalogueCacheText()},
				plugui.Field{Label: "发布到宿主", Value: publishText(result)},
			),
		),
		plugui.Action{Label: "返回状态", Path: "status", Kind: "primary"},
	))
}

// catalogueRefreshJSON is the machine-readable form of the manual refresh.
//
// ZCode declares no route in the global `/v0/management/` namespace, so this is
// reached through the status resource route with `?format=json` — the same
// convention every other script-facing answer on this plugin uses.
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
		Refresh:  func() (catalog.Outcome, error) { return catalogueRefresh(h, settings()) },
	})
	body := map[string]any{
		"status":      "ok",
		"models":      result.Models,
		"changed":     result.Changed,
		"published":   result.Published,
		"duration_ms": result.Duration.Milliseconds(),
	}
	switch {
	case result.Err != nil:
		body["status"] = "error"
		body["error"] = result.Err.Error()
	case result.PublishErr != nil:
		body["status"] = "warning"
		body["publish_error"] = result.PublishErr.Error()
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// publishText describes whether the catalogue reached the host registry.
func publishText(result catalog.Result) string {
	switch {
	case result.Published:
		return "已通知宿主重新注册，/v1/models 约 1 秒后生效"
	case result.PublishErr != nil:
		return "未发布（" + result.PublishErr.Error() + "）：插件缓存已刷新，/v1/models 要等宿主下次重新注册"
	default:
		return "未发布：没有可用的账号文件；插件缓存已刷新"
	}
}

// catalogueSourceText says where the published catalogue comes from.
func catalogueSourceText(cfg Config) string {
	if !cfg.DiscoverModels {
		return "内置表（discover_models 已关闭；GLM-5.3 / GLM-5.3-Flash）"
	}
	if len(peekDiscoveredModels()) > 0 {
		return "远端目录 " + ClientConfigsPath + "（已拉取）"
	}
	return "远端目录 " + ClientConfigsPath + "（尚未拉取，当前展示内置表）"
}

// catalogueCacheText reports what the catalogue cache currently holds.
//
// The built-in table is reported as such: it is NOT a cached fetch, and showing
// it as one would hide that the remote catalogue was never pulled.
func catalogueCacheText() string {
	discovered := peekDiscoveredModels()
	if len(discovered) == 0 {
		return "尚未拉取（当前展示内置表）"
	}
	return strconv.Itoa(len(discovered)) + " 条（来自 " + ClientConfigsPath + "）"
}

// autoRefreshText describes the background catalogue refresh.
func autoRefreshText(cfg Config) string {
	if cfg.ModelRefreshMS <= 0 {
		return "已关闭（model_refresh_ms = 0）；手动刷新请用上面的「刷新目录」按钮"
	}
	_, runs, lastRun, lastErr := catalogueScheduler.Status()
	text := "每 " + (time.Duration(cfg.ModelRefreshMS) * time.Millisecond).String() +
		"（仅在目录变化时才写凭据文件通知宿主）；已完成 " + strconv.Itoa(runs) + " 次"
	if !lastRun.IsZero() {
		text += "，最近一次 " + lastRun.Local().Format("15:04:05")
	}
	if lastErr != "" {
		text += "，最近一次失败：" + lastErr
	}
	return text
}
