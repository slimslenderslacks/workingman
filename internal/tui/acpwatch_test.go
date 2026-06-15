package tui

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/acpclient"
	"github.com/slimslenderslacks/work/internal/session"
)

// fakeACPConn is an in-memory acpConn: Connect/Prompt push lifecycle and stream
// events onto an Events() channel, and Close closes it so the watcher's pump
// drains. It stands in for a real acpclient.Client over a socket.
type fakeACPConn struct {
	events     chan acpclient.Event
	connectErr error

	mu          sync.Mutex
	closed      bool
	connectCWD  string
	promptsSent []string
}

func newFakeACPConn() *fakeACPConn {
	return &fakeACPConn{events: make(chan acpclient.Event, 16)}
}

func (f *fakeACPConn) Connect(_ context.Context, cwd string) error {
	if f.connectErr != nil {
		return f.connectErr
	}
	f.mu.Lock()
	f.connectCWD = cwd
	f.mu.Unlock()
	f.events <- acpclient.Event{State: acpclient.StateConnected}
	return nil
}

func (f *fakeACPConn) Prompt(_ context.Context, text string) (string, error) {
	f.mu.Lock()
	f.promptsSent = append(f.promptsSent, text)
	f.mu.Unlock()
	f.events <- acpclient.Event{State: acpclient.StateStreaming}
	f.events <- acpclient.Event{State: acpclient.StateStreaming, Text: "hi from agent"}
	f.events <- acpclient.Event{State: acpclient.StateCompleted, StopReason: "end_turn"}
	return "end_turn", nil
}

func (f *fakeACPConn) Events() <-chan acpclient.Event { return f.events }

func (f *fakeACPConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}

// writeRunningSession persists a StatusRunning session.json under root so the
// watcher discovers it.
func writeRunningSession(t *testing.T, root, id string) session.Store {
	t.Helper()
	store := session.Store{Root: root}
	rec := session.Session{
		ID:          id,
		SandboxName: id,
		Status:      session.StatusRunning,
		CreatedAt:   time.Now(),
		SocketPath:  store.SocketPath(id),
		Workspaces:  []string{"/work/" + id},
	}
	if err := store.Write(rec); err != nil {
		t.Fatalf("write session %s: %v", id, err)
	}
	return store
}

func TestWatchDiscoversSessionAndStreamsTranscript(t *testing.T) {
	root := t.TempDir()
	writeRunningSession(t, root, "task-one")

	conn := newFakeACPConn()
	dial := func(_ context.Context, _ string) (acpConn, error) { return conn, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, "go read it")

	// Collect events until we've seen the added tab, the prompt, and the
	// completed stream event.
	var (
		sawAdded, sawPrompt, sawCompleted bool
		gotPromptText                     string
	)
	deadline := time.After(2 * time.Second)
	for !(sawAdded && sawPrompt && sawCompleted) {
		select {
		case ev := <-ch:
			switch ev.kind {
			case acpTabAdded:
				if ev.id == "task-one" {
					sawAdded = true
				}
			case acpTabPrompt:
				sawPrompt = true
				gotPromptText = ev.text
			case acpTabStream:
				if ev.ev.State == acpclient.StateCompleted {
					sawCompleted = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out; added=%v prompt=%v completed=%v", sawAdded, sawPrompt, sawCompleted)
		}
	}

	if gotPromptText != "go read it" {
		t.Errorf("prompt text = %q, want %q", gotPromptText, "go read it")
	}
	conn.mu.Lock()
	cwd := conn.connectCWD
	prompts := append([]string(nil), conn.promptsSent...)
	conn.mu.Unlock()
	if cwd != "/work/task-one" {
		t.Errorf("Connect cwd = %q, want the session's first workspace", cwd)
	}
	if len(prompts) != 1 || prompts[0] != "go read it" {
		t.Errorf("prompts sent = %v, want [\"go read it\"]", prompts)
	}
}

func TestWatchEmitsRemovedWhenSessionDirGone(t *testing.T) {
	root := t.TempDir()
	store := writeRunningSession(t, root, "task-gone")

	conn := newFakeACPConn()
	dial := func(_ context.Context, _ string) (acpConn, error) { return conn, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, "")

	// Wait for the tab to be added, then delete the session directory.
	waitForKind(t, ch, acpTabAdded, 2*time.Second)
	if err := store.Remove("task-gone"); err != nil {
		t.Fatalf("remove session: %v", err)
	}

	ev := waitForKind(t, ch, acpTabRemoved, 2*time.Second)
	if ev.id != "task-gone" {
		t.Errorf("removed id = %q, want task-gone", ev.id)
	}
}

func TestWatchDialFailureMarksDisconnected(t *testing.T) {
	root := t.TempDir()
	writeRunningSession(t, root, "task-stale")

	dial := func(_ context.Context, _ string) (acpConn, error) {
		return nil, context.DeadlineExceeded // any dial error
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, "")

	waitForKind(t, ch, acpTabAdded, 2*time.Second)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.kind == acpTabStream && ev.ev.State == acpclient.StateDisconnected {
				return // success: a dead socket surfaces as a disconnected tab
			}
		case <-deadline:
			t.Fatal("dial failure never surfaced a disconnected stream event")
		}
	}
}

// waitForKind drains ch until an event of the given kind arrives or the timeout
// elapses.
func waitForKind(t *testing.T, ch <-chan acpTabEvent, kind acpEventKind, dur time.Duration) acpTabEvent {
	t.Helper()
	deadline := time.After(dur)
	for {
		select {
		case ev := <-ch:
			if ev.kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event kind %d", kind)
			return acpTabEvent{}
		}
	}
}
