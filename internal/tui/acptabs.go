package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/acpclient"
)

// acptabs.go holds the ACP-session tab view: a tab per live ACP session, each
// showing the prompts sent to that agent and the assistant output streaming
// back. A persistent (conversational) session — the wolf — is also typeable:
// its tab is marked interactive and carries a draft message line. The state here is pure (no sockets, no goroutines) so it can
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
	// entryError is why the session failed before its agent ever ran.
	entryError
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

	// lines caches the entry's scrollback weight (see weight) so the tab's
	// running total can be kept without rescanning every entry per chunk.
	lines int
}

// acpScrollbackLimit bounds each tab's scrollback, in source lines (see
// transcriptEntry.weight). Once a transcript grows past it the oldest entries
// are dropped, so a long-running session can't grow the TUI's memory without
// bound. A var so tests can shrink it.
var acpScrollbackLimit = 5000

// textLines counts the lines in s (0 for the empty string).
func textLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// tailLines keeps the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// weight is the entry's size in source lines for scrollback accounting: its
// label/heading row, its body's lines, and the blank separator under it. It
// ignores wrapping (that depends on the terminal width), so it is an estimate
// of the rendered height, which is all a memory bound needs.
func (e *transcriptEntry) weight() int {
	n := 2
	switch e.kind {
	case entryTool:
		n += textLines(e.toolOutput)
	case entryPlan:
		n += len(e.plan)
	default:
		n += textLines(e.text)
	}
	return n
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
	// placeholder marks a tab created for a session still in session.StatusStarting
	// — discovered but not yet dialed, so status/entries carry no real information
	// yet. upsert (called once the session is actually watched) clears this.
	placeholder bool
	// interactive marks a persistent (conversational) session — the wolf — that
	// accepts typed messages. draft is the message being composed in this tab; it
	// is per-tab so switching tabs doesn't lose or leak a half-typed message.
	interactive bool
	draft       string

	// Scrollback accounting: lineCount is the sum of the entries' weights, kept
	// at or under acpScrollbackLimit by trim; dropped is how many source lines
	// trim has discarded, shown as a marker atop the transcript.
	lineCount int
	dropped   int
	// Scroll position. A tab follows its newest output (the "live" mode) until
	// the user scrolls up; then inspecting is set and the view is pinned to a
	// spot in the history — anchorLine rows into entry anchorEntry's rendered
	// block — so output arriving below it (or trim dropping entries above it)
	// doesn't move what the user is reading. Anchoring to an entry rather than
	// a row also keeps the spot across a resize that rewraps everything.
	inspecting  bool
	anchorEntry int
	anchorLine  int
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

// appendTab adds t to the end of the collection. It never steals focus: a tab
// appearing because some agent elsewhere just started a session must not yank
// the user off the tab they are reading. The sole exception is the first tab,
// where there is no existing focus to preserve.
func (a *acpTabs) appendTab(t acpTab) {
	a.tabs = append(a.tabs, t)
	if len(a.tabs) == 1 {
		a.sel = 0
	}
}

// upsert adds a tab for id when one doesn't already exist. Creating a tab the
// moment a session starts is half the lifecycle contract; removeTab is the
// other half. A new tab starts in StateConnecting with an empty transcript and
// no in-progress message (curMsg -1). The first tab added becomes selected.
// Calling upsert on an id that already has a (placeholder) tab clears the
// placeholder flag — this is how a starting-session placeholder (see
// upsertPlaceholder) turns into a real, actively-watched tab once the session
// flips to running and watchOneACPSession takes over.
func (a *acpTabs) upsert(id, title string) {
	if i := a.indexOf(id); i >= 0 {
		a.tabs[i].placeholder = false
		return
	}
	a.appendTab(acpTab{id: id, title: title, status: acpclient.StateConnecting, curMsg: -1})
}

// setInteractive marks the tab for id as accepting typed messages (or not).
func (a *acpTabs) setInteractive(id string, interactive bool) {
	if i := a.indexOf(id); i >= 0 {
		a.tabs[i].interactive = interactive
	}
}

// selectID moves the selection to the tab with the given id and reports whether
// it exists. Used to jump straight to a session's tab from the sessions pane.
func (a *acpTabs) selectID(id string) bool {
	i := a.indexOf(id)
	if i < 0 {
		return false
	}
	a.sel = i
	return true
}

// upsertPlaceholder adds a placeholder tab for a session discovered in
// session.StatusStarting — no watcher is dialing it yet, so there is no real
// status or transcript to show, just a marker that the session exists. It is a
// no-op if a tab for id already exists (placeholder or real), so it never
// clobbers a tab that upsert has already promoted to real.
func (a *acpTabs) upsertPlaceholder(id, title string) {
	if a.indexOf(id) >= 0 {
		return
	}
	a.appendTab(acpTab{id: id, title: title, status: acpclient.StateConnecting, curMsg: -1, placeholder: true})
}

// dead reports whether the tab's session has ended for good — it failed to
// start, errored, or its transport disconnected — so dismissing it (`x`) can't
// interrupt anything. A placeholder is still starting and is never dead.
func (t *acpTab) dead() bool {
	return !t.placeholder && t.status.IsTerminal()
}

// markFailed records that the session id failed before its agent ever ran:
// the tab (created if this is the first the TUI hears of the session) shows
// the errored status and reason as its transcript, instead of a placeholder
// that reads as still starting.
func (a *acpTabs) markFailed(id, title, reason string) {
	i := a.indexOf(id)
	if i < 0 {
		a.appendTab(acpTab{id: id, title: title, curMsg: -1})
		i = a.indexOf(id)
	}
	t := &a.tabs[i]
	t.placeholder = false
	t.status = acpclient.StateErrored
	t.curMsg = -1
	t.entries = append(t.entries, transcriptEntry{kind: entryError, text: reason})
	t.touch(len(t.entries) - 1)
}

// remove drops the tab for id (a session whose resources were cleaned up),
// keeping the focus on whatever tab the user was reading. Because sel is an
// index, dropping a tab that sits BEFORE it would otherwise slide a different
// session under the selection — often a tab that had just been appended, which
// reads as the view spontaneously jumping to a new tab. So the selected tab is
// re-found by id after the deletion; only when the removed tab was itself the
// selected one does the index clamp into the gap it left.
func (a *acpTabs) remove(id string) {
	i := a.indexOf(id)
	if i < 0 {
		return
	}
	selID := ""
	if t, ok := a.selected(); ok {
		selID = t.id
	}
	a.tabs = append(a.tabs[:i], a.tabs[i+1:]...)
	if selID != "" && selID != id {
		if j := a.indexOf(selID); j >= 0 {
			a.sel = j
			return
		}
	}
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
	t := &a.tabs[i]
	t.entries = append(t.entries, transcriptEntry{kind: entryPrompt, text: text})
	t.curMsg = -1
	t.touch(len(t.entries) - 1)
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
		t.touch(t.curMsg)
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
			t.touch(idx)
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
	t.touch(len(t.entries) - 1)
}

// applyPlan records the agent's task list. ACP sends the whole plan on every
// change, so a single plan block is kept and replaced in place rather than
// appended each time. The plan is not part of the streamed message flow, so it
// leaves curMsg untouched.
func (t *acpTab) applyPlan(ev acpclient.Event) {
	for i := range t.entries {
		if t.entries[i].kind == entryPlan {
			t.entries[i].plan = ev.Plan
			t.touch(i)
			return
		}
	}
	t.entries = append(t.entries, transcriptEntry{kind: entryPlan, plan: ev.Plan})
	t.touch(len(t.entries) - 1)
}

// touch re-weighs entry i after it was appended or mutated, then trims the
// scrollback back under acpScrollbackLimit. Callers must have finished
// recording any index of theirs (curMsg, toolIdx) first: trim shifts them.
func (t *acpTab) touch(i int) {
	e := &t.entries[i]
	t.lineCount -= e.lines
	e.lines = e.weight()
	t.lineCount += e.lines
	t.trim()
}

// trim enforces acpScrollbackLimit: it drops the oldest entries until the
// transcript fits, always keeping the newest one (it may be mid-stream). If
// that last entry alone is over the limit, its body is cut to its tail.
func (t *acpTab) trim() {
	limit := acpScrollbackLimit
	if limit <= 0 || t.lineCount <= limit {
		return
	}
	n := 0
	for t.lineCount > limit && n < len(t.entries)-1 {
		t.lineCount -= t.entries[n].lines
		t.dropped += t.entries[n].lines
		n++
	}
	if n > 0 {
		t.dropFront(n)
	}
	if t.lineCount > limit && len(t.entries) == 1 {
		e := &t.entries[0]
		keep := limit - 2
		if keep < 1 {
			keep = 1
		}
		switch e.kind {
		case entryTool:
			e.toolOutput = tailLines(e.toolOutput, keep)
		case entryPlan:
			if len(e.plan) > keep {
				e.plan = e.plan[len(e.plan)-keep:]
			}
		default:
			e.text = tailLines(e.text, keep)
		}
		before := e.lines
		e.lines = e.weight()
		t.lineCount += e.lines - before
		t.dropped += before - e.lines
	}
}

// dropFront removes the first n entries, shifting every stored entry index
// (curMsg, toolIdx, the inspection anchor) to match. The dropped slots are
// zeroed so their text is garbage-collectable before the next reallocation.
func (t *acpTab) dropFront(n int) {
	for i := 0; i < n; i++ {
		t.entries[i] = transcriptEntry{}
	}
	t.entries = t.entries[n:]
	if t.curMsg >= 0 {
		t.curMsg -= n
		if t.curMsg < 0 {
			t.curMsg = -1
		}
	}
	for id, idx := range t.toolIdx {
		if idx < n {
			delete(t.toolIdx, id)
		} else {
			t.toolIdx[id] = idx - n
		}
	}
	if t.inspecting {
		t.anchorEntry -= n
		if t.anchorEntry < 0 {
			t.anchorEntry, t.anchorLine = 0, 0
		}
	}
}

// --- scrolling ---

// viewTop is the index of the first transcript row shown in a height-row
// body, given the rendered rows' total and each entry's first row (starts, as
// returned by acpTranscriptLines). Following, it's whatever puts the newest row
// at the bottom; inspecting, it's the anchor's row. Either way it is clamped to
// the transcript, which is what keeps a resize (which rewraps everything) from
// scrolling past either end.
func (t *acpTab) viewTop(starts []int, total, height int) int {
	maxTop := total - height
	if maxTop < 0 {
		maxTop = 0
	}
	if !t.inspecting || len(starts) == 0 {
		return maxTop
	}
	i := t.anchorEntry
	if i >= len(starts) {
		i = len(starts) - 1
	}
	if i < 0 {
		i = 0
	}
	top := starts[i] + t.anchorLine
	if top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	return top
}

// scroll moves the view delta rows (negative is up, toward older output).
// Scrolling up off the bottom enters inspection mode, pinning the view to
// the history being read; reaching the bottom again returns to follow mode.
func (t *acpTab) scroll(delta int, starts []int, total, height int) {
	maxTop := total - height
	if maxTop < 0 {
		maxTop = 0
	}
	top := t.viewTop(starts, total, height) + delta
	// Everything already fits (nothing to inspect), or the move reaches the
	// bottom: follow.
	if maxTop == 0 || top >= maxTop || len(starts) == 0 {
		t.follow()
		return
	}
	if top < 0 {
		top = 0
	}
	i := 0
	for j, s := range starts {
		if s > top {
			break
		}
		i = j
	}
	t.inspecting = true
	t.anchorEntry = i
	t.anchorLine = top - starts[i]
}

// follow pins the view back to the newest output (G).
func (t *acpTab) follow() {
	t.inspecting = false
	t.anchorEntry, t.anchorLine = 0, 0
}

// acpScrollStatus is the scroll-mode half of the status line: "live" while
// following, or "inspection" plus how to get back.
func acpScrollStatus(t *acpTab) string {
	if t.inspecting {
		return "inspection — press G to follow"
	}
	return "live"
}

// acpPageRows is how far ctrl+f / ctrl+b move in a height-row body: a page,
// less two rows of overlap for context (as in vim).
func acpPageRows(height int) int {
	if height > 2 {
		return height - 2
	}
	return 1
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
	acpInspectStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("214"))

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

// acpTabLabel is the unstyled text of a tab's cell: its status glyph (or a
// "starting…" marker while the session is a placeholder, since it has no real
// acpclient.State yet) followed by its title.
func acpTabLabel(t acpTab) string {
	if t.placeholder {
		return "○ " + t.title + " (starting…)"
	}
	return acpStatusGlyph(t.status) + " " + t.title
}

// renderACPTabBar draws the row of tab labels, each carrying its status glyph,
// with the selected tab highlighted. When the tabs don't all fit in width, it
// shows a scrolled window that always contains the selected tab — sliding as
// selection moves past either edge — plus a "«+N" / "+N»" affordance naming
// how many tabs are hidden off that side. width <= 0 means "unbounded" (no
// scrolling, used by tests that don't care about overflow).
func renderACPTabBar(tabs []acpTab, sel int, width int) string {
	if len(tabs) == 0 {
		return ""
	}
	if sel < 0 || sel >= len(tabs) {
		sel = 0
	}

	cells := make([]string, len(tabs))
	for i, t := range tabs {
		label := acpTabLabel(t)
		if i == sel {
			cells[i] = acpTabSelectedStyle.Render(label)
		} else {
			cells[i] = acpTabStyle.Render(label)
		}
	}

	render := func(start, end int) string {
		parts := make([]string, 0, end-start+2)
		if start > 0 {
			parts = append(parts, dimStyle.Render(fmt.Sprintf("«+%d", start)))
		}
		parts = append(parts, cells[start:end]...)
		if end < len(tabs) {
			parts = append(parts, dimStyle.Render(fmt.Sprintf("+%d»", len(tabs)-end)))
		}
		return strings.Join(parts, " ")
	}

	full := render(0, len(tabs))
	if width <= 0 || lipgloss.Width(full) <= width {
		return full
	}

	// Grow a window outward from the selected tab, alternating sides, for as
	// long as the result (including whatever overflow indicators it needs)
	// still fits. This keeps the window minimal (no tabs shown beyond what
	// width allows) while guaranteeing sel is always inside it.
	start, end := sel, sel+1
	for start > 0 || end < len(tabs) {
		grew := false
		if end < len(tabs) && lipgloss.Width(render(start, end+1)) <= width {
			end++
			grew = true
		}
		if start > 0 && lipgloss.Width(render(start-1, end)) <= width {
			start--
			grew = true
		}
		if !grew {
			break
		}
	}
	return render(start, end)
}

// acpTranscriptLines lays out a tab's whole transcript at width columns, oldest
// first. Each entry is a labelled block — "▷ prompt" or "◁ agent" — followed
// by its wrapped text and a blank separator. Tool calls render as a single
// collapsed summary line; their output is shown only when expandTools is set
// (toggled by the `z` key). starts[i] is the row entry i's block begins on,
// which is what the inspection anchor is resolved against.
func acpTranscriptLines(t *acpTab, width int, expandTools bool) (lines []string, starts []int) {
	if len(t.entries) == 0 {
		return []string{dimStyle.Render("(waiting for output…)")}, nil
	}
	if t.dropped > 0 {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("… %d earlier lines dropped from scrollback", t.dropped)))
	}
	starts = make([]int, len(t.entries))
	for i, e := range t.entries {
		starts[i] = len(lines)
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
		case entryError:
			lines = append(lines, statusErrStyle.Render("✗ failed to start"))
			for _, l := range acpWrapLines(e.text, width) {
				lines = append(lines, statusErrStyle.Render(l))
			}
		case entryPlan:
			lines = append(lines, acpPlanLabelStyle.Render("☰ plan"))
			for _, pe := range e.plan {
				lines = append(lines, acpWrapLines(acpPlanGlyph(pe.Status)+" "+pe.Content, width)...)
			}
		}
		lines = append(lines, "")
	}
	return lines, starts
}

