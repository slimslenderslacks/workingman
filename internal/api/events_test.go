package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ev(typ, project string) Event {
	return NewEvent(typ, project, CauseFile, map[string]string{"t": typ})
}

func TestHubRingAndSince(t *testing.T) {
	h := NewHub("100", 3)
	if h.Cursor() != 0 {
		t.Fatalf("cursor = %d", h.Cursor())
	}
	if evs, ok := h.Since(0); !ok || len(evs) != 0 {
		t.Errorf("Since(0) on an empty hub = %v %v", evs, ok)
	}
	for i := 0; i < 5; i++ {
		h.Publish([]Event{ev(EventTaskUpdated, "p")}, nil)
	}
	if h.Cursor() != 5 {
		t.Fatalf("cursor = %d", h.Cursor())
	}
	// The ring holds 3..5.
	if evs, ok := h.Since(2); !ok || len(evs) != 3 || evs[0].Seq != 3 || evs[2].Seq != 5 {
		t.Errorf("Since(2) = %v %v", evs, ok)
	}
	if evs, ok := h.Since(4); !ok || len(evs) != 1 || evs[0].Seq != 5 {
		t.Errorf("Since(4) = %v %v", evs, ok)
	}
	if evs, ok := h.Since(5); !ok || len(evs) != 0 {
		t.Errorf("Since(5) = %v %v", evs, ok)
	}
	if _, ok := h.Since(1); ok {
		t.Error("Since(1) after event 2 left the ring should fail")
	}
	if _, ok := h.Since(9); ok {
		t.Error("Since a seq ahead of the hub should fail")
	}
	// Many more: the ring stays bounded.
	for i := 0; i < 100; i++ {
		h.Publish([]Event{ev(EventTaskUpdated, "p"), ev(EventAudit, "p")}, nil)
	}
	if len(h.ring) > 2*h.size {
		t.Errorf("ring grew to %d", len(h.ring))
	}
	if evs, ok := h.Since(h.Cursor() - 3); !ok || len(evs) != 3 {
		t.Errorf("tail = %v %v", evs, ok)
	}
}

func TestParseCursor(t *testing.T) {
	h := NewHub("100", 0)
	for _, tc := range []struct {
		in   string
		want resume
		err  bool
	}{
		{"", resume{}, false},
		{"7", resume{given: true, seq: 7}, false},
		{"100.7", resume{given: true, seq: 7}, false},
		{"99.7", resume{given: true, invalid: true}, false},
		{"x", resume{given: true, invalid: true}, true},
		{"100.x", resume{given: true, invalid: true}, true},
	} {
		got, err := h.parseCursor(tc.in)
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("parseCursor(%q) = %+v, %v", tc.in, got, err)
		}
	}
}

func TestEventFilter(t *testing.T) {
	all := eventFilter{}
	if !all.match(ev(EventTaskStatus, "p")) || all.match(ev(EventAudit, "p")) {
		t.Error("the default subscription is everything but audit")
	}
	f := eventFilter{projects: []string{"p_0.a"}, types: []string{"task.*", "audit"}}
	for _, tc := range []struct {
		e    Event
		want bool
	}{
		{ev(EventTaskStatus, "p_0.a"), true},
		{ev(EventAudit, "p_0.a"), true},
		{ev(EventTaskStatus, "p_0.b"), false},
		{ev(EventQueueUpdated, "p_0.a"), false},
		{ev(EventMessageCreated, ""), false},
	} {
		if f.match(tc.e) != tc.want {
			t.Errorf("match(%s %s) = %v", tc.e.Type, tc.e.Project, !tc.want)
		}
	}
}

// streamServer is a server whose hub a test publishes into.
func streamServer(t *testing.T, hub *Hub) (*httptest.Server, *Client, *Server) {
	t.Helper()
	s, tok := newTestServer(t, &fakeSource{hub: hub, info: map[string]any{"state": "running"}})
	s.heartbeat = 50 * time.Millisecond
	ts := httptest.NewServer(s.Handler(""))
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	// The Host check wants localhost.
	c := &Client{Addr: "localhost:" + u.Port(), Token: tok}
	return ts, c, s
}

// collect reads frames from a stream into a channel until ctx ends.
func collect(ctx context.Context, c *Client, lastID string, q url.Values) (<-chan StreamEvent, <-chan error) {
	ch := make(chan StreamEvent, 1024)
	errc := make(chan error, 1)
	go func() {
		errc <- c.Events(ctx, lastID, q, func(e StreamEvent) error { ch <- e; return nil })
	}()
	return ch, errc
}

func recv(t *testing.T, ch <-chan StreamEvent) StreamEvent {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return StreamEvent{}
}

