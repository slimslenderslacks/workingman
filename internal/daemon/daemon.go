package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/notify"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/scheduler"
)

// Daemon watches a list of root directories for changes to .project.yaml and
// (eventually) tasks/*.yaml files, and dispatches work to claude-code agents.
//
// When constructed without WithRunner the daemon is observation-only: it
// records what it *would* dispatch in the audit log but does not launch any
// agents. WithRunner wires it to runner.Runner so the project_empty path
// actually starts a ProjectAgent session.
type Daemon struct {
	roots     []string
	audit     *audit.Logger
	watcher   *fsnotify.Watcher
	runner    *runner.Runner
	notifier  notify.Sender
	scheduler *scheduler.Scheduler
	ctx       context.Context // assigned at Run() entry; used by session goroutines

	sessionsMu sync.Mutex
	sessions   map[string]sessionEntry // keyed by project file path
}

// sessionEntry bundles a live agent.Session with the metadata the TUI's
// sessions view needs. Stored under Daemon.sessions and exposed via
// ListSessions / WatchSessions.
type sessionEntry struct {
	sess      agent.Session
	kind      agent.Kind
	startedAt time.Time
}

type Option func(*Daemon)

// WithRunner makes the daemon dispatch real agent sessions via r.
func WithRunner(r *runner.Runner) Option {
	return func(d *Daemon) { d.runner = r }
}

// WithNotifier replaces the default Noop notifier. The wolf agent path uses
// this to alert the user that a project has been blocked.
func WithNotifier(n notify.Sender) Option {
	return func(d *Daemon) { d.notifier = n }
}

// WithScheduler enables cron-driven re-evaluation of projects. The daemon
// registers each .project.yaml's cron field on observation; on firing it
// re-runs handleProject as if a fresh fsnotify event had arrived.
func WithScheduler(s *scheduler.Scheduler) Option {
	return func(d *Daemon) { d.scheduler = s }
}

func New(roots []string, a *audit.Logger, opts ...Option) (*Daemon, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("daemon: at least one root is required")
	}
	if a == nil {
		return nil, fmt.Errorf("daemon: audit logger is required")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	d := &Daemon{
		roots:    roots,
		audit:    a,
		watcher:  w,
		notifier: notify.Noop{},
		sessions: map[string]sessionEntry{},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d, nil
}

// Run blocks until ctx is done. The watcher is closed before returning.
// Any sessions still running at shutdown are closed.
func (d *Daemon) Run(ctx context.Context) error {
	d.ctx = ctx
	defer d.shutdown()
	if d.scheduler != nil {
		d.scheduler.Start()
	}
	for _, r := range d.roots {
		if err := d.addTree(r); err != nil {
			return fmt.Errorf("daemon: watch %s: %w", r, err)
		}
		d.audit.Log("watch_root", "path", r)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-d.watcher.Events:
			if !ok {
				return nil
			}
			d.handle(ev)
		case err, ok := <-d.watcher.Errors:
			if !ok {
				return nil
			}
			d.audit.Log("watcher_error", "err", err.Error())
		}
	}
}

func (d *Daemon) shutdown() {
	d.watcher.Close()
	if d.scheduler != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.scheduler.Stop(stopCtx)
	}
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	for key, entry := range d.sessions {
		if err := entry.sess.Close(); err != nil {
			d.audit.Log("session_close_error", "key", key, "err", err.Error())
		}
		delete(d.sessions, key)
	}
}

// trackSession registers sess under key and spawns a goroutine that waits for
// it to exit, removes it from the map, and (if non-nil) invokes onEnd. The
// onEnd callback runs *after* the map entry is cleared, so it is free to
// launch the next agent for the same key — task→commit and commit→next-task
// transitions are chained this way.
//
// kind is stored alongside the session so ListSessions / WatchSessions can
// surface it to the TUI without consulting the runner Plan.
//
// Returns false if a session is already running for key (caller should treat
// the new launch as a duplicate).
func (d *Daemon) trackSession(key string, sess agent.Session, kind agent.Kind, onEnd func()) bool {
	d.sessionsMu.Lock()
	if _, ok := d.sessions[key]; ok {
		d.sessionsMu.Unlock()
		return false
	}
	d.sessions[key] = sessionEntry{
		sess:      sess,
		kind:      kind,
		startedAt: time.Now(),
	}
	d.sessionsMu.Unlock()

	go func() {
		_ = sess.Wait(d.ctx)
		d.sessionsMu.Lock()
		delete(d.sessions, key)
		d.sessionsMu.Unlock()
		d.audit.Log("session_ended", "key", key, "name", sess.Name())
		if onEnd != nil {
			onEnd()
		}
	}()
	return true
}

func (d *Daemon) hasSession(key string) bool {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	_, ok := d.sessions[key]
	return ok
}
