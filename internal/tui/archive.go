package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/project"
)

// workspaceRemover is the sliver of workspace.Manager `:archive` needs: tearing
// down the wsp workspace belonging to the work stream being archived, plus
// resolving where that workspace lives so the orchestrator's own `.orch`
// control directory can be cleared out of it first. Narrowing it to those two
// methods keeps the TUI decoupled from the concrete wsp driver and lets tests
// substitute a fake that records the branch it was asked to remove.
type workspaceRemover interface {
	Path(branch string) (string, error)
	Remove(ctx context.Context, branch string) error
}

// archiveWorkspaceTimeout bounds the `wsp rm` shell-out. The archive runs
// inline on the UI goroutine, so a wedged wsp would otherwise freeze the TUI.
const archiveWorkspaceTimeout = 30 * time.Second

// handleConfirmArchiveKey processes a keystroke while the `:archive` confirm
// modal is open. `y` performs the move; `n`/esc cancels. Any other key is
// ignored so a stray keypress can't archive by accident.
func (m model) handleConfirmArchiveKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		name := projectDisplayName(m.archiveTarget)
		if err := archiveProject(m.projectRoot, m.archiveTarget, m.wspRemover); err != nil {
			m.statusMsg = "archive: " + err.Error()
		} else {
			m.statusMsg = "archived " + name + " to " + backupRootFor(m.projectRoot)
		}
		m.mode = modeNormal
		m.archiveTarget = ""
		return m, nil
	case "n", "N", "esc":
		m.mode = modeNormal
		m.archiveTarget = ""
		m.statusMsg = ""
		return m, nil
	}
	return m, nil
}

// backupRootFor is the archive destination root for a workspace root: the
// sibling "<root>.backup" directory (e.g. ~/orch -> ~/orch.backup).
func backupRootFor(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Clean(root) + ".backup"
}

// loadArchivable loads the project at projectPath and reports whether it may be
// archived at all. Only a work stream the cleanup agent has already finished —
// `archive: true` in its .project.yaml — is eligible; anything else is a
// refusal, including a project file that won't load (we can't tell whether it
// was cleaned up, so we don't touch it). Callers turn the error into the status
// message, so the wording is user-facing and points at the next step.
func loadArchivable(projectPath string) (*project.Project, error) {
	if projectPath == "" {
		return nil, errors.New("no work stream selected")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return nil, err
	}
	if !p.Archive {
		return nil, fmt.Errorf("%s has not been cleaned up — run :cleanup first", projectDisplayName(projectPath))
	}
	return p, nil
}

// archiveProject moves the project whose .project.yaml is at projectPath out of
// the workspace root and into the sibling backup root, keeping its directory
// name (e.g. ~/orch/weather-tui -> ~/orch.backup/weather-tui), and removes the
// project's wsp workspace along the way. Because it's a move, the project
// leaves the workspace root immediately and the daemon's project scan drops the
// entry on its next pass — no explicit cleanup needed.
//
// Only a cleaned-up project (`archive: true`) is archivable; see loadArchivable.
//
// Refuses to overwrite an existing archive of the same name so a prior backup
// is never clobbered, and only archives a direct child of the configured root
// so a bad selection can't move an arbitrary directory.
//
// Ordering and failure policy: every validation runs first, then the workspace
// is torn down — its `.orch` directory first, then the workspace itself — and
// only then does the project directory move. Doing the removals first means a
// failure there aborts the archive with the project still in its original
// place — nothing is lost and the user can retry once wsp is healthy — whereas
// moving first would leave a half-archived work stream whose workspace still
// exists. Removal errors are reported to the caller (and so to the status line)
// rather than swallowed. wsp is nil in models with no workspace manager wired in
// (standalone `orch tui`); both removals are skipped then, as they are for a
// project with no `branch`.
func archiveProject(root, projectPath string, wsp workspaceRemover) error {
	if root == "" {
		return errors.New("no orch root configured")
	}
	if projectPath == "" {
		return errors.New("no project selected")
	}
	p, err := loadArchivable(projectPath)
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
	backupRoot := backupRootFor(root)
	dest := filepath.Join(backupRoot, name)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("already archived at %s", dest)
	} else if !os.IsNotExist(err) {
		return err
	}
	if wsp != nil && p.Branch != "" {
		if err := removeWorkspaceOrch(wsp, p.Branch); err != nil {
			return fmt.Errorf("workspace %s: .orch not removed (%v); %s left in place", p.Branch, err, name)
		}
		ctx, cancel := context.WithTimeout(context.Background(), archiveWorkspaceTimeout)
		defer cancel()
		if err := wsp.Remove(ctx, p.Branch); err != nil {
			return fmt.Errorf("workspace %s not removed (%v); %s left in place", p.Branch, err, name)
		}
	}
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		return err
	}
	return os.Rename(projectDir, dest)
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
func removeWorkspaceOrch(wsp workspaceRemover, branch string) error {
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

// renderConfirmArchiveModal draws the centered yes/no dialog for `:archive`.
// Like the other modals it returns a full-screen string so View can substitute
// it for the body while modeConfirmArchive is active.
func (m model) renderConfirmArchiveModal() string {
	name := projectDisplayName(m.archiveTarget)
	src := filepath.Dir(m.archiveTarget)
	dest := filepath.Join(backupRootFor(m.projectRoot), name)

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Archive work stream"))
	b.WriteString("\n\n")
	b.WriteString("Archive " + name + "? It will be moved out of the workspace:")
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render(src))
	b.WriteString("\n  → ")
	b.WriteString(dimStyle.Render(dest))
	b.WriteString("\n\n")
	// State the two things the confirm is really agreeing to: the wsp
	// workspace goes away too, and only a cleaned-up work stream qualifies
	// (the guard already rejected the others before this modal opened).
	b.WriteString(dimStyle.Render("Its wsp workspace is removed too. Requires :cleanup (archive: true)."))
	b.WriteString("\n\n")
	b.WriteString(hintStyle.Render("y: archive  •  n/esc: cancel"))

	width := 64
	if m.width > 0 && m.width-4 < width {
		width = m.width - 4
	}
	if width < 20 {
		width = 20
	}

	modal := modalBorder.Width(width).Render(b.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, modal)
}
