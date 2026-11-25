package tui

import (
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/control"
)

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
			m.statusMsg = "archived " + name + " to " + control.BackupRoot(m.projectRoot)
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

// renderConfirmArchiveModal draws the centered yes/no dialog for `:archive`.
// Like the other modals it returns a full-screen string so View can substitute
// it for the body while modeConfirmArchive is active.
func (m model) renderConfirmArchiveModal() string {
	name := projectDisplayName(m.archiveTarget)
	src := filepath.Dir(m.archiveTarget)
	dest := filepath.Join(control.BackupRoot(m.projectRoot), name)

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
	b.WriteString(dimStyle.Render("Its wsp workspace, task worktrees and wm/ branches are removed too. Requires :cleanup (archive: true)."))
	b.WriteString("\n\n")
	b.WriteString(m.withInboxBadge(hintStyle.Render("y: archive  •  n/esc: cancel")))

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
