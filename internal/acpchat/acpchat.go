// Package acpchat is the reusable "tune in" primitive: it lets the daemon join
// the ACP conversation of an agent that is ALREADY RUNNING — the wolf, or the
// workingman agent — send it text as a prompt, collect the reply, and watch what
// the agent says to other clients, all while a TUI stays attached to the same
// session.
//
// # How a session is shared
//
// Every session directory (<SessionsRoot>/<id>/) holds session.json and the
// agent.sock the acp-wrapper bridge serves. The bridge multiplexes one sandboxed
// ACP agent over any number of unix-socket clients: every agent frame is copied
// to every client, and every client's frames are written to the agent's stdin.
// The TUI's watcher is normally the first client — it runs initialize +
// session/new + session/set_mode and sends the opening prompt — so the
// conversation already exists, as an ACP sessionId, before we arrive.
//
// # Joining the existing conversation (the spike's conclusion)
//
// Attach does NOT run session/new (that would start a second, empty
// conversation next to the one the human is in) and does NOT use session/load
// (ACP obliges the agent to replay the whole history as session/update
// notifications, which the bridge would fan out to the TUI too, duplicating its
// scrollback). Instead it adopts the existing sessionId — acpclient.Client.Adopt
// — and sends session/prompt straight to it. The id is recovered from the
// session's stream.log (the bridge records every agent frame there, and every
// session/update and session/new response carries the id; the latest wins), or,
// for a session nobody has talked to yet, from the first frame seen on the live
// socket. If the TUI later re-runs session/new (it does after its own restart)
// the response is fanned out to us as well, and the conversation follows it.
// This has been exercised against an in-process fake agent speaking the ACP
// frames the real ones do (see the tests); the wire behaviour of the real
// claude-acp-client under adoption — a prompt to a session this connection never
// created — is the one thing a live smoke test should confirm.
//
// # Sharing hazards handled here
//
//   - Response fan-out: the bridge broadcasts every response to every client, so
//     two clients counting request ids from 1 would swallow each other's
//     answers. Conversations dial with acpclient.Options.DistinctIDs.
//   - Agent→client requests are broadcast too. Permission requests are surfaced
//     (Options.OnPermission, EventPermission) and answered; the default policy
//     rejects. The first reply wins at the agent, so a TUI that also answers can
//     race this client — in practice orch sessions run in bypassPermissions and
//     never ask.
//   - Slow clients are dropped by the bridge. The conversation notices the socket
//     closing and reconnects (same session id) for as long as the session's
//     directory says it is alive.
package acpchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/acpclient"
	"github.com/slimslenderslacks/work/internal/session"
)

var (
	// ErrBusy is returned by Ask while another Ask on the same Conversation is in
	// flight. A conversation has one turn at a time; callers that want queueing
	// (the channel router) serialize above this.
	ErrBusy = errors.New("acpchat: a turn is already in flight")

	// ErrSessionGone means the session is over: its directory is gone or its
	// status is exited/failed, or its socket stayed unreachable for the whole
	// ConnectTimeout. Terminal — Attach again to join a restarted session.
	ErrSessionGone = errors.New("acpchat: session is gone")

	// ErrNoSession means the session is up but no ACP session exists to join
	// (nobody has run session/new within DiscoverTimeout) and CreateIfMissing is
	// off.
	ErrNoSession = errors.New("acpchat: no ACP session to attach to")

	// ErrClosed is returned after Close.
	ErrClosed = errors.New("acpchat: conversation closed")

	// ErrRefused is returned when the agent ended a turn with stopReason
	// "refusal" and produced no text — hermes answers that way for a session id it
	// does not know, which is how a stale adoption shows up.
	ErrRefused = errors.New("acpchat: agent refused the prompt")
)

