package project

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlannedSnapshotPath(t *testing.T) {
	got := PlannedSnapshotPath("/orch/widget/.project.yaml")
	want := filepath.Join("/orch/widget", "last-planned.yaml")
	if got != want {
		t.Errorf("PlannedSnapshotPath = %q, want %q", got, want)
	}
}

func TestLoadPlannedSnapshotMissingFile(t *testing.T) {
	_, err := LoadPlannedSnapshot(filepath.Join(t.TempDir(), "last-planned.yaml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestSaveAndLoadPlannedSnapshotRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last-planned.yaml")
	s := &PlannedSnapshot{
		Description: "ship the widget",
		Repos:       []Repo{{Org: "docker", Name: "desktop"}},
		NewRepos:    []Repo{{Org: "docker", Name: "widget-tui", Visibility: "public"}},
		Branch:      "feat/widget",
		Cron:        "@every 1h",
		CronMaxRuns: 10,
	}
	if err := SavePlannedSnapshot(path, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadPlannedSnapshot(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Description != s.Description || got.Branch != s.Branch || got.Cron != s.Cron || got.CronMaxRuns != s.CronMaxRuns {
		t.Errorf("round-tripped snapshot = %+v, want %+v", got, s)
	}
	if len(got.Repos) != 1 || got.Repos[0] != s.Repos[0] {
		t.Errorf("repos = %+v, want %+v", got.Repos, s.Repos)
	}
	if len(got.NewRepos) != 1 || got.NewRepos[0] != s.NewRepos[0] {
		t.Errorf("new_repos = %+v, want %+v", got.NewRepos, s.NewRepos)
	}
}

func TestAddedReposFindsOnlyNewOnes(t *testing.T) {
	snapshot := &PlannedSnapshot{
		Repos:    []Repo{{Org: "docker", Name: "desktop"}},
		NewRepos: []Repo{{Org: "docker", Name: "widget-tui"}},
	}
	p := &Project{
		Repos: []Repo{
			{Org: "docker", Name: "desktop"}, // unchanged
			{Org: "docker", Name: "cli"},     // added
		},
		NewRepos: []Repo{
			{Org: "docker", Name: "widget-tui"}, // unchanged
			{Org: "docker", Name: "brand-new"},  // added
		},
	}
	addedExisting, addedNew := snapshot.AddedRepos(p)
	if len(addedExisting) != 1 || addedExisting[0].Name != "cli" {
		t.Errorf("addedExisting = %+v, want just docker/cli", addedExisting)
	}
	if len(addedNew) != 1 || addedNew[0].Name != "brand-new" {
		t.Errorf("addedNew = %+v, want just docker/brand-new", addedNew)
	}
}

func TestAddedReposEmptyWhenNothingChanged(t *testing.T) {
	snapshot := &PlannedSnapshot{
		Repos: []Repo{{Org: "docker", Name: "desktop"}},
	}
	p := &Project{
		Repos: []Repo{{Org: "docker", Name: "desktop"}},
	}
	addedExisting, addedNew := snapshot.AddedRepos(p)
	if len(addedExisting) != 0 || len(addedNew) != 0 {
		t.Errorf("expected no additions, got existing=%+v new=%+v", addedExisting, addedNew)
	}
}

func TestAddedReposRepoMovedBetweenListsIsNotAnAddition(t *testing.T) {
	snapshot := &PlannedSnapshot{
		NewRepos: []Repo{{Org: "docker", Name: "widget-tui"}},
	}
	// The repo moved from new_repos to repos (it got created since last plan)
	// — same identity, so it should not read as a new addition either way.
	p := &Project{
		Repos: []Repo{{Org: "docker", Name: "widget-tui"}},
	}
	addedExisting, addedNew := snapshot.AddedRepos(p)
	if len(addedExisting) != 0 || len(addedNew) != 0 {
		t.Errorf("expected no additions for a repo that only moved lists, got existing=%+v new=%+v", addedExisting, addedNew)
	}
}

func TestDescribeReportsEachChangedField(t *testing.T) {
	snapshot := &PlannedSnapshot{
		Description: "old description",
		Repos:       []Repo{{Org: "docker", Name: "desktop"}},
		Branch:      "feat/old",
		Cron:        "",
	}
	p := &Project{
		Description: "new description",
		Repos: []Repo{
			{Org: "docker", Name: "cli"}, // desktop removed, cli added
		},
		Branch: "feat/old",
		Cron:   "@every 1h",
	}
	changes := snapshot.Describe(p)

	wantSubstrings := []string{
		"description changed",
		"repos: added docker/cli",
		"repos: removed docker/desktop",
		`cron changed from "" to "@every 1h"`,
	}
	joined := strings.Join(changes, "\n")
	for _, want := range wantSubstrings {
		if !strings.Contains(joined, want) {
			t.Errorf("Describe() missing %q; got:\n%s", want, joined)
		}
	}
	// Branch didn't change, so it must not appear.
	if strings.Contains(joined, "branch changed") {
		t.Errorf("Describe() reported an unchanged branch:\n%s", joined)
	}
}

func TestDescribeEmptyWhenNothingChanged(t *testing.T) {
	snapshot := &PlannedSnapshot{
		Description: "x", Branch: "b",
		Repos: []Repo{{Org: "docker", Name: "desktop"}},
	}
	p := &Project{
		Description: "x", Branch: "b",
		Repos: []Repo{{Org: "docker", Name: "desktop"}},
	}
	if changes := snapshot.Describe(p); len(changes) != 0 {
		t.Errorf("Describe() = %v, want no changes", changes)
	}
}

func TestSnapshotFromProjectCapturesIntentFields(t *testing.T) {
	p := &Project{
		Description: "x",
		Repos:       []Repo{{Org: "docker", Name: "desktop"}},
		NewRepos:    []Repo{{Org: "docker", Name: "widget-tui"}},
		Branch:      "feat/x",
		Cron:        "@every 1h",
		CronMaxRuns: 5,
		// Non-intent fields that must NOT leak into the snapshot's diffing:
		Status:        StatusWorking,
		BlockedReason: "should not appear",
	}
	s := SnapshotFromProject(p)
	if s.Description != p.Description || s.Branch != p.Branch || s.Cron != p.Cron || s.CronMaxRuns != p.CronMaxRuns {
		t.Errorf("snapshot = %+v, want it to mirror p's intent fields", s)
	}
	if len(s.Repos) != 1 || s.Repos[0] != p.Repos[0] {
		t.Errorf("snapshot repos = %+v", s.Repos)
	}

	// Mutating the source project afterward must not retroactively change a
	// snapshot already taken (SnapshotFromProject must copy slices, not alias
	// them).
	p.Repos[0].Name = "mutated"
	if s.Repos[0].Name != "desktop" {
		t.Errorf("snapshot aliased the project's Repos slice; got %+v", s.Repos)
	}
}
