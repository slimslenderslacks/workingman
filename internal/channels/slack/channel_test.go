package slack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

const (
	self = "U0CC1K1MFNX"
	dm   = "D0CBT6QV8NT"
)

type harness struct {
	t     *testing.T
	fake  *fakeSlack
	ch    *Channel
	dir   string
	inbox chan channels.InboundMessage
}

func testConfig() Config {
	return Config{
		GatewayURL:   DefaultGatewayURL,
		Channel:      self,
		AllowFrom:    []string{self},
		PollInterval: time.Hour, // tests poll by hand
		WatchTTL:     DefaultWatchTTL,
	}
}

func newHarness(t *testing.T, fake *fakeSlack, dir string) *harness {
	t.Helper()
	h := &harness{t: t, fake: fake, dir: dir, inbox: make(chan channels.InboundMessage, 32)}
	h.ch = New("slack", testConfig(), fake, dir, nil)
	h.ch.now = fake.now
	h.ch.firstPoll = time.Hour
	h.ch.sleep = func(context.Context, time.Duration) error { return nil }
	if err := h.ch.Start(context.Background(), func(_ context.Context, m channels.InboundMessage) { h.inbox <- m }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.ch.Close() })
	h.poll() // the first poll, by hand
	return h
}

// poll runs one poll on the worker and waits for it.
func (h *harness) poll() {
	h.t.Helper()
	if err := h.ch.do(context.Background(), h.ch.poll); err != nil {
		h.t.Fatalf("poll: %v", err)
	}
}

func (h *harness) send(msg channels.OutboundMessage) string {
	h.t.Helper()
	id, err := h.ch.Send(context.Background(), msg)
	if err != nil {
		h.t.Fatalf("Send: %v", err)
	}
	return id
}

func (h *harness) next() channels.InboundMessage {
	h.t.Helper()
	select {
	case m := <-h.inbox:
		return m
	case <-time.After(2 * time.Second):
		h.t.Fatal("no inbound message")
	}
	return channels.InboundMessage{}
}