// renderACPTabBody renders the window of the selected tab's transcript that
// fits a width×height box: the newest rows while following (stream semantics:
// the eye lands on the latest chunk), or the pinned spot in the history while
// inspecting (see acpTab.viewTop).
func renderACPTabBody(t *acpTab, width, height int, expandTools bool) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines, starts := acpTranscriptLines(t, width, expandTools)
	top := t.viewTop(starts, len(lines), height)
	end := top + height
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[top:end], "\n")
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
	hintText := "⌥j/⌥k: switch tab  •  j/k ^f/^b: scroll  •  G: follow  •  " + toolHint + "  •  esc: back  •  q: quit"
	hint := hintStyle.Render(truncate(hintText, width))

	if len(m.acp.tabs) == 0 {
		body := dimStyle.Render("(no active ACP sessions)")
		return lipgloss.JoinVertical(lipgloss.Left, header, "", body, "", hint)
	}

	bar := lipgloss.NewStyle().MaxWidth(width).Render(renderACPTabBar(m.acp.tabs, m.acp.sel, width))

	t, _ := m.acp.selected()
	if t.dead() {
		hintText = "x: dismiss  •  " + hintText
		hint = hintStyle.Render(truncate(hintText, width))
	}
	// A conversational tab gets one extra row for its message line (and a hint
	// that says how to use it); acpBodySize shrinks the body by the same row so
	// the composed view's height is unchanged.
	if t.interactive {
		if m.acpComposing {
			hintText = "enter: send  •  esc: stop typing  •  ctrl+u: clear  •  ^f/^b: scroll  •  ⌥j/⌥k: switch tab"
		} else {
			hintText = "enter: type a message  •  " + hintText
		}
		hint = hintStyle.Render(truncate(hintText, width))
	}
	var statusLine string
	if t.placeholder {
		statusLine = dimStyle.Render("○ starting…")
	} else {
		statusLine = acpStatusStyle(t.status).Render(acpStatusGlyph(t.status)+" "+acpStatusLabel(t.status)) + "  •  "
		if t.inspecting {
			statusLine += acpInspectStyle.Render(acpScrollStatus(t))
		} else {
			statusLine += dimStyle.Render(acpScrollStatus(t))
		}
	}
	statusLine = lipgloss.NewStyle().MaxWidth(width).Render(statusLine)

	innerW, innerH := m.acpBodySize(t)
	bodyContent := renderACPTabBody(t, innerW, innerH, m.acpToolsExpanded)
	// lipgloss's Width sets the content+padding width, so to give the body an
	// innerW-wide text area (what renderACPTabBody drew to) the box Width must add
	// back the horizontal padding. Without this the text area is padding-narrower
	// than the body, so every line — and every message box border — wraps by two
	// columns and the cards render torn.
	box := unfocusedBorder.
		Width(innerW + unfocusedBorder.GetHorizontalPadding()).
		Height(innerH).MaxWidth(width).Render(bodyContent)

	parts := []string{header, bar, statusLine, box}
	if t.interactive {
		parts = append(parts, m.renderACPInputLine(t, width))
	} else if m.acpInputErr != "" {
		// No message line on this tab: an error (e.g. a refused `x`) takes the
		// hint's row so the view's height doesn't change.
		hint = statusErrStyle.Render(truncate(m.acpInputErr, width))
	}
	parts = append(parts, hint)
	view := lipgloss.JoinVertical(lipgloss.Left, parts...)
	// Defensive clamp: whatever the pieces above composed to, never hand the
	// altscreen more than m.height rows — a single bad line count upstream
	// should not by itself be able to scroll the terminal.
	if lines := strings.Split(view, "\n"); len(lines) > height {
		view = strings.Join(lines[:height], "\n")
	}
	return view
}

