package signal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
)

const (
	pat     = "+15557654321"
	patEnv  = `"sourceNumber":"` + pat + `"`
	allowed = `"allow_from"`
)

func patAccess() AccessConfig { return AccessConfig{AllowFrom: []string{pat}} }

func dmEnvelope(ts int, text string) string {
	return fmt.Sprintf(`{%s,"sourceName":"Pat","timestamp":%d,"dataMessage":{"timestamp":%d,"message":%q}}`, patEnv, ts, ts, text)
}

func TestInboundDelivery(t *testing.T) {
	d := newFakeDaemon(t)
	c := testChannel(t, d, patAccess())
	in := startChannel(t, c)

	d.push(dmEnvelope(100, "hello"))
	d.push(`{"sourceNumber":"+15559998888","timestamp":101,"dataMessage":{"message":"let me in"}}`) // stranger
	d.push(`{` + patEnv + `,"timestamp":102,"receiptMessage":{"type":"READ"}}`)
	d.push(`{` + patEnv + `,"timestamp":103,"typingMessage":{"action":"STARTED"}}`)
	d.push(dmEnvelope(100, "hello"))                                                                       // redelivery: deduped
	d.push(`{` + patEnv + `,"timestamp":104,"dataMessage":{"message":"re","quote":{"id":1699999999000}}}`) // quoted reply
	d.push(dmEnvelope(105, "last"))

	eventually(t, "3 messages", func() bool { return len(in.all()) == 3 })
	time.Sleep(20 * time.Millisecond) // anything extra would show up now
	got := in.all()
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(got), got)
	}
	want := []struct{ id, text, reply string }{{"100", "hello", ""}, {"104", "re", "1699999999000"}, {"105", "last", ""}}
	for i, w := range want {
		m := got[i]
		if m.MessageID != w.id || m.Text != w.text || m.ReplyToID != w.reply || m.ChatID != pat || m.SenderID != pat || m.Channel != "signal" {
			t.Errorf("msg %d = %+v, want id=%s text=%q reply=%q", i, m, w.id, w.text, w.reply)
		}
	}
	if got[0].SenderName != "Pat" || got[0].Timestamp.UnixMilli() != 100 {
		t.Errorf("sender name / timestamp not mapped: %+v", got[0])
	}
}

func TestInboundGroupPolicy(t *testing.T) {
	tests := []struct {
		name   string
		access AccessConfig
		want   int
	}{
		{"groups off", patAccess(), 0},
		{"group on and listed", AccessConfig{AllowFrom: []string{pat}, Groups: true, GroupAllowFrom: []string{"R3JvdXA="}}, 1},
		{"other group listed", AccessConfig{AllowFrom: []string{pat}, Groups: true, GroupAllowFrom: []string{"b3RoZXI="}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			c := testChannel(t, d, tt.access)
			in := startChannel(t, c)
			d.push(`{` + patEnv + `,"timestamp":1,"dataMessage":{"message":"team","groupInfo":{"groupId":"R3JvdXA="}}}`)
			d.push(dmEnvelope(2, "marker")) // always delivered; proves the group event was processed first
			eventually(t, "marker", func() bool {
				for _, m := range in.all() {
					if m.Text == "marker" {
						return true
					}
				}
				return false
			})
			var groups int
			for _, m := range in.all() {
				if m.ChatID == "group:R3JvdXA=" {
					groups++
				}
			}
			if groups != tt.want {
				t.Errorf("group messages delivered = %d, want %d", groups, tt.want)
			}
		})
	}
}

