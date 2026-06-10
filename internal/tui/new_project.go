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
			m.newProjErr = ""
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
// open. Enter creates the empty .project.yaml and dismisses the modal on
// success or surfaces the OS error inline so the user can fix the name and
// retry. Esc cancels.
func (m model) handleNewProjectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.newProjName = ""
		m.newProjErr = ""
		return m, nil
	case "backspace":
		if n := len(m.newProjName); n > 0 {
			m.newProjName = m.newProjName[:n-1]
		}
		m.newProjErr = ""
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.newProjName)
		if err := createEmptyProject(m.projectRoot, name); err != nil {
			m.newProjErr = err.Error()
			return m, nil
		}
		m.mode = modeNormal
		m.newProjName = ""
		m.newProjErr = ""
		m.statusMsg = "created project " + name
		return m, nil
	}
	if len(msg.Runes) == 1 && isProjectNameChar(msg.Runes[0]) {
		m.newProjName += string(msg.Runes[0])
		m.newProjErr = ""
	}
	return m, nil
}

// createEmptyProject writes an empty `<root>/<name>/.project.yaml` so the
// daemon's project watcher will pick it up on its next scan. The file is
// intentionally empty — the project agent fills it in later.
//
// Refuses to clobber an existing file: if `<root>/<name>/.project.yaml`
// already exists we surface that to the user instead of silently truncating
// what may be work-in-progress on disk.
func createEmptyProject(root, name string) error {
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
	return os.WriteFile(path, []byte{}, 0o644)
}

// renderNewProjectModal draws the centered prompt that asks for a project
// name. Returned as a full-screen string so View() can substitute it in for
// the normal body when modeNewProject is active — bubbletea has no native
// overlay primitive, so the modal takes over the whole screen.
func (m model) renderNewProjectModal() string {
	cursor := "▌"
	field := "Name: " + m.newProjName + cursor

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("New work stream"))
	b.WriteString("\n\n")
	b.WriteString(field)
	if m.newProjErr != "" {
		b.WriteString("\n\n")
		b.WriteString(statusErrStyle.Render(m.newProjErr))
	}
	b.WriteString("\n\n")
	b.WriteString(hintStyle.Render("enter: create  •  esc: cancel"))

	width := 48
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
