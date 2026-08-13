package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// typeChars feeds each rune of s into the model as if the user typed them.
// Each tea.KeyMsg carries a single rune in .Runes so the mode handlers'
// printable-char branch fires the way it would for a real keystroke.
func typeChars(t *testing.T, m model, s string) model {
	t.Helper()
	for _, r := range s {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	return m
}

func focusProjectsPane(t *testing.T, m model) model {
	t.Helper()
	// Cycle pane focus forward (⌥j) until the projects pane is active. The
	// default focus is sessions and the cycle now includes the audit pane, so
	// the number of steps varies — loop rather than assume a fixed count.
	for i := 0; i < 5; i++ {
		if m.focus == paneProjects {
			return m
		}
		step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}, Alt: true})
		m = step.(model)
	}
	if m.focus != paneProjects {
		t.Fatalf("expected projects focus after cycling, got %v", m.focus)
	}
	return m
}

// openCommandPicker presses `:` (the projects pane must already be focused)
// and asserts the command menu opened.
func openCommandPicker(t *testing.T, m model) model {
	t.Helper()
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	m = step.(model)
	if m.mode != modeCommandPicker {
		t.Fatalf("after `:`, mode = %v, want modeCommandPicker", m.mode)
	}
	return m
}

// runProjectCommand opens the `:` menu and runs the command with the given key
// via its first-letter shortcut, returning the resulting model and any command
// (e.g. an interactive launch) the dispatch produced.
func runProjectCommand(t *testing.T, m model, key string) (model, tea.Cmd) {
	t.Helper()
	m = openCommandPicker(t, m)
	step, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune(key[0])}})
	return step.(model), cmd
}

func TestColonOpensCommandPickerFromProjectsPane(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	m = step.(model)

	if m.mode != modeCommandPicker {
		t.Errorf("after `:`, mode = %v, want modeCommandPicker", m.mode)
	}
	if m.cmdPickerIdx != 0 {
		t.Errorf("cmdPickerIdx = %d, want 0 on entry", m.cmdPickerIdx)
	}
}

func TestColonIgnoredOutsideProjectsPane(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	// Sessions is the default focus.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	m = step.(model)
	if m.mode != modeNormal {
		t.Errorf("`:` outside projects pane should be a no-op; mode = %v", m.mode)
	}
}

func TestCommandPickerNewOpensModal(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m, _ = runProjectCommand(t, m, "new")

	if m.mode != modeNewProject {
		t.Errorf("after picking `new`, mode = %v, want modeNewProject", m.mode)
	}
}

func TestCommandPickerEnterRunsHighlighted(t *testing.T) {
	// The first row is `task`; with no work stream selected it should surface
	// the "no work stream" error rather than opening the task modal.
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = openCommandPicker(t, m)
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)
	if m.mode != modeNormal {
		t.Errorf("after enter on `task` with no selection, mode = %v, want modeNormal", m.mode)
	}
	if !strings.Contains(m.statusMsg, "no work stream") {
		t.Errorf("statusMsg = %q, want a no-work-stream message", m.statusMsg)
	}
}

func TestCommandPickerUnknownLetterIgnored(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = openCommandPicker(t, m)
	// `z` matches no command's first letter — the menu stays open.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	m = step.(model)
	if m.mode != modeCommandPicker {
		t.Errorf("after an unmatched letter, mode = %v, want modeCommandPicker", m.mode)
	}
}

func TestCommandPickerEscapeCancels(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = openCommandPicker(t, m)
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf("after esc, mode = %v, want modeNormal", m.mode)
	}
}