// Options configure Attach. The zero value is usable.
type Options struct {
	// Root is the sessions root holding the session directory; "" means
	// session.DefaultRoot. Ignored when Attach is given a directory path.
	Root string

	// OnPermission decides a tool-permission request the agent raises. It may
	// block (e.g. while the router asks the human on WhatsApp) up to
	// PermissionTimeout, then the request is rejected. nil rejects immediately.
	OnPermission func(ctx context.Context, req PermissionRequest) PermissionDecision

	// PermissionTimeout bounds OnPermission; 0 means two minutes.
	PermissionTimeout time.Duration

	// DiscoverTimeout is how long Attach waits on a live session for some client
	// to create an ACP session when stream.log has none. 0 means five seconds.
	DiscoverTimeout time.Duration

	// CreateIfMissing makes Attach create the ACP session itself (full
	// initialize/session/new handshake, cwd = the session's first workspace) when
	// none appears within DiscoverTimeout — for a headless daemon with no TUI.
	// Off by default so we never race a TUI's own session/new.
	CreateIfMissing bool

	// ConnectTimeout bounds how long (re)connecting keeps retrying a session that
	// is still starting or whose socket refuses; 0 means 30s.
	ConnectTimeout time.Duration

	// RetryInterval is the pause between connect attempts; 0 means 250ms.
	RetryInterval time.Duration
}

func (o Options) permissionTimeout() time.Duration {
	return orDefault(o.PermissionTimeout, 2*time.Minute)
}
func (o Options) discoverTimeout() time.Duration { return orDefault(o.DiscoverTimeout, 5*time.Second) }
func (o Options) connectTimeout() time.Duration  { return orDefault(o.ConnectTimeout, 30*time.Second) }
func (o Options) retryInterval() time.Duration {
	return orDefault(o.RetryInterval, 250*time.Millisecond)
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// ProgressFunc observes a turn as it streams: delta is the newest assistant text
// chunk, reply the cleaned text accumulated so far. It runs on the conversation's
// reader goroutine — keep it quick and never call back into the Conversation.
type ProgressFunc func(delta, reply string)

// AskOption tunes one Ask.
type AskOption func(*askConfig)

type askConfig struct {
	progress  ProgressFunc
	finalOnly bool
}

// OnProgress reports streamed text while a long turn runs.
func OnProgress(fn ProgressFunc) AskOption { return func(c *askConfig) { c.progress = fn } }

// FinalSegmentOnly returns just the assistant text after the turn's last tool
// call, dropping the "let me check…" narration that precedes tool use.
func FinalSegmentOnly() AskOption { return func(c *askConfig) { c.finalOnly = true } }

// Conversation is a live attachment to one session's ACP conversation. It is safe
// for concurrent use; Ask calls are serialized (ErrBusy), Events is a single
// stream.
type Conversation struct {
	opts  Options
	store session.Store
	id    string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	changed    chan struct{} // closed + replaced on every state change; see wait
	rec        session.Session
	client     *acpclient.Client // nil while reconnecting
	acpSession string
	turn       *turn
	foreign    turnText // assistant text of turns other clients are running
	// foreignActive is true from the first chunk of a turn another client started
	// until the response to its prompt (fanned out by the bridge) arrives. While
	// it holds, chunks are attributed to that turn even if an Ask is in flight —
	// the agent runs one turn per session at a time, so our own turn's text only
	// starts once the foreign one has ended. Reset on disconnect, since a missed
	// response would otherwise leave it stuck.
	foreignActive bool
	gone          error // set once, terminal
	created       bool  // this conversation ran session/new itself (CreateIfMissing)

	events eventQueue
}

// ID is the session directory id this conversation is attached to.
func (c *Conversation) ID() string { return c.id }

// Created reports whether this conversation had to create the ACP session
// itself (CreateIfMissing) because no other client had. Such a session has not
// been given its opening prompt by anyone, so the caller should.
func (c *Conversation) Created() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.created
}

// SessionID is the ACP session id currently being spoken to ("" mid-reconnect to
// a restarted session).
func (c *Conversation) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acpSession
}

