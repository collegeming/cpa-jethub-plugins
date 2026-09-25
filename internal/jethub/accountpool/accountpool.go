// Package accountpool is the Go port of the Jet-Hub account pool
// (jethub-src/src/account-pool.ts) and of the account-entry shape declared in
// jethub-src/src/types.ts.
//
// The TypeScript pool persists its index in the `jet-hub` settings namespace and
// keeps credential bodies in the host credential store; the Go port keeps the
// same information in memory and leaves persistence to the caller. What is
// ported here is the selection and rate-limit bookkeeping semantics, not the
// settings plumbing.
//
// Correspondence with the TypeScript implementation:
//
//	Go                                   Jet-Hub
//	------------------------------------ ----------------------------------------
//	Entry                                ProviderAccountEntry (types.ts:42-75)
//	Pool.Upsert                          addAccount / updateAccount (index merge)
//	Pool.Remove                          removeAccount
//	Pool.Pick / Pool.PickExcluding       getAvailableAccount (account-pool.ts:587)
//	Pool.MarkRateLimited                 updateModelRateLimit (account-pool.ts:657)
//	Pool.ClearModelRateLimits            clearModelRateLimits (account-pool.ts:533)
//	Pool.SweepExpiredRateLimits          sweepExpiredRateLimits (account-pool.ts:677)
//	Pool.MarkAuthError                   (no equivalent; see MarkAuthError)
//	Pool.MarkUsed                        (no equivalent; LastUsed feeds the LRU order)
//	Pool.Snapshot                        listAccounts / listAllAccounts
//
// Two deliberate differences from the TypeScript pool:
//
//   - Candidate order. Jet-Hub iterates the account array in the user's Jet Hub
//     drag order and explicitly refuses to re-sort ("NO re-sort: array order =
//     user's Jet Hub drag order = selection priority", account-pool.ts:601).
//     The Go port has no drag order, so Pick orders candidates by Priority and
//     then by least-recently-used, as specified for this port.
//   - Expiry. Jet-Hub does not filter on `expiresAt` inside getAvailableAccount
//     (callers pre-check it, and the chat loop refreshes on a 401). Pick here
//     also skips entries whose ExpiresAt is in the past, because the Go port has
//     no equivalent pre-check at the call site.
package accountpool

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimit is one model's cooldown marker. Jet-Hub stores a bare reset
// timestamp (`Record<modelId, resetAtMs>`); the code and message are carried
// along so callers can surface why the account was parked.
type RateLimit struct {
	// ResetAt is when the cooldown ends. The zero value means "not limited".
	ResetAt time.Time
	// Code is the upstream business code that triggered the cooldown, if any
	// (e.g. CodeBuddy's 6004).
	Code string
	// Message is the upstream message that triggered the cooldown, if any.
	Message string
}

// Entry is one account-pool entry, the Go counterpart of ProviderAccountEntry.
type Entry struct {
	// ID is the account's unique identifier, e.g. "codearts-a1b2c3d4".
	ID string
	// Provider owns the entry; the empty string matches any pool provider.
	Provider string
	// CredentialRef names the credential body in the host credential store,
	// e.g. "CODEARTS_ACCOUNT_A1B2C3D4".
	CredentialRef string
	// Nickname is the user-facing label.
	Nickname string
	// Enabled is the user's switch. Disabled entries never take part in
	// automatic selection, but remain in the pool.
	Enabled bool
	// Priority orders candidates; the lower value wins. Ties break on LastUsed
	// (oldest first) and then on ID.
	Priority int
	// ExpiresAt is the credential expiry. The zero value means "unknown", which
	// is treated as not expired.
	ExpiresAt time.Time
	// LastUsed is when the entry was last handed out by Pick.
	LastUsed time.Time
	// Refreshable mirrors ProviderAccountEntry.refreshable.
	Refreshable bool
	// ModelRateLimits maps a model ID to its cooldown marker.
	ModelRateLimits map[string]RateLimit
	// AuthFailed is set by MarkAuthError and takes the entry out of selection
	// until ClearAuthError or a fresh Upsert.
	AuthFailed bool
}

