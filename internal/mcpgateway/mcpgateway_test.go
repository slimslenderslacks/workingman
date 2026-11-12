package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
)

func TestSandboxURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:3333/gateways/abc":     "http://host.docker.internal:3333/gateways/abc",
		"http://127.0.0.1:3333/gateways/abc":     "http://host.docker.internal:3333/gateways/abc",
		"http://[::1]:3333/gateways/abc":         "http://host.docker.internal:3333/gateways/abc",
		"http://localhost/gateways/abc":          "http://host.docker.internal/gateways/abc",
		"https://gw.example.com:8443/gateways/x": "https://gw.example.com:8443/gateways/x",
	} {
		got, err := SandboxURL(in)
		if err != nil || got != want {
			t.Errorf("SandboxURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := SandboxURL("/no/host"); err == nil {
		t.Error("SandboxURL without a host: want an error")
	}
}

func TestPolicyRules(t *testing.T) {
	allow := func(r string) policy.Rule {
		return policy.Rule{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: r}
	}
	got, err := PolicyRules("http://localhost:3333")
	if want := []policy.Rule{allow("host.docker.internal:3333"), allow("localhost:3333")}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("loopback: %v, %v; want %v", got, err, want)
	}
	got, err = PolicyRules("https://gw.example.com")
	if want := []policy.Rule{allow("gw.example.com:443")}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("remote: %v, %v; want %v", got, err, want)
	}
}

