package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/session"
)

// relayHub stands in for acp-wrapper's hub and the agent behind it: a unix
// socket whose clients all see every agent frame, each of which is appended
// to stream.log first, like the real hub. A session/prompt streams one chunk
// and ends the turn; prompts are recorded, as are session/cancel calls.
type relayHub struct {
	t   *testing.T
	ln  net.Listener
	log *os.File

	mu      sync.Mutex
	clients map[net.Conn]struct{}
	prompts []string
	cancels int
	joined  int
}

func newRelayHub(t *testing.T, socket, logPath string) *relayHub {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	h := &relayHub{t: t, ln: ln, log: lf, clients: map[net.Conn]struct{}{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.clients[c] = struct{}{}
			h.joined++
			h.mu.Unlock()
			go h.serve(c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		h.mu.Lock()
		for c := range h.clients {
			c.Close()
		}
		h.mu.Unlock()
		lf.Close()
	})
	return h
}

func (h *relayHub) connected() (now, ever int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients), h.joined
}

// emit logs a frame, then sends it to every client.
func (h *relayHub) emit(v any) {
	b, _ := json.Marshal(v)
	b = append(b, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log.Write(b)
	for c := range h.clients {
		c.Write(b)
	}
}

func (h *relayHub) serve(c net.Conn) {
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
		c.Close()
	}()
	br := bufio.NewReader(c)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return
		}
		var f struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
			Params struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		switch f.Method {
		case "session/prompt":
			text := ""
			if len(f.Params.Prompt) > 0 {
				text = f.Params.Prompt[0].Text
			}
			h.mu.Lock()
			h.prompts = append(h.prompts, f.Params.SessionID+":"+text)
			h.mu.Unlock()
			h.chunk(f.Params.SessionID, "echo: "+text)
			h.emit(map[string]any{"jsonrpc": "2.0", "id": *f.ID, "result": map[string]any{"stopReason": "end_turn"}})
		case "session/cancel":
			h.mu.Lock()
			h.cancels++
			h.mu.Unlock()
		}
	}
}

func (h *relayHub) chunk(sessionID, text string) {
	h.emit(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": sessionID,
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}},
	}})
}

type relayMsg struct {
	T          string          `json:"t"`
	Offset     int64           `json:"offset"`
	Frame      json.RawMessage `json:"frame"`
	State      string          `json:"state"`
	StopReason string          `json:"stop_reason"`
	Reason     string          `json:"reason"`
	Code       string          `json:"code"`
	Kind       string          `json:"kind"`
	Allowed    bool            `json:"interactive_allowed"`
}

type relayConn struct {
	t *testing.T
	c *websocket.Conn
}

func (r *relayConn) next() relayMsg {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := r.c.Read(ctx)
	if err != nil {
		r.t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), secretToken) {
		r.t.Errorf("relay leaks a secret: %s", data)
	}
	var m relayMsg
	if err := json.Unmarshal(data, &m); err != nil {
		r.t.Fatalf("decode %s: %v", data, err)
	}
	return m
}

func (r *relayConn) until(typ string) (relayMsg, []relayMsg) {
	r.t.Helper()
	var before []relayMsg
	for {
		m := r.next()
		if m.T == typ {
			return m, before
		}
		before = append(before, m)
	}
}

// relayFixture serves the API fixture over HTTP, with build's task session
// given a short socket path and a fake hub behind it.
func newRelayFixture(t *testing.T) (*apiFixture, *httptest.Server, *relayHub, string) {
	t.Helper()
	f := newAPIFixture(t)
	const sid = "task-alpha-build"
	store := session.Store{Root: f.sessRoot}
	rec, err := store.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "wmrelay")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	rec.SocketPath = sockDir + "/agent.sock"
	if err := store.Write(rec); err != nil {
		t.Fatal(err)
	}
	// The fixture's log ends in a partial line; finish it, as the wrapper
	// would.
	lf, err := os.OpenFile(store.Dir(sid)+"/stream.log", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	lf.WriteString("1}\n")
	lf.Close()
	hub := newRelayHub(t, rec.SocketPath, store.Dir(sid)+"/stream.log")
	// Serve through the daemon's own API server, so the session resource
	// sees the relay's observers.
	srv := api.New(api.Config{Mode: api.ModeUnix, TokenPath: f.tokPath}, apiSource{f.d})
	f.d.api = srv
	f.h = srv.Handler("")
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	return f, ts, hub, sid
}

func dialRelay(t *testing.T, ts *httptest.Server, tok, sid, query string) *relayConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := strings.Replace(ts.URL, "http://", "ws://", 1) + api.Prefix + "/sessions/" + sid + "/acp"
	if query != "" {
		u += "?" + query
	}
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		Subprotocols: []string{api.SubprotocolACP},
		HTTPHeader:   map[string][]string{"Authorization": {"Bearer " + tok}},
	})
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &relayConn{t: t, c: c}
}

