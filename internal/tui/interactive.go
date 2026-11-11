package tui

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/terminal"
)

// InteractiveLauncher opens ad-hoc interactive sandbox sessions for a work
// stream, each in a new terminal tab titled after the work stream: OpenShell
// backs the `:dir` command (a bash shell in the work stream's workspace) and
// OpenSession backs `:session` (a persistent, resumable claude session). Both
// return the session name of the tab they opened. When the work stream's tab
// is already open they focus it instead of opening a second one and return a
// *terminal.AlreadyOpenError alongside the name. Wired from cmd/orch in
// integrated daemon mode; nil in standalone `orch tui` and most tests, where
// the commands report that interactive sessions aren't available.
type InteractiveLauncher interface {
	OpenShell(ctx context.Context, projectPath string) (string, error)
	OpenSession(ctx context.Context, projectPath string) (string, error)
	// OpenOrchDir backs `:orch` (a shell in the work stream's orch directory)
	// and OpenWorkspaceDir backs `:workspace` (a shell at the root of its wsp
	// workspace). Neither provisions anything: a missing directory is an error.
	OpenOrchDir(ctx context.Context, projectPath string) (string, error)
	OpenWorkspaceDir(ctx context.Context, projectPath string) (string, error)
}

// interactiveLaunchedMsg carries the result of an InteractiveLauncher call back
// into Update so the model can report the new tab or surface an error.
type interactiveLaunchedMsg struct {
	kind   string // "shell", "session", "orch" or "workspace" — for the status message
	target string
	err    error
}

// openInteractive kicks off a `:dir` (kind "shell"), `:session` (kind
// "session"), `:orch` (kind "orch") or `:workspace` (kind "workspace") launch. It validates the preconditions on the UI goroutine and
// returns a tea.Cmd that performs the actual launch — which may block on
// workspace provisioning and sandbox creation — off the UI goroutine, reporting
// the outcome via interactiveLaunchedMsg.
func (m model) openInteractive(kind string) (tea.Model, tea.Cmd) {
	if m.interactive == nil {
		m.statusMsg = "interactive sessions not available (run the integrated daemon)"
		return m, nil
	}
	if m.projSel == "" {
		m.statusMsg = "no work stream selected"
		return m, nil
	}
	projectPath := m.projSel
	launcher := m.interactive
	m.statusMsg = "opening " + kind + "…"
	return m, func() tea.Msg {
		var (
			target string
			err    error
		)
		ctx := context.Background()
		switch kind {
		case "session":
			target, err = launcher.OpenSession(ctx, projectPath)
		case "orch":
			target, err = launcher.OpenOrchDir(ctx, projectPath)
		case "workspace":
			target, err = launcher.OpenWorkspaceDir(ctx, projectPath)
		default:
			target, err = launcher.OpenShell(ctx, projectPath)
		}
		return interactiveLaunchedMsg{kind: kind, target: target, err: err}
	}
}

// handleInteractiveLaunched reports the outcome of a `:dir`/`:session` launch.
// The launcher has already opened (and the terminal focused) the new tab; when
// the work stream's tab was already open it warns that no second tab was
// opened and that it switched to the existing one instead.
func (m model) handleInteractiveLaunched(msg interactiveLaunchedMsg) (tea.Model, tea.Cmd) {
	var already *terminal.AlreadyOpenError
	switch {
	case errors.As(msg.err, &already):
		m.statusMsg = "⚠ " + msg.kind + " tab for " + already.Title + " is already open — "
		if already.FocusErr != nil {
			m.statusMsg += "switch to it yourself (" + already.FocusErr.Error() + ")"
		} else {
			m.statusMsg += "switched to it"
		}
	case msg.err != nil:
		m.statusMsg = "open " + msg.kind + ": " + msg.err.Error()
	default:
		m.statusMsg = "opened " + msg.kind + " in a new tab → " + msg.target
	}
	return m, nil
}
