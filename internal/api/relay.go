package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// The session relay (§A.5.2): GET /sessions/{sid}/acp upgrades to a
// WebSocket that streams one ACP session to a browser. The server holds at
// most one relay per session, and a relay holds at most one upstream
// connection to the session's agent.sock, however many browsers watch.
//
// Frames come from the session's stream.log, not from the socket: the
// wrapper's hub writes every agent frame to the log before it broadcasts it,
// so the log is the full record, its byte offsets are stable positions for
// replay=after:<offset>, and replay and live frames come through one reader
// with no gap or overlap between them. The upstream connection carries the
// relay's prompts and cancels and wakes the reader when a frame arrives;
// without it the reader polls.
//
// The relay keeps a ring of the frames it has read. Every browser follows
// the ring at its own pace, and one that falls more than the ring behind is
// dropped by the relay (the hub drops slow clients too, which would drop the
// relay and every browser behind it).
//
// Lifetime parity with the TUI: a one-shot session ends when its last client
// leaves, so the relay connects to one only while browsers watch, only once
// the session has been prompted (prompt_count > 0, see §A.5.4), and only
// until its turn ends (a response carrying a stopReason). A persistent
// session ignores its client count, so the relay stays connected while
// anyone watches. The relay never sends the opening prompt.

// SubprotocolACP is the session relay's WebSocket subprotocol. A client
// without a bearer header may also offer "bearer.<token>" beside it.
const SubprotocolACP = "workingman.acp.v1"

// bearerSubprotocol prefixes the API token offered as a WebSocket
// subprotocol (§A.6.3), for clients that cannot set Authorization.
const bearerSubprotocol = "bearer."

// Relay modes (?mode=).
const (
	// ModeObserve streams the session to the browser and accepts nothing
	// from it but pings. It is the default and works for every kind.
	ModeObserve = "observe"
	// ModeInteractive also accepts prompt and cancel, for the sessions whose
	// [RelaySession.Interactive] is set.
	ModeInteractive = "interactive"
)

// Turn states in a turn message.
const (
	TurnRunning = "running"
	TurnIdle    = "idle"
)

// Reasons in an ended message.
const (
	// EndedDaemonStopping: the daemon is shutting down; the session itself
	// carries on and a client may re-attach to the next daemon process.
	EndedDaemonStopping = "daemon_stopping"
)

// Relay timing and sizes.
const (
	// RelayRingSize is how many frames a relay keeps for its browsers. A
	// browser further behind than that is dropped.
	RelayRingSize = 4096
	// relayPoll is how often a relay re-reads the stream log and
	// session.json when nothing woke it.
	relayPoll = 250 * time.Millisecond
	// relayRetry is the pause between upstream connect attempts.
	relayRetry = 2 * time.Second
	// relayWriteTimeout bounds one write to a browser; a browser that
	// cannot take a message in that time is dropped.
	relayWriteTimeout = 10 * time.Second
	// relayReadBatch is how many frames one read of the log takes.
	relayReadBatch = 512
)

// Relays is what the session relay needs of the daemon; a [Source] that
// implements it serves GET /sessions/{sid}/acp (else the endpoint is a 503).
type Relays interface {
	// RelaySession resolves a session id (or session-map key) to the session
	// to relay. [ErrNotFound] is a 404, [ErrInvalidState] a 409 (for example
	// a session in a terminal tab, which has no ACP stream).
	RelaySession(ctx context.Context, id string) (RelaySession, error)
}

// RelaySession is one ACP session as the relay sees it.
type RelaySession struct {
	// ID is the session directory's id. The server keeps one relay per ID.
	ID string
	// Kind is the agent kind ("task", "wolf", ...).
	Kind string
	// Persistent is true for a conversational session, whose lifetime does
	// not depend on its client count.
	Persistent bool
	// Interactive is true when a browser may prompt the session (mode
	// interactive): the persistent kinds a human types into.
	Interactive bool
	// State reads the session's record now.
	State func() (RelayState, error)
	// OpenLog opens the session's stream log at byte offset after.
	OpenLog func(after int64) (LogReader, error)
	// Attach joins the live session upstream. wake is called (from the
	// connection's reader, so it must not block) whenever a frame arrives.
	Attach func(ctx context.Context, wake func()) (Upstream, error)
}

