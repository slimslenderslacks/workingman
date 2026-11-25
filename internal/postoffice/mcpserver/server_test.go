package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/session"
)

const testToken = "test-token"

// env is a post-office served over httptest with a temp store, fake gateway
// records and fake sessions.
type env struct {
	t        *testing.T
	store    *postoffice.Store
	gateways mcpgateway.Store
	sessions session.Store
	srv      *Server
	url      string
	tasks    string

	// skew is added to the clock of the store and the server.
	skew atomic.Int64

	mu   sync.Mutex
	done []postoffice.DoneRecord
}

func (e *env) now() time.Time { return time.Now().Add(time.Duration(e.skew.Load())) }

func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	root := t.TempDir()
	store, err := postoffice.Open(filepath.Join(root, "post-office"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t:        t,
		store:    store,
		gateways: mcpgateway.Store{Dir: filepath.Join(root, "gateways")},
		sessions: session.Store{Root: filepath.Join(root, "sessions")},
		tasks:    filepath.Join(root, "orch", "proj", "tasks"),
	}
	store.Now = e.now
	cfg := Config{
		Now:        e.now,
		Store:      store,
		Resolver:   &Resolver{Gateways: e.gateways, Sessions: e.sessions},
		MaxWait:    3 * time.Second,
		Heartbeat:  100 * time.Millisecond,
		OutboxPoll: 50 * time.Millisecond,
		ExpiryTick: 50 * time.Millisecond,
		Logf:       t.Logf,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.finalized = func(d postoffice.DoneRecord) {
		e.mu.Lock()
		e.done = append(e.done, d)
		e.mu.Unlock()
	}
	e.srv = srv
	ctx, cancel := context.WithCancel(context.Background())
	bg := make(chan struct{})
	go func() { defer close(bg); srv.Run(ctx) }()
	hs := httptest.NewServer(RequireBearer(testToken, srv.Handler()))
	e.url = hs.URL
	t.Cleanup(func() {
		hs.Close()
		cancel()
		<-bg
	})
	return e
}

// agent registers a gateway record for sandbox with servers, and a running
// task session, and returns the gateway id.
func (e *env) agent(sandbox, taskName string, servers ...string) string {
	e.t.Helper()
	id := "gw-" + sandbox
	if err := e.gateways.Save(mcpgateway.Record{Sandbox: sandbox, ID: id, Servers: servers, CreatedAt: time.Now()}); err != nil {
		e.t.Fatal(err)
	}
	taskPath := ""
	if taskName != "" {
		if err := os.MkdirAll(e.tasks, 0o755); err != nil {
			e.t.Fatal(err)
		}
		taskPath = filepath.Join(e.tasks, "00-"+taskName+".yaml")
		if err := os.WriteFile(taskPath, []byte("name: "+taskName+"\n"), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.sessions.Write(session.Session{
		ID: "sess-" + sandbox, SandboxName: sandbox, Status: session.StatusRunning,
		CreatedAt: time.Now(), Kind: "task",
		ProjectPath: filepath.Join(filepath.Dir(e.tasks), "project.yaml"), TaskPath: taskPath,
	}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// headerTransport adds the bearer token and the gateway id to every request.
type headerTransport struct {
	token, gatewayID string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if h.token != "" {
		r.Header.Set("Authorization", "Bearer "+h.token)
	}
	if h.gatewayID != "" {
		r.Header.Set(GatewayIDHeader, h.gatewayID)
	}
	return http.DefaultTransport.RoundTrip(r)
}

type client struct {
	t  *testing.T
	cs *mcp.ClientSession

	mu       sync.Mutex
	progress []string
}

// connect opens an MCP session as the gateway gatewayID ("" sends none).
func (e *env) connect(gatewayID string) *client {
	e.t.Helper()
	c := &client{t: e.t}
	cl := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			c.mu.Lock()
			c.progress = append(c.progress, r.Params.Message)
			c.mu.Unlock()
		},
	})
	cs, err := cl.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   e.url,
		HTTPClient: &http.Client{Transport: headerTransport{token: testToken, gatewayID: gatewayID}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		e.t.Fatalf("connect: %v", err)
	}
	e.t.Cleanup(func() { _ = cs.Close() })
	c.cs = cs
	return c
}

// call runs a tool and returns its text, failing on a protocol error.
func (c *client) call(name string, args map[string]any) (string, map[string]any) {
	c.t.Helper()
	text, sc, err := c.tryCall(context.Background(), name, args, nil)
	if err != nil {
		c.t.Fatalf("%s: %v", name, err)
	}
	return text, sc
}

func (c *client) tryCall(ctx context.Context, name string, args map[string]any, token any) (string, map[string]any, error) {
	p := &mcp.CallToolParams{Name: name, Arguments: args}
	if token != nil {
		p.SetProgressToken(token)
	}
	res, err := c.cs.CallTool(ctx, p)
	if err != nil {
		return "", nil, err
	}
	var parts []string
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	var sc map[string]any
	if res.StructuredContent != nil {
		data, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(data, &sc)
	}
	return strings.Join(parts, "\n"), sc, nil
}

// field returns a "key: value" line's value from a result text.
func field(text, key string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return v
		}
	}
	return ""
}

