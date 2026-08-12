package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// projectCommand is one row of the `:` command-picker menu. key is the
// canonical command name dispatchProjectCommand switches on and doubles as the
// first-letter keyboard shortcut; label/desc are what the menu shows.
type projectCommand struct {
	key   string
	label string
	desc  string
}

// projectCommands is the menu shown after `:` on the work-streams pane, in
// display order. Every entry is a work-stream action; the ones that need a
// selected work stream report "no work stream selected" when run without one
// (dispatchProjectCommand enforces that), so the menu stays complete rather
// than shifting rows as selection changes. Each key has a unique first letter,
// so typing that letter runs the command directly.
var projectCommands = []projectCommand{
	{"task", "task", "queue a new task for planning"},
	{"dir", "dir", "open a shell in the workspace sandbox"},
	{"session", "session", "open/resume an interactive claude session"},
	{"wolf", "wolf", "summon the wolf to investigate"},
	{"new", "new", "create a new work stream"},
	{"archive", "archive", "archive this work stream"},
}

// handleCommandPickerKey drives the `:` command menu: j/k (or arrows) move the
// highlight, enter runs the highlighted command, a command's first letter runs
// it directly, and esc dismisses the menu without doing anything.
func (m model) handleCommandPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		return m, nil
	case "up", "k":
		if m.cmdPickerIdx > 0 {
			m.cmdPickerIdx--
		}
		return m, nil
	case "down", "j":
		if m.cmdPickerIdx < len(projectCommands)-1 {
			m.cmdPickerIdx++
		}
		return m, nil
	case "enter":
		return m.dispatchProjectCommand(projectCommands[m.cmdPickerIdx].key)
	}
	// First-letter shortcut: run the matching command outright.
	if len(msg.Runes) == 1 {
		for _, c := range projectCommands {
			if rune(c.key[0]) == msg.Runes[0] {
				return m.dispatchProjectCommand(c.key)
			}
		}
	}
	return m, nil
}

// dispatchProjectCommand runs one work-stream command, chosen from the picker.
// It leaves the picker (mode back to normal) and then performs the command:
// new/task/archive open their own modal, wolf acts immediately, and dir/session
// launch an interactive window off the UI goroutine. Commands that operate on a
// specific work stream require one to be selected.
func (m model) dispatchProjectCommand(cmd string) (tea.Model, tea.Cmd) {
	m.mode = modeNormal
	switch cmd {
	case "new":
		// `new` creates a work stream from scratch, so it needs no selection.
		m.mode = modeNewProject
		m.newProjName = ""
		m.newProjDesc = ""
		m.newProjFocus = 0
		m.newProjErr = ""
	case "task":
		// `task` seeds a new task into the selected work stream and re-runs the
		// planning agent to flesh it out.
		if m.projSel == "" {
			m.statusMsg = "no work stream selected"
			return m, nil
		}
		m.mode = modeNewTask
		m.newTaskDesc = ""
		m.newTaskErr = ""
	case "archive":
		// `archive` moves the selected work stream's tree into the sibling
		// backup dir and removes its wsp workspace. It's destructive, so it
		// goes through a yes/no confirm.
		if m.projSel == "" {
			m.statusMsg = "no work stream selected"
			return m, nil
		}
		// Only a cleaned-up work stream can be archived. Check before opening
		// the confirm modal: a dialog whose only possible answer is "no" is a
		// poor affordance, so the refusal lands directly on the status line.
		if _, err := loadArchivable(m.projSel); err != nil {
			m.statusMsg = "archive: " + err.Error()
			return m, nil
		}
		m.archiveTarget = m.projSel
		m.mode = modeConfirmArchive
	case "wolf":
		// `wolf` flips the work stream to status:blocked; the daemon launches
		// the wolf agent in response. Immediate — no modal.
		if m.projSel == "" {
			m.statusMsg = "no work stream selected"
			return m, nil
		}
		name, err := summonWolf(m.projSel)
		if err != nil {
			m.statusMsg = "summon wolf: " + err.Error()
			return m, nil
		}
		m.statusMsg = "summoned the wolf for " + name
	case "dir":
		return m.openInteractive("shell")
	case "session":
		return m.openInteractive("session")
	default:
		m.statusMsg = "unknown command: " + cmd
	}
	return m, nil
}

// renderCommandPickerModal draws the `:` command menu as a centered modal,
// mirroring the other modals' box style. The highlighted row uses the same
// accent as the sessions pane's selection so the "active choice" reads the same
// way across the UI.
func (m model) renderCommandPickerModal() string {
	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Commands"))
	b.WriteString("\n\n")
	for i, c := range projectCommands {
		marker := "  "
		line := c.label + "  —  " + dimStyle.Render(c.desc)
		if i == m.cmdPickerIdx {
			marker = "▸ "
			line = sessionRowSelectedStyle.Render(c.label) + "  —  " + dimStyle.Render(c.desc)
		}
		b.WriteString(marker + line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("j/k: move  •  enter: run  •  esc: cancel"))

	width := 60
	if m.width > 0 && m.width-4 < width {
		width = m.width - 4
	}
	if width < 20 {
		width = 20
	}
	modal := modalBorder.Width(width).Render(b.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, modal)
}
