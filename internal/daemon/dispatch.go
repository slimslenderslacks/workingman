package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/taskgraph"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// handle is the single entry-point for fsnotify events. It routes by filename:
// new directories are added to the watch set, .project.yaml events are sent to
// handleProject. Task files are deliberately *not* watched — task-lifecycle
// reactions are driven off session-end callbacks (see dispatch_lifecycle.go)
// to avoid the race where an agent writes status:success and exits before
// the daemon's session tracker sees the session end.
func (d *Daemon) handle(ev fsnotify.Event) {
	if ev.Op.Has(fsnotify.Create) && d.maybeWatchNewDir(ev.Name) {
		return
	}
	h := d.handlerFor(ev.Name)
	if h == nil {
		return
	}
	if !ev.Op.Has(fsnotify.Write) && !ev.Op.Has(fsnotify.Create) {
		return
	}
	h(ev.Name)
}

// handleProject reads the .project.yaml file, drops the event if the daemon
// wrote it (to break the fsnotify-loop), and routes based on observed state.
//
// Wired:
//   - empty file       → project agent
//   - cleanup: true    → archive agent (checked ahead of the status routing)
//   - status: ready    → planning agent
//   - status: working  → dispatch first ready task
//   - status: blocked  → wolf agent
//   - status: done     → no-op
func (d *Daemon) handleProject(path string) {
	p, err := project.Load(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		d.audit.Log("project_load_error", "path", path, "err", err.Error())
		return
	}
	if p.UpdatedBy == project.WriterDaemon {
		return
	}
	d.dispatchProject(path, p)
}

// revisitProject re-evaluates a project from disk, bypassing
// handleProject's daemon-write filter. Used by callers (session-end
// callbacks, cron firings, startup scan) that must re-dispatch even when
// the file's last writer was the daemon — typically the case right after
// our own created_at stamp save lands on disk.
func (d *Daemon) revisitProject(path string) {
	p, err := project.Load(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		d.audit.Log("project_load_error", "path", path, "err", err.Error())
		return
	}
	d.dispatchProject(path, p)
}

// dispatchProject runs the empty-check, created_at stamp, and routing
// logic. Factored out of handleProject so the cron callback can invoke it
// directly: cron-driven re-evaluations must skip handleProject's
// daemon-write filter (otherwise our own created_at stamp save, also
// written as `daemon`, would silence every subsequent cron firing).
func (d *Daemon) dispatchProject(path string, p *project.Project) {
	if p.Unpopulated() {
		// Covers both the legacy zero-byte file and the `:new` description
		// seed (a file with only `description:` set). Either way the project
		// agent runs next to fill in the rest; we return before the created_at
		// stamp, which lands when the agent later saves status:ready.
		d.audit.Log("project_unpopulated", "path", path)
		d.launchProjectRootAgent(path, agent.ProjectAgent, p)
		return
	}
	// Stamp created_at the first time we see a populated project on disk.
	// The save uses `updated_by: daemon`, which prevents the resulting
	// fsnotify event from re-entering dispatch via handleProject — but
	// cron callbacks reach this function directly and re-read the file,
	// so they still re-evaluate as expected.
	if p.CreatedAt == nil {
		now := time.Now()
		stamped := *p
		stamped.CreatedAt = &now
		if err := project.Save(path, &stamped); err != nil {
			d.audit.Log("project_save_error", "path", path, "err", err.Error())
		} else {
			d.audit.Log("project_created_stamped", "path", path, "at", now.UTC().Format(time.RFC3339))
			p = &stamped
		}
	}
	d.audit.Log("project_updated",
		"path", path,
		"status", string(p.Status),
		"writer", string(p.UpdatedBy),
	)
	// A cron schedule with no stop condition blocks the project (and launches
	// the wolf) from in here; there is nothing left for the status routing below
	// to do in that case.
	if !d.registerCronIfAny(path, p) {
		return
	}
	// A cleanup request outranks status routing, so `:cleanup` works on a
	// project sitting in any status (see project.Project.Cleanup). We return
	// instead of also running the status's normal agent: that agent would be
	// working — and committing — in the very workspace the archive agent is
	// trying to leave clean and pushed. Normal routing resumes from
	// afterArchiveSession, which revisits the project once the request flag has
	// been cleared.
	if p.Cleanup {
		d.launchArchiveAgent(path, p)
		return
	}
	switch p.Status {
	case project.StatusReady:
		d.launchProjectRootAgent(path, agent.PlanningAgent, p)
	case project.StatusWorking:
		d.dispatchNextTask(path, p)
	case project.StatusBlocked:
		// Could be set by the user, the project agent, or some other agent.
		// Daemon-driven blocks go through transitionProjectBlocked, which
		// launches wolf inline rather than relying on this fsnotify path.
		// Prefer the persisted reason — it covers daemon restarts when the
		// original transition's in-memory reason is gone.
		reason := p.BlockedReason
		if reason == "" {
			reason = "project marked blocked by " + string(p.UpdatedBy)
		}
		d.launchWolfAgent(path, p, reason)
	case project.StatusDone:
		// terminal
	}
}