func (h *harness) none() {
	h.t.Helper()
	select {
	case m := <-h.inbox:
		h.t.Fatalf("unexpected inbound message %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func request(text string) channels.OutboundMessage {
	return channels.OutboundMessage{ChatID: self, Topic: postOfficeTopic, Text: "❓ [req r1t8w] task fix-ci (wm)\n" + text}
}

func TestSendReturnsResolvedChannelAndTS(t *testing.T) {
	h := newHarness(t, newFakeSlack(), t.TempDir())
	id := h.send(channels.OutboundMessage{ChatID: self, Topic: "wolf", Text: "hello"})
	ch, ts := SplitMessageID(id)
	if ch != dm || ts == "" {
		t.Fatalf("id = %q, want %s:<ts>", id, dm)
	}
	sends := h.fake.callsTo(toolSend)
	if len(sends) != 1 || sends[0].Tool != "slack_xaa-slack_send_message" {
		t.Fatalf("send calls = %+v", sends)
	}
	if got := sends[0].Args["message"]; got != "[wolf] hello" {
		t.Errorf("message = %q, want the topic prefix", got)
	}
	// A reply threads on the parent's ts in the parent's channel.
	h.send(channels.OutboundMessage{ChatID: self, ReplyToID: id, Text: "follow-up"})
	sends = h.fake.callsTo(toolSend)
	if a := sends[1].Args; a["thread_ts"] != ts || a["channel_id"] != dm {
		t.Errorf("reply args = %v", a)
	}
	// Post-office text keeps its own label; only requests are watched.
	h.send(channels.OutboundMessage{ChatID: self, Topic: postOfficeTopic, Text: "📣 [note abcdef] fyi"})
	req := h.send(request("Which bucket?"))
	if got := h.fake.callsTo(toolSend)[2].Args["message"]; got != "📣 [note abcdef] fyi" {
		t.Errorf("post-office message = %q", got)
	}
	if ws := h.ch.st.watch.Watches; len(ws) != 1 || ws[0].ID != req || ws[0].Chat != self {
		t.Errorf("watches = %+v, want just %s", ws, req)
	}
	// Both files are persisted, private.
	for _, f := range []string{SentFile, WatchFile} {
		fi, err := os.Stat(filepath.Join(h.dir, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if got := len(h.ch.st.sent.Sent); got != 4 {
		t.Errorf("sent set has %d entries, want 4", got)
	}
}

func TestHeaderScriptFailuresAreRetried(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, "")
	fake.mu.Lock()
	fake.headerFailures = 2
	fake.mu.Unlock()
	h.send(channels.OutboundMessage{Text: "x"})
	if n := len(fake.callsTo(toolSend)); n != 3 {
		t.Errorf("send attempts = %d, want 3", n)
	}

	fake.mu.Lock()
	fake.headerFailures = 100
	fake.mu.Unlock()
	_, err := h.ch.Send(context.Background(), channels.OutboundMessage{Text: "y"})
	if err == nil || !strings.Contains(err.Error(), "header script failed") {
		t.Fatalf("Send = %v, want the header-script error", err)
	}
	if n := len(fake.callsTo(toolSend)); n != 3+len(headerRetries)+1 {
		t.Errorf("send attempts = %d, want %d", n, 3+len(headerRetries)+1)
	}
}

func TestOtherToolErrorsAreNotRetried(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, "")
	before := len(fake.callsTo(toolReaction))
	err := h.ch.do(context.Background(), func(ctx context.Context) error {
		_, err := h.ch.call(ctx, toolReaction, map[string]any{"channel_id": dm, "message_ts": "1.000001", "emoji": "x"})
		return err
	})
	if err == nil || len(fake.callsTo(toolReaction)) != before+1 {
		t.Fatalf("err = %v, calls = %d", err, len(fake.callsTo(toolReaction))-before)
	}
}

func TestCallsAreSerialized(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.send(request("q")) }()
		go func() { defer wg.Done(); h.poll() }()
	}
	wg.Wait()
	if fake.maxInflight != 1 {
		t.Fatalf("max concurrent calls = %d, want 1", fake.maxInflight)
	}
}

func TestTopLevelHumanMessagesAreEmitted(t *testing.T) {
	fake := newFakeSlack()
	fake.human(self, "", "from before the channel started")
	h := newHarness(t, fake, "")
	h.none() // history is never replayed

	h.send(channels.OutboundMessage{Text: "our own post"})
	ts := fake.human(self, "", "what's running?")
	h.poll()
	m := h.next()
	if m.Text != "what's running?" || m.MessageID != dm+":"+ts || m.ReplyToID != "" || m.ChatID != self || m.SenderID != self || m.SenderName != "Jim Clark" {
		t.Fatalf("inbound = %+v", m)
	}
	if m.Timestamp.Unix() == 0 {
		t.Errorf("no timestamp")
	}
	h.poll()
	h.none() // seen once only, and our post never
}

func TestThreadRepliesToARequestAreEmitted(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, "")
	req := h.send(request("Which bucket?"))
	_, reqTS := SplitMessageID(req)

	h.send(channels.OutboundMessage{ReplyToID: req, Text: "our own thread note"})
	ts := fake.human(dm, reqTS, "staging")
	threadsBefore := len(fake.callsTo(toolThread))
	h.poll()
	m := h.next()
	if m.Text != "staging" || m.ReplyToID != req || m.MessageID != dm+":"+ts || m.ChatID != self {
		t.Fatalf("inbound = %+v", m)
	}
	h.none()

	// No new replies: the thread is not read again.
	h.poll()
	if n := len(fake.callsTo(toolThread)) - threadsBefore; n != 1 {
		t.Errorf("read_thread calls = %d, want 1", n)
	}
	h.none()
}

func TestAllowFromDropsOtherSenders(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, "")
	req := h.send(request("q"))
	_, reqTS := SplitMessageID(req)
	fake.humanAs(dm, "", "U0OTHER0001", "Mallory", "top level")
	fake.humanAs(dm, reqTS, "U0OTHER0001", "Mallory", "prod!")
	h.poll()
	h.none()
}

