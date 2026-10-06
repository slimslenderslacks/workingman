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
//
// A second driver (the daemon "tuning in" to a session a TUI also watches) is
// supported through DialWith + Adopt: Options.DistinctIDs moves the client's
// request ids into a private range so the TUI's responses never complete the
// daemon's calls (or vice versa), Adopt binds to the session id the first
// client created instead of running session/new, and Options.OnRequest /
// ObserveResponses expose the agent→client requests and the other clients'
// response frames that the shared fan-out also delivers here. See
// internal/acpchat for the consumer.
package acpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strings"
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

// EventKind classifies what an Event carries, orthogonally to its connection
// State. The zero value (EventStream) is an assistant message chunk — the
// original and most common Event — so every Event constructed without setting
// Kind keeps its historical meaning. The other kinds surface activity that used
// to be decoded and dropped: the agent's private reasoning, its tool
// invocations, and its task plan.
type EventKind int

const (
	// EventStream is a streamed assistant message chunk; Text holds the delta.
	EventStream EventKind = iota
	// EventThought is a streamed agent reasoning chunk; Text holds the delta.
	EventThought
	// EventToolCall is a tool invocation appearing or changing. ToolCallID
	// identifies it across updates, ToolTitle/ToolKind describe it, ToolStatus is
	// its latest status, and Text holds any output emitted so far.
	EventToolCall
	// EventPlan is the agent's current task list; Plan holds the full list, sent
	// fresh on every change.
	EventPlan
	// EventResponse is another client's response frame, fanned out by the bridge
	// to this one (only emitted with Options.ObserveResponses). Result holds the
	// raw JSON result — a session/new answer carries sessionId, a session/prompt
	// answer carries stopReason — and Err an agent error. It arrives in stream
	// order relative to the session/update chunks around it.
	EventResponse
)

// PlanEntry is one task in an EventPlan's Plan.
type PlanEntry struct {
	Content string
	Status  string // pending | in_progress | completed
}

// Event is one transition or streamed delta on a Client's connection. Kind
// classifies the payload; State carries the connection lifecycle independently
// (a tool call or plan update, for instance, arrives while State is streaming).
// The shapes:
//
//   - StateConnecting / StateConnected: lifecycle markers, no payload.
//   - StateStreaming, Kind EventStream: Text holds an incremental assistant
//     chunk (empty Text marks the start of a turn, before the first chunk).
//   - StateStreaming, Kind EventThought: Text holds an incremental reasoning
//     chunk.
//   - StateStreaming, Kind EventToolCall: ToolCallID/ToolTitle/ToolKind/
//     ToolStatus describe the tool call and Text holds any output so far.
//   - StateStreaming, Kind EventPlan: Plan holds the agent's task list.
//   - StateCompleted: StopReason holds the turn's terminal reason.
//   - StateDisconnected / StateErrored: Err holds the cause (nil for a clean
//     disconnect).
//
// Events are delivered in order on the channel returned by Events(); the channel
// is closed after the single terminal Event, so a `for range` over it drains
// naturally when the session ends.
type Event struct {
	// Kind classifies the payload; see EventKind. The zero value EventStream is
	// an assistant message chunk.
	Kind EventKind

	// SessionID is the ACP session a session/update belongs to (empty on
	// lifecycle Events). A session shared through the bridge can carry updates
	// for more than one ACP session — e.g. after a TUI restart re-ran
	// session/new — so a client that cares filters on it.
	SessionID string

	State      State
	Text       string
	StopReason string
	Err        error

	// Tool-call fields, set when Kind == EventToolCall.
	ToolCallID string
	ToolTitle  string
	ToolKind   string
	ToolStatus string

	// Plan is the agent's task list, set when Kind == EventPlan.
	Plan []PlanEntry

	// Result is the raw response result, set when Kind == EventResponse.
	Result json.RawMessage
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
	events       chan Event
	closeOnce    sync.Once
	emitMu       sync.RWMutex // see emit
	eventsClosed bool

	// done is closed when the read loop exits (connection gone). Pending calls
	// select on it to fail fast with ErrConnectionClosed.
	done chan struct{}

	// ctx is cancelled when the connection goes away; it bounds request handlers
	// (Options.OnRequest) so one blocked on a human never outlives the socket.
	ctx    context.Context
	cancel context.CancelFunc

	// onRequest and observe are the optional Options hooks; immutable after
	// newClient.
	onRequest RequestHandler
	observe   bool
}

