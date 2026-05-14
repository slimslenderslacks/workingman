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

// TestProjectAgentEndToEnd is the step-4b deliverable: write an empty
// .project.yaml inside a watched root and verify the full pipeline runs —
// daemon detects it, runner drops .orch/{context.yaml,instructions.md} into
// the working dir, tmux launcher starts a session, and the session ends
// cleanly.
//
// The Runner's CommandBuilder returns `sleep 1` so no claude binary is
// needed to exercise the plumbing.
func TestProjectAgentEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	stubRoot := t.TempDir()
	socket := fmt.Sprintf("orch-e2e-%d", time.Now().UnixNano())
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
		Command: func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 1"} },
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

	projectPath := filepath.Join(root, ".project.yaml")
	if err := writeFile(projectPath, nil); err != nil {
		t.Fatalf("write empty project: %v", err)
	}

	if ok, snap := waitFor(t, buf, "session_started"); !ok {
		t.Fatalf("no session_started.\naudit:\n%s", snap)
	}

	// The session ran in the dir holding .project.yaml (project agent runs
	// without a wsp workspace) — so .orch/ should be alongside it.
	for _, want := range []string{"context.yaml", "instructions.md"} {
		p := filepath.Join(root, ".orch", want)
		if _, err := os.Stat(p); err != nil {
			t.Errorf(".orch/%s missing: %v", want, err)
		}
	}

	snap := buf.String()
	if !strings.Contains(snap, "kind=project") {
		t.Errorf("audit should record kind=project:\n%s", snap)
	}

	// sleep 1 + 50ms poll → ~1.1s; allow 4s before giving up.
	if ok, snap := waitForWithin(t, buf, "session_ended", 4*time.Second); !ok {
		t.Fatalf("session never ended.\naudit:\n%s", snap)
	}
}

func TestStatusReadyTriggersPlanningAgent(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	socket := fmt.Sprintf("orch-plan-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		// Workspaces left nil: planning agent must NOT need a wsp workspace.
		Launcher: &agent.TmuxLauncher{
			Socket:       socket,
			PollInterval: 50 * time.Millisecond,
		},
		Audit:   a,
		Command: func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 1"} },
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

	// Populated, ready-to-plan project — written by the (notional) project agent.
	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Description: "test project",
		Branch:      "feat/healthz-probe",
		Status:      project.StatusReady,
		Repos: []project.Repo{
			{Org: "docker", Name: "gateway"},
		},
	}
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	if ok, snap := waitFor(t, buf, "session_started"); !ok {
		t.Fatalf("planning session never started.\naudit:\n%s", snap)
	}
	snap := buf.String()
	if !strings.Contains(snap, "kind=planning") {
		t.Errorf("expected kind=planning, got:\n%s", snap)
	}

	// Planning agent runs in the project root, not a workspace dir.
	ctxData, err := os.ReadFile(filepath.Join(root, ".orch", "context.yaml"))
	if err != nil {
		t.Fatalf("read context.yaml: %v", err)
	}
	for _, want := range []string{"kind: planning", "feat/healthz-probe", "tasks_dir"} {
		if !strings.Contains(string(ctxData), want) {
			t.Errorf("context.yaml missing %q:\n%s", want, ctxData)
		}
	}
	if ok, snap := waitForWithin(t, buf, "session_ended", 4*time.Second); !ok {
		t.Fatalf("session never ended.\naudit:\n%s", snap)
	}
}

