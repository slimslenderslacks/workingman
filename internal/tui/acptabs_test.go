package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/acpclient"
)

func TestUpsertAddsTabOnceAndSelectsFirst(t *testing.T) {
	var a acpTabs
	a.upsert("task-x", "task-x")
	a.upsert("task-x", "task-x") // duplicate id is a no-op
	a.upsert("plan-y", "plan-y")
	if len(a.tabs) != 2 {
		t.Fatalf("tabs len = %d, want 2", len(a.tabs))
	}
	if a.sel != 0 {
		t.Errorf("sel = %d, want 0 (first tab)", a.sel)
	}
	if a.tabs[0].status != acpclient.StateConnecting {
		t.Errorf("new tab status = %q, want connecting", a.tabs[0].status)
	}
}

func TestRemoveTabClampsSelection(t *testing.T) {
	var a acpTabs
	a.upsert("a", "a")
	a.upsert("b", "b")
	a.upsert("c", "c")
	a.sel = 2 // "c"
	a.remove("c")
	if len(a.tabs) != 2 {
		t.Fatalf("tabs len = %d, want 2", len(a.tabs))
	}
	if a.sel != 1 {
		t.Errorf("sel after removing last = %d, want 1", a.sel)
	}
	a.remove("a")
	a.remove("b")
	if len(a.tabs) != 0 || a.sel != 0 {
		t.Errorf("after removing all: len=%d sel=%d, want 0/0", len(a.tabs), a.sel)
	}
}

func TestApplyStreamsAssistantTextIntoOneMessage(t *testing.T) {
	var a acpTabs
	a.upsert("s", "s")
	// Turn start (empty text) then two chunks should accrete into a single
	// assistant message block.
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming})
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: "Hello"})
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: " world"})
	a.apply("s", acpclient.Event{State: acpclient.StateCompleted, StopReason: "end_turn"})

	tab := a.tabs[0]
	if tab.status != acpclient.StateCompleted {
		t.Errorf("status = %q, want completed", tab.status)
	}
	msgs := messagesOf(tab)
	if len(msgs) != 1 || msgs[0] != "Hello world" {
		t.Errorf("assistant messages = %v, want [\"Hello world\"]", msgs)
	}
	if tab.curMsg != -1 {
		t.Errorf("curMsg after completion = %d, want -1", tab.curMsg)
	}
}

func TestPromptsAndMessagesInterleave(t *testing.T) {
	var a acpTabs
	a.upsert("s", "s")
	a.addPrompt("s", "first prompt")
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: "answer one"})
	a.apply("s", acpclient.Event{State: acpclient.StateCompleted})
	a.addPrompt("s", "second prompt")
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: "answer two"})

	tab := a.tabs[0]
	if len(tab.entries) != 4 {
		t.Fatalf("entries = %d, want 4 (prompt,msg,prompt,msg)", len(tab.entries))
	}
	wantKinds := []transcriptKind{entryPrompt, entryMessage, entryPrompt, entryMessage}
	for i, e := range tab.entries {
		if e.kind != wantKinds[i] {
			t.Errorf("entry %d kind = %v, want %v", i, e.kind, wantKinds[i])
		}
	}
	// A new prompt must reset the in-progress message so the next chunk opens a
	// fresh block rather than appending to the previous answer.
	if tab.entries[3].text != "answer two" {
		t.Errorf("second answer = %q, want %q", tab.entries[3].text, "answer two")
	}
}

func TestApplyToolCallOpensAndUpdatesInPlace(t *testing.T) {
	var a acpTabs
	a.upsert("s", "s")
	// Some assistant text, then a tool call interrupts it.
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: "let me run that"})
	a.apply("s", acpclient.Event{
		Kind: acpclient.EventToolCall, State: acpclient.StateStreaming,
		ToolCallID: "c1", ToolTitle: "Run tests", ToolKind: "execute", ToolStatus: "in_progress",
	})
	// The update for the same id mutates the existing block, not a second one.
	a.apply("s", acpclient.Event{
		Kind: acpclient.EventToolCall, State: acpclient.StateStreaming,
		ToolCallID: "c1", ToolStatus: "completed", Text: "PASS\n",
	})
	// Assistant text after the tool call opens a fresh message block.
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: "all green"})

	tab := a.tabs[0]
	wantKinds := []transcriptKind{entryMessage, entryTool, entryMessage}
	if len(tab.entries) != len(wantKinds) {
		t.Fatalf("entries = %d, want %d (%v)", len(tab.entries), len(wantKinds), wantKinds)
	}
	for i, k := range wantKinds {
		if tab.entries[i].kind != k {
			t.Errorf("entry %d kind = %v, want %v", i, tab.entries[i].kind, k)
		}
	}
	tool := tab.entries[1]
	if tool.toolStatus != "completed" {
		t.Errorf("tool status = %q, want completed", tool.toolStatus)
	}
	if tool.toolOutput != "PASS\n" {
		t.Errorf("tool output = %q, want %q", tool.toolOutput, "PASS\n")
	}
	if tab.entries[2].text != "all green" {
		t.Errorf("post-tool message = %q, want %q", tab.entries[2].text, "all green")
	}
}

