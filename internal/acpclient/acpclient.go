// Package acpclient is the workingman TUI's side of an ACP session: it dials a
// session's agent.sock (the unix-domain socket the acp-wrapper bridge exposes at
// ~/.workingman/sessions/<id>/agent.sock), speaks the Agent Client Protocol over
// it, sends prompts to the sandboxed Claude agent, and decodes the streamed
// assistant output for incremental display.
//
// The transport is newline-delimited JSON-RPC 2.0 (see protocol.go and the
// acp-wrapper bridge). A single Client owns one connection and one read loop. The
// read loop classifies every inbound frame: responses unblock the in-flight
// request that issued them; session/update notifications are decoded into
// Events. Callers drive the session with the blocking Connect/Prompt methods and,
// concurrently, range over Events() to render streamed chunks and react to the
// connection's lifecycle (connected → streaming → completed, and
// disconnected/errored on the way out).
//
// Lifecycle signalling is the contract downstream tasks build on: the session
// tabs render from Events, and cleanup keys off StateDisconnected. In particular,
// a socket whose backing sandbox is gone accepts the connection and then closes
// it immediately (the bridge tears down clients once the ACP client's stdout
// hits EOF); the read loop surfaces that as a StateDisconnected event and fails
// every pending call with ErrConnectionClosed, so the caller can clean up the
// dead session directory.
//
// Concurrency: Connect, Prompt, and Close are each safe to call from one
// goroutine while another ranges over Events(); the read loop is the sole emitter
// on the events channel and is the sole closer of it. Several TUIs can share one
// session through the bridge's fan-out, but request/response correlation assumes
// a single *driving* client per session — additional clients should watch
// (consume Events) rather than issue their own requests, since the shared agent
// stdout would deliver every client's responses to every client.
package acpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// ErrConnectionClosed is returned by Connect/Prompt (and any other pending call)
// when the connection drops before the response arrives. It is the signal that
// the socket's backing sandbox is gone or the agent exited: the caller (e.g. the
// cleanup-lifecycle path) can treat it as "this session is dead, reclaim it".
var ErrConnectionClosed = errors.New("acpclient: connection closed")

// State is the coarse lifecycle of a Client's connection, surfaced on every
// Event so the TUI can drive UI and cleanup off a single stream of transitions.
type State string

const (
	// StateConnecting is the initial state: dialed, handshake not yet complete.
	StateConnecting State = "connecting"
	// StateConnected means initialize + session/new succeeded; the session is
	// live and ready for a prompt.
	StateConnected State = "connected"
	// StateStreaming means a prompt turn is in flight and assistant chunks are
	// arriving. Each streamed chunk is delivered as a StateStreaming Event whose
	// Text holds the incremental assistant output.
	StateStreaming State = "streaming"
	// StateCompleted means the in-flight turn finished; the Event's StopReason
	// says why (e.g. "end_turn"). The session returns to being ready for another
	// prompt.
	StateCompleted State = "completed"
	// StateDisconnected means the connection closed — cleanly (agent exited) or
	// because the backing sandbox is gone. The Event's Err is nil for a clean
	// EOF and set for an unexpected read error. Terminal.
	StateDisconnected State = "disconnected"
	// StateErrored means a protocol/transport fault the client couldn't recover
	// from (bad frame, write failure). The Event's Err carries the cause.
	// Terminal.
	StateErrored State = "errored"
)

// IsTerminal reports whether no further Events follow a transition into s. The
// TUI uses it to know when to stop watching a Client and trigger cleanup.
func (s State) IsTerminal() bool {
	return s == StateDisconnected || s == StateErrored
}

// Event is one transition or streamed delta on a Client's connection. Exactly
// one Event shape is produced per State:
//
//   - StateConnecting / StateConnected: lifecycle markers, no payload.
//   - StateStreaming: Text holds an incremental assistant chunk (empty Text
//     marks the start of a turn, before the first chunk).
//   - StateCompleted: StopReason holds the turn's terminal reason.
//   - StateDisconnected / StateErrored: Err holds the cause (nil for a clean
//     disconnect).
//
// Events are delivered in order on the channel returned by Events(); the channel
// is closed after the single terminal Event, so a `for range` over it drains
// naturally when the session ends.
type Event struct {
	State      State
	Text       string
	StopReason string
	Err        error
}

