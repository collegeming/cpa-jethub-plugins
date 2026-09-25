// Package credits ports the Jet-Hub credential-refresh scheduling
// (jethub-src/src/refresh.ts) and the daily check-in framework used by the
// CodeArts credit claim (jethub-src/src/credits.ts and codearts-credits.ts).
//
// Correspondence with the TypeScript implementation:
//
//	Go                                      Jet-Hub
//	--------------------------------------- -------------------------------------
//	RefreshLeadMS                           REFRESH_LEAD_MS          (refresh.ts:2)
//	RefreshRetryMS                          REFRESH_RETRY_MS         (refresh.ts:4)
//	RefreshAbnormalNetworkRetryMS           REFRESH_ABNORMAL_NETWORK_RETRY_MS
//	FirstRefreshDelay                       computeFirstRefreshDelayMs (refresh.ts:32)
//	RefreshScheduler                        RefreshScheduler         (refresh.ts:43)
//	RefreshScheduler.Arm                    arm()
//	RefreshScheduler.Stop                   stop()
//	RefreshScheduler.RefreshOnce            refreshOnce()
//	RefreshAll                              service.ts refreshAll + index.ts
//	                                        refreshAllCredentials / REFRESH_INTERVAL_MS
//	IsAbnormalNetworkError                  isAbnormalNetworkError   (refresh.ts:9)
//	IsRefreshTokenExpired                   isRefreshTokenExpired    (refresh.ts:21)
//	RetryDelay                              the retry branch of RefreshScheduler.run
//	Signer, Scheduler, Outcome              credits.ts ClaimOutcome + the Jet Hub
//	                                        daily-check-in driver; the concrete
//	                                        CodeArts claim lives in
//	                                        plugins/codearts/credits.go
//
// The package is transport-free: callers inject the refresh/sign-in operations,
// the clock and the sleep function, which keeps the scheduling rules unit
// testable without wall-clock waits.
package credits

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"sync"
	"time"
)

// Timing constants, taken verbatim from refresh.ts:2-6:
//
//	/** 在凭据过期前提前这么长时间触发刷新（1 小时；对齐真实插件的 36e5）。 */
//	export const REFRESH_LEAD_MS = 3_600_000
//	/** 普通刷新失败后的重试间隔（10 分钟；...）。 */
//	export const REFRESH_RETRY_MS = 600_000
//	/** 异常网络（fetch failed 等）后的重试间隔（1 分钟；...）。 */
//	export const REFRESH_ABNORMAL_NETWORK_RETRY_MS = 60_000
const (
	// RefreshLeadMS is how long before expiry the refresh is triggered.
	RefreshLeadMS = 3_600_000 * time.Millisecond
	// RefreshRetryMS is the retry interval after an ordinary refresh failure.
	RefreshRetryMS = 600_000 * time.Millisecond
	// RefreshAbnormalNetworkRetryMS is the retry interval after a transient
	// network failure.
	RefreshAbnormalNetworkRetryMS = 60_000 * time.Millisecond
	// RefreshScanIntervalMS is the batch scan interval from index.ts:486
	// (`const REFRESH_INTERVAL_MS = 30 * 60 * 1000`). It bounds how often
	// RefreshAll is expected to sweep the pool.
	RefreshScanIntervalMS = 30 * 60 * 1000 * time.Millisecond
)

// Errors returned by the refresh scheduler.
var (
	// ErrRefreshTokenExpired marks a terminal refresh failure: the refresh
	// token is dead and the user must sign in again. It mirrors oauth.ts'
	// RefreshTokenExpiredError, which refresh.ts matches structurally rather
	// than by `instanceof` (see IsRefreshTokenExpired).
	ErrRefreshTokenExpired = errors.New("credits: refresh token expired")
	// ErrStopped is returned by Run when Stop was called.
	ErrStopped = errors.New("credits: refresh scheduler stopped")
	// ErrNotArmed is returned by Run when Arm was never called.
	ErrNotArmed = errors.New("credits: refresh scheduler is not armed")
	// ErrAlreadyRunning is returned by Run when a run is already in flight; the
	// TypeScript scheduler silently ignores the second trigger (`if
	// (this.pending) return`).
	ErrAlreadyRunning = errors.New("credits: refresh scheduler is already running")
)

// RefreshTokenExpiredError is the Go counterpart of the providers'
// RefreshTokenExpiredError classes. It reports itself through errors.Is as
// ErrRefreshTokenExpired.
type RefreshTokenExpiredError struct {
	Message string
}

