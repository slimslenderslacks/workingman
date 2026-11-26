package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event types of the live stream (§A.3.3). The server itself emits hello,
// heartbeat and reset; the daemon's publisher emits the others.
const (
	EventHello          = "hello"
	EventHeartbeat      = "heartbeat"
	EventReset          = "reset"
	EventProjectCreated = "project.created"
	EventProjectUpdated = "project.updated"
	EventProjectStatus  = "project.status"
	EventProjectRemoved = "project.removed"
	EventProjectLive    = "project.live"
	EventTaskCreated    = "task.created"
	EventTaskUpdated    = "task.updated"
	EventTaskStatus     = "task.status"
	EventTaskRemoved    = "task.removed"
	EventQueueUpdated   = "queue.updated"
	EventSessionStarted = "session.started"
	EventSessionUpdated = "session.updated"
	EventSessionEnded   = "session.ended"
	EventIntakeCreated  = "intake.created"
	EventIntakeUpdated  = "intake.updated"
	EventIntakeDone     = "intake.processed"
	EventIntakeRemoved  = "intake.removed"
	EventPRUpdated      = "pr.updated"
	EventMessageCreated = "message.created"
	EventMessageClosed  = "message.closed"
	EventAudit          = "audit"
)

// Event causes: what made the state change.
const (
	CauseFile    = "file"
	CauseSession = "session"
	CauseLive    = "live"
	CauseAudit   = "audit"
	CauseDaemon  = "daemon"
)

// Replay modes in the hello frame.
const (
	// ReplayFull: the client gave no cursor; synthetic project.created,
	// task.created and session.started events for the current state follow.
	ReplayFull = "full"
	// ReplayDelta: the cursor is in the ring; every event after it follows.
	ReplayDelta = "delta"
	// ReplayReset: the cursor is older than the ring, from another daemon
	// process or unreadable; a reset frame follows and the client refetches.
	ReplayReset = "reset"
)

// DefaultRingSize is how many events a [Hub] keeps for resuming clients.
const DefaultRingSize = 5000

// HeartbeatInterval is how long a stream goes quiet before a heartbeat.
const HeartbeatInterval = 15 * time.Second

// subscriberBuffer is how many events a stream may fall behind before the
// hub drops it. The client reconnects with Last-Event-ID and replays the
// missed events from the ring.
const subscriberBuffer = 1024

// Event is one frame of the stream: the common envelope of §A.3.3. Data is
// the type's payload, already JSON (and already redacted).
type Event struct {
	Seq     uint64          `json:"seq"`
	Time    time.Time       `json:"time"`
	Type    string          `json:"type"`
	Project string          `json:"project,omitempty"`
	Cause   string          `json:"cause,omitempty"`
	Data    json.RawMessage `json:"data"`
}

// NewEvent builds an event (without seq and time, which [Hub.Publish]
// assigns) with data marshalled to JSON.
func NewEvent(typ, project, cause string, data any) Event {
	raw, err := json.Marshal(data)
	if err != nil {
		raw, _ = json.Marshal(map[string]string{"error": err.Error()})
	}
	return Event{Type: typ, Project: project, Cause: cause, Data: raw}
}

// Hello is the first frame's data.
type Hello struct {
	Cursor     uint64 `json:"cursor"`
	Epoch      string `json:"epoch"`
	APIVersion string `json:"api_version"`
	Daemon     any    `json:"daemon"`
	Replay     string `json:"replay"`
}

// CursorData is the data of a reset or heartbeat frame.
type CursorData struct {
	Cursor uint64 `json:"cursor"`
}

// Hub is the event stream's ring and fan-out. The daemon's publisher calls
// [Hub.Publish] with each batch of events and the current state; every
// /events request subscribes to it. seq is per Hub, so per daemon process;
// the epoch (the daemon's start time) tells two processes' seqs apart in an
// SSE id, "<epoch>.<seq>".
type Hub struct {
	epoch string
	size  int
	now   func() time.Time

	mu    sync.Mutex
	seq   uint64
	ring  []Event // oldest first, contiguous seqs
	state func() []Event
	subs  map[*subscription]struct{}
}

// NewHub returns a hub whose ids carry epoch, keeping the last size events
// (<= 0: [DefaultRingSize]).
func NewHub(epoch string, size int) *Hub {
	if size <= 0 {
		size = DefaultRingSize
	}
	return &Hub{epoch: epoch, size: size, now: time.Now, subs: map[*subscription]struct{}{}}
}

// Epoch is the prefix of the hub's SSE ids.
func (h *Hub) Epoch() string { return h.epoch }

