package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/scheduler"
)

// startReplanDaemon is startCronDaemon plus a handle on the Daemon itself, for
// the tests that drive a transition directly instead of waiting for one. No
// runner is wired up: every assertion here is about what lands in the project
// file and the scheduler, and launchProjectRootAgent is a no-op without a
// runner — so these tests need neither tmux nor a sandbox.
func startReplanDaemon(t *testing.T, root string) (*Daemon, *safeBuf, *scheduler.Scheduler) {
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
	return d, buf, sched
}

// waitForProject polls the project file until cond holds, so a test can wait on
// the daemon's own write rather than on an audit line that only implies it.
func waitForProject(t *testing.T, path string, dur time.Duration, cond func(*project.Project) bool) *project.Project {
	t.Helper()
	deadline := time.Now().Add(dur)
	var last *project.Project
	for time.Now().Before(deadline) {
		p, err := project.Load(path)
		if err == nil {
			last = p
			if cond(p) {
				return p
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return last
}

// TestCronFiringRequestsReplanOnDoneProject is the recurring cycle's first half:
// a finished project on a live schedule wakes up as ready-to-be-re-planned
// rather than staying done, which is what gets the planning agent dispatched
// again.
func TestCronFiringRequestsReplanOnDoneProject(t *testing.T) {
	root := t.TempDir()
	_, buf, _ := startReplanDaemon(t, root)

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "nightly triage",
		Branch:      "feat/triage",
		Status:      project.StatusDone,
		Cron:        "@every 1s",
		CronMaxRuns: 20, // stop condition; required for the schedule to register
		Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	if ok, snap := waitForWithin(t, buf, "cron_replan_requested", 4*time.Second); !ok {
		t.Fatalf("cron firing never requested a re-plan:\n%s", snap)
	}
	got := waitForProject(t, projectPath, 2*time.Second, func(p *project.Project) bool {
		return p.Status == project.StatusReady && p.Replan
	})
	if got.Status != project.StatusReady {
		t.Errorf("status = %q, want ready so the planning agent is dispatched", got.Status)
	}
	if !got.Replan {
		t.Errorf("replan flag not set: %+v", got)
	}
	// The re-plan request is daemon bookkeeping, so it must be written as the
	// daemon — an agent-marked write would re-enter dispatch through
	// handleProject on top of the revisit the firing already does.
	if got.UpdatedBy != project.WriterDaemon {
		t.Errorf("re-plan written by %q, want daemon", got.UpdatedBy)
	}
	// The firing still counts against the run limit.
	if got.CronRuns < 1 {
		t.Errorf("cron_runs = %d, want the firing counted", got.CronRuns)
	}
}

// TestCronFiringSkipsProjectsWithWorkInFlight guards the case that makes a
// blanket "cron fires → re-plan" rule unsafe: re-planning a project whose tasks
// are live would rewrite the graph out from under running agents. A working
// project must keep its status and fall through to the recovery poll it got
// before.
func TestCronFiringSkipsProjectsWithWorkInFlight(t *testing.T) {
	root := t.TempDir()
	_, buf, _ := startReplanDaemon(t, root)

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "mid-flight",
		Branch:      "feat/inflight",
		Status:      project.StatusWorking,
		Cron:        "@every 1s",
		CronMaxRuns: 20,
		Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	if ok, snap := waitForWithin(t, buf, "cron_replan_skipped", 4*time.Second); !ok {
		t.Fatalf("working project should have been skipped for re-planning:\n%s", snap)
	}
	if s := buf.String(); strings.Contains(s, "cron_replan_requested") {
		t.Errorf("re-plan requested on a working project:\n%s", s)
	}
	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.Status != project.StatusWorking {
		t.Errorf("status = %q, want it left at working", p.Status)
	}
	if p.Replan {
		t.Errorf("replan set on a project with work in flight: %+v", p)
	}
}

// TestCronSkippedFiringIsNotCounted is the second half of the run-accounting
// fix. A firing that starts no cycle used to increment cron_runs anyway, so a
// wake-up landing while the previous cycle was still running silently ate one
// of the user's N cycles — the other half of the reported "28 of 30".
//
// The skip must still poke the project (the recovery poll
// TestCronFiringSkipsProjectsWithWorkInFlight pins down) and must say so in the
// audit log, so the next such shortfall is diagnosable from the log alone.
func TestCronSkippedFiringIsNotCounted(t *testing.T) {
	root := t.TempDir()
	_, buf, sched := startReplanDaemon(t, root)

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "busy through its window",
		Branch:      "feat/uncounted",
		Status:      project.StatusWorking,
		Cron:        "@every 1s",
		CronMaxRuns: 2,
		Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	// Enough firings to have exhausted a 2-run budget under the old accounting.
	if ok, snap := waitForCount(t, buf, "cron_run_not_counted", 3, 6*time.Second); !ok {
		t.Fatalf("skipped firings not reported as uncounted:\naudit:\n%s", snap)
	}
	if s := buf.String(); !strings.Contains(s, "status working is not idle") {
		t.Errorf("cron_run_not_counted should carry the skip reason:\n%s", s)
	}
	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.CronRuns != 0 {
		t.Errorf("cron_runs = %d, want 0 — firings that start no cycle don't spend the budget", p.CronRuns)
	}
	// Deliberate tradeoff: the schedule outlives cron_max_runs while the project
	// stays non-idle. cron_until, not a hidden firing cap, is the bound there.
	if got := sched.Spec(projectPath); got != "@every 1s" {
		t.Errorf("schedule dropped without ever running a cycle; Spec = %q", got)
	}
	// Uncounted or not, the wake-up still re-evaluates the project.
	if s := buf.String(); !strings.Contains(s, "no_ready_tasks") {
		t.Errorf("skipped firing did not poke the project:\n%s", s)
	}
}

// TestCronFiringSkipsProjectBeingArchived covers the other skip: a cleanup in
// flight (or already finished) means the work stream is being wound down, and a
// planning run would dirty the workspace the archive agent just left clean.
func TestCronFiringSkipsProjectBeingArchived(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*project.Project)
	}{
		{"cleanup requested", func(p *project.Project) { p.Cleanup = true }},
		{"already archived", func(p *project.Project) { p.Archive = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			_, buf, _ := startReplanDaemon(t, root)

			projectPath := filepath.Join(root, ".project.yaml")
			p := &project.Project{
				Description: "winding down",
				Branch:      "feat/done",
				Status:      project.StatusDone,
				Cron:        "@every 1s",
				CronMaxRuns: 20,
				Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
			}
			tc.mutate(p)
			if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
				t.Fatalf("SaveAs: %v", err)
			}

			if ok, snap := waitForWithin(t, buf, "cron_replan_skipped", 4*time.Second); !ok {
				t.Fatalf("expected the firing to skip re-planning:\n%s", snap)
			}
			got, err := project.Load(projectPath)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got.Replan {
				t.Errorf("replan set on a project being archived: %+v", got)
			}
			if got.Status != project.StatusDone {
				t.Errorf("status = %q, want it left at done", got.Status)
			}
		})
	}
}

