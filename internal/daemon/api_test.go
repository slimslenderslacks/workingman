package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/audit"
)

// apiTestDir is a short temp dir, since a unix socket path must fit in
// sun_path (~104 bytes) and macOS's $TMPDIR is long.
func apiTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wmd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func apiGet(t *testing.T, sock, path, token string) (int, []byte) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	req, _ := http.NewRequest("GET", "http://localhost"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("API socket %s never appeared", path)
}

// The daemon starts the API in Run, serves the live snapshot model over it,
// and removes the socket on shutdown.
func TestDaemonServesAPIOverUnixSocket(t *testing.T) {
	root := writeFixtureRoot(t)
	wm := apiTestDir(t)
	cfg, err := api.ParseConfig("unix", "", wm)
	if err != nil {
		t.Fatal(err)
	}
	auditBuf := &safeBuf{}
	d, err := newDaemon(t, []string{root}, audit.New(auditBuf),
		WithRuntimeInfo(RuntimeInfo{WorkspaceManager: "stub", Headless: true}),
		WithAPI(cfg),
	)
	if err != nil {
		t.Fatal(err)
	}
	if d.API() == nil {
		t.Fatal("WithAPI(unix) left the daemon without an API")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitForSocket(t, cfg.Listen)
	tok, err := api.ReadToken(cfg.TokenPath)
	if err != nil {
		t.Fatalf("daemon did not create the token file: %v", err)
	}

	alpha := filepath.Join(root, "alpha", "project.yaml")
	d.trackSession(alpha, newStubSession("orch-task-alpha"), agent.TaskAgent, "build", nil)

	code, body := apiGet(t, cfg.Listen, api.Prefix+"/status", tok)
	if code != http.StatusOK {
		t.Fatalf("status: %d %s", code, body)
	}
	var st struct {
		Daemon DaemonInfo  `json:"daemon"`
		API    api.APIInfo `json:"api"`
		Cursor uint64      `json:"cursor"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Daemon.State != DaemonStateRunning || st.Daemon.PID != os.Getpid() || st.Daemon.WorkspaceManager != "stub" ||
		len(st.Daemon.Roots) != 1 || st.Daemon.Roots[0] != root {
		t.Errorf("status daemon = %+v", st.Daemon)
	}
	if st.API.Binding != "unix" || st.API.Listen != cfg.Listen || st.Cursor == 0 || st.Cursor > d.audit.LastSeq() {
		t.Errorf("status api = %+v cursor = %d (last seq %d)", st.API, st.Cursor, d.audit.LastSeq())
	}

	code, body = apiGet(t, cfg.Listen, api.Prefix+"/snapshot?audit_events=3", tok)
	if code != http.StatusOK {
		t.Fatalf("snapshot: %d %s", code, body)
	}
	var snap Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Version != SnapshotVersion || snap.Source != SnapshotSourceDaemon || !snap.LiveStateAvailable {
		t.Errorf("snapshot header = v%d %s live=%v", snap.Version, snap.Source, snap.LiveStateAvailable)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].Task != "build" {
		t.Errorf("snapshot sessions = %+v, want the tracked task session", snap.Sessions)
	}
	if live := findInList(snap.Projects, "alpha").Live; live == nil || len(live.RunningTasks) != 1 {
		t.Errorf("alpha live = %+v", live)
	}
	if len(snap.AuditEvents) != 3 {
		t.Errorf("audit_events=3 gave %d events", len(snap.AuditEvents))
	}
	if strings.Contains(string(body), secretToken) || strings.Contains(string(body), "hunter2") {
		t.Error("snapshot over the API leaks a secret")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(cfg.Listen); !os.IsNotExist(err) {
		t.Errorf("API socket left behind after shutdown: %v", err)
	}
	log := auditBuf.String()
	if !strings.Contains(log, "api_listen ") || !strings.Contains(log, "api_stop ") {
		t.Errorf("audit log lacks api_listen/api_stop:\n%s", log)
	}
}

// A listener that cannot bind is logged as api_listen_error; the daemon runs
// on without it.
func TestDaemonRunsOnWhenAPICannotListen(t *testing.T) {
	root := writeFixtureRoot(t)
	wm := apiTestDir(t)
	blocker := filepath.Join(wm, "api.sock")
	if err := os.WriteFile(blocker, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := api.ParseConfig("unix", blocker, wm)
	if err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(t.TempDir(), "state", "snapshot.json")
	auditBuf := &safeBuf{}
	d, err := newDaemon(t, []string{root}, audit.New(auditBuf), WithAPI(cfg), WithStateFile(stateFile, 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// The snapshot is published after the API start: the daemon got past it.
	waitForFile(t, stateFile)
	if !strings.Contains(auditBuf.String(), "api_listen_error ") {
		t.Errorf("no api_listen_error in the audit log:\n%s", auditBuf.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("daemon exited with %v after an API bind failure", err)
	}
	if b, _ := os.ReadFile(blocker); string(b) != "not a socket" {
		t.Error("the API overwrote a file that is not a socket")
	}
}

func TestWithAPIOffLeavesNoServer(t *testing.T) {
	d, err := newDaemon(t, []string{t.TempDir()}, audit.New(&safeBuf{}), WithAPI(api.Config{Mode: api.ModeOff}))
	if err != nil {
		t.Fatal(err)
	}
	if d.API() != nil {
		t.Error("WithAPI(off) created a server")
	}
}