// launchProjectRootAgent starts an agent that runs in the project's control
// directory (i.e. the dir holding the .project.yaml). The project, planning,
// and wolf agents all use this path — they do not need a wsp workspace.
//
// After the agent's session ends the daemon re-runs handleProject on the
// same file. This is the *only* reliable trigger for the project→planning
// and planning→working handoffs: the agent's file write almost always
// arrives while its own session is still in the session map, so the
// dispatch call it would have caused gets dedup-skipped. Re-handling on
// session end re-reads whatever the agent left behind and advances state.
func (d *Daemon) launchProjectRootAgent(projectPath string, kind agent.Kind, p *project.Project) {
	if d.runner == nil {
		return
	}
	if d.hasSession(projectPath) {
		d.audit.Log("session_skip_duplicate", "path", projectPath, "kind", kind.String())
		return
	}
	root := filepath.Dir(projectPath)
	plan := runner.Plan{
		Kind:        kind,
		WorkingDir:  root,
		ProjectPath: projectPath,
		TasksDir:    filepath.Join(root, "tasks"),
		Branch:      p.Branch,
		Repos:       workspaceReposFor(p),
		// Only the planning template reads this; the project and wolf agents
		// ignore it. Forwarded from disk so a re-plan survives a daemon restart
		// between the cron firing that requested it and the launch.
		Replan: p.Replan,
	}
	err := d.startSession(projectPath, plan, func(waitErr error) {
		// The planning and project agents each get a circuit breaker: now that
		// both run under `claude --print` and exit every turn, a session that
		// ends without advancing the project (planning still status:ready, or
		// the project agent leaving the file still unpopulated) would be
		// relaunched unbounded — a crash loop when the sandbox is broken.
		// afterPlanningSession / afterProjectSession bound the retries and
		// block the project instead. Wolf just re-evaluates.
		switch kind {
		case agent.PlanningAgent:
			d.afterPlanningSession(projectPath, waitErr)
		case agent.ProjectAgent:
			d.afterProjectSession(projectPath, waitErr)
		default:
			d.revisitProject(projectPath)
		}
	})
	// Wolf agent failure: the project is already blocked, recursing won't
	// help. Planning and project agent failures should block so the project
	// doesn't strand — planning in status:ready, the project agent in an
	// unpopulated seed the daemon would otherwise keep relaunching.
	if err != nil && (kind == agent.PlanningAgent || kind == agent.ProjectAgent) {
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("%s agent failed to start: %v", kind.String(), err))
	}
}