// TestProjectDoneKeepsLiveCronSchedule is the other half of the cycle. The
// done-transition used to unregister unconditionally, which killed the schedule
// on the first completion and made every cron project a one-shot: nothing
// re-registered it while the finished file sat untouched.
func TestProjectDoneKeepsLiveCronSchedule(t *testing.T) {
	root := t.TempDir()
	d, buf, sched := startReplanDaemon(t, root)

	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Description: "recurring",
		Branch:      "feat/recurring",
		Status:      project.StatusWorking,
		Cron:        "@every 1h", // long enough not to fire during the test
		CronMaxRuns: 20,
		Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
	}
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if ok, snap := waitFor(t, buf, "project_updated"); !ok {
		t.Fatalf("project_updated never seen: %s", snap)
	}
	if got := sched.Spec(projectPath); got != "@every 1h" {
		t.Fatalf("cron should be registered before the transition; Spec = %q", got)
	}

	d.transitionProjectDone(projectPath, p)

	if got := sched.Spec(projectPath); got != "@every 1h" {
		t.Errorf("schedule dropped on done; Spec = %q, want it kept so the next firing re-plans", got)
	}
	if ok, snap := waitFor(t, buf, "cron_kept_on_done"); !ok {
		t.Errorf("expected cron_kept_on_done in the audit log:\n%s", snap)
	}
}