// RelayState is a session's record as the relay needs it.
type RelayState struct {
	PromptCount int
	// Ended is "" while the session is live, else why it is over ("exited",
	// "failed", "gone").
	Ended string
}

// LogFrame is one line of a stream log: the frame, already redacted, and the
// byte offsets where its line starts and where the next one starts.
type LogFrame struct {
	Start, End int64
	Data       []byte
}

// LogReader reads a stream log forward.
type LogReader interface {
	// Next returns up to limit whole frames appended since the last call
	// (none at the end of the log). A last line without its newline waits
	// for a later call.
	Next(limit int) ([]LogFrame, error)
	Close() error
}

// Upstream is the relay's connection to a session.
type Upstream interface {
	// Prompt sends text as a session/prompt on the adopted session and
	// returns when the turn ends or the connection closes. It never cancels
	// the turn: closing the upstream detaches without session/cancel.
	Prompt(text string) error
	// Cancel sends session/cancel for the turn in flight.
	Cancel() error
	// Done is closed when the connection is over for good.
	Done() <-chan struct{}
	Close() error
}

// relayHub holds the server's relays.
type relayHub struct {
	log      func(event string, kv ...string)
	stop     <-chan struct{}
	poll     time.Duration
	retry    time.Duration
	ringSize int

	// active counts the relay connections being served. Their connections
	// are hijacked, so http.Server.Shutdown does not wait for them; drain
	// does.
	active sync.WaitGroup

	mu     sync.Mutex
	relays map[string]*relay
}

// drain waits, until ctx is done, for every relay connection to finish. The
// server closes stop first, which makes each send its ended message.
func (h *relayHub) drain(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		h.active.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func newRelayHub(log func(string, ...string), stop <-chan struct{}) *relayHub {
	return &relayHub{log: log, stop: stop, poll: relayPoll, retry: relayRetry, ringSize: RelayRingSize, relays: map[string]*relay{}}
}

// observers is how many browsers watch session id.
func (h *relayHub) observers(id string) int {
	h.mu.Lock()
	r := h.relays[id]
	h.mu.Unlock()
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs)
}

// join registers sub with the session's relay, starting one if none runs,
// and waits until the relay has read the log so far.
func (h *relayHub) join(sess RelaySession, sub *subscriber) *relay {
	h.mu.Lock()
	r := h.relays[sess.ID]
	if r == nil {
		r = newRelay(h, sess)
		h.relays[sess.ID] = r
		go r.run()
	}
	r.mu.Lock()
	r.subs[sub] = struct{}{}
	r.mu.Unlock()
	h.mu.Unlock()
	<-r.ready
	return r
}

// leave unregisters sub. The last browser to leave stops the relay, which
// closes its upstream connection.
func (h *relayHub) leave(r *relay, sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subs, sub)
	if len(r.subs) == 0 && r.ended == "" {
		if h.relays[r.sess.ID] == r {
			delete(h.relays, r.sess.ID)
		}
		r.quitOnce.Do(func() { close(r.quit) })
	}
}

// ringItem is one message in a relay's ring: a log frame, or a control
// message (encoded already) such as a turn change.
type ringItem struct {
	frame   *LogFrame
	control []byte
}

