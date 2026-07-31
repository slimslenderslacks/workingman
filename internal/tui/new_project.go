package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/project"
)

// handleCommandLineKey processes a keystroke while the user is typing a vim
// style `:command`. The only command we recognise today is `new`, which
// transitions into the new-project modal; anything else gets reported via
// the footer's status line and the mode resets to normal.
func (m model) handleCommandLineKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.cmdInput = ""
		return m, nil
	case "backspace":
		if n := len(m.cmdInput); n > 0 {
			m.cmdInput = m.cmdInput[:n-1]
		}
		return m, nil
	case "enter":
		cmd := strings.TrimSpace(m.cmdInput)
		m.cmdInput = ""
		m.mode = modeNormal
		switch cmd {
		case "":
			// User pressed enter on an empty line; just exit cmdline mode.
		case "new":
			m.mode = modeNewProject
			m.newProjName = ""
			m.newProjDesc = ""
			m.newProjFocus = 0
			m.newProjErr = ""
		case "task":
			// `:task` seeds a new task into the selected project and re-runs
			// the planning agent to flesh it out. It needs a project to attach
			// to; with none selected there's nothing to add a task to.
			if m.projSel == "" {
				m.statusMsg = "no project selected"
				return m, nil
			}
			m.mode = modeNewTask
			m.newTaskDesc = ""
			m.newTaskErr = ""
		case "archive":
			// `:archive` moves the selected project's tree out of the
			// workspace root into the sibling backup dir. It's destructive
			// (the project leaves the active workspace), so it goes through a
			// yes/no confirm modal rather than acting immediately.
			if m.projSel == "" {
				m.statusMsg = "no project selected"
				return m, nil
			}
			m.archiveTarget = m.projSel
			m.mode = modeConfirmArchive
		case "wolf":
			// `:wolf` summons the wolf agent for the selected project. The
			// daemon launches the wolf in response to status:blocked, so this
			// is an immediate action (no modal) that flips the project there.
			if m.projSel == "" {
				m.statusMsg = "no project selected"
				return m, nil
			}
			name, err := summonWolf(m.projSel)
			if err != nil {
				m.statusMsg = "summon wolf: " + err.Error()
				return m, nil
			}
			m.statusMsg = "summoned the wolf for " + name
		default:
			m.statusMsg = "unknown command: :" + cmd
		}
		return m, nil
	}
	if len(msg.Runes) == 1 && isPrintableASCII(msg.Runes[0]) {
		m.cmdInput += string(msg.Runes[0])
	}
	return m, nil
}

// handleNewProjectKey processes a keystroke while the new-project modal is
// open. The modal has two fields: a short Name (the on-disk directory /
// work-stream identity) and a free-form description that seeds the
// .project.yaml. Tab / shift+tab switches focus between them; enter submits;
// esc cancels. On submit it writes the project seed and dismisses the modal on
// success, or surfaces the error inline so the user can fix it and retry.
func (m model) handleNewProjectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.newProjName = ""
		m.newProjDesc = ""
		m.newProjFocus = 0
		m.newProjErr = ""
		return m, nil
	case "tab", "shift+tab":
		// Two fields, so tab just toggles between them. (Cursor keys are
		// unbound app-wide; the name field can't repurpose j/k since they're
		// literal input, so tab is the field switch here.)
		m.newProjFocus ^= 1
		return m, nil
	case "backspace":
		if m.newProjFocus == 0 {
			if n := len(m.newProjName); n > 0 {
				m.newProjName = m.newProjName[:n-1]
			}
		} else {
			if n := len(m.newProjDesc); n > 0 {
				m.newProjDesc = m.newProjDesc[:n-1]
			}
		}
		m.newProjErr = ""
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.newProjName)
		desc := strings.TrimSpace(m.newProjDesc)
		if err := createProjectSeed(m.projectRoot, name, desc); err != nil {
			m.newProjErr = err.Error()
			return m, nil
		}
		m.mode = modeNormal
		m.newProjName = ""
		m.newProjDesc = ""
		m.newProjFocus = 0
		m.newProjErr = ""
		m.statusMsg = "created project " + name
		return m, nil
	}
	// Route printable input to the focused field. The name field is restricted
	// to filesystem-safe characters (single keystrokes only); the description
	// takes any printable prose and accepts multi-line pastes, collapsing them
	// to spaces the same way the new-task modal does.
	if m.newProjFocus == 0 {
		if len(msg.Runes) == 1 && isProjectNameChar(msg.Runes[0]) {
			m.newProjName += string(msg.Runes[0])
			m.newProjErr = ""
		}
		return m, nil
	}
	if runes := sanitizePastedRunes(msg.Runes); runes != "" {
		m.newProjDesc += runes
		m.newProjErr = ""
	}
	return m, nil
}

// createProjectSeed writes `<root>/<name>/.project.yaml` as a project seed: a
// file carrying only the user's free-form description, written as `agent` so
// the daemon acts on it. The daemon sees an unpopulated project (no status
// yet) and launches the project agent, which reads the description and fills
// in the rest — the project analogue of the task seed queueTaskForPlanning
// writes for `:task`.
//
// Refuses to clobber an existing file: if `<root>/<name>/.project.yaml`
// already exists we surface that to the user instead of silently truncating
// what may be work-in-progress on disk.
func createProjectSeed(root, name, description string) error {
	if root == "" {
		return errors.New("no orch root configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name required")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid project name %q", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid project name %q (no slashes)", name)
	}
	if strings.TrimSpace(description) == "" {
		return errors.New("description required")
	}
	dir := filepath.Join(root, name)
	path := filepath.Join(dir, ".project.yaml")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("project %q already exists", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return project.SaveAs(path, &project.Project{Description: description}, project.WriterAgent)
}

// renderNewProjectModal draws the centered prompt that asks for a project
// name and description. Returned as a full-screen string so View() can
// substitute it in for the normal body when modeNewProject is active —
// bubbletea has no native overlay primitive, so the modal takes over the whole
// screen. The cursor sits on whichever field currently has focus.
func (m model) renderNewProjectModal() string {
	cursor := "▌"
	nameCursor, descCursor := "", ""
	if m.newProjFocus == 0 {
		nameCursor = cursor
	} else {
		descCursor = cursor
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("New work stream"))
	b.WriteString("\n\n")
	b.WriteString("Name: " + m.newProjName + nameCursor)
	b.WriteString("\n\n")
	b.WriteString("Describe the project:\n\n" + m.newProjDesc + descCursor)
	if m.newProjErr != "" {
		b.WriteString("\n\n")
		b.WriteString(statusErrStyle.Render(m.newProjErr))
	}
	b.WriteString("\n\n")
	b.WriteString(hintStyle.Render("tab: switch field  •  enter: create & plan  •  esc: cancel"))

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

// modalBorder is the box style used by the new-project dialog. The same
// accent colour as the focused-pane border tells the user "this is the
// active thing" without needing a separate visual language.
var modalBorder = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("212")).
	Padding(1, 2)

func isPrintableASCII(r rune) bool {
	return r >= 0x20 && r < 0x7f
}

// isProjectNameChar gates which keystrokes the modal accepts into the name
// buffer. Restricted to characters that are safe in a filesystem path on
// both macOS and Linux: ASCII alphanumeric plus dash, underscore, and dot.
// Spaces and slashes are deliberately excluded; the latter would let the
// user write through a parent directory and the former produces directory
// names that are awkward to work with from a shell.
func isProjectNameChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '-', r == '_', r == '.':
		return true
	}
	return false
}
