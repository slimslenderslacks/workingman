package acpchat

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/acpclient"
	"github.com/slimslenderslacks/work/internal/session"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// tuiClient plays the TUI: the first client, which creates the ACP session.
func tuiClient(t *testing.T, a *fakeAgent) *acpclient.Client {
	t.Helper()
	ctx := testCtx(t)
	c, err := acpclient.Dial(ctx, a.socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Connect(ctx, "/ws"); err != nil {
		t.Fatal(err)
	}
	// Drain so the client's event buffer never fills.
	go func() {
		for range c.Events() {
		}
	}()
	return c
}

// seedSession creates the ACP session the way the TUI does, then hangs up, so a
// test that needs the conversation to exist has no second client answering the
// agent's requests (the first reply wins at a real agent).
func seedSession(t *testing.T, a *fakeAgent) {
	t.Helper()
	tuiClient(t, a).Close()
	waitFor(t, "the seeding client to be dropped", func() bool { return a.numClients() == 0 })
}

func attach(t *testing.T, a *fakeAgent) *Conversation {
	t.Helper()
	before := a.numClients()
	conv, err := Attach(testCtx(t), a.id, a.options())
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { conv.Close() })
	// Dial returns once the kernel accepts; the fake registers the client for
	// fan-out a moment later, and frames broadcast before that are (as with the
	// real bridge) not delivered to it.
	waitFor(t, "the fake to register our client", func() bool { return a.numClients() > before })
	return conv
}

func nextEvent(t *testing.T, ch <-chan Event, want EventKind) Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("events closed while waiting for kind %d", want)
			}
			if ev.Kind == want {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for event kind %d", want)
		}
	}
}

// The conversation joins the session the TUI created: ids come from stream.log,
// and the only thing we put on the wire is session/prompt — no initialize,
// session/new or session/load — under a request id far from the TUI's.
func TestAttachAdoptsExistingSessionFromStreamLog(t *testing.T) {
	a := newFakeAgent(t)
	tui := tuiClient(t, a)
	if _, err := tui.Prompt(testCtx(t), "opening"); err != nil {
		t.Fatal(err)
	}
	before := len(a.requests())

	conv := attach(t, a)
	if got := conv.SessionID(); got != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", got)
	}
	reply, err := conv.Ask(testCtx(t), "what is running?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if reply != "echo: what is running?" {
		t.Errorf("reply = %q", reply)
	}

	ours := a.requests()[before:]
	if len(ours) != 1 || ours[0].Method != "session/prompt" || ours[0].SessionID != "sess-1" {
		t.Fatalf("our wire traffic = %+v, want exactly one session/prompt to sess-1", ours)
	}
	if ours[0].ID < 1<<20 {
		t.Errorf("request id %d collides with the ordinary id range", ours[0].ID)
	}
}

func TestAttachByDirectoryPath(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	conv, err := Attach(testCtx(t), a.dir(), a.options())
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	if conv.ID() != a.id || conv.SessionID() != "sess-1" {
		t.Errorf("ID=%q SessionID=%q", conv.ID(), conv.SessionID())
	}
}

// A turn the TUI user started is still streaming when our Ask goes out; the
// agent finishes it first. Its tail must not leak into our reply, and it still
// reaches Events as another client's turn.
func TestForeignTurnTailDoesNotLeakIntoReply(t *testing.T) {
	a := newFakeAgent(t)
	tui := tuiClient(t, a)
	var turnMu sync.Mutex // one turn at a time per session, like a real agent
	release := make(chan struct{})
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {
		turnMu.Lock()
		defer turnMu.Unlock()
		if text == "from tui" {
			a.chunk(sid, "tui part 1;")
			<-release
			a.chunk(sid, "tui part 2")
		} else {
			a.chunk(sid, "echo: "+text)
		}
		a.result(id, map[string]any{"stopReason": "end_turn"})
	}
	conv := attach(t, a)
	evs := conv.Events()

	tuiDone := make(chan error, 1)
	go func() { _, err := tui.Prompt(testCtx(t), "from tui"); tuiDone <- err }()
	if ev := nextEvent(t, evs, EventText); ev.Own || ev.Text != "tui part 1;" {
		t.Fatalf("first event = %+v", ev)
	}

	type res struct {
		reply string
		err   error
	}
	asked := make(chan res, 1)
	go func() {
		r, err := conv.Ask(testCtx(t), "from channel")
		asked <- res{r, err}
	}()
	waitFor(t, "our prompt to reach the agent", func() bool {
		n := 0
		for _, r := range a.requests() {
			if r.Method == "session/prompt" {
				n++
			}
		}
		return n == 2
	})
	close(release)

	r := <-asked
	if r.err != nil || r.reply != "echo: from channel" {
		t.Errorf("Ask = %q, %v; want only our own turn's text", r.reply, r.err)
	}
	if err := <-tuiDone; err != nil {
		t.Errorf("tui prompt: %v", err)
	}
	end := nextEvent(t, evs, EventTurnEnd)
	if end.Own || end.Text != "tui part 1;tui part 2" {
		t.Errorf("foreign turn end = %+v", end)
	}
}

