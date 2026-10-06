package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/acpchat"
	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/channeltest"
)

const (
	chatID  = "15551234567"
	chanNm  = "wa"
	waitFor = 3 * time.Second
)

// --- fakes ---

// fakeAgent is a scriptable acpchat conversation.
type fakeAgent struct {
	mu     sync.Mutex
	asks   []string
	askFn  func(ctx context.Context, text string) (string, error)
	events chan acpchat.Event
	closed bool
	onPerm func(ctx context.Context, req acpchat.PermissionRequest) acpchat.PermissionDecision
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{events: make(chan acpchat.Event, 16)}
}

func (a *fakeAgent) Ask(ctx context.Context, text string, _ ...acpchat.AskOption) (string, error) {
	a.mu.Lock()
	a.asks = append(a.asks, text)
	fn := a.askFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, text)
	}
	return "echo: " + text, nil
}

func (a *fakeAgent) Events() <-chan acpchat.Event { return a.events }

func (a *fakeAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		close(a.events)
	}
	return nil
}

func (a *fakeAgent) Asks() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asks...)
}

func (a *fakeAgent) emit(ev acpchat.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.events <- ev
	}
}

// fakeSource is a mutable daemon.
type fakeSource struct {
	mu     sync.Mutex
	agent  *Session
	wolves []Session
	status string
}

func (s *fakeSource) WorkingmanAgent() (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil {
		return Session{}, false
	}
	return *s.agent, true
}

func (s *fakeSource) WolfSessions() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Session(nil), s.wolves...)
}

func (s *fakeSource) Status() string { return s.status }

func (s *fakeSource) setWolves(w ...Session) {
	s.mu.Lock()
	s.wolves = w
	s.mu.Unlock()
}

func (s *fakeSource) setAgent(a *Session) {
	s.mu.Lock()
	s.agent = a
	s.mu.Unlock()
}

// richChannel adds the optional Typer and Chunker behaviours to the channeltest fake.
type richChannel struct {
	*channeltest.Fake
	typing atomic.Int32
	chunk  func(string) []string
}

func (c *richChannel) Typing(context.Context, string) error { c.typing.Add(1); return nil }

func (c *richChannel) ChunkMessage(text string) []string {
	if c.chunk == nil {
		return nil
	}
	return c.chunk(text)
}

type auditBuf struct {
	mu    sync.Mutex
	lines []string
}

func (a *auditBuf) Log(event string, kv ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lines = append(a.lines, event+" "+strings.Join(kv, " "))
}

func (a *auditBuf) all() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.lines, "\n")
}

type harness struct {
	t      *testing.T
	r      *Router
	reg    *channels.Registry
	ch     *richChannel
	src    *fakeSource
	index  *channels.FileConversationIndex
	audit  *auditBuf
	mu     sync.Mutex
	agents map[string]*fakeAgent // by session Ref
	opts   map[string]acpchat.Options
	attach atomic.Int32
	nextID atomic.Int32
}

var agentSess = Session{Key: "workingman-agent", ID: "workingman-agent-1", Ref: "/sessions/workingman-agent-1"}

func wolfSess(ws string) Session {
	return Session{Key: "/orch/" + ws + "/.project.yaml#wolf", ID: "wolf-" + ws + "-1", Ref: "/sessions/wolf-" + ws + "-1", WorkStream: ws}
}

// newHarness builds a router over a started channeltest channel. The workingman
// agent is up unless a test says otherwise.
func newHarness(t *testing.T, mod func(*Config)) *harness {
	t.Helper()
	a := agentSess
	h := &harness{
		t:      t,
		src:    &fakeSource{agent: &a, status: "2 projects · 1 live session"},
		audit:  &auditBuf{},
		agents: map[string]*fakeAgent{},
		opts:   map[string]acpchat.Options{},
	}
	h.agents[agentSess.Ref] = newFakeAgent()
	idx, err := channels.NewFileConversationIndex("")
	if err != nil {
		t.Fatal(err)
	}
	h.index = idx
	h.ch = &richChannel{Fake: channeltest.New(chanNm)}
	h.reg = channels.NewRegistry()
	if err := h.reg.Add(h.ch); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Source: h.src, Out: h.reg, Index: idx, Audit: h.audit,
		Attach: func(_ context.Context, ref string, opts acpchat.Options) (Agent, error) {
			h.attach.Add(1)
			h.mu.Lock()
			defer h.mu.Unlock()
			ag, ok := h.agents[ref]
			if !ok {
				return nil, fmt.Errorf("%w: %s", acpchat.ErrSessionGone, ref)
			}
			h.opts[ref] = opts
			ag.onPerm = opts.OnPermission
			return ag, nil
		},
		ThinkingAfter: time.Hour, TypingEvery: time.Hour,
	}
	if mod != nil {
		mod(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.r = r
	if err := h.reg.Start(ctx, r.Handler()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.Close()
		cancel()
		_ = h.reg.Close()
	})
	return h
}

