package acpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agentConn is the fake ACP agent side of a connection: it reads the client's
// newline-delimited JSON-RPC requests and writes responses/notifications back,
// standing in for the acp-wrapper bridge + sandboxed claude-agent-acp.
type agentConn struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

// readRequest blocks for the next request line and decodes it. It fails the test
// on a read error so a hung handshake surfaces as a test failure, not a deadlock.
func (a *agentConn) readRequest() request {
	a.t.Helper()
	line, err := a.br.ReadBytes('\n')
	if err != nil {
		a.t.Fatalf("agent read: %v", err)
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		a.t.Fatalf("agent decode request %q: %v", line, err)
	}
	return req
}

func (a *agentConn) write(v any) {
	a.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		a.t.Fatalf("agent marshal: %v", err)
	}
	if _, err := a.c.Write(append(data, '\n')); err != nil {
		a.t.Fatalf("agent write: %v", err)
	}
}

// result replies to request id with a JSON-RPC result.
func (a *agentConn) result(id int, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		a.t.Fatalf("agent marshal result: %v", err)
	}
	a.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(raw)})
}

// rpcErr replies to request id with a JSON-RPC error.
func (a *agentConn) rpcErr(id, code int, message string) {
	a.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

// holdOpen blocks until the client closes its end, keeping the connection alive
// between/after turns the way a real agent does. Returning from a handler closes
// the pipe, so without this the test would race a turn's completion against the
// disconnect; holding open until the client hangs up keeps that race out of the
// tests that assert on a completed turn.
func (a *agentConn) holdOpen() {
	for {
		if _, err := a.br.ReadBytes('\n'); err != nil {
			return
		}
	}
}

// updateText sends a session/update notification streaming one assistant chunk.
func (a *agentConn) updateText(sessionID, kind, text string) {
	a.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update": map[string]any{
				"sessionUpdate": kind,
				"content":       map[string]any{"type": "text", "text": text},
			},
		},
	})
}

// newTestClient wires a Client to a fake agent over an in-memory pipe and runs
// handler as the agent. The pipe stands in for agent.sock; net.Pipe gives a
// faithful streaming, framed-by-newline transport without touching the disk.
func newTestClient(t *testing.T, handler func(a *agentConn)) *Client {
	t.Helper()
	clientConn, agentSide := net.Pipe()
	a := &agentConn{t: t, c: agentSide, br: bufio.NewReader(agentSide)}
	go func() {
		defer agentSide.Close()
		handler(a)
	}()
	c := newClient(clientConn)
	t.Cleanup(func() { c.Close() })
	return c
}

// stdAgent answers the initialize + session/new handshake, then serves prompts:
// for each session/prompt it streams the given chunks as agent_message_chunk
// updates and replies with stopReason. It returns after one prompt unless more
// are expected; the test drives how many prompts it sends.
func stdHandshake(a *agentConn, sessionID string) {
	init := a.readRequest()
	if init.Method != methodInitialize {
		a.t.Fatalf("first request = %q, want initialize", init.Method)
	}
	a.result(init.ID, map[string]any{
		"protocolVersion": 1,
		"agentInfo":       map[string]any{"name": "fake", "version": "0.0.1"},
	})

	newReq := a.readRequest()
	if newReq.Method != methodSessionNew {
		a.t.Fatalf("second request = %q, want session/new", newReq.Method)
	}
	a.result(newReq.ID, map[string]any{"sessionId": sessionID})
}

func TestConnectAndPromptStreamsChunks(t *testing.T) {
	const sessionID = "sess-1"
	c := newTestClient(t, func(a *agentConn) {
		stdHandshake(a, sessionID)

		prompt := a.readRequest()
		if prompt.Method != methodPrompt {
			t.Errorf("third request = %q, want session/prompt", prompt.Method)
		}
		// The prompt text must round-trip into a single text content block.
		var p promptParams
		if err := json.Unmarshal(prompt.Params, &p); err != nil {
			t.Fatalf("decode prompt params: %v", err)
		}
		if p.SessionID != sessionID {
			t.Errorf("prompt sessionId = %q, want %q", p.SessionID, sessionID)
		}
		if len(p.Prompt) != 1 || p.Prompt[0].Type != "text" || p.Prompt[0].Text != "hi agent" {
			t.Errorf("prompt blocks = %+v, want one text block %q", p.Prompt, "hi agent")
		}

		a.updateText(sessionID, updateAgentMessageChunk, "Hello")
		a.updateText(sessionID, updateAgentMessageChunk, " world")
		a.result(prompt.ID, map[string]any{"stopReason": "end_turn"})
		a.holdOpen()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Connect(ctx, "/work"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := c.SessionID(); got != sessionID {
		t.Errorf("SessionID() = %q, want %q", got, sessionID)
	}
	if got := c.State(); got != StateConnected {
		t.Errorf("State after Connect = %q, want connected", got)
	}

	stop, err := c.Prompt(ctx, "hi agent")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", stop)
	}

	// Drain the events emitted across Connect+Prompt and assert the lifecycle and
	// the incrementally streamed assistant text.
	var states []State
	var text string
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatalf("events channel closed before completion; got states %v", states)
			}
			states = append(states, ev.State)
			if ev.State == StateStreaming {
				text += ev.Text
			}
			if ev.State == StateCompleted {
				if ev.StopReason != "end_turn" {
					t.Errorf("completed event StopReason = %q, want end_turn", ev.StopReason)
				}
				goto done
			}
		case <-ctx.Done():
			t.Fatalf("timed out collecting events; got states %v", states)
		}
	}
