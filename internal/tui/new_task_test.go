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

// selectProject writes a populated .project.yaml under root/<name> and returns
// a model with that project selected, projects pane focused. The project
// starts in status:done so tests can assert `:task` flips it back to ready.
func selectProject(t *testing.T, root, name string) (model, string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".project.yaml")
	yaml := "description: a project\nbranch: feature/x\nstatus: done\nupdated_by: daemon\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m.projects = []ProjectView{{Name: name, Path: path, Status: project.StatusDone}}
	m.projSel = path
	m = focusProjectsPane(t, m)
	return m, path
}

func TestCommandLineTaskOpensModal(t *testing.T) {
	m, _ := selectProject(t, t.TempDir(), "widget")
	m = typeChars(t, m, ":task")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNewTask {
		t.Errorf("after :task<enter>, mode = %v, want modeNewTask", m.mode)
	}
}

func TestCommandLineTaskWithoutSelectionSurfacesError(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = typeChars(t, m, ":task")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf(":task with no project should stay normal; mode = %v", m.mode)
	}
	if !strings.Contains(m.statusMsg, "no project") {
		t.Errorf("statusMsg = %q, want it to explain no project is selected", m.statusMsg)
	}
}

func TestNewTaskModalSeedsTaskAndReplans(t *testing.T) {
	root := t.TempDir()
	m, projPath := selectProject(t, root, "widget")

	m = typeChars(t, m, ":task")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	m = typeChars(t, m, "Add a /healthz endpoint")
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf("after creation, mode = %v, want modeNormal", m.mode)
	}
	if !strings.Contains(m.statusMsg, "planning") {
		t.Errorf("statusMsg = %q, want it to confirm queueing for planning", m.statusMsg)
	}

	// A seed task file should exist with the description, no name, status ready.
	tasksDir := filepath.Join(root, "widget", "tasks")
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		t.Fatalf("reading tasks dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 seed task file, got %d", len(entries))
	}
	seed, err := task.Load(filepath.Join(tasksDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("loading seed: %v", err)
	}
	if seed.Name != "" {
		t.Errorf("seed name = %q, want empty (the planner fills it in)", seed.Name)
	}
	if seed.Description != "Add a /healthz endpoint" {
		t.Errorf("seed description = %q, want the typed text", seed.Description)
	}
	if seed.Status != task.StatusReady {
		t.Errorf("seed status = %q, want ready", seed.Status)
	}

	// The project must be flipped back to ready (so the daemon re-runs
	// planning) with updated_by: agent (so the daemon acts on the change).
	p, err := project.Load(projPath)
	if err != nil {
		t.Fatalf("loading project: %v", err)
	}
	if p.Status != project.StatusReady {
		t.Errorf("project status = %q, want ready", p.Status)
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("project updated_by = %q, want agent", p.UpdatedBy)
	}
}

func TestNewTaskModalRejectsEmptyDescription(t *testing.T) {
	root := t.TempDir()
	m, projPath := selectProject(t, root, "widget")
	m.mode = modeNewTask

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNewTask {
		t.Errorf("empty enter should keep modal open; mode = %v", m.mode)
	}
	if m.newTaskErr == "" {
		t.Errorf("expected newTaskErr for empty description")
	}
	// Nothing should have been written: no tasks dir, project untouched.
	if _, err := os.Stat(filepath.Join(root, "widget", "tasks")); !os.IsNotExist(err) {
		t.Errorf("empty description must not create a tasks dir")
	}
	p, _ := project.Load(projPath)
	if p.Status != project.StatusDone {
		t.Errorf("project status = %q, want it untouched (done)", p.Status)
	}
}

func TestNewTaskModalEscapeCancels(t *testing.T) {
	root := t.TempDir()
	m, _ := selectProject(t, root, "widget")
	m.mode = modeNewTask
	m.newTaskDesc = "halfway typed"

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf("esc should dismiss modal; mode = %v", m.mode)
	}
	if _, err := os.Stat(filepath.Join(root, "widget", "tasks")); !os.IsNotExist(err) {
		t.Errorf("esc-cancelled modal must not write to disk")
	}
}

func TestNewTaskModalAcceptsSpacesAndPunctuation(t *testing.T) {
	root := t.TempDir()
	m, _ := selectProject(t, root, "widget")
	m.mode = modeNewTask

	m = typeChars(t, m, "wire up the CI: build & test!")
	if m.newTaskDesc != "wire up the CI: build & test!" {
		t.Errorf("description = %q, want spaces and punctuation preserved", m.newTaskDesc)
	}
}

func TestNewTaskModalAcceptsPastedText(t *testing.T) {
	root := t.TempDir()
	m, _ := selectProject(t, root, "widget")
	m.mode = modeNewTask

	// A paste arrives as one KeyMsg carrying every pasted rune (bracketed
	// paste sets Paste:true); the modal must append the whole thing rather
	// than dropping it as it once did with a length==1 guard.
	pasted := "Add a /healthz endpoint that returns 200"
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})
	m = step.(model)

	if m.newTaskDesc != pasted {
		t.Errorf("description = %q, want the full pasted text %q", m.newTaskDesc, pasted)
	}
}

func TestNewTaskModalCollapsesPastedNewlines(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.mode = modeNewTask

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line one\nline\ttwo\r\n"), Paste: true})
	m = step.(model)

	if m.newTaskDesc != "line one line two " { // trailing \r\n -> one space
		t.Errorf("description = %q, want newlines and tabs collapsed to spaces", m.newTaskDesc)
	}
}

func TestNewTaskModalBackspaceTrimsDescription(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.mode = modeNewTask
	m = typeChars(t, m, "abc")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = step.(model)
	if m.newTaskDesc != "ab" {
		t.Errorf("after backspace, newTaskDesc = %q, want %q", m.newTaskDesc, "ab")
	}
}

func TestSecondSeedDoesNotClobberFirst(t *testing.T) {
	root := t.TempDir()
	m, _ := selectProject(t, root, "widget")

	queue := func(desc string) model {
		mm := typeChars(t, m, ":task")
		step, _ := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
		mm = step.(model)
		mm = typeChars(t, mm, desc)
		step, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
		return step.(model)
	}
	// Two descriptions that slug to the same stem.
	_ = queue("deploy the service")
	_ = queue("deploy the service")

	entries, err := os.ReadDir(filepath.Join(root, "widget", "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 distinct seed files, got %d: %v", len(entries), entries)
	}
}

func TestTaskSlug(t *testing.T) {
	cases := map[string]string{
		"Add a /healthz endpoint": "add-a-healthz-endpoint",
		"   trim me   ":           "trim-me",
		"!!!":                     "task",
		"":                        "task",
		"UPPER and CamelCase":     "upper-and-camelcase",
	}
	for in, want := range cases {
		if got := taskSlug(in); got != want {
			t.Errorf("taskSlug(%q) = %q, want %q", in, got, want)
		}
	}
	// Long descriptions are capped.
	long := taskSlug(strings.Repeat("word ", 40))
	if len(long) > 40 {
		t.Errorf("taskSlug did not cap length: got %d chars", len(long))
	}
}

func TestProjectsFooterHintIncludesTaskWhenSelected(t *testing.T) {
	root := t.TempDir()
	m, _ := selectProject(t, root, "widget")
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	m = sized.(model)
	if !strings.Contains(m.View(), ":task") {
		t.Errorf("projects footer should advertise :task when a project is selected; got:\n%s", m.View())
	}
}