// afterPlanningSession is the planning agent's session-end callback and its
// crash-loop circuit breaker.
//
// A planning agent's job is to move the project off status:ready — by writing
// tasks and flipping it to working. The daemon relaunches planning whenever it
// observes status:ready, so a planning session that ends with the project
// still in status:ready is a non-productive cycle. Relaunching it unbounded is
// exactly what lets a broken environment (e.g. an acp-kit path that doesn't
// exist, so the sandbox can't be created) spin the daemon many times a second.
//
// Decision table for a finished planning session:
//   - project advanced off status:ready → productive; reset the counter and
//     dispatch whatever the agent left behind.
//   - still status:ready, non-nil waitErr → the agent process itself failed to
//     run (the acp-wrapper exited non-zero, e.g. `sbx create` failed). That is
//     an environment/config error that relaunching cannot fix, so block
//     immediately without consuming the retry budget.
//   - still status:ready, clean exit → the agent ran but produced nothing.
//     Could be transient, so retry up to maxPlanningRetries with backoff, then
//     block.
func (d *Daemon) afterPlanningSession(projectPath string, waitErr error) {
	p, err := project.Load(projectPath)
	if err != nil {
		// File gone or unreadable: nothing to relaunch or block. Clear any
		// failure record so a future recreation starts fresh.
		d.resetPlanningFailures(projectPath)
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		}
		return
	}
	if p.Status != project.StatusReady {
		// Planning advanced the project. Clear the counter and dispatch the
		// new state (revisitProject would just reload what we already have).
		// Any re-plan request is satisfied at this point: the run that consumed
		// it produced the new task graph we're about to dispatch. Leaving it set
		// would make the *next* planning run — an incremental one, triggered by a
		// human adding a task — re-plan the whole project instead.
		d.resetPlanningFailures(projectPath)
		d.dispatchProject(projectPath, d.clearReplan(projectPath, p))
		return
	}
	if waitErr != nil {
		// Hard launch failure — relaunching won't help. Block now.
		d.resetPlanningFailures(projectPath)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("planning agent could not run (session exited with error: %v); not retrying", waitErr))
		return
	}
	n := d.bumpPlanningFailures(projectPath)
	if n >= maxPlanningRetries {
		d.resetPlanningFailures(projectPath)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("planning agent made no progress after %d attempts; project still in status:ready", n))
		return
	}
	d.audit.Log("planning_retry", "path", projectPath, "attempt", fmt.Sprintf("%d", n))
	if !d.backoffPlanning(n) {
		// ctx cancelled during backoff (daemon shutting down).
		return
	}
	d.revisitProject(projectPath)
}

// afterProjectSession is the project agent's session-end callback and its
// crash-loop circuit breaker — the project-bootstrap analogue of
// afterPlanningSession.
//
// The project agent's job is to turn the `:new` description seed into a
// populated `.project.yaml` (status:ready), or, when the description isn't
// enough, to escalate by setting status:blocked with a specific question.
// Either outcome moves the project off the unpopulated (status:"") state the
// daemon routes to the project agent. A session that ends with the file still
// unpopulated made no progress, and relaunching it unbounded is what would let
// a broken environment spin the daemon.
//
// Decision table mirrors afterPlanningSession:
//   - populated (status set) → productive; reset the counter and dispatch
//     whatever the agent left behind (ready→planning, blocked→wolf).
//   - still unpopulated, non-nil waitErr → the agent process failed to run;
//     an environment error a relaunch can't fix, so block (→wolf) immediately.
//   - still unpopulated, clean exit → ran but produced nothing; retry up to
//     maxProjectRetries with backoff, then block (→wolf).
func (d *Daemon) afterProjectSession(projectPath string, waitErr error) {
	p, err := project.Load(projectPath)
	if err != nil {
		d.resetProjectFailures(projectPath)
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		}
		return
	}
	if !p.Unpopulated() {
		// The project agent populated the file (status:ready) or escalated
		// (status:blocked). Clear the counter and dispatch the new state.
		d.resetProjectFailures(projectPath)
		d.dispatchProject(projectPath, p)
		return
	}
	if waitErr != nil {
		d.resetProjectFailures(projectPath)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("project agent could not run (session exited with error: %v); not retrying", waitErr))
		return
	}
	n := d.bumpProjectFailures(projectPath)
	if n >= maxProjectRetries {
		d.resetProjectFailures(projectPath)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("project agent made no progress after %d attempts; .project.yaml still unpopulated", n))
		return
	}
	d.audit.Log("project_retry", "path", projectPath, "attempt", fmt.Sprintf("%d", n))
	if !d.backoffPlanning(n) {
		// ctx cancelled during backoff (daemon shutting down).
		return
	}
	d.revisitProject(projectPath)
}

// bumpProjectFailures / resetProjectFailures track the project agent's
// consecutive non-productive cycles, mirroring the planning counters. They key
// on the same project file path but a separate map so a project-bootstrap loop
// and a later planning loop don't share a budget.
func (d *Daemon) bumpProjectFailures(projectPath string) int {
	d.planningMu.Lock()
	defer d.planningMu.Unlock()
	d.projectFailures[projectPath]++
	return d.projectFailures[projectPath]
}

func (d *Daemon) resetProjectFailures(projectPath string) {
	d.planningMu.Lock()
	defer d.planningMu.Unlock()
	delete(d.projectFailures, projectPath)
}

