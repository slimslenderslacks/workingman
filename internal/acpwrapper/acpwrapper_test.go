package acpwrapper

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
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

func TestExecArgsInjectsGitIdentity(t *testing.T) {
	c := Config{
		SandboxName: "acp-s",
		Workspaces:  []string{"/host/repo"},
		GitName:     "Jim Clark",
		GitEmail:    "jim@example.com",
	}
	got := c.execArgs()
	want := []string{
		"exec",
		"-e", "GIT_AUTHOR_NAME=Jim Clark",
		"-e", "GIT_AUTHOR_EMAIL=jim@example.com",
		"-e", "GIT_COMMITTER_NAME=Jim Clark",
		"-e", "GIT_COMMITTER_EMAIL=jim@example.com",
		"-w", "/host/repo",
		"acp-s", "--", "claude-acp-client",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("execArgs() = %v, want %v", got, want)
	}
}

func TestExecArgsInjectsSigningConfig(t *testing.T) {
	c := Config{
		SandboxName: "acp-s",
		Workspaces:  []string{"/host/repo"},
		GitName:     "Jim Clark",
		GitEmail:    "jim@example.com",
		SigningKey:  "ssh-ed25519 AAAAKEY",
	}
	got := c.execArgs()
	want := []string{
		"exec",
		"-e", "GIT_AUTHOR_NAME=Jim Clark",
		"-e", "GIT_AUTHOR_EMAIL=jim@example.com",
		"-e", "GIT_COMMITTER_NAME=Jim Clark",
		"-e", "GIT_COMMITTER_EMAIL=jim@example.com",
		"-e", "GIT_CONFIG_COUNT=4",
		"-e", "GIT_CONFIG_KEY_0=user.signingkey",
		"-e", "GIT_CONFIG_VALUE_0=ssh-ed25519 AAAAKEY",
		"-e", "GIT_CONFIG_KEY_1=gpg.format",
		"-e", "GIT_CONFIG_VALUE_1=ssh",
		"-e", "GIT_CONFIG_KEY_2=gpg.ssh.program",
		"-e", "GIT_CONFIG_VALUE_2=ssh-keygen",
		"-e", "GIT_CONFIG_KEY_3=commit.gpgsign",
		"-e", "GIT_CONFIG_VALUE_3=true",
		"-w", "/host/repo",
		"acp-s", "--", "claude-acp-client",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("execArgs() = %v, want %v", got, want)
	}
}

func TestExecArgsOmitsSigningWhenNoKey(t *testing.T) {
	// No SigningKey → no GIT_CONFIG_* signing env at all.
	c := Config{SandboxName: "acp-s", Workspaces: []string{"/host/repo"}}
	for _, a := range c.execArgs() {
		if strings.HasPrefix(a, "GIT_CONFIG_") {
			t.Fatalf("unexpected signing env %q with no SigningKey", a)
		}
	}
}

// SSH agent forwarding is no longer done by the wrapper: sbx exposes a working
// agent inside every sandbox at /run/ssh-agent.sock, and the wrapper leaves
// SSH_AUTH_SOCK untouched. execArgs must never inject an SSH_AUTH_SOCK override,
// even when signing is configured.
func TestExecArgsNeverOverridesSSHAuthSock(t *testing.T) {
	c := Config{
		SandboxName: "acp-s",
		Workspaces:  []string{"/host/repo"},
		SigningKey:  "ssh-ed25519 AAAAKEY",
	}
	for _, a := range c.execArgs() {
		if strings.HasPrefix(a, "SSH_AUTH_SOCK=") {
			t.Fatalf("wrapper must not override SSH_AUTH_SOCK; got %q", a)
		}
	}
}

