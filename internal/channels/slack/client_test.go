package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

func TestClientSelfListPostRepliesReact(t *testing.T) {
	fake := newFakeSlack()
	cl := NewClient(fake)
	cl.c.sleep = func(context.Context, time.Duration) error { return nil }
	ctx := context.Background()

	fake.mu.Lock()
	fake.headerFailures = 2 // retried like the channel's calls
	fake.mu.Unlock()
	p, err := cl.Self(ctx)
	if err != nil || p.UserID != self || p.Name() != "Jim Clark" || p.Username != "jim.clark" {
		t.Fatalf("Self = %+v, %v", p, err)
	}

	page, err := cl.ListConversations(ctx, "public_channel,private_channel,im", "wm", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Conversations) != 2 || page.Cursor != "aW1f" {
		t.Fatalf("page = %+v", page)
	}
	if c := page.Conversations[0]; c.ID != "G0WMJIM01" || c.Name != "#wm-jim" || c.Type != "Private Channel" || c.Archived {
		t.Errorf("first = %+v", c)
	}
	if c := page.Conversations[1]; c.ID != "D0CJVBFKL23" || c.UserID != "U01F5258FDF" {
		t.Errorf("second = %+v", c)
	}
	if a := fake.listArgs[0]; a["name_prefix"] != "wm" || a["exclude_archived"] != true || a["types"] != "public_channel,private_channel,im" {
		t.Errorf("list args = %v", a)
	}

	ch, ts, err := cl.Post(ctx, self, "", TestPrefix+" hi")
	if err != nil || ch != dm || ts == "" {
		t.Fatalf("Post = %q %q %v", ch, ts, err)
	}
	if rs, err := cl.Replies(ctx, ch, ts, ""); err != nil || len(rs) != 0 {
		t.Fatalf("Replies before = %+v, %v", rs, err)
	}
	fake.human(dm, ts, "pong")
	rs, err := cl.Replies(ctx, ch, ts, "")
	if err != nil || len(rs) != 1 || rs[0].Text != "pong" || rs[0].UserID != self {
		t.Fatalf("Replies = %+v, %v", rs, err)
	}
	if err := cl.React(ctx, ch, ts, "white_check_mark"); err != nil {
		t.Fatal(err)
	}
	if m := fake.find(dm, ts); len(m.reactions) != 1 {
		t.Errorf("reactions = %v", m.reactions)
	}
	if missing, err := cl.MissingTools(ctx); err != nil || len(missing) != 0 {
		t.Errorf("MissingTools = %v, %v", missing, err)
	}
}

func TestClientMissingTools(t *testing.T) {
	cl := NewClient(onlyTools{newFakeSlack(), []string{"slack_xaa-" + toolSend}})
	missing, err := cl.MissingTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(missing, ",") != strings.Join(RequiredTools[1:], ",") {
		t.Errorf("missing = %v", missing)
	}
}

// onlyTools narrows a fake's tools/list.
type onlyTools struct {
	*fakeSlack
	names []string
}

func (o onlyTools) ListTools(context.Context) ([]mcpgateway.Tool, error) {
	var out []mcpgateway.Tool
	for _, n := range o.names {
		out = append(out, mcpgateway.Tool{Name: n})
	}
	return out, nil
}

func TestTestPostsAreNotEmitted(t *testing.T) {
	fake := newFakeSlack()
	h := newHarness(t, fake, "")
	fake.mu.Lock()
	// `orch slack test` posts through the MCP from another process.
	fake.msgs[dm] = append(fake.msgs[dm], &fakeMsg{ts: fake.nextTS(), user: self, name: "Jim Clark", text: TestPrefix + " hello", viaMCP: true})
	fake.mu.Unlock()
	fake.human(dm, "", TestPrefix+" typed by hand")
	h.poll()
	if m := h.next(); m.Text != TestPrefix+" typed by hand" {
		t.Errorf("emitted %q", m.Text)
	}
	h.none()
}