func TestAskReturnsErrBusyWhileTurnInFlight(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	release := make(chan struct{})
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {
		if text == "slow" {
			<-release
		}
		a.chunk(sid, "done "+text)
		a.result(id, map[string]any{"stopReason": "end_turn"})
	}
	conv := attach(t, a)

	first := make(chan string, 1)
	go func() {
		r, err := conv.Ask(testCtx(t), "slow")
		if err != nil {
			t.Errorf("first Ask: %v", err)
		}
		first <- r
	}()
	waitFor(t, "first prompt to reach the agent", func() bool {
		for _, r := range a.requests() {
			if r.Method == "session/prompt" {
				return true
			}
		}
		return false
	})
	if _, err := conv.Ask(testCtx(t), "second"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Ask error = %v, want ErrBusy", err)
	}
	close(release)
	if got := <-first; got != "done slow" {
		t.Errorf("first reply = %q", got)
	}
	if r, err := conv.Ask(testCtx(t), "third"); err != nil || r != "done third" {
		t.Errorf("Ask after the turn ended = %q, %v", r, err)
	}
}

func TestReplyStripsToolNoiseAndThoughts(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {
		a.thought(sid, "secret reasoning")
		a.chunk(sid, "Let me ")
		a.chunk(sid, "check.")
		a.toolCall(sid, "t1")
		a.chunk(sid, "Two projects ")
		a.chunk(sid, "are open.")
		a.result(id, map[string]any{"stopReason": "end_turn"})
	}
	conv := attach(t, a)

	var mu sync.Mutex
	var deltas []string
	var last string
	reply, err := conv.Ask(testCtx(t), "status?", OnProgress(func(delta, soFar string) {
		mu.Lock()
		deltas = append(deltas, delta)
		last = soFar
		mu.Unlock()
	}))
	if err != nil {
		t.Fatal(err)
	}
	const want = "Let me check.\n\nTwo projects are open."
	if reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(deltas, "|") != "Let me |check.|Two projects |are open." || last != want {
		t.Errorf("progress deltas=%v last=%q", deltas, last)
	}

	final, err := conv.Ask(testCtx(t), "status?", FinalSegmentOnly())
	if err != nil || final != "Two projects are open." {
		t.Errorf("FinalSegmentOnly = %q, %v", final, err)
	}
}

// Observe-only: what the agent says in a turn another client started is relayed
// as Own=false events, closed by a TurnEnd carrying the whole reply.
func TestEventsRelayTurnsStartedByOtherClients(t *testing.T) {
	a := newFakeAgent(t)
	tui := tuiClient(t, a)
	conv := attach(t, a)
	evs := conv.Events()

	if _, err := tui.Prompt(testCtx(t), "hello wolf"); err != nil {
		t.Fatal(err)
	}
	txt := nextEvent(t, evs, EventText)
	if txt.Own || txt.Text != "echo: hello wolf" {
		t.Errorf("text event = %+v", txt)
	}
	end := nextEvent(t, evs, EventTurnEnd)
	if end.Own || end.Text != "echo: hello wolf" || end.StopReason != "end_turn" {
		t.Errorf("turn end = %+v", end)
	}

	// Our own turn is flagged Own so a relay can skip it.
	if _, err := conv.Ask(testCtx(t), "mine"); err != nil {
		t.Fatal(err)
	}
	txt = nextEvent(t, evs, EventText)
	if !txt.Own {
		t.Errorf("own turn's text event not flagged Own: %+v", txt)
	}
	if end := nextEvent(t, evs, EventTurnEnd); !end.Own || end.Text != "echo: mine" {
		t.Errorf("own turn end = %+v", end)
	}
}

func permissionAgent(a *fakeAgent) {
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {
		a.send(map[string]any{"jsonrpc": "2.0", "id": 777, "method": "session/request_permission",
			"params": map[string]any{
				"sessionId": sid,
				"toolCall":  map[string]any{"toolCallId": "t9", "title": "Run `git push`", "kind": "execute"},
				"options": []any{
					map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
					map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"},
				},
			}})
		reply := <-a.replies
		a.chunk(sid, string(reply.Result))
		a.result(id, map[string]any{"stopReason": "end_turn"})
	}
}

