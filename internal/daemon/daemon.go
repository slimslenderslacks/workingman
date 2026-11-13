package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/notify"
	"github.com/slimslenderslacks/work/internal/power"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/scheduler"
)

// Daemon watches a list of root directories for changes to project.yaml,
// project.md, intake/*.md and tasks/*.yaml files, advances each project and
// task through its state machine, and dispatches the work to agent sessions.
// Build one with New and drive it with Run; the exported methods (ListSessions,
// WatchSessions, WolfSessions, Snapshot, ...) are read-only views that are safe
// to call from other goroutines while Run is going.
//
// When constructed without WithRunner the daemon is observation-only: it
// records what it *would* dispatch in the audit log but does not launch any
// agents. WithRunner wires it to runner.Runner so dispatch actually starts
// agent sessions.
type Daemon struct {
	roots     []string
	audit     *audit.Logger
	watcher   *fsnotify.Watcher
	runner    *runner.Runner
	notifier  notify.Sender
	channels  *channelNotify // nil unless WithChannels; see channels_notify.go
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
	// planningBusy (also guarded by planningMu) marks a project whose planning
	// agent has been dispatched and whose session-end callback has not run yet.
	// It is set before runner.Start — which can take real seconds provisioning
	// the worktree — so a second dispatch in that window cannot start a second
	// planner; the session map alone only covers the tracked lifetime. See
	// beginPlanning / planningInFlight.
	planningBusy map[string]bool // keyed by project file path
	// planningOffered (also guarded by planningMu) is the intake file list the
	// most recent planning run for a project was handed at launch. It is what
	// separates a request that arrived mid-run, which is owed a follow-up run
	// (see unofferedIntake), from one a run was given and left unprocessed,
	// which must not relaunch planning in a loop. Kept after the run ends; the
	// next launch overwrites it.
	planningOffered map[string][]string // keyed by project file path

	// cleanupMu guards cleanupInFlight, the set of projects whose archive
	// (cleanup) agent has been dispatched and whose `cleanup: true` request
	// flag has not been cleared yet. See beginCleanup for why the session map
	// can't serve as this guard on its own.
	cleanupMu       sync.Mutex
	cleanupInFlight map[string]bool // keyed by project file path

	// reviewMu guards reviewFixCycles, the count of *consecutive* PR-review
	// cycles in which the review agent produced fix work (flipped the project
	// reviewing→working) without the PR ever coming back clean. It is the
	// runaway guard for the PR-resolution loop: incremented each time a review
	// cycle creates fix tasks, reset the moment a poll finds the PR clean (or the
	// loop ends), and once it exceeds maxReviewFixCycles the project is blocked
	// for the wolf. In-memory on purpose — a daemon restart charitably resets it,
	// and it must not penalise a long-lived PR that is merely being polled while
	// idle (idle polls never touch this counter).
	reviewMu        sync.Mutex
	reviewFixCycles map[string]int    // keyed by project file path
	reviewErrors    map[string]int    // consecutive review-agent crashes, keyed by project file path
	reviewFinger    map[string]string // last PR fingerprint reconciled, keyed by project file path
	reviewBackoff   map[string]int    // adaptive poll-interval index (into reviewPollSpecs), keyed by project file path
	// reviewFingerprintFn, when set, overrides the GitHub MCP PR fingerprint
	// used by the change-detection gate. Nil in production (uses the real MCP
	// probe, review_mcp.go); tests set it to exercise the gate without a
	// network/gateway dependency.
	reviewFingerprintFn func(projectPath string, p *project.Project) (string, bool)
	// reposWithCommitsFn, when set, overrides the git probe that lists repos
	// whose branch has commits ahead of base (see reposWithCommits). Nil in
	// production; tests set it to avoid needing a real workspace.
	reposWithCommitsFn func(projectPath string, p *project.Project) ([]string, bool)

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

	// cronHistory is the persisted per-project cron history (last fire, missed
	// ticks); see cron_missed.go. cronStatePath is its file ("" = memory only),
	// clock is the time source (tests replace it), sleepCheckEvery the
	// wall-clock sampling period of the sleep watcher.
	cronHistory     *cronState
	cronStatePath   string
	clock           func() time.Time
	sleepCheckEvery time.Duration

	// power gates dispatching on AC power (nil = no gate); see power_gate.go.
	power           power.Source
	powerCheckEvery time.Duration
	powerMu         sync.Mutex
	powerPaused     bool
	powerDeferred   map[string]bool
	powerRevisit    func(string) // test hook; nil = revisitProject

	// snapshot is the state-snapshot publisher (see snapshot_live.go); inert
	// unless WithStateFile configured a path.
	snapshot snapshotState

	// workingman is the workingman-agent supervision state; nil unless
	// WithWorkingmanAgent. See workingman_agent.go.
	workingman *workingmanAgent

	// reviewGW is the daemon's own github-only MCP gateway for the PR
	// fingerprint check; nil unless WithReviewGateway. See review_gateway.go.
	// reviewGatewaySrc, when set, replaces it as what reviewGateway() returns
	// (test seam).
	reviewGW         *reviewGateway
	reviewGatewaySrc reviewGatewaySource
	// reviewMCPc is the daemon's one MCP session (over reviewGateway()) used
	// by the PR fingerprint probe, created on first use; guarded by
	// reviewMCPMu. See review_mcp.go.
	reviewMCPMu sync.Mutex
	reviewMCPc  *reviewMCP
}

