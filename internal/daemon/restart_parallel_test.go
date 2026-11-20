package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// The tests in this file are docs/parallel-task-worktrees.md X7's restart
// list: what a daemon starting over a project left mid-flight by its
// predecessor adopts, replays and resumes. The first group adopts live
// sessions (reconcileSessions); the second starts a real daemon over task
// files and worktrees seeded the way an earlier daemon would have left them.

// startup runs the startup steps for the env's project, in Run's order:
// reconcile adopted sessions, then the per-project scan.
func (e *schedEnv) startup() {
	e.t.Helper()
	e.d.reconcileSessions()
	e.d.releaseOrphanedTasks(e.projectPath)
	e.d.resumeOrphanedOutcomes(e.projectPath)
	e.pass()
}

func TestReconcileAdoptsTwoTaskSessionsUnderTaskKeys(t *testing.T) {
	e := newSchedEnv(t, 2)
	e.addTask("a", []string{"gateway/a/**"})
	e.addTask("b", []string{"gateway/b/**"})
	e.setTask("a", func(t *task.Task) { t.Status = task.StatusRunning })
	e.setTask("b", func(t *task.Task) { t.Status = task.StatusRunning })
	e.adopt(agent.TaskAgent, "a")
	e.adopt(agent.TaskAgent, "b")
	e.startup()

	for _, n := range []string{"a", "b"} {
		if !e.d.hasSession(taskSessionKey(e.projectPath, n)) {
			t.Errorf("%s not adopted under its task key", n)
		}
		if tk := e.task(n); tk.Status != task.StatusRunning {
			t.Errorf("%s = %s, want running (its agent survived)", n, tk.Status)
		}
	}
	if n := e.d.tokensInUse(); n != 2 {
		t.Errorf("tokens = %d, want 2 re-taken", n)
	}
	if n := e.lch.count(agent.TaskAgent); n != 0 {
		t.Errorf("startup launched %d duplicate task agents", n)
	}
}

func TestReconcileAdoptsCommitSessionRetakesCommitBusy(t *testing.T) {
	e := newSchedEnv(t, 2)
	e.addTask("a", nil)
	e.addTask("b", nil)
	e.setTask("a", func(t *task.Task) { t.Status = task.StatusSuccess })
	e.setTask("b", func(t *task.Task) { t.Status = task.StatusSuccess })
	id := e.adopt(agent.CommitAgent, "a")
	e.startup()

	if !e.d.hasSession(commitSessionKey(e.projectPath)) || !e.d.commitReserved(e.projectPath) {
		t.Fatal("adopted commit session not under #commit with the reservation re-taken")
	}
	if n := e.lch.count(agent.CommitAgent); n != 0 {
		t.Fatalf("a second integration started beside the adopted one")
	}
	// The adopted integration finishes; the queue moves on to b.
	e.setTask("a", func(t *task.Task) { t.Status = task.StatusCommitted })
	e.endAdopted(id)
	e.eventually("b integrating", func() bool { return e.integrating() == "b" })
	if tk := e.task("a"); tk.CompletedAt == nil {
		t.Error("the adopted commit session's end did not run afterCommitSession")
	}
}

func TestAdoptedTaskSessionEndRoutesThroughAfterTaskSession(t *testing.T) {
	e := newSchedEnv(t, 2)
	e.addTask("a", []string{"gateway/a/**"})
	e.addTask("b", []string{"gateway/b/**"})
	for _, n := range []string{"a", "b"} {
		e.setTask(n, func(t *task.Task) {
			t.Status = task.StatusRunning
			t.TaskBranch = worktree.BranchName("feat/proj", t.Name)
		})
	}
	ida := e.adopt(agent.TaskAgent, "a")
	idb := e.adopt(agent.TaskAgent, "b")
	e.startup()

	// Both finish while adopted: a succeeds, b fails.
	e.setTask("a", func(t *task.Task) { t.Status = task.StatusSuccess })
	e.setTask("b", func(t *task.Task) { t.Status = task.StatusFailed })
	e.endAdopted(ida)
	e.endAdopted(idb)

	e.eventually("a queued and integrating", func() bool { return e.integrating() == "a" })
	if tk := e.task("a"); tk.SucceededAt == nil {
		t.Error("a's success while adopted was not stamped")
	}
	e.eventually("b retried", func() bool { return e.lch.count(agent.TaskAgent) == 1 && eqStrings(e.running(), "b") })
	if tk := e.task("b"); tk.Attempts != 1 {
		t.Errorf("b attempts = %d, want 1 (a retry)", tk.Attempts)
	}
	if n := e.d.tokensInUse(); n != 1 {
		t.Errorf("tokens = %d, want 1 (b's retry)", n)
	}
}

