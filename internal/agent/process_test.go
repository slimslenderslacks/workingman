package agent

import (
	"context"
	"testing"
	"time"
)

func TestProcessLauncherValidation(t *testing.T) {
	l := &ProcessLauncher{}
	if _, err := l.Launch(context.Background(), Spec{Command: []string{"true"}}); err == nil {
		t.Error("expected error when spec.Name is empty")
	}
	if _, err := l.Launch(context.Background(), Spec{Name: "x"}); err == nil {
		t.Error("expected error when spec.Command is empty")
	}
}

func TestProcessLauncherWaitsForExit(t *testing.T) {
	l := &ProcessLauncher{}
	sess, err := l.Launch(context.Background(), Spec{Name: "ok", Command: []string{"sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if sess.Name() != "ok" {
		t.Errorf("Name = %q, want ok", sess.Name())
	}
	if err := waitWithin(t, sess, time.Second); err != nil {
		t.Errorf("Wait returned %v, want nil for a clean exit", err)
	}
}

func TestProcessLauncherWaitSurfacesExitError(t *testing.T) {
	l := &ProcessLauncher{}
	sess, err := l.Launch(context.Background(), Spec{Name: "fail", Command: []string{"sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := waitWithin(t, sess, time.Second); err == nil {
		t.Error("Wait returned nil, want the process's non-zero exit error")
	}
}

func TestProcessLauncherCloseTerminates(t *testing.T) {
	l := &ProcessLauncher{}
	sess, err := l.Launch(context.Background(), Spec{Name: "sleep", Command: []string{"sh", "-c", "sleep 30"}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After Close the process is gone, so Wait must return promptly rather than
	// blocking for the full sleep.
	if err := waitWithin(t, sess, 2*time.Second); err == nil {
		t.Error("Wait returned nil after Close, want the killed-process error")
	}
	// Close is idempotent.
	if err := sess.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestProcessLauncherCtxCancelTearsDown(t *testing.T) {
	l := &ProcessLauncher{}
	sess, err := l.Launch(context.Background(), Spec{Name: "sleep", Command: []string{"sh", "-c", "sleep 30"}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sess.Wait(ctx); err == nil {
		t.Error("Wait returned nil for a cancelled context, want ctx.Err()")
	}
	// The process should have been torn down by Wait's ctx path; a follow-up
	// Wait on a live context returns promptly because the process is already
	// gone.
	if err := waitWithin(t, sess, 2*time.Second); err == nil {
		t.Error("process still running after ctx cancel")
	}
}

// waitWithin runs sess.Wait in a goroutine and fails if it doesn't return
// within d. It returns whatever Wait returned.
func waitWithin(t *testing.T, sess Session, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- sess.Wait(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("Wait did not return within %s", d)
		return nil
	}
}
