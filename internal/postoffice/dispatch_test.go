package postoffice

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/channeltest"
)

// resolverChannel adds channels.Resolver to the channeltest fake.
type resolverChannel struct {
	*channeltest.Fake
	mu       sync.Mutex
	resolved []string // "chat|message|outcome|via|note"
}

func (c *resolverChannel) Resolve(_ context.Context, chatID, messageID string, r channels.Resolution) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolved = append(c.resolved, strings.Join([]string{chatID, messageID, r.Outcome, r.Via, r.Note}, "|"))
	return nil
}

func (c *resolverChannel) Resolved() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.resolved...)
}

// chunkChannel adds channels.Chunker to the channeltest fake.
type chunkChannel struct {
	*channeltest.Fake
	size int
}

func (c *chunkChannel) ChunkMessage(text string) []string {
	var out []string
	for len(text) > c.size {
		out = append(out, text[:c.size])
		text = text[c.size:]
	}
	return append(out, text)
}

type memAudit struct {
	mu    sync.Mutex
	lines []string
}

func (a *memAudit) Log(event string, kv ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lines = append(a.lines, event+" "+strings.Join(kv, " "))
}

func (a *memAudit) has(event string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.lines {
		if strings.HasPrefix(l, event+" ") {
			return true
		}
	}
	return false
}

type dispatchEnv struct {
	t     *testing.T
	s     *Store
	clk   *clock
	reg   *channels.Registry
	index *channels.FileConversationIndex
	audit *memAudit
	d     *Dispatcher
}