// seedWorktreeTask creates name's worktree and branch the way a dispatch
// would, commits edits (repo → content of <name>.txt) on the branch, and
// writes the task file at status with worktree, task_branch and base set.
func (e *wtEnv) seedWorktreeTask(name string, status task.Status, edits map[string]string) {
	e.t.Helper()
	e.provision()
	res, err := worktree.Manager{}.Ensure(context.Background(), worktree.Spec{
		Shared: e.shared, Task: name, Branch: worktree.BranchName(wtBranch, name),
		SharedBranch: wtBranch, Repos: e.repos,
	})
	if err != nil {
		e.t.Fatalf("Ensure: %v", err)
	}
	var base []task.Commit
	for _, rr := range res.Repos {
		base = append(base, task.Commit{Repo: rr.Repo, Hash: rr.Base})
	}
	for repo, content := range edits {
		dir := worktree.Path(e.shared, name, repo)
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(content+"\n"), 0o644); err != nil {
			e.t.Fatal(err)
		}
		e.git(dir, "add", "-A")
		e.git(dir, append(strings.Fields(gitID), "commit", "-q", "-m", name)...)
	}
	mustSaveTask(e.t, e.taskPath(name), &task.Task{
		Name: name, Status: status, Worktree: res.Root, TaskBranch: res.Branch, Base: base,
	})
}

// TestRestartResumesHalfPublishedIntegration: the commit agent fast-forwarded
// repo alpha and died before beta, while no daemon was watching. The new
// daemon finds the task at success and integrates it again from the top:
// alpha is already published and still recorded, beta lands.
func TestRestartResumesHalfPublishedIntegration(t *testing.T) {
	e := newWtEnv(t)
	e.repos = []string{"alpha", "beta"}
	e.seedWorktreeTask("api", task.StatusSuccess, map[string]string{"alpha": "a", "beta": "b"})
	e.git(filepath.Join(e.shared, "alpha"), "merge", "-q", "--ff-only", worktree.BranchName(wtBranch, "api"))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))
	e.saveProject()
	e.start()
	e.wait("project_idle")

	tk := e.loadTask("api")
	if tk.Status != task.StatusCommitted || formatCommits(tk.Commits) == "" || len(tk.Commits) != 2 {
		t.Fatalf("api = %s commits %v, want committed with both repos", tk.Status, tk.Commits)
	}
	if got := e.git(filepath.Join(e.shared, "beta"), "show", "HEAD:api.txt"); got != "b" {
		t.Errorf("beta not published: %q", got)
	}
	if tk.SucceededAt == nil {
		t.Error("the restart did not stamp succeeded_at on the orphaned success")
	}
}

func TestRestartAbortsInterruptedRebase(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusSuccess, map[string]string{"gateway": "api"})
	// The shared branch moved on, and a commit agent died mid-rebase onto it.
	sharedRepo := filepath.Join(e.shared, "gateway")
	if err := os.WriteFile(filepath.Join(sharedRepo, "other.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git(sharedRepo, "add", "-A")
	e.git(sharedRepo, append(strings.Fields(gitID), "commit", "-q", "-m", "other")...)
	wt := worktree.Path(e.shared, "api", "gateway")
	_, _ = execSh(wt, fmt.Sprintf("git %s rebase -q --exec false %s", gitID, wtBranch))
	if _, err := os.Stat(filepath.Join(e.git(wt, "rev-parse", "--absolute-git-dir"), "rebase-merge")); err != nil {
		t.Fatalf("could not leave a rebase in progress: %v", err)
	}
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))
	e.saveProject()
	e.start()
	snap := e.wait("project_idle")
	if !strings.Contains(snap, "integration_rebase_aborted") {
		t.Errorf("the interrupted rebase was not aborted:\n%s", snap)
	}
	if got := e.git(sharedRepo, "show", "HEAD:api.txt"); got != "api" {
		t.Errorf("api not integrated: %q", got)
	}
}

