package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeRelaySession is a session for the relay: a stream log in a temp dir,
// a record the test edits, and upstream connections it counts.
type fakeRelaySession struct {
	t    *testing.T
	log  string
	kind string
	pers bool

	mu          sync.Mutex
	promptCount int
	ended       string
	attaches    int
	ups         []*fakeUpstream
	attachErr   error
}

func newFakeRelaySession(t *testing.T, kind string, persistent bool, promptCount int) *fakeRelaySession {
	t.Helper()
	f := &fakeRelaySession{t: t, log: filepath.Join(t.TempDir(), "stream.log"), kind: kind, pers: persistent, promptCount: promptCount}
	if err := os.WriteFile(f.log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeRelaySession) append(lines ...string) {
	f.t.Helper()
	fh, err := os.OpenFile(f.log, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	for _, l := range lines {
		if _, err := fh.WriteString(l); err != nil {
			f.t.Fatal(err)
		}
	}
	f.mu.Lock()
	ups := append([]*fakeUpstream(nil), f.ups...)
	f.mu.Unlock()
	for _, u := range ups {
		if u.wake != nil {
			u.wake()
		}
	}
}

func (f *fakeRelaySession) set(promptCount int, ended string) {
	f.mu.Lock()
	f.promptCount, f.ended = promptCount, ended
	f.mu.Unlock()
}

func (f *fakeRelaySession) counts() (attaches, open int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.ups {
		if !u.isClosed() {
			open++
		}
	}
	return f.attaches, open
}

func (f *fakeRelaySession) upstream(i int) *fakeUpstream {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ups[i]
}

func (f *fakeRelaySession) relaySession(id string) RelaySession {
	return RelaySession{
		ID: id, Kind: f.kind, Persistent: f.pers, Interactive: f.pers,
		State: func() (RelayState, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return RelayState{PromptCount: f.promptCount, Ended: f.ended}, nil
		},
		OpenLog: func(after int64) (LogReader, error) { return openTestLog(f.log, after) },
		Attach: func(ctx context.Context, wake func()) (Upstream, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.attaches++
			if f.attachErr != nil {
				return nil, f.attachErr
			}
			u := &fakeUpstream{wake: wake, done: make(chan struct{}), prompts: make(chan string, 8)}
			f.ups = append(f.ups, u)
			return u, nil
		},
	}
}

type fakeUpstream struct {
	wake    func()
	prompts chan string
	cancels atomic.Int32
	release chan struct{} // when set, Prompt blocks until it is closed

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

func (u *fakeUpstream) Prompt(text string) error {
	u.prompts <- text
	if u.release != nil {
		<-u.release
	}
	return nil
}
func (u *fakeUpstream) Cancel() error         { u.cancels.Add(1); return nil }
func (u *fakeUpstream) Done() <-chan struct{} { return u.done }
func (u *fakeUpstream) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.closed {
		u.closed = true
		close(u.done)
	}
	return nil
}
func (u *fakeUpstream) isClosed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.closed
}

// testLog is a minimal LogReader over a file.
type testLog struct {
	f   *os.File
	pos int64
}

func openTestLog(path string, after int64) (LogReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &testLog{f: f, pos: after}, nil
}

func (l *testLog) Next(limit int) ([]LogFrame, error) {
	if _, err := l.f.Seek(l.pos, io.SeekStart); err != nil {
		return nil, err
	}
	br := bufio.NewReader(l.f)
	var out []LogFrame
	for len(out) < limit {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return out, nil
		}
		start := l.pos
		l.pos += int64(len(line))
		out = append(out, LogFrame{Start: start, End: l.pos, Data: bytes.TrimRight(line, "\n")})
	}
	return out, nil
}

func (l *testLog) Close() error { return l.f.Close() }

// relaySource is a Source that also serves the relay.
type relaySource struct {
	fakeSource
	sessions map[string]*fakeRelaySession
}

func (s *relaySource) RelaySession(_ context.Context, id string) (RelaySession, error) {
	f, ok := s.sessions[id]
	if !ok {
		return RelaySession{}, ErrNotFound
	}
	return f.relaySession(id), nil
}

type relayEnv struct {
	srv  *Server
	ts   *httptest.Server
	tok  string
	mu   sync.Mutex
	logs []string
}

