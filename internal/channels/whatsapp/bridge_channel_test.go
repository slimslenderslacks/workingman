package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp/bridge"
)

const fakeToken = "test-token"

// fakeBridge is an in-process stand-in for bridge.js: the same HTTP API, none
// of WhatsApp. Tests push events with push and inspect what was sent.
type fakeBridge struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	queue       []map[string]any
	notify      chan struct{}
	sent        []map[string]string
	typing      []string
	reads       []map[string]any
	connected   bool
	sendStatus  int // when non-zero, /send fails with it
	nextID      int
	polls       int
	sendHeaders []string
}

func newFakeBridge(t *testing.T) *fakeBridge {
	f := &fakeBridge{t: t, notify: make(chan struct{}, 1), connected: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBridge) push(ev map[string]any) {
	f.mu.Lock()
	f.queue = append(f.queue, ev)
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func (f *fakeBridge) setConnected(v bool) {
	f.mu.Lock()
	f.connected = v
	f.mu.Unlock()
}

func (f *fakeBridge) sentMessages() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.sent...)
}

func (f *fakeBridge) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fakeToken {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	switch r.URL.Path {
	case "/messages":
		f.mu.Lock()
		f.polls++
		f.mu.Unlock()
		deadline := time.After(2 * time.Second) // stands in for ?timeout
		for {
			f.mu.Lock()
			if len(f.queue) > 0 {
				q := f.queue
				f.queue = nil
				f.mu.Unlock()
				json.NewEncoder(w).Encode(q)
				return
			}
			f.mu.Unlock()
			select {
			case <-f.notify:
			case <-deadline:
				w.Write([]byte("[]"))
				return
			case <-r.Context().Done():
				return
			}
		}
	case "/send":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.connected {
			w.WriteHeader(503)
			w.Write([]byte(`{"error":"Not connected to WhatsApp"}`))
			return
		}
		if f.sendStatus != 0 {
			w.WriteHeader(f.sendStatus)
			w.Write([]byte(`{"error":"send failed","messageIds":["PARTIAL1"]}`))
			return
		}
		f.sent = append(f.sent, in)
		f.nextID++
		id := fmt.Sprintf("OUT%d", f.nextID)
		fmt.Fprintf(w, `{"success":true,"messageId":%q,"messageIds":[%q,%q]}`, id, id, id+"-b")
	case "/typing":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.typing = append(f.typing, in["chatId"])
		f.mu.Unlock()
		w.Write([]byte(`{"success":true}`))
	case "/read":
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.reads = append(f.reads, in)
		f.mu.Unlock()
		w.Write([]byte(`{"success":true,"marked":true}`))
	case "/health":
		f.mu.Lock()
		st := "disconnected"
		if f.connected {
			st = "connected"
		}
		f.mu.Unlock()
		fmt.Fprintf(w, `{"status":%q}`, st)
	default:
		http.NotFound(w, r)
	}
}

const (
	owner    = "15551234567@s.whatsapp.net"
	stranger = "15559998888@s.whatsapp.net"
)

func bdm(id, from, text string) map[string]any {
	return map[string]any{
		"messageId": id, "chatId": from, "senderId": from, "senderName": "Sam",
		"body": text, "fromMe": false, "timestamp": 1700000000,
		"botIds":         []string{"15550001111@s.whatsapp.net"},
		"readReceiptKey": map[string]any{"remoteJid": from, "id": id, "fromMe": false},
	}
}

func bfromMe(id, chat, text string) map[string]any {
	ev := bdm(id, chat, text)
	ev["fromMe"] = true
	return ev
}

type received struct {
	mu   sync.Mutex
	msgs []channels.InboundMessage
	ch   chan channels.InboundMessage
}

func newReceived() *received { return &received{ch: make(chan channels.InboundMessage, 32)} }

func (r *received) handler(_ context.Context, m channels.InboundMessage) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	r.ch <- m
}

func (r *received) next(t *testing.T) channels.InboundMessage {
	t.Helper()
	select {
	case m := <-r.ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an inbound message")
		return channels.InboundMessage{}
	}
}

func (r *received) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-r.ch:
		t.Fatalf("unexpected inbound message: %+v", m)
	case <-time.After(150 * time.Millisecond):
	}
}

