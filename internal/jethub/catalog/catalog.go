// Package catalog refreshes a plugin's model catalogue without waiting for a
// client request, and reports what the refresh did.
//
// The catalogue a plugin publishes lives in two places, and only the first is
// the plugin's to change:
//
//  1. the plugin's own cache, filled from the vendor's listing endpoint;
//  2. the host's registry, which is what `GET /v1/models` serves.
//
// A refresh always covers (1): it drops the cache and runs the plugin's normal
// discovery path, so a refresh cannot return something a client request would
// not. Covering (2) needs the host to re-run its model registration for this
// provider, and the plugin ABI has no call for that — `sdk/pluginabi` exposes no
// model-refresh method, and the host's own three-hourly refresh re-registers
// only providers named in its remote models.json, which none of this plugin
// family is. What the host does react to is a semantic change to an auth file:
// it re-registers that provider's models about a second later. Publish performs
// exactly that change.
package catalog

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// PublishKey is the auth-file member the host nudge rewrites.
//
// Its value is bookkeeping for the host's change detector, not state a reader
// may rely on. The host rewrites the auth file when it renews a token, and what
// survives that rewrite is whatever bytes it last read, so a reader that needs
// to know when the catalogue was last refreshed must read the plugin's own
// cache, never this member.
const PublishKey = "model_catalog_published_at"

// refreshLock serialises refreshes. Two accounts refreshing at once would fetch
// the same vendor catalogue twice and race on the same cache.
var refreshLock sync.Mutex

// Outcome is what one catalogue refresh produced.
type Outcome struct {
	// Models is the size of the refreshed catalogue.
	Models int
	// Changed reports whether the refreshed catalogue differs from the one it
	// replaced. The plugin decides this, because only it can see the previous
	// catalogue and the new one.
	Changed bool
}

// Refresh drops the plugin's catalogue cache and refetches. It is what makes a
// refresh equivalent to a client request: the same discovery path, with the
// cache the only thing removed.
type Refresh func() (Outcome, error)

// Request describes one refresh.
type Request struct {
	// Provider is the provider key. It selects which auth file an automatic
	// publish writes through, and labels the log line.
	Provider string
	// Host is the handle the publish uses. It may be nil when no publish is
	// wanted; a cache-only refresh leaves it nil.
	Host *abiboot.Host
	// AuthName is the auth file to publish through, chosen by the caller. A
	// non-empty name publishes unconditionally, which is what the manual button
	// wants: an operator pressing it expects the host to re-register even when
	// the catalogue did not move.
	AuthName string
	// PublishOnChange publishes automatically when the catalogue moved,
	// resolving the auth file from Provider. This is the automatic path: the
	// host re-registers only when there is something new, so a catalogue that
	// is stable costs no auth-file writes at all.
	//
	// It matters because the catalogue a client sees is the host's registry,
	// not this plugin's cache: an automatic refresh that never published would
	// leave `GET /v1/models` stale forever.
	PublishOnChange bool
	// Refresh performs the work. Required.
	Refresh Refresh
}

// Result reports what one refresh did. Every field is evidence for the
// management page: an operator running a manual refresh needs to see whether
// the catalogue moved, not merely that the button responded.
type Result struct {
	// Models is the size of the refreshed catalogue.
	Models int
	// Changed reports whether the catalogue moved.
	Changed bool
	// Published reports whether the host was asked to re-register this
	// provider's models.
	Published bool
	// PublishSkipped explains why a publish did not happen, when the request
	// asked for one. It distinguishes "there was nothing to publish" from
	// "publishing failed".
	PublishSkipped string
	// Duration is how long the refresh took.
	Duration time.Duration
	// Err is the refresh failure, if any. A failed refresh leaves the previous
	// catalogue in place.
	Err error
	// PublishErr is the nudge failure, if any; the catalogue is still refreshed.
	PublishErr error
}

// Run refreshes the catalogue and publishes it by making the host re-register
// this provider's models.
//
// The refresh runs first. Publishing before the new catalogue exists would have
// the host re-register the old one, and the change would stay invisible until
// the next publish.
//
// A publish happens when AuthName names a file, or — for a request that set
// PublishOnChange — when the catalogue actually moved. The two are exclusive in
// practice: the manual path names a file, the automatic path watches for change.
func Run(request Request) Result {
	started := time.Now()
	refreshLock.Lock()
	defer refreshLock.Unlock()

	result := Result{}
	if request.Refresh == nil {
		result.Err = abiboot.Errorf("catalog_no_refresh", "catalogue refresh is not configured")
		return result
	}
	outcome, errRefresh := request.Refresh()
	result.Duration = time.Since(started)
	if errRefresh != nil {
		result.Err = errRefresh
		logOutcome(request, result)
		return result
	}
	result.Models = outcome.Models
	result.Changed = outcome.Changed

	publishName, skip := publishTarget(request, result)
	result.PublishSkipped = skip
	if publishName != "" {
		if errPublish := Publish(request.Host, publishName); errPublish != nil {
			result.PublishErr = errPublish
		} else {
			result.Published = true
		}
	}
	logOutcome(request, result)
	return result
}

