package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler maps string keys to cron entries so a caller can re-register a
// key's schedule whenever it changes without duplicating entries. The zero
// value is not usable; create one with New.
type Scheduler struct {
	cron *cron.Cron

	mu      sync.Mutex
	entries map[string]cron.EntryID
	specs   map[string]string
	fns     map[string]func()
}

// New returns a Scheduler with no entries. It fires nothing until Start.
func New() *Scheduler {
	return &Scheduler{
		cron:    cron.New(),
		entries: map[string]cron.EntryID{},
		specs:   map[string]string{},
		fns:     map[string]func(){},
	}
}

// Register associates key with a schedule; subsequent firings invoke fn.
// If key is already registered with the same spec, Register is a no-op.
// If the spec differs, the old entry is removed and a new one is added.
// An invalid spec returns an error without touching the existing entry.
func (s *Scheduler) Register(key, spec string, fn func()) error {
	if key == "" {
		return fmt.Errorf("scheduler: key is required")
	}
	if spec == "" {
		return fmt.Errorf("scheduler: spec is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.specs[key]; ok && existing == spec {
		return nil
	}
	id, err := s.cron.AddFunc(spec, fn)
	if err != nil {
		return fmt.Errorf("scheduler: add %q: %w", spec, err)
	}
	if old, ok := s.entries[key]; ok {
		s.cron.Remove(old)
	}
	s.entries[key] = id
	s.specs[key] = spec
	s.fns[key] = fn
	return nil
}

// Unregister removes the schedule for key. No-op if key is not registered.
func (s *Scheduler) Unregister(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.entries[key]; ok {
		s.cron.Remove(id)
		delete(s.entries, key)
		delete(s.specs, key)
		delete(s.fns, key)
	}
}

// Rearm re-seeds every registered entry so its next-run time is computed from
// the current wall clock, keeping each key, spec and callback. It fires no
// callbacks itself: entries are removed and re-added, and a freshly added
// entry's Next is Schedule.Next(now), which is strictly in the future.
//
// Why this exists: robfig/cron v3 arms a single time.Timer for
// entries[0].Next.Sub(now), computed once when the entry set last changed.
// Go timers run on the monotonic clock, which does not advance while macOS is
// asleep (mach_absolute_time stops), so after a suspend the timer fires
// late by roughly the sleep duration, and "@every" entries (whose Next is
// derived from a monotonic-bearing time.Now()) drift the same way. Cron-spec
// entries compare Next against wall time on wake, so they are correct once the
// timer fires, but the wake itself is delayed. Calling Rearm after a detected
// clock jump re-arms the timer against the wall clock. Entries whose spec
// fails to re-parse (cannot happen for specs Register accepted) are left as-is.
func (s *Scheduler) Rearm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, spec := range s.specs {
		fn := s.fns[key]
		id, err := s.cron.AddFunc(spec, fn)
		if err != nil {
			continue
		}
		s.cron.Remove(s.entries[key])
		s.entries[key] = id
	}
}

// Spec returns the currently-registered spec for key (or empty if absent).
// Useful for tests asserting the registration state.
func (s *Scheduler) Spec(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.specs[key]
}

// Start begins firing schedules. Calling Start multiple times is safe.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop waits for in-flight callbacks to finish, bounded by ctx. Returns
// ctx.Err() if the deadline elapses before all callbacks complete; otherwise
// nil. Stop should be called from the daemon's shutdown path.
func (s *Scheduler) Stop(ctx context.Context) error {
	doneCtx := s.cron.Stop()
	select {
	case <-doneCtx.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TicksBetween counts the ticks of spec that fall in the half-open interval
// (after, upTo], using the same parser Register does. It returns the count and
// the latest such tick (zero when there are none). Counting stops at limit
// (when positive) so a dense schedule such as "@every 1s" measured across a
// long gap cannot spin; the returned count is then a lower bound, and last is
// the limit'th tick rather than the true latest.
func TicksBetween(spec string, after, upTo time.Time, limit int) (count int, last time.Time, err error) {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("scheduler: parse %q: %w", spec, err)
	}
	for t := sched.Next(after); !t.IsZero() && !t.After(upTo); t = sched.Next(t) {
		count++
		last = t
		if limit > 0 && count >= limit {
			break
		}
	}
	return count, last, nil
}
