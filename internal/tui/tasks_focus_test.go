package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

// withTaskFixtures returns a model wired up with one project containing two
// real task YAML files on disk, sized big enough to render every pane. The
// projects pane is the default-focused one because most callers want to
// then tab forward to reach the Tasks pane.
//
// Tasks are named "alpha" and "beta" so the alphabetical order taskgraph
// uses to list them matches the natural-language order of the variables —
// avoiding the trap of "first task" meaning two different things.
func withTaskFixtures(t *testing.T) (m model, taskAPath, taskBPath string) {
	t.Helper()
	root := t.TempDir()
	pDir := filepath.Join(root, "proj")
	if err := os.MkdirAll(filepath.Join(pDir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(pDir, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "proj description",
		Branch:      "feat/x",
		Status:      project.StatusWorking,
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
	taskAPath = filepath.Join(pDir, "tasks", "00-alpha.yaml")
	if err := task.Save(taskAPath, &task.Task{
		Name:   "alpha",
		Status: task.StatusReady,
	}); err != nil {
		t.Fatal(err)
	}
	taskBPath = filepath.Join(pDir, "tasks", "01-beta.yaml")
	if err := task.Save(taskBPath, &task.Task{
		Name:      "beta",
		Status:    task.StatusReady,
		DependsOn: []string{"alpha"},
	}); err != nil {
		t.Fatal(err)
	}

	m = newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	m = sized.(model)
	views, err := ScanProjects([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	step, _ := m.Update(projectsMsg{views: views})
	m = step.(model)
	return m, taskAPath, taskBPath
}

// focusPane focuses the given pane directly. Pane focus is a pure model
// field with no side effects of its own, so tests that only need a
// particular pane focused as setup can skip simulating the alt-h/alt-j/
// alt-k/alt-l navigation keys entirely.
func focusPane(t *testing.T, m model, target pane) model {
	t.Helper()
	m.focus = target
	if target == paneProjects || target == paneTasks || target == paneAudit {
		m.lastCenterFocus = target
	}
	return m
}

func TestTaskViewCarriesPathFromDisk(t *testing.T) {
	m, taskAPath, _ := withTaskFixtures(t)
	if len(m.projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(m.projects))
	}
	tasks := m.projects[0].Tasks
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	if tasks[0].Path != taskAPath {
		t.Errorf("Tasks[0].Path = %q, want %q", tasks[0].Path, taskAPath)
	}
}

// TestAltJKCyclesCenterColumnPanesIncludingAudit covers the reworked alt-j/
// alt-k scope: now that Audit stacks in the center column too, alt-j/alt-k
// cycles through all three center-column panes — Projects, Tasks, Audit —
// wrapping at either end, restoring the keyboard path onto Audit. It still
// doesn't reach the side columns (Sessions, the detail dashboard) — alt-h/
// alt-l own those.
func TestAltJKCyclesCenterColumnPanesIncludingAudit(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	m = focusPane(t, m, paneProjects)

	altJ := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}, Alt: true}
	altK := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}, Alt: true}

	step, _ := m.Update(altJ)
	m = step.(model)
	if m.focus != paneTasks {
		t.Fatalf("alt-j from projects: focus = %v, want paneTasks", m.focus)
	}

	step, _ = m.Update(altJ)
	m = step.(model)
	if m.focus != paneAudit {
		t.Fatalf("alt-j from tasks: focus = %v, want paneAudit", m.focus)
	}

	step, _ = m.Update(altJ)
	m = step.(model)
	if m.focus != paneProjects {
		t.Fatalf("alt-j from audit: focus = %v, want paneProjects (wraps)", m.focus)
	}

	// alt-k walks the same cycle in reverse.
	step, _ = m.Update(altK)
	m = step.(model)
	if m.focus != paneAudit {
		t.Fatalf("alt-k from projects: focus = %v, want paneAudit (wraps back)", m.focus)
	}
	step, _ = m.Update(altK)
	m = step.(model)
	if m.focus != paneTasks {
		t.Fatalf("alt-k from audit: focus = %v, want paneTasks", m.focus)
	}

	// A no-op on the side columns.
	for _, p := range []pane{paneSessions, paneProjectDetail} {
		m = focusPane(t, m, p)
		step, _ := m.Update(altJ)
		m = step.(model)
		if m.focus != p {
			t.Errorf("alt-j from %v: focus = %v, want it to stay put", p, m.focus)
		}
	}
}

