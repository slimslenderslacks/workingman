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