// newDispatchEnv starts a Registry over chans with routes and builds a
// Dispatcher over a fresh store, sharing the store's test clock.
func newDispatchEnv(t *testing.T, routes []channels.Route, chans ...channels.Channel) *dispatchEnv {
	t.Helper()
	s, clk := newStore(t)
	reg := channels.NewRegistry()
	for _, ch := range chans {
		if err := reg.Add(ch); err != nil {
			t.Fatal(err)
		}
	}
	reg.SetRoutes(routes)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = reg.Close() })
	if err := reg.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	idx, err := channels.NewFileConversationIndex("")
	if err != nil {
		t.Fatal(err)
	}
	env := &dispatchEnv{t: t, s: s, clk: clk, reg: reg, index: idx, audit: &memAudit{}}
	env.d, err = NewDispatcher(DispatcherConfig{Store: s, Out: reg, Index: idx, Audit: env.audit, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *dispatchEnv) sync() { e.d.Sync(context.Background()) }

func (e *dispatchEnv) record(id string) DeliveryRecord {
	e.t.Helper()
	r, ok, err := e.s.Deliveries(id)
	if err != nil || !ok {
		e.t.Fatalf("deliveries %s: ok=%v err=%v", id, ok, err)
	}
	return r
}

// close claims r and finalises it, as post-office would.
func (e *dispatchEnv) close(r Resolution) {
	e.t.Helper()
	if _, err := e.s.Claim(r); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.Finalize(r.ID); err != nil {
		e.t.Fatal(err)
	}
}

func routes(topic string, chans ...string) []channels.Route {
	var out []channels.Route
	for _, c := range chans {
		out = append(out, channels.Route{Topic: topic, Channel: c, Chat: "chat-" + c})
	}
	return out
}

func TestDispatchSendsRequestToEveryRouteAndIndexesIt(t *testing.T) {
	sig, wa := channeltest.New("signal"), channeltest.New("wa")
	e := newDispatchEnv(t, routes(WildcardForTest, "signal", "wa"), sig, wa)
	m := putRequest(t, e.s, "staging", "prod")

	e.sync()
	for _, f := range []*channeltest.Fake{sig, wa} {
		sent := f.Sent()
		if len(sent) != 1 {
			t.Fatalf("%s sent %d messages, want 1", f.Name(), len(sent))
		}
		got := sent[0]
		if got.Topic != Topic || got.ChatID != "chat-"+f.Name() {
			t.Errorf("%s: topic %q chat %q", f.Name(), got.Topic, got.ChatID)
		}
		for _, want := range []string{"❓ [req " + m.ShortID() + "]", "task fix-ci (p)", "Which bucket?", "Options: 1) staging  2) prod", "#" + m.ShortID(), "Expires in 24h"} {
			if !strings.Contains(got.Text, want) {
				t.Errorf("%s text missing %q:\n%s", f.Name(), want, got.Text)
			}
		}
		ct, ok := e.index.Lookup(f.Name(), f.Name()+"-1")
		if !ok || ct.Kind != channels.ConversationKindPostOffice || ct.SessionKey != m.ID {
			t.Errorf("%s: index = %+v, %v", f.Name(), ct, ok)
		}
	}
	rec := e.record(m.ID)
	if got := rec.DeliveredTo(); strings.Join(got, ",") != "signal,wa" {
		t.Errorf("DeliveredTo = %v", got)
	}
	for _, dl := range rec.Deliveries {
		if dl.Status != DeliverySent || dl.MessageID != dl.Channel+"-1" || dl.SentAt == nil {
			t.Errorf("delivery %+v", dl)
		}
	}

	// A second pass sends nothing new.
	e.sync()
	if len(sig.Sent()) != 1 || len(wa.Sent()) != 1 {
		t.Fatalf("resent: signal %d, wa %d", len(sig.Sent()), len(wa.Sent()))
	}
}

// WildcardForTest is the wildcard topic: post-office traffic with no
// post-office route goes to the "*" routes.
const WildcardForTest = channels.WildcardTopic

func TestDispatchPrefersPostOfficeRoutes(t *testing.T) {
	sig, wa := channeltest.New("signal"), channeltest.New("wa")
	rts := append(routes(WildcardForTest, "signal"), routes(Topic, "wa")...)
	e := newDispatchEnv(t, rts, sig, wa)
	putRequest(t, e.s)
	e.sync()
	if len(sig.Sent()) != 0 || len(wa.Sent()) != 1 {
		t.Fatalf("signal %d, wa %d; want only the post-office route", len(sig.Sent()), len(wa.Sent()))
	}
}

func TestDispatchSkipsAgentRecipientsClosedAndExpired(t *testing.T) {
	sig := channeltest.New("signal")
	e := newDispatchEnv(t, routes(Topic, "signal"), sig)

	toAgent, err := NewMessage(Draft{Kind: KindRequest, From: sender, Recipient: "step-up", Body: "network please"}, e.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.Put(toAgent); err != nil {
		t.Fatal(err)
	}
	claimed := putRequest(t, e.s)
	if _, err := e.s.Claim(answer(claimed.ID, "done", ViaTUI)); err != nil {
		t.Fatal(err)
	}
	m, err := NewMessage(Draft{Kind: KindRequest, From: sender, Body: "soon gone", ExpiresIn: time.Minute}, e.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.Put(m); err != nil {
		t.Fatal(err)
	}
	e.clk.Add(2 * time.Minute)

	e.sync()
	if n := len(sig.Sent()); n != 0 {
		t.Fatalf("sent %d messages, want none: %+v", n, sig.Sent())
	}
	for _, id := range []string{toAgent.ID, claimed.ID, m.ID} {
		if _, ok, _ := e.s.Deliveries(id); ok {
			t.Errorf("%s has a delivery record", id)
		}
	}
}

func TestDispatchNotificationsHonourNotifyFalse(t *testing.T) {
	sig, wa := channeltest.New("signal"), channeltest.New("wa")
	off := false
	rts := []channels.Route{
		{Topic: Topic, Channel: "signal", Chat: "chat-signal"},
		{Topic: Topic, Channel: "wa", Chat: "chat-wa", Notify: &off},
	}
	e := newDispatchEnv(t, rts, sig, wa)
	n := putNotification(t, e.s)
	r := putRequest(t, e.s)
	e.sync()

	if len(sig.Sent()) != 2 {
		t.Fatalf("signal got %d, want the notification and the request", len(sig.Sent()))
	}
	if !strings.HasPrefix(sig.Sent()[0].Text, "📣 [note "+n.ShortID()+"]") || strings.Contains(sig.Sent()[0].Text, "Expires") {
		t.Errorf("notification text:\n%s", sig.Sent()[0].Text)
	}
	if len(wa.Sent()) != 1 || !strings.Contains(wa.Sent()[0].Text, r.ShortID()) {
		t.Fatalf("wa got %+v, want only the request", wa.Sent())
	}
	// Notifications are not indexed: a reply to one is not an answer.
	if _, ok := e.index.Lookup("signal", "signal-1"); ok {
		t.Error("notification was registered in the conversation index")
	}

	// Dismissing a notification sends no closing note.
	e.close(Resolution{ID: n.ID, Outcome: OutcomeDismissed, Responder: &Responder{Kind: ResponderOperator, Via: ViaTUI}})
	e.sync()
	if len(sig.Sent()) != 2 {
		t.Fatalf("closing note sent for a notification: %+v", sig.Sent()[2:])
	}
	if e.record(n.ID).ClosedNotified == nil {
		t.Error("notification record not marked closed_notified")
	}
}

func TestDispatchRetriesWithBackoffThenFails(t *testing.T) {
	sig := channeltest.New("signal")
	var fail sync.Mutex
	failing := true
	sig.SendFunc = func(ctx context.Context, msg channels.OutboundMessage) (string, error) {
		fail.Lock()
		defer fail.Unlock()
		if failing {
			return "", errors.New("signal-cli down")
		}
		return "ok-1", nil
	}
	wa := channeltest.New("wa")
	e := newDispatchEnv(t, routes(Topic, "signal", "wa"), sig, wa)
	m := putRequest(t, e.s)

	e.sync()
	rec := e.record(m.ID)
	if rec.Deliveries[0].Status != DeliveryPending || rec.Deliveries[0].Attempts != 1 || rec.Deliveries[0].LastError == "" {
		t.Fatalf("after first failure: %+v", rec.Deliveries[0])
	}
	if rec.Deliveries[1].Status != DeliverySent {
		t.Fatalf("the healthy channel was held up: %+v", rec.Deliveries[1])
	}
	if !e.audit.has("postoffice_delivery_error") {
		t.Error("no delivery_error audit line")
	}

	// Not due yet: no attempt.
	e.clk.Add(10 * time.Second)
	e.sync()
	if got := e.record(m.ID).Deliveries[0].Attempts; got != 1 {
		t.Fatalf("attempted before the backoff: %d", got)
	}
	// Due after 30s; still failing.
	e.clk.Add(25 * time.Second)
	e.sync()
	if got := e.record(m.ID).Deliveries[0].Attempts; got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	// Recovery on the third attempt (2m later).
	fail.Lock()
	failing = false
	fail.Unlock()
	e.clk.Add(2 * time.Minute)
	e.sync()
	dl := e.record(m.ID).Deliveries[0]
	if dl.Status != DeliverySent || dl.MessageID != "ok-1" || dl.Attempts != 3 || dl.LastError != "" {
		t.Fatalf("after recovery: %+v", dl)
	}
	if len(wa.Sent()) != 1 {
		t.Fatalf("wa resent: %d", len(wa.Sent()))
	}
}

func TestDispatchGivesUpAfterLastBackoff(t *testing.T) {
	sig := channeltest.New("signal")
	sig.SendErr = errors.New("nope")
	e := newDispatchEnv(t, routes(Topic, "signal"), sig)
	m := putRequest(t, e.s)
	for _, step := range []time.Duration{0, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour} {
		e.clk.Add(step)
		e.sync()
	}
	dl := e.record(m.ID).Deliveries[0]
	if dl.Status != DeliveryFailed || dl.Attempts != 4 {
		t.Fatalf("delivery %+v, want failed after 4 attempts", dl)
	}
	if n := len(sig.Sent()); n != 4 {
		t.Fatalf("%d send attempts, want 4", n)
	}
}

func TestDispatchClosingNotesSkipTheAnsweringChat(t *testing.T) {
	sig, wa := channeltest.New("signal"), channeltest.New("wa")
	e := newDispatchEnv(t, routes(Topic, "signal", "wa"), sig, wa)
	m := putRequest(t, e.s, "staging", "prod")
	e.sync()

	e.close(Resolution{ID: m.ID, Outcome: OutcomeAnswered, Response: "1",
		Responder: &Responder{Kind: ResponderOperator, Via: "wa", Chat: "chat-wa"}})
	e.sync()

	if len(wa.Sent()) != 1 {
		t.Fatalf("the answering channel got a closing note: %+v", wa.Sent()[1:])
	}
	sent := sig.Sent()
	if len(sent) != 2 {
		t.Fatalf("signal got %d messages, want request + closing note", len(sent))
	}
	note := sent[1]
	if note.ReplyToID != "signal-1" || note.ChatID != "chat-signal" {
		t.Errorf("closing note not a reply to the request: %+v", note)
	}
	if want := "✅ [req " + m.ShortID() + "] Answered via wa: staging"; note.Text != want {
		t.Errorf("note = %q, want %q", note.Text, want)
	}
	rec := e.record(m.ID)
	if rec.ClosedNotified == nil || rec.Deliveries[0].Closed != CloseSent || rec.Deliveries[1].Closed != CloseAnswerer {
		t.Fatalf("record after close: %+v", rec)
	}

	// Idempotent.
	e.sync()
	if len(sig.Sent()) != 2 {
		t.Fatal("closing note sent twice")
	}
}

func TestDispatchClosingNotesForTUIAnswerCancelAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  func(id string) Resolution
		want string
	}{
		{"tui", func(id string) Resolution { return answer(id, "Use the staging bucket.", ViaTUI) }, "Answered via tui: Use the staging bucket."},
		{"cancel", func(id string) Resolution {
			return Resolution{ID: id, Outcome: OutcomeCancelled, Reason: "found it in the docs"}
		}, "No longer needed — found it in the docs"},
		{"expire", func(id string) Resolution { return Resolution{ID: id, Outcome: OutcomeExpired} }, "Expired — the agent stopped waiting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig := channeltest.New("signal")
			rv := &resolverChannel{Fake: channeltest.New("slack")}
			e := newDispatchEnv(t, routes(Topic, "signal", "slack"), sig, rv)
			m := putRequest(t, e.s)
			e.sync()
			e.close(tc.res(m.ID))
			e.sync()

			sent := sig.Sent()
			if len(sent) != 2 || !strings.Contains(sent[1].Text, tc.want) {
				t.Fatalf("signal = %+v, want a note containing %q", sent, tc.want)
			}
			// The Resolver channel is resolved, not replied to.
			if len(rv.Sent()) != 1 {
				t.Fatalf("resolver channel got a plain reply: %+v", rv.Sent()[1:])
			}
			got := rv.Resolved()
			if len(got) != 1 || !strings.HasPrefix(got[0], "chat-slack|slack-1|") || !strings.Contains(got[0], tc.want) {
				t.Fatalf("resolved = %v", got)
			}
		})
	}
}