// Client is one TUI-side ACP connection to a session's agent.sock. Construct it
// with Dial; drive it with Connect then Prompt; observe it via Events. It is not
// reusable across connections — make a new Client per dial.
type Client struct {
	conn net.Conn

	// writeMu serializes whole-frame writes so a request from one goroutine is
	// never interleaved mid-line with a request from another (the bridge frames
	// on '\n', so a split line would corrupt the agent's stdin).
	writeMu sync.Mutex

	// mu guards nextID, pending, sessionID, state, and closed.
	mu        sync.Mutex
	nextID    int
	pending   map[int]chan response
	sessionID string
	state     State
	closed    bool

	// events carries lifecycle/stream Events to the caller. The read loop is the
	// only sender and the only closer; closeOnce guards the close.
	events    chan Event
	closeOnce sync.Once

	// done is closed when the read loop exits (connection gone). Pending calls
	// select on it to fail fast with ErrConnectionClosed.
	done chan struct{}
}

// response is the read loop's delivery to a blocked caller: a decoded result or
// an error (a JSON-RPC error from the agent, or a decode failure).
type response struct {
	result json.RawMessage
	err    error
}

// eventBuffer bounds how many Events the read loop may get ahead of a slow
// consumer before it blocks. Streamed chunks can burst; a healthy buffer keeps
// the read loop draining the socket (and matching responses) without stalling on
// a TUI that is mid-render.
const eventBuffer = 256

// Dial connects to the agent.sock at socketPath and starts the read loop. It
// returns a Client already in StateConnecting; the caller then runs Connect to
// finish the ACP handshake. A dial failure (e.g. the socket file is stale and
// nothing is listening — a sandbox that never came up) is returned directly so
// the caller can distinguish "couldn't connect at all" from "connected then
// dropped" (the latter arrives as a StateDisconnected Event).
func Dial(ctx context.Context, socketPath string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("acpclient: dial %s: %w", socketPath, err)
	}
	return newClient(conn), nil
}

