package daemon

import (
	"context"
	"fmt"
	"path/filepath"
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
// agents. WithRunner wires it to runner.Runner so the unpopulated-project path
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

	// planningMu guards planningFailures and projectFailures, the per-project
	// counts of consecutive agent cycles that ended without advancing the
	// project — planning that didn't move off status:ready, or a project agent
	// that left the file unpopulated. They drive the planning/project circuit
	// breakers (afterPlanningSession / afterProjectSession) so an agent that
	// can't make progress — e.g. its sandbox won't build — blocks the project
	// instead of being relaunched in a tight loop.
	planningMu       sync.Mutex
	planningFailures map[string]int // keyed by project file path
	projectFailures  map[string]int // keyed by project file path

	// cleanupMu guards cleanupInFlight, the set of projects whose archive
	// (cleanup) agent has been dispatched and whose `cleanup: true` request
	// flag has not been cleared yet. See beginCleanup for why the session map
	// can't serve as this guard on its own.
	cleanupMu       sync.Mutex
	cleanupInFlight map[string]bool // keyed by project file path

	// dispatchMu guards dispatchChains, which chains fsnotify events keyed by
	// their containing directory (see dispatchEvent) so that events for the
	// same project still run in the order they arrived even though each is
	// now handled on its own goroutine. Events in different directories carry
	// no dependency and so run fully concurrently.
	dispatchMu     sync.Mutex
	dispatchChains map[string]chan struct{}

	// sessionIdleTimeout bounds how long a tracked session may go without any
	// ACP stream activity before the stranded-session reaper terminates it.
	// See reaper.go.
	sessionIdleTimeout time.Duration
}

const (
	// maxPlanningRetries is how many consecutive non-productive planning
	// cycles (agent ran, project still status:ready) the daemon tolerates
	// before blocking the project. A hard launch failure (non-nil wait
	// error) blocks immediately and does not consume a retry.
	maxPlanningRetries = 3
	// maxProjectRetries is the same tolerance for the project agent: how many
	// consecutive cycles that left .project.yaml unpopulated the daemon
	// tolerates before blocking (which summons the wolf). A hard launch
	// failure blocks immediately and does not consume a retry.
	maxProjectRetries = 3
	// planningBackoffStep / planningBackoffMax bound the delay inserted
	// before relaunching a planning agent after a non-productive cycle, so
	// even within the retry budget the daemon cannot spin.
	planningBackoffStep = 2 * time.Second
	planningBackoffMax  = 30 * time.Second
)

// sessionEntry bundles a live agent.Session with the metadata the TUI's
// sessions view needs. Stored under Daemon.sessions and exposed via
// ListSessions / WatchSessions.
//
// taskName is only set for task and commit agents — the other kinds
// (project, planning, wolf) don't operate on a single task. The TUI uses
// it to surface "what is this agent working on" in the sessions row.
type sessionEntry struct {
	sess      agent.Session
	kind      agent.Kind
	startedAt time.Time
	taskName  string
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

// WithSessionIdleTimeout overrides how long a tracked session may go without
// ACP stream activity before the stranded-session reaper terminates it. A
// non-positive value leaves the default (defaultSessionIdleTimeout).
func WithSessionIdleTimeout(timeout time.Duration) Option {
	return func(d *Daemon) {
		if timeout > 0 {
			d.sessionIdleTimeout = timeout
		}
	}
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
		roots:              roots,
		audit:              a,
		watcher:            w,
		notifier:           notify.Noop{},
		sessions:           map[string]sessionEntry{},
		planningFailures:   map[string]int{},
		projectFailures:    map[string]int{},
		cleanupInFlight:    map[string]bool{},
		dispatchChains:     map[string]chan struct{}{},
		sessionIdleTimeout: defaultSessionIdleTimeout,
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
	// Reconcile against on-disk sessions before startupScan: adopting any
	// still-running ACP session into d.sessions first means startupScan's
	// revisitProject calls see hasSession(key) == true and skip re-dispatching
	// a duplicate agent for a project a prior orch process already has a
	// live session for.
	d.reconcileSessions()
	d.startupScan()
	go d.reapLoop(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-d.watcher.Events:
			if !ok {
				return nil
			}
			// Ordering guarantee: events are dispatched concurrently across
			// directories, but two events for the same directory (e.g. two
			// rapid-fire writes to one project's .project.yaml) still run in
			// the order they were received here. See dispatchEvent.
			d.dispatchEvent(ev)
		case err, ok := <-d.watcher.Errors:
			if !ok {
				return nil
			}
			d.audit.Log("watcher_error", "err", err.Error())
		}
	}
}

