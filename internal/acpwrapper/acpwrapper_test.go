package acpwrapper

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
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

// TestRemoveSandboxOnExitReleasesToPoolWhenEmpty is the pool's basic donation
// path: a clean exit with nothing yet idle for this signature should release
// the sandbox into the pool instead of `sbx rm --force`-ing it.
func TestRemoveSandboxOnExitReleasesToPoolWhenEmpty(t *testing.T) {
	c := Config{
		SessionID:   "s",
		SandboxName: "acp-s",
		SbxPath:     "sbx",
		KitPath:     "/kits/acp",
		Workspaces:  []string{"/repo"},
		PoolRoot:    t.TempDir(),
		PoolCap:     2,
	}
	f := &fakeSbx{}
	removeSandboxOnExit(context.Background(), f.run, c, false)

	for _, call := range f.calls {
		if len(call) >= 2 && call[1] == "rm" {
			t.Fatalf("unexpected sbx rm call: %v", call)
		}
	}
	pool := c.pool()
	n, err := pool.idleCount(c.poolSignature())
	if err != nil {
		t.Fatalf("idleCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("idle count = %d, want 1", n)
	}
}

// TestEnsureSandboxAdoptsIdlePoolEntryOnMatchingSignature is the pool's basic
// adoption path: a session whose derived name has never been created should
// find and claim a matching idle spare instead of issuing `sbx create`.
func TestEnsureSandboxAdoptsIdlePoolEntryOnMatchingSignature(t *testing.T) {
	root := t.TempDir()
	c := Config{
		SessionID:   "s2",
		SandboxName: "acp-s2",
		SbxPath:     "sbx",
		KitPath:     "/kits/acp",
		Workspaces:  []string{"/repo"},
		PoolRoot:    root,
		PoolCap:     2,
	}
	sig := c.poolSignature()
	pool := c.pool()
	if _, err := pool.release(sig, "acp-pool-spare", 2); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-pool-spare","workspaces":["/repo"]}]}`}
	name, err := ensureSandbox(context.Background(), f.run, c)
	if err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if name != "acp-pool-spare" {
		t.Errorf("adopted name = %q, want %q", name, "acp-pool-spare")
	}
	for _, call := range f.calls {
		if len(call) >= 2 && call[1] == "create" {
			t.Fatalf("unexpected sbx create call: %v", call)
		}
	}
	if n, _ := pool.idleCount(sig); n != 0 {
		t.Errorf("idle count after adoption = %d, want 0 (claimed as busy)", n)
	}
}

// TestEnsureSandboxIgnoresPoolOnSignatureMismatch checks a session whose
// signature (kit/workspaces/StaticMCPs/Policies) differs from every idle
// spare's still creates fresh rather than adopting a mismatched entry, and
// leaves that unrelated entry untouched.
func TestEnsureSandboxIgnoresPoolOnSignatureMismatch(t *testing.T) {
	root := t.TempDir()
	seed := Config{KitPath: "/kits/acp", Workspaces: []string{"/repo"}, PoolRoot: root, PoolCap: 2}
	seedSig := seed.poolSignature()
	pool := seed.pool()
	if _, err := pool.release(seedSig, "acp-pool-spare", 2); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	c := Config{
		SessionID:   "s3",
		SandboxName: "acp-s3",
		SbxPath:     "sbx",
		KitPath:     "/kits/other", // different kit -> different signature
		Workspaces:  []string{"/repo"},
		PoolRoot:    root,
		PoolCap:     2,
	}
	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-pool-spare","workspaces":["/repo"]}]}`}
	name, err := ensureSandbox(context.Background(), f.run, c)
	if err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if name != "acp-s3" {
		t.Errorf("name = %q, want %q (fresh create, not adopted)", name, "acp-s3")
	}
	var created bool
	for _, call := range f.calls {
		if len(call) >= 2 && call[1] == "create" {
			created = true
		}
	}
	if !created {
		t.Fatalf("expected sbx create call, got %v", f.calls)
	}
	// The other signature's idle entry must be untouched by this session.
	if n, _ := pool.idleCount(seedSig); n != 1 {
		t.Errorf("unrelated signature idle count = %d, want 1 (untouched)", n)
	}
}

// TestEnsureSandboxRecreatesAdoptedPoolEntryOnWorkspaceMismatch is the other
// half of broadening poolSignatureFor: a claimed idle spare's signature
// (kit/StaticMCPs/Policies) matches, but it was warmed for a different
// project's workspace set (exactly what happens once two different
// projects/branches can share a bucket). ensureSandbox must never hand that
// sandbox back with the wrong workspace mounted — since sbx can't remount an
// existing sandbox, it must rm+recreate it in place, reusing the claimed
// spare's name rather than abandoning it and minting a fresh one under
// c.SandboxName.
func TestEnsureSandboxRecreatesAdoptedPoolEntryOnWorkspaceMismatch(t *testing.T) {
	root := t.TempDir()
	c := Config{
		SessionID:   "s5",
		SandboxName: "acp-s5",
		SbxPath:     "sbx",
		KitPath:     "/kits/acp",
		Workspaces:  []string{"/repo-b", "/orch-b"},
		PoolRoot:    root,
		PoolCap:     2,
	}
	sig := c.poolSignature()
	pool := c.pool()
	if _, err := pool.release(sig, "acp-pool-spare", 2); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	// The spare exists but is mounted for a different project's workspace.
	f := &fakeSbx{lsOutput: `{"sandboxes":[{"name":"acp-pool-spare","workspaces":["/repo-a","/orch-a"]}]}`}
	name, err := ensureSandbox(context.Background(), f.run, c)
	if err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if name != "acp-pool-spare" {
		t.Errorf("name = %q, want %q (repaired in place, not abandoned for a fresh name)", name, "acp-pool-spare")
	}
	if len(f.calls) != 3 {
		t.Fatalf("expected ls, rm, create, got %v", f.calls)
	}
	if f.calls[1][1] != "rm" || f.calls[1][len(f.calls[1])-1] != "acp-pool-spare" {
		t.Errorf("call 1 = %v, want sbx rm --force acp-pool-spare", f.calls[1])
	}
	wantCreate := []string{"sbx", "create", "claude", "--name", "acp-pool-spare", "--kit", "/kits/acp", "/repo-b", "/orch-b"}
	if !reflect.DeepEqual(f.calls[2], wantCreate) {
		t.Errorf("call 2 = %v, want %v", f.calls[2], wantCreate)
	}
	// The pool bookkeeping must still show it busy (claimed and in use), not
	// discarded — it's a perfectly good spare now, just repaired.
	if n, _ := pool.idleCount(sig); n != 0 {
		t.Errorf("idle count = %d, want 0 (still claimed/busy)", n)
	}
}

// TestRemoveSandboxOnExitRemovesWhenPoolAtCap checks that donating a sandbox
// back once its signature's idle pool is already at cap still falls back to
// `sbx rm --force`, so the pool can't grow without bound.
func TestRemoveSandboxOnExitRemovesWhenPoolAtCap(t *testing.T) {
	root := t.TempDir()
	seed := Config{KitPath: "/kits/acp", Workspaces: []string{"/repo"}, PoolRoot: root, PoolCap: 1}
	sig := seed.poolSignature()
	pool := seed.pool()
	if _, err := pool.release(sig, "acp-existing-spare", 1); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	c := Config{
		SessionID:   "s4",
		SandboxName: "acp-s4",
		SbxPath:     "sbx",
		KitPath:     "/kits/acp",
		Workspaces:  []string{"/repo"},
		PoolRoot:    root,
		PoolCap:     1,
	}
	f := &fakeSbx{}
	removeSandboxOnExit(context.Background(), f.run, c, false)

	var removed bool
	for _, call := range f.calls {
		if len(call) >= 2 && call[1] == "rm" {
			removed = true
			want := []string{"sbx", "rm", "--force", "acp-s4"}
			if !reflect.DeepEqual(call, want) {
				t.Errorf("rm call = %v, want %v", call, want)
			}
		}
	}
	if !removed {
		t.Fatalf("expected sbx rm --force when pool at cap, got %v", f.calls)
	}
	if n, _ := pool.idleCount(sig); n != 1 {
		t.Errorf("idle count = %d, want 1 (unchanged, still at cap)", n)
	}
}

// TestPoolSignatureIgnoresOrderExceptForPolicies checks that Workspaces and
// StaticMCPs are treated as unordered sets (matching sameWorkspaceSet's own
// semantics and sbx's lack of mount-order guarantees), while Policies are
// NOT — their declaration order changes evaluation (deny-all + allow-host is
// not the same rule set in reverse), so it must be part of the signature.
func TestPoolSignatureIgnoresOrderExceptForPolicies(t *testing.T) {
	mcps1, mcps2 := []string{"github", "web"}, []string{"web", "github"}
	if got, want := poolSignatureFor("k", mcps1, nil), poolSignatureFor("k", mcps2, nil); got != want {
		t.Errorf("signature depends on MCP order: %q != %q", got, want)
	}

	deny := policy.Rule{Action: policy.ActionDeny, Kind: policy.KindNetwork, Resource: "**"}
	allow := policy.Rule{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "api.github.com"}
	forward := poolSignatureFor("k", nil, []policy.Rule{deny, allow})
	reverse := poolSignatureFor("k", nil, []policy.Rule{allow, deny})
	if forward == reverse {
		t.Errorf("signature must depend on policy declaration order, got same signature for both orders")
	}

	if got, want := poolSignatureFor("k1", nil, nil), poolSignatureFor("k2", nil, nil); got == want {
		t.Errorf("different kit paths produced the same signature %q", got)
	}
}

// TestPoolSignatureIgnoresWorkspace is the core of the broadened pool
// signature this task adds: two Configs whose Workspaces differ entirely
// (as they always will across projects/branches, since wsp keys a workspace
// on the branch — see sandboxWorkspaces/resolveWorkingDir in the runner
// package) but whose kit/StaticMCPs/Policies match must land in the SAME
// pool bucket, so a brand-new project's first task can adopt a spare warmed
// by a completely unrelated project instead of always cold-starting.
func TestPoolSignatureIgnoresWorkspace(t *testing.T) {
	a := Config{KitPath: "/kits/acp", Workspaces: []string{"/repo-a", "/orch-a"}}
	b := Config{KitPath: "/kits/acp", Workspaces: []string{"/repo-b", "/orch-b", "/extra-b"}}
	if got, want := a.poolSignature(), b.poolSignature(); got != want {
		t.Errorf("signature depends on workspace set: %q != %q, want equal (workspace excluded from the key)", got, want)
	}

	// A kit/StaticMCPs/Policies mismatch must still force a different bucket,
	// workspace aside.
	c := Config{KitPath: "/kits/other", Workspaces: a.Workspaces}
	if got, want := a.poolSignature(), c.poolSignature(); got == want {
		t.Errorf("different kit paths produced the same signature %q", got)
	}
	d := Config{KitPath: a.KitPath, Workspaces: a.Workspaces, StaticMCPs: []string{"github"}}
	if got, want := a.poolSignature(), d.poolSignature(); got == want {
		t.Errorf("different StaticMCPs produced the same signature %q", got)
	}
}

// TestPoolClaimIsExclusive checks that claiming an idle entry actually
// removes it from idle (a second claim for the same signature must not see
// the same spare again) and that a signature with nothing idle reports ok=false.
func TestPoolClaimIsExclusive(t *testing.T) {
	pool := Pool{Root: t.TempDir()}
	const sig = "sig-a"

	if _, ok, err := pool.claim(sig); err != nil || ok {
		t.Fatalf("claim on empty pool: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	if _, err := pool.release(sig, "spare-1", 5); err != nil {
		t.Fatalf("release: %v", err)
	}
	name, ok, err := pool.claim(sig)
	if err != nil || !ok || name != "spare-1" {
		t.Fatalf("claim = (%q, %v, %v), want (\"spare-1\", true, nil)", name, ok, err)
	}
	if _, ok, err := pool.claim(sig); err != nil || ok {
		t.Fatalf("second claim: ok=%v err=%v, want ok=false (already claimed)", ok, err)
	}
}

// TestPoolReleaseRespectsCap checks that release refuses once the signature's
// idle count already meets the cap, and that a discarded stale entry frees up
// room again.
func TestPoolReleaseRespectsCap(t *testing.T) {
	pool := Pool{Root: t.TempDir()}
	const sig = "sig-b"

	released, err := pool.release(sig, "spare-1", 1)
	if err != nil || !released {
		t.Fatalf("first release: released=%v err=%v, want true, nil", released, err)
	}
	released, err = pool.release(sig, "spare-2", 1)
	if err != nil || released {
		t.Fatalf("second release at cap: released=%v err=%v, want false, nil", released, err)
	}
	if n, err := pool.idleCount(sig); err != nil || n != 1 {
		t.Fatalf("idleCount = %d, err %v, want 1", n, err)
	}

	pool.discard(sig, "spare-1")
	if n, err := pool.idleCount(sig); err != nil || n != 0 {
		t.Fatalf("idleCount after discard = %d, err %v, want 0", n, err)
	}
	released, err = pool.release(sig, "spare-2", 1)
	if err != nil || !released {
		t.Fatalf("release after discard: released=%v err=%v, want true, nil", released, err)
	}
}

// TestPoolReconcileDemotesOrphanedBusyEntries is the crash-recovery path: a
// busy entry with no corresponding live session must be moved back to idle
// so it isn't stuck unusable forever, while a busy entry that IS still live
// must be left alone.
func TestPoolReconcileDemotesOrphanedBusyEntries(t *testing.T) {
	pool := Pool{Root: t.TempDir()}
	const sig = "sig-c"

	if _, err := pool.release(sig, "orphan", 5); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	if _, ok, err := pool.claim(sig); err != nil || !ok {
		t.Fatalf("claim orphan: ok=%v err=%v", ok, err)
	}
	if _, err := pool.release(sig, "still-live", 5); err != nil {
		t.Fatalf("seed still-live: %v", err)
	}
	if _, ok, err := pool.claim(sig); err != nil || !ok {
		t.Fatalf("claim still-live: ok=%v err=%v", ok, err)
	}

	if err := pool.reconcile(map[string]bool{"still-live": true}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// "orphan" has no live session backing it -> demoted to idle and
	// claimable again; "still-live" has one -> must stay busy (not claimable).
	name, ok, err := pool.claim(sig)
	if err != nil || !ok || name != "orphan" {
		t.Fatalf("claim after reconcile = (%q, %v, %v), want (\"orphan\", true, nil)", name, ok, err)
	}
	if _, ok, err := pool.claim(sig); err != nil || ok {
		t.Fatalf("claim after reconcile (2nd) = ok=%v err=%v, want false (still-live must stay busy)", ok, err)
	}
}

// TestMaybeEagerPrewarmFiresBelowCapNotOnlyAtZero exercises the eager
// pre-warm path this task adds: it must fire as soon as the idle count is
// below cap, without first waiting for a session to exhaust the pool to
// zero. Seeded with 1 idle spare against a cap of 2 — short by one, but not
// empty — which the old (idle == 0) gate would have ignored entirely.
func TestMaybeEagerPrewarmFiresBelowCapNotOnlyAtZero(t *testing.T) {
	pool := Pool{Root: t.TempDir()}
	const sig = "sig-eager"
	if _, err := pool.release(sig, "existing-spare", 2); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	var warmed bool
	maybeEagerPrewarm(pool, sig, 2, func() { warmed = true })
	if !warmed {
		t.Fatalf("expected eager pre-warm to fire when idle count (1) < cap (2), even though the pool is not empty")
	}
}

// TestMaybeEagerPrewarmSkipsAtCap checks the other side: once idle count
// already meets cap, no warming should be triggered (keeps the existing
// cap-based release behavior in removeSandboxOnExit meaningful — there's no
// point building spares release() would immediately refuse to keep).
func TestMaybeEagerPrewarmSkipsAtCap(t *testing.T) {
	pool := Pool{Root: t.TempDir()}
	const sig = "sig-full"
	for _, name := range []string{"spare-1", "spare-2"} {
		if _, err := pool.release(sig, name, 2); err != nil {
			t.Fatalf("seed pool: %v", err)
		}
	}

	var warmed bool
	maybeEagerPrewarm(pool, sig, 2, func() { warmed = true })
	if warmed {
		t.Fatalf("expected no eager pre-warm when idle count already meets cap")
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