// newClient wraps an established connection and launches its read loop. Split out
// from Dial so tests can drive a Client over an in-memory pipe.
func newClient(conn net.Conn) *Client {
	c := &Client{
		conn:    conn,
		pending: make(map[int]chan response),
		state:   StateConnecting,
		events:  make(chan Event, eventBuffer),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Events returns the channel of lifecycle/stream Events. It is closed after the
// terminal (StateDisconnected/StateErrored) Event, so ranging over it ends when
// the session does. There is one events channel per Client.
func (c *Client) Events() <-chan Event { return c.events }

// State returns the Client's current lifecycle state. Events() is the primary
// interface; State is a convenience for a caller that wants to poll (e.g. to
// label a tab) rather than subscribe.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// SessionID returns the agent-assigned session id from session/new, or "" before
// Connect has completed the handshake.
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// Connect performs the ACP handshake: initialize, then session/new with the
// given cwd (which must exist inside the sandbox — pass the workspace's native
// host path, see acp-kit's "cwd gotcha"). On success the session id is stored, a
// StateConnected Event is emitted, and the session is ready for Prompt. If the
// connection drops mid-handshake — the hallmark of a socket whose sandbox is
// already gone — Connect returns ErrConnectionClosed.
func (c *Client) Connect(ctx context.Context, cwd string) error {
	initParams := initializeParams{
		ProtocolVersion:    protocolVersion,
		ClientCapabilities: map[string]any{},
	}
	var initRes initializeResult
	if err := c.call(ctx, methodInitialize, initParams, &initRes); err != nil {
		return fmt.Errorf("acpclient: initialize: %w", err)
	}

	newParams := newSessionParams{Cwd: cwd, McpServers: []any{}}
	var newRes newSessionResult
	if err := c.call(ctx, methodSessionNew, newParams, &newRes); err != nil {
		return fmt.Errorf("acpclient: session/new: %w", err)
	}
	if newRes.SessionID == "" {
		return errors.New("acpclient: session/new returned an empty sessionId")
	}

	c.mu.Lock()
	c.sessionID = newRes.SessionID
	c.mu.Unlock()

	// Flip the session into bypassPermissions so the bridge stops issuing
	// session/request_permission for escalated tool calls. This client doesn't
	// implement that method; without bypass, the first Bash/Edit/etc. would fail
	// with "method not found: session/request_permission" and the agent would
	// give up. The mode id is one of the modes session/new just advertised in
	// availableModes (see protocol.go ModeBypassPermissions).
	setModeP := setModeParams{SessionID: newRes.SessionID, ModeID: ModeBypassPermissions}
	if err := c.call(ctx, methodSessionSetMode, setModeP, nil); err != nil {
		return fmt.Errorf("acpclient: session/set_mode: %w", err)
	}

	c.setState(StateConnected)
	c.emit(Event{State: StateConnected})
	return nil
}

// Prompt sends one user turn (a single text block) to the agent and blocks until
// the turn completes, returning the stop reason (e.g. "end_turn"). While the turn
// is in flight the read loop emits StateStreaming Events carrying assistant text
// chunks; Prompt itself emits a leading StateStreaming marker (empty Text) when
// the turn starts and a StateCompleted Event when it ends. Connect must have
// succeeded first. A dropped connection mid-turn returns ErrConnectionClosed.
func (c *Client) Prompt(ctx context.Context, text string) (stopReason string, err error) {
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()
	if sessionID == "" {
		return "", errors.New("acpclient: Prompt before Connect (no session id)")
	}

	c.setState(StateStreaming)
	c.emit(Event{State: StateStreaming})

	params := promptParams{
		SessionID: sessionID,
		Prompt:    []contentBlock{textBlock(text)},
	}
	var res promptResult
	if err := c.call(ctx, methodPrompt, params, &res); err != nil {
		return "", fmt.Errorf("acpclient: session/prompt: %w", err)
	}

	c.setState(StateCompleted)
	c.emit(Event{State: StateCompleted, StopReason: res.StopReason})
	return res.StopReason, nil
}

// Close shuts the connection and ends the session. The read loop unblocks on the
// closed connection, fails any pending calls with ErrConnectionClosed, and emits
// the terminal Event. Safe to call more than once and from any goroutine.
func (c *Client) Close() error {
	return c.conn.Close()
}

// call issues a JSON-RPC request and blocks until its response, ctx is done, or
// the connection drops. It registers a one-shot result channel under a fresh id,
// writes the framed request, then waits. On success result is unmarshalled into
// out (when non-nil). A connection drop returns ErrConnectionClosed so callers
// uniformly recognise "the session died" regardless of which call was in flight.
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal %s params: %w", method, err)
	}

	ch := make(chan response, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrConnectionClosed
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	// Ensure the pending entry is reclaimed on every exit path (response,
	// timeout, cancellation) so a one-shot waiter never lingers in the map.
	defer c.clearPending(id)

	req := request{JSONRPC: jsonrpcVersion, ID: id, Method: method, Params: raw}
	if err := c.writeFrame(req); err != nil {
		return fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		if resp.err != nil {
			return resp.err
		}
		if out != nil && len(resp.result) > 0 {
			if err := json.Unmarshal(resp.result, out); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrConnectionClosed
	}
}

// writeFrame marshals v and writes it as one newline-terminated line under
// writeMu, preserving the bridge's frame boundary so concurrent writers never
// split each other's JSON.
func (c *Client) writeFrame(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	data = append(data, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.conn.Write(data)
	return err
}

// readLoop is the single reader. It frames the connection on '\n', classifies
// each line, routes responses to their pending callers, and decodes
// session/update notifications into Events. It runs until the connection reaches
// EOF or errors, then shuts the Client down — failing every pending call and
// emitting the terminal Event — so no caller is left blocked on a dead socket.
func (c *Client) readLoop() {
	br := bufio.NewReader(c.conn)
	var readErr error
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	c.shutdown(readErr)
}

// dispatch classifies one inbound line and acts on it. A malformed line is
// ignored rather than fatal: the bridge only ever carries the agent's own
// well-formed JSON-RPC, so a non-JSON line is noise (e.g. a stray log leak), and
// dropping it is safer than tearing down a live session.
func (c *Client) dispatch(line []byte) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		return
	}

	switch {
	case f.ID != nil && f.Method == "":
		// Response to one of our requests.
		c.deliver(*f.ID, f.Result, f.Error)
	case f.ID != nil && f.Method != "":
		// An agent→client request. We advertise no client capabilities, so the
		// agent shouldn't call us; reply with method-not-found so a stray call
		// never leaves the agent blocked waiting on us.
		c.rejectRequest(*f.ID, f.Method)
	case f.Method == methodUpdate:
		c.handleUpdate(f.Params)
	default:
		// Other notifications (no id, not session/update) carry no display
		// payload for this client; ignore them.
	}
}

// deliver hands a response to the waiting caller registered under id. An id with
// no waiter is ignored — it belongs to another client sharing this session's
// fan-out (or to a call that already timed out). The pending entry is removed so
// the slot can't be reused for a later, unrelated id.
func (c *Client) deliver(id int, result json.RawMessage, rpcErr *rpcError) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		return
	}

	var err error
	if rpcErr != nil {
		err = rpcErr
	}
	ch <- response{result: result, err: err}
}