func TestClientCreateDeleteAlive(t *testing.T) {
	mux := http.NewServeMux()
	var lastBody map[string]any
	mux.HandleFunc("POST /gateways", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Servers []string }
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		lastBody = nil
		_ = json.Unmarshal(raw, &lastBody)
		if len(body.Servers) == 1 && body.Servers[0] == "nope" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown_servers", "unknown": []string{"nope"}})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Gateway{ID: "abc", URL: "http://" + r.Host + "/gateways/abc"})
	})
	mux.HandleFunc("/gateways/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "abc" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := Client{BaseURL: srv.URL + "/"}
	ctx := context.Background()

	gw, err := c.Create(ctx, []string{"gmail"}, false)
	if err != nil || gw.ID != "abc" || gw.URL != srv.URL+"/gateways/abc" {
		t.Fatalf("Create = %+v, %v", gw, err)
	}
	if _, sent := lastBody["dynamic"]; sent {
		t.Errorf("non-dynamic Create sent %v, want no dynamic key", lastBody)
	}
	if _, err := c.Create(ctx, []string{"gmail"}, true); err != nil || lastBody["dynamic"] != true {
		t.Errorf("dynamic Create sent %v (err %v), want \"dynamic\": true", lastBody, err)
	}
	var unknown *UnknownServersError
	if _, err := c.Create(ctx, []string{"nope"}, false); !errors.As(err, &unknown) || unknown.Names[0] != "nope" {
		t.Errorf("Create unknown = %v, want *UnknownServersError", err)
	}
	if _, err := c.Create(ctx, nil, false); err == nil {
		t.Error("Create with no servers: want an error")
	}
	if _, err := c.Create(ctx, nil, true); err != nil {
		t.Errorf("dynamic Create with no servers: %v", err)
	}
	if srv, ok := lastBody["servers"].([]any); !ok || len(srv) != 0 || lastBody["dynamic"] != true {
		t.Errorf("empty dynamic Create sent %v, want \"servers\": [] and \"dynamic\": true", lastBody)
	}
	if ok, err := c.Alive(ctx, "abc"); !ok || err != nil {
		t.Errorf("Alive(abc) = %v, %v", ok, err)
	}
	if ok, err := c.Alive(ctx, "gone"); ok || err != nil {
		t.Errorf("Alive(gone) = %v, %v", ok, err)
	}
	if err := c.Delete(ctx, "abc"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := c.Delete(ctx, "gone"); err != nil {
		t.Errorf("Delete of a gone gateway (404) must not fail: %v", err)
	}
}

// Mint drops catalog-unknown names (a task's static_mcps may name servers
// the catalog lacks) rather than failing the whole gateway.
func TestManagerMintDropsUnknownServers(t *testing.T) {
	var posted [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Servers []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, body.Servers)
		for _, s := range body.Servers {
			if s == "web-search" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown_servers", "unknown": []string{"web-search"}})
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Gateway{ID: "x", URL: "http://localhost:1/gateways/x"})
	}))
	defer srv.Close()
	m := Manager{Client: Client{BaseURL: srv.URL}, Store: Store{Dir: t.TempDir()}, Servers: []string{"gmail", "web-search"}}
	rec, err := m.Mint(context.Background(), "sb")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.Servers, []string{"gmail"}) || !reflect.DeepEqual(rec.Requested, []string{"gmail", "web-search"}) {
		t.Errorf("record servers = %v requested = %v", rec.Servers, rec.Requested)
	}
	if rec.SandboxURL != "http://host.docker.internal:1/gateways/x" {
		t.Errorf("SandboxURL = %q", rec.SandboxURL)
	}
	if len(posted) != 2 {
		t.Errorf("POSTs = %v, want the retry without web-search", posted)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if _, ok, err := s.Load("sb"); ok || err != nil {
		t.Fatalf("empty Load = %v, %v", ok, err)
	}
	rec := Record{Sandbox: "sb", ID: "abc", Servers: []string{"gmail"}}
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load("sb")
	if err != nil || !ok || got.ID != "abc" {
		t.Errorf("Load = %+v, %v, %v", got, ok, err)
	}
	if list, _ := s.List(); len(list) != 1 {
		t.Errorf("List = %v", list)
	}
	if err := s.Remove("sb"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("sb"); err != nil {
		t.Errorf("second Remove: %v", err)
	}
	if err := s.Save(Record{Sandbox: "../x"}); err == nil {
		t.Error("Save with a path-like sandbox name: want an error")
	}
}

// Prepare mints without recording; a disabled manager or a failed mint yields
// a zero Record, which Save and Discard ignore.
func TestManagerPrepare(t *testing.T) {
	if rec := (Manager{Servers: []string{"gmail"}}).Prepare(context.Background(), "sb"); rec.ID != "" {
		t.Errorf("disabled Prepare = %+v, want zero", rec)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Gateway{ID: "x", URL: "http://localhost:1/gateways/x"})
	}))
	defer srv.Close()
	m := Manager{Client: Client{BaseURL: srv.URL}, Store: Store{Dir: t.TempDir()}, Servers: []string{"gmail"}, Log: io.Discard}
	rec := m.Prepare(context.Background(), "sb")
	if rec.ID != "x" {
		t.Fatalf("Prepare = %+v, want gateway x", rec)
	}
	if _, ok, _ := m.Store.Load("sb"); ok {
		t.Error("Prepare must not record the gateway")
	}
	if err := m.Save(Record{Sandbox: "sb"}); err != nil {
		t.Errorf("Save(zero) = %v", err)
	}
	if _, ok, _ := m.Store.Load("sb"); ok {
		t.Error("Save(zero) wrote a record")
	}
	down := Manager{Client: Client{BaseURL: "http://127.0.0.1:1"}, Servers: []string{"gmail"}, Log: io.Discard}
	if rec := down.Prepare(context.Background(), "sb"); rec.ID != "" {
		t.Errorf("Prepare with the gateway down = %+v, want zero", rec)
	}
}

