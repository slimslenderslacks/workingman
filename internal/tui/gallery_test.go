package tui

import (
	"strings"
	"testing"

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
	out := renderProjectCard(v, 32)
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
	out := renderProjectCard(v, 32)
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

	wide := renderProjectGrid(views, 200)
	narrow := renderProjectGrid(views, 30)

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
