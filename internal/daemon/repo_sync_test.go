package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// recordingManager is a workspace.Manager whose AddRepos calls are recorded
// (and optionally made to fail) for repo_sync_test.go's assertions. Create/
// Remove are unused by syncPlannedRepos and just return harmlessly. Path
// reports the workspace as not existing unless noWorkspaceYet is false —
// syncPlannedRepos's adoption path (adoptExistingWorkspace) branches on this
// to distinguish a genuinely new project from one whose workspace predates
// the last-planned snapshot feature.
type recordingManager struct {
	mu             sync.Mutex
	added          []workspace.Repo
	addErr         error
	addCalls       int
	noWorkspaceYet bool
}

func (m *recordingManager) Create(context.Context, string, []workspace.Repo) (string, error) {
	return "/tmp/ws", nil
}
func (m *recordingManager) Path(branch string) (string, error) {
	if m.noWorkspaceYet {
		return "", fmt.Errorf("wsp ls: workspace %q not found", branch)
	}
	return "/tmp/ws/" + branch, nil
}
func (m *recordingManager) Remove(context.Context, string, bool) error { return nil }

func (m *recordingManager) AddRepos(_ context.Context, _ string, repos []workspace.Repo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addCalls++
	m.added = append(m.added, repos...)
	return m.addErr
}

// newRepoSyncTestDaemon builds a daemon wired to mgr (or no runner at all
// when mgr is nil), suitable for calling syncPlannedRepos directly.
func newRepoSyncTestDaemon(t *testing.T, mgr workspace.Manager) (*Daemon, *safeBuf) {
	t.Helper()
	buf := &safeBuf{}
	a := audit.New(buf)
	var opts []Option
	if mgr != nil {
		opts = append(opts, WithRunner(&runner.Runner{Workspaces: mgr}))
	}
	d, err := New([]string{t.TempDir()}, a, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.ctx = context.Background()
	return d, buf
}

func TestSyncPlannedReposFirstPlanSeedsSnapshotWithoutAdding(t *testing.T) {
	mgr := &recordingManager{noWorkspaceYet: true}
	d, _ := newRepoSyncTestDaemon(t, mgr)

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Description: "widget", Branch: "feat/widget",
		Repos: []project.Repo{{Org: "docker", Name: "desktop"}},
	}

	changes := d.syncPlannedRepos(projectPath, p)
	if len(changes) != 0 {
		t.Errorf("changes = %v, want none on a first plan", changes)
	}
	if mgr.addCalls != 0 {
		t.Errorf("AddRepos called %d times, want 0 on a genuinely new project (Create handles it)", mgr.addCalls)
	}

	snap, err := project.LoadPlannedSnapshot(project.PlannedSnapshotPath(projectPath))
	if err != nil {
		t.Fatalf("LoadPlannedSnapshot: %v", err)
	}
	if snap.Description != p.Description || len(snap.Repos) != 1 || snap.Repos[0] != p.Repos[0] {
		t.Errorf("snapshot = %+v, want it seeded from p", snap)
	}
}

// TestSyncPlannedReposAdoptsPreExistingWorkspace is the regression for a
// project that already had a wsp workspace before the last-planned snapshot
// feature existed: no snapshot yet does NOT mean "Create will handle
// everything" in that case (Create no-ops on an existing workspace), so
// every current repo must be sent through AddRepos once, unconditionally,
// to catch anything the original Create call never saw.
func TestSyncPlannedReposAdoptsPreExistingWorkspace(t *testing.T) {
	mgr := &recordingManager{noWorkspaceYet: false}
	d, buf := newRepoSyncTestDaemon(t, mgr)

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Description: "widget", Branch: "feat/widget",
		Repos: []project.Repo{
			{Org: "docker", Name: "desktop"},
			{Org: "docker", Name: "cli"},
		},
	}

	changes := d.syncPlannedRepos(projectPath, p)
	if len(changes) != 0 {
		t.Errorf("changes = %v, want none on the adoption cycle itself (nothing to diff against yet)", changes)
	}
	if mgr.addCalls != 1 {
		t.Fatalf("AddRepos called %d times, want 1", mgr.addCalls)
	}
	if len(mgr.added) != 2 {
		t.Errorf("AddRepos got %+v, want both current repos sent through unconditionally", mgr.added)
	}
	if !strings.Contains(buf.String(), "repo_sync_adopted") {
		t.Errorf("expected repo_sync_adopted in audit:\n%s", buf.String())
	}

	snap, err := project.LoadPlannedSnapshot(project.PlannedSnapshotPath(projectPath))
	if err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}
	if len(snap.Repos) != 2 {
		t.Errorf("snapshot repos = %+v, want both recorded after adoption", snap.Repos)
	}
}