func newRelayEnv(t *testing.T, sessions map[string]*fakeRelaySession) *relayEnv {
	t.Helper()
	src := &relaySource{sessions: sessions}
	s, tok := newTestServer(t, src)
	e := &relayEnv{srv: s, tok: tok}
	s.Log = func(event string, kv ...string) {
		e.mu.Lock()
		e.logs = append(e.logs, event+" "+strings.Join(kv, " "))
		e.mu.Unlock()
	}
	s.relays.poll = 10 * time.Millisecond
	s.relays.retry = 10 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ts := httptest.NewUnstartedServer(s.Handler(port))
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	e.ts = ts
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s.stopOnce.Do(func() { close(s.stopStreams) })
		s.relays.drain(ctx)
		ts.Close()
	})
	return e
}

func (e *relayEnv) logged(prefix string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, l := range e.logs {
		if strings.HasPrefix(l, prefix+" ") {
			out = append(out, l)
		}
	}
	return out
}

func (e *relayEnv) url(sid, query string) string {
	u := strings.Replace(e.ts.URL, "http://", "ws://", 1) + Prefix + "/sessions/" + sid + "/acp"
	if query != "" {
		u += "?" + query
	}
	return u
}

// dial opens a relay connection with the bearer token.
func (e *relayEnv) dial(t *testing.T, sid, query string) *wsClient {
	t.Helper()
	c, resp, err := e.tryDial(sid, query, http.Header{"Authorization": {"Bearer " + e.tok}}, []string{SubprotocolACP})
	if err != nil {
		t.Fatalf("dial %s?%s: %v (%v)", sid, query, err, resp)
	}
	return &wsClient{t: t, c: c}
}

func (e *relayEnv) tryDial(sid, query string, hdr http.Header, protos []string) (*websocket.Conn, *httpResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, e.url(sid, query), &websocket.DialOptions{HTTPHeader: hdr, Subprotocols: protos})
	var res *httpResult
	if resp != nil {
		var body []byte
		if resp.Body != nil {
			body, _ = io.ReadAll(resp.Body)
		}
		res = &httpResult{code: resp.StatusCode, body: string(body)}
	}
	return c, res, err
}

type httpResult struct {
	code int
	body string
}

type wsClient struct {
	t *testing.T
	c *websocket.Conn
}

type wsMsg struct {
	T          string          `json:"t"`
	Offset     int64           `json:"offset"`
	Frame      json.RawMessage `json:"frame"`
	State      string          `json:"state"`
	StopReason string          `json:"stop_reason"`
	Reason     string          `json:"reason"`
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	Replay     string          `json:"replay"`
	Mode       string          `json:"mode"`
	Persistent bool            `json:"persistent"`
	Allowed    bool            `json:"interactive_allowed"`
	Prompts    int             `json:"prompt_count"`
	Kind       string          `json:"kind"`
}

func (w *wsClient) next() wsMsg {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := w.c.Read(ctx)
	if err != nil {
		w.t.Fatalf("read: %v", err)
	}
	var m wsMsg
	if err := json.Unmarshal(data, &m); err != nil {
		w.t.Fatalf("decode %s: %v", data, err)
	}
	return m
}

// until reads messages until one of type typ, returning those before it.
func (w *wsClient) until(typ string) (wsMsg, []wsMsg) {
	w.t.Helper()
	var before []wsMsg
	for {
		m := w.next()
		if m.T == typ {
			return m, before
		}
		before = append(before, m)
	}
}

// live reads through the replay to the live marker and the turn state that
// follows it, and returns that turn message.
func (w *wsClient) live() wsMsg {
	w.t.Helper()
	w.until(MsgLive)
	m := w.next()
	if m.T != MsgTurn {
		w.t.Fatalf("after live: %+v, want a turn message", m)
	}
	return m
}

func (w *wsClient) send(v any) {
	w.t.Helper()
	data, _ := json.Marshal(v)
	if err := w.c.Write(context.Background(), websocket.MessageText, data); err != nil {
		w.t.Fatalf("write: %v", err)
	}
}

func (w *wsClient) close() { w.c.Close(websocket.StatusNormalClosure, "") }

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

const (
	frameUpdate = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}` + "\n"
	frameNew    = `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1"}}` + "\n"
	frameEnd    = `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}` + "\n"
)