func (h *harness) addWolf(ws string) (Session, *fakeAgent) {
	s := wolfSess(ws)
	ag := newFakeAgent()
	h.mu.Lock()
	h.agents[s.Ref] = ag
	h.mu.Unlock()
	h.src.mu.Lock()
	h.src.wolves = append(h.src.wolves, s)
	h.src.mu.Unlock()
	return s, ag
}

func (h *harness) agentFake() *fakeAgent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agents[agentSess.Ref]
}

func (h *harness) say(text string) { h.sayReply(text, "") }

func (h *harness) sayReply(text, replyTo string) {
	h.t.Helper()
	id := fmt.Sprintf("in-%d", h.nextID.Add(1))
	if err := h.ch.Inject(channels.InboundMessage{ChatID: chatID, SenderID: chatID, MessageID: id, ReplyToID: replyTo, Text: text}); err != nil {
		h.t.Fatal(err)
	}
}

// sent returns every outbound message as the user would see it.
func (h *harness) sent() []string {
	var out []string
	for _, m := range h.ch.Sent() {
		out = append(out, channels.FormatTopic(m.Topic, m.Text))
	}
	return out
}

// wait blocks until at least n messages were sent and returns them all.
func (h *harness) wait(n int) []string {
	h.t.Helper()
	deadline := time.Now().Add(waitFor)
	for {
		if got := h.sent(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %d messages; got %q", n, h.sent())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle asserts no further message arrives, returning what was sent.
func (h *harness) settle() []string {
	time.Sleep(60 * time.Millisecond)
	return h.sent()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func contains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("got %q, want it to contain %q", got, want)
	}
}

// --- rule 3: default routing ---

func TestDefaultRoutesToWorkingmanAgent(t *testing.T) {
	h := newHarness(t, nil)
	h.say("what projects are open?")
	got := h.wait(1)
	if got[0] != "echo: what projects are open?" {
		t.Fatalf("reply = %q", got[0])
	}
	if asks := h.agentFake().Asks(); len(asks) != 1 || asks[0] != "what projects are open?" {
		t.Fatalf("asks = %q", asks)
	}
	if n := h.attach.Load(); n != 1 {
		t.Fatalf("attached %d times", n)
	}
	h.say("and tasks?")
	h.wait(2)
	if n := h.attach.Load(); n != 1 {
		t.Fatalf("second message re-attached (%d attaches)", n)
	}
}

// createdAgent is a fakeAgent whose conversation had to create the ACP session
// (nobody else was driving it), so it has not been given its opening prompt.
type createdAgent struct{ *fakeAgent }

func (createdAgent) Created() bool { return true }

// A workingman session nobody has started a conversation in (the daemon runs
// without the TUI) is created by the router, which then gives it its opening
// prompt before the first question. A wolf's session is never created here.
func TestCreatedWorkingmanSessionIsPrimedBeforeTheFirstQuestion(t *testing.T) {
	var opts []acpchat.Options
	var optsMu sync.Mutex
	var fake *fakeAgent
	h := newHarness(t, func(c *Config) {
		inner := c.Attach
		c.Attach = func(ctx context.Context, ref string, o acpchat.Options) (Agent, error) {
			ag, err := inner(ctx, ref, o)
			if err != nil {
				return nil, err
			}
			optsMu.Lock()
			opts = append(opts, o)
			fake = ag.(*fakeAgent)
			optsMu.Unlock()
			return createdAgent{fake}, nil
		}
	})
	h.say("what projects are open?")
	h.wait(1)
	asks := h.agentFake().Asks()
	if len(asks) != 2 || asks[0] != OpeningPrompt || asks[1] != "what projects are open?" {
		t.Fatalf("asks = %q, want the opening prompt then the question", asks)
	}
	if got := h.sent(); len(got) != 1 || got[0] != "echo: what projects are open?" {
		t.Fatalf("the opening prompt's reply leaked to the chat: %q", got)
	}
	optsMu.Lock()
	defer optsMu.Unlock()
	if len(opts) != 1 || !opts[0].CreateIfMissing {
		t.Fatalf("workingman attach options = %+v, want CreateIfMissing", opts)
	}

	// An existing conversation is not primed again, and a wolf is never created.
	h.say("and tasks?")
	h.wait(2)
	if asks := h.agentFake().Asks(); len(asks) != 3 {
		t.Fatalf("asks = %q, want no second opening prompt", asks)
	}
}

func TestWolfAttachDoesNotCreateTheSession(t *testing.T) {
	h := newHarness(t, nil)
	s, _ := h.addWolf("alpha")
	h.say("/wolf alpha")
	h.say("hello")
	h.wait(2)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.opts[s.Ref].CreateIfMissing {
		t.Fatal("a wolf's ACP session must be created by its TUI, not by the router")
	}
}

func TestEmptyAndPathLikeMessages(t *testing.T) {
	h := newHarness(t, nil)
	h.say("   ")
	h.say("/etc/hosts is odd")
	got := h.wait(1)
	if got[0] != "echo: /etc/hosts is odd" {
		t.Fatalf("path-like text must reach the agent, got %q", got[0])
	}
	if len(h.settle()) != 1 {
		t.Fatalf("blank message produced a reply: %q", h.sent())
	}
}

// --- rule 1: reply-to routing ---

func TestReplyToNotificationRoutesToThatWolf(t *testing.T) {
	h := newHarness(t, nil)
	ws, wolfA := h.addWolf("alpha")
	_, wolfB := h.addWolf("beta")
	if err := h.index.Register(chanNm, "notif-1", channels.ConversationTarget{
		Kind: channels.ConversationKindWolf, WorkStream: "alpha", SessionKey: ws.Key, SessionID: ws.ID,
	}); err != nil {
		t.Fatal(err)
	}

	h.sayReply("retry the failed task", "notif-1")
	got := h.wait(1)
	if got[0] != "[wolf alpha] echo: retry the failed task" {
		t.Fatalf("reply = %q", got[0])
	}
	if asks := wolfA.Asks(); len(asks) != 1 || asks[0] != "retry the failed task" {
		t.Fatalf("wolf alpha asks = %q", asks)
	}
	if len(wolfB.Asks()) != 0 || len(h.agentFake().Asks()) != 0 {
		t.Fatal("reply leaked to another agent")
	}

	// The relayed answer is itself a conversation: replying to it reaches alpha.
	sentMsg := h.ch.Sent()[0]
	if sentMsg.Topic != "wolf alpha" {
		t.Fatalf("topic = %q", sentMsg.Topic)
	}
	tgt, ok := h.index.Lookup(chanNm, chanNm+"-1")
	if !ok || tgt.SessionKey != ws.Key {
		t.Fatalf("relayed wolf reply not indexed: %+v %v", tgt, ok)
	}
	h.sayReply("and then?", chanNm+"-1")
	h.wait(2)
	if asks := wolfA.Asks(); len(asks) != 2 {
		t.Fatalf("follow-up did not reach alpha: %q", asks)
	}

	// A reply does not rebind the chat.
	h.say("plain question")
	got = h.wait(3)
	if got[2] != "echo: plain question" {
		t.Fatalf("after a reply-to the chat should still be on the agent, got %q", got[2])
	}
}

func TestReplyToEndedOrReplacedWolf(t *testing.T) {
	h := newHarness(t, nil)
	ws, _ := h.addWolf("alpha")
	reg := func(id string, sess Session) {
		_ = h.index.Register(chanNm, id, channels.ConversationTarget{Kind: channels.ConversationKindWolf, WorkStream: "alpha", SessionKey: sess.Key, SessionID: sess.ID})
	}
	reg("old", Session{Key: ws.Key, ID: "wolf-alpha-OLD"})
	h.sayReply("hello", "old")
	got := h.wait(1)
	contains(t, got[0], "earlier wolf session for alpha")

	h.src.setWolves() // the wolf ends
	reg("n2", ws)
	h.sayReply("hello?", "n2")
	got = h.wait(2)
	contains(t, got[1], "has ended")
	if len(h.agentFake().Asks()) != 0 {
		t.Fatal("an unroutable wolf reply must not be asked of the workingman agent")
	}

	// An unknown reply-to ID is just a message for the default agent.
	h.sayReply("hi", "who-knows")
	got = h.wait(3)
	if got[2] != "echo: hi" {
		t.Fatalf("got %q", got[2])
	}
}

// --- rule 2: commands ---

func TestCommands(t *testing.T) {
	h := newHarness(t, nil)

	h.say("/help")
	h.say("/STATUS")
	h.say("/who")
	h.say("/wolf")
	h.say("/bogus")
	got := h.wait(5)
	contains(t, got[0], "/wolf [work-stream]")
	contains(t, got[0], "/agent")
	if got[1] != "2 projects · 1 live session" {
		t.Fatalf("/status = %q", got[1])
	}
	contains(t, got[2], "workingman agent")
	if got[3] != "No wolf running." {
		t.Fatalf("/wolf with none = %q", got[3])
	}
	contains(t, got[4], "Unknown command /bogus")
	if n := len(h.agentFake().Asks()); n != 0 || h.attach.Load() != 0 {
		t.Fatalf("commands must not reach an LLM (asks=%d attaches=%d)", n, h.attach.Load())
	}
}

func TestWolfCommandSelection(t *testing.T) {
	h := newHarness(t, nil)
	h.addWolf("alpha")
	h.addWolf("alphabet")
	h.addWolf("beta")
	hostWolf := Session{Key: "k-host#wolf", ID: "wolf-host", WorkStream: "hosty"} // Ref "": tmux wolf
	h.src.mu.Lock()
	h.src.wolves = append(h.src.wolves, hostWolf)
	h.src.mu.Unlock()

	h.say("/wolf")
	h.say("/wolf gamma")
	h.say("/wolf alp")
	h.say("/wolf hosty")
	got := h.wait(4)
	contains(t, got[0], "Several wolves are running: alpha, alphabet, beta, hosty")
	contains(t, got[1], `No wolf running for "gamma"`)
	contains(t, got[2], "matches several wolves")
	contains(t, got[3], "runs on the host")

	h.say("/wolf BETA") // exact match wins, case-insensitively
	got = h.wait(5)
	contains(t, got[4], "Now talking to the wolf for beta")
	h.say("/wolf alpha") // exact beats the longer "alphabet"
	got = h.wait(6)
	contains(t, got[5], "Now talking to the wolf for alpha")
}

// --- rule 3/4: binding lifecycle ---

func TestBindingLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	ws, wolf := h.addWolf("alpha")

	h.say("/wolf")
	got := h.wait(1)
	contains(t, got[0], "Now talking to the wolf for alpha")
	eventually(t, "wolf attach", func() bool { return h.attach.Load() == 1 })

	h.say("/who")
	got = h.wait(2)
	contains(t, got[1], "wolf for alpha")

	h.say("how is it going")
	got = h.wait(3)
	if got[2] != "[wolf alpha] echo: how is it going" {
		t.Fatalf("got %q", got[2])
	}
	if len(h.agentFake().Asks()) != 0 {
		t.Fatal("bound chat must not reach the workingman agent")
	}

	h.say("/agent")
	h.say("and now?")
	got = h.wait(5)
	contains(t, got[3], "workingman agent")
	if got[4] != "echo: and now?" {
		t.Fatalf("after /agent: %q", got[4])
	}

	// Rebind, then the wolf session ends: fall back and tell the user.
	h.say("/wolf alpha")
	h.wait(6)
	h.src.setWolves()
	h.say("anyone there?")
	got = h.wait(7)
	contains(t, got[6], "The wolf for alpha has ended")
	contains(t, got[6], "workingman agent")
	if asks := h.agentFake().Asks(); len(asks) != 1 {
		t.Fatalf("message to an ended wolf was forwarded to the agent: %q", asks)
	}
	h.say("now the agent")
	got = h.wait(8)
	if got[7] != "echo: now the agent" {
		t.Fatalf("after fall-back: %q", got[7])
	}
	_ = ws
	_ = wolf
}

func TestWolfSessionEndsWhileBound(t *testing.T) {
	h := newHarness(t, nil)
	_, wolf := h.addWolf("alpha")
	h.say("/wolf")
	h.wait(1)
	eventually(t, "attach", func() bool { return h.attach.Load() == 1 })

	wolf.emit(acpchat.Event{Kind: acpchat.EventGone, Err: acpchat.ErrSessionGone})
	got := h.wait(2)
	contains(t, got[1], "The wolf for alpha has ended")

	h.say("next")
	got = h.wait(3)
	if got[2] != "echo: next" {
		t.Fatalf("got %q", got[2])
	}
}

func TestWolfRelaunchedUnderSameKey(t *testing.T) {
	h := newHarness(t, nil)
	ws, _ := h.addWolf("alpha")
	h.say("/wolf")
	h.wait(1)
	eventually(t, "attach", func() bool { return h.attach.Load() == 1 })

	// New session id under the same key: a different conversation.
	next := ws
	next.ID, next.Ref = "wolf-alpha-2", "/sessions/wolf-alpha-2"
	h.mu.Lock()
	h.agents[next.Ref] = newFakeAgent()
	h.mu.Unlock()
	h.src.setWolves(next)

	h.say("hello")
	got := h.wait(2)
	contains(t, got[1], "has ended")

	h.say("/wolf")
	h.say("hello again")
	got = h.wait(4)
	if got[3] != "[wolf alpha] echo: hello again" {
		t.Fatalf("got %q", got[3])
	}
	eventually(t, "old conversation closed", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		old := h.agents[ws.Ref]
		old.mu.Lock()
		defer old.mu.Unlock()
		return old.closed
	})
}

