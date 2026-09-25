package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestTLTogglesLeftColumn(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	if !m.leftVisible {
		t.Fatal("leftVisible should default to true")
	}
	m = pressKey(m, "t")
	m = pressKey(m, "l")
	if m.leftVisible {
		t.Error("tl should have toggled leftVisible off")
	}
	if m.pendingKey != "" {
		t.Errorf("pendingKey = %q after tl, want cleared", m.pendingKey)
	}
	m = pressKey(m, "t")
	m = pressKey(m, "l")
	if !m.leftVisible {
		t.Error("second tl should have toggled leftVisible back on")
	}
}

func TestTRTogglesRightColumn(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	if m.rightVisible {
		t.Fatal("rightVisible should default to false — the detail pane is opt-in")
	}
	m = pressKey(m, "t")
	m = pressKey(m, "r")
	if !m.rightVisible {
		t.Error("tr should have toggled rightVisible on")
	}
	if m.pendingKey != "" {
		t.Errorf("pendingKey = %q after tr, want cleared", m.pendingKey)
	}
	m = pressKey(m, "t")
	m = pressKey(m, "r")
	if m.rightVisible {
		t.Error("second tr should have toggled rightVisible back off")
	}
}

// TestTChordDoesNotResolveUntilCompletedOrTimedOut is the crux of the
// "tl"/"tr" chord: a lone "t" must not immediately toggle anything, or "tl"/
// "tr" could never be distinguished from "t" followed by an unrelated key.
func TestTChordDoesNotResolveUntilCompletedOrTimedOut(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m = pressKey(m, "t")
	if m.pendingKey != "t" {
		t.Fatalf("pendingKey = %q after t, want %q", m.pendingKey, "t")
	}
	if m.leftVisible != true || m.rightVisible != false {
		t.Errorf("column visibility changed before the chord resolved: left=%v right=%v", m.leftVisible, m.rightVisible)
	}
}

// TestTChordTimeoutClearsPendingKey covers the "t" alone case: no completing
// "l"/"r" arrives, so chordTimeoutDelay elapsing (simulated via resolveChord
// instead of a real sleep) just abandons the chord — there's no standalone
// "t" action left to fall back to.
func TestTChordTimeoutClearsPendingKey(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m = pressKey(m, "t")
	m = resolveChord(m)
	if m.pendingKey != "" {
		t.Errorf("pendingKey = %q after timeout, want cleared", m.pendingKey)
	}
	if m.leftVisible != true || m.rightVisible != false {
		t.Errorf("column visibility should be untouched by a chord that timed out: left=%v right=%v", m.leftVisible, m.rightVisible)
	}
}

// TestTChordFallsThroughToFreshKeyWhenNotLR covers "t" followed by any key
// other than "l"/"r": the pending "t" is abandoned and the new key is then
// handled normally, rather than being swallowed by the chord.
func TestTChordFallsThroughToFreshKeyWhenNotLR(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	step, _ := m.Update(sessionsMsg{views: []SessionView{{ID: "a"}, {ID: "b"}}})
	m = step.(model)

	m = pressKey(m, "t")
	m = pressKey(m, "j") // not l/r: abandons t, then moves the session selection

	if m.pendingKey != "" {
		t.Errorf("pendingKey = %q, want cleared", m.pendingKey)
	}
	if m.sessSel != "b" {
		t.Errorf("sessSel = %q, want %q (j should have been handled fresh)", m.sessSel, "b")
	}
}

// TestStaleChordTimeoutIsIgnored guards against a chord timer that fires
// after its chord already resolved a different way (e.g. "tl" completed the
// toggle before chordTimeoutDelay elapsed): the stale message must not
// re-trigger anything.
func TestStaleChordTimeoutIsIgnored(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m = pressKey(m, "t")
	staleSeq := m.pendingSeq
	m = pressKey(m, "l") // resolves the chord as toggle-left, clearing pendingKey

	next, _ := m.Update(chordTimeoutMsg{seq: staleSeq})
	m = next.(model)

	if m.leftVisible {
		t.Error("leftVisible should still be false; stale timeout must not re-toggle it")
	}
}

func TestFooterAdvertisesColumnToggle(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	m = sized.(model)
	view := m.View()
	if !strings.Contains(view, "tl/tr") {
		t.Errorf("footer should advertise the tl/tr column toggle; got:\n%s", view)
	}
	if !strings.Contains(view, "sessions/detail") {
		t.Errorf("footer should describe tl/tr as toggling sessions/detail columns; got:\n%s", view)
	}
	if !strings.Contains(view, "⌥h/⌥l") {
		t.Errorf("footer should advertise the ⌥h/⌥l column-switch keys; got:\n%s", view)
	}
}

