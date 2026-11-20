package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// The tests in this file are docs/parallel-task-worktrees.md phase 6's
// worktree GC: the W7 sweep's decision table (one test per case, driving
// sweepWorktrees directly over real git), its place in the startup order
// (S6, X7), the reaper's interval, and the git-version preflight (R8).

// sweepDaemon builds a worktree-enabled daemon over the env without running
// it, for direct sweepWorktrees calls.
func (e *wtEnv) sweepDaemon() *Daemon {
	e.t.Helper()
	a := audit.New(e.buf)
	d, err := newDaemon(e.t, []string{e.root}, a, WithRunner(&runner.Runner{Workspaces: e.stub, Audit: a}))
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	d.ctx = context.Background()
	return d
}

func (e *wtEnv) exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (e *wtEnv) branchExists(repo, name string) bool {
	e.t.Helper()
	ok, err := worktree.Manager{}.BranchExists(context.Background(), filepath.Join(e.shared, repo), worktree.BranchName(wtBranch, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
}

// integrate fast-forwards the shared branch onto name's task branch in repo
// and returns the new tip.
func (e *wtEnv) integrate(repo, name string) string {
	e.t.Helper()
	dir := filepath.Join(e.shared, repo)
	e.git(dir, "merge", "-q", "--ff-only", worktree.BranchName(wtBranch, name))
	return e.git(dir, "rev-parse", "HEAD")
}

// W7 step 2, "task file missing": a deleted task's worktree that holds nothing
// beyond its base is removed, branch and all.
func TestSweepTaskMissingCleanRemoved(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("gone", task.StatusRunning, nil)
	if err := os.Remove(e.taskPath("gone")); err != nil {
		t.Fatal(err)
	}
	path := e.saveProject()
	d := e.sweepDaemon()
	d.sweepWorktrees(path)

	if e.exists(worktree.Root(e.shared, "gone")) || e.branchExists("gateway", "gone") {
		t.Errorf("deleted task's clean worktree or branch survived the sweep")
	}
	if s := e.buf.String(); !strings.Contains(s, "task_worktree_gc") || !strings.Contains(s, "reason=task_missing") {
		t.Errorf("no task_worktree_gc reason=task_missing:\n%s", s)
	}
}

// "Task file missing" with committed work not on the shared branch: kept,
// logged as task_worktree_orphaned once, not on every sweep.
func TestSweepTaskMissingUnintegratedKept(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("gone", task.StatusSuccess, map[string]string{"gateway": "work"})
	if err := os.Remove(e.taskPath("gone")); err != nil {
		t.Fatal(err)
	}
	path := e.saveProject()
	d := e.sweepDaemon()
	d.sweepWorktrees(path)
	d.sweepWorktrees(path)

	wt := worktree.Path(e.shared, "gone", "gateway")
	if got, err := os.ReadFile(filepath.Join(wt, "gone.txt")); err != nil || strings.TrimSpace(string(got)) != "work" {
		t.Errorf("unintegrated work lost: %q %v", got, err)
	}
	if !e.branchExists("gateway", "gone") {
		t.Error("branch of the unintegrated work deleted")
	}
	s := e.buf.String()
	if n := strings.Count(s, "task_worktree_orphaned"); n != 1 {
		t.Errorf("task_worktree_orphaned logged %d times, want 1:\n%s", n, s)
	}
	if strings.Contains(s, "task_worktree_gc ") {
		t.Errorf("sweep removed something:\n%s", s)
	}
}

// "Task file missing" with only uncommitted edits: those are not on any
// branch, so the worktree is kept.
func TestSweepTaskMissingDirtyKept(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("gone", task.StatusRunning, nil)
	if err := os.Remove(e.taskPath("gone")); err != nil {
		t.Fatal(err)
	}
	wt := worktree.Path(e.shared, "gone", "gateway")
	if err := os.WriteFile(filepath.Join(wt, "half.txt"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := e.saveProject()
	e.sweepDaemon().sweepWorktrees(path)

	if !e.exists(filepath.Join(wt, "half.txt")) {
		t.Error("uncommitted edit of a deleted task removed")
	}
	if s := e.buf.String(); !strings.Contains(s, "task_worktree_orphaned") || !strings.Contains(s, "uncommitted") {
		t.Errorf("no task_worktree_orphaned for the dirty tree:\n%s", s)
	}
}

// A task file that can't be parsed is not a deleted task: its worktree is
// left alone.
func TestSweepUnreadableTaskFileKeepsWorktree(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusRunning, nil)
	if err := os.WriteFile(e.taskPath("api"), []byte("name: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := e.saveProject()
	e.sweepDaemon().sweepWorktrees(path)
	if !e.exists(worktree.Root(e.shared, "api")) {
		t.Error("worktree removed although its task file was only unreadable")
	}
}

// W7 step 2, "committed": the cleanup after an integration was interrupted.
// The worktree and branch go, and the task's worktree: is cleared.
func TestSweepCommittedRemoved(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusCommitted, map[string]string{"gateway": "api"})
	e.integrate("gateway", "api")
	path := e.saveProject()
	e.sweepDaemon().sweepWorktrees(path)

	if e.exists(worktree.Root(e.shared, "api")) || e.branchExists("gateway", "api") {
		t.Error("committed task's leftover worktree or branch survived")
	}
	if tk := e.loadTask("api"); tk.Worktree != "" {
		t.Errorf("worktree: still %q", tk.Worktree)
	}
	if s := e.buf.String(); !strings.Contains(s, "reason=committed") {
		t.Errorf("no task_worktree_gc reason=committed:\n%s", s)
	}
}

// A committed task whose branch never reached the shared branch is a broken
// record, not a leftover: the work is kept.
func TestSweepCommittedButUnintegratedKept(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusCommitted, map[string]string{"gateway": "api"})
	path := e.saveProject()
	e.sweepDaemon().sweepWorktrees(path)

	if !e.exists(worktree.Root(e.shared, "api")) || !e.branchExists("gateway", "api") {
		t.Error("unintegrated work of a committed task removed")
	}
	if s := e.buf.String(); !strings.Contains(s, "task_worktree_kept") || !strings.Contains(s, "reason=unintegrated") {
		t.Errorf("no task_worktree_kept reason=unintegrated:\n%s", s)
	}
}

// W7 step 2, every other status: kept, dirty or not, whatever its branch says.
func TestSweepKeepsLiveStatuses(t *testing.T) {
	for _, st := range []task.Status{task.StatusReady, task.StatusRunning, task.StatusSuccess, task.StatusFailed, task.StatusBlocked} {
		t.Run(string(st), func(t *testing.T) {
			e := newWtEnv(t)
			e.seedWorktreeTask("api", st, map[string]string{"gateway": "api"})
			// Even a branch already on the shared branch is kept: the task's
			// status, not its branch, says whether it is done.
			e.integrate("gateway", "api")
			path := e.saveProject()
			e.sweepDaemon().sweepWorktrees(path)
			if !e.exists(worktree.Path(e.shared, "api", "gateway")) || !e.branchExists("gateway", "api") {
				t.Errorf("%s task's worktree or branch removed", st)
			}
			if s := e.buf.String(); strings.Contains(s, "task_worktree_gc ") {
				t.Errorf("sweep removed something:\n%s", s)
			}
		})
	}
}

// A task with a live session is never touched, committed or not: its
// afterCommitSession may be finishing it.
func TestSweepSkipsLiveSession(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusCommitted, map[string]string{"gateway": "api"})
	e.integrate("gateway", "api")
	path := e.saveProject()
	d := e.sweepDaemon()
	sess := newStubSession("commit-api")
	if !d.trackSession(commitSessionKey(path), sess, agent.CommitAgent, "api", func(error) {}) {
		t.Fatal("trackSession")
	}
	t.Cleanup(func() { _ = sess.Close() })
	d.sweepWorktrees(path)
	if !e.exists(worktree.Root(e.shared, "api")) {
		t.Error("worktree of a task with a live session removed")
	}
}

// W7 step 1: a worktree directory deleted by hand leaves a git admin entry,
// which the sweep prunes. The task (ready) keeps its branch for re-attach.
func TestSweepPrunesMissingWorktreeKeepsBranch(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusReady, map[string]string{"gateway": "api"})
	if err := os.RemoveAll(worktree.Root(e.shared, "api")); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(e.shared, "gateway")
	if !strings.Contains(e.git(repo, "worktree", "list", "--porcelain"), "prunable") {
		t.Fatal("no prunable admin entry to start from")
	}
	path := e.saveProject()
	e.sweepDaemon().sweepWorktrees(path)
	if out := e.git(repo, "worktree", "list", "--porcelain"); strings.Contains(out, ".worktrees") {
		t.Errorf("admin entry not pruned:\n%s", out)
	}
	if !e.branchExists("gateway", "api") {
		t.Error("ready task's branch deleted")
	}
}

// W7 step 3: a committed task's branch with no worktree (a crash between
// `worktree remove` and `branch -D`) is deleted; a failed task's is kept for
// W4 to re-attach.
func TestSweepBranchlessRefs(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("done", task.StatusCommitted, map[string]string{"gateway": "done"})
	e.integrate("gateway", "done")
	e.seedWorktreeTask("again", task.StatusFailed, map[string]string{"gateway": "again"})
	repo := filepath.Join(e.shared, "gateway")
	for _, name := range []string{"done", "again"} {
		e.git(repo, "worktree", "remove", "--force", worktree.Path(e.shared, name, "gateway"))
		if err := os.RemoveAll(worktree.Root(e.shared, name)); err != nil {
			t.Fatal(err)
		}
	}
	path := e.saveProject()
	e.sweepDaemon().sweepWorktrees(path)

	if e.branchExists("gateway", "done") {
		t.Error("committed task's branchless ref survived")
	}
	if !e.branchExists("gateway", "again") {
		t.Error("failed task's branch deleted")
	}
	if s := e.buf.String(); !strings.Contains(s, "reason=committed_ref") {
		t.Errorf("no task_worktree_gc reason=committed_ref:\n%s", s)
	}
}

// .worktrees/.stale/ is never swept.
func TestSweepLeavesStaleDir(t *testing.T) {
	e := newWtEnv(t)
	e.provision()
	stale := filepath.Join(e.shared, worktree.DirName, worktree.StaleDirName, "api-1", "gateway")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	path := e.saveProject()
	e.sweepDaemon().sweepWorktrees(path)
	if !e.exists(stale) {
		t.Error(".stale/ swept")
	}
}

// startupScan sweeps after releaseOrphanedTasks and before the first pass
// (S6, X7): a deleted task's leftover is collected before anything is
// dispatched, and the orphaned running task, released to ready first, keeps
// its worktree and is re-dispatched into it.
func TestStartupSweepOrder(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("gone", task.StatusRunning, nil)
	if err := os.Remove(e.taskPath("gone")); err != nil {
		t.Fatal(err)
	}
	e.seedWorktreeTask("api", task.StatusRunning, nil)
	e.script(agent.TaskAgent, setStatus(e.taskPath("api"), "success"))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))
	e.saveProject()
	e.start()
	snap := e.wait("project_idle")

	released := strings.Index(snap, "task_released")
	gc := strings.Index(snap, "task_worktree_gc")
	reused := strings.Index(snap, "worktree_reused")
	if released < 0 || gc < 0 || reused < 0 || !(released < gc && gc < reused) {
		t.Errorf("want task_released < task_worktree_gc < worktree_reused, got %d %d %d:\n%s", released, gc, reused, snap)
	}
	if e.exists(worktree.Root(e.shared, "gone")) {
		t.Error("deleted task's worktree survived startup")
	}
}