// --- rule 4: observe mode ---

func TestObserveRelaysForeignTurnsToBoundChat(t *testing.T) {
	h := newHarness(t, nil)
	_, wolf := h.addWolf("alpha")
	h.say("/wolf")
	h.wait(1)
	eventually(t, "attach", func() bool { return h.attach.Load() == 1 })

	wolf.emit(acpchat.Event{Kind: acpchat.EventTurnEnd, Own: true, Text: "own turn, already answered"})
	wolf.emit(acpchat.Event{Kind: acpchat.EventTurnEnd, Own: false, Text: "I restarted the task for you"})
	wolf.emit(acpchat.Event{Kind: acpchat.EventTurnEnd, Own: false, Text: "   "})
	got := h.wait(2)
	if got[1] != "[wolf alpha] I restarted the task for you" {
		t.Fatalf("got %q", got[1])
	}
	if got = h.settle(); len(got) != 2 {
		t.Fatalf("own/blank turns were relayed: %q", got)
	}

	// Once the chat is back on the agent the wolf's chatter stays quiet.
	h.say("/agent")
	h.wait(3)
	wolf.emit(acpchat.Event{Kind: acpchat.EventTurnEnd, Text: "tui chatter"})
	if got = h.settle(); len(got) != 3 {
		t.Fatalf("unbound chat saw wolf output: %q", got)
	}
}