func (e *RefreshTokenExpiredError) Error() string {
	if e == nil || e.Message == "" {
		return "refresh token expired, please sign in again"
	}
	return e.Message
}

// Unwrap makes errors.Is(err, ErrRefreshTokenExpired) true.
func (e *RefreshTokenExpiredError) Unwrap() error { return ErrRefreshTokenExpired }

// Clock supplies the current time. Injected so scheduling is unit testable.
type Clock func() time.Time

// SleepFunc waits for d, honouring ctx cancellation. Injected for the same
// reason as Clock.
type SleepFunc func(ctx context.Context, d time.Duration) error

// DefaultSleep is the production SleepFunc: it returns ctx.Err() when ctx is
// cancelled before d elapses, and nil otherwise.
func DefaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isAbnormalNetworkError ports refresh.ts:9-12 verbatim, with Go network error
// strings added:
//
//	function isAbnormalNetworkError(error: unknown): boolean {
//	  const message = error instanceof Error ? error.message : String(error)
//	  return /fetch failed|ENOTFOUND|ECONNREFUSED|proxy|unresolved host|getaddrinfo/i.test(message)
//	}
//
// `connection refused` and `no such host` are the Go net.DNSError / net.OpError
// wordings for two of the JavaScript cases.
var abnormalNetworkPattern = regexp.MustCompile(
	`(?i)fetch failed|ENOTFOUND|ECONNREFUSED|proxy|unresolved host|getaddrinfo|connection refused|no such host`,
)

// IsAbnormalNetworkError reports whether err is a transient network failure
// that deserves the short retry interval.
func IsAbnormalNetworkError(err error) bool {
	if err == nil {
		return false
	}
	return abnormalNetworkPattern.MatchString(err.Error())
}

// refreshTokenPattern ports the message half of refresh.ts:21-25:
//
//	function isRefreshTokenExpired(error: unknown): boolean {
//	  if (!(error instanceof Error)) return false
//	  if (error.name === 'RefreshTokenExpiredError') return true
//	  return /refresh[_ ]?token/i.test(error.message)
//	}
//
// The TypeScript comments explain the structural check: CodeArts and Buddy each
// export a class of the same name, so `instanceof` would let one provider's
// signal through as retryable and retry forever.
var refreshTokenPattern = regexp.MustCompile(`(?i)refresh[_ ]?token`)

// IsRefreshTokenExpired reports whether err is terminal. Mirroring the
// TypeScript, this is a structural check: the sentinel, the typed error, or any
// error whose message mentions the refresh token.
func IsRefreshTokenExpired(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRefreshTokenExpired) {
		return true
	}
	var typed *RefreshTokenExpiredError
	if errors.As(err, &typed) {
		return true
	}
	return refreshTokenPattern.MatchString(err.Error())
}

// RetryDelay is the interval the scheduler waits before retrying a failed
// refresh: the abnormal-network interval for transient errors, the ordinary
// retry interval otherwise.
func RetryDelay(err error) time.Duration {
	if IsAbnormalNetworkError(err) {
		return RefreshAbnormalNetworkRetryMS
	}
	return RefreshRetryMS
}

// terminalError normalises a terminal refresh failure so every caller can test
// errors.Is(err, ErrRefreshTokenExpired) regardless of how the provider reported
// it (sentinel, typed error, or a message that merely mentions the token).
func terminalError(err error) error {
	if errors.Is(err, ErrRefreshTokenExpired) {
		return err
	}
	return errors.Join(ErrRefreshTokenExpired, err)
}