// A crash during `git worktree add` leaves a task worktree half-created:
// repo alpha checked out (with an uncommitted edit), beta's branch cut but
// its directory gone, gamma never reached. After a restart the task is
// released, the sweep keeps what is there and prunes beta's admin entry, and
// the dispatch completes the worktree: alpha reused as it is, beta
// re-attached, gamma cut.
func TestRestartCompletesHalfCreatedWorktree(t *testing.T) {
	e := newWtEnv(t)
	e.repos = []string{"alpha", "beta", "gamma"}
	e.provision()
	res, err := worktree.Manager{}.Ensure(context.Background(), worktree.Spec{
		Shared: e.shared, Task: "api", Branch: worktree.BranchName(wtBranch, "api"),
		SharedBranch: wtBranch, Repos: []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	alphaBase := res.Repos[0].Base
	if err := os.WriteFile(filepath.Join(worktree.Path(e.shared, "api", "alpha"), "half.txt"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(worktree.Path(e.shared, "api", "beta")); err != nil {
		t.Fatal(err)
	}
	mustSaveTask(t, e.taskPath("api"), &task.Task{
		Name: "api", Status: task.StatusRunning, Worktree: res.Root, TaskBranch: res.Branch,
		Base: []task.Commit{{Repo: "alpha", Hash: alphaBase}},
	})
	probe := filepath.Join(t.TempDir(), "probe")
	e.script(agent.TaskAgent, fmt.Sprintf(
		"cat alpha/half.txt > %[1]q\ngit -C beta rev-parse --abbrev-ref HEAD >> %[1]q\ngit -C gamma rev-parse --abbrev-ref HEAD >> %[1]q\n%[2]s",
		probe, setStatus(e.taskPath("api"), "success")))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))
	e.saveProject()
	e.start()
	snap := e.wait("project_idle")

	got, err := os.ReadFile(probe)
	want := "half\n" + res.Branch + "\n" + res.Branch + "\n"
	if err != nil || string(got) != want {
		t.Errorf("task saw %q (%v), want %q", got, err, want)
	}
	if !strings.Contains(snap, "worktree_reused") {
		t.Errorf("not re-dispatched into its worktree:\n%s", snap)
	}
	if strings.Contains(snap, "worktree_stale_moved") || strings.Contains(snap, "task_worktree_gc ") {
		t.Errorf("part of the half-created worktree was discarded:\n%s", snap)
	}
	if tk := e.loadTask("api"); tk.Status != task.StatusCommitted || tk.Attempts != 0 {
		t.Errorf("api = %s attempts %d, want committed with no attempt spent", tk.Status, tk.Attempts)
	}
}

// The reaper runs the sweep on its own interval, not only at startup.
func TestReaperRunsWorktreeSweep(t *testing.T) {
	e := newWtEnv(t)
	e.opts = []Option{WithWorktreeSweepInterval(50 * time.Millisecond)}
	e.provision()
	e.saveProject()
	e.start()
	// Left after startup: only the periodic sweep can collect it.
	e.seedWorktreeTask("gone", task.StatusRunning, nil)
	if err := os.Remove(e.taskPath("gone")); err != nil {
		t.Fatal(err)
	}
	e.wait("task_worktree_gc")
	if e.exists(worktree.Root(e.shared, "gone")) {
		t.Error("worktree still on disk")
	}
}

// R8: with worktrees enabled, Run refuses to start on a git older than
// worktree.MinGitVersion, or with no usable git at all.
func TestGitPreflightRefusesOldGit(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  worktree.RunFunc
	}{
		{"old", func(context.Context, string, ...string) ([]byte, error) { return []byte("git version 2.20.1\n"), nil }},
		{"missing", func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("exec: git: not found") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newWtEnv(t)
			d := e.sweepDaemon()
			d.worktrees = worktree.Manager{Run: tc.run}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := d.Run(ctx); err == nil || !strings.Contains(err.Error(), "git preflight") {
				t.Errorf("Run = %v, want a git preflight error", err)
			}
			if s := e.buf.String(); !strings.Contains(s, "git_preflight_failed") || strings.Contains(s, "watch_root") {
				t.Errorf("want git_preflight_failed before anything is watched:\n%s", s)
			}
		})
	}
}

// The real git passes, and is logged; a daemon without git-backed workspaces
// never asks.
func TestGitPreflightPassesAndSkips(t *testing.T) {
	e := newWtEnv(t)
	e.start()
	if s := e.buf.String(); !strings.Contains(s, "git_preflight version=") {
		t.Errorf("no git_preflight line:\n%s", s)
	}

	buf := &safeBuf{}
	d, err := newDaemon(t, []string{t.TempDir()}, audit.New(buf))
	if err != nil {
		t.Fatal(err)
	}
	d.worktrees = worktree.Manager{Run: func(context.Context, string, ...string) ([]byte, error) {
		t.Error("git run without worktrees enabled")
		return nil, errors.New("no")
	}}
	d.ctx = context.Background()
	if err := d.gitPreflight(); err != nil {
		t.Errorf("gitPreflight = %v without worktrees", err)
	}
}
