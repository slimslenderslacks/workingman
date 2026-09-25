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
	"github.com/slimslenderslacks/work/internal/scheduler"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// TestReviewingRoutesPendingTasksBeforePR pins the routing rule that a project
// with an open PR (`reviewing`) still runs manual tasks queued alongside the
// watch: when a ready task exists the dispatch takes priority and the project
// keeps its `reviewing` status, and only a fully-committed graph falls through
// to (re)arm the PR poll. Runner-less, so the dispatch itself no-ops — the
// observable is the status/poll routing, which is what changed.
func TestReviewingRoutesPendingTasksBeforePR(t *testing.T) {
	t.Run("ready task takes priority and keeps reviewing", func(t *testing.T) {
		root := t.TempDir()
		d, _, sched := newReviewDaemon(t, root)
		projectPath := filepath.Join(root, ".project.yaml")
		tasksDir := filepath.Join(root, "tasks")
		if err := os.MkdirAll(tasksDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// The original work is committed (this is how the project reached
		// reviewing); a human then appended a manual task that is still ready.
		mustSaveTask(t, filepath.Join(tasksDir, "landed.yaml"), &task.Task{Name: "landed", Status: task.StatusCommitted})
		mustSaveTask(t, filepath.Join(tasksDir, "manual-fix.yaml"), &task.Task{Name: "manual-fix", Status: task.StatusReady})

		p := &project.Project{
			Description: "x", Branch: "b", Status: project.StatusReviewing,
			Repos: []project.Repo{{Org: "docker", Name: "gateway"}},
		}
		d.dispatchProject(projectPath, p)

		got, err := project.Load(projectPath)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Status != project.StatusReviewing {
			t.Errorf("status = %q, want reviewing (pending work must not change the status)", got.Status)
		}
		// Pending work took priority and returned before the poll was armed.
		if spec := sched.Spec(reviewPollKey(projectPath)); spec != "" {
			t.Errorf("review poll armed while a manual task was pending: %q", spec)
		}
	})

	t.Run("all committed falls through to the PR poll", func(t *testing.T) {
		root := t.TempDir()
		d, _, sched := newReviewDaemon(t, root)
		projectPath := filepath.Join(root, ".project.yaml")
		tasksDir := filepath.Join(root, "tasks")
		if err := os.MkdirAll(tasksDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		mustSaveTask(t, filepath.Join(tasksDir, "landed.yaml"), &task.Task{Name: "landed", Status: task.StatusCommitted})

		p := &project.Project{
			Description: "x", Branch: "b", Status: project.StatusReviewing,
			Repos: []project.Repo{{Org: "docker", Name: "gateway"}},
		}
		d.dispatchProject(projectPath, p)

		got, err := project.Load(projectPath)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Status != project.StatusReviewing {
			t.Errorf("status = %q, want reviewing", got.Status)
		}
		if spec := sched.Spec(reviewPollKey(projectPath)); spec != reviewPollSpecs[0] {
			t.Errorf("review poll spec = %q, want %q (a committed graph must keep watching the PR)", spec, reviewPollSpecs[0])
		}
	})
}

// TestDoneRoutesPendingTasksWithoutReopening covers the `done` counterpart: a
// finished project someone appended a task to runs it, and — crucially — a done
// project is never spuriously moved. dispatchPendingTasks does not re-run the
// completion transition, so neither a pending nor a fully-committed graph flips
// a done project (a done project with repos would otherwise re-open the PR
// watch).
func TestDoneRoutesPendingTasksWithoutReopening(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seed    []*task.Task
		wantErr string
	}{
		{"pending manual task", []*task.Task{
			{Name: "landed", Status: task.StatusCommitted},
			{Name: "manual-fix", Status: task.StatusReady},
		}, ""},
		{"fully committed", []*task.Task{
			{Name: "landed", Status: task.StatusCommitted},
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			d, _, _ := newReviewDaemon(t, root)
			projectPath := filepath.Join(root, ".project.yaml")
			tasksDir := filepath.Join(root, "tasks")
			if err := os.MkdirAll(tasksDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			for _, tk := range tc.seed {
				mustSaveTask(t, filepath.Join(tasksDir, tk.Name+".yaml"), tk)
			}

			p := &project.Project{
				Description: "x", Branch: "b", Status: project.StatusDone,
				Repos: []project.Repo{{Org: "docker", Name: "gateway"}},
			}
			d.dispatchProject(projectPath, p)

			got, err := project.Load(projectPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Status != project.StatusDone {
				t.Errorf("status = %q, want done (a done project must not be moved by dispatch)", got.Status)
			}
		})
	}
}

// TestRestingProjectWithPendingSeedReArmsPlanning covers the orphaned-seed
// recovery: an intake addition flips a project to `ready` so planning fleshes
// the seed into a real task, but that flip can be skipped or clobbered while
// another agent holds the project slot (the mcp-server-instructions case — a
// review agent's `done` write landed after the flip). When the daemon later
// observes a resting project (done/reviewing) that still carries a pending
// seed, it must re-arm planning by flipping the status back to `ready`.
func TestRestingProjectWithPendingSeedReArmsPlanning(t *testing.T) {
	for _, from := range []project.Status{project.StatusDone, project.StatusReviewing} {
		t.Run(string(from), func(t *testing.T) {
			root := t.TempDir()
			d, buf, _ := newReviewDaemon(t, root)
			projectPath := filepath.Join(root, ".project.yaml")
			tasksDir := filepath.Join(root, "tasks")
			if err := os.MkdirAll(tasksDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			// The original work committed; then an intake file queued a seed
			// (blank name + description) that never got planned.
			mustSaveTask(t, filepath.Join(tasksDir, "landed.yaml"), &task.Task{Name: "landed", Status: task.StatusCommitted})
			if err := os.WriteFile(filepath.Join(tasksDir, "seed.yaml"),
				[]byte("name: \"\"\ndescription: sync and re-pin the dependency\nstatus: ready\n"), 0o644); err != nil {
				t.Fatalf("write seed: %v", err)
			}

			p := &project.Project{
				Description: "x", Branch: "b", Status: from,
				Repos: []project.Repo{{Org: "docker", Name: "gateway"}},
			}
			d.dispatchProject(projectPath, p)

			got, err := project.Load(projectPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Status != project.StatusReady {
				t.Errorf("status = %q, want ready (a pending seed must re-arm planning)", got.Status)
			}
			if !strings.Contains(buf.String(), "seed_replan") {
				t.Errorf("expected seed_replan in audit:\n%s", buf.String())
			}
		})
	}

	t.Run("no seed leaves done untouched", func(t *testing.T) {
		root := t.TempDir()
		d, _, sched := newReviewDaemon(t, root)
		projectPath := filepath.Join(root, ".project.yaml")
		tasksDir := filepath.Join(root, "tasks")
		if err := os.MkdirAll(tasksDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		mustSaveTask(t, filepath.Join(tasksDir, "landed.yaml"), &task.Task{Name: "landed", Status: task.StatusCommitted})

		p := &project.Project{Description: "x", Branch: "b", Status: project.StatusDone}
		d.dispatchProject(projectPath, p)

		got, err := project.Load(projectPath)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Status != project.StatusDone {
			t.Errorf("status = %q, want done (no seed, nothing to re-arm)", got.Status)
		}
		if spec := sched.Spec(reviewPollKey(projectPath)); spec != "" {
			t.Errorf("unexpected review poll for a plain done project: %q", spec)
		}
	})
}

// TestReviewingProjectDispatchesReadyTaskAgent is the end-to-end proof that a
// ready task under `reviewing` actually launches a task agent (not just a
// routing decision): the manual task runs even though the project has an open
// PR. Same shape as the push-dispatch test — a real tmux launcher, a stub
// workspace, and a fake command that just sleeps.
func TestReviewingProjectDispatchesReadyTaskAgent(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	root := t.TempDir()
	socket := fmt.Sprintf("orch-reviewing-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A committed original plus a ready manual task added while the PR is open.
	mustSaveTask(t, filepath.Join(tasksDir, "landed.yaml"), &task.Task{Name: "landed", Status: task.StatusCommitted})
	mustSaveTask(t, filepath.Join(tasksDir, "manual-fix.yaml"), &task.Task{Name: "manual-fix", Status: task.StatusReady})

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "reviewing dispatch",
		Branch:      "feat/reviewing",
		Status:      project.StatusReviewing,
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
	d, err := New([]string{root}, a, WithRunner(r), WithScheduler(scheduler.New()))
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

	// The ready manual task must dispatch a task agent even though the project is
	// only `reviewing` (open PR), not `working`.
	expectKindSession(t, buf, "task", 1)
}
