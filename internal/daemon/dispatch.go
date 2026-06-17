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
//   - status: ready    → planning agent
//   - status: working  → dispatch first ready task
//   - status: done     → no-op
//
// Still TODO: status: blocked → wolf agent (slice c.2).
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
	if p.Empty() {
		d.audit.Log("project_empty", "path", path)
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
	d.registerCronIfAny(path, p)
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
		Repos:       toWorkspaceRepos(p.Repos),
	}
	err := d.startSession(projectPath, plan, func() {
		d.revisitProject(projectPath)
	})
	// Project agent failure leaves an empty file — nothing to block.
	// Wolf agent failure: the project is already blocked, recursing won't
	// help. Planning agent failure should block so the project doesn't
	// strand in status: ready forever.
	if err != nil && kind == agent.PlanningAgent {
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("planning agent failed to start: %v", err))
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
func (d *Daemon) startSession(key string, plan runner.Plan, onEnd func()) error {
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
// scheduler. On each firing the callback re-invokes handleProject as if a
// fresh fsnotify event had arrived — that path already encodes every
// dispatch decision the daemon makes, so cron firings get correct behaviour
// for free (planning relaunch if status=ready, next-task dispatch if
// status=working, wolf if blocked, no-op if done).
func (d *Daemon) registerCronIfAny(projectPath string, p *project.Project) {
	if d.scheduler == nil || p.Cron == "" {
		return
	}
	err := d.scheduler.Register(projectPath, p.Cron, func() {
		d.audit.Log("cron_fired", "path", projectPath, "spec", p.Cron)
		d.revisitProject(projectPath)
	})
	if err != nil {
		d.audit.Log("cron_register_error", "path", projectPath, "spec", p.Cron, "err", err.Error())
	}
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
		Repos:       toWorkspaceRepos(p.Repos),
		ProjectPath: projectPath,
		TasksDir:    filepath.Join(root, "tasks"),
		// Use the path the task was loaded from — filenames may carry
		// sort prefixes ("00-register-repo.yaml") that don't match Name.
		TaskPath:   t.Path,
		TaskName:   t.Name,
		StaticMCPs: t.StaticMCPs,
		Policies:   t.Policies,
	}
	err := d.startSession(projectPath, plan, func() {
		d.afterTaskSession(projectPath, plan.TaskPath, p)
	})
	if err != nil {
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("task agent failed to start for %q: %v", t.Name, err))
	}
}