// bumpPlanningFailures increments and returns the consecutive non-productive
// planning-cycle count for projectPath.
func (d *Daemon) bumpPlanningFailures(projectPath string) int {
	d.planningMu.Lock()
	defer d.planningMu.Unlock()
	d.planningFailures[projectPath]++
	return d.planningFailures[projectPath]
}

// resetPlanningFailures clears the planning-cycle failure count for
// projectPath, called whenever planning makes progress or the project goes
// away.
func (d *Daemon) resetPlanningFailures(projectPath string) {
	d.planningMu.Lock()
	defer d.planningMu.Unlock()
	delete(d.planningFailures, projectPath)
}

// backoffPlanning sleeps for a bounded, attempt-scaled delay before the next
// planning relaunch. Returns false if the daemon's context was cancelled
// during the wait (caller should abandon the relaunch).
func (d *Daemon) backoffPlanning(attempt int) bool {
	delay := time.Duration(attempt) * planningBackoffStep
	if delay > planningBackoffMax {
		delay = planningBackoffMax
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-d.ctx.Done():
		return false
	}
}

// startSession is the single launching point: build a Spec via Runner, track
// it, and arrange for onEnd to fire after it exits.
//
// Returns the Runner error (and logs session_start_error) when the launch
// itself fails. Callers decide how to recover: task and commit launches
// transition the project to blocked so it doesn't strand; the wolf launch
// only logs (we are already blocked, recursing won't help); the project
// agent launch only logs (the file is empty, there is no project state to
// block).
func (d *Daemon) startSession(key string, plan runner.Plan, onEnd func(error)) error {
	sess, err := d.runner.Start(d.ctx, plan)
	if err != nil {
		d.audit.Log("session_start_error",
			"kind", plan.Kind.String(),
			"key", key,
			"err", err.Error(),
		)
		return err
	}
	if !d.trackSession(key, sess, plan.Kind, plan.TaskName, onEnd) {
		_ = sess.Close()
	}
	return nil
}

// handleTask logs every task-file change as a `task_file_updated` audit
// entry. It is observation-only: dispatch decisions are driven off
// session-end callbacks (see dispatch_lifecycle.go), not file events,
// because the daemon's per-project session lock would otherwise race
// against an agent writing status:success immediately before exiting.
//
// Logging captures both agent-driven writes (an agent updating its task
// mid-flight or at completion) and daemon-driven writes (retry resets in
// handleTaskFailure). Both are useful audit trail.
func (d *Daemon) handleTask(path string) {
	t, err := task.Load(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		d.audit.Log("task_load_error", "path", path, "err", err.Error())
		return
	}
	d.audit.Log("task_file_updated",
		"path", path,
		"name", t.Name,
		"status", string(t.Status),
		"attempts", fmt.Sprintf("%d", t.Attempts),
	)
}

// registerCronIfAny registers the project's cron schedule with the
// scheduler. On each firing onCronFired re-evaluates the project as if a
// fresh fsnotify event had arrived — that path already encodes every
// dispatch decision the daemon makes, so cron firings get correct behaviour
// for free (planning relaunch if status=ready, next-task dispatch if
// status=working, wolf if blocked, no-op if done).
//
// Every schedule must carry a stop condition (`cron_until` or
// `cron_max_runs`), and this is where both halves of that rule are enforced:
//
//   - Stop condition already met → do not register, and Unregister anything
//     left over, so a daemon restart cannot revive an expired schedule.
//   - No stop condition at all → refuse to register and block the project so
//     the wolf agent can get a human to fix the file. We deliberately do not
//     invent a default deadline.
//
// Returns false when the caller must abandon the rest of dispatch: the block
// above already wrote status:blocked and launched the wolf, so running the
// project's normal status routing on top of it would dispatch work the file no
// longer asks for.
func (d *Daemon) registerCronIfAny(projectPath string, p *project.Project) bool {
	if d.scheduler == nil || p.Cron == "" {
		return true
	}
	if p.CronUnbounded() {
		// Nothing should be registered, but a schedule may predate the
		// requirement — clear it before blocking.
		d.scheduler.Unregister(projectPath)
		d.audit.Log("cron_missing_stop_condition",
			"path", projectPath,
			"spec", p.Cron,
			"missing", "cron_until|cron_max_runs",
		)
		if p.Status == project.StatusBlocked {
			// Already blocked (typically by this very check on an earlier
			// observation). Re-blocking would rewrite the file and re-launch
			// the wolf on every event; the status routing below sends it to the
			// wolf anyway.
			return true
		}
		d.transitionProjectBlocked(projectPath, p, fmt.Sprintf(
			"cron %q has no stop condition: add `cron_until: <RFC3339 timestamp>` "+
				"or `cron_max_runs: <int>` to .project.yaml so the schedule can "+
				"unschedule itself", p.Cron))
		return false
	}
	if reason := p.CronStopReason(); reason != "" {
		d.scheduler.Unregister(projectPath)
		d.audit.Log("cron_unscheduled", "path", projectPath, "spec", p.Cron, "reason", reason)
		return true
	}
	err := d.scheduler.Register(projectPath, p.Cron, func() { d.onCronFired(projectPath) })
	if err != nil {
		d.audit.Log("cron_register_error", "path", projectPath, "spec", p.Cron, "err", err.Error())
	}
	return true
}

