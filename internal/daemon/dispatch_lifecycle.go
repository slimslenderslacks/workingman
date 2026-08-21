package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/taskgraph"
)

// maxTaskAttempts is the retry budget enforced against task.Attempts. A task
// that fails when Attempts has already reached this value will not be
// restarted — instead the project transitions to blocked and the wolf agent
// is launched. The bound is intentionally a const: tests seed task files
// with a near-limit Attempts value to exercise the boundary.
const maxTaskAttempts = 3

// afterTaskSession runs after a task agent's session has ended. It re-reads
// the task file and decides the next action.
//
//   - status:success   → launch commit agent
//   - status:failed    → restart (if attempts < maxTaskAttempts) or block
//   - status:running   → agent exited without writing a terminal status;
//     treated the same as failed (the agent crashed)
//   - status:ready     → same as running — agent never started
//   - status:blocked   → the agent decided to block; project blocked
//     immediately (no retry)
//   - status:committed → task agent should not commit; treat as invariant
//     violation and block
func (d *Daemon) afterTaskSession(projectPath, taskPath string, p *project.Project) {
	t, err := task.Load(taskPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("task_missing_after_session", "task", taskPath)
			d.transitionProjectBlocked(projectPath, p, "task file missing after session: "+taskPath)
			return
		}
		d.audit.Log("task_load_error", "task", taskPath, "err", err.Error())
		d.transitionProjectBlocked(projectPath, p, "task load error: "+err.Error())
		return
	}
	d.audit.Log("task_observed",
		"task", t.Name,
		"status", string(t.Status),
		"attempts", fmt.Sprintf("%d", t.Attempts),
		"after", "task-session",
	)
	switch t.Status {
	case task.StatusSuccess:
		d.launchCommitAgent(projectPath, p, t)
	case task.StatusFailed, task.StatusRunning, task.StatusReady:
		d.handleTaskFailure(projectPath, taskPath, p, t)
	case task.StatusBlocked:
		// The agent chose to block; the wolf will investigate. Keep the
		// sandbox for it to inspect (durably, across a manual relaunch).
		d.retainSandbox(taskPath, t)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("task %q reported blocked: %s", t.Name, t.BlockedReason))
	case task.StatusCommitted:
		// Task agents must not commit on their own.
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("task %q ended with status=committed without commit agent", t.Name))
	}
}

// handleTaskFailure bumps Attempts and either resets the task to ready for
// another run, or — if the retry budget is exhausted — leaves the task as
// failed and blocks the project. The task file is written by the daemon, but
// task files are not watched, so no fsnotify loop is created.
func (d *Daemon) handleTaskFailure(projectPath, taskPath string, p *project.Project, t *task.Task) {
	if t.Attempts >= maxTaskAttempts {
		d.audit.Log("task_retry_exhausted",
			"task", t.Name,
			"attempts", fmt.Sprintf("%d", t.Attempts),
		)
		// Retries are done and the project is about to block for the wolf.
		// Keep this task's sandbox so it can be inspected — the failed run's
		// wrapper already preserved it; persist the flag so it survives a
		// manual relaunch and shows up in the task file.
		d.retainSandbox(taskPath, t)
		reason := fmt.Sprintf("task %q failed after %d attempts", t.Name, t.Attempts)
		if t.FailureReason != "" {
			reason += ": " + t.FailureReason
		}
		d.transitionProjectBlocked(projectPath, p, reason)
		return
	}
	updated := *t
	updated.Attempts++
	updated.Status = task.StatusReady
	updated.FailureReason = ""
	if err := task.Save(taskPath, &updated); err != nil {
		d.audit.Log("task_save_error", "task", taskPath, "err", err.Error())
		d.transitionProjectBlocked(projectPath, p, "task save error: "+err.Error())
		return
	}
	d.audit.Log("task_retry",
		"task", updated.Name,
		"attempts", fmt.Sprintf("%d", updated.Attempts),
	)
	d.launchTaskAgent(projectPath, p, &updated)
}