// A reused record's MCP_URL is re-derived from its host-side URL, so a record
// whose stored sandbox_url still names localhost is rewritten, not trusted.
func TestManagerReusableRewritesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed) // live
	}))
	defer srv.Close()
	m := Manager{Client: Client{BaseURL: srv.URL}, Store: Store{Dir: t.TempDir()}, Servers: []string{"gmail"}, Log: io.Discard}
	stale := Record{
		Sandbox: "sb", BaseURL: srv.URL, ID: "x",
		URL:        "http://localhost:3333/gateways/x",
		SandboxURL: "http://localhost:3333/gateways/x",
		Requested:  []string{"gmail"}, Servers: []string{"gmail"},
	}
	if err := m.Store.Save(stale); err != nil {
		t.Fatal(err)
	}
	rec, ok := m.Reusable(context.Background(), "sb")
	if !ok {
		t.Fatalf("Reusable = %+v, false; want true", rec)
	}
	if want := "http://host.docker.internal:3333/gateways/x"; rec.SandboxURL != want {
		t.Errorf("SandboxURL = %q, want %q", rec.SandboxURL, want)
	}
}

func TestCreateEnvArgs(t *testing.T) {
	if got := CreateEnvArgs(Record{}); got != nil {
		t.Errorf("CreateEnvArgs(zero) = %v, want nil", got)
	}
	got := CreateEnvArgs(Record{SandboxURL: "http://host.docker.internal:3333/gateways/x"})
	want := []string{"-e", "MCP_URL=http://host.docker.internal:3333/gateways/x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CreateEnvArgs = %v, want %v", got, want)
	}
}

// The env-file script must work for the sandbox's agent user, who owns the
// file but not /etc: an in-place sed (`sed -i`) fails there creating its
// temporary file. Run it against a file in a read-only directory, twice,
// and check the MCP_URL line is replaced and the other lines kept.
func TestPersistEnvArgsRewritesInPlace(t *testing.T) {
	etc := filepath.Join(t.TempDir(), "etc")
	if err := os.Mkdir(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(etc, "sandbox-persistent.sh")
	if err := os.WriteFile(f, []byte("export FOO=1\nexport MCP_URL='old'\nexport BAR=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(etc, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(etc, 0o755) })

	for _, u := range []string{"http://first", "http://second"} {
		args := PersistEnvArgs("sb", u)
		script := strings.ReplaceAll(args[5], EnvFile, f)
		if out, err := exec.Command("sh", "-c", script, "sh", u).CombinedOutput(); err != nil {
			t.Fatalf("script: %v: %s", err, out)
		}
	}
	got, _ := os.ReadFile(f)
	if want := "export FOO=1\nexport BAR=2\nexport MCP_URL='http://second'\n"; string(got) != want {
		t.Errorf("env file = %q, want %q", got, want)
	}
}

// POST /gateways answers only once every backend has attached, which can take
// tens of seconds (gmail through its proxy takes ~30s), so Create allows far
// more than the 15s every other call gets.
func TestCreateTimeoutAllowsSlowBackends(t *testing.T) {
	if createTimeout < 60*time.Second || createTimeout <= defaultTimeout {
		t.Errorf("createTimeout = %s, want >= 60s (standalone-codemode's per-backend attach budget) and > defaultTimeout %s", createTimeout, defaultTimeout)
	}
	if got := (Client{}).createClient().Timeout; got != createTimeout {
		t.Errorf("Create's client timeout = %s, want %s", got, createTimeout)
	}
	if got := (Client{}).httpClient().Timeout; got != defaultTimeout {
		t.Errorf("other calls' client timeout = %s, want %s", got, defaultTimeout)
	}
}

// A dynamic manager is enabled with no servers; a static one is not.
func TestManagerEnabledEmptyDynamic(t *testing.T) {
	c := Client{BaseURL: "http://localhost:3333"}
	if (Manager{Client: c}).Enabled() {
		t.Error("static manager with no servers must be disabled")
	}
	if !(Manager{Client: c, Dynamic: true}).Enabled() {
		t.Error("dynamic manager with no servers must be enabled")
	}
	if (Manager{Dynamic: true}).Enabled() {
		t.Error("no base URL must disable the manager")
	}
}

// Mint reports every requested server the new gateway does not serve, and a
// missing dynamic, so a sandbox started without its MCPs is visible in the log.
func TestManagerMintReportsGaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "url": "http://localhost:1/gateways/x", "dynamic": false,
			"servers": map[string]any{
				"github": map[string]any{"state": "connected", "tools": 40},
				"gmail":  map[string]any{"state": "needs_auth", "tools": 0},
				"miro":   map[string]any{"state": "deferred", "tools": 0, "error": "dial tcp: connection refused"},
				"linear": map[string]any{"state": "connected", "tools": 0},
			},
		})
	}))
	defer srv.Close()
	var log strings.Builder
	m := Manager{Client: Client{BaseURL: srv.URL}, Servers: []string{"github", "gmail", "miro", "linear", "notion"}, Dynamic: true, Log: &log}
	if _, err := m.Mint(context.Background(), "sb"); err != nil {
		t.Fatal(err)
	}
	out := log.String()
	for _, want := range []string{
		"gmail: needs_auth, 0 tools (authorize it on the host: gateway authorize gmail)",
		"miro: deferred, 0 tools: dial tcp: connection refused (remote unreachable",
		"linear: connected, 0 tools",
		"notion: not in the gateway",
		"mcp-find/mcp-add: dynamic was requested but the gateway is not dynamic",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "github:") {
		t.Errorf("a serving server must not be reported:\n%s", out)
	}
}

