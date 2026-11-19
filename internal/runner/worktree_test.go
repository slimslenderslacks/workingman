package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// A task agent with a task worktree runs in it and mounts only it, the repos'
// git common dirs and the control dir — never the shared working trees
// (docs/parallel-task-worktrees.md W5). The commit agent keeps the shared
// workspace as its cwd, in a sandbox of its own.
func TestTaskWorktreeWorkingDirAndMounts(t *testing.T) {
	orch := t.TempDir()
	projectPath := filepath.Join(orch, "project.yaml")
	_ = os.WriteFile(projectPath, nil, 0o644)
	stubRoot := t.TempDir()
	shared := filepath.Join(stubRoot, "feat/x")
	wt := filepath.Join(shared, ".worktrees", "api")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(shared, "gateway", ".git")
	cache := filepath.Join(shared, ".gomodcache")

	var mounts [][]string
	var names []string
	launcher := &fakeLauncher{}
	r := &Runner{
		Workspaces: workspace.NewStub(stubRoot),
		Launcher:   launcher,
		Command:    func(agent.Kind, string) []string { return []string{"true"} },
		Sandbox: func(_ context.Context, spec SandboxSpec) error {
			names = append(names, spec.Name)
			mounts = append(mounts, append([]string(nil), spec.Workspaces...))
			return nil
		},
	}
	base := Plan{
		Branch:       "feat/x",
		Repos:        []workspace.Repo{{Identity: "github.com/docker/gateway"}},
		ProjectPath:  projectPath,
		TasksDir:     filepath.Join(orch, "tasks"),
		TaskPath:     filepath.Join(orch, "tasks", "api.yaml"),
		TaskName:     "api",
		TaskWorktree: wt,
		TaskBranch:   "wm/feat/x/api",
		TaskBase:     []task.Commit{{Repo: "gateway", Hash: "abc123"}},
	}

	taskPlan := base
	taskPlan.Kind = agent.TaskAgent
	taskPlan.Attempt = 1
	taskPlan.WorktreeMounts = []string{gitDir, cache}
	if _, err := r.Start(context.Background(), taskPlan); err != nil {
		t.Fatalf("Start task: %v", err)
	}
	if launcher.last.Workspace != wt {
		t.Errorf("task cwd = %q, want the worktree %q", launcher.last.Workspace, wt)
	}
	if want := []string{wt, gitDir, cache, orch}; !reflect.DeepEqual(mounts[0], want) {
		t.Errorf("task mounts = %v, want %v", mounts[0], want)
	}
	instr, err := os.ReadFile(filepath.Join(wt, ".orch", "instructions.md"))
	if err != nil {
		t.Fatalf("instructions not written into the worktree: %v", err)
	}
	if !strings.Contains(string(instr), "wm/feat/x/api") || !strings.Contains(string(instr), "This is attempt 2") {
		t.Errorf("task instructions missing the worktree/retry wording:\n%s", instr)
	}
	ctxYAML, _ := os.ReadFile(filepath.Join(wt, ".orch", "context.yaml"))
	for _, want := range []string{"task_worktree: " + wt, "task_branch: wm/feat/x/api", "attempt: 1"} {
		if !strings.Contains(string(ctxYAML), want) {
			t.Errorf("context.yaml missing %q:\n%s", want, ctxYAML)
		}
	}
	if _, err := os.Stat(filepath.Join(shared, ".orch", "instructions.md")); err == nil {
		t.Error("the task agent's handoff must not land in the shared workspace root")
	}

	commitPlan := base
	commitPlan.Kind = agent.CommitAgent
	commitPlan.WorktreeMounts = []string{gitDir} // ignored for the commit agent
	if _, err := r.Start(context.Background(), commitPlan); err != nil {
		t.Fatalf("Start commit: %v", err)
	}
	if launcher.last.Workspace != shared {
		t.Errorf("commit cwd = %q, want the shared workspace %q", launcher.last.Workspace, shared)
	}
	if want := []string{shared, orch}; !reflect.DeepEqual(mounts[1], want) {
		t.Errorf("commit mounts = %v, want %v", mounts[1], want)
	}
	if names[0] == names[1] || !strings.HasSuffix(names[1], "-commit") {
		t.Errorf("sandbox names task=%q commit=%q; the commit agent needs its own", names[0], names[1])
	}
	instr, _ = os.ReadFile(filepath.Join(shared, ".orch", "instructions.md"))
	if !strings.Contains(string(instr), "merge --ff-only wm/feat/x/api") {
		t.Errorf("commit instructions missing the worktree branch:\n%s", instr)
	}
}

// Without a TaskWorktree the task agent keeps the legacy shape: the shared
// workspace plus the control dir.
func TestTaskWithoutWorktreeKeepsSharedWorkspace(t *testing.T) {
	orch := t.TempDir()
	projectPath := filepath.Join(orch, "project.yaml")
	p := Plan{Kind: agent.TaskAgent, ProjectPath: projectPath, WorktreeMounts: []string{"/x/.git"}}
	if got, want := sandboxWorkspaces(p, "/ws", ""), []string{"/ws", orch}; !reflect.DeepEqual(got, want) {
		t.Errorf("sandboxWorkspaces = %v, want %v", got, want)
	}
}
