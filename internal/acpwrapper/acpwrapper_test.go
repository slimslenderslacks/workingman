package acpwrapper

import (
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

// TestBridge wires a fake TUI connection (one end of net.Pipe) through bridge to
// a fake ACP client stdio (io.Pipe pairs), asserting bytes flow both ways and
// that bridge returns when the TUI hangs up.
func TestBridge(t *testing.T) {
	tuiSide, wrapperSide := net.Pipe()

	// ACP client stdin: bridge writes here; we read what the TUI sent.
	stdinR, stdinW := io.Pipe()
	// ACP client stdout: we write here; bridge forwards to the TUI.
	stdoutR, stdoutW := io.Pipe()
	// Closing stdout (client exit) lets the detached stdout->TUI copy retire.
	defer stdoutW.Close()

	bridgeDone := make(chan struct{})
	go func() {
		bridge(wrapperSide, stdinW, stdoutR)
		close(bridgeDone)
	}()

	// ACP client emits a streamed response -> TUI should receive it.
	go func() { stdoutW.Write([]byte("from-agent\n")) }()
	buf := make([]byte, len("from-agent\n"))
	tuiSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(tuiSide, buf); err != nil {
		t.Fatalf("read agent->tui: %v", err)
	}
	if string(buf) != "from-agent\n" {
		t.Errorf("tui got %q, want %q", buf, "from-agent\n")
	}

	// TUI sends a prompt -> ACP client stdin should receive it.
	go func() { tuiSide.Write([]byte("from-tui\n")) }()
	pbuf := make([]byte, len("from-tui\n"))
	if _, err := io.ReadFull(stdinR, pbuf); err != nil {
		t.Fatalf("read tui->agent: %v", err)
	}
	if string(pbuf) != "from-tui\n" {
		t.Errorf("agent stdin got %q, want %q", pbuf, "from-tui\n")
	}

	// TUI hangs up -> bridge returns.
	tuiSide.Close()
	select {
	case <-bridgeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not return after TUI closed")
	}
}
