package acpwrapper

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNormalizeDefaults(t *testing.T) {
	c := Config{
		SessionID:    "sess1",
		KitPath:      "/kits/acp-kit",
		SessionsRoot: "/tmp/sessions",
		Workspaces:   []string{"/repo"},
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got, want := c.SandboxName, "acp-sess1"; got != want {
		t.Errorf("SandboxName = %q, want %q", got, want)
	}
	if got, want := c.SbxPath, "sbx"; got != want {
		t.Errorf("SbxPath = %q, want %q", got, want)
	}
	if got, want := c.SessionDir(), "/tmp/sessions/sess1"; got != want {
		t.Errorf("SessionDir = %q, want %q", got, want)
	}
	if got, want := c.SocketPath(), "/tmp/sessions/sess1/agent.sock"; got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
}

func TestNormalizeDefaultSessionsRoot(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	c := Config{SessionID: "s", KitPath: "k", Workspaces: []string{"/repo"}}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := filepath.Join("/home/test", ".workingman", "sessions")
	if c.SessionsRoot != want {
		t.Errorf("SessionsRoot = %q, want %q", c.SessionsRoot, want)
	}
}

func TestNormalizeSandboxNameSanitizesUnderscores(t *testing.T) {
	c := Config{SessionID: "a_b_c", KitPath: "k", Workspaces: []string{"/repo"}}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	// sbx rejects underscores in sandbox names.
	if c.SandboxName != "acp-a-b-c" {
		t.Errorf("SandboxName = %q, want %q", c.SandboxName, "acp-a-b-c")
	}
}

func TestNormalizeWorkspacesMadeAbsolute(t *testing.T) {
	c := Config{SessionID: "s", KitPath: "k", Workspaces: []string{"relative/dir"}}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !filepath.IsAbs(c.Workspaces[0]) {
		t.Errorf("workspace not absolute: %q", c.Workspaces[0])
	}
}

func TestNormalizeErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no session id", Config{KitPath: "k", Workspaces: []string{"/r"}}, "session id is required"},
		{"blank session id", Config{SessionID: "  ", KitPath: "k", Workspaces: []string{"/r"}}, "session id is required"},
		{"slash session id", Config{SessionID: "a/b", KitPath: "k", Workspaces: []string{"/r"}}, "single path segment"},
		{"dotdot session id", Config{SessionID: "..", KitPath: "k", Workspaces: []string{"/r"}}, "single path segment"},
		{"no kit", Config{SessionID: "s", Workspaces: []string{"/r"}}, "kit path is required"},
		{"no workspace", Config{SessionID: "s", KitPath: "k"}, "at least one workspace is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			err := cfg.normalize()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("normalize() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestExecArgs(t *testing.T) {
	c := Config{
		SessionID:   "s",
		KitPath:     "k",
		SandboxName: "acp-s",
		Workspaces:  []string{"/host/repo", "/host/orch"},
	}
	got := c.execArgs()
	want := []string{"exec", "-w", "/host/repo", "acp-s", "--", "claude-acp-client"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("execArgs() = %v, want %v", got, want)
	}
}

func TestExecArgsNoWorkspace(t *testing.T) {
	c := Config{SandboxName: "acp-s"}
	got := c.execArgs()
	want := []string{"exec", "acp-s", "--", "claude-acp-client"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("execArgs() = %v, want %v", got, want)
	}
}

// fakeSbx records calls and returns canned responses keyed by the first arg.
type fakeSbx struct {
	calls    [][]string
	lsOutput string
	lsErr    error
	failCmd  string // subcommand to fail (e.g. "create")
}

func (f *fakeSbx) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "ls":
		return []byte(f.lsOutput), f.lsErr
	default:
		if f.failCmd != "" && args[0] == f.failCmd {
			return []byte("boom"), errors.New("exit status 1")
		}
		return nil, nil
	}
}