// TestRightColumnWidthIsHalfTerminalWidth covers the requirement that the
// detail right column, unlike the fixed-width sessions column, is sized to
// half of whatever the terminal's current width is — and stays that way
// live across a resize.
func TestRightColumnWidthIsHalfTerminalWidth(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m.rightVisible = true
	m.width, m.height = 200, 40

	l := m.computeLayout()
	if want := m.width / 2; l.rightW != want {
		t.Errorf("rightW = %d, want %d (half of terminal width %d)", l.rightW, want, m.width)
	}

	m.width = 300
	l = m.computeLayout()
	if want := m.width / 2; l.rightW != want {
		t.Errorf("after resize: rightW = %d, want %d (half of terminal width %d)", l.rightW, want, m.width)
	}
}

// TestTaskPaneMovedToCenterColumn is the layout-level companion to the
// pane-assignment change: Tasks stacks in the center column below Projects,
// sized by splitCenterColumn, while the right column ("tr") holds the
// project detail dashboard instead.
func TestTaskPaneMovedToCenterColumn(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m.rightVisible = true
	m.width, m.height = 200, 40
	m.projects = []ProjectView{{
		Name:   "p",
		Path:   "/p",
		Status: "working",
		Tasks:  []TaskView{{Name: "zzztask", Path: "/p/tasks/t.yaml", Status: "ready"}},
	}}
	m.projSel = "/p"

	l := m.computeLayout()
	if l.tasksH <= 0 {
		t.Fatalf("precondition: tasks should have room in the center column; tasksH=%d", l.tasksH)
	}
	if want := m.width / 2; l.rightW != want {
		t.Fatalf("precondition: right column should be half the terminal width; rightW=%d, want %d", l.rightW, want)
	}

	view := m.View()
	if !strings.Contains(view, "zzztask") {
		t.Errorf("view should render the selected project's tasks in the center column; got:\n%s", view)
	}
}

func TestComputeLayoutDropsColumnsWhenToggledOff(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m.rightVisible = true
	m.width, m.height = 200, 40

	full := m.computeLayout()
	if full.leftW == 0 || full.rightW == 0 {
		t.Fatalf("precondition: both columns should show when both are toggled on; got leftW=%d rightW=%d", full.leftW, full.rightW)
	}

	m.leftVisible = false
	m.rightVisible = false
	collapsed := m.computeLayout()
	if collapsed.leftW != 0 {
		t.Errorf("leftW = %d, want 0 when leftVisible is false", collapsed.leftW)
	}
	if collapsed.rightW != 0 {
		t.Errorf("rightW = %d, want 0 when rightVisible is false", collapsed.rightW)
	}
	if collapsed.centerW <= full.centerW {
		t.Errorf("centerW = %d, want it to grow beyond %d once both columns are hidden", collapsed.centerW, full.centerW)
	}
}

// TestComputeLayoutHidesColumnWhenTerminalTooNarrow covers the fallback that
// keeps a toggled-on column from starving the center column: on a narrow
// terminal the column is dropped for this render regardless of the toggle
// state, and reappears once the terminal widens again.
func TestComputeLayoutHidesColumnWhenTerminalTooNarrow(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m.width, m.height = 50, 40 // too narrow for leftColumnWidth + minCenterWidth

	l := m.computeLayout()
	if l.leftW != 0 {
		t.Errorf("leftW = %d, want 0 on a terminal too narrow to fit it", l.leftW)
	}

	m.width = 200
	l = m.computeLayout()
	if l.leftW == 0 {
		t.Error("leftW = 0, want it to reappear once the terminal is wide enough again")
	}
}