// FirstRefreshDelay ports computeFirstRefreshDelayMs (refresh.ts:32-40):
//
//	export function computeFirstRefreshDelayMs(expiresAtMs: number, nowMs = Date.now()): number {
//	  if (!Number.isFinite(expiresAtMs)) return 0
//	  const leadTrigger = nowMs + REFRESH_LEAD_MS
//	  if (leadTrigger >= expiresAtMs) return 0
//	  const trigger = new Date(leadTrigger)
//	  trigger.setSeconds(Math.floor(60 * Math.random()))
//	  const delay = trigger.getTime() - nowMs
//	  return delay > 0 ? delay : 0
//	}
//
// ⚠️ Faithful porting note: the TypeScript *comment* says the random 0-59s value
// is "叠加" (added), but `Date.setSeconds` **replaces the seconds field** of the
// 1h-later instant while keeping its milliseconds. The delay is therefore
//
//	1h + (jitterSeconds - second(now)) * 1s
//
// i.e. jitter spans 1h±59s rather than [1h, 1h+60s). At second(now)==0 — the
// case the TS test uses — the two readings coincide. This port keeps the code's
// behaviour, not the comment's.
//
// A zero expiresAt behaves like the TypeScript `!Number.isFinite`: refresh now.
func FirstRefreshDelay(expiresAt, now time.Time, jitterSeconds int) time.Duration {
	if expiresAt.IsZero() {
		return 0
	}
	leadTrigger := now.Add(RefreshLeadMS)
	if !leadTrigger.Before(expiresAt) {
		return 0
	}
	if jitterSeconds < 0 {
		jitterSeconds = 0
	}
	if jitterSeconds > 59 {
		jitterSeconds = 59
	}
	trigger := leadTrigger.Add(-time.Duration(leadTrigger.Second()-jitterSeconds) * time.Second)
	delay := trigger.Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

// RefreshSchedulerOptions configures NewRefreshScheduler.
type RefreshSchedulerOptions struct {
	// Refresh performs one token refresh.
	Refresh func(ctx context.Context) error
	// OnError receives every failed refresh before the retry decision, matching
	// the TypeScript `onError` constructor argument.
	OnError func(error)
	// Clock supplies now. Defaults to time.Now.
	Clock Clock
	// Sleep waits between attempts. Defaults to DefaultSleep.
	Sleep SleepFunc
	// Jitter returns the 0-59 seconds value that replaces the seconds field of
	// the 1h-later trigger. Defaults to a random value.
	Jitter func() int
}

// RefreshScheduler is the Go port of refresh.ts' RefreshScheduler: one trigger
// plus retry-on-failure, stopping permanently on a dead refresh token.
//
// Contract differences from the TypeScript timer-based class: this scheduler
// owns a blocking Run instead of a `setTimeout` it re-arms internally. Arm sets
// (or replaces) the schedule; Run waits for it, refreshes, and retries. An Arm
// or Stop during a wait interrupts that wait:
//
//   - Arm during a wait makes Run adopt the new schedule and keep going;
//   - Stop during a wait makes Run return ErrStopped.
//
// A successful refresh ends Run (the TypeScript leaves the scheduler idle until
// the next arm). A terminal failure also ends Run, returning the terminal error
// so the caller can clear the account's refreshable flag.
type RefreshScheduler struct {
	refresh func(ctx context.Context) error
	onError func(error)
	clock   Clock
	sleep   SleepFunc
	jitter  func() int

	mu          sync.Mutex
	armed       bool
	expiresAt   time.Time
	generation  int
	running     bool
	sleepCancel context.CancelFunc
}

// NewRefreshScheduler builds a scheduler. Refresh must be non-nil.
func NewRefreshScheduler(options RefreshSchedulerOptions) *RefreshScheduler {
	scheduler := &RefreshScheduler{
		refresh: options.Refresh,
		onError: options.OnError,
		clock:   options.Clock,
		sleep:   options.Sleep,
		jitter:  options.Jitter,
	}
	if scheduler.clock == nil {
		scheduler.clock = time.Now
	}
	if scheduler.sleep == nil {
		scheduler.sleep = DefaultSleep
	}
	if scheduler.jitter == nil {
		scheduler.jitter = func() int { return rand.IntN(60) }
	}
	return scheduler
}

// Arm sets the credential expiry to schedule against, replacing any previous
// schedule. Port of RefreshScheduler.arm.
func (s *RefreshScheduler) Arm(expiresAt time.Time) {
	s.mu.Lock()
	s.generation++
	s.expiresAt = expiresAt
	s.armed = true
	cancel := s.sleepCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Stop cancels the schedule and any wait in progress. Port of
// RefreshScheduler.stop.
func (s *RefreshScheduler) Stop() {
	s.mu.Lock()
	s.generation++
	s.armed = false
	cancel := s.sleepCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// RefreshOnce cancels the schedule and refreshes immediately without arming a
// retry. Port of RefreshScheduler.refreshOnce.
func (s *RefreshScheduler) RefreshOnce(ctx context.Context) error {
	s.Stop()
	err := s.refresh(ctx)
	if err != nil && s.onError != nil {
		s.onError(err)
	}
	return err
}

// Run waits for the armed schedule, refreshes, and retries per the Jet-Hub
// rules. It returns:
//
//   - nil after a successful refresh;
//   - the terminal error when IsRefreshTokenExpired reports one (permanent stop);
//   - ErrStopped when Stop is called, ErrAlreadyRunning on a concurrent call,
//     ErrNotArmed when no expiry was armed;
//   - ctx.Err() when ctx is cancelled.
func (s *RefreshScheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	if !s.armed {
		s.mu.Unlock()
		return ErrNotArmed
	}
	s.running = true
	generation := s.generation
	expiresAt := s.expiresAt
	sleepCtx, cancelSleep := context.WithCancel(ctx)
	s.sleepCancel = cancelSleep
	s.mu.Unlock()

	defer func() {
		cancelSleep()
		s.mu.Lock()
		if s.sleepCancel != nil {
			s.sleepCancel = nil
		}
		s.running = false
		s.mu.Unlock()
	}()

	delay := FirstRefreshDelay(expiresAt, s.clock(), s.jitter())
	for {
		errSleep := s.sleep(sleepCtx, delay)
		if errSleep != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			nextGeneration, nextExpires, armed := s.state()
			if !armed {
				return ErrStopped
			}
			if nextGeneration == generation {
				// An unrelated SleepFunc failure: surface it.
				return errSleep
			}
			generation, expiresAt = nextGeneration, nextExpires
			delay = FirstRefreshDelay(expiresAt, s.clock(), s.jitter())
			continue
		}

		// Honour an Arm/Stop that landed while we were waiting.
		nextGeneration, nextExpires, armed := s.state()
		if !armed {
			return ErrStopped
		}
		if nextGeneration != generation {
			generation, expiresAt = nextGeneration, nextExpires
			delay = FirstRefreshDelay(expiresAt, s.clock(), s.jitter())
			continue
		}

		errRefresh := s.refresh(ctx)
		if errRefresh == nil {
			return nil
		}
		if s.onError != nil {
			s.onError(errRefresh)
		}

		// TypeScript order: generation check first, then the terminal check.
		// The first discards a retry armed by an obsolete schedule; the second
		// stops permanently when the refresh token is dead.
		nextGeneration, nextExpires, armed = s.state()
		if !armed {
			return ErrStopped
		}
		if nextGeneration != generation {
			generation, expiresAt = nextGeneration, nextExpires
			delay = FirstRefreshDelay(expiresAt, s.clock(), s.jitter())
			continue
		}
		if IsRefreshTokenExpired(errRefresh) {
			return terminalError(errRefresh)
		}
		delay = RetryDelay(errRefresh)
	}
}

// state reads the current schedule under the lock.
func (s *RefreshScheduler) state() (generation int, expiresAt time.Time, armed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation, s.expiresAt, s.armed
}

// RefreshableAccount is the account-pool view RefreshAll needs.
type RefreshableAccount struct {
	// ID is the account identifier handed back to the refresh callback.
	ID string
	// Enabled is deliberately unused by RefreshAll; it exists so callers pass
	// the pool entry directly.
	Enabled bool
	// Refreshable mirrors ProviderAccountEntry.refreshable.
	Refreshable bool
}

// RefreshAllOptions configures RefreshAll.
type RefreshAllOptions struct {
	// Refresh renews one account.
	Refresh func(ctx context.Context, id string) error
	// OnTerminal, when set, is called for an account that failed terminally
	// (dead refresh token, or credentials that no longer parse) so the caller
	// can clear its refreshable flag — exactly what service.ts does.
	OnTerminal func(id string, err error)
}

// RefreshAll ports service.ts refreshAll together with index.ts'
// refreshAllCredentials scan.
//
// ⚠️ Only `Refreshable` is filtered on, never `Enabled`: disabling an account
// only removes it from automatic selection, it must not stop its credential from
// being kept fresh (refresh.ts / index.ts:526 document this as a real defect the
// TypeScript once had). A failure on one account never stops the scan.
//
// The returned slice holds one error per failed account, in scan order.
func RefreshAll(ctx context.Context, accounts []RefreshableAccount, options RefreshAllOptions) []error {
	var failures []error
	for _, account := range accounts {
		if !account.Refreshable {
			continue
		}
		if err := options.Refresh(ctx, account.ID); err != nil {
			failures = append(failures, fmt.Errorf("refresh account %s: %w", account.ID, err))
			if options.OnTerminal != nil && IsRefreshTokenExpired(err) {
				options.OnTerminal(account.ID, err)
			}
		}
	}
	return failures
}