const (
	// maxPlanningRetries is how many consecutive non-productive planning
	// cycles (agent ran, project still status:ready) the daemon tolerates
	// before blocking the project. A hard launch failure (non-nil wait
	// error) blocks immediately and does not consume a retry.
	maxPlanningRetries = 3
	// maxProjectRetries is the same tolerance for the project agent: how many
	// consecutive cycles that left project.yaml unpopulated the daemon
	// tolerates before blocking (which summons the wolf). A hard launch
	// failure blocks immediately and does not consume a retry.
	maxProjectRetries = 3
	// planningBackoffStep / planningBackoffMax bound the delay inserted
	// before relaunching a planning agent after a non-productive cycle, so
	// even within the retry budget the daemon cannot spin.
	planningBackoffStep = 2 * time.Second
	planningBackoffMax  = 30 * time.Second

	// maxReviewFixCycles bounds consecutive PR-review fix cycles that never
	// converge (each flips reviewing→working without the PR coming back clean),
	// after which the project is blocked for the wolf. It counts fix churn, not
	// wall-clock or idle polls, so a long-lived PR being watched while quiet is
	// never penalised — see Daemon.reviewFixCycles.
	maxReviewFixCycles = 10
	// maxReviewErrors bounds consecutive review-agent crashes (its process
	// failed, usually a sandbox that couldn't be created) before the project is
	// blocked so the user is told the loop is stuck rather than retrying forever.
	maxReviewErrors = 5
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

// Option configures a Daemon at construction; pass any number to New.
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
// registers each project.yaml's cron field on observation; on firing it
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

// New returns a Daemon watching roots that logs to a (both required). It
// applies opts, then validates the workingman-agent scratch dir and the state
// file (both must lie outside every root). Nothing is watched or dispatched
// until Run.
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
		planningBusy:       map[string]bool{},
		planningOffered:    map[string][]string{},
		cleanupInFlight:    map[string]bool{},
		reviewFixCycles:    map[string]int{},
		reviewErrors:       map[string]int{},
		reviewFinger:       map[string]string{},
		reviewBackoff:      map[string]int{},
		dispatchChains:     map[string]chan struct{}{},
		sessionIdleTimeout: defaultSessionIdleTimeout,
		clock:              time.Now,
		sleepCheckEvery:    cronSleepCheckEvery,
		powerCheckEvery:    defaultPowerCheckEvery,
	}
	d.snapshot.startedAt = time.Now()
	d.snapshot.pid = os.Getpid()
	d.snapshot.collector = newSnapshotCollector()
	d.snapshot.kick = make(chan struct{}, 1)
	for _, opt := range opts {
		opt(d)
	}
	if err := d.resolveWorkingman(); err != nil {
		w.Close()
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if d.cronStatePath != "" {
		if err := validateStateFile(d.cronStatePath, roots); err != nil {
			w.Close()
			return nil, fmt.Errorf("daemon: %w", err)
		}
	}
	hist, herr := loadCronState(d.cronStatePath)
	d.cronHistory = hist // usable (empty) even when the file was unreadable
	if herr != nil {
		a.Log("cron_state_load_error", "path", d.cronStatePath, "err", herr.Error())
	}
	if d.snapshot.path != "" {
		if err := validateStateFile(d.snapshot.path, roots); err != nil {
			w.Close()
			return nil, fmt.Errorf("daemon: %w", err)
		}
		// Any audit line is a hint that state moved (session start/stop,
		// project/task transitions, failures); the loop debounces the burst.
		a.OnLog(d.markSnapshotDirty)
	}
	return d, nil
}