func TestRestartBlocksOnDirtySharedTree(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusSuccess, map[string]string{"gateway": "api"})
	if err := os.WriteFile(filepath.Join(e.shared, "gateway", "stray.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	projectPath := e.saveProject()
	e.start()
	e.wait("project_blocked")
	if r := e.blockedReason(projectPath); !strings.Contains(r, "uncommitted changes") {
		t.Errorf("blocked_reason = %q", r)
	}
	if n := e.count(agent.CommitAgent); n != 0 {
		t.Errorf("a commit agent ran over a dirty shared tree")
	}
}

func TestRestartFinalizesCommittedTaskWithLeftoverWorktree(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusCommitted, map[string]string{"gateway": "api"})
	sharedRepo := filepath.Join(e.shared, "gateway")
	e.git(sharedRepo, "merge", "-q", "--ff-only", worktree.BranchName(wtBranch, "api"))
	tip := e.git(sharedRepo, "rev-parse", "HEAD")
	if err := task.Update(e.taskPath("api"), func(t *task.Task) bool {
		t.Commits = []task.Commit{{Repo: "gateway", Hash: tip}}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	e.saveProject()
	e.start()
	snap := e.wait("project_idle")
	tk := e.loadTask("api")
	if tk.Worktree != "" || tk.CompletedAt == nil {
		t.Errorf("api worktree %q completed_at %v, want cleared and stamped", tk.Worktree, tk.CompletedAt)
	}
	if _, err := os.Stat(worktree.Root(e.shared, "api")); !os.IsNotExist(err) {
		t.Errorf("worktree still on disk: %v", err)
	}
	if !strings.Contains(snap, "worktree_removed") {
		t.Errorf("no worktree_removed:\n%s", snap)
	}
}

func TestRestartReplaysConflictOutcome(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusFailed, map[string]string{"gateway": "api"})
	if err := task.Update(e.taskPath("api"), func(t *task.Task) bool {
		t.Integration = &task.Integration{Result: integrationConflict, Repo: "gateway", Files: []string{"api.txt"}}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	e.script(agent.TaskAgent, setStatus(e.taskPath("api"), "success"))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))
	e.saveProject()
	e.start()
	snap := e.wait("project_idle")
	if !strings.Contains(snap, "cause=conflict") {
		t.Errorf("the conflict was not replayed as a re-run:\n%s", snap)
	}
	if tk := e.loadTask("api"); tk.Attempts != 1 || tk.Status != task.StatusCommitted {
		t.Errorf("api = %s attempts %d, want committed after one re-run", tk.Status, tk.Attempts)
	}
}

func TestRestartOrphanedRunningTaskReusesWorktree(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("api", task.StatusRunning, nil)
	wt := worktree.Path(e.shared, "api", "gateway")
	if err := os.WriteFile(filepath.Join(wt, "half.txt"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(t.TempDir(), "probe")
	e.script(agent.TaskAgent, fmt.Sprintf("cat gateway/half.txt > %q\n%s", probe, setStatus(e.taskPath("api"), "success")))
	e.script(agent.CommitAgent, e.integrateScript("api", integrateOpts{}))
	e.saveProject()
	e.start()
	snap := e.wait("project_idle")
	if !strings.Contains(snap, "worktree_reused") || !strings.Contains(snap, "dirty=true") {
		t.Errorf("not re-dispatched into its dirty worktree:\n%s", snap)
	}
	if got, err := os.ReadFile(probe); err != nil || strings.TrimSpace(string(got)) != "half" {
		t.Errorf("the re-run did not see the uncommitted edit: %q %v", got, err)
	}
	if tk := e.loadTask("api"); tk.Attempts != 0 {
		t.Errorf("attempts = %d, want 0 (a lost session costs none)", tk.Attempts)
	}
}

// A task the previous daemon ran in the shared tree (no task_branch, D7)
// finishes there and commits the legacy way, and a second task, independent
// by its hints and with max_parallel_tasks 2, does not start beside it: it
// waits, then gets a worktree of its own.
func TestRestartLegacyInFlightTaskUsesSharedTree(t *testing.T) {
	e := newWtEnv(t)
	e.limit = 2
	e.provision()
	sharedRepo := filepath.Join(e.shared, "gateway")
	if err := os.WriteFile(filepath.Join(sharedRepo, "old.txt"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTouches, newTouches := []string{"gateway/old.txt"}, []string{"gateway/new/**"}
	mustSaveTask(t, e.taskPath("old"), &task.Task{Name: "old", Status: task.StatusRunning, Touches: &oldTouches})
	mustSaveTask(t, e.taskPath("other"), &task.Task{Name: "other", Status: task.StatusReady, Touches: &newTouches})
	mark := t.TempDir()
	e.script(agent.TaskAgent, fmt.Sprintf("pwd > %q/old\necho done >> old.txt 2>/dev/null || echo done >> gateway/old.txt\n%s", mark, setStatus(e.taskPath("old"), "success")))
	e.script(agent.CommitAgent, fmt.Sprintf("git -C %[1]q add -A\ngit -C %[1]q %[2]s commit -q -m old\n%[3]s", sharedRepo, gitID, setStatus(e.taskPath("old"), "committed")))
	e.script(agent.TaskAgent, fmt.Sprintf("pwd > %q/other\nmkdir -p gateway/new && echo n > gateway/new/f.txt\n%s", mark, setStatus(e.taskPath("other"), "success")))
	e.script(agent.CommitAgent, e.integrateScript("other", integrateOpts{}))
	e.saveProject()
	e.start()
	snap := e.wait("project_idle")

	if !strings.Contains(snap, "worktree_skipped_legacy") {
		t.Errorf("the legacy task did not stay in the shared tree:\n%s", snap)
	}
	if tk := e.loadTask("old"); tk.TaskBranch != "" || tk.Status != task.StatusCommitted {
		t.Errorf("old = %s branch %q, want committed the legacy way", tk.Status, tk.TaskBranch)
	}
	if got := e.git(sharedRepo, "show", "HEAD:new/f.txt"); got != "n" {
		t.Errorf("other not integrated: %q", got)
	}
	if tk := e.loadTask("other"); tk.TaskBranch == "" {
		t.Errorf("other ran without a worktree")
	}
	// other started only after old committed.
	oldDone := strings.Index(snap, "task_completed_stamped task=old")
	otherRan := strings.Index(snap, "task_running task=other")
	if oldDone < 0 || otherRan < oldDone {
		t.Errorf("other ran beside the legacy task (old done at %d, other ran at %d):\n%s", oldDone, otherRan, snap)
	}
}