func recvType(t *testing.T, ch <-chan StreamEvent, typ string) StreamEvent {
	t.Helper()
	for {
		if e := recv(t, ch); e.Type == typ {
			return e
		}
	}
}

func helloOf(t *testing.T, e StreamEvent) Hello {
	t.Helper()
	if e.Type != EventHello {
		t.Fatalf("first frame is %s, want hello", e.Type)
	}
	var h Hello
	if err := json.Unmarshal(e.Data, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

// Without a cursor: hello{full}, the state without ids, then live events
// with ids.
func TestEventsFullReplay(t *testing.T) {
	hub := NewHub("42", 10)
	hub.Publish([]Event{ev(EventTaskUpdated, "p")}, func() []Event {
		return []Event{ev(EventProjectCreated, "p"), ev(EventTaskCreated, "p")}
	})
	_, c, _ := streamServer(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := collect(ctx, c, "", nil)

	h := helloOf(t, recv(t, ch))
	if h.Replay != ReplayFull || h.Cursor != 1 || h.Epoch != "42" || h.APIVersion != Version {
		t.Errorf("hello = %+v", h)
	}
	for _, want := range []string{EventProjectCreated, EventTaskCreated} {
		e := recv(t, ch)
		if e.Type != want || e.ID != "" || e.Seq != 1 || e.Cause != CauseFile {
			t.Errorf("state frame = %+v, want %s without id at seq 1", e, want)
		}
	}
	waitSubscribers(t, hub, 1)
	hub.Publish([]Event{ev(EventTaskStatus, "p")}, nil)
	e := recvType(t, ch, EventTaskStatus)
	if e.ID != "42.2" || e.Seq != 2 {
		t.Errorf("live frame = %+v", e)
	}
}

func waitSubscribers(t *testing.T, hub *Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for hub.Subscribers() != n {
		if time.Now().After(deadline) {
			t.Fatalf("subscribers = %d, want %d", hub.Subscribers(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// With a cursor in the ring: hello{delta} and every event after it, so a
// reconnect misses nothing; ?since= works the same, and the header wins.
func TestEventsDeltaResume(t *testing.T) {
	hub := NewHub("42", 10)
	for i := 0; i < 5; i++ {
		hub.Publish([]Event{ev(EventTaskUpdated, "p")}, nil)
	}
	_, c, _ := streamServer(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, tc := range []struct {
		name   string
		lastID string
		q      url.Values
	}{
		{"Last-Event-ID", "42.3", nil},
		{"since", "", url.Values{"since": {"3"}}},
		{"since with epoch", "", url.Values{"since": {"42.3"}}},
		{"header wins", "42.3", url.Values{"since": {"1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, scancel := context.WithCancel(ctx)
			defer scancel()
			ch, _ := collect(sctx, c, tc.lastID, tc.q)
			if h := helloOf(t, recv(t, ch)); h.Replay != ReplayDelta || h.Cursor != 5 {
				t.Errorf("hello = %+v", h)
			}
			for _, want := range []string{"42.4", "42.5"} {
				if e := recv(t, ch); e.ID != want {
					t.Errorf("frame id = %q, want %q", e.ID, want)
				}
			}
		})
	}
}

// A cursor older than the ring, or from another epoch: hello{reset}, then
// reset{cursor}, then live events.
func TestEventsReset(t *testing.T) {
	hub := NewHub("42", 2)
	for i := 0; i < 5; i++ {
		hub.Publish([]Event{ev(EventTaskUpdated, "p")}, nil)
	}
	_, c, _ := streamServer(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, id := range []string{"42.1", "41.5", "garbage", "42.99"} {
		t.Run(id, func(t *testing.T) {
			sctx, scancel := context.WithCancel(ctx)
			defer scancel()
			ch, _ := collect(sctx, c, id, nil)
			if h := helloOf(t, recv(t, ch)); h.Replay != ReplayReset {
				t.Errorf("hello = %+v", h)
			}
			r := recv(t, ch)
			var cd CursorData
			_ = json.Unmarshal(r.Data, &cd)
			if r.Type != EventReset || cd.Cursor != 5 || r.ID != "42.5" {
				t.Errorf("reset = %+v %+v", r, cd)
			}
		})
	}
}

// ?project= and ?types= filter replayed and live events alike; a heartbeat
// keeps a quiet stream alive and carries the cursor.
func TestEventsFilterAndHeartbeat(t *testing.T) {
	hub := NewHub("42", 10)
	hub.Publish([]Event{ev(EventTaskStatus, "a"), ev(EventTaskStatus, "b"), ev(EventAudit, "a")}, nil)
	_, c, _ := streamServer(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := collect(ctx, c, "42.0", url.Values{"project": {"a"}, "types": {"task.*,audit"}})
	helloOf(t, recv(t, ch))
	if e := recv(t, ch); e.Type != EventTaskStatus || e.Project != "a" {
		t.Errorf("first = %+v", e)
	}
	if e := recv(t, ch); e.Type != EventAudit {
		t.Errorf("second = %+v", e)
	}
	waitSubscribers(t, hub, 1)
	hub.Publish([]Event{ev(EventQueueUpdated, "a"), ev(EventTaskStatus, "b")}, nil)
	hb := recv(t, ch)
	var cd CursorData
	_ = json.Unmarshal(hb.Data, &cd)
	if hb.Type != EventHeartbeat || cd.Cursor != 5 || hb.ID != "" {
		t.Errorf("after filtered-out events: %+v %+v, want a heartbeat at cursor 5", hb, cd)
	}

	var se *StatusError
	if err := c.Events(ctx, "", url.Values{"types": {"["}}, nil); !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Errorf("bad types = %v", err)
	}
	if err := c.Events(ctx, "", url.Values{"since": {"x"}}, nil); !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Errorf("bad since = %v", err)
	}
}

// A stream that falls behind is dropped and resumes from its last id.
func TestEventsSlowClientDropped(t *testing.T) {
	hub := NewHub("42", 10*subscriberBuffer)
	sub, _, _, _ := hub.subscribe(resume{given: true})
	evs := make([]Event, subscriberBuffer+1)
	for i := range evs {
		evs[i] = ev(EventTaskUpdated, "p")
	}
	hub.Publish(evs, nil)
	select {
	case <-sub.done:
	default:
		t.Fatal("an overflowing subscriber was not dropped")
	}
	if hub.Subscribers() != 0 {
		t.Errorf("subscribers = %d", hub.Subscribers())
	}
	hub.unsubscribe(sub) // idempotent
	if got, ok := hub.Since(uint64(subscriberBuffer)); !ok || len(got) != 1 {
		t.Errorf("the dropped client cannot resume: %d %v", len(got), ok)
	}
}

// Close ends open streams, so the daemon's shutdown is not held up.
func TestEventsEndOnClose(t *testing.T) {
	dir := shortTempDir(t)
	hub := NewHub("42", 10)
	s := New(Config{Mode: ModeUnix, Listen: dir + "/api.sock", TokenPath: dir + "/token"}, &fakeSource{hub: hub})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	tok, _ := ReadToken(dir + "/token")
	c := &Client{Addr: dir + "/api.sock", Token: tok}
	ch, errc := collect(context.Background(), c, "", nil)
	helloOf(t, recv(t, ch))
	waitSubscribers(t, hub, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("Close waited %s for an open stream", time.Since(start))
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrStreamEnded) {
			t.Errorf("Events = %v, want ErrStreamEnded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after Close")
	}
}

// No hub: 503.
func TestEventsUnavailable(t *testing.T) {
	_, c, _ := streamServer(t, nil)
	var se *StatusError
	if err := c.Events(context.Background(), "", nil, nil); !errors.As(err, &se) || se.Status != http.StatusServiceUnavailable {
		t.Errorf("Events without a hub = %v", err)
	}
}

// Status carries the epoch, so a client can build an id from a cursor.
func TestStatusEpoch(t *testing.T) {
	hub := NewHub("42", 10)
	hub.Publish([]Event{ev(EventTaskUpdated, "p")}, nil)
	_, c, _ := streamServer(t, hub)
	var st Status
	if err := c.Get(context.Background(), Prefix+"/status", &st); err != nil {
		t.Fatal(err)
	}
	if st.Epoch != "42" {
		t.Errorf("status = %+v", st)
	}
}

func TestReadSSE(t *testing.T) {
	body := ": comment\n" +
		"id: 1.1\nevent: task.status\ndata: {\"seq\":1,\"type\":\"task.status\",\"data\":{}}\n\n" +
		"retry: 100\n" +
		"event: heartbeat\ndata: {\"seq\":1,\n" + "data: \"type\":\"heartbeat\",\"data\":{}}\n\n" +
		"\n"
	var got []string
	if err := ReadSSE(strings.NewReader(body), func(e StreamEvent) error {
		got = append(got, e.ID+"/"+e.Type+"/"+strconv.FormatUint(e.Seq, 10))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "1.1/task.status/1 /heartbeat/1" {
		t.Errorf("frames = %v", got)
	}
	if err := ReadSSE(strings.NewReader("data: nope\n\n"), func(StreamEvent) error { return nil }); err == nil {
		t.Error("bad data parsed")
	}
}
