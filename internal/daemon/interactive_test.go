package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/workspace"
)

var uuidV5Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSessionUUIDStableAndWellFormed(t *testing.T) {
	// Deterministic: the same work stream always yields the same id, so
	// `:session` resumes the same claude conversation on every launch.
	a := sessionUUID("widget")
	b := sessionUUID("widget")
	if a != b {
		t.Errorf("sessionUUID not stable: %q != %q", a, b)
	}
	if !uuidV5Re.MatchString(a) {
		t.Errorf("sessionUUID(%q) = %q, not a well-formed v5 UUID", "widget", a)
	}
	// Distinct work streams get distinct ids.
	if c := sessionUUID("gadget"); c == a {
		t.Errorf("distinct work streams collided on %q", c)
	}
}

func TestSanitizeSandboxName(t *testing.T) {
	// sbx rejects underscores; they must become hyphens.
	if got := sanitizeSandboxName("session-my_work_stream"); got != "session-my-work-stream" {
		t.Errorf("sanitizeSandboxName = %q, want session-my-work-stream", got)
	}
	if got := sanitizeSandboxName("shell-widget"); got != "shell-widget" {
		t.Errorf("sanitizeSandboxName should leave clean names alone; got %q", got)
	}
}

// dirTabLauncher records the specs the runner hands the terminal launcher.
type dirTabLauncher struct{ specs []agent.Spec }

func (l *dirTabLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	l.specs = append(l.specs, spec)
	return dirTabSession(spec.Name), nil
}

type dirTabSession string

func (s dirTabSession) Name() string               { return string(s) }
func (s dirTabSession) Wait(context.Context) error { return nil }
func (s dirTabSession) Close() error               { return nil }

// newDirTabDaemon builds a daemon whose runner records tab launches, with a
// stub workspace manager rooted under wsRoot, and writes a project.yaml for
// work stream "widget" (branch "widget-branch") under a fresh orch root.
func newDirTabDaemon(t *testing.T, wsRoot string) (*Daemon, *dirTabLauncher, string) {
	t.Helper()
	orchDir := filepath.Join(t.TempDir(), "widget")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(orchDir, project.FileName)
	if err := os.WriteFile(projectPath, []byte("description: x\nbranch: widget-branch\nstatus: working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	launcher := &dirTabLauncher{}
	d, err := New([]string{t.TempDir()}, audit.New(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	d.runner = &runner.Runner{Launcher: launcher, Workspaces: workspace.NewStub(wsRoot)}
	return d, launcher, projectPath
}

func TestOpenOrchDirTab(t *testing.T) {
	d, launcher, projectPath := newDirTabDaemon(t, t.TempDir())
	target, err := d.OpenOrchDirTab(context.Background(), projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if target != "orch-widget" || len(launcher.specs) != 1 {
		t.Fatalf("target = %q, specs = %v", target, launcher.specs)
	}
	if got, want := launcher.specs[0].Workspace, filepath.Dir(projectPath); got != want {
		t.Errorf("tab cwd = %q, want %q", got, want)
	}
}

func TestOpenOrchDirTabMissingDir(t *testing.T) {
	d, launcher, projectPath := newDirTabDaemon(t, t.TempDir())
	if err := os.RemoveAll(filepath.Dir(projectPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.OpenOrchDirTab(context.Background(), projectPath); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v, want a does-not-exist error", err)
	}
	if len(launcher.specs) != 0 {
		t.Errorf("no tab should open; got %v", launcher.specs)
	}
}

func TestOpenWorkspaceDirTab(t *testing.T) {
	wsRoot := t.TempDir()
	d, launcher, projectPath := newDirTabDaemon(t, wsRoot)
	wsDir := filepath.Join(wsRoot, "widget-branch")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := d.OpenWorkspaceDirTab(context.Background(), projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if target != "workspace-widget" || len(launcher.specs) != 1 {
		t.Fatalf("target = %q, specs = %v", target, launcher.specs)
	}
	if got := launcher.specs[0].Workspace; got != wsDir {
		t.Errorf("tab cwd = %q, want %q", got, wsDir)
	}
}

func TestOpenWorkspaceDirTabMissingDir(t *testing.T) {
	d, launcher, projectPath := newDirTabDaemon(t, t.TempDir())
	// The workspace was never provisioned; :workspace must not create it.
	if _, err := d.OpenWorkspaceDirTab(context.Background(), projectPath); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v, want a does-not-exist error", err)
	}
	if len(launcher.specs) != 0 {
		t.Errorf("no tab should open; got %v", launcher.specs)
	}
}