// A gateway that gives everything gets one confirmation line; one too old to
// report server states gets nothing.
func TestManagerMintAllProvidedAndOldGateway(t *testing.T) {
	body := map[string]any{"id": "x", "url": "http://localhost:1/gateways/x", "dynamic": true,
		"servers": map[string]any{"github": map[string]any{"state": "connected", "tools": 3}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	var log strings.Builder
	m := Manager{Client: Client{BaseURL: srv.URL}, Servers: []string{"github"}, Dynamic: true, Log: &log}
	if _, err := m.Mint(context.Background(), "sb"); err != nil {
		t.Fatal(err)
	}
	if out := log.String(); strings.Contains(out, "WARNING") || !strings.Contains(out, "provides every requested server (github) plus mcp-find/mcp-add") {
		t.Errorf("log = %q", out)
	}
	body = map[string]any{"id": "x", "url": "http://localhost:1/gateways/x"}
	log.Reset()
	if _, err := m.Mint(context.Background(), "sb"); err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Errorf("old gateway: log = %q, want nothing", log.String())
	}
}

// WaitReady waits out a gateway that is still starting (refusing, then 503),
// and gives up with an error after the timeout.
func TestManagerWaitReady(t *testing.T) {
	var up atomic.Bool
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/healthz" || !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	var log strings.Builder
	m := Manager{Client: Client{BaseURL: srv.URL}, Servers: []string{"github"}, Log: &log, RetryBase: 5 * time.Millisecond}

	go func() { time.Sleep(30 * time.Millisecond); up.Store(true) }()
	if err := m.WaitReady(context.Background(), "sb", time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if hits.Load() < 2 || !strings.Contains(log.String(), "not ready") || !strings.Contains(log.String(), "ready after") {
		t.Errorf("hits=%d log=%q", hits.Load(), log.String())
	}

	// Already up: no waiting, nothing logged.
	log.Reset()
	if err := m.WaitReady(context.Background(), "sb", time.Second); err != nil || log.Len() != 0 {
		t.Errorf("ready gateway: err=%v log=%q", err, log.String())
	}

	// Never up: error after the timeout.
	up.Store(false)
	if err := m.WaitReady(context.Background(), "sb", 40*time.Millisecond); err == nil || !strings.Contains(err.Error(), "not ready after") {
		t.Errorf("down gateway: err = %v", err)
	}

	// Disabled manager: no wait at all.
	if err := (Manager{}).WaitReady(context.Background(), "sb", time.Second); err != nil {
		t.Errorf("disabled: %v", err)
	}
}
