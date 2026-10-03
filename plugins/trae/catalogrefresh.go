package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	// Aliased: this package already owns a `catalog` type (the cached
	// discovery result, models.go:709), so the shared package cannot take that
	// name here.
	jethubcatalog "github.com/collegeming/cpa-jethub-plugins/internal/jethub/catalog"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/plugui"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogueScheduler refreshes this plugin's catalog in the background. It is
// started from Configure and stopped from Quiesce/Shutdown, so a config reload
// cannot accumulate loops.
var catalogueScheduler = jethubcatalog.NewScheduler(0)

// catalogueRefresh drops the cached catalog and refetches it for the first usable
// account.
//
// The cache is dropped through the same invalidateCatalog the status page's
// "clear cache" action uses, and the refetch goes through `catalogFor` — the very
// function `handleModelForAuth` calls for a client request — so a refresh cannot
// return something a client request would not, and the two paths cannot drift if
// `catalogFor` ever grows a rule. With the entry invalidated, `catalogFor` always
// misses `cachedCatalog` and therefore always takes its fetch-and-store branch.
//
// The previous listing is captured before the invalidation so the caller can
// report whether the catalog actually moved: a manual refresh whose button merely
// responded is not evidence the catalog changed. `peekCatalog` is used rather
// than `cachedCatalog` because the entry may already have aged out of its TTL,
// and an aged-out entry is still the listing the next request would have
// replaced.
//
// The honesty requirement needs no special handling here: `fetchCatalog` returns
// an ERROR on a non-2xx answer, an unparsable body or an answer with no callable
// entry, and never substitutes the static table itself. The static fallback lives
// in `handleModelForAuth`, which is the right place for a client request that
// must not be left with nothing — and it is exactly why a failed refresh must
// surface as a failure here instead of being reported as the fallback.
func catalogueRefresh(h *abiboot.Host, cfg Config) (jethubcatalog.Outcome, error) {
	if !cfg.DiscoverModels {
		return jethubcatalog.Outcome{}, abiboot.Errorf("trae_catalogue_disabled",
			"远端目录发现已在配置中关闭（discover_models=false），目录固定为内置列表，无可刷新内容")
	}

	entries := traeAccounts(h)
	if len(entries) == 0 {
		return jethubcatalog.Outcome{}, abiboot.Errorf("trae_no_account",
			"没有可用来拉取目录的 TRAE 账号")
	}
	credential, _, errCredential := credentialOf(h, entries[0])
	if errCredential != nil {
		return jethubcatalog.Outcome{}, errCredential
	}

	previous, _ := peekCatalog(cfg, credential)
	previousIDs := callableIDSet(previous.models)

	invalidateCatalog(cfg, credential)

	stored, errCatalog := catalogFor(h, credential, cfg)
	if errCatalog != nil {
		return jethubcatalog.Outcome{}, errCatalog
	}
	ids := callableIDSet(stored.models)
	return jethubcatalog.Outcome{Models: len(ids), Changed: !sameIDSet(previousIDs, ids)}, nil
}

// callableIDSet collects the sorted ids of the models a client would actually
// see: the ones `isModelCallable` keeps and `modelInfoForRemote` publishes.
//
// Comparing the PUBLISHED ids — rather than every entry the vendor returned — is
// what makes `Changed` mean "the model list moved". A flag flip that hides a
// model changes what `model.for_auth` reports while leaving the raw entry in
// place, and reporting that as "no change" would be wrong. `modelInfoForRemote`
// passes `model.ID` through unchanged, so reading the id directly here is the
// same value without building a descriptor per entry.
func callableIDSet(models []remoteModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if !isModelCallable(model) {
			continue
		}
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
// The automatic path publishes only when the catalog actually MOVED
// (`PublishOnChange` resolves the auth file from the provider at each tick,
// because a background tick has no request to select an account from and
// `Configure` runs before any login). A stable catalog therefore costs no
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
	catalogueScheduler.Start(jethubcatalog.Request{
		Host:     host,
		Provider: ProviderKey,
		// A tick publishes only when the catalogue actually moved, so a stable
		// catalogue costs no auth-file writes while `GET /v1/models` still
		// follows the vendor on its own.
		PublishOnChange: true,
		Refresh:         func() (jethubcatalog.Outcome, error) { return catalogueRefresh(host, settings()) },
	})
}