func TestPermissionRequestsRejectedByDefault(t *testing.T) {
	a := newFakeAgent(t)
	seedSession(t, a)
	permissionAgent(a)
	conv := attach(t, a)
	evs := conv.Events()

	reply, err := conv.Ask(testCtx(t), "push it")
	if err != nil {
		t.Fatal(err)
	}
	if reply != `{"outcome":{"optionId":"no","outcome":"selected"}}` {
		t.Errorf("agent got %s, want the reject option", reply)
	}
	ev := nextEvent(t, evs, EventPermission)
	if ev.Permission == nil || ev.Permission.Title != "Run `git push`" || len(ev.Permission.Options) != 2 {
		t.Errorf("permission event = %+v", ev.Permission)
	}
}

func TestOnPermissionDecides(t *testing.T) {
	a := newFakeAgent(t)
	seedSession(t, a)
	permissionAgent(a)
	opts := a.options()
	var asked PermissionRequest
	opts.OnPermission = func(ctx context.Context, req PermissionRequest) PermissionDecision {
		asked = req
		return req.Allow()
	}
	conv, err := Attach(testCtx(t), a.id, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()

	reply, err := conv.Ask(testCtx(t), "push it")
	if err != nil {
		t.Fatal(err)
	}
	if reply != `{"outcome":{"optionId":"yes","outcome":"selected"}}` {
		t.Errorf("agent got %s, want the allow option", reply)
	}
	if asked.ToolCallID != "t9" || asked.SessionID != "sess-1" {
		t.Errorf("OnPermission saw %+v", asked)
	}
}

func TestPermissionTimeoutRejects(t *testing.T) {
	a := newFakeAgent(t)
	seedSession(t, a)
	permissionAgent(a)
	opts := a.options()
	opts.PermissionTimeout = 50 * time.Millisecond
	opts.OnPermission = func(ctx context.Context, req PermissionRequest) PermissionDecision {
		<-ctx.Done() // the human never answers
		return req.Allow()
	}
	conv, err := Attach(testCtx(t), a.id, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	reply, err := conv.Ask(testCtx(t), "push it")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, `"optionId":"no"`) {
		t.Errorf("agent got %s, want a rejection after the timeout", reply)
	}
}

func TestPermissionDecisionHelpers(t *testing.T) {
	req := PermissionRequest{Options: []PermissionOption{{ID: "a", Kind: "allow_always"}, {ID: "r", Kind: "reject_always"}}}
	if req.Allow().OptionID != "a" || req.Reject().OptionID != "r" {
		t.Errorf("allow=%+v reject=%+v", req.Allow(), req.Reject())
	}
	if (PermissionRequest{}).Reject().OptionID != "" {
		t.Error("no options: Reject should be the zero (cancelled) decision")
	}
}

// A dropped socket (the bridge evicts slow watchers) is repaired transparently:
// same ACP session, no new handshake.
func TestReconnectsAfterSocketDrops(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	conv := attach(t, a)
	evs := conv.Events()
	if _, err := conv.Ask(testCtx(t), "one"); err != nil {
		t.Fatal(err)
	}
	before := len(a.requests())

	a.dropClients()
	nextEvent(t, evs, EventReconnecting)
	re := nextEvent(t, evs, EventReconnected)
	if re.SessionChanged || re.SessionID != "sess-1" {
		t.Errorf("reconnected event = %+v", re)
	}

	reply, err := conv.Ask(testCtx(t), "two")
	if err != nil || reply != "echo: two" {
		t.Fatalf("Ask after reconnect = %q, %v", reply, err)
	}
	for _, r := range a.requests()[before:] {
		if r.Method != "session/prompt" {
			t.Errorf("reconnect re-ran %s", r.Method)
		}
	}
}

func TestAskWaitsOutReconnect(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	conv := attach(t, a)
	a.dropClients()
	// Immediately: the drop may not even be noticed yet, or the reconnect may be
	// mid-flight — either way Ask must succeed rather than fail on a dead socket.
	waitFor(t, "ask to succeed across a drop", func() bool {
		r, err := conv.Ask(testCtx(t), "x")
		return err == nil && r == "echo: x"
	})
}

func TestMidTurnDropReturnsPartialTextAndConnectionError(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {
		if text == "doomed" {
			a.chunk(sid, "half an ans")
			time.Sleep(50 * time.Millisecond)
			a.dropClients()
			return
		}
		a.chunk(sid, "echo: "+text)
		a.result(id, map[string]any{"stopReason": "end_turn"})
	}
	conv := attach(t, a)

	reply, err := conv.Ask(testCtx(t), "doomed")
	if !errors.Is(err, acpclient.ErrConnectionClosed) {
		t.Fatalf("err = %v, want ErrConnectionClosed", err)
	}
	if reply != "half an ans" {
		t.Errorf("partial reply = %q", reply)
	}
	// The session is alive, so the conversation recovers.
	waitFor(t, "recovery", func() bool {
		r, err := conv.Ask(testCtx(t), "again")
		return err == nil && r == "echo: again"
	})
}

func TestSessionGoneWhenDirectoryRemoved(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	conv := attach(t, a)
	evs := conv.Events()

	a.stop()
	if err := os.RemoveAll(a.dir()); err != nil {
		t.Fatal(err)
	}
	gone := nextEvent(t, evs, EventGone)
	if !errors.Is(gone.Err, ErrSessionGone) {
		t.Errorf("gone err = %v", gone.Err)
	}
	if _, err := conv.Ask(testCtx(t), "hello?"); !errors.Is(err, ErrSessionGone) {
		t.Errorf("Ask after gone = %v, want ErrSessionGone", err)
	}
	select {
	case _, ok := <-evs:
		if ok {
			t.Error("events not closed after EventGone")
		}
	case <-time.After(2 * time.Second):
		t.Error("events channel never closed")
	}
}

func TestSessionGoneWhenStatusExited(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	conv := attach(t, a)
	a.writeSession(session.StatusExited)
	a.stop()
	waitFor(t, "ErrSessionGone", func() bool {
		_, err := conv.Ask(testCtx(t), "x")
		return errors.Is(err, ErrSessionGone)
	})
}

func TestSessionGoneWhenSocketStaysUnreachable(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	opts := a.options()
	opts.ConnectTimeout = 150 * time.Millisecond
	conv, err := Attach(testCtx(t), a.id, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	a.stop() // dir and session.json remain: a wedged wrapper
	waitFor(t, "ErrSessionGone", func() bool {
		_, err := conv.Ask(testCtx(t), "x")
		return errors.Is(err, ErrSessionGone)
	})
}

func TestAttachMissingSessionIsGone(t *testing.T) {
	root := t.TempDir()
	_, err := Attach(testCtx(t), "nope", Options{Root: root})
	if !errors.Is(err, ErrSessionGone) {
		t.Errorf("err = %v, want ErrSessionGone", err)
	}
}

func TestAttachWaitsForStartingSessionToRun(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	a.writeSession(session.StatusStarting)
	go func() {
		time.Sleep(100 * time.Millisecond)
		a.writeSession(session.StatusRunning)
	}()
	conv, err := Attach(testCtx(t), a.id, a.options())
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	conv.Close()
}

// A brand-new session nobody has talked to: Attach waits for the TUI's
// session/new to show up on the live socket instead of failing.
func TestAttachDiscoversSessionFromLiveStream(t *testing.T) {
	a := newFakeAgent(t)
	type res struct {
		conv *Conversation
		err  error
	}
	out := make(chan res, 1)
	go func() {
		c, err := Attach(testCtx(t), a.id, a.options())
		out <- res{c, err}
	}()
	time.Sleep(100 * time.Millisecond)
	tuiClient(t, a)
	r := <-out
	if r.err != nil {
		t.Fatalf("Attach: %v", r.err)
	}
	defer r.conv.Close()
	if got := r.conv.SessionID(); got != "sess-1" {
		t.Errorf("SessionID = %q", got)
	}
	if reply, err := r.conv.Ask(testCtx(t), "hi"); err != nil || reply != "echo: hi" {
		t.Errorf("Ask = %q, %v", reply, err)
	}
}

func TestAttachWithoutSessionIsErrNoSession(t *testing.T) {
	a := newFakeAgent(t)
	opts := a.options()
	opts.DiscoverTimeout = 100 * time.Millisecond
	if _, err := Attach(testCtx(t), a.id, opts); !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestAdoptedSessionIsNotCreated(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	if conv := attach(t, a); conv.Created() {
		t.Error("Created() = true for a session another client had already created")
	}
}

func TestCreateIfMissingRunsHandshake(t *testing.T) {
	a := newFakeAgent(t)
	opts := a.options()
	opts.DiscoverTimeout = 100 * time.Millisecond
	opts.CreateIfMissing = true
	conv, err := Attach(testCtx(t), a.id, opts)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer conv.Close()
	var methods []string
	for _, r := range a.requests() {
		methods = append(methods, r.Method)
	}
	if strings.Join(methods, ",") != "initialize,session/new,session/set_mode" {
		t.Errorf("handshake = %v", methods)
	}
	if !conv.Created() {
		t.Error("Created() = false for a session the conversation created itself")
	}
	if reply, err := conv.Ask(testCtx(t), "hi"); err != nil || reply != "echo: hi" {
		t.Errorf("Ask = %q, %v", reply, err)
	}
}

// If the TUI starts a fresh ACP session (its restart path re-runs session/new),
// the conversation follows it.
func TestFollowsNewSessionCreatedByAnotherClient(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	conv := attach(t, a)
	evs := conv.Events()
	if conv.SessionID() != "sess-1" {
		t.Fatalf("SessionID = %q", conv.SessionID())
	}

	tuiClient(t, a) // a second session/new → sess-2
	ev := nextEvent(t, evs, EventSessionChanged)
	if ev.SessionID != "sess-2" {
		t.Errorf("changed to %q", ev.SessionID)
	}
	before := len(a.requests())
	if _, err := conv.Ask(testCtx(t), "x"); err != nil {
		t.Fatal(err)
	}
	if got := a.requests()[before]; got.Method != "session/prompt" || got.SessionID != "sess-2" {
		t.Errorf("prompt went to %+v, want sess-2", got)
	}
}

func TestChunksOfOtherSessionsAreIgnored(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {
		a.chunk("some-other-session", "NOT OURS")
		a.chunk(sid, "ours")
		a.result(id, map[string]any{"stopReason": "end_turn"})
	}
	conv := attach(t, a)
	if reply, err := conv.Ask(testCtx(t), "x"); err != nil || reply != "ours" {
		t.Errorf("reply = %q, %v", reply, err)
	}
}

func TestCancelledAskCancelsTheTurn(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {
		a.chunk(sid, "working")
		// never finishes
	}
	conv := attach(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	reply, err := conv.Ask(ctx, "long")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if reply != "working" {
		t.Errorf("partial = %q", reply)
	}
	select {
	case sid := <-a.cancels:
		if sid != "sess-1" {
			t.Errorf("cancelled %q", sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent never received session/cancel")
	}
	// And the conversation is usable again.
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {
		a.chunk(sid, "back")
		a.result(id, map[string]any{"stopReason": "end_turn"})
	}
	if r, err := conv.Ask(testCtx(t), "next"); err != nil || r != "back" {
		t.Errorf("Ask after cancel = %q, %v", r, err)
	}
}

func TestRefusalWithoutTextIsAnError(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {
		a.result(id, map[string]any{"stopReason": "refusal"})
	}
	conv := attach(t, a)
	if _, err := conv.Ask(testCtx(t), "x"); !errors.Is(err, ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
}

func TestCloseUnblocksAskAndClosesEvents(t *testing.T) {
	a := newFakeAgent(t)
	tuiClient(t, a)
	a.onPrompt = func(a *fakeAgent, id int, sid, text string) {} // hangs
	conv := attach(t, a)
	evs := conv.Events()

	errc := make(chan error, 1)
	go func() { _, err := conv.Ask(testCtx(t), "x"); errc <- err }()
	waitFor(t, "prompt in flight", func() bool {
		for _, r := range a.requests() {
			if r.Method == "session/prompt" {
				return true
			}
		}
		return false
	})
	conv.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("Ask after Close = %v, want ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask still blocked after Close")
	}
	select {
	case _, ok := <-evs:
		for ok {
			_, ok = <-evs
		}
	case <-time.After(2 * time.Second):
		t.Error("events not closed after Close")
	}
	if _, err := conv.Ask(testCtx(t), "y"); !errors.Is(err, ErrClosed) {
		t.Errorf("Ask on closed = %v", err)
	}
}

func TestLastSessionIDInLog(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/stream.log"
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := lastSessionIDInLog(path); got != "" {
		t.Errorf("missing log: %q", got)
	}
	write(`{"id":1,"result":{"protocolVersion":1}}` + "\n" +
		`{"id":2,"result":{"sessionId":"a"}}` + "\n" +
		`{"method":"session/update","params":{"sessionId":"a","update":{}}}` + "\n" +
		`not json` + "\n" +
		`{"id":9,"result":null}` + "\n" +
		`{"id":3,"result":{"sessionId":"b"}}`) // unterminated final frame still counts
	if got := lastSessionIDInLog(path); got != "b" {
		t.Errorf("got %q, want the newest id b", got)
	}
}
