package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// TestPushTaskDispatchesCommitAgent verifies the review-loop routing: a ready
// pr-push task has no code work, so the daemon sends it straight to the commit
// agent (which publishes the accumulated commit-only fixes), never a task agent.
func TestPushTaskDispatchesCommitAgent(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	root := t.TempDir()
	socket := fmt.Sprintf("orch-push-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pushPath := filepath.Join(tasksDir, "publish.yaml")
	mustSaveTask(t, pushPath, &task.Task{
		Name:   "publish",
		Status: task.StatusReady,
		Source: &task.Source{Kind: task.SourceKindPush, PR: 1},
	})

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "push routing",
		Branch:      "feat/push",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		Workspaces: workspace.NewStub(t.TempDir()),
		Launcher:   &agent.TmuxLauncher{Socket: socket, PollInterval: 50 * time.Millisecond},
		Audit:      a,
		Command:    func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 1"} },
	}
	d, err := New([]string{root}, a, WithRunner(r))
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

	// The ready push task must dispatch a commit agent directly.
	expectKindSession(t, buf, "commit", 1)
	// And never a task agent — a push task carries no code work.
	if n := countStartedKind(buf.String(), "task"); n != 0 {
		t.Errorf("push task launched %d task agents, want 0 (it should skip the task-agent phase):\n%s", n, buf.String())
	}
}
