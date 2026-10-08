package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
)

// wmLauncher is a fake ACP launcher for the supervision tests. It records every
// workingman launch (with its time) and the stub session handed back, so a test
// can "crash" the agent by closing that session; failFirst makes the first N
// workingman launches fail. Other kinds launch normally.
type wmLauncher struct {
	mu        sync.Mutex
	failFirst int
	specs     []agent.Spec
	times     []time.Time
	sessions  []*stubSession
	otherSpec []agent.Spec
}

func (l *wmLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if spec.Kind != agent.WorkingmanAgent {
		l.otherSpec = append(l.otherSpec, spec)
		return newStubSession(spec.Name), nil
	}
	l.times = append(l.times, time.Now())
	if len(l.times) <= l.failFirst {
		return nil, errors.New("sbx exploded")
	}
	s := newStubSession(spec.Name)
	l.specs = append(l.specs, spec)
	l.sessions = append(l.sessions, s)
	return ctxClosingSession{s}, nil
}

// ctxClosingSession behaves like the production process session: Wait closes
// the session when its context is cancelled (the daemon's shutdown), which is
// how a non-detachable agent actually gets stopped.
type ctxClosingSession struct{ *stubSession }

func (s ctxClosingSession) Wait(ctx context.Context) error {
	err := s.stubSession.Wait(ctx)
	if err != nil {
		_ = s.Close()
	}
	return err
}

func (l *wmLauncher) attempts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.times)
}

func (l *wmLauncher) started() []*stubSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*stubSession(nil), l.sessions...)
}

func (l *wmLauncher) others() []agent.Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]agent.Spec(nil), l.otherSpec...)
}

type wmHarness struct {
	d        *Daemon
	launcher *wmLauncher
	buf      *safeBuf
	root     string
	base     string // parent of sessions/, state/, workingman-agent/
	cancel   context.CancelFunc
	done     chan struct{}
}

// newWMHarness builds a daemon wired like production for the workingman agent
// (ACP runner, state file, audit log) over temp dirs, but does not Run it.
func newWMHarness(t *testing.T, cfg WorkingmanAgentConfig, failFirst int, extra ...Option) *wmHarness {
	t.Helper()
	base := t.TempDir()
	root := t.TempDir()
	l := &wmLauncher{failFirst: failFirst}
	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		Launcher:     spawningLauncher{},
		AcpLauncher:  l,
		Kit:          "kit",
		SessionsRoot: filepath.Join(base, "sessions"),
		Audit:        a,
	}
	if cfg.MinBackoff == 0 {
		cfg.MinBackoff = 10 * time.Millisecond
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 40 * time.Millisecond
	}
	if cfg.StableAfter == 0 {
		cfg.StableAfter = time.Hour
	}
	opts := []Option{
		WithRunner(r),
		WithStateFile(filepath.Join(base, "state", "snapshot.json"), 0),
		WithRuntimeInfo(RuntimeInfo{AuditLog: filepath.Join(base, "logs", "audit.log")}),
		WithWorkingmanAgent(cfg),
	}
	opts = append(opts, extra...)
	d, err := New([]string{root}, a, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &wmHarness{d: d, launcher: l, buf: buf, root: root, base: base}
}

func (h *wmHarness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		_ = h.d.Run(ctx)
		close(h.done)
	}()
	t.Cleanup(h.stop)
}

func (h *wmHarness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	<-h.done
	h.cancel = nil
}