// startChannel builds a BridgeChannel talking to fb, with a quiet logger and no
// real process: the fake bridge is "already running".
func startChannel(t *testing.T, fb *fakeBridge, access AccessConfig, mutate func(*BridgeChannel)) (*BridgeChannel, *received) {
	t.Helper()
	opts := BridgeOptions{SessionDir: t.TempDir(), InstallDir: t.TempDir()}
	ch, err := NewBridgeChannel("whatsapp", opts, access, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ch.preflight = func() (string, error) { return "node", nil }
	ch.endpoint, ch.token = fb.srv.URL, fakeToken
	ch.connectWait = 3 * time.Second
	if mutate != nil {
		mutate(ch)
	}
	rec := newReceived()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ch.Close() })
	if err := ch.Start(ctx, rec.handler); err != nil {
		t.Fatal(err)
	}
	return ch, rec
}

var botAccess = AccessConfig{Mode: ModeBot, AllowFrom: []string{"+1 555 123 4567"}}

func TestBridgeInboundLongPollAdmitsAllowlistedSender(t *testing.T) {
	fb := newFakeBridge(t)
	_, rec := startChannel(t, fb, botAccess, nil)

	// The poll is already parked on the bridge when the message arrives.
	time.Sleep(50 * time.Millisecond)
	fb.push(bdm("M1", owner, "what is running?"))
	m := rec.next(t)
	if m.Text != "what is running?" || m.ChatID != owner || m.SenderID != owner || m.MessageID != "M1" {
		t.Fatalf("message = %+v", m)
	}
	if m.Channel != "whatsapp" || m.Timestamp.Unix() != 1700000000 {
		t.Fatalf("channel/timestamp = %q/%v", m.Channel, m.Timestamp)
	}
}

func TestBridgeInboundDeniesStrangers(t *testing.T) {
	fb := newFakeBridge(t)
	_, rec := startChannel(t, fb, botAccess, nil)
	fb.push(bdm("M1", stranger, "hello?"))
	rec.none(t)
	if got := fb.sentMessages(); len(got) != 0 {
		t.Fatalf("silent deny sent %v", got)
	}
	// ... and the loop keeps going afterwards.
	fb.push(bdm("M2", owner, "ok"))
	if m := rec.next(t); m.MessageID != "M2" {
		t.Fatalf("got %+v", m)
	}
}

func TestBridgeDenyReplySentOnceThroughBridge(t *testing.T) {
	fb := newFakeBridge(t)
	access := botAccess
	access.DenyReply = "not authorized"
	_, rec := startChannel(t, fb, access, nil)
	fb.push(bdm("M1", stranger, "hi"))
	fb.push(bdm("M2", stranger, "hi again"))
	rec.none(t)
	got := fb.sentMessages()
	if len(got) != 1 || got[0]["message"] != "not authorized" || got[0]["chatId"] != stranger {
		t.Fatalf("sent = %v, want exactly one deny reply", got)
	}
}

func TestBridgeSelfChatOwnerGate(t *testing.T) {
	fb := newFakeBridge(t)
	access := AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{"15551234567"}}
	_, rec := startChannel(t, fb, access, nil)

	fb.push(bdm("S1", stranger, "spam"))                          // someone else messaging us: never admitted
	fb.push(bfromMe("S2", stranger, "owner talking to a friend")) // owner's message in another chat
	rec.none(t)

	fb.push(bfromMe("S3", owner, "status please")) // owner's own chat
	m := rec.next(t)
	if m.Text != "status please" || m.ChatID != owner {
		t.Fatalf("message = %+v", m)
	}
}

func TestBridgeEchoSuppression(t *testing.T) {
	fb := newFakeBridge(t)
	access := AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{"15551234567"}}
	ch, rec := startChannel(t, fb, access, nil)

	id, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: "15551234567", Text: "wolf started"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "OUT1" {
		t.Fatalf("id = %q", id)
	}
	// The sent message comes back as a fromMe event in our own chat, as the
	// first chunk and as the second; neither may be read as the owner typing.
	fb.push(bfromMe("OUT1", owner, "wolf started"))
	fb.push(bfromMe("OUT1-b", owner, "wolf started (cont.)"))
	rec.none(t)

	// A genuine owner message right after still gets through.
	fb.push(bfromMe("REAL1", owner, "thanks"))
	if m := rec.next(t); m.MessageID != "REAL1" {
		t.Fatalf("got %+v", m)
	}
}

