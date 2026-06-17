package tui

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// aliveProbe is a sandboxProbe that always reports the sandbox is up, so tests
// exercising the connection paths aren't short-circuited by the liveness check.
func aliveProbe(_ context.Context, _ string) (bool, error) { return true, nil }

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
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, aliveProbe, "go read it")
	defer stopWatch(cancel, ch)

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
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, aliveProbe, "")
	defer stopWatch(cancel, ch)

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
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, aliveProbe, "")
	defer stopWatch(cancel, ch)

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

// agentMessageFrame is one recorded ACP stream-log line carrying an assistant
// text chunk — what replayPriorContext decodes back into a streamed Event.
func agentMessageFrame(text string) string {
	return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + text + `"}}}}` + "\n"
}

// TestWatchReconnectReplaysAndSkipsPrompt models the reconnect-on-restart path: a
// session already prompted in a prior TUI run (PromptCount>0) with recorded
// output. The watcher must replay the prior prompt + output and must NOT re-send
// the opening prompt over the wire (which would restart the agent).
func TestWatchReconnectReplaysAndSkipsPrompt(t *testing.T) {
	root := t.TempDir()
	store := session.Store{Root: root}

	logPath := filepath.Join(store.Dir("task-resume"), "stream.log")
	if err := os.MkdirAll(store.Dir("task-resume"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(agentMessageFrame("prior output")), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := session.Session{
		ID:          "task-resume",
		SandboxName: "task-resume",
		Status:      session.StatusRunning,
		CreatedAt:   time.Now(),
		SocketPath:  store.SocketPath("task-resume"),
		Workspaces:  []string{"/work/task-resume"},
		LogPath:     logPath,
		PromptCount: 1,
	}
	if err := store.Write(rec); err != nil {
		t.Fatalf("write session: %v", err)
	}

	conn := newFakeACPConn()
	dial := func(_ context.Context, _ string) (acpConn, error) { return conn, nil }

	ctx, cancel := context.WithCancel(context.Background())
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, aliveProbe, "go read it")
	defer stopWatch(cancel, ch)

	var (
		sawPrompt, sawReplay, sawConnected bool
		promptText, replayText             string
	)
	deadline := time.After(2 * time.Second)
	for !(sawPrompt && sawReplay && sawConnected) {
		select {
		case ev := <-ch:
			switch ev.kind {
			case acpTabPrompt:
				sawPrompt = true
				promptText = ev.text
			case acpTabStream:
				switch ev.ev.State {
				case acpclient.StateStreaming:
					if ev.ev.Text != "" {
						sawReplay = true
						replayText = ev.ev.Text
					}
				case acpclient.StateConnected:
					sawConnected = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out; prompt=%v replay=%v connected=%v", sawPrompt, sawReplay, sawConnected)
		}
	}

	if promptText != "go read it" {
		t.Errorf("replayed prompt = %q, want %q", promptText, "go read it")
	}
	if replayText != "prior output" {
		t.Errorf("replayed output = %q, want %q", replayText, "prior output")
	}
	conn.mu.Lock()
	prompts := append([]string(nil), conn.promptsSent...)
	conn.mu.Unlock()
	if len(prompts) != 0 {
		t.Errorf("reconnect re-sent the opening prompt over the wire: %v", prompts)
	}
}

// TestWatchDialFailureCleansUpDeadSession asserts a session whose socket won't
// dial (sandbox gone) is routed to cleanup: its leftover directory is removed and
// the tab is dropped via acpTabRemoved.
func TestWatchDialFailureCleansUpDeadSession(t *testing.T) {
	root := t.TempDir()
	store := writeRunningSession(t, root, "task-dead")

	dial := func(_ context.Context, _ string) (acpConn, error) {
		return nil, context.DeadlineExceeded // any dial error: nothing is listening
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, aliveProbe, "")
	defer stopWatch(cancel, ch)

	ev := waitForKind(t, ch, acpTabRemoved, 2*time.Second)
	if ev.id != "task-dead" {
		t.Errorf("removed id = %q, want task-dead", ev.id)
	}
	// The orphaned directory must be reclaimed.
	waitForDirGone(t, store.Dir("task-dead"), 2*time.Second)
}

// waitForDirGone polls until dir no longer exists or the timeout elapses.
func waitForDirGone(t *testing.T, dir string, dur time.Duration) {
	t.Helper()
	deadline := time.After(dur)
	for {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return // cleaned up
		}
		select {
		case <-deadline:
			t.Fatalf("directory %s was not removed", dir)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestWatchSandboxGoneCleansUpStaleSession asserts that when a discovered
// session's backing sandbox is gone (the probe reports it absent), the watcher
// reaps the stale directory and drops the tab WITHOUT ever dialing the leftover
// socket — the socket can't be trusted once the sandbox is gone, and dialing a
// lingering one could hang with no per-dial timeout.
func TestWatchSandboxGoneCleansUpStaleSession(t *testing.T) {
	root := t.TempDir()
	store := writeRunningSession(t, root, "task-orphan")

	var dialed int32
	dial := func(_ context.Context, _ string) (acpConn, error) {
		atomic.AddInt32(&dialed, 1)
		return newFakeACPConn(), nil // would "succeed" — but must never be called
	}
	goneProbe := func(_ context.Context, _ string) (bool, error) { return false, nil }

	ctx, cancel := context.WithCancel(context.Background())
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, goneProbe, "go read it")
	defer stopWatch(cancel, ch)

	// The stale tab is marked dead and dropped.
	var sawDisconnected bool
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch {
			case ev.kind == acpTabStream && ev.ev.State == acpclient.StateDisconnected:
				sawDisconnected = true
			case ev.kind == acpTabRemoved && ev.id == "task-orphan":
				if !sawDisconnected {
					t.Error("tab removed without first marking it disconnected")
				}
				if n := atomic.LoadInt32(&dialed); n != 0 {
					t.Errorf("dialed a stale socket whose sandbox is gone (%d times)", n)
				}
				// The orphaned directory must be reclaimed.
				waitForDirGone(t, store.Dir("task-orphan"), 2*time.Second)
				return
			}
		case <-deadline:
			t.Fatal("sandbox-gone session was never cleaned up")
		}
	}
}

// TestWatchSandboxProbeErrorFallsThroughToConnection asserts an inconclusive
// probe (sbx unavailable) does NOT reap the session: the watcher falls through
// to dialing and lets the connection's own behavior decide liveness, so a flaky
// `sbx ls` can never tear down a session whose socket is actually live.
func TestWatchSandboxProbeErrorFallsThroughToConnection(t *testing.T) {
	root := t.TempDir()
	writeRunningSession(t, root, "task-probeerr")

	conn := newFakeACPConn()
	var dialed int32
	dial := func(_ context.Context, _ string) (acpConn, error) {
		atomic.AddInt32(&dialed, 1)
		return conn, nil
	}
	errProbe := func(_ context.Context, _ string) (bool, error) {
		return false, context.DeadlineExceeded // inconclusive: probe couldn't run
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, errProbe, "go read it")
	defer stopWatch(cancel, ch)

	// A live session connects and streams despite the probe error.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.kind == acpTabStream && ev.ev.State == acpclient.StateConnected {
				if n := atomic.LoadInt32(&dialed); n == 0 {
					t.Error("probe error short-circuited the dial instead of falling through")
				}
				return
			}
			if ev.kind == acpTabRemoved {
				t.Fatal("inconclusive probe wrongly reaped a live session")
			}
		case <-deadline:
			t.Fatal("session never connected after an inconclusive probe")
		}
	}
}