// Signing must not add any bind mount: sbx forwards the agent itself, so the
// only mounts are the configured workspaces.
func TestEnsureSandboxMountsOnlyWorkspacesWhenSigning(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`}
	c := Config{
		SandboxName: "acp-s",
		KitPath:     "/kits/acp",
		SbxPath:     "sbx",
		Workspaces:  []string{"/repo"},
		SigningKey:  "ssh-ed25519 AAAAKEY",
	}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	create := f.calls[1]
	want := []string{"sbx", "create", "claude", "--name", "acp-s", "--kit", "/kits/acp", "/repo"}
	if !reflect.DeepEqual(create, want) {
		t.Errorf("create call = %v, want %v", create, want)
	}
}

// A prebuilt TemplateImage must reach `sbx create` as `-t <image>` with an
// explicit `--pull never`: sbx defaults to --pull always, and the sandbox
// runtime's image store is separate from the host docker's, so a locally built
// image would otherwise fail at PREPARE IMAGE with a registry 403. The kit
// stays on the command line alongside it — the image removes the kit's install
// cost, it does not replace the mixin.
func TestEnsureSandboxCreatesFromTemplateImage(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`}
	c := Config{
		SandboxName:   "acp-s",
		KitPath:       "/kits/acp",
		TemplateImage: "slimslenderslacks/claude-code-acp:0.88.0",
		SbxPath:       "sbx",
		Workspaces:    []string{"/repo"},
	}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	create := f.calls[1]
	want := []string{
		"sbx", "create", "claude", "--name", "acp-s", "--kit", "/kits/acp",
		"-t", "slimslenderslacks/claude-code-acp:0.88.0", "--pull", "never", "/repo",
	}
	if !reflect.DeepEqual(create, want) {
		t.Errorf("create call = %v, want %v", create, want)
	}
}

// The no-image path must stay byte-identical to the pre-change argv so hosts
// without a prebuilt image keep sbx's own template and pull defaults.
func TestEnsureSandboxOmitsTemplateFlagsWhenUnset(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`}
	c := Config{SandboxName: "acp-s", KitPath: "/kits/acp", SbxPath: "sbx", Workspaces: []string{"/repo"}}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	for _, arg := range f.calls[1] {
		if arg == "-t" || arg == "--pull" {
			t.Errorf("create call carries %q with no TemplateImage: %v", arg, f.calls[1])
		}
	}
}

func TestExecArgsOmitsGitIdentityWhenIncomplete(t *testing.T) {
	// A half-set identity (name but no email) must NOT inject env — that would
	// produce commits with a blank email. Fall back to the sandbox default.
	c := Config{SandboxName: "acp-s", GitName: "Jim Clark"}
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
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
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

// TestEnsureSandboxIgnoresOtherSandboxesOnCacheMiss confirms that a cache
// miss (no sandbox named c.SandboxName) always issues a plain `sbx create
// ... --name <c.SandboxName>` and never looks up or adopts any other
// sandbox by a different name, even when other sandboxes exist and would
// otherwise look like plausible spares.
func TestEnsureSandboxIgnoresOtherSandboxesOnCacheMiss(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[
		{"name":"acp-some-other-session","workspaces":["/repo"]},
		{"name":"acp-pool-deadbeef-1234","workspaces":["/repo"]}
	]}`}
	c := Config{SandboxName: "acp-s", KitPath: "/kits/acp", SbxPath: "sbx", Workspaces: []string{"/repo"}}
	name, err := ensureSandbox(context.Background(), f.run, c)
	if err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if name != "acp-s" {
		t.Errorf("name = %q, want %q (fresh create under own name, no adoption)", name, "acp-s")
	}
	if len(f.calls) != 2 {
		t.Fatalf("expected 2 sbx calls (ls, create), got %d: %v", len(f.calls), f.calls)
	}
	want := []string{"sbx", "create", "claude", "--name", "acp-s", "--kit", "/kits/acp", "/repo"}
	if !reflect.DeepEqual(f.calls[1], want) {
		t.Errorf("create call = %v, want %v", f.calls[1], want)
	}
}

func TestEnsureSandboxForwardsStaticMCPs(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`}
	c := Config{
		SandboxName: "acp-s",
		KitPath:     "/kits/acp",
		SbxPath:     "sbx",
		Workspaces:  []string{"/repo"},
		StaticMCPs:  []string{"github", "web-search"},
	}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("expected 2 sbx calls, got %d: %v", len(f.calls), f.calls)
	}
	create := f.calls[1]
	want := []string{
		"sbx", "create", "claude", "--name", "acp-s", "--kit", "/kits/acp",
		"--static-mcp", "github",
		"--static-mcp", "web-search",
		"/repo",
	}
	if !reflect.DeepEqual(create, want) {
		t.Errorf("create call = %v, want %v", create, want)
	}
}