func TestDispatchClosingWhileRetryingSkipsPending(t *testing.T) {
	sig := channeltest.New("signal")
	sig.SendErr = errors.New("down")
	wa := channeltest.New("wa")
	e := newDispatchEnv(t, routes(Topic, "signal", "wa"), sig, wa)
	m := putRequest(t, e.s)
	e.sync()
	e.close(answer(m.ID, "ok", ViaTUI))
	e.clk.Add(time.Hour)
	e.sync()

	rec := e.record(m.ID)
	if rec.Deliveries[0].Status != DeliverySkipped || rec.Deliveries[1].Closed != CloseSent || rec.ClosedNotified == nil {
		t.Fatalf("record %+v", rec)
	}
	if n := len(sig.Sent()); n != 1 {
		t.Fatalf("signal attempted %d sends, want just the first", n)
	}
}

func TestDispatchRecoversOnStartup(t *testing.T) {
	sig := channeltest.New("signal")
	e := newDispatchEnv(t, routes(Topic, "signal"), sig)
	// Sent by an earlier daemon, then answered while it was down.
	done := putRequest(t, e.s)
	sent := e.clk.Now()
	if err := e.s.putDeliveries(DeliveryRecord{Version: DeliveriesVersion, ID: done.ID, Kind: KindRequest,
		Deliveries: []Delivery{{Channel: "signal", Chat: "chat-signal", MessageID: "old-7", Status: DeliverySent, SentAt: &sent}}}); err != nil {
		t.Fatal(err)
	}
	e.close(answer(done.ID, "fine", ViaTUI))
	// Arrived while the daemon was down.
	fresh := putRequest(t, e.s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.d.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for len(sig.Sent()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	got := sig.Sent()
	if len(got) != 2 {
		t.Fatalf("sent %+v, want a closing note and the new request", got)
	}
	var note, req bool
	for _, msg := range got {
		note = note || (msg.ReplyToID == "old-7" && strings.Contains(msg.Text, "Answered via tui"))
		req = req || strings.Contains(msg.Text, fresh.ShortID())
	}
	if !note || !req {
		t.Fatalf("note=%v req=%v: %+v", note, req, got)
	}
}

func TestDispatchWatchesInbox(t *testing.T) {
	sig := channeltest.New("signal")
	e := newDispatchEnv(t, routes(Topic, "signal"), sig)
	e.s.Now = nil // real time, so Watch and the clock agree
	e.d.cfg.Now = time.Now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	m := putRequest(t, e.s)
	deadline := time.Now().Add(DefaultInboxPoll + 3*time.Second)
	for len(sig.Sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(sig.Sent()) != 1 || !strings.Contains(sig.Sent()[0].Text, m.ShortID()) {
		t.Fatalf("sent %+v", sig.Sent())
	}
}

func TestDispatchChunksAndIndexesEveryPiece(t *testing.T) {
	ch := &chunkChannel{Fake: channeltest.New("signal"), size: 40}
	e := newDispatchEnv(t, routes(Topic, "signal"), ch)
	m := putRequest(t, e.s)
	e.sync()
	sent := ch.Sent()
	if len(sent) < 2 {
		t.Fatalf("not chunked: %+v", sent)
	}
	for i := range sent {
		id := "signal-" + string(rune('1'+i))
		if ct, ok := e.index.Lookup("signal", id); !ok || ct.SessionKey != m.ID {
			t.Errorf("piece %s not indexed", id)
		}
	}
	if got := e.record(m.ID).Deliveries[0].MessageID; got != "signal-1" {
		t.Errorf("MessageID = %q, want the first piece", got)
	}
}

func TestDispatchNoRoutesLeavesNoRecord(t *testing.T) {
	sig := channeltest.New("signal")
	e := newDispatchEnv(t, nil, sig)
	m := putRequest(t, e.s)
	e.sync()
	if _, ok, _ := e.s.Deliveries(m.ID); ok {
		t.Fatal("a delivery record was written with no routes")
	}
}

func TestRenderMessageRedactsAndClosingNoteCuts(t *testing.T) {
	now := time.Now().UTC()
	m, err := NewMessage(Draft{Kind: KindRequest, From: sender, Subject: "Token?", Body: "my key is ghp_abcdefghijklmnopqrstuvwxyz0123456789", ExpiresIn: 90 * time.Minute}, now)
	if err != nil {
		t.Fatal(err)
	}
	text := RenderMessage(m, now)
	if strings.Contains(text, "ghp_abcdef") {
		t.Errorf("secret not redacted:\n%s", text)
	}
	if !strings.Contains(text, "Expires in 2h") {
		t.Errorf("expiry:\n%s", text)
	}
	note := ClosingNote(m, answer(m.ID, strings.Repeat("x", 500), ViaTUI))
	if n := len([]rune(note)); n > 260 {
		t.Errorf("closing note is %d runes", n)
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second:   "1m",
		45 * time.Minute:   "45m",
		90 * time.Minute:   "2h",
		24 * time.Hour:     "24h",
		7 * 24 * time.Hour: "7d",
	} {
		if got := HumanDuration(d); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
