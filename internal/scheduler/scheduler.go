// Package scheduler wraps robfig/cron with a small key/spec model the daemon
// can use to (re)register a project's cron schedule each time a .project.yaml
// is observed. Register is idempotent on (key, spec) so the daemon's
// handleProject can call it on every observed update without churning the
// cron table.
//
// Supported spec syntax includes the standard 5-field cron expression plus
// robfig's @every shortcut (e.g. "@every 5m") — both are useful: 5-field for
// production, @every for fast-cycle tests.
package scheduler

import (
	"context"
	"fmt"
	"sync"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	cron *cron.Cron

	mu      sync.Mutex
	entries map[string]cron.EntryID
	specs   map[string]string
}

func New() *Scheduler {
	return &Scheduler{
		cron:    cron.New(),
		entries: map[string]cron.EntryID{},
		specs:   map[string]string{},
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
