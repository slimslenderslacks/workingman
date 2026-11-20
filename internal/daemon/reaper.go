package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/taskgraph"
	"github.com/slimslenderslacks/work/internal/worktree"
)

const (
	// defaultSessionIdleTimeout is the default for Daemon.sessionIdleTimeout:
	// how long a tracked session may stream nothing before the reaper assumes it
	// has wedged mid-turn and terminates it. Generous on purpose — a single long
	// tool call (a multi-minute build or test run) legitimately emits no ACP
	// frames, so the bound must exceed the longest such gap to avoid killing
	// healthy work. It mirrors the TUI watcher's acpTurnIdleTimeout.
	defaultSessionIdleTimeout = 20 * time.Minute

	// sessionDoneGrace is how long after an agent has declared its stage complete
	// (its task/project file reached a status terminal for the agent's kind) the
	// reaper waits for the wrapper to exit on its own before terminating it.
	// Short: once the work is recorded there is no reason for the wrapper to
	// linger, but the normal exit-when-empty path deserves a moment to fire first.
	sessionDoneGrace = 90 * time.Second

	// reapInterval is how often the reaper scans the tracked sessions.
	reapInterval = 30 * time.Second

	// defaultWorktreeSweepInterval is the default for
	// Daemon.worktreeSweepEvery: how often the reaper runs the worktree sweep
	// (docs/parallel-task-worktrees.md W7). Slower than reapInterval: the sweep
	// shells out to git for every project, and what it collects (crash
	// leftovers) costs only disk while it waits.
	defaultWorktreeSweepInterval = 5 * time.Minute
)

// reapLoop periodically reaps stranded sessions until ctx is cancelled. It runs
// as its own goroutine (started from Run) rather than inside the main select so
// a slow Close — Close blocks up to the agent's term grace — never stalls
// fsnotify event handling.
//
// It also runs the worktree sweep (sweepAllWorktrees) on its own, slower
// ticker.
func (d *Daemon) reapLoop(ctx context.Context) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	every := d.worktreeSweepEvery
	if every <= 0 {
		every = defaultWorktreeSweepInterval
	}
	sweep := time.NewTicker(every)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.reapStrandedSessions()
		case <-sweep.C:
			d.sweepAllWorktrees()
		}
	}
}

// reapStrandedSessions terminates tracked sessions whose agent has finished (or
// wedged) but whose acp-wrapper has not exited. It is the daemon's backstop for
// the wrapper's exit-when-empty path (and the TUI's turn-scoped idle watchdog)
// failing to bring a session down: a stranded session holds the project's single
// session slot indefinitely, silently stalling the pipeline — e.g. a task that
// wrote status:success never advances to its commit agent. Closing the session
// is, from the daemon's view, identical to a clean exit: it unblocks the
// trackSession wait goroutine, which logs session_ended and runs the onEnd
// callback that dispatches the next stage (commit-after-task, next task, planning
// retry, …).
func (d *Daemon) reapStrandedSessions() {
	type victim struct {
		key     string
		entry   sessionEntry
		verdict reapVerdict
	}
	var victims []victim
	d.sessionsMu.Lock()
	for key, e := range d.sessions {
		if v := d.strandedVerdict(key, e); v.reap {
			victims = append(victims, victim{key: key, entry: e, verdict: v})
		}
	}
	d.sessionsMu.Unlock()

	// Close outside the lock: Close blocks until the child exits (up to the
	// agent term grace) and the wait goroutine it unblocks re-acquires
	// sessionsMu to delete the entry — closing under the lock would deadlock.
	for _, v := range victims {
		// Record every reap with structured fields so the reasons can be
		// aggregated later to diagnose WHY sessions strand (cause=stage_complete
		// points at the wrapper's exit-when-empty path failing; cause=idle_timeout
		// points at a mid-turn agent/stream hang). The trackSession wait goroutine
		// also logs the subsequent session_ended.
		d.audit.Log("session_reaped",
			"key", v.key,
			"name", v.entry.sess.Name(),
			"kind", v.entry.kind.String(),
			"task", v.entry.taskName,
			"cause", v.verdict.cause,
			"idle", v.verdict.idle.Round(time.Second).String(),
			"age", time.Since(v.entry.startedAt).Round(time.Second).String(),
		)
		if err := v.entry.sess.Close(); err != nil {
			d.audit.Log("session_reap_error", "key", v.key, "name", v.entry.sess.Name(), "err", err.Error())
		}
	}
}