// Attach joins the live session identified by ref: a session id resolved under
// opts.Root, or — when ref contains a path separator — the session directory
// itself. See the package comment for how the existing conversation is bound.
func Attach(ctx context.Context, ref string, opts Options) (*Conversation, error) {
	var store session.Store
	var id string
	if strings.ContainsAny(ref, `/\`) {
		dir := filepath.Clean(ref)
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("acpchat: %w", err)
		}
		store = session.Store{Root: filepath.Dir(abs)}
		id = filepath.Base(abs)
	} else {
		var err error
		if store, err = session.NewStore(opts.Root); err != nil {
			return nil, err
		}
		id = ref
	}

	cctx, cancel := context.WithCancel(context.Background())
	c := &Conversation{
		opts: opts, store: store, id: id,
		ctx: cctx, cancel: cancel,
		changed: make(chan struct{}),
	}
	c.events.init(cctx)

	client, err := c.establish(ctx, session.Session{}, true)
	if err != nil {
		cancel()
		return nil, err
	}
	c.wg.Add(1)
	go c.run(client)
	return c, nil
}

// Close detaches: the socket is closed (the session itself keeps running — the
// bridge only ends a persistent session on its own terms), a blocked Ask returns
// ErrClosed, and the Events channel closes.
func (c *Conversation) Close() error {
	c.mu.Lock()
	client := c.client
	if c.gone == nil {
		c.gone = ErrClosed
	}
	c.bump()
	c.mu.Unlock()
	c.cancel()
	if client != nil {
		client.Close()
	}
	c.wg.Wait()
	c.events.close()
	return nil
}

// Ask sends text to the agent as a prompt and blocks until the turn ends,
// returning the assistant's reply with reasoning and tool-call activity stripped
// (assistant text separated by tool calls is joined with a blank line). ctx
// bounds the turn: when it expires the agent is asked to cancel (session/cancel)
// and ctx.Err() is returned with whatever text had streamed. Errors: ErrBusy,
// ErrSessionGone, ErrClosed, ErrRefused, or — when the socket drops mid-turn and
// the session is still alive — a wrapped acpclient.ErrConnectionClosed (the turn
// may or may not have completed agent-side; Ask never retries a prompt).
func (c *Conversation) Ask(ctx context.Context, text string, options ...AskOption) (string, error) {
	var cfg askConfig
	for _, o := range options {
		o(&cfg)
	}
	t := &turn{progress: cfg.progress, done: make(chan struct{})}

	c.mu.Lock()
	if c.gone != nil {
		err := c.gone
		c.mu.Unlock()
		return "", err
	}
	if c.turn != nil {
		c.mu.Unlock()
		return "", ErrBusy
	}
	c.turn = t
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.turn == t {
			c.turn = nil
		}
		c.mu.Unlock()
	}()

	client, err := c.waitClient(ctx)
	if err != nil {
		return "", err
	}

	stop, err := client.Prompt(ctx, text)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			_ = client.Cancel()
			return t.reply(cfg.finalOnly), ctx.Err()
		case errors.Is(err, acpclient.ErrConnectionClosed):
			// Let the reader drain what the dying socket delivered, then report.
			select {
			case <-t.done:
			case <-time.After(time.Second):
			}
			if g := c.goneErr(); g != nil {
				return t.reply(cfg.finalOnly), g
			}
			return t.reply(cfg.finalOnly), fmt.Errorf("acpchat: connection lost mid-turn: %w", err)
		}
		return t.reply(cfg.finalOnly), err
	}

	// Prompt returned on the response frame; the chunks before it may still be in
	// the reader's queue. The reader closes t.done at this turn's StateCompleted
	// event, which is ordered after every chunk.
	select {
	case <-t.done:
	case <-ctx.Done():
		return t.reply(cfg.finalOnly), ctx.Err()
	case <-c.ctx.Done():
		return t.reply(cfg.finalOnly), c.goneErr()
	}

	reply := t.reply(cfg.finalOnly)
	c.events.push(Event{Kind: EventTurnEnd, Own: true, Text: t.reply(false), StopReason: stop})
	if stop == "refusal" && reply == "" {
		return "", ErrRefused
	}
	return reply, nil
}

// waitClient blocks until a connected client is available, the conversation is
// over, or ctx is done (Ask during a reconnect waits it out).
func (c *Conversation) waitClient(ctx context.Context) (*acpclient.Client, error) {
	for {
		c.mu.Lock()
		if c.gone != nil {
			err := c.gone
			c.mu.Unlock()
			return nil, err
		}
		if c.client != nil && c.acpSession != "" {
			cl := c.client
			c.mu.Unlock()
			return cl, nil
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// bump wakes every waitClient. Caller holds c.mu.
func (c *Conversation) bump() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Conversation) goneErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gone
}

// markGone makes the conversation terminal (once) and tells Events consumers.
func (c *Conversation) markGone(err error) {
	c.mu.Lock()
	if c.gone == nil {
		c.gone = err
	}
	err = c.gone
	c.client = nil
	c.bump()
	c.mu.Unlock()
	c.events.push(Event{Kind: EventGone, Err: err})
	c.events.close()
}

// run is the reader: it consumes the live client's events until the socket drops,
// then reconnects, until the session is gone or the conversation closed.
func (c *Conversation) run(client *acpclient.Client) {
	defer c.wg.Done()
	for {
		c.consume(client)
		if c.ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		c.client = nil
		c.foreign = turnText{}
		c.foreignActive = false
		c.bump()
		rec := c.rec
		c.mu.Unlock()
		c.events.push(Event{Kind: EventReconnecting})

		next, err := c.establish(c.ctx, rec, false)
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			c.markGone(err)
			return
		}
		client = next
	}
}

// consume drains one client's events, folding them into turn/foreign state and
// the Events stream, until its channel closes (socket dropped or Close).
func (c *Conversation) consume(client *acpclient.Client) {
	defer c.finishTurn()
	for ev := range client.Events() {
		c.handle(client, ev)
	}
}

// finishTurn releases an Ask waiting on a turn the dying socket will never
// complete.
func (c *Conversation) finishTurn() {
	c.mu.Lock()
	t := c.turn
	c.mu.Unlock()
	if t != nil {
		t.finish()
	}
}

func (c *Conversation) handle(client *acpclient.Client, ev acpclient.Event) {
	switch ev.Kind {
	case acpclient.EventResponse:
		// (Checked before State: a response event carries whatever state the
		// client was in, possibly StateCompleted from our own last turn.)
		c.handleForeignResponse(client, ev)
		return
	case acpclient.EventToolCall:
		c.mu.Lock()
		t := c.turn
		if t == nil || c.foreignActive {
			t = nil
			c.foreign.toolBoundary()
		}
		c.mu.Unlock()
		if t != nil {
			t.toolBoundary()
		}
		return
	case acpclient.EventStream:
		// below
	default:
		return // thoughts, plans
	}
	// Prompt's own StateCompleted marker (Kind is the zero EventStream) ends the
	// turn. It is queued behind every chunk of that turn, which is what lets Ask
	// return the complete text.
	if ev.State == acpclient.StateCompleted {
		c.finishTurn()
		return
	}
	if ev.Text == "" || ev.State != acpclient.StateStreaming {
		return
	}

	c.mu.Lock()
	if ev.SessionID != "" && c.acpSession != "" && ev.SessionID != c.acpSession {
		c.mu.Unlock()
		return // another ACP session on the same agent; not our conversation
	}
	t := c.turn
	if t != nil && c.foreignActive {
		t = nil // the tail of a turn another client started before our Ask
	}
	if t == nil {
		c.foreign.add(ev.Text)
		c.foreignActive = true
	}
	c.mu.Unlock()

	if t != nil {
		reply := t.add(ev.Text)
		c.events.push(Event{Kind: EventText, Own: true, Text: ev.Text})
		if t.progress != nil {
			t.progress(ev.Text, reply)
		}
		return
	}
	c.events.push(Event{Kind: EventText, Text: ev.Text})
}

// handleForeignResponse reacts to another client's answer, fanned out to us: a
// session/new result re-points the conversation at the new ACP session; a
// session/prompt result ends a turn we were only watching.
func (c *Conversation) handleForeignResponse(client *acpclient.Client, ev acpclient.Event) {
	var r struct {
		SessionID  string `json:"sessionId"`
		StopReason string `json:"stopReason"`
	}
	if ev.Err == nil && json.Unmarshal(ev.Result, &r) != nil {
		return
	}
	switch {
	case r.SessionID != "":
		c.mu.Lock()
		changed := r.SessionID != c.acpSession
		if changed {
			c.acpSession = r.SessionID
			c.foreign = turnText{}
			c.foreignActive = false
			c.bump()
		}
		c.mu.Unlock()
		if changed {
			_ = client.Adopt(r.SessionID)
			c.events.push(Event{Kind: EventSessionChanged, SessionID: r.SessionID})
		}
	case r.StopReason != "" || ev.Err != nil:
		// The answer to a prompt another client sent. (The answer to ours goes to
		// the waiting call, never here.) An agent error ends the turn too; the one
		// ambiguity — an error answering some other request — can only end
		// tracking early.
		c.mu.Lock()
		if !c.foreignActive {
			c.mu.Unlock()
			return
		}
		text := c.foreign.join(false)
		c.foreign = turnText{}
		c.foreignActive = false
		c.mu.Unlock()
		stop := r.StopReason
		if stop == "" {
			stop = "error"
		}
		c.events.push(Event{Kind: EventTurnEnd, Own: false, Text: text, StopReason: stop})
	}
}

// --- permission ---

// PermissionOption is one choice the agent offers for a permission request.
type PermissionOption struct {
	ID   string
	Name string
	Kind string // allow_once | allow_always | reject_once | reject_always
}

// PermissionRequest is the agent asking to run a tool call.
type PermissionRequest struct {
	SessionID  string
	ToolCallID string
	Title      string // e.g. "Run `git push`"
	ToolKind   string
	Options    []PermissionOption
}

// PermissionDecision is the answer to a request: the id of one of its Options.
// The zero value (and any id the agent did not offer) rejects.
type PermissionDecision struct{ OptionID string }

// Allow selects the request's allow-once option (else allow-always).
func (r PermissionRequest) Allow() PermissionDecision { return r.pick("allow_once", "allow_always") }

// Reject selects the request's reject-once option (else reject-always); with
// neither on offer the reply is "cancelled", which agents treat as a rejection.
func (r PermissionRequest) Reject() PermissionDecision { return r.pick("reject_once", "reject_always") }

func (r PermissionRequest) pick(kinds ...string) PermissionDecision {
	for _, k := range kinds {
		for _, o := range r.Options {
			if o.Kind == k {
				return PermissionDecision{OptionID: o.ID}
			}
		}
	}
	return PermissionDecision{}
}

func (r PermissionRequest) has(id string) bool {
	for _, o := range r.Options {
		if o.ID == id {
			return true
		}
	}
	return false
}

// handleRequest serves agent→client requests: permissions go to OnPermission
// (default reject); everything else is method-not-found, as before.
func (c *Conversation) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method != acpclient.MethodRequestPermission {
		return nil, acpclient.ErrMethodNotFound
	}
	var p struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ID    string `json:"toolCallId"`
			Title string `json:"title"`
			Kind  string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			ID   string `json:"optionId"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("decode permission request: %w", err)
	}
	req := PermissionRequest{SessionID: p.SessionID, ToolCallID: p.ToolCall.ID, Title: p.ToolCall.Title, ToolKind: p.ToolCall.Kind}
	for _, o := range p.Options {
		req.Options = append(req.Options, PermissionOption{ID: o.ID, Name: o.Name, Kind: o.Kind})
	}
	c.events.push(Event{Kind: EventPermission, Permission: &req})

	decision := req.Reject()
	if c.opts.OnPermission != nil {
		pctx, cancel := context.WithTimeout(ctx, c.opts.permissionTimeout())
		defer cancel()
		ch := make(chan PermissionDecision, 1)
		go func() { ch <- c.opts.OnPermission(pctx, req) }()
		select {
		case d := <-ch:
			decision = d
		case <-pctx.Done(): // timed out or disconnected: stay rejected
		}
	}
	if decision.OptionID != "" && !req.has(decision.OptionID) {
		decision = req.Reject() // an id the agent never offered
	}
	if decision.OptionID == "" {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": decision.OptionID}}, nil
}

// --- connecting ---

// establish dials the session and binds a client to its ACP conversation,
// retrying while the session is still starting or its socket refuses, until
// ConnectTimeout. prev is the record the previous connection was made to (zero on
// the first attach): an unchanged CreatedAt means the same agent process, so the
// known ACP session id is reused; a changed one means a restart, which is
// re-discovered. Returns ErrSessionGone (wrapped) when the session is over.
func (c *Conversation) establish(ctx context.Context, prev session.Session, initial bool) (*acpclient.Client, error) {
	deadline := time.Now().Add(c.opts.connectTimeout())
	var lastErr error
	for {
		rec, err := c.store.Read(c.id)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("%w: %s: no such session", ErrSessionGone, c.id)
		case err != nil:
			lastErr = err
		case rec.Status == session.StatusExited || rec.Status == session.StatusFailed:
			return nil, fmt.Errorf("%w: %s is %s", ErrSessionGone, c.id, rec.Status)
		case rec.Status == session.StatusRunning:
			restarted := !initial && !rec.CreatedAt.Equal(prev.CreatedAt)
			client, err := c.dialAndBind(ctx, rec, initial || restarted)
			if err == nil {
				c.mu.Lock()
				c.rec = rec
				c.client = client
				c.bump()
				c.mu.Unlock()
				if !initial {
					c.events.push(Event{Kind: EventReconnected, SessionChanged: restarted, SessionID: client.SessionID()})
				}
				return client, nil
			}
			if errors.Is(err, ErrNoSession) {
				return nil, err
			}
			lastErr = err
		default: // starting: the wrapper hasn't confirmed the agent is up
			lastErr = fmt.Errorf("session %s is %s", c.id, rec.Status)
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s unreachable after %s: %v", ErrSessionGone, c.id, c.opts.connectTimeout(), lastErr)
		}
		select {
		case <-time.After(c.opts.retryInterval()):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// dialAndBind opens a client (distinct request ids; permission + foreign-response
// hooks) and binds it to an ACP session id. rediscover forces re-reading the id
// from the stream log rather than reusing the one already known.
func (c *Conversation) dialAndBind(ctx context.Context, rec session.Session, rediscover bool) (*acpclient.Client, error) {
	socket := rec.SocketPath
	if socket == "" {
		socket = c.store.SocketPath(c.id)
	}
	// Dial FIRST, then read the log: a session/new another client runs between the
	// two arrives live (as an EventResponse) rather than falling in the gap.
	client, err := acpclient.DialWith(ctx, socket, acpclient.Options{
		DistinctIDs:      true,
		OnRequest:        c.handleRequest,
		ObserveResponses: true,
	})
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	id := c.acpSession
	c.mu.Unlock()
	if rediscover {
		id = ""
	}
	if id == "" {
		id = lastSessionIDInLog(c.logPath(rec))
	}
	if id == "" {
		id, err = waitForSessionID(ctx, client, c.opts.discoverTimeout())
		if err != nil {
			client.Close()
			return nil, err
		}
	}
	if id == "" && c.opts.CreateIfMissing {
		cwd := ""
		if len(rec.Workspaces) > 0 {
			cwd = rec.Workspaces[0]
		}
		cctx, cancel := context.WithTimeout(ctx, c.opts.connectTimeout())
		err := client.Connect(cctx, cwd)
		cancel()
		if err != nil {
			client.Close()
			return nil, fmt.Errorf("acpchat: create session: %w", err)
		}
		id = client.SessionID()
		c.mu.Lock()
		c.created = true
		c.mu.Unlock()
	} else if id == "" {
		client.Close()
		return nil, fmt.Errorf("%w in %s", ErrNoSession, c.id)
	} else if err := client.Adopt(id); err != nil {
		client.Close()
		return nil, err
	}

	c.mu.Lock()
	c.acpSession = id
	c.mu.Unlock()
	return client, nil
}

func (c *Conversation) logPath(rec session.Session) string {
	if rec.LogPath != "" {
		return rec.LogPath
	}
	return filepath.Join(c.store.Dir(c.id), "stream.log") // acpwrapper.LogName
}