// TestSyncPlannedReposAdoptionFailureDoesNotSeedSnapshot guards against
// silently believing the adoption succeeded when AddRepos actually failed:
// the snapshot must stay unseeded so the whole adoption is retried in full
// next cycle, rather than the project looking "already planned" while its
// workspace is still missing repos.
func TestSyncPlannedReposAdoptionFailureDoesNotSeedSnapshot(t *testing.T) {
	mgr := &recordingManager{noWorkspaceYet: false, addErr: errors.New("clone failed")}
	d, buf := newRepoSyncTestDaemon(t, mgr)

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Branch: "feat/widget",
		Repos:  []project.Repo{{Org: "docker", Name: "desktop"}},
	}

	d.syncPlannedRepos(projectPath, p)
	if !strings.Contains(buf.String(), "repo_sync_error") {
		t.Errorf("expected repo_sync_error in audit:\n%s", buf.String())
	}
	if _, err := project.LoadPlannedSnapshot(project.PlannedSnapshotPath(projectPath)); err == nil {
		t.Errorf("snapshot should not have been seeded after a failed adoption")
	}
}

func TestSyncPlannedReposClonesNewlyAddedRepo(t *testing.T) {
	mgr := &recordingManager{}
	d, buf := newRepoSyncTestDaemon(t, mgr)

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SavePlannedSnapshot(project.PlannedSnapshotPath(projectPath), &project.PlannedSnapshot{
		Description: "widget", Branch: "feat/widget",
		Repos: []project.Repo{{Org: "docker", Name: "desktop"}},
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	p := &project.Project{
		Description: "widget", Branch: "feat/widget",
		Repos: []project.Repo{
			{Org: "docker", Name: "desktop"},
			{Org: "docker", Name: "cli"},
		},
	}

	changes := d.syncPlannedRepos(projectPath, p)
	if !containsSubstring(changes, "repos: added docker/cli") {
		t.Errorf("changes = %v, want it to mention the added repo", changes)
	}
	if mgr.addCalls != 1 {
		t.Fatalf("AddRepos called %d times, want 1", mgr.addCalls)
	}
	if len(mgr.added) != 1 || mgr.added[0].Identity != "github.com/docker/cli" {
		t.Errorf("AddRepos got %+v, want just github.com/docker/cli", mgr.added)
	}
	if !strings.Contains(buf.String(), "repo_sync_added") {
		t.Errorf("expected repo_sync_added in audit:\n%s", buf.String())
	}

	snap, err := project.LoadPlannedSnapshot(project.PlannedSnapshotPath(projectPath))
	if err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}
	if len(snap.Repos) != 2 {
		t.Errorf("snapshot repos = %+v, want both repos recorded after a successful add", snap.Repos)
	}
}

func TestSyncPlannedReposNoChangesIsNoop(t *testing.T) {
	mgr := &recordingManager{}
	d, _ := newRepoSyncTestDaemon(t, mgr)

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Description: "widget", Branch: "feat/widget",
		Repos: []project.Repo{{Org: "docker", Name: "desktop"}},
	}
	if err := project.SavePlannedSnapshot(project.PlannedSnapshotPath(projectPath), project.SnapshotFromProject(p)); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	changes := d.syncPlannedRepos(projectPath, p)
	if len(changes) != 0 {
		t.Errorf("changes = %v, want none", changes)
	}
	if mgr.addCalls != 0 {
		t.Errorf("AddRepos called %d times, want 0 when nothing changed", mgr.addCalls)
	}
}

