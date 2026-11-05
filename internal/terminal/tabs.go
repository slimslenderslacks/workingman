package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
)

// ForwardedEnv lists the environment variables TabLauncher passes from orch to
// the command it runs in a tab. A tab's process is a child of the terminal, not
// of orch, so it would otherwise see the terminal's environment: without these
// the wolf and `:session` windows lose the PATH orch augmented at startup and
// the private ssh-agent start.sh exports for commit signing.
var ForwardedEnv = []string{"PATH", "SSH_AUTH_SOCK"}

// AlreadyOpenError is returned by TabLauncher.Open when the session already has
// a live tab. Instead of opening a second one, Open tried to focus the existing
// tab; FocusErr is that attempt's error, nil when the switch worked.
type AlreadyOpenError struct {
	Name     string // session name
	Title    string // the tab's title
	FocusErr error
}

func (e *AlreadyOpenError) Error() string {
	if e.FocusErr != nil {
		return fmt.Sprintf("a tab for %q is already open (could not switch to it: %v)", e.Title, e.FocusErr)
	}
	return fmt.Sprintf("a tab for %q is already open; switched to it", e.Title)
}

// TabLauncher opens each session in a new terminal tab and tracks it until the
// command in the tab exits. It is the agent.Launcher for every non-ACP session
// (the --wolf-host wolf, the TUI's `:dir`/`:session` windows).
//
// Tracking is file-based so it survives orch restarts: the command is wrapped so
// it records its pid in <StateDir>/<name>.pid before exec'ing, and Open writes
// <StateDir>/<name>.json (title, terminal, tab id) as soon as the terminal has
// accepted the tab. A session is "open" while that pid is alive, or — for the
// moment between the terminal accepting the tab and the command starting —
// while the .json is younger than StartTimeout.
type TabLauncher struct {
	// Resolve picks the terminal. It is called each time a tab is needed (not at
	// startup), so a misconfiguration only fails the launch that needs a tab.
	// Defaults to Resolve with the real environment and settings file.
	Resolve func() (Launcher, error)
	// StateDir holds the per-session tracking files. Defaults to
	// ~/.workingman/tabs.
	StateDir string
	// Env is the environment forwarded into tabs. Defaults to the current values
	// of ForwardedEnv.
	Env []string
	// PollInterval is how often a session's Wait checks its pid. Defaults to
	// 500ms.
	PollInterval time.Duration
	// StartTimeout bounds how long a newly opened tab may take to start its
	// command before Wait gives up on it. Defaults to 1 minute.
	StartTimeout time.Duration

	mu sync.Mutex // serializes check-then-open so one session never gets two tabs
}

// tabState is the <name>.json record.
type tabState struct {
	Title    string    `json:"title"`
	Terminal string    `json:"terminal"`
	TabID    string    `json:"tab_id,omitempty"`
	OpenedAt time.Time `json:"opened_at"`
}

// Launch implements agent.Launcher. A session whose tab is already open is
// adopted rather than opened twice: the existing tab is focused and the
// returned Session tracks it (TabSession.Reused reports this).
func (l *TabLauncher) Launch(ctx context.Context, spec agent.Spec) (agent.Session, error) {
	sess, err := l.Open(ctx, spec)
	var already *AlreadyOpenError
	if errors.As(err, &already) {
		return sess, nil
	}
	return sess, err
}

// Open opens spec in a new tab titled spec.Title (spec.Name when empty). When
// the session's tab is already open it focuses that tab instead and returns its
// Session together with an *AlreadyOpenError, so the caller can warn the user.
func (l *TabLauncher) Open(ctx context.Context, spec agent.Spec) (*TabSession, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("terminal: spec.Name is required")
	}
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("terminal: spec.Command is required")
	}
	title := spec.Title
	if title == "" {
		title = spec.Name
	}
	dir, err := l.stateDir()
	if err != nil {
		return nil, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if st, ok := l.live(dir, spec.Name); ok {
		sess := l.session(dir, spec.Name, st.Title)
		sess.reused = true
		return sess, &AlreadyOpenError{Name: spec.Name, Title: st.Title, FocusErr: l.focus(ctx, st)}
	}

	term, err := l.resolve()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("terminal: state dir: %w", err)
	}
	pidPath := filepath.Join(dir, fileStem(spec.Name)+".pid")
	_ = os.Remove(pidPath)
	id, err := term.OpenTab(ctx, Tab{
		Title:   title,
		Dir:     spec.Workspace,
		Command: wrapCommand(pidPath, l.env(), spec.Command),
	})
	if err != nil {
		return nil, err
	}
	st := tabState{Title: title, Terminal: term.Name(), TabID: id, OpenedAt: time.Now()}
	if err := writeState(dir, spec.Name, st); err != nil {
		return nil, err
	}
	return l.session(dir, spec.Name, title), nil
}

