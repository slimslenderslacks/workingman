package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestReconcileSelectionKeepsExistingID(t *testing.T) {
	views := []SessionView{
		{ID: "a", AgentName: "task", Project: "alpha"},
		{ID: "b", AgentName: "project", Project: "bravo"},
	}
	if got := reconcileSelection(views, "b"); got != "b" {
		t.Errorf("reconcileSelection kept ID = %q, want %q", got, "b")
	}
}

func TestReconcileSelectionFallsBackToFirst(t *testing.T) {
	views := []SessionView{
		{ID: "a"},
		{ID: "b"},
	}
	if got := reconcileSelection(views, "gone"); got != "a" {
		t.Errorf("reconcileSelection fallback = %q, want %q", got, "a")
	}
	if got := reconcileSelection(views, ""); got != "a" {
		t.Errorf("reconcileSelection empty prev = %q, want %q", got, "a")
	}
}

func TestReconcileSelectionEmpty(t *testing.T) {
	if got := reconcileSelection(nil, "x"); got != "" {
		t.Errorf("reconcileSelection on empty = %q, want %q", got, "")
	}
}

func TestMoveSelectionClampsAtEnds(t *testing.T) {
	views := []SessionView{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	if got := moveSelection(views, "a", -1); got != "a" {
		t.Errorf("up-from-first = %q, want %q", got, "a")
	}
	if got := moveSelection(views, "c", 1); got != "c" {
		t.Errorf("down-from-last = %q, want %q", got, "c")
	}
	if got := moveSelection(views, "a", 1); got != "b" {
		t.Errorf("down from a = %q, want %q", got, "b")
	}
	if got := moveSelection(views, "b", -1); got != "a" {
		t.Errorf("up from b = %q, want %q", got, "a")
	}
}

func TestMoveSelectionUnknownCurrent(t *testing.T) {
	views := []SessionView{{ID: "a"}, {ID: "b"}}
	if got := moveSelection(views, "", 1); got != "a" {
		t.Errorf("down from unset = %q, want %q", got, "a")
	}
	if got := moveSelection(views, "", -1); got != "b" {
		t.Errorf("up from unset = %q, want %q", got, "b")
	}
}

func TestSessionsMsgPopulatesPaneAndSelection(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, nil)
	if m.sessLoaded {
		t.Fatal("model with non-nil sessCh should start unloaded")
	}
	next, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", AgentName: "task", Project: "alpha", Status: "running"},
		{ID: "b", AgentName: "project", Project: "bravo", Status: "running"},
	}})
	m = next.(model)
	if !m.sessLoaded {
		t.Fatal("sessLoaded not set after sessionsMsg")
	}
	if len(m.sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2", len(m.sessions))
	}
	if m.sessSel != "a" {
		t.Errorf("sessSel after first snapshot = %q, want %q", m.sessSel, "a")
	}
}

func TestSessionsMsgPreservesSelectionAcrossRefresh(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, nil)
	step1, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", AgentName: "task"},
		{ID: "b", AgentName: "project"},
		{ID: "c", AgentName: "wolf"},
	}})
	m = step1.(model)
	// Move selection to "b".
	step2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = step2.(model)
	if m.sessSel != "b" {
		t.Fatalf("after Right, sessSel = %q, want %q", m.sessSel, "b")
	}
	// Refresh: "b" still present, must remain selected even though "a" left.
	step3, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "b", AgentName: "project"},
		{ID: "c", AgentName: "wolf"},
	}})
	m = step3.(model)
	if m.sessSel != "b" {
		t.Errorf("after refresh, sessSel = %q, want %q (selection should be stable)", m.sessSel, "b")
	}
}

func TestSessionsMsgFallsBackWhenSelectionDisappears(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, nil)
	step1, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a"}, {ID: "b"},
	}})
	m = step1.(model)
	step2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = step2.(model)
	if m.sessSel != "b" {
		t.Fatalf("setup: sessSel = %q, want %q", m.sessSel, "b")
	}
	// "b" disappears — selection should fall back to first remaining row.
	step3, _ := m.Update(sessionsMsg{views: []SessionView{{ID: "a"}}})
	m = step3.(model)
	if m.sessSel != "a" {
		t.Errorf("after disappearance, sessSel = %q, want %q", m.sessSel, "a")
	}
}

func TestArrowKeysOnlyAffectSessionsWhenFocused(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, nil)
	step1, _ := m.Update(sessionsMsg{views: []SessionView{{ID: "a"}, {ID: "b"}}})
	m = step1.(model)
	// Focus the projects pane (down cycles pane focus).
	step2, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = step2.(model)
	if m.focus != paneProjects {
		t.Fatalf("expected projects focus after Down, got %v", m.focus)
	}
	// The selection key (right) should move the project selection, not the
	// sessions selection, while the projects pane is focused.
	step3, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = step3.(model)
	if m.sessSel != "a" {
		t.Errorf("Right moved sessSel while projects focused: got %q, want %q", m.sessSel, "a")
	}
}

