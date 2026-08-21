package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/session"
)

// writeReconcileSession persists a session.json under store with the given
// status/sandbox name/created-at, mirroring what an acp-wrapper (or a prior
// orch process's daemon) would have written to disk.
func writeReconcileSession(t *testing.T, store session.Store, id string, status session.Status, sandboxName string, createdAt time.Time) {
	t.Helper()
	rec := session.Session{
		ID:          id,
		SandboxName: sandboxName,
		Status:      status,
		CreatedAt:   createdAt,
		SocketPath:  store.SocketPath(id),
	}
	if err := store.Write(rec); err != nil {
		t.Fatalf("write session %s: %v", id, err)
	}
}

// waitReturns runs sess.Wait(ctx) on its own goroutine and reports whether it
// returned within dur, along with the error it returned (if it did).
func waitReturns(sess *reconciledSession, ctx context.Context, dur time.Duration) (returned bool, err error) {
	done := make(chan error, 1)
	go func() { done <- sess.Wait(ctx) }()
	select {
	case err = <-done:
		return true, err
	case <-time.After(dur):
		return false, nil
	}
}

// TestReconciledSessionWaitReapsDeadSandbox asserts that a StatusRunning
// session whose sandbox probe reports gone is reaped immediately: Wait
// returns nil and the on-disk record is removed. This is the "wrapper crashed
// or the sandbox was torn down after the fact" case an adopted session has no
// other way to notice, since the process that would normally remove the
// directory on exit is the one that died.
func TestReconciledSessionWaitReapsDeadSandbox(t *testing.T) {
	store := session.Store{Root: t.TempDir()}
	writeReconcileSession(t, store, "sess-dead", session.StatusRunning, "acp-sess-dead", time.Now())

	goneProbe := func(_ context.Context, _ string) (bool, error) { return false, nil }
	sess := newReconciledSession("sess-dead", store, goneProbe)

	returned, err := waitReturns(sess, context.Background(), 2*time.Second)
	if !returned {
		t.Fatal("Wait never returned for a session with a confirmed-dead sandbox")
	}
	if err != nil {
		t.Errorf("Wait err = %v, want nil", err)
	}
	if _, readErr := store.Read("sess-dead"); readErr == nil {
		t.Error("on-disk record was not removed after reaping")
	}
}

// TestReconciledSessionWaitKeepsTrackingAliveSandbox asserts that a
// StatusRunning session whose sandbox probe confirms alive is never reaped:
// Wait stays blocked (only ctx cancellation ends it) and the on-disk record
// survives.
func TestReconciledSessionWaitKeepsTrackingAliveSandbox(t *testing.T) {
	store := session.Store{Root: t.TempDir()}
	writeReconcileSession(t, store, "sess-alive", session.StatusRunning, "acp-sess-alive", time.Now())

	aliveProbe := func(_ context.Context, _ string) (bool, error) { return true, nil }
	sess := newReconciledSession("sess-alive", store, aliveProbe)

	ctx, cancel := context.WithCancel(context.Background())
	if returned, _ := waitReturns(sess, ctx, 150*time.Millisecond); returned {
		t.Fatal("Wait returned for a session with a confirmed-alive sandbox")
	}
	if _, readErr := store.Read("sess-alive"); readErr != nil {
		t.Errorf("on-disk record removed for a live session: %v", readErr)
	}

	cancel()
	returned, err := waitReturns(sess, ctx, 2*time.Second)
	if !returned {
		t.Fatal("Wait never returned after ctx was cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Wait err = %v, want context.Canceled", err)
	}
}

// TestReconciledSessionWaitKeepsTrackingOnProbeError asserts that an
// inconclusive probe (sbx unavailable) never reaps a StatusRunning session —
// the same conservative rule the TUI watcher applies.
func TestReconciledSessionWaitKeepsTrackingOnProbeError(t *testing.T) {
	store := session.Store{Root: t.TempDir()}
	writeReconcileSession(t, store, "sess-probeerr", session.StatusRunning, "acp-sess-probeerr", time.Now())

	errProbe := func(_ context.Context, _ string) (bool, error) { return false, context.DeadlineExceeded }
	sess := newReconciledSession("sess-probeerr", store, errProbe)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if returned, _ := waitReturns(sess, ctx, 150*time.Millisecond); returned {
		t.Fatal("Wait returned despite an inconclusive probe")
	}
	if _, readErr := store.Read("sess-probeerr"); readErr != nil {
		t.Errorf("on-disk record removed despite an inconclusive probe: %v", readErr)
	}
}

// TestReconciledSessionWaitDoesNotReapStartingWithinGrace asserts that a
// StatusStarting session whose CreatedAt is recent is NOT reaped even though
// its sandbox probe reports gone — acp-wrapper writes StatusStarting before
// it ever creates the sandbox, so a brand-new adopted session legitimately
// has no sandbox yet.
func TestReconciledSessionWaitDoesNotReapStartingWithinGrace(t *testing.T) {
	store := session.Store{Root: t.TempDir()}
	writeReconcileSession(t, store, "sess-fresh-starting", session.StatusStarting, "acp-sess-fresh-starting", time.Now())

	goneProbe := func(_ context.Context, _ string) (bool, error) { return false, nil }
	sess := newReconciledSession("sess-fresh-starting", store, goneProbe)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if returned, _ := waitReturns(sess, ctx, 150*time.Millisecond); returned {
		t.Fatal("Wait reaped a starting session within its grace period")
	}
	if _, readErr := store.Read("sess-fresh-starting"); readErr != nil {
		t.Errorf("on-disk record removed within the grace period: %v", readErr)
	}
}

// TestReconciledSessionWaitReapsStartingPastGrace asserts that a
// StatusStarting session whose CreatedAt is already past
// reconcileStartingGrace and whose sandbox probe reports gone is reaped.
func TestReconciledSessionWaitReapsStartingPastGrace(t *testing.T) {
	store := session.Store{Root: t.TempDir()}
	writeReconcileSession(t, store, "sess-stale-starting", session.StatusStarting, "acp-sess-stale-starting",
		time.Now().Add(-reconcileStartingGrace-time.Second))

	goneProbe := func(_ context.Context, _ string) (bool, error) { return false, nil }
	sess := newReconciledSession("sess-stale-starting", store, goneProbe)

	returned, err := waitReturns(sess, context.Background(), 2*time.Second)
	if !returned {
		t.Fatal("Wait never returned for a stale starting session with a confirmed-dead sandbox")
	}
	if err != nil {
		t.Errorf("Wait err = %v, want nil", err)
	}
	if _, readErr := store.Read("sess-stale-starting"); readErr == nil {
		t.Error("on-disk record was not removed after reaping a stale starting session")
	}
}