func TestApplyThoughtSeparateFromMessage(t *testing.T) {
	var a acpTabs
	a.upsert("s", "s")
	a.apply("s", acpclient.Event{Kind: acpclient.EventThought, State: acpclient.StateStreaming, Text: "I should "})
	a.apply("s", acpclient.Event{Kind: acpclient.EventThought, State: acpclient.StateStreaming, Text: "check the file"})
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: "Here is the answer"})

	tab := a.tabs[0]
	if len(tab.entries) != 2 {
		t.Fatalf("entries = %d, want 2 (thought, message)", len(tab.entries))
	}
	if tab.entries[0].kind != entryThought || tab.entries[0].text != "I should check the file" {
		t.Errorf("thought block = %+v, want accreted thought text", tab.entries[0])
	}
	if tab.entries[1].kind != entryMessage || tab.entries[1].text != "Here is the answer" {
		t.Errorf("message block = %+v, want fresh message text", tab.entries[1])
	}
}

func TestApplyPlanReplacesInPlace(t *testing.T) {
	var a acpTabs
	a.upsert("s", "s")
	a.apply("s", acpclient.Event{Kind: acpclient.EventPlan, State: acpclient.StateStreaming, Plan: []acpclient.PlanEntry{
		{Content: "a", Status: "in_progress"},
	}})
	a.apply("s", acpclient.Event{Kind: acpclient.EventPlan, State: acpclient.StateStreaming, Plan: []acpclient.PlanEntry{
		{Content: "a", Status: "completed"},
		{Content: "b", Status: "in_progress"},
	}})

	tab := a.tabs[0]
	plans := 0
	for _, e := range tab.entries {
		if e.kind == entryPlan {
			plans++
		}
	}
	if plans != 1 {
		t.Fatalf("plan blocks = %d, want 1 (replaced in place)", plans)
	}
	if got := tab.entries[0].plan; len(got) != 2 || got[0].Status != "completed" {
		t.Errorf("plan = %+v, want the latest 2-entry list", got)
	}
}

func TestRenderACPTabBodyCollapsesToolByDefault(t *testing.T) {
	tab := &acpTab{id: "s", title: "s", curMsg: -1}
	tab.entries = []transcriptEntry{
		{kind: entryTool, text: "Run tests", toolKind: "execute", toolStatus: "completed", toolOutput: "PASS"},
		{kind: entryPlan, plan: []acpclient.PlanEntry{{Content: "ship it", Status: "in_progress"}}},
	}

	// Collapsed (default): the summary line shows, but the output does not, and a
	// ▸ affordance signals it can be expanded.
	collapsed := renderACPTabBody(tab, 60, 30, false)
	for _, want := range []string{"Run tests", "completed", "▸", "plan", "ship it"} {
		if !strings.Contains(collapsed, want) {
			t.Errorf("collapsed body missing %q; got:\n%s", want, collapsed)
		}
	}
	if strings.Contains(collapsed, "PASS") {
		t.Errorf("collapsed body should hide tool output; got:\n%s", collapsed)
	}

	// Expanded (z): the output appears and the affordance flips to ▾.
	expanded := renderACPTabBody(tab, 60, 30, true)
	for _, want := range []string{"Run tests", "PASS", "▾"} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded body missing %q; got:\n%s", want, expanded)
		}
	}
}

func TestApplyTerminalPreservesTranscript(t *testing.T) {
	var a acpTabs
	a.upsert("s", "s")
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: "partial"})
	a.apply("s", acpclient.Event{State: acpclient.StateDisconnected})

	tab := a.tabs[0]
	if tab.status != acpclient.StateDisconnected {
		t.Errorf("status = %q, want disconnected", tab.status)
	}
	msgs := messagesOf(tab)
	if len(msgs) != 1 || msgs[0] != "partial" {
		t.Errorf("transcript after disconnect = %v, want [\"partial\"]", msgs)
	}
}

