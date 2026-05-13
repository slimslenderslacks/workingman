package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

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
	if p.Empty() {
		d.audit.Log("project_empty", "path", path)
		d.launchProjectRootAgent(path, agent.ProjectAgent, p, nil)
		return
	}
	d.audit.Log("project_updated",
		"path", path,
		"status", string(p.Status),
		"writer", string(p.UpdatedBy),
	)
	d.registerCronIfAny(path, p)
	switch p.Status {
	case project.StatusReady:
		d.launchProjectRootAgent(path, agent.PlanningAgent, p, nil)
	case project.StatusWorking:
		d.dispatchNextTask(path, p)
	case project.StatusBlocked:
		// Could be set by the user, the project agent, or some other agent.
		// Daemon-driven blocks go through transitionProjectBlocked, which
		// launches wolf inline rather than relying on this fsnotify path.
		d.launchWolfAgent(path, p, "project marked blocked by "+string(p.UpdatedBy))
	case project.StatusDone:
		// terminal
	}
}

// launchProjectRootAgent starts an agent that runs in the project's control
// directory (i.e. the dir holding the .project.yaml). The project, planning,
// and wolf agents all use this path — they do not need a wsp workspace.
//
// onEnd is invoked after the agent's session exits and the entry is cleared
// from the session map. Pass nil if no follow-up action is needed.
func (d *Daemon) launchProjectRootAgent(projectPath string, kind agent.Kind, p *project.Project, onEnd func()) {
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
	d.startSession(projectPath, plan, onEnd)
}

// startSession is the single launching point: build a Spec via Runner, track
// it, and arrange for onEnd to fire after it exits.
func (d *Daemon) startSession(key string, plan runner.Plan, onEnd func()) {
	sess, err := d.runner.Start(d.ctx, plan)
	if err != nil {
		d.audit.Log("session_start_error",
			"kind", plan.Kind.String(),
			"key", key,
			"err", err.Error(),
		)
		return
	}
	if !d.trackSession(key, sess, onEnd) {
		_ = sess.Close()
	}
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
		d.handleProject(projectPath)
	})
	if err != nil {
		d.audit.Log("cron_register_error", "path", projectPath, "spec", p.Cron, "err", err.Error())
	}
}

// toWorkspaceRepos converts the project's repo schema into workspace.Repo
// values for downstream wsp use. We map {Org,Name} → identity
// "github.com/<org>/<name>" since wsp's registry is GitHub-keyed.
func toWorkspaceRepos(in []project.Repo) []workspace.Repo {
	out := make([]workspace.Repo, len(in))
	for i, r := range in {
		out[i] = workspace.Repo{
			Identity: "github.com/" + r.Org + "/" + r.Name,
		}
	}
	return out
}

// dispatchNextTask loads the task graph for projectPath and launches a task
// agent for the first ready task. If the graph is empty, the project is
// freshly working but the planning agent hasn't written any tasks yet — we
// log and wait for a tasks/*.yaml change to re-trigger evaluation. If every
// task is already committed, we transition the project to done.
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
	ready := g.Ready()
	if len(ready) == 0 {
		d.audit.Log("no_ready_tasks", "path", projectPath, "tasks", "0")
		return
	}
	d.launchTaskAgent(projectPath, p, ready[0])
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
		TaskPath:    filepath.Join(root, "tasks", t.Name+".yaml"),
		TaskName:    t.Name,
	}
	d.startSession(projectPath, plan, func() {
		d.afterTaskSession(projectPath, plan.TaskPath, p)
	})
}