func TestRelayReplaysThenStreamsLive(t *testing.T) {
	f := newFakeRelaySession(t, "task", false, 1)
	f.append(frameNew, frameUpdate)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"task-x": f})

	w := e.dial(t, "task-x", "")
	hello := w.next()
	if hello.T != MsgHello || hello.Kind != "task" || hello.Persistent || hello.Allowed || hello.Mode != ModeObserve || hello.Prompts != 1 || hello.Replay != "all" {
		t.Fatalf("hello = %+v", hello)
	}
	live, replayed := w.until(MsgLive)
	_ = live
	if len(replayed) != 2 || replayed[0].T != MsgReplay || replayed[1].T != MsgReplay {
		t.Fatalf("replay = %+v", replayed)
	}
	if replayed[0].Offset != int64(len(frameNew)) || replayed[1].Offset != int64(len(frameNew)+len(frameUpdate)) {
		t.Errorf("offsets = %d, %d", replayed[0].Offset, replayed[1].Offset)
	}
	if !strings.Contains(string(replayed[1].Frame), "agent_message_chunk") {
		t.Errorf("frame = %s", replayed[1].Frame)
	}
	if turn := w.next(); turn.T != MsgTurn || turn.State != TurnRunning {
		t.Errorf("turn after live = %+v, want running", turn)
	}

	f.append(frameUpdate)
	if m := w.next(); m.T != MsgFrame || m.Offset != int64(len(frameNew)+2*len(frameUpdate)) {
		t.Errorf("live frame = %+v", m)
	}
	if got := e.logged("api_attach"); len(got) != 1 || !strings.Contains(got[0], "mode observe") || !strings.Contains(got[0], "client bearer") {
		t.Errorf("api_attach = %q", got)
	}
}

func TestRelayReplayModes(t *testing.T) {
	f := newFakeRelaySession(t, "task", false, 1)
	f.append(frameNew, frameUpdate)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"task-x": f})

	w := e.dial(t, "task-x", "replay=none")
	if h := w.next(); h.Replay != "none" {
		t.Errorf("hello = %+v", h)
	}
	if m := w.next(); m.T != MsgLive {
		t.Errorf("replay=none sent %+v before live", m)
	}
	w.close()

	w = e.dial(t, "task-x", "replay=after:"+strconv.Itoa(len(frameNew)))
	if h := w.next(); h.Replay != "after" {
		t.Errorf("hello = %+v", h)
	}
	_, replayed := w.until(MsgLive)
	if len(replayed) != 1 || !strings.Contains(string(replayed[0].Frame), "agent_message_chunk") {
		t.Errorf("replay=after = %+v", replayed)
	}
	w.close()

	w = e.dial(t, "task-x", "replay=after:99999")
	if h := w.next(); h.Replay != "reset" {
		t.Errorf("past-the-end offset: hello = %+v, want reset", h)
	}
	if _, replayed := w.until(MsgLive); len(replayed) != 2 {
		t.Errorf("reset replays the whole log; got %d frames", len(replayed))
	}
	w.close()

	if _, res, err := e.tryDial("task-x", "replay=sideways", http.Header{"Authorization": {"Bearer " + e.tok}}, []string{SubprotocolACP}); err == nil || res == nil || res.code != http.StatusBadRequest {
		t.Errorf("bad replay: %v %+v", err, res)
	}
}

func TestRelayOneUpstreamPerSession(t *testing.T) {
	f := newFakeRelaySession(t, "wolf", true, 1)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"wolf-x": f})

	a := e.dial(t, "wolf-x", "")
	a.live()
	b := e.dial(t, "wolf-x", "")
	b.live()
	eventually(t, "an upstream", func() bool { _, open := f.counts(); return open == 1 })
	if got := e.srv.Observers("wolf-x"); got != 2 {
		t.Errorf("Observers = %d, want 2", got)
	}
	time.Sleep(50 * time.Millisecond)
	if attaches, _ := f.counts(); attaches != 1 {
		t.Errorf("attaches = %d, want 1 for two browsers", attaches)
	}

	// Both browsers see a frame once.
	f.append(frameUpdate)
	for _, w := range []*wsClient{a, b} {
		if m, _ := w.until(MsgFrame); !strings.Contains(string(m.Frame), "agent_message_chunk") {
			t.Errorf("frame = %+v", m)
		}
	}

	a.close()
	time.Sleep(50 * time.Millisecond)
	if _, open := f.counts(); open != 1 {
		t.Errorf("upstream closed while a browser still watches")
	}
	b.close()
	eventually(t, "the upstream to close with the last browser", func() bool { _, open := f.counts(); return open == 0 })
	eventually(t, "no observers", func() bool { return e.srv.Observers("wolf-x") == 0 })
}

