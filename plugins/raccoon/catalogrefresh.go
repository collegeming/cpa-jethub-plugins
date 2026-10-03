package main

import (
	"fmt"
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
// The cache is dropped through the same resetDiscoveredModels Configure uses, and
// the refetch goes through discoverModels — the very call `model.for_auth` makes
// — so a refresh cannot return something a client request would not. The only
// difference is that the cache was emptied first.
//
// The previous listing is captured before the reset so the caller can report
// whether the catalogue actually moved: a manual refresh whose button merely
// responded is not evidence the catalogue changed.
//
// ⚠️ `catalogueForAuth` falls back to the bundled table when the fetch answers
// nothing, and `discoverModels` returns an empty slice rather than an error on
// every failure (trap #10). That fallback is the right answer for a client
// request, which must not be left with nothing, but it is NOT a successful
// refresh: reporting it as one would tell the operator the vendor agreed when it
// never answered. Only a non-empty discovery result counts here, and only that
// result is cached — the same distinction `catalogueForAuth` already makes.
func catalogueRefresh(h *abiboot.Host, cfg Config) (catalog.Outcome, error) {
	if !cfg.DiscoverModels {
		return catalog.Outcome{}, abiboot.Errorf("raccoon_catalogue_disabled",
			"动态发现已在配置中关闭（discover_models=false），目录固定为内置表，无可刷新内容")
	}

	previous, _ := peekCachedModels()
	previousIDs := catalogueIDSet(previous)

	resetDiscoveredModels()

	entries := raccoonAccounts(h)
	if len(entries) == 0 {
		return catalog.Outcome{}, abiboot.Errorf("raccoon_no_account",
			"没有可用来拉取目录的 Raccoon 账号（登录后才能读取实时目录）")
	}
	credential, _, errCredential := credentialOf(h, entries[0])
	if errCredential != nil {
		return catalog.Outcome{}, errCredential
	}
	discovered := discoverModels(h, credential, cfg)
	if len(discovered) == 0 {
		return catalog.Outcome{}, abiboot.Errorf("raccoon_catalogue_degraded",
			"上游目录接口未返回可用内容，目录未更新（仍沿用上一次结果）")
	}
	putCachedModels(discovered)
	ids := catalogueIDSet(discovered)
	return catalog.Outcome{Models: len(discovered), Changed: !sameIDSet(previousIDs, ids)}, nil
}

// catalogueIDSet collects the sorted model ids of a catalogue.
func catalogueIDSet(entries []catalogueEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, strings.TrimSpace(entry.ID))
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
// The automatic path publishes only when the catalogue actually MOVED
// (`PublishOnChange` resolves the auth file from the provider at each tick,
// because a background tick has no request to select an account from and
// `Configure` runs before any login). A stable catalogue therefore costs no
// auth-file writes at all, while `GET /v1/models` — which serves the host's
// registry, not this cache — still follows the vendor on its own.
//
// That write is deliberately rare rather than absent: the host rewrites the same
// auth file whenever it renews a token, so a loop that wrote unconditionally
// every interval would fight it continuously. Writing only on a real change turns
// that conflict from constant into occasional, and it is what makes an automatic
// refresh actually visible to a client.
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
// This is the manual counterpart to the background scheduler, and the only path
// that rewrites an auth file: the write is what makes the host re-register this
// provider's models, and it happens only because an operator asked for it.
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
	return pluguiPage("Raccoon", plugui.Card("刷新模型目录",
		plugui.Group(
			plugui.Notice(kind, result.Describe()),
			plugui.Fields(
				plugui.Field{Label: "实时目录缓存", Value: catalogueCacheText()},
				plugui.Field{Label: "发布到宿主", Value: publishText(result)},
			),
		),
		plugui.Action{Label: "返回状态", Path: "status", Kind: "primary"},
	))
}

// catalogueRefreshJSON is the machine-readable form of the manual refresh.
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

// catalogueCacheText reports what the catalogue cache currently holds.
func catalogueCacheText() string {
	cached, fetchedAt := peekCachedModels()
	if len(cached) == 0 {
		return "尚未拉取（model.for_auth 时获取）"
	}
	return fmt.Sprintf("%d 条，获取于 %s", len(cached), fetchedAt.Local().Format("2006-01-02 15:04"))
}

// autoRefreshText describes the background catalogue refresh.
//
// The wording must not promise "no credential writes": the automatic path does
// write — once, when the catalogue changes — and that write is the whole reason
// `/v1/models` follows the vendor. What it promises instead is that a catalogue
// which stays put costs nothing.
func autoRefreshText(cfg Config) string {
	if cfg.ModelRefreshMS <= 0 {
		return "已关闭（model_refresh_ms = 0）；开启后仅当目录变化时才通知宿主重新注册（稳定期零写入）"
	}
	_, runs, lastRun, lastErr := catalogueScheduler.Status()
	text := "每 " + (time.Duration(cfg.ModelRefreshMS) * time.Millisecond).String() +
		"（仅目录变化时通知宿主重新注册，稳定期零写入）；已完成 " + strconv.Itoa(runs) + " 次"
	if !lastRun.IsZero() {
		text += "，最近一次 " + lastRun.Local().Format("15:04:05")
	}
	if lastErr != "" {
		text += "，最近一次失败：" + lastErr
	}
	return text
}