// Option customises a Pool.
type Option func(*Pool)

// WithClock injects the time source used for expiry and rate-limit checks.
func WithClock(clock func() time.Time) Option {
	return func(p *Pool) {
		if clock != nil {
			p.now = clock
		}
	}
}

// Pool is an in-memory account pool. It is safe for concurrent use.
type Pool struct {
	mu       sync.Mutex
	provider string
	now      func() time.Time
	entries  map[string]Entry
}

// New creates a pool. When provider is non-empty, Pick only considers entries
// whose Provider is that value or empty.
func New(provider string, opts ...Option) *Pool {
	pool := &Pool{
		provider: provider,
		now:      time.Now,
		entries:  make(map[string]Entry, 4),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(pool)
		}
	}
	return pool
}

// Provider is the provider key this pool serves (possibly empty).
func (p *Pool) Provider() string { return p.provider }

// Len reports how many entries the pool holds.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// Upsert inserts or fully replaces an entry. Jet-Hub splits this across
// addAccount and updateAccount; both end in the same "replace the whole list"
// write, so a single Upsert is the faithful primitive.
//
// Upserting an entry clears its AuthFailed flag: a caller that just wrote a
// fresh credential body is exactly the case the flag was guarding against.
func (p *Pool) Upsert(entry Entry) {
	if entry.ID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry.ModelRateLimits = cloneRateLimits(entry.ModelRateLimits)
	entry.AuthFailed = false
	p.entries[entry.ID] = entry
}

// Remove drops an entry. Removing an unknown id is a no-op.
func (p *Pool) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, id)
}

// Get returns a copy of one entry.
func (p *Pool) Get(id string) (Entry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[id]
	if !ok {
		return Entry{}, false
	}
	entry.ModelRateLimits = cloneRateLimits(entry.ModelRateLimits)
	return entry, true
}

// MarkUsed records that an entry was just selected.
func (p *Pool) MarkUsed(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[id]
	if !ok {
		return
	}
	entry.LastUsed = p.now()
	p.entries[id] = entry
}

// MarkRateLimited mirrors updateModelRateLimit: it merges a cooldown for one
// model into the entry. An unknown id is a no-op (Jet-Hub logs a warning).
func (p *Pool) MarkRateLimited(id, model string, resetAt time.Time, code, message string) {
	if model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[id]
	if !ok {
		return
	}
	if entry.ModelRateLimits == nil {
		entry.ModelRateLimits = make(map[string]RateLimit, 1)
	}
	entry.ModelRateLimits[model] = RateLimit{ResetAt: resetAt, Code: code, Message: message}
	p.entries[id] = entry
}

// ClearModelRateLimits mirrors clearModelRateLimits: called with no model IDs it
// clears every marker for the entry, otherwise only the named models. It
// returns how many markers were removed.
func (p *Pool) ClearModelRateLimits(id string, models ...string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[id]
	if !ok || len(entry.ModelRateLimits) == 0 {
		return 0
	}
	removed := 0
	if len(models) == 0 {
		removed = len(entry.ModelRateLimits)
		entry.ModelRateLimits = nil
	} else {
		for _, model := range models {
			if _, ok := entry.ModelRateLimits[model]; ok {
				delete(entry.ModelRateLimits, model)
				removed++
			}
		}
	}
	p.entries[id] = entry
	return removed
}

// SweepExpiredRateLimits mirrors sweepExpiredRateLimits: markers whose reset
// time has passed are dropped. Pick calls it implicitly; it is exported for
// callers that want to reclaim the memory on their own schedule.
func (p *Pool) SweepExpiredRateLimits() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepExpiredRateLimitsLocked(p.now())
}

// MarkAuthError takes an entry out of automatic selection after the upstream
// rejected its credential (401/403/APIG.0602 in the TypeScript adapter).
//
// Jet-Hub has no pool-level equivalent: llm-adapter.ts refreshes the credential
// once behind an `authRefreshed` guard and retries the same account. The Go port
// records the failure so a pool-driven caller stops re-picking a dead account;
// ClearAuthError or a fresh Upsert (a successful re-login/refresh) puts it back.
func (p *Pool) MarkAuthError(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[id]
	if !ok {
		return
	}
	entry.AuthFailed = true
	p.entries[id] = entry
}

