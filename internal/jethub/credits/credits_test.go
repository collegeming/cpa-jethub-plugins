package credits

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeTime is an injectable clock + sleep pair: every sleep advances the clock
// and records the requested duration, so scheduling rules can be asserted
// without waiting on the wall clock.
type fakeTime struct {
	mu     sync.Mutex
	now    time.Time
	delays []time.Duration
}

func newFakeTime(now time.Time) *fakeTime { return &fakeTime{now: now} }

func (f *fakeTime) Clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeTime) Sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	f.delays = append(f.delays, d)
	f.now = f.now.Add(d)
	f.mu.Unlock()
	return ctx.Err()
}

func (f *fakeTime) Delays() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.delays...)
}

func TestRefreshConstantsMatchRefreshTS(t *testing.T) {
	if RefreshLeadMS != time.Hour {
		t.Fatalf("RefreshLeadMS = %v, want 1h", RefreshLeadMS)
	}
	if RefreshRetryMS != 10*time.Minute {
		t.Fatalf("RefreshRetryMS = %v, want 10m", RefreshRetryMS)
	}
	if RefreshAbnormalNetworkRetryMS != time.Minute {
		t.Fatalf("RefreshAbnormalNetworkRetryMS = %v, want 1m", RefreshAbnormalNetworkRetryMS)
	}
	if RefreshScanIntervalMS != 30*time.Minute {
		t.Fatalf("RefreshScanIntervalMS = %v, want 30m", RefreshScanIntervalMS)
	}
}

func TestFirstRefreshDelayLeadWindow(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	if got := FirstRefreshDelay(now.Add(RefreshLeadMS-time.Millisecond), now, 0); got != 0 {
		t.Fatalf("delay inside the lead window = %v, want 0", got)
	}
	if got := FirstRefreshDelay(now, now, 0); got != 0 {
		t.Fatalf("delay for an expired credential = %v, want 0", got)
	}
	if got := FirstRefreshDelay(time.Time{}, now, 0); got != 0 {
		t.Fatalf("delay for an unknown expiry = %v, want 0", got)
	}
}

func TestFirstRefreshDelayJitterWindow(t *testing.T) {
	// Seconds are zero, which is the case refresh.spec.ts exercises and the case
	// where Date.setSeconds and "add 0-59s" coincide.
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(24 * time.Hour)

	if got := FirstRefreshDelay(expiresAt, now, 0); got != RefreshLeadMS {
		t.Fatalf("jitter 0 delay = %v, want %v", got, RefreshLeadMS)
	}
	if got := FirstRefreshDelay(expiresAt, now, 59); got != RefreshLeadMS+59*time.Second {
		t.Fatalf("jitter 59 delay = %v, want %v", got, RefreshLeadMS+59*time.Second)
	}

	// The default jitter must land inside [lead, lead+60s) when now is exactly
	// on a minute boundary.
	for i := 0; i < 64; i++ {
		got := FirstRefreshDelay(expiresAt, now, i%60)
		if got < RefreshLeadMS || got >= RefreshLeadMS+time.Minute {
			t.Fatalf("jitter %d delay = %v, want within [%v, %v)", i%60, got, RefreshLeadMS, RefreshLeadMS+time.Minute)
		}
	}
}

// TestFirstRefreshDelayReplacesSeconds documents the faithful port of
// `trigger.setSeconds(...)`: it replaces the seconds field rather than adding an
// offset, so a nonzero second(now) shifts the delay below the lead time.
func TestFirstRefreshDelayReplacesSeconds(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 45, 500, time.UTC)
	expiresAt := now.Add(24 * time.Hour)

	want := RefreshLeadMS - 45*time.Second
	if got := FirstRefreshDelay(expiresAt, now, 0); got != want {
		t.Fatalf("delay = %v, want %v (setSeconds semantics)", got, want)
	}
	if got := FirstRefreshDelay(expiresAt, now, 59); got != want+59*time.Second {
		t.Fatalf("delay jitter 59 = %v, want %v", got, want+59*time.Second)
	}
}