// reapVerdict is the outcome of evaluating one session for reaping. cause is a
// stable, greppable category (not free text) so reaps can be aggregated in the
// audit log to understand which failure mode is stranding sessions.
type reapVerdict struct {
	reap  bool
	cause string // "stage_complete" | "idle_timeout"
	idle  time.Duration
}

const (
	// reapCauseStageComplete: the agent recorded a terminal status for its stage
	// but its wrapper kept running past the done-grace — the exit-when-empty path
	// (or its TUI watchdog) failed to bring it down.
	reapCauseStageComplete = "stage_complete"
	// reapCauseIdleTimeout: the session produced no ACP stream activity for
	// longer than the idle bound — a mid-turn hang (e.g. a stalled model stream).
	reapCauseIdleTimeout = "idle_timeout"
)

// strandedVerdict decides whether the session for key should be reaped and why.
// A session is stranded when either its agent has declared its stage complete
// (so any further wrapper lifetime is pure zombie) and a short grace has elapsed,
// or it has produced no ACP stream activity for longer than the idle timeout.
func (d *Daemon) strandedVerdict(key string, e sessionEntry) reapVerdict {
	// Interactive sessions are never reaped by any timer. The wolf agent
	// (dispatched to unblock a stuck project) is human-driven: it has no
	// terminal-status signal (stageComplete returns false for it) and may sit
	// idle indefinitely while a human thinks or steps away. Killing it on an idle
	// timeout would silently discard that context, so it holds its slot until it
	// exits on its own. (The project and archive agents used to be interactive
	// too but now run autonomously; both have a terminal signal in
	// stageComplete.)
	if e.kind.Interactive() {
		return reapVerdict{}
	}
	// The workingman agent is always-on: it is idle by design between questions
	// (hours, overnight), has no stage that can complete, and is supervised and
	// restarted by the daemon itself. Reaping it as stale would just make the
	// supervisor relaunch it, dropping the conversation for nothing.
	if e.kind == agent.WorkingmanAgent {
		return reapVerdict{}
	}
	idle := d.sessionIdle(e)
	if d.stageComplete(key, e) {
		if idle >= sessionDoneGrace {
			return reapVerdict{reap: true, cause: reapCauseStageComplete, idle: idle}
		}
		return reapVerdict{}
	}
	if idle >= d.sessionIdleTimeout {
		return reapVerdict{reap: true, cause: reapCauseIdleTimeout, idle: idle}
	}
	return reapVerdict{}
}

// sessionIdle reports how long since the session last showed activity: the more
// recent of its start time and the last write to its ACP stream log. A session
// whose log does not exist yet (just launched, or never connected) is measured
// from startedAt, so a wrapper that dies before ever streaming is still reaped.
func (d *Daemon) sessionIdle(e sessionEntry) time.Duration {
	last := e.startedAt
	if d.runner != nil {
		if logPath, err := d.runner.SessionLogPath(e.sess.Name()); err == nil {
			if info, err := os.Stat(logPath); err == nil && info.ModTime().After(last) {
				last = info.ModTime()
			}
		}
	}
	return time.Since(last)
}