// acpBodySize is the text area (columns × rows) the selected tab's transcript
// gets inside the ACP view's border. The scroll keys need it too, to know how
// far a page is and where the bottom of the transcript sits.
func (m model) acpBodySize(t *acpTab) (innerW, innerH int) {
	width, height := m.width, m.height
	inputRows := 0
	if t.interactive {
		inputRows = 1
	}
	// The body gets height - acpChromeRows - 1 rows, not height - acpChromeRows:
	// composing header+bar+statusLine+box+hint to land on EXACTLY m.height rows
	// leaves zero vertical slack, so a transient mismatch between the cached
	// m.width/m.height and the real terminal size (a resize, or the frame right
	// as this view is entered) renders one row too tall for the real viewport.
	// A full-screen altscreen takeover overflowing by even one row scrolls the
	// terminal, stranding the previous frame's tab bar above the current one —
	// see acp-session-screen-rendering-glitch. Reserving this row means the
	// composed view is always at least one row shorter than m.height, so that
	// transient overshoot can never reach the real ceiling.
	bodyOuterH := height - acpChromeRows - 1 - inputRows
	if bodyOuterH < 3 {
		bodyOuterH = 3
	}
	innerW = width - unfocusedBorder.GetHorizontalFrameSize()
	if innerW < 1 {
		innerW = 1
	}
	innerH = bodyOuterH - unfocusedBorder.GetVerticalFrameSize()
	if innerH < 1 {
		innerH = 1
	}
	return innerW, innerH
}