// rejectRequest answers an unexpected agent→client request with a JSON-RPC
// method-not-found error so the agent doesn't block awaiting a reply. Best
// effort: a write failure here means the connection is already going away, which
// the read loop will observe on its own.
func (c *Client) rejectRequest(id int, method string) {
	resp := struct {
		JSONRPC string   `json:"jsonrpc"`
		ID      int      `json:"id"`
		Error   rpcError `json:"error"`
	}{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", method)},
	}
	_ = c.writeFrame(resp)
}

// handleUpdate decodes a session/update notification and emits the display
// Event it maps to. Assistant message chunks become StateStreaming Events
// carrying the incremental text; thought chunks are surfaced the same way (they
// are still streamed model output). Tool-call and other kinds carry no assistant
// text and are not rendered as reply text here — surfacing them richly is the
// session-tabs task's concern.
func (c *Client) handleUpdate(params json.RawMessage) {
	if ev, ok := eventFromUpdateParams(params); ok {
		c.emit(ev)
	}
}

// eventFromUpdateParams maps a session/update notification's params to the
// display Event it produces, or ok=false when the update carries no assistant
// text. It is the single decode point shared by the live read loop
// (handleUpdate) and the log-replay path (ParseStreamFrame), so a reconnecting
// TUI rebuilds scrollback through exactly the same Event shape the live stream
// would have produced.
func eventFromUpdateParams(params json.RawMessage) (Event, bool) {
	var p updateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return Event{}, false
	}
	switch p.Update.SessionUpdate {
	case updateAgentMessageChunk, updateAgentThoughtChunk:
		if p.Update.Content != nil && p.Update.Content.Text != "" {
			return Event{State: StateStreaming, Text: p.Update.Content.Text}, true
		}
	}
	return Event{}, false
}

// ParseStreamFrame decodes one raw newline-delimited ACP frame — as recorded in
// a session's stream log by the acp-wrapper bridge — into the display Event it
// maps to. A reconnecting TUI replays the log through this so the prior
// assistant output is rebuilt as StateStreaming Events, identical to what the
// live read loop emits. It returns ok=false for frames that carry no assistant
// display text (responses, agent→client requests, tool-call updates, or
// malformed lines), which the caller simply skips.
func ParseStreamFrame(line []byte) (Event, bool) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		return Event{}, false
	}
	// Only session/update *notifications* (method set, no id) carry stream text.
	if f.Method != methodUpdate || f.ID != nil {
		return Event{}, false
	}
	return eventFromUpdateParams(f.Params)
}

// shutdown is the read loop's single teardown. It marks the client closed (so no
// new call registers), fails every pending caller with ErrConnectionClosed,
// emits the terminal Event (StateErrored when readErr is set, else
// StateDisconnected), and closes the events channel. Idempotent via closeOnce on
// the channel close; the done channel close signals waiting calls.
func (c *Client) shutdown(readErr error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = make(map[int]chan response)
	if readErr != nil {
		c.state = StateErrored
	} else {
		c.state = StateDisconnected
	}
	state := c.state
	c.mu.Unlock()

	// Unblock every in-flight call. Buffered channels (cap 1) so the send never
	// blocks even though the caller may have already left via ctx/done.
	for _, ch := range pending {
		ch <- response{err: ErrConnectionClosed}
	}
	close(c.done)

	c.emit(Event{State: state, Err: readErr})
	c.closeOnce.Do(func() { close(c.events) })
}

// setState records a non-terminal state transition. Terminal states are set only
// by shutdown (under the same lock that flips closed), so setState refuses to
// overwrite a terminal state — a Prompt that loses the race with a disconnect
// can't stomp StateDisconnected back to StateStreaming.
func (c *Client) setState(s State) {
	c.mu.Lock()
	if !c.state.IsTerminal() {
		c.state = s
	}
	c.mu.Unlock()
}

// emit delivers an Event to the consumer, dropping it if the client has already
// shut down (its events channel closed) so a late send never panics. The done
// channel gates the send so emit and the channel close in shutdown can't race.
func (c *Client) emit(ev Event) {
	// The terminal Event is sent by shutdown itself before it closes the
	// channel, so allow that send through; for all other senders, a closed done
	// means the channel is closing and the Event is dropped.
	if ev.State.IsTerminal() {
		c.events <- ev
		return
	}
	select {
	case <-c.done:
		// Connection gone; no consumer will read further non-terminal Events.
	case c.events <- ev:
	}
}

// clearPending removes a one-shot waiter, used on every call() exit so a waiter
// abandoned via ctx/done is not left in the map for a late response to find.
func (c *Client) clearPending(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}
