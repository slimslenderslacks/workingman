package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestArchiveProjectMovesTree(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "weather-tui")
	if err := os.MkdirAll(filepath.Join(projDir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	projPath := filepath.Join(projDir, ".project.yaml")
	if err := os.WriteFile(projPath, []byte("status: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "tasks", "a.yaml"), []byte("name: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := archiveProject(root, projPath); err != nil {
		t.Fatalf("archiveProject: %v", err)
	}

	// Original is gone from the workspace root.
	if _, err := os.Stat(projDir); !os.IsNotExist(err) {
		t.Errorf("original project dir still present (err=%v)", err)
	}
	// The whole tree landed under <root>.backup/<name>.
	dest := filepath.Join(root+".backup", "weather-tui")
	if _, err := os.Stat(filepath.Join(dest, ".project.yaml")); err != nil {
		t.Errorf("archived .project.yaml missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "tasks", "a.yaml")); err != nil {
		t.Errorf("archived task file missing: %v", err)
	}
}

func TestArchiveProjectRefusesExistingArchive(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "dup")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projPath := filepath.Join(projDir, ".project.yaml")
	if err := os.WriteFile(projPath, []byte("status: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A prior archive of the same name already exists.
	prior := filepath.Join(root+".backup", "dup")
	if err := os.MkdirAll(prior, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := archiveProject(root, projPath); err == nil {
		t.Errorf("expected archiveProject to refuse clobbering an existing backup")
	}
	// Original must be left intact when the archive is refused.
	if _, err := os.Stat(projPath); err != nil {
		t.Errorf("original should be intact after refused archive: %v", err)
	}
}

func TestArchiveProjectRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	// A project path that is not a direct child of root.
	outside := filepath.Join(t.TempDir(), "elsewhere", ".project.yaml")
	if err := archiveProject(root, outside); err == nil {
		t.Errorf("expected archiveProject to reject a path outside the root")
	}
}

func TestArchiveCommandOpensConfirmModal(t *testing.T) {
	root := t.TempDir()
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.projectRoot = root
	m.projSel = filepath.Join(root, "alpha", ".project.yaml")
	m = focusProjectsPane(t, m)

	m, _ = runProjectCommand(t, m, "archive")

	if m.mode != modeConfirmArchive {
		t.Fatalf("after picking `archive`, mode = %v, want modeConfirmArchive", m.mode)
	}
	if m.archiveTarget != m.projSel {
		t.Errorf("archiveTarget = %q, want %q", m.archiveTarget, m.projSel)
	}

	// n cancels without touching disk.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = step.(model)
	if m.mode != modeNormal {
		t.Errorf("after n, mode = %v, want modeNormal", m.mode)
	}
}
