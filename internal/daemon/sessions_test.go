package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
)

// stubSession is a controllable agent.Session for tests. Wait blocks until
// Close (or ctx cancellation). It avoids any tmux/process dependency so the
// session-data tests can run unit-fast on every platform.
type stubSession struct {
	name string
	done chan struct{}
}

func newStubSession(name string) *stubSession {
	return &stubSession{name: name, done: make(chan struct{})}
}

func (s *stubSession) Name() string { return s.name }

func (s *stubSession) Wait(ctx context.Context) error {
	if ctx == nil {
		<-s.done
		return nil
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *stubSession) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

// newTestDaemon spins up a Daemon ready for direct trackSession calls. It
// never enters Run() — the test drives sessions in-memory, so wiring d.ctx
// here is enough for trackSession's wait goroutine.
func newTestDaemon(t *testing.T) (*Daemon, context.CancelFunc) {
	t.Helper()
	buf := &safeBuf{}
	d, err := New([]string{t.TempDir()}, audit.New(buf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	t.Cleanup(func() {
		cancel()
		_ = d.watcher.Close()
	})
	return d, cancel
}

func TestListSessionsReturnsTrackedSessions(t *testing.T) {
	d, _ := newTestDaemon(t)

	keyA := filepath.Join(t.TempDir(), "alpha", ".project.yaml")
	keyB := filepath.Join(t.TempDir(), "bravo", ".project.yaml")

	sa := newStubSession("orch-task-alpha")
	if !d.trackSession(keyA, sa, agent.TaskAgent, "alpha-task", nil) {
		t.Fatal("trackSession A returned false")
	}
	// Force distinct StartedAt so ordering is deterministic.
	time.Sleep(2 * time.Millisecond)
	sb := newStubSession("orch-project-bravo")
	if !d.trackSession(keyB, sb, agent.ProjectAgent, "", nil) {
		t.Fatal("trackSession B returned false")
	}

	got := d.ListSessions()
	if len(got) != 2 {
		t.Fatalf("ListSessions len = %d, want 2: %+v", len(got), got)
	}
	if got[0].ID != keyA {
		t.Errorf("got[0].ID = %q, want %q (older session first)", got[0].ID, keyA)
	}
	if got[0].AgentName != "task" {
		t.Errorf("got[0].AgentName = %q, want %q", got[0].AgentName, "task")
	}
	if got[0].Project != "alpha" {
		t.Errorf("got[0].Project = %q, want %q", got[0].Project, "alpha")
	}
	if got[0].TmuxTarget != "orch-task-alpha" {
		t.Errorf("got[0].TmuxTarget = %q", got[0].TmuxTarget)
	}
	if got[0].Status != SessionStatusRunning {
		t.Errorf("got[0].Status = %q, want %q", got[0].Status, SessionStatusRunning)
	}
	if got[0].StartedAt.IsZero() {
		t.Errorf("got[0].StartedAt is zero")
	}
	if !got[0].StartedAt.Before(got[1].StartedAt) {
		t.Errorf("StartedAt not strictly increasing: %v vs %v",
			got[0].StartedAt, got[1].StartedAt)
	}
	if got[1].AgentName != "project" {
		t.Errorf("got[1].AgentName = %q, want %q", got[1].AgentName, "project")
	}

	// Cleanly drain the wait goroutines so the test exits without leaking.
	sa.Close()
	sb.Close()
}

func TestListSessionsRemovesEndedSessions(t *testing.T) {
	d, _ := newTestDaemon(t)
	key := filepath.Join(t.TempDir(), "alpha", ".project.yaml")
	s := newStubSession("orch-task-alpha")
	if !d.trackSession(key, s, agent.TaskAgent, "", nil) {
		t.Fatal("trackSession returned false")
	}
	if got := d.ListSessions(); len(got) != 1 {
		t.Fatalf("before end: len = %d, want 1: %+v", len(got), got)
	}
	s.Close()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(d.ListSessions()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session never removed after Close: %+v", d.ListSessions())
}

func TestWatchSessionsEmitsUpdates(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := d.WatchSessions(ctx, 10*time.Millisecond)

	select {
	case snap := <-ch:
		if len(snap) != 0 {
			t.Fatalf("initial snapshot non-empty: %+v", snap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no initial snapshot")
	}

	key := filepath.Join(t.TempDir(), "alpha", ".project.yaml")
	s := newStubSession("orch-task-alpha")
	if !d.trackSession(key, s, agent.TaskAgent, "", nil) {
		t.Fatal("trackSession returned false")
	}

	select {
	case snap := <-ch:
		if len(snap) != 1 || snap[0].AgentName != "task" || snap[0].Project != "alpha" {
			t.Fatalf("after-track snapshot: %+v", snap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot after trackSession")
	}

	s.Close()

	select {
	case snap := <-ch:
		if len(snap) != 0 {
			t.Fatalf("after-end snapshot non-empty: %+v", snap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot after session end")
	}
}

func TestWatchSessionsClosesOnCancel(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := d.WatchSessions(ctx, 10*time.Millisecond)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no initial snapshot")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// A late snapshot is allowed, but the channel must close shortly.
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("channel did not close after cancel")
				}
			case <-time.After(time.Second):
				t.Fatal("channel did not close after cancel")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close after cancel")
	}
}
