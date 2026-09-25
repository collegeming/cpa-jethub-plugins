package accountpool

import (
	"testing"
	"time"
)

func testNow() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

func entry(id string) Entry {
	return Entry{
		ID:            id,
		Provider:      "codearts",
		CredentialRef: "CODEARTS_ACCOUNT_" + id,
		Enabled:       true,
	}
}

func TestPickSkipsDisabledExpiredRateLimitedAndAuthFailed(t *testing.T) {
	now := testNow()
	pool := New("codearts", WithClock(func() time.Time { return now }))

	disabled := entry("disabled")
	disabled.Enabled = false
	expired := entry("expired")
	expired.ExpiresAt = now.Add(-time.Minute)
	limited := entry("limited")
	authFailed := entry("auth")
	usable := entry("usable")

	pool.Upsert(disabled)
	pool.Upsert(expired)
	pool.Upsert(limited)
	pool.Upsert(authFailed)
	pool.Upsert(usable)
	pool.MarkRateLimited("limited", "glm-5.3-flash", now.Add(time.Hour), "6004", "rate limited")
	pool.MarkAuthError("auth")

	picked, ok := pool.Pick("glm-5.3-flash")
	if !ok {
		t.Fatal("Pick returned no entry")
	}
	if picked.ID != "usable" {
		t.Fatalf("Pick returned %q, want usable", picked.ID)
	}

	// An empty model has no key to compare against, so the cooldown is ignored
	// (Jet-Hub: "空 modelId（未知目标模型）：无可比对的键，保持候选不变").
	picked, ok = pool.PickExcluding("", map[string]struct{}{"usable": {}})
	if !ok || picked.ID != "limited" {
		t.Fatalf("PickExcluding = %q (%v), want limited", picked.ID, ok)
	}
}

func TestPickOrdersByPriorityThenLeastRecentlyUsed(t *testing.T) {
	now := testNow()
	pool := New("codearts", WithClock(func() time.Time { return now }))

	highPriority := entry("p1") // Priority 1 beats 2
	highPriority.Priority = 1
	highPriority.LastUsed = now.Add(-time.Minute)

	lowPriorityNew := entry("p2-new")
	lowPriorityNew.Priority = 2
	lowPriorityNew.LastUsed = now.Add(-time.Hour) // oldest, but worse priority

	lowPriorityOld := entry("p2-old")
	lowPriorityOld.Priority = 2
	lowPriorityOld.LastUsed = now.Add(-2 * time.Hour)

	pool.Upsert(lowPriorityNew)
	pool.Upsert(lowPriorityOld)
	pool.Upsert(highPriority)

	picked, ok := pool.Pick("")
	if !ok || picked.ID != "p1" {
		t.Fatalf("Pick = %q (%v), want p1 (priority wins)", picked.ID, ok)
	}

	pool.MarkUsed("p1")
	if updated, _ := pool.Get("p1"); !updated.LastUsed.Equal(now) {
		t.Fatalf("MarkUsed did not stamp LastUsed: %v", updated.LastUsed)
	}

	pool.Remove("p1")
	picked, ok = pool.Pick("")
	if !ok || picked.ID != "p2-old" {
		t.Fatalf("Pick = %q (%v), want p2-old (least recently used)", picked.ID, ok)
	}
}

func TestRateLimitExpiresWithClock(t *testing.T) {
	now := testNow()
	clock := func() time.Time { return now }
	pool := New("codearts", WithClock(clock))

	pool.Upsert(entry("a"))
	pool.MarkRateLimited("a", "deepseek-v4-flash", now.Add(10*time.Minute), "429", "too many requests")

	if _, ok := pool.Pick("deepseek-v4-flash"); ok {
		t.Fatal("Pick returned a rate-limited entry")
	}
	stored, _ := pool.Get("a")
	if limit := stored.ModelRateLimits["deepseek-v4-flash"]; limit.Code != "429" || limit.Message != "too many requests" {
		t.Fatalf("stored rate limit = %+v", limit)
	}

	now = now.Add(11 * time.Minute)
	picked, ok := pool.Pick("deepseek-v4-flash")
	if !ok || picked.ID != "a" {
		t.Fatalf("Pick after reset = %q (%v), want a", picked.ID, ok)
	}
	// The expired marker is swept lazily.
	stored, _ = pool.Get("a")
	if _, present := stored.ModelRateLimits["deepseek-v4-flash"]; present {
		t.Fatal("expired rate-limit marker was not swept")
	}
	if removed := pool.ClearModelRateLimits("a"); removed != 0 {
		t.Fatalf("ClearModelRateLimits removed %d, want 0", removed)
	}
}

func TestClearModelRateLimitsSelective(t *testing.T) {
	now := testNow()
	pool := New("codearts", WithClock(func() time.Time { return now }))
	pool.Upsert(entry("a"))
	pool.MarkRateLimited("a", "m1", now.Add(time.Hour), "", "")
	pool.MarkRateLimited("a", "m2", now.Add(time.Hour), "", "")

	if removed := pool.ClearModelRateLimits("a", "m1"); removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	stored, _ := pool.Get("a")
	if _, present := stored.ModelRateLimits["m1"]; present {
		t.Fatal("m1 marker still present")
	}
	if _, present := stored.ModelRateLimits["m2"]; !present {
		t.Fatal("m2 marker was removed")
	}
	if _, ok := pool.Pick("m2"); ok {
		t.Fatal("Pick returned an entry still limited on m2")
	}
	if _, ok := pool.Pick("m1"); !ok {
		t.Fatal("Pick skipped an entry whose m1 marker was cleared")
	}
}

