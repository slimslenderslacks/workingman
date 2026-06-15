package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/acpclient"
)

// acptabs.go holds the ACP-session tab view: a tab per live non-interactive ACP
// session, each showing the prompts sent to that agent and the assistant output
// streaming back. The state here is pure (no sockets, no goroutines) so it can
// be unit-tested in isolation; acpwatch.go owns the live wiring that feeds these
// structures from real session.Store discovery + acpclient connections.

// transcriptKind distinguishes the two kinds of entries in a tab's transcript:
// a prompt the TUI sent to the agent, and a chunk of assistant output streamed
// back. Keeping them typed lets the renderer label each block ("prompt" vs
// "agent") so the conversation reads top-to-bottom.
type transcriptKind int

const (
	entryPrompt transcriptKind = iota
	entryMessage
)

// transcriptEntry is one block in a tab's scrollback: either a whole prompt or
// an assistant message accreted from streamed chunks.
type transcriptEntry struct {
	kind transcriptKind
	text string
}

// acpTab is the per-session state the tab view renders. status is the latest
// acpclient.State seen for the session (driving the status glyph/label), and
// entries is the ordered prompt/message transcript. curMsg points at the
// in-progress assistant message so streamed chunks append to it; it is -1
// between turns (after a completed turn or a freshly sent prompt), which forces
// the next chunk to open a new message block.
type acpTab struct {
	id      string
	title   string
	status  acpclient.State
	entries []transcriptEntry
	curMsg  int
}

// acpTabs is the collection of tabs plus the selected index. The zero value is
// usable (no tabs, sel 0).
type acpTabs struct {
	tabs []acpTab
	sel  int
}

// indexOf returns the slice index of the tab with the given id, or -1.
func (a *acpTabs) indexOf(id string) int {
	for i := range a.tabs {
		if a.tabs[i].id == id {
			return i
		}
	}
	return -1
}

// upsert adds a tab for id when one doesn't already exist. Creating a tab the
// moment a session starts is half the lifecycle contract; removeTab is the
// other half. A new tab starts in StateConnecting with an empty transcript and
// no in-progress message (curMsg -1). The first tab added becomes selected.
func (a *acpTabs) upsert(id, title string) {
	if a.indexOf(id) >= 0 {
		return
	}
	a.tabs = append(a.tabs, acpTab{
		id:     id,
		title:  title,
		status: acpclient.StateConnecting,
		curMsg: -1,
	})
	if len(a.tabs) == 1 {
		a.sel = 0
	}
}

// remove drops the tab for id (a session whose resources were cleaned up) and
// clamps the selection so it still points at a real tab.
func (a *acpTabs) remove(id string) {
	i := a.indexOf(id)
	if i < 0 {
		return
	}
	a.tabs = append(a.tabs[:i], a.tabs[i+1:]...)
	if a.sel >= len(a.tabs) {
		a.sel = len(a.tabs) - 1
	}
	if a.sel < 0 {
		a.sel = 0
	}
}

// addPrompt records a prompt sent to the session as a new transcript block and
// resets curMsg so the agent's reply opens a fresh message block beneath it.
func (a *acpTabs) addPrompt(id, text string) {
	i := a.indexOf(id)
	if i < 0 {
		return
	}
	a.tabs[i].entries = append(a.tabs[i].entries, transcriptEntry{kind: entryPrompt, text: text})
	a.tabs[i].curMsg = -1
}

// apply folds one acpclient.Event into the tab's state: it updates the status
// and, for streaming chunks, accretes the assistant text.
//
//   - StateStreaming with empty Text marks the start of a turn: curMsg is reset
//     so the next non-empty chunk opens a new message block.
//   - StateStreaming with text appends to the in-progress message (or opens one
//     if none is open).
//   - StateCompleted ends the turn (curMsg -1).
//   - Terminal states (disconnected/errored) just record the status so the tab
//     shows the session ended; the transcript is preserved.
func (a *acpTabs) apply(id string, ev acpclient.Event) {
	i := a.indexOf(id)
	if i < 0 {
		return
	}
	t := &a.tabs[i]
	switch ev.State {
	case acpclient.StateStreaming:
		t.status = ev.State
		if ev.Text == "" {
			t.curMsg = -1
			return
		}
		if t.curMsg < 0 {
			t.entries = append(t.entries, transcriptEntry{kind: entryMessage, text: ev.Text})
			t.curMsg = len(t.entries) - 1
		} else {
			t.entries[t.curMsg].text += ev.Text
		}
	case acpclient.StateCompleted:
		t.status = ev.State
		t.curMsg = -1
	default:
		t.status = ev.State
		if ev.State.IsTerminal() {
			t.curMsg = -1
		}
	}
}

// next / prev cycle the selected tab, wrapping at the ends so left/right keys
// never dead-end.
func (a *acpTabs) next() {
	if len(a.tabs) == 0 {
		return
	}
	a.sel = (a.sel + 1) % len(a.tabs)
}

func (a *acpTabs) prev() {
	if len(a.tabs) == 0 {
		return
	}
	a.sel = (a.sel - 1 + len(a.tabs)) % len(a.tabs)
}

// selected returns a pointer to the currently-selected tab, or false when there
// are none.
func (a *acpTabs) selected() (*acpTab, bool) {
	if a.sel < 0 || a.sel >= len(a.tabs) {
		return nil, false
	}
	return &a.tabs[a.sel], true
}

// --- status presentation ---

