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

func tabTo(t *testing.T, m model, target pane) model {
	t.Helper()
	for i := 0; i < 4; i++ {
		if m.focus == target {
			return m
		}
		step, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = step.(model)
	}
	t.Fatalf("could not tab to %v in 4 presses (current=%v)", target, m.focus)
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

func TestTabCycleReachesTasksPane(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Visual order top-to-bottom in the right column is projects → tasks →
	// yaml, so tab cycles: sessions → projects → tasks → yaml → sessions.
	want := []pane{paneProjects, paneTasks, paneProjectYAML, paneSessions}
	for i, expected := range want {
		step, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = step.(model)
		if m.focus != expected {
			t.Errorf("tab #%d focus = %v, want %v", i+1, m.focus, expected)
		}
	}
}

func TestTaskSelectionDefaultsToFirstTask(t *testing.T) {
	m, taskAPath, _ := withTaskFixtures(t)
	if m.taskSel != taskAPath {
		t.Errorf("taskSel after load = %q, want %q (first task)", m.taskSel, taskAPath)
	}
}

func TestDownArrowOnTasksPaneAdvancesSelection(t *testing.T) {
	m, taskAPath, taskBPath := withTaskFixtures(t)
	m = tabTo(t, m, paneTasks)

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = step.(model)
	if m.taskSel != taskBPath {
		t.Errorf("after down on tasks pane, taskSel = %q, want %q", m.taskSel, taskBPath)
	}

	// Up returns to the first task.
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = step.(model)
	if m.taskSel != taskAPath {
		t.Errorf("after up, taskSel = %q, want %q", m.taskSel, taskAPath)
	}
}

func TestArrowsLeaveProjectSelectionAloneWhenTasksFocused(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	projBefore := m.projSel
	m = tabTo(t, m, paneTasks)

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = step.(model)

	if m.projSel != projBefore {
		t.Errorf("down on Tasks pane changed projSel: was %q, now %q", projBefore, m.projSel)
	}
}

func TestYAMLViewerShowsTaskFileAfterPressingT(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Default is project YAML; pressing t flips the source to the task.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)

	view := m.View()
	if !strings.Contains(view, "Task YAML") {
		t.Errorf("expected pane title 'Task YAML' after pressing t; got:\n%s", view)
	}
	// Default selection is the first task alphabetically — "alpha".
	if !strings.Contains(view, "name: alpha") {
		t.Errorf("expected YAML pane to show selected task's content (name: alpha); got:\n%s", view)
	}
}

func TestYAMLViewerSwapsTaskFilesOnSelectionChange(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Switch to task source first, then change the selection.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)
	m = tabTo(t, m, paneTasks)
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = step.(model)

	view := m.View()
	if !strings.Contains(view, "name: beta") {
		t.Errorf("after selecting second task, YAML pane should show 'name: beta'; got:\n%s", view)
	}
}

func TestYAMLViewerStaysOnTaskWhenFocusMovesAway(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Flip to task source; the viewer now shows task YAML.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)
	if !strings.Contains(m.View(), "Task YAML") {
		t.Fatalf("expected Task YAML title after pressing t")
	}

	// Tab around. The viewer should keep showing task YAML regardless of
	// which pane is focused — p/t are the only switches now.
	for i := 0; i < 4; i++ {
		step, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = step.(model)
		if !strings.Contains(m.View(), "Task YAML") {
			t.Errorf("Task YAML title lost after tab #%d (focus=%v)", i+1, m.focus)
		}
	}

	// Pressing p flips back to project YAML.
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = step.(model)
	if !strings.Contains(m.View(), "Project YAML") {
		t.Errorf("expected Project YAML title after pressing p")
	}
}

func TestYamlScrollResetsOnTaskSelectionChange(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Switch to task source so scroll position belongs to the task file.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)
	m = tabTo(t, m, paneTasks)
	m.yamlScroll = 7

	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = step.(model)

	if m.yamlScroll != 0 {
		t.Errorf("yamlScroll = %d, want 0 after switching task selection", m.yamlScroll)
	}
}

func TestYamlScrollResetsOnPTToggle(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Scroll into the project file, then flip to task source.
	m.yamlScroll = 5
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)
	if m.yamlScroll != 0 {
		t.Errorf("yamlScroll = %d after t toggle, want 0 (different file)", m.yamlScroll)
	}

	// Scroll the task file, then flip back to project.
	m.yamlScroll = 4
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = step.(model)
	if m.yamlScroll != 0 {
		t.Errorf("yamlScroll = %d after p toggle, want 0 (different file)", m.yamlScroll)
	}
}

func TestPTReplaysSameSourceDoesNotResetScroll(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Already on project source by default. Pressing p again must not
	// blow away the user's scroll position.
	m.yamlScroll = 6
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = step.(model)
	if m.yamlScroll != 6 {
		t.Errorf("pressing p while already on project should preserve scroll; got %d, want 6", m.yamlScroll)
	}
}

func TestFooterAdvertisesPTToggle(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	view := m.View()
	if !strings.Contains(view, "p/t") {
		t.Errorf("footer should advertise the p/t YAML toggle; got:\n%s", view)
	}
}

func TestMouseClickOnTaskRowSelectsIt(t *testing.T) {
	m, taskAPath, taskBPath := withTaskFixtures(t)
	l := m.computeLayout()
	// Tasks pane sits directly below projects in the new bottom-yaml
	// layout: header(1) + projectsH rows, then the tasks pane.
	const headerRows = 1
	tasksStart := headerRows + l.projectsH
	// Inside the tasks pane: row 0 is the top border, row 1 the "Tasks"
	// title, row 2 a blank, row 3 the first task. So the second task is
	// at tasksStart + 4.
	clickY := tasksStart + 4
	step, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      l.sessionsW + 5,
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
