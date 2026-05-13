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
