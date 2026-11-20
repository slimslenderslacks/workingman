package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
)

// fakeCodemode stands in for standalone-codemode's /gateways API over a
// catalog of github and post-office. Each sub-gateway at /gateways/{id}
// forwards MCP to post-office the way the real gateway does for an attached
// backend: with the catalog entry's bearer token and X-Codemode-Gateway-Id
// set to the sub-gateway's own id. It forwards only while post-office is
// attached: named at creation, or mcp-added later (see mcpAdd).
type fakeCodemode struct {
	t     *testing.T
	po    *url.URL
	token string
	srv   *httptest.Server

	mu       sync.Mutex
	n        int
	attached map[string][]string // gateway id -> attached servers
}

func newFakeCodemode(t *testing.T, postOffice, token string) *fakeCodemode {
	t.Helper()
	po, err := url.Parse(postOffice)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCodemode{t: t, po: po, token: token, attached: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /gateways", f.create)
	mux.HandleFunc("/gateways/{id}", f.gateway)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCodemode) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Servers []string `json:"servers"`
		Dynamic bool     `json:"dynamic"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var unknown []string
	for _, s := range body.Servers {
		if s != "github" && s != ServerName {
			unknown = append(unknown, s)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if len(unknown) > 0 {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown_servers", "unknown": unknown})
		return
	}
	f.mu.Lock()
	f.n++
	id := fmt.Sprintf("gw%d", f.n)
	f.attached[id] = slices.Clone(body.Servers)
	f.mu.Unlock()
	states := map[string]mcpgateway.ServerState{}
	for _, s := range body.Servers {
		states[s] = mcpgateway.ServerState{State: "connected", Tools: 1}
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(mcpgateway.Gateway{ID: id, URL: f.srv.URL + "/gateways/" + id, Servers: states, Dynamic: body.Dynamic})
}

func (f *fakeCodemode) gateway(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	servers, ok := f.attached[id]
	if ok && r.Method == http.MethodDelete {
		delete(f.attached, id)
	}
	f.mu.Unlock()
	switch {
	case !ok:
		http.NotFound(w, r)
	case r.Method == http.MethodGet && r.Header.Get("Accept") == "":
		w.WriteHeader(http.StatusOK) // mcpgateway.Client.Alive
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case !slices.Contains(servers, ServerName):
		http.Error(w, "post-office is not attached to this gateway", http.StatusNotFound)
	default:
		(&httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(f.po)
			pr.Out.URL.Path = f.po.Path
			pr.Out.Header.Set("Authorization", "Bearer "+f.token)
			pr.Out.Header.Set(GatewayIDHeader, id)
		}, FlushInterval: -1}).ServeHTTP(w, r)
	}
}

// mcpAdd attaches post-office to a dynamic gateway, as mcp-add would if the
// catalog entry lacked `dynamic_addable: false`.
func (f *fakeCodemode) mcpAdd(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached[id] = append(f.attached[id], ServerName)
}

// launchTask does what the daemon and acp-wrapper do for a task agent's
// sandbox, as far as post-office can see: load the task, mint its gateway
// over the task's static_mcps (else defaults), dynamic as task gateways are
// by default, save the record once "the sandbox exists", and write the
// running session.json.
func launchTask(t *testing.T, e *env, codemode *fakeCodemode, name, yaml string, defaults []string) mcpgateway.Record {
	t.Helper()
	if err := os.MkdirAll(e.tasks, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.tasks, name+".yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	tk, err := task.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	servers := tk.StaticMCPs
	if len(servers) == 0 {
		servers = defaults
	}
	sandbox := "wm-proj-" + name
	m := mcpgateway.Manager{
		Client:  mcpgateway.Client{BaseURL: codemode.srv.URL},
		Store:   e.gateways,
		Servers: servers,
		Dynamic: true,
		Log:     testWriter{t},
	}
	rec, err := m.Mint(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("mint %s: %v", sandbox, err)
	}
	if err := m.Save(rec); err != nil {
		t.Fatal(err)
	}
	if err := e.sessions.Write(session.Session{
		ID: "sess-" + name, SandboxName: sandbox, Status: session.StatusRunning,
		CreatedAt: time.Now(), Kind: "task",
		ProjectPath: filepath.Join(filepath.Dir(e.tasks), "project.yaml"), TaskPath: path,
	}); err != nil {
		t.Fatal(err)
	}
	return rec
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// connectVia opens an MCP session on a sub-gateway's URL, as the agent's
// okta-gateway MCP client does. It sends no gateway id of its own.
func connectVia(t *testing.T, endpoint string) *client {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "sandbox", Version: "0"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: endpoint, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", endpoint, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &client{t: t, cs: cs}
}

// TestStaticMCPsCapabilityEndToEnd: a task with `static_mcps: [post-office]`
// can call send_notification through its gateway, and the message is
// attributed to it; a task without it cannot, even when its dynamic gateway
// has post-office attached by mcp-add.
func TestStaticMCPsCapabilityEndToEnd(t *testing.T) {
	e := newEnv(t)
	codemode := newFakeCodemode(t, e.url, testToken)
	defaults := []string{"github"}

	with := launchTask(t, e, codemode, "ask-operator", "name: ask-operator\nstatus: ready\nstatic_mcps:\n  - post-office\n", defaults)
	if !slices.Contains(with.Servers, ServerName) {
		t.Fatalf("record servers = %v, want post-office", with.Servers)
	}
	c := connectVia(t, with.URL)
	tools, err := c.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tools.Tools, func(tl *mcp.Tool) bool { return tl.Name == "send_notification" }) {
		t.Fatalf("tools/list has no send_notification")
	}
	text, _ := c.call("send_notification", map[string]any{"subject": "hello", "body": "from a task"})
	wantStatus(t, text, "sent")
	m, err := e.store.Get(field(text, "message_id"))
	if err != nil {
		t.Fatal(err)
	}
	if m.From.Sandbox != "wm-proj-ask-operator" || m.From.Task != "ask-operator" || m.From.Agent != "task" {
		t.Fatalf("from = %+v", m.From)
	}

	without := launchTask(t, e, codemode, "plain", "name: plain\nstatus: ready\n", defaults)
	if slices.Contains(without.Servers, ServerName) {
		t.Fatalf("record servers = %v, want no post-office", without.Servers)
	}
	// Not attached: the gateway has nothing to forward to.
	resp, err := http.Post(without.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("post-office through a gateway without it: status %d", resp.StatusCode)
	}
	// Attached by mcp-add: post-office refuses, because the gateway record
	// lists only what it was minted with.
	codemode.mcpAdd(without.ID)
	c = connectVia(t, without.URL)
	if _, err := c.cs.ListTools(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("tools/list after mcp-add: err = %v, want not enabled", err)
	}
	_, _, err = c.tryCall(context.Background(), "send_notification", map[string]any{"subject": "s", "body": "b"}, nil)
	if err == nil || !strings.Contains(err.Error(), ErrNotEnabled.Error()) {
		t.Fatalf("send_notification after mcp-add: err = %v, want %q", err, ErrNotEnabled)
	}
	if open, _ := e.store.ListOpen(); len(open) != 1 {
		t.Fatalf("store has %d open messages, want only the permitted one", len(open))
	}
}
