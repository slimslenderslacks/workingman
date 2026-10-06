package acpclient

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Adopt must bind to an existing session with NO wire traffic: the first frame
// the agent sees is the session/prompt itself, addressed to the adopted id.
func TestAdoptPromptsExistingSessionWithoutHandshake(t *testing.T) {
	c := newTestClientOpts(t, Options{}, func(a *agentConn) {
		req := a.readRequest()
		if req.Method != methodPrompt {
			t.Errorf("first request = %q, want session/prompt (no initialize/session/new)", req.Method)
		}
		var p promptParams
		_ = json.Unmarshal(req.Params, &p)
		if p.SessionID != "existing-7" {
			t.Errorf("prompt sessionId = %q, want existing-7", p.SessionID)
		}
		a.updateText("existing-7", updateAgentMessageChunk, "hi")
		a.result(req.ID, map[string]any{"stopReason": "end_turn"})
		a.holdOpen()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Adopt("existing-7"); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := c.SessionID(); got != "existing-7" {
		t.Errorf("SessionID() = %q", got)
	}
	if got := c.State(); got != StateConnected {
		t.Errorf("State after Adopt = %q, want connected", got)
	}
	if _, err := c.Prompt(ctx, "ping"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var sawText bool
	for ev := range drainUntilCompleted(t, ctx, c) {
		if ev.Kind == EventStream && ev.Text == "hi" {
			sawText = true
			if ev.SessionID != "existing-7" {
				t.Errorf("chunk SessionID = %q, want existing-7", ev.SessionID)
			}
		}
	}
	if !sawText {
		t.Error("no streamed chunk observed")
	}
}

func TestAdoptRejectsEmptyID(t *testing.T) {
	c := newTestClientOpts(t, Options{}, func(a *agentConn) { a.holdOpen() })
	if err := c.Adopt(""); err == nil {
		t.Error("Adopt(\"\") succeeded, want error")
	}
}

// Re-adopting re-points the client without emitting a second StateConnected.
func TestAdoptRebindDoesNotEmit(t *testing.T) {
	c := newTestClientOpts(t, Options{}, func(a *agentConn) { a.holdOpen() })
	if err := c.Adopt("one"); err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt("two"); err != nil {
		t.Fatal(err)
	}
	if got := c.SessionID(); got != "two" {
		t.Errorf("SessionID() = %q, want two", got)
	}
	<-c.Events() // the first adoption's StateConnected
	select {
	case ev := <-c.Events():
		t.Errorf("unexpected second event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// Cancel is a notification: a session/cancel frame with no "id" member (an id —
// even 0 — would make the agent treat it as a request awaiting a reply).
func TestCancelSendsIDlessNotification(t *testing.T) {
	got := make(chan map[string]json.RawMessage, 1)
	c := newTestClientOpts(t, Options{}, func(a *agentConn) {
		line, err := a.br.ReadBytes('\n')
		if err != nil {
			return
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(line, &m)
		got <- m
		a.holdOpen()
	})
	if err := c.Cancel(); err == nil {
		t.Error("Cancel before Adopt succeeded, want error")
	}
	if err := c.Adopt("s1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case m := <-got:
		if string(m["method"]) != `"session/cancel"` {
			t.Errorf("method = %s", m["method"])
		}
		if _, hasID := m["id"]; hasID {
			t.Errorf("cancel frame carries an id: %s", m["id"])
		}
		if string(m["params"]) != `{"sessionId":"s1"}` {
			t.Errorf("params = %s", m["params"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent never saw the cancel frame")
	}
}

// DistinctIDs moves request ids far above an ordinary client's range, so a
// sibling client's response to its own small id can't complete our call.
func TestDistinctIDsAvoidSmallIDs(t *testing.T) {
	ids := make(chan int, 1)
	c := newTestClientOpts(t, Options{DistinctIDs: true}, func(a *agentConn) {
		req := a.readRequest()
		ids <- req.ID
		// A sibling's response to ITS id 1 must not satisfy our call...
		a.result(1, map[string]any{"stopReason": "end_turn"})
		// ...ours does.
		a.result(req.ID, map[string]any{"stopReason": "max_tokens"})
		a.holdOpen()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Adopt("s"); err != nil {
		t.Fatal(err)
	}
	stop, err := c.Prompt(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if stop != "max_tokens" {
		t.Errorf("stopReason = %q: a foreign response completed our call", stop)
	}
	if id := <-ids; id < 1<<20 {
		t.Errorf("request id = %d, want >= 2^20", id)
	}
}

// With ObserveResponses, a response nobody here is waiting for surfaces as an
// ordered EventResponse; without it, it is dropped (the TUI's behaviour).
func TestObserveResponsesEmitsForeignResponses(t *testing.T) {
	c := newTestClientOpts(t, Options{ObserveResponses: true}, func(a *agentConn) {
		a.updateText("s", updateAgentMessageChunk, "before")
		a.result(99, map[string]any{"sessionId": "s2"})
		a.holdOpen()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var kinds []EventKind
	var res json.RawMessage
	for len(kinds) < 2 {
		select {
		case ev := <-c.Events():
			kinds = append(kinds, ev.Kind)
			if ev.Kind == EventResponse {
				res = ev.Result
			}
		case <-ctx.Done():
			t.Fatalf("timed out; kinds=%v", kinds)
		}
	}
	if kinds[0] != EventStream || kinds[1] != EventResponse {
		t.Errorf("kinds = %v, want [stream response] in stream order", kinds)
	}
	if string(res) != `{"sessionId":"s2"}` {
		t.Errorf("result = %s", res)
	}
}

func TestForeignResponsesDroppedByDefault(t *testing.T) {
	c := newTestClientOpts(t, Options{}, func(a *agentConn) {
		a.result(99, map[string]any{"sessionId": "s2"})
		a.holdOpen()
	})
	select {
	case ev := <-c.Events():
		t.Errorf("unexpected event %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// OnRequest serves agent→client requests (off the read loop) and a handler's
// ErrMethodNotFound keeps the historical -32601 reply.
func TestOnRequestServesAgentRequests(t *testing.T) {
	handler := func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method == MethodRequestPermission {
			return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
		}
		return nil, ErrMethodNotFound
	}
	replies := make(chan map[string]json.RawMessage, 2)
	c := newTestClientOpts(t, Options{OnRequest: handler}, func(a *agentConn) {
		a.write(map[string]any{"jsonrpc": "2.0", "id": 5, "method": MethodRequestPermission, "params": map[string]any{}})
		a.write(map[string]any{"jsonrpc": "2.0", "id": 6, "method": "fs/read_text_file", "params": map[string]any{}})
		for i := 0; i < 2; i++ {
			line, err := a.br.ReadBytes('\n')
			if err != nil {
				return
			}
			var m map[string]json.RawMessage
			_ = json.Unmarshal(line, &m)
			replies <- m
		}
		a.holdOpen()
	})
	_ = c
	got := map[string]map[string]json.RawMessage{}
	for i := 0; i < 2; i++ {
		select {
		case m := <-replies:
			got[string(m["id"])] = m
		case <-time.After(2 * time.Second):
			t.Fatal("agent got fewer than 2 replies")
		}
	}
	if string(got["5"]["result"]) != `{"outcome":{"outcome":"cancelled"}}` {
		t.Errorf("permission reply = %v", got["5"])
	}
	var e rpcError
	_ = json.Unmarshal(got["6"]["error"], &e)
	if e.Code != -32601 {
		t.Errorf("unhandled method error code = %d, want -32601", e.Code)
	}
}

// A handler blocked on a human must not stall the read loop, and its context
// is cancelled when the connection goes away.
func TestOnRequestHandlerDoesNotBlockReadLoopAndSeesDisconnect(t *testing.T) {
	started := make(chan struct{})
	ctxDone := make(chan struct{})
	handler := func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		close(ctxDone)
		return nil, ctx.Err()
	}
	c := newTestClientOpts(t, Options{OnRequest: handler}, func(a *agentConn) {
		a.write(map[string]any{"jsonrpc": "2.0", "id": 5, "method": MethodRequestPermission, "params": map[string]any{}})
		<-started
		a.updateText("s", updateAgentMessageChunk, "still flowing")
		a.holdOpen()
	})
	select {
	case ev := <-c.Events():
		if ev.Text != "still flowing" {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read loop stalled behind the blocked handler")
	}
	c.Close()
	select {
	case <-ctxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler ctx not cancelled on disconnect")
	}
}
