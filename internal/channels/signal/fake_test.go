package signal

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
)

const testAccount = "+15550001111"

// rpcCall is one JSON-RPC request the fake daemon received.
type rpcCall struct {
	Method string
	Params map[string]any
}

// fakeDaemon is an httptest signal-cli: /api/v1/check, an SSE stream fed from
// events, and /api/v1/rpc answered by handler.
type fakeDaemon struct {
	t       *testing.T
	srv     *httptest.Server
	events  chan string
	drop    chan struct{} // closing the current SSE connection
	connect atomic.Int32
	status  atomic.Int32 // non-zero: events endpoint answers with this status

	mu      sync.Mutex
	calls   []rpcCall
	handler func(call rpcCall) (result any, rpcErr *rpcError)
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	d := &fakeDaemon{t: t, events: make(chan string, 64), drop: make(chan struct{}, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/check", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/events", d.serveEvents)
	mux.HandleFunc("/api/v1/rpc", d.serveRPC)
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDaemon) serveEvents(w http.ResponseWriter, r *http.Request) {
	d.connect.Add(1)
	if got := r.URL.Query().Get("account"); got != testAccount {
		d.t.Errorf("events account = %q, want %q", got, testAccount)
	}
	if s := d.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	_, _ = io.WriteString(w, ": keepalive\n\n")
	fl.Flush()
	for {
		select {
		case ev := <-d.events:
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			fl.Flush()
		case <-d.drop:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (d *fakeDaemon) serveRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
		ID     any            `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	call := rpcCall{Method: req.Method, Params: req.Params}
	d.mu.Lock()
	d.calls = append(d.calls, call)
	h := d.handler
	d.mu.Unlock()

	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if h == nil {
		resp["result"] = map[string]any{"timestamp": 1700000000000, "results": []map[string]any{{"type": "SUCCESS"}}}
	} else if result, rerr := h(call); rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (d *fakeDaemon) setHandler(h func(rpcCall) (any, *rpcError)) {
	d.mu.Lock()
	d.handler = h
	d.mu.Unlock()
}

func (d *fakeDaemon) rpcCalls() []rpcCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]rpcCall(nil), d.calls...)
}

// push queues an SSE event carrying the envelope JSON.
func (d *fakeDaemon) push(envelope string) {
	d.events <- `{"envelope":` + envelope + `}`
}

// inbox collects messages delivered to the handler.
type inbox struct {
	mu   sync.Mutex
	msgs []channels.InboundMessage
}

func (b *inbox) handle(_ context.Context, m channels.InboundMessage) {
	b.mu.Lock()
	b.msgs = append(b.msgs, m)
	b.mu.Unlock()
}

func (b *inbox) all() []channels.InboundMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]channels.InboundMessage(nil), b.msgs...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testChannel builds a Channel against d with fast reconnects and no send pacing.
func testChannel(t *testing.T, d *fakeDaemon, access AccessConfig) *Channel {
	t.Helper()
	c := New("signal", Config{HTTPURL: d.srv.URL, Account: testAccount, Access: access}, quietLogger())
	c.retryInitial, c.retryMax = time.Millisecond, 5*time.Millisecond
	c.pace.gap = 0
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func startChannel(t *testing.T, c *Channel) *inbox {
	t.Helper()
	in := &inbox{}
	if err := c.Start(context.Background(), in.handle); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return in
}
