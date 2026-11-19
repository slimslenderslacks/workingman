package prompts

import (
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
)

func worktreeData() Data {
	return Data{
		Workspace:    "/ws/.worktrees/api",
		Branch:       "feat/x",
		TaskPath:     "/orch/p/tasks/api.yaml",
		TaskName:     "api",
		TaskWorktree: "/ws/.worktrees/api",
		TaskBranch:   "wm/feat/x/api",
		TaskBase:     []RepoCommit{{Repo: "gateway", Hash: "abc123"}},
	}
}

func TestRenderTaskWorktreeFirstAttempt(t *testing.T) {
	out, err := Render(agent.TaskAgent, worktreeData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"own worktree", "`wm/feat/x/api`", "never touch `feat/x`"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "previous attempt") || strings.Contains(out, "This is attempt") {
		t.Errorf("a first attempt must not carry the retry block:\n%s", out)
	}
	if strings.Contains(out, "Do not commit") {
		t.Errorf("a worktree task may commit on its own branch:\n%s", out)
	}
}

// A retry runs in the worktree the failed attempt left, dirty or not, and the
// prompt has to say so: what may be there, how to look, how to undo it.
func TestRenderTaskWorktreeRetryBlock(t *testing.T) {
	d := worktreeData()
	d.Attempt = 1
	out, err := Render(agent.TaskAgent, d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"This is attempt 2",
		"committed and uncommitted changes",
		"git -C /ws/.worktrees/api/gateway status",
		"git -C /ws/.worktrees/api/gateway diff abc123",
		"log --oneline abc123..HEAD",
		"Keep, fix or undo",
		"git reset --hard <base>",
		"git clean -fd",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("retry prompt missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTaskWorktreeResumedBlock(t *testing.T) {
	d := worktreeData()
	d.Resumed = true
	out, err := Render(agent.TaskAgent, d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "interrupted") || !strings.Contains(out, "Keep, fix or undo") {
		t.Errorf("resumed prompt missing the reuse note:\n%s", out)
	}
}

// The legacy task (no worktree) keeps today's wording, retry or not.
func TestRenderTaskLegacyUnchanged(t *testing.T) {
	out, err := Render(agent.TaskAgent, Data{Workspace: "/ws", Branch: "feat/x", TaskPath: "/t.yaml", TaskName: "t", Attempt: 2})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "all on branch `feat/x`") || !strings.Contains(out, "Do not commit, push") {
		t.Errorf("legacy prompt changed:\n%s", out)
	}
	if strings.Contains(out, "worktree") {
		t.Errorf("legacy prompt mentions worktrees:\n%s", out)
	}
}

func TestRenderCommitWorktree(t *testing.T) {
	d := worktreeData()
	d.Workspace = "/ws"
	out, err := Render(agent.CommitAgent, d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"Commit leftovers",
		"git -C /ws/.worktrees/api/<repo> status --porcelain",
		"`gateway`: `abc123`",
		"merge --ff-only wm/feat/x/api",
		"Do NOT\n   rebase",
		"status: blocked",
		"Do not push",
		"status: committed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("worktree commit prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sitting in the\nworking tree of each repo in this workspace") {
		t.Errorf("worktree commit prompt kept the legacy framing:\n%s", out)
	}

	legacy, err := Render(agent.CommitAgent, Data{Workspace: "/ws", Branch: "feat/x", TaskPath: "/t.yaml"})
	if err != nil {
		t.Fatalf("Render legacy: %v", err)
	}
	if strings.Contains(legacy, "ff-only") || !strings.Contains(legacy, "sitting in the\nworking tree") {
		t.Errorf("legacy commit prompt changed:\n%s", legacy)
	}
}