func TestEnsureSandboxAppliesPoliciesAfterCreate(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`}
	c := Config{
		SandboxName: "acp-s",
		KitPath:     "/kits/acp",
		SbxPath:     "sbx",
		Workspaces:  []string{"/repo"},
		Policies: []policy.Rule{
			{Action: policy.ActionDeny, Kind: policy.KindNetwork, Resource: "**"},
			{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "api.github.com"},
		},
	}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	// Expect ls, create, then one policy call per rule, in declaration order.
	if len(f.calls) != 4 {
		t.Fatalf("expected 4 sbx calls, got %d: %v", len(f.calls), f.calls)
	}
	if f.calls[1][1] != "create" {
		t.Fatalf("call 1 = %v, want sbx create", f.calls[1])
	}
	wantDeny := []string{"sbx", "policy", "deny", "network", "--sandbox", "acp-s", "**"}
	wantAllow := []string{"sbx", "policy", "allow", "network", "--sandbox", "acp-s", "api.github.com"}
	if !reflect.DeepEqual(f.calls[2], wantDeny) {
		t.Errorf("policy call 1 = %v, want %v", f.calls[2], wantDeny)
	}
	if !reflect.DeepEqual(f.calls[3], wantAllow) {
		t.Errorf("policy call 2 = %v, want %v", f.calls[3], wantAllow)
	}
}

func TestEnsureSandboxNoopWhenSameWorkspaces(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-s","workspaces":["/repo"]}]}`}
	c := Config{SandboxName: "acp-s", KitPath: "k", SbxPath: "sbx", Workspaces: []string{"/repo"}}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected only ls call, got %v", f.calls)
	}
}

func TestEnsureSandboxRecreatesOnDrift(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-s","workspaces":["/old"]}]}`}
	c := Config{SandboxName: "acp-s", KitPath: "k", SbxPath: "sbx", Workspaces: []string{"/repo"}}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
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
	_, err := ensureSandbox(context.Background(), f.run, c)
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

func TestKeepForTaskStatus(t *testing.T) {
	keep := []task.Status{task.StatusFailed, task.StatusBlocked, task.StatusRunning}
	drop := []task.Status{task.StatusSuccess, task.StatusCommitted, task.StatusReady}
	for _, s := range keep {
		if !keepForTaskStatus(s) {
			t.Errorf("keepForTaskStatus(%q) = false, want true", s)
		}
	}
	for _, s := range drop {
		if keepForTaskStatus(s) {
			t.Errorf("keepForTaskStatus(%q) = true, want false", s)
		}
	}
}

// writeTaskFile writes a minimal task YAML for the removeSandboxOnExit tests and
// returns its path.
func writeTaskFile(t *testing.T, status task.Status, saveSandbox bool) string {
	t.Helper()
	tk := &task.Task{Name: "first", Status: status, SaveSandbox: saveSandbox}
	path := filepath.Join(t.TempDir(), "first.yaml")
	if err := task.Save(path, tk); err != nil {
		t.Fatalf("save task: %v", err)
	}
	return path
}

func TestRemoveSandboxOnExit(t *testing.T) {
	base := Config{SessionID: "s", SandboxName: "acp-s", SbxPath: "sbx"}

	tests := []struct {
		name         string
		cfg          func(Config) Config
		shuttingDown bool
		wantRemove   bool
	}{
		{
			name:       "success task is removed",
			cfg:        func(c Config) Config { c.TaskPath = writeTaskFile(t, task.StatusSuccess, false); return c },
			wantRemove: true,
		},
		{
			name:       "committed task is removed",
			cfg:        func(c Config) Config { c.TaskPath = writeTaskFile(t, task.StatusCommitted, false); return c },
			wantRemove: true,
		},
		{
			name:       "failed task is kept",
			cfg:        func(c Config) Config { c.TaskPath = writeTaskFile(t, task.StatusFailed, false); return c },
			wantRemove: false,
		},
		{
			name:       "blocked task is kept",
			cfg:        func(c Config) Config { c.TaskPath = writeTaskFile(t, task.StatusBlocked, false); return c },
			wantRemove: false,
		},
		{
			name:       "save_sandbox in task file is kept",
			cfg:        func(c Config) Config { c.TaskPath = writeTaskFile(t, task.StatusSuccess, true); return c },
			wantRemove: false,
		},
		{
			name:       "save_sandbox config override is kept",
			cfg:        func(c Config) Config { c.SaveSandbox = true; return c },
			wantRemove: false,
		},
		{
			name:       "no task path (planning) is removed",
			cfg:        func(c Config) Config { return c },
			wantRemove: true,
		},
		{
			name:       "unreadable task file is kept",
			cfg:        func(c Config) Config { c.TaskPath = "/no/such/task.yaml"; return c },
			wantRemove: false,
		},
		{
			name:         "shutdown never removes",
			cfg:          func(c Config) Config { c.TaskPath = writeTaskFile(t, task.StatusSuccess, false); return c },
			shuttingDown: true,
			wantRemove:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeSbx{}
			removeSandboxOnExit(context.Background(), f.run, tt.cfg(base), tt.shuttingDown)
			var removed bool
			for _, call := range f.calls {
				if len(call) >= 2 && call[1] == "rm" {
					removed = true
					if want := []string{"sbx", "rm", "--force", "acp-s"}; !reflect.DeepEqual(call, want) {
						t.Errorf("rm call = %v, want %v", call, want)
					}
				}
			}
			if removed != tt.wantRemove {
				t.Errorf("sandbox removed = %v, want %v (calls: %v)", removed, tt.wantRemove, f.calls)
			}
		})
	}
}

