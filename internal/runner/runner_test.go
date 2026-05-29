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
	// Every kind now runs claude interactively so a human can attach to the
	// tmux window, watch the output stream, and respond if claude prompts.
	// --print is never added; the initial prompt is passed on argv either way
	// so the agent starts work immediately.
	allKinds := []agent.Kind{
		agent.ProjectAgent,
		agent.PlanningAgent,
		agent.TaskAgent,
		agent.WolfAgent,
		agent.CommitAgent,
	}
	for _, k := range allKinds {
		cmd := DefaultCommandBuilder(k, "/ws")
		if contains(cmd, "--print") {
			t.Errorf("%s must NOT use --print (every kind is now interactive), got %v", k, cmd)
		}
		if !contains(cmd, "--dangerously-skip-permissions") {
			t.Errorf("%s missing --dangerously-skip-permissions: %v", k, cmd)
		}
		if !contains(cmd, initialPrompt) {
			t.Errorf("%s missing initial prompt: %v", k, cmd)
		}
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
