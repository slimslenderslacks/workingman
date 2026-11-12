package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// fakeGateway is a stand-in for standalone-codemode's /gateways API.
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
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

// The terminal-tab path mints a per-sandbox gateway over the task's
// static_mcps, passes the gateway's allow rules to the sandbox creator, and
// hands MCP_URL (loopback rewritten to host.docker.internal) to `sbx exec`.
// A relaunch reuses the live gateway instead of minting another.
func TestSandboxedLaunchGetsMCPGateway(t *testing.T) {
	gw := newFakeGateway(t)
	wsRoot := t.TempDir()
	orchDir := t.TempDir()
	projectPath := filepath.Join(orchDir, "project.yaml")
	_ = os.WriteFile(projectPath, nil, 0o644)

	var specs []SandboxSpec
	launcher := &fakeLauncher{}
	r := &Runner{
		Workspaces:    workspace.NewStub(wsRoot),
		Launcher:      launcher,
		Command:       func(agent.Kind, string) []string { return []string{"claude", "--print", "hi"} },
		SessionsRoot:  filepath.Join(t.TempDir(), "sessions"),
		MCPGatewayURL: gw.srv.URL,
		Sandbox: func(_ context.Context, spec SandboxSpec) error {
			specs = append(specs, spec)
			return nil
		},
	}
	plan := Plan{
		Kind:        agent.TaskAgent,
		Branch:      "feat-x",
		ProjectPath: projectPath,
		TaskPath:    filepath.Join(orchDir, "tasks", "first.yaml"),
		TaskName:    "first",
		StaticMCPs:  []string{"gmail"},
	}
	for i := 0; i < 2; i++ {
		if _, err := r.Start(context.Background(), plan); err != nil {
			t.Fatalf("Start #%d: %v", i, err)
		}
	}

	if len(gw.created) != 1 || len(gw.created[0]) != 1 || gw.created[0][0] != "gmail" {
		t.Fatalf("POST /gateways calls = %v, want exactly one for [gmail]", gw.created)
	}
	port := gw.srv.URL[strings.LastIndex(gw.srv.URL, ":")+1:]
	rules, err := specs[0].MCPGateway.PolicyRules()
	if err != nil {
		t.Fatal(err)
	}
	want := policy.Rule{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "host.docker.internal:" + port}
	if len(rules) == 0 || rules[0] != want {
		t.Errorf("gateway policy rules = %v, want %v first", rules, want)
	}
	wantEnv := "MCP_URL=http://host.docker.internal:" + port + "/gateways/gw-1"
	if v := argValues(launcher.last.Command, "-e"); len(v) != 1 || v[0] != wantEnv {
		t.Errorf("sbx exec -e = %v, want [%s] (cmd %v)", v, wantEnv, launcher.last.Command)
	}
	if hasFlag(launcher.last.Command, "--static-mcp") {
		t.Errorf("--static-mcp must not be sent: %v", launcher.last.Command)
	}

	// The gateway is recorded beside the sessions root, keyed by sandbox.
	store := mcpgateway.Store{Dir: mcpgateway.StateDir(r.SessionsRoot)}
	rec, ok, err := store.Load(specs[0].Name)
	if err != nil || !ok || rec.ID != "gw-1" {
		t.Fatalf("recorded gateway = %+v, %v, %v; want gw-1", rec, ok, err)
	}

	// Gone gateway (standalone-codemode restarted): the next launch mints a
	// fresh one and the sandbox is NOT recreated for it.
	gw.mu.Lock()
	delete(gw.live, "gw-1")
	gw.mu.Unlock()
	if _, err := r.Start(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(gw.created) != 2 {
		t.Fatalf("POST /gateways calls = %d, want a second after the first vanished", len(gw.created))
	}
	if v := argValues(launcher.last.Command, "-e"); len(v) != 1 || !strings.HasSuffix(v[0], "/gateways/gw-2") {
		t.Errorf("sbx exec -e = %v, want the new gateway", v)
	}
}

// With no gateway URL there is no gateway: no MCP_URL, no extra rules.
func TestSandboxedLaunchWithoutMCPGateway(t *testing.T) {
	workingDir := t.TempDir()
	projectPath := filepath.Join(workingDir, "project.yaml")
	_ = os.WriteFile(projectPath, nil, 0o644)
	var spec SandboxSpec
	launcher := &fakeLauncher{}
	r := &Runner{
		Launcher:          launcher,
		Command:           func(agent.Kind, string) []string { return []string{"claude"} },
		SessionsRoot:      t.TempDir(),
		MCPGatewayServers: []string{"gmail"},
		Sandbox:           func(_ context.Context, s SandboxSpec) error { spec = s; return nil },
	}
	if _, err := r.Start(context.Background(), Plan{Kind: agent.PlanningAgent, WorkingDir: workingDir, ProjectPath: projectPath, Branch: "b"}); err != nil {
		t.Fatal(err)
	}
	if rules, _ := spec.MCPGateway.PolicyRules(); len(rules) != 0 {
		t.Errorf("rules = %v, want none", rules)
	}
	if hasFlag(launcher.last.Command, "-e") {
		t.Errorf("unexpected env in %v", launcher.last.Command)
	}
}

// DefaultSandboxCreator POSTs the sandbox's gateway before `sbx create`,
// applies the task's policies and then the gateway's allow rule before
// anything is exec'd, records the gateway for the caller's Ensure to reuse,
// and never sends --static-mcp.
func TestDefaultSandboxCreatorMintsGatewayFirst(t *testing.T) {
	gw := newFakeGateway(t)
	logPath := filepath.Join(t.TempDir(), "events.log")
	appendEvent := func(line string) {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		_, _ = f.WriteString(line + "\n")
	}
	// Wrap the fake gateway so POSTs land in the same ordered event log as
	// the fake sbx's invocations.
	inner := gw.srv.Config.Handler
	gw.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			appendEvent("POST " + r.URL.Path)
		}
		inner.ServeHTTP(w, r)
	})
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"sbx $*\" >> '" + logPath + "'\n" +
		"[ \"$1\" = ls ] && echo '{\"sandboxes\":[]}'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "sbx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := mcpgateway.Store{Dir: t.TempDir()}
	m := mcpgateway.Manager{
		Client:  mcpgateway.Client{BaseURL: gw.srv.URL},
		Store:   store,
		Servers: []string{"github", "logseq", "gmail"},
	}
	taskRule := policy.Rule{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "api.github.com"}
	if err := DefaultSandboxCreator(context.Background(), SandboxSpec{
		Name: "task-x", Workspaces: []string{"/ws"}, Policies: []policy.Rule{taskRule}, MCPGateway: m,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	port := gw.srv.URL[strings.LastIndex(gw.srv.URL, ":")+1:]
	want := []string{
		"sbx ls --json",
		"POST /gateways",
		"sbx create claude --name task-x -e MCP_URL=http://host.docker.internal:" + port + "/gateways/gw-1 /ws",
		"sbx policy allow network --sandbox task-x api.github.com",
		"sbx policy allow network --sandbox task-x host.docker.internal:" + port,
		"sbx policy allow network --sandbox task-x localhost:" + port,
	}
	if got := strings.Split(strings.TrimSpace(string(data)), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(string(data), "--static-mcp") {
		t.Errorf("--static-mcp must not be sent: %s", data)
	}
	if len(gw.created) != 1 || strings.Join(gw.created[0], ",") != "github,logseq,gmail" {
		t.Errorf("POST /gateways servers = %v, want [github logseq gmail]", gw.created)
	}
	rec, ok, err := store.Load("task-x")
	if err != nil || !ok || rec.ID != "gw-1" {
		t.Fatalf("record = %+v, %v, %v; want gw-1", rec, ok, err)
	}
	// The caller's Ensure reuses the gateway minted at create time.
	got, err := m.Ensure(context.Background(), "task-x")
	if err != nil || got.ID != "gw-1" || got.SandboxURL != "http://host.docker.internal:"+port+"/gateways/gw-1" {
		t.Errorf("Ensure = %+v, %v; want gw-1 at host.docker.internal", got, err)
	}
	if len(gw.created) != 1 {
		t.Errorf("POST /gateways calls = %d, want 1", len(gw.created))
	}
}

// Reusing an existing sandbox (same mounts) re-applies only the gateway's
// allow rules — no create, no POST: the caller's Ensure handles MCP_URL.
func TestDefaultSandboxCreatorReuseReappliesGatewayRules(t *testing.T) {
	gw := newFakeGateway(t)
	logPath := filepath.Join(t.TempDir(), "sbx.log")
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"sbx $*\" >> '" + logPath + "'\n" +
		"[ \"$1\" = ls ] && echo '{\"sandboxes\":[{\"name\":\"task-x\",\"workspaces\":[\"/ws\"]}]}'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "sbx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	m := mcpgateway.Manager{
		Client:  mcpgateway.Client{BaseURL: gw.srv.URL},
		Store:   mcpgateway.Store{Dir: t.TempDir()},
		Servers: []string{"github"},
	}
	if err := DefaultSandboxCreator(context.Background(), SandboxSpec{Name: "task-x", Workspaces: []string{"/ws"}, MCPGateway: m}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(logPath)
	port := gw.srv.URL[strings.LastIndex(gw.srv.URL, ":")+1:]
	want := "sbx ls --json\n" +
		"sbx policy allow network --sandbox task-x host.docker.internal:" + port + "\n" +
		"sbx policy allow network --sandbox task-x localhost:" + port + "\n"
	if string(data) != want {
		t.Errorf("sbx calls =\n%s\nwant\n%s", data, want)
	}
	if len(gw.created) != 0 {
		t.Errorf("POST /gateways on reuse = %v, want none", gw.created)
	}
}