// TestRemoveSandboxOnExitAlwaysRemovesOnCleanExit confirms that a clean exit
// with none of SaveSandbox/keepForTaskStatus/shuttingDown set always issues a
// plain `sbx rm --force <c.SandboxName>` — there is no donate-to-pool path,
// and exactly one sbx call is made.
func TestRemoveSandboxOnExitAlwaysRemovesOnCleanExit(t *testing.T) {
	c := Config{SessionID: "s", SandboxName: "acp-s", SbxPath: "sbx"}
	f := &fakeSbx{}
	removeSandboxOnExit(context.Background(), f.run, c, false)

	if len(f.calls) != 1 {
		t.Fatalf("expected exactly 1 sbx call, got %d: %v", len(f.calls), f.calls)
	}
	want := []string{"sbx", "rm", "--force", "acp-s"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("call = %v, want %v", f.calls[0], want)
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
	h = newHub(stdinW, nil)
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

// TestHubLogsAgentFrames asserts the hub tees each agent stdout frame into the
// session's stream log, so a TUI that reconnects after a restart can replay the
// prior output. The log must record the same whole frames the live clients see.
func TestHubLogsAgentFrames(t *testing.T) {
	var log bytes.Buffer

	stdoutR, stdoutW := io.Pipe()
	_, stdinW := io.Pipe()
	h := newHub(stdinW, &log)
	runDone := make(chan struct{})
	go func() { h.run(stdoutR); close(runDone) }()

	tui, wrapper := net.Pipe()
	defer tui.Close()
	h.add(wrapper)

	// Stream two frames; wait for the live client to receive each so run() has
	// processed (and logged) it before we close stdout.
	go stdoutW.Write([]byte("frame-one\n"))
	if got := scanLine(t, tui); got != "frame-one\n" {
		t.Fatalf("tui got %q, want %q", got, "frame-one\n")
	}
	go stdoutW.Write([]byte("frame-two\n"))
	if got := scanLine(t, tui); got != "frame-two\n" {
		t.Fatalf("tui got %q, want %q", got, "frame-two\n")
	}

	stdoutW.Close() // EOF -> run() returns; close(runDone) happens-after all logging
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("hub.run did not return after stdout closed")
	}

	if got, want := log.String(), "frame-one\nframe-two\n"; got != want {
		t.Errorf("stream log = %q, want %q", got, want)
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

func TestSigningPreflight(t *testing.T) {
	base := Config{SandboxName: "acp-s", SbxPath: "sbx", SigningKey: "ssh-ed25519 AAAA"}

	// Signing not configured -> not checked, and run is never invoked.
	noKey := base
	noKey.SigningKey = ""
	if checked, _ := signingPreflight(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("run must not be called when signing is unconfigured")
		return nil, nil
	}, noKey); checked {
		t.Errorf("expected checked=false when SigningKey is empty")
	}

	// Forwarded agent lists a key -> ok.
	okRun := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("256 SHA256:abc123 github (ED25519)\n"), nil
	}
	if checked, ok := signingPreflight(context.Background(), okRun, base); !checked || !ok {
		t.Errorf("agent-with-key: got checked=%v ok=%v, want true true", checked, ok)
	}

	// Empty agent: `ssh-add -l` exits non-zero, so run returns an error and no
	// fingerprint -> checked but not ok.
	emptyRun := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("The agent has no identities.\n"), errors.New("exit status 1")
	}
	if checked, ok := signingPreflight(context.Background(), emptyRun, base); !checked || ok {
		t.Errorf("empty-agent: got checked=%v ok=%v, want true false", checked, ok)
	}

	// The probe runs exactly `sbx exec <sandbox> -- ssh-add -l`.
	var gotArgs []string
	capRun := func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return []byte("SHA256:x"), nil
	}
	signingPreflight(context.Background(), capRun, base)
	want := []string{"sbx", "exec", "acp-s", "--", "ssh-add", "-l"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("preflight argv = %v, want %v", gotArgs, want)
	}
}