// Cursor is the seq of the latest event, 0 before any.
func (h *Hub) Cursor() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// Subscribers is how many streams are connected.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Publish assigns the next seqs (and the time, when unset) to evs, appends
// them to the ring and sends them to every stream. state, when non-nil,
// replaces the function a full replay reads the current state from; it
// must describe the state after evs. Both happen under one lock, so a
// client that connects without a cursor sees the state as of some seq and
// then exactly the events after it. A stream that has fallen too far
// behind is dropped.
func (h *Hub) Publish(evs []Event, state func() []Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if state != nil {
		h.state = state
	}
	if len(evs) == 0 {
		return
	}
	now := h.now().UTC()
	for i := range evs {
		h.seq++
		evs[i].Seq = h.seq
		if evs[i].Time.IsZero() {
			evs[i].Time = now
		}
	}
	h.ring = append(h.ring, evs...)
	if len(h.ring) > 2*h.size {
		// Trimming in batches keeps Publish amortized O(len(evs)).
		h.ring = append([]Event(nil), h.ring[len(h.ring)-h.size:]...)
	}
	for s := range h.subs {
		for _, e := range evs {
			select {
			case s.ch <- e:
			default:
				h.dropLocked(s)
			}
			if s.dropped {
				break
			}
		}
	}
}

// Since returns the events after seq still in the ring, oldest first; ok
// is false when the ring no longer holds all of them, or seq is ahead of
// the hub.
func (h *Hub) Since(seq uint64) ([]Event, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sinceLocked(seq)
}

// sinceLocked returns the events after seq, oldest first; ok is false when the
// ring no longer holds all of them, or seq is ahead of the hub.
func (h *Hub) sinceLocked(seq uint64) ([]Event, bool) {
	if seq > h.seq {
		return nil, false
	}
	start := len(h.ring) - h.size
	if start < 0 {
		start = 0
	}
	ring := h.ring[start:]
	if len(ring) == 0 {
		return nil, seq == h.seq
	}
	first := ring[0].Seq
	if seq+1 < first {
		return nil, false
	}
	return append([]Event(nil), ring[seq+1-first:]...), true
}

// subscription is one connected stream.
type subscription struct {
	ch      chan Event
	done    chan struct{} // closed when the hub drops the stream
	dropped bool
}

func (h *Hub) dropLocked(s *subscription) {
	if !s.dropped {
		s.dropped = true
		close(s.done)
	}
	delete(h.subs, s)
}

func (h *Hub) unsubscribe(s *subscription) {
	h.mu.Lock()
	h.dropLocked(s)
	h.mu.Unlock()
}

// resume is where a client asks to start from.
type resume struct {
	given   bool   // a cursor was given
	seq     uint64 // valid when given and !invalid
	invalid bool   // unreadable, or from another epoch
}

// subscribe registers a stream and returns what it must send first: the
// replay mode, the cursor live events continue from, and the backlog (the
// missed events for a delta, the state for a full replay).
func (h *Hub) subscribe(from resume) (s *subscription, replay string, cursor uint64, backlog []Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s = &subscription{ch: make(chan Event, subscriberBuffer), done: make(chan struct{})}
	h.subs[s] = struct{}{}
	cursor = h.seq
	switch {
	case !from.given:
		replay = ReplayFull
		if h.state != nil {
			for _, e := range h.state() {
				e.Seq, e.Time = cursor, h.now().UTC()
				if e.Cause == "" {
					e.Cause = CauseDaemon
				}
				backlog = append(backlog, e)
			}
		}
	case from.invalid:
		replay = ReplayReset
	default:
		evs, ok := h.sinceLocked(from.seq)
		if !ok {
			replay = ReplayReset
		} else {
			replay, backlog = ReplayDelta, evs
		}
	}
	return s, replay, cursor, backlog
}

// ID renders a seq as an SSE id, "<epoch>.<seq>".
func (h *Hub) ID(seq uint64) string { return h.epoch + "." + strconv.FormatUint(seq, 10) }

// parseCursor reads a client's cursor: "<epoch>.<seq>" (an SSE id) or a
// bare seq (a list's cursor), which is taken to be from this epoch.
func (h *Hub) parseCursor(v string) (resume, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return resume{}, nil
	}
	epoch, seqStr, hasEpoch := strings.Cut(v, ".")
	if !hasEpoch {
		seqStr = epoch
	}
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		return resume{given: true, invalid: true}, fmt.Errorf("cursor %q is not <epoch>.<seq> or <seq>", v)
	}
	if hasEpoch && epoch != h.epoch {
		return resume{given: true, invalid: true}, nil
	}
	return resume{given: true, seq: seq}, nil
}