func TestResolvePostsNoteAndReactsThenAnswersLateReplies(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, "")
	req := h.send(request("q"))
	_, reqTS := SplitMessageID(req)

	err := h.ch.Resolve(context.Background(), self, req, channels.Resolution{Outcome: "answered", Via: "tui", Note: "✅ [req r1t8w] Answered via tui: staging"})
	if err != nil {
		t.Fatal(err)
	}
	sends := fake.callsTo(toolSend)
	last := sends[len(sends)-1].Args
	if last["thread_ts"] != reqTS || last["message"] != "✅ [req r1t8w] Answered via tui: staging" || last["reply_broadcast"] != nil {
		t.Errorf("closing note = %v", last)
	}
	if p := fake.find(dm, reqTS); len(p.reactions) != 1 || p.reactions[0] != "white_check_mark" {
		t.Errorf("reactions = %v", p.reactions)
	}
	// Resolving twice posts no second note.
	if err := h.ch.Resolve(context.Background(), self, req, channels.Resolution{Outcome: "answered", Note: "again"}); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.callsTo(toolSend)); n != len(sends) {
		t.Errorf("second Resolve posted again")
	}

	fake.human(dm, reqTS, "too late")
	h.poll()
	h.none()
	sends = fake.callsTo(toolSend)
	last = sends[len(sends)-1].Args
	if last["thread_ts"] != reqTS || last["message"] != "This request is already closed (answered via tui)." {
		t.Errorf("late reply answer = %v", last)
	}
	// The bot's own answer is not mistaken for a human reply.
	h.poll()
	h.none()
	if n := len(fake.callsTo(toolSend)); n != len(sends) {
		t.Errorf("answered the late reply twice")
	}
}

func TestReactionPerOutcome(t *testing.T) {
	for outcome, emoji := range map[string]string{"answered": "white_check_mark", "expired": "hourglass", "cancelled": "no_entry_sign"} {
		fake := newFakeSlack()
		h := newHarness(t, fake, "")
		req := h.send(request("q"))
		if err := h.ch.Resolve(context.Background(), self, req, channels.Resolution{Outcome: outcome, Note: "n"}); err != nil {
			t.Fatal(err)
		}
		_, ts := SplitMessageID(req)
		if p := fake.find(dm, ts); len(p.reactions) != 1 || p.reactions[0] != emoji {
			t.Errorf("%s: reactions = %v, want %s", outcome, p.reactions, emoji)
		}
	}
}

func TestUnparsableChannelFallsBackToPerThreadPolling(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, "")
	req := h.send(request("q"))
	_, reqTS := SplitMessageID(req)
	fake.mu.Lock()
	fake.garble = true
	fake.mu.Unlock()
	fake.human(dm, reqTS, "staging")
	h.poll()
	if m := h.next(); m.Text != "staging" || m.ReplyToID != req {
		t.Fatalf("inbound = %+v", m)
	}
	if !h.ch.fallback {
		t.Error("fallback not set")
	}
	// Each poll reads every watched thread while in fallback.
	n := len(fake.callsTo(toolThread))
	h.poll()
	if got := len(fake.callsTo(toolThread)) - n; got != 1 {
		t.Errorf("read_thread calls in fallback = %d, want 1", got)
	}
	fake.mu.Lock()
	fake.garble = false
	fake.mu.Unlock()
	h.poll()
	if h.ch.fallback {
		t.Error("fallback not cleared once read_channel parses")
	}
	h.none()
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeSlack()
	h := newHarness(t, fake, dir)
	req := h.send(request("q"))
	_, reqTS := SplitMessageID(req)
	if err := h.ch.Close(); err != nil {
		t.Fatal(err)
	}

	fake.human(dm, reqTS, "answered while the daemon was down")
	h2 := newHarness(t, fake, dir)
	if m := h2.next(); m.Text != "answered while the daemon was down" || m.ReplyToID != req {
		t.Fatalf("inbound after restart = %+v", m)
	}
	h2.none()
}

func TestToolNamesResolveBySuffix(t *testing.T) {
	fake := newFakeSlack()
	fake.prefix = "other-"
	h := newHarness(t, fake, "")
	h.send(channels.OutboundMessage{Text: "x"})
	if c := fake.callsTo(toolSend); c[0].Tool != "other-slack_send_message" {
		t.Errorf("tool = %q", c[0].Tool)
	}
}