// --- rule 4: permission round trip ---

func permReq() acpchat.PermissionRequest {
	return acpchat.PermissionRequest{
		SessionID: "s1", ToolCallID: "t1", Title: "Run `git push`",
		Options: []acpchat.PermissionOption{
			{ID: "allow-1", Name: "Allow once", Kind: "allow_once"},
			{ID: "allow-all", Name: "Always allow", Kind: "allow_always"},
			{ID: "reject-1", Name: "Reject", Kind: "reject_once"},
		},
	}
}

// askPerm runs the wolf's OnPermission as the acpchat reader would.
func askPerm(h *harness, ref string) <-chan acpchat.PermissionDecision {
	h.mu.Lock()
	fn := h.opts[ref].OnPermission
	h.mu.Unlock()
	out := make(chan acpchat.PermissionDecision, 1)
	go func() { out <- fn(context.Background(), permReq()) }()
	return out
}

func decisionOf(t *testing.T, ch <-chan acpchat.PermissionDecision) acpchat.PermissionDecision {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(waitFor):
		t.Fatal("permission decision never arrived")
		return acpchat.PermissionDecision{}
	}
}

func TestPermissionRoundTripWhileBound(t *testing.T) {
	for _, tc := range []struct{ answer, want string }{
		{"yes", "allow-1"},
		{"Yes!", "allow-1"},
		{"no", "reject-1"},
		{"always", "allow-all"},
		{"3", "reject-1"},
		{"Always allow", "allow-all"},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			h := newHarness(t, nil)
			ws, _ := h.addWolf("alpha")
			h.say("/wolf")
			h.wait(1)
			eventually(t, "attach", func() bool { return h.attach.Load() == 1 })

			dec := askPerm(h, ws.Ref) // the TUI user's turn asks; nobody has an Ask in flight
			got := h.wait(2)
			contains(t, got[1], "[wolf alpha] 🔐 Permission needed: Run `git push`")
			contains(t, got[1], "1) Allow once")
			contains(t, got[1], "Reply yes or no")

			h.say(tc.answer)
			if d := decisionOf(t, dec); d.OptionID != tc.want {
				t.Fatalf("decision = %q, want %q", d.OptionID, tc.want)
			}
			got = h.wait(3)
			if !strings.HasPrefix(got[2], "👍") && !strings.HasPrefix(got[2], "🚫") {
				t.Fatalf("no confirmation: %q", got[2])
			}
			// The answer was consumed, not forwarded as a prompt.
			if asks := h.agents[ws.Ref].Asks(); len(asks) != 0 {
				t.Fatalf("answer reached the wolf as a prompt: %q", asks)
			}
		})
	}
}