func TestNextPrevWrapAround(t *testing.T) {
	var a acpTabs
	a.upsert("a", "a")
	a.upsert("b", "b")
	a.upsert("c", "c")
	a.prev() // from 0 wraps to last
	if a.sel != 2 {
		t.Errorf("prev from first = %d, want 2", a.sel)
	}
	a.next() // wraps back to 0
	if a.sel != 0 {
		t.Errorf("next from last = %d, want 0", a.sel)
	}
	a.next()
	if a.sel != 1 {
		t.Errorf("next = %d, want 1", a.sel)
	}
}

func TestStatusLabelAndGlyphCoverAllStates(t *testing.T) {
	cases := []struct {
		state acpclient.State
		label string
		glyph string
	}{
		{acpclient.StateConnecting, "connecting", "○"},
		{acpclient.StateConnected, "connected", "○"},
		{acpclient.StateStreaming, "streaming", "●"},
		{acpclient.StateCompleted, "completed", "✓"},
		{acpclient.StateDisconnected, "disconnected", "✗"},
		{acpclient.StateErrored, "errored", "✗"},
	}
	for _, c := range cases {
		if got := acpStatusLabel(c.state); got != c.label {
			t.Errorf("label(%q) = %q, want %q", c.state, got, c.label)
		}
		if got := acpStatusGlyph(c.state); got != c.glyph {
			t.Errorf("glyph(%q) = %q, want %q", c.state, got, c.glyph)
		}
	}
}

func TestRenderACPTabBarMarksSelected(t *testing.T) {
	tabs := []acpTab{
		{id: "a", title: "task-a", status: acpclient.StateStreaming},
		{id: "b", title: "plan-b", status: acpclient.StateConnected},
	}
	bar := renderACPTabBar(tabs, 1)
	if !strings.Contains(bar, "task-a") || !strings.Contains(bar, "plan-b") {
		t.Errorf("bar missing a tab title; got:\n%s", bar)
	}
}

func TestRenderACPTabBodyShowsPromptAndMessage(t *testing.T) {
	tab := &acpTab{id: "s", title: "s"}
	tab.curMsg = -1
	tab.entries = []transcriptEntry{
		{kind: entryPrompt, text: "do the thing"},
		{kind: entryMessage, text: "working on it"},
	}
	out := renderACPTabBody(tab, 40, 20, false)
	for _, want := range []string{"prompt", "do the thing", "agent", "working on it"} {
		if !strings.Contains(out, want) {
			t.Errorf("body missing %q; got:\n%s", want, out)
		}
	}
	// Prompt and message turns render as bordered cards (rounded corners).
	for _, corner := range []string{"╭", "╰"} {
		if !strings.Contains(out, corner) {
			t.Errorf("expected message boxes (missing %q); got:\n%s", corner, out)
		}
	}
}

func TestRenderACPTabBodyEmptyState(t *testing.T) {
	tab := &acpTab{id: "s", title: "s", curMsg: -1}
	out := renderACPTabBody(tab, 40, 10, false)
	if !strings.Contains(out, "waiting") {
		t.Errorf("empty body should hint it's waiting; got:\n%s", out)
	}
}

// TestModelACPViewLifecycle drives the tab view through the model: enter via the
// `a` key, ingest a tab-added + prompt + streamed message, and confirm the
// rendered ACP view shows the streamed content. Then leave with esc.
func TestModelACPViewLifecycle(t *testing.T) {
	m := newModel(nil, nil, nil, nil)
	m.acpCh = make(chan acpTabEvent) // non-nil so `a` opens the view
	m.width, m.height = 100, 30

	// Feed lifecycle events as the watcher would.
	step, _ := m.Update(acpTabEvent{kind: acpTabAdded, id: "task-x", title: "task-x"})
	m = step.(model)
	step, _ = m.Update(acpTabEvent{kind: acpTabPrompt, id: "task-x", text: "read the instructions"})
	m = step.(model)
	step, _ = m.Update(acpTabEvent{kind: acpTabStream, id: "task-x", ev: acpclient.Event{State: acpclient.StateStreaming, Text: "on it now"}})
	m = step.(model)

	if len(m.acp.tabs) != 1 {
		t.Fatalf("tabs = %d, want 1", len(m.acp.tabs))
	}

	// Open the view.
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = step.(model)
	if !m.showACP {
		t.Fatal("`a` did not open the ACP view")
	}
	out := m.View()
	for _, want := range []string{"task-x", "read the instructions", "on it now", "streaming"} {
		if !strings.Contains(out, want) {
			t.Errorf("ACP view missing %q; got:\n%s", want, out)
		}
	}

	// Leave the view.
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = step.(model)
	if m.showACP {
		t.Error("esc did not leave the ACP view")
	}
}