func TestRelayOneShotLifetimeParity(t *testing.T) {
	f := newFakeRelaySession(t, "task", false, 0)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"task-x": f})

	w := e.dial(t, "task-x", "")
	w.live()
	// Not primed: the relay must not be the first client to connect.
	time.Sleep(80 * time.Millisecond)
	if attaches, _ := f.counts(); attaches != 0 {
		t.Fatalf("relay connected to an unprimed one-shot session (%d attaches)", attaches)
	}

	f.set(1, "") // the daemon primed it
	eventually(t, "the relay to connect once primed", func() bool { _, open := f.counts(); return open == 1 })

	f.append(frameUpdate, frameEnd)
	if m, _ := w.until(MsgTurn); m.State != TurnRunning {
		t.Errorf("turn = %+v", m)
	}
	if m, _ := w.until(MsgTurn); m.State != TurnIdle || m.StopReason != "end_turn" {
		t.Errorf("turn = %+v, want idle end_turn", m)
	}
	eventually(t, "the relay to disconnect at end of turn", func() bool { _, open := f.counts(); return open == 0 })
	time.Sleep(80 * time.Millisecond)
	if attaches, _ := f.counts(); attaches != 1 {
		t.Errorf("attaches = %d after the turn ended, want 1 (no reconnect)", attaches)
	}

	// The browser keeps its stream between turns, and gets ended when the
	// session goes.
	f.set(1, "exited")
	if m, _ := w.until(MsgEnded); m.Reason != "exited" {
		t.Errorf("ended = %+v", m)
	}
}

func TestRelayOneShotAlreadyOverNeverConnects(t *testing.T) {
	f := newFakeRelaySession(t, "task", false, 1)
	f.append(frameNew, frameUpdate, frameEnd)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"task-x": f})
	w := e.dial(t, "task-x", "")
	if m := w.live(); m.State != TurnIdle || m.StopReason != "end_turn" {
		t.Errorf("turn = %+v", m)
	}
	time.Sleep(80 * time.Millisecond)
	if attaches, _ := f.counts(); attaches != 0 {
		t.Errorf("relay connected to a one-shot session whose turn is over")
	}
}

func TestRelayObserveRejectsInput(t *testing.T) {
	f := newFakeRelaySession(t, "wolf", true, 1)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"wolf-x": f})
	w := e.dial(t, "wolf-x", "mode=observe")
	w.live()
	w.send(RelayClientMessage{T: MsgPrompt, Text: "hello"})
	if m, _ := w.until(MsgError); m.Code != CodeForbidden {
		t.Errorf("error = %+v", m)
	}
	w.send(RelayClientMessage{T: MsgPing})
	if m, _ := w.until(MsgPong); m.T != MsgPong {
		t.Errorf("pong = %+v", m)
	}
	w.send(map[string]string{"t": "session/new"})
	if m, _ := w.until(MsgError); m.Code != CodeBadRequest {
		t.Errorf("unknown type: %+v", m)
	}
	if n := len(e.logged("api_prompt")); n != 0 {
		t.Errorf("api_prompt logged %d times", n)
	}
}

func TestRelayInteractivePromptAndCancel(t *testing.T) {
	f := newFakeRelaySession(t, "wolf", true, 1)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"wolf-x": f})
	w := e.dial(t, "wolf-x", "mode=interactive&replay=none")
	if h := w.next(); h.Mode != ModeInteractive || !h.Allowed {
		t.Fatalf("hello = %+v", h)
	}
	w.live()
	eventually(t, "an upstream", func() bool { _, open := f.counts(); return open == 1 })
	up := f.upstream(0)
	up.release = make(chan struct{})

	const secret = "do the secret thing"
	w.send(RelayClientMessage{T: MsgPrompt, Text: secret})
	select {
	case got := <-up.prompts:
		if got != secret {
			t.Errorf("prompt = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt never reached the upstream")
	}
	if m, _ := w.until(MsgTurn); m.State != TurnRunning {
		t.Errorf("turn = %+v", m)
	}
	// A second prompt while the turn runs is refused.
	w.send(RelayClientMessage{T: MsgPrompt, Text: "again"})
	if m, _ := w.until(MsgError); m.Code != CodeConflict {
		t.Errorf("busy prompt: %+v", m)
	}
	w.send(RelayClientMessage{T: MsgCancel})
	eventually(t, "the cancel", func() bool { return up.cancels.Load() == 1 })
	close(up.release)
	f.append(`{"jsonrpc":"2.0","id":9,"result":{"stopReason":"cancelled"}}` + "\n")
	if m, _ := w.until(MsgTurn); m.State != TurnIdle || m.StopReason != "cancelled" {
		t.Errorf("turn = %+v", m)
	}

	prompts := e.logged("api_prompt")
	if len(prompts) != 1 || !strings.Contains(prompts[0], "session wolf-x") {
		t.Errorf("api_prompt = %q", prompts)
	}
	e.mu.Lock()
	all := strings.Join(e.logs, "\n")
	e.mu.Unlock()
	if strings.Contains(all, secret) {
		t.Errorf("the prompt text reached the audit log: %s", all)
	}
	if len(e.logged("api_cancel")) != 1 {
		t.Errorf("api_cancel not logged")
	}
}