func TestSendWithoutSlackToolFails(t *testing.T) {
	c := New("slack", testConfig(), noTools{newFakeSlack()}, "", nil)
	if err := c.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, err := c.Send(context.Background(), channels.OutboundMessage{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "serves no slack_send_message") {
		t.Fatalf("Send = %v", err)
	}
}

type noTools struct{ *fakeSlack }

func (noTools) ListTools(context.Context) ([]mcpgateway.Tool, error) { return nil, nil }

// emptyGateway serves no tools until it is reset.
type emptyGateway struct {
	*fakeSlack
	mu     sync.Mutex
	resets int
}

func (g *emptyGateway) ListTools(ctx context.Context) ([]mcpgateway.Tool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resets == 0 {
		return nil, nil
	}
	return g.fakeSlack.ListTools(ctx)
}

func (g *emptyGateway) ResetGateway(context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resets++
}

func TestGatewayWithoutToolsIsReset(t *testing.T) {
	gw := &emptyGateway{fakeSlack: newFakeSlack()}
	c := New("slack", testConfig(), gw, "", nil)
	c.firstPoll = time.Hour
	if err := c.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Send(context.Background(), channels.OutboundMessage{Text: "x"}); err == nil {
		t.Fatal("Send on an empty gateway succeeded")
	}
	if gw.resets != 1 {
		t.Fatalf("resets = %d, want 1", gw.resets)
	}
	if _, err := c.Send(context.Background(), channels.OutboundMessage{Text: "x"}); err != nil {
		t.Fatalf("Send after reset = %v", err)
	}
}

func TestSendBeforeStartOrAfterClose(t *testing.T) {
	c := New("slack", testConfig(), newFakeSlack(), "", nil)
	if _, err := c.Send(context.Background(), channels.OutboundMessage{Text: "x"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("Send before Start = %v", err)
	}
	h := newHarness(t, newFakeSlack(), "")
	_ = h.ch.Close()
	if _, err := h.ch.Send(context.Background(), channels.OutboundMessage{Text: "x"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("Send after Close = %v", err)
	}
	if err := h.ch.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

// TestLostSendResponseIsReconciled covers a send that was posted but whose
// response was lost (the gateway dropped the stream, as seen live): the
// channel finds the post in Slack, returns its real id, and never takes its
// echo for a human reply.
func TestLostSendResponseIsReconciled(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, "")
	req := h.send(request("q"))
	_, reqTS := SplitMessageID(req)

	fake.mu.Lock()
	fake.loseResponses = 1
	fake.mu.Unlock()
	err := h.ch.Resolve(context.Background(), self, req, channels.Resolution{Outcome: "answered", Via: "tui", Note: "✅ [req r1t8w] Answered via tui: staging"})
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if p := fake.find(dm, reqTS); len(p.reactions) != 1 {
		t.Errorf("reactions = %v, want the check mark", p.reactions)
	}
	h.poll()
	h.none()

	// A lost top-level send is reconciled too, with its real id.
	fake.mu.Lock()
	fake.loseResponses = 1
	fake.mu.Unlock()
	id := h.send(channels.OutboundMessage{Text: "status: all green"})
	if ch, ts := SplitMessageID(id); ch != dm || fake.find(dm, ts) == nil {
		t.Errorf("reconciled id = %q", id)
	}
	h.poll()
	h.none()
}

// TestUnconfirmedSendEchoIsIgnored covers a lost send that cannot be found
// right away: the send fails, and when the post does show up it is ignored.
func TestUnconfirmedSendEchoIsIgnored(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, "")
	req := h.send(request("q"))
	_, reqTS := SplitMessageID(req)

	fake.mu.Lock()
	fake.loseResponses, fake.hideLost = 1, true
	fake.mu.Unlock()
	_, err := h.ch.Send(context.Background(), channels.OutboundMessage{ReplyToID: req, Text: "✅ Sent to task fix-ci"})
	if err == nil {
		t.Fatal("Send of unknown outcome succeeded")
	}
	fake.reveal()
	fake.human(dm, reqTS, "staging")
	h.poll()
	if m := h.next(); m.Text != "staging" {
		t.Fatalf("inbound = %+v", m)
	}
	h.none()
	if n := len(h.ch.st.watch.Unconfirmed); n != 0 {
		t.Errorf("%d unconfirmed sends left", n)
	}
}

func TestEchoKey(t *testing.T) {
	sent := "❓ [req live01] task slack-channel (workingman-messaging)\nLive test — please ignore."
	back := ":question: [req live01] task slack-channel (workingman-messaging)\nLive test \u2014 please ignore."
	if echoKey(sent) != echoKey(back) {
		t.Errorf("echoKey(%q) = %q != echoKey(%q) = %q", sent, echoKey(sent), back, echoKey(back))
	}
	if echoKey("yes") == echoKey("no") {
		t.Error("different texts share a key")
	}
}
