package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/scheduler"
	"github.com/slimslenderslacks/work/internal/task"
)

// newReviewDaemon builds a scheduler-backed, runner-less daemon. Like the cron
// stop-condition tests, the PR-loop routing decisions live entirely in the
// dispatch/scheduler path — nothing here launches an agent (launchReviewAgent
// no-ops without a runner) — so these tests need neither tmux nor Run().
func newReviewDaemon(t *testing.T, root string) (*Daemon, *safeBuf, *scheduler.Scheduler) {
	t.Helper()
	buf := &safeBuf{}
	sched := scheduler.New()
	d, err := New([]string{root}, audit.New(buf), WithScheduler(sched))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, buf, sched
}

// TestTransitionProjectCompleteRoutes covers the entry decision into the
// PR-resolution loop: only an explicit signal — `review: true`, or a
// PullRequests record already stamped by an earlier review cycle — enters
// reviewing and arms the #review poll. Everything else, repos or no repos,
// goes straight to done; there is no automatic probing for a PR that might
// exist (a human starts the loop with `:review`).
func TestTransitionProjectCompleteRoutes(t *testing.T) {
	cases := []struct {
		name       string
		p          project.Project
		wantStatus project.Status
		wantPoll   bool
	}{
		{
			name:       "repos alone do not enter the loop",
			p:          project.Project{Description: "x", Branch: "b", Repos: []project.Repo{{Org: "docker", Name: "gateway"}}},
			wantStatus: project.StatusDone,
			wantPoll:   false,
		},
		{
			name:       "repoless no flag goes done",
			p:          project.Project{Description: "x", Branch: "b"},
			wantStatus: project.StatusDone,
			wantPoll:   false,
		},
		{
			name:       "review flag forces loop even repoless",
			p:          project.Project{Description: "x", Branch: "b", Review: true},
			wantStatus: project.StatusReviewing,
			wantPoll:   true,
		},
		{
			// A fix cycle returning through here (task work finished, back to
			// AllCommitted) must keep watching the PR it already recorded, even
			// without `review: true` set.
			name: "already-known PR keeps the loop going",
			p: project.Project{
				Description: "x", Branch: "b",
				Repos:        []project.Repo{{Org: "docker", Name: "gateway"}},
				PullRequests: []project.PullRequest{{Repo: "docker/gateway", Number: 1, State: "open"}},
			},
			wantStatus: project.StatusReviewing,
			wantPoll:   true,
		},
		{
			// A live recurring cron project commits directly and opens no PR, so
			// it rests at done and re-fires on cron rather than entering the loop.
			name:       "recurring cron with repos goes done",
			p:          project.Project{Description: "x", Branch: "b", Repos: []project.Repo{{Org: "docker", Name: "gateway"}}, Cron: "0 * * * *", CronMaxRuns: 720, CronRuns: 390},
			wantStatus: project.StatusDone,
			wantPoll:   false,
		},
		{
			// The flag still opts a recurring project into the loop explicitly.
			name:       "recurring cron with review flag still reviews",
			p:          project.Project{Description: "x", Branch: "b", Repos: []project.Repo{{Org: "docker", Name: "gateway"}}, Cron: "0 * * * *", CronMaxRuns: 720, CronRuns: 390, Review: true},
			wantStatus: project.StatusReviewing,
			wantPoll:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			d, _, sched := newReviewDaemon(t, root)
			projectPath := filepath.Join(root, ".project.yaml")

			d.transitionProjectComplete(projectPath, &tc.p)

			got, err := project.Load(projectPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			spec := sched.Spec(reviewPollKey(projectPath))
			if tc.wantPoll && spec != reviewPollSpecs[0] {
				t.Errorf("review poll spec = %q, want %q", spec, reviewPollSpecs[0])
			}
			if !tc.wantPoll && spec != "" {
				t.Errorf("review poll registered for a non-review project: %q", spec)
			}
		})
	}
}

// TestReviewPollSelfHeals: a #review firing on a project that has since left
// reviewing must retire its own schedule rather than keep dispatching.
func TestReviewPollSelfHeals(t *testing.T) {
	root := t.TempDir()
	d, _, sched := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")

	// Arm the poll, then move the project off reviewing (e.g. the reconciler
	// concluded the PR was merged).
	d.ensureReviewPoll(projectPath)
	if sched.Spec(reviewPollKey(projectPath)) != reviewPollSpecs[0] {
		t.Fatalf("precondition: poll should be registered")
	}
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b", Status: project.StatusDone,
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	d.onReviewPoll(projectPath)

	if got := sched.Spec(reviewPollKey(projectPath)); got != "" {
		t.Errorf("poll survived a firing after the project left reviewing: %q", got)
	}
}