// onCronFired is the scheduler callback for a project's cron schedule. It
// re-reads the project file rather than closing over the *project.Project the
// registration was made from: the file changes under a long-lived schedule
// (agents write it, the daemon stamps it), and the run counter below is a
// read-modify-write that must not be based on stale fields.
//
// `cron_max_runs: N` means N cycles that actually started, not N wake-ups, so
// the order here is entry-check → work → count → exit-check:
//
//  1. The stop condition is checked on ENTRY, against the count already on
//     disk. Already met → unregister and return without doing work, which is
//     what keeps a daemon restart from reviving an expired schedule.
//  2. requestCronReplan gets its chance to start a cycle and reports back
//     whether it did.
//  3. Only a firing that started a cycle is charged: countCronRun increments
//     and persists cron_runs (as the daemon, which self-filters in
//     handleProject so this bookkeeping write cannot itself retrigger
//     dispatch), and the stop condition is re-evaluated against the new count.
//     Unscheduling happens *after* the Nth working run, not instead of it.
//  4. A skipped firing is not counted; it logs cron_run_not_counted so a
//     schedule that is burning wake-ups without doing work is visible in the
//     audit log rather than showing up later as a mysterious shortfall.
//
// The deliberate tradeoff: because skipped firings no longer decrement the
// budget, a project wedged in a non-idle status (`working` that never
// finishes, `blocked` awaiting a human) can keep waking up past N. That is the
// intended reading — `cron_max_runs` counts work — and `cron_until` remains the
// absolute wall-clock bound for a project that never goes idle. There is
// deliberately no second, hidden cap on firings.
func (d *Daemon) onCronFired(projectPath string) {
	p, err := project.Load(projectPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		}
		return
	}
	d.audit.Log("cron_fired", "path", projectPath, "spec", p.Cron)

	// Entry check: the budget on disk may already be spent (a restart that
	// re-registered from a stale read, or a schedule left over from before the
	// counter reached the limit). Nothing left to do for this project.
	if reason := p.CronStopReason(); reason != "" {
		d.scheduler.Unregister(projectPath)
		d.audit.Log("cron_unscheduled", "path", projectPath, "spec", p.Cron, "reason", reason)
		return
	}
	// A firing on an idle project starts a fresh cycle: back to ready with a
	// re-plan requested. This is what makes a cron project recurring rather than
	// a one-shot that merely gets poked. The write lands before revisitProject so
	// the re-read below picks it up and routes to the planning agent.
	if skip := d.requestCronReplan(projectPath, p); skip != "" {
		// No cycle started, so no run is spent. The wake-up still pokes the
		// project — that recovery poll is the whole point of a firing landing on
		// a project with work in flight.
		d.audit.Log("cron_run_not_counted", "path", projectPath, "reason", skip)
		d.revisitProject(projectPath)
		return
	}
	if reason := d.countCronRun(projectPath).CronStopReason(); reason != "" {
		d.scheduler.Unregister(projectPath)
		d.audit.Log("cron_unscheduled", "path", projectPath, "spec", p.Cron, "reason", reason)
	}
	d.revisitProject(projectPath)
}