func TestRelayInteractiveRefusedForOneShot(t *testing.T) {
	f := newFakeRelaySession(t, "task", false, 1)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"task-x": f})
	_, res, err := e.tryDial("task-x", "mode=interactive", http.Header{"Authorization": {"Bearer " + e.tok}}, []string{SubprotocolACP})
	if err == nil || res == nil || res.code != http.StatusConflict || !strings.Contains(res.body, "mode=observe") {
		t.Fatalf("interactive on a one-shot: %v %+v", err, res)
	}
	if got := e.logged("api_attach_refused"); len(got) != 1 {
		t.Errorf("api_attach_refused = %q", got)
	}
}

func TestRelayRefusals(t *testing.T) {
	f := newFakeRelaySession(t, "wolf", true, 1)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"wolf-x": f})
	bearer := http.Header{"Authorization": {"Bearer " + e.tok}}
	for name, tc := range map[string]struct {
		sid, query string
		hdr        http.Header
		protos     []string
		want       int
	}{
		"no credentials":   {sid: "wolf-x", hdr: http.Header{}, protos: []string{SubprotocolACP}, want: http.StatusUnauthorized},
		"wrong token":      {sid: "wolf-x", hdr: http.Header{"Authorization": {"Bearer nope"}}, protos: []string{SubprotocolACP}, want: http.StatusUnauthorized},
		"no subprotocol":   {sid: "wolf-x", hdr: bearer, want: http.StatusBadRequest},
		"unknown session":  {sid: "nope", hdr: bearer, protos: []string{SubprotocolACP}, want: http.StatusNotFound},
		"bad mode":         {sid: "wolf-x", query: "mode=root", hdr: bearer, protos: []string{SubprotocolACP}, want: http.StatusBadRequest},
		"foreign origin":   {sid: "wolf-x", hdr: http.Header{"Authorization": {"Bearer " + e.tok}, "Origin": {"http://evil.example"}}, protos: []string{SubprotocolACP}, want: http.StatusForbidden},
		"bad bearer proto": {sid: "wolf-x", hdr: http.Header{}, protos: []string{SubprotocolACP, "bearer.nope"}, want: http.StatusUnauthorized},
	} {
		_, res, err := e.tryDial(tc.sid, tc.query, tc.hdr, tc.protos)
		if err == nil || res == nil || res.code != tc.want {
			t.Errorf("%s: %v %+v, want %d", name, err, res, tc.want)
		}
	}

	// The token as a subprotocol works (a browser cannot set Authorization).
	c, res, err := e.tryDial("wolf-x", "", http.Header{}, []string{SubprotocolACP, bearerSubprotocol + e.tok})
	if err != nil {
		t.Fatalf("bearer subprotocol: %v %+v", err, res)
	}
	if c.Subprotocol() != SubprotocolACP {
		t.Errorf("subprotocol = %q", c.Subprotocol())
	}
	c.CloseNow()

	// A plain GET is not an upgrade.
	req := httptest.NewRequest("GET", Prefix+"/sessions/wolf-x/acp", nil)
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+e.tok)
	rec := httptest.NewRecorder()
	e.srv.Handler("").ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "WebSocket") {
		t.Errorf("plain GET: %d %s", rec.Code, rec.Body)
	}
}