// stopCatalogueScheduler ends the background loop at shutdown.
func stopCatalogueScheduler() { catalogueScheduler.Stop() }

// catalogueRefreshPage refreshes the model catalog and publishes it.
//
// This is the manual counterpart to the background scheduler, and the only path
// that rewrites an auth file: the write is what makes the host re-register this
// provider's models, and it happens only because an operator asked for it.
//
// It is deliberately separate from the status route's existing `action=refresh`,
// which only cleared the cache. That older action is kept as it was: it never
// published, and quietly turning it into a credential-file write would change
// what a link that already exists on other pages does.
func catalogueRefreshPage(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	entry, found := selectAccount(h, request)
	authName := ""
	if found {
		authName = entry.Name
	}
	result := jethubcatalog.Run(jethubcatalog.Request{
		Host:     h,
		Provider: ProviderKey,
		AuthName: authName,
		Refresh:  func() (jethubcatalog.Outcome, error) { return catalogueRefresh(h, cfg) },
	})
	kind := "success"
	if result.Err != nil {
		kind = "danger"
	} else if result.PublishErr != nil {
		kind = "warning"
	}
	return plugui.HTML("TRAE", plugui.Card("刷新模型目录",
		plugui.Group(
			plugui.Notice(kind, result.Describe()),
			plugui.Fields(
				plugui.Field{Label: "远端目录缓存", Value: catalogueCacheText(h, request)},
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
	result := jethubcatalog.Run(jethubcatalog.Request{
		Host:     h,
		Provider: ProviderKey,
		AuthName: authName,
		Refresh:  func() (jethubcatalog.Outcome, error) { return catalogueRefresh(h, settings()) },
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

// publishText describes whether the catalog reached the host registry.
func publishText(result jethubcatalog.Result) string {
	switch {
	case result.Published:
		return "已通知宿主重新注册，/v1/models 约 1 秒后生效"
	case result.PublishErr != nil:
		return "未发布（" + result.PublishErr.Error() + "）：插件缓存已刷新，/v1/models 要等宿主下次重新注册"
	default:
		return "未发布：没有可用的账号文件；插件缓存已刷新"
	}
}

// catalogueCacheText reports what the remote catalog cache currently holds for
// the account the request selected. It answers through the same credential
// resolution the rest of the page uses, and states plainly when nothing can be
// read.
func catalogueCacheText(h *abiboot.Host, request pluginapi.ManagementRequest) string {
	snapshot, ok := currentCatalogSnapshot(h, request)
	if !ok || !snapshot.stored {
		return "尚未拉取（model.for_auth 时获取）"
	}
	return fmt.Sprintf("%d 条，获取于 %s", len(snapshot.models), snapshot.fetchedAt.Local().Format("2006-01-02 15:04"))
}

// catalogSnapshot is the cached listing of the page's selected account.
type catalogSnapshot struct {
	models    []remoteModel
	fetchedAt time.Time
	stored    bool
}

// currentCatalogSnapshot resolves the selected account's cached catalog. A
// failure to resolve it is reported as "nothing cached" rather than as an error:
// the figure is a diagnostic on a page that must render without any account too.
func currentCatalogSnapshot(h *abiboot.Host, request pluginapi.ManagementRequest) (catalogSnapshot, bool) {
	entry, found := selectAccount(h, request)
	if !found {
		return catalogSnapshot{}, false
	}
	credential, _, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return catalogSnapshot{}, false
	}
	cached, ok := peekCatalog(settings(), credential)
	if !ok {
		return catalogSnapshot{}, false
	}
	return catalogSnapshot{models: cached.models, fetchedAt: cached.fetchedAt, stored: true}, true
}

// autoRefreshText describes the background catalog refresh.
//
// The wording must not promise "no credential writes": the automatic path does
// write — once, when the catalog changes — and that write is the whole reason
// `/v1/models` follows the vendor. What it promises instead is that a catalog
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
