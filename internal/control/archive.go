package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// WorkspaceRemover is the sliver of workspace.Manager [Archive] needs: tearing
// down the wsp workspace belonging to the work stream being archived, plus
// resolving where that workspace lives so the orchestrator's own `.orch`
// control directory can be cleared out of it first. Narrowing it to those two
// methods keeps callers decoupled from the concrete wsp driver and lets tests
// substitute a fake that records the branch it was asked to remove.
type WorkspaceRemover interface {
	Path(branch string) (string, error)
	Remove(ctx context.Context, branch string, force bool) error
}

// archiveWorkspaceTimeout bounds each worktree and `wsp rm` shell-out. The
// TUI runs the archive inline on its UI goroutine, so a wedged wsp would
// otherwise freeze it.
const archiveWorkspaceTimeout = 30 * time.Second

// BackupRoot is the archive destination root for a workspace root: the
// sibling "<root>.backup" directory (e.g. ~/orch -> ~/orch.backup).
func BackupRoot(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Clean(root) + ".backup"
}

// LoadArchivable loads the project at projectPath and reports whether it may be
// archived at all. Only a work stream the cleanup agent has already finished —
// `archive: true` in its project.yaml — is eligible; anything else is a
// refusal, including a project file that won't load (we can't tell whether it
// was cleaned up, so we don't touch it). Callers turn the error into a status
// message, so the wording is user-facing and points at the next step.
func LoadArchivable(projectPath string) (*project.Project, error) {
	if projectPath == "" {
		return nil, errors.New("no work stream selected")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return nil, err
	}
	if !p.Archive {
		return nil, fmt.Errorf("%s has not been cleaned up — run :cleanup first", DisplayName(projectPath))
	}
	return p, nil
}

// Archive moves the project whose project.yaml is at projectPath out of the
// workspace root and into the sibling backup root, keeping its directory name
// (e.g. ~/orch/weather-tui -> ~/orch.backup/weather-tui), and removes the
// project's wsp workspace along the way. Because it's a move, the project
// leaves the workspace root immediately and the daemon's project scan drops the
// entry on its next pass — no explicit cleanup needed.
//
// Only a cleaned-up project (`archive: true`) is archivable; see LoadArchivable.
//
// Refuses to overwrite an existing archive of the same name so a prior backup
// is never clobbered, and only archives a direct child of the configured root
// so a bad selection can't move an arbitrary directory.
//
// Ordering and failure policy: every validation runs first, then the workspace
// is torn down — its task worktrees and the project's `wm/` refs first
// (removeTaskWorktrees), then its `.orch` directory, then the workspace itself
// — and only then does the project directory move. Doing the removals first
// means a failure there aborts the archive with the project still in its
// original place — nothing is lost and the user can retry once wsp is healthy —
// whereas moving first would leave a half-archived work stream whose workspace
// still exists. Removal errors are returned rather than swallowed. wsp is nil
// when no workspace manager is wired in (standalone `orch tui`); both removals
// are skipped then, as they are for a project with no `branch`.
func Archive(root, projectPath string, wsp WorkspaceRemover) error {
	if root == "" {
		return errors.New("no orch root configured")
	}
	if projectPath == "" {
		return errors.New("no project selected")
	}
	p, err := LoadArchivable(projectPath)
	if err != nil {
		return err
	}
	projectDir := filepath.Dir(projectPath)
	name := filepath.Base(projectDir)
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("refusing to archive %q", projectDir)
	}
	if filepath.Dir(projectDir) != filepath.Clean(root) {
		return fmt.Errorf("%s is not a project under %s", projectDir, root)
	}
	backupRoot := BackupRoot(root)
	dest := filepath.Join(backupRoot, name)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("already archived at %s", dest)
	} else if !os.IsNotExist(err) {
		return err
	}
	if wsp != nil && p.Branch != "" {
		if err := removeTaskWorktrees(wsp, p.Branch); err != nil {
			return fmt.Errorf("workspace %s: task worktrees not removed (%v); %s left in place", p.Branch, err, name)
		}
		if err := removeWorkspaceOrch(wsp, p.Branch); err != nil {
			return fmt.Errorf("workspace %s: .orch not removed (%v); %s left in place", p.Branch, err, name)
		}
		ctx, cancel := context.WithTimeout(context.Background(), archiveWorkspaceTimeout)
		defer cancel()
		// force=true: the cleanup task (archive: true) has already validated the
		// workspace is ready to be removed, so bypass wsp's unmerged/pending-changes
		// guard rather than have it block the archive.
		if err := wsp.Remove(ctx, p.Branch, true); err != nil {
			return fmt.Errorf("workspace %s not removed (%v); %s left in place", p.Branch, err, name)
		}
	}
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		return err
	}
	return os.Rename(projectDir, dest)
}

// removeTaskWorktrees deletes the project's per-task worktrees and refs from
// every repo of its wsp workspace (docs/parallel-task-worktrees.md X6): each
// worktree under .worktrees/, then refs/heads/wm/<branch>/* and
// refs/wm/<branch>/*, through worktree.Manager.RemoveProject — the helper the
// daemon's cleanup uses too, so the two agree on paths and ref names. The refs
// go explicitly, rather than with the workspace, because a repo dir that is a
// worktree of a shared mirror keeps its refs in the mirror, where `wsp rm`
// would leave them.
//
// The archive agent has already reported any unintegrated work they held
// (archive: true), so this deletes it. A workspace wsp cannot resolve, or one
// that does not exist, has nothing to remove; repo dirs without a .git are
// skipped.
func removeTaskWorktrees(wsp WorkspaceRemover, branch string) error {
	root, err := wsp.Path(branch)
	if err != nil || root == "" {
		return nil
	}
	repos, err := worktree.GitRepos(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), archiveWorkspaceTimeout)
	defer cancel()
	return worktree.Manager{}.RemoveProject(ctx, root, branch, repos)
}

// removeWorkspaceOrch deletes the `.orch` directory at the root of the wsp
// workspace for branch — the orchestrator's own control directory (agent
// instructions and context), not repo content. It runs before the workspace
// itself is removed so wsp never has to step over it or leave it behind.
//
// The workspace is keyed on the project's `branch`, exactly as the wsp removal
// that follows is. Nothing to do is not an error: a branchless project, a branch
// wsp cannot resolve (there is then no workspace to clean, and the removal that
// follows is a no-op for it too), or a workspace with no `.orch` directory all
// return nil. A resolved root that is the filesystem root is refused rather than
// trusted, so a bad wsp answer can't turn into `rm -rf /.orch`.
func removeWorkspaceOrch(wsp WorkspaceRemover, branch string) error {
	if wsp == nil || branch == "" {
		return nil
	}
	root, err := wsp.Path(branch)
	if err != nil || root == "" {
		return nil
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) || root == "." {
		return fmt.Errorf("refusing to clean .orch under %q", root)
	}
	dir := filepath.Join(root, ".orch")
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(dir)
}
