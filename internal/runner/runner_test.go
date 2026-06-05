package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// fakeLauncher records the Spec it was called with and returns a Session that
// reports as alive until Close is called.
type fakeLauncher struct {
	mu   sync.Mutex
	last agent.Spec
}

func (f *fakeLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = spec
	return &fakeSession{name: spec.Name}, nil
}

type fakeSession struct {
	mu     sync.Mutex
	name   string
	closed bool
}

func (s *fakeSession) Name() string { return s.name }
func (s *fakeSession) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func TestDefaultCommandBuilderModes(t *testing.T) {
	// Autonomous kinds (planning/task/commit) run with --print so claude
	// executes one turn and exits, closing the tmux window and letting the
	// daemon advance project state. Interactive kinds (project/wolf) omit
	// --print so the human can drive the conversation. Every kind carries
	// --dangerously-skip-permissions and the initial prompt on argv.
	cases := []struct {
		kind        agent.Kind
		wantPrint   bool
		description string
	}{
		{agent.ProjectAgent, false, "project agent interviews the user"},
		{agent.WolfAgent, false, "wolf agent asks for guidance"},
		{agent.PlanningAgent, true, "planning agent runs one autonomous turn"},
		{agent.TaskAgent, true, "task agent runs one autonomous turn"},
		{agent.CommitAgent, true, "commit agent runs one autonomous turn"},
	}
	for _, tc := range cases {
		t.Run(tc.kind.String(), func(t *testing.T) {
			cmd := DefaultCommandBuilder(tc.kind, "/ws")
			got := contains(cmd, "--print")
			if got != tc.wantPrint {
				t.Errorf("--print=%v, want %v (%s): %v", got, tc.wantPrint, tc.description, cmd)
			}
			if !contains(cmd, "--dangerously-skip-permissions") {
				t.Errorf("missing --dangerously-skip-permissions: %v", cmd)
			}
			if !contains(cmd, initialPrompt) {
				t.Errorf("missing initial prompt: %v", cmd)
			}
		})
	}
}

func TestSandboxNameFor(t *testing.T) {
	const projectPath = "/orch/myproj/.project.yaml"
	cases := []struct {
		kind agent.Kind
		want string
	}{
		{agent.ProjectAgent, ""},
		{agent.PlanningAgent, "myproj"},
		{agent.WolfAgent, ""},
		{agent.TaskAgent, "myproj-worktree"},
		{agent.CommitAgent, "myproj-worktree"},
	}
	for _, tc := range cases {
		t.Run(tc.kind.String(), func(t *testing.T) {
			if got := sandboxNameFor(tc.kind, projectPath); got != tc.want {
				t.Errorf("sandboxNameFor = %q, want %q", got, tc.want)
			}
		})
	}
	if got := sandboxNameFor(agent.PlanningAgent, ""); got != "" {
		t.Errorf("empty projectPath should produce empty name, got %q", got)
	}
}

