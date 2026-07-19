package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestCenterWindow(t *testing.T) {
	cases := []struct {
		name               string
		n, sel, maxRows    int
		wantStart, wantEnd int
	}{
		{"fits: whole list", 3, 2, 10, 0, 3},
		{"no selection pins top", 20, -1, 5, 0, 5},
		{"selection centered", 20, 10, 5, 8, 13}, // 10 - 5/2 = 8
		{"clamp at top", 20, 1, 5, 0, 5},         // 1-2 < 0 -> 0
		{"clamp at bottom", 20, 19, 5, 15, 20},   // start capped at n-maxRows
		{"exact fit", 5, 4, 5, 0, 5},
		{"zero rows", 20, 3, 0, 0, 0},
		{"empty list", 0, 0, 5, 0, 0},
	}
	for _, tc := range cases {
		start, end := centerWindow(tc.n, tc.sel, tc.maxRows)
		if start != tc.wantStart || end != tc.wantEnd {
			t.Errorf("%s: centerWindow(%d,%d,%d) = (%d,%d), want (%d,%d)",
				tc.name, tc.n, tc.sel, tc.maxRows, start, end, tc.wantStart, tc.wantEnd)
		}
		// The window never exceeds the viewport or the list.
		if end-start > tc.maxRows && tc.maxRows > 0 {
			t.Errorf("%s: window %d rows exceeds maxRows %d", tc.name, end-start, tc.maxRows)
		}
		// When there is a selection and the list overflows, it must be inside.
		if tc.maxRows > 0 && tc.n > tc.maxRows && tc.sel >= 0 {
			if tc.sel < start || tc.sel >= end {
				t.Errorf("%s: selected %d not in window [%d,%d)", tc.name, tc.sel, start, end)
			}
		}
	}
}

func TestTasksPaneScrollsToSelection(t *testing.T) {
	const n = 20
	tasks := make([]TaskView, n)
	for i := range tasks {
		tasks[i] = TaskView{
			Name:   fmt.Sprintf("task-%02d", i),
			Path:   fmt.Sprintf("/p/tasks/%02d.yaml", i),
			Status: "ready",
		}
	}
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m.width = 120
	m.projects = []ProjectView{{Name: "p", Path: "/p", Status: "working", Tasks: tasks}}
	m.projSel = "/p"
	m.taskSel = tasks[15].Path // far past the first window

	// height 10 -> innerHeight 8 -> maxRows 5, so only 5 of 20 rows show.
	out := m.renderTasks(m.width, 10)
	if !strings.Contains(out, "task-15") {
		t.Errorf("selected row task-15 should be visible after scrolling; got:\n%s", out)
	}
	if strings.Contains(out, "task-00") {
		t.Errorf("top row task-00 should have scrolled out of view; got:\n%s", out)
	}
}

func TestSessionsPaneScrollsToSelection(t *testing.T) {
	const n = 20
	sessions := make([]SessionView, n)
	for i := range sessions {
		sessions[i] = SessionView{
			ID:        fmt.Sprintf("sess-%02d", i),
			AgentName: fmt.Sprintf("agent-%02d", i),
			Project:   "p",
			Status:    "running",
		}
	}
	m := newModel(nil, nil, nil, &fakeAttacher{})
	m.width = 120
	m.sessLoaded = true
	m.sessions = sessions
	m.sessSel = sessions[15].ID

	out := m.renderSessions(m.width, 10)
	if !strings.Contains(out, "agent-15") {
		t.Errorf("selected session agent-15 should be visible after scrolling; got:\n%s", out)
	}
	if strings.Contains(out, "agent-00") {
		t.Errorf("top session agent-00 should have scrolled out of view; got:\n%s", out)
	}
}
