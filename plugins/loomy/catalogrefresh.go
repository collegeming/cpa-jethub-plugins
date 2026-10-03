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
// The cache is dropped through the same resetDiscoveredModels Configure uses,
// and the refetch goes through discoverModels — the function catalogueForAuth
// itself calls — so a refresh cannot return something `model.for_auth` would
// not; the only difference is that the cache was emptied first.
//
// The previous listing is captured before the reset so the caller can report
// whether the catalogue actually moved: a manual refresh whose button merely
// responded is not evidence the catalogue changed.
//
// ⚠️ `catalogueForAuth` is deliberately NOT used here. It falls back to the
// bundled table when the remote call fails, which is the right answer for a
// client request — the provider must not lose its model list because a listing
// endpoint hiccuped — but it makes the fallback indistinguishable from a
// successful fetch. Reporting that as success would tell the operator the vendor
// had confirmed a catalogue when it never answered.
func catalogueRefresh(h *abiboot.Host, cfg Config) (catalog.Outcome, error) {
	if !cfg.DiscoverModels {
		return catalog.Outcome{}, abiboot.Errorf("loomy_catalogue_disabled",
			"实时目录发现已关闭（discover_models=false），目录固定为内置表，无可刷新内容")
	}
	previous := peekDiscoveredModels()
	previousIDs := modelIDSet(previous)

	resetDiscoveredModels()

	entries := loomyAccounts(h)
	if len(entries) == 0 {
		return catalog.Outcome{}, abiboot.Errorf("loomy_no_account",
			"没有可用来拉取目录的 Loomy 账号")
	}
	credential, errCredential := credentialOf(h, entries[0])
	if errCredential != nil {
		return catalog.Outcome{}, errCredential
	}
	discovered := discoverModels(h, credential, cfg)
	if len(discovered) == 0 {
		return catalog.Outcome{}, abiboot.Errorf("loomy_catalogue_degraded",
			"上游 GET %s 未返回可用内容，目录未更新（仍沿用上一次结果）", ModelsPath)
	}
	putCachedModels(discovered)
	ids := modelIDSet(discovered)
	return catalog.Outcome{Models: len(discovered), Changed: !sameIDSet(previousIDs, ids)}, nil
}

// peekDiscoveredModels returns a copy of the cached catalogue without touching
// the network, so a refresh can report what it replaced.
//
// It reads the raw store instead of cachedModels so an entry past its TTL still
// counts as "what was there before": the comparison is about the listing, not
// about whether it was still inside its lifetime.
func peekDiscoveredModels() []modelDescriptor {
	discoveredCatalogue.mu.Lock()
	defer discoveredCatalogue.mu.Unlock()
	if len(discoveredCatalogue.models) == 0 {
		return nil
	}
	out := make([]modelDescriptor, len(discoveredCatalogue.models))
	copy(out, discoveredCatalogue.models)
	return out
}

