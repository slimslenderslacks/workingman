package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	if !m.rightVisible {
		t.Fatal("rightVisible should default to true")
	}
	m = pressKey(m, "t")
	m = pressKey(m, "r")
	if m.rightVisible {
		t.Error("tr should have toggled rightVisible off")
	}
	if m.pendingKey != "" {
		t.Errorf("pendingKey = %q after tr, want cleared", m.pendingKey)
	}
	m = pressKey(m, "t")
	m = pressKey(m, "r")
	if !m.rightVisible {
		t.Error("second tr should have toggled rightVisible back on")
	}
}

// TestTChordDoesNotResolveUntilCompletedOrTimedOut is the crux of the
// "without breaking the existing single-key t/p toggle" requirement: a lone
// "t" must not immediately fire the standalone YAML-source switch, or "tl"/
// "tr" could never be distinguished from "t" followed by an unrelated key.
func TestTChordDoesNotResolveUntilCompletedOrTimedOut(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m = pressKey(m, "t")
	if m.pendingKey != "t" {
		t.Fatalf("pendingKey = %q after t, want %q", m.pendingKey, "t")
	}
	if m.yamlSrc != yamlSourceProject {
		t.Errorf("yamlSrc changed before the chord resolved: %v", m.yamlSrc)
	}
}

// TestTChordTimeoutResolvesStandaloneAction covers the "t" alone case: no
// completing "l"/"r" arrives, so chordTimeoutDelay elapsing (simulated via
// resolveChord instead of a real sleep) falls back to the plain YAML-viewer
// toggle.
func TestTChordTimeoutResolvesStandaloneAction(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m = pressKey(m, "t")
	m = resolveChord(m)
	if m.pendingKey != "" {
		t.Errorf("pendingKey = %q after timeout, want cleared", m.pendingKey)
	}
	if m.yamlSrc != yamlSourceTask {
		t.Errorf("yamlSrc = %v after chord timeout, want yamlSourceTask", m.yamlSrc)
	}
}

// TestTChordFallsThroughToFreshKeyWhenNotLR covers "t" followed by any key
// other than "l"/"r": the pending "t" resolves to its standalone action
// (switch YAML viewer to task) and the new key is then handled normally,
// rather than being swallowed by the chord.
func TestTChordFallsThroughToFreshKeyWhenNotLR(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	step, _ := m.Update(sessionsMsg{views: []SessionView{{ID: "a"}, {ID: "b"}}})
	m = step.(model)

	m = pressKey(m, "t")
	m = pressKey(m, "j") // not l/r: resolves t, then moves the session selection

	if m.yamlSrc != yamlSourceTask {
		t.Errorf("yamlSrc = %v, want yamlSourceTask (t should have resolved)", m.yamlSrc)
	}
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
// re-trigger the standalone "t" action.
func TestStaleChordTimeoutIsIgnored(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m = pressKey(m, "t")
	staleSeq := m.pendingSeq
	m = pressKey(m, "l") // resolves the chord as toggle-left, clearing pendingKey

	next, _ := m.Update(chordTimeoutMsg{seq: staleSeq})
	m = next.(model)

	if m.yamlSrc != yamlSourceProject {
		t.Errorf("stale chord timeout changed yamlSrc to %v, want it to stay yamlSourceProject", m.yamlSrc)
	}
	if m.leftVisible {
		t.Error("leftVisible should still be false; stale timeout must not re-toggle it")
	}
}

func TestFooterAdvertisesColumnToggle(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	m = sized.(model)
	if !strings.Contains(m.View(), "tl/tr") {
		t.Errorf("footer should advertise the tl/tr column toggle; got:\n%s", m.View())
	}
}

func TestComputeLayoutDropsColumnsWhenToggledOff(t *testing.T) {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m.width, m.height = 200, 40

	full := m.computeLayout()
	if full.leftW == 0 || full.rightW == 0 {
		t.Fatalf("precondition: both columns should show by default; got leftW=%d rightW=%d", full.leftW, full.rightW)
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
