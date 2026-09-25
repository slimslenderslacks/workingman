package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/project"
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

// focusProjectsPane focuses the projects pane directly. Pane focus is a pure
// model field with no side effects of its own (⌥j/⌥k/⌥h/⌥l just reassign
// it), so tests that only need "projects is focused" as setup can skip
// simulating the navigation keys entirely.
func focusProjectsPane(t *testing.T, m model) model {
	t.Helper()
	m.focus = paneProjects
	m.lastCenterFocus = paneProjects
	return m
}

// openCommandPicker presses `:` and asserts the command menu opened. `:`
// opens the picker regardless of which pane is focused (see
// TestColonOpensCommandPickerFromAnyPane), so callers don't need to focus
// projects first — most still do simply because that's the pane most
// project-command tests care about afterward.
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

// selectProject writes a minimal .project.yaml under root/<name> and returns
// a model with that project selected, projects pane focused. The project
// starts in status:done so tests can assert a command flips it back to ready.
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
