package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/terminal"
)

// fakeInteractive records the calls the model makes and returns canned results.
type fakeInteractive struct {
	shellCalls   []string
	sessionCalls []string
	target       string
	err          error
}

func (f *fakeInteractive) OpenShell(_ context.Context, projectPath string) (string, error) {
	f.shellCalls = append(f.shellCalls, projectPath)
	return f.target, f.err
}

func (f *fakeInteractive) OpenSession(_ context.Context, projectPath string) (string, error) {
	f.sessionCalls = append(f.sessionCalls, projectPath)
	return f.target, f.err
}

func TestDirCommandLaunchesShell(t *testing.T) {
	fake := &fakeInteractive{target: "shell-widget"}
	m, projPath := selectProject(t, t.TempDir(), "widget")
	m.interactive = fake

	m, cmd := runProjectCommand(t, m, "dir")
	if m.mode != modeNormal {
		t.Fatalf("mode = %v, want modeNormal", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a launch command, got nil")
	}
	// Running the command performs the launch and yields the result message.
	msg := cmd()
	launched, ok := msg.(interactiveLaunchedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want interactiveLaunchedMsg", msg)
	}
	if launched.kind != "shell" || launched.target != "shell-widget" {
		t.Errorf("launched = %+v, want shell → shell-widget", launched)
	}
	if len(fake.shellCalls) != 1 || fake.shellCalls[0] != projPath {
		t.Errorf("OpenShell calls = %v, want [%q]", fake.shellCalls, projPath)
	}
	if len(fake.sessionCalls) != 0 {
		t.Errorf("OpenShell must not call OpenSession; got %v", fake.sessionCalls)
	}
}

func TestSessionCommandLaunchesSession(t *testing.T) {
	fake := &fakeInteractive{target: "session-widget"}
	m, projPath := selectProject(t, t.TempDir(), "widget")
	m.interactive = fake

	m, cmd := runProjectCommand(t, m, "session")
	if cmd == nil {
		t.Fatal("expected a launch command, got nil")
	}
	launched := cmd().(interactiveLaunchedMsg)
	if launched.kind != "session" || launched.target != "session-widget" {
		t.Errorf("launched = %+v, want session → session-widget", launched)
	}
	if len(fake.sessionCalls) != 1 || fake.sessionCalls[0] != projPath {
		t.Errorf("OpenSession calls = %v, want [%q]", fake.sessionCalls, projPath)
	}
}

func TestInteractiveCommandsRequireLauncher(t *testing.T) {
	// With no launcher wired (standalone tui), :dir reports unavailable.
	m, _ := selectProject(t, t.TempDir(), "widget")
	m.interactive = nil
	m, cmd := runProjectCommand(t, m, "dir")
	if cmd != nil {
		t.Errorf("expected no launch command without a launcher")
	}
	if !strings.Contains(m.statusMsg, "not available") {
		t.Errorf("statusMsg = %q, want an unavailability message", m.statusMsg)
	}
}

func TestInteractiveCommandsRequireSelection(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m.interactive = &fakeInteractive{}
	m = focusProjectsPane(t, m)
	m, cmd := runProjectCommand(t, m, "session")
	if cmd != nil {
		t.Errorf("expected no launch command with no work stream selected")
	}
	if !strings.Contains(m.statusMsg, "no work stream") {
		t.Errorf("statusMsg = %q, want a no-work-stream message", m.statusMsg)
	}
}

func TestInteractiveLaunchedReportsNewTab(t *testing.T) {
	attacher := &fakeAttacher{}
	m := newModel(nil, make(<-chan []SessionView), nil, attacher)
	next, cmd := m.handleInteractiveLaunched(interactiveLaunchedMsg{kind: "session", target: "session-widget"})
	m = next.(model)
	// The launcher already opened (and the terminal focused) the new tab, so
	// there is nothing left to attach.
	if cmd != nil || len(attacher.targets) != 0 {
		t.Errorf("expected no attach after opening a new tab; targets=%v", attacher.targets)
	}
	if !strings.Contains(m.statusMsg, "new tab") || !strings.Contains(m.statusMsg, "session-widget") {
		t.Errorf("statusMsg = %q, want it to mention the new tab", m.statusMsg)
	}
}

// When the work stream's tab is already open the launcher focuses it instead
// of opening a second one; the TUI must say so with a warning.
func TestInteractiveLaunchedWarnsWhenTabAlreadyOpen(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	next, _ := m.handleInteractiveLaunched(interactiveLaunchedMsg{
		kind:   "session",
		target: "session-widget",
		err:    &terminal.AlreadyOpenError{Name: "session-widget", Title: "widget"},
	})
	m = next.(model)
	for _, want := range []string{"⚠", "widget", "already open", "switched to it"} {
		if !strings.Contains(m.statusMsg, want) {
			t.Errorf("statusMsg = %q, want it to contain %q", m.statusMsg, want)
		}
	}
	if strings.HasPrefix(m.statusMsg, "open session:") {
		t.Errorf("already-open must be a warning, not a launch failure: %q", m.statusMsg)
	}
}

func TestInteractiveLaunchedWarnsWhenAlreadyOpenTabCannotBeFocused(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	next, _ := m.handleInteractiveLaunched(interactiveLaunchedMsg{
		kind: "shell",
		err: &terminal.AlreadyOpenError{Name: "shell-widget", Title: "widget",
			FocusErr: fmt.Errorf("ghostty on linux: %w", terminal.ErrFocusUnsupported)},
	})
	m = next.(model)
	for _, want := range []string{"already open", "switch to it yourself", "cannot switch tabs"} {
		if !strings.Contains(m.statusMsg, want) {
			t.Errorf("statusMsg = %q, want it to contain %q", m.statusMsg, want)
		}
	}
}

func TestInteractiveLaunchedSurfacesError(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	next, cmd := m.handleInteractiveLaunched(interactiveLaunchedMsg{kind: "shell", err: errors.New("boom")})
	m = next.(model)
	if cmd != nil {
		t.Errorf("expected no attach command on error")
	}
	if !strings.Contains(m.statusMsg, "boom") {
		t.Errorf("statusMsg = %q, want it to carry the error", m.statusMsg)
	}
}

func TestCommandPickerNavigation(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	m = focusProjectsPane(t, m)
	m = openCommandPicker(t, m)
	// j moves down, k moves back up; clamped at the ends.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = step.(model)
	if m.cmdPickerIdx != 1 {
		t.Errorf("after j, idx = %d, want 1", m.cmdPickerIdx)
	}
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = step.(model)
	if m.cmdPickerIdx != 0 {
		t.Errorf("after k, idx = %d, want 0", m.cmdPickerIdx)
	}
	// k at the top stays at 0.
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = step.(model)
	if m.cmdPickerIdx != 0 {
		t.Errorf("k at top should clamp; idx = %d, want 0", m.cmdPickerIdx)
	}
}

func TestCommandPickerModalRenders(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = sized.(model)
	m = focusProjectsPane(t, m)
	m = openCommandPicker(t, m)
	view := m.View()
	for _, want := range []string{"Commands", "dir", "session", "wolf"} {
		if !strings.Contains(view, want) {
			t.Errorf("picker view missing %q; got:\n%s", want, view)
		}
	}
}
