package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultUmbrellaSession is the tmux session every agent's window lives in
// unless TmuxLauncher.SessionName overrides it. Keeping all agents inside
// one session means the user can stay attached to a single tmux client and
// flip between agents with Ctrl-<prefix> n/p/w, instead of juggling many
// top-level sessions.
const DefaultUmbrellaSession = "orch"

// TmuxLauncher starts each Spec as a window inside a shared umbrella tmux
// session (default "orch"). The user can `tmux attach -t orch` once and
// stay attached as agents come and go — each new launch adds a window,
// each session-end closes a window. By default it uses the user's running
// tmux server; tests can set Socket (mapping to `tmux -L <socket>`) to run
// against an isolated server instead.
type TmuxLauncher struct {
	// Binary is the tmux executable. Defaults to "tmux" on PATH.
	Binary string
	// Socket maps to `tmux -L <socket>`. Leave empty to use the default server.
	Socket string
	// SessionName is the umbrella tmux session every agent's window lives
	// inside. Defaults to DefaultUmbrellaSession ("orch").
	SessionName string
	// PollInterval is how often Session.Wait polls `list-windows`.
	// Defaults to 200ms.
	PollInterval time.Duration
}

func (t *TmuxLauncher) binary() string {
	if t.Binary != "" {
		return t.Binary
	}
	return "tmux"
}

func (t *TmuxLauncher) baseArgs() []string {
	if t.Socket == "" {
		return nil
	}
	return []string{"-L", t.Socket}
}

func (t *TmuxLauncher) sessionName() string {
	if t.SessionName != "" {
		return t.SessionName
	}
	return DefaultUmbrellaSession
}

func (t *TmuxLauncher) poll() time.Duration {
	if t.PollInterval > 0 {
		return t.PollInterval
	}
	return 200 * time.Millisecond
}

// Launch puts spec into a new window inside the umbrella session. If the
// umbrella session doesn't yet exist, it's created with this agent as its
// first window in a single new-session call — avoiding the dead default
// window tmux would otherwise leave around.
//
// We avoid a pre-flight `has-session` check on purpose: when an umbrella's
// last window dies tmux is racy about its own bookkeeping, and an earlier
// version of this code hit "duplicate session: orch" from new-session
// immediately after has-session had said the session was gone (the
// destruction was still in flight). Instead we try the operation we think
// applies, and fall back on the actual tmux error:
//
//  1. Try new-window — the steady-state case.
//  2. If tmux says "can't find session", fall back to new-session.
//  3. If new-session loses a race with another launcher and says
//     "duplicate session", retry as new-window.
//
// Each tmux command is its own ground truth, so there is no window where
// our cached belief about the session can drift from reality.
func (t *TmuxLauncher) Launch(ctx context.Context, spec Spec) (Session, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("agent: spec.Name is required")
	}
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("agent: spec.Command is required")
	}
	umbrella := t.sessionName()
	winName := spec.Name

	out, err := t.runNewWindow(ctx, umbrella, winName, spec)
	if err != nil && isSessionMissing(out) {
		out, err = t.runNewSession(ctx, umbrella, winName, spec)
		if err != nil && isDuplicateSession(out) {
			out, err = t.runNewWindow(ctx, umbrella, winName, spec)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("tmux launch: %w: %s", err, bytes.TrimSpace(out))
	}
	return &tmuxSession{
		sessionName: umbrella,
		windowName:  winName,
		binary:      t.binary(),
		base:        t.baseArgs(),
		poll:        t.poll(),
	}, nil
}

func (t *TmuxLauncher) runNewWindow(ctx context.Context, umbrella, winName string, spec Spec) ([]byte, error) {
	args := append(t.baseArgs(), "new-window", "-t", umbrella+":", "-n", winName)
	if spec.Workspace != "" {
		args = append(args, "-c", spec.Workspace)
	}
	args = append(args, spec.Command...)
	return exec.CommandContext(ctx, t.binary(), args...).CombinedOutput()
}

func (t *TmuxLauncher) runNewSession(ctx context.Context, umbrella, winName string, spec Spec) ([]byte, error) {
	args := append(t.baseArgs(), "new-session", "-d", "-s", umbrella, "-n", winName)
	if spec.Workspace != "" {
		args = append(args, "-c", spec.Workspace)
	}
	args = append(args, spec.Command...)
	return exec.CommandContext(ctx, t.binary(), args...).CombinedOutput()
}

// isSessionMissing matches the family of tmux stderr messages that all
// mean "the umbrella isn't reachable right now": session not in the list
// ("can't find session"), no tmux server at all ("no server running"),
// and the test-socket case where tmux couldn't even open the socket
// ("error connecting"). In any of these cases new-session is the correct
// recovery. Lower-cased substring match so minor wording differences
// across tmux versions still trigger the fallback.
func isSessionMissing(out []byte) bool {
	low := bytes.ToLower(out)
	for _, needle := range [][]byte{
		[]byte("can't find session"),
		[]byte("no server running"),
		[]byte("error connecting"),
	} {
		if bytes.Contains(low, needle) {
			return true
		}
	}
	return false
}

// isDuplicateSession matches tmux's "duplicate session" stderr from
// new-session when the umbrella was created between our calls — the race
// we're guarding against by retrying as new-window.
func isDuplicateSession(out []byte) bool {
	return bytes.Contains(bytes.ToLower(out), []byte("duplicate session"))
}

type tmuxSession struct {
	sessionName string
	windowName  string
	binary      string
	base        []string
	poll        time.Duration
}

// Name returns the tmux target spec for this window — "session:window".
// Callers that need to attach to it (TUI's switch-client / new-Terminal
// fallback) can pass this straight to tmux's -t flag.
func (s *tmuxSession) Name() string {
	return s.sessionName + ":" + s.windowName
}

func (s *tmuxSession) Wait(ctx context.Context) error {
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		if !s.exists() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// exists reports whether this window is currently present in the umbrella
// session. Uses `list-windows -F '#W'` and matches by name. Any tmux error
// (including the umbrella session itself being gone) counts as "not
// present" — same effect either way: the agent isn't running anymore.
func (s *tmuxSession) exists() bool {
	args := append(s.base, "list-windows", "-t", s.sessionName, "-F", "#W")
	out, err := exec.Command(s.binary, args...).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == s.windowName {
			return true
		}
	}
	return false
}

// Close kills just this agent's window — the umbrella session and other
// agents' windows stay running. Idempotent: a call on a window that's
// already gone is a no-op.
func (s *tmuxSession) Close() error {
	if !s.exists() {
		return nil
	}
	args := append(s.base, "kill-window", "-t", s.sessionName+":"+s.windowName)
	out, err := exec.Command(s.binary, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kill-window: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