// retainSandbox persists save_sandbox: true on a task whose sandbox should be
// kept for post-mortem inspection — a terminal failure (retries exhausted) or
// an agent-reported block, both of which route to the wolf agent. The wrapper
// of the run that just ended already preserved the live sandbox by reading the
// task's non-success status; this write makes that retention visible in the
// task file and durable across a manual relaunch. It is only called on terminal
// paths with no further automatic retry, so a later successful run can't inherit
// the flag and leak its sandbox. Best-effort: a save failure is logged, not
// fatal — the project is being blocked regardless. No-op if already set.
func (d *Daemon) retainSandbox(taskPath string, t *task.Task) {
	if t.SaveSandbox {
		return
	}
	t.SaveSandbox = true
	if err := task.Save(taskPath, t); err != nil {
		d.audit.Log("task_save_error", "task", taskPath, "err", err.Error())
		return
	}
	d.audit.Log("sandbox_retained", "task", t.Name)
}

// afterCommitSession runs after a commit agent's session has ended. The
// commit agent should have moved the task to status:committed. We reload the
// graph and either dispatch the next ready task or transition the project to
// done if every task is committed. Any other terminal state from the commit
// agent is treated as a hard failure — the project is blocked for the wolf
// agent to investigate.
func (d *Daemon) afterCommitSession(projectPath, taskPath string, p *project.Project) {
	t, err := task.Load(taskPath)
	if err != nil {
		d.audit.Log("task_load_error", "task", taskPath, "err", err.Error())
		d.transitionProjectBlocked(projectPath, p, "task load error after commit: "+err.Error())
		return
	}
	d.audit.Log("task_observed",
		"task", t.Name,
		"status", string(t.Status),
		"after", "commit-session",
	)
	if t.Status != task.StatusCommitted {
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("commit agent ended with task %q in status %q", t.Name, t.Status))
		return
	}

	// Stamp the completion time the first time we observe the task committed,
	// then re-save. This runs after the commit agent's session has ended, so
	// the daemon write can't race or be clobbered by the agent. A save failure
	// is non-fatal — it's only display metadata, not worth blocking the project
	// over. The TUI sorts the Tasks pane by this so tasks show in run order.
	if t.CompletedAt == nil {
		now := time.Now()
		t.CompletedAt = &now
		if err := task.Save(taskPath, t); err != nil {
			d.audit.Log("task_save_error", "task", taskPath, "err", err.Error())
		} else {
			d.audit.Log("task_completed_stamped", "task", t.Name, "at", now.UTC().Format(time.RFC3339))
		}
	}

	root := filepath.Dir(projectPath)
	tasksDir := filepath.Join(root, "tasks")
	g, err := taskgraph.Load(tasksDir)
	if err != nil {
		d.audit.Log("taskgraph_error", "path", projectPath, "err", err.Error())
		d.transitionProjectBlocked(projectPath, p, "taskgraph error: "+err.Error())
		return
	}
	if g.AllCommitted() {
		d.transitionProjectDone(projectPath, p)
		return
	}
	if next := firstUncommittedSuccess(g); next != nil {
		d.audit.Log("resume_pending_commit", "task", next.Name)
		d.launchCommitAgent(projectPath, p, next)
		return
	}
	ready := g.Ready()
	if len(ready) == 0 {
		// No ready tasks but not all committed — something is stuck (a
		// failed task with no retry budget, or a graph oddity). Block.
		d.transitionProjectBlocked(projectPath, p,
			"no ready tasks remain but project is not fully committed")
		return
	}
	d.launchTaskAgent(projectPath, p, ready[0])
}