// stageComplete reports whether the agent for this session has declared its work
// done by advancing the on-disk state to a status terminal for its kind — after
// which a still-running wrapper is stranded, not working. Kinds without a single
// terminal-status signal (the interactive wolf) return false and are covered
// only by the idle timeout; so is an archive agent that could not finish.
func (d *Daemon) stageComplete(key string, e sessionEntry) bool {
	switch e.kind {
	case agent.TaskAgent, agent.CommitAgent:
		if e.taskName == "" {
			return false
		}
		g, err := taskgraph.Load(filepath.Join(filepath.Dir(key), "tasks"))
		if err != nil {
			return false
		}
		t := g.Task(e.taskName)
		if t == nil {
			return false
		}
		if e.kind == agent.CommitAgent {
			// The commit agent's job is to turn success into committed.
			return t.Status == task.StatusCommitted
		}
		// The task agent's turn is over once it records a terminal outcome.
		return t.Status == task.StatusSuccess || t.Status == task.StatusFailed
	case agent.PlanningAgent:
		// Its key carries the "#planning" marker that keeps it out of the
		// project's main slot; strip it to reach the project file.
		projectPath := strings.TrimSuffix(key, planningKeyMarker)
		p, err := project.Load(projectPath)
		if err != nil {
			return false
		}
		// Planning is done once it moves the project off ready (to working, or
		// blocked/done if it gave up) — and, for a planner run alongside tasks
		// on a project that was already working, once it has processed the
		// intake it was handed (the status alone says nothing there).
		return p.Status != project.StatusReady && !d.plannerOwesIntake(projectPath)
	case agent.ProjectAgent:
		p, err := project.Load(key)
		if err != nil {
			return false
		}
		// The project agent is done once it populates the file — status:ready
		// on success, or status:blocked if it escalated to the wolf. Either
		// way status is no longer empty (the unpopulated seed state).
		return !p.Unpopulated()
	case agent.ArchiveAgent:
		// Its key carries the "#archive" marker that keeps it out of the
		// project's main slot; strip it to reach the project file.
		p, err := project.Load(strings.TrimSuffix(key, archiveKeyMarker))
		if err != nil {
			return false
		}
		// The archive agent's whole job is to turn a dirty workspace into
		// `archive: true`. An agent that gave up never sets it and so has no
		// stage-complete signal — the idle timeout is what covers that case.
		return p.Archive
	default:
		return false
	}
}

// Reasons the worktree sweep gives in task_worktree_gc (removed) and
// task_worktree_kept / task_worktree_orphaned (kept).
const (
	sweepReasonCommitted    = "committed"     // a committed task's leftover worktree
	sweepReasonCommittedRef = "committed_ref" // a committed task's branch with no worktree
	sweepReasonTaskMissing  = "task_missing"  // no task file, and nothing unintegrated
	sweepReasonUnintegrated = "unintegrated"  // committed, but its branch is not on the shared branch
)

// sweepAllWorktrees runs sweepWorktrees for every project under the roots.
// The reaper calls it every worktreeSweepEvery.
func (d *Daemon) sweepAllWorktrees() {
	if !d.worktreesEnabled() {
		return
	}
	for _, root := range d.roots {
		_ = filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				if p != root && skipWatchDir(entry.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Base(p) == project.FileName {
				d.sweepWorktrees(p)
			}
			return nil
		})
	}
}

