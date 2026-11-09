package acpwrapper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

// fakeGateway stands in for standalone-codemode's /gateways API.
type fakeGateway struct {
	mu      sync.Mutex
	srv     *httptest.Server
	created [][]string
	deleted []string
	live    map[string]bool
	n       int
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{live: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /gateways", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Servers []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.n++
		id := "gw-" + string(rune('0'+g.n))
		g.created = append(g.created, body.Servers)
		g.live[id] = true
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "url": "http://" + r.Host + "/gateways/" + id})
	})
	mux.HandleFunc("/gateways/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		g.mu.Lock()
		defer g.mu.Unlock()
		if !g.live[id] {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			delete(g.live, id)
			g.deleted = append(g.deleted, id)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) port() string { return g.srv.URL[strings.LastIndex(g.srv.URL, ":")+1:] }

func gatewayConfig(t *testing.T, g *fakeGateway, servers ...string) Config {
	return Config{
		SessionID:   "s",
		SandboxName: "acp-s",
		KitPath:     "/kits/acp",
		SbxPath:     "sbx",
		Workspaces:  []string{"/repo"},
		MCPGateway: mcpgateway.Manager{
			Client:  mcpgateway.Client{BaseURL: g.srv.URL},
			Store:   mcpgateway.Store{Dir: t.TempDir()},
			Servers: servers,
		},
	}
}

// A new sandbox is created with no --static-mcp flags and gets the gateway's
// allow rules after the task's own policies; ensureMCPGateway then mints a
// gateway over the requested servers, records it, writes MCP_URL into the
// sandbox's env file, and execArgs passes it as -e MCP_URL.
func TestCreateSandboxWithMCPGateway(t *testing.T) {
	g := newFakeGateway(t)
	c := gatewayConfig(t, g, "gmail", "github")
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	wantCreate := []string{"sbx", "create", "claude", "--name", "acp-s", "--kit", "/kits/acp", "/repo"}
	if !reflect.DeepEqual(f.calls[1], wantCreate) {
		t.Errorf("create = %v, want %v", f.calls[1], wantCreate)
	}
	wantPolicies := [][]string{
		{"sbx", "policy", "allow", "network", "--sandbox", "acp-s", "host.docker.internal:" + g.port()},
		{"sbx", "policy", "allow", "network", "--sandbox", "acp-s", "localhost:" + g.port()},
	}
	if !reflect.DeepEqual(f.calls[2:], wantPolicies) {
		t.Errorf("policy calls = %v, want %v", f.calls[2:], wantPolicies)
	}
	if len(g.created) != 0 {
		t.Errorf("ensureSandbox must not mint the gateway itself: %v", g.created)
	}

	f.calls = nil
	c.mcpURL = ensureMCPGateway(context.Background(), f.run, c)
	wantURL := "http://host.docker.internal:" + g.port() + "/gateways/gw-1"
	if c.mcpURL != wantURL {
		t.Fatalf("mcpURL = %q, want %q", c.mcpURL, wantURL)
	}
	if !reflect.DeepEqual(g.created, [][]string{{"gmail", "github"}}) {
		t.Errorf("POST /gateways = %v", g.created)
	}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0][1:], mcpgateway.PersistEnvArgs("acp-s", wantURL)) {
		t.Errorf("env-file call = %v", f.calls)
	}
	if rec, ok, _ := c.MCPGateway.Store.Load("acp-s"); !ok || rec.ID != "gw-1" || rec.SandboxURL != wantURL {
		t.Errorf("record = %+v, %v", rec, ok)
	}
	args := c.execArgs()
	if i := slices.Index(args, "MCP_URL="+wantURL); i < 1 || args[i-1] != "-e" {
		t.Errorf("execArgs = %v, want -e MCP_URL=%s", args, wantURL)
	}
}

