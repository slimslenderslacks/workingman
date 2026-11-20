package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/acpchat"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
)

// suLauncher is a fake ACP launcher that records Step-Up launches.
type suLauncher struct {
	mu       sync.Mutex
	specs    []agent.Spec
	sessions []*stubSession
}

func (l *suLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := newStubSession(spec.Name)
	if spec.Kind == agent.StepUpAgent {
		l.specs = append(l.specs, spec)
		l.sessions = append(l.sessions, s)
	}
	return ctxClosingSession{s}, nil
}

func (l *suLauncher) launches() []agent.Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]agent.Spec(nil), l.specs...)
}

// fakeStepUpConv is an attached Step-Up conversation: it records each prompt
// and runs onAsk (the "agent") during the turn.
type fakeStepUpConv struct {
	mu      sync.Mutex
	prompts []string
	created bool
	onAsk   func()
	events  chan acpchat.Event
}

func (c *fakeStepUpConv) Ask(_ context.Context, text string, _ ...acpchat.AskOption) (string, error) {
	c.mu.Lock()
	c.prompts = append(c.prompts, text)
	fn := c.onAsk
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
	return "ok", nil
}

func (c *fakeStepUpConv) Events() <-chan acpchat.Event { return c.events }
func (c *fakeStepUpConv) Created() bool                { return c.created }
func (c *fakeStepUpConv) Close() error                 { return nil }

func (c *fakeStepUpConv) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.prompts)
}

type suHarness struct {
	d        *Daemon
	launcher *suLauncher
	conv     *fakeStepUpConv
	buf      *safeBuf
	base     string
	po       *postoffice.Store
	cancel   context.CancelFunc
	done     chan struct{}
}

func newSUHarness(t *testing.T, cfg StepUpAgentConfig) *suHarness {
	t.Helper()
	base := t.TempDir()
	l := &suLauncher{}
	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		Launcher:      spawningLauncher{},
		AcpLauncher:   l,
		Kit:           "kit",
		SessionsRoot:  filepath.Join(base, "sessions"),
		MCPGatewayURL: "http://localhost:3333",
		Audit:         a,
	}
	conv := &fakeStepUpConv{events: make(chan acpchat.Event)}
	cfg.PostOfficeDir = filepath.Join(base, "post-office")
	cfg.MinBackoff, cfg.MaxBackoff = 10*time.Millisecond, 40*time.Millisecond
	cfg.Poll, cfg.MinGap = 20*time.Millisecond, time.Millisecond
	if cfg.Reminder == 0 {
		cfg.Reminder = time.Hour
	}
	cfg.Attach = func(context.Context, string, acpchat.Options) (StepUpConversation, error) { return conv, nil }
	d, err := newDaemon(t, []string{t.TempDir()}, a,
		WithRunner(r),
		WithStateFile(filepath.Join(base, "state", "snapshot.json"), 0),
		WithRuntimeInfo(RuntimeInfo{AuditLog: filepath.Join(base, "logs", "audit.log")}),
		WithStepUpAgent(cfg),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	po, err := postoffice.Open(cfg.PostOfficeDir)
	if err != nil {
		t.Fatal(err)
	}
	return &suHarness{d: d, launcher: l, conv: conv, buf: buf, base: base, po: po}
}

func (h *suHarness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		_ = h.d.Run(ctx)
		close(h.done)
	}()
	t.Cleanup(func() {
		if h.cancel != nil {
			h.cancel()
			<-h.done
		}
	})
}

