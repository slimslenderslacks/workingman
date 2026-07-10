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
		Repos:       toWorkspaceRepos(p.Repos),
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
		StaticMCPs: t.StaticMCPs,
		Policies:   t.Policies,
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
// updated_by:daemon, then unregisters its cron schedule so a finished
// project does not keep waking up. The daemon write self-filters in
// handleProject so this will not retrigger dispatch.
func (d *Daemon) transitionProjectDone(projectPath string, p *project.Project) {
	updated := *p
	updated.Status = project.StatusDone
	if err := project.Save(projectPath, &updated); err != nil {
		d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
		return
	}
	if d.scheduler != nil {
		d.scheduler.Unregister(projectPath)
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
		Repos:         toWorkspaceRepos(p.Repos),
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