// scrollACP scrolls the selected tab by delta transcript rows (negative is up).
func (m model) scrollACP(delta int) {
	t, ok := m.acp.selected()
	if !ok || t.placeholder {
		return
	}
	w, h := m.acpBodySize(t)
	lines, starts := acpTranscriptLines(t, w, m.acpToolsExpanded)
	t.scroll(delta, starts, len(lines), h)
}

// acpPage is one ctrl+f / ctrl+b step for the selected tab.
func (m model) acpPage() int {
	t, ok := m.acp.selected()
	if !ok {
		return 1
	}
	_, h := m.acpBodySize(t)
	return acpPageRows(h)
}

// renderACPInputLine draws the message line under a conversational tab: the
// draft with a cursor while composing, a prompt to start otherwise, and any
// send error from the last attempt. Truncated to width so a long draft can't
// wrap onto a phantom row (see the altscreen overflow notes in renderACPView);
// when it overflows, the tail (where the cursor is) is what stays visible.
func (m model) renderACPInputLine(t *acpTab, width int) string {
	if m.acpInputErr != "" {
		return statusErrStyle.Render(truncate(m.acpInputErr, width))
	}
	if !m.acpComposing {
		return dimStyle.Render(truncate("(enter to message this session)", width))
	}
	line := "› " + t.draft
	if r := []rune(line); width > 2 && len(r) > width-1 {
		line = "…" + string(r[len(r)-(width-2):])
	}
	return line + "▏"
}