// put stores a request from a task agent to step-up, as post-office would.
func (h *suHarness) put(t *testing.T) postoffice.Message {
	t.Helper()
	m, err := postoffice.NewMessage(postoffice.Draft{
		Kind:      postoffice.KindRequest,
		From:      postoffice.Sender{Agent: "task", Sandbox: "wm-p-fix-ci", Task: "fix-ci"},
		Recipient: "step-up",
		Body:      "Need network access to proxy.golang.org",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.po.Put(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStepUpAgentStartsWithItsPlan(t *testing.T) {
	h := newSUHarness(t, StepUpAgentConfig{})
	h.run(t)
	eventually(t, "step-up tracked", func() bool { return h.d.hasSession(stepUpSessionKey) })

	specs := h.launcher.launches()
	if len(specs) != 1 {
		t.Fatalf("launches = %d, want 1", len(specs))
	}
	joined := strings.Join(specs[0].Command, " ") + " "
	scratch := filepath.Join(h.base, "step-up-agent")
	stepUpDir := filepath.Join(h.base, "step-up")
	for _, want := range []string{
		"--persistent", "--kind step-up", "--sandbox step-up", "--session-id step-up-agent",
		"--require-okta-gateway", "--mcp-server post-office ",
		"--workspace " + scratch + " ",
		"--workspace " + filepath.Join(stepUpDir, "requests") + " ",
		"--workspace " + filepath.Join(h.base, "state") + ":ro",
		"--workspace " + filepath.Join(h.base, "logs") + ":ro",
		"--workspace " + filepath.Join(h.base, "sessions") + ":ro",
		"--workspace " + filepath.Join(stepUpDir, "results") + ":ro",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrapper command missing %q:\n%s", want, joined)
		}
	}
	for _, bad := range []string{"--mcp-dynamic", "--idle-timeout", "--exit-when-empty", "--project-path"} {
		if strings.Contains(joined, bad) {
			t.Errorf("wrapper command must not contain %q", bad)
		}
	}
	for _, info := range h.d.ListSessions() {
		if info.AgentName == "step-up" && (info.Project != "step-up" || info.SandboxName != "step-up") {
			t.Errorf("session info = %+v", info)
		}
	}
	for _, s := range h.d.Snapshot().Sessions {
		if s.Kind == "step-up" && (s.ProjectPath != "" || s.WorkStream != "") {
			t.Errorf("snapshot session = %+v", s)
		}
	}
	sess, ok := h.d.StepUpAgentSession()
	if !ok || sess.ID != "step-up-agent" || sess.SandboxName != "step-up" ||
		sess.Dir != filepath.Join(h.base, "sessions", "step-up-agent") {
		t.Errorf("StepUpAgentSession = %+v, %v", sess, ok)
	}
}

// The driver prompts the agent when a message arrives for it, not again while
// nothing changes, and again when there is something new.
func TestStepUpDriverPromptsOnNewMessages(t *testing.T) {
	h := newSUHarness(t, StepUpAgentConfig{})
	var mu sync.Mutex
	var taken []string
	h.conv.onAsk = func() {
		// The agent takes every waiting message during its turn.
		msgs, _ := h.po.ListOpen()
		for _, m := range msgs {
			if m.ReceivedAt == nil {
				if _, err := h.po.MarkReceived(m.ID, time.Now()); err == nil {
					mu.Lock()
					taken = append(taken, m.ID)
					mu.Unlock()
				}
			}
		}
	}
	h.run(t)
	eventually(t, "driver attached", func() bool { return strings.Contains(h.buf.String(), "step_up_driver_attached") })
	time.Sleep(100 * time.Millisecond)
	if n := h.conv.count(); n != 0 {
		t.Fatalf("prompted %d times with nothing to do", n)
	}

	m1 := h.put(t)
	eventually(t, "first prompt", func() bool { return h.conv.count() == 1 })
	if h.conv.prompts[0] != StepUpPrompt {
		t.Errorf("prompt = %q", h.conv.prompts[0])
	}
	// The agent took it during the turn; that alone is no reason to prompt again.
	time.Sleep(150 * time.Millisecond)
	if n := h.conv.count(); n != 1 {
		t.Fatalf("prompts = %d after the agent took the message, want 1", n)
	}

	h.put(t)
	eventually(t, "second prompt", func() bool { return h.conv.count() == 2 })
	mu.Lock()
	if len(taken) != 2 || taken[0] != m1.ID {
		t.Errorf("taken = %v", taken)
	}
	mu.Unlock()
	if !strings.Contains(h.buf.String(), "step_up_prompted") {
		t.Errorf("no step_up_prompted audit event")
	}
}

// An answer to one of the agent's own requests (its approval request to the
// operator) closing is a reason to prompt it.
func TestStepUpDriverPromptsWhenItsRequestCloses(t *testing.T) {
	h := newSUHarness(t, StepUpAgentConfig{})
	h.run(t)
	eventually(t, "driver attached", func() bool { return strings.Contains(h.buf.String(), "step_up_driver_attached") })

	m, err := postoffice.NewMessage(postoffice.Draft{
		Kind: postoffice.KindRequest, From: postoffice.Sender{Agent: "step-up", Sandbox: "step-up"},
		Body: "CHANGE: sandbox_network_allow proxy.golang.org → wm-p-fix-ci", Options: []string{"done", "deny"},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.po.Put(m); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := h.conv.count(); n != 0 {
		t.Fatalf("prompted %d times for an open operator request", n)
	}
	if _, err := h.po.Claim(postoffice.Resolution{ID: m.ID, Outcome: postoffice.OutcomeAnswered, Response: "done",
		Responder: &postoffice.Responder{Kind: postoffice.ResponderOperator, Via: postoffice.ViaTUI}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.po.Finalize(m.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "prompt after the answer", func() bool { return h.conv.count() == 1 })
}

// A session the driver had to create itself gets one prompt even with nothing
// to do, so the agent has read its instructions.
func TestStepUpDriverPrimesASessionItCreated(t *testing.T) {
	h := newSUHarness(t, StepUpAgentConfig{})
	h.conv.created = true
	h.run(t)
	eventually(t, "priming prompt", func() bool { return h.conv.count() == 1 })
}

// Work the agent holds but has not finished gets a reminder prompt.
func TestStepUpDriverReminds(t *testing.T) {
	h := newSUHarness(t, StepUpAgentConfig{Reminder: 150 * time.Millisecond})
	h.conv.onAsk = func() {
		msgs, _ := h.po.ListOpen()
		for _, m := range msgs {
			_, _ = h.po.MarkReceived(m.ID, time.Now())
		}
	}
	h.run(t)
	h.put(t)
	eventually(t, "first prompt", func() bool { return h.conv.count() == 1 })
	eventually(t, "reminder", func() bool { return h.conv.count() >= 2 })
}

// While another client (the TUI) runs a turn, the driver does not prompt.
func TestStepUpDriverWaitsForAForeignTurn(t *testing.T) {
	h := newSUHarness(t, StepUpAgentConfig{})
	h.run(t)
	eventually(t, "driver attached", func() bool { return strings.Contains(h.buf.String(), "step_up_driver_attached") })
	h.conv.events <- acpchat.Event{Kind: acpchat.EventText, Text: "reading instructions"}
	h.put(t)
	time.Sleep(150 * time.Millisecond)
	if n := h.conv.count(); n != 0 {
		t.Fatalf("prompted %d times during a foreign turn", n)
	}
	h.conv.events <- acpchat.Event{Kind: acpchat.EventTurnEnd, Text: "done"}
	eventually(t, "prompt after the foreign turn", func() bool { return h.conv.count() == 1 })
}

func TestStepUpAgentUnavailableWithoutGateway(t *testing.T) {
	buf := &safeBuf{}
	a := audit.New(buf)
	base := t.TempDir()
	r := &runner.Runner{Launcher: spawningLauncher{}, AcpLauncher: &suLauncher{}, Kit: "kit", SessionsRoot: filepath.Join(base, "sessions")}
	d, err := newDaemon(t, []string{t.TempDir()}, a, WithRunner(r), WithStepUpAgent(StepUpAgentConfig{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if ok, log := waitFor(t, buf, "step_up_agent_unavailable"); !ok {
		t.Fatalf("expected step_up_agent_unavailable:\n%s", log)
	}
	if d.hasSession(stepUpSessionKey) {
		t.Error("agent must not run without a gateway")
	}
}

func TestStepUpAgentDirsMustBeOutsideRoots(t *testing.T) {
	root := t.TempDir()
	r := &runner.Runner{Launcher: spawningLauncher{}, AcpLauncher: &suLauncher{}, Kit: "kit", SessionsRoot: t.TempDir()}
	for _, cfg := range []StepUpAgentConfig{
		{Dir: filepath.Join(root, "scratch")},
		{StepUpDir: filepath.Join(root, "step-up")},
		{PostOfficeDir: root},
	} {
		_, err := newDaemon(t, []string{root}, audit.New(&safeBuf{}), WithRunner(r), WithStepUpAgent(cfg))
		if err == nil || !strings.Contains(err.Error(), "overlaps watched root") {
			t.Errorf("%+v: err = %v, want an overlap error", cfg, err)
		}
	}
}

func TestStepUpAgentLifecycleSpecialCases(t *testing.T) {
	h := newSUHarness(t, StepUpAgentConfig{})
	if h.d.detachable(agent.StepUpAgent) {
		t.Error("the Step-Up agent must stop with the daemon")
	}
	old := sessionEntry{sess: newStubSession("x"), kind: agent.StepUpAgent, startedAt: time.Now().Add(-48 * time.Hour)}
	h.d.sessionIdleTimeout = time.Millisecond
	if v := h.d.strandedVerdict(stepUpSessionKey, old); v.reap {
		t.Errorf("Step-Up agent reaped as stale: %+v", v)
	}

	// A live one a prior daemon left behind is adopted under its key.
	store, err := session.NewStore(filepath.Join(h.base, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(session.Session{
		ID: "step-up-agent", SandboxName: "step-up", Status: session.StatusRunning,
		CreatedAt: time.Now(), Kind: "step-up", Persistent: true,
	}); err != nil {
		t.Fatal(err)
	}
	h.d.ctx = context.Background()
	h.d.reconcileSessions()
	h.d.sessionsMu.Lock()
	e, ok := h.d.sessions[stepUpSessionKey]
	h.d.sessionsMu.Unlock()
	if !ok || e.kind != agent.StepUpAgent {
		t.Fatalf("live Step-Up session not adopted: %+v %v", e, ok)
	}

	// A daemon not supervising one leaves it alone.
	d2, _ := newTestDaemon(t)
	d2.runner = h.d.runner
	d2.reconcileSessions()
	if d2.hasSession(stepUpSessionKey) {
		t.Error("a daemon not supervising the agent must not adopt it")
	}
}

func TestStepUpPlanCreatesRequestDirs(t *testing.T) {
	h := newSUHarness(t, StepUpAgentConfig{})
	if _, err := h.d.stepUpPlan(); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{StepUpRequestsDir, StepUpResultsDir} {
		if fi, err := os.Stat(filepath.Join(h.base, "step-up", sub)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", sub, err)
		}
	}
}