// TestMouseClickOnAuditStripFocusesIt covers the click-to-focus affordance
// every center-column pane has (see handleMouse) applied to Audit — alt-j/
// alt-k also reaches Audit as part of the center column's focus cycle (see
// cycleCenterFocus and TestAltJKCyclesCenterColumnPanesIncludingAudit in
// tasks_focus_test.go).
func TestMouseClickOnAuditStripFocusesIt(t *testing.T) {
	m := newModel(nil, nil, make(<-chan []string), &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	m = sized.(model)
	l := m.computeLayout()
	if l.auditH <= 0 {
		t.Fatalf("precondition: audit pane should have room; auditH=%d", l.auditH)
	}

	// Audit now stacks at the bottom of the center column rather than
	// spanning a full-width row below it, so the click must land within the
	// center column's X range and the audit band's Y range (the last
	// l.auditH rows of the column's stack).
	final, _ := m.Update(tea.MouseMsg{
		X:      l.leftW + 5,
		Y:      l.bodyH - 1,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	m = final.(model)
	if m.focus != paneAudit {
		t.Errorf("click on audit pane should focus paneAudit, got %v", m.focus)
	}
}

// TestAuditStacksInCenterColumnBelowTasks covers the center-column-audit
// requirement directly at the layout level: Audit fills out the bottom of
// the center column's own vertical stack (projectsH + tasksH + auditH ==
// bodyH) rather than being a fourth full-width row below the three columns,
// and renderAudit draws it at the column's width, not the terminal's.
func TestAuditStacksInCenterColumnBelowTasks(t *testing.T) {
	m := newModel(nil, nil, make(<-chan []string), &fakeAttacher{})
	m.width, m.height = 200, 40
	l := m.computeLayout()
	if l.auditH <= 0 {
		t.Fatalf("precondition: audit should have room; auditH=%d", l.auditH)
	}
	if l.tasksH <= 0 {
		t.Fatalf("precondition: tasks should have room; tasksH=%d", l.tasksH)
	}
	if got, want := l.projectsH+l.tasksH+l.auditH, l.bodyH; got != want {
		t.Errorf("projectsH+tasksH+auditH = %d, want bodyH (%d): audit should fill out the center column's stack instead of sitting below it", got, want)
	}

	rendered := m.renderAudit(l.centerW, l.auditH)
	for _, line := range strings.Split(rendered, "\n") {
		if w := lipgloss.Width(line); w > l.centerW {
			t.Errorf("audit line width %d exceeds centerW %d (terminal width is %d): audit should render at the column's width", w, l.centerW, m.width)
		}
	}
}

// TestSideColumnsFullHeightRegardlessOfAudit is the regression test for the
// original bug: because audit's height used to be carved out of the window
// height before the columns were sized, showing Audit shrank the left
// (sessions) and right (detail) columns even though it never visually
// occupied any of their space. Now that audit sizing is scoped to the
// center column, the side columns must get the full body height (window
// height minus only the footer) whether or not Audit is showing.
func TestSideColumnsFullHeightRegardlessOfAudit(t *testing.T) {
	withAudit := newModel(nil, nil, make(<-chan []string), &fakeAttacher{})
	withAudit.rightVisible = true
	withAudit.width, withAudit.height = 200, 40
	lWith := withAudit.computeLayout()
	if lWith.auditH <= 0 {
		t.Fatalf("precondition: audit should have room; auditH=%d", lWith.auditH)
	}

	withoutAudit := newModel(nil, nil, nil, &fakeAttacher{})
	withoutAudit.rightVisible = true
	withoutAudit.width, withoutAudit.height = 200, 40
	lWithout := withoutAudit.computeLayout()
	if lWithout.auditH != 0 {
		t.Fatalf("precondition: no audit source wired up, want auditH=0, got %d", lWithout.auditH)
	}

	if lWith.sessionsH != lWith.bodyH {
		t.Errorf("sessionsH = %d, want it to equal bodyH (%d) — full height minus only the footer", lWith.sessionsH, lWith.bodyH)
	}
	if lWith.yamlH != lWith.bodyH {
		t.Errorf("yamlH = %d, want it to equal bodyH (%d) — full height minus only the footer", lWith.yamlH, lWith.bodyH)
	}
	if lWith.sessionsH != lWithout.sessionsH {
		t.Errorf("sessionsH = %d with audit showing, %d without; want equal — audit must not shrink the left column", lWith.sessionsH, lWithout.sessionsH)
	}
	if lWith.yamlH != lWithout.yamlH {
		t.Errorf("yamlH = %d with audit showing, %d without; want equal — audit must not shrink the right column", lWith.yamlH, lWithout.yamlH)
	}
}

func TestMouseClickInHiddenLeftColumnFallsThroughToCenter(t *testing.T) {
	att := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, att)
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	m = sized.(model)
	m.leftVisible = false
	step, _ := m.Update(sessionsMsg{views: []SessionView{{ID: "a", TmuxTarget: "orch:task-alpha"}}})
	m = step.(model)

	// A click at the x-coordinate that would have been inside the sessions
	// column when visible must now land in the center column instead.
	final, cmd := m.Update(tea.MouseMsg{
		X:      5,
		Y:      4,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	m = final.(model)
	_ = runCmd(t, cmd)
	if len(att.targets) != 0 {
		t.Errorf("targets = %v, want none (sessions column is hidden)", att.targets)
	}
	if m.focus == paneSessions {
		t.Error("click should not have focused the hidden sessions pane")
	}
}