func wantStatus(t *testing.T, text, want string) {
	t.Helper()
	first, _, _ := strings.Cut(text, "\n")
	if first != "status: "+want {
		t.Fatalf("first line %q, want status: %s\n%s", first, want, text)
	}
}

func (e *env) answer(id, response, via string) {
	e.t.Helper()
	if _, err := e.store.Claim(postoffice.Resolution{
		ID: id, Outcome: postoffice.OutcomeAnswered, Response: response,
		Responder: &postoffice.Responder{Kind: postoffice.ResponderOperator, Via: via},
	}); err != nil {
		e.t.Fatalf("claim: %v", err)
	}
}

func TestToolsListedWithExactSchemas(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxWait = DefaultMaxWait })
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	res, err := c.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		byName[tl.Name] = tl
	}
	slices.Sort(names)
	want := []string{"cancel_request", "check_response", "list_requests", "send_notification", "send_request", "wait_for_response"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	schema := func(name string) map[string]any {
		data, _ := json.Marshal(byName[name].InputSchema)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		return m
	}
	ws := schema("send_request")["properties"].(map[string]any)["wait_seconds"].(map[string]any)
	if ws["maximum"] != 50.0 || ws["default"] != 45.0 || ws["minimum"] != 0.0 {
		t.Errorf("wait_seconds schema = %v", ws)
	}
	if req := schema("send_request")["required"]; len(req.([]any)) != 1 {
		t.Errorf("send_request required = %v", req)
	}
	if !strings.Contains(byName["send_request"].Description, "**To wait for the answer**, call `wait_for_response`") {
		t.Errorf("send_request description lacks the waiting instructions:\n%s", byName["send_request"].Description)
	}
	if !strings.Contains(byName["wait_for_response"].Description, "(max 50)") {
		t.Errorf("wait_for_response description: %s", byName["wait_for_response"].Description)
	}
}

