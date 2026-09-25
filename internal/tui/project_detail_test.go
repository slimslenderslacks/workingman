package tui

import (
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

// TestRenderProjectDetailBadgesShowsPRURLs pins the watching-PR badge's
// content: unlike the compact "org/name#123" the project card grid uses, the
// summary pane prints each open PR's actual URL as visible text (falling
// back to the canonical GitHub URL when the review agent didn't record one)
// — many terminals don't support OSC 8 hyperlinks, but virtually all of them
// auto-linkify plain URL text, so this is what makes the link visible and
// clickable rather than an opaque short label. It's still wrapped in an
// OSC 8 escape too, for terminals that do support it.
func TestRenderProjectDetailBadgesShowsPRURLs(t *testing.T) {
	v := ProjectView{
		WatchingPR: true,
		PullRequests: []project.PullRequest{
			{Repo: "docker/desktop", Number: 42, State: "open", URL: "https://github.com/docker/desktop/pull/42"},
			{Repo: "docker/cli", Number: 9, State: "open"},
			{Repo: "docker/cli", Number: 3, State: "merged"},
		},
	}
	got := strings.Join(renderProjectDetailBadges(v, 100), "\n")
	for _, want := range []string{
		"https://github.com/docker/desktop/pull/42",
		"https://github.com/docker/cli/pull/9",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("badges missing PR URL %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/pull/3") {
		t.Errorf("merged PR should not get a link; got:\n%s", got)
	}
	if want := hyperlink("https://github.com/docker/desktop/pull/42", "https://github.com/docker/desktop/pull/42"); !strings.Contains(got, want) {
		t.Errorf("PR URL should be wrapped in an OSC 8 hyperlink escape; got:\n%s", got)
	}
}

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
