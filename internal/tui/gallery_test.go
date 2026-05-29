package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

func TestRenderProjectCardIncludesNameStatusAndBreakdown(t *testing.T) {
	v := ProjectView{
		Name:   "alpha",
		Branch: "feat/alpha",
		Status: project.StatusWorking,
		TaskCounts: map[task.Status]int{
			task.StatusReady:     2,
			task.StatusRunning:   1,
			task.StatusBlocked:   0,
			task.StatusSuccess:   1,
			task.StatusCommitted: 3,
			task.StatusFailed:    1,
		},
	}
	out := renderProjectCard(v, 32, false)
	if !strings.Contains(out, "alpha") {
		t.Errorf("card missing project name; got:\n%s", out)
	}
	if !strings.Contains(out, "working") {
		t.Errorf("card missing status; got:\n%s", out)
	}
	// Done == success + committed = 4.
	if !strings.Contains(out, "R:2") ||
		!strings.Contains(out, "W:1") ||
		!strings.Contains(out, "B:0") ||
		!strings.Contains(out, "D:4") ||
		!strings.Contains(out, "F:1") {
		t.Errorf("breakdown line missing or wrong; got:\n%s", out)
	}
}

func TestRenderProjectCardHandlesNoTasks(t *testing.T) {
	v := ProjectView{Name: "bare", Status: project.StatusReady}
	out := renderProjectCard(v, 32, false)
	if !strings.Contains(out, "no tasks") {
		t.Errorf("empty-task card should say 'no tasks'; got:\n%s", out)
	}
}

func TestRenderProjectGridReflowsByWidth(t *testing.T) {
	views := []ProjectView{
		{Name: "alpha", Status: project.StatusReady},
		{Name: "bravo", Status: project.StatusWorking},
		{Name: "charlie", Status: project.StatusBlocked},
		{Name: "delta", Status: project.StatusDone},
	}

	// Pass a large row budget so the grid renders every project; this test
	// only exercises the width-based reflow.
	wide := renderProjectGrid(views, "", 200, 100)
	narrow := renderProjectGrid(views, "", 30, 100)

	wideRows := strings.Count(wide, "\n")
	narrowRows := strings.Count(narrow, "\n")
	if narrowRows <= wideRows {
		t.Errorf("narrow grid should produce more rows than wide grid; wide=%d narrow=%d", wideRows, narrowRows)
	}

	// Every project must appear in both layouts.
	for _, v := range views {
		if !strings.Contains(wide, v.Name) {
			t.Errorf("wide grid missing %q", v.Name)
		}
		if !strings.Contains(narrow, v.Name) {
			t.Errorf("narrow grid missing %q", v.Name)
		}
	}

	// Sanity: the rendered grid should fit within the requested width.
	for _, line := range strings.Split(wide, "\n") {
		if lipgloss.Width(line) > 200 {
			t.Errorf("wide line exceeds requested width: %d", lipgloss.Width(line))
		}
	}
}

func TestRenderTasksShowsNamesAndStatus(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{
			Name:   "alpha",
			Path:   "/x/alpha/.project.yaml",
			Status: project.StatusWorking,
			Tasks: []TaskView{
				{Name: "register-repo", Status: task.StatusCommitted},
				{Name: "survey", Status: task.StatusRunning},
				{Name: "implement", Status: task.StatusReady},
			},
		},
	}})
	m = step.(model)

	view := m.View()
	for _, want := range []string{"Tasks", "register-repo", "survey", "implement", "committed", "running", "ready"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

func TestRenderTasksSwapsOnProjectSelection(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{
			Name: "alpha", Path: "/x/alpha/.project.yaml",
			Status: project.StatusWorking,
			Tasks:  []TaskView{{Name: "alpha-only", Status: task.StatusReady}},
		},
		{
			Name: "bravo", Path: "/x/bravo/.project.yaml",
			Status: project.StatusWorking,
			Tasks:  []TaskView{{Name: "bravo-only", Status: task.StatusReady}},
		},
	}})
	m = step.(model)

	v1 := m.View()
	if !strings.Contains(v1, "alpha-only") || strings.Contains(v1, "bravo-only") {
		t.Errorf("initial view should show alpha-only and not bravo-only:\n%s", v1)
	}

	// Tab to projects pane, then down-arrow to select bravo.
	tabbed, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = tabbed.(model)
	down, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = down.(model)

	v2 := m.View()
	if !strings.Contains(v2, "bravo-only") || strings.Contains(v2, "alpha-only") {
		t.Errorf("after selecting bravo, view should show bravo-only and not alpha-only:\n%s", v2)
	}
}