func TestPermissionDuringOwnTurnBypassesTheQueue(t *testing.T) {
	h := newHarness(t, nil)
	ws, wolf := h.addWolf("alpha")
	var decided atomic.Value
	wolf.askFn = func(ctx context.Context, text string) (string, error) {
		h.mu.Lock()
		fn := h.opts[ws.Ref].OnPermission
		h.mu.Unlock()
		d := fn(ctx, permReq())
		decided.Store(d.OptionID)
		return "done: " + d.OptionID, nil
	}
	h.say("/wolf")
	h.wait(1)
	h.say("push it")
	got := h.wait(2)
	contains(t, got[1], "Permission needed")
	// The turn is blocked on the human; the answer must still get through.
	h.say("yes")
	got = h.wait(4)
	contains(t, got[2], "Allowed")
	if got[3] != "[wolf alpha] done: allow-1" {
		t.Fatalf("got %q", got[3])
	}
}

func TestPermissionAmbiguousAnswerRePrompts(t *testing.T) {
	h := newHarness(t, nil)
	ws, _ := h.addWolf("alpha")
	h.say("/wolf")
	h.wait(1)
	eventually(t, "attach", func() bool { return h.attach.Load() == 1 })
	dec := askPerm(h, ws.Ref)
	h.wait(2)

	h.say("hmm, what would that do?")
	got := h.wait(3)
	contains(t, got[2], "Please reply yes or no")
	select {
	case d := <-dec:
		t.Fatalf("ambiguous answer resolved the request: %+v", d)
	case <-time.After(50 * time.Millisecond):
	}
	h.say("n")
	if d := decisionOf(t, dec); d.OptionID != "reject-1" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestPermissionTimesOutRejected(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PermissionTimeout = 80 * time.Millisecond })
	ws, _ := h.addWolf("alpha")
	h.say("/wolf")
	h.wait(1)
	eventually(t, "attach", func() bool { return h.attach.Load() == 1 })

	h.mu.Lock()
	opts := h.opts[ws.Ref]
	h.mu.Unlock()
	if opts.PermissionTimeout != 80*time.Millisecond {
		t.Fatalf("PermissionTimeout not passed to Attach: %v", opts.PermissionTimeout)
	}
	// acpchat bounds OnPermission's context by PermissionTimeout; emulate it.
	ctx, cancel := context.WithTimeout(context.Background(), opts.PermissionTimeout)
	defer cancel()
	d := opts.OnPermission(ctx, permReq())
	if d.OptionID != "reject-1" {
		t.Fatalf("timeout decision = %+v, want reject", d)
	}
	got := h.wait(3)
	contains(t, got[2], "No answer in time")

	// A late "yes" is just a message now, not a stale approval.
	h.say("yes")
	got = h.wait(4)
	if got[3] != "[wolf alpha] echo: yes" {
		t.Fatalf("got %q", got[3])
	}
}

func TestPermissionWithNobodyToAskIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	ws, _ := h.addWolf("alpha")
	// Attach by replying to a notification, then leave: no binding, no turn.
	_ = h.index.Register(chanNm, "n1", channels.ConversationTarget{Kind: channels.ConversationKindWolf, WorkStream: "alpha", SessionKey: ws.Key, SessionID: ws.ID})
	h.sayReply("hi", "n1")
	h.wait(1)

	dec := askPerm(h, ws.Ref)
	if d := decisionOf(t, dec); d.OptionID != "reject-1" {
		t.Fatalf("decision = %+v, want reject", d)
	}
	if got := h.settle(); len(got) != 1 {
		t.Fatalf("a message went out with nobody bound: %q", got)
	}
}

// --- rule 5: robustness ---

func TestBusyChatQueuesAndAcks(t *testing.T) {
	h := newHarness(t, nil)
	gate := make(chan struct{})
	h.agentFake().askFn = func(ctx context.Context, text string) (string, error) {
		if text == "first" {
			<-gate
		}
		return "re: " + text, nil
	}
	h.say("first")
	eventually(t, "first ask in flight", func() bool { return len(h.agentFake().Asks()) == 1 })
	h.say("second")
	h.say("third")
	got := h.wait(2)
	contains(t, got[0], "busy")
	contains(t, got[0], "#1")
	contains(t, got[1], "#2")

	close(gate)
	got = h.wait(5)
	want := []string{"re: first", "re: second", "re: third"}
	if got[2] != want[0] || got[3] != want[1] || got[4] != want[2] {
		t.Fatalf("answers out of order: %q", got[2:])
	}
	if asks := h.agentFake().Asks(); strings.Join(asks, ",") != "first,second,third" {
		t.Fatalf("asks = %q", asks)
	}
}

func TestQueueIsBounded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.QueueSize = 1 })
	gate := make(chan struct{})
	h.agentFake().askFn = func(ctx context.Context, text string) (string, error) {
		<-gate
		return "re: " + text, nil
	}
	h.say("a")
	eventually(t, "a in flight", func() bool { return len(h.agentFake().Asks()) == 1 })
	h.say("b") // queued
	h.say("c") // over the bound
	got := h.wait(2)
	contains(t, got[0], "queued")
	contains(t, got[1], "queue is full")
	close(gate)
	got = h.wait(4)
	if got[2] != "re: a" || got[3] != "re: b" {
		t.Fatalf("got %q", got)
	}
	if got = h.settle(); len(got) != 4 {
		t.Fatalf("dropped message was answered: %q", got)
	}
}

func TestCommandsAnswerWhileATurnRuns(t *testing.T) {
	h := newHarness(t, nil)
	gate := make(chan struct{})
	h.agentFake().askFn = func(ctx context.Context, text string) (string, error) {
		<-gate
		return "late", nil
	}
	h.say("slow question")
	eventually(t, "ask in flight", func() bool { return len(h.agentFake().Asks()) == 1 })
	h.say("/status")
	got := h.wait(1)
	if got[0] != "2 projects · 1 live session" {
		t.Fatalf("/status = %q", got[0])
	}
	close(gate)
	h.wait(2)
}

