package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/project"
)

func TestCommandPickerCleanupRequestsArchiveAgent(t *testing.T) {
	root := t.TempDir()
	m, projPath := selectProject(t, root, "widget")

	m, _ = runProjectCommand(t, m, "cleanup")

	if m.mode != modeNormal {
		t.Errorf("`cleanup` should not open a modal; mode = %v", m.mode)
	}
	if !strings.Contains(m.statusMsg, "cleanup") {
		t.Errorf("statusMsg = %q, want it to confirm the request", m.statusMsg)
	}

	p, err := project.Load(projPath)
	if err != nil {
		t.Fatalf("loading project: %v", err)
	}
	if !p.Cleanup {
		t.Errorf("cleanup flag = false, want true so the daemon dispatches the archive agent")
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("project updated_by = %q, want agent (the daemon ignores its own writes)", p.UpdatedBy)
	}
	// The request must not disturb the work stream's own state.
	if p.Status != project.StatusIdle {
		t.Errorf("project status = %q, want it left at done", p.Status)
	}
	if p.Archive {
		t.Errorf("archive = true, want it left to the agent to set")
	}
}

func TestCommandPickerCleanupWithoutSelectionSurfacesError(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m, _ = runProjectCommand(t, m, "cleanup")

	if !strings.Contains(m.statusMsg, "no work stream") {
		t.Errorf("statusMsg = %q, want it to explain no work stream is selected", m.statusMsg)
	}
}

// writeProjectFile drops a project.yaml with the given body under root/<name>
// and returns its path.
func writeProjectFile(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRequestCleanupSetsFlagAsAgent(t *testing.T) {
	path := writeProjectFile(t, t.TempDir(), "widget",
		"description: p\nbranch: b\nstatus: working\nupdated_by: daemon\n")

	name, err := requestCleanup(path)
	if err != nil {
		t.Fatalf("requestCleanup: %v", err)
	}
	if name != "widget" {
		t.Errorf("display name = %q, want widget", name)
	}

	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Cleanup {
		t.Errorf("cleanup = false, want true")
	}
	if p.UpdatedBy != project.WriterAgent {
		t.Errorf("updated_by = %q, want agent", p.UpdatedBy)
	}
}

func TestRequestCleanupOnArchivedProjectIsRefused(t *testing.T) {
	path := writeProjectFile(t, t.TempDir(), "widget",
		"description: p\nbranch: b\nstatus: idle\narchive: true\nupdated_by: agent\n")

	_, err := requestCleanup(path)
	if err == nil {
		t.Fatalf("requestCleanup on an already-cleaned-up project should error")
	}
	if !strings.Contains(err.Error(), "already cleaned up") {
		t.Errorf("err = %q, want it to say the work stream is already cleaned up", err)
	}

	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Cleanup {
		t.Errorf("cleanup = true, want the request skipped rather than re-running the agent")
	}
}

func TestRequestCleanupWithoutProjectErrors(t *testing.T) {
	if _, err := requestCleanup(""); err == nil {
		t.Errorf("requestCleanup(\"\") should error")
	}
}

func TestCommandPickerListsCleanupNextToArchive(t *testing.T) {
	idx := func(key string) int {
		for i, c := range projectCommands {
			if c.key == key {
				return i
			}
		}
		return -1
	}
	cleanup, archive := idx("cleanup"), idx("archive")
	if cleanup < 0 {
		t.Fatalf("projectCommands is missing a `cleanup` entry")
	}
	if archive < 0 {
		t.Fatalf("projectCommands is missing an `archive` entry")
	}
	if archive-cleanup != 1 {
		t.Errorf("cleanup at %d, archive at %d; the pair should be adjacent", cleanup, archive)
	}
	if projectCommands[cleanup].desc == "" {
		t.Errorf("cleanup entry has no description; the menu would show a bare label")
	}
}

// TestCommandPickerIgnoresFirstLetters guards that the picker has no letter
// shortcuts: typing a command's first letter must neither dispatch it nor
// close the menu. j and k are excluded — they're the navigation keys.
func TestCommandPickerIgnoresFirstLetters(t *testing.T) {
	for _, c := range projectCommands {
		r := rune(c.key[0])
		if r == 'j' || r == 'k' {
			continue
		}
		t.Run(c.key, func(t *testing.T) {
			fake := &fakeInteractive{target: "t"}
			m, path := selectProject(t, t.TempDir(), "widget")
			m.interactive = fake
			m = openCommandPicker(t, m)
			idx := m.cmdPickerIdx
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			step, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			m = step.(model)

			if m.mode != modeCommandPicker {
				t.Errorf("typing %q: mode = %v, want the picker to stay open", string(r), m.mode)
			}
			if m.cmdPickerIdx != idx {
				t.Errorf("typing %q moved the highlight %d -> %d", string(r), idx, m.cmdPickerIdx)
			}
			if cmd != nil {
				t.Errorf("typing %q returned a command; want no-op", string(r))
			}
			if m.statusMsg != "" {
				t.Errorf("typing %q set statusMsg = %q; want no-op", string(r), m.statusMsg)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("typing %q modified project.yaml", string(r))
			}
			if len(fake.sessionCalls)+len(fake.orchCalls)+len(fake.wsCalls) != 0 {
				t.Errorf("typing %q launched an interactive window", string(r))
			}
		})
	}
}

// TestEveryProjectCommandDispatches makes sure every menu entry is handled by
// dispatchProjectCommand — an entry with no case falls through to "unknown
// command", which the menu gives no hint of.
func TestEveryProjectCommandDispatches(t *testing.T) {
	root := t.TempDir()
	for _, c := range projectCommands {
		t.Run(c.key, func(t *testing.T) {
			m, _ := selectProject(t, root, "widget-"+c.key)
			next, _ := m.dispatchProjectCommand(c.key)
			if got := next.(model).statusMsg; strings.Contains(got, "unknown command") {
				t.Errorf("dispatch(%q) = %q, want the command handled", c.key, got)
			}
		})
	}
}