// TestAfterReviewSessionCleanResets: a reconciler run that leaves the project in
// reviewing (PR clean this cycle) clears the fix-churn counter and keeps
// polling — the loop must not converge toward the runaway guard while idle.
func TestAfterReviewSessionCleanResets(t *testing.T) {
	root := t.TempDir()
	d, buf, _ := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b", Status: project.StatusReviewing,
		Repos: []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	// Pretend prior cycles churned.
	d.bumpReviewFixCycles(projectPath)
	d.bumpReviewFixCycles(projectPath)

	d.afterReviewSession(projectPath, nil)

	if !strings.Contains(buf.String(), "review_clean") {
		t.Errorf("expected review_clean in audit:\n%s", buf.String())
	}
	// Counter reset: the next bump starts from 1 again.
	if n := d.bumpReviewFixCycles(projectPath); n != 1 {
		t.Errorf("fix-cycle counter = %d after a clean poll, want reset to 0 (bump→1)", n)
	}
}

// TestReviewCrashDoesNotReadAsCleanAndBlocksAfterMax pins the bug the live run
// exposed: a review agent whose process fails (e.g. its sandbox can't be created
// because the github MCP needs auth) leaves the project in reviewing, and must
// NOT be counted as a clean cycle. Instead it is logged as review_error and, once
// the crashes persist, the project is blocked so the user is told the loop is
// stuck rather than it retrying silently forever.
func TestReviewCrashDoesNotReadAsCleanAndBlocksAfterMax(t *testing.T) {
	root := t.TempDir()
	d, buf, _ := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b", Status: project.StatusReviewing,
		Repos: []project.Repo{{Org: "docker", Name: "gateway"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	crash := errors.New("exit status 1")
	for i := 0; i < maxReviewErrors; i++ {
		d.afterReviewSession(projectPath, crash)
	}

	if strings.Contains(buf.String(), "review_clean") {
		t.Errorf("a crashed review agent was misread as clean:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "review_error") {
		t.Errorf("expected review_error in audit:\n%s", buf.String())
	}
	got, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != project.StatusBlocked {
		t.Errorf("status = %q after %d crashes, want blocked", got.Status, maxReviewErrors)
	}
}

// TestReviewPollBacksOffWhenQuietAndResetsOnChange exercises the change-detection
// gate and adaptive backoff together (via the fingerprint seam, so no gh/network
// is needed): an unchanged PR fingerprint steps the poll to a slower cadence
// without launching the agent, and a changed fingerprint snaps it back to the
// fast cadence and reconciles.
func TestReviewPollBacksOffWhenQuietAndResetsOnChange(t *testing.T) {
	root := t.TempDir()
	d, buf, sched := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b", Status: project.StatusReviewing,
		Repos:        []project.Repo{{Org: "docker", Name: "gateway"}},
		PullRequests: []project.PullRequest{{Repo: "docker/gateway", Number: 1}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	// Fingerprint seam: return whatever `fp` currently holds.
	fp := "same"
	d.reviewFingerprintFn = func(string, *project.Project) (string, bool) { return fp, true }

	// Establish the baseline the way a clean reconcile does.
	d.afterReviewSession(projectPath, nil)
	if got, ok := d.lastReviewFinger(projectPath); !ok || got != "same" {
		t.Fatalf("baseline fingerprint = (%q,%v), want (same,true)", got, ok)
	}
	d.ensureReviewPoll(projectPath) // arm at base cadence
	if sched.Spec(reviewPollKey(projectPath)) != reviewPollSpecs[0] {
		t.Fatalf("poll should start at the fast cadence")
	}

	// Quiet ticks: fingerprint unchanged → back off one rung each, no agent run.
	for i := 1; i < len(reviewPollSpecs); i++ {
		d.onReviewPoll(projectPath)
		if got := sched.Spec(reviewPollKey(projectPath)); got != reviewPollSpecs[i] {
			t.Fatalf("after quiet tick %d, cadence = %q, want %q", i, got, reviewPollSpecs[i])
		}
	}
	if !strings.Contains(buf.String(), "review_poll_quiet") {
		t.Errorf("expected review_poll_quiet in audit:\n%s", buf.String())
	}
	// Capped: another quiet tick stays at the slowest rung.
	d.onReviewPoll(projectPath)
	if got := sched.Spec(reviewPollKey(projectPath)); got != reviewPollSpecs[len(reviewPollSpecs)-1] {
		t.Errorf("cadence past cap = %q, want %q", got, reviewPollSpecs[len(reviewPollSpecs)-1])
	}

	// A change snaps back to the fast cadence and fires the agent.
	fp = "changed"
	d.onReviewPoll(projectPath)
	if got := sched.Spec(reviewPollKey(projectPath)); got != reviewPollSpecs[0] {
		t.Errorf("after change, cadence = %q, want fast %q", got, reviewPollSpecs[0])
	}
	if !strings.Contains(buf.String(), "review_poll_fired") {
		t.Errorf("expected review_poll_fired after a detected change:\n%s", buf.String())
	}
}

// TestReviewNowKickForcesImmediateReconcile pins the `:review` kick on an
// already-reviewing project: a ReviewNow request must clear the flag (so it's
// one-shot and the daemon's own clearing write can't re-fire it), snap a
// backed-off poll cadence back to the fast rung, and skip the fingerprint gate
// so the reconcile happens now instead of at the next (slow) tick.
func TestReviewNowKickForcesImmediateReconcile(t *testing.T) {
	root := t.TempDir()
	d, buf, sched := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")
	if err := project.SaveAs(projectPath, &project.Project{
		Description: "x", Branch: "b", Status: project.StatusReviewing,
		Repos:     []project.Repo{{Org: "docker", Name: "gateway"}},
		ReviewNow: true,
	}, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	// Arm the poll and back it all the way off, as a long-quiet PR would be.
	d.ensureReviewPoll(projectPath)
	for i := 1; i < len(reviewPollSpecs); i++ {
		d.growReviewBackoff(projectPath)
	}
	d.rescheduleReviewPoll(projectPath, reviewPollSpecs[len(reviewPollSpecs)-1])

	d.handleProject(projectPath)

	// The one-shot flag is cleared (as the daemon, so it can't retrigger).
	got, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ReviewNow {
		t.Errorf("ReviewNow still set; the kick must clear it")
	}
	if got.UpdatedBy != project.WriterDaemon {
		t.Errorf("clear written by %q, want daemon (so it doesn't re-fire)", got.UpdatedBy)
	}
	// Cadence snapped back to fast, and the forced reconcile was logged.
	if spec := sched.Spec(reviewPollKey(projectPath)); spec != reviewPollSpecs[0] {
		t.Errorf("poll cadence = %q, want fast %q after a kick", spec, reviewPollSpecs[0])
	}
	if !strings.Contains(buf.String(), "review_poll_forced") {
		t.Errorf("expected review_poll_forced in audit:\n%s", buf.String())
	}
}

// TestReviewPRFingerprintNoPRs pins the fingerprint gate's guards: with no PRs
// to watch (or only entries missing a repo/number) it reports ok=false without
// shelling gh, so onReviewPoll falls through to running the agent rather than
// treating the project as quiet.
func TestReviewPRFingerprintNoPRs(t *testing.T) {
	root := t.TempDir()
	d, _, _ := newReviewDaemon(t, root)
	path := filepath.Join(root, ".project.yaml")

	if _, ok := d.reviewPRFingerprint(path, &project.Project{}); ok {
		t.Errorf("fingerprint ok=true with no PRs, want false")
	}
	if _, ok := d.reviewPRFingerprint(path, &project.Project{PullRequests: []project.PullRequest{{}}}); ok {
		t.Errorf("fingerprint ok=true with an unusable PR entry, want false")
	}
}

// TestReviewFixCyclesBlockAfterMax: a PR that keeps generating fix work without
// ever coming back clean must eventually stop and hand off to the wolf, rather
// than churning forever.
func TestReviewFixCyclesBlockAfterMax(t *testing.T) {
	root := t.TempDir()
	d, buf, _ := newReviewDaemon(t, root)
	projectPath := filepath.Join(root, ".project.yaml")

	// A ready task keeps dispatchNextTask (reached via revisitProject on the
	// working branch) a no-op under the runner-less daemon while leaving the
	// project in status:working.
	tasksDir := filepath.Join(root, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	if err := task.Save(filepath.Join(tasksDir, "fix.yaml"), &task.Task{Name: "fix", Status: task.StatusReady}); err != nil {
		t.Fatalf("save task: %v", err)
	}

	writeWorking := func() {
		if err := project.SaveAs(projectPath, &project.Project{
			Description: "x", Branch: "b", Status: project.StatusWorking,
			Repos: []project.Repo{{Org: "docker", Name: "gateway"}},
		}, project.WriterAgent); err != nil {
			t.Fatalf("SaveAs working: %v", err)
		}
	}

	// Each afterReviewSession that finds the project in working counts one fix
	// cycle. Drive maxReviewFixCycles+1 of them; the last exceeds the bound.
	for i := 0; i <= maxReviewFixCycles; i++ {
		writeWorking()
		d.afterReviewSession(projectPath, nil)
	}

	got, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != project.StatusBlocked {
		t.Fatalf("status = %q after %d fix cycles, want blocked", got.Status, maxReviewFixCycles+1)
	}
	if !strings.Contains(buf.String(), "project_blocked") {
		t.Errorf("expected project_blocked in audit:\n%s", buf.String())
	}
	if !strings.Contains(got.BlockedReason, "converging") {
		t.Errorf("blocked_reason = %q, want it to explain the non-convergence", got.BlockedReason)
	}
}
