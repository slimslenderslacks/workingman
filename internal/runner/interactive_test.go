package runner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

var errFakeNoKey = errors.New("exit status 1")

func TestLaunchInteractiveEnsuresSandboxAndWrapsCommand(t *testing.T) {
	launcher := &fakeLauncher{}
	var gotSpec SandboxSpec
	r := &Runner{
		Launcher: launcher,
		SbxPath:  "/bin/sbx",
		// Inject a sandbox creator so the test doesn't shell out to real sbx.
		Sandbox: func(_ context.Context, spec SandboxSpec) error {
			gotSpec = spec
			return nil
		},
	}

	target, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		SandboxName: "session-widget",
		Workspaces:  []string{"/ws/widget", "/orch/widget"},
		Cwd:         "/ws/widget",
		Inner:       []string{"claude", "--session-id", "abc"},
		WindowName:  "session-widget",
	})
	if err != nil {
		t.Fatalf("LaunchInteractive: %v", err)
	}

	// The sandbox was ensured with exactly the requested name + mounts.
	if gotSpec.Name != "session-widget" {
		t.Errorf("ensured sandbox name = %q, want session-widget", gotSpec.Name)
	}
	if !reflect.DeepEqual(gotSpec.Workspaces, []string{"/ws/widget", "/orch/widget"}) {
		t.Errorf("ensured workspaces = %v", gotSpec.Workspaces)
	}

	// The launched command wraps the inner command in `sbx exec -it -w <cwd>`.
	wantCmd := []string{"/bin/sbx", "exec", "-it", "-w", "/ws/widget", "session-widget", "claude", "--session-id", "abc"}
	if !reflect.DeepEqual(launcher.last.Command, wantCmd) {
		t.Errorf("command = %v, want %v", launcher.last.Command, wantCmd)
	}
	if launcher.last.Name != "session-widget" {
		t.Errorf("window name = %q, want session-widget", launcher.last.Name)
	}
	if launcher.last.Workspace != "/ws/widget" {
		t.Errorf("window workspace = %q, want /ws/widget", launcher.last.Workspace)
	}
	if target != "session-widget" {
		t.Errorf("target = %q, want the launched session name", target)
	}
}

// TestLaunchInteractiveHostShell covers the `:dir` path: an empty SandboxName
// runs the command directly on the host — no sandbox ensured, no `sbx exec`
// wrapping — in the given cwd.
func TestLaunchInteractiveHostShell(t *testing.T) {
	launcher := &fakeLauncher{}
	ensured := false
	r := &Runner{
		Launcher: launcher,
		SbxPath:  "/bin/sbx",
		Sandbox:  func(context.Context, SandboxSpec) error { ensured = true; return nil },
	}

	target, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		Cwd:        "/ws/widget",
		Inner:      []string{"bash"},
		WindowName: "shell-widget",
	})
	if err != nil {
		t.Fatalf("LaunchInteractive: %v", err)
	}
	if ensured {
		t.Error("host shell must not create a sandbox")
	}
	if want := []string{"bash"}; !reflect.DeepEqual(launcher.last.Command, want) {
		t.Errorf("command = %v, want %v (no sbx wrapping)", launcher.last.Command, want)
	}
	if launcher.last.Workspace != "/ws/widget" {
		t.Errorf("window cwd = %q, want /ws/widget", launcher.last.Workspace)
	}
	if target != "shell-widget" {
		t.Errorf("target = %q, want shell-widget", target)
	}
}

