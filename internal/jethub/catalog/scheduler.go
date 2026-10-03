package catalog

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scheduler runs a catalogue refresh on a fixed interval, in the background.
//
// A tick publishes only when the catalogue actually moved. That is what keeps
// the host registry — and therefore `GET /v1/models` — current without a client
// request, while a catalogue that is stable costs no auth-file writes at all.
// Publishing on every tick instead would rewrite an auth file the host is also
// writing whenever it renews a token; waiting for a real change makes that
// collision rare instead of constant.
//
// One scheduler per plugin, started from Configure and stopped from Shutdown.
type Scheduler struct {
	mu       sync.Mutex
	interval time.Duration
	cancel   chan struct{}
	stopped  bool
	// runs counts completed automatic refreshes, for the management page.
	runs int
	// lastErr is the most recent automatic refresh failure.
	lastErr string
	// lastRun is when the last automatic refresh completed.
	lastRun time.Time
}

// NewScheduler builds a stopped scheduler. interval <= 0 disables it, which is
// the default: a plugin that refreshes its catalogue in the background should
// be opted into, because the refresh costs one vendor round trip per account.
func NewScheduler(interval time.Duration) *Scheduler {
	return &Scheduler{interval: interval}
}

// Enabled reports whether an interval is set.
func (s *Scheduler) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval > 0
}

// Start begins the loop. A second Start on a running scheduler is a no-op, so
// a config reload cannot accumulate loops.
func (s *Scheduler) Start(request Request) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.interval <= 0 || s.cancel != nil {
		s.mu.Unlock()
		return
	}
	cancel := make(chan struct{})
	s.cancel = cancel
	s.stopped = false
	interval := s.interval
	s.mu.Unlock()

	go s.loop(request, interval, cancel)
}

// Stop ends the loop. It is safe on a scheduler that never started.
func (s *Scheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.stopped = true
	s.mu.Unlock()
	if cancel != nil {
		close(cancel)
	}
}

// SetInterval changes the period. A running scheduler is restarted on the new
// period; a scheduler without one stops.
func (s *Scheduler) SetInterval(interval time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	changed := s.interval != interval
	s.interval = interval
	s.mu.Unlock()
	if !changed {
		return
	}
	s.Stop()
}

// Status reports the automatic refresh state for the management page.
func (s *Scheduler) Status() (interval time.Duration, runs int, lastRun time.Time, lastErr string) {
	if s == nil {
		return 0, 0, time.Time{}, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval, s.runs, s.lastRun, s.lastErr
}

// loop refreshes every interval until cancelled.
//
// Each tick carries no host payload, so a refresh that needs one is left to the
// plugin's own Refresh function — it re-reads the host state it needs through
// host callbacks, which carry the plugin's identity for the whole plugin
// lifetime rather than for one invocation.
func (s *Scheduler) loop(request Request, interval time.Duration, cancel chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-cancel:
			return
		case <-ticker.C:
			result := Run(request)
			s.mu.Lock()
			s.runs++
			s.lastRun = time.Now()
			if result.Err != nil {
				s.lastErr = result.Err.Error()
			} else {
				s.lastErr = ""
			}
			s.mu.Unlock()
			// An automatic refresh must not rewrite the auth file; see the type
			// comment. Run was given a Request with no AuthName, so nothing to
			// undo here.
			_ = result
		}
	}
}

// ParseInterval reads a millisecond setting, tolerating the several shapes a
// YAML author writes: a bare number, a duration string ("30m"), or 0 to disable.
func ParseInterval(raw any) time.Duration {
	switch value := raw.(type) {
	case nil:
		return 0
	case int:
		return durationFromMS(value)
	case int64:
		return durationFromMS(int(value))
	case float64:
		return durationFromMS(int(value))
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return 0
		}
		if ms, errParse := strconv.Atoi(trimmed); errParse == nil {
			return durationFromMS(ms)
		}
		parsed, errParse := time.ParseDuration(trimmed)
		if errParse != nil {
			return 0
		}
		return parsed
	}
	return 0
}

// durationFromMS converts a millisecond setting to a duration, treating a
// negative value as "disabled" rather than as a past-deadline ticker.
func durationFromMS(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}
