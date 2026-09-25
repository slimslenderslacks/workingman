package tui

import (
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

// TestRenderProjectDetailHeaderListsRepos pins the header's repos line: every
// entry in ProjectView.Repos, formatted "org/name" and comma-joined, so the
// summary shows what a multi-repo project actually spans without opening
// .project.yaml. A project with no repos recorded yet (still being planned)
// gets no such line at all, rather than an empty "repos: " one.
func TestRenderProjectDetailHeaderListsRepos(t *testing.T) {
	v := ProjectView{
		Name: "alpha",
		Repos: []project.Repo{
			{Org: "docker", Name: "desktop"},
			{Org: "docker", Name: "cli"},
		},
	}
	got := strings.Join(renderProjectDetailHeader(v, 80), "\n")
	if !strings.Contains(got, "repos: docker/desktop, docker/cli") {
		t.Errorf("header missing repos line; got:\n%s", got)
	}

	none := strings.Join(renderProjectDetailHeader(ProjectView{Name: "alpha"}, 80), "\n")
	if strings.Contains(none, "repos:") {
		t.Errorf("header should omit the repos line with no repos recorded; got:\n%s", none)
	}
}

// TestRenderProjectDetailTasksShowsCommittedRepos pins the task graph's
// per-task commit line: the repos a committed task's commits landed in (task
// file field `commits[].repo`, a workspace-relative repo dir name — not
// "org/name", see commit.tmpl), comma-joined beneath its status row. A task
// with no commits recorded gets no such line.
func TestRenderProjectDetailTasksShowsCommittedRepos(t *testing.T) {
	v := ProjectView{Tasks: []TaskView{
		{
			Name:   "fix-flake",
			Status: task.StatusCommitted,
			Commits: []task.Commit{
				{Repo: "desktop", Hash: "abc123"},
				{Repo: "cli", Hash: "def456"},
			},
		},
		{Name: "still-running", Status: task.StatusRunning},
	}}
	lines := renderProjectDetailTasks(v, 80)
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "⎇ desktop, cli") {
		t.Errorf("task graph missing committed-repos line; got:\n%s", got)
	}

	// The commit line must be attached to fix-flake, not floating free — it
	// should immediately follow that task's own status row.
	for i, l := range lines {
		if strings.Contains(l, "fix-flake") {
			if i+1 >= len(lines) || !strings.Contains(lines[i+1], "⎇ desktop, cli") {
				t.Fatalf("commit line did not immediately follow fix-flake's row; lines:\n%s", got)
			}
		}
	}

	if strings.Count(got, "⎇") != 1 {
		t.Errorf("still-running (no commits) should not get a commit line; got:\n%s", got)
	}
}