// TestWithSigningPreflightResultDisablesOnFailure is the crux of the fix:
// a failed preflight (checked && !ok) must clear SigningKey so execArgs stops
// forcing commit.gpgsign=true — otherwise every git commit in the sandbox
// hard-fails signing against an agent with no key, rather than landing
// unsigned. It must also flag signingPreflightFailed so the degradation is
// recorded into session.json (see TestSessionRecordReflectsSigningBroken)
// instead of only appearing in a stderr log line.
func TestWithSigningPreflightResultDisablesOnFailure(t *testing.T) {
	base := Config{SandboxName: "acp-s", Workspaces: []string{"/repo"}, SigningKey: "ssh-ed25519 AAAA"}

	got := withSigningPreflightResult(base, true, false)
	if got.SigningKey != "" {
		t.Errorf("SigningKey = %q, want cleared after failed preflight", got.SigningKey)
	}
	if !got.signingPreflightFailed {
		t.Errorf("signingPreflightFailed = false, want true after failed preflight")
	}

	// The clearing must actually change execArgs' output, not just the field —
	// this is the "provably changes execArgs' behavior" requirement.
	for _, a := range got.execArgs() {
		if strings.HasPrefix(a, "GIT_CONFIG_") {
			t.Fatalf("execArgs still injected signing config after failed preflight: %v", got.execArgs())
		}
	}
}

// TestWithSigningPreflightResultLeavesOtherCasesUnchanged asserts the two
// non-failure outcomes (signing unconfigured, or preflight passed) leave
// Config untouched, so a passing preflight keeps forcing commit.gpgsign=true
// exactly as before.
func TestWithSigningPreflightResultLeavesOtherCasesUnchanged(t *testing.T) {
	base := Config{SandboxName: "acp-s", Workspaces: []string{"/repo"}, SigningKey: "ssh-ed25519 AAAA"}

	tests := []struct {
		name        string
		checked, ok bool
	}{
		{"not configured", false, false},
		{"preflight passed", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withSigningPreflightResult(base, tt.checked, tt.ok)
			if got.SigningKey != base.SigningKey {
				t.Errorf("SigningKey = %q, want unchanged %q", got.SigningKey, base.SigningKey)
			}
			if got.signingPreflightFailed {
				t.Errorf("signingPreflightFailed = true, want false")
			}
		})
	}
}

// TestSessionRecordReflectsSigningBroken asserts the signingPreflightFailed
// flag withSigningPreflightResult sets is surfaced into session.json's
// SigningBroken field, so the degradation is visible to a reconnecting
// TUI/daemon reading session state, not only to a stderr tail.
func TestSessionRecordReflectsSigningBroken(t *testing.T) {
	c := Config{SessionID: "s", SandboxName: "acp-s", SigningKey: "ssh-ed25519 AAAA"}
	c = withSigningPreflightResult(c, true, false)

	rec := c.sessionRecord(session.StatusRunning, time.Time{}, time.Time{})
	if !rec.SigningBroken {
		t.Errorf("sessionRecord().SigningBroken = false, want true after failed preflight")
	}
}

