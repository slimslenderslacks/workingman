package tui

import (
	"context"
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/terminal"
)

// attachResultMsg is delivered after an attempt to switch to a session's
// terminal tab finishes. err is nil when the tab was brought to the front;
// otherwise it carries the reason so the model can surface it in the footer.
type attachResultMsg struct {
	target string
	err    error
}

// tabAttacher is the seam between the TUI and the terminal: attaching to a
// non-ACP session row (the --wolf-host wolf) focuses the terminal tab it runs
// in. The default implementation goes through terminal.TabLauncher; tests stub
// it out.
type tabAttacher interface {
	Attach(target string) tea.Cmd
}

// TabFocuser focuses the terminal tab of a session by its session name —
// terminal.TabLauncher.Focus. Wired from cmd/orch so the TUI and the daemon
// share one TabLauncher (and so one view of which tabs are open).
type TabFocuser interface {
	Focus(ctx context.Context, name string) error
}

// focusAttacher is the production tabAttacher.
type focusAttacher struct {
	focuser TabFocuser
}

func newTabAttacher(f TabFocuser) *focusAttacher {
	if f == nil {
		f = &terminal.TabLauncher{}
	}
	return &focusAttacher{focuser: f}
}

func (a *focusAttacher) Attach(target string) tea.Cmd {
	return func() tea.Msg {
		if target == "" {
			return attachResultMsg{target: target, err: fmt.Errorf("no terminal tab for this session")}
		}
		err := a.focuser.Focus(context.Background(), target)
		switch {
		case errors.Is(err, terminal.ErrTabNotFound):
			err = fmt.Errorf("its terminal tab is no longer open")
		case errors.Is(err, terminal.ErrFocusUnsupported):
			err = fmt.Errorf("switch to its tab yourself (%w)", err)
		}
		return attachResultMsg{target: target, err: err}
	}
}