// TestAltHAltLStepOneColumnAtATime covers the fixed column-switch keys: they
// must always move focus exactly one column over — alt-h to the left, alt-l
// to the right — passing through the center column rather than jumping
// straight between the two side columns, and stopping (never wrapping) once
// focus reaches whichever end column is visible. Returning to the center
// column restores whichever of Projects/Tasks/Audit was last focused there.
func TestAltHAltLStepOneColumnAtATime(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	m.rightVisible = true
	m = focusPane(t, m, paneTasks)

	altH := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}, Alt: true}
	altL := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}, Alt: true}

	// alt-h from center steps onto the left column.
	step, _ := m.Update(altH)
	m = step.(model)
	if m.focus != paneSessions {
		t.Fatalf("alt-h from tasks: focus = %v, want paneSessions", m.focus)
	}
	// Already at the left end: alt-h stays put instead of wrapping.
	step, _ = m.Update(altH)
	m = step.(model)
	if m.focus != paneSessions {
		t.Fatalf("alt-h at left end: focus = %v, want it to stay paneSessions (no wrap)", m.focus)
	}

	// alt-l steps back to center, remembering Tasks...
	step, _ = m.Update(altL)
	m = step.(model)
	if m.focus != paneTasks {
		t.Fatalf("alt-l from sessions: focus = %v, want paneTasks (lastCenterFocus)", m.focus)
	}
	// ...and a second alt-l is needed to reach the right column — proving the
	// old bug (jumping straight from one side column to the other) is fixed.
	step, _ = m.Update(altL)
	m = step.(model)
	if m.focus != paneProjectDetail {
		t.Fatalf("alt-l from tasks: focus = %v, want paneProjectDetail", m.focus)
	}
	// Already at the right end: alt-l stays put instead of wrapping.
	step, _ = m.Update(altL)
	m = step.(model)
	if m.focus != paneProjectDetail {
		t.Fatalf("alt-l at right end: focus = %v, want it to stay paneProjectDetail (no wrap)", m.focus)
	}

	// alt-h steps back to center, remembering Tasks.
	step, _ = m.Update(altH)
	m = step.(model)
	if m.focus != paneTasks {
		t.Fatalf("alt-h from detail pane: focus = %v, want paneTasks (lastCenterFocus)", m.focus)
	}

	// Toggled-off side columns make the step towards them a no-op from center.
	m.leftVisible = false
	m.rightVisible = false
	step, _ = m.Update(altH)
	m = step.(model)
	if m.focus != paneTasks {
		t.Errorf("alt-h with left column hidden: focus = %v, want it to stay paneTasks", m.focus)
	}
	step, _ = m.Update(altL)
	m = step.(model)
	if m.focus != paneTasks {
		t.Errorf("alt-l with right column hidden: focus = %v, want it to stay paneTasks", m.focus)
	}
}

// TestAltHFromRightColumnStepsToCenterNotLeft is a regression test for the
// reported bug: alt-h from the right (detail) column used to jump straight
// to the left (sessions) column because the old handler only checked
// whether focus was already on the sessions pane. One alt-h press must land
// on center; a second is required to reach the left column.
func TestAltHFromRightColumnStepsToCenterNotLeft(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	m.rightVisible = true
	m = focusPane(t, m, paneProjects)
	m = focusPane(t, m, paneProjectDetail) // arrive on the right column with Projects as lastCenterFocus

	altH := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}, Alt: true}
	step, _ := m.Update(altH)
	m = step.(model)
	if m.focus != paneProjects {
		t.Fatalf("alt-h from detail pane: focus = %v, want paneProjects (center) rather than skipping straight to sessions", m.focus)
	}
	step, _ = m.Update(altH)
	m = step.(model)
	if m.focus != paneSessions {
		t.Fatalf("second alt-h: focus = %v, want paneSessions", m.focus)
	}
}

func TestTaskSelectionDefaultsToFirstTask(t *testing.T) {
	m, taskAPath, _ := withTaskFixtures(t)
	if m.taskSel != taskAPath {
		t.Errorf("taskSel after load = %q, want %q (first task)", m.taskSel, taskAPath)
	}
}

func TestRightArrowOnTasksPaneAdvancesSelection(t *testing.T) {
	m, taskAPath, taskBPath := withTaskFixtures(t)
	m = focusPane(t, m, paneTasks)

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = step.(model)
	if m.taskSel != taskBPath {
		t.Errorf("after right on tasks pane, taskSel = %q, want %q", m.taskSel, taskBPath)
	}

	// Left returns to the first task.
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = step.(model)
	if m.taskSel != taskAPath {
		t.Errorf("after left, taskSel = %q, want %q", m.taskSel, taskAPath)
	}
}

func TestArrowsLeaveProjectSelectionAloneWhenTasksFocused(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	projBefore := m.projSel
	m = focusPane(t, m, paneTasks)

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = step.(model)

	if m.projSel != projBefore {
		t.Errorf("right on Tasks pane changed projSel: was %q, now %q", projBefore, m.projSel)
	}
}

// TestDetailPaneShowsSelectedProjectsTaskGraph is the integration-level
// companion to project_detail_test.go's unit tests: with the right column
// shown, it renders the selected project's name and its task graph,
// end-to-end through Update/View rather than calling the render helpers
// directly.
func TestDetailPaneShowsSelectedProjectsTaskGraph(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	m.rightVisible = true

	view := m.View()
	for _, want := range []string{"proj", "alpha", "beta", "← alpha"} {
		if !strings.Contains(view, want) {
			t.Errorf("detail pane missing %q; got:\n%s", want, view)
		}
	}
}

func TestMouseClickOnTaskRowSelectsIt(t *testing.T) {
	m, taskAPath, taskBPath := withTaskFixtures(t)
	l := m.computeLayout()
	// Tasks is now stacked in the center column below Projects, starting at
	// y = l.projectsH. Inside its own band: row 0 top border, row 1 column
	// header, row 2 the first task, row 3 the second task.
	clickX := l.leftW + 5
	clickY := l.projectsH + 3
	step, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      clickX,
		Y:      clickY,
	})
	m = step.(model)
	if m.focus != paneTasks {
		t.Errorf("click in tasks region should focus paneTasks, got %v", m.focus)
	}
	if m.taskSel != taskBPath {
		t.Errorf("click on second task row should select it; taskSel = %q, want %q (alpha was %q)",
			m.taskSel, taskBPath, taskAPath)
	}
}