// TestProjectAgentHandoffToPlanning is the regression for the scenario
// reported in the field: project agent runs, writes status=ready, exits —
// daemon must launch the planning agent. The bug was that the agent's
// status=ready write fires fsnotify while the project agent's session is
// still in the session map; the planning launch attempt got dedup-skipped.
// The fix is to re-run handleProject as the project agent's onEnd.
// TestStartupScanDispatchesExistingProject is the regression for restart
// reliability: a populated .project.yaml with status=ready already on disk
// when the daemon starts must trigger planning without anyone touching the
// file afterwards. Without the startup scan, fsnotify never fires (the file
// is unchanged) and the project is stranded until the user manually saves it.
// TestResumeStuckSuccessTask is the regression for the scenario observed in
// production: a task agent finished (status:success) but the commit agent
// never ran (because the project went through a block/wolf cycle that
// bypassed afterTaskSession). On the next dispatch — startup, status=working
// observation, cron firing — the daemon must resume by launching the commit
// agent, not log no_ready_tasks.
// TestNumericPrefixTaskFilenames is the regression for the bug the user
// reported: the planning agent writes task files with sort-friendly names
// like "00-foo.yaml" while the task's `name:` field is just "foo". The
// daemon used to reconstruct the file path as `tasks/<name>.yaml` after the
// session ended, missing the prefixed file and treating the task as deleted.
// With the Path fix, the daemon uses the actual loaded path throughout.
func TestNumericPrefixTaskFilenames(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	socket := fmt.Sprintf("orch-prefix-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Note: filename has a prefix, task name does not.
	prefixedPath := filepath.Join(tasksDir, "00-register-repo.yaml")
	mustSaveTask(t, prefixedPath, &task.Task{
		Name:   "register-repo",
		Status: task.StatusSuccess, // pending commit
	})

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "prefix test",
		Branch:      "feat/prefix",
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

	// Resume detection should find register-repo at status=success and
	// launch the commit agent. Then we simulate the commit agent writing
	// status=committed to the SAME prefixed file path.
	expectKindSession(t, buf, "commit", 1)

	mustSaveTask(t, prefixedPath, &task.Task{
		Name:   "register-repo",
		Status: task.StatusCommitted,
	})

	// afterCommitSession must reload from the prefixed path — not from
	// tasks/register-repo.yaml — or it would log task_load_error and block.
	// Project should reach done (no other tasks).
	if ok, snap := waitForWithin(t, buf, "project_done", 6*time.Second); !ok {
		t.Fatalf("project never reached done — likely task_load_error from wrong path.\naudit:\n%s", snap)
	}
	if strings.Contains(buf.String(), "task_missing_after_session") || strings.Contains(buf.String(), "task_load_error") {
		t.Errorf("daemon used wrong path:\n%s", buf.String())
	}
}

func TestResumeStuckSuccessTask(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	socket := fmt.Sprintf("orch-resume-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	// Pre-seed: project status=working; task A at status=success
	// (commit never ran); task B at status=ready depending on A.
	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	taskA := filepath.Join(tasksDir, "first.yaml")
	taskB := filepath.Join(tasksDir, "second.yaml")
	mustSaveTask(t, taskA, &task.Task{Name: "first", Status: task.StatusSuccess})
	mustSaveTask(t, taskB, &task.Task{Name: "second", Status: task.StatusReady, DependsOn: []string{"first"}})

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "resume test",
		Branch:      "feat/resume",
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

	// First scheduled action should be the commit agent for `first` —
	// not a task agent, and definitely not no_ready_tasks.
	if ok, snap := waitForWithin(t, buf, "resume_pending_commit", 3*time.Second); !ok {
		t.Fatalf("daemon did not resume pending commit.\naudit:\n%s", snap)
	}
	expectKindSession(t, buf, "commit", 1)
	if strings.Contains(buf.String(), "no_ready_tasks") {
		t.Errorf("dispatch shouldn't reach no_ready_tasks when a success task is pending commit:\n%s", buf.String())
	}
}

func TestStartupScanDispatchesExistingProject(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	// Seed the project file *before* starting the daemon.
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "pre-existing",
		Branch:      "feat/startup",
		Status:      project.StatusReady,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	socket := fmt.Sprintf("orch-startup-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})
	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		Launcher: &agent.TmuxLauncher{
			Socket:       socket,
			PollInterval: 50 * time.Millisecond,
		},
		Audit:   a,
		Command: func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 1"} },
	}
	d, err := New([]string{root}, a, WithRunner(r))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	if ok, snap := waitFor(t, buf, "startup_scan"); !ok {
		t.Fatalf("no startup_scan entry: %s", snap)
	}
	expectKindSession(t, buf, "planning", 1)
	if !strings.Contains(buf.String(), "projects=1") {
		t.Errorf("startup_scan should report 1 project:\n%s", buf.String())
	}
}

func TestProjectAgentHandoffToPlanning(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	socket := fmt.Sprintf("orch-handoff-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		// Planning agent runs in project root, no workspace manager needed.
		Launcher: &agent.TmuxLauncher{
			Socket:       socket,
			PollInterval: 50 * time.Millisecond,
		},
		Audit:   a,
		Command: func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 1"} },
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

	projectPath := filepath.Join(root, ".project.yaml")
	if err := writeFile(projectPath, nil); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	// 1) Project agent session starts.
	expectKindSession(t, buf, "project", 1)

	// While the project agent is alive, simulate it writing the populated
	// file with status=ready. This is the dedup-skip scenario.
	pop := &project.Project{
		Description: "test",
		Branch:      "feat/handoff",
		Status:      project.StatusReady,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}
	if err := project.SaveAs(projectPath, pop, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	// Project agent's `sleep 1` finishes; onEnd re-runs handleProject;
	// status=ready dispatches the planning agent. We should see a second
	// session_started with kind=planning within a few seconds.
	expectKindSession(t, buf, "planning", 1)
}

func TestDuplicateEmptyEventDoesNotLaunchTwice(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	socket := fmt.Sprintf("orch-dup-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		Workspaces: workspace.NewStub(t.TempDir()),
		Launcher: &agent.TmuxLauncher{
			Socket:       socket,
			PollInterval: 50 * time.Millisecond,
		},
		Audit: a,
		// Long sleep so the first session is definitely still alive when we
		// touch the file again.
		Command: func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 30"} },
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

	projectPath := filepath.Join(root, ".project.yaml")
	if err := writeFile(projectPath, nil); err != nil {
		t.Fatalf("write empty project: %v", err)
	}
	if ok, snap := waitFor(t, buf, "session_started"); !ok {
		t.Fatalf("first session_started missing.\naudit:\n%s", snap)
	}

	// Second touch — same empty file, fsnotify will deliver another Create
	// or Write. The daemon must drop it because the session is still alive.
	if err := writeFile(projectPath, nil); err != nil {
		t.Fatalf("second touch: %v", err)
	}
	if ok, _ := waitForWithin(t, buf, "session_skip_duplicate", 1*time.Second); !ok {
		// Not strictly required to log the skip, but we do — and the real
		// invariant is "exactly one session_started", checked below.
		t.Logf("no skip log seen; checking session_started count")
	}

	// Allow late deliveries to settle, then assert only one session_started.
	time.Sleep(500 * time.Millisecond)
	if got := strings.Count(buf.String(), "session_started"); got != 1 {
		t.Errorf("got %d session_started entries, want 1.\naudit:\n%s", got, buf.String())
	}
}
