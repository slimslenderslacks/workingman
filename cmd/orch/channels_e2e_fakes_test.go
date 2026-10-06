package main

// Fakes for the channels end-to-end test (channels_e2e_test.go): a Graph API,
// ACP agents behind real unix sockets, and a goroutine-leak check. Nothing here
// touches the network beyond loopback or starts a real sandbox.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/session"
)

// lockedBuffer is an io.Writer whose contents can be read while it is written.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// --- fake Graph API ---

// sentMessage is one text message the daemon sent through the fake Graph API.
type sentMessage struct {
	To, Body, ReplyTo, ID string
}

// e2eGraph records every text message POSTed to /<version>/<phone>/messages and
// answers with a fresh wamid, like Meta does. Typing indicators and read
// receipts go to the same endpoint and are acknowledged but not recorded.
type e2eGraph struct {
	srv *httptest.Server

	mu   sync.Mutex
	sent []sentMessage
	next int
}

func newE2EGraph(t *testing.T) *e2eGraph {
	t.Helper()
	g := &e2eGraph{}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *e2eGraph) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		To      string                `json:"to"`
		Type    string                `json:"type"`
		Text    struct{ Body string } `json:"text"`
		Context struct {
			MessageID string `json:"message_id"`
		} `json:"context"`
	}
	_ = json.Unmarshal(raw, &body)
	g.mu.Lock()
	g.next++
	id := fmt.Sprintf("wamid.OUT%d", g.next)
	if body.Type == "text" {
		g.sent = append(g.sent, sentMessage{To: body.To, Body: body.Text.Body, ReplyTo: body.Context.MessageID, ID: id})
	}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"messaging_product":"whatsapp","contacts":[{"input":%q,"wa_id":%q}],"messages":[{"id":%q}]}`, body.To, body.To, id)
}

func (g *e2eGraph) messages() []sentMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]sentMessage(nil), g.sent...)
}

// find returns the sent messages whose body contains substr.
func (g *e2eGraph) find(substr string) []sentMessage {
	var out []sentMessage
	for _, m := range g.messages() {
		if strings.Contains(m.Body, substr) {
			out = append(out, m)
		}
	}
	return out
}

// --- fake ACP agent ---

// fakeACP stands in for acp-wrapper's bridge plus the sandboxed ACP agent: a unix
// socket at <session dir>/agent.sock whose clients see every agent frame (also
// appended to stream.log, like the real bridge) and whose requests a tiny ACP
// agent answers. reply decides what a session/prompt answers.
type fakeACP struct {
	dir   string
	ln    net.Listener
	log   *os.File
	reply func(prompt string) string

	mu       sync.Mutex
	clients  map[net.Conn]struct{}
	prompts  []string
	nextSess int
	wg       sync.WaitGroup
}

type acpFrame struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func startFakeACP(dir string, reply func(prompt string) string) (*fakeACP, error) {
	log, err := os.OpenFile(filepath.Join(dir, "stream.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", filepath.Join(dir, session.SocketName))
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	a := &fakeACP{dir: dir, ln: ln, log: log, reply: reply, clients: map[net.Conn]struct{}{}}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			a.mu.Lock()
			a.clients[conn] = struct{}{}
			a.mu.Unlock()
			a.wg.Add(1)
			go func() {
				defer a.wg.Done()
				a.serve(conn)
			}()
		}
	}()
	return a, nil
}

func (a *fakeACP) serve(conn net.Conn) {
	defer func() {
		a.mu.Lock()
		delete(a.clients, conn)
		a.mu.Unlock()
		conn.Close()
	}()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			a.handle(line)
		}
		if err != nil {
			return
		}
	}
}

func (a *fakeACP) handle(line []byte) {
	var f acpFrame
	if json.Unmarshal(line, &f) != nil || f.Method == "" {
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
	switch f.Method {
	case "initialize":
		a.result(id, map[string]any{"protocolVersion": 1})
	case "session/new":
		a.mu.Lock()
		a.nextSess++
		sid := fmt.Sprintf("acp-%d", a.nextSess)
		a.mu.Unlock()
		a.result(id, map[string]any{"sessionId": sid})
	case "session/set_mode":
		a.result(id, map[string]any{})
	case "session/prompt":
		text := ""
		if len(p.Prompt) > 0 {
			text = p.Prompt[0].Text
		}
		a.mu.Lock()
		a.prompts = append(a.prompts, text)
		a.mu.Unlock()
		// A prompt runs on its own goroutine, like the real agent's turn.
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.chunk(p.SessionID, a.reply(text))
			a.result(id, map[string]any{"stopReason": "end_turn"})
		}()
	}
}

func (a *fakeACP) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
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

func (a *fakeACP) result(id int, v any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": v})
}

func (a *fakeACP) chunk(sessionID, text string) {
	a.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": text},
		},
	}})
}

// seedConversation records an ACP session id in stream.log, as the TUI's
// watcher does when it runs session/new on a freshly discovered session.
func (a *fakeACP) seedConversation(sessionID string) { a.chunk(sessionID, "(session started)") }

func (a *fakeACP) promptsSeen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.prompts...)
}

func (a *fakeACP) stop() {
	_ = a.ln.Close()
	a.mu.Lock()
	for c := range a.clients {
		c.Close()
	}
	a.mu.Unlock()
	a.wg.Wait()
	_ = a.log.Close()
}

// --- fake ACP launcher ---

// fakeACPLauncher is the runner's AcpLauncher: instead of an acp-wrapper
// process it starts a fakeACP in the session directory the runner prepared and
// marks the session running. Wolves get a seeded conversation (the TUI is
// assumed to be driving them); the workingman agent does not (headless).
type fakeACPLauncher struct {
	root   string
	script func(kind agent.Kind) func(prompt string) string

	mu     sync.Mutex
	agents map[string]*fakeACP // by session name
	sess   []*fakeACPSession
	kinds  map[string]agent.Kind
}

func (l *fakeACPLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	store := session.Store{Root: l.root}
	dir := store.Dir(spec.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	a, err := startFakeACP(dir, l.script(spec.Kind))
	if err != nil {
		return nil, err
	}
	if spec.Kind == agent.WolfAgent {
		a.seedConversation("wolf-conversation")
	}
	rec, err := store.Read(spec.Name)
	if err != nil {
		a.stop()
		return nil, err
	}
	rec.Status = session.StatusRunning
	rec.UpdatedAt = time.Now()
	if err := store.Write(rec); err != nil {
		a.stop()
		return nil, err
	}
	l.mu.Lock()
	if l.agents == nil {
		l.agents, l.kinds = map[string]*fakeACP{}, map[string]agent.Kind{}
	}
	l.agents[spec.Name] = a
	l.kinds[spec.Name] = spec.Kind
	l.mu.Unlock()
	s := &fakeACPSession{name: spec.Name, done: make(chan struct{})}
	l.mu.Lock()
	l.sess = append(l.sess, s)
	l.mu.Unlock()
	return s, nil
}

// ofKind returns the agent launched for the first session of kind, or nil.
func (l *fakeACPLauncher) ofKind(k agent.Kind) *fakeACP {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, a := range l.agents {
		if l.kinds[name] == k {
			return a
		}
	}
	return nil
}

func (l *fakeACPLauncher) stopAll() {
	l.mu.Lock()
	agents, sessions := l.agents, l.sess
	l.agents, l.sess = nil, nil
	l.mu.Unlock()
	for _, a := range agents {
		a.stop()
	}
	// A wrapper process that dies ends its session's Wait; so does this.
	for _, s := range sessions {
		_ = s.Close()
	}
}

// fakeACPSession is the agent.Session for a fakeACP: Wait blocks until the
// session is closed or ctx ends, as the process-backed sessions do.
type fakeACPSession struct {
	name string
	done chan struct{}
	once sync.Once
}

func (s *fakeACPSession) Name() string { return s.name }

func (s *fakeACPSession) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	}
}

func (s *fakeACPSession) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// --- goroutine-leak check ---

// goroutineIDs returns the ids of the goroutines running now.
func goroutineIDs() map[string]bool {
	ids := map[string]bool{}
	for _, g := range goroutineDump() {
		ids[goroutineID(g)] = true
	}
	return ids
}

func goroutineDump() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	return strings.Split(strings.TrimSpace(string(buf)), "\n\n")
}

func goroutineID(dump string) string {
	if f := strings.Fields(dump); len(f) >= 2 {
		return f[1]
	}
	return ""
}

// leakedGoroutines returns the stacks of goroutines that are not in before and
// run this module's code. The standard library's own helpers (HTTP idle
// connections, timers) are ignored: only goroutines the daemon, the router or a
// channel started and failed to stop are of interest. It retries for a while,
// since goroutines finish asynchronously after Close returns.
func leakedGoroutines(before map[string]bool, wait time.Duration) []string {
	deadline := time.Now().Add(wait)
	for {
		var leaked []string
		for _, g := range goroutineDump() {
			if before[goroutineID(g)] || !strings.Contains(g, "github.com/slimslenderslacks/work/") {
				continue
			}
			leaked = append(leaked, g)
		}
		if len(leaked) == 0 || time.Now().After(deadline) {
			return leaked
		}
		time.Sleep(25 * time.Millisecond)
	}
}
