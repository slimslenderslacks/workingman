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
	"github.com/slimslenderslacks/work/internal/task"
)

// handleNewTaskKey processes a keystroke while the new-task modal is open.
// Enter seeds the task into the selected project and re-runs planning;
// backspace edits the description; esc cancels. Unlike the project-name
// field, the description accepts any printable character (including spaces
// and punctuation) since it becomes free-form prose the planning agent reads.
func (m model) handleNewTaskKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.newTaskDesc = ""
		m.newTaskErr = ""
		return m, nil
	case "backspace":
		if n := len(m.newTaskDesc); n > 0 {
			m.newTaskDesc = m.newTaskDesc[:n-1]
		}
		m.newTaskErr = ""
		return m, nil
	case "enter":
		desc := strings.TrimSpace(m.newTaskDesc)
		name, err := queueTaskForPlanning(m.projSel, desc)
		if err != nil {
			m.newTaskErr = err.Error()
			return m, nil
		}
		m.mode = modeNormal
		m.newTaskDesc = ""
		m.newTaskErr = ""
		m.statusMsg = "queued task " + name + " for planning"
		return m, nil
	}
	// A single keystroke and a paste both arrive as a KeyMsg carrying Runes;
	// a paste (bracketed paste is on by default) simply has many. Append every
	// printable rune so pasted descriptions land intact rather than being
	// dropped by a length==1 guard. Newlines and tabs from a multi-line paste
	// collapse to spaces so words don't run together in the single-line field.
	if runes := sanitizePastedRunes(msg.Runes); runes != "" {
		m.newTaskDesc += runes
		m.newTaskErr = ""
	}
	return m, nil
}

// sanitizePastedRunes keeps the printable ASCII from a keystroke or paste,
// turning embedded newlines and tabs into single spaces so a multi-line paste
// reads as continuous prose in the one-line description field. Other control
// characters are dropped.
func sanitizePastedRunes(runes []rune) string {
	var b strings.Builder
	for _, r := range runes {
		switch {
		case isPrintableASCII(r):
			b.WriteRune(r)
		case r == '\n' || r == '\t':
			b.WriteByte(' ')
			// A carriage return is dropped rather than turned into a space so a
			// CRLF ("\r\n") paste yields a single separating space, not two.
		}
	}
	return b.String()
}

// queueTaskForPlanning seeds a new task into the project identified by
// projectPath (the path of its .project.yaml) and flips the project back to
// status:ready so the daemon re-launches the planning agent to flesh the seed
// out into a full task. It returns the slug the seed file was written under.
//
// The seed carries only the user's description plus the bootstrap fields
// (status:ready, attempts:0, model:default) and deliberately leaves `name`
// empty — an unnamed task with a description is the signal the planning agent
// keys off to know this is a request it must complete rather than an existing
// task to leave alone. Because the project moves to status:ready (not
// working), the daemon routes to planning rather than dispatching the unnamed
// seed as a runnable task.
func queueTaskForPlanning(projectPath, description string) (string, error) {
	if projectPath == "" {
		return "", errors.New("no project selected")
	}
	if description == "" {
		return "", errors.New("description required")
	}

	dir := filepath.Dir(projectPath)
	tasksDir := filepath.Join(dir, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		return "", err
	}

	slug := uniqueTaskSlug(tasksDir, taskSlug(description))
	seed := &task.Task{
		Description: description,
		Status:      task.StatusReady,
		Attempts:    0,
		Model:       task.ModelDefault,
	}
	seedPath := filepath.Join(tasksDir, slug+".yaml")
	if err := task.Save(seedPath, seed); err != nil {
		return "", err
	}

	// Flip the project to status:ready so the daemon relaunches planning.
	// A stale blocked_reason would be misleading now that we're moving the
	// project forward, so clear it — planning is the agent that owns leaving
	// the blocked state. Written as `agent` so the daemon acts on the change
	// instead of ignoring it as one of its own writes.
	p, err := project.Load(projectPath)
	if err != nil {
		// Roll back the seed so a failed queue doesn't leave an orphan task
		// file the next plan would pick up unexpectedly.
		_ = os.Remove(seedPath)
		return "", err
	}
	p.Status = project.StatusReady
	p.BlockedReason = ""
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		_ = os.Remove(seedPath)
		return "", err
	}
	return slug, nil
}

// taskSlug derives a filesystem-safe file stem from a free-form description:
// lowercase, non-alphanumeric runs collapsed to single dashes, trimmed, and
// capped so the filename stays reasonable. Falls back to "task" when the
// description has no usable characters. The planning agent may rename the
// file to match the `name` it assigns; this is only a placeholder stem.
func taskSlug(description string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(description) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		return "task"
	}
	return slug
}

// uniqueTaskSlug returns base, or base-2 / base-3 / ... — the first stem for
// which no <stem>.yaml already exists in tasksDir. Keeps a second `:task` with
// a similar description from clobbering the first seed before planning runs.
func uniqueTaskSlug(tasksDir, base string) string {
	candidate := base
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(tasksDir, candidate+".yaml")); os.IsNotExist(err) {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

// renderNewTaskModal draws the centered prompt that asks for a task
// description. Like renderNewProjectModal it returns a full-screen string so
// View() can substitute it for the normal body while modeNewTask is active.
func (m model) renderNewTaskModal() string {
	cursor := "▌"
	field := "Describe the task:\n\n" + m.newTaskDesc + cursor

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("New task in " + projectDisplayName(m.projSel)))
	b.WriteString("\n\n")
	b.WriteString(field)
	if m.newTaskErr != "" {
		b.WriteString("\n\n")
		b.WriteString(statusErrStyle.Render(m.newTaskErr))
	}
	b.WriteString("\n\n")
	b.WriteString(hintStyle.Render("enter: create & plan  •  esc: cancel"))

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

// projectDisplayName is the human-facing name for the project whose
// .project.yaml lives at path — the basename of its control directory, which
// is what the daemon and sandbox naming also key off. Empty path yields a
// placeholder so the modal title never renders a bare "in ".
func projectDisplayName(path string) string {
	if path == "" {
		return "(none)"
	}
	return filepath.Base(filepath.Dir(path))
}