func TestNormalizePersistentValidation(t *testing.T) {
	base := Config{SessionID: "s", KitPath: "k", Workspaces: []string{"/r"}}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string // "" = must normalize cleanly
	}{
		{"persistent alone is fine", func(c *Config) { c.Persistent = true }, ""},
		{"persistent with end conditions", func(c *Config) {
			c.Persistent, c.ProjectPath, c.UnblockGrace, c.IdleTimeout = true, "/p/.project.yaml", time.Minute, time.Hour
		}, ""},
		{"persistent and exit-when-empty clash", func(c *Config) { c.Persistent, c.ExitWhenEmpty = true, true }, "mutually exclusive"},
		{"unblock grace needs persistent", func(c *Config) { c.ProjectPath, c.UnblockGrace = "/p", time.Minute }, "only apply to a persistent session"},
		{"idle timeout needs persistent", func(c *Config) { c.IdleTimeout = time.Minute }, "only apply to a persistent session"},
		{"unblock grace needs a project path", func(c *Config) { c.Persistent, c.UnblockGrace = true, time.Minute }, "needs the project path"},
		{"negative idle timeout", func(c *Config) { c.Persistent, c.IdleTimeout = true, -time.Second }, "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.Workspaces = append([]string(nil), base.Workspaces...)
			tt.mutate(&cfg)
			err := cfg.normalize()
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("normalize() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("normalize() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// TestSessionRecordReflectsPersistent: session.json carries Persistent so
// watchers know the first completed turn is not the end of the session.
func TestSessionRecordReflectsPersistent(t *testing.T) {
	if rec := (Config{SessionID: "s"}).sessionRecord(session.StatusRunning, time.Time{}, time.Time{}); rec.Persistent {
		t.Error("one-shot session recorded as persistent")
	}
	if rec := (Config{SessionID: "s", Persistent: true}).sessionRecord(session.StatusRunning, time.Time{}, time.Time{}); !rec.Persistent {
		t.Error("persistent session not recorded as persistent")
	}
}

func TestUnblockTracker(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tr := unblockTracker{grace: 10 * time.Second}

	if tr.observe(t0, true) {
		t.Fatal("blocked project must never end the session")
	}
	// Unblocked, but the grace hasn't elapsed.
	if tr.observe(t0.Add(1*time.Second), false) {
		t.Fatal("ended immediately on unblock; the wolf needs its grace to finish")
	}
	if tr.observe(t0.Add(10*time.Second), false) {
		t.Fatal("ended 9s into a 10s grace")
	}
	// A re-block inside the grace resets the clock...
	if tr.observe(t0.Add(11*time.Second), true) {
		t.Fatal("blocked project must never end the session")
	}
	if tr.observe(t0.Add(20*time.Second), false) {
		t.Fatal("grace should restart from the second unblock, not the first")
	}
	// ...and a full grace after the second unblock ends it.
	if !tr.observe(t0.Add(30*time.Second), false) {
		t.Fatal("grace elapsed with the project continuously unblocked; want end")
	}
}

func TestWatchUnblockedEndsOnceProjectLeavesBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".project.yaml")
	if err := project.SaveAs(path, &project.Project{Branch: "b", Status: project.StatusBlocked, BlockedReason: "x"}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchUnblocked(ctx, path, 60*time.Millisecond, 10*time.Millisecond, func() { close(ended) })

	select {
	case <-ended:
		t.Fatal("session ended while the project was still blocked")
	case <-time.After(200 * time.Millisecond):
	}

	if err := project.SaveAs(path, &project.Project{Branch: "b", Status: project.StatusWorking}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not end after the project left blocked")
	}
}

func TestWatchUnblockedTreatsMissingProjectAsUnblocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone", ".project.yaml")
	ended := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchUnblocked(ctx, path, 30*time.Millisecond, 10*time.Millisecond, func() { close(ended) })
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("a deleted project file should end the wolf's session")
	}
}

func TestWatchUnblockedStopsWithContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".project.yaml")
	if err := project.SaveAs(path, &project.Project{Branch: "b", Status: project.StatusBlocked, BlockedReason: "x"}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchUnblocked(ctx, path, time.Hour, 10*time.Millisecond, func() { t.Error("end called after cancel") })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchUnblocked did not return after ctx cancel")
	}
}