// countCronRun charges one run against the schedule's budget and returns the
// project the caller should evaluate the stop condition against.
//
// It re-reads the file immediately before the write rather than reusing the
// copy onCronFired loaded: requestCronReplan has just saved status+replan, and
// writing a pre-replan copy back would undo it. That re-read also narrows —
// but does not close — the read-modify-write race described on
// project.Project.CronRuns; an agent write landing between this Load and the
// Save is still lost, and an agent's own whole-file write can still roll the
// counter backwards.
//
// A failed save (or a file that vanished mid-firing) leaves the count where it
// was: the run isn't charged, the schedule stays registered, and the deadline
// or the next successful save stops it. Dropping the wake-up entirely would be
// the worse failure.
func (d *Daemon) countCronRun(projectPath string) *project.Project {
	p, err := project.Load(projectPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		}
		return &project.Project{}
	}
	counted := *p
	counted.CronRuns++
	if err := project.Save(projectPath, &counted); err != nil {
		d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
		return p
	}
	d.audit.Log("cron_run_counted", "path", projectPath, "runs", fmt.Sprintf("%d", counted.CronRuns))
	return &counted
}

// requestCronReplan flips an idle cron project back to `status: ready` with
// Replan set, so the firing produces a new planning cycle instead of a poke
// that a finished project has nothing to do with.
//
// "Idle" is the whole subtlety. A firing must not disturb a project that is
// mid-flight, so only `done` (the recurring case: last cycle finished, this one
// begins) and `ready` (planning hasn't run or is being retried — mark it as a
// re-plan and let it run) are rewritten. The others are deliberately left to
// their existing routing:
//
//   - working — tasks are running or waiting to run. Rewriting the status would
//     re-plan the graph out from under live task agents, and the tasks the
//     planner deleted would still be mid-commit. The firing falls through to
//     dispatchNextTask's recovery poll, which is the pre-existing behaviour, and
//     the cycle re-plans on the firing after the project reaches done.
//   - blocked — something needs a human or the wolf. Re-planning would paper
//     over the failure and lose the blocked_reason that explains it.
//
// A project being wound down is skipped for the same reason `:cleanup` outranks
// status routing in dispatchProject: the archive agent is trying to leave the
// workspace clean and pushed, and a planning run mid-cleanup would dirty it
// again. Archive means that already happened, so there is nothing to re-plan.
//
// Returns "" when a cycle actually started, and otherwise the reason it did
// not. onCronFired charges a run against `cron_max_runs` only on the "" case,
// so this outcome cannot be swallowed here: a firing that produced no cycle
// must not spend part of the budget the user asked for in cycles.
func (d *Daemon) requestCronReplan(projectPath string, p *project.Project) string {
	skip := ""
	switch {
	case p.Cleanup:
		skip = "cleanup requested"
	case p.Archive:
		skip = "project is archived"
	case p.Status != project.StatusDone && p.Status != project.StatusReady:
		skip = "status " + string(p.Status) + " is not idle"
	}
	if skip != "" {
		d.audit.Log("cron_replan_skipped", "path", projectPath, "reason", skip)
		return skip
	}
	updated := *p
	updated.Status = project.StatusReady
	updated.Replan = true
	if err := project.Save(projectPath, &updated); err != nil {
		// The request didn't persist, so this firing degrades to the plain
		// re-evaluation it used to be rather than being lost outright — and,
		// having started no cycle, it isn't charged either.
		d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
		return "re-plan request could not be saved"
	}
	d.audit.Log("cron_replan_requested", "path", projectPath, "from", string(p.Status))
	return ""
}

// clearReplan drops a satisfied re-plan request. Written as the daemon so the
// clear cannot retrigger dispatch, and returns the project the caller should go
// on to use — the updated copy on success, the original if the write failed (the
// flag stays set on disk, so the next planning run re-plans again; a duplicate
// re-plan is a far cheaper failure than a lost project state).
func (d *Daemon) clearReplan(projectPath string, p *project.Project) *project.Project {
	if !p.Replan {
		return p
	}
	updated := *p
	updated.Replan = false
	if err := project.Save(projectPath, &updated); err != nil {
		d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
		return p
	}
	d.audit.Log("replan_cleared", "path", projectPath)
	return &updated
}

// toWorkspaceRepos converts the project's repo schema into workspace.Repo
// values for downstream wsp use. We map {Org,Name} → identity
// "github.com/<org>/<name>" since wsp's registry is GitHub-keyed.
// BaseBranch is forwarded as-is so WspManager.Create can reset the feature
// branch's HEAD on first creation.
func toWorkspaceRepos(in []project.Repo) []workspace.Repo {
	out := make([]workspace.Repo, len(in))
	for i, r := range in {
		out[i] = workspace.Repo{
			Identity:   "github.com/" + r.Org + "/" + r.Name,
			BaseBranch: r.BaseBranch,
		}
	}
	return out
}