func TestRenderTasksEmptyState(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "empty", Path: "/x/empty/.project.yaml", Status: project.StatusReady},
	}})
	m = step.(model)
	if !strings.Contains(m.View(), "Tasks") {
		t.Errorf("tasks pane title missing")
	}
	if !strings.Contains(m.View(), "(none)") {
		t.Errorf("empty tasks should show (none); got:\n%s", m.View())
	}
}

func TestProjectsPaneSpillsOverMultipleRows(t *testing.T) {
	// Eight projects at width=160 fits two cards per row (innerWidth ≈ 100,
	// cardTargetWidth=30 → perRow=3), so the gallery needs three rows to
	// show every card. With a generous height the layout must grow the
	// projects pane to fit them all instead of clipping to the historical
	// one-row default.
	views := make([]ProjectView, 0, 8)
	for _, name := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"} {
		views = append(views, ProjectView{
			Name:   name,
			Path:   "/x/" + name + "/.project.yaml",
			Status: project.StatusReady,
		})
	}

	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 80})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: views})
	m = step.(model)

	view := m.View()
	for _, v := range views {
		if !strings.Contains(view, v.Name) {
			t.Errorf("expected every project to render; missing %q in view", v.Name)
		}
	}

	// Sanity: the projects pane must have grown beyond the one-card-row
	// minimum so the spill-over is actually layout-driven and not just the
	// renderer overrunning its allocation.
	l := m.computeLayout()
	if l.projectsH <= projectsMinHeight {
		t.Errorf("projectsH = %d, want > projectsMinHeight(%d) to confirm spill-over",
			l.projectsH, projectsMinHeight)
	}
}

func TestDesiredProjectsHeightGrowsWithCount(t *testing.T) {
	one := desiredProjectsHeight(80, 1)
	many := desiredProjectsHeight(80, 8)
	if one != projectsMinHeight {
		t.Errorf("one project: desiredProjectsHeight = %d, want %d", one, projectsMinHeight)
	}
	if many <= one {
		t.Errorf("eight projects (%d rows) should need more height than one (%d)", many, one)
	}
	// Empty + zero-width edge cases must return the floor without dividing
	// by zero or producing nonsense values.
	if got := desiredProjectsHeight(80, 0); got != projectsMinHeight {
		t.Errorf("zero projects: desiredProjectsHeight = %d, want %d", got, projectsMinHeight)
	}
	if got := desiredProjectsHeight(0, 5); got != projectsMinHeight {
		t.Errorf("zero width: desiredProjectsHeight = %d, want %d", got, projectsMinHeight)
	}
}

func TestSelectedCardBorderUsesAccentColor(t *testing.T) {
	// lipgloss strips colours in test environments (no TTY), so we can't
	// compare rendered output. Verify the style values directly via the
	// per-side getter (BorderForeground sets all four sides identically).
	plain := cardBorder.GetBorderTopForeground()
	sel := cardSelectedBorder.GetBorderTopForeground()
	focused := focusedBorder.GetBorderTopForeground()
	if plain == sel {
		t.Errorf("cardSelectedBorder must use a different colour than cardBorder; both = %v", plain)
	}
	if sel != focused {
		t.Errorf("cardSelectedBorder should share the focus accent colour; got %v, want %v", sel, focused)
	}
}