// TestHubExitWhenEmptyVsPersistent contrasts the two lifecycles: one-shot mode
// closes the agent's stdin once the last client leaves; persistent mode (the
// default hub, no enableExitWhenEmpty) keeps the agent running so the human
// and other processes can come and go.
func TestHubExitWhenEmptyVsPersistent(t *testing.T) {
	run := func(t *testing.T, oneShot bool) (stdinEOF bool) {
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()
		defer stdoutW.Close()
		h := newHub(stdinW, nil)
		if oneShot {
			h.enableExitWhenEmpty(stdinW)
		}
		go h.run(stdoutR)

		tui, wrapper := net.Pipe()
		h.add(wrapper)
		tui.Close() // the only client leaves

		got := make(chan error, 1)
		go func() {
			_, err := stdinR.Read(make([]byte, 1))
			got <- err
		}()
		select {
		case err := <-got:
			return errors.Is(err, io.EOF)
		case <-time.After(300 * time.Millisecond):
			stdinW.Close() // unblock the reader goroutine
			return false
		}
	}
	if !run(t, true) {
		t.Error("exit-when-empty: agent stdin should hit EOF after the last client leaves")
	}
	if run(t, false) {
		t.Error("persistent: agent stdin must stay open after the last client leaves")
	}
}

func TestHubWatchIdleEndsQuietSession(t *testing.T) {
	h, _, _ := newTestHub(t)
	ended := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.watchIdle(ctx, 80*time.Millisecond, func() { close(ended) })
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("idle session was never ended")
	}
}

