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
		"rebase onto[<repo>]",
		"git rebase --abort",
		"reset --hard\n     saved[<repo>]",
		"at most twice",
		"result: <conflict | verify_failed | partial | error>",
		"safe to run again from the top",
		"still equals its base",
		"status: blocked",
		"status: failed",
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

// The build check paragraph appears only when the project sets verify:, and a
// docs-only task is told to skip it.
func TestRenderCommitWorktreeBuildCheck(t *testing.T) {
	d := worktreeData()
	d.Workspace = "/ws"
	none, err := Render(agent.CommitAgent, d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(none, "Build check") {
		t.Errorf("no verify: configured, but the prompt has a build check:\n%s", none)
	}
	d.Verify = "go build ./..."
	out, err := Render(agent.CommitAgent, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Build check", "go build ./...", "result: verify_failed", "If nothing was rebased"} {
		if !strings.Contains(out, want) {
			t.Errorf("build check prompt missing %q:\n%s", want, out)
		}
	}
	d.DocsOnly = true
	docs, err := Render(agent.CommitAgent, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(docs, "documentation only") || strings.Contains(docs, "go build ./...") {
		t.Errorf("docs-only prompt should skip the check:\n%s", docs)
	}
}

// A conflict re-run is told what conflicted, what landed, and to rebase its
// own branch onto the shared branch.
func TestRenderTaskConflictReRun(t *testing.T) {
	d := worktreeData()
	d.Attempt = 1
	d.Integration = &IntegrationReport{Result: "conflict", Repo: "gateway", Files: []string{"api/handler.go"}, Landed: []string{"auth"}}
	out, err := Render(agent.TaskAgent, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"This run is a re-run", "conflicted in `gateway`", "`api/handler.go`", "`auth`",
		"git rebase refs/heads/feat/x", "git rebase --continue", "This is attempt 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("re-run prompt missing %q:\n%s", want, out)
		}
	}
	d.Integration = &IntegrationReport{Result: "verify_failed", Output: "undefined: Foo"}
	out, err = Render(agent.TaskAgent, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "build check\nthen failed") || !strings.Contains(out, "undefined: Foo") {
		t.Errorf("verify re-run prompt missing the failure:\n%s", out)
	}
	plain, _ := Render(agent.TaskAgent, worktreeData())
	if strings.Contains(plain, "re-run") {
		t.Errorf("an ordinary run must not carry the re-run block:\n%s", plain)
	}
}

// The wolf learns the three ways out of an integration block and is warned
// off the shared branch while an integration is in flight.
func TestRenderWolfIntegrationParagraph(t *testing.T) {
	out, err := Render(agent.WolfAgent, Data{Workspace: "/orch/p", ProjectPath: "/orch/p/project.yaml", Branch: "feat/x", Worktree: "/ws"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Integration conflicts",
		"`wm/feat/x/<task>`",
		"(`/ws`)",
		"`attempts: 0`",
		"reuses\n     the worktree",
		"`integration.result: partial`",
		"THE SHARED BRANCH",
		"do not commit, rebase, reset or otherwise move\n`feat/x`",
		"A block does not stop a commit agent",
		"set that task to `success`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wolf prompt missing %q:\n%s", want, out)
		}
	}
}

// With a state snapshot, the wolf's shared-branch check reads its
// live.integrating / live.commit_reserved fields instead of the task files
// (X3); without one it falls back to "any task at success".
func TestRenderWolfSharedBranchCheckUsesSnapshot(t *testing.T) {
	base := Data{Workspace: "/orch/p", ProjectPath: "/orch/p/project.yaml", Branch: "feat/x", Worktree: "/ws"}
	withSnap := base
	withSnap.SnapshotFile = "/home/.workingman/state/snapshot.json"
	out, err := Render(agent.WolfAgent, withSnap)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`/home/.workingman/state/snapshot.json`", "`live.integrating`", "`live.commit_reserved`", "`commit_queue`", "`generated_at`"} {
		if !strings.Contains(out, want) {
			t.Errorf("wolf prompt with a snapshot missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "while any\ntask is at `success`") {
		t.Errorf("wolf prompt with a snapshot still uses the task-file check:\n%s", out)
	}
	plain, err := Render(agent.WolfAgent, base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain, "while any\ntask is at `success`") || strings.Contains(plain, "live.integrating") {
		t.Errorf("wolf prompt without a snapshot should use the task files:\n%s", plain)
	}
}

// The wolf is told which tasks are still in flight, which one is integrating,
// and that a block neither stops them nor an integration already running
// (docs/parallel-task-worktrees.md X3). With nothing in flight the list is
// left out.
func TestRenderWolfInFlightTasks(t *testing.T) {
	base := Data{Workspace: "/orch/p", ProjectPath: "/orch/p/project.yaml", Branch: "feat/x"}
	withTasks := base
	withTasks.InFlightTasks = []InFlightTask{
		{Name: "api", Status: "running"},
		{Name: "docs", Status: "success", QueuePosition: 1, Integrating: true},
		{Name: "ui", Status: "success", QueuePosition: 2},
	}
	out, err := Render(agent.WolfAgent, withTasks)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"still in flight when you were launched",
		"  - api: running\n",
		"  - docs: integrating now\n",
		"  - ui: queued for integration (#2)\n",
		"Do not edit these tasks",
		"except for an integration that was already running",
		"appended to `blocked_reason`",
		"the commit agent was integrating task `docs`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wolf prompt missing %q:\n%s", want, out)
		}
	}
	plain, err := Render(agent.WolfAgent, base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "still in flight when you were launched") || strings.Contains(plain, "was integrating task") {
		t.Errorf("wolf prompt with nothing in flight should not list tasks:\n%s", plain)
	}
}
