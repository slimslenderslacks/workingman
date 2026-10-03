package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/channeltest"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
)

// capturingLauncher hands back stubSessions it remembers, so a test can end a
// wolf session on demand, and can be told to fail launches.
type capturingLauncher struct {
	mu       sync.Mutex
	sessions []*stubSession
	err      error
}

func (l *capturingLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	s := newStubSession(fmt.Sprintf("%s-%d", spec.Name, len(l.sessions)+1))
	l.sessions = append(l.sessions, s)
	return s, nil
}

func (l *capturingLauncher) last() *stubSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessions[len(l.sessions)-1]
}

type channelHarness struct {
	d         *Daemon
	audit     *safeBuf
	fake      *channeltest.Fake
	index     *channels.FileConversationIndex
	launcher  *capturingLauncher
	proj      string
	projectYA *project.Project
	indexFile string
}

// newChannelHarness builds a daemon with a real Registry over a fake channel
// routed for the `wolf` topic. A project directory "myproj" with a blocked
// .project.yaml exists under a temp root.
func newChannelHarness(t *testing.T, copts ...ChannelOption) *channelHarness {
	t.Helper()
	buf := &safeBuf{}
	fake := channeltest.New("whatsapp")
	reg := channels.NewRegistry()
	if err := reg.Add(fake); err != nil {
		t.Fatal(err)
	}
	reg.SetRoutes([]channels.Route{{Topic: "wolf", Channel: "whatsapp", Chat: "15551234567"}})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := reg.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}

	indexFile := filepath.Join(t.TempDir(), "state", "conv.json")
	idx, err := channels.NewFileConversationIndex(indexFile)
	if err != nil {
		t.Fatal(err)
	}
	launcher := &capturingLauncher{}
	d, err := New([]string{t.TempDir()}, audit.New(buf), WithChannels(reg, idx, copts...))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.ctx = ctx
	d.runner = &runner.Runner{
		Launcher: launcher,
		Command:  func(agent.Kind, string) []string { return []string{"true"} },
	}
	t.Cleanup(func() { _ = d.watcher.Close() })

	projectPath := filepath.Join(t.TempDir(), "myproj", ".project.yaml")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// The file says "stopped" so that when a wolf session ends the daemon's
	// re-evaluation dispatches nothing (a blocked file would relaunch the wolf).
	p := &project.Project{Branch: "feat/x", Status: project.StatusBlocked}
	if err := project.Save(projectPath, &project.Project{Branch: "feat/x", Status: project.StatusStopped}); err != nil {
		t.Fatal(err)
	}
	return &channelHarness{d: d, audit: buf, fake: fake, index: idx, launcher: launcher, proj: projectPath, projectYA: p, indexFile: indexFile}
}

func (h *channelHarness) launch(reason string) {
	h.d.launchWolfAgent(h.proj, h.projectYA, reason)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWolfStartMessageSentOnceAndCorrelated(t *testing.T) {
	h := newChannelHarness(t)
	taskDir := filepath.Join(filepath.Dir(h.proj), "tasks")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "build-it.yaml"), []byte("name: build-it\nstatus: failed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.launch("the build is broken")
	eventually(t, "wolf start message", func() bool { return len(h.fake.Sent()) == 1 })

	msg := h.fake.Sent()[0]
	if msg.Topic != "wolf" || msg.ChatID != "15551234567" {
		t.Errorf("message routed as topic=%q chat=%q, want wolf / 15551234567", msg.Topic, msg.ChatID)
	}
	for _, want := range []string{"🐺 wolf is running for myproj", "Blocked: the build is broken", "Failed tasks: build-it", "Reply to this message to talk to the wolf."} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("message missing %q:\n%s", want, msg.Text)
		}
	}

	// The wamid-equivalent maps back to the wolf session.
	var target channels.ConversationTarget
	eventually(t, "correlation recorded", func() bool {
		var ok bool
		target, ok = h.index.Lookup("whatsapp", "whatsapp-1")
		return ok
	})
	if target.Kind != channels.ConversationKindWolf || target.SessionKey != wolfSessionKey(h.proj) ||
		target.ProjectPath != h.proj || target.WorkStream != "myproj" || target.SessionID == "" {
		t.Errorf("target = %+v", target)
	}

	// It is on disk too: a fresh index over the same file resolves it.
	reloaded, err := channels.NewFileConversationIndex(h.indexFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Lookup("whatsapp", "whatsapp-1"); !ok {
		t.Error("correlation not persisted to the state dir")
	}

	// A second summon while the wolf runs is a dedup no-op: no new message.
	h.launch("summoned again")
	time.Sleep(50 * time.Millisecond)
	if got := len(h.fake.Sent()); got != 1 {
		t.Errorf("dedup'd launch sent a message; total = %d, want 1", got)
	}
}

func TestWolfStartMessageNotSentWhenLaunchFails(t *testing.T) {
	h := newChannelHarness(t)
	h.launcher.err = errors.New("sandbox won't build")
	h.launch("x")
	time.Sleep(50 * time.Millisecond)
	if got := len(h.fake.Sent()); got != 0 {
		t.Errorf("a failed wolf launch announced itself: %d messages", got)
	}
}

func TestWolfEndMessageAfterStart(t *testing.T) {
	h := newChannelHarness(t)
	h.launch("boom")
	eventually(t, "start message", func() bool { return len(h.fake.Sent()) == 1 })

	_ = h.launcher.last().Close()
	eventually(t, "end message", func() bool { return len(h.fake.Sent()) == 2 })
	if got := h.fake.Sent()[1]; got.Topic != "wolf" || got.Text != "🐺 wolf finished for myproj: project now stopped" {
		t.Errorf("end message = %+v", got)
	}
}