// TestWatchNewSessionMarksPrompted asserts that after the watcher drives a fresh
// session's opening prompt it records PromptCount in session.json, so a later
// restart reconnects-and-replays instead of re-prompting.
func TestWatchNewSessionMarksPrompted(t *testing.T) {
	root := t.TempDir()
	store := writeRunningSession(t, root, "task-fresh")

	conn := newFakeACPConn()
	dial := func(_ context.Context, _ string) (acpConn, error) { return conn, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watchACPSessions(ctx, root, 10*time.Millisecond, dial, aliveProbe, "go read it")
	go func() {
		for range ch { // drain so the watcher never blocks emitting
		}
	}()

	deadline := time.After(2 * time.Second)
	for {
		rec, err := store.Read("task-fresh")
		if err == nil && rec.PromptCount == 1 {
			return // marked
		}
		select {
		case <-deadline:
			t.Fatalf("PromptCount never reached 1 (err=%v, count=%d)", err, rec.PromptCount)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// stopWatch shuts the watcher down deterministically: it cancels the context and
// then drains the event channel until it closes. watchACPSessions closes the
// channel only after wg.Wait() returns — i.e. after every per-session goroutine
// has finished, including any trailing markPrompted store.Write. Waiting for that
// close before the test returns keeps those late writes from racing t.TempDir's
// RemoveAll cleanup (which otherwise fails with "directory not empty").
func stopWatch(cancel context.CancelFunc, ch <-chan acpTabEvent) {
	cancel()
	for range ch {
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