// Run blocks until ctx is done. The watcher is closed before returning.
// Any sessions still running at shutdown are closed.
func (d *Daemon) Run(ctx context.Context) error {
	d.ctx = ctx
	defer d.shutdown()
	if d.power != nil {
		// Synchronously first, so a daemon started on battery never dispatches.
		d.checkPower(ctx)
		go d.powerWatchLoop(ctx)
	}
	if d.scheduler != nil {
		// Before Start: count the ticks missed while the daemon was down. They
		// are recorded, never fired.
		d.detectMissedAtStartup()
		d.scheduler.Start()
		go d.sleepWatchLoop(ctx)
	}
	for _, r := range d.roots {
		// Before the watch is installed, so the renames aren't seen as events.
		migrated, err := project.MigrateLegacy(r)
		for _, p := range migrated {
			d.audit.Log("project_file_migrated", "path", p)
		}
		if err != nil {
			d.audit.Log("project_file_migrate_error", "root", r, "err", err.Error())
		}
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
	// The workingman agent is supervised on its own goroutine, after the
	// reconcile above so a still-live one a prior orch left behind is adopted
	// rather than duplicated, and before the scan so a slow project dispatch
	// can't delay it (nor it them).
	d.beginWorkingmanAgent(ctx)
	// Off the startup path: minting can take a while (or fail and back off),
	// and nothing but the PR fingerprint check needs it.
	d.startReviewGateway(ctx)
	d.startupScan()
	d.launchSleepyHead()
	go d.reapLoop(ctx)
	if d.snapshot.path != "" {
		// Publish immediately so a reader never sees a stale file from a
		// previous run once we're up, then keep it fresh.
		if err := d.writeSnapshot(true); err != nil {
			d.logSnapshotError(err)
		}
		go d.snapshotLoop(ctx)
	}
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
			// rapid-fire writes to one project's project.yaml) still run in
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
// that aren't ACP-backed (the legacy terminal-tab + `sbx exec` path, and interactive
// project/wolf agents) are actually closed here — detachable ones are simply
// dropped from local tracking, exactly like a deliberate detach.
func (d *Daemon) shutdown() {
	defer d.writeFinalSnapshot()
	d.watcher.Close()
	// The MCP session lives on the review gateway: end it first.
	d.closeReviewMCP()
	if d.reviewGW != nil {
		d.reviewGW.close()
	}
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
// shutdown/restart rather than being closed: exactly the ACP-backed kinds
// (project, planning, task, commit, review, archive, and the wolf unless it
// runs on the host via WolfOnHost), which run as a standalone acp-wrapper host
// process the daemon does not need to keep alive to keep running. A
// --wolf-host wolf and the legacy terminal-tab path (no AcpLauncher configured) are
// unaffected — those sessions are still closed on shutdown as before.
//
// The workingman and sleepy-head agents are the exceptions among the ACP kinds:
// they are owned by the daemon (mounts derived from this run's state) rather
// than by any project, and reconcileSessions does not re-adopt them, so they
// stop with the daemon and the next boot starts a fresh one — for sleepy-head,
// only if something is still behind.
func (d *Daemon) detachable(kind agent.Kind) bool {
	if kind == agent.WorkingmanAgent || kind == agent.SleepyHeadAgent {
		return false
	}
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
	d.markSnapshotDirty()

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
		d.markSnapshotDirty()
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

// sessionKind returns the kind of the session tracked under key, and whether
// there is one.
func (d *Daemon) sessionKind(key string) (agent.Kind, bool) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	e, ok := d.sessions[key]
	return e.kind, ok
}