// acpStatusLabel maps an acpclient.State to the human label shown in the status
// line. Unknown states pass through verbatim so a future state still renders.
func acpStatusLabel(s acpclient.State) string {
	switch s {
	case acpclient.StateConnecting:
		return "connecting"
	case acpclient.StateConnected:
		return "connected"
	case acpclient.StateStreaming:
		return "streaming"
	case acpclient.StateCompleted:
		return "completed"
	case acpclient.StateDisconnected:
		return "disconnected"
	case acpclient.StateErrored:
		return "errored"
	}
	return string(s)
}

// acpStatusGlyph pairs a compact symbol with the status: a filled dot while
// streaming, a check when a turn completes, a cross when the session ended, and
// an open dot for the in-between connecting/connected states.
func acpStatusGlyph(s acpclient.State) string {
	switch s {
	case acpclient.StateStreaming:
		return "●"
	case acpclient.StateCompleted:
		return "✓"
	case acpclient.StateDisconnected, acpclient.StateErrored:
		return "✗"
	}
	return "○"
}

// acpStatusStyle colours the status to match the palette the rest of the UI
// uses: green-ish for active/done, red for ended, dim for transitional.
func acpStatusStyle(s acpclient.State) lipgloss.Style {
	switch s {
	case acpclient.StateStreaming:
		return statusRunning
	case acpclient.StateCompleted:
		return statusDone
	case acpclient.StateConnected:
		return statusReady
	case acpclient.StateDisconnected, acpclient.StateErrored:
		return statusErrStyle
	}
	return dimStyle
}

var (
	acpTabStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")).
			Padding(0, 1)
	acpTabSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("212")).
				Background(lipgloss.Color("236")).
				Padding(0, 1)
	acpPromptLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("220"))
	acpMsgLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("110"))
)

// acpChromeRows is the non-body height of the ACP view: header, tab bar, status
// line, and footer hint (one row each). The transcript box gets the rest.
const acpChromeRows = 4

// renderACPTabBar draws the row of tab labels, each carrying its status glyph,
// with the selected tab highlighted. The caller clamps the result to the
// terminal width (lipgloss MaxWidth is ANSI-aware, so clamping the styled bar
// can't tear an escape sequence the way a byte-slice truncate would).
func renderACPTabBar(tabs []acpTab, sel int) string {
	cells := make([]string, 0, len(tabs))
	for i, t := range tabs {
		label := acpStatusGlyph(t.status) + " " + t.title
		if i == sel {
			cells = append(cells, acpTabSelectedStyle.Render(label))
		} else {
			cells = append(cells, acpTabStyle.Render(label))
		}
	}
	return strings.Join(cells, " ")
}

// renderACPTabBody renders the selected tab's transcript into a width×height
// box, newest content at the bottom (stream semantics: the eye lands on the
// latest chunk). Each entry is a labelled block — "▷ prompt" or "◁ agent" —
// followed by its wrapped text and a blank separator. When the transcript is
// taller than the box, only the trailing lines that fit are shown.
func renderACPTabBody(t *acpTab, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	var lines []string
	if len(t.entries) == 0 {
		lines = append(lines, dimStyle.Render("(waiting for output…)"))
	}
	for _, e := range t.entries {
		switch e.kind {
		case entryPrompt:
			lines = append(lines, acpPromptLabelStyle.Render("▷ prompt"))
		case entryMessage:
			lines = append(lines, acpMsgLabelStyle.Render("◁ agent"))
		}
		lines = append(lines, acpWrapLines(e.text, width)...)
		lines = append(lines, "")
	}
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	return strings.Join(lines, "\n")
}

// acpWrapLines hard-wraps plain text to width columns using lipgloss's
// width-aware wrapper. The input is always plain agent/prompt text (no ANSI), so
// wrapping is safe and the per-line padding lipgloss adds is harmless in the
// left-aligned transcript.
func acpWrapLines(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(s)
	return strings.Split(wrapped, "\n")
}

// renderACPView paints the full-window ACP tab view: a title, the tab bar, the
// selected session's status line, a bordered transcript box, and a key hint.
// With no tabs it shows an empty-state line instead. This is what View() returns
// while m.showACP is set.
func (m model) renderACPView() string {
	width, height := m.width, m.height
	if width <= 0 || height <= 0 {
		return "starting acp view…"
	}

	header := titleStyle.Render("ACP Sessions")
	hint := hintStyle.Render("←/→: switch tab  •  esc: back  •  q: quit")

	if len(m.acp.tabs) == 0 {
		body := dimStyle.Render("(no active ACP sessions)")
		return lipgloss.JoinVertical(lipgloss.Left, header, "", body, "", hint)
	}

	bar := lipgloss.NewStyle().MaxWidth(width).Render(renderACPTabBar(m.acp.tabs, m.acp.sel))

	t, _ := m.acp.selected()
	statusLine := acpStatusStyle(t.status).Render(acpStatusGlyph(t.status) + " " + acpStatusLabel(t.status))

	bodyOuterH := height - acpChromeRows
	if bodyOuterH < 3 {
		bodyOuterH = 3
	}
	innerW := width - unfocusedBorder.GetHorizontalFrameSize()
	if innerW < 1 {
		innerW = 1
	}
	innerH := bodyOuterH - unfocusedBorder.GetVerticalFrameSize()
	if innerH < 1 {
		innerH = 1
	}
	bodyContent := renderACPTabBody(t, innerW, innerH)
	box := unfocusedBorder.Width(innerW).Height(innerH).MaxWidth(width).Render(bodyContent)

	return lipgloss.JoinVertical(lipgloss.Left, header, bar, statusLine, box, hint)
}
