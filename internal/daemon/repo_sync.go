package daemon

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// syncPlannedRepos is called once per planning launch, before the session
// starts. It compares the project's current repos/new_repos (and other
// intent fields) against the last-planned snapshot recorded the previous
// time this ran, and:
//
//   - clones any newly-added repo into the ALREADY-provisioned wsp workspace
//     via workspace.Manager.AddRepos — the counterpart to Create, which is a
//     no-op once the workspace exists (see workspace.Manager.AddRepos's doc).
//   - returns a human-readable line per changed intent field (description,
//     repos/new_repos, branch, cron) for the caller to forward into the
//     planning template as runner.Plan.ProjectChanges.
//
// A missing snapshot is ambiguous on its own: it means either a project's
// very first plan (Create is about to clone everything listed, so there is
// nothing to add and nothing to diff against yet) OR a project whose
// workspace already existed before this snapshot mechanism shipped (Create
// is a no-op for an existing workspace, so anything in repos/new_repos that
// isn't already cloned would otherwise be silently missed forever, and the
// gap would never resurface once the snapshot got seeded). adoptExistingWorkspace
// resolves the ambiguity by checking whether the workspace actually exists.
func (d *Daemon) syncPlannedRepos(projectPath string, p *project.Project) []string {
	snapPath := project.PlannedSnapshotPath(projectPath)
	snapshot, err := project.LoadPlannedSnapshot(snapPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("planned_snapshot_load_error", "path", snapPath, "err", err.Error())
		}
		return d.adoptExistingWorkspace(projectPath, snapPath, p)
	}

	changes := snapshot.Describe(p)
	addedExisting, addedNew := snapshot.AddedRepos(p)
	if len(addedExisting) == 0 && len(addedNew) == 0 {
		// Nothing to clone, but other fields may have changed — advance the
		// snapshot regardless so a later description/branch/cron edit diffs
		// against this state, not a stale one already reported once.
		if len(changes) > 0 {
			if err := project.SavePlannedSnapshot(snapPath, project.SnapshotFromProject(p)); err != nil {
				d.audit.Log("planned_snapshot_save_error", "path", snapPath, "err", err.Error())
			}
		}
		return changes
	}

	if d.runner.Workspaces == nil {
		// No workspace manager wired up (some test configurations run the
		// planning agent without one). Report the diff, but leave the
		// snapshot's repo lists untouched so the addition is retried once a
		// workspace manager IS available, instead of silently dropped.
		return changes
	}

	added := addedWorkspaceRepos(addedExisting, addedNew)
	if err := d.runner.Workspaces.AddRepos(d.ctx, p.Branch, added); err != nil {
		d.audit.Log("repo_sync_error", "path", projectPath, "branch", p.Branch, "err", err.Error())
		// Leave the snapshot's repo lists untouched: a failed clone must stay
		// "added but not yet accounted for" so it's retried next cycle rather
		// than silently disappearing from the diff.
		return changes
	}
	d.audit.Log("repo_sync_added", "path", projectPath, "branch", p.Branch,
		"count", fmt.Sprintf("%d", len(added)))

	if err := project.SavePlannedSnapshot(snapPath, project.SnapshotFromProject(p)); err != nil {
		d.audit.Log("planned_snapshot_save_error", "path", snapPath, "err", err.Error())
	}
	return changes
}

// adoptExistingWorkspace handles a project with no last-planned snapshot yet.
// If its wsp workspace doesn't exist either, this genuinely is the first
// plan: Create() is about to clone every listed repo, so there's nothing to
// do here but seed the snapshot for next time.
//
// If the workspace DOES already exist, this project predates the snapshot
// feature — its repos/new_repos may already include entries Create() never
// saw (it no-ops once a workspace exists). Every current repo is sent
// through AddRepos unconditionally rather than guessing which ones are
// missing: `wsp repo add` is confirmed idempotent for a repo already present
// ("already in workspace, skipping"), so this is safe even though it will
// usually be a full no-op in practice.
func (d *Daemon) adoptExistingWorkspace(projectPath, snapPath string, p *project.Project) []string {
	if d.runner.Workspaces != nil {
		if _, err := d.runner.Workspaces.Path(p.Branch); err == nil {
			all := addedWorkspaceRepos(p.Repos, p.NewRepos)
			if len(all) > 0 {
				if err := d.runner.Workspaces.AddRepos(d.ctx, p.Branch, all); err != nil {
					d.audit.Log("repo_sync_error", "path", projectPath, "branch", p.Branch, "err", err.Error())
					// Don't seed the snapshot on failure: leave this project
					// looking like it still has no snapshot, so the adoption
					// is retried in full next cycle instead of silently
					// believing it already happened.
					return nil
				}
				d.audit.Log("repo_sync_adopted", "path", projectPath, "branch", p.Branch,
					"count", fmt.Sprintf("%d", len(all)))
			}
		}
	}
	if err := project.SavePlannedSnapshot(snapPath, project.SnapshotFromProject(p)); err != nil {
		d.audit.Log("planned_snapshot_save_error", "path", snapPath, "err", err.Error())
	}
	return nil
}

// addedWorkspaceRepos converts newly-added project.Repo entries into
// workspace.Repo, mirroring workspaceReposFor's split: addedNew (sourced from
// a project's new_repos) is flagged Create so the workspace manager creates
// each one empty on the remote before cloning, exactly as a fresh Create
// would for the same entries.
func addedWorkspaceRepos(addedExisting, addedNew []project.Repo) []workspace.Repo {
	out := toWorkspaceRepos(addedExisting)
	for _, r := range addedNew {
		out = append(out, workspace.Repo{
			Identity:   "github.com/" + r.Org + "/" + r.Name,
			BaseBranch: r.BaseBranch,
			Create:     true,
			Visibility: r.Visibility,
		})
	}
	return out
}