// eventFilter is a stream's subscription: ?project= and ?types=.
type eventFilter struct {
	projects []string
	types    []string // globs; empty: everything but audit
}

// defaultEventTypes is the subscription when ?types= is not given.
var defaultEventTypes = []string{"project.*", "task.*", "queue.*", "session.*", "intake.*", "pr.*", "message.*"}

func (f eventFilter) match(e Event) bool {
	if len(f.projects) > 0 && !slices.Contains(f.projects, e.Project) {
		return false
	}
	types := f.types
	if len(types) == 0 {
		types = defaultEventTypes
	}
	for _, g := range types {
		if ok, _ := path.Match(g, e.Type); ok {
			return true
		}
	}
	return false
}

// handleEvents serves GET /events, the SSE stream of §A.3.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	hub := s.src.Events()
	if hub == nil {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "the event stream is not running")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal, "the response cannot be streamed")
		return
	}
	f := eventFilter{projects: csv(r, "project"), types: csv(r, "types")}
	for _, g := range f.types {
		if _, err := path.Match(g, ""); err != nil {
			writeErr(w, badQuery("types: bad pattern "+strconv.Quote(g)))
			return
		}
	}
	// A browser's reconnect sends Last-Event-ID and repeats the URL, so the
	// header wins over ?since=. An unreadable header is a reset, not an
	// error: the client cannot change what it sends.
	from, err := hub.parseCursor(r.URL.Query().Get("since"))
	if err != nil {
		writeErr(w, badQuery("since: "+err.Error()))
		return
	}
	if id := r.Header.Get("Last-Event-ID"); strings.TrimSpace(id) != "" {
		from, _ = hub.parseCursor(id)
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	sub, replay, cursor, backlog := hub.subscribe(from)
	defer hub.unsubscribe(sub)
	sw := &sseWriter{w: w, hub: hub}
	sw.frame(false, Event{Seq: cursor, Time: s.now().UTC(), Type: EventHello, Cause: CauseDaemon,
		Data: mustJSON(Hello{Cursor: cursor, Epoch: hub.Epoch(), APIVersion: Version, Daemon: s.src.DaemonInfo(), Replay: replay})})
	switch replay {
	case ReplayReset:
		sw.frame(true, Event{Seq: cursor, Time: s.now().UTC(), Type: EventReset, Cause: CauseDaemon, Data: mustJSON(CursorData{Cursor: cursor})})
	case ReplayFull:
		// No ids: the client's position is still "nothing", so a
		// reconnect in the middle of the state asks for all of it again.
		for _, e := range backlog {
			if f.match(e) {
				sw.frame(false, e)
			}
		}
	case ReplayDelta:
		for _, e := range backlog {
			if f.match(e) {
				sw.frame(true, e)
			}
		}
	}
	flusher.Flush()
	if sw.err != nil {
		return
	}

	beat := time.NewTimer(s.heartbeat)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stopStreams:
			return
		case <-sub.done:
			// Dropped for falling behind: end the response, and the client
			// resumes from its Last-Event-ID.
			return
		case e := <-sub.ch:
			cursor = e.Seq
			if !f.match(e) {
				continue
			}
			sw.frame(true, e)
		case <-beat.C:
			sw.frame(false, Event{Seq: cursor, Time: s.now().UTC(), Type: EventHeartbeat, Cause: CauseDaemon, Data: mustJSON(CursorData{Cursor: cursor})})
		}
		// Drain what is already queued before flushing once.
		for drained := false; !drained; {
			select {
			case e := <-sub.ch:
				cursor = e.Seq
				if f.match(e) {
					sw.frame(true, e)
				}
			default:
				drained = true
			}
		}
		flusher.Flush()
		if sw.err != nil {
			return
		}
		beat.Reset(s.heartbeat)
	}
}

// sseWriter writes SSE frames and remembers the first write error.
type sseWriter struct {
	w   http.ResponseWriter
	hub *Hub
	err error
	buf []byte
}

// frame writes e; withID adds its "<epoch>.<seq>" id line, which is what a
// reconnecting client resumes from.
func (sw *sseWriter) frame(withID bool, e Event) {
	if sw.err != nil {
		return
	}
	data, err := json.Marshal(e)
	if err != nil {
		sw.err = err
		return
	}
	b := sw.buf[:0]
	if withID {
		b = append(b, "id: "...)
		b = append(b, sw.hub.ID(e.Seq)...)
		b = append(b, '\n')
	}
	b = append(b, "event: "...)
	b = append(b, e.Type...)
	b = append(b, "\ndata: "...)
	b = append(b, data...)
	b = append(b, "\n\n"...)
	sw.buf = b
	_, sw.err = sw.w.Write(b)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
