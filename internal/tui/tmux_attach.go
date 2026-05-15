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
// nil when at least one tmux client was successfully switched to the target
// window; otherwise it carries the pre-flight or switch error so the model
// can surface it in the footer.
type attachResultMsg struct {
	target string
	err    error
}

// tmuxAttacher is the seam between the TUI and tmux. The default
// implementation drives tmux directly; tests stub it out.
type tmuxAttacher interface {
	Attach(target string) tea.Cmd
}

// defaultTmuxAttacher asks tmux to switch any currently-attached client to
// the requested window. With orch's umbrella-session model the user is
// expected to have a single tmux client attached to the umbrella; clicking
// a session row in the TUI flips that client to the agent's window.
//
// If no tmux client is attached, the attach returns a friendly error
// instead of trying to spawn a new terminal. The user can run
// `tmux attach -t orch` themselves and click again.
type defaultTmuxAttacher struct {
	binary       string                              // resolved tmux binary; defaults to "tmux"
	lookPath     func(string) (string, error)        // defaults to exec.LookPath
	exists       func(binary, target string) bool    // defaults to tmuxSessionExists
	switchClient func(binary, target string) bool    // defaults to tmuxSwitchClient
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
			return attachResultMsg{target: target, err: fmt.Errorf("tmux window %q is not alive", target)}
		}
	}
	return func() tea.Msg {
		if a.switchClient != nil && a.switchClient(a.binary, target) {
			return attachResultMsg{target: target, err: nil}
		}
		umbrella := umbrellaFromTarget(target)
		return attachResultMsg{
			target: target,
			err:    fmt.Errorf("no tmux client attached; run `tmux attach -t %s` and click again", umbrella),
		}
	}
}

// umbrellaFromTarget extracts the umbrella session name from a "sess:window"
// target. Returns the agent default if the target doesn't look like the
// canonical form — the error message stays useful even for legacy targets.
func umbrellaFromTarget(target string) string {
	if sess, _, ok := strings.Cut(target, ":"); ok && sess != "" {
		return sess
	}
	return agent.DefaultUmbrellaSession
}

// tmuxSwitchClient asks tmux to flip every attached client over to the
// requested target window. Returns false (without erroring) if no client
// is attached — the attacher uses that signal to surface a "run tmux
// attach first" hint to the user.
//
// We switch every client rather than just the first so a user who has the
// same umbrella session open in two terminals sees both update.
func tmuxSwitchClient(binary, target string) bool {
	out, err := exec.Command(binary, "list-clients", "-F", "#{client_tty}").Output()
	if err != nil {
		return false
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return false
	}
	var any bool
	for _, tty := range strings.Split(string(trimmed), "\n") {
		tty = strings.TrimSpace(tty)
		if tty == "" {
			continue
		}
		if err := exec.Command(binary, "switch-client", "-c", tty, "-t", target).Run(); err == nil {
			any = true
		}
	}
	return any
}

// tmuxSessionExists reports whether the given tmux target is currently
// alive. The target is the canonical "session:window" form produced by
// agent.tmuxSession.Name; we check window-level existence with
// list-windows so an attach attempt can't race against a window that
// already closed even though its umbrella session is still up. A bare
// session name (no colon) falls back to has-session for callers that pass
// the legacy form.
func tmuxSessionExists(binary, target string) bool {
	sess, win, hasWin := strings.Cut(target, ":")
	if !hasWin || win == "" {
		return exec.Command(binary, "has-session", "-t", sess).Run() == nil
	}
	out, err := exec.Command(binary, "list-windows", "-t", sess, "-F", "#W").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == win {
			return true
		}
	}
	return false
}
