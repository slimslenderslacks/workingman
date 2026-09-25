package project

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// PlannedSnapshot is the daemon's durable record of the "intent" fields a
// project carried the last time a planning agent successfully ran against
// it, stored at the path returned by PlannedSnapshotPath (a
// `last-planned.yaml` file next to `.project.yaml` and `tasks/`).
//
// It exists so the daemon can tell what a human changed in .project.yaml
// between planning cycles: repos.Repos/NewRepos additions get cloned into
// the already-provisioned wsp workspace via workspace.Manager.AddRepos
// (Create is a no-op once a workspace exists — see AddedRepos), and any
// other change gets surfaced to the planning agent as a human-readable diff
// (see Describe) so it doesn't have to guess what prompted a re-invocation.
//
// Deliberately daemon-only: unlike BlockedSession (which the wolf agent also
// writes), nothing but the daemon ever reads or writes this file, so there
// is no Writer/UpdatedBy field to arbitrate self-write loops.
type PlannedSnapshot struct {
	Description string     `yaml:"description"`
	Repos       []Repo     `yaml:"repos,omitempty"`
	NewRepos    []Repo     `yaml:"new_repos,omitempty"`
	Branch      string     `yaml:"branch"`
	Cron        string     `yaml:"cron,omitempty"`
	CronUntil   *time.Time `yaml:"cron_until,omitempty"`
	CronMaxRuns int        `yaml:"cron_max_runs,omitempty"`
}

// PlannedSnapshotPath returns the path to the last-planned record that
// belongs next to the .project.yaml at projectPath.
func PlannedSnapshotPath(projectPath string) string {
	return filepath.Join(filepath.Dir(projectPath), "last-planned.yaml")
}

// LoadPlannedSnapshot reads and parses the last-planned record at path. As
// with Load, a missing file is reported as an error satisfying
// errors.Is(err, fs.ErrNotExist) — callers for which "never planned before"
// is unremarkable (every caller today) check for that explicitly rather than
// treating it as a failure.
func LoadPlannedSnapshot(path string) (*PlannedSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return &PlannedSnapshot{}, nil
	}
	var s PlannedSnapshot
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &s, nil
}

// SavePlannedSnapshot writes the last-planned record at path.
func SavePlannedSnapshot(path string, s *PlannedSnapshot) error {
	data, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// SnapshotFromProject captures p's intent fields into a PlannedSnapshot, for
// the daemon to save once a planning session has successfully run against it.
func SnapshotFromProject(p *Project) *PlannedSnapshot {
	return &PlannedSnapshot{
		Description: p.Description,
		Repos:       append([]Repo{}, p.Repos...),
		NewRepos:    append([]Repo{}, p.NewRepos...),
		Branch:      p.Branch,
		Cron:        p.Cron,
		CronUntil:   p.CronUntil,
		CronMaxRuns: p.CronMaxRuns,
	}
}

// repoIdentity is the key AddedRepos and Describe match repos on: org/name,
// ignoring base_branch/visibility so those alone don't read as an addition.
func repoIdentity(r Repo) string { return r.Org + "/" + r.Name }

func reposByIdentity(repos []Repo) map[string]Repo {
	m := make(map[string]Repo, len(repos))
	for _, r := range repos {
		m[repoIdentity(r)] = r
	}
	return m
}

// AddedRepos returns the repos in p.Repos/p.NewRepos that were not part of
// this snapshot, split the same way the project itself splits them:
// addedExisting for entries from p.Repos (already exist, just need cloning
// into the workspace), addedNew for entries from p.NewRepos (need creating
// on the remote first, same as a fresh Create would do). A repo present
// under either list in the snapshot counts as already accounted for, so
// moving a repo from new_repos to repos between cycles is not reported as
// an addition.
func (s *PlannedSnapshot) AddedRepos(p *Project) (addedExisting, addedNew []Repo) {
	have := reposByIdentity(append(append([]Repo{}, s.Repos...), s.NewRepos...))
	for _, r := range p.Repos {
		if _, ok := have[repoIdentity(r)]; !ok {
			addedExisting = append(addedExisting, r)
		}
	}
	for _, r := range p.NewRepos {
		if _, ok := have[repoIdentity(r)]; !ok {
			addedNew = append(addedNew, r)
		}
	}
	return addedExisting, addedNew
}

// Describe returns a human-readable line per intent field that changed
// between this snapshot and p — description, repos/new_repos additions and
// removals, branch, and cron — for the daemon to hand the planning agent so
// it knows what prompted this invocation instead of having to guess. Nil
// when nothing changed.
//
// Repo removals are reported here but not acted on: AddedRepos (and the
// daemon's use of it) only ever clones repos in, deliberately leaving
// removal — which could mean losing uncommitted work in that repo's
// worktree — to a human decision rather than an automatic wsp repo rm.
func (s *PlannedSnapshot) Describe(p *Project) []string {
	var changes []string
	if s.Description != p.Description {
		changes = append(changes, fmt.Sprintf("description changed:\n    was: %s\n    now: %s", s.Description, p.Description))
	}
	if lines := describeRepoChanges("repos", s.Repos, p.Repos); len(lines) > 0 {
		changes = append(changes, lines...)
	}
	if lines := describeRepoChanges("new_repos", s.NewRepos, p.NewRepos); len(lines) > 0 {
		changes = append(changes, lines...)
	}
	if s.Branch != p.Branch {
		changes = append(changes, fmt.Sprintf("branch changed from %q to %q", s.Branch, p.Branch))
	}
	if s.Cron != p.Cron {
		changes = append(changes, fmt.Sprintf("cron changed from %q to %q", s.Cron, p.Cron))
	}
	return changes
}

// describeRepoChanges reports additions/removals between two Repo lists
// under the given field name ("repos" or "new_repos"), matched by identity.
func describeRepoChanges(field string, before, after []Repo) []string {
	oldSet, newSet := reposByIdentity(before), reposByIdentity(after)
	var added, removed []string
	for id := range newSet {
		if _, ok := oldSet[id]; !ok {
			added = append(added, id)
		}
	}
	for id := range oldSet {
		if _, ok := newSet[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	var out []string
	if len(added) > 0 {
		out = append(out, fmt.Sprintf("%s: added %s", field, strings.Join(added, ", ")))
	}
	if len(removed) > 0 {
		out = append(out, fmt.Sprintf("%s: removed %s", field, strings.Join(removed, ", ")))
	}
	return out
}
