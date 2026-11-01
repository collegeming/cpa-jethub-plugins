package main

import (
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// pointsCacheEntry remembers one balance read long enough that a status page
// reload or a quota.fetch burst inside the TTL costs zero upstream calls.
type pointsCacheEntry struct {
	// Snapshot is nil-and-cacheable: "the server did not publish balance" is
	// itself a fact for the TTL, not a reason to retry on every page load.
	Snapshot *pointsSnapshot
	Fetched  time.Time
}

var pointsCache sync.Map // account key -> pointsCacheEntry

// pointsCacheKey identifies the account one snapshot belongs to. UserID is
// the stable account id; the phone number is the fallback for credentials
// that carry none (WeChat-bound accounts).
func pointsCacheKey(credential *Credential) string {
	if credential == nil {
		return ""
	}
	if id := strings.TrimSpace(credential.UserID); id != "" {
		return "u:" + id
	}
	if phone := strings.TrimSpace(credential.Phone); phone != "" {
		return "p:" + phone
	}
	return ""
}

// cachedFetchPoints wraps fetchPoints with the configured per-account TTL.
// Read failures are never cached: a transient transport error must not stick
// to the account for the whole TTL.
func cachedFetchPoints(h *abiboot.Host, credential *Credential, cfg Config) (*pointsSnapshot, error) {
	ttl := time.Duration(cfg.PointsCacheTTLMS) * time.Millisecond
	key := pointsCacheKey(credential)
	if ttl <= 0 || key == "" {
		return fetchPoints(h, credential, cfg)
	}
	if value, ok := pointsCache.Load(key); ok {
		if entry, ok := value.(pointsCacheEntry); ok && time.Since(entry.Fetched) < ttl {
			return entry.Snapshot, nil
		}
	}
	snapshot, err := fetchPoints(h, credential, cfg)
	if err == nil {
		pointsCache.Store(key, pointsCacheEntry{Snapshot: snapshot, Fetched: time.Now()})
	}
	return snapshot, err
}

// invalidatePoints drops one account's remembered balance so the read after a
// write (daily grant claim) shows the fresh figure.
func invalidatePoints(credential *Credential) {
	if key := pointsCacheKey(credential); key != "" {
		pointsCache.Delete(key)
	}
}

// resetPointsCache clears every remembered balance; tests use it so a stubbed
// transport is not masked by an earlier test's reads.
func resetPointsCache() {
	pointsCache.Range(func(key, _ any) bool {
		pointsCache.Delete(key)
		return true
	})
}