// launchCommitAgent runs in the same workspace shape as a task agent — it
// needs the repos checked out so it can `git commit` inside each one. On
// session end the daemon re-reads the task and, if it reached committed,
// either launches the next ready task or marks the project done.
func (d *Daemon) launchCommitAgent(projectPath string, p *project.Project, t *task.Task) {
	if d.runner == nil {
		return
	}
	if d.hasSession(projectPath) {
		d.audit.Log("session_skip_duplicate", "path", projectPath, "kind", agent.CommitAgent.String())
		return
	}
	root := filepath.Dir(projectPath)
	plan := runner.Plan{
		Kind:        agent.CommitAgent,
		Branch:      p.Branch,
		Repos:       workspaceReposFor(p),
		ProjectPath: projectPath,
		TasksDir:    filepath.Join(root, "tasks"),
		TaskPath:    t.Path,
		TaskName:    t.Name,
		// Commit shares the task's per-task sandbox (same sandbox name);
		// passing the same MCPs + policies keeps the rule set consistent on
		// reuse. `sbx create` is skipped when the sandbox already exists, so
		// policies typically remain whatever the task agent's first create
		// applied — we forward them here for the case where commit happens
		// to be the first launch (retry after a crash, etc.).
		StaticMCPs:  t.StaticMCPs,
		Policies:    t.Policies,
		SaveSandbox: t.SaveSandbox,
	}
	err := d.startSession(projectPath, plan, func(error) {
		d.afterCommitSession(projectPath, plan.TaskPath, p)
	})
	if err != nil {
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("commit agent failed to start for %q: %v", t.Name, err))
	}
}

// transitionProjectDone writes the project file with status:done and
// updated_by:daemon. The daemon write self-filters in handleProject so this
// will not retrigger dispatch.
//
// A project with a live cron schedule keeps it: `done` is the resting state
// between cycles, not the end of the work stream, and the next firing is what
// flips it back to ready for a re-plan (see requestCronReplan). Unregistering
// here — which this function used to do unconditionally — made every cron
// project a one-shot, because the schedule died on the first completion and
// nothing re-registered it while the file sat untouched.
//
// What stops such a project waking up forever is its stop condition, not this
// transition: every schedule must declare `cron_until` or `cron_max_runs` (see
// registerCronIfAny), and once that trips the schedule unregisters itself. So a
// finished one-shot — no cron, or a cron that has expired — still gets dropped
// here.
func (d *Daemon) transitionProjectDone(projectPath string, p *project.Project) {
	updated := *p
	updated.Status = project.StatusDone
	if err := project.Save(projectPath, &updated); err != nil {
		d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
		return
	}
	if d.scheduler != nil {
		if recurring := updated.Cron != "" && !updated.CronExpired(); recurring {
			d.audit.Log("cron_kept_on_done", "path", projectPath, "spec", updated.Cron)
		} else {
			d.scheduler.Unregister(projectPath)
		}
	}
	d.audit.Log("project_done", "path", projectPath)
}

// transitionProjectBlocked writes status:blocked to the project file as the
// daemon (no fsnotify loop), pings the user via the configured notifier,
// and launches the wolf agent to investigate. Idempotent: calling it again
// while the wolf agent is already running is a session-dedup no-op.
func (d *Daemon) transitionProjectBlocked(projectPath string, p *project.Project, reason string) {
	updated := *p
	updated.Status = project.StatusBlocked
	updated.BlockedReason = reason
	if err := project.Save(projectPath, &updated); err != nil {
		d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
		// Even if we couldn't persist the block status, still launch wolf —
		// the in-memory state is enough to drive the agent.
	}
	d.audit.Log("project_blocked", "path", projectPath, "reason", reason)
	if err := d.notifier.Send("Project blocked", reason); err != nil {
		d.audit.Log("notify_error", "err", err.Error())
	}
	d.launchWolfAgent(projectPath, &updated, reason)
}