func TestCallerRejections(t *testing.T) {
	e := newEnv(t)
	noPO := e.agent("wm-p-nopo", "nopo", "github") // mcp-add cannot add post-office to Servers
	e.agent("wm-p-ok", "ok", "post-office")
	// A gateway whose sandbox has no live session.
	if err := e.gateways.Save(mcpgateway.Record{Sandbox: "wm-p-gone", ID: "gw-gone", Servers: []string{"post-office"}}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"subject": "s", "body": "b"}
	for _, tc := range []struct {
		name, gateway, want string
	}{
		{"no header", "", "no X-Codemode-Gateway-Id header"},
		{"unknown id", "gw-nope", "unknown gateway"},
		{"not minted with post-office", noPO, "post-office is not enabled for this sandbox (add static_mcps: [post-office] to the task)"},
		{"no live session", "gw-gone", "no live session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := e.connect(tc.gateway)
			_, _, err := c.tryCall(context.Background(), "send_notification", args, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// tools/list is refused only for a gateway not minted with post-office.
	if _, err := e.connect(noPO).cs.ListTools(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("tools/list without post-office: err = %v", err)
	}
	if _, err := e.connect("gw-pending").cs.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("tools/list before the record exists: %v", err)
	}
	if open, _ := e.store.ListOpen(); len(open) != 0 {
		t.Fatalf("rejected calls stored %d messages", len(open))
	}
}

func TestBearerTokenRequired(t *testing.T) {
	e := newEnv(t)
	for _, auth := range []string{"", "Bearer wrong", "test-token"} {
		req, _ := http.NewRequest("POST", e.url, strings.NewReader(`{}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", auth, resp.StatusCode)
		}
	}
}

func TestSendNotification(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-proj-fix-ci", "fix-ci", "post-office"))
	text, sc := c.call("send_notification", map[string]any{"subject": "Tagged v1", "body": "Release **v1** tagged", "recipient": "Operator"})
	wantStatus(t, text, "sent")
	id := field(text, "message_id")
	if sc["message_id"] != id || sc["status"] != "sent" {
		t.Fatalf("structured = %v", sc)
	}
	m, err := e.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != postoffice.KindNotification || m.Recipient != "operator" || m.Subject != "Tagged v1" {
		t.Fatalf("record = %+v", m)
	}
	// The sender comes from the gateway and session, never the arguments.
	want := postoffice.Sender{Agent: "task", Sandbox: "wm-proj-fix-ci", Project: filepath.Join(filepath.Dir(e.tasks), "project.yaml"), Task: "fix-ci", SessionID: "sess-wm-proj-fix-ci"}
	if m.From != want {
		t.Fatalf("from = %+v, want %+v", m.From, want)
	}
	if got := m.From.Display(); got != "task fix-ci (proj)" {
		t.Fatalf("display = %q", got)
	}

	text, _ = c.call("send_notification", map[string]any{"subject": "x", "body": "y", "recipient": "stepup"})
	wantStatus(t, text, "rejected")
	if got := field(text, "reason"); got != `unknown recipient "stepup"; known: operator, step-up` {
		t.Fatalf("reason = %q", got)
	}
	text, _ = c.call("send_notification", map[string]any{"subject": strings.Repeat("x", 201), "body": "y"})
	wantStatus(t, text, "rejected")
	text, _ = c.call("send_notification", map[string]any{"body": "y"})
	wantStatus(t, text, "rejected")
}

func TestSendRequestAnsweredWhileWaiting(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	go func() {
		// Answer once the request is in the inbox.
		for range 200 {
			if open, _ := e.store.ListOpen(); len(open) == 1 {
				e.answer(open[0].ID, "2", postoffice.ViaSlack)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	text, sc := c.call("send_request", map[string]any{
		"body": "Which bucket for the export?", "options": []string{"staging", "prod"}, "wait_seconds": 3,
	})
	wantStatus(t, text, "answered")
	if field(text, "via") != "slack" || field(text, "responder") != "operator" || field(text, "option") != "prod" {
		t.Fatalf("result:\n%s", text)
	}
	if !strings.HasSuffix(text, "\nresponse:\n2") {
		t.Fatalf("response must be last:\n%s", text)
	}
	if sc["response"] != "2" || sc["option"] != "prod" {
		t.Fatalf("structured = %v", sc)
	}
	// It is finalised: out of the inbox, in done/.
	id := field(text, "request_id")
	if _, err := e.store.Get(id); !errors.Is(err, postoffice.ErrNotFound) {
		t.Fatalf("inbox record still there: %v", err)
	}
}

func TestPendingThenWaitForResponse(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	text, _ := c.call("send_request", map[string]any{"body": "Need a decision\nmore", "wait_seconds": 0})
	wantStatus(t, text, "pending")
	id := field(text, "request_id")
	if field(text, "expires_at") == "" || field(text, "delivered_to") != "tui" {
		t.Fatalf("pending result:\n%s", text)
	}
	m, _ := e.store.Get(id)
	if m.Subject != "Need a decision" || m.ExpiresAt.Sub(m.CreatedAt) != 24*time.Hour {
		t.Fatalf("record = %+v", m)
	}

	text, _ = c.call("check_response", map[string]any{"request_id": id})
	wantStatus(t, text, "pending")

	// A bounded wait with nothing happening returns pending.
	start := time.Now()
	text, _ = c.call("wait_for_response", map[string]any{"request_id": id, "timeout_seconds": 1})
	wantStatus(t, text, "pending")
	if d := time.Since(start); d < 900*time.Millisecond || d > 3*time.Second {
		t.Fatalf("waited %s, want about 1s", d)
	}

	// The answer arrives while a wait is blocked; it wakes promptly.
	time.AfterFunc(200*time.Millisecond, func() { e.answer(id, "Use the staging bucket.\nThanks", postoffice.ViaTUI) })
	start = time.Now()
	text, _ = c.call("wait_for_response", map[string]any{"request_id": id, "timeout_seconds": 3})
	wantStatus(t, text, "answered")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("woke after %s", d)
	}
	if !strings.HasSuffix(text, "response:\nUse the staging bucket.\nThanks") || field(text, "via") != "tui" {
		t.Fatalf("result:\n%s", text)
	}
	// Later checks keep returning the answer from done/.
	text, _ = c.call("check_response", map[string]any{"request_id": id})
	wantStatus(t, text, "answered")
}

func TestWaitArgumentLimits(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	text, _ := c.call("send_request", map[string]any{"body": "q", "wait_seconds": 4}) // MaxWait is 3s
	wantStatus(t, text, "rejected")
	text, _ = c.call("send_request", map[string]any{"body": "q", "expires_in_minutes": 0})
	wantStatus(t, text, "rejected")
	text, _ = c.call("send_request", map[string]any{"body": "q", "wait_seconds": 0, "options": []string{"a", strings.Repeat("b", 101)}})
	wantStatus(t, text, "rejected")
	text, _ = c.call("send_request", map[string]any{"body": "q", "wait_seconds": 0})
	id := field(text, "request_id")
	text, _ = c.call("wait_for_response", map[string]any{"request_id": id, "timeout_seconds": 0})
	wantStatus(t, text, "rejected")
	text, _ = c.call("list_requests", map[string]any{"status": "bogus"})
	wantStatus(t, text, "rejected")
}

func TestProgressHeartbeats(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	text, _, err := c.tryCall(context.Background(), "send_request", map[string]any{"body": "q", "wait_seconds": 1}, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, text, "pending")
	c.mu.Lock()
	got := slices.Clone(c.progress)
	c.mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("got %d heartbeats in 1s at 100ms, want several: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "waiting for operator (") {
		t.Fatalf("heartbeat = %q", got[0])
	}
}

func TestClientCancelDoesNotCancelRequest(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	text, _ := c.call("send_request", map[string]any{"body": "q", "wait_seconds": 0})
	id := field(text, "request_id")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := c.tryCall(ctx, "wait_for_response", map[string]any{"request_id": id, "timeout_seconds": 3}, nil); err == nil {
		t.Fatal("interrupted wait returned no error")
	}
	if _, res, err := e.store.Lookup(id); err != nil || res != nil {
		t.Fatalf("request closed by an interrupted wait: %v %v", res, err)
	}
	text, _ = c.call("check_response", map[string]any{"request_id": id})
	wantStatus(t, text, "pending")
}

func TestCancelRequest(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	text, _ := c.call("send_request", map[string]any{"body": "q", "wait_seconds": 0})
	id := field(text, "request_id")
	text, _ = c.call("cancel_request", map[string]any{"request_id": id, "reason": "found it in the docs"})
	wantStatus(t, text, "cancelled")
	if field(text, "reason") != "found it in the docs" {
		t.Fatalf("result:\n%s", text)
	}
	d, ok, _ := e.store.Done(id)
	if !ok || d.Resolution.Outcome != postoffice.OutcomeCancelled {
		t.Fatalf("done = %+v %v", d, ok)
	}

	// An answer that won the race is returned instead.
	text, _ = c.call("send_request", map[string]any{"body": "q2", "wait_seconds": 0})
	id2 := field(text, "request_id")
	e.answer(id2, "yes", postoffice.ViaSignal)
	text, _ = c.call("cancel_request", map[string]any{"request_id": id2})
	wantStatus(t, text, "answered")
	if !strings.HasSuffix(text, "response:\nyes") {
		t.Fatalf("result:\n%s", text)
	}
}

func TestOwnershipChecks(t *testing.T) {
	e := newEnv(t)
	a := e.connect(e.agent("wm-p-a", "a", "post-office"))
	b := e.connect(e.agent("wm-p-b", "b", "post-office"))
	text, _ := a.call("send_request", map[string]any{"body": "q", "wait_seconds": 0})
	id := field(text, "request_id")
	for _, tool := range []string{"wait_for_response", "check_response", "cancel_request"} {
		text, _ := b.call(tool, map[string]any{"request_id": id})
		wantStatus(t, text, "rejected")
		if field(text, "reason") != "not your request" {
			t.Fatalf("%s: %s", tool, text)
		}
	}
	text, _ = b.call("list_requests", nil)
	if strings.Contains(text, id) {
		t.Fatalf("b lists a's request:\n%s", text)
	}
	text, _ = b.call("check_response", map[string]any{"request_id": "nope"})
	wantStatus(t, text, "rejected")
	// A notification id is not a request.
	text, _ = a.call("send_notification", map[string]any{"subject": "s", "body": "b"})
	text, _ = a.call("check_response", map[string]any{"request_id": field(text, "message_id")})
	wantStatus(t, text, "rejected")
	// b cannot read a's message resource either.
	if _, err := b.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: ResourceScheme + id}); err == nil {
		t.Fatal("b read a's message resource")
	}
}

func TestListRequests(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	text, _ := c.call("send_request", map[string]any{"body": "first", "wait_seconds": 0})
	open := field(text, "request_id")
	text, _ = c.call("send_request", map[string]any{"body": "second", "wait_seconds": 0, "recipient": "step-up"})
	closed := field(text, "request_id")
	c.call("cancel_request", map[string]any{"request_id": closed})
	c.call("send_notification", map[string]any{"subject": "n", "body": "n"})

	text, sc := c.call("list_requests", nil)
	wantStatus(t, text, "ok")
	if !strings.Contains(text, "\n"+open+" pending operator ") || strings.Contains(text, closed) {
		t.Fatalf("open list:\n%s", text)
	}
	if rows, _ := sc["requests"].([]any); len(rows) != 1 {
		t.Fatalf("structured requests = %v", sc["requests"])
	}
	text, _ = c.call("list_requests", map[string]any{"status": "closed"})
	if !strings.Contains(text, closed+" cancelled step-up ") || strings.Contains(text, open) {
		t.Fatalf("closed list:\n%s", text)
	}
	text, _ = c.call("list_requests", map[string]any{"status": "all"})
	if !strings.Contains(text, open) || !strings.Contains(text, closed) {
		t.Fatalf("all list:\n%s", text)
	}
}

func TestExpiry(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	text, _ := c.call("send_request", map[string]any{"body": "q", "wait_seconds": 0, "expires_in_minutes": 1})
	id := field(text, "request_id")
	// Move the clock past expiry: the ticker claims and finalises it, and a
	// blocked wait wakes with expired.
	e.skew.Store(int64(2 * time.Minute))
	text, _ = c.call("wait_for_response", map[string]any{"request_id": id, "timeout_seconds": 3})
	wantStatus(t, text, "expired")
	d, ok, _ := e.store.Done(id)
	if !ok || d.Resolution.Outcome != postoffice.OutcomeExpired {
		t.Fatalf("done = %+v %v", d, ok)
	}
}

func TestExpiryTickerAndStartupRecovery(t *testing.T) {
	root := t.TempDir()
	store, err := postoffice.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mk := func(body string, created time.Time) postoffice.Message {
		m, err := postoffice.NewMessage(postoffice.Draft{Kind: postoffice.KindRequest, From: postoffice.Sender{Sandbox: "x"}, Body: body, ExpiresIn: time.Minute}, created)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	overdue := mk("overdue", now.Add(-time.Hour))
	claimed := mk("claimed", now)
	// A claim left behind by a crash before finalisation.
	if _, err := store.Claim(postoffice.Resolution{ID: claimed.ID, Outcome: postoffice.OutcomeAnswered, Response: "ok"}); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: store, Resolver: &Resolver{}, ExpiryTick: time.Hour, OutboxPoll: time.Hour, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bg := make(chan struct{})
	go func() { defer close(bg); srv.Run(ctx) }()
	defer func() { cancel(); <-bg }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a, okA, _ := store.Done(overdue.ID)
		b, okB, _ := store.Done(claimed.ID)
		if okA && okB {
			if a.Resolution.Outcome != postoffice.OutcomeExpired || b.Resolution.Outcome != postoffice.OutcomeAnswered {
				t.Fatalf("outcomes %s, %s", a.Resolution.Outcome, b.Resolution.Outcome)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("startup did not expire and finalise")
}

func TestMessageResource(t *testing.T) {
	e := newEnv(t)
	c := e.connect(e.agent("wm-p-a", "a", "post-office"))
	text, _ := c.call("send_request", map[string]any{"body": "q", "wait_seconds": 0})
	id := field(text, "request_id")
	read := func() map[string]any {
		t.Helper()
		res, err := c.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: ResourceScheme + id})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(res.Contents[0].Text), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := read(); m["id"] != id || m["resolution"] != nil {
		t.Fatalf("open record = %v", m)
	}
	e.answer(id, "done", postoffice.ViaTUI)
	c.call("wait_for_response", map[string]any{"request_id": id, "timeout_seconds": 3})
	if m := read(); m["resolution"].(map[string]any)["outcome"] != "answered" {
		t.Fatalf("done record = %v", m)
	}
	if _, err := c.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: ResourceScheme + "msg_bogus"}); err == nil {
		t.Fatal("bogus id read")
	}
}

func TestSingletonAgentIdentity(t *testing.T) {
	e := newEnv(t)
	// Step-Up has a fixed sandbox; it is identified even without a session.
	if err := e.gateways.Save(mcpgateway.Record{Sandbox: "step-up", ID: "gw-step-up", Servers: []string{"post-office"}}); err != nil {
		t.Fatal(err)
	}
	c := e.connect("gw-step-up")
	text, _ := c.call("send_notification", map[string]any{"subject": "s", "body": "b"})
	m, err := e.store.Get(field(text, "message_id"))
	if err != nil {
		t.Fatal(err)
	}
	if m.From.Agent != "step-up" || m.From.Sandbox != "step-up" || m.From.Display() != "step-up" {
		t.Fatalf("from = %+v", m.From)
	}
}

func TestResolverNewestLiveSessionAndCache(t *testing.T) {
	root := t.TempDir()
	r := &Resolver{
		Gateways: mcpgateway.Store{Dir: filepath.Join(root, "gw")},
		Sessions: session.Store{Root: filepath.Join(root, "sess")},
	}
	now := time.Now()
	r.Now = func() time.Time { return now }
	_ = r.Gateways.Save(mcpgateway.Record{Sandbox: "sb", ID: "g1", Servers: []string{"github", "post-office"}})
	for _, s := range []session.Session{
		{ID: "old", SandboxName: "sb", Status: session.StatusRunning, CreatedAt: now.Add(-2 * time.Hour), Kind: "task"},
		{ID: "newer", SandboxName: "sb", Status: session.StatusRunning, CreatedAt: now.Add(-time.Hour), Kind: "commit"},
		{ID: "newest-exited", SandboxName: "sb", Status: session.StatusExited, CreatedAt: now, Kind: "archive"},
		{ID: "other", SandboxName: "other", Status: session.StatusRunning, CreatedAt: now, Kind: "wolf"},
	} {
		if err := r.Sessions.Write(s); err != nil {
			t.Fatal(err)
		}
	}
	c, err := r.Resolve("g1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Sender.SessionID != "newer" || c.Sender.Agent != "commit" {
		t.Fatalf("sender = %+v", c.Sender)
	}
	// Cached for the TTL, even if the record goes away.
	_ = r.Gateways.Remove("sb")
	if _, err := r.Resolve("g1"); err != nil {
		t.Fatalf("cached: %v", err)
	}
	now = now.Add(DefaultIdentityTTL + time.Second)
	if _, err := r.Resolve("g1"); !errors.Is(err, ErrUnknownGateway) {
		t.Fatalf("after TTL: %v", err)
	}
}

func TestEnsureToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "po", TokenFile)
	tok, created, err := EnsureToken(path, false)
	if err != nil || !created || len(tok) != 64 {
		t.Fatalf("EnsureToken = %q %v %v", tok, created, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	again, created, err := EnsureToken(path, false)
	if err != nil || created || again != tok {
		t.Fatalf("second EnsureToken = %q %v %v", again, created, err)
	}
	rotated, created, err := EnsureToken(path, true)
	if err != nil || !created || rotated == tok {
		t.Fatalf("rotate = %q %v %v", rotated, created, err)
	}
	if got, _ := ReadToken(path); got != rotated {
		t.Fatalf("ReadToken = %q", got)
	}
}