func TestNewProjectModalCreatesSeedYAML(t *testing.T) {
	root := t.TempDir()
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m = focusProjectsPane(t, m)
	m, _ = runProjectCommand(t, m, "new")
	// Name field first, then tab to the description field and type the seed.
	m = typeChars(t, m, "widget")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = step.(model)
	m = typeChars(t, m, "build a widget in acme/widgets")
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf("after creation, mode = %v, want modeNormal", m.mode)
	}
	if !strings.Contains(m.statusMsg, "widget") {
		t.Errorf("statusMsg = %q, want it to confirm creation", m.statusMsg)
	}

	path := filepath.Join(root, "widget", ".project.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected .project.yaml at %s, got err: %v", path, err)
	}
	// The seed carries the user's description (unpopulated otherwise) so the
	// daemon routes it to the project agent, which fills in the rest.
	if !strings.Contains(string(data), "build a widget in acme/widgets") {
		t.Errorf("seed .project.yaml missing description; got:\n%s", string(data))
	}
	if !strings.Contains(string(data), "updated_by: agent") {
		t.Errorf("seed should be written as updated_by: agent; got:\n%s", string(data))
	}
}

func TestNewProjectModalRejectsEmptyDescription(t *testing.T) {
	root := t.TempDir()
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m.mode = modeNewProject
	// A name but no description: submit must be rejected and the modal stays.
	m = typeChars(t, m, "widget")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNewProject {
		t.Errorf("missing description should keep modal open; mode = %v", m.mode)
	}
	if m.newProjErr == "" {
		t.Errorf("expected newProjErr to be set for empty description")
	}
}

func TestNewProjectModalRejectsEmptyName(t *testing.T) {
	root := t.TempDir()
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m.mode = modeNewProject

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNewProject {
		t.Errorf("empty enter should keep modal open; mode = %v", m.mode)
	}
	if m.newProjErr == "" {
		t.Errorf("expected newProjErr to be set for empty name")
	}
}

func TestNewProjectModalRefusesSlashes(t *testing.T) {
	root := t.TempDir()
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m.mode = modeNewProject

	// `/` is gated by isProjectNameChar so it never lands in the buffer in
	// the first place; check that here, then test the createProjectSeed
	// guard for callers that bypass the keystroke filter.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = step.(model)
	if m.newProjName != "" {
		t.Errorf("slash should not be accepted; newProjName = %q", m.newProjName)
	}

	if err := createProjectSeed(root, "bad/name", "desc"); err == nil {
		t.Errorf("createProjectSeed should refuse a name containing /")
	}
}

func TestNewProjectModalRefusesDuplicates(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dup", ".project.yaml"), []byte("description: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := createProjectSeed(root, "dup", "desc"); err == nil {
		t.Errorf("createProjectSeed should refuse to clobber an existing .project.yaml")
	}

	// And the existing file content must remain intact.
	data, err := os.ReadFile(filepath.Join(root, "dup", ".project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "description: x\n" {
		t.Errorf("existing file was modified: %q", string(data))
	}
}

func TestNewProjectModalEscapeCancels(t *testing.T) {
	root := t.TempDir()
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m.mode = modeNewProject
	m.newProjName = "halfway"

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf("esc should dismiss modal; mode = %v", m.mode)
	}
	// No file should have been created.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("esc-cancelled modal must not write to disk; root contains %d entries", len(entries))
	}
}

func TestProjectsFooterShowsColonMenuWithoutSelection(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = sized.(model)
	m = focusProjectsPane(t, m)
	// The footer advertises the `:` menu, not the individual commands.
	if !strings.Contains(m.View(), "  •  :  •  ") {
		t.Errorf("projects footer should advertise the `:` command menu; got:\n%s", m.View())
	}
}

func TestModalReplacesViewBody(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = sized.(model)
	m.mode = modeNewProject

	view := m.View()
	if !strings.Contains(view, "New work stream") {
		t.Errorf("modal view missing title; got:\n%s", view)
	}
	// The normal Sessions / Projects panes should NOT render when the
	// modal owns the screen.
	if strings.Contains(view, "Agent Sessions") {
		t.Errorf("modal view should not include the sessions pane; got:\n%s", view)
	}
}

func TestCtrlCExitsFromModal(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.mode = modeNewProject
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatalf("ctrl+c in modal must return a Quit command")
	}
}
