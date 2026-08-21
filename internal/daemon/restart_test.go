package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
)

// selfClosingStubSession mimics the one behavior that matters about
// processSession (the real ACP-backed Session): Wait itself closes the
// session — sends the SIGTERM-equivalent — the instant its ctx is cancelled,
// not only when Close is called directly. stubSession (used elsewhere in this
// package) doesn't reproduce that, so it can't catch a regression where
// trackSession hands a detachable session's wait goroutine the daemon's own
// shutdown ctx.
type selfClosingStubSession struct {
	name string
	done chan struct{}

	mu     sync.Mutex
	closed bool
}

func newSelfClosingStubSession(name string) *selfClosingStubSession {
	return &selfClosingStubSession{name: name, done: make(chan struct{})}
}

func (s *selfClosingStubSession) Name() string { return s.name }

func (s *selfClosingStubSession) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	}
}

func (s *selfClosingStubSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

func (s *selfClosingStubSession) wasClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// TestRestartDoesNotCloseDetachableSession simulates an orch restart (the
// context passed to Run is cancelled) for an ACP-backed session and asserts
// the session is never closed — matching processSession.Close's real-world
// effect of sending SIGTERM to acp-wrapper, which would tear down the sandbox
// the whole point of detaching is to leave running.
//
// Closing over shutdown() alone isn't enough to prove this: trackSession's
// own wait goroutine calls sess.Wait(ctx) independently, and a Session like
// processSession that closes itself on ctx.Done() would undo shutdown()'s
// detach regardless of what shutdown() itself does. This test exercises that
// whole path: trackSession's wait goroutine plus shutdown().
func TestRestartDoesNotCloseDetachableSession(t *testing.T) {
	d, cancel := newTestDaemon(t)
	// A non-nil AcpLauncher makes detachable(agent.TaskAgent) true — see
	// Daemon.detachable / Runner.UsesACP.
	d.runner = &runner.Runner{AcpLauncher: stubLauncher{}}

	key := filepath.Join(t.TempDir(), "alpha", ".project.yaml")
	s := newSelfClosingStubSession("orch-task-alpha")
	if !d.trackSession(key, s, agent.TaskAgent, "", nil) {
		t.Fatal("trackSession returned false")
	}

	// Simulate Run()'s shutdown path: cancel the ctx sessions wait under,
	// then run shutdown() exactly as Run's defer does.
	cancel()
	d.shutdown()

	// Give the wait goroutine a chance to react if it's going to.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !s.wasClosed() {
		time.Sleep(10 * time.Millisecond)
	}

	if s.wasClosed() {
		t.Fatal("detachable session was closed on restart — a real acp-wrapper would have been sent SIGTERM, killing the reconnectable session")
	}
	if d.hasSession(key) {
		t.Error("expected session to be detached (dropped from local tracking) after shutdown")
	}

	s.Close() // drain the leaked wait goroutine
}

// TestReconcileSessionsDiscoversOnDiskSession asserts that a freshly started
// daemon adopts an ACP session that was already on disk (written by a prior
// orch process's acp-wrapper) before the new daemon's own startup scan runs —
// so it neither loses track of the session nor dispatches a duplicate agent
// for the same project.
func TestReconcileSessionsDiscoversOnDiskSession(t *testing.T) {
	root := t.TempDir()
	sessionsRoot := t.TempDir()

	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Description: "test",
		Branch:      "feat/x",
		Status:      project.StatusReady,
	}
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	store, err := session.NewStore(sessionsRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	now := time.Now()
	rec := session.Session{
		ID:          "orch-planning-alpha",
		SandboxName: "acp-planning-alpha",
		Status:      session.StatusRunning,
		CreatedAt:   now,
		UpdatedAt:   now,
		ProjectPath: projectPath,
		Kind:        "planning",
	}
	if err := store.Write(rec); err != nil {
		t.Fatalf("Write session record: %v", err)
	}

	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{AcpLauncher: stubLauncher{}, SessionsRoot: sessionsRoot}
	d, err := New([]string{root}, a, WithRunner(r))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	if ok, snap := waitFor(t, buf, "session_reconciled"); !ok {
		t.Fatalf("daemon never reconciled the on-disk session.\naudit:\n%s", snap)
	}

	if !d.hasSession(projectPath) {
		t.Fatal("reconciled session not tracked under the project path")
	}
	got := d.ListSessions()
	if len(got) != 1 || got[0].AgentName != "planning" {
		t.Fatalf("ListSessions after reconcile = %+v", got)
	}

	// startupScan runs right after reconcileSessions and would otherwise
	// dispatch a planning agent for this status:ready project — it must see
	// hasSession(projectPath) == true and skip instead of launching a
	// duplicate (which would show up as an acp/session_started audit entry).
	if ok, snap := waitFor(t, buf, "session_skip_duplicate"); !ok {
		t.Fatalf("startupScan did not dedup against the reconciled session.\naudit:\n%s", snap)
	}
	if strings.Contains(buf.String(), "session_started") {
		t.Errorf("a duplicate agent was dispatched despite the reconciled session.\naudit:\n%s", buf.String())
	}
}