func TestUpsertClearsAuthFailureAndRemoveForgets(t *testing.T) {
	pool := New("codearts")
	pool.Upsert(entry("a"))
	pool.MarkAuthError("a")
	if _, ok := pool.Pick(""); ok {
		t.Fatal("Pick returned an auth-failed entry")
	}
	pool.ClearAuthError("a")
	if _, ok := pool.Pick(""); !ok {
		t.Fatal("ClearAuthError did not restore the entry")
	}
	pool.MarkAuthError("a")
	pool.Upsert(entry("a"))
	if _, ok := pool.Pick(""); !ok {
		t.Fatal("Upsert did not clear the auth failure")
	}

	pool.Remove("a")
	if pool.Len() != 0 {
		t.Fatalf("Len = %d, want 0", pool.Len())
	}
	if _, ok := pool.Pick(""); ok {
		t.Fatal("Pick returned an entry after Remove")
	}
}

func TestProviderFilter(t *testing.T) {
	pool := New("codearts")
	pool.Upsert(entry("a"))
	other := entry("b")
	other.Provider = "buddy"
	pool.Upsert(other)

	snapshot := pool.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snapshot))
	}
	picked, ok := pool.PickExcluding("", map[string]struct{}{"a": {}})
	if ok {
		t.Fatalf("PickExcluding returned %q, want no codearts entry", picked.ID)
	}
}

func TestSnapshotIsADeepCopy(t *testing.T) {
	now := testNow()
	pool := New("codearts", WithClock(func() time.Time { return now }))
	pool.Upsert(entry("a"))
	pool.MarkRateLimited("a", "m", now.Add(time.Hour), "1", "x")

	snapshot := pool.Snapshot()
	snapshot[0].ModelRateLimits["m"] = RateLimit{ResetAt: now}
	delete(snapshot[0].ModelRateLimits, "m")

	stored, _ := pool.Get("a")
	if _, present := stored.ModelRateLimits["m"]; !present {
		t.Fatal("mutating the snapshot changed the pool")
	}
}

func TestSnapshotOrdersByPriorityThenID(t *testing.T) {
	pool := New("")
	second := entry("b")
	second.Priority = 5
	first := entry("a")
	first.Priority = 1
	pool.Upsert(second)
	pool.Upsert(first)

	snapshot := pool.Snapshot()
	if snapshot[0].ID != "a" || snapshot[1].ID != "b" {
		t.Fatalf("Snapshot order = %q, %q", snapshot[0].ID, snapshot[1].ID)
	}
}

func TestParseResetAt(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // RFC3339, or "" for "not parseable"
	}{
		{
			name: "chinese portal sentence",
			raw:  "您的使用量已超出频率限制，将在 2026-09-11 18:08:17 UTC+8 重置",
			want: "2026-09-11T10:08:17Z",
		},
		{
			name: "english portal sentence",
			raw:  "your usage will reset at 2026-09-17 09:09:36 UTC+8, alternatively, contact support",
			want: "2026-09-17T01:09:36Z",
		},
		{
			name: "portal sentence with minutes in the offset",
			raw:  "将在 2026-09-11 18:08:17 UTC+05:30 重置",
			want: "2026-09-11T12:38:17Z",
		},
		{name: "rfc3339", raw: "2026-09-11T18:08:17Z", want: "2026-09-11T18:08:17Z"},
		{name: "rfc3339 with offset", raw: "2026-09-11T18:08:17+08:00", want: "2026-09-11T10:08:17Z"},
		{name: "rfc3339 nano", raw: "2026-09-11T18:08:17.123456789Z", want: "2026-09-11T18:08:17Z"},
		{name: "iso space separated", raw: "2026-09-11 18:08:17", want: "2026-09-11T18:08:17Z"},
		{name: "bare date", raw: "2026-09-11", want: "2026-09-11T00:00:00Z"},
		{name: "unix seconds", raw: "1757500000", want: "2025-09-10T10:26:40Z"},
		{name: "unix milliseconds", raw: "1757500000000", want: "2025-09-10T10:26:40Z"},
		{name: "unix seconds with fraction", raw: "1757500000.5", want: "2025-09-10T10:26:40Z"},
		// Huawei / CodeBuddy numeric business codes must NOT become 1970 epochs.
		{name: "business code 6004", raw: "6004", want: ""},
		{name: "empty", raw: "   ", want: ""},
		{name: "garbage", raw: "not a time", want: ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := ParseResetAt(testCase.raw)
			if testCase.want == "" {
				if ok {
					t.Fatalf("ParseResetAt(%q) = %v, want not parseable", testCase.raw, got)
				}
				return
			}
			if !ok {
				t.Fatalf("ParseResetAt(%q) not parseable, want %s", testCase.raw, testCase.want)
			}
			if got.UTC().Format(time.RFC3339) != testCase.want {
				t.Fatalf("ParseResetAt(%q) = %s, want %s", testCase.raw, got.UTC().Format(time.RFC3339), testCase.want)
			}
		})
	}
}

func TestParseResetAtFractionalSeconds(t *testing.T) {
	got, ok := ParseResetAt("1757500000.25")
	if !ok {
		t.Fatal("fractional unix seconds not parsed")
	}
	if got.Nanosecond() != 250_000_000 {
		t.Fatalf("nanos = %d, want 250000000", got.Nanosecond())
	}
}

func TestUpsertIgnoresEmptyID(t *testing.T) {
	pool := New("")
	pool.Upsert(Entry{CredentialRef: "x"})
	if pool.Len() != 0 {
		t.Fatalf("Len = %d, want 0", pool.Len())
	}
}