// workspaceReposFor is the full repo set for a project's workspace: its
// existing repos (cloned as-is) followed by its new_repos, flagged Create so
// the workspace manager creates them empty on the remote before cloning. Both
// land in the same wsp workspace, so agents can work in either. Every launch
// that provisions or references the branch workspace uses this so the new
// repos are present from the first provision (the planning dispatch) onward.
func workspaceReposFor(p *project.Project) []workspace.Repo {
	out := toWorkspaceRepos(p.Repos)
	for _, r := range p.NewRepos {
		out = append(out, workspace.Repo{
			Identity:   "github.com/" + r.Org + "/" + r.Name,
			BaseBranch: r.BaseBranch,
			Create:     true,
			Visibility: r.Visibility,
		})
	}
	return out
}

// dispatchNextTask loads the task graph for projectPath and decides what to
// run next. This is the single recovery point called from every angle —
// status=working observation, startup_scan, cron firings — so it must be
// idempotent and infer state purely from disk.
//
// Order of operations:
//  1. All committed → transition to done.
//  2. Any task stuck at status:success → resume its commit agent. This is
//     the recovery for an interrupted task→commit handoff (daemon restart
//     between task end and commit start, or a wolf-cycle that bypassed the
//     normal afterTaskSession path).
//  3. Any task in Ready() → launch task agent for the first.
//  4. Nothing to do → log no_ready_tasks with the total count.
func (d *Daemon) dispatchNextTask(projectPath string, p *project.Project) {
	root := filepath.Dir(projectPath)
	tasksDir := filepath.Join(root, "tasks")

	g, err := taskgraph.Load(tasksDir)
	if err != nil {
		d.audit.Log("taskgraph_error", "path", projectPath, "err", err.Error())
		return
	}
	if g.AllCommitted() {
		d.transitionProjectDone(projectPath, p)
		return
	}
	if t := firstUncommittedSuccess(g); t != nil {
		d.audit.Log("resume_pending_commit", "task", t.Name)
		d.launchCommitAgent(projectPath, p, t)
		return
	}
	ready := g.Ready()
	if len(ready) == 0 {
		d.audit.Log("no_ready_tasks",
			"path", projectPath,
			"total", fmt.Sprintf("%d", len(g.Tasks())),
		)
		return
	}
	d.launchTaskAgent(projectPath, p, ready[0])
}

// firstUncommittedSuccess returns the first task in deterministic order
// whose status is success — i.e. the task agent finished but the commit
// agent has not yet committed. nil if no such task exists.
func firstUncommittedSuccess(g *taskgraph.Graph) *task.Task {
	for _, t := range g.Tasks() {
		if t.Status == task.StatusSuccess {
			return t
		}
	}
	return nil
}

// launchTaskAgent dispatches the first ready task to a task agent in a fresh
// workspace. On session end the daemon re-reads the task file and, if the
// agent wrote status:success, launches the commit agent for the same task.
func (d *Daemon) launchTaskAgent(projectPath string, p *project.Project, t *task.Task) {
	if d.runner == nil {
		return
	}
	if d.hasSession(projectPath) {
		d.audit.Log("session_skip_duplicate", "path", projectPath, "kind", agent.TaskAgent.String())
		return
	}
	root := filepath.Dir(projectPath)
	plan := runner.Plan{
		Kind: agent.TaskAgent,
		// No WorkingDir: workspace.Manager creates one keyed on Branch.
		Branch:      p.Branch,
		Repos:       workspaceReposFor(p),
		ProjectPath: projectPath,
		TasksDir:    filepath.Join(root, "tasks"),
		// Use the path the task was loaded from — filenames may carry
		// sort prefixes ("00-register-repo.yaml") that don't match Name.
		TaskPath:    t.Path,
		TaskName:    t.Name,
		StaticMCPs:  t.StaticMCPs,
		Policies:    t.Policies,
		SaveSandbox: t.SaveSandbox,
	}
	err := d.startSession(projectPath, plan, func(error) {
		d.afterTaskSession(projectPath, plan.TaskPath, p)
	})
	if err != nil {
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("task agent failed to start for %q: %v", t.Name, err))
	}
}
