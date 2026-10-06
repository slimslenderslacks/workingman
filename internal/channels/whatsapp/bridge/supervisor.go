package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ExitLoggedOut is the exit code bridge.js uses when WhatsApp ended the linked
// session (or the session was never paired). Restarting cannot fix it, so the
// Supervisor stops and reports ErrLoggedOut.
const ExitLoggedOut = 78

// ErrLoggedOut is returned by Supervisor.Run when the bridge exited with
// ExitLoggedOut: the number must be re-paired with `orch whatsapp pair`.
var ErrLoggedOut = errors.New("whatsapp session logged out or not paired; run `orch whatsapp pair`")

// Process is one running bridge.
type Process interface {
	// Wait blocks until the process exits and returns its exit code (-1 if
	// unknown, e.g. killed by a signal) and a description of how it ended.
	Wait() (code int, err error)
	// Stop asks the process to exit and, if it does not, kills it. It does
	// not wait; Wait observes the exit. Safe to call repeatedly.
	Stop()
}

// Starter launches a bridge process. ctx outlives the process: it is the
// supervisor's context, not a per-process one.
type Starter func(ctx context.Context) (Process, error)

// Backoff shapes the delay between restarts.
type Backoff struct {
	Initial time.Duration // first delay; default 1s
	Max     time.Duration // cap; default 1m
	// ResetAfter: a process that ran at least this long was healthy, so the
	// next failure starts from Initial again. Default 30s.
	ResetAfter time.Duration
}

func (b Backoff) withDefaults() Backoff {
	if b.Initial <= 0 {
		b.Initial = time.Second
	}
	if b.Max <= 0 {
		b.Max = time.Minute
	}
	if b.Max < b.Initial {
		b.Max = b.Initial
	}
	if b.ResetAfter <= 0 {
		b.ResetAfter = 30 * time.Second
	}
	return b
}

// delay returns the wait before restart number attempt (0-based).
func (b Backoff) delay(attempt int) time.Duration {
	d := b.Initial
	for i := 0; i < attempt && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	return d
}

// SupervisorStatus is a point-in-time view for health reporting.
type SupervisorStatus struct {
	Running   bool
	Restarts  int // starts after the first
	LastExit  int // exit code of the most recent run, valid once Restarts>0 or !Running
	LastErr   string
	LoggedOut bool
}

// Supervisor runs one bridge process at a time and restarts it with
// exponential backoff whenever it exits, until its context is cancelled or the
// bridge reports it is logged out.
type Supervisor struct {
	Start   Starter
	Backoff Backoff
	Log     *slog.Logger

	// Sleep and Now are test seams. Sleep must return early with ctx.Err()
	// when ctx is cancelled.
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time

	mu      sync.Mutex
	current Process
	status  SupervisorStatus
	kick    bool // Restart() was requested: skip the backoff once
}

func (s *Supervisor) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Supervisor) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Status returns the current state.
func (s *Supervisor) Status() SupervisorStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Restart stops the current process (if any) so Run starts a fresh one
// immediately, without extending the backoff.
func (s *Supervisor) Restart() {
	s.mu.Lock()
	p := s.current
	s.kick = true
	s.mu.Unlock()
	if p != nil {
		p.Stop()
	}
}

// Run supervises until ctx is cancelled (returns nil) or the bridge exits with
// ExitLoggedOut (returns ErrLoggedOut). A Starter error is treated like an
// immediate crash and retried with backoff. The process is stopped and reaped
// before Run returns.
func (s *Supervisor) Run(ctx context.Context) error {
	b := s.Backoff.withDefaults()
	log := s.logger()
	attempt := 0
	first := true
	for {
		if ctx.Err() != nil {
			return nil
		}
		started := s.now()
		proc, err := s.Start(ctx)
		var code int
		if err != nil {
			code = -1
			s.setExit(code, err)
			log.Warn("whatsapp bridge failed to start", "err", err)
		} else {
			s.mu.Lock()
			s.current = proc
			s.status.Running = true
			if !first {
				s.status.Restarts++
			}
			s.mu.Unlock()
			first = false
			// Cancellation stops the process; Wait below then returns.
			done := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					proc.Stop()
				case <-done:
				}
			}()
			var werr error
			code, werr = proc.Wait()
			close(done)
			s.mu.Lock()
			s.current = nil
			s.status.Running = false
			s.mu.Unlock()
			s.setExit(code, werr)
			if ctx.Err() != nil {
				return nil
			}
			if code == ExitLoggedOut {
				s.mu.Lock()
				s.status.LoggedOut = true
				s.mu.Unlock()
				log.Error("whatsapp bridge reports the session is logged out; not restarting")
				return ErrLoggedOut
			}
			log.Warn("whatsapp bridge exited", "code", code, "err", werr)
		}

		ranFor := s.now().Sub(started)
		if ranFor >= b.ResetAfter {
			attempt = 0
		}
		s.mu.Lock()
		kicked := s.kick
		s.kick = false
		s.mu.Unlock()
		if kicked {
			continue
		}
		d := b.delay(attempt)
		attempt++
		log.Info("restarting whatsapp bridge", "in", d.String(), "attempt", attempt)
		if err := s.sleep(ctx, d); err != nil {
			return nil
		}
	}
}

func (s *Supervisor) setExit(code int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastExit = code
	s.status.LastErr = ""
	if err != nil {
		s.status.LastErr = err.Error()
	}
}

// Launch describes how to start the real bridge process.
type Launch struct {
	Node      string   // node binary
	Dir       string   // install dir holding bridge.js (also the working dir)
	Args      []string // after bridge.js
	Env       []string // extra KEY=VALUE, appended to the daemon's environment
	Log       io.Writer
	StopGrace time.Duration // SIGTERM → SIGKILL delay; default 5s
}

// ExecStarter returns a Starter that runs `node bridge.js <args>` with
// stdout and stderr going to l.Log (never the terminal: the daemon's TUI owns
// it).
func ExecStarter(l Launch) Starter {
	grace := l.StopGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	return func(ctx context.Context) (Process, error) {
		pctx, cancel := context.WithCancel(ctx)
		cmd := exec.CommandContext(pctx, l.Node, append([]string{Script}, l.Args...)...)
		cmd.Dir = l.Dir
		cmd.Env = append(os.Environ(), l.Env...)
		cmd.Stdout, cmd.Stderr = l.Log, l.Log
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = grace
		if l.Log != nil {
			fmt.Fprintf(l.Log, "--- bridge start %s\n", time.Now().Format(time.RFC3339))
		}
		if err := cmd.Start(); err != nil {
			cancel()
			return nil, fmt.Errorf("start %s: %w", l.Node, err)
		}
		return &execProcess{cmd: cmd, cancel: cancel}, nil
	}
}

type execProcess struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

func (p *execProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	p.cancel()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), err
	}
	return -1, err
}

func (p *execProcess) Stop() { p.cancel() }

// OpenLog opens (creating directories as needed) the bridge log for append.
// A log over maxLogBytes is first rotated to <path>.1, so an always-on daemon
// cannot fill the disk.
func OpenLog(logPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("bridge: create log dir: %w", err)
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > maxLogBytes {
		_ = os.Rename(logPath, logPath+".1")
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("bridge: open log: %w", err)
	}
	return f, nil
}

const maxLogBytes = 5 << 20
