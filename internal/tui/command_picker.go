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
// so typing that letter runs the command directly — except stop/start, which
// both share `session`'s "s" and so are only reachable via j/k + enter; "stop"
// and "start" were the names asked for, and shadowing the far more common
// `session` shortcut for them isn't worth it.
var projectCommands = []projectCommand{
	{"dir", "dir", "open a shell in the workspace sandbox"},
	{"session", "session", "open/resume an interactive claude session"},
	{"wolf", "wolf", "summon the wolf to investigate"},
	{"review", "review", "watch this work stream's PR (or poll it now)"},
	{"cleanup", "cleanup", "prepare this work stream for archiving"},
	{"archive", "archive", "archive this work stream"},
	{"stop", "stop", "pause this work stream and kill its running agents"},
	{"start", "start", "resume a stopped work stream"},
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
// archive opens its own modal, wolf/cleanup act immediately, and dir/session
// launch an interactive window off the UI goroutine. Commands that operate on
// a specific work stream require one to be selected. A new work stream is
// created by dropping a project.md file under the orch root — see the
// daemon's handleProjectSeed — and new tasks are queued by dropping a
// markdown file into the work stream's intake/ dir — see the daemon's
// handleIntakeFile — neither goes through a TUI command.
func (m model) dispatchProjectCommand(cmd string) (tea.Model, tea.Cmd) {
	m.mode = modeNormal
	switch cmd {
	case "cleanup":
		// `cleanup` sets the work stream's cleanup request flag; the daemon
		// launches the archive agent in response, which commits and pushes
		// whatever still needs it and then marks the work stream
		// `archive: true`. Immediate — no modal.
		if m.projSel == "" {
			m.statusMsg = "no work stream selected"
			return m, nil
		}
		name, err := requestCleanup(m.projSel)
		if err != nil {
			m.statusMsg = "cleanup: " + err.Error()
			return m, nil
		}
		m.statusMsg = "requested cleanup of " + name
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
	case "review":
		// `review` drives the PR-resolution loop for the selected work stream: on a
		// `done` project it flips to status:reviewing (the daemon arms the PR poll
		// and dispatches the review agent); on a project already `reviewing` it
		// kicks an immediate reconcile instead of waiting out the poll backoff.
		// Immediate — no modal.
		if m.projSel == "" {
			m.statusMsg = "no work stream selected"
			return m, nil
		}
		name, kicked, err := requestReview(m.projSel)
		if err != nil {
			m.statusMsg = "review: " + err.Error()
			return m, nil
		}
		if kicked {
			m.statusMsg = "running the review agent now for " + name
		} else {
			m.statusMsg = "watching the PR for " + name
		}
	case "stop":
		// `stop` pauses the selected work stream: the daemon kills every agent
		// session running for it and unregisters its cron/#review schedules.
		// Immediate — no modal, since it's the emergency-brake command.
		if m.projSel == "" {
			m.statusMsg = "no work stream selected"
			return m, nil
		}
		name, err := requestStop(m.projSel)
		if err != nil {
			m.statusMsg = "stop: " + err.Error()
			return m, nil
		}
		m.statusMsg = "stopped " + name
	case "start":
		// `start` resumes a work stream `:stop` paused, handing it back to the
		// daemon's normal routing. Immediate — no modal.
		if m.projSel == "" {
			m.statusMsg = "no work stream selected"
			return m, nil
		}
		name, err := requestStart(m.projSel)
		if err != nil {
			m.statusMsg = "start: " + err.Error()
			return m, nil
		}
		m.statusMsg = "resumed " + name
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