// TestModelACPToolExpandToggle drives the z-key tool collapse/expand through the
// model: a tool call renders collapsed (output hidden) by default, z reveals the
// output, and z again re-collapses it.
func TestModelACPToolExpandToggle(t *testing.T) {
	m := newModel(nil, nil, nil, nil)
	m.acpCh = make(chan acpTabEvent)
	m.width, m.height = 100, 30

	step, _ := m.Update(acpTabEvent{kind: acpTabAdded, id: "task-x", title: "task-x"})
	m = step.(model)
	step, _ = m.Update(acpTabEvent{kind: acpTabStream, id: "task-x", ev: acpclient.Event{
		Kind: acpclient.EventToolCall, ToolCallID: "t1", ToolTitle: "Run tests",
		ToolKind: "execute", ToolStatus: "completed", Text: "SECRET_OUTPUT",
	}})
	m = step.(model)
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}) // open view
	m = step.(model)

	if out := m.View(); strings.Contains(out, "SECRET_OUTPUT") {
		t.Errorf("tool output should be collapsed by default; got:\n%s", out)
	}

	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")}) // expand
	m = step.(model)
	if !m.acpToolsExpanded {
		t.Fatal("z did not set acpToolsExpanded")
	}
	if out := m.View(); !strings.Contains(out, "SECRET_OUTPUT") {
		t.Errorf("z should reveal tool output; got:\n%s", out)
	}

	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")}) // collapse again
	m = step.(model)
	if m.acpToolsExpanded {
		t.Fatal("second z did not collapse")
	}
	if out := m.View(); strings.Contains(out, "SECRET_OUTPUT") {
		t.Errorf("second z should re-hide tool output; got:\n%s", out)
	}
}

// TestRenderACPViewNestedBoxesIntact guards against the message/prompt cards
// tearing when nested inside the transcript border: if the body is rendered
// wider than the outer box's true text area, every card line wraps and its
// corners split across two rows. A clean top border has ╭ and ╮ on ONE line, so
// with an outer box plus an inner prompt card there must be at least two such
// complete-top-border lines.
func TestRenderACPViewNestedBoxesIntact(t *testing.T) {
	m := newModel(nil, nil, nil, nil)
	m.width, m.height = 90, 34
	m.showACP = true
	tab := acpTab{id: "s", title: "s", status: acpclient.StateStreaming, curMsg: -1}
	tab.entries = []transcriptEntry{
		{kind: entryPrompt, text: "Read .orch/instructions.md and .orch/context.yaml, then follow the instructions."},
	}
	m.acp.tabs = []acpTab{tab}

	out := m.renderACPView()
	complete := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "╮") {
			complete++
		}
	}
	if complete < 2 {
		t.Fatalf("expected >=2 intact top borders (outer + prompt card), got %d; view:\n%s", complete, out)
	}
}

func TestModelACPKeyNavigatesTabs(t *testing.T) {
	m := newModel(nil, nil, nil, nil)
	m.acpCh = make(chan acpTabEvent)
	m.width, m.height = 100, 30
	for _, id := range []string{"a", "b", "c"} {
		step, _ := m.Update(acpTabEvent{kind: acpTabAdded, id: id, title: id})
		m = step.(model)
	}
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = step.(model)
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = step.(model)
	if m.acp.sel != 1 {
		t.Errorf("after right, sel = %d, want 1", m.acp.sel)
	}
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = step.(model)
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = step.(model)
	if m.acp.sel != 2 {
		t.Errorf("after wrapping left, sel = %d, want 2", m.acp.sel)
	}
}

func TestAKeyIgnoredWithoutACPSource(t *testing.T) {
	m := newModel(nil, nil, nil, nil) // acpCh stays nil
	m.width, m.height = 100, 30
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = step.(model)
	if m.showACP {
		t.Error("`a` opened the ACP view with no ACP source wired in")
	}
}

// messagesOf returns the text of every assistant-message entry in a tab.
func messagesOf(t acpTab) []string {
	var out []string
	for _, e := range t.entries {
		if e.kind == entryMessage {
			out = append(out, e.text)
		}
	}
	return out
}