func TestSandboxCreatorInvokedAndWraps(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	_ = os.WriteFile(projectPath, nil, 0o644)

	type call struct {
		name       string
		workspaces []string
	}
	var sbCalls []call
	launcher := &fakeLauncher{}
	r := &Runner{
		Launcher: launcher,
		Command:  func(_ agent.Kind, _ string) []string { return []string{"claude", "--print", "hi"} },
		Sandbox: func(_ context.Context, name string, workspaces []string) error {
			sbCalls = append(sbCalls, call{name, append([]string(nil), workspaces...)})
			return nil
		},
	}
	_, err := r.Start(context.Background(), Plan{
		Kind:        agent.PlanningAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
		Branch:      "feat/x",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantName := filepath.Base(workingDir)
	if len(sbCalls) != 1 || sbCalls[0].name != wantName ||
		len(sbCalls[0].workspaces) != 1 || sbCalls[0].workspaces[0] != workingDir {
		t.Fatalf("sandbox creator calls = %+v; want one with name=%q workspaces=[%q]", sbCalls, wantName, workingDir)
	}

	cmd := launcher.last.Command
	wantPrefix := []string{"sbx", "exec", "-it", "-w", workingDir, wantName}
	if len(cmd) < len(wantPrefix) {
		t.Fatalf("wrapped command too short: %v", cmd)
	}
	for i, w := range wantPrefix {
		if cmd[i] != w {
			t.Errorf("cmd[%d] = %q, want %q (full cmd: %v)", i, cmd[i], w, cmd)
		}
	}
	if !contains(cmd, "claude") {
		t.Errorf("wrapped command should still include claude: %v", cmd)
	}
}

func TestTaskAgentSandboxMountsOrchDir(t *testing.T) {
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, ".project.yaml")
	_ = os.WriteFile(projectPath, nil, 0o644)

	type call struct {
		name       string
		workspaces []string
	}
	var sbCalls []call
	launcher := &fakeLauncher{}
	r := &Runner{
		Workspaces: workspace.NewStub(wsRoot),
		Launcher:   launcher,
		Command:    func(_ agent.Kind, _ string) []string { return []string{"claude", "--print", "hi"} },
		Sandbox: func(_ context.Context, name string, workspaces []string) error {
			sbCalls = append(sbCalls, call{name, append([]string(nil), workspaces...)})
			return nil
		},
	}
	_, err := r.Start(context.Background(), Plan{
		Kind:        agent.TaskAgent,
		Branch:      "feat-x",
		ProjectPath: projectPath,
		TaskPath:    filepath.Join(orchDir, "tasks", "first.yaml"),
		TaskName:    "first",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantName := filepath.Base(orchDir) + "-worktree"
	wantWorktree := filepath.Join(wsRoot, "feat-x")
	if len(sbCalls) != 1 {
		t.Fatalf("expected one sandbox create, got %d: %+v", len(sbCalls), sbCalls)
	}
	got := sbCalls[0]
	if got.name != wantName {
		t.Errorf("sandbox name = %q, want %q", got.name, wantName)
	}
	if len(got.workspaces) != 2 || got.workspaces[0] != wantWorktree || got.workspaces[1] != orchDir {
		t.Errorf("workspaces = %v, want [%q %q]", got.workspaces, wantWorktree, orchDir)
	}
}

func TestProjectAgentSkipsSandbox(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	_ = os.WriteFile(projectPath, nil, 0o644)

	launcher := &fakeLauncher{}
	r := &Runner{
		Launcher: launcher,
		Command:  func(_ agent.Kind, _ string) []string { return []string{"claude", "hi"} },
		Sandbox: func(_ context.Context, _ string, _ []string) error {
			t.Fatal("sandbox creator must not be called for the project agent")
			return nil
		},
	}
	if _, err := r.Start(context.Background(), Plan{
		Kind:        agent.ProjectAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if launcher.last.Command[0] == "sbx" {
		t.Errorf("project agent command must not be wrapped in sbx exec: %v", launcher.last.Command)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestProjectAgentSkipsWorkspaceCreate(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, ".project.yaml")
	_ = os.WriteFile(projectPath, nil, 0o644)

	launcher := &fakeLauncher{}
	var auditBuf bytes.Buffer
	r := &Runner{
		// Workspaces left nil on purpose — ProjectAgent must not need it.
		Launcher: launcher,
		Audit:    audit.New(&auditBuf),
		Command:  func(k agent.Kind, ws string) []string { return []string{"sh", "-c", "exit 0"} },
	}

	sess, err := r.Start(context.Background(), Plan{
		Kind:        agent.ProjectAgent,
		WorkingDir:  workingDir,
		ProjectPath: projectPath,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sess.Name() == "" {
		t.Error("session has no name")
	}
	if launcher.last.Workspace != workingDir {
		t.Errorf("Spec.Workspace = %q, want %q", launcher.last.Workspace, workingDir)
	}

	for _, want := range []string{"context.yaml", "instructions.md"} {
		p := filepath.Join(workingDir, ".orch", want)
		if _, err := os.Stat(p); err != nil {
			t.Errorf(".orch/%s missing: %v", want, err)
		}
	}
	if !strings.Contains(auditBuf.String(), "session_started") {
		t.Errorf("audit log missing session_started:\n%s", auditBuf.String())
	}
}

func TestNonProjectAgentRequiresWorkspaces(t *testing.T) {
	r := &Runner{Launcher: &fakeLauncher{}}
	_, err := r.Start(context.Background(), Plan{
		Kind:   agent.TaskAgent,
		Branch: "feat/x",
	})
	if err == nil || !strings.Contains(err.Error(), "workspace manager") {
		t.Errorf("expected workspace-manager error, got %v", err)
	}
}

func TestTaskAgentUsesWorkspaceManager(t *testing.T) {
	root := t.TempDir()
	mgr := workspace.NewStub(root)
	launcher := &fakeLauncher{}

	r := &Runner{
		Workspaces: mgr,
		Launcher:   launcher,
		Command:    func(k agent.Kind, ws string) []string { return []string{"sh", "-c", "exit 0"} },
	}

	_, err := r.Start(context.Background(), Plan{
		Kind:     agent.TaskAgent,
		Branch:   "feat/healthz-probe",
		Repos:    []workspace.Repo{{Shortname: "gateway"}},
		TaskPath: "/x/tasks/01.yaml",
		TaskName: "add-healthz-handler",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := filepath.Join(root, "feat/healthz-probe")
	if launcher.last.Workspace != want {
		t.Errorf("Spec.Workspace = %q, want %q", launcher.last.Workspace, want)
	}
	if _, err := os.Stat(filepath.Join(want, ".orch", "context.yaml")); err != nil {
		t.Errorf("context.yaml not written in workspace: %v", err)
	}
}

func TestSessionNamePrefersTaskNameForTaskKinds(t *testing.T) {
	cases := []struct {
		desc string
		plan Plan
		want string
	}{
		{
			desc: "task agent with task name beats branch in the window name",
			plan: Plan{Kind: agent.TaskAgent, Branch: "feat/self-contained", TaskName: "explore-self-contained"},
			want: "task-explore-self-contained",
		},
		{
			desc: "commit agent with task name",
			plan: Plan{Kind: agent.CommitAgent, Branch: "feat/x", TaskName: "wire-it-up"},
			want: "commit-wire-it-up",
		},
		{
			desc: "planning agent has no task name so branch wins",
			plan: Plan{Kind: agent.PlanningAgent, Branch: "feat/x"},
			want: "planning-feat/x",
		},
		{
			desc: "task agent without task name falls back to branch",
			plan: Plan{Kind: agent.TaskAgent, Branch: "feat/x"},
			want: "task-feat/x",
		},
		{
			desc: "explicit SessionName overrides everything",
			plan: Plan{Kind: agent.TaskAgent, Branch: "feat/x", TaskName: "wire", SessionName: "task-custom"},
			want: "task-custom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			if got := sessionName(tc.plan); got != tc.want {
				t.Errorf("sessionName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlanWithoutWorkingDirOrBranch(t *testing.T) {
	r := &Runner{Launcher: &fakeLauncher{}}
	_, err := r.Start(context.Background(), Plan{Kind: agent.ProjectAgent})
	if err == nil {
		t.Error("expected error when neither WorkingDir nor Branch is set")
	}
}

func TestPlanningAgentRunsInProjectRoot(t *testing.T) {
	workingDir := t.TempDir()
	launcher := &fakeLauncher{}
	r := &Runner{
		// Workspaces nil on purpose: PlanningAgent must use WorkingDir.
		Launcher: launcher,
		Command:  func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "exit 0"} },
	}
	_, err := r.Start(context.Background(), Plan{
		Kind:        agent.PlanningAgent,
		WorkingDir:  workingDir,
		ProjectPath: filepath.Join(workingDir, ".project.yaml"),
		TasksDir:    filepath.Join(workingDir, "tasks"),
		Branch:      "feat/x",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if launcher.last.Workspace != workingDir {
		t.Errorf("Spec.Workspace = %q, want %q", launcher.last.Workspace, workingDir)
	}
}