// publishTarget decides which auth file this run publishes through, and why not
// when it does not. An explicit name always wins; otherwise an automatic
// publish waits for a real change, so a stable catalogue writes nothing.
func publishTarget(request Request, result Result) (string, string) {
	if name := strings.TrimSpace(request.AuthName); name != "" {
		return name, ""
	}
	if !request.PublishOnChange {
		return "", ""
	}
	if !result.Changed {
		return "", "目录未变化，无需通知宿主"
	}
	name := firstAuthName(request.Host, request.Provider)
	if name == "" {
		return "", "没有可用账号，无法通知宿主重新注册"
	}
	return name, ""
}

// firstAuthName returns the auth file of this provider's first credential, which
// is what an automatic publish needs: a background tick has no request to select
// an account from, and every account of one provider publishes the same
// catalogue.
func firstAuthName(h *abiboot.Host, provider string) string {
	if h == nil || strings.TrimSpace(provider) == "" {
		return ""
	}
	entries, errList := h.ListAuth()
	if errList != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Provider != provider && entry.Type != provider {
			continue
		}
		if name := strings.TrimSpace(entry.Name); name != "" {
			return name
		}
	}
	return ""
}

// logOutcome records one refresh in the host log.
//
// The provider key and the outcome go into the MESSAGE: CPA's console formatter
// prints only the field names in its `logFieldOrder` whitelist
// (`internal/logging/global_logger.go:55-59`), so any field outside that list is
// dropped without a trace.
func logOutcome(request Request, result Result) {
	host := request.Host
	if host == nil {
		return
	}
	level := "info"
	switch {
	case result.Err != nil:
		level = "warn"
	case result.PublishErr != nil:
		level = "warn"
	}
	host.Log(level, request.Provider+" 目录刷新："+result.Describe(), map[string]any{"provider": request.Provider})
}

// Publish makes the host re-register this provider's models by writing a changed
// bookkeeping member into its auth file.
//
// The host deduplicates auth-file events by content hash, so a rewrite that only
// changes formatting is ignored; the written value must differ from the one
// already in the file, which is why the member carries a timestamp.
func Publish(h *abiboot.Host, authName string) error {
	if h == nil {
		return abiboot.Errorf("catalog_no_host", "no host handle")
	}
	name := strings.TrimSpace(authName)
	if name == "" {
		return abiboot.Errorf("catalog_no_auth", "no auth file to publish through")
	}
	auth, errGet := h.GetAuth(authIndexFor(h, name))
	if errGet != nil {
		return errGet
	}
	members := map[string]json.RawMessage{}
	if errUnmarshal := json.Unmarshal(auth.JSON, &members); errUnmarshal != nil {
		return abiboot.Errorf("catalog_auth_unreadable", "auth file %s is not a JSON object", name)
	}
	stamp, errStamp := json.Marshal(time.Now().UTC().Format(time.RFC3339Nano))
	if errStamp != nil {
		return errStamp
	}
	members[PublishKey] = stamp
	updated, errMarshal := json.Marshal(members)
	if errMarshal != nil {
		return errMarshal
	}
	// SaveAuth carries the members the plugin does not define back in, so this
	// write cannot drop the host-owned routing members (`priority`, `weight`)
	// that share the file. It also reads the stored file when the invocation
	// carried none, which is the case for a management route.
	if _, errSave := h.SaveAuth(name, updated); errSave != nil {
		return errSave
	}
	return nil
}

// authIndexFor resolves the runtime index `host.auth.get` requires from the file
// name, which is what a plugin knows. An unresolved name is returned unchanged:
// the host also accepts it, and a lookup failure must not block the publish.
func authIndexFor(h *abiboot.Host, name string) string {
	entries, errList := h.ListAuth()
	if errList != nil {
		return name
	}
	for _, entry := range entries {
		if entry.Name != name && entry.ID != name {
			continue
		}
		if strings.TrimSpace(entry.AuthIndex) != "" {
			return entry.AuthIndex
		}
	}
	return name
}

// Describe renders the result as the one-line notice a management page shows.
func (r Result) Describe() string {
	var builder strings.Builder
	if r.Err != nil {
		builder.WriteString("刷新失败：" + r.Err.Error())
		if r.PublishErr != nil {
			builder.WriteString("；通知宿主也失败：" + r.PublishErr.Error())
		}
		return builder.String()
	}
	if r.Changed {
		builder.WriteString("目录已更新，共 " + strconv.Itoa(r.Models) + " 个模型")
	} else {
		builder.WriteString("目录无变化，共 " + strconv.Itoa(r.Models) + " 个模型")
	}
	switch {
	case r.Published:
		builder.WriteString("；已通知宿主重新注册")
	case r.PublishErr != nil:
		builder.WriteString("；通知宿主失败：" + r.PublishErr.Error())
	case r.PublishSkipped != "":
		builder.WriteString("；" + r.PublishSkipped)
	}
	builder.WriteString("（" + r.Duration.Round(time.Millisecond).String() + "）")
	return builder.String()
}
