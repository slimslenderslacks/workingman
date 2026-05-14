package tui

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/agent"
)

// attachResultMsg is delivered after a tmux attach attempt finishes. err is
// nil when the requested session was made visible to the user (either by
// switching an existing tmux client or by opening a new Terminal window);
// otherwise it carries the pre-flight or process error so the model can
// surface it in the footer.
type attachResultMsg struct {
	target string
	err    error
}

// tmuxAttacher builds the tea.Cmd that takes the user from the TUI to the
// requested tmux session. The interface lets tests stub out the exec layer
// without spinning up a real tmux server or actually opening a window.
type tmuxAttacher interface {
	Attach(target string) tea.Cmd
}

// defaultTmuxAttacher routes through tmux in two stages:
//
//  1. If any tmux client is already attached, send `switch-client -t <target>`
//     to that client. The user's existing tmux instance flips to the
//     target session — far less disruptive than spawning a new window.
//  2. If no client is attached, open a new Terminal.app window running
//     `tmux attach -t <target>` via osascript. macOS prompts once for
//     Automation permission; approve and it's silent thereafter.
//
// Each hook is a function field so tests can drive every branch without
// touching tmux or osascript.
type defaultTmuxAttacher struct {
	binary       string                              // resolved tmux binary; defaults to "tmux"
	lookPath     func(string) (string, error)        // defaults to exec.LookPath
	exists       func(binary, target string) bool    // defaults to tmuxSessionExists
	switchClient func(binary, target string) bool    // defaults to tmuxSwitchClient
	openInTerm   func(binary, target string) error   // defaults to openTerminalWithTmux
}

func newTmuxAttacher() *defaultTmuxAttacher {
	bin := "tmux"
	if resolved, err := agent.ResolveTmux(); err == nil {
		bin = resolved
	}
	return &defaultTmuxAttacher{
		binary:       bin,
		lookPath:     exec.LookPath,
		exists:       tmuxSessionExists,
		switchClient: tmuxSwitchClient,
		openInTerm:   openTerminalWithTmux,
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
	if !a.exists(a.binary, target) {
		return func() tea.Msg {
			return attachResultMsg{target: target, err: fmt.Errorf("tmux session %q is not alive", target)}
		}
	}
	return func() tea.Msg {
		// Prefer the existing-client path: zero-disruption, the user's
		// current tmux window just changes session.
		if a.switchClient != nil && a.switchClient(a.binary, target) {
			return attachResultMsg{target: target, err: nil}
		}
		// No attached client → spawn a new Terminal window. Pass the
		// resolved tmux binary so the new shell's PATH doesn't matter.
		if err := a.openInTerm(a.binary, target); err != nil {
			return attachResultMsg{target: target, err: err}
		}
		return attachResultMsg{target: target, err: nil}
	}
}

// tmuxSwitchClient asks tmux to switch an already-attached client to the
// requested session. Returns false (without erroring) if there's no
// attached client to switch — that's the "no existing tmux" signal, which
// the attacher uses to decide whether to spawn a new Terminal window.
//
// When multiple clients are attached we pick the first one returned by
// list-clients; tmux orders them most-recently-active first, which is
// almost always the right choice.
func tmuxSwitchClient(binary, target string) bool {
	out, err := exec.Command(binary, "list-clients", "-F", "#{client_tty}").Output()
	if err != nil {
		return false
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return false
	}
	tty := strings.SplitN(string(trimmed), "\n", 2)[0]
	tty = strings.TrimSpace(tty)
	if tty == "" {
		return false
	}
	return exec.Command(binary, "switch-client", "-c", tty, "-t", target).Run() == nil
}

// openTerminalWithTmux runs osascript to ask Terminal.app to open a new
// window that runs `<binary> attach -t <target>`. binary is the resolved
// absolute path to tmux, so the new shell does not need tmux in its PATH.
func openTerminalWithTmux(binary, target string) error {
	cmd := fmt.Sprintf("%s attach -t %s", applescriptEscape(binary), applescriptEscape(target))
	script := fmt.Sprintf(`tell application "Terminal" to do script "%s"`, cmd)
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript Terminal: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// applescriptEscape escapes a string for embedding inside an AppleScript
// double-quoted literal. Backslash and double-quote are the only
// metacharacters that need quoting.
func applescriptEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// tmuxSessionExists asks tmux whether a session with the given name is alive.
// Any non-zero exit (including "session not found") is treated as dead, which
// matches what the user would see if they tried `tmux attach` manually.
func tmuxSessionExists(binary, target string) bool {
	return exec.Command(binary, "has-session", "-t", target).Run() == nil
}