func TestRenderSessionRowIncludesAgentProjectAndStatus(t *testing.T) {
	view := SessionView{ID: "a", AgentName: "task", Project: "alpha", Status: "running"}
	cols := sessionColumnWidths([]SessionView{view}, time.Time{}, 80)
	row := renderSessionRow(view, cols, time.Time{}, false)
	if !strings.Contains(row, "task") {
		t.Errorf("row missing agent name; got:\n%s", row)
	}
	if !strings.Contains(row, "alpha") {
		t.Errorf("row missing project; got:\n%s", row)
	}
	if !strings.Contains(row, "running") {
		t.Errorf("row missing status text; got:\n%s", row)
	}
}

func TestRenderSessionRowSelectedHasMarker(t *testing.T) {
	view := SessionView{ID: "a", AgentName: "task", Project: "alpha", Status: "running"}
	cols := sessionColumnWidths([]SessionView{view}, time.Time{}, 80)
	rowOn := renderSessionRow(view, cols, time.Time{}, true)
	rowOff := renderSessionRow(view, cols, time.Time{}, false)
	if !strings.Contains(rowOn, sessionMarkerSelected) {
		t.Errorf("selected row missing marker %q; got:\n%s", sessionMarkerSelected, rowOn)
	}
	if strings.Contains(rowOff, sessionMarkerSelected) {
		t.Errorf("unselected row contains selected-marker %q; got:\n%s", sessionMarkerSelected, rowOff)
	}
}

func TestSessionAgeFormatsCompactly(t *testing.T) {
	base := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		started time.Time
		want    string
	}{
		{"zero", time.Time{}, "—"},
		{"seconds", base.Add(-45 * time.Second), "45s"},
		{"minutes", base.Add(-4 * time.Minute), "4m"},
		{"hours", base.Add(-90 * time.Minute), "1h30m"},
		{"days", base.Add(-50 * time.Hour), "2d02h"},
		{"future clamps to zero", base.Add(30 * time.Second), "0s"},
	}
	for _, c := range cases {
		if got := sessionAge(c.started, base); got != c.want {
			t.Errorf("%s: sessionAge = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRenderSessionRowShowsAge(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	view := SessionView{
		ID: "a", AgentName: "task", Project: "alpha", Status: "running",
		StartedAt: now.Add(-5 * time.Minute),
	}
	cols := sessionColumnWidths([]SessionView{view}, now, 80)
	if got := renderSessionRow(view, cols, now, false); !strings.Contains(got, "5m") {
		t.Errorf("row should show age 5m; got:\n%s", got)
	}
}

// A wide pane must spend its width on the data: long task/sandbox names show in
// full (no truncation) and the columns fill the pane rather than leaving dead
// space on the right. This is the "maximize horizontal space" behaviour.
func TestSessionColumnsFillWidthWithoutTruncation(t *testing.T) {
	const innerWidth = 200
	longSandbox := "entra-eval-marp-entra-presentation"
	longTask := "marp-entra-presentation"
	view := SessionView{
		ID: "a", AgentName: "task", Project: "entra-eval", TaskName: longTask,
		Status: "running", SandboxName: longSandbox,
	}
	cols := sessionColumnWidths([]SessionView{view}, time.Time{}, innerWidth)

	if cols.sandbox < len(longSandbox) {
		t.Errorf("sandbox column %d too narrow to show %q (%d) on a wide pane",
			cols.sandbox, longSandbox, len(longSandbox))
	}
	if cols.task < len(longTask) {
		t.Errorf("task column %d too narrow to show %q (%d) on a wide pane",
			cols.task, longTask, len(longTask))
	}
	got := renderSessionRow(view, cols, time.Time{}, false)
	if !strings.Contains(got, longSandbox) {
		t.Errorf("wide pane should show the full sandbox name; got:\n%s", got)
	}
	if !strings.Contains(got, longTask) {
		t.Errorf("wide pane should show the full task name; got:\n%s", got)
	}
	// Columns consume the full inner width — no trailing dead space.
	if cols.totalWidth() != innerWidth {
		t.Errorf("columns should fill the pane exactly; totalWidth=%d, want %d",
			cols.totalWidth(), innerWidth)
	}
}

// When the pane is too narrow for every column at content width, the flexible
// columns shrink to their minimums instead of overflowing.
func TestSessionColumnsShrinkWhenNarrow(t *testing.T) {
	view := SessionView{
		ID: "a", AgentName: "planning", Project: "some-long-project-name",
		TaskName: "a-fairly-long-task-name", Status: "running",
		SandboxName: "a-long-sandbox-name-here",
	}
	cols := sessionColumnWidths([]SessionView{view}, time.Time{}, 40)
	if cols.project < sessionColMinProject || cols.task < sessionColMinTask ||
		cols.sandbox < sessionColMinSandbox || cols.agent < sessionColMinAgent {
		t.Errorf("flexible columns fell below their minimums: %+v", cols)
	}
}

func TestRenderSessionsEmpty(t *testing.T) {
	m := newModel(nil, nil, nil, nil)
	out := m.renderSessions(30, 10)
	if !strings.Contains(out, "(none)") {
		t.Errorf("empty sessions should say '(none)'; got:\n%s", out)
	}
}

func TestRenderSessionsLoadingState(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, nil)
	out := m.renderSessions(30, 10)
	if !strings.Contains(out, "loading") {
		t.Errorf("unloaded sessions should show loading hint; got:\n%s", out)
	}
}