// sweepWorktrees is the worktree GC of docs/parallel-task-worktrees.md W7 for
// one project: it collects what crashes, interrupted cleanups and deleted
// tasks leave in the shared workspace. It runs from the reaper and once from
// startupScan, after releaseOrphanedTasks and before the first scheduling
// pass. In three steps:
//
//  1. `git worktree prune` in every repo, dropping the admin entries of
//     worktrees whose directory is gone.
//  2. Each .worktrees/<task>/ is judged by its task (sweepVerdict): a
//     committed task's is removed, so is a deleted task's that holds nothing
//     unintegrated, and every other one is kept, for the task's next dispatch
//     or for the wolf. .worktrees/.stale/ is never touched.
//  3. Each wm/<pb>/<task> branch with no worktree is deleted when its task is
//     committed, and kept otherwise (W4 re-attaches it on dispatch).
//
// It never touches a task with a live session, never removes work that is not
// on the shared branch, and runs under the project's worktreeLock so no
// launch cuts or reuses a worktree while it decides. Every removal is audited
// (task_worktree_gc); a failure is logged and left for the next sweep.
func (d *Daemon) sweepWorktrees(projectPath string) {
	if !d.worktreesEnabled() {
		return
	}
	p, err := project.Load(projectPath)
	if err != nil || p.Branch == "" || p.Archive {
		// An archived project's workspace is about to be removed whole.
		return
	}
	shared, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		return
	}
	if info, err := os.Stat(shared); err != nil || !info.IsDir() {
		return // never provisioned: nothing to collect, and nothing to create
	}

	mu := d.worktreeLock(projectPath)
	mu.Lock()
	defer mu.Unlock()

	repos := repoDirNames(p)
	var gitRepos []string
	for _, repo := range repos {
		dir := filepath.Join(shared, repo)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if err := d.worktrees.Prune(d.ctx, dir); err != nil {
			d.audit.Log("task_worktree_gc_error", "path", projectPath, "repo", repo, "err", err.Error())
			continue
		}
		gitRepos = append(gitRepos, repo)
	}

	// Read after taking the lock: a launch reads and stamps task files under
	// it too, so what is decided here is not overtaken by a dispatch.
	tasks, complete := sweepTasks(filepath.Join(filepath.Dir(projectPath), "tasks"))
	sw := &sweep{d: d, projectPath: projectPath, p: p, shared: shared, repos: repos, complete: complete}

	// Step 2: the task worktree roots.
	entries, _ := os.ReadDir(filepath.Join(shared, worktree.DirName))
	for _, e := range entries {
		if !e.IsDir() || e.Name() == worktree.StaleDirName {
			continue
		}
		sw.root(e.Name(), tasks[e.Name()])
	}

	// Step 3: task branches left without a worktree.
	prefix := worktree.BranchPrefix + p.Branch + "/"
	for _, repo := range gitRepos {
		sw.branches(repo, prefix, tasks)
	}
}

// sweep is one sweepWorktrees run over one project.
type sweep struct {
	d           *Daemon
	projectPath string
	p           *project.Project
	shared      string
	repos       []string
	// complete is false when some task file could not be read: a worktree
	// with no task is then not known to be a deleted task's, and is kept.
	complete bool
}

// sweepTasks loads the project's task files by name. complete is false when
// the directory or any .yaml file in it could not be read or parsed, so the
// caller can't tell a deleted task from an unreadable one.
func sweepTasks(tasksDir string) (map[string]*task.Task, bool) {
	tasks := map[string]*task.Task{}
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return tasks, errors.Is(err, fs.ErrNotExist)
	}
	complete := true
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		t, err := task.Load(filepath.Join(tasksDir, e.Name()))
		if err != nil {
			complete = false
			continue
		}
		if t.Name == "" {
			t.Name = strings.TrimSuffix(e.Name(), ".yaml")
		}
		tasks[t.Name] = t
	}
	return tasks, complete
}

// busy reports why the sweep must leave t alone whatever its status, or "".
// A task with a live session is in use. A committed task is left while an
// integration is in flight (its afterCommitSession may be finishing it, W6)
// and while the project is blocked (the wolf may be looking at it).
func (sw *sweep) busy(t *task.Task) bool {
	if sw.d.taskSessionLive(sw.projectPath, t.Name) {
		return true
	}
	if t.Status != task.StatusCommitted {
		return false
	}
	return sw.d.commitReserved(sw.projectPath) || sw.p.Status == project.StatusBlocked
}