func TestNoteToSelfInbound(t *testing.T) {
	note := func(ts int, text string) string {
		return fmt.Sprintf(`{"sourceNumber":%q,"sourceUuid":"11111111-2222-3333-4444-555555555555","timestamp":%d,"syncMessage":{"sentMessage":{"destinationNumber":%q,"timestamp":%d,"message":%q}}}`,
			testAccount, ts, testAccount, ts, text)
	}
	t.Run("opt-in admits owner", func(t *testing.T) {
		d := newFakeDaemon(t)
		c := testChannel(t, d, AccessConfig{NoteToSelf: true})
		in := startChannel(t, c)
		d.push(note(200, "remember the milk"))
		eventually(t, "note", func() bool { return len(in.all()) == 1 })
		m := in.all()[0]
		if m.ChatID != testAccount || m.SenderID != testAccount || m.MessageID != "200" || m.Text != "remember the milk" {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("default denies", func(t *testing.T) {
		d := newFakeDaemon(t)
		c := testChannel(t, d, patAccess())
		in := startChannel(t, c)
		d.push(note(201, "x"))
		d.push(dmEnvelope(202, "marker"))
		eventually(t, "marker", func() bool { return len(in.all()) == 1 })
		if in.all()[0].Text != "marker" {
			t.Errorf("note to self was admitted without opt-in: %+v", in.all())
		}
	})
}

// TestNoteToSelfEchoSuppressed covers our own reply to Note to Self coming back
// as a sync message, including the race where the echo arrives before the send
// RPC has returned its timestamp.
func TestNoteToSelfEchoSuppressed(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprintf("echo before response=%v", race), func(t *testing.T) {
			d := newFakeDaemon(t)
			c := testChannel(t, d, AccessConfig{NoteToSelf: true})
			in := startChannel(t, c)
			echo := fmt.Sprintf(`{"sourceNumber":%q,"timestamp":777,"syncMessage":{"sentMessage":{"destinationNumber":%q,"timestamp":777,"message":"[wolf] hi"}}}`, testAccount, testAccount)
			d.setHandler(func(call rpcCall) (any, *rpcError) {
				if race {
					d.push(echo)
					time.Sleep(100 * time.Millisecond) // let the channel see the echo first
				}
				return map[string]any{"timestamp": 777, "results": []map[string]any{{"type": "SUCCESS"}}}, nil
			})
			id, err := c.Send(context.Background(), channels.OutboundMessage{ChatID: testAccount, Text: "hi", Topic: "wolf"})
			if err != nil || id != "777" {
				t.Fatalf("Send = %q, %v", id, err)
			}
			if !race {
				d.push(echo)
			}
			// A genuine note afterwards still gets through, and the echo did not.
			d.push(fmt.Sprintf(`{"sourceNumber":%q,"timestamp":778,"syncMessage":{"sentMessage":{"destinationNumber":%q,"timestamp":778,"message":"real"}}}`, testAccount, testAccount))
			eventually(t, "real note", func() bool { return len(in.all()) >= 1 })
			time.Sleep(20 * time.Millisecond)
			got := in.all()
			if len(got) != 1 || got[0].Text != "real" {
				t.Errorf("delivered %+v, want only the real note", got)
			}
		})
	}
}

func TestReconnect(t *testing.T) {
	t.Run("stream dropped", func(t *testing.T) {
		d := newFakeDaemon(t)
		c := testChannel(t, d, patAccess())
		in := startChannel(t, c)
		d.push(dmEnvelope(1, "before"))
		eventually(t, "first message", func() bool { return len(in.all()) == 1 })

		d.drop <- struct{}{}
		eventually(t, "reconnect", func() bool { return d.connect.Load() >= 2 })
		d.push(dmEnvelope(2, "after"))
		eventually(t, "message after reconnect", func() bool { return len(in.all()) == 2 })
	})
	t.Run("daemon unavailable then recovers", func(t *testing.T) {
		d := newFakeDaemon(t)
		d.status.Store(503)
		c := testChannel(t, d, patAccess())
		in := startChannel(t, c)
		eventually(t, "retries while down", func() bool { return d.connect.Load() >= 3 })
		d.status.Store(0)
		d.push(dmEnvelope(3, "recovered"))
		eventually(t, "message once up", func() bool { return len(in.all()) == 1 })
	})
	t.Run("backoff grows and is capped", func(t *testing.T) {
		d := newFakeDaemon(t)
		d.status.Store(500)
		c := testChannel(t, d, patAccess())
		c.retryInitial, c.retryMax = 10*time.Millisecond, 40*time.Millisecond
		var mu sync.Mutex
		var waits []time.Duration
		c.sleep = func(ctx context.Context, w time.Duration) error {
			mu.Lock()
			waits = append(waits, w)
			n := len(waits)
			mu.Unlock()
			if n >= 6 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}
		startChannel(t, c)
		eventually(t, "6 waits", func() bool { mu.Lock(); defer mu.Unlock(); return len(waits) >= 6 })
		mu.Lock()
		defer mu.Unlock()
		for i, base := range []time.Duration{10, 20, 40, 40, 40} {
			base *= time.Millisecond
			if waits[i] < base || waits[i] > base+base/4 {
				t.Errorf("wait %d = %v, want %v plus <=20%% jitter", i, waits[i], base)
			}
		}
	})
}

func TestStartTolerance(t *testing.T) {
	d := newFakeDaemon(t)
	url := d.srv.URL
	d.srv.Close() // nothing is listening
	c := New("signal", Config{HTTPURL: url, Account: testAccount, Access: patAccess()}, quietLogger())
	c.retryInitial, c.retryMax = time.Hour, time.Hour
	if err := c.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start with daemon down: %v", err)
	}
	if err := c.Start(context.Background(), nil); err == nil {
		t.Error("second Start should fail")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := c.Start(context.Background(), nil); err == nil {
		t.Error("Start after Close should fail")
	}
}

func TestCloseStopsStream(t *testing.T) {
	d := newFakeDaemon(t)
	c := testChannel(t, d, patAccess())
	startChannel(t, c)
	eventually(t, "connected", func() bool { return d.connect.Load() == 1 })
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return while the stream was open")
	}
}

