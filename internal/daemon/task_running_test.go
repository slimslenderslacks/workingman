package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// runRunningDaemon starts a daemon whose agents run cmd, with a working project
// holding one task seeded from seed. It returns the audit buffer and the task's
// path.
func runRunningDaemon(t *testing.T, launcher agent.Launcher, cmd string, seed *task.Task) (*safeBuf, string) {
	t.Helper()
	root := t.TempDir()
	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		Workspaces: workspace.NewStub(t.TempDir()),
		Launcher:   launcher,
		Audit:      a,
		Command:    func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", cmd} },
	}
	d, err := newDaemon(t, []string{root}, a, WithRunner(r))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if ok, snap := waitFor(t, buf, "watch_root"); !ok {
		t.Fatalf("daemon never ready: %s", snap)
	}

	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	taskPath := filepath.Join(tasksDir, seed.Name+".yaml")
	mustSaveTask(t, taskPath, seed)
	if err := project.SaveAs(filepath.Join(root, "project.yaml"), &project.Project{
		Description: "test",
		Branch:      "feat/running",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	return buf, taskPath
}

func mustLoadTask(t *testing.T, path string) *task.Task {
	t.Helper()
	got, err := task.Load(path)
	if err != nil {
		t.Fatalf("load task %s: %v", path, err)
	}
	return got
}

// TestDispatchMarksTaskRunning: the daemon itself persists ready → running when
// it launches the task agent, rather than relying on the agent to, and keeps
// every other field of the task file intact.
func TestDispatchMarksTaskRunning(t *testing.T) {
	buf, taskPath := runRunningDaemon(t, &agent.ProcessLauncher{}, "sleep 5", &task.Task{
		Name:        "work",
		Description: "do the work",
		Status:      task.StatusReady,
		Model:       "haiku",
		StaticMCPs:  []string{"github"},
	})
	expectKindSession(t, buf, "task", 1)

	got := mustLoadTask(t, taskPath)
	if got.Status != task.StatusRunning {
		t.Fatalf("status after dispatch = %q, want running", got.Status)
	}
	if got.Description != "do the work" || got.Model != "haiku" ||
		len(got.StaticMCPs) != 1 || got.StaticMCPs[0] != "github" || got.Attempts != 0 {
		t.Errorf("running write clobbered other fields: %+v", got)
	}
	if !strings.Contains(buf.String(), "task_running") {
		t.Errorf("expected task_running in audit:\n%s", buf.String())
	}
	// Running is not ready: nothing may dispatch the task a second time while
	// its agent works.
	time.Sleep(500 * time.Millisecond)
	if n := countStartedKind(buf.String(), "task"); n != 1 {
		t.Errorf("task agents started = %d, want 1:\n%s", n, buf.String())
	}
}

// TestCrashWhileRunningRetries: an agent that exits without writing a terminal
// status leaves the daemon's running mark behind, which afterTaskSession still
// treats as a crash — one attempt spent, the task relaunched (and so running
// again).
func TestCrashWhileRunningRetries(t *testing.T) {
	buf, taskPath := runRunningDaemon(t, &agent.ProcessLauncher{}, "sleep 0.3", &task.Task{
		Name:   "crashy",
		Status: task.StatusReady,
		// At the limit after one retry, so the run settles (blocked) instead of
		// retrying for the rest of the test.
		Attempts: 2,
	})
	if ok, snap := waitForWithin(t, buf, "task_retry", 6*time.Second); !ok {
		t.Fatalf("crash was not retried:\n%s", snap)
	}
	if !strings.Contains(buf.String(), "status=running") {
		t.Errorf("expected the crashed session to be observed at running:\n%s", buf.String())
	}
	if ok, snap := waitForWithin(t, buf, "task_retry_exhausted", 6*time.Second); !ok {
		t.Fatalf("retries never exhausted:\n%s", snap)
	}
	// task_retry_exhausted is logged before save_sandbox is persisted;
	// sandbox_retained is logged after, so the task file is final by then.
	if ok, snap := waitForWithin(t, buf, "sandbox_retained", 6*time.Second); !ok {
		t.Fatalf("sandbox was never retained:\n%s", snap)
	}
	got := mustLoadTask(t, taskPath)
	if got.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", got.Attempts)
	}
	if !got.SaveSandbox {
		t.Errorf("save_sandbox should be set once retries are exhausted")
	}
}