// TestProjectDoneUnregistersExpiredCron is the complement: the stop condition,
// not the done-transition, is what ends a schedule — so a project finishing with
// an exhausted run limit still gets unregistered.
func TestProjectDoneUnregistersExpiredCron(t *testing.T) {
	root := t.TempDir()
	d, buf, sched := startReplanDaemon(t, root)

	projectPath := filepath.Join(root, ".project.yaml")
	live := &project.Project{
		Description: "last cycle",
		Branch:      "feat/last",
		Status:      project.StatusWorking,
		Cron:        "@every 1h",
		CronMaxRuns: 5,
		Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
	}
	if err := project.SaveAs(projectPath, live, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if ok, snap := waitFor(t, buf, "project_updated"); !ok {
		t.Fatalf("project_updated never seen: %s", snap)
	}
	if got := sched.Spec(projectPath); got == "" {
		t.Fatalf("cron should be registered before the transition")
	}

	// Same project, run limit now reached.
	spent := *live
	spent.CronRuns = spent.CronMaxRuns
	d.transitionProjectDone(projectPath, &spent)

	if !waitForSpecGone(t, sched, projectPath) {
		t.Errorf("expired schedule survived the done transition; Spec = %q", sched.Spec(projectPath))
	}
}

// TestReplanClearedWhenPlanningAdvancesProject pins the flag's lifecycle. If a
// satisfied request stayed on disk, the next planning run — the incremental one
// a human triggers by adding a single task — would read itself as a re-plan and
// be told it may delete their existing tasks.
func TestReplanClearedWhenPlanningAdvancesProject(t *testing.T) {
	root := t.TempDir()
	d, buf, _ := startReplanDaemon(t, root)

	projectPath := filepath.Join(root, ".project.yaml")
	// The state a planning agent leaves behind at the end of a re-plan cycle:
	// advanced to working, with the request it consumed still set.
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "replanned",
		Branch:      "feat/replanned",
		Status:      project.StatusWorking,
		Replan:      true,
		Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if ok, snap := waitFor(t, buf, "project_updated"); !ok {
		t.Fatalf("project_updated never seen: %s", snap)
	}

	d.afterPlanningSession(projectPath, nil)

	got := waitForProject(t, projectPath, 2*time.Second, func(p *project.Project) bool {
		return !p.Replan
	})
	if got.Replan {
		t.Errorf("replan flag survived a productive planning session: %+v", got)
	}
	if got.Status != project.StatusWorking {
		t.Errorf("status = %q, want working — clearing the flag must not disturb it", got.Status)
	}
}

// TestReplanSurvivesUnproductivePlanningSession is the other side of that
// lifecycle: a planning session that ended without advancing the project still
// owes the re-plan, so the flag has to stay for the retry.
func TestReplanSurvivesUnproductivePlanningSession(t *testing.T) {
	root := t.TempDir()
	d, buf, _ := startReplanDaemon(t, root)

	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "planning made no progress",
		Branch:      "feat/stuck",
		Status:      project.StatusReady,
		Replan:      true,
		Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if ok, snap := waitFor(t, buf, "project_updated"); !ok {
		t.Fatalf("project_updated never seen: %s", snap)
	}

	d.afterPlanningSession(projectPath, nil)

	p, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !p.Replan {
		t.Errorf("replan flag cleared without the project advancing: %+v", p)
	}
}