func TestSend(t *testing.T) {
	ok := func(ts int) (any, *rpcError) {
		return map[string]any{"timestamp": ts, "results": []map[string]any{{"type": "SUCCESS"}}}, nil
	}
	tests := []struct {
		name   string
		msg    channels.OutboundMessage
		plain  bool
		reply  func(rpcCall) (any, *rpcError)
		wantID string
		err    string
		check  func(t *testing.T, calls []rpcCall)
	}{
		{
			name: "dm with topic prefix", msg: channels.OutboundMessage{ChatID: pat, Text: "wolf is up", Topic: "wolf"},
			reply: func(rpcCall) (any, *rpcError) { return ok(1700000000123) }, wantID: "1700000000123",
			check: func(t *testing.T, calls []rpcCall) {
				p := calls[0].Params
				if p["message"] != "[wolf] wolf is up" || p["account"] != testAccount || fmt.Sprint(p["recipient"]) != "["+pat+"]" {
					t.Errorf("params = %v", p)
				}
				if _, has := p["groupId"]; has {
					t.Error("dm must not set groupId")
				}
			},
		},
		{
			name: "markdown becomes text styles", msg: channels.OutboundMessage{ChatID: pat, Text: "a **b** and `c`"},
			reply: func(rpcCall) (any, *rpcError) { return ok(1) }, wantID: "1",
			check: func(t *testing.T, calls []rpcCall) {
				p := calls[0].Params
				if p["message"] != "a b and c" || fmt.Sprint(p["textStyles"]) != "[2:1:BOLD 8:1:MONOSPACE]" {
					t.Errorf("params = %v", p)
				}
			},
		},
		{
			name: "single style uses textStyle", msg: channels.OutboundMessage{ChatID: pat, Text: "**b**"},
			reply: func(rpcCall) (any, *rpcError) { return ok(1) }, wantID: "1",
			check: func(t *testing.T, calls []rpcCall) {
				if p := calls[0].Params; p["textStyle"] != "0:1:BOLD" || p["textStyles"] != nil {
					t.Errorf("params = %v", p)
				}
			},
		},
		{
			name: "plain_text sends markdown untouched", plain: true, msg: channels.OutboundMessage{ChatID: pat, Text: "**b**"},
			reply: func(rpcCall) (any, *rpcError) { return ok(1) }, wantID: "1",
			check: func(t *testing.T, calls []rpcCall) {
				if p := calls[0].Params; p["message"] != "**b**" || p["textStyle"] != nil || p["textStyles"] != nil {
					t.Errorf("params = %v", p)
				}
			},
		},
		{
			name: "group target", msg: channels.OutboundMessage{ChatID: "group:R3JvdXA=", Text: "hi"},
			reply: func(rpcCall) (any, *rpcError) { return ok(2) }, wantID: "2",
			check: func(t *testing.T, calls []rpcCall) {
				p := calls[0].Params
				if p["groupId"] != "R3JvdXA=" || p["recipient"] != nil {
					t.Errorf("params = %v", p)
				}
			},
		},
		{
			name: "reply quotes the message", msg: channels.OutboundMessage{ChatID: pat, Text: "ack", ReplyToID: "1699999999000"},
			reply: func(rpcCall) (any, *rpcError) { return ok(3) }, wantID: "3",
			check: func(t *testing.T, calls []rpcCall) {
				p := calls[0].Params
				if fmt.Sprint(p["quoteTimestamp"]) != "1.699999999e+12" && fmt.Sprint(p["quoteTimestamp"]) != "1699999999000" || p["quoteAuthor"] != pat {
					t.Errorf("params = %v", p)
				}
			},
		},
		{
			name: "non-numeric reply id is ignored", msg: channels.OutboundMessage{ChatID: pat, Text: "ack", ReplyToID: "abc"},
			reply: func(rpcCall) (any, *rpcError) { return ok(4) }, wantID: "4",
			check: func(t *testing.T, calls []rpcCall) {
				if _, has := calls[0].Params["quoteTimestamp"]; has {
					t.Errorf("params = %v", calls[0].Params)
				}
			},
		},
		{name: "empty text sends nothing", msg: channels.OutboundMessage{ChatID: pat, Text: "  "}, check: func(t *testing.T, calls []rpcCall) {
			if len(calls) != 0 {
				t.Errorf("calls = %v", calls)
			}
		}},
		{name: "empty chat", msg: channels.OutboundMessage{Text: "x"}, err: "empty chat id"},
		{
			name: "rpc error", msg: channels.OutboundMessage{ChatID: pat, Text: "x"},
			reply: func(rpcCall) (any, *rpcError) { return nil, &rpcError{Code: -1, Message: "Unregistered user"} },
			err:   "Unregistered user",
		},
		{
			name: "every recipient failed", msg: channels.OutboundMessage{ChatID: pat, Text: "x"},
			reply: func(rpcCall) (any, *rpcError) {
				return map[string]any{"timestamp": 9, "results": []map[string]any{{"type": "UNREGISTERED_FAILURE"}}}, nil
			},
			err: "UNREGISTERED_FAILURE",
		},
		{
			name: "partial group success is a success", msg: channels.OutboundMessage{ChatID: "group:g", Text: "x"},
			reply: func(rpcCall) (any, *rpcError) {
				return map[string]any{"timestamp": 11, "results": []map[string]any{{"type": "NETWORK_FAILURE"}, {"type": "SUCCESS"}}}, nil
			},
			wantID: "11",
		},
		{
			name: "success with no timestamp returns empty id", msg: channels.OutboundMessage{ChatID: pat, Text: "x"},
			reply: func(rpcCall) (any, *rpcError) { return map[string]any{}, nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			if tt.reply != nil {
				d.setHandler(tt.reply)
			}
			c := testChannel(t, d, patAccess())
			c.cfg.PlainText = tt.plain
			id, err := c.Send(context.Background(), tt.msg)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want containing %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
			if tt.check != nil {
				tt.check(t, d.rpcCalls())
			}
		})
	}
}

