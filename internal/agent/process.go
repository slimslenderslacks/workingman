package agent

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// termGrace is how long Close waits for a child to exit after SIGTERM before
// escalating to SIGKILL. acp-wrapper traps SIGTERM to remove its socket and
// session.json, so a graceful term is preferred — but a wedged process must
// still be reaped.
const termGrace = 3 * time.Second

// ProcessLauncher runs each Spec as a plain host child process. It is the
// launch mechanism for non-interactive ACP agents: their acp-wrapper process
// runs on the host (creating the sandbox, exec'ing the ACP client, serving
// agent.sock) rather than living in a tmux window. Unlike TmuxLauncher there
// is no terminal to attach to — the session is observed by the TUI over ACP,
// and the child's stderr is forwarded to Stderr (if set) for diagnostics.
type ProcessLauncher struct {
	// Stderr, when non-nil, receives the child's stderr. Leave nil to discard
	// it (os/exec connects an unset Stderr to the null device). In the TUI'd
	// daemon this is pointed at a log file so wrapper diagnostics don't corrupt
	// the alt-screen.
	Stderr io.Writer
}

// Launch starts spec.Command as a host process in spec.Workspace and returns a
// Session backed by it. The process is detached from the launch ctx: its
// lifetime is owned by the returned Session (Wait observes its exit; Close
// tears it down), matching TmuxLauncher's window semantics where ctx scopes
// only the launch call, not the running agent.
func (l *ProcessLauncher) Launch(_ context.Context, spec Spec) (Session, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("agent: spec.Name is required")
	}
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("agent: spec.Command is required")
	}

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Workspace
	cmd.Stderr = l.Stderr
	// Put the child in its own process group. Without this it inherits orch's
	// group, so a terminal SIGINT (Ctrl-C, the normal way to stop/restart
	// orch) is delivered to acp-wrapper directly by the kernel — independent
	// of anything this package does — and acp-wrapper treats SIGINT as an
	// ordinary shutdown request (see cmd/acp-wrapper/main.go), tearing down
	// the very session a restart is supposed to leave behind for reconnection.
	// Isolating the group means the only way this process receives a signal
	// is an explicit one from Close(), which the daemon now reserves for
	// sessions it actually means to end (see Daemon.shutdown's ACP detach path).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("agent: start %s: %w", spec.Command[0], err)
	}

	s := &processSession{name: spec.Name, cmd: cmd, done: make(chan struct{})}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.done)
	}()
	return s, nil
}

// processSession is a Session backed by a host child process.
type processSession struct {
	name      string
	cmd       *exec.Cmd
	done      chan struct{} // closed once the process has exited
	waitErr   error         // the process's exit error; read only after done is closed
	closeOnce sync.Once
}

func (s *processSession) Name() string { return s.name }

// Wait blocks until the process exits or ctx is cancelled. On ctx cancellation
// it tears the process down (so a daemon shutdown doesn't leak the wrapper) and
// returns ctx.Err(); on normal exit it returns the process's exit error.
func (s *processSession) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return s.waitErr
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	}
}

// Close terminates the process and is idempotent. It sends SIGTERM first to let
// acp-wrapper clean up its socket/session.json, then escalates to SIGKILL if
// the process hasn't exited within termGrace.
func (s *processSession) Close() error {
	s.closeOnce.Do(func() {
		if s.cmd.Process == nil {
			return
		}
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.done:
		case <-time.After(termGrace):
			_ = s.cmd.Process.Kill()
		}
	})
	return nil
}