// Reusing an existing sandbox keeps a live gateway (no new POST) and mints a
// replacement — without recreating the sandbox — when the old one is gone.
func TestEnsureMCPGatewayReuseAndReplace(t *testing.T) {
	g := newFakeGateway(t)
	c := gatewayConfig(t, g, "gmail")
	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-s","workspaces":["/repo"]}]}`}

	first := ensureMCPGateway(context.Background(), f.run, c)
	if again := ensureMCPGateway(context.Background(), f.run, c); again != first || len(g.created) != 1 {
		t.Fatalf("reuse: url %q vs %q, %d POSTs; want same url, 1 POST", again, first, len(g.created))
	}

	g.mu.Lock()
	delete(g.live, "gw-1")
	g.mu.Unlock()
	f.calls = nil
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if call[1] == "rm" || call[1] == "create" {
			t.Errorf("a vanished gateway must not recreate the sandbox: %v", f.calls)
		}
	}
	if got := ensureMCPGateway(context.Background(), f.run, c); !strings.HasSuffix(got, "/gateways/gw-2") {
		t.Errorf("after the gateway vanished: url %q, want a fresh gw-2", got)
	}

	// A changed server set also mints a new gateway, deleting the old one.
	c.MCPGateway.Servers = []string{"gmail", "linear"}
	if got := ensureMCPGateway(context.Background(), f.run, c); !strings.HasSuffix(got, "/gateways/gw-3") {
		t.Errorf("after the server set changed: url %q, want gw-3", got)
	}
	if !slices.Contains(g.deleted, "gw-2") {
		t.Errorf("deleted = %v, want the superseded gw-2", g.deleted)
	}
}

// Recreating a sandbox (workspace drift) and removing it on exit both
// DELETE its recorded gateway, so neither leaks one.
func TestSandboxRemovalReleasesMCPGateway(t *testing.T) {
	g := newFakeGateway(t)
	c := gatewayConfig(t, g, "gmail")
	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-s","workspaces":["/other"]}]}`}
	ensureMCPGateway(context.Background(), f.run, c)
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.deleted, []string{"gw-1"}) {
		t.Errorf("after recreate: deleted = %v, want [gw-1]", g.deleted)
	}
	if _, ok, _ := c.MCPGateway.Store.Load("acp-s"); ok {
		t.Error("record survived the recreate")
	}

	ensureMCPGateway(context.Background(), f.run, c)
	removeSandboxOnExit(context.Background(), f.run, c, false)
	if !reflect.DeepEqual(g.deleted, []string{"gw-1", "gw-2"}) {
		t.Errorf("after exit: deleted = %v, want [gw-1 gw-2]", g.deleted)
	}

	// A kept sandbox keeps its gateway.
	ensureMCPGateway(context.Background(), f.run, c)
	c.SaveSandbox = true
	removeSandboxOnExit(context.Background(), f.run, c, false)
	if len(g.deleted) != 2 {
		t.Errorf("kept sandbox lost its gateway: deleted = %v", g.deleted)
	}
}

// Sweep releases the gateways of recorded sandboxes sbx no longer lists.
func TestSweepMCPGateways(t *testing.T) {
	g := newFakeGateway(t)
	c := gatewayConfig(t, g, "gmail")
	other := c
	other.SandboxName = "acp-gone"
	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-s","workspaces":["/repo"]}]}`}
	ensureMCPGateway(context.Background(), f.run, other)
	ensureMCPGateway(context.Background(), f.run, c)
	// Age the records past the grace.
	for _, name := range []string{"acp-s", "acp-gone"} {
		rec, _, _ := c.MCPGateway.Store.Load(name)
		rec.CreatedAt = time.Now().Add(-2 * mcpgateway.SweepGrace)
		_ = c.MCPGateway.Store.Save(rec)
	}
	sweepMCPGateways(context.Background(), f.run, c)
	if !reflect.DeepEqual(g.deleted, []string{"gw-1"}) {
		t.Errorf("deleted = %v, want only the gone sandbox's gw-1", g.deleted)
	}
}

// No gateway URL: no gateway calls, no extra policy rules, no MCP_URL.
func TestMCPGatewayDisabled(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`}
	c := Config{SandboxName: "acp-s", KitPath: "/kits/acp", SbxPath: "sbx", Workspaces: []string{"/repo"},
		MCPGateway: mcpgateway.Manager{Servers: []string{"gmail"}}}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Errorf("calls = %v, want just ls + create", f.calls)
	}
	if got := ensureMCPGateway(context.Background(), f.run, c); got != "" {
		t.Errorf("mcpURL = %q, want empty", got)
	}
	for _, a := range c.execArgs() {
		if strings.HasPrefix(a, "MCP_URL=") {
			t.Errorf("execArgs carries %s with the gateway disabled", a)
		}
	}
}
