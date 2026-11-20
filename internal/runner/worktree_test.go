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

// The wolf gets the daemon's state dir as a read-only mount, after its control
// dir and source worktree, so it can read the live snapshot (X3). A read-only
// path nested in a writable mount is dropped. Other kinds ignore the field.
func TestWolfMountsStateDirReadOnly(t *testing.T) {
	p := Plan{Kind: agent.WolfAgent, ReadOnlyMounts: []string{"/home/.workingman/state", "/orch/p/sub", ""}}
	want := []string{"/orch/p", "/ws", "/home/.workingman/state" + ReadOnlySuffix}
	if got := sandboxWorkspaces(p, "/orch/p", "/ws"); !reflect.DeepEqual(got, want) {
		t.Errorf("wolf sandboxWorkspaces = %v, want %v", got, want)
	}
	p.Kind = agent.PlanningAgent
	if got := sandboxWorkspaces(p, "/orch/p", "/ws"); !reflect.DeepEqual(got, []string{"/orch/p", "/ws"}) {
		t.Errorf("planning sandboxWorkspaces = %v", got)
	}
}

// A task's hints reach its prompt and .orch/context.yaml, and the planner
// gets the limit and the repo dir names its touches globs start with
// (docs/parallel-task-worktrees.md H4).
func TestHintsReachPromptAndContext(t *testing.T) {
	orch := t.TempDir()
	projectPath := filepath.Join(orch, "project.yaml")
	_ = os.WriteFile(projectPath, nil, 0o644)
	stubRoot := t.TempDir()
	wt := filepath.Join(stubRoot, "feat/x", ".worktrees", "api")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Workspaces: workspace.NewStub(stubRoot),
		Launcher:   &fakeLauncher{},
		Command:    func(agent.Kind, string) []string { return []string{"true"} },
	}
	repos := []workspace.Repo{{Identity: "github.com/docker/gateway"}, {Shortname: "docs"}}
	touches := []string{"gateway/internal/api/**"}
	if _, err := r.Start(context.Background(), Plan{
		Kind: agent.TaskAgent, Branch: "feat/x", Repos: repos, ProjectPath: projectPath,
		TaskPath: filepath.Join(orch, "tasks", "api.yaml"), TaskName: "api",
		TaskWorktree: wt, TaskBranch: "wm/feat/x/api",
		Touches: &touches, Areas: []string{"go-deps"},
	}); err != nil {
		t.Fatalf("Start task: %v", err)
	}
	instr, _ := os.ReadFile(filepath.Join(wt, ".orch", "instructions.md"))
	if !strings.Contains(string(instr), "- `gateway/internal/api/**`") || !strings.Contains(string(instr), "- `go-deps`") {
		t.Errorf("task instructions missing the hints:\n%s", instr)
	}
	ctxYAML, _ := os.ReadFile(filepath.Join(wt, ".orch", "context.yaml"))
	for _, want := range []string{"touches:\n    - gateway/internal/api/**\n", "areas:\n    - go-deps\n"} {
		if !strings.Contains(string(ctxYAML), want) {
			t.Errorf("task context.yaml missing %q:\n%s", want, ctxYAML)
		}
	}
	if strings.Contains(string(ctxYAML), "repos:") {
		t.Errorf("only the planner's context lists the repos:\n%s", ctxYAML)
	}

	if _, err := r.Start(context.Background(), Plan{
		Kind: agent.PlanningAgent, WorkingDir: orch, Branch: "feat/x", Repos: repos,
		ProjectPath: projectPath, TasksDir: filepath.Join(orch, "tasks"), MaxParallelTasks: 2,
	}); err != nil {
		t.Fatalf("Start planning: %v", err)
	}
	instr, _ = os.ReadFile(filepath.Join(orch, ".orch", "instructions.md"))
	if !strings.Contains(string(instr), "directory name (`gateway`, `docs`)") {
		t.Errorf("planning instructions missing the repo dir names:\n%s", instr)
	}
	ctxYAML, _ = os.ReadFile(filepath.Join(orch, ".orch", "context.yaml"))
	for _, want := range []string{"max_parallel_tasks: 2\n", "repos:\n    - gateway\n    - docs\n"} {
		if !strings.Contains(string(ctxYAML), want) {
			t.Errorf("planning context.yaml missing %q:\n%s", want, ctxYAML)
		}
	}
}