func TestWorkingmanAgentStartsAtBootAndIsObservable(t *testing.T) {
	h := newWMHarness(t, WorkingmanAgentConfig{}, 0)
	h.run(t)
	eventually(t, "workingman agent tracked", func() bool { return h.d.hasSession(workingmanSessionKey) })

	specs := func() []agent.Spec {
		h.launcher.mu.Lock()
		defer h.launcher.mu.Unlock()
		return append([]agent.Spec(nil), h.launcher.specs...)
	}()
	if len(specs) != 1 {
		t.Fatalf("launches = %d, want 1", len(specs))
	}
	cmd := specs[0].Command
	joined := strings.Join(cmd, " ")
	scratch := filepath.Join(h.base, "workingman-agent")
	for _, want := range []string{
		"--persistent", "--kind workingman", "--sandbox workingman-agent", "--session-id workingman-agent",
		"--workspace " + scratch + " ", // writable scratch, first
		"--workspace " + h.root,
		"--workspace " + filepath.Join(h.base, "state") + ":ro",
		"--workspace " + filepath.Join(h.base, "logs") + ":ro",
		"--workspace " + filepath.Join(h.base, "sessions") + ":ro",
	} {
		if !strings.Contains(joined+" ", want) {
			t.Errorf("wrapper command missing %q:\n%s", want, joined)
		}
	}
	for _, bad := range []string{"--exit-when-empty", "--idle-timeout", "--project-path", "--unblock-grace", "--static-mcp"} {
		if strings.Contains(joined, bad) {
			t.Errorf("wrapper command must not contain %q: %s", bad, joined)
		}
	}
	if _, err := os.Stat(filepath.Join(scratch, ".orch", "instructions.md")); err != nil {
		t.Errorf("handoff files not written to the scratch dir: %v", err)
	}

	// Shown in the TUI session list under its own name, not as a work stream ".".
	var found bool
	for _, info := range h.d.ListSessions() {
		if info.AgentName != "workingman" {
			continue
		}
		found = true
		if info.Project != "workingman" || info.SandboxName != "workingman-agent" || info.Interactive {
			t.Errorf("session info = %+v", info)
		}
	}
	if !found {
		t.Errorf("workingman agent missing from ListSessions: %+v", h.d.ListSessions())
	}
	// ...and in the snapshot with no project.
	for _, s := range h.d.Snapshot().Sessions {
		if s.Kind == "workingman" && (s.ProjectPath != "" || s.WorkStream != "" || s.ACP == nil) {
			t.Errorf("snapshot session = %+v", s)
		}
	}

	// The accessor the router attaches through.
	sess, ok := h.d.WorkingmanAgentSession()
	if !ok {
		t.Fatal("WorkingmanAgentSession: not running")
	}
	sessionsRoot := filepath.Join(h.base, "sessions")
	if sess.ID != "workingman-agent" || sess.Dir != filepath.Join(sessionsRoot, "workingman-agent") ||
		sess.SocketPath != filepath.Join(sessionsRoot, "workingman-agent", session.SocketName) ||
		sess.SandboxName != "workingman-agent" || sess.StartedAt.IsZero() {
		t.Errorf("WorkingmanAgentSession = %+v", sess)
	}
	if !strings.Contains(h.buf.String(), "workingman_agent_started") {
		t.Errorf("no workingman_agent_started audit event:\n%s", h.buf.String())
	}
}

func TestWorkingmanAgentSessionAbsentWhenNotRunning(t *testing.T) {
	// Not configured at all.
	d, _ := newTestDaemon(t)
	if _, ok := d.WorkingmanAgentSession(); ok {
		t.Error("a daemon without the agent reported a session")
	}
	// Configured but not started yet.
	h := newWMHarness(t, WorkingmanAgentConfig{}, 0)
	if _, ok := h.d.WorkingmanAgentSession(); ok {
		t.Error("session reported before Run")
	}
}

func TestWorkingmanAgentRestartsWithBackoffAfterCrash(t *testing.T) {
	h := newWMHarness(t, WorkingmanAgentConfig{
		MinBackoff: 40 * time.Millisecond,
		MaxBackoff: 160 * time.Millisecond,
	}, 0)
	h.run(t)

	// Crash it five times; each relaunch must wait out a doubling, capped delay.
	var wantMin = []time.Duration{40, 80, 160, 160}
	for i := 0; i < len(wantMin); i++ {
		n := i + 1
		eventually(t, "launch", func() bool { return len(h.launcher.started()) >= n })
		eventually(t, "tracked", func() bool { return h.d.hasSession(workingmanSessionKey) })
		h.launcher.started()[n-1].Close() // the agent exits
		eventually(t, "relaunch", func() bool { return len(h.launcher.started()) >= n+1 })
	}
	h.launcher.mu.Lock()
	times := append([]time.Time(nil), h.launcher.times...)
	h.launcher.mu.Unlock()
	for i, min := range wantMin {
		gap := times[i+1].Sub(times[i])
		if gap < min*time.Millisecond*9/10 {
			t.Errorf("relaunch %d came after %v, want >= %v (exponential backoff)", i+1, gap, min*time.Millisecond)
		}
		// Capped: never anywhere near double the cap even on a loaded machine.
		if gap > 160*time.Millisecond+time.Second {
			t.Errorf("relaunch %d came after %v; the cap is 160ms", i+1, gap)
		}
	}
	log := h.buf.String()
	for _, want := range []string{"workingman_agent_ended", "workingman_agent_restart_scheduled", "delay=40ms", "delay=80ms", "delay=160ms"} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log missing %q:\n%s", want, log)
		}
	}
}

