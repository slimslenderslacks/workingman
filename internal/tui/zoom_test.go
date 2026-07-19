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
	m.width, m.height = 120, 40
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

func TestZoomTogglesFocusedPaneOnly(t *testing.T) {
	m := zoomTestModel() // focus == paneSessions

	// Normal layout shows every pane.
	normal := m.View()
	for _, want := range []string{"Work Streams", "Tasks", "Agent Sessions"} {
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
	if !strings.Contains(zoomed, "Agent Sessions") {
		t.Errorf("zoomed view should show the focused Sessions pane; got:\n%s", zoomed)
	}
	for _, gone := range []string{"Work Streams", "Tasks"} {
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
	for _, want := range []string{"Work Streams", "Tasks", "Agent Sessions"} {
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
	// down cycles focus; zoom stays on and now maximizes the newly focused pane.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if !m.zoomed {
		t.Error("moving focus should not exit zoom")
	}
	if m.focus == paneSessions {
		t.Error("down should have moved focus off the sessions pane")
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
