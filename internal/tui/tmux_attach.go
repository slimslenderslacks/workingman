package tui

import (
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// attachResultMsg is delivered after a tmux attach attempt finishes. err is
// nil when the user detached cleanly; otherwise it carries the pre-flight or
// process error so the model can surface it in the footer.
type attachResultMsg struct {
	target string
	err    error
}

// tmuxAttacher builds the tea.Cmd that suspends the TUI and runs
// `tmux attach -t <target>` in the foreground. The interface lets tests stub
// out the exec without spinning up a real tmux server.
type tmuxAttacher interface {
	Attach(target string) tea.Cmd
}

// defaultTmuxAttacher runs the user's tmux binary against its default server.
// Pre-flight checks (binary on PATH, session alive, stdin is a TTY) run
// synchronously so failures surface immediately instead of after a
// terminal-swap flicker.
type defaultTmuxAttacher struct {
	binary   string                              // overridable for tests; defaults to "tmux"
	lookPath func(string) (string, error)        // defaults to exec.LookPath
	hasTTY   func() bool                         // defaults to stdinIsTTY
	exists   func(binary, target string) bool    // defaults to tmuxSessionExists
	build    func(binary, target string) *exec.Cmd // defaults to exec.Command(binary,"attach","-t",target)
}

func newTmuxAttacher() *defaultTmuxAttacher {
	return &defaultTmuxAttacher{
		binary:   "tmux",
		lookPath: exec.LookPath,
		hasTTY:   stdinIsTTY,
		exists:   tmuxSessionExists,
		build: func(binary, target string) *exec.Cmd {
			return exec.Command(binary, "attach", "-t", target)
		},
	}
}

func (a *defaultTmuxAttacher) Attach(target string) tea.Cmd {
	if target == "" {
		return func() tea.Msg {
			return attachResultMsg{target: target, err: fmt.Errorf("no tmux target for this session")}
		}
	}
	if _, err := a.lookPath(a.binary); err != nil {
		return func() tea.Msg {
			return attachResultMsg{target: target, err: fmt.Errorf("tmux not found on PATH")}
		}
	}
	if !a.hasTTY() {
		return func() tea.Msg {
			return attachResultMsg{target: target, err: fmt.Errorf("cannot attach without a TTY")}
		}
	}
	if !a.exists(a.binary, target) {
		return func() tea.Msg {
			return attachResultMsg{target: target, err: fmt.Errorf("tmux session %q is not alive", target)}
		}
	}
	cmd := a.build(a.binary, target)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return attachResultMsg{target: target, err: err}
	})
}

// tmuxSessionExists asks tmux whether a session with the given name is alive.
// Any non-zero exit (including "session not found") is treated as dead, which
// matches what the user would see if they tried `tmux attach` manually.
func tmuxSessionExists(binary, target string) bool {
	return exec.Command(binary, "has-session", "-t", target).Run() == nil
}

// stdinIsTTY reports whether stdin is connected to a character device. Used
// as the pre-flight TTY check before attaching — tmux's own error in this
// case is opaque, so we catch it ourselves.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