// TestLaunchInteractiveInjectsSigning covers the `:session` fix: with a git
// identity and a signing key set — and a preflight that finds a key in the
// forwarded agent — the wrapped `sbx exec` carries the GIT_AUTHOR_*/COMMITTER_*
// identity env AND the GIT_CONFIG_* signing env, so commits in the window are
// attributed and SSH-signed just like the ACP commit agent.
func TestLaunchInteractiveInjectsSigning(t *testing.T) {
	launcher := &fakeLauncher{}
	r := &Runner{
		Launcher: launcher,
		SbxPath:  "/bin/sbx",
		Sandbox:  func(context.Context, SandboxSpec) error { return nil },
		// Preflight finds a key → signing stays on.
		preflightRun: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("256 SHA256:abc key (ED25519)\n"), nil
		},
	}

	_, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		SandboxName: "session-widget",
		Workspaces:  []string{"/ws/widget", "/orch/widget"},
		Cwd:         "/ws/widget",
		Inner:       []string{"claude", "--session-id", "abc"},
		WindowName:  "session-widget",
		GitName:     "Jim Clark",
		GitEmail:    "jim@example.com",
		SigningKey:  "ssh-ed25519 AAAAKEY",
	})
	if err != nil {
		t.Fatalf("LaunchInteractive: %v", err)
	}

	want := []string{
		"/bin/sbx", "exec", "-it", "-w", "/ws/widget",
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
		"session-widget", "claude", "--session-id", "abc",
	}
	if !reflect.DeepEqual(launcher.last.Command, want) {
		t.Errorf("command = %v\nwant %v", launcher.last.Command, want)
	}
}

// TestLaunchInteractiveSigningDegradesWhenAgentEmpty covers the safety valve:
// when the forwarded agent holds no key, forcing commit.gpgsign on would make
// every `git commit` hard-fail, so signing is dropped (no GIT_CONFIG_* env)
// while the git identity is still injected.
func TestLaunchInteractiveSigningDegradesWhenAgentEmpty(t *testing.T) {
	launcher := &fakeLauncher{}
	r := &Runner{
		Launcher: launcher,
		SbxPath:  "/bin/sbx",
		Sandbox:  func(context.Context, SandboxSpec) error { return nil },
		// `ssh-add -l` exits non-zero when the agent has no identities.
		preflightRun: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("The agent has no identities."), errFakeNoKey
		},
	}

	_, err := r.LaunchInteractive(context.Background(), InteractiveSpec{
		SandboxName: "session-widget",
		Workspaces:  []string{"/ws/widget"},
		Cwd:         "/ws/widget",
		Inner:       []string{"claude"},
		WindowName:  "session-widget",
		GitName:     "Jim Clark",
		GitEmail:    "jim@example.com",
		SigningKey:  "ssh-ed25519 AAAAKEY",
	})
	if err != nil {
		t.Fatalf("LaunchInteractive: %v", err)
	}

	for _, arg := range launcher.last.Command {
		if strings.HasPrefix(arg, "GIT_CONFIG_") {
			t.Fatalf("signing env %q injected despite an empty agent; command: %v", arg, launcher.last.Command)
		}
	}
	// Identity must still be present — only signing degrades.
	if !containsArg(launcher.last.Command, "GIT_AUTHOR_NAME=Jim Clark") {
		t.Errorf("identity env dropped along with signing; command: %v", launcher.last.Command)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestLaunchInteractiveValidates(t *testing.T) {
	r := &Runner{Launcher: &fakeLauncher{}, Sandbox: func(context.Context, SandboxSpec) error { return nil }}
	// A sandboxed session (SandboxName set) needs at least one workspace mount.
	if _, err := r.LaunchInteractive(context.Background(), InteractiveSpec{WindowName: "w", SandboxName: "s", Inner: []string{"claude"}}); err == nil {
		t.Error("expected an error with no workspaces for a sandboxed session")
	}
	// Every launch needs a command to run.
	if _, err := r.LaunchInteractive(context.Background(), InteractiveSpec{WindowName: "w", Cwd: "/ws"}); err == nil {
		t.Error("expected an error with no inner command")
	}
	noLauncher := &Runner{Sandbox: func(context.Context, SandboxSpec) error { return nil }}
	if _, err := noLauncher.LaunchInteractive(context.Background(), InteractiveSpec{WindowName: "w", Cwd: "/ws", Inner: []string{"bash"}}); err == nil {
		t.Error("expected an error with no launcher")
	}
}