// ErrMethodNotFound is what an Options.OnRequest handler returns for a method it
// does not serve; the client answers the agent with JSON-RPC -32601, exactly as a
// Client without a handler does.
var ErrMethodNotFound = errors.New("acpclient: method not found")

// RequestHandler answers one agent→client request (e.g. session/request_permission).
// A nil error replies to the agent with result; ErrMethodNotFound replies
// "method not found"; any other error replies with a JSON-RPC internal error. It
// runs on its own goroutine, so it may block (waiting on a human) without
// stalling the read loop; ctx is cancelled when the connection closes.
type RequestHandler func(ctx context.Context, method string, params json.RawMessage) (result any, err error)

// Options tunes a Client for use as an additional participant in a session other
// clients already drive. The zero value is the historical Dial behaviour.
type Options struct {
	// DistinctIDs starts this client's JSON-RPC request ids at a random point far
	// above where an ordinary client (ids from 1) will ever reach. The bridge
	// broadcasts every response to every client, so two clients both counting
	// from 1 would each receive — and wrongly accept — the other's response to
	// "their" id 3. A client sharing a session with a TUI should set this.
	DistinctIDs bool

	// OnRequest, when set, serves agent→client requests instead of the default
	// blanket method-not-found.
	OnRequest RequestHandler

	// ObserveResponses emits an EventResponse for every response frame that no
	// local call is waiting for — i.e. another client's reply to its own request,
	// which the bridge fans out to everyone. It is how an observer learns of a
	// session/new (result.sessionId) or turn end (result.stopReason) it did not
	// issue. Off by default: the TUI's tab model would not know what to do with
	// the extra Events.
	ObserveResponses bool
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
	return DialWith(ctx, socketPath, Options{})
}

// DialWith is Dial with Options; see Options for when each knob matters.
func DialWith(ctx context.Context, socketPath string, opts Options) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("acpclient: dial %s: %w", socketPath, err)
	}
	return newClient(conn, opts), nil
}