// dispatchEvent runs d.handle(ev) on its own goroutine so that a slow
// dispatch for one project (typically workspace provisioning blocking inside
// runner.Runner.Start) cannot delay the Run() loop from picking up the next
// fsnotify event for an unrelated project.
//
// Ordering within a single project is preserved by chaining: events are
// keyed by their containing directory, and each event waits for the
// previously-chained event for that same key to finish before it runs. Events
// under different directories share no chain and so proceed in parallel.
func (d *Daemon) dispatchEvent(ev fsnotify.Event) {
	key := filepath.Dir(ev.Name)

	d.dispatchMu.Lock()
	prev := d.dispatchChains[key]
	done := make(chan struct{})
	d.dispatchChains[key] = done
	d.dispatchMu.Unlock()

	go func() {
		defer close(done)
		if prev != nil {
			<-prev
		}
		d.handle(ev)
		// Drop the chain entry if nothing newer has queued behind us, so a
		// project that goes quiet doesn't leave its key pinned in the map
		// forever.
		d.dispatchMu.Lock()
		if d.dispatchChains[key] == done {
			delete(d.dispatchChains, key)
		}
		d.dispatchMu.Unlock()
	}()
}

// shutdown runs when ctx is cancelled — a normal orch exit/restart as well as
// a crash-adjacent path. It must NOT tear down ACP-backed sessions: an
// acp-wrapper process is meant to keep running (and its sandbox with it) so a
// future orch process can reconnect over the session's socket. Only sessions
// that aren't ACP-backed (the legacy tmux + `sbx exec` path, and interactive
// project/wolf agents) are actually closed here — detachable ones are simply
// dropped from local tracking, exactly like a deliberate detach.
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
		if d.detachable(entry.kind) {
			d.audit.Log("session_detached", "key", key, "kind", entry.kind.String())
			delete(d.sessions, key)
			continue
		}
		if err := entry.sess.Close(); err != nil {
			d.audit.Log("session_close_error", "key", key, "err", err.Error())
		}
		delete(d.sessions, key)
	}
}

// detachable reports whether a session of this kind should survive an orch
// shutdown/restart rather than being closed: exactly the ACP-backed,
// non-interactive kinds (project, planning, task, commit), which run as a
// standalone acp-wrapper host process the daemon does not need to keep alive
// to keep running. Interactive kinds (wolf, archive) and the legacy tmux path
// (no AcpLauncher configured) are unaffected — those sessions are still
// closed on shutdown as before.
func (d *Daemon) detachable(kind agent.Kind) bool {
	return d.runner != nil && d.runner.UsesACP(kind)
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
//
// onEnd receives the session's wait error: nil on a clean exit, or the
// underlying process's exit error when the agent (or the acp-wrapper hosting
// it) terminated abnormally. Callers that can recover from a failed launch —
// notably the planning agent — inspect it; the others ignore it.
func (d *Daemon) trackSession(key string, sess agent.Session, kind agent.Kind, taskName string, onEnd func(error)) bool {
	d.sessionsMu.Lock()
	if _, ok := d.sessions[key]; ok {
		d.sessionsMu.Unlock()
		return false
	}
	d.sessions[key] = sessionEntry{
		sess:      sess,
		kind:      kind,
		startedAt: time.Now(),
		taskName:  taskName,
	}
	d.sessionsMu.Unlock()

	// Detachable (ACP-backed) sessions must not be waited on under d.ctx:
	// processSession.Wait itself closes the session (SIGTERM) the moment its
	// ctx is cancelled, so feeding it the daemon's shutdown ctx would tear
	// down the very acp-wrapper a restart is supposed to leave running —
	// regardless of what shutdown()'s own loop does. Waiting under
	// context.Background() instead means this goroutine only ends when the
	// process actually exits; it is abandoned harmlessly when the daemon's
	// own process later exits.
	waitCtx := d.ctx
	if d.detachable(kind) {
		waitCtx = context.Background()
	}

	go func() {
		waitErr := sess.Wait(waitCtx)
		d.sessionsMu.Lock()
		delete(d.sessions, key)
		d.sessionsMu.Unlock()
		fields := []string{"key", key, "name", sess.Name()}
		if waitErr != nil {
			fields = append(fields, "err", waitErr.Error())
		}
		d.audit.Log("session_ended", fields...)
		if onEnd == nil {
			return
		}
		// Skip onEnd when the daemon is shutting down. handleProject (used
		// as onEnd by project-root agents) would otherwise re-dispatch
		// against the still-on-disk state and cascade — every shutdown
		// session-close spawns another launch, which is then immediately
		// closed too, and so on until the test framework times out.
		select {
		case <-d.ctx.Done():
			return
		default:
		}
		onEnd(waitErr)
	}()
	return true
}

func (d *Daemon) hasSession(key string) bool {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	_, ok := d.sessions[key]
	return ok
}