func TestEnsureSandboxCreatesWhenMissing(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`}
	c := Config{SandboxName: "acp-s", KitPath: "/kits/acp", SbxPath: "sbx", Workspaces: []string{"/repo"}}
	if err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	// Expect ls then create with --kit.
	if len(f.calls) != 2 {
		t.Fatalf("expected 2 sbx calls, got %d: %v", len(f.calls), f.calls)
	}
	create := f.calls[1]
	want := []string{"sbx", "create", "claude", "--name", "acp-s", "--kit", "/kits/acp", "/repo"}
	if !reflect.DeepEqual(create, want) {
		t.Errorf("create call = %v, want %v", create, want)
	}
}

func TestEnsureSandboxNoopWhenSameWorkspaces(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-s","workspaces":["/repo"]}]}`}
	c := Config{SandboxName: "acp-s", KitPath: "k", SbxPath: "sbx", Workspaces: []string{"/repo"}}
	if err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected only ls call, got %v", f.calls)
	}
}

func TestEnsureSandboxRecreatesOnDrift(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-s","workspaces":["/old"]}]}`}
	c := Config{SandboxName: "acp-s", KitPath: "k", SbxPath: "sbx", Workspaces: []string{"/repo"}}
	if err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("expected ls, rm, create, got %v", f.calls)
	}
	if f.calls[1][1] != "rm" || f.calls[2][1] != "create" {
		t.Errorf("expected rm then create, got %v", f.calls)
	}
}

