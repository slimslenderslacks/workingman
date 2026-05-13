package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/notify"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// setupRunnable wires a daemon with the standard test stack: stub workspace
// manager, tmux launcher on an isolated socket, recording notifier, and a
// command that just sleeps for 1s. Returns the audit buffer, the project
// root the test should drop files into, the notifier recorder, and a
// teardown the test can rely on via t.Cleanup.
func setupRunnable(t *testing.T) (buf *safeBuf, root string, rec *notify.Recorder) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root = t.TempDir()
	stubRoot := t.TempDir()
	socket := fmt.Sprintf("orch-fail-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	buf = &safeBuf{}
	a := audit.New(buf)
	rec = &notify.Recorder{}
	r := &runner.Runner{
		Workspaces: workspace.NewStub(stubRoot),
		Launcher: &agent.TmuxLauncher{
			Socket:       socket,
			PollInterval: 50 * time.Millisecond,
		},
		Audit:   a,
		Command: func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 1"} },
	}
	d, err := New([]string{root}, a, WithRunner(r), WithNotifier(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })

	if ok, snap := waitFor(t, buf, "watch_root"); !ok {
		t.Fatalf("daemon never ready: %s", snap)
	}
	return buf, root, rec
}

func TestTaskFailureRetries(t *testing.T) {
	buf, root, _ := setupRunnable(t)

	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	taskPath := filepath.Join(tasksDir, "flaky.yaml")
	mustSaveTask(t, taskPath, &task.Task{
		Name:     "flaky",
		Status:   task.StatusReady,
		Attempts: 0,
	})

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "test",
		Branch:      "feat/retry",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	// First task agent starts.
	expectKindSession(t, buf, "task", 1)

	// Simulate the agent reporting failure.
	mustSaveTask(t, taskPath, &task.Task{
		Name:          "flaky",
		Status:        task.StatusFailed,
		Attempts:      0,
		FailureReason: "synthetic test failure",
	})

	// Daemon should bump Attempts → 1 and reset to ready, then launch a new task agent.
	expectKindSession(t, buf, "task", 2)

	// Verify the persisted state mid-flight.
	reloaded, err := task.Load(taskPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", reloaded.Attempts)
	}
	if reloaded.Status != task.StatusReady {
		t.Errorf("status = %q, want ready", reloaded.Status)
	}
	if reloaded.FailureReason != "" {
		t.Errorf("failure_reason should be cleared on retry, got %q", reloaded.FailureReason)
	}

	if !strings.Contains(buf.String(), "task_retry") {
		t.Errorf("expected task_retry in audit:\n%s", buf.String())
	}
}

func TestTaskFailureExhaustsRetriesAndBlocks(t *testing.T) {
	buf, root, rec := setupRunnable(t)

	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	taskPath := filepath.Join(tasksDir, "doomed.yaml")
	// Seed at the retry limit so the very next failure blocks.
	mustSaveTask(t, taskPath, &task.Task{
		Name:     "doomed",
		Status:   task.StatusReady,
		Attempts: 3,
	})

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "test",
		Branch:      "feat/doomed",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	expectKindSession(t, buf, "task", 1)

	// Fail the run. With Attempts already at the limit, daemon must block.
	mustSaveTask(t, taskPath, &task.Task{
		Name:          "doomed",
		Status:        task.StatusFailed,
		Attempts:      3,
		FailureReason: "still broken",
	})

	if ok, snap := waitForWithin(t, buf, "project_blocked", 6*time.Second); !ok {
		t.Fatalf("project never blocked.\naudit:\n%s", snap)
	}
	expectKindSession(t, buf, "wolf", 1)

	// Notification fired.
	calls := rec.Calls()
	if len(calls) == 0 {
		t.Fatalf("notifier never called")
	}
	if !strings.Contains(calls[0].Message, "doomed") {
		t.Errorf("notification message missing task name: %+v", calls[0])
	}

	// Project file persisted as blocked, written by the daemon.
	reloaded, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != project.StatusBlocked {
		t.Errorf("project status = %q, want blocked", reloaded.Status)
	}
	if reloaded.UpdatedBy != project.WriterDaemon {
		t.Errorf("project updated_by = %q, want daemon", reloaded.UpdatedBy)
	}

	// Audit recorded the retry exhaustion.
	if !strings.Contains(buf.String(), "task_retry_exhausted") {
		t.Errorf("expected task_retry_exhausted in audit:\n%s", buf.String())
	}

	// No further task or commit agents launched after the block.
	snap := buf.String()
	if got := strings.Count(snap, "session_started"); got != 2 {
		// 1 task + 1 wolf = 2.
		t.Errorf("session_started count = %d, want 2.\naudit:\n%s", got, snap)
	}
}

func TestTaskBlockedStatusBlocksProject(t *testing.T) {
	buf, root, rec := setupRunnable(t)

	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	taskPath := filepath.Join(tasksDir, "self-block.yaml")
	mustSaveTask(t, taskPath, &task.Task{
		Name:   "self-block",
		Status: task.StatusReady,
	})

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "test",
		Branch:      "feat/self-block",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	expectKindSession(t, buf, "task", 1)

	// Agent explicitly blocks itself — no retry, project goes blocked immediately.
	mustSaveTask(t, taskPath, &task.Task{
		Name:          "self-block",
		Status:        task.StatusBlocked,
		BlockedReason: "needs human input",
	})

	if ok, snap := waitForWithin(t, buf, "project_blocked", 6*time.Second); !ok {
		t.Fatalf("project never blocked.\naudit:\n%s", snap)
	}
	expectKindSession(t, buf, "wolf", 1)

	if len(rec.Calls()) == 0 {
		t.Fatalf("notifier never called")
	}
	if !strings.Contains(buf.String(), "needs human input") {
		t.Errorf("blocked_reason not propagated through audit/notify:\n%s", buf.String())
	}
}
