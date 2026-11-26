package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/acpchat"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
)

// fakePrimeConv is a PrimeConversation that records what the primer does.
// Ask blocks until release is closed (the end of the turn), unless release is
// nil.
type fakePrimeConv struct {
	mu      sync.Mutex
	asks    []string
	closed  bool
	asked   chan struct{} // closed on the first Ask
	release chan struct{}
	done    chan struct{} // closed on the first Close
	created bool
}

func newFakePrimeConv() *fakePrimeConv {
	return &fakePrimeConv{asked: make(chan struct{}), done: make(chan struct{})}
}

func (c *fakePrimeConv) Ask(ctx context.Context, text string, _ ...acpchat.AskOption) (string, error) {
	c.mu.Lock()
	c.asks = append(c.asks, text)
	if len(c.asks) == 1 {
		close(c.asked)
	}
	release := c.release
	c.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-c.done:
			return "", acpchat.ErrClosed
		}
	}
	return "ok", nil
}

func (c *fakePrimeConv) Created() bool { return c.created }

func (c *fakePrimeConv) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	return nil
}

func (c *fakePrimeConv) state() (asks []string, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asks...), c.closed
}

// primeEnv is a daemon wired for priming tests: an ACP runner over a temp
// sessions root and an attach seam that hands out conv.
type primeEnv struct {
	d       *Daemon
	buf     *safeBuf
	store   session.Store
	conv    *fakePrimeConv
	attachN int
	mu      sync.Mutex
}