func TestRefreshSchedulerImmediateTrigger(t *testing.T) {
	clock := newFakeTime(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	calls := 0
	scheduler := NewRefreshScheduler(RefreshSchedulerOptions{
		Refresh: func(context.Context) error { calls++; return nil },
		Clock:   clock.Clock,
		Sleep:   clock.Sleep,
		Jitter:  func() int { return 0 },
	})
	scheduler.Arm(clock.Clock().Add(2 * time.Minute)) // inside the 1h lead window

	if err := scheduler.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
	if delays := clock.Delays(); len(delays) != 1 || delays[0] != 0 {
		t.Fatalf("sleeps = %v, want [0]", delays)
	}
}

func TestRefreshSchedulerSchedulesAtJitteredLeadTime(t *testing.T) {
	clock := newFakeTime(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	scheduler := NewRefreshScheduler(RefreshSchedulerOptions{
		Refresh: func(context.Context) error { return nil },
		Clock:   clock.Clock,
		Sleep:   clock.Sleep,
		Jitter:  func() int { return 17 },
	})
	scheduler.Arm(clock.Clock().Add(24 * time.Hour))

	if err := scheduler.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := RefreshLeadMS + 17*time.Second
	if delays := clock.Delays(); len(delays) != 1 || delays[0] != want {
		t.Fatalf("sleeps = %v, want [%v]", delays, want)
	}
}

func TestRefreshSchedulerRetriesOnFailure(t *testing.T) {
	clock := newFakeTime(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	calls := 0
	var reported []error
	scheduler := NewRefreshScheduler(RefreshSchedulerOptions{
		Refresh: func(context.Context) error {
			calls++
			if calls == 1 {
				return errors.New("network down")
			}
			return nil
		},
		OnError: func(err error) { reported = append(reported, err) },
		Clock:   clock.Clock,
		Sleep:   clock.Sleep,
		Jitter:  func() int { return 0 },
	})
	scheduler.Arm(clock.Clock().Add(-time.Minute))

	if err := scheduler.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("refresh calls = %d, want 2", calls)
	}
	if len(reported) != 1 {
		t.Fatalf("onError calls = %d, want 1", len(reported))
	}
	delays := clock.Delays()
	if len(delays) != 2 || delays[0] != 0 || delays[1] != RefreshRetryMS {
		t.Fatalf("sleeps = %v, want [0 %v]", delays, RefreshRetryMS)
	}
}

func TestRefreshSchedulerAbnormalNetworkRetry(t *testing.T) {
	for _, message := range []string{
		"fetch failed",
		"dial tcp: lookup sts.example: no such host",
		"dial tcp 127.0.0.1:8080: connect: connection refused",
		"proxy error",
		"getaddrinfo ENOTFOUND",
	} {
		t.Run(message, func(t *testing.T) {
			clock := newFakeTime(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
			calls := 0
			scheduler := NewRefreshScheduler(RefreshSchedulerOptions{
				Refresh: func(context.Context) error {
					calls++
					if calls == 1 {
						return errors.New(message)
					}
					return nil
				},
				Clock:  clock.Clock,
				Sleep:  clock.Sleep,
				Jitter: func() int { return 0 },
			})
			scheduler.Arm(clock.Clock().Add(-time.Minute))

			if err := scheduler.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			delays := clock.Delays()
			if len(delays) != 2 || delays[1] != RefreshAbnormalNetworkRetryMS {
				t.Fatalf("sleeps = %v, want [0 %v]", delays, RefreshAbnormalNetworkRetryMS)
			}
		})
	}
}

func TestRefreshSchedulerStopsOnTerminalError(t *testing.T) {
	cases := map[string]error{
		"sentinel":          ErrRefreshTokenExpired,
		"typed":             &RefreshTokenExpiredError{Message: "refresh token rejected"},
		"wrapped sentinel":  fmt.Errorf("sts exchange: %w", ErrRefreshTokenExpired),
		"message heuristic": errors.New("refresh_token 已失效，请重新登录"),
		"snake case":        errors.New("invalid refresh_token"),
	}

	for name, terminal := range cases {
		t.Run(name, func(t *testing.T) {
			clock := newFakeTime(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
			calls := 0
			onErrorCalls := 0
			scheduler := NewRefreshScheduler(RefreshSchedulerOptions{
				Refresh: func(context.Context) error { calls++; return terminal },
				OnError: func(error) { onErrorCalls++ },
				Clock:   clock.Clock,
				Sleep:   clock.Sleep,
				Jitter:  func() int { return 0 },
			})
			scheduler.Arm(clock.Clock().Add(-time.Minute))

			err := scheduler.Run(context.Background())
			if !errors.Is(err, ErrRefreshTokenExpired) {
				t.Fatalf("Run error = %v, want ErrRefreshTokenExpired", err)
			}
			if calls != 1 {
				t.Fatalf("refresh calls = %d, want 1 (no retry after a terminal error)", calls)
			}
			if onErrorCalls != 1 {
				t.Fatalf("onError calls = %d, want 1", onErrorCalls)
			}
			if delays := clock.Delays(); len(delays) != 1 {
				t.Fatalf("sleeps = %v, want exactly the initial trigger", delays)
			}
		})
	}
}

func TestRefreshSchedulerStopCancelsRetry(t *testing.T) {
	clock := newFakeTime(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	calls := 0
	var scheduler *RefreshScheduler
	sleeps := 0
	sleep := func(ctx context.Context, d time.Duration) error {
		sleeps++
		if sleeps == 2 {
			// Simulate a logout landing while the retry is pending.
			scheduler.Stop()
		}
		return clock.Sleep(ctx, d)
	}
	scheduler = NewRefreshScheduler(RefreshSchedulerOptions{
		Refresh: func(context.Context) error { calls++; return errors.New("network down") },
		Clock:   clock.Clock,
		Sleep:   sleep,
		Jitter:  func() int { return 0 },
	})
	scheduler.Arm(clock.Clock().Add(-time.Minute))

	err := scheduler.Run(context.Background())
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("Run error = %v, want ErrStopped", err)
	}
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}

func TestRefreshSchedulerRunRequiresArm(t *testing.T) {
	scheduler := NewRefreshScheduler(RefreshSchedulerOptions{Refresh: func(context.Context) error { return nil }})
	if err := scheduler.Run(context.Background()); !errors.Is(err, ErrNotArmed) {
		t.Fatalf("Run error = %v, want ErrNotArmed", err)
	}
}

func TestRefreshSchedulerRefreshOnceDoesNotRetry(t *testing.T) {
	calls := 0
	scheduler := NewRefreshScheduler(RefreshSchedulerOptions{
		Refresh: func(context.Context) error { calls++; return errors.New("network down") },
	})
	if err := scheduler.RefreshOnce(context.Background()); err == nil {
		t.Fatal("RefreshOnce error = nil, want failure")
	}
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}

func TestRefreshAllIteratesDisabledRefreshableAccounts(t *testing.T) {
	accounts := []RefreshableAccount{
		{ID: "enabled-refreshable", Enabled: true, Refreshable: true},
		{ID: "disabled-refreshable", Enabled: false, Refreshable: true},
		{ID: "enabled-static", Enabled: true, Refreshable: false},
		{ID: "terminal", Enabled: false, Refreshable: true},
	}
	var refreshed []string
	var terminal []string

	failures := RefreshAll(context.Background(), accounts, RefreshAllOptions{
		Refresh: func(_ context.Context, id string) error {
			refreshed = append(refreshed, id)
			if id == "terminal" {
				return &RefreshTokenExpiredError{Message: "dead"}
			}
			return nil
		},
		OnTerminal: func(id string, _ error) { terminal = append(terminal, id) },
	})

	if len(failures) != 1 {
		t.Fatalf("failures = %v, want 1", failures)
	}
	want := []string{"enabled-refreshable", "disabled-refreshable", "terminal"}
	if len(refreshed) != len(want) {
		t.Fatalf("refreshed = %v, want %v", refreshed, want)
	}
	for index := range want {
		if refreshed[index] != want[index] {
			t.Fatalf("refreshed = %v, want %v", refreshed, want)
		}
	}
	if len(terminal) != 1 || terminal[0] != "terminal" {
		t.Fatalf("OnTerminal = %v, want [terminal]", terminal)
	}
}

func TestIsRefreshTokenExpiredAndAbnormalNetwork(t *testing.T) {
	if IsRefreshTokenExpired(nil) {
		t.Fatal("IsRefreshTokenExpired(nil) = true")
	}
	if IsRefreshTokenExpired(errors.New("plain failure")) {
		t.Fatal("plain failure reported as terminal")
	}
	if !IsAbnormalNetworkError(errors.New("dial tcp: connection refused")) {
		t.Fatal("connection refused not detected")
	}
	if IsAbnormalNetworkError(errors.New("HTTP 401 unauthorized")) {
		t.Fatal("HTTP error reported as a network error")
	}
	if got := RetryDelay(errors.New("fetch failed")); got != RefreshAbnormalNetworkRetryMS {
		t.Fatalf("RetryDelay(fetch failed) = %v", got)
	}
	if got := RetryDelay(errors.New("boom")); got != RefreshRetryMS {
		t.Fatalf("RetryDelay(boom) = %v", got)
	}
}

func TestSchedulerTriggerOnce(t *testing.T) {
	scheduler := NewScheduler(SchedulerOptions{})
	scheduler.Register("a", SignerFunc(func(context.Context) (Outcome, error) {
		return Outcome{Status: StatusClaimed, Amount: 1000, Message: "ok"}, nil
	}))
	scheduler.Register("b", SignerFunc(func(context.Context) (Outcome, error) {
		return Outcome{Status: StatusAlreadyClaimed, Message: "今天已领取"}, nil
	}))

	outcome, err := scheduler.TriggerOnce(context.Background(), "a")
	if err != nil {
		t.Fatalf("TriggerOnce: %v", err)
	}
	if outcome.Status != StatusClaimed || outcome.Amount != 1000 {
		t.Fatalf("outcome = %+v", outcome)
	}

	outcome, err = scheduler.TriggerOnce(context.Background(), "b")
	if err != nil {
		t.Fatalf("TriggerOnce(b): %v", err)
	}
	if outcome.Status != StatusAlreadyClaimed {
		t.Fatalf("outcome = %+v", outcome)
	}

	if _, err = scheduler.TriggerOnce(context.Background(), "missing"); !errors.Is(err, ErrSignerNotFound) {
		t.Fatalf("TriggerOnce(missing) error = %v, want ErrSignerNotFound", err)
	}
	if got := scheduler.Accounts(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Accounts() = %v", got)
	}

	scheduler.Unregister("a")
	if got := scheduler.Accounts(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("Accounts() after Unregister = %v", got)
	}
}

func TestSchedulerRunSignsEveryAccountOncePerPass(t *testing.T) {
	clock := newFakeTime(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	var mu sync.Mutex
	signed := map[string]int{}

	var scheduler *Scheduler
	// Stop the scheduler during its first inter-pass sleep so Run terminates.
	sleep := func(ctx context.Context, d time.Duration) error {
		scheduler.Stop()
		return clock.Sleep(ctx, d)
	}
	scheduler = NewScheduler(SchedulerOptions{
		Clock:    clock.Clock,
		Sleep:    sleep,
		Interval: time.Hour,
		OnResult: func(accountID string, outcome Outcome, err error) {
			if err != nil {
				t.Errorf("sign in %s: %v", accountID, err)
			}
			mu.Lock()
			signed[accountID]++
			mu.Unlock()
		},
	})
	for _, id := range []string{"a", "b", "c"} {
		scheduler.Register(id, SignerFunc(func(context.Context) (Outcome, error) {
			return Outcome{Status: StatusClaimed, Amount: 100}, nil
		}))
	}

	scheduler.Run(context.Background())

	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"a", "b", "c"} {
		if signed[id] != 1 {
			t.Fatalf("account %s signed %d times, want 1", id, signed[id])
		}
	}
	if delays := clock.Delays(); len(delays) != 1 || delays[0] != time.Hour {
		t.Fatalf("inter-pass sleeps = %v, want [1h]", delays)
	}
}