// TestLaunchFailureReleasesRunning: when the task agent can't be started the
// running mark is undone, so the task reads ready exactly as before dispatch
// (the project blocks, as it always did).
func TestLaunchFailureReleasesRunning(t *testing.T) {
	buf, taskPath := runRunningDaemon(t, &erroringLauncher{msg: "boom"}, "true", &task.Task{
		Name:   "unlaunchable",
		Status: task.StatusReady,
	})
	if ok, snap := waitForWithin(t, buf, "project_blocked", 3*time.Second); !ok {
		t.Fatalf("no project_blocked after start failure: %s", snap)
	}
	if got := mustLoadTask(t, taskPath); got.Status != task.StatusReady {
		t.Errorf("status after failed launch = %q, want ready", got.Status)
	}
	if !strings.Contains(buf.String(), "task_released") {
		t.Errorf("expected task_released in audit:\n%s", buf.String())
	}
}

// TestPushTaskMarkedRunning: a push task skips the task agent, so the commit
// agent dispatched for it is what marks it running.
func TestPushTaskMarkedRunning(t *testing.T) {
	buf, taskPath := runRunningDaemon(t, &agent.ProcessLauncher{}, "sleep 5", &task.Task{
		Name:   "publish",
		Status: task.StatusReady,
		Source: &task.Source{Kind: task.SourceKindPush, PR: 1},
	})
	expectKindSession(t, buf, "commit", 1)
	if got := mustLoadTask(t, taskPath); got.Status != task.StatusRunning {
		t.Errorf("push task status after dispatch = %q, want running", got.Status)
	}
}

// TestMarkTaskRunningOnlyMovesReady: a task that is no longer ready on disk
// (its copy in the caller is stale) is not touched.
func TestMarkTaskRunningOnlyMovesReady(t *testing.T) {
	root := t.TempDir()
	d, _, _ := newReviewDaemon(t, root)
	path := filepath.Join(root, "done.yaml")
	mustSaveTask(t, path, &task.Task{Name: "done", Status: task.StatusSuccess})
	stale := &task.Task{Name: "done", Status: task.StatusReady, Path: path}
	if d.markTaskRunning(stale) {
		t.Error("markTaskRunning reported a write for a non-ready task")
	}
	if got := mustLoadTask(t, path); got.Status != task.StatusSuccess {
		t.Errorf("status = %q, want success untouched", got.Status)
	}
}

// TestReleaseOrphanedTasks: at startup a task left at running with no adopted
// session working it goes back to ready (so the startup dispatch re-runs it),
// while one whose agent survived the restart stays running.
func TestReleaseOrphanedTasks(t *testing.T) {
	root := t.TempDir()
	d, buf, _ := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, "project.yaml")
	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	orphan := filepath.Join(tasksDir, "orphan.yaml")
	live := filepath.Join(tasksDir, "live.yaml")
	done := filepath.Join(tasksDir, "done.yaml")
	mustSaveTask(t, orphan, &task.Task{Name: "orphan", Status: task.StatusRunning, Attempts: 1})
	mustSaveTask(t, live, &task.Task{Name: "live", Status: task.StatusRunning})
	mustSaveTask(t, done, &task.Task{Name: "done", Status: task.StatusSuccess})

	d.sessions[taskSessionKey(projectPath, "live")] = sessionEntry{sess: newStubSession("x"), kind: agent.TaskAgent, taskName: "live"}
	d.releaseOrphanedTasks(projectPath)

	if got := mustLoadTask(t, orphan); got.Status != task.StatusReady || got.Attempts != 1 {
		t.Errorf("orphan = status %q attempts %d, want ready/1", got.Status, got.Attempts)
	}
	if got := mustLoadTask(t, live); got.Status != task.StatusRunning {
		t.Errorf("live = %q, want running (its agent survived)", got.Status)
	}
	if got := mustLoadTask(t, done); got.Status != task.StatusSuccess {
		t.Errorf("done = %q, want success untouched", got.Status)
	}
	if !strings.Contains(buf.String(), "task_released") {
		t.Errorf("expected task_released in audit:\n%s", buf.String())
	}
}