func newPrimeEnv(t *testing.T) *primeEnv {
	t.Helper()
	buf := &safeBuf{}
	root := filepath.Join(t.TempDir(), "sessions")
	d, err := newDaemon(t, []string{t.TempDir()}, audit.New(buf),
		WithRunner(&runner.Runner{AcpLauncher: stubLauncher{}, SessionsRoot: root}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	e := &primeEnv{d: d, buf: buf, store: session.Store{Root: root}, conv: newFakePrimeConv()}
	d.primePoll = 5 * time.Millisecond
	d.primeAttach = func(ctx context.Context, ref string, opts acpchat.Options) (PrimeConversation, error) {
		e.mu.Lock()
		e.attachN++
		e.mu.Unlock()
		if !opts.CreateIfMissing {
			t.Errorf("attach without CreateIfMissing")
		}
		if ref != e.store.Dir(filepath.Base(ref)) {
			t.Errorf("attach ref = %q, want a session dir under %s", ref, root)
		}
		return e.conv, nil
	}
	t.Cleanup(func() {
		cancel()
		d.endHandlers()
	})
	return e
}

func (e *primeEnv) attaches() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.attachN
}

func (e *primeEnv) writeSession(t *testing.T, id string, kind agent.Kind, status session.Status, prompts int) {
	t.Helper()
	if err := e.store.Write(session.Session{
		ID: id, Kind: kind.String(), Status: status, PromptCount: prompts, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("write session: %v", err)
	}
}

func (e *primeEnv) promptCount(t *testing.T, id string) int {
	t.Helper()
	rec, err := e.store.Read(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	return rec.PromptCount
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPrimeSendsOpeningPromptOnce: a prompt_count==0 session gets the opening
// prompt exactly once, prompt_count goes to 1, and the primer waits for the
// wrapper to mark the session running before it attaches.
func TestPrimeSendsOpeningPromptOnce(t *testing.T) {
	e := newPrimeEnv(t)
	const id = "orch-task-alpha"
	e.writeSession(t, id, agent.TaskAgent, session.StatusStarting, 0)

	key := filepath.Join(t.TempDir(), "alpha", "project.yaml")
	s := newStubSession(id)
	defer s.Close()
	if !e.d.trackSession(key, s, agent.TaskAgent, "alpha-task", nil) {
		t.Fatal("trackSession returned false")
	}

	time.Sleep(30 * time.Millisecond)
	if n := e.attaches(); n != 0 {
		t.Fatalf("attached %d times while the session was still starting", n)
	}
	e.writeSession(t, id, agent.TaskAgent, session.StatusRunning, 0)

	waitUntil(t, "end-of-turn disconnect", func() bool { _, closed := e.conv.state(); return closed })
	e.d.endHandlers()

	asks, _ := e.conv.state()
	if len(asks) != 1 || asks[0] != runner.OpeningPrompt {
		t.Fatalf("asks = %q, want exactly [%q]", asks, runner.OpeningPrompt)
	}
	if n := e.attaches(); n != 1 {
		t.Errorf("attaches = %d, want 1", n)
	}
	if got := e.promptCount(t, id); got != 1 {
		t.Errorf("prompt_count = %d, want 1", got)
	}
	if !strings.Contains(e.buf.String(), "session_primed") {
		t.Errorf("audit missing session_primed:\n%s", e.buf.String())
	}
}

// TestPrimeSkipsWhenClaimTaken: the TUI claimed the opening prompt between
// the primer reading prompt_count==0 and attaching, so the primer sends
// nothing.
func TestPrimeSkipsWhenClaimTaken(t *testing.T) {
	e := newPrimeEnv(t)
	const id = "orch-task-bravo"
	e.writeSession(t, id, agent.TaskAgent, session.StatusRunning, 0)

	inner := e.d.primeAttach
	e.d.primeAttach = func(ctx context.Context, ref string, opts acpchat.Options) (PrimeConversation, error) {
		// The TUI wins the race while the daemon is attaching.
		if won, err := e.store.ClaimOpeningPrompt(id); err != nil || !won {
			t.Errorf("TUI claim = %v, %v; want true, nil", won, err)
		}
		return inner(ctx, ref, opts)
	}

	s := newStubSession(id)
	defer s.Close()
	if !e.d.trackSession(filepath.Join(t.TempDir(), "bravo", "project.yaml"), s, agent.TaskAgent, "", nil) {
		t.Fatal("trackSession returned false")
	}
	waitUntil(t, "primer to detach", func() bool { _, closed := e.conv.state(); return closed })
	e.d.endHandlers()

	if asks, _ := e.conv.state(); len(asks) != 0 {
		t.Fatalf("primer sent %q after losing the claim", asks)
	}
	if got := e.promptCount(t, id); got != 1 {
		t.Errorf("prompt_count = %d, want 1 (the TUI's claim)", got)
	}
	if !strings.Contains(e.buf.String(), "session_prime_skipped") {
		t.Errorf("audit missing session_prime_skipped:\n%s", e.buf.String())
	}
}

// TestPrimeSkipsAlreadyPrompted: a session adopted with prompt_count > 0 (a
// TUI or a previous daemon prompted it) is not attached to at all.
func TestPrimeSkipsAlreadyPrompted(t *testing.T) {
	e := newPrimeEnv(t)
	const id = "orch-task-charlie"
	e.writeSession(t, id, agent.TaskAgent, session.StatusRunning, 3)

	s := newStubSession(id)
	defer s.Close()
	if !e.d.trackSession(filepath.Join(t.TempDir(), "charlie", "project.yaml"), s, agent.TaskAgent, "", nil) {
		t.Fatal("trackSession returned false")
	}
	e.d.endHandlers()

	if n := e.attaches(); n != 0 {
		t.Errorf("attaches = %d, want 0", n)
	}
	if got := e.promptCount(t, id); got != 3 {
		t.Errorf("prompt_count = %d, want 3", got)
	}
}

// TestPrimeDisconnectsOneShotAtEndOfTurn: the primer stays attached for the
// whole turn and disconnects as soon as it ends, which is what lets a
// one-shot (--exit-when-empty) session finish.
func TestPrimeDisconnectsOneShotAtEndOfTurn(t *testing.T) {
	e := newPrimeEnv(t)
	e.conv.release = make(chan struct{})
	const id = "orch-commit-delta"
	e.writeSession(t, id, agent.CommitAgent, session.StatusRunning, 0)

	s := newStubSession(id)
	defer s.Close()
	if !e.d.trackSession(filepath.Join(t.TempDir(), "delta", "project.yaml"), s, agent.CommitAgent, "", nil) {
		t.Fatal("trackSession returned false")
	}

	select {
	case <-e.conv.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("opening prompt never sent")
	}
	time.Sleep(30 * time.Millisecond)
	if _, closed := e.conv.state(); closed {
		t.Fatal("primer disconnected mid-turn")
	}

	close(e.conv.release) // the turn ends
	waitUntil(t, "end-of-turn disconnect", func() bool { _, closed := e.conv.state(); return closed })
	e.d.endHandlers()
	if !strings.Contains(e.buf.String(), "session_prime_turn_ended") {
		t.Errorf("audit missing session_prime_turn_ended:\n%s", e.buf.String())
	}
}

// TestPrimeSkipsDaemonAgents: the daemon's own agents get their turns from
// their drivers, not the primer.
func TestPrimeSkipsDaemonAgents(t *testing.T) {
	e := newPrimeEnv(t)
	for _, kind := range []agent.Kind{agent.WorkingmanAgent, agent.StepUpAgent, agent.SleepyHeadAgent} {
		if e.d.primes(kind) {
			t.Errorf("primes(%s) = true, want false", kind)
		}
	}
	if !e.d.primes(agent.TaskAgent) {
		t.Error("primes(task) = false, want true")
	}
}