// launchWolfAgent starts the wolf agent in the project's control directory.
// It populates the FailedTasks slice in the Plan so the rendered prompt and
// .orch/context.yaml both surface which tasks need attention.
//
// As with the other project-root agents, the session-end callback re-runs
// handleProject so that if wolf flipped the project back to ready/working,
// the daemon picks up where things left off.
func (d *Daemon) launchWolfAgent(projectPath string, p *project.Project, reason string) {
	if d.runner == nil {
		return
	}
	// The wolf is tracked under a key distinct from the project path so it can
	// run in tandem with the project's other agents: summoning the wolf while a
	// task/commit/planning session is live must bring it up immediately rather
	// than waiting for the project's single main slot to free up. Dedup is
	// against that wolf key, so a second summon while a wolf is already running
	// is still a no-op.
	key := wolfSessionKey(projectPath)
	if d.hasSession(key) {
		d.audit.Log("session_skip_duplicate", "path", projectPath, "kind", agent.WolfAgent.String())
		return
	}
	root := filepath.Dir(projectPath)
	tasksDir := filepath.Join(root, "tasks")
	plan := runner.Plan{
		Kind:          agent.WolfAgent,
		WorkingDir:    root,
		ProjectPath:   projectPath,
		TasksDir:      tasksDir,
		Branch:        p.Branch,
		Repos:         workspaceReposFor(p),
		FailedTasks:   failedOrBlockedTaskPaths(tasksDir),
		BlockedReason: reason,
	}
	d.audit.Log("wolf_dispatch", "path", projectPath, "reason", reason)
	// Ignore start error here: the project is already blocked, recursing
	// into transitionProjectBlocked would loop. The session_start_error
	// audit entry plus the notification we already sent are enough to
	// surface the failure. onEnd revisits under the real project path (not the
	// wolf key) so post-wolf re-evaluation reloads the actual .project.yaml.
	_ = d.startSession(key, plan, func(error) {
		d.revisitProject(projectPath)
	})
}

// launchArchiveAgent starts the archive (cleanup) agent for a project whose
// `cleanup: true` request flag the daemon has just observed (see
// project.Project.Cleanup for the full contract). Unlike the other project-root
// agents it gets no WorkingDir: its job — `git status`, a final commit, a push —
// happens in the repos, so the runner provisions/resolves the project's wsp
// workspace from Branch + Repos exactly as it does for task and commit agents.
//
// Like the wolf, it is tracked under a key of its own so it can run in tandem
// with whatever the project's main slot is doing and dedup independently of it.
// Dedup is against the in-flight guard rather than that key, because the guard
// also covers the window between the session ending and the request flag being
// cleared — see beginCleanup.
//
// On session end afterArchiveSession clears the flag and resumes normal routing.
func (d *Daemon) launchArchiveAgent(projectPath string, p *project.Project) {
	if d.runner == nil {
		return
	}
	if !d.beginCleanup(projectPath) {
		d.audit.Log("session_skip_duplicate", "path", projectPath, "kind", agent.ArchiveAgent.String())
		return
	}
	root := filepath.Dir(projectPath)
	plan := runner.Plan{
		Kind: agent.ArchiveAgent,
		// No WorkingDir on purpose: the runner resolves the wsp workspace.
		Branch:      p.Branch,
		Repos:       workspaceReposFor(p),
		ProjectPath: projectPath,
		TasksDir:    filepath.Join(root, "tasks"),
	}
	d.audit.Log("archive_dispatch", "path", projectPath, "branch", p.Branch)
	if err := d.startSession(archiveSessionKey(projectPath), plan, func(waitErr error) {
		d.afterArchiveSession(projectPath, waitErr)
	}); err != nil {
		// Nothing started, so afterArchiveSession will never run to release the
		// guard — release it here. `cleanup: true` deliberately stays on disk:
		// the request is still outstanding, so the next project event (or a
		// daemon restart) retries it. We don't block the project either; a
		// cleanup that couldn't start shouldn't derail the work itself. The
		// session_start_error entry startSession logged carries the reason.
		d.endCleanup(projectPath)
	}
}

