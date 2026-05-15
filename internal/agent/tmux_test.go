package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Each test gets its own tmux server (via -L) and a unique session name so
// parallel runs and the user's real sessions are never touched. The server
// is torn down in cleanup.
func newTestLauncher(t *testing.T) (*TmuxLauncher, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("orch-test-%d-%d", time.Now().UnixNano(), randSeq())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})
	return &TmuxLauncher{
		Socket:       socket,
		PollInterval: 50 * time.Millisecond,
	}, socket
}

func TestLaunchSleepCompletes(t *testing.T) {
	l, _ := newTestLauncher(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := l.Launch(ctx, Spec{
		Kind:    TaskAgent,
		Name:    "sleeper",
		Command: []string{"sh", "-c", "sleep 1"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := sess.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if alive := sess.(*tmuxSession).exists(); alive {
		t.Errorf("session still exists after Wait returned")
	}
}

func TestCloseTerminatesRunningSession(t *testing.T) {
	l, _ := newTestLauncher(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := l.Launch(ctx, Spec{
		Kind:    TaskAgent,
		Name:    "long-sleep",
		Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !sess.(*tmuxSession).exists() {
		t.Fatalf("session should be alive immediately after launch")
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if sess.(*tmuxSession).exists() {
		t.Errorf("session still exists after Close")
	}
	// Second Close is a no-op.
	if err := sess.Close(); err != nil {
		t.Errorf("idempotent Close returned error: %v", err)
	}
}

func TestWorkspaceIsHonored(t *testing.T) {
	l, _ := newTestLauncher(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	marker := dir + "/.cwd.txt"
	sess, err := l.Launch(ctx, Spec{
		Kind:      TaskAgent,
		Name:      "pwd",
		Workspace: dir,
		// Write CWD into a file inside the workspace, then exit.
		Command: []string{"sh", "-c", "pwd > .cwd.txt"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := sess.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	got, err := readFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(got), dir) {
		t.Errorf("pwd = %q, want prefix %q", got, dir)
	}
}

func TestLaunchRejectsEmptySpec(t *testing.T) {
	l, _ := newTestLauncher(t)
	ctx := context.Background()
	if _, err := l.Launch(ctx, Spec{Command: []string{"sh", "-c", "true"}}); err == nil {
		t.Error("expected error for empty Name")
	}
	if _, err := l.Launch(ctx, Spec{Name: "x"}); err == nil {
		t.Error("expected error for empty Command")
	}
}

func TestMultipleAgentsShareUmbrellaSession(t *testing.T) {
	l, _ := newTestLauncher(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Two long-running agents launched back-to-back. Both should end up as
	// windows inside the same umbrella session, not as two separate sessions.
	a, err := l.Launch(ctx, Spec{
		Kind:    TaskAgent,
		Name:    "alpha",
		Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Launch alpha: %v", err)
	}
	b, err := l.Launch(ctx, Spec{
		Kind:    TaskAgent,
		Name:    "bravo",
		Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Launch bravo: %v", err)
	}
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	// Name() now returns "session:window" — both windows live under the
	// same session name.
	if want, got := DefaultUmbrellaSession+":alpha", a.Name(); got != want {
		t.Errorf("alpha.Name() = %q, want %q", got, want)
	}
	if want, got := DefaultUmbrellaSession+":bravo", b.Name(); got != want {
		t.Errorf("bravo.Name() = %q, want %q", got, want)
	}

	// Both windows must be present in the umbrella session at the same
	// time — proving that the second Launch added a window rather than
	// failing on "duplicate session".
	if !a.(*tmuxSession).exists() || !b.(*tmuxSession).exists() {
		t.Fatalf("both windows should be live in umbrella session")
	}

	// Killing one window must leave the other alive — proving that
	// kill-window is scoped to a single window, not the whole session.
	if err := a.Close(); err != nil {
		t.Fatalf("Close alpha: %v", err)
	}
	if a.(*tmuxSession).exists() {
		t.Errorf("alpha window still present after Close")
	}
	if !b.(*tmuxSession).exists() {
		t.Errorf("bravo window was killed when alpha closed; sharing model broken")
	}
}

func TestSessionNameOverride(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("orch-test-%d-%d", time.Now().UnixNano(), randSeq())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	l := &TmuxLauncher{
		Socket:       socket,
		SessionName:  "custom-umbrella",
		PollInterval: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := l.Launch(ctx, Spec{
		Kind:    TaskAgent,
		Name:    "x",
		Command: []string{"sh", "-c", "sleep 1"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got, want := sess.Name(), "custom-umbrella:x"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	// Confirm the umbrella session — not the default — is what tmux sees.
	if err := exec.Command("tmux", "-L", socket, "has-session", "-t", "custom-umbrella").Run(); err != nil {
		t.Errorf("custom umbrella session not created: %v", err)
	}
	_ = sess.Close()
}
