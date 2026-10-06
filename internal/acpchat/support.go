package acpchat

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/acpclient"
)

// turnText accumulates a turn's assistant text as segments: consecutive chunks
// extend a segment, and a tool call starts a fresh one, so narration around tool
// use doesn't run together ("Let me check.Here is…").
type turnText struct {
	segs      []string
	afterTool bool
}

func (t *turnText) add(s string) {
	if len(t.segs) == 0 || t.afterTool {
		t.segs = append(t.segs, s)
		t.afterTool = false
		return
	}
	t.segs[len(t.segs)-1] += s
}

func (t *turnText) toolBoundary() {
	if len(t.segs) > 0 {
		t.afterTool = true
	}
}

// join renders the text: segments trimmed, blank ones dropped, separated by a
// blank line; with finalOnly just the last segment.
func (t turnText) join(finalOnly bool) string {
	var out []string
	for _, s := range t.segs {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if finalOnly && len(out) > 0 {
		return out[len(out)-1]
	}
	return strings.Join(out, "\n\n")
}

// turn is one in-flight Ask.
type turn struct {
	progress ProgressFunc
	done     chan struct{} // closed when the turn's last event has been processed
	once     sync.Once

	mu   sync.Mutex
	text turnText
}

func (t *turn) finish() { t.once.Do(func() { close(t.done) }) }

func (t *turn) add(s string) (reply string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.text.add(s)
	return t.text.join(false)
}

func (t *turn) toolBoundary() {
	t.mu.Lock()
	t.text.toolBoundary()
	t.mu.Unlock()
}

func (t *turn) reply(finalOnly bool) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.text.join(finalOnly)
}

// EventKind classifies an Event from Conversation.Events.
type EventKind int

const (
	// EventText is a chunk of assistant text. Own reports whether it belongs to a
	// turn started by this Conversation's Ask (true) or by another client — the
	// TUI user typing into the agent (false). A channel relaying "what the agent
	// says" wants the Own==false ones (the Ask caller already gets its reply).
	EventText EventKind = iota + 1
	// EventTurnEnd closes a turn: Text is the whole cleaned reply, StopReason why
	// it ended. For turns by other clients it is derived from the response the
	// bridge fans out, so it arrives only if this client was connected at the end.
	EventTurnEnd
	// EventPermission reports a tool-permission request the agent raised
	// (Permission); it is answered per Options.OnPermission regardless.
	EventPermission
	// EventReconnecting: the socket dropped; reconnecting.
	EventReconnecting
	// EventReconnected: back on the socket. SessionChanged is true when the session
	// had been restarted under the same id (a different agent process).
	EventReconnected
	// EventSessionChanged: another client started a new ACP session on the agent;
	// the conversation now follows SessionID.
	EventSessionChanged
	// EventGone is the last event: the session ended (Err wraps ErrSessionGone).
	EventGone
)

// Event is one thing observed on the conversation.
type Event struct {
	Kind           EventKind
	Own            bool
	Text           string
	StopReason     string
	SessionID      string
	SessionChanged bool
	Permission     *PermissionRequest
	Err            error
}

// Events returns the stream of what happens on the conversation, including
// assistant output produced by OTHER clients — the observe-only mode. Events are
// recorded only from the first call on (a Conversation that is only Asked buffers
// nothing), queued without bound so a slow consumer never stalls the agent
// stream, and the channel closes after EventGone or Close. There is one stream;
// every call returns it.
func (c *Conversation) Events() <-chan Event { return c.events.subscribe() }

// eventQueue is the unbounded, lazily-started queue behind Events.
type eventQueue struct {
	stop <-chan struct{}
	out  chan Event
	wake chan struct{}
	once sync.Once

	mu         sync.Mutex
	buf        []Event
	subscribed bool
	closed     bool
}

func (q *eventQueue) init(ctx context.Context) {
	q.stop = ctx.Done()
	q.out = make(chan Event)
	q.wake = make(chan struct{}, 1)
}

func (q *eventQueue) subscribe() <-chan Event {
	q.once.Do(func() {
		q.mu.Lock()
		q.subscribed = true
		q.mu.Unlock()
		go q.pump()
	})
	return q.out
}

func (q *eventQueue) push(ev Event) {
	q.mu.Lock()
	if !q.subscribed || q.closed {
		q.mu.Unlock()
		return
	}
	q.buf = append(q.buf, ev)
	q.mu.Unlock()
	q.signal()
}

// close lets the pump drain what is queued, then end the stream. Idempotent.
func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) pump() {
	defer close(q.out)
	for {
		q.mu.Lock()
		if len(q.buf) > 0 {
			ev := q.buf[0]
			q.buf = q.buf[1:]
			q.mu.Unlock()
			select {
			case q.out <- ev:
			case <-q.stop:
				return
			}
			continue
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-q.wake:
		case <-q.stop:
			return
		}
	}
}

// lastSessionIDInLog returns the newest ACP session id recorded in a session's
// stream log — the id on the latest session/update, permission request, or
// session/new response — or "" when the log is missing or holds none.
func lastSessionIDInLog(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var last string
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var fr struct {
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(line, &fr) == nil {
				if id := sessionIDOf(fr.Params); id != "" {
					last = id
				} else if id := sessionIDOf(fr.Result); id != "" {
					last = id
				}
			}
		}
		if err != nil {
			return last
		}
	}
}

func sessionIDOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.SessionID
}

// waitForSessionID watches a freshly dialed client's live stream for the first
// sign of an ACP session — an update carrying a sessionId, or another client's
// session/new response — for up to timeout. It returns "" (nil error) on timeout,
// and an error if the connection dies or ctx ends first. It consumes the client's
// events, so call it before anything else reads them.
func waitForSessionID(ctx context.Context, client *acpclient.Client, timeout time.Duration) (string, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case ev, ok := <-client.Events():
			if !ok || ev.State.IsTerminal() {
				if ev.Err != nil {
					return "", ev.Err
				}
				return "", acpclient.ErrConnectionClosed
			}
			if ev.SessionID != "" {
				return ev.SessionID, nil
			}
			if ev.Kind == acpclient.EventResponse && ev.Err == nil {
				if id := sessionIDOf(ev.Result); id != "" {
					return id, nil
				}
			}
		case <-t.C:
			return "", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}