// newClient wraps an established connection and launches its read loop. Split out
// from Dial so tests can drive a Client over an in-memory pipe.
func newClient(conn net.Conn, opts Options) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		conn:      conn,
		pending:   make(map[int]chan response),
		state:     StateConnecting,
		events:    make(chan Event, eventBuffer),
		done:      make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		onRequest: opts.OnRequest,
		observe:   opts.ObserveResponses,
	}
	if opts.DistinctIDs {
		// 2^20 blocks of 2^20 ids: two such clients collide with probability
		// ~1e-6 and only if one issues over a million requests.
		c.nextID = int(1+rand.Int64N(1<<20)) << 20
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

// Adopt binds the client to an ACP session another client already created —
// the "tune in" half of sharing a session through the bridge — without any wire
// traffic: no initialize, no session/new, no session/set_mode. The agent process
// behind agent.sock is already initialized and holds the session (and its
// permission mode) in memory; Prompt then addresses it by id.
//
// session/load is deliberately NOT used. ACP requires the agent to replay the
// whole prior conversation as session/update notifications before answering a
// load, and the bridge fans those out to every connected client — the TUI would
// render the history a second time. (hermes-agent's server.py behaves this way;
// its session/prompt on an id it does not know answers stopReason "refusal".)
//
// A StateConnected Event is emitted on the first adoption, mirroring Connect;
// calling Adopt again just re-points the client at another session id.
func (c *Client) Adopt(sessionID string) error {
	if sessionID == "" {
		return errors.New("acpclient: Adopt requires a session id")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrConnectionClosed
	}
	rebind := c.sessionID != ""
	c.sessionID = sessionID
	c.mu.Unlock()
	if rebind {
		// Re-pointing an adopted client at a newer session (see acpchat) must not
		// emit: it is called from the goroutine that consumes Events, which would
		// deadlock against a full buffer.
		return nil
	}
	c.setState(StateConnected)
	c.emit(Event{State: StateConnected})
	return nil
}

// Cancel asks the agent to stop the session's in-flight turn (the ACP
// session/cancel notification — no response). The in-flight Prompt then returns
// with stopReason "cancelled". Use it when the caller abandons a turn (its
// context was cancelled) so the agent doesn't keep working, and so the next
// prompt isn't queued behind it.
func (c *Client) Cancel() error {
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()
	if sessionID == "" {
		return errors.New("acpclient: Cancel before Connect/Adopt (no session id)")
	}
	raw, err := json.Marshal(cancelParams{SessionID: sessionID})
	if err != nil {
		return err
	}
	return c.writeFrame(notification{JSONRPC: jsonrpcVersion, Method: methodCancel, Params: raw})
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

	finish := func(resp response) error {
		if resp.err != nil {
			return resp.err
		}
		if out != nil && len(resp.result) > 0 {
			if err := json.Unmarshal(resp.result, out); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	}
	select {
	case resp := <-ch:
		return finish(resp)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		// A response delivered just before the connection closed (the agent
		// answered and hung up) is ready too, and select picks at random: prefer
		// the real answer over the generic "connection closed".
		select {
		case resp := <-ch:
			return finish(resp)
		default:
			return ErrConnectionClosed
		}
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
		// An agent→client request. By default we advertise no client
		// capabilities, so the agent shouldn't call us; reply with
		// method-not-found so a stray call never leaves the agent blocked
		// waiting on us. A client built with Options.OnRequest serves it instead.
		if c.onRequest != nil {
			go c.serveRequest(*f.ID, f.Method, f.Params)
		} else {
			c.rejectRequest(*f.ID, f.Method)
		}
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
	var err error
	if rpcErr != nil {
		err = rpcErr
	}
	if !ok {
		if c.observe {
			c.emit(Event{Kind: EventResponse, State: c.State(), Result: result, Err: err})
		}
		return
	}
	ch <- response{result: result, err: err}
}

// serveRequest runs Options.OnRequest for one agent→client request and writes the
// reply. It owns the whole exchange on its own goroutine so a handler blocked on a
// human never stalls the read loop.
func (c *Client) serveRequest(id int, method string, params json.RawMessage) {
	result, err := c.onRequest(c.ctx, method, params)
	switch {
	case errors.Is(err, ErrMethodNotFound):
		c.rejectRequest(id, method)
	case err != nil:
		_ = c.writeFrame(errorResponse{JSONRPC: jsonrpcVersion, ID: id, Error: rpcError{Code: -32603, Message: err.Error()}})
	default:
		_ = c.writeFrame(resultResponse{JSONRPC: jsonrpcVersion, ID: id, Result: result})
	}
}

// rejectRequest answers an unexpected agent→client request with a JSON-RPC
// method-not-found error so the agent doesn't block awaiting a reply. Best
// effort: a write failure here means the connection is already going away, which
// the read loop will observe on its own.
func (c *Client) rejectRequest(id int, method string) {
	_ = c.writeFrame(errorResponse{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", method)},
	})
}

// handleUpdate decodes a session/update notification and emits the display
// Event it maps to. Assistant message chunks, agent thought chunks, tool calls,
// and plan updates each become an Event (distinguished by Event.Kind); kinds we
// don't render decode to ok=false and are dropped.
func (c *Client) handleUpdate(params json.RawMessage) {
	if ev, ok := eventFromUpdateParams(params); ok {
		c.emit(ev)
	}
}

// eventFromUpdateParams maps a session/update notification's params to the
// display Event it produces, or ok=false when the update is a kind we don't
// render. It is the single decode point shared by the live read loop
// (handleUpdate) and the log-replay path (ParseStreamFrame), so a reconnecting
// TUI rebuilds scrollback through exactly the same Event shapes the live stream
// would have produced.
func eventFromUpdateParams(params json.RawMessage) (Event, bool) {
	var p updateParams
	if err := json.Unmarshal(params, &p); err != nil || len(p.Update) == 0 {
		return Event{}, false
	}
	var k updateKind
	if err := json.Unmarshal(p.Update, &k); err != nil {
		return Event{}, false
	}

	switch k.SessionUpdate {
	case updateAgentMessageChunk, updateAgentThoughtChunk:
		var m messageUpdate
		if err := json.Unmarshal(p.Update, &m); err != nil {
			return Event{}, false
		}
		if m.Content == nil || m.Content.Text == "" {
			return Event{}, false
		}
		kind := EventStream
		if k.SessionUpdate == updateAgentThoughtChunk {
			kind = EventThought
		}
		return Event{Kind: kind, SessionID: p.SessionID, State: StateStreaming, Text: m.Content.Text}, true

	case updateToolCall, updateToolCallUpdate:
		var tc toolCallUpdate
		if err := json.Unmarshal(p.Update, &tc); err != nil {
			return Event{}, false
		}
		return Event{
			Kind:       EventToolCall,
			SessionID:  p.SessionID,
			State:      StateStreaming,
			ToolCallID: tc.ToolCallID,
			ToolTitle:  tc.Title,
			ToolKind:   tc.Kind,
			ToolStatus: tc.Status,
			Text:       flattenToolOutput(tc.Content),
		}, true

	case updatePlan:
		var pl planUpdate
		if err := json.Unmarshal(p.Update, &pl); err != nil {
			return Event{}, false
		}
		entries := make([]PlanEntry, 0, len(pl.Entries))
		for _, e := range pl.Entries {
			entries = append(entries, PlanEntry{Content: e.Content, Status: e.Status})
		}
		return Event{Kind: EventPlan, SessionID: p.SessionID, State: StateStreaming, Plan: entries}, true
	}

	return Event{}, false
}

// flattenToolOutput concatenates the text carried by a tool call's content
// blocks. The common block is {"type":"content","content":{...text...}}; a bare
// inline "text" is also read. Non-text variants (diffs, terminal handles)
// contribute nothing.
func flattenToolOutput(blocks []toolCallContent) string {
	var b strings.Builder
	for _, c := range blocks {
		switch {
		case c.Content != nil && c.Content.Text != "":
			b.WriteString(c.Content.Text)
		case c.Text != "":
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// ParseStreamFrame decodes one raw newline-delimited ACP frame — as recorded in
// a session's stream log by the acp-wrapper bridge — into the display Event it
// maps to. A reconnecting TUI replays the log through this so the prior assistant
// output, thoughts, tool calls, and plan are rebuilt as Events identical to what
// the live read loop emits. It returns ok=false for frames that carry no display
// payload (responses, agent→client requests, unrendered notification kinds, or
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
	c.cancel()

	c.emit(Event{State: state, Err: readErr})
	c.closeOnce.Do(func() {
		c.emitMu.Lock()
		c.eventsClosed = true
		close(c.events)
		c.emitMu.Unlock()
	})
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
	// emitMu makes "channel not yet closed" and the send one step: shutdown takes
	// the write lock to close the channel, so a send can never land on a closed
	// channel (a select that picks the send case there panics even when done is
	// also ready). done is closed before that, so a blocked sender here always
	// wakes and releases the lock.
	c.emitMu.RLock()
	defer c.emitMu.RUnlock()
	if c.eventsClosed {
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