// relay is one session's upstream connection, log reader and ring.
type relay struct {
	hub  *relayHub
	sess RelaySession

	ready    chan struct{} // closed once the log so far is in the ring
	quit     chan struct{} // closed when the last browser leaves
	quitOnce sync.Once
	done     chan struct{} // closed when run returns
	wake     chan struct{}

	mu      sync.Mutex
	changed chan struct{} // closed and replaced whenever the ring grows or the relay ends
	subs    map[*subscriber]struct{}
	ring    []ringItem
	base    uint64 // seq of ring[0]
	pos     int64  // log offset after the last frame read
	// promptCount is the record's prompt_count as last read.
	promptCount int
	// turn is TurnRunning or TurnIdle, derived from the frames; stopReason
	// is the last turn's. turnEnded records that some turn has ended: for a
	// one-shot session, its only one.
	turn       string
	stopReason string
	turnEnded  bool
	upstream   Upstream
	prompting  bool
	ended      string
}

func newRelay(h *relayHub, sess RelaySession) *relay {
	return &relay{
		hub: h, sess: sess,
		ready: make(chan struct{}), quit: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1),
		changed: make(chan struct{}), subs: map[*subscriber]struct{}{}, turn: TurnIdle,
	}
}

// interactive reports whether a browser may prompt this session.
func (r *relay) interactive() bool { return r.sess.Interactive && r.sess.Persistent }

func (r *relay) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// bump wakes every browser's writer. Caller holds r.mu.
func (r *relay) bump() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// next is the seq after the ring's last item. Caller holds r.mu.
func (r *relay) next() uint64 { return r.base + uint64(len(r.ring)) }

// pushLocked appends an item, trimming the ring to at least ringSize items.
// Caller holds r.mu.
func (r *relay) pushLocked(it ringItem) {
	r.ring = append(r.ring, it)
	if n := r.hub.ringSize; n > 0 && len(r.ring) >= 2*n {
		drop := len(r.ring) - n
		r.ring = append([]ringItem(nil), r.ring[drop:]...)
		r.base += uint64(drop)
	}
}

// setTurnLocked records a turn state and, when it changed, queues a turn
// message. Caller holds r.mu.
func (r *relay) setTurnLocked(state, stop string) {
	if state == TurnIdle && stop != "" && stop != turnError {
		r.turnEnded = true
	}
	if state == r.turn && (state == TurnRunning || stop == r.stopReason) {
		return
	}
	r.turn = state
	if state == TurnIdle {
		r.stopReason = stop
	}
	r.pushLocked(ringItem{control: turnMessage(state, stop)})
}

// addFrames puts frames read from the log into the ring and follows the
// turn state they imply.
func (r *relay) addFrames(frames []LogFrame) {
	if len(frames) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range frames {
		f := frames[i]
		if !json.Valid(f.Data) {
			// A line that was not JSON, redacted as text: relay it as a
			// string so the message stays valid JSON.
			f.Data, _ = json.Marshal(string(f.Data))
		}
		r.pushLocked(ringItem{frame: &f})
		r.pos = f.End
		if state, stop, ok := turnSignal(f.Data); ok {
			if r.turn == state && (state == TurnRunning || stop == turnError) {
				continue
			}
			r.setTurnLocked(state, stop)
		}
	}
	r.bump()
}

// wantUpstream reports whether the relay should hold a connection now (see
// the lifetime rules above). Caller holds r.mu.
func (r *relay) wantUpstreamLocked() bool {
	if r.ended != "" || len(r.subs) == 0 {
		return false
	}
	if r.sess.Persistent {
		return true
	}
	return r.promptCount > 0 && !r.turnEnded
}

type attachResult struct {
	up  Upstream
	err error
}