// root judges one .worktrees/<name>/ directory; t is its task, nil when the
// task file is gone.
func (sw *sweep) root(name string, t *task.Task) {
	d := sw.d
	rootDir := worktree.Root(sw.shared, name)
	if d.taskSessionLive(sw.projectPath, name) {
		return
	}
	switch {
	case t == nil:
		if !sw.complete {
			return
		}
		spec := sw.spec(name, "")
		if why := sw.unintegrated(spec, true); why != "" {
			d.noteSweepKept("task_worktree_orphaned", sw.projectPath, name,
				"worktree", rootDir, "branch", spec.Branch, "why", why)
			return
		}
		sw.remove(spec, sweepReasonTaskMissing, nil)
	case t.Status == task.StatusCommitted:
		if sw.busy(t) {
			return
		}
		spec := sw.spec(name, t.TaskBranch)
		if why := sw.unintegrated(spec, false); why != "" {
			d.noteSweepKept("task_worktree_kept", sw.projectPath, name,
				"worktree", rootDir, "branch", spec.Branch, "reason", sweepReasonUnintegrated, "why", why)
			return
		}
		sw.remove(spec, sweepReasonCommitted, t)
	default:
		// ready, running, success, failed, blocked: in use, queued, or kept
		// for the next dispatch (W4, W6).
	}
}

func (sw *sweep) spec(name, branch string) worktree.Spec {
	if branch == "" {
		branch = worktree.BranchName(sw.p.Branch, name)
	}
	return worktree.Spec{Shared: sw.shared, Task: name, Branch: branch, SharedBranch: sw.p.Branch, Repos: sw.repos}
}

// unintegrated returns why the task worktree spec still holds work that is
// not on the shared branch, or "" when removing it loses nothing: in every
// repo the task branch, where it exists, is an ancestor of the shared branch.
// strict (a task whose file is gone) also requires every repo's worktree to
// be clean and on the task branch, and the root to hold nothing but those and
// .orch/: without a task file nothing says what else there might be.
func (sw *sweep) unintegrated(spec worktree.Spec, strict bool) string {
	d := sw.d
	if strict {
		entries, err := os.ReadDir(spec.Root())
		if err != nil {
			return err.Error()
		}
		for _, e := range entries {
			if e.Name() != ".orch" && !slices.Contains(spec.Repos, e.Name()) {
				return "unrecognised " + e.Name()
			}
		}
	}
	for _, repo := range spec.Repos {
		repoDir := filepath.Join(spec.Shared, repo)
		if _, err := os.Stat(repoDir); err != nil {
			continue
		}
		path := worktree.Path(spec.Shared, spec.Task, repo)
		if strict {
			if _, err := os.Lstat(path); err == nil {
				branch, registered, err := d.worktrees.CheckedOut(d.ctx, repoDir, path)
				switch {
				case err != nil:
					return err.Error()
				case !registered:
					return repo + " is not a registered worktree"
				case branch != "refs/heads/"+spec.Branch:
					return repo + " is not on " + spec.Branch
				}
				dirty, err := d.worktrees.Dirty(d.ctx, path)
				if err != nil {
					return err.Error()
				}
				if dirty {
					return repo + " has uncommitted changes"
				}
			}
		}
		exists, err := d.worktrees.BranchExists(d.ctx, repoDir, spec.Branch)
		if err != nil {
			return err.Error()
		}
		if !exists {
			continue
		}
		ok, err := d.worktrees.IsAncestor(d.ctx, repoDir, spec.Branch, spec.SharedBranch)
		if err != nil {
			return err.Error()
		}
		if !ok {
			return spec.Branch + " in " + repo + " is not on " + spec.SharedBranch
		}
	}
	return ""
}

