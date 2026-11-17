package tui

import (
	"fmt"
	"io"
	"os"
	"strings"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// clipboardOut is where copyToClipboard writes its OSC 52 sequence. The
// terminal, not the TUI, owns the clipboard: OSC 52 asks it to set the system
// clipboard, which also works over ssh. stderr rather than stdout so the
// sequence doesn't land in the middle of a frame bubbletea is writing. A var
// so tests can capture it.
var clipboardOut io.Writer = os.Stderr

// copyToClipboard sends text to the terminal's clipboard via OSC 52, wrapped
// for tmux/screen passthrough when running inside one of them. Terminals that
// don't support OSC 52 (or have it disabled) silently ignore the sequence.
func copyToClipboard(text string) error {
	if text == "" {
		return fmt.Errorf("nothing to copy")
	}
	seq := osc52.New(text)
	switch {
	case os.Getenv("TMUX") != "":
		seq = seq.Tmux()
	case os.Getenv("STY") != "":
		seq = seq.Screen()
	}
	_, err := seq.WriteTo(clipboardOut)
	return err
}

// toggleMouse flips mouse capture (`m`). Capture off is select mode: the
// terminal handles click-drag itself, so text can be highlighted and copied
// natively. Capture on is mouse mode: clicks focus panes, select cards/rows
// and attach sessions (handleMouse), and native selection needs the
// terminal's bypass modifier (Shift-drag in most terminals, Option-drag in
// iTerm2/Terminal.app).
func (m model) toggleMouse() (tea.Model, tea.Cmd) {
	m.mouseCapture = !m.mouseCapture
	if m.mouseCapture {
		m.statusMsg = "mouse mode: clicks select panes (shift/option-drag to select text; m: select mode)"
		return m, tea.EnableMouseCellMotion
	}
	m.statusMsg = "select mode: click-drag to highlight text, copy with your terminal (m: mouse mode)"
	return m, tea.DisableMouse
}

// copySelection copies the focused pane's selected item (`y`): a session's
// tab name, a work stream's directory, a task's file path, or the detail
// pane's text. Paths are what the user most often wants to paste into a
// shell; the detail pane is the one place where the text itself is the point.
func (m model) copySelection() (tea.Model, tea.Cmd) {
	var text, what string
	switch m.focus {
	case paneSessions:
		for _, s := range m.sessions {
			if s.ID == m.sessSel {
				text, what = s.TabName, "session "+s.TabName
				if text == "" {
					text, what = s.ID, "session "+s.ID
				}
			}
		}
	case paneProjects:
		if p, ok := m.selectedProject(); ok {
			text = strings.TrimSuffix(p.Path, "/project.yaml")
			what = text
		}
	case paneTasks:
		if m.taskSel != "" {
			text, what = m.taskSel, m.taskSel
		}
	case paneProjectDetail:
		if p, ok := m.selectedProject(); ok {
			text = plainText(renderProjectDetailBody(p, max(m.width, 80)))
			what = "detail of " + p.Name
		}
	}
	if text == "" {
		m.statusMsg = "nothing selected to copy"
		return m, nil
	}
	if err := copyToClipboard(text); err != nil {
		m.statusMsg = "copy: " + err.Error()
		return m, nil
	}
	m.statusMsg = "copied " + what + " to the clipboard"
	return m, nil
}

// acpTranscriptText is a tab's whole transcript as plain text — what `y` in
// the ACP view copies. It's the same layout the view renders (wrapped at a
// generous width) with styling stripped and trailing padding trimmed, so it
// pastes cleanly.
func acpTranscriptText(t *acpTab, expandTools bool) string {
	lines, _ := acpTranscriptLines(t, 200, expandTools)
	return plainText(strings.Join(lines, "\n"))
}

// plainText strips ANSI styling and each line's trailing padding.
func plainText(s string) string {
	lines := strings.Split(ansi.Strip(s), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