// run is the relay's loop: it reads the log, watches the record and keeps
// the upstream connection in line with the lifetime rules, until the session
// ends, the last browser leaves or the server stops.
func (r *relay) run() {
	defer close(r.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var log LogReader
	openLog := func() {
		if log != nil || r.sess.OpenLog == nil {
			return
		}
		r.mu.Lock()
		pos := r.pos
		r.mu.Unlock()
		if l, err := r.sess.OpenLog(pos); err == nil {
			log = l
		}
	}
	readLog := func() {
		openLog()
		for log != nil {
			frames, err := log.Next(relayReadBatch)
			r.addFrames(frames)
			if err != nil || len(frames) < relayReadBatch {
				return
			}
		}
	}
	readState := func() string {
		if r.sess.State == nil {
			return ""
		}
		st, err := r.sess.State()
		if err != nil {
			return ""
		}
		r.mu.Lock()
		r.promptCount = st.PromptCount
		r.mu.Unlock()
		return st.Ended
	}

	readLog()
	readState()
	close(r.ready)

	var (
		up         Upstream
		upDone     <-chan struct{}
		connecting bool
		lastTry    time.Time
		results    = make(chan attachResult)
	)
	closeUp := func() {
		if up != nil {
			up.Close()
		}
		up, upDone = nil, nil
		r.mu.Lock()
		r.upstream = nil
		r.mu.Unlock()
	}
	defer func() {
		closeUp()
		if log != nil {
			log.Close()
		}
	}()
	tick := time.NewTicker(r.hub.poll)
	defer tick.Stop()
	for {
		readLog()
		if why := readState(); why != "" {
			readLog() // the frames written before the end
			r.end(why)
			return
		}
		r.mu.Lock()
		want := r.wantUpstreamLocked()
		r.mu.Unlock()
		if !want && up != nil {
			// A one-shot session's turn ended: disconnect, as the TUI does.
			closeUp()
		}
		if want && up == nil && !connecting && time.Since(lastTry) >= r.hub.retry && r.sess.Attach != nil {
			connecting, lastTry = true, time.Now()
			go func() {
				u, err := r.sess.Attach(ctx, r.signal)
				select {
				case results <- attachResult{u, err}:
				case <-r.done:
					if u != nil {
						u.Close()
					}
				}
			}()
		}

		select {
		case <-tick.C:
		case <-r.wake:
		case <-r.quit:
			return
		case <-r.hub.stop:
			return
		case <-upDone:
			closeUp()
			lastTry = time.Now()
		case res := <-results:
			connecting = false
			if res.err != nil {
				if ctx.Err() == nil {
					r.hub.log("api_relay_attach_error", "session", r.sess.ID, "err", res.err.Error())
				}
				continue
			}
			// The turn may have ended while we dialed: read up to now first.
			readLog()
			r.mu.Lock()
			want := r.wantUpstreamLocked()
			if want {
				r.upstream = res.up
			}
			r.mu.Unlock()
			if !want {
				res.up.Close()
				continue
			}
			up, upDone = res.up, res.up.Done()
		}
	}
}

// end marks the relay over: every browser gets an ended message after the
// frames it has not seen yet.
func (r *relay) end(why string) {
	r.hub.mu.Lock()
	if r.hub.relays[r.sess.ID] == r {
		delete(r.hub.relays, r.sess.ID)
	}
	r.hub.mu.Unlock()
	r.mu.Lock()
	r.ended = why
	r.bump()
	r.mu.Unlock()
}

// prompt sends text to the session as a new turn. It fails while the relay
// has no upstream connection or a turn is running. A failure that happens
// once the turn is under way goes to onErr.
func (r *relay) prompt(text string, onErr func(error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.ended != "":
		return fmt.Errorf("%w: the session has ended", ErrInvalidState)
	case r.upstream == nil:
		return fmt.Errorf("%w: not connected to the session yet; try again in a moment", ErrUnavailable)
	case r.prompting || r.turn == TurnRunning:
		return fmt.Errorf("%w: a turn is running; wait for it to end or cancel it", ErrConflict)
	}
	up := r.upstream
	r.prompting = true
	r.setTurnLocked(TurnRunning, "")
	r.bump()
	go func() {
		err := up.Prompt(text)
		r.mu.Lock()
		r.prompting = false
		if err != nil && r.turn == TurnRunning {
			r.setTurnLocked(TurnIdle, turnError)
			r.bump()
		}
		r.mu.Unlock()
		if err != nil && onErr != nil {
			onErr(err)
		}
	}()
	return nil
}

// cancel sends session/cancel for the running turn.
func (r *relay) cancel() error {
	r.mu.Lock()
	up := r.upstream
	r.mu.Unlock()
	if up == nil {
		return fmt.Errorf("%w: not connected to the session", ErrUnavailable)
	}
	return up.Cancel()
}

// turnError is the stop reason of a turn that ended in an error response
// rather than a stopReason. It does not count as a one-shot session's end:
// the error may answer some other request.
const turnError = "error"

// turnSignal reads the turn state a frame implies: a response carrying a
// stopReason ends a turn, and so (as turnError) does an error response; a
// session/update with agent output means one is running. Other frames (the
// handshake's responses, command lists) say nothing.
func turnSignal(frame []byte) (state, stop string, ok bool) {
	var f struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Error  json.RawMessage `json:"error"`
		Result struct {
			StopReason string `json:"stopReason"`
		} `json:"result"`
		Params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &f) != nil {
		return "", "", false
	}
	switch {
	case len(f.ID) > 0 && f.Method == "" && f.Result.StopReason != "":
		return TurnIdle, f.Result.StopReason, true
	case len(f.ID) > 0 && f.Method == "" && len(f.Error) > 0 && string(f.Error) != "null":
		return TurnIdle, turnError, true
	case f.Method == "session/update":
		switch f.Params.Update.SessionUpdate {
		case "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update", "plan":
			return TurnRunning, "", true
		}
	}
	return "", "", false
}