// modelIDSet collects the sorted ids of a listing.
func modelIDSet(models []modelDescriptor) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
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
	return pluguiPage("Loomy", plugui.Card("刷新模型目录",
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
// Loomy declares no route in the global `/v0/management/` namespace, so this is
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

// catalogueRefreshAction is the GET link that triggers a manual refresh. It is
// repeated on the catalogue card because that is the card an operator is looking
// at when they want a refresh; the settings card carries the same action.
func catalogueRefreshAction() plugui.Action {
	return plugui.Action{Label: "刷新目录", Query: "action=refresh-catalog", Kind: "primary"}
}

// catalogueForPage returns the listing the status page renders plus the source
// line that describes those very rows.
//
// It never touches the network: the cached remote listing when one was fetched,
// the bundled table otherwise. A page load must not trigger a fetch — the card
// reports what this deployment currently has — which is why `activeCatalogue` is
// deliberately not used here: on a cold cache it CALLS the vendor, and a page
// view is not a reason to ask upstream anything.
func catalogueForPage(cfg Config) ([]modelDescriptor, string) {
	if !cfg.DiscoverModels {
		return fallbackModels(), "内置兜底表（discover_models 已关闭，不访问网络）"
	}
	if cached := peekDiscoveredModels(); len(cached) > 0 {
		fetchedAt := discoveredCatalogueFetchedAt()
		return cached, "实时目录 GET " + ModelsPath + "（缓存于 " +
			fetchedAt.Local().Format("15:04") + "）"
	}
	return fallbackModels(), "内置兜底表（实时目录 GET " + ModelsPath + " 尚未拉取）"
}

// discoveredCatalogueFetchedAt reports when the cached listing was fetched.
func discoveredCatalogueFetchedAt() time.Time {
	discoveredCatalogue.mu.Lock()
	defer discoveredCatalogue.mu.Unlock()
	return discoveredCatalogue.fetchedAt
}

// catalogueModelEntries maps the page's listing onto the shared card's rows.
//
// Native is the vendor's OWN model name, verbatim: `modelDescriptor.Name` is
// documented as "the raw wire name" (models.go:99-101), filled from the `name`
// field of the vendor's `/models` answer (models.go:330-335).
//
// The trailing multiplier inside it is the VENDOR's own spelling, not this
// plugin's annotation: models.go:19-23 records that the price is embedded in the
// name and nowhere else ("a search for credit/multiplier/price/factor/rate in the
// `/models` response returns zero hits"), and models.go:22-23 lists the three
// vendor spellings — `MiniMax M3 （x4.0）`, `Qwen 3.8 Max (x12.0)`,
// `GLM 5.3 Flash(x0.8)`. So the multiplier stays in the label because that is
// where upstream put it; stripping it (which `bareName` does for a different
// purpose) would edit the vendor's string rather than report it.
//
// `ModelInfo.Name` is deliberately NOT the source, for the exact reason the task
// asked about: `info()` fills it with `bareName()` (models.go:153), which is
// `splitLoomyRate(m.Name)` — a LOCAL normalisation that strips that multiplier.
// Using it would display a name upstream never published. `ModelInfo.ID` is the
// wire `id`, a slug (`MiniMax-M3`, `deepseek-v4-flash-0731`) that requests are
// routed by, so it belongs in the row as the routed name, not as the label.
func catalogueModelEntries(entries []modelDescriptor) []plugui.ModelEntry {
	now := time.Now()
	rows := make([]plugui.ModelEntry, 0, len(entries))
	for _, entry := range entries {
		info := entry.info(now)
		native := strings.TrimSpace(entry.Name)
		if native == "" {
			// A deserialised entry with no name: the id is all upstream gave,
			// and the shared card would drop a blank row entirely.
			native = strings.TrimSpace(entry.ID)
		}
		detail := ""
		if info.ContextLength > 0 {
			detail = "上下文 " + strconv.FormatInt(info.ContextLength, 10)
		} else {
			// 0 means unknown, and models.go:102-104 forbids replacing it with a
			// guess, so the row says so rather than printing 0.
			detail = "上下文未知"
		}
		detail += "，最大输出 " + strconv.FormatInt(info.MaxCompletionTokens, 10)
		if len(info.SupportedInputModalities) > 1 {
			detail += "，输入 text+image"
		} else {
			detail += "，输入 text"
		}
		rows = append(rows, plugui.ModelEntry{
			Native: native,
			// Empty when it matches Native, so the card omits 请求用名 rather
			// than printing the same string twice.
			ID:     routedName(native, entry.ID),
			Detail: detail,
		})
	}
	return rows
}

// routedName reports the id a request must use, or "" when it is the same string
// as the name already shown.
func routedName(native, id string) string {
	if strings.TrimSpace(id) == native {
		return ""
	}
	return strings.TrimSpace(id)
}

// catalogueCacheText reports what the catalogue cache currently holds.
//
// It reads the raw store, so an entry past its TTL is still shown: "there is a
// cached listing, fetched at HH:MM" and "the next request will refetch it" are
// different facts, and the page should not hide the first.
func catalogueCacheText() string {
	discoveredCatalogue.mu.Lock()
	count := len(discoveredCatalogue.models)
	fetchedAt := discoveredCatalogue.fetchedAt
	discoveredCatalogue.mu.Unlock()
	if count == 0 {
		return "尚未拉取（model.for_auth 时获取）"
	}
	return strconv.Itoa(count) + " 条，获取于 " + fetchedAt.Local().Format("2006-01-02 15:04")
}

// autoRefreshText describes the background catalogue refresh.
func autoRefreshText(cfg Config) string {
	if cfg.ModelRefreshMS <= 0 {
		return "已关闭（model_refresh_ms = 0）；手动刷新请用上面的「刷新目录」按钮"
	}
	_, runs, lastRun, lastErr := catalogueScheduler.Status()
	text := "每 " + (time.Duration(cfg.ModelRefreshMS) * time.Millisecond).String() +
		"（仅在目录变化时才写凭据文件通知宿主）；已完成 " + itoaInt(runs) + " 次"
	if !lastRun.IsZero() {
		text += "，最近一次 " + lastRun.Local().Format("15:04:05")
	}
	if lastErr != "" {
		text += "，最近一次失败：" + lastErr
	}
	return text
}
