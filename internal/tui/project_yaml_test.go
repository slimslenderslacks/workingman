package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/project"
)

func TestProjectYAMLBodyReturnsRawFileContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".project.yaml")
	if err := project.SaveAs(path, &project.Project{
		Description: "alpha description",
		Branch:      "feat/alpha",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "acme", Name: "alpha"}},
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}

	body, isErr := projectYAMLBody(path)
	if isErr {
		t.Fatalf("projectYAMLBody returned error body: %s", body)
	}
	for _, want := range []string{
		"description: alpha description",
		"branch: feat/alpha",
		"status: working",
		"org: acme",
		"name: alpha",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("YAML body missing %q; got:\n%s", want, body)
		}
	}
}

func TestProjectYAMLBodyEmptySelection(t *testing.T) {
	body, isErr := projectYAMLBody("")
	if isErr {
		t.Errorf("empty selection should not be an error; got %q", body)
	}
	if !strings.Contains(body, "(none)") {
		t.Errorf("empty selection body = %q, want it to contain '(none)'", body)
	}
}

func TestProjectYAMLBodyMissingFile(t *testing.T) {
	body, isErr := projectYAMLBody("/nonexistent/.project.yaml")
	if !isErr {
		t.Errorf("missing file should be an error; got %q", body)
	}
}

func TestProjectYAMLPaneRendersSelectedProject(t *testing.T) {
	dir := t.TempDir()
	pDir := filepath.Join(dir, "alpha")
	if err := os.MkdirAll(pDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pDir, ".project.yaml")
	if err := project.SaveAs(path, &project.Project{
		Description: "alpha",
		Branch:      "main",
		Status:      project.StatusReady,
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}

	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "alpha", Path: path, Status: project.StatusReady},
	}})
	m = step.(model)

	view := m.View()
	if !strings.Contains(view, "Project YAML") {
		t.Errorf("view missing YAML pane title:\n%s", view)
	}
	if !strings.Contains(view, "branch:") {
		t.Errorf("view missing YAML branch field:\n%s", view)
	}
}

func TestProjectYAMLScrollAdvancesOnRight(t *testing.T) {
	m := model{focus: paneProjectYAML}
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = step.(model)
	if m.yamlScroll != 1 {
		t.Errorf("right arrow on yaml pane: scroll = %d, want 1", m.yamlScroll)
	}
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = step.(model)
	if m.yamlScroll != 0 {
		t.Errorf("left arrow on yaml pane: scroll = %d, want 0", m.yamlScroll)
	}
	// Left at zero must clamp to zero (no underflow).
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = step.(model)
	if m.yamlScroll != 0 {
		t.Errorf("left arrow at zero scroll: scroll = %d, want 0", m.yamlScroll)
	}
}

func TestCtrlFCtrlBPageYAMLRegardlessOfFocus(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	// Use a window tall enough that the YAML pane gets a meaningful slice.
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "alpha", Path: "/x/alpha/.project.yaml", Status: project.StatusReady},
	}})
	m = step.(model)

	// Force focus to projects so we prove the binding is independent of
	// pane focus.
	m.focus = paneProjects
	page := yamlPageSize(m)
	if page <= 0 {
		t.Fatalf("yamlPageSize = %d; need a positive page size for the test (height too small?)", page)
	}

	fwd, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	m = fwd.(model)
	if m.yamlScroll != page {
		t.Errorf("ctrl+f from focus=projects: scroll = %d, want %d", m.yamlScroll, page)
	}
	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	m = back.(model)
	if m.yamlScroll != 0 {
		t.Errorf("ctrl+b back to top: scroll = %d, want 0", m.yamlScroll)
	}
	// Ctrl+b at zero must not underflow.
	back2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	m = back2.(model)
	if m.yamlScroll != 0 {
		t.Errorf("ctrl+b at zero: scroll = %d, want 0", m.yamlScroll)
	}
}

func TestProjectYAMLScrollResetsWhenSelectionChanges(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "alpha", Path: "/x/alpha/.project.yaml", Status: project.StatusReady},
		{Name: "bravo", Path: "/x/bravo/.project.yaml", Status: project.StatusReady},
	}})
	m = step.(model)
	m.yamlScroll = 5

	// Down once to land on projects, then right-arrow to advance selection.
	focused, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = focused.(model)
	right, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = right.(model)

	if m.projSel != "/x/bravo/.project.yaml" {
		t.Fatalf("projSel = %q, want bravo", m.projSel)
	}
	if m.yamlScroll != 0 {
		t.Errorf("yamlScroll = %d, want 0 after project selection change", m.yamlScroll)
	}
}

func TestWrapDisplayWidthSplitsLongLinesAndPreservesBlanks(t *testing.T) {
	in := "short\n\nverylongline-of-text-that-must-wrap-cleanly"
	got := wrapDisplayWidth(in, 10)

	// First entry is "short" (≤ 10 cols, no wrap).
	if got[0] != "short" {
		t.Errorf("wrap[0] = %q, want %q", got[0], "short")
	}
	// Blank source line is preserved as a single empty entry.
	if got[1] != "" {
		t.Errorf("wrap[1] = %q, want empty (blank line preserved)", got[1])
	}
	// Remaining entries are wrapped fragments, each ≤ 10 cols.
	if len(got) < 4 {
		t.Fatalf("expected long line to wrap into >= 2 fragments, got %d total entries: %v", len(got), got)
	}
	for i, line := range got[2:] {
		if len(line) > 10 {
			t.Errorf("wrap[%d] = %q exceeds 10 cols", i+2, line)
		}
	}
	// Reassembled (skipping the blank), wrapped fragments must match the
	// original long line so no characters were dropped.
	if joined := strings.Join(got[2:], ""); joined != "verylongline-of-text-that-must-wrap-cleanly" {
		t.Errorf("wrapped fragments do not reassemble: %q", joined)
	}
}
