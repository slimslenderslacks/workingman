package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// handleConfirmArchiveKey processes a keystroke while the `:archive` confirm
// modal is open. `y` performs the move; `n`/esc cancels. Any other key is
// ignored so a stray keypress can't archive by accident.
func (m model) handleConfirmArchiveKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		name := projectDisplayName(m.archiveTarget)
		if err := archiveProject(m.projectRoot, m.archiveTarget); err != nil {
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

// archiveProject moves the project whose .project.yaml is at projectPath out of
// the workspace root and into the sibling backup root, keeping its directory
// name (e.g. ~/orch/weather-tui -> ~/orch.backup/weather-tui). Because it's a
// move, the project leaves the workspace root immediately and the daemon's
// project scan drops the entry on its next pass — no explicit cleanup needed.
//
// Refuses to overwrite an existing archive of the same name so a prior backup
// is never clobbered, and only archives a direct child of the configured root
// so a bad selection can't move an arbitrary directory.
func archiveProject(root, projectPath string) error {
	if root == "" {
		return errors.New("no orch root configured")
	}
	if projectPath == "" {
		return errors.New("no project selected")
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
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		return err
	}
	return os.Rename(projectDir, dest)
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
