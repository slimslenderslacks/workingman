package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/project"
)

func TestCommandLineWolfBlocksProjectAndSummons(t *testing.T) {
	root := t.TempDir()
	m, projPath := selectProject(t, root, "widget")

	m = typeChars(t, m, ":wolf")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if m.mode != modeNormal {
		t.Errorf(":wolf should not open a modal; mode = %v", m.mode)
	}
	if !strings.Contains(m.statusMsg, "wolf") {
		t.Errorf("statusMsg = %q, want it to confirm the summon", m.statusMsg)
	}

	p, err := project.Load(projPath)
	if err != nil {
		t.Fatalf("loading project: %v", err)
	}
	if p.Status != project.StatusBlocked {
		t.Errorf("project status = %q, want blocked", p.Status)
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("project updated_by = %q, want agent", p.UpdatedBy)
	}
	if p.BlockedReason == "" {
		t.Errorf("blocked_reason should be set when summoning a non-blocked project")
	}
}

func TestCommandLineWolfWithoutSelectionSurfacesError(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = typeChars(t, m, ":wolf")
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = step.(model)

	if !strings.Contains(m.statusMsg, "no project") {
		t.Errorf("statusMsg = %q, want it to explain no project is selected", m.statusMsg)
	}
}

func TestSummonWolfPreservesExistingReason(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "widget")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".project.yaml")
	yaml := "description: p\nbranch: b\nstatus: blocked\nblocked_reason: task foo crashed\nupdated_by: daemon\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := summonWolf(path); err != nil {
		t.Fatalf("summonWolf: %v", err)
	}

	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.BlockedReason != "task foo crashed" {
		t.Errorf("blocked_reason = %q, want the daemon's original reason preserved", p.BlockedReason)
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("updated_by = %q, want agent (so the daemon re-launches wolf)", p.UpdatedBy)
	}
}

func TestSummonWolfWithoutProjectErrors(t *testing.T) {
	if _, err := summonWolf(""); err == nil {
		t.Errorf("summonWolf(\"\") should error")
	}
}

func TestProjectsFooterHintIncludesWolfWhenSelected(t *testing.T) {
	root := t.TempDir()
	m, _ := selectProject(t, root, "widget")
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 220, Height: 40})
	m = sized.(model)
	if !strings.Contains(m.View(), ":wolf") {
		t.Errorf("projects footer should advertise :wolf when a project is selected; got:\n%s", m.View())
	}
}
