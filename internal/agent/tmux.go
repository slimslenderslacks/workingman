package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// TmuxLauncher starts each Spec in its own detached tmux session so the user
// can `tmux attach -t <name>` at any time. By default it uses the user's
// running tmux server; tests can set Socket (mapping to `tmux -L <socket>`)
// to run against an isolated server instead.
type TmuxLauncher struct {
	// Binary is the tmux executable. Defaults to "tmux" on PATH.
	Binary string
	// Socket maps to `tmux -L <socket>`. Leave empty to use the default server.
	Socket string
	// PollInterval is how often Session.Wait polls `has-session`.
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

func (t *TmuxLauncher) poll() time.Duration {
	if t.PollInterval > 0 {
		return t.PollInterval
	}
	return 200 * time.Millisecond
}

func (t *TmuxLauncher) Launch(ctx context.Context, spec Spec) (Session, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("agent: spec.Name is required")
	}
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("agent: spec.Command is required")
	}
	args := append(t.baseArgs(), "new-session", "-d", "-s", spec.Name)
	if spec.Workspace != "" {
		args = append(args, "-c", spec.Workspace)
	}
	args = append(args, spec.Command...)
	out, err := exec.CommandContext(ctx, t.binary(), args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tmux new-session: %w: %s", err, bytes.TrimSpace(out))
	}
	return &tmuxSession{
		name:   spec.Name,
		binary: t.binary(),
		base:   t.baseArgs(),
		poll:   t.poll(),
	}, nil
}

type tmuxSession struct {
	name   string
	binary string
	base   []string
	poll   time.Duration
}

func (s *tmuxSession) Name() string { return s.name }

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

func (s *tmuxSession) exists() bool {
	args := append(s.base, "has-session", "-t", s.name)
	return exec.Command(s.binary, args...).Run() == nil
}

// Close is idempotent — calling it on a session that already exited is a no-op.
func (s *tmuxSession) Close() error {
	if !s.exists() {
		return nil
	}
	args := append(s.base, "kill-session", "-t", s.name)
	out, err := exec.Command(s.binary, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kill-session: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