func TestRenderSessionsShowsSelectedMarker(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, nil)
	step, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", AgentName: "task", Project: "alpha", Status: "running"},
		{ID: "b", AgentName: "project", Project: "bravo", Status: "running"},
	}})
	m = step.(model)
	out := m.renderSessions(40, 20)
	if !strings.Contains(out, "task") || !strings.Contains(out, "project") {
		t.Errorf("rendered pane missing one or both rows; got:\n%s", out)
	}
	if !strings.Contains(out, sessionMarkerSelected) {
		t.Errorf("rendered pane missing selected marker %q; got:\n%s", sessionMarkerSelected, out)
	}
}

func TestSessionViewZeroTime(t *testing.T) {
	// Sanity: SessionView's StartedAt is a value type, so a zero SessionView
	// has a zero StartedAt. This is the only guarantee callers rely on.
	var v SessionView
	if !v.StartedAt.Equal(time.Time{}) {
		t.Errorf("zero SessionView.StartedAt = %v, want zero time", v.StartedAt)
	}
}

func TestRenderSessionRowInteractiveBadge(t *testing.T) {
	auto := SessionView{ID: "a", AgentName: "task", Project: "alpha", Status: "running"}
	intr := SessionView{ID: "b", AgentName: "project", Project: "bravo", Status: "running", Interactive: true}
	cols := sessionColumnWidths([]SessionView{auto, intr}, time.Time{}, 80)

	autoOut := renderSessionRow(auto, cols, time.Time{}, false)
	intrOut := renderSessionRow(intr, cols, time.Time{}, false)

	if strings.Contains(autoOut, interactiveBadge) {
		t.Errorf("autonomous row should not show the interactive badge; got:\n%s", autoOut)
	}
	if !strings.Contains(intrOut, interactiveBadge) {
		t.Errorf("interactive row should include the badge %q; got:\n%s", interactiveBadge, intrOut)
	}
}

func TestRenderSessionsTableHeader(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	step, _ := m.Update(sessionsMsg{views: []SessionView{
		{ID: "a", AgentName: "task", Project: "alpha", Status: "running"},
	}})
	m = step.(model)
	out := m.renderSessions(80, 10)
	for _, header := range []string{"agent", "project", "task", "status", "age", "sandbox"} {
		if !strings.Contains(out, header) {
			t.Errorf("table header missing column %q; got:\n%s", header, out)
		}
	}
}

func TestRenderSessionRowShowsSandboxNameForACP(t *testing.T) {
	acp := SessionView{
		ID: "a", AgentName: "task", Project: "alpha", TaskName: "scaffold",
		Status: "running", SandboxName: "alpha-scaffold",
	}
	legacy := SessionView{
		ID: "b", AgentName: "wolf", Project: "bravo", Status: "running",
		Interactive: true,
	}
	cols := sessionColumnWidths([]SessionView{acp, legacy}, time.Time{}, 80)
	if got := renderSessionRow(acp, cols, time.Time{}, false); !strings.Contains(got, "alpha-scaffold") {
		t.Errorf("ACP row should expose sandbox name; got:\n%s", got)
	}
	if got := renderSessionRow(legacy, cols, time.Time{}, false); !strings.Contains(got, "—") {
		t.Errorf("non-ACP row should render an em-dash in the sandbox column; got:\n%s", got)
	}
}

func TestInteractiveStyleDiffersFromAutonomous(t *testing.T) {
	// Render-output comparison is unreliable in test environments because
	// lipgloss strips colour without a TTY. Verify the style values
	// directly: the interactive style's foreground must differ from the
	// dim status / selected styles.
	intr := sessionRowInteractiveStyle.GetForeground()
	sel := sessionRowSelectedStyle.GetForeground()
	dim := dimStyle.GetForeground()
	if intr == sel || intr == dim {
		t.Errorf("interactive foreground %v should differ from selected (%v) and dim (%v)",
			intr, sel, dim)
	}
}
