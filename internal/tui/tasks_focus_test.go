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
	if target == paneProjects || target == paneTasks {
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

// TestAltJKOnlyCyclesCenterColumnPanes covers the reworked alt-j/alt-k scope:
// now that Tasks lives in the center column alongside Projects, alt-j/alt-k
// only toggles focus between those two — it no longer reaches Sessions, the
// YAML viewer, or Audit (alt-h/alt-l own the side columns; Audit is
// click-only — see handleMouse).
func TestAltJKOnlyCyclesCenterColumnPanes(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	m = focusPane(t, m, paneProjects)

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}, Alt: true})
	m = step.(model)
	if m.focus != paneTasks {
		t.Fatalf("alt-j from projects: focus = %v, want paneTasks", m.focus)
	}

	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}, Alt: true})
	m = step.(model)
	if m.focus != paneProjects {
		t.Fatalf("alt-k from tasks: focus = %v, want paneProjects", m.focus)
	}

	// A no-op everywhere else.
	for _, p := range []pane{paneSessions, paneProjectYAML, paneAudit} {
		m = focusPane(t, m, p)
		step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}, Alt: true})
		m = step.(model)
		if m.focus != p {
			t.Errorf("alt-j from %v: focus = %v, want it to stay put", p, m.focus)
		}
	}
}

// TestAltHAltLSwitchColumns covers the new column-switch keys: alt-h toggles
// focus between the center column and the sessions column (left), alt-l
// between the center column and the YAML viewer column (right). Both are a
// no-op when the target column is toggled off, and returning to center
// restores whichever of Projects/Tasks was last focused there.
func TestAltHAltLSwitchColumns(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	m = focusPane(t, m, paneTasks)

	altH := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}, Alt: true}
	altL := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}, Alt: true}

	step, _ := m.Update(altH)
	m = step.(model)
	if m.focus != paneSessions {
		t.Fatalf("alt-h from tasks: focus = %v, want paneSessions", m.focus)
	}
	step, _ = m.Update(altH)
	m = step.(model)
	if m.focus != paneTasks {
		t.Fatalf("alt-h back from sessions: focus = %v, want paneTasks (lastCenterFocus)", m.focus)
	}

	step, _ = m.Update(altL)
	m = step.(model)
	if m.focus != paneProjectYAML {
		t.Fatalf("alt-l from tasks: focus = %v, want paneProjectYAML", m.focus)
	}
	step, _ = m.Update(altL)
	m = step.(model)
	if m.focus != paneTasks {
		t.Fatalf("alt-l back from yaml: focus = %v, want paneTasks (lastCenterFocus)", m.focus)
	}

	// Toggled-off side columns make alt-h/alt-l a no-op.
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

func TestYAMLViewerShowsTaskFileAfterPressingT(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Default is project YAML; pressing t starts a possible "tl"/"tr" chord,
	// which resolves to the plain YAML-source flip once chordTimeoutDelay
	// elapses without a completing "l"/"r" (simulated here via resolveChord
	// instead of a real sleep).
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)
	m = resolveChord(m)

	view := m.View()
	// Default selection is the first task alphabetically — "alpha". The task
	// file content (not a pane title) is what proves the source flipped.
	if !strings.Contains(view, "name: alpha") {
		t.Errorf("expected YAML pane to show selected task's content (name: alpha); got:\n%s", view)
	}
}

func TestYAMLViewerSwapsTaskFilesOnSelectionChange(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Switch to task source first, then change the selection.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)
	m = focusPane(t, m, paneTasks)
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = step.(model)

	view := m.View()
	if !strings.Contains(view, "name: beta") {
		t.Errorf("after selecting second task, YAML pane should show 'name: beta'; got:\n%s", view)
	}
}

func TestYAMLViewerStaysOnTaskWhenFocusMovesAway(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Flip to task source; the viewer now shows task YAML (task content, not a
	// pane title, is the tell now that titles are gone). "t" alone resolves
	// to the flip once the chord timeout elapses (see resolveChord).
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)
	m = resolveChord(m)
	if !strings.Contains(m.View(), "name: alpha") {
		t.Fatalf("expected task YAML content after pressing t")
	}

	// Cycle pane focus with down. The viewer should keep showing task YAML
	// regardless of which pane is focused — p/t are the only switches now.
	for i := 0; i < 4; i++ {
		step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}, Alt: true})
		m = step.(model)
		if !strings.Contains(m.View(), "name: alpha") {
			t.Errorf("task YAML content lost after down #%d (focus=%v)", i+1, m.focus)
		}
	}

	// Pressing p flips back to project YAML (project fields reappear).
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = step.(model)
	if !strings.Contains(m.View(), "branch:") {
		t.Errorf("expected project YAML content after pressing p")
	}
}

func TestYamlScrollResetsOnTaskSelectionChange(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Switch to task source so scroll position belongs to the task file.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)
	m = focusPane(t, m, paneTasks)
	m.yamlScroll = 7

	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = step.(model)

	if m.yamlScroll != 0 {
		t.Errorf("yamlScroll = %d, want 0 after switching task selection", m.yamlScroll)
	}
}

func TestYamlScrollResetsOnPTToggle(t *testing.T) {
	m, _, _ := withTaskFixtures(t)
	// Scroll into the project file, then flip to task source. "t" alone
	// resolves to the flip once the chord timeout elapses (see resolveChord).
	m.yamlScroll = 5
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = step.(model)
	m = resolveChord(m)
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
