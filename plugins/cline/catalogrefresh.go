package main

import (
	"sort"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/catalog"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogueScheduler refreshes this plugin's catalogue in the background. It is
// started from Configure and stopped from Shutdown, so a config reload cannot
// accumulate loops.
var catalogueScheduler = catalog.NewScheduler(0)

// catalogueRefresh drops the cached catalogue and refetches it for the first
// usable account.
//
// The cache is dropped through the same modelCache.reset the TTL path uses, and
// the refetch goes through executeModelCatalog, so a refresh cannot return
// something a client request would not — the only difference is that the cache
// was emptied first.
//
// The previous listing is captured before the reset so the caller can report
// whether the catalogue actually moved: a manual refresh whose button merely
// responded is not evidence the catalogue changed.
func catalogueRefresh(h *abiboot.Host, cfg Config) (catalog.Outcome, error) {
	previous, _ := discoveredModels.peek()
	previousIDs := modelIDSet(previous)

	discoveredModels.reset()

	entries := clineAccounts(h)
	if len(entries) == 0 {
		return catalog.Outcome{}, abiboot.Errorf("cline_no_account",
			"没有可用来拉取目录的 Cline 账号")
	}

	credential, _, errCredential := credentialOf(h, entries[0])
	if errCredential != nil {
		return catalog.Outcome{}, errCredential
	}
	models := executeModelCatalog(h, credential, cfg)
	// executeModelCatalog falls back to the static table when both endpoints
	// fail. That is the right answer for a client request, which must not be
	// left with nothing, but it is not a successful refresh: reporting it as
	// one would tell the operator the vendor agreed when it never answered.
	if !hasRemoteData() {
		return catalog.Outcome{}, abiboot.Errorf("cline_catalogue_degraded",
			"上游目录端点均未返回可用内容，目录未更新（仍沿用上一次结果）")
	}
	ids := modelIDSet(models)
	return catalog.Outcome{Models: len(models), Changed: !sameIDSet(previousIDs, ids)}, nil
}

// hasRemoteData reports whether the most recent fetch reached the vendor.
//
// executeModelCatalog caches only a fetch that actually reached a remote source
// (`discoveredModels.put` is skipped otherwise), so a non-empty cache after a
// refresh means the vendor answered. An empty cache means the static table is
// in play.
func hasRemoteData() bool {
	cached, _ := discoveredModels.peek()
	return len(cached) > 0
}

// modelIDSet collects the model ids of a listing.
func modelIDSet(models []pluginapi.ModelInfo) []string {
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
// The automatic path refreshes the plugin's cache only: it never rewrites an
// auth file, because the host rewrites that same file whenever it renews a
// token. Publishing is the manual button's job, where the operator asked for it.
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

// catalogueForPage returns the catalogue the status page should list, without
// touching the network: the cached remote listing when one was fetched, the
// static table otherwise. A page load must never trigger a fetch — the card
// reports what this deployment currently publishes.
//
// The rows carry the provider's OWN model name (`ModelInfo.Name`, filled from the
// vendor's answer) rather than the routed id, because the card answers "which
// models does Cline offer". The two differ where this deployment renames an id so
// one model does not appear twice under two spellings.
func catalogueForPage() ([]pluginapi.ModelInfo, string) {
	if cached, fetchedAt := discoveredModels.peek(); len(cached) > 0 {
		return cached, "线上目录（缓存于 " + fetchedAt.Local().Format("15:04") + "）"
	}
	cfg := settings()
	if !cfg.ModelDiscovery {
		return staticModelInfos(), "内置静态表（model_discovery 已关闭）"
	}
	return staticModelInfos(), "内置静态表（尚未拉取线上目录）"
}

// catalogueModelEntries maps the page's listing onto the shared card's rows.
func catalogueModelEntries(infos []pluginapi.ModelInfo) []plugui.ModelEntry {
	return plugui.ModelEntriesFromInfo(infos, func(info pluginapi.ModelInfo) string {
		detail := "上限 " + trimNumber(float64(info.MaxCompletionTokens))
		if len(info.SupportedInputModalities) > 0 {
			detail += "，输入 " + strings.Join(info.SupportedInputModalities, "+")
		}
		return detail
	})
}