func TestWorkingmanAgentBackoffResetsAfterStableRun(t *testing.T) {
	h := newWMHarness(t, WorkingmanAgentConfig{
		MinBackoff:  30 * time.Millisecond,
		MaxBackoff:  240 * time.Millisecond,
		StableAfter: 60 * time.Millisecond,
	}, 0)
	h.run(t)

	eventually(t, "first launch", func() bool { return len(h.launcher.started()) >= 1 })
	eventually(t, "tracked", func() bool { return h.d.hasSession(workingmanSessionKey) })
	time.Sleep(120 * time.Millisecond) // outlive StableAfter
	h.launcher.started()[0].Close()
	eventually(t, "second launch", func() bool { return len(h.launcher.started()) >= 2 })
	eventually(t, "tracked again", func() bool { return h.d.hasSession(workingmanSessionKey) })
	h.launcher.started()[1].Close() // dies at once: a crash loop
	eventually(t, "third launch", func() bool { return len(h.launcher.started()) >= 3 })
	h.launcher.mu.Lock()
	times := append([]time.Time(nil), h.launcher.times...)
	h.launcher.mu.Unlock()
	// After the stable first run the delay restarted at the minimum (30ms)...
	if gap := times[1].Sub(times[0]); gap > 120*time.Millisecond+30*time.Millisecond+time.Second {
		t.Errorf("restart after a stable run took %v", gap)
	}
	// ...and the immediate crash then doubled it (60ms), not kept it at 30ms.
	if gap := times[2].Sub(times[1]); gap < 54*time.Millisecond {
		t.Errorf("restart after a quick crash came after %v, want >= 60ms", gap)
	}
}

