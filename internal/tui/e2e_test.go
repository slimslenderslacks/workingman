package tui

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/daemon"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// TestEndToEndDaemonAndTUI is the smoke test the orchestrator's slice-4 plan
// asks for: a daemon, a fake agent (the e2eFakeLauncher), a fake project
// dropped on disk, and a headless bubbletea program wired to the daemon's
// live snapshots. It asserts that the sessions and projects panes both
// render the right content and that pressing Enter dispatches a tmux-attach
// command with the correct target name — without needing tmux or a TTY.
func TestEndToEndDaemonAndTUI(t *testing.T) {
	root := t.TempDir()
	stubRoot := t.TempDir()

	auditBuf := &e2eSafeBuf{}
	a := audit.New(auditBuf)
	launcher := &e2eFakeLauncher{}
	t.Cleanup(launcher.shutdown)

	r := &runner.Runner{
		Workspaces: workspace.NewStub(stubRoot),
		Launcher:   launcher,
		Audit:      a,
		// Runner asks the builder for a command even though the fake launcher
		// ignores it; return something harmless so the call doesn't panic.
		Command: func(_ agent.Kind, _ string) []string { return []string{"true"} },
	}
	d, err := daemon.New([]string{root}, a, daemon.WithRunner(r))
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	daemonDone := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(daemonDone)
	}()
	t.Cleanup(func() {
		cancel()
		launcher.shutdown()
		<-daemonDone
	})

	if err := waitForString(auditBuf, "watch_root", 2*time.Second); err != nil {
		t.Fatalf("daemon never logged watch_root:\n%s", auditBuf.String())
	}

	// Drop a populated, status:ready project. The daemon dispatches a
	// planning agent for that state, which the fake launcher captures.
	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Description: "smoke",
		Branch:      "feat/smoke",
		Status:      project.StatusReady,
		Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
	}
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		t.Fatalf("save project: %v", err)
	}

	if err := waitForString(auditBuf, "session_started", 3*time.Second); err != nil {
		t.Fatalf("session never started:\n%s", auditBuf.String())
	}

	// Wire the daemon's live state into the TUI the same way cmd/orch does.
	sessions := adaptDaemonSessions(ctx, d.WatchSessions(ctx, 50*time.Millisecond))
	projects := WatchProjects(ctx, []string{root}, 50*time.Millisecond)

	att := &e2eAttacher{}

	// Run the TUI fully headless: nil input means the program never reads
	// from a terminal, and the renderer writes its frames into outBuf so the
	// test can assert on pane content. tea.Send delivers messages directly.
	outBuf := &e2eSafeBuf{}
	tuiCtx, tuiCancel := context.WithCancel(ctx)
	defer tuiCancel()

	prog := tea.NewProgram(
		newModel(projects, sessions, nil, att),
		tea.WithInput(nil),
		tea.WithOutput(outBuf),
		tea.WithContext(tuiCtx),
		tea.WithoutSignalHandler(),
	)

	progDone := make(chan error, 1)
	go func() {
		_, err := prog.Run()
		progDone <- err
	}()
	t.Cleanup(func() {
		prog.Quit()
		select {
		case <-progDone:
		case <-time.After(3 * time.Second):
		}
	})

	// Force a known size so View() actually paints something.
	prog.Send(tea.WindowSizeMsg{Width: 120, Height: 40})

	// The project basename comes from t.TempDir; the session row shows the
	// kind ("planning") fed by the fake launcher.
	projectName := filepath.Base(root)
	if err := waitForOutput(outBuf, []string{projectName, "planning"}, 4*time.Second); err != nil {
		t.Fatalf("output never contained expected fields: %v\n--- audit ---\n%s\n--- output ---\n%s",
			err, auditBuf.String(), outBuf.String())
	}

	// Press Enter: the sessions pane is the default focus, so this triggers
	// the attach code path. The stubbed attacher records the target name
	// without touching tmux or os/exec.
	prog.Send(tea.KeyMsg{Type: tea.KeyEnter})

	if err := waitForCondition(2*time.Second, func() bool { return att.count() >= 1 }); err != nil {
		t.Fatalf("attacher never invoked:\n--- audit ---\n%s\n--- output ---\n%s",
			auditBuf.String(), outBuf.String())
	}

	// runner.sessionName produces "<kind>-<branch>"; the real TmuxLauncher
	// prepends the umbrella session, giving the canonical "session:window"
	// target. The e2eFakeLauncher mirrors that so the TUI's click-to-attach
	// path is exercised end-to-end.
	if got, want := att.firstTarget(), agent.DefaultUmbrellaSession+":planning-feat/smoke"; got != want {
		t.Errorf("attach target = %q, want %q", got, want)
	}
}

