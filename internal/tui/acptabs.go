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

// transcriptKind distinguishes the kinds of entries in a tab's transcript: a
// prompt the TUI sent, a chunk of assistant output, the agent's private
// reasoning, a tool invocation, and the agent's task plan. Keeping them typed
// lets the renderer label each block so the conversation reads top-to-bottom.
type transcriptKind int

const (
	entryPrompt transcriptKind = iota
	entryMessage
	entryThought
	entryTool
	entryPlan
)

// transcriptEntry is one block in a tab's scrollback. text holds the prose for
// the prompt/message/thought kinds and the title for a tool block; the tool* and
// plan fields carry the extra structure those kinds render.
type transcriptEntry struct {
	kind transcriptKind
	text string

	// entryTool fields.
	toolID     string
	toolKind   string
	toolStatus string
	toolOutput string

	// entryPlan field: the agent's current task list.
	plan []acpclient.PlanEntry
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
	// toolIdx maps a tool call id to the index of its entryTool block so a later
	// tool_call_update mutates the block in place instead of appending a copy.
	// Lazily created; entries are only ever appended, so stored indices stay valid.
	toolIdx map[string]int
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
// and routes the payload by Event.Kind.
//
//   - EventToolCall and EventPlan are handled by their helpers below.
//   - EventStream / EventThought share the streaming-accretion path, differing
//     only in the entry kind they open:
//   - StateStreaming with empty Text marks the start of a turn: curMsg is reset
//     so the next non-empty chunk opens a new block.
//   - StateStreaming with text appends to the in-progress block of the same
//     kind (or opens one if none is open, or the open block is a different kind).
//   - StateCompleted ends the turn (curMsg -1).
//   - Terminal states (disconnected/errored) just record the status so the tab
//     shows the session ended; the transcript is preserved.
func (a *acpTabs) apply(id string, ev acpclient.Event) {
	i := a.indexOf(id)
	if i < 0 {
		return
	}
	t := &a.tabs[i]

	switch ev.Kind {
	case acpclient.EventToolCall:
		t.applyToolCall(ev)
		return
	case acpclient.EventPlan:
		t.applyPlan(ev)
		return
	}

	switch ev.State {
	case acpclient.StateStreaming:
		t.status = ev.State
		if ev.Text == "" {
			t.curMsg = -1
			return
		}
		kind := entryMessage
		if ev.Kind == acpclient.EventThought {
			kind = entryThought
		}
		if t.curMsg < 0 || t.entries[t.curMsg].kind != kind {
			t.entries = append(t.entries, transcriptEntry{kind: kind, text: ev.Text})
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

// applyToolCall folds a tool_call / tool_call_update event into the transcript.
// A tool call interrupts any in-progress assistant or thought block (curMsg is
// reset) so the following text opens a fresh block. The first event for a tool
// id opens a tool block; later events for the same id mutate it in place —
// updating status/title and accreting output — rather than appending a copy.
func (t *acpTab) applyToolCall(ev acpclient.Event) {
	t.status = ev.State
	t.curMsg = -1
	if t.toolIdx == nil {
		t.toolIdx = make(map[string]int)
	}
	if ev.ToolCallID != "" {
		if idx, ok := t.toolIdx[ev.ToolCallID]; ok {
			e := &t.entries[idx]
			if ev.ToolTitle != "" {
				e.text = ev.ToolTitle
			}
			if ev.ToolKind != "" {
				e.toolKind = ev.ToolKind
			}
			if ev.ToolStatus != "" {
				e.toolStatus = ev.ToolStatus
			}
			e.toolOutput += ev.Text
			return
		}
	}
	t.entries = append(t.entries, transcriptEntry{
		kind:       entryTool,
		text:       ev.ToolTitle,
		toolID:     ev.ToolCallID,
		toolKind:   ev.ToolKind,
		toolStatus: ev.ToolStatus,
		toolOutput: ev.Text,
	})
	if ev.ToolCallID != "" {
		t.toolIdx[ev.ToolCallID] = len(t.entries) - 1
	}
}

// applyPlan records the agent's task list. ACP sends the whole plan on every
// change, so a single plan block is kept and replaced in place rather than
// appended each time. The plan is not part of the streamed message flow, so it
// leaves curMsg untouched.
func (t *acpTab) applyPlan(ev acpclient.Event) {
	for i := range t.entries {
		if t.entries[i].kind == entryPlan {
			t.entries[i].plan = ev.Plan
			return
		}
	}
	t.entries = append(t.entries, transcriptEntry{kind: entryPlan, plan: ev.Plan})
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

// acpToolGlyph maps a tool's kind to a compact leading glyph so the transcript
// reads like the reference client — a distinct icon per kind (read/edit/search/
// execute/…), falling back to a generic gear for unknown or unset kinds.
func acpToolGlyph(kind string) string {
	switch kind {
	case "read":
		return "◎"
	case "edit", "write":
		return "✎"
	case "delete":
		return "␡"
	case "move":
		return "↦"
	case "search":
		return "⌕"
	case "execute":
		return "❯"
	case "fetch":
		return "↓"
	case "think":
		return "◌"
	}
	return "⚙"
}

// acpToolHeading renders a tool block's one-line header: a collapse affordance
// (▸ collapsed / ▾ expanded, or a space when the call has no output to reveal),
// a kind glyph, the tool's title (falling back to its kind, then a generic
// label), and its current status. The whole heading is styled as one unit so
// the status rides along with the title. This single line is all a tool call
// shows when collapsed — its output is gated behind the `z` toggle.
func acpToolHeading(e transcriptEntry, expanded bool) string {
	title := e.text
	if title == "" {
		title = e.toolKind
	}
	if title == "" {
		title = "tool"
	}
	affordance := " "
	if e.toolOutput != "" {
		if expanded {
			affordance = "▾"
		} else {
			affordance = "▸"
		}
	}
	head := affordance + " " + acpToolGlyph(e.toolKind) + " " + title
	if e.toolStatus != "" {
		head += " · " + e.toolStatus
	}
	return acpToolLabelStyle.Render(head)
}

// acpPlanGlyph maps a plan entry's status to a checkbox-style glyph: done, in
// progress, or pending.
func acpPlanGlyph(status string) string {
	switch status {
	case "completed":
		return "✓"
	case "in_progress":
		return "▸"
	default:
		return "○"
	}
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
	acpThoughtLabelStyle = lipgloss.NewStyle().
				Italic(true).
				Foreground(lipgloss.Color("103"))
	acpToolLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("180"))
	acpPlanLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("147"))

	// acpPromptBox / acpMsgBox wrap a conversation turn in a rounded border,
	// colour-coded by role (prompt vs assistant), so the transcript reads as a
	// series of message cards — tool calls stay as bare collapsed lines between
	// them. Padding gives the text breathing room inside the border.
	acpPromptBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("220")).
			Padding(0, 1)
	acpMsgBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("110")).
			Padding(0, 1)
)

// acpBoxLines renders a conversation turn as a bordered card: a styled role
// label on the first line, then the (border-wrapped) body, split into lines
// ready to append to the transcript. The box's total width is width columns (the
// rounded border adds the two edge columns to the content+padding width), so a
// card lines up flush with the transcript's other rows.
func acpBoxLines(label string, labelStyle lipgloss.Style, text string, box lipgloss.Style, width int) []string {
	bw := width - 2 // leave room for the rounded border's two edge columns
	if bw < 1 {
		bw = 1
	}
	content := labelStyle.Render(label)
	if text != "" {
		content += "\n" + text
	}
	return strings.Split(box.Width(bw).Render(content), "\n")
}

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
// followed by its wrapped text and a blank separator. Tool calls render as a
// single collapsed summary line; their output is shown only when expandTools is
// set (toggled by the `z` key). When the transcript is taller than the box,
// only the trailing lines that fit are shown.
func renderACPTabBody(t *acpTab, width, height int, expandTools bool) string {
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
			lines = append(lines, acpBoxLines("▷ prompt", acpPromptLabelStyle, e.text, acpPromptBox, width)...)
		case entryMessage:
			lines = append(lines, acpBoxLines("◁ agent", acpMsgLabelStyle, e.text, acpMsgBox, width)...)
		case entryThought:
			lines = append(lines, acpThoughtLabelStyle.Render("◌ thinking"))
			for _, l := range acpWrapLines(e.text, width) {
				lines = append(lines, dimStyle.Render(l))
			}
		case entryTool:
			lines = append(lines, acpWrapLines(acpToolHeading(e, expandTools), width)...)
			if expandTools && e.toolOutput != "" {
				for _, l := range acpWrapLines(e.toolOutput, width) {
					lines = append(lines, dimStyle.Render(l))
				}
			}
		case entryPlan:
			lines = append(lines, acpPlanLabelStyle.Render("☰ plan"))
			for _, pe := range e.plan {
				lines = append(lines, acpWrapLines(acpPlanGlyph(pe.Status)+" "+pe.Content, width)...)
			}
		}
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
	toolHint := "z: expand tools"
	if m.acpToolsExpanded {
		toolHint = "z: collapse tools"
	}
	hint := hintStyle.Render("←/→: switch tab  •  " + toolHint + "  •  esc: back  •  q: quit")

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
	bodyContent := renderACPTabBody(t, innerW, innerH, m.acpToolsExpanded)
	// lipgloss's Width sets the content+padding width, so to give the body an
	// innerW-wide text area (what renderACPTabBody drew to) the box Width must add
	// back the horizontal padding. Without this the text area is padding-narrower
	// than the body, so every line — and every message box border — wraps by two
	// columns and the cards render torn.
	box := unfocusedBorder.
		Width(innerW + unfocusedBorder.GetHorizontalPadding()).
		Height(innerH).MaxWidth(width).Render(bodyContent)

	return lipgloss.JoinVertical(lipgloss.Left, header, bar, statusLine, box, hint)
}