// TestHubWatchIdleCountsTrafficBothWays: frames from the agent AND from a client
// reset the idle clock, so a conversation in progress is never cut off; only
// real traffic counts, not merely being connected.
func TestHubWatchIdleCountsTrafficBothWays(t *testing.T) {
	h, stdinR, stdoutW := newTestHub(t)
	go io.Copy(io.Discard, stdinR)

	tui, wrapper := net.Pipe()
	defer tui.Close()
	h.add(wrapper)
	go io.Copy(io.Discard, tui)

	var ended atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.watchIdle(ctx, 150*time.Millisecond, func() { ended.Store(true) })

	// 500ms of alternating traffic, each gap well under the timeout.
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			stdoutW.Write([]byte("agent\n"))
		} else {
			tui.Write([]byte("client\n"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ended.Load() {
		t.Fatal("session ended despite steady traffic")
	}
	// Traffic stops; the connected-but-silent client must not keep it alive.
	deadline := time.Now().Add(3 * time.Second)
	for !ended.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !ended.Load() {
		t.Fatal("session never ended after traffic stopped")
	}
}

// TestServePersistentIdleEndsAgent wires the idle timeout through serve the way
// Run does: endIdle closes the agent's stdin (the agent's EOF/exit signal) while
// a persistent session otherwise ignores clients coming and going.
func TestServePersistentIdleEndsAgent(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "a.sock"))
	if err != nil {
		t.Skipf("cannot listen on a unix socket here: %v", err)
	}
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	defer stdoutW.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() {
		serve(ctx, ln, stdinW, stdoutR, nil, serveOptions{
			idleTimeout: 80 * time.Millisecond,
			endIdle:     func() { stdinW.Close() },
		})
		close(served)
	}()

	got := make(chan error, 1)
	go func() {
		_, err := stdinR.Read(make([]byte, 1))
		got <- err
	}()
	select {
	case err := <-got:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("agent stdin read = %v, want EOF from the idle end", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle persistent session never closed the agent's stdin")
	}
	ln.Close()
	<-served
}

func TestSplitJoinMount(t *testing.T) {
	for _, tc := range []struct {
		in   string
		path string
		ro   bool
	}{
		{"/orch", "/orch", false},
		{"/orch:ro", "/orch", true},
		{"/a:b/c", "/a:b/c", false}, // only a trailing :ro is a mode
	} {
		path, ro := SplitMount(tc.in)
		if path != tc.path || ro != tc.ro {
			t.Errorf("SplitMount(%q) = (%q, %v), want (%q, %v)", tc.in, path, ro, tc.path, tc.ro)
		}
		if got := JoinMount(path, ro); got != tc.in {
			t.Errorf("JoinMount round trip of %q = %q", tc.in, got)
		}
	}
}

// A read-only mount keeps its :ro suffix through normalize (so sbx still gets
// it) but never becomes the cwd, and the first mount may not be read-only: the
// ACP client needs a writable cwd.
func TestNormalizeKeepsReadOnlyMounts(t *testing.T) {
	c := Config{SessionID: "s", KitPath: "k", Workspaces: []string{"scratch", "rel/orch:ro"}}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !filepath.IsAbs(c.Workspaces[0]) || strings.HasSuffix(c.Workspaces[0], ":ro") {
		t.Errorf("primary workspace = %q, want absolute and writable", c.Workspaces[0])
	}
	if p, ro := SplitMount(c.Workspaces[1]); !ro || !filepath.IsAbs(p) {
		t.Errorf("read-only workspace = %q, want absolute with :ro kept", c.Workspaces[1])
	}
	if got := c.execArgs(); got[2] != c.Workspaces[0] {
		t.Errorf("cwd = %q, want the writable primary %q", got[2], c.Workspaces[0])
	}

	bad := Config{SessionID: "s", KitPath: "k", Workspaces: []string{"/orch:ro", "/scratch"}}
	if err := bad.normalize(); err == nil || !strings.Contains(err.Error(), "must be writable") {
		t.Errorf("read-only primary workspace: err = %v, want a 'must be writable' error", err)
	}
}

func TestEnsureSandboxPassesReadOnlyMountsToSbx(t *testing.T) {
	f := &fakeSbx{lsOutput: `{"sandboxes":[]}`}
	c := Config{
		SandboxName: "workingman-agent",
		KitPath:     "/kits/acp",
		SbxPath:     "sbx",
		Workspaces:  []string{"/scratch", "/orch:ro", "/sessions:ro"},
	}
	if _, err := ensureSandbox(context.Background(), f.run, c); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	want := []string{"sbx", "create", "claude", "--name", "workingman-agent", "--kit", "/kits/acp", "/scratch", "/orch:ro", "/sessions:ro"}
	if !reflect.DeepEqual(f.calls[1], want) {
		t.Errorf("create call = %v, want %v", f.calls[1], want)
	}
}

// Whether `sbx ls` reports a read-only mount with or without its :ro suffix, a
// relaunch must find the existing sandbox and reuse it rather than recreate it.
func TestSameWorkspaceSetIgnoresReadOnlySuffix(t *testing.T) {
	if !sameWorkspaceSet([]string{"/scratch", "/orch"}, []string{"/scratch", "/orch:ro"}) {
		t.Error("same host paths with and without :ro should match")
	}
	if sameWorkspaceSet([]string{"/scratch", "/orch"}, []string{"/scratch", "/other:ro"}) {
		t.Error("different host paths must not match")
	}
}

func TestGoModCacheDirSkipsReadOnlyMounts(t *testing.T) {
	ro := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ro, ".gomodcache"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := Config{Workspaces: []string{t.TempDir(), ro + ":ro"}}
	if got := c.goModCacheDir(); got != "" {
		t.Errorf("goModCacheDir = %q; a read-only mount can't be a writable module cache", got)
	}
}

func TestPushPreflightError(t *testing.T) {
	fail := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 1")
	}
	// Non-commit kinds never push: no command runs, no error.
	task := Config{Kind: "task", SandboxName: "acp-s", SbxPath: "sbx"}
	if err := pushPreflightError(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("run must not be called for non-commit kinds")
		return nil, nil
	}, task); err != nil {
		t.Errorf("task kind: err = %v, want nil", err)
	}
	// Commit agent with a dead agent fails loudly with an actionable message.
	commit := Config{Kind: "commit", SandboxName: "acp-s", SbxPath: "sbx"}
	err := pushPreflightError(context.Background(), fail, commit)
	if err == nil || !strings.Contains(err.Error(), "check-signing.sh") {
		t.Errorf("commit kind with dead agent: err = %v, want actionable error", err)
	}
	// Commit agent with a working push passes.
	good := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[len(args)-1] == "-l" {
			return []byte("SHA256:x"), nil
		}
		return []byte("successfully authenticated"), errors.New("exit status 1")
	}
	if err := pushPreflightError(context.Background(), good, commit); err != nil {
		t.Errorf("commit kind with good agent: err = %v", err)
	}
}

func TestSessionRecordReflectsPushBroken(t *testing.T) {
	c := Config{pushPreflightFailed: true}
	if !c.sessionRecord(session.StatusFailed, time.Now(), time.Now()).PushBroken {
		t.Errorf("PushBroken = false, want true")
	}
}
