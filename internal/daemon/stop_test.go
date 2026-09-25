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

// TestDispatchStoppedUnregistersSchedules pins the routing-level half of
// `:stop`: observing status:stopped must drop both the project's cron
// schedule and its #review poll, whichever happen to be registered, and must
// do so without launching anything (dispatchProject returns before the normal
// status switch — runner-less here, so any launch attempt would be visible as
// an audit entry it never produces).
func TestDispatchStoppedUnregistersSchedules(t *testing.T) {
	root := t.TempDir()
	d, buf, sched := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")

	if err := sched.Register(projectPath, "@every 1m", func() {}); err != nil {
		t.Fatalf("register cron: %v", err)
	}
	if err := sched.Register(reviewPollKey(projectPath), "@every 1m", func() {}); err != nil {
		t.Fatalf("register review poll: %v", err)
	}

	p := &project.Project{
		Description: "x", Branch: "b",
		Status:      project.StatusStopped,
		StoppedFrom: project.StatusWorking,
	}
	d.dispatchProject(projectPath, p)

	if spec := sched.Spec(projectPath); spec != "" {
		t.Errorf("cron schedule still registered: %q", spec)
	}
	if spec := sched.Spec(reviewPollKey(projectPath)); spec != "" {
		t.Errorf("review poll still registered: %q", spec)
	}
	if strings.Contains(buf.String(), "session_started") {
		t.Errorf("a stopped project must not launch anything:\n%s", buf.String())
	}
}

// TestDispatchStoppedIsIdempotent covers the case dispatchProject must
// tolerate: repeat observations of an already-stopped project (a daemon
// restart onto one, or a second unrelated fsnotify event) with nothing left
// to unregister or kill.
func TestDispatchStoppedIsIdempotent(t *testing.T) {
	root := t.TempDir()
	d, _, sched := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")

	p := &project.Project{Description: "x", Branch: "b", Status: project.StatusStopped, StoppedFrom: project.StatusDone}
	d.dispatchProject(projectPath, p)
	d.dispatchProject(projectPath, p)

	if spec := sched.Spec(projectPath); spec != "" {
		t.Errorf("unexpected cron registration: %q", spec)
	}
}

// TestStoppedGuardsSkipTaskSessionEnd pins the guard that keeps a
// `:stop`-killed task-agent session from being read as a crash: with the
// project already status:stopped, afterTaskSession must bail out instead of
// bumping Attempts / relaunching / blocking — any of which would fight the
// stop and, in the blocking case, overwrite status:stopped outright.
func TestStoppedGuardsSkipTaskSessionEnd(t *testing.T) {
	root := t.TempDir()
	d, buf, _ := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")
	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	taskPath := filepath.Join(tasksDir, "manual-fix.yaml")
	// Nothing in this codebase ever writes task status:running (see
	// dispatch_lifecycle.go's afterTaskSession doc), so a task agent killed
	// mid-run leaves its task exactly where it started: ready.
	mustSaveTask(t, taskPath, &task.Task{Name: "manual-fix", Status: task.StatusReady})

	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b",
		Status:      project.StatusStopped,
		StoppedFrom: project.StatusWorking,
	}, project.WriterDaemon); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// A stale pointer, as the real call site would pass: dispatchNextTask read
	// this before :stop landed.
	stale := &project.Project{Description: "x", Branch: "b", Status: project.StatusWorking}
	d.afterTaskSession(projectPath, taskPath, stale)

	if !strings.Contains(buf.String(), "session_end_skipped_stopped") {
		t.Errorf("expected session_end_skipped_stopped in audit:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "project_blocked") || strings.Contains(buf.String(), "task_retry") {
		t.Errorf("stopped project must not be retried or blocked:\n%s", buf.String())
	}
	got, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != project.StatusStopped {
		t.Errorf("status = %q, want stopped left untouched", got.Status)
	}
}

// TestStoppedGuardsSkipCommitSessionEnd is the commit-agent analogue: a task
// left at status:success (the commit agent was killed before it could write
// status:committed) must not trip afterCommitSession's "commit agent ended
// without committing" block.
func TestStoppedGuardsSkipCommitSessionEnd(t *testing.T) {
	root := t.TempDir()
	d, buf, _ := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")
	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	taskPath := filepath.Join(tasksDir, "manual-fix.yaml")
	mustSaveTask(t, taskPath, &task.Task{Name: "manual-fix", Status: task.StatusSuccess})

	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b",
		Status:      project.StatusStopped,
		StoppedFrom: project.StatusWorking,
	}, project.WriterDaemon); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	stale := &project.Project{Description: "x", Branch: "b", Status: project.StatusWorking}
	d.afterCommitSession(projectPath, taskPath, stale)

	if !strings.Contains(buf.String(), "session_end_skipped_stopped") {
		t.Errorf("expected session_end_skipped_stopped in audit:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "project_blocked") {
		t.Errorf("stopped project must not be blocked:\n%s", buf.String())
	}
	got, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != project.StatusStopped {
		t.Errorf("status = %q, want stopped left untouched", got.Status)
	}
}

// TestStoppedGuardsSkipReviewSessionEnd covers the review agent: a session
// enforceStopped killed reports a non-nil wait error, which afterReviewSession
// would otherwise count toward maxReviewErrors.
func TestStoppedGuardsSkipReviewSessionEnd(t *testing.T) {
	root := t.TempDir()
	d, buf, _ := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")

	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b",
		Status:      project.StatusStopped,
		StoppedFrom: project.StatusReviewing,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterDaemon); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	d.afterReviewSession(projectPath, fmt.Errorf("signal: terminated"))

	if !strings.Contains(buf.String(), "session_end_skipped_stopped") {
		t.Errorf("expected session_end_skipped_stopped in audit:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "review_error") || strings.Contains(buf.String(), "project_blocked") {
		t.Errorf("a stop-killed review session must not be counted as a crash:\n%s", buf.String())
	}
	got, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != project.StatusStopped {
		t.Errorf("status = %q, want stopped left untouched", got.Status)
	}
}

// TestStopKillsRunningTaskSession is the end-to-end proof that `:stop` doesn't
// just refuse to launch new work — it actually terminates an agent already
// running. A real tmux launcher runs a long sleep as the "task agent"; once
// it's up, the project file is flipped to status:stopped exactly as `:stop`
// would write it, and the daemon must close that session in response.
func TestStopKillsRunningTaskSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	root := t.TempDir()
	socket := fmt.Sprintf("orch-stop-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustSaveTask(t, filepath.Join(tasksDir, "manual-fix.yaml"), &task.Task{Name: "manual-fix", Status: task.StatusReady})

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "stop kills sessions",
		Branch:      "feat/stop",
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
		Command:    func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 30"} },
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
	expectKindSession(t, buf, "task", 1)

	stopped, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	stopped.StoppedFrom = stopped.Status
	stopped.Status = project.StatusStopped
	if err := project.SaveAs(projectPath, stopped, project.WriterAgent); err != nil {
		t.Fatalf("save stopped: %v", err)
	}

	if ok, snap := waitFor(t, buf, "session_stopped"); !ok {
		t.Fatalf("session was never stopped: %s", snap)
	}
	if ok, snap := waitFor(t, buf, "session_end_skipped_stopped"); !ok {
		t.Fatalf("afterTaskSession did not recognize the stop: %s", snap)
	}
	if strings.Contains(buf.String(), "project_blocked") {
		t.Errorf("killed session must not have blocked the project:\n%s", buf.String())
	}

	got, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != project.StatusStopped {
		t.Errorf("status = %q, want stopped left untouched", got.Status)
	}
}
