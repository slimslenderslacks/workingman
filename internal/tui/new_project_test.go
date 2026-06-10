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
	// Tab once to land on projects (sessions → projects → yaml cycle).
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = step.(model)
	if m.focus != paneProjects {
		t.Fatalf("expected projects focus after one tab, got %v", m.focus)
	}
	return m
}

func TestColonOpensCommandLineFromProjectsPane(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	m = step.(model)

	if m.mode != modeCommandLine {
		t.Errorf("after `:`, mode = %v, want modeCommandLine", m.mode)
	}
	if m.cmdInput != "" {
		t.Errorf("cmdInput = %q, want empty on entry", m.cmdInput)
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

func TestCommandLineNewOpensModal(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = typeChars(t, m, ":new")

	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNewProject {
		t.Errorf("after :new<enter>, mode = %v, want modeNewProject", m.mode)
	}
	if m.cmdInput != "" {
		t.Errorf("cmdInput = %q, want empty after command executed", m.cmdInput)
	}
}

func TestCommandLineUnknownCommandSurfacesError(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = typeChars(t, m, ":wat")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf("after unknown command, mode = %v, want modeNormal", m.mode)
	}
	if !strings.Contains(m.statusMsg, "wat") {
		t.Errorf("statusMsg = %q, want it to mention the bad command", m.statusMsg)
	}
}

func TestCommandLineEscapeCancels(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = typeChars(t, m, ":new")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf("after esc, mode = %v, want modeNormal", m.mode)
	}
	if m.cmdInput != "" {
		t.Errorf("cmdInput = %q, want cleared on cancel", m.cmdInput)
	}
}

func TestCommandLineBackspaceTrimsInput(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = typeChars(t, m, ":new")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = step.(model)
	if m.cmdInput != "ne" {
		t.Errorf("after backspace, cmdInput = %q, want %q", m.cmdInput, "ne")
	}
}

func TestNewProjectModalCreatesEmptyYAML(t *testing.T) {
	root := t.TempDir()
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m = focusProjectsPane(t, m)
	m = typeChars(t, m, ":new")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)
	m = typeChars(t, m, "widget")
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf("after creation, mode = %v, want modeNormal", m.mode)
	}
	if !strings.Contains(m.statusMsg, "widget") {
		t.Errorf("statusMsg = %q, want it to confirm creation", m.statusMsg)
	}

	path := filepath.Join(root, "widget", ".project.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected .project.yaml at %s, got err: %v", path, err)
	}
	if info.Size() != 0 {
		t.Errorf("expected empty .project.yaml, got %d bytes", info.Size())
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
	// the first place; check that here, then test the createEmptyProject
	// guard for callers that bypass the keystroke filter.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = step.(model)
	if m.newProjName != "" {
		t.Errorf("slash should not be accepted; newProjName = %q", m.newProjName)
	}

	if err := createEmptyProject(root, "bad/name"); err == nil {
		t.Errorf("createEmptyProject should refuse a name containing /")
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

	if err := createEmptyProject(root, "dup"); err == nil {
		t.Errorf("createEmptyProject should refuse to clobber an existing .project.yaml")
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

func TestProjectsFooterHintIncludesNew(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = sized.(model)
	m = focusProjectsPane(t, m)
	if !strings.Contains(m.View(), ":new") {
		t.Errorf("projects footer should advertise :new; got:\n%s", m.View())
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