func TestSlowTurnGetsTypingAndThinkingAck(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.ThinkingAfter = 40 * time.Millisecond
		c.TypingEvery = 20 * time.Millisecond
	})
	h.agentFake().askFn = func(ctx context.Context, text string) (string, error) {
		time.Sleep(200 * time.Millisecond)
		return "finally", nil
	}
	h.say("hard question")
	got := h.wait(2)
	if got[0] != "…thinking" || got[1] != "finally" {
		t.Fatalf("got %q", got)
	}
	if n := h.ch.typing.Load(); n < 2 {
		t.Fatalf("typing indicator shown %d times, want a refreshing indicator", n)
	}
	if got = h.settle(); len(got) != 2 {
		t.Fatalf("extra messages after the reply: %q", got)
	}
}

func TestFastTurnGetsNoThinkingAck(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ThinkingAfter = 500 * time.Millisecond })
	h.say("quick")
	h.wait(1)
	if got := h.settle(); len(got) != 1 {
		t.Fatalf("got %q", got)
	}
}

func TestTurnTimeoutIsFriendly(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.TurnTimeout = 60 * time.Millisecond })
	h.agentFake().askFn = func(ctx context.Context, text string) (string, error) {
		<-ctx.Done()
		return "partial thoughts", ctx.Err()
	}
	h.say("never ends")
	got := h.wait(1)
	contains(t, got[0], "took longer than")
	contains(t, got[0], "cancelled")
	contains(t, got[0], "partial thoughts")

	// The chat is usable afterwards.
	h.agentFake().mu.Lock()
	h.agentFake().askFn = nil
	h.agentFake().mu.Unlock()
	h.say("again")
	got = h.wait(2)
	if got[1] != "echo: again" {
		t.Fatalf("got %q", got[1])
	}
}

func TestAgentDownRepliesImmediately(t *testing.T) {
	h := newHarness(t, nil)
	h.src.setAgent(nil)
	start := time.Now()
	h.say("anyone home?")
	got := h.wait(1)
	if got[0] != AgentStartingMessage {
		t.Fatalf("got %q", got[0])
	}
	if time.Since(start) > time.Second {
		t.Fatal("agent-down reply was not immediate")
	}
	if h.attach.Load() != 0 {
		t.Fatal("router tried to attach to an agent that is down")
	}

	// It comes back: the very next message works.
	a := agentSess
	h.src.setAgent(&a)
	h.say("ok now?")
	got = h.wait(2)
	if got[1] != "echo: ok now?" {
		t.Fatalf("got %q", got[1])
	}
}

func TestAgentSessionUnreachableMeansStartingUp(t *testing.T) {
	h := newHarness(t, nil)
	h.mu.Lock()
	delete(h.agents, agentSess.Ref) // up per the daemon, but the attach fails
	h.mu.Unlock()
	h.say("hello")
	got := h.wait(1)
	if got[0] != AgentStartingMessage {
		t.Fatalf("got %q", got[0])
	}
}

func TestAgentRestartReattaches(t *testing.T) {
	h := newHarness(t, nil)
	h.say("one")
	h.wait(1)
	first := h.agentFake()

	next := agentSess
	next.ID, next.Ref = "workingman-agent-2", "/sessions/workingman-agent-2"
	second := newFakeAgent()
	h.mu.Lock()
	h.agents[next.Ref] = second
	h.mu.Unlock()
	h.src.setAgent(&next)

	h.say("two")
	got := h.wait(2)
	if got[1] != "echo: two" || len(second.Asks()) != 1 || len(first.Asks()) != 1 {
		t.Fatalf("got %q; first=%q second=%q", got, first.Asks(), second.Asks())
	}
	eventually(t, "old agent detached", func() bool {
		first.mu.Lock()
		defer first.mu.Unlock()
		return first.closed
	})
}

func TestSessionGoneMidTurnDropsConversation(t *testing.T) {
	h := newHarness(t, nil)
	h.agentFake().askFn = func(ctx context.Context, text string) (string, error) {
		return "", fmt.Errorf("%w: gone", acpchat.ErrSessionGone)
	}
	h.say("hi")
	got := h.wait(1)
	if got[0] != AgentStartingMessage {
		t.Fatalf("got %q", got[0])
	}
	h.agentFake().mu.Lock()
	h.agentFake().askFn = nil
	h.agentFake().mu.Unlock()
	h.say("hi again")
	h.wait(2)
	if h.attach.Load() != 2 {
		t.Fatalf("expected a re-attach after the session was lost, attaches=%d", h.attach.Load())
	}
}