func TestWorkingmanAgentLaunchFailureRetriesAndNeverBlocksDispatch(t *testing.T) {
	// The first three workingman launches fail. Meanwhile an ordinary project
	// must still be dispatched.
	h := newWMHarness(t, WorkingmanAgentConfig{}, 3)
	proj := filepath.Join(h.root, "myproj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "description: x\nstatus: ready\nbranch: feat-x\nrepos: []\nupdated_by: human\n"
	if err := os.WriteFile(filepath.Join(proj, ".project.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h.run(t)

	eventually(t, "planning dispatched despite the failing workingman launch", func() bool {
		for _, s := range h.launcher.others() {
			if s.Kind == agent.PlanningAgent {
				return true
			}
		}
		return false
	})
	eventually(t, "agent up after retries", func() bool { return h.d.hasSession(workingmanSessionKey) })
	if got := h.launcher.attempts(); got != 4 {
		t.Errorf("launch attempts = %d, want 4 (3 failures then success)", got)
	}
	log := h.buf.String()
	if got := strings.Count(log, "workingman_agent_start_error"); got != 3 {
		t.Errorf("workingman_agent_start_error x%d, want 3:\n%s", got, log)
	}
}

func TestWorkingmanAgentStopsOnShutdown(t *testing.T) {
	h := newWMHarness(t, WorkingmanAgentConfig{}, 0)
	h.run(t)
	eventually(t, "tracked", func() bool { return h.d.hasSession(workingmanSessionKey) })
	sess := h.launcher.started()[0]

	h.stop()

	select {
	case <-sess.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the workingman agent was left running after shutdown")
	}
	if h.d.hasSession(workingmanSessionKey) {
		t.Error("session still tracked after shutdown")
	}
	// And it is not relaunched once the daemon is down.
	time.Sleep(100 * time.Millisecond)
	if n := len(h.launcher.started()); n != 1 {
		t.Errorf("launches after shutdown = %d, want 1", n)
	}
}

func TestWorkingmanAgentIsNotDetachable(t *testing.T) {
	h := newWMHarness(t, WorkingmanAgentConfig{}, 0)
	if h.d.detachable(agent.WorkingmanAgent) {
		t.Error("the workingman agent must stop with the daemon, not survive it")
	}
	if !h.d.detachable(agent.PlanningAgent) {
		t.Error("control: ACP planning stays detachable")
	}
}

func TestWorkingmanAgentUnavailableWithoutACP(t *testing.T) {
	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{Launcher: spawningLauncher{}} // no AcpLauncher
	d, err := New([]string{t.TempDir()}, a, WithRunner(r), WithWorkingmanAgent(WorkingmanAgentConfig{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if ok, log := waitFor(t, buf, "workingman_agent_unavailable"); !ok {
		t.Fatalf("expected workingman_agent_unavailable:\n%s", log)
	}
	if d.hasSession(workingmanSessionKey) {
		t.Error("agent must not run without ACP")
	}
}

func TestWorkingmanAgentDirMustBeOutsideRoots(t *testing.T) {
	root := t.TempDir()
	l := &wmLauncher{}
	r := &runner.Runner{Launcher: spawningLauncher{}, AcpLauncher: l, Kit: "kit", SessionsRoot: t.TempDir()}
	for _, dir := range []string{filepath.Join(root, "scratch"), root, filepath.Dir(root)} {
		_, err := New([]string{root}, audit.New(&safeBuf{}), WithRunner(r), WithWorkingmanAgent(WorkingmanAgentConfig{Dir: dir}))
		if err == nil || !strings.Contains(err.Error(), "overlaps watched root") {
			t.Errorf("Dir %q: err = %v, want an overlap error", dir, err)
		}
	}
}

// A workingman agent a prior daemon left running is adopted (not duplicated)
// when the new daemon supervises one; a daemon that doesn't, leaves it alone.
func TestWorkingmanAgentAdoptedOnRestart(t *testing.T) {
	h := newWMHarness(t, WorkingmanAgentConfig{}, 0)
	store, err := session.NewStore(filepath.Join(h.base, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(session.Session{
		ID: "workingman-agent", SandboxName: "workingman-agent", Status: session.StatusRunning,
		CreatedAt: time.Now(), Kind: "workingman", Persistent: true,
	}); err != nil {
		t.Fatal(err)
	}

	h.d.ctx = context.Background()
	h.d.reconcileSessions()
	if !h.d.hasSession(workingmanSessionKey) {
		t.Fatal("live workingman session was not adopted")
	}
	h.d.sessionsMu.Lock()
	kind := h.d.sessions[workingmanSessionKey].kind
	h.d.sessionsMu.Unlock()
	if kind != agent.WorkingmanAgent {
		t.Errorf("adopted kind = %v", kind)
	}

	// A daemon with the agent disabled leaves it untracked.
	d2, _ := newTestDaemon(t)
	d2.runner = h.d.runner
	d2.reconcileSessions()
	if d2.hasSession(workingmanSessionKey) {
		t.Error("a daemon not supervising the agent must not adopt it")
	}
}

func TestWorkingmanAgentIsNeverReaped(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.sessionIdleTimeout = time.Millisecond
	old := sessionEntry{sess: newStubSession("x"), startedAt: time.Now().Add(-48 * time.Hour)}

	old.kind = agent.WorkingmanAgent
	if v := d.strandedVerdict(workingmanSessionKey, old); v.reap {
		t.Errorf("workingman agent reaped as stale: %+v", v)
	}
	// Control: the same staleness reaps an ordinary autonomous agent.
	old.kind = agent.ReviewAgent
	if v := d.strandedVerdict("/orch/p/.project.yaml", old); !v.reap {
		t.Errorf("control: a stale review session should be reaped: %+v", v)
	}
}

func TestWorkingmanBackoffLadder(t *testing.T) {
	lo, hi := defaultWorkingmanMinBackoff, defaultWorkingmanMaxBackoff
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := workingmanBackoff(i+1, lo, hi); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := workingmanBackoff(1000, lo, hi); got != hi {
		t.Errorf("backoff(1000) = %v, want the cap %v", got, hi)
	}
	if got := workingmanBackoff(0, lo, hi); got != lo {
		t.Errorf("backoff(0) = %v, want %v", got, lo)
	}
}