done:
	if text != "Hello world" {
		t.Errorf("streamed text = %q, want %q", text, "Hello world")
	}
	// Must observe the connected marker and at least one streaming event before
	// completion, in that order.
	if states[0] != StateConnected {
		t.Errorf("first event state = %q, want connected", states[0])
	}
	if !containsState(states, StateStreaming) {
		t.Errorf("no streaming event in %v", states)
	}
}

func TestThoughtChunksAlsoStream(t *testing.T) {
	const sessionID = "sess-think"
	c := newTestClient(t, func(a *agentConn) {
		stdHandshake(a, sessionID)
		prompt := a.readRequest()
		a.updateText(sessionID, updateAgentThoughtChunk, "thinking...")
		a.updateText(sessionID, updateAgentMessageChunk, "answer")
		a.result(prompt.ID, map[string]any{"stopReason": "end_turn"})
		a.holdOpen()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Connect(ctx, "/work"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.Prompt(ctx, "q"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var text string
	for ev := range drainUntilCompleted(t, ctx, c) {
		if ev.State == StateStreaming {
			text += ev.Text
		}
	}
	if text != "thinking...answer" {
		t.Errorf("streamed text = %q, want %q", text, "thinking...answer")
	}
}

func TestSandboxGoneSignalsDisconnect(t *testing.T) {
	// The bridge closes a freshly accepted connection immediately when the hub
	// has already shut down (the ACP client's sandbox is gone). Model that: the
	// agent closes without answering initialize.
	c := newTestClient(t, func(a *agentConn) {
		// Read the initialize request so the client's write completes on the
		// pipe, then drop the connection without replying.
		_ = a.readRequest()
		a.c.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Connect(ctx, "/work")
	if !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("Connect after sandbox gone = %v, want ErrConnectionClosed", err)
	}

	// The lifecycle must terminate at StateDisconnected so the caller can clean
	// up the dead session directory.
	ev := waitTerminal(t, ctx, c)
	if ev.State != StateDisconnected {
		t.Errorf("terminal state = %q, want disconnected", ev.State)
	}
	if !ev.State.IsTerminal() {
		t.Errorf("disconnect state should be terminal")
	}
	if got := c.State(); got != StateDisconnected {
		t.Errorf("State() = %q, want disconnected", got)
	}
}

func TestDisconnectMidTurnFailsPrompt(t *testing.T) {
	const sessionID = "sess-drop"
	c := newTestClient(t, func(a *agentConn) {
		stdHandshake(a, sessionID)
		_ = a.readRequest() // the prompt
		a.updateText(sessionID, updateAgentMessageChunk, "partial")
		// Drop the connection mid-turn without a stopReason.
		a.c.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Connect(ctx, "/work"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err := c.Prompt(ctx, "q")
	if !errors.Is(err, ErrConnectionClosed) {
		t.Errorf("Prompt mid-turn drop = %v, want ErrConnectionClosed", err)
	}
}

func TestPromptBeforeConnect(t *testing.T) {
	c := newTestClient(t, func(a *agentConn) {
		// Never completes the handshake; the test errors out before any read.
		<-time.After(time.Second)
	})
	_, err := c.Prompt(context.Background(), "hi")
	if err == nil {
		t.Fatal("Prompt before Connect should error")
	}
}

func TestAgentErrorPropagates(t *testing.T) {
	c := newTestClient(t, func(a *agentConn) {
		init := a.readRequest()
		a.rpcErr(init.ID, -32600, "bad protocol version")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Connect(ctx, "/work")
	if err == nil {
		t.Fatal("Connect should propagate the agent's initialize error")
	}
	if got := err.Error(); !strings.Contains(got, "bad protocol version") {
		t.Errorf("error = %q, want it to mention the agent message", got)
	}
}

func TestDialStaleSocketFails(t *testing.T) {
	// A session.json may point at a socket whose wrapper is gone: nothing is
	// listening, so the dial itself fails (distinct from connect-then-drop).
	stale := filepath.Join(t.TempDir(), "agent.sock")
	_, err := Dial(context.Background(), stale)
	if err == nil {
		t.Fatal("Dial to a non-existent socket should fail")
	}
}

func TestContextCancelUnblocksCall(t *testing.T) {
	// An agent that accepts but never replies: Connect must honour ctx, not hang.
	c := newTestClient(t, func(a *agentConn) {
		_ = a.readRequest()
		<-time.After(2 * time.Second)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.Connect(ctx, "/work")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Connect with expiring ctx = %v, want DeadlineExceeded", err)
	}
}

// --- helpers ---

func containsState(states []State, want State) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

// drainUntilCompleted returns a channel of Events up to and including the
// StateCompleted event, then closes. Fails the test if the context expires first.
func drainUntilCompleted(t *testing.T, ctx context.Context, c *Client) <-chan Event {
	t.Helper()
	out := make(chan Event, eventBuffer)
	go func() {
		defer close(out)
		for {
			select {
			case ev, ok := <-c.Events():
				if !ok {
					return
				}
				out <- ev
				if ev.State == StateCompleted || ev.State.IsTerminal() {
					return
				}
			case <-ctx.Done():
				t.Errorf("timed out draining events")
				return
			}
		}
	}()
	return out
}

// waitTerminal blocks for the terminal lifecycle event.
func waitTerminal(t *testing.T, ctx context.Context, c *Client) Event {
	t.Helper()
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatalf("events channel closed without a terminal event")
				return Event{}
			}
			if ev.State.IsTerminal() {
				return ev
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for terminal event")
			return Event{}
		}
	}
}
