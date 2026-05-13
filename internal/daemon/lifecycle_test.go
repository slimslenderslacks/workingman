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
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// TestHappyPathFullProject walks a populated .project.yaml all the way from
// status:working through the task→commit→committed→next chain and finally
// to status:done. Two tasks (the second depending on the first) make sure
// the DAG re-evaluation after each commit actually picks the next ready
// task instead of stopping early.
//
// All agents use `sleep 2` as their command. The test writes the simulated
// status update immediately after seeing session_started, but under the
// race detector the test goroutine can be delayed by hundreds of ms — a
// 2-second window leaves comfortable slack so the daemon's afterX callback
// always reads the updated file. Total test wall-clock is ~10s.
func TestHappyPathFullProject(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	stubRoot := t.TempDir()
	socket := fmt.Sprintf("orch-happy-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		Workspaces: workspace.NewStub(stubRoot),
		Launcher: &agent.TmuxLauncher{
			Socket:       socket,
			PollInterval: 50 * time.Millisecond,
		},
		Audit:   a,
		Command: func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 2"} },
	}
	d, err := New([]string{root}, a, WithRunner(r))
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

	// Seed two tasks with a dependency edge before kicking the project off.
	// Both start ready; the second is blocked on the first until status=committed.
	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	task1Path := filepath.Join(tasksDir, "first.yaml")
	task2Path := filepath.Join(tasksDir, "second.yaml")
	mustSaveTask(t, task1Path, &task.Task{Name: "first", Status: task.StatusReady})
	mustSaveTask(t, task2Path, &task.Task{Name: "second", Status: task.StatusReady, DependsOn: []string{"first"}})

	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Description: "test project",
		Branch:      "feat/happy",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	// 1) task agent for "first" launches.
	expectKindSession(t, buf, "task", 1)
	// Simulate the agent finishing the task.
	mustSaveTask(t, task1Path, &task.Task{Name: "first", Status: task.StatusSuccess})

	// 2) commit agent for "first" launches once the task session ends.
	expectKindSession(t, buf, "commit", 1)
	mustSaveTask(t, task1Path, &task.Task{Name: "first", Status: task.StatusCommitted})

	// 3) task agent for "second" launches once the commit session ends.
	expectKindSession(t, buf, "task", 2)
	mustSaveTask(t, task2Path, &task.Task{Name: "second", Status: task.StatusSuccess, DependsOn: []string{"first"}})

	// 4) commit agent for "second".
	expectKindSession(t, buf, "commit", 2)
	mustSaveTask(t, task2Path, &task.Task{Name: "second", Status: task.StatusCommitted, DependsOn: []string{"first"}})

	// 5) all tasks committed → daemon transitions project to done.
	if ok, snap := waitForWithin(t, buf, "project_done", 6*time.Second); !ok {
		t.Fatalf("project never reached done.\naudit:\n%s", snap)
	}

	reloaded, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if reloaded.Status != project.StatusDone {
		t.Errorf("project status = %q, want done", reloaded.Status)
	}
	if reloaded.UpdatedBy != project.WriterDaemon {
		t.Errorf("project updated_by = %q, want daemon", reloaded.UpdatedBy)
	}

	// Sanity: each Kind launched exactly the expected number of times.
	snap := buf.String()
	if got := strings.Count(snap, "kind=task"); got != 2 {
		t.Errorf("task launches = %d, want 2.\naudit:\n%s", got, snap)
	}
	if got := strings.Count(snap, "kind=commit"); got != 2 {
		t.Errorf("commit launches = %d, want 2.\naudit:\n%s", got, snap)
	}
}

// expectKindSession waits until N session_started entries with kind=<kind>
// have appeared in the audit buffer. The orchestrator naturally interleaves
// "session_started kind=task" and "session_started kind=commit" entries, so
// we can't just count session_started — we have to grep for the kind.
func expectKindSession(t *testing.T, buf *safeBuf, kind string, n int) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	want := fmt.Sprintf("kind=%s", kind)
	for time.Now().Before(deadline) {
		// Each session_started log line carries kind=<kind>; count those.
		count := 0
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "session_started") && strings.Contains(line, want) {
				count++
			}
		}
		if count >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not see %d session_started kind=%s entries in time.\naudit:\n%s", n, kind, buf.String())
}

func mustSaveTask(t *testing.T, path string, tk *task.Task) {
	t.Helper()
	if err := task.Save(path, tk); err != nil {
		t.Fatalf("save task %s: %v", path, err)
	}
}