func TestRelayBrowserNeedsCSRFAndScopes(t *testing.T) {
	f := newFakeRelaySession(t, "wolf", true, 1)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"wolf-x": f})
	c, si := login(t, e.srv.Handler("7078"), e.tok)
	if !strings.Contains(strings.Join(si.Scopes, ","), "attach,interact") {
		t.Errorf("browser scopes = %v", si.Scopes)
	}
	origin := e.ts.URL
	hdr := http.Header{"Cookie": {c.Name + "=" + c.Value}, "Origin": {origin}}

	if _, res, err := e.tryDial("wolf-x", "", hdr, []string{SubprotocolACP}); err == nil || res == nil || res.code != http.StatusForbidden {
		t.Errorf("no csrf: %v %+v", err, res)
	}
	conn, res, err := e.tryDial("wolf-x", "csrf="+url.QueryEscape(si.CSRF), hdr, []string{SubprotocolACP})
	if err != nil {
		t.Fatalf("browser attach: %v %+v", err, res)
	}
	conn.CloseNow()

	for _, bs := range e.srv.logins.sessions {
		bs.scopes = []string{ScopeRead, ScopeAttach}
	}
	if _, res, err := e.tryDial("wolf-x", "mode=interactive&csrf="+url.QueryEscape(si.CSRF), hdr, []string{SubprotocolACP}); err == nil || res == nil || res.code != http.StatusForbidden || !strings.Contains(res.body, "interact scope") {
		t.Errorf("no interact scope: %v %+v", err, res)
	}
	for _, bs := range e.srv.logins.sessions {
		bs.scopes = []string{ScopeRead}
	}
	if _, res, err := e.tryDial("wolf-x", "csrf="+url.QueryEscape(si.CSRF), hdr, []string{SubprotocolACP}); err == nil || res == nil || res.code != http.StatusForbidden || !strings.Contains(res.body, "attach scope") {
		t.Errorf("no attach scope: %v %+v", err, res)
	}
}

func TestRelayEndsOnServerClose(t *testing.T) {
	f := newFakeRelaySession(t, "wolf", true, 1)
	e := newRelayEnv(t, map[string]*fakeRelaySession{"wolf-x": f})
	w := e.dial(t, "wolf-x", "")
	w.live()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		e.srv.stopOnce.Do(func() { close(e.srv.stopStreams) })
		e.srv.relays.drain(ctx)
	}()
	if m, _ := w.until(MsgEnded); m.Reason != EndedDaemonStopping {
		t.Errorf("ended = %+v", m)
	}
}

func TestRelayAttachErrorIsRetried(t *testing.T) {
	f := newFakeRelaySession(t, "wolf", true, 1)
	f.attachErr = errors.New("socket refused")
	e := newRelayEnv(t, map[string]*fakeRelaySession{"wolf-x": f})
	w := e.dial(t, "wolf-x", "")
	w.live()
	eventually(t, "a failed attach", func() bool { a, _ := f.counts(); return a >= 1 })
	f.mu.Lock()
	f.attachErr = nil
	f.mu.Unlock()
	eventually(t, "a retried attach", func() bool { _, open := f.counts(); return open == 1 })
	if len(e.logged("api_relay_attach_error")) == 0 {
		t.Error("attach error not logged")
	}
}

func TestRelayRingDropsTooSlowBrowser(t *testing.T) {
	h := newRelayHub(nil, make(chan struct{}))
	h.ringSize = 4
	r := newRelay(h, RelaySession{ID: "x"})
	for i := range 10 {
		r.addFrames([]LogFrame{{Start: int64(i), End: int64(i + 1), Data: []byte(`{"n":1}`)}})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.base == 0 || len(r.ring) >= 2*h.ringSize || r.next() != 10 {
		t.Errorf("ring base=%d len=%d next=%d", r.base, len(r.ring), r.next())
	}
}

func TestTurnSignal(t *testing.T) {
	for _, tc := range []struct {
		frame, state, stop string
		ok                 bool
	}{
		{frame: frameEnd, state: TurnIdle, stop: "end_turn", ok: true},
		{frame: frameUpdate, state: TurnRunning, ok: true},
		{frame: frameNew},
		{frame: `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"available_commands_update"}}}`},
		{frame: `{"jsonrpc":"2.0","id":4,"error":{"code":-32603,"message":"boom"}}`, state: TurnIdle, stop: turnError, ok: true},
		{frame: `not json`},
	} {
		state, stop, ok := turnSignal([]byte(tc.frame))
		if state != tc.state || stop != tc.stop || ok != tc.ok {
			t.Errorf("turnSignal(%s) = %q %q %v", tc.frame, state, stop, ok)
		}
	}
}