// ClearAuthError re-enables an entry parked by MarkAuthError.
func (p *Pool) ClearAuthError(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[id]
	if !ok {
		return
	}
	entry.AuthFailed = false
	p.entries[id] = entry
}

// Pick returns the best entry for a model request.
//
// Candidates must be Enabled, not AuthFailed, not expired, and not rate-limited
// for that exact model. They are ordered by Priority, then by LastUsed (oldest
// first, i.e. least-recently-used), then by ID. An empty model skips the
// rate-limit filter, mirroring Jet-Hub ("空 modelId：无可比对的键").
func (p *Pool) Pick(model string) (Entry, bool) {
	return p.PickExcluding(model, nil)
}

// PickExcluding is Pick with a set of account IDs to skip. Jet-Hub uses
// `excludeAccountIds` so a request-level rotation loop does not pick the same
// account twice; the Go port exposes the same escape hatch for the same reason.
func (p *Pool) PickExcluding(model string, exclude map[string]struct{}) (Entry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	p.sweepExpiredRateLimitsLocked(now)

	candidates := make([]Entry, 0, len(p.entries))
	for id, entry := range p.entries {
		if p.provider != "" && entry.Provider != "" && entry.Provider != p.provider {
			continue
		}
		if !entry.Enabled || entry.AuthFailed {
			continue
		}
		if !entry.ExpiresAt.IsZero() && !now.Before(entry.ExpiresAt) {
			continue
		}
		if exclude != nil {
			if _, skip := exclude[id]; skip {
				continue
			}
		}
		if model != "" {
			if limit, ok := entry.ModelRateLimits[model]; ok && !limit.ResetAt.IsZero() && now.Before(limit.ResetAt) {
				continue
			}
		}
		entry.ModelRateLimits = cloneRateLimits(entry.ModelRateLimits)
		candidates = append(candidates, entry)
	}
	if len(candidates) == 0 {
		return Entry{}, false
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		if !candidates[i].LastUsed.Equal(candidates[j].LastUsed) {
			return candidates[i].LastUsed.Before(candidates[j].LastUsed)
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], true
}

// Snapshot returns a copy of every entry, ordered by Priority then ID. The
// returned entries own their rate-limit maps; mutating them does not touch the
// pool.
func (p *Pool) Snapshot() []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()

	snapshot := make([]Entry, 0, len(p.entries))
	for _, entry := range p.entries {
		entry.ModelRateLimits = cloneRateLimits(entry.ModelRateLimits)
		snapshot = append(snapshot, entry)
	}
	sort.Slice(snapshot, func(i, j int) bool {
		if snapshot[i].Priority != snapshot[j].Priority {
			return snapshot[i].Priority < snapshot[j].Priority
		}
		return snapshot[i].ID < snapshot[j].ID
	})
	return snapshot
}

// sweepExpiredRateLimitsLocked drops markers that have already reset. Callers
// must hold p.mu.
func (p *Pool) sweepExpiredRateLimitsLocked(now time.Time) {
	for id, entry := range p.entries {
		if len(entry.ModelRateLimits) == 0 {
			continue
		}
		changed := false
		for model, limit := range entry.ModelRateLimits {
			if !limit.ResetAt.IsZero() && !now.Before(limit.ResetAt) {
				delete(entry.ModelRateLimits, model)
				changed = true
			}
		}
		if changed {
			p.entries[id] = entry
		}
	}
}

func cloneRateLimits(source map[string]RateLimit) map[string]RateLimit {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]RateLimit, len(source))
	for model, limit := range source {
		cloned[model] = limit
	}
	return cloned
}