func TestEnsureSandboxCreateError(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`, failCmd: "create"}
	c := Config{SandboxName: "acp-s", KitPath: "k", SbxPath: "sbx", Workspaces: []string{"/repo"}}
	err := ensureSandbox(context.Background(), f.run, c)
	if err == nil || !strings.Contains(err.Error(), "sbx create") {
		t.Fatalf("expected create error, got %v", err)
	}
}

func TestSameWorkspaceSet(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{[]string{"/a", "/b"}, []string{"/b", "/a"}, true},
		{[]string{"/a"}, []string{"/a", "/b"}, false},
		{[]string{"/a"}, []string{"/b"}, false},
		{nil, nil, true},
	}
	for _, tt := range tests {
		if got := sameWorkspaceSet(tt.a, tt.b); got != tt.want {
			t.Errorf("sameWorkspaceSet(%v,%v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// scanLine reads one '\n'-terminated frame from r with a deadline guard, used
// by the hub tests to assert a client received a specific whole frame.
func scanLine(t *testing.T, r net.Conn) string {
	t.Helper()
	r.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(r).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return string(line)
}

// newTestHub starts a hub fed by an in-memory ACP client stdio pair. It returns
// the hub, a reader over the client's stdin (what TUIs sent), and a writer to
// the client's stdout (what the hub broadcasts). Closing stdoutW ends run().
func newTestHub(t *testing.T) (h *hub, stdinR *io.PipeReader, stdoutW *io.PipeWriter) {
	t.Helper()
	var stdinW *io.PipeWriter
	var stdoutR *io.PipeReader
	stdinR, stdinW = io.Pipe()
	stdoutR, stdoutW = io.Pipe()
	h = newHub(stdinW)
	runDone := make(chan struct{})
	go func() { h.run(stdoutR); close(runDone) }()
	t.Cleanup(func() {
		stdoutW.Close() // EOF on stdout -> run() returns and shuts the hub down
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Error("hub.run did not return after stdout closed")
		}
	})
	return h, stdinR, stdoutW
}

// TestHubBidirectional is the single-client smoke test: a streamed frame from
// the agent reaches the TUI, and a prompt from the TUI reaches the agent's
// stdin — both with frame boundaries preserved.
func TestHubBidirectional(t *testing.T) {
	h, stdinR, stdoutW := newTestHub(t)

	tui, wrapper := net.Pipe()
	defer tui.Close()
	h.add(wrapper)

	// Agent streams a response -> TUI receives the whole frame.
	go stdoutW.Write([]byte("from-agent\n"))
	if got := scanLine(t, tui); got != "from-agent\n" {
		t.Errorf("tui got %q, want %q", got, "from-agent\n")
	}

	// TUI sends a prompt -> agent stdin receives the whole frame.
	go tui.Write([]byte("from-tui\n"))
	if got, err := bufio.NewReader(stdinR).ReadBytes('\n'); err != nil || string(got) != "from-tui\n" {
		t.Fatalf("agent stdin got %q (err %v), want %q", got, err, "from-tui\n")
	}
}

// TestHubFanOut asserts every connected TUI receives a copy of each broadcast
// frame — the property a single per-connection io.Copy(conn, stdout) could not
// provide, since the lone stdout cannot be read by N goroutines without each
// seeing only a fraction of the stream.
func TestHubFanOut(t *testing.T) {
	h, _, stdoutW := newTestHub(t)

	tuiA, wrapperA := net.Pipe()
	tuiB, wrapperB := net.Pipe()
	defer tuiA.Close()
	defer tuiB.Close()
	h.add(wrapperA)
	h.add(wrapperB)

	go stdoutW.Write([]byte("broadcast\n"))
	if got := scanLine(t, tuiA); got != "broadcast\n" {
		t.Errorf("tuiA got %q, want %q", got, "broadcast\n")
	}
	if got := scanLine(t, tuiB); got != "broadcast\n" {
		t.Errorf("tuiB got %q, want %q", got, "broadcast\n")
	}
}

// TestHubLateReconnect models a watcher that disconnects and a new one that
// connects afterward: the late client must receive frames the agent streams
// from that point on. This is the task's minimum reconnection guarantee.
func TestHubLateReconnect(t *testing.T) {
	h, _, stdoutW := newTestHub(t)

	// First watcher connects, sees one frame, then hangs up.
	tuiA, wrapperA := net.Pipe()
	h.add(wrapperA)
	go stdoutW.Write([]byte("first\n"))
	if got := scanLine(t, tuiA); got != "first\n" {
		t.Errorf("tuiA got %q, want %q", got, "first\n")
	}
	tuiA.Close()

	// A later watcher connects and must receive ongoing stream output.
	tuiB, wrapperB := net.Pipe()
	defer tuiB.Close()
	h.add(wrapperB)
	go stdoutW.Write([]byte("second\n"))
	if got := scanLine(t, tuiB); got != "second\n" {
		t.Errorf("reconnecting tuiB got %q, want %q", got, "second\n")
	}
}

// TestScanFramesReassemblesPartialReads checks the framing reassembles a single
// frame delivered across several Read calls and still emits a trailing,
// unterminated chunk at EOF — so partial reads never split or drop a frame.
func TestScanFramesReassemblesPartialReads(t *testing.T) {
	pr, pw := io.Pipe()
	var frames []string
	done := make(chan struct{})
	go func() {
		scanFrames(pr, func(f []byte) bool { frames = append(frames, string(f)); return true })
		close(done)
	}()

	pw.Write([]byte("hel"))
	pw.Write([]byte("lo\nwor")) // completes "hello\n", starts "wor"
	pw.Write([]byte("ld"))      // "world" left unterminated
	pw.Close()                  // EOF flushes the trailing "world"

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanFrames did not finish")
	}
	want := []string{"hello\n", "world"}
	if !reflect.DeepEqual(frames, want) {
		t.Errorf("frames = %v, want %v", frames, want)
	}
}

// TestHubStdinNoInterleave drives two clients writing large frames concurrently
// and asserts each frame lands in the agent's stdin whole — never split by the
// other client's bytes. This is the stdin-serialization guarantee that a naive
// shared io.Copy(stdin, conn) per connection cannot make.
func TestHubStdinNoInterleave(t *testing.T) {
	h, stdinR, _ := newTestHub(t)

	frameA := strings.Repeat("A", 50000) + "\n"
	frameB := strings.Repeat("B", 50000) + "\n"

	tuiA, wrapperA := net.Pipe()
	tuiB, wrapperB := net.Pipe()
	defer tuiA.Close()
	defer tuiB.Close()
	h.add(wrapperA)
	h.add(wrapperB)

	go tuiA.Write([]byte(frameA))
	go tuiB.Write([]byte(frameB))

	br := bufio.NewReader(stdinR)
	got1, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read frame 1: %v", err)
	}
	got2, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read frame 2: %v", err)
	}
	// Each frame must be exactly one of the inputs, intact and homogeneous.
	for i, got := range []string{string(got1), string(got2)} {
		if got != frameA && got != frameB {
			t.Fatalf("frame %d was interleaved/corrupted (len %d, prefix %q)", i, len(got), got[:min(8, len(got))])
		}
	}
	if string(got1) == string(got2) {
		t.Errorf("expected the two distinct frames, got the same one twice")
	}
}
