package acpchat

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/session"
)

// fakeAgent stands in for acp-wrapper's bridge + the sandboxed ACP agent: a unix
// socket at <root>/<id>/agent.sock whose clients all see every agent frame (which
// is also appended to stream.log, like the real hub) and whose requests are
// answered by a tiny ACP agent. onPrompt scripts what a prompt does.
type fakeAgent struct {
	t    *testing.T
	root string
	id   string
	ln   net.Listener
	log  *os.File

	mu       sync.Mutex
	clients  map[net.Conn]struct{}
	seen     []seenReq
	nextSess int
	replies  chan rpcFrame // client→agent responses (permission answers)
	cancels  chan string   // session ids from session/cancel

	// onPrompt handles a session/prompt; default echoes. It runs on its own
	// goroutine, so it may block.
	onPrompt func(a *fakeAgent, id int, sessionID, text string)
}

type seenReq struct {
	Method    string
	ID        int
	SessionID string
}

type rpcFrame struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	root, err := os.MkdirTemp("", "acpchat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	a := &fakeAgent{
		t: t, root: root, id: "wolf-test",
		clients: map[net.Conn]struct{}{},
		replies: make(chan rpcFrame, 16),
		cancels: make(chan string, 16),
	}
	a.onPrompt = func(a *fakeAgent, id int, sessionID, text string) {
		a.chunk(sessionID, "echo: "+text)
		a.result(id, map[string]any{"stopReason": "end_turn"})
	}
	dir := filepath.Join(root, a.id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if a.log, err = os.OpenFile(filepath.Join(dir, "stream.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
		t.Fatal(err)
	}
	a.listen()
	t.Cleanup(a.stop)
	a.writeSession(session.StatusRunning)
	return a
}

func (a *fakeAgent) dir() string    { return filepath.Join(a.root, a.id) }
func (a *fakeAgent) socket() string { return filepath.Join(a.dir(), session.SocketName) }

func (a *fakeAgent) listen() {
	ln, err := net.Listen("unix", a.socket())
	if err != nil {
		a.t.Fatal(err)
	}
	a.ln = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			a.mu.Lock()
			a.clients[conn] = struct{}{}
			a.mu.Unlock()
			go a.serve(conn)
		}
	}()
}

func (a *fakeAgent) writeSession(status session.Status) {
	store := session.Store{Root: a.root}
	err := store.Write(session.Session{
		ID: a.id, Status: status, SocketPath: a.socket(), Persistent: true,
		CreatedAt: time.Unix(1_700_000_000, 0), Workspaces: []string{"/ws"},
		LogPath: filepath.Join(a.dir(), "stream.log"),
	})
	if err != nil {
		a.t.Fatal(err)
	}
}

func (a *fakeAgent) options() Options {
	return Options{Root: a.root, RetryInterval: 10 * time.Millisecond, ConnectTimeout: 2 * time.Second, DiscoverTimeout: 2 * time.Second}
}

func (a *fakeAgent) serve(conn net.Conn) {
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			a.handle(line)
		}
		if err != nil {
			a.mu.Lock()
			delete(a.clients, conn)
			a.mu.Unlock()
			conn.Close()
			return
		}
	}
}

func (a *fakeAgent) handle(line []byte) {
	var f rpcFrame
	if json.Unmarshal(line, &f) != nil {
		return
	}
	if f.Method == "" { // a client's reply to something we asked
		a.replies <- f
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(f.Params, &p)
	id := -1
	if f.ID != nil {
		id = *f.ID
	}
	a.mu.Lock()
	a.seen = append(a.seen, seenReq{Method: f.Method, ID: id, SessionID: p.SessionID})
	a.mu.Unlock()

	switch f.Method {
	case "initialize":
		a.result(id, map[string]any{"protocolVersion": 1})
	case "session/new":
		a.mu.Lock()
		a.nextSess++
		sid := "sess-" + string(rune('0'+a.nextSess))
		a.mu.Unlock()
		a.result(id, map[string]any{"sessionId": sid})
	case "session/set_mode":
		a.result(id, map[string]any{})
	case "session/cancel":
		a.cancels <- p.SessionID
	case "session/prompt":
		text := ""
		if len(p.Prompt) > 0 {
			text = p.Prompt[0].Text
		}
		go a.onPrompt(a, id, p.SessionID, text)
	}
}

// send broadcasts one frame to every client and records it in stream.log.
func (a *fakeAgent) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		a.t.Error(err)
		return
	}
	data = append(data, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.log.Write(data)
	for c := range a.clients {
		_, _ = c.Write(data)
	}
}

func (a *fakeAgent) result(id int, v any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": v})
}

func (a *fakeAgent) update(sessionID string, upd map[string]any) {
	a.send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": sessionID, "update": upd}})
}

func (a *fakeAgent) chunk(sessionID, text string) {
	a.update(sessionID, map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
	})
}

func (a *fakeAgent) thought(sessionID, text string) {
	a.update(sessionID, map[string]any{
		"sessionUpdate": "agent_thought_chunk",
		"content":       map[string]any{"type": "text", "text": text},
	})
}

func (a *fakeAgent) toolCall(sessionID, id string) {
	a.update(sessionID, map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": id, "title": "Run ls", "kind": "execute", "status": "in_progress",
		"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "TOOL OUTPUT NOISE"}}},
	})
}

// dropClients severs every client connection, as the bridge does to a lagging
// watcher.
func (a *fakeAgent) dropClients() {
	a.mu.Lock()
	for c := range a.clients {
		c.Close()
	}
	a.mu.Unlock()
}

func (a *fakeAgent) stop() {
	a.ln.Close()
	a.dropClients()
}

func (a *fakeAgent) numClients() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.clients)
}

// requests returns a copy of the requests seen so far.
func (a *fakeAgent) requests() []seenReq {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]seenReq(nil), a.seen...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