func TestRelayWatchesOneShotTaskWithoutChangingWhenItEnds(t *testing.T) {
	f, ts, hub, sid := newRelayFixture(t)
	w := dialRelay(t, ts, f.tok, sid, "")
	hello, _ := w.until(api.MsgHello)
	if hello.Kind != "task" || hello.Allowed {
		t.Errorf("hello = %+v", hello)
	}
	_, replayed := w.until(api.MsgLive)
	if len(replayed) != 3 {
		t.Fatalf("replayed %d frames, want 3: %+v", len(replayed), replayed)
	}
	for _, m := range replayed {
		if m.T != api.MsgReplay || !json.Valid(m.Frame) {
			t.Errorf("replay message %+v", m)
		}
	}

	// The task was primed (prompt_count 1) and its turn is running, so the
	// relay joins it upstream.
	waitUntil(t, "the relay to connect", func() bool { now, _ := hub.connected(); return now == 1 })
	var sess SessionResource
	f.get(t, api.Prefix+"/sessions/"+sid, &sess)
	if sess.ACP == nil || sess.ACP.Observers != 1 || sess.Links["attach"] != api.Prefix+"/sessions/"+sid+"/acp" {
		t.Errorf("session acp = %+v links = %v", sess.ACP, sess.Links)
	}

	// The turn the daemon started ends: the browser sees it, and the relay
	// leaves, as the TUI does, so the wrapper's exit-when-empty can fire.
	hub.chunk("s1", "done")
	hub.emit(map[string]any{"jsonrpc": "2.0", "id": 7, "result": map[string]any{"stopReason": "end_turn"}})
	m, _ := w.until(api.MsgTurn)
	for m.StopReason == "" {
		m, _ = w.until(api.MsgTurn)
	}
	if m.State != api.TurnIdle || m.StopReason != "end_turn" {
		t.Errorf("turn = %+v", m)
	}
	waitUntil(t, "the relay to disconnect at end of turn", func() bool { now, _ := hub.connected(); return now == 0 })
	time.Sleep(300 * time.Millisecond)
	if _, ever := hub.connected(); ever != 1 {
		t.Errorf("the relay connected %d times; a one-shot session gets one connection", ever)
	}

	// The session ends: the browser is told.
	store := session.Store{Root: f.sessRoot}
	rec, _ := store.Read(sid)
	rec.Status = session.StatusExited
	if err := store.Write(rec); err != nil {
		t.Fatal(err)
	}
	if m, _ := w.until(api.MsgEnded); m.Reason != "exited" {
		t.Errorf("ended = %+v", m)
	}
}

func TestRelayInteractiveIsForPersistentSessionsOnly(t *testing.T) {
	f, ts, hub, sid := newRelayFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := strings.Replace(ts.URL, "http://", "ws://", 1) + api.Prefix + "/sessions/" + sid + "/acp?mode=interactive"
	_, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		Subprotocols: []string{api.SubprotocolACP},
		HTTPHeader:   map[string][]string{"Authorization": {"Bearer " + f.tok}},
	})
	if err == nil || resp == nil || resp.StatusCode != 409 {
		t.Fatalf("interactive on a task session: %v %v", err, resp)
	}

	// Make it a wolf: persistent, typed into.
	store := session.Store{Root: f.sessRoot}
	rec, _ := store.Read(sid)
	rec.Kind, rec.Persistent = "wolf", true
	if err := store.Write(rec); err != nil {
		t.Fatal(err)
	}
	w := dialRelay(t, ts, f.tok, sid, "mode=interactive&replay=none")
	if hello, _ := w.until(api.MsgHello); !hello.Allowed {
		t.Errorf("hello = %+v", hello)
	}
	w.until(api.MsgLive)
	w.until(api.MsgTurn)
	waitUntil(t, "the relay to connect", func() bool { now, _ := hub.connected(); return now == 1 })

	data, _ := json.Marshal(api.RelayClientMessage{T: api.MsgPrompt, Text: "what now?"})
	var m relayMsg
	for {
		if err := w.c.Write(context.Background(), websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
		// The first prompt may race the adoption; the relay says so.
		m = w.next()
		if m.T != api.MsgError || m.Code != api.CodeUnavailable {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if m.T != api.MsgTurn || m.State != api.TurnRunning {
		t.Errorf("after the prompt: %+v, want turn running", m)
	}
	frame, _ := w.until(api.MsgFrame)
	if !strings.Contains(string(frame.Frame), "echo: what now?") {
		t.Errorf("frame = %s", frame.Frame)
	}
	if m, _ := w.until(api.MsgTurn); m.State != api.TurnIdle {
		t.Errorf("turn = %+v", m)
	}
	hub.mu.Lock()
	prompts := append([]string(nil), hub.prompts...)
	hub.mu.Unlock()
	if len(prompts) != 1 || prompts[0] != "s1:what now?" {
		t.Errorf("prompts = %q, want one on the adopted session s1", prompts)
	}

	cancelMsg, _ := json.Marshal(api.RelayClientMessage{T: api.MsgCancel})
	if err := w.c.Write(context.Background(), websocket.MessageText, cancelMsg); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "session/cancel", func() bool { hub.mu.Lock(); defer hub.mu.Unlock(); return hub.cancels == 1 })

	// A persistent session keeps the relay connected after the turn.
	time.Sleep(200 * time.Millisecond)
	if now, _ := hub.connected(); now != 1 {
		t.Errorf("relay left a persistent session after its turn (clients %d)", now)
	}
}

func TestRelaySessionRefusesTabSessions(t *testing.T) {
	f := newAPIFixture(t)
	if _, err := (apiSource{f.d}).RelaySession(context.Background(), "nope"); err == nil {
		t.Error("an unknown session resolved")
	}
}

func TestRelayLogKeepsPartialLines(t *testing.T) {
	path := t.TempDir() + "/stream.log"
	if err := os.WriteFile(path, []byte("{\"a\":1}\n{\"b\":"), 0o644); err != nil {
		t.Fatal(err)
	}
	lr, err := openRelayLog(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lr.Close()
	frames, err := lr.Next(10)
	if err != nil || len(frames) != 1 || frames[0].End != 8 {
		t.Fatalf("first read = %+v, %v", frames, err)
	}
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString("2}\n")
	fh.Close()
	frames, err = lr.Next(10)
	if err != nil || len(frames) != 1 || string(frames[0].Data) != `{"b":2}` || frames[0].Start != 8 {
		t.Fatalf("second read = %+v, %v", frames, err)
	}
}