func TestParseProfileAndConversations(t *testing.T) {
	p := ParseProfile("User ID: U0CC1K1MFNX\nUsername: jim.clark\nReal Name: \nBot: No\n")
	if p.UserID != self || p.Name() != "jim.clark" {
		t.Errorf("profile = %+v", p)
	}
	page := ParseConversations("## My Channels (showing 1)\n\n### #a &amp; b\n- **ID:** C012\n- **Type:** Public Channel\n- **Archived:** Yes\n")
	if len(page.Conversations) != 1 || page.Conversations[0].Name != "#a & b" || !page.Conversations[0].Archived || page.Cursor != "" {
		t.Errorf("page = %+v", page)
	}
	if got := ParseConversations("nothing here"); len(got.Conversations) != 0 {
		t.Errorf("garbage = %+v", got)
	}
}

func TestRebase(t *testing.T) {
	for _, tc := range []struct{ gw, base, want string }{
		{"http://localhost:3333/gateways/x", "http://host.docker.internal:3333", "http://host.docker.internal:3333/gateways/x"},
		{"http://localhost:3333/gateways/x", "http://localhost:3333", "http://localhost:3333/gateways/x"},
		{"http://127.0.0.1:3333/gateways/x", "http://127.0.0.1:3333/", "http://127.0.0.1:3333/gateways/x"},
		{"http://gw.example/gateways/x", "http://host.docker.internal:3333", "http://gw.example/gateways/x"},
	} {
		if got := rebase(tc.gw, tc.base); got != tc.want {
			t.Errorf("rebase(%q, %q) = %q, want %q", tc.gw, tc.base, got, tc.want)
		}
	}
}

// fakeCodemode is a standalone-codemode root: POST /gateways mints a
// gateway, the first `zero` of them serving slack with no tools, and each
// gateway is a minimal MCP endpoint.
type fakeCodemode struct {
	mu      sync.Mutex
	zero    int
	created []string
	deleted []string
	srv     *httptest.Server
}

func newFakeCodemode(t *testing.T, zero int) *fakeCodemode {
	f := &fakeCodemode{zero: zero}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCodemode) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/gateways":
		id := "gw" + string(rune('0'+len(f.created)))
		f.created = append(f.created, id)
		tools := 4
		if len(f.created) <= f.zero {
			tools = 0
		}
		w.WriteHeader(http.StatusCreated)
		// Answer with a localhost URL, as standalone-codemode does.
		u := strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "url": u + "/gateways/" + id,
			"servers": map[string]any{"slack": map[string]any{"state": "connected", "tools": tools}}})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/gateways/"):
		if r.Header.Get("Mcp-Session-Id") == "" {
			f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/gateways/"))
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/gateways/"):
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

func TestDialReplacesAGatewayServingNoTools(t *testing.T) {
	old := dialRetry
	dialRetry = time.Millisecond
	t.Cleanup(func() { dialRetry = old })

	f := newFakeCodemode(t, 1)
	cl, err := Dial(context.Background(), f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if gw := cl.Gateway(); gw.ID != "gw1" || !gw.Servers[GatewayServer].Serving() {
		t.Errorf("gateway = %+v", gw)
	}
	cl.Close(context.Background())
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.created, ",") != "gw0,gw1" || strings.Join(f.deleted, ",") != "gw0,gw1" {
		t.Errorf("created %v, deleted %v", f.created, f.deleted)
	}
}

func TestDialSettlesAfterAttempts(t *testing.T) {
	old := dialRetry
	dialRetry = time.Millisecond
	t.Cleanup(func() { dialRetry = old })

	f := newFakeCodemode(t, 10)
	cl, err := Dial(context.Background(), f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close(context.Background())
	if st := cl.Gateway().Servers[GatewayServer]; st.Serving() {
		t.Errorf("state = %+v", st)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) != dialAttempts {
		t.Errorf("created %v, want %d", f.created, dialAttempts)
	}
}

func TestReadState(t *testing.T) {
	dir := t.TempDir()
	if st, err := ReadState(dir); err != nil || st.Sent != 0 || len(st.Watches) != 0 {
		t.Fatalf("empty = %+v, %v", st, err)
	}
	fake := newFakeSlack()
	h := newHarness(t, fake, dir)
	id := h.send(request("Which bucket?"))
	h.send(channels.OutboundMessage{ChatID: self, Topic: "wolf", Text: "hi"})
	st, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.ChannelID != dm || st.TopCursor == "" || st.Sent != 2 || len(st.Watches) != 1 || st.Watches[0].ID != id {
		t.Errorf("state = %+v", st)
	}
	if err := os.WriteFile(filepath.Join(dir, WatchFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(dir); err == nil {
		t.Error("corrupt watch.json: no error")
	}
}
