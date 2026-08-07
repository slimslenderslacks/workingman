package runner

import (
	"context"
	"reflect"
	"testing"
)

func TestLaunchInteractiveEnsuresSandboxAndWrapsCommand(t *testing.T) {
	launcher := &fakeLauncher{}
	var gotSpec SandboxSpec
	r := &Runner{
		Launcher: launcher,
		SbxPath:  "/bin/sbx",
		// No existing sandbox, so LaunchInteractive takes the create path.
		SandboxExists: func(context.Context, string) (bool, error) { return false, nil },
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

// TestLaunchInteractiveReusesExistingSandbox covers the `:session` reopen path:
// when a sandbox by the requested name already exists (e.g. it was created on a
// previous open and later stopped), LaunchInteractive must reuse it with
// `sbx run --name` rather than creating another one — re-passing the agent args
// so claude resumes the same conversation.
func TestLaunchInteractiveReusesExistingSandbox(t *testing.T) {
	launcher := &fakeLauncher{}
	created := false
	r := &Runner{
		Launcher:      launcher,
		SbxPath:       "/bin/sbx",
		SandboxExists: func(context.Context, string) (bool, error) { return true, nil },
		Sandbox:       func(context.Context, SandboxSpec) error { created = true; return nil },
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
	if created {
		t.Error("reusing an existing sandbox must not create another one")
	}

	// The launched command runs the existing sandbox with `sbx run --name`,
	// re-passing the agent args after `--`.
	wantCmd := []string{"/bin/sbx", "run", "--name", "session-widget", "--", "--session-id", "abc"}
	if !reflect.DeepEqual(launcher.last.Command, wantCmd) {
		t.Errorf("command = %v, want %v", launcher.last.Command, wantCmd)
	}
	if target != "session-widget" {
		t.Errorf("target = %q, want session-widget", target)
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