func TestBridgeSelfChatReplyPrefix(t *testing.T) {
	fb := newFakeBridge(t)
	access := AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{"15551234567"}, ReplyPrefix: "[wm] "}
	ch, rec := startChannel(t, fb, access, nil)

	if _, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	got := fb.sentMessages()
	if len(got) != 1 || got[0]["message"] != "[wm] hello" {
		t.Fatalf("sent = %v", got)
	}
	// Even with an unknown id (bridge restarted, id forgotten) the prefix marks it ours.
	fb.push(bfromMe("UNKNOWN-ID", owner, "[wm] hello"))
	rec.none(t)
}

func TestBridgeBotModeDoesNotPrefix(t *testing.T) {
	fb := newFakeBridge(t)
	access := botAccess
	access.ReplyPrefix = "[wm] "
	ch, _ := startChannel(t, fb, access, nil)
	ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "hello"})
	if got := fb.sentMessages(); got[0]["message"] != "hello" {
		t.Fatalf("bot mode prefixed: %v", got)
	}
}

func TestBridgeSendFormatsTopicAndJID(t *testing.T) {
	fb := newFakeBridge(t)
	ch, _ := startChannel(t, fb, botAccess, nil)
	_, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: "+1 (555) 123-4567", Text: "agent up", Topic: "wolf", ReplyToID: "Q9"})
	if err != nil {
		t.Fatal(err)
	}
	got := fb.sentMessages()[0]
	if got["chatId"] != owner || got["message"] != "[wolf] agent up" || got["replyTo"] != "Q9" {
		t.Fatalf("sent = %v", got)
	}
}

func TestBridgeSendValidation(t *testing.T) {
	fb := newFakeBridge(t)
	ch, _ := startChannel(t, fb, botAccess, nil)
	if _, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "  "}); err == nil {
		t.Error("empty text accepted")
	}
	if _, err := ch.Send(context.Background(), channels.OutboundMessage{Text: "x"}); err == nil {
		t.Error("empty chat accepted")
	}
}

func TestBridgeSendWaitsForConnection(t *testing.T) {
	fb := newFakeBridge(t)
	fb.setConnected(false)
	ch, _ := startChannel(t, fb, botAccess, nil)
	go func() {
		time.Sleep(400 * time.Millisecond)
		fb.setConnected(true)
	}()
	id, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "early"})
	if err != nil || id == "" {
		t.Fatalf("Send = %q, %v; want it to wait for the bridge", id, err)
	}
}

func TestBridgeSendTimesOutWhenNeverConnected(t *testing.T) {
	fb := newFakeBridge(t)
	fb.setConnected(false)
	ch, _ := startChannel(t, fb, botAccess, func(c *BridgeChannel) { c.connectWait = 300 * time.Millisecond })
	_, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("err = %v", err)
	}
}

func TestBridgePartialSendFailureStillRecordsIDs(t *testing.T) {
	fb := newFakeBridge(t)
	fb.sendStatus = 500
	access := AccessConfig{Mode: ModeSelfChat, SelfIDs: []string{"15551234567"}}
	ch, rec := startChannel(t, fb, access, nil)
	if _, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "long"}); err == nil {
		t.Fatal("want error")
	}
	fb.push(bfromMe("PARTIAL1", owner, "long (first chunk)"))
	rec.none(t)
}

func TestBridgeReadReceiptsOnlyForAdmittedAndOnlyWhenEnabled(t *testing.T) {
	fb := newFakeBridge(t)
	_, rec := startChannel(t, fb, botAccess, func(c *BridgeChannel) { c.opts.SendReadReceipts = true })
	fb.push(bdm("R1", stranger, "no"))
	fb.push(bdm("R2", owner, "yes"))
	rec.next(t)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.reads) != 1 {
		t.Fatalf("reads = %v, want one (admitted message only)", fb.reads)
	}
	key := fb.reads[0]["key"].(map[string]any)
	if key["id"] != "R2" {
		t.Fatalf("read key = %v", key)
	}

	fb2 := newFakeBridge(t)
	_, rec2 := startChannel(t, fb2, botAccess, nil) // default: off
	fb2.push(bdm("R3", owner, "yes"))
	rec2.next(t)
	fb2.mu.Lock()
	defer fb2.mu.Unlock()
	if len(fb2.reads) != 0 {
		t.Fatalf("read receipts sent while disabled: %v", fb2.reads)
	}
}