// remove deletes the task worktree and its branches (worktree.Manager.Remove)
// and, for a task that still has a file, clears its worktree: field.
func (sw *sweep) remove(spec worktree.Spec, reason string, t *task.Task) {
	d := sw.d
	if err := d.worktrees.Remove(d.ctx, spec); err != nil {
		d.audit.Log("task_worktree_gc_error", "path", sw.projectPath, "task", spec.Task, "worktree", spec.Root(), "err", err.Error())
		return
	}
	d.audit.Log("task_worktree_gc", "path", sw.projectPath, "task", spec.Task, "worktree", spec.Root(), "reason", reason)
	d.forgetSweepKept(sw.projectPath, spec.Task)
	if t == nil || t.Worktree == "" {
		return
	}
	if err := task.Update(t.Path, func(cur *task.Task) bool {
		if cur.Worktree == "" {
			return false
		}
		cur.Worktree = ""
		return true
	}); err != nil {
		d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
	}
}

// branches is step 3 for one repo: every task branch under prefix that no
// worktree has checked out is deleted when its task is committed and its
// branch is on the shared branch. A branch whose name has a further "/"
// belongs to another project whose branch nests under this one's (W3).
func (sw *sweep) branches(repo, prefix string, tasks map[string]*task.Task) {
	d := sw.d
	repoDir := filepath.Join(sw.shared, repo)
	names, err := d.worktrees.Branches(d.ctx, repoDir, prefix)
	if err != nil {
		d.audit.Log("task_worktree_gc_error", "path", sw.projectPath, "repo", repo, "err", err.Error())
		return
	}
	if len(names) == 0 {
		return
	}
	wts, err := d.worktrees.List(d.ctx, repoDir)
	if err != nil {
		d.audit.Log("task_worktree_gc_error", "path", sw.projectPath, "repo", repo, "err", err.Error())
		return
	}
	checkedOut := map[string]bool{}
	for _, wt := range wts {
		checkedOut[wt.Branch] = true
	}
	for _, branch := range names {
		name := strings.TrimPrefix(branch, prefix)
		if strings.Contains(name, "/") || checkedOut["refs/heads/"+branch] {
			continue
		}
		t := tasks[name]
		if t == nil || t.Status != task.StatusCommitted || sw.busy(t) {
			continue // re-attached on the task's next dispatch (W4), or :archive's
		}
		if t.TaskBranch != "" && t.TaskBranch != branch {
			continue // not the branch the task file records
		}
		ok, err := d.worktrees.IsAncestor(d.ctx, repoDir, branch, sw.p.Branch)
		if err != nil || !ok {
			why := "not on " + sw.p.Branch
			if err != nil {
				why = err.Error()
			}
			d.noteSweepKept("task_worktree_kept", sw.projectPath, name,
				"repo", repo, "branch", branch, "reason", sweepReasonUnintegrated, "why", why)
			continue
		}
		if err := d.worktrees.DeleteBranch(d.ctx, repoDir, branch); err != nil {
			d.audit.Log("task_worktree_gc_error", "path", sw.projectPath, "task", name, "repo", repo, "err", err.Error())
			continue
		}
		d.audit.Log("task_worktree_gc", "path", sw.projectPath, "task", name, "repo", repo, "branch", branch, "reason", sweepReasonCommittedRef)
	}
}

// noteSweepKept logs a sweep's decision to keep a task's work (event plus
// kv), once per project and task until that changes, rather than on every
// sweep.
func (d *Daemon) noteSweepKept(event, projectPath, taskName string, kv ...string) {
	key := projectPath + "\x00" + taskName
	line := event + "\x00" + strings.Join(kv, "\x00")
	d.sweepMu.Lock()
	if d.sweepKept[key] == line {
		d.sweepMu.Unlock()
		return
	}
	d.sweepKept[key] = line
	d.sweepMu.Unlock()
	d.audit.Log(event, append([]string{"path", projectPath, "task", taskName}, kv...)...)
}

func (d *Daemon) forgetSweepKept(projectPath, taskName string) {
	d.sweepMu.Lock()
	delete(d.sweepKept, projectPath+"\x00"+taskName)
	d.sweepMu.Unlock()
}