func TestWolfStartRateLimitedPerWorkStream(t *testing.T) {
	h := newChannelHarness(t, WithWolfStartInterval(10*time.Minute))
	now := time.Now()
	h.d.channels.now = func() time.Time { return now }

	h.launch("first")
	eventually(t, "first start message", func() bool { return len(h.fake.Sent()) == 1 })
	_ = h.launcher.last().Close()
	eventually(t, "end message", func() bool { return len(h.fake.Sent()) == 2 })
	eventually(t, "session gone", func() bool { return !h.d.hasSession(wolfSessionKey(h.proj)) })

	// Relaunched in a loop within the window: no new start, and (since no
	// start went out) no end message either.
	h.launch("second")
	eventually(t, "suppression logged", func() bool { return strings.Contains(h.audit.String(), "wolf_start_suppressed") })
	_ = h.launcher.last().Close()
	eventually(t, "session gone", func() bool { return !h.d.hasSession(wolfSessionKey(h.proj)) })
	time.Sleep(50 * time.Millisecond)
	if got := len(h.fake.Sent()); got != 2 {
		t.Fatalf("messages = %d, want 2 (rate-limited relaunch must be silent)", got)
	}

	// After the interval the next wolf is announced again.
	now = now.Add(11 * time.Minute)
	h.launch("third")
	eventually(t, "third start message", func() bool { return len(h.fake.Sent()) == 3 })

	// A different work stream has its own budget.
	other := filepath.Join(t.TempDir(), "otherproj", ".project.yaml")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	h.d.launchWolfAgent(other, &project.Project{Branch: "b", Status: project.StatusBlocked}, "other")
	eventually(t, "other stream start message", func() bool { return len(h.fake.Sent()) == 4 })
}

func TestChannelSendFailureDoesNotStallDispatch(t *testing.T) {
	h := newChannelHarness(t, WithChannelSendTimeout(100*time.Millisecond))
	h.fake.SendFunc = func(ctx context.Context, _ channels.OutboundMessage) (string, error) {
		<-ctx.Done() // a hung network call, only freed by the send timeout
		return "", ctx.Err()
	}

	returned := make(chan struct{})
	go func() {
		h.launch("stuck channel")
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("launchWolfAgent waited on a hung channel send")
	}
	if !h.d.hasSession(wolfSessionKey(h.proj)) {
		t.Fatal("wolf session was not started")
	}

	eventually(t, "channel_send_error audit", func() bool { return strings.Contains(h.audit.String(), "channel_send_error") })
	if _, ok := h.index.Lookup("whatsapp", "whatsapp-1"); ok {
		t.Error("a failed send must not be recorded in the conversation index")
	}

	// The failure released the rate-limit slot, so the next wolf start retries.
	h.fake.SendFunc = nil
	_ = h.launcher.last().Close()
	eventually(t, "session gone", func() bool { return !h.d.hasSession(wolfSessionKey(h.proj)) })
	h.launch("retry")
	eventually(t, "retry delivered", func() bool { return len(h.fake.Sent()) >= 2 && strings.Contains(h.fake.Sent()[1].Text, "retry") })
}

func TestChannelSendPanicDoesNotCrashDispatch(t *testing.T) {
	h := newChannelHarness(t)
	h.fake.PanicOnSend = true
	h.launch("panicky channel")
	eventually(t, "channel_send_error audit", func() bool { return strings.Contains(h.audit.String(), "channel_send_error") })
	if !h.d.hasSession(wolfSessionKey(h.proj)) {
		t.Fatal("wolf session should be running")
	}
}

// TestChannelsDisabledRegression: with no WithChannels the wolf launches
// exactly as before and nothing channel-related is emitted.
func TestChannelsDisabledRegression(t *testing.T) {
	d, _ := newTestDaemon(t)
	if d.channels != nil {
		t.Fatal("channels must be off by default")
	}
	d.runner = &runner.Runner{
		Launcher: spawningLauncher{},
		Command:  func(agent.Kind, string) []string { return []string{"true"} },
	}
	projectPath := filepath.Join(t.TempDir(), "myproj", ".project.yaml")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	d.launchWolfAgent(projectPath, &project.Project{Branch: "b", Status: project.StatusBlocked}, "x")
	if !d.hasSession(wolfSessionKey(projectPath)) {
		t.Fatal("wolf did not launch with channels disabled")
	}
}

func TestWithChannelsNilSenderLeavesChannelsOff(t *testing.T) {
	d, err := New([]string{t.TempDir()}, audit.New(&safeBuf{}), WithChannels(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer d.watcher.Close()
	if d.channels != nil {
		t.Error("nil sender should leave channels disabled")
	}
}

func TestWolfStartTextTruncatesAndCapsTasks(t *testing.T) {
	long := strings.Repeat("x", 1000)
	var tasks []string
	for i := 0; i < 8; i++ {
		tasks = append(tasks, fmt.Sprintf("/p/tasks/t%d.yaml", i))
	}
	got := wolfStartText("ws", "line one\nline two "+long, tasks)
	if !strings.Contains(got, "Blocked: line one line two ") || !strings.Contains(got, "…") {
		t.Errorf("reason not collapsed/truncated:\n%s", got)
	}
	if strings.Count(got, "x") > maxWolfReasonRunes {
		t.Errorf("reason not truncated to %d runes", maxWolfReasonRunes)
	}
	if !strings.Contains(got, "Failed tasks: t0, t1, t2, t3, t4, +3 more") {
		t.Errorf("tasks not capped:\n%s", got)
	}
	if bare := wolfStartText("ws", "", nil); bare != "🐺 wolf is running for ws\n"+wolfReplyInstruction {
		t.Errorf("bare message = %q", bare)
	}
}