func TestSyncPlannedReposFailedAddDoesNotAdvanceSnapshot(t *testing.T) {
	mgr := &recordingManager{addErr: errors.New("clone failed: network unreachable")}
	d, buf := newRepoSyncTestDaemon(t, mgr)

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")
	original := &project.PlannedSnapshot{
		Description: "widget", Branch: "feat/widget",
		Repos: []project.Repo{{Org: "docker", Name: "desktop"}},
	}
	if err := project.SavePlannedSnapshot(project.PlannedSnapshotPath(projectPath), original); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	p := &project.Project{
		Description: "widget", Branch: "feat/widget",
		Repos: []project.Repo{
			{Org: "docker", Name: "desktop"},
			{Org: "docker", Name: "cli"},
		},
	}

	changes := d.syncPlannedRepos(projectPath, p)
	if !containsSubstring(changes, "repos: added docker/cli") {
		t.Errorf("changes = %v, want the attempted addition still reported", changes)
	}
	if !strings.Contains(buf.String(), "repo_sync_error") {
		t.Errorf("expected repo_sync_error in audit:\n%s", buf.String())
	}

	snap, err := project.LoadPlannedSnapshot(project.PlannedSnapshotPath(projectPath))
	if err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}
	if len(snap.Repos) != 1 {
		t.Errorf("snapshot repos = %+v, want the failed add left OUT so it's retried next cycle", snap.Repos)
	}
}

func TestSyncPlannedReposNoWorkspaceManagerStillReportsDiff(t *testing.T) {
	// A runner is wired up (kind == PlanningAgent would proceed to launch a
	// session) but its Workspaces field is nil — some test configurations run
	// this way deliberately (see runner_test.go's "planning agent must NOT
	// need a wsp workspace" cases).
	d, _ := newRepoSyncTestDaemon(t, nil)
	d.runner = &runner.Runner{}

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SavePlannedSnapshot(project.PlannedSnapshotPath(projectPath), &project.PlannedSnapshot{
		Branch: "feat/widget",
		Repos:  []project.Repo{{Org: "docker", Name: "desktop"}},
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	p := &project.Project{
		Branch: "feat/widget",
		Repos: []project.Repo{
			{Org: "docker", Name: "desktop"},
			{Org: "docker", Name: "cli"},
		},
	}

	changes := d.syncPlannedRepos(projectPath, p)
	if !containsSubstring(changes, "repos: added docker/cli") {
		t.Errorf("changes = %v, want the diff still reported with no workspace manager", changes)
	}
}

func TestSyncPlannedReposDescribesNonRepoChanges(t *testing.T) {
	mgr := &recordingManager{}
	d, _ := newRepoSyncTestDaemon(t, mgr)

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SavePlannedSnapshot(project.PlannedSnapshotPath(projectPath), &project.PlannedSnapshot{
		Description: "old description", Branch: "feat/widget",
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	p := &project.Project{Description: "new description", Branch: "feat/widget"}

	changes := d.syncPlannedRepos(projectPath, p)
	if !containsSubstring(changes, "description changed") {
		t.Errorf("changes = %v, want the description change reported", changes)
	}
	if mgr.addCalls != 0 {
		t.Errorf("AddRepos called %d times, want 0 for a non-repo change", mgr.addCalls)
	}

	snap, err := project.LoadPlannedSnapshot(project.PlannedSnapshotPath(projectPath))
	if err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}
	if snap.Description != "new description" {
		t.Errorf("snapshot description = %q, want it advanced to the new value", snap.Description)
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