func TestOtherAskErrors(t *testing.T) {
	h := newHarness(t, nil)
	h.agentFake().askFn = func(ctx context.Context, text string) (string, error) {
		switch text {
		case "refuse":
			return "", acpchat.ErrRefused
		}
		return "", errors.New("boom token=supersecretvalue123")
	}
	h.say("refuse")
	h.wait(1)
	h.say("break")
	got := h.wait(2)
	contains(t, got[0], "declined")
	contains(t, got[1], "Something went wrong")
	if strings.Contains(got[1], "supersecretvalue123") {
		t.Fatalf("error leaked a secret: %q", got[1])
	}
}

// --- rule 5: reply shaping ---

func TestLongReplyIsSplitByTheChannelsChunker(t *testing.T) {
	h := newHarness(t, nil)
	h.ch.chunk = func(s string) []string {
		var out []string
		for len(s) > 10 {
			out, s = append(out, s[:10]), s[10:]
		}
		return append(out, s)
	}
	reply := strings.Repeat("abcdefghij", 3) + "xyz"
	h.agentFake().askFn = func(context.Context, string) (string, error) { return reply, nil }
	h.say("long please")
	got := h.wait(4)
	if strings.Join(got[:4], "") != reply || len(got) != 4 {
		t.Fatalf("chunks = %q", got)
	}
}

func TestChannelWithoutChunkerGetsTheWholeReply(t *testing.T) {
	h := newHarness(t, nil) // richChannel.ChunkMessage returns nil: "I chunk inside Send"
	reply := strings.Repeat("word ", 2000)
	h.agentFake().askFn = func(context.Context, string) (string, error) { return reply, nil }
	h.say("long")
	got := h.wait(1)
	if len(got) != 1 || got[0] != strings.TrimSpace(reply) && got[0] != reply {
		t.Fatalf("got %d messages of len %d", len(got), len(got[0]))
	}
}

func TestReplyIsCappedAndRedacted(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxReplyRunes = 100 })
	h.agentFake().askFn = func(context.Context, string) (string, error) {
		return "env dump: GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789 and password: hunter2\n" + strings.Repeat("x", 500), nil
	}
	h.say("dump env")
	got := h.wait(1)
	if strings.Contains(got[0], "ghp_abc") || strings.Contains(got[0], "hunter2") {
		t.Fatalf("secret echoed to the chat: %q", got[0])
	}
	contains(t, got[0], "[REDACTED]")
	contains(t, got[0], "reply cut")
	if len([]rune(got[0])) > 200 {
		t.Fatalf("reply not capped: %d runes", len([]rune(got[0])))
	}
}

func TestEmptyReplyGetsAPlaceholder(t *testing.T) {
	h := newHarness(t, nil)
	h.agentFake().askFn = func(context.Context, string) (string, error) { return "  ", nil }
	h.say("hm")
	contains(t, h.wait(1)[0], "without a text reply")
}

// --- rule 6: audit ---

func TestAuditNeverHoldsMessageTextOrRawChatID(t *testing.T) {
	h := newHarness(t, nil)
	h.say("the launch codes are 0000")
	h.say("/wolf")
	h.say("/status")
	h.wait(3)
	log := h.audit.all()
	if log == "" {
		t.Fatal("nothing audit-logged")
	}
	for _, leaked := range []string{"launch codes", "0000", chatID} {
		if strings.Contains(log, leaked) {
			t.Fatalf("audit log leaks %q:\n%s", leaked, log)
		}
	}
	contains(t, log, "router_inbound chat "+chatRef(chanNm, chatID))
	contains(t, log, "router_route")
	contains(t, log, "router_command")
}

// --- misc ---

func TestChatsAreIndependent(t *testing.T) {
	h := newHarness(t, nil)
	h.addWolf("alpha")
	h.say("/wolf")
	h.wait(1)
	if err := h.ch.Inject(channels.InboundMessage{ChatID: "15559999999", MessageID: "x", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	got := h.wait(2)
	if got[1] != "echo: hello" {
		t.Fatalf("a second chat inherited the first chat's wolf binding: %q", got[1])
	}
}

func TestNewRequiresSourceAndOut(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("want an error")
	}
}

func TestParseCommand(t *testing.T) {
	for in, want := range map[string]string{
		"/help": "help", "/WOLF x": "wolf", "/status   ": "status",
	} {
		if name, _, ok := parseCommand(strings.TrimSpace(in)); !ok || name != want {
			t.Errorf("parseCommand(%q) = %q, %v", in, name, ok)
		}
	}
	for _, in := range []string{"hello", "/etc/hosts", "/", "/a/b c", "//x"} {
		if _, _, ok := parseCommand(in); ok {
			t.Errorf("parseCommand(%q) treated as a command", in)
		}
	}
	if _, args, _ := parseCommand("/wolf  my-proj "); args != "my-proj" {
		t.Errorf("args = %q", args)
	}
}
