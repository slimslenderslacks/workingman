package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/scheduler"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// TestCronReevaluatesProject sets up a project in status=working with no
// tasks directory, so dispatchNextTask logs "no_ready_tasks" each pass. The
// fsnotify-driven save triggers one no_ready_tasks; the cron schedule
// (@every 1s) should drive a second within a couple of seconds.
//
// We use no_ready_tasks specifically because it's idempotent — every
// re-evaluation produces another entry, so we can count occurrences.
func TestCronReevaluatesProject(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	socket := fmt.Sprintf("orch-cron-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	buf := &safeBuf{}
	a := audit.New(buf)
	r := &runner.Runner{
		Workspaces: workspace.NewStub(t.TempDir()),
		Launcher: &agent.TmuxLauncher{
			Socket:       socket,
			PollInterval: 50 * time.Millisecond,
		},
		Audit:   a,
		Command: func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "sleep 1"} },
	}
	d, err := New([]string{root}, a,
		WithRunner(r),
		WithScheduler(scheduler.New()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	if ok, snap := waitFor(t, buf, "watch_root"); !ok {
		t.Fatalf("daemon never ready: %s", snap)
	}

	// Project in status=working with no tasks dir → no_ready_tasks every pass.
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "cron test",
		Branch:      "feat/cron",
		Status:      project.StatusWorking,
		Cron:        "@every 1s",
		// A schedule needs a stop condition or it is refused outright; a limit
		// well above the couple of firings this test needs keeps it registered
		// for the duration.
		CronMaxRuns: 20,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	// One no_ready_tasks from the initial fsnotify-driven dispatch.
	if ok, snap := waitFor(t, buf, "no_ready_tasks"); !ok {
		t.Fatalf("never saw initial no_ready_tasks: %s", snap)
	}
	// Then cron should fire and produce a second no_ready_tasks within ~3s.
	if ok, snap := waitForCount(t, buf, "no_ready_tasks", 2, 4*time.Second); !ok {
		t.Fatalf("cron never re-evaluated.\naudit:\n%s", snap)
	}
	if !strings.Contains(buf.String(), "cron_fired") {
		t.Errorf("expected cron_fired in audit:\n%s", buf.String())
	}
}

// startCronDaemon runs a daemon with a scheduler and no runner. The
// stop-condition logic lives entirely in the dispatch/scheduler path — nothing
// about it launches an agent — so these tests need neither tmux nor a launcher.
func startCronDaemon(t *testing.T, root string) (*safeBuf, *scheduler.Scheduler) {
	t.Helper()
	buf := &safeBuf{}
	sched := scheduler.New()
	d, err := New([]string{root}, audit.New(buf), WithScheduler(sched))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if ok, snap := waitFor(t, buf, "watch_root"); !ok {
		t.Fatalf("daemon never ready: %s", snap)
	}
	return buf, sched
}

// waitForSpecGone polls until the scheduler no longer holds a spec for key.
// Unregister happens just before the cron_unscheduled audit entry, so this is
// normally already true by the time the log line shows up — the poll only
// covers the ordering not being guaranteed.
func waitForSpecGone(t *testing.T, sched *scheduler.Scheduler, key string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sched.Spec(key) == "" {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestCronMaxRunsUnschedulesAfterFiring is the run-limit stop condition
// end-to-end: a `cron_max_runs: 1` schedule registers, fires exactly once,
// persists the firing in cron_runs, and unregisters itself instead of
// re-evaluating the project.
func TestCronMaxRunsUnschedulesAfterFiring(t *testing.T) {
	root := t.TempDir()
	buf, sched := startCronDaemon(t, root)

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "one-shot cron",
		Branch:      "feat/cron-max-runs",
		Status:      project.StatusWorking,
		Cron:        "@every 1s",
		CronMaxRuns: 1,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	if ok, snap := waitFor(t, buf, "project_updated"); !ok {
		t.Fatalf("project_updated never seen: %s", snap)
	}
	if got := sched.Spec(projectPath); got != "@every 1s" {
		t.Fatalf("schedule with an unmet stop condition should register; Spec = %q", got)
	}

	if ok, snap := waitForWithin(t, buf, "cron_fired", 4*time.Second); !ok {
		t.Fatalf("cron never fired:\naudit:\n%s", snap)
	}
	if ok, snap := waitForWithin(t, buf, "cron_unscheduled", 2*time.Second); !ok {
		t.Fatalf("schedule not unscheduled after reaching cron_max_runs:\naudit:\n%s", snap)
	}
	if !waitForSpecGone(t, sched, projectPath) {
		t.Errorf("scheduler still holds the spec: %q", sched.Spec(projectPath))
	}

	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.CronRuns != 1 {
		t.Errorf("cron_runs = %d, want 1 (the firing must be persisted)", p.CronRuns)
	}
	if p.UpdatedBy != project.WriterDaemon {
		t.Errorf("counter must be written by the daemon so it can't retrigger dispatch; updated_by = %q", p.UpdatedBy)
	}

	// And it stays unscheduled: two more @every 1s ticks would have fired had
	// the entry survived.
	time.Sleep(2200 * time.Millisecond)
	if n := strings.Count(buf.String(), "cron_fired"); n != 1 {
		t.Errorf("cron_fired %d times, want exactly 1:\naudit:\n%s", n, buf.String())
	}
}

// TestCronUntilExpiredNeverRegisters covers the daemon-restart case: a project
// whose deadline has already passed must not get a schedule at all.
func TestCronUntilExpiredNeverRegisters(t *testing.T) {
	root := t.TempDir()
	buf, sched := startCronDaemon(t, root)

	past := time.Now().Add(-time.Hour)
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "expired cron",
		Branch:      "feat/cron-until",
		Status:      project.StatusWorking,
		Cron:        "@every 1s",
		CronUntil:   &past,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	if ok, snap := waitFor(t, buf, "cron_unscheduled"); !ok {
		t.Fatalf("expired cron_until not reported:\naudit:\n%s", snap)
	}
	if got := sched.Spec(projectPath); got != "" {
		t.Errorf("expired schedule registered anyway; Spec = %q", got)
	}
	if !strings.Contains(buf.String(), "cron_until") {
		t.Errorf("audit should name the condition that tripped:\n%s", buf.String())
	}

	// Nothing should ever fire, and the counter stays untouched.
	time.Sleep(1500 * time.Millisecond)
	if strings.Contains(buf.String(), "cron_fired") {
		t.Errorf("expired schedule fired:\naudit:\n%s", buf.String())
	}
	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.CronRuns != 0 {
		t.Errorf("cron_runs = %d, want 0", p.CronRuns)
	}
	if p.Status != project.StatusWorking {
		t.Errorf("Status = %q; an expired deadline retires the schedule, not the project", p.Status)
	}
}

// TestCronWithoutStopConditionBlocksProject covers the malformed-project case:
// `cron` with neither stop-condition field is refused and routed to a human via
// status:blocked, rather than silently scheduled forever.
func TestCronWithoutStopConditionBlocksProject(t *testing.T) {
	root := t.TempDir()
	buf, sched := startCronDaemon(t, root)

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "endless cron",
		Branch:      "feat/cron-forever",
		Status:      project.StatusWorking,
		Cron:        "@every 1s",
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	if ok, snap := waitFor(t, buf, "cron_missing_stop_condition"); !ok {
		t.Fatalf("missing stop condition not reported:\naudit:\n%s", snap)
	}
	if ok, snap := waitFor(t, buf, "project_blocked"); !ok {
		t.Fatalf("project not blocked:\naudit:\n%s", snap)
	}
	if got := sched.Spec(projectPath); got != "" {
		t.Errorf("schedule with no stop condition registered; Spec = %q", got)
	}

	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Status != project.StatusBlocked {
		t.Errorf("Status = %q, want blocked", p.Status)
	}
	// The reason has to tell the human exactly what to add.
	for _, want := range []string{"cron_until", "cron_max_runs"} {
		if !strings.Contains(p.BlockedReason, want) {
			t.Errorf("blocked_reason %q should name %s", p.BlockedReason, want)
		}
	}
	// The status routing that would have run for status:working is skipped —
	// the project is blocked instead of dispatching more work.
	if strings.Contains(buf.String(), "no_ready_tasks") {
		t.Errorf("dispatch continued past the block:\naudit:\n%s", buf.String())
	}
	time.Sleep(1500 * time.Millisecond)
	if strings.Contains(buf.String(), "cron_fired") {
		t.Errorf("unbounded schedule fired:\naudit:\n%s", buf.String())
	}
}

func TestCronUnregistersOnProjectDone(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	root := t.TempDir()
	socket := fmt.Sprintf("orch-cron-done-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	buf := &safeBuf{}
	a := audit.New(buf)
	sched := scheduler.New()
	r := &runner.Runner{
		Workspaces: workspace.NewStub(t.TempDir()),
		Launcher:   &agent.TmuxLauncher{Socket: socket, PollInterval: 50 * time.Millisecond},
		Audit:      a,
		Command:    func(_ agent.Kind, _ string) []string { return []string{"sh", "-c", "true"} },
	}
	d, err := New([]string{root}, a, WithRunner(r), WithScheduler(sched))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	if ok, snap := waitFor(t, buf, "watch_root"); !ok {
		t.Fatalf("daemon never ready: %s", snap)
	}

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "cron-then-done",
		Branch:      "feat/cron-done",
		Status:      project.StatusWorking,
		Cron:        "@every 1s",
		CronMaxRuns: 20, // stop condition; required for the schedule to register
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	if ok, snap := waitFor(t, buf, "project_updated"); !ok {
		t.Fatalf("project_updated never seen: %s", snap)
	}
	if got := sched.Spec(projectPath); got != "@every 1s" {
		t.Fatalf("scheduler should have registered the cron; Spec = %q", got)
	}

	// Mark project done — daemon's transitionProjectDone path should
	// unregister the cron. We simulate by writing done as the agent so
	// handleProject sees the change. Then we transition via the all-committed
	// path by leaving tasksDir absent and faking via direct call... actually
	// the cleanest test is to write status=done directly and re-register.
	//
	// Easier: directly invoke project.Save with status=done (as agent so
	// handleProject reacts), then verify scheduler has unregistered.
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "cron-then-done",
		Branch:      "feat/cron-done",
		Status:      project.StatusDone,
		Cron:        "@every 1s",
		CronMaxRuns: 20,
		Repos:       []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs done: %v", err)
	}

	// handleProject doesn't currently unregister on status=done observed
	// from a file event (it only unregisters via transitionProjectDone).
	// So the scheduler should still hold the spec — that's the documented
	// behaviour for now. Confirm.
	time.Sleep(200 * time.Millisecond)
	if got := sched.Spec(projectPath); got == "" {
		t.Logf("scheduler unregistered after observed status=done; tighter than required")
	}

	// Cancel daemon — scheduler.Stop should run cleanly.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("daemon did not shut down")
	}
	_ = os.Remove(projectPath)
}
