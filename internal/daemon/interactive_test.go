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
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
	"github.com/slimslenderslacks/work/internal/worktree"
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
	d, err := newDaemon(t, []string{t.TempDir()}, audit.New(&bytes.Buffer{}))
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

// fakeSbx writes an sbx stand-in that logs every invocation to log, one line
// per call, and answers `sbx ls` with no sandboxes. It returns its path.
func fakeSbx(t *testing.T, log string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sbx")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n[ \"$1\" = ls ] && echo '[]'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// sessionDaemon builds a worktree-enabled daemon whose runner launches `:session`
// tabs through a fake sbx and a recording tab launcher.
func (e *wtEnv) sessionDaemon() (*Daemon, *dirTabLauncher, string) {
	e.t.Helper()
	log := filepath.Join(e.t.TempDir(), "sbx.log")
	launcher := &dirTabLauncher{}
	a := audit.New(e.buf)
	d, err := newDaemon(e.t, []string{e.root}, a, WithRunner(&runner.Runner{
		Workspaces:   e.stub,
		Launcher:     launcher,
		Audit:        a,
		SbxPath:      fakeSbx(e.t, log),
		SessionsRoot: filepath.Join(e.t.TempDir(), "sessions"),
	}))
	if err != nil {
		e.t.Fatal(err)
	}
	d.ctx = context.Background()
	return d, launcher, log
}

// The shared workspace's `:session` tab warns, before claude starts, that the
// shared tree belongs to the commit queue (docs/parallel-task-worktrees.md X3).
func TestInteractiveSessionBannerWarnsAboutSharedTree(t *testing.T) {
	e := newWtEnv(t)
	e.provision()
	path := e.saveProject()
	d, launcher, _ := e.sessionDaemon()

	if _, err := d.OpenInteractiveSession(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if len(launcher.specs) != 1 {
		t.Fatalf("specs = %v", launcher.specs)
	}
	spec := launcher.specs[0]
	if spec.Workspace != e.shared {
		t.Errorf("cwd = %q, want the shared workspace %q", spec.Workspace, e.shared)
	}
	cmd := strings.Join(spec.Command, " ")
	for _, want := range []string{"printf", "shared workspace of " + wtBranch, "block the next integration", "while an integration is in flight"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("session command lacks %q:\n%s", want, cmd)
		}
	}
	if i, j := strings.Index(cmd, "printf"), strings.Index(cmd, " run --name session-"); i < 0 || j < i {
		t.Errorf("banner does not come before sbx run:\n%s", cmd)
	}
}

// Without per-task worktrees nothing integrates into the shared tree, so the
// session opens with no banner.
func TestInteractiveSessionNoBannerWithoutWorktrees(t *testing.T) {
	wsRoot := t.TempDir()
	d, launcher, projectPath := newDirTabDaemon(t, wsRoot)
	d.runner.SbxPath = fakeSbx(t, filepath.Join(t.TempDir(), "sbx.log"))
	d.runner.SessionsRoot = filepath.Join(t.TempDir(), "sessions")
	if _, err := d.OpenInteractiveSession(context.Background(), projectPath); err != nil {
		t.Fatal(err)
	}
	if len(launcher.specs) != 1 || strings.Contains(strings.Join(launcher.specs[0].Command, " "), "printf") {
		t.Errorf("specs = %v, want one with no banner", launcher.specs)
	}
}

// `:session` on a task with a worktree runs in that worktree (X3): it is the
// cwd and first mount, the repos' git common dirs and the orch dir are
// mounted next to it, the shared tree is not, and the sandbox, tab and
// conversation are the task's own.
func TestOpenTaskSessionUsesWorktreeAsCwd(t *testing.T) {
	e := newWtEnv(t)
	e.seedWorktreeTask("stuck", task.StatusFailed, map[string]string{"gateway": "x"})
	path := e.saveProject()
	d, launcher, log := e.sessionDaemon()

	target, err := d.OpenTaskSession(context.Background(), path, e.taskPath("stuck"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "session-"+filepath.Base(e.root)+"-stuck" || len(launcher.specs) != 1 {
		t.Fatalf("target = %q, specs = %v", target, launcher.specs)
	}
	root := worktree.Root(e.shared, "stuck")
	spec := launcher.specs[0]
	if spec.Workspace != root {
		t.Errorf("cwd = %q, want the task worktree %q", spec.Workspace, root)
	}
	cmd := strings.Join(spec.Command, " ")
	for _, want := range []string{"task stuck's worktree", worktree.BranchName(wtBranch, "stuck")} {
		if !strings.Contains(cmd, want) {
			t.Errorf("task session command lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--session-id "+sessionUUID(filepath.Base(e.root))) {
		t.Error("task session shares the work stream's conversation")
	}

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var create []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "create ") {
			create = strings.Fields(l)
		}
	}
	if len(create) == 0 {
		t.Fatalf("no sbx create:\n%s", raw)
	}
	commonDir, err := worktree.Manager{}.GitCommonDir(context.Background(), filepath.Join(e.shared, "gateway"))
	if err != nil {
		t.Fatal(err)
	}
	mounts := create[len(create)-3:]
	if want := []string{root, commonDir, e.root}; strings.Join(mounts, " ") != strings.Join(want, " ") {
		t.Errorf("mounts = %v, want %v", mounts, want)
	}
}

// A task with no worktree has nothing to open.
func TestOpenTaskSessionWithoutWorktree(t *testing.T) {
	e := newWtEnv(t)
	e.provision()
	mustSaveTask(t, e.taskPath("plain"), &task.Task{Name: "plain", Status: task.StatusReady})
	path := e.saveProject()
	d, launcher, _ := e.sessionDaemon()
	if _, err := d.OpenTaskSession(context.Background(), path, e.taskPath("plain")); err == nil || !strings.Contains(err.Error(), "no worktree") {
		t.Errorf("err = %v, want a no-worktree error", err)
	}
	if len(launcher.specs) != 0 {
		t.Errorf("no tab should open; got %v", launcher.specs)
	}
}