// resetTimePattern is the reset-time sentence lifted verbatim from
// llm-adapter.ts:1527:
//
//	const RESET_TIME_PATTERN = /(?:将在|reset at)\s+([\d-]+\s+[\d:]+)\s+(UTC[+-]\d+(?::\d+)?)/i
//
// It captures the wall-clock stamp and the timezone the server announced, e.g.
//
//	"您的使用量已超出频率限制，将在 2026-09-11 18:08:17 UTC+8 重置"
//	"... your usage will reset at 2026-09-17 09:09:36 UTC+8, alternatively, ..."
var resetTimePattern = regexp.MustCompile(`(?i)(?:将在|reset at)\s+([\d-]+\s+[\d:]+)\s+(UTC[+-]\d+(?::\d+)?)`)

// minEpochSeconds is the smallest numeric value ParseResetAt accepts as an
// epoch timestamp (2001-09-09T01:46:40Z). It exists so that short numeric
// business codes are not silently read as 1970-era timestamps.
const minEpochSeconds = 1_000_000_000

// ParseResetAt extracts a rate-limit reset time from an upstream value.
//
// Accepted forms, in the order they are tried:
//
//  1. the reset-time sentence above, with the announced UTC offset applied;
//  2. RFC3339 / RFC3339Nano / ISO-8601 date-times, with or without a zone and
//     with either a "T" or a space between date and time;
//  3. a bare calendar date;
//  4. unix seconds (including a fractional part) and unix milliseconds,
//     distinguished by magnitude: values >= 1e12 are milliseconds.
//
// Huawei numeric error codes: the TypeScript does nothing specific with them, so
// neither does this port. For the record, the only reset-time logic in
// llm-adapter.ts:1527-1554 is the pattern above plus this fallback:
//
//	// fallback if no parseable time: Date.now() + 3_600_000  (1h)
//
// i.e. a body that is rate-limited but has no parseable stamp yields
// `now + 1h`, and a body that is not rate-limited at all yields null. Business
// codes are never interpreted as timestamps; accordingly ParseResetAt rejects
// values below minEpochSeconds rather than inventing a code-to-time mapping.
func ParseResetAt(raw string) (time.Time, bool) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return time.Time{}, false
	}

	if match := resetTimePattern.FindStringSubmatch(text); match != nil {
		if parsed, ok := parseOffsetStamp(match[1], match[2]); ok {
			return parsed, true
		}
	}

	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed, true
		}
	}

	return parseEpochNumber(text)
}

// utcOffsetPattern matches the zone the reset-time sentence announces, e.g.
// "UTC+8", "UTC+08:00", "UTC-5:30".
var utcOffsetPattern = regexp.MustCompile(`(?i)^UTC([+-])(\d{1,2})(?::(\d{2}))?$`)

// parseOffsetStamp parses the sentence's wall-clock stamp and applies its
// announced UTC offset. Go's own "-07" layout only accepts a zero-padded hour,
// while the portal sends "UTC+8", so the offset is applied by hand.
func parseOffsetStamp(stamp, zone string) (time.Time, bool) {
	match := utcOffsetPattern.FindStringSubmatch(strings.TrimSpace(zone))
	if match == nil {
		return time.Time{}, false
	}
	hours, errHours := strconv.Atoi(match[2])
	if errHours != nil {
		return time.Time{}, false
	}
	minutes := 0
	if match[3] != "" {
		parsedMinutes, errMinutes := strconv.Atoi(match[3])
		if errMinutes != nil {
			return time.Time{}, false
		}
		minutes = parsedMinutes
	}
	offset := (hours*60 + minutes) * 60
	if match[1] == "-" {
		offset = -offset
	}
	parsed, errParse := time.Parse("2006-01-02 15:04:05", stamp)
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed.Add(-time.Duration(offset) * time.Second), true
}

// parseEpochNumber interprets a numeric reset time as unix seconds or
// milliseconds.
func parseEpochNumber(raw string) (time.Time, bool) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < minEpochSeconds {
		return time.Time{}, false
	}
	if value >= 1e12 {
		return time.UnixMilli(int64(value)), true
	}
	seconds := int64(value)
	nanos := int64((value - float64(seconds)) * float64(time.Second))
	return time.Unix(seconds, nanos), true
}