func TestSendHTTPFailures(t *testing.T) {
	t.Run("daemon down", func(t *testing.T) {
		d := newFakeDaemon(t)
		c := testChannel(t, d, patAccess())
		d.srv.Close()
		if _, err := c.Send(context.Background(), channels.OutboundMessage{ChatID: pat, Text: "x"}); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("context cancelled", func(t *testing.T) {
		d := newFakeDaemon(t)
		c := testChannel(t, d, patAccess())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := c.Send(ctx, channels.OutboundMessage{ChatID: pat, Text: "x"}); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

func TestSendChunking(t *testing.T) {
	d := newFakeDaemon(t)
	var mu sync.Mutex
	next := 1000
	d.setHandler(func(rpcCall) (any, *rpcError) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return map[string]any{"timestamp": next, "results": []map[string]any{{"type": "SUCCESS"}}}, nil
	})
	c := testChannel(t, d, patAccess())
	c.maxLen = 60
	text := strings.Repeat("alpha beta gamma delta\n", 10)
	id, err := c.Send(context.Background(), channels.OutboundMessage{ChatID: pat, Text: text, Topic: "wolf", ReplyToID: "5"})
	if err != nil {
		t.Fatal(err)
	}
	calls := d.rpcCalls()
	if len(calls) < 2 {
		t.Fatalf("calls = %d, want several chunks", len(calls))
	}
	if id != "1001" {
		t.Errorf("id = %q, want the first chunk's timestamp 1001", id)
	}
	for i, call := range calls {
		msg := call.Params["message"].(string)
		if n := len([]rune(msg)); n > 60 {
			t.Errorf("chunk %d has %d chars, limit 60", i, n)
		}
		_, quoted := call.Params["quoteTimestamp"]
		if quoted != (i == 0) {
			t.Errorf("chunk %d quoted=%v; only the first chunk should quote", i, quoted)
		}
	}
	if !strings.HasPrefix(calls[0].Params["message"].(string), "[wolf] ") {
		t.Errorf("first chunk lacks topic prefix: %q", calls[0].Params["message"])
	}
	// Router path: ChunkMessage is what channels.Chunker callers use.
	if pieces := c.ChunkMessage(text); len(pieces) < 2 {
		t.Errorf("ChunkMessage returned %d pieces", len(pieces))
	}
	if pieces := c.ChunkMessage("short"); len(pieces) != 1 || pieces[0] != "short" {
		t.Errorf("short text pieces = %q", pieces)
	}
}

func TestSendChunkFailureStops(t *testing.T) {
	d := newFakeDaemon(t)
	calls := 0
	d.setHandler(func(rpcCall) (any, *rpcError) {
		calls++
		if calls == 2 {
			return nil, &rpcError{Code: -1, Message: "boom"}
		}
		return map[string]any{"timestamp": calls, "results": []map[string]any{{"type": "SUCCESS"}}}, nil
	})
	c := testChannel(t, d, patAccess())
	c.maxLen = 40
	id, err := c.Send(context.Background(), channels.OutboundMessage{ChatID: pat, Text: strings.Repeat("word ", 40)})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if id != "1" {
		t.Errorf("id = %q; the already-sent first chunk's id should still be reported", id)
	}
	if got := len(d.rpcCalls()); got != 2 {
		t.Errorf("rpc calls = %d, want 2 (stop at first failure)", got)
	}
}

func TestSendRateLimit(t *testing.T) {
	limited := func(retry float64) func(rpcCall) (any, *rpcError) {
		n := 0
		return func(rpcCall) (any, *rpcError) {
			n++
			if n == 1 {
				return nil, &rpcError{Code: rpcErrorRateLimit, Message: "rate limited", Data: []byte(fmt.Sprintf(`{"response":{"results":[{"retryAfterSeconds":%v}]}}`, retry))}
			}
			return map[string]any{"timestamp": 42, "results": []map[string]any{{"type": "SUCCESS"}}}, nil
		}
	}
	tests := []struct {
		name      string
		reply     func(rpcCall) (any, *rpcError)
		wantID    string
		wantErr   string
		wantWait  time.Duration
		wantCalls int
	}{
		{"retries after the server's hint", limited(7), "42", "", 7 * time.Second, 2},
		{"legacy message form", func() func(rpcCall) (any, *rpcError) {
			n := 0
			return func(rpcCall) (any, *rpcError) {
				n++
				if n == 1 {
					return nil, &rpcError{Code: -1, Message: "[429] RateLimitException: Retry after 3 seconds"}
				}
				return map[string]any{"timestamp": 43, "results": []map[string]any{{"type": "SUCCESS"}}}, nil
			}
		}(), "43", "", 3 * time.Second, 2},
		{"default wait without a hint", func() func(rpcCall) (any, *rpcError) {
			n := 0
			return func(rpcCall) (any, *rpcError) {
				n++
				if n == 1 {
					return map[string]any{"timestamp": 1, "results": []map[string]any{{"type": "RATE_LIMIT_FAILURE"}}}, nil
				}
				return map[string]any{"timestamp": 44, "results": []map[string]any{{"type": "SUCCESS"}}}, nil
			}
		}(), "44", "", defaultRetryAfter, 2},
		{"hint too long gives up", limited(600), "", "too long", 0, 1},
		{"still limited after the retry", func(rpcCall) (any, *rpcError) {
			return nil, &rpcError{Code: rpcErrorRateLimit, Message: "rate limited"}
		}, "", "rate limited", defaultRetryAfter, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			d.setHandler(tt.reply)
			c := testChannel(t, d, patAccess())
			clock := time.Unix(1_000_000, 0)
			c.pace.now = func() time.Time { return clock }
			var waited []time.Duration
			c.pace.waitFor = func(_ context.Context, w time.Duration) error {
				waited = append(waited, w)
				clock = clock.Add(w)
				return nil
			}
			id, err := c.Send(context.Background(), channels.OutboundMessage{ChatID: pat, Text: "x"})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
			} else if err != nil || id != tt.wantID {
				t.Fatalf("Send = %q, %v; want %q", id, err, tt.wantID)
			}
			if got := len(d.rpcCalls()); got != tt.wantCalls {
				t.Errorf("rpc calls = %d, want %d", got, tt.wantCalls)
			}
			var total time.Duration
			for _, w := range waited {
				total += w
			}
			if total != tt.wantWait {
				t.Errorf("waited %v (%v), want %v", total, waited, tt.wantWait)
			}
		})
	}
}

func TestPacerSpacesAndBacksOff(t *testing.T) {
	clock := time.Unix(0, 0)
	p := newPacer(100 * time.Millisecond)
	p.now = func() time.Time { return clock }
	var waits []time.Duration
	p.waitFor = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }

	for i := 0; i < 3; i++ {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(waits) != 2 || waits[0] != 100*time.Millisecond || waits[1] != 200*time.Millisecond {
		t.Errorf("waits = %v, want [100ms 200ms] (first send is immediate)", waits)
	}
	waits = nil
	clock = clock.Add(time.Second) // quiet period: back to immediate
	p.backoff(5 * time.Second)
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != 5*time.Second {
		t.Errorf("after backoff waits = %v, want [5s]", waits)
	}
}

func TestTyping(t *testing.T) {
	d := newFakeDaemon(t)
	c := testChannel(t, d, patAccess())
	var _ channels.Typer = c
	if err := c.Typing(context.Background(), pat); err != nil {
		t.Fatal(err)
	}
	if err := c.Typing(context.Background(), "group:Zm9v"); err != nil {
		t.Fatal(err)
	}
	calls := d.rpcCalls()
	if len(calls) != 2 || calls[0].Method != "sendTyping" || fmt.Sprint(calls[0].Params["recipient"]) != "["+pat+"]" || calls[1].Params["groupId"] != "Zm9v" {
		t.Errorf("calls = %+v", calls)
	}
}

// TestRegistryRoundTrip drives the channel through the shared Registry the way
// the daemon does: SendTopic out, inbound message in.
func TestRegistryRoundTrip(t *testing.T) {
	d := newFakeDaemon(t)
	c := testChannel(t, d, patAccess())
	reg := channels.NewRegistry()
	if err := reg.Add(c); err != nil {
		t.Fatal(err)
	}
	reg.SetRoutes([]channels.Route{{Topic: "wolf", Channel: "signal", Chat: pat}})
	in := &inbox{}
	if err := reg.Start(context.Background(), in.handle); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	res, err := reg.SendTopic(context.Background(), "wolf", "wolf started")
	if err != nil || len(res) != 1 || res[0].MessageID != "1700000000000" {
		t.Fatalf("SendTopic = %+v, %v", res, err)
	}
	d.push(dmEnvelope(300, "thanks"))
	eventually(t, "inbound", func() bool { return len(in.all()) == 1 })
}
