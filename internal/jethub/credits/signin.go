package credits

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Outcome statuses. These mirror the `kind` discriminants of ClaimOutcome in
// jethub-src/src/credits.ts, which the CodeArts claim also returns
// (codearts-credits.ts reuses the same type so the Jet Hub summary UI needs no
// change).
const (
	// StatusClaimed means credits were granted by this call.
	StatusClaimed = "claimed"
	// StatusAlreadyClaimed means the daily reward was already taken; the
	// TypeScript reports it as a non-fatal, idempotent result.
	StatusAlreadyClaimed = "already-claimed"
	// StatusInactive means the account or campaign is not eligible.
	StatusInactive = "inactive"
	// StatusFailed means the request failed and may be retried.
	StatusFailed = "failed"
)

// ErrSignerNotFound is returned by TriggerOnce for an unregistered account.
var ErrSignerNotFound = errors.New("credits: no signer registered for account")

// Outcome is the result of one daily check-in.
//
// It flattens credits.ts' ClaimOutcome union: Claimed carries Amount,
// AlreadyClaimed/Inactive/Failed carry a Message, and Failed may carry an
// upstream code in Message exactly as the TypeScript builds it.
type Outcome struct {
	// Status is one of the Status* constants.
	Status string
	// Message is the user-facing explanation.
	Message string
	// Amount is the number of credits granted; zero for non-claimed outcomes.
	Amount float64
}

// Signer performs one daily check-in for a single account. The CodeArts
// implementation wraps claimCodeArtsDailyCheckin (codearts-credits.ts).
type Signer interface {
	Sign(ctx context.Context) (Outcome, error)
}

// SignerFunc adapts a function to Signer.
type SignerFunc func(ctx context.Context) (Outcome, error)

// Sign implements Signer.
func (f SignerFunc) Sign(ctx context.Context) (Outcome, error) { return f(ctx) }

// SchedulerOptions configures NewScheduler.
type SchedulerOptions struct {
	// Clock supplies now; it keeps the daily cadence free of drift. Defaults to
	// time.Now.
	Clock Clock
	// Sleep waits between passes. Defaults to DefaultSleep.
	Sleep SleepFunc
	// Interval is the delay between two passes. Defaults to 24h.
	Interval time.Duration
	// OnResult receives the outcome of every attempt; a nil error means the
	// sign-in call itself succeeded (the Outcome may still be non-claimed).
	OnResult func(accountID string, outcome Outcome, err error)
}

// Scheduler drives a daily check-in per registered account. It ports the Jet
// Hub "claim all" behaviour: every account is attempted, and one account's
// failure never stops the pass.
//
// The scheduler is transport-free: each account supplies its own Signer, which
// owns credential resolution and the upstream request.
type Scheduler struct {
	mu       sync.Mutex
	signers  map[string]Signer
	order    []string
	clock    Clock
	sleep    SleepFunc
	interval time.Duration
	onResult func(accountID string, outcome Outcome, err error)

	cancel  context.CancelFunc
	running bool
}

// NewScheduler builds a check-in scheduler.
func NewScheduler(options SchedulerOptions) *Scheduler {
	scheduler := &Scheduler{
		signers:  make(map[string]Signer, 4),
		clock:    options.Clock,
		sleep:    options.Sleep,
		interval: options.Interval,
		onResult: options.OnResult,
	}
	if scheduler.clock == nil {
		scheduler.clock = time.Now
	}
	if scheduler.sleep == nil {
		scheduler.sleep = DefaultSleep
	}
	if scheduler.interval <= 0 {
		scheduler.interval = 24 * time.Hour
	}
	return scheduler
}

// Register adds or replaces the signer for an account. Registration order is
// preserved so a pass is deterministic.
func (s *Scheduler) Register(accountID string, signer Signer) {
	if accountID == "" || signer == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.signers[accountID]; !exists {
		s.order = append(s.order, accountID)
	}
	s.signers[accountID] = signer
}

// Unregister drops an account from the scheduler.
func (s *Scheduler) Unregister(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.signers[accountID]; !exists {
		return
	}
	delete(s.signers, accountID)
	for index, id := range s.order {
		if id == accountID {
			s.order = append(s.order[:index], s.order[index+1:]...)
			break
		}
	}
}

// Accounts lists the registered account IDs in registration order.
func (s *Scheduler) Accounts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// TriggerOnce signs in a single account immediately. It is the Go counterpart of
// the one-account path behind Jet Hub's per-account "claim" button.
func (s *Scheduler) TriggerOnce(ctx context.Context, accountID string) (Outcome, error) {
	s.mu.Lock()
	signer, ok := s.signers[accountID]
	s.mu.Unlock()
	if !ok {
		return Outcome{}, fmt.Errorf("%w: %s", ErrSignerNotFound, accountID)
	}
	return signer.Sign(ctx)
}

// Run performs one pass over every registered account immediately, then repeats
// every Interval until ctx is cancelled or Stop is called.
//
// The pass is unconditional: the daily claim is idempotent server-side (the
// CodeArts flow pre-checks the campaign's `claimable` flag), so a restart must
// not skip a day.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.running = true
	interval := s.interval
	s.mu.Unlock()

	defer func() {
		cancel()
		s.mu.Lock()
		s.cancel = nil
		s.running = false
		s.mu.Unlock()
	}()

	for {
		s.runPass(runCtx)
		next := s.clock().Add(interval)
		delay := next.Sub(s.clock())
		if delay < 0 {
			delay = 0
		}
		if err := s.sleep(runCtx, delay); err != nil {
			return
		}
	}
}

// Stop cancels a running scheduler. It is safe to call when Run is not running.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// runPass attempts every registered account once, in registration order.
func (s *Scheduler) runPass(ctx context.Context) {
	for _, accountID := range s.Accounts() {
		s.mu.Lock()
		signer, ok := s.signers[accountID]
		s.mu.Unlock()
		if !ok {
			continue
		}
		outcome, err := signer.Sign(ctx)
		if s.onResult != nil {
			s.onResult(accountID, outcome, err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}
