package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/project"
)

// TestRequestStopPausesProject pins the core contract: `:stop` records the
// project's current status in StoppedFrom, flips Status to stopped, and
// writes as `agent` so the daemon (not the TUI, which has no direct handle on
// it) sees the write and reacts by killing the project's running sessions.
func TestRequestStopPausesProject(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "widget")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".project.yaml")
	yaml := "description: p\nbranch: b\nstatus: working\nupdated_by: daemon\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	name, err := requestStop(path)
	if err != nil {
		t.Fatalf("requestStop: %v", err)
	}
	if name == "" {
		t.Errorf("requestStop returned empty display name")
	}

	got, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != project.StatusStopped {
		t.Errorf("status = %q, want stopped", got.Status)
	}
	if got.StoppedFrom != project.StatusWorking {
		t.Errorf("stopped_from = %q, want working", got.StoppedFrom)
	}
	if got.UpdatedBy != project.WriterAgent {
		t.Errorf("updated_by = %q, want agent", got.UpdatedBy)
	}
}

// TestRequestStopRefusals covers the two states `:stop` won't act on: a
// project that has never been populated (nothing to pause) and one that's
// already stopped (re-stopping would overwrite StoppedFrom with "stopped"
// itself, losing what `:start` should restore).
func TestRequestStopRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"unpopulated", "description: seed\n"},
		{"already stopped", "description: p\nbranch: b\nstatus: stopped\nstopped_from: working\nupdated_by: daemon\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ".project.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := requestStop(path); err == nil {
				t.Errorf("requestStop should have been refused")
			}
			// Must not have touched the file.
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.yaml {
				t.Errorf("file changed on a refused stop:\ngot:  %q\nwant: %q", data, tc.yaml)
			}
		})
	}
}

func TestRequestStopWithoutProjectErrors(t *testing.T) {
	if _, err := requestStop(""); err == nil {
		t.Errorf("requestStop(\"\") should error")
	}
}

// TestRequestStartResumesToStoppedFrom pins the resume contract: `:start`
// restores Status from StoppedFrom (whatever it was — ready, working,
// blocked, idle) and clears the field, handing the project back to the
// daemon's ordinary routing with no extra state left over.
func TestRequestStartResumesToStoppedFrom(t *testing.T) {
	for _, from := range []project.Status{
		project.StatusReady, project.StatusWorking, project.StatusBlocked,
		project.StatusIdle,
	} {
		t.Run(string(from), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ".project.yaml")
			yaml := "description: p\nbranch: b\nstatus: stopped\nstopped_from: " + string(from) + "\nupdated_by: daemon\n"
			if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
				t.Fatal(err)
			}

			name, err := requestStart(path)
			if err != nil {
				t.Fatalf("requestStart: %v", err)
			}
			if name == "" {
				t.Errorf("requestStart returned empty display name")
			}

			got, err := project.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != from {
				t.Errorf("status = %q, want %q restored", got.Status, from)
			}
			if got.StoppedFrom != "" {
				t.Errorf("stopped_from = %q, want cleared", got.StoppedFrom)
			}
			if got.UpdatedBy != project.WriterAgent {
				t.Errorf("updated_by = %q, want agent", got.UpdatedBy)
			}
		})
	}
}

// TestRequestStartFallsBackToDoneWithoutStoppedFrom covers a hand-edited file
// that reached status:stopped without ever recording StoppedFrom: restoring an
// empty status would read back as the "unpopulated" placeholder and misroute
// to the project agent, so `:start` falls back to done instead.
func TestRequestStartFallsBackToDoneWithoutStoppedFrom(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".project.yaml")
	yaml := "description: p\nbranch: b\nstatus: stopped\nupdated_by: daemon\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := requestStart(path); err != nil {
		t.Fatalf("requestStart: %v", err)
	}
	got, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != project.StatusIdle {
		t.Errorf("status = %q, want done fallback", got.Status)
	}
}

func TestRequestStartRefusesNonStopped(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".project.yaml")
	yaml := "description: p\nbranch: b\nstatus: working\nupdated_by: daemon\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := requestStart(path); err == nil {
		t.Errorf("requestStart on a working project should error")
	}
}

func TestRequestStartWithoutProjectErrors(t *testing.T) {
	if _, err := requestStart(""); err == nil {
		t.Errorf("requestStart(\"\") should error")
	}
}

// TestCommandPickerStopAndStart is the end-to-end proof through
// dispatchProjectCommand rather than requestStop/requestStart directly. It
// calls dispatchProjectCommand with the literal command key instead of going
// through runProjectCommand's first-letter shortcut: "stop"/"start" share
// "session"'s "s" (see projectCommands), so the shortcut path would run
// `session` instead — j/k + enter is how a real user reaches these two.
func TestCommandPickerStopAndStart(t *testing.T) {
	m, path := selectProject(t, t.TempDir(), "widget") // seeded status:done

	stepped, _ := m.dispatchProjectCommand("stop")
	m = stepped.(model)
	if !strings.Contains(m.statusMsg, "stopped") {
		t.Errorf("statusMsg = %q, want it to confirm the stop", m.statusMsg)
	}
	p, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != project.StatusStopped {
		t.Errorf("status = %q, want stopped", p.Status)
	}
	if p.StoppedFrom != project.StatusIdle {
		t.Errorf("stopped_from = %q, want done", p.StoppedFrom)
	}

	stepped, _ = m.dispatchProjectCommand("start")
	m = stepped.(model)
	if !strings.Contains(m.statusMsg, "resumed") {
		t.Errorf("statusMsg = %q, want it to confirm the resume", m.statusMsg)
	}
	p, err = project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != project.StatusIdle {
		t.Errorf("status = %q, want done restored", p.Status)
	}
}
