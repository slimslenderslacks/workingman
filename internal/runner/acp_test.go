package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// argValue returns the token following the first occurrence of flag in args, or
// "" if the flag isn't present (or has no following token).
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// hasFlag reports whether flag appears anywhere in args. For boolean flags like
// --exit-when-empty that take no following token.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// argValues returns every token following an occurrence of flag — used for
// repeatable flags like --workspace.
func argValues(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestACPLaunchPlanningAgent(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionsRoot := t.TempDir()

	acp := &fakeLauncher{}
	tmux := &fakeLauncher{}
	r := &Runner{
		Launcher:       tmux,
		AcpLauncher:    acp,
		Kit:            "/kits/acp-kit",
		SessionsRoot:   sessionsRoot,
		AcpWrapperPath: "/bin/acp-wrapper",
		SbxPath:        "/bin/sbx",
	}

	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.PlanningAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
		Branch:      "feat-x",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The tmux launcher must not be used for a non-interactive ACP launch.
	if tmux.last.Name != "" {
		t.Errorf("tmux launcher was used for an ACP agent: %+v", tmux.last)
	}

	cmd := acp.last.Command
	if len(cmd) == 0 || cmd[0] != "/bin/acp-wrapper" {
		t.Fatalf("command[0] = %v, want /bin/acp-wrapper (full: %v)", cmd, cmd)
	}
	wantID := "planning-feat-x"
	if got := argValue(cmd, "--session-id"); got != wantID {
		t.Errorf("--session-id = %q, want %q", got, wantID)
	}
	if got := argValue(cmd, "--kit"); got != "/kits/acp-kit" {
		t.Errorf("--kit = %q, want /kits/acp-kit", got)
	}
	if got := argValue(cmd, "--sandbox"); got != filepath.Base(workingDir) {
		t.Errorf("--sandbox = %q, want %q", got, filepath.Base(workingDir))
	}
	if got := argValue(cmd, "--sessions-root"); got != sessionsRoot {
		t.Errorf("--sessions-root = %q, want %q", got, sessionsRoot)
	}
	if got := argValue(cmd, "--sbx"); got != "/bin/sbx" {
		t.Errorf("--sbx = %q, want /bin/sbx", got)
	}
	if ws := argValues(cmd, "--workspace"); len(ws) != 1 || ws[0] != workingDir {
		t.Errorf("--workspace = %v, want [%q]", ws, workingDir)
	}
	if !hasFlag(cmd, "--exit-when-empty") {
		t.Errorf("expected --exit-when-empty in argv, got %v", cmd)
	}
	if acp.last.Name != wantID {
		t.Errorf("spec.Name = %q, want %q", acp.last.Name, wantID)
	}

	// The initial session.json must be on disk for a restarting TUI to find.
	store := session.Store{Root: sessionsRoot}
	rec, err := store.Read(wantID)
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	if rec.Status != session.StatusStarting {
		t.Errorf("status = %q, want %q", rec.Status, session.StatusStarting)
	}
	if rec.SandboxName != filepath.Base(workingDir) {
		t.Errorf("sandbox = %q, want %q", rec.SandboxName, filepath.Base(workingDir))
	}
	if rec.Kit != "/kits/acp-kit" {
		t.Errorf("kit = %q, want /kits/acp-kit", rec.Kit)
	}
	if rec.SocketPath != store.SocketPath(wantID) {
		t.Errorf("socket_path = %q, want %q", rec.SocketPath, store.SocketPath(wantID))
	}
	if rec.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
}

func TestACPLaunchTaskAgentMountsOrchDir(t *testing.T) {
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionsRoot := t.TempDir()

	acp := &fakeLauncher{}
	r := &Runner{
		Workspaces:   workspace.NewStub(wsRoot),
		Launcher:     &fakeLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit-ref",
		SessionsRoot: sessionsRoot,
	}

	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.TaskAgent,
		Branch:      "feat-x",
		ProjectPath: projectPath,
		TaskPath:    filepath.Join(orchDir, "tasks", "first.yaml"),
		TaskName:    "first",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cmd := acp.last.Command
	if cmd[0] != "acp-wrapper" {
		t.Errorf("default binary = %q, want acp-wrapper", cmd[0])
	}
	wantWorktree := filepath.Join(wsRoot, "feat-x")
	ws := argValues(cmd, "--workspace")
	if len(ws) != 2 || ws[0] != wantWorktree || ws[1] != orchDir {
		t.Errorf("--workspace = %v, want [%q %q]", ws, wantWorktree, orchDir)
	}
	if got := argValue(cmd, "--sandbox"); got != filepath.Base(orchDir)+"-worktree" {
		t.Errorf("--sandbox = %q, want %q", got, filepath.Base(orchDir)+"-worktree")
	}
	if got := argValue(cmd, "--session-id"); got != "task-first" {
		t.Errorf("--session-id = %q, want task-first", got)
	}
	// --sbx omitted when SbxPath is unset.
	if got := argValue(cmd, "--sbx"); got != "" {
		t.Errorf("--sbx = %q, want empty (unset)", got)
	}
}

func TestInteractiveAgentNeverUsesACP(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tmux := &fakeLauncher{}
	r := &Runner{
		Launcher:     tmux,
		AcpLauncher:  &failLauncher{t: t},
		Kit:          "kit",
		SessionsRoot: t.TempDir(),
		Command:      func(_ agent.Kind, _ string) []string { return []string{"claude", "hi"} },
	}
	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.ProjectAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tmux.last.Command[0] != "claude" {
		t.Errorf("project agent should use the tmux launcher with the built command, got %v", tmux.last.Command)
	}
}

func TestACPRequiresKit(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Launcher:     &fakeLauncher{},
		AcpLauncher:  &fakeLauncher{},
		SessionsRoot: t.TempDir(),
		// Kit deliberately empty.
	}
	_, err := r.Start(context.Background(), Plan{
		Kind:        agent.PlanningAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
		Branch:      "feat-x",
	})
	if err == nil || !strings.Contains(err.Error(), "Kit") {
		t.Errorf("expected a Kit-required error, got %v", err)
	}
}

// failLauncher fails the test if its Launch is ever called. Used to prove the
// ACP path is not taken for interactive agents.
type failLauncher struct{ t *testing.T }

func (f *failLauncher) Launch(context.Context, agent.Spec) (agent.Session, error) {
	f.t.Fatal("AcpLauncher must not be used for interactive agents")
	return nil, nil
}