func TestBridgeTyping(t *testing.T) {
	fb := newFakeBridge(t)
	ch, _ := startChannel(t, fb, botAccess, nil)
	if err := ch.Typing(context.Background(), "15551234567"); err != nil {
		t.Fatal(err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.typing) != 1 || fb.typing[0] != owner {
		t.Fatalf("typing = %v", fb.typing)
	}
}

func TestBridgePollSurvivesBridgeOutage(t *testing.T) {
	fb := newFakeBridge(t)
	_, rec := startChannel(t, fb, botAccess, nil)
	// The bridge's API goes away (process restarting) and comes back.
	fb.srv.CloseClientConnections()
	fb.push(bdm("M1", owner, "after blip"))
	if m := rec.next(t); m.MessageID != "M1" {
		t.Fatalf("got %+v", m)
	}
}

func TestBridgeHandlerPanicDoesNotKillLoop(t *testing.T) {
	fb := newFakeBridge(t)
	opts := BridgeOptions{SessionDir: t.TempDir(), InstallDir: t.TempDir()}
	ch, err := NewBridgeChannel("whatsapp", opts, botAccess, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ch.preflight = func() (string, error) { return "node", nil }
	ch.endpoint, ch.token = fb.srv.URL, fakeToken
	defer ch.Close()
	got := make(chan string, 4)
	err = ch.Start(context.Background(), func(_ context.Context, m channels.InboundMessage) {
		got <- m.MessageID
		if m.MessageID == "BOOM" {
			panic("handler bug")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	fb.push(bdm("BOOM", owner, "x"))
	fb.push(bdm("OK", owner, "y"))
	for _, want := range []string{"BOOM", "OK"} {
		select {
		case id := <-got:
			if id != want {
				t.Fatalf("got %q, want %q", id, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

// --- lifecycle and supervision ---

type stubProc struct {
	exit    chan int
	stopped chan struct{}
	once    sync.Once
}

func (p *stubProc) Wait() (int, error) {
	code := <-p.exit
	if code != 0 {
		return code, fmt.Errorf("exit status %d", code)
	}
	return 0, nil
}
func (p *stubProc) Stop() {
	p.once.Do(func() {
		close(p.stopped)
		select {
		case p.exit <- 0:
		default:
		}
	})
}

// stubLauncher hands out stub processes that exit with the scripted codes
// and then stay up.
type stubLauncher struct {
	mu     sync.Mutex
	codes  []int
	starts int
}

func (l *stubLauncher) launch(string, int, string, io.Writer) bridge.Starter {
	return func(ctx context.Context) (bridge.Process, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		p := &stubProc{exit: make(chan int, 1), stopped: make(chan struct{})}
		if l.starts < len(l.codes) {
			p.exit <- l.codes[l.starts]
		}
		l.starts++
		return p, nil
	}
}

func (l *stubLauncher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBridgeSupervisesProcessWithBackoff(t *testing.T) {
	fb := newFakeBridge(t)
	l := &stubLauncher{codes: []int{1, 137}}
	ch, rec := startChannel(t, fb, botAccess, func(c *BridgeChannel) {
		c.launcher = l.launch
		c.backoff = bridge.Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond}
	})
	eventually(t, "restarts", func() bool { return l.count() == 3 && ch.Status().Running })
	if st := ch.Status(); st.Restarts != 2 {
		t.Fatalf("status = %+v, want 2 restarts", st)
	}
	// The channel still works across the restarts.
	fb.push(bdm("M1", owner, "still here"))
	rec.next(t)

	ch.Restart()
	eventually(t, "manual restart", func() bool { return l.count() == 4 })
}

func TestBridgeLoggedOutIsFatalAndReported(t *testing.T) {
	fb := newFakeBridge(t)
	l := &stubLauncher{codes: []int{bridge.ExitLoggedOut}}
	ch, _ := startChannel(t, fb, botAccess, func(c *BridgeChannel) {
		c.launcher = l.launch
		c.backoff = bridge.Backoff{Initial: time.Millisecond}
	})
	eventually(t, "logged-out status", func() bool { return ch.Status().LoggedOut })
	// Supervisor status flips just before the channel records the fatal error.
	var err error
	eventually(t, "Send to report the logout", func() bool {
		_, err = ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "x"})
		return err != nil
	})
	if !errors.Is(err, bridge.ErrLoggedOut) || !strings.Contains(err.Error(), "orch whatsapp pair") {
		t.Fatalf("Send err = %v, want ErrLoggedOut with the re-pair hint", err)
	}
	if l.count() != 1 {
		t.Fatalf("restarted %d times after logout", l.count())
	}
}

func TestBridgeCloseStopsProcessAndIsIdempotent(t *testing.T) {
	fb := newFakeBridge(t)
	l := &stubLauncher{}
	var proc *stubProc
	ch, _ := startChannel(t, fb, botAccess, func(c *BridgeChannel) {
		c.launcher = func(node string, port int, token string, w io.Writer) bridge.Starter {
			inner := l.launch(node, port, token, w)
			return func(ctx context.Context) (bridge.Process, error) {
				p, err := inner(ctx)
				proc = p.(*stubProc)
				return p, err
			}
		}
	})
	eventually(t, "running", func() bool { return ch.Status().Running })
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proc.stopped:
	default:
		t.Fatal("Close did not stop the bridge process")
	}
	if _, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "x"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("Send after Close = %v, want ErrNotRunning", err)
	}
	if err := ch.Start(context.Background(), newReceived().handler); err == nil {
		t.Fatal("Start after Close succeeded")
	}
}

func TestBridgeSendBeforeStart(t *testing.T) {
	ch, err := NewBridgeChannel("whatsapp", BridgeOptions{SessionDir: t.TempDir(), InstallDir: t.TempDir()}, botAccess, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "x"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeStartRejectsNilHandlerAndDoubleStart(t *testing.T) {
	fb := newFakeBridge(t)
	ch, _ := startChannel(t, fb, botAccess, nil)
	if err := ch.Start(context.Background(), newReceived().handler); err == nil {
		t.Fatal("second Start succeeded")
	}
	fresh, _ := NewBridgeChannel("w2", BridgeOptions{SessionDir: t.TempDir(), InstallDir: t.TempDir()}, botAccess, t.TempDir(), nil)
	if err := fresh.Start(context.Background(), nil); err == nil {
		t.Fatal("nil handler accepted")
	}
}

// --- preflight (the real checks, no fakes) ---

func TestBridgeStartFailsClearlyWithoutNode(t *testing.T) {
	opts := BridgeOptions{Node: "/nonexistent/node", SessionDir: t.TempDir(), InstallDir: t.TempDir()}
	ch, err := NewBridgeChannel("whatsapp", opts, botAccess, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ch.Start(context.Background(), newReceived().handler)
	if !errors.Is(err, bridge.ErrNodeUnavailable) || !strings.Contains(err.Error(), "Node.js 18") {
		t.Fatalf("err = %v, want a clear node-missing error", err)
	}
	if _, serr := ch.Send(context.Background(), channels.OutboundMessage{ChatID: owner, Text: "x"}); !errors.Is(serr, channels.ErrNotRunning) {
		t.Fatalf("failed Start left the channel running: %v", serr)
	}
}

func TestBridgeStartFailsWhenNotInstalledOrNotPaired(t *testing.T) {
	node := fakeNodeBinary(t)
	opts := BridgeOptions{Node: node, SessionDir: t.TempDir(), InstallDir: t.TempDir()}
	ch, _ := NewBridgeChannel("whatsapp", opts, botAccess, t.TempDir(), nil)
	err := ch.Start(context.Background(), newReceived().handler)
	if err == nil || !strings.Contains(err.Error(), "orch whatsapp pair") || !strings.Contains(err.Error(), "dependencies") {
		t.Fatalf("not installed: err = %v", err)
	}

	// Installed (marker + node_modules via the real Install with a fake npm), but not paired.
	npmDir := t.TempDir()
	if err := bridge.Install(context.Background(), opts.InstallDir, fakeNPMBinary(t, npmDir), nil); err != nil {
		t.Fatal(err)
	}
	err = ch.Start(context.Background(), newReceived().handler)
	if err == nil || !strings.Contains(err.Error(), "not paired") || !strings.Contains(err.Error(), "orch whatsapp pair") {
		t.Fatalf("not paired: err = %v", err)
	}
}

// --- configuration ---

func TestParseBackend(t *testing.T) {
	cases := map[string]struct {
		opts map[string]any
		want Backend
		bad  bool
	}{
		"default":  {nil, BackendCloud, false},
		"cloud":    {map[string]any{"mode": "cloud"}, BackendCloud, false},
		"bridge":   {map[string]any{"mode": " Bridge "}, BackendBridge, false},
		"empty":    {map[string]any{"mode": ""}, BackendCloud, false},
		"unknown":  {map[string]any{"mode": "baileys"}, "", true},
		"not-text": {map[string]any{"mode": 3}, "", true},
	}
	for name, c := range cases {
		got, err := ParseBackend(c.opts)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("%s: ParseBackend = %q, %v", name, got, err)
		}
	}
}

func TestParseBridgeOptions(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	o, err := ParseBridgeOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.SessionDir != "/home/test/.workingman/whatsapp/session" || o.InstallDir != "/home/test/.workingman/whatsapp/bridge" || o.Port != 0 {
		t.Fatalf("defaults = %+v", o)
	}
	o, err = ParseBridgeOptions(map[string]any{"bridge": map[string]any{
		"node": "/opt/node", "port": 4321, "session_dir": "~/sess", "send_read_receipts": true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if o.Node != "/opt/node" || o.Port != 4321 || o.SessionDir != "/home/test/sess" || !o.SendReadReceipts {
		t.Fatalf("parsed = %+v", o)
	}
	for _, bad := range []map[string]any{
		{"bridge": map[string]any{"nodee": "x"}},
		{"bridge": map[string]any{"port": 70000}},
		{"bridge": map[string]any{"port": -1}},
	} {
		if _, err := ParseBridgeOptions(bad); err == nil {
			t.Errorf("ParseBridgeOptions(%v) succeeded, want error", bad)
		}
	}
}

func TestFactoryBuildsBridgeChannelFromConfig(t *testing.T) {
	cfg, err := channels.Parse([]byte(`
channels:
  whatsapp:
    type: whatsapp
    options:
      mode: bridge
      access:
        mode: self-chat
        self_ids: ["15551234567"]
      bridge:
        session_dir: ` + t.TempDir() + `
        install_dir: ` + t.TempDir() + `
routes:
  - {topic: wolf, channel: whatsapp, chat: "15551234567"}
`))
	if err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	reg, err := channels.Build(cfg, t.TempDir(), map[string]channels.Factory{"whatsapp": Factory(FactoryDeps{LogDir: logDir})})
	if err != nil {
		t.Fatal(err)
	}
	ch, ok := reg.Get("whatsapp")
	if !ok {
		t.Fatal("channel not registered")
	}
	bc, ok := ch.(*BridgeChannel)
	if !ok {
		t.Fatalf("channel is %T, want *BridgeChannel", ch)
	}
	if !bc.selfChat || !strings.HasSuffix(bc.logPath, logDir+"/"+BridgeLogName) {
		t.Fatalf("selfChat=%v logPath=%q (log should sit in the supplied dir)", bc.selfChat, bc.logPath)
	}
}

func TestFactoryCloudModeAndErrors(t *testing.T) {
	cloudCalls := 0
	cloud := func(name string, spec channels.ChannelConfig, creds channels.Credentials) (channels.Channel, error) {
		cloudCalls++
		return nil, errors.New("cloud stub")
	}
	spec := func(opts map[string]any) channels.ChannelConfig {
		return channels.ChannelConfig{Type: "whatsapp", Options: opts}
	}

	// Cloud is the default and goes to the cloud factory.
	if _, err := Factory(FactoryDeps{Cloud: cloud})("whatsapp", spec(nil), nil); err == nil || cloudCalls != 1 {
		t.Fatalf("default mode did not use the cloud factory (calls=%d, err=%v)", cloudCalls, err)
	}
	// No cloud backend wired in: say so rather than panic.
	if _, err := Factory(FactoryDeps{})("whatsapp", spec(map[string]any{"mode": "cloud"}), nil); err == nil || !strings.Contains(err.Error(), "mode: bridge") {
		t.Fatalf("err = %v", err)
	}
	// Garbage mode, and a bad access block, fail at build time.
	if _, err := Factory(FactoryDeps{})("whatsapp", spec(map[string]any{"mode": "carrier-pigeon"}), nil); err == nil {
		t.Fatal("bad mode accepted")
	}
	bad := spec(map[string]any{"mode": "bridge", "access": map[string]any{"mode": "self-chat"}}) // no self_ids
	if _, err := Factory(FactoryDeps{})("whatsapp", bad, nil); err == nil || !strings.Contains(err.Error(), "self_ids") {
		t.Fatalf("err = %v", err)
	}
}