// Relay messages (§A.5.2): every WebSocket message is one JSON object whose
// "t" says what it is.
const (
	MsgHello  = "hello"
	MsgReplay = "replay"
	MsgFrame  = "frame"
	MsgLive   = "live"
	MsgTurn   = "turn"
	MsgEnded  = "ended"
	MsgError  = "error"
	MsgPong   = "pong"
	MsgPrompt = "prompt"
	MsgCancel = "cancel"
	MsgPing   = "ping"
)

// RelayHello is the first message on a relay connection.
type RelayHello struct {
	T                  string `json:"t"`
	Session            string `json:"session"`
	Kind               string `json:"kind"`
	Persistent         bool   `json:"persistent"`
	InteractiveAllowed bool   `json:"interactive_allowed"`
	Mode               string `json:"mode"`
	PromptCount        int    `json:"prompt_count"`
	// Replay is "all", "none" or "after", as asked, or "reset" when the
	// asked offset lies past the end of the log (an older, longer log),
	// which replays it all.
	Replay string `json:"replay"`
}

// RelayFrame is a replay or frame message: an ACP JSON-RPC frame from the
// stream log and the offset after it, to resume from with
// replay=after:<offset>.
type RelayFrame struct {
	T      string          `json:"t"`
	Offset int64           `json:"offset"`
	Frame  json.RawMessage `json:"frame"`
}

// RelayTurn is a turn message: the turn state derived from the frames.
type RelayTurn struct {
	T          string `json:"t"`
	State      string `json:"state"`
	StopReason string `json:"stop_reason,omitempty"`
}

// RelayEnded is the last message: why the stream is over.
type RelayEnded struct {
	T      string `json:"t"`
	Reason string `json:"reason"`
}

// RelayError reports a refused or failed client message.
type RelayError struct {
	T       string `json:"t"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RelayClientMessage is what a browser sends: prompt (with Text), cancel
// or ping.
type RelayClientMessage struct {
	T    string `json:"t"`
	Text string `json:"text,omitempty"`
}

func turnMessage(state, stop string) []byte {
	b, _ := json.Marshal(RelayTurn{T: MsgTurn, State: state, StopReason: stop})
	return b
}

func frameMessage(t string, f *LogFrame) []byte {
	b, _ := json.Marshal(RelayFrame{T: t, Offset: f.End, Frame: f.Data})
	return b
}