// afterArchiveSession is the archive agent's session-end callback. It re-reads
// the project file and does two things, in this order:
//
//  1. Clears the `cleanup: true` request flag, written as the daemon. This is
//     the crash guard for the whole feature: while the flag is set every
//     dispatch of this project launches the archive agent, so leaving it set
//     would relaunch in a loop. It is cleared whether the agent succeeded (it
//     set `archive: true`) or not — an agent that gave up should not be
//     restarted on the next unrelated file event; the user re-issues `:cleanup`.
//  2. Revisits the project so normal status routing resumes.
//
// The order matters. revisitProject deliberately bypasses handleProject's
// daemon-write filter and re-reads from disk, so it must not run until the
// cleared flag has landed — otherwise it would read the request back and
// dispatch a second archive agent. If the clear fails to persist we skip the
// revisit entirely for the same reason.
//
// An agent that ended without setting `archive: true` is recorded, not
// punished: the project is left exactly as it was (`:archive` already refuses
// to archive a project without the flag), so there is nothing here worth
// blocking over.
func (d *Daemon) afterArchiveSession(projectPath string, waitErr error) {
	// Held until every step below is done, so a late .project.yaml event — the
	// agent's own `archive: true` write, typically — can't slip in and launch a
	// second agent while we are still clearing the request.
	defer d.endCleanup(projectPath)

	p, err := project.Load(projectPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		}
		return
	}
	if !p.Archive {
		reason := "session ended without the agent setting archive: true"
		if waitErr != nil {
			reason = fmt.Sprintf("archive session exited with error: %v", waitErr)
		}
		d.audit.Log("archive_incomplete", "path", projectPath, "reason", reason)
	}
	if p.Cleanup {
		cleared := *p
		cleared.Cleanup = false
		if err := project.Save(projectPath, &cleared); err != nil {
			d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
			return
		}
		d.audit.Log("cleanup_flag_cleared",
			"path", projectPath,
			"archive", fmt.Sprintf("%t", p.Archive),
		)
	}
	d.revisitProject(projectPath)
}

// beginCleanup reserves the archive slot for projectPath, returning false if a
// cleanup run is already in flight (the caller should treat its launch as a
// duplicate). endCleanup releases it.
//
// The reservation spans from the launch until the request flag has been cleared
// — strictly wider than the session's lifetime, which is why it, and not
// hasSession(archiveSessionKey(...)), is the dedup check. The session entry is
// removed the moment the agent exits, leaving a window in which a .project.yaml
// event still carrying `cleanup: true` (the agent's own write of `archive:
// true`, delivered late) would dispatch a second archive agent. Guarding the
// whole span is what makes one `:cleanup` produce exactly one archive session.
func (d *Daemon) beginCleanup(projectPath string) bool {
	d.cleanupMu.Lock()
	defer d.cleanupMu.Unlock()
	if d.cleanupInFlight[projectPath] {
		return false
	}
	d.cleanupInFlight[projectPath] = true
	return true
}

func (d *Daemon) endCleanup(projectPath string) {
	d.cleanupMu.Lock()
	defer d.cleanupMu.Unlock()
	delete(d.cleanupInFlight, projectPath)
}

// archiveSessionKey is the session-map key for a project's archive (cleanup)
// agent — the same trick as wolfSessionKey, with an "#archive" marker. It keeps
// the archive agent out of the single slot the project's other agents share, so
// a cleanup requested mid-flight starts immediately instead of queueing behind
// them. As with the wolf marker it is appended to the final path element, not
// added as a path segment, so filepath.Dir(key) still yields the project dir
// ListSessions labels the sessions pane from.
func archiveSessionKey(projectPath string) string {
	return projectPath + "#archive"
}

// wolfSessionKey is the session-map key for a project's wolf agent. It appends
// a "#wolf" marker to the project path so the wolf occupies a slot separate
// from the one every other agent (project/planning/task/commit) shares under
// the bare project path — that separation is what lets the wolf run alongside
// them. The marker is appended to the final path element rather than as a new
// path segment, so filepath.Dir(key) still yields the project directory that
// ListSessions relies on to label the sessions pane.
func wolfSessionKey(projectPath string) string {
	return projectPath + "#wolf"
}

// failedOrBlockedTaskPaths returns absolute paths to every task in tasksDir
// whose current status is failed or blocked. Returns nil if the dir is
// unreadable — the wolf agent can still operate from the project file and
// audit log alone.
func failedOrBlockedTaskPaths(tasksDir string) []string {
	g, err := taskgraph.Load(tasksDir)
	if err != nil || g.Empty() {
		return nil
	}
	var out []string
	for _, t := range g.Tasks() {
		if t.Status == task.StatusFailed || t.Status == task.StatusBlocked {
			out = append(out, filepath.Join(tasksDir, t.Name+".yaml"))
		}
	}
	return out
}
