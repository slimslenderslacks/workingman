package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// zoomTestModel builds a fully-populated, sized model so View renders every
// pane. Default focus is paneSessions (see newModel).
func zoomTestModel() model {
	m := newModel(nil, nil, nil, &fakeAttacher{})
	// Wide enough that the sessions left column and tasks right column both
	// have room alongside the projects/YAML center column.
	m.width, m.height = 200, 40
	m.loaded = true
	m.sessLoaded = true
	m.projects = []ProjectView{{Name: "alpha", Path: "/a", Status: "working"}}
	m.projSel = "/a"
	m.sessions = []SessionView{{ID: "s", AgentName: "task", Project: "alpha", Status: "running"}}
	m.sessSel = "s"
	return m
}

func pressKey(m model, s string) model {
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return next.(model)
}

// resolveChord simulates chordTimeoutDelay elapsing on the model's current
// pending chord (if any), same as pressKey but for the timer path instead of
// a keystroke. A lone "t" (not the start of "tl"/"tr") only takes effect once
// it's clear no completing key is coming — in real usage that's the timeout
// firing; tests trigger it directly instead of sleeping.
func resolveChord(m model) model {
	next, _ := m.Update(chordTimeoutMsg{seq: m.pendingSeq})
	return next.(model)
}

func TestZoomTogglesFocusedPaneOnly(t *testing.T) {
	m := zoomTestModel() // focus == paneSessions

	// Panes no longer carry titles, so identify each by content unique to it:
	// the projects card's "no tasks" breakdown, the tasks pane's "(none)"
	// empty state, and the sessions table's "sandbox" column header.
	const (
		projectsSig = "no tasks"
		tasksSig    = "(none)"
		sessionsSig = "sandbox"
	)

	// Normal layout shows every pane.
	normal := m.View()
	for _, want := range []string{projectsSig, tasksSig, sessionsSig} {
		if !strings.Contains(normal, want) {
			t.Fatalf("normal view missing %q; got:\n%s", want, normal)
		}
	}

	// z maximizes the focused (sessions) pane: only it renders.
	m = pressKey(m, "z")
	if !m.zoomed {
		t.Fatal("z did not set zoomed")
	}
	zoomed := m.View()
	if !strings.Contains(zoomed, sessionsSig) {
		t.Errorf("zoomed view should show the focused Sessions pane; got:\n%s", zoomed)
	}
	for _, gone := range []string{projectsSig, tasksSig} {
		if strings.Contains(zoomed, gone) {
			t.Errorf("zoomed view should hide %q; got:\n%s", gone, zoomed)
		}
	}

	// z again restores the full stacked layout.
	m = pressKey(m, "z")
	if m.zoomed {
		t.Fatal("second z did not clear zoomed")
	}
	restored := m.View()
	for _, want := range []string{projectsSig, tasksSig, sessionsSig} {
		if !strings.Contains(restored, want) {
			t.Errorf("restored view missing %q; got:\n%s", want, restored)
		}
	}
}

func TestZoomFollowsFocus(t *testing.T) {
	m := zoomTestModel()
	m = pressKey(m, "z") // maximize sessions
	if !m.zoomed || m.focus != paneSessions {
		t.Fatalf("precondition: want zoomed sessions, got zoomed=%v focus=%v", m.zoomed, m.focus)
	}
	// alt-l moves focus off the sessions column onto center; zoom stays on
	// and now maximizes the newly focused pane. (alt-h is a no-op here since
	// sessions is already the leftmost column — alt-h/alt-l step one column
	// at a time and never wrap. alt-j/alt-k don't apply either — they only
	// cycle within the center column's Projects/Tasks/Audit stack.)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}, Alt: true})
	m = next.(model)
	if !m.zoomed {
		t.Error("moving focus should not exit zoom")
	}
	if m.focus == paneSessions {
		t.Error("alt-l should have moved focus off the sessions pane")
	}
}

func TestZoomFooterHint(t *testing.T) {
	m := zoomTestModel()
	if !strings.Contains(m.renderFooter(), "z: maximize pane") {
		t.Errorf("normal footer should advertise maximize; got: %s", m.renderFooter())
	}
	m = pressKey(m, "z")
	if !strings.Contains(m.renderFooter(), "z: restore panes") {
		t.Errorf("zoomed footer should advertise restore; got: %s", m.renderFooter())
	}
}