// e2eFakeSession is a controllable agent.Session for the e2e smoke test.
// Wait blocks until Close (or ctx cancellation) so the daemon's per-session
// goroutine keeps the entry alive until the test tears down.
type e2eFakeSession struct {
	name string
	done chan struct{}
}

func newE2EFakeSession(name string) *e2eFakeSession {
	return &e2eFakeSession{name: name, done: make(chan struct{})}
}

func (s *e2eFakeSession) Name() string { return s.name }

func (s *e2eFakeSession) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *e2eFakeSession) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

// e2eFakeLauncher stands in for agent.TmuxLauncher in the e2e test. It hands
// the daemon back a session it can track without ever shelling out, and
// retains the live sessions so the test cleanup can release them.
type e2eFakeLauncher struct {
	mu       sync.Mutex
	sessions []*e2eFakeSession
}

func (l *e2eFakeLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	// Mirror the real TmuxLauncher's Name format ("session:window") so the
	// TUI's TmuxTarget plumbing exercises the same shape it sees in prod.
	s := newE2EFakeSession(agent.DefaultUmbrellaSession + ":" + spec.Name)
	l.mu.Lock()
	l.sessions = append(l.sessions, s)
	l.mu.Unlock()
	return s, nil
}

func (l *e2eFakeLauncher) shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.sessions {
		_ = s.Close()
	}
}

// e2eAttacher is a thread-safe attacher used by the e2e test. The unit-test
// fakeAttacher in tmux_attach_test.go doesn't lock because its callers run
// synchronously; here the attach call comes from the tea event-loop
// goroutine while the test reads in its own goroutine, so we need a mutex
// to avoid a data race.
type e2eAttacher struct {
	mu      sync.Mutex
	targets []string
}

func (a *e2eAttacher) Attach(target string) tea.Cmd {
	a.mu.Lock()
	a.targets = append(a.targets, target)
	a.mu.Unlock()
	return func() tea.Msg { return attachResultMsg{target: target} }
}

func (a *e2eAttacher) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.targets)
}

func (a *e2eAttacher) firstTarget() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.targets) == 0 {
		return ""
	}
	return a.targets[0]
}

// e2eSafeBuf is a concurrency-safe buffer for the audit log and the
// bubbletea renderer's output, both of which write from background
// goroutines while the test reads in the foreground.
type e2eSafeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *e2eSafeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *e2eSafeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// adaptDaemonSessions mirrors cmd/orch/main.go's adapter so the test exercises
// the same channel-shape conversion the production binary does — keeping the
// two in sync is the whole point of an end-to-end test.
func adaptDaemonSessions(ctx context.Context, in <-chan []daemon.SessionInfo) <-chan []SessionView {
	out := make(chan []SessionView)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case infos, ok := <-in:
				if !ok {
					return
				}
				views := make([]SessionView, len(infos))
				for i, s := range infos {
					views[i] = SessionView{
						ID:          s.ID,
						AgentName:   s.AgentName,
						Project:     s.Project,
						TmuxTarget:  s.TmuxTarget,
						Status:      string(s.Status),
						StartedAt:   s.StartedAt,
						TaskName:    s.TaskName,
						Interactive: s.Interactive,
						SandboxName: s.SandboxName,
					}
				}
				select {
				case out <- views:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func waitForString(buf *e2eSafeBuf, want string, dur time.Duration) error {
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("string %q not seen within %s", want, dur)
}

func waitForOutput(buf *e2eSafeBuf, wants []string, dur time.Duration) error {
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		s := buf.String()
		ok := true
		for _, w := range wants {
			if !strings.Contains(s, w) {
				ok = false
				break
			}
		}
		if ok {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("output missing one of %v within %s", wants, dur)
}

func waitForCondition(dur time.Duration, pred func() bool) error {
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		if pred() {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("condition not met within %s", dur)
}
