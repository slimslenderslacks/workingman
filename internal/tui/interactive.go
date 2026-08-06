package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// InteractiveLauncher opens ad-hoc interactive sandbox windows for a work
// stream: OpenShell backs the `:dir` command (a bash shell in the work stream's
// workspace) and OpenSession backs `:session` (a persistent, resumable claude
// session). Both return the tmux target ("session:window") of the window they
// created or reused so the caller can switch a client to it. Wired from
// cmd/orch in integrated daemon mode; nil in standalone `orch tui` and most
// tests, where the commands report that interactive sessions aren't available.
type InteractiveLauncher interface {
	OpenShell(ctx context.Context, projectPath string) (string, error)
	OpenSession(ctx context.Context, projectPath string) (string, error)
}

// interactiveLaunchedMsg carries the result of an InteractiveLauncher call back
// into Update so the model can switch to the new window or surface an error.
type interactiveLaunchedMsg struct {
	kind   string // "shell" or "session" — for the status message
	target string
	err    error
}

// openInteractive kicks off a `:dir` (kind "shell") or `:session` (kind
// "session") launch. It validates the preconditions on the UI goroutine and
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
		if kind == "session" {
			target, err = launcher.OpenSession(context.Background(), projectPath)
		} else {
			target, err = launcher.OpenShell(context.Background(), projectPath)
		}
		return interactiveLaunchedMsg{kind: kind, target: target, err: err}
	}
}

// handleInteractiveLaunched reports the outcome of a `:dir`/`:session` launch
// and, on success, switches the attached tmux client straight to the new
// window (reusing the same attach path as clicking a session row).
func (m model) handleInteractiveLaunched(msg interactiveLaunchedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.statusMsg = "open " + msg.kind + ": " + msg.err.Error()
		return m, nil
	}
	m.statusMsg = "opened " + msg.kind + " → " + msg.target
	if m.attacher != nil && msg.target != "" {
		return m, m.attacher.Attach(msg.target)
	}
	return m, nil
}
