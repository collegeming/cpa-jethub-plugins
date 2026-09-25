package main

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Client version resolution, ported from jethub-src/src/lobsterai.ts:501-603.
//
// The version is a fetch-time dependency for three call sites: the exchange
// body (`version`), the refresh body and the check-in query. It is a
// date-shaped value (for example 2026.9.4) that changes at client release
// cadence, so it is cached for twelve hours.

// clientVersionPattern mirrors the reference validator
// `^(\d+(?:\.\d+)*)(?:-[0-9A-Za-z.-]+)?$` (lobsterai.ts:520). Validating
// matters because the version is a mandatory check-in query parameter: passing
// an HTML error page through would fail later with a much less readable error.
var clientVersionPattern = regexp.MustCompile(`^(\d+(?:\.\d+)*)(?:-[0-9A-Za-z.-]+)?$`)

// parseClientVersion normalises a version literal, returning ok=false when the
// shape is wrong (including null, empty and non-string input).
func parseClientVersion(raw any) (string, bool) {
	text, isText := raw.(string)
	if !isText {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if !clientVersionPattern.MatchString(trimmed) {
		return "", false
	}
	return trimmed, true
}

// parseClientVersionFromUpdate extracts data.value.version from the update
// endpoint response. That response is not the business envelope: code/msg sit
// at the outer level and the payload is under data.value
// (lobsterai.ts:524-538).
func parseClientVersionFromUpdate(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	var decoded any
	if errUnmarshal := decodeJSON(body, &decoded); errUnmarshal != nil {
		return "", false
	}
	return parseClientVersionFromUpdateValue(decoded)
}

// parseClientVersionFromUpdateValue is parseClientVersionFromUpdate over an
// already-decoded JSON value.
func parseClientVersionFromUpdateValue(body any) (string, bool) {
	outer := asRecord(body)
	if outer == nil {
		return "", false
	}
	value := asRecord(outer["data"])
	if value == nil {
		return "", false
	}
	payload := asRecord(value["value"])
	if payload == nil {
		return "", false
	}
	return parseClientVersion(payload["version"])
}

// clientVersionResolver holds the process-wide version cache. The reference
// implementation extracts this into a class so unit tests do not share mutable
// state; the Go port keeps one instance but exposes the fields so tests can
// reset them.
type clientVersionResolver struct {
	mu       sync.Mutex
	cached   string
	cachedAt time.Time
}

var versionResolver clientVersionResolver

// resolve returns the client version and its provenance
// ("config" | "cache" | "remote" | "fallback").
//
// A failed fetch intentionally does NOT raise: the caller keeps working with
// the fallback value and records the source, exactly like the reference
// implementation (lobsterai.ts:562-597). The fallback is never cached so a
// transient failure cannot poison the next twelve hours.
func (r *clientVersionResolver) resolve(h *abiboot.Host, cfg Config, now time.Time) (string, string) {
	if override := strings.TrimSpace(cfg.ClientVersionOverride); override != "" {
		return override, "config"
	}

	ttl := VersionCacheTTL
	r.mu.Lock()
	if r.cached != "" && now.Sub(r.cachedAt) < ttl {
		cached := r.cached
		r.mu.Unlock()
		return cached, "cache"
	}
	r.mu.Unlock()

	if h == nil {
		return FallbackClientVersion, "fallback"
	}
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", UserAgent)
	response, errDo := h.HTTPDo(abiboot.HTTPDoRequest{
		Method:  http.MethodGet,
		URL:     ClientVersionAPI,
		Headers: headers,
	})
	if errDo != nil {
		return FallbackClientVersion, "fallback"
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return FallbackClientVersion, "fallback"
	}
	version, ok := parseClientVersionFromUpdate(response.Body)
	if !ok {
		return FallbackClientVersion, "fallback"
	}

	r.mu.Lock()
	r.cached = version
	r.cachedAt = now
	r.mu.Unlock()
	return version, "remote"
}

// reset drops the cached version. Used by tests and by "force refresh" paths.
func (r *clientVersionResolver) reset() {
	r.mu.Lock()
	r.cached = ""
	r.cachedAt = time.Time{}
	r.mu.Unlock()
}

// resolveClientVersion is the package-level convenience used by the request
// paths.
func resolveClientVersion(h *abiboot.Host, cfg Config) string {
	version, _ := versionResolver.resolve(h, cfg, time.Now())
	return version
}