// Focus brings the open tab of the named session to the front.
func (l *TabLauncher) Focus(ctx context.Context, name string) error {
	dir, err := l.stateDir()
	if err != nil {
		return err
	}
	st, ok := l.live(dir, name)
	if !ok {
		return fmt.Errorf("no open tab for %q: %w", name, ErrTabNotFound)
	}
	return l.focus(ctx, st)
}

func (l *TabLauncher) focus(ctx context.Context, st tabState) error {
	term, err := l.resolve()
	if err != nil {
		return err
	}
	return term.FocusTab(ctx, TabRef{ID: st.TabID, Title: st.Title})
}

func (l *TabLauncher) resolve() (Launcher, error) {
	if l.Resolve != nil {
		return l.Resolve()
	}
	return Resolve(os.Getenv, "")
}

func (l *TabLauncher) stateDir() (string, error) {
	if l.StateDir != "" {
		return l.StateDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("terminal: home dir: %w", err)
	}
	return filepath.Join(home, ".workingman", "tabs"), nil
}

func (l *TabLauncher) env() []string {
	if l.Env != nil {
		return l.Env
	}
	var env []string
	for _, k := range ForwardedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (l *TabLauncher) startTimeout() time.Duration {
	if l.StartTimeout > 0 {
		return l.StartTimeout
	}
	return time.Minute
}

func (l *TabLauncher) session(dir, name, title string) *TabSession {
	poll := l.PollInterval
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	return &TabSession{
		name:         name,
		title:        title,
		dir:          dir,
		poll:         poll,
		startTimeout: l.startTimeout(),
	}
}

// live reports whether the named session's tab is open, returning its state.
// Stale records (dead pid, or a tab that never started its command) are
// removed.
func (l *TabLauncher) live(dir, name string) (tabState, bool) {
	st, err := readState(dir, name)
	if err != nil {
		return tabState{}, false
	}
	switch pid, ok := readPid(dir, name); {
	case ok && pidAlive(pid):
		return st, true
	case !ok && time.Since(st.OpenedAt) < l.startTimeout():
		return st, true // accepted by the terminal, command not started yet
	}
	removeState(dir, name)
	return tabState{}, false
}

// wrapCommand runs command under `env` with the forwarded environment, after a
// small sh prologue that records the shell's pid in pidPath. The prologue
// exec's, so the recorded pid is the command's own: it lives exactly as long as
// the command does, which is how TabSession tells the tab is still open.
func wrapCommand(pidPath string, env, command []string) []string {
	argv := []string{"sh", "-c", `echo $$ > "$0.tmp" && mv "$0.tmp" "$0"; exec "$@"`, pidPath, "env"}
	argv = append(argv, env...)
	return append(argv, command...)
}

// TabSession is the agent.Session for a command running in a terminal tab.
type TabSession struct {
	name         string
	title        string
	dir          string
	poll         time.Duration
	startTimeout time.Duration
	reused       bool
	closeOnce    sync.Once
}

// Name is the session name; TabLauncher.Focus accepts it.
func (s *TabSession) Name() string { return s.name }

// Title is the tab's title.
func (s *TabSession) Title() string { return s.title }

// Reused reports whether the session adopted an already-open tab instead of
// opening a new one.
func (s *TabSession) Reused() bool { return s.reused }

// Wait blocks until the command in the tab exits (or ctx is done), then removes
// the session's tracking files. It fails if the tab never started its command
// within the start timeout — e.g. the user closed it at a shell prompt.
func (s *TabSession) Wait(ctx context.Context) error {
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	deadline := time.Now().Add(s.startTimeout)
	for {
		pid, ok := readPid(s.dir, s.name)
		if ok && !pidAlive(pid) {
			removeState(s.dir, s.name)
			return nil
		}
		if !ok && time.Now().After(deadline) {
			removeState(s.dir, s.name)
			return fmt.Errorf("terminal: tab %q never started its command", s.title)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Close ends the command in the tab with SIGHUP — what a terminal sends when
// its tab is closed — which closes the tab. Idempotent.
func (s *TabSession) Close() error {
	s.closeOnce.Do(func() {
		if pid, ok := readPid(s.dir, s.name); ok && pidAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGHUP)
		}
	})
	return nil
}

// fileStem maps a session name ("wolf-feat/x") to a file-name-safe stem.
func fileStem(name string) string {
	return url.PathEscape(name)
}

func readState(dir, name string) (tabState, error) {
	var st tabState
	data, err := os.ReadFile(filepath.Join(dir, fileStem(name)+".json"))
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}

func writeState(dir, name string, st tabState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, fileStem(name)+".json"), data, 0o644); err != nil {
		return fmt.Errorf("terminal: write tab state: %w", err)
	}
	return nil
}

func removeState(dir, name string) {
	_ = os.Remove(filepath.Join(dir, fileStem(name)+".json"))
	_ = os.Remove(filepath.Join(dir, fileStem(name)+".pid"))
}

func readPid(dir, name string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, fileStem(name)+".pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// pidAlive reports whether pid names a live process. EPERM still means it
// exists (it belongs to someone else).
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
