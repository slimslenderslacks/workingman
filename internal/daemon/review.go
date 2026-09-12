package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
)

// reviewPollSpecs is the adaptive backoff ladder for the #review poll: a project
// entering reviewing polls at the fastest cadence (checks are running, comments
// likely), and each quiet tick (the PR fingerprint unchanged since the last
// reconcile) steps one rung slower, capped at the last entry. Any detected
// change snaps it back to the front. This keeps a busy PR responsive while a
// quiet one stops waking the daemon every couple of minutes.
var reviewPollSpecs = []string{"@every 2m", "@every 5m", "@every 15m", "@every 30m"}

// transitionProjectComplete decides what "all tasks committed" means for a
// project. A project that produced (or is expected to produce) a pull request
// isn't finished when the code lands — reviewers and GitHub Actions keep acting
// on the PR — so it enters the PR-resolution loop (status:reviewing) instead of
// going straight to done.
//
// The trigger is "flag + probe": Review forces the loop; absent it, any project
// with repos still gets a one-time PR probe (the review agent checks for an open
// PR and goes done immediately if there is none). A repo-less project can't have
// a PR, so it keeps the original terminal behaviour.
//
// A live recurring cron project is the exception: it rests at `done` between
// firings and does its work by committing directly (it opens no PR), so the
// probe would fire on EVERY cycle — spinning up a review agent each period only
// to find no PR and return to done (the EmailPromotionSummary oscillation). Its
// repos are not a PR signal, so the probe is skipped; such a project enters the
// review loop only when it explicitly asks via `review: true`. An expired
// schedule is treated as a one-shot again and keeps the probe.
func (d *Daemon) transitionProjectComplete(projectPath string, p *project.Project) {
	// The PR-resolution loop is scheduler-driven — it polls the PR on a cadence —
	// so, like cron, it can only run when the daemon has a scheduler. Production
	// always wires one; a scheduler-less daemon (dev/tests) keeps the original
	// terminal behaviour instead of stranding the project in reviewing.
	recurring := p.Cron != "" && !p.CronExpired()
	if d.scheduler != nil && (p.Review || (p.HasRepos() && !recurring)) {
		d.transitionProjectReviewing(projectPath, p)
		return
	}
	d.transitionProjectDone(projectPath, p)
}

// transitionProjectReviewing writes status:reviewing (as the daemon, so it does
// not retrigger dispatch), registers the project's #review poll, and kicks an
// immediate review-agent run so the first (or post-fix) reconcile doesn't wait a
// full poll interval.
func (d *Daemon) transitionProjectReviewing(projectPath string, p *project.Project) {
	updated := *p
	updated.Status = project.StatusReviewing
	if err := project.Save(projectPath, &updated); err != nil {
		d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
		return
	}
	d.audit.Log("project_reviewing", "path", projectPath, "review", fmt.Sprintf("%t", updated.Review))
	// ensureReviewPoll arms the schedule and kicks the first poll immediately.
	d.ensureReviewPoll(projectPath)
}

// ensureReviewPoll arms the repeating PR poll for projectPath and, when it is
// the call that arms it, kicks the first review agent immediately rather than
// waiting a full interval. "The call that arms it" covers every way a project
// enters the loop: the automatic completion transition, a human's `:review`
// (which writes status:reviewing and lands here via the fsnotify path), and a
// daemon restart onto a project already in reviewing.
//
// It is idempotent: an observation of a project already being polled returns
// without re-arming or re-dispatching, so it is safe to call on every reviewing
// observation and — importantly — cannot re-dispatch on the review agent's own
// clean-leave write (which keeps the project in reviewing between cycles).
func (d *Daemon) ensureReviewPoll(projectPath string) {
	if d.scheduler == nil {
		return
	}
	key := reviewPollKey(projectPath)
	if d.scheduler.Spec(key) != "" {
		return // already being polled (at whatever backoff rung it has reached)
	}
	if err := d.scheduler.Register(key, reviewPollSpecs[0], func() { d.onReviewPoll(projectPath) }); err != nil {
		d.audit.Log("review_poll_register_error", "path", projectPath, "spec", reviewPollSpecs[0], "err", err.Error())
		return
	}
	p, err := project.Load(projectPath)
	if err != nil {
		// No file to poll (e.g. armed before the project was written): the
		// schedule will self-heal on its first firing via onReviewPoll.
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		}
		return
	}
	d.dispatchReviewAgent(projectPath, p)
}

// onReviewPoll is the #review schedule callback. It re-reads the project (the
// file changes under a long-lived poll) and self-heals: if the project has left
// reviewing since the poll was registered, it unregisters itself.
//
// Otherwise it applies the change-detection gate before spending the expensive
// review agent: a cheap host-side `gh` fingerprint of the PR (head SHA +
// updatedAt + check-rollup) stands in for "did anything happen." When the
// fingerprint matches the last reconcile, the tick is quiet — the poll steps to
// a slower cadence and does NOT launch the agent. A changed (or not-yet-known)
// fingerprint runs the agent and snaps the cadence back to the front.
func (d *Daemon) onReviewPoll(projectPath string) {
	if d.scheduler == nil {
		return
	}
	p, err := project.Load(projectPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		}
		d.scheduler.Unregister(reviewPollKey(projectPath))
		return
	}
	if p.Status != project.StatusReviewing {
		d.scheduler.Unregister(reviewPollKey(projectPath))
		return
	}
	if d.hasSession(projectPath) {
		// A review (or other project-root) agent is mid-run; skip this tick
		// without disturbing the cadence.
		d.audit.Log("review_poll_skip_busy", "path", projectPath)
		return
	}
	if fp, ok := d.reviewFingerprint(projectPath, p); ok {
		if prev, seen := d.lastReviewFinger(projectPath); seen && prev == fp {
			spec := d.growReviewBackoff(projectPath)
			d.rescheduleReviewPoll(projectPath, spec)
			d.audit.Log("review_poll_quiet", "path", projectPath, "next", spec)
			return
		}
	}
	// Changed, no baseline yet, no PR to fingerprint, or gh unavailable: reconcile
	// now and return to the fast cadence.
	d.resetReviewBackoff(projectPath)
	d.rescheduleReviewPoll(projectPath, reviewPollSpecs[0])
	d.audit.Log("review_poll_fired", "path", projectPath)
	d.dispatchReviewAgent(projectPath, p)
}

// forceReviewNow honors a `:review` kick on an already-reviewing project: it
// clears the one-shot ReviewNow request, snaps the poll cadence back to the
// fast rung, and dispatches the review agent immediately — bypassing the poll's
// change-detection gate so the reconcile happens now rather than at the next
// (possibly 30m-away) tick.
//
// The flag is cleared as the daemon so the clearing write can't retrigger
// dispatch, and cleared before the launch rather than after: the #review poll is
// still armed (ensureReviewPoll ran just before this), so a request lost to a
// mid-run crash just falls back to the normal cadence. A run already in flight
// makes the dispatch a dedup no-op (dispatchReviewAgent checks hasSession), which
// is the right outcome — the kick is already being served.
func (d *Daemon) forceReviewNow(projectPath string, p *project.Project) {
	cleared := *p
	cleared.ReviewNow = false
	if err := project.Save(projectPath, &cleared); err != nil {
		d.audit.Log("project_save_error", "path", projectPath, "err", err.Error())
	} else {
		p = &cleared
	}
	d.resetReviewBackoff(projectPath)
	d.rescheduleReviewPoll(projectPath, reviewPollSpecs[0])
	d.audit.Log("review_poll_forced", "path", projectPath)
	d.dispatchReviewAgent(projectPath, p)
}

// dispatchReviewAgent launches a review agent unless one (or any other
// project-root agent) is already running for the project — the review agent
// shares the bare project session slot, so it can't run concurrently with a
// task/commit/planning agent, and a poll landing mid-run is a no-op.
func (d *Daemon) dispatchReviewAgent(projectPath string, p *project.Project) {
	if d.hasSession(projectPath) {
		d.audit.Log("session_skip_duplicate", "path", projectPath, "kind", agent.ReviewAgent.String())
		return
	}
	d.launchReviewAgent(projectPath, p)
}

// launchReviewAgent starts the review agent in the project's control directory
// with the github MCP attached. Like the planning agent it runs autonomously
// (ACP, `claude --print`) and needs no wsp workspace: it reads the project goal
// from .project.yaml, reaches the PR through the MCP, and writes new task files
// and status back into the control dir. On session end afterReviewSession
// inspects what the reconciler decided and routes accordingly.
func (d *Daemon) launchReviewAgent(projectPath string, p *project.Project) {
	if d.runner == nil {
		return
	}
	root := filepath.Dir(projectPath)
	plan := runner.Plan{
		Kind:        agent.ReviewAgent,
		WorkingDir:  root,
		ProjectPath: projectPath,
		TasksDir:    filepath.Join(root, "tasks"),
		Branch:      p.Branch,
		Repos:       workspaceReposFor(p),
		StaticMCPs:  []string{"github"},
		Policies:    reviewNetworkPolicies(),
	}
	d.audit.Log("review_dispatch", "path", projectPath, "branch", p.Branch)
	// A failed launch is not fatal: the #review poll stays registered, so the
	// next firing retries. Leaving the project in reviewing is correct — there is
	// nothing to block over. startSession already logged session_start_error.
	_ = d.startSession(projectPath, plan, func(waitErr error) {
		d.afterReviewSession(projectPath, waitErr)
	})
}

// afterReviewSession is the review agent's session-end callback. The reconciler
// expresses its decision purely through the project status it left behind:
//
//   - reviewing → nothing to do this cycle (PR clean, or no PR yet with
//     review:true). The PR is converging, so reset the fix-churn counter and
//     keep polling.
//   - working  → it created fix tasks. Count a fix cycle, stop polling while the
//     tasks run, and route to them — unless the PR has churned through too many
//     consecutive fix cycles without ever coming back clean, in which case block
//     for the wolf.
//   - done/blocked → the loop is over (PR merged/closed, or a contested comment
//     the reconciler escalated). Stop polling and route the new status.
func (d *Daemon) afterReviewSession(projectPath string, waitErr error) {
	// A non-nil wait error means the review agent process itself failed — most
	// often its sandbox could not be created (e.g. the github MCP gateway needs
	// auth), so it never even looked at the PR. That is NOT a clean cycle: keying
	// only on the resulting project status would misread the unchanged
	// status:reviewing as "nothing to do" and retry forever, hiding the failure.
	// Count consecutive crashes and, once they persist, block the project so the
	// user is actually told the loop is stuck.
	if waitErr != nil {
		n := d.bumpReviewErrors(projectPath)
		d.audit.Log("review_error", "path", projectPath, "attempt", fmt.Sprintf("%d", n), "err", waitErr.Error())
		if n < maxReviewErrors {
			return // keep polling; the schedule retries next interval
		}
		if d.scheduler != nil {
			d.scheduler.Unregister(reviewPollKey(projectPath))
		}
		p, err := project.Load(projectPath)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
			}
			return
		}
		d.transitionProjectBlocked(projectPath, p, fmt.Sprintf(
			"PR review agent failed to run %d times in a row (last error: %v); the loop is stuck — commonly the github MCP sandbox can't be created (check `sbx mcp ls` / `sbx mcp auth`)", n, waitErr))
		return
	}
	d.resetReviewErrors(projectPath)
	p, err := project.Load(projectPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		}
		return
	}
	switch p.Status {
	case project.StatusReviewing:
		d.resetReviewFixCycles(projectPath)
		// Record the PR fingerprint AS OF this reconcile, so scheduled polls can
		// tell when something new has happened since (and skip the agent when
		// nothing has). Computed post-run so the agent's own thread resolves —
		// which bump the PR's updatedAt — are already folded into the baseline
		// and don't self-trigger the next tick.
		if fp, ok := d.reviewFingerprint(projectPath, p); ok {
			d.storeReviewFinger(projectPath, fp)
		}
		d.audit.Log("review_clean", "path", projectPath)
	case project.StatusWorking:
		n := d.bumpReviewFixCycles(projectPath)
		if d.scheduler != nil {
			d.scheduler.Unregister(reviewPollKey(projectPath))
		}
		// Leaving reviewing while tasks run: drop the poll-cadence state (a fresh
		// baseline is taken when the project returns to reviewing). The fix-cycle
		// counter is deliberately preserved — it only resets on a clean poll.
		d.clearReviewPolling(projectPath)
		if n > maxReviewFixCycles {
			d.transitionProjectBlocked(projectPath, p, fmt.Sprintf(
				"PR review produced %d consecutive fix cycles without converging; needs a human", n))
			return
		}
		d.audit.Log("review_fix_cycle", "path", projectPath, "cycle", fmt.Sprintf("%d", n))
		d.revisitProject(projectPath)
	default:
		if d.scheduler != nil {
			d.scheduler.Unregister(reviewPollKey(projectPath))
		}
		d.clearReviewState(projectPath)
		d.revisitProject(projectPath)
	}
}

// bumpReviewFixCycles increments and returns the consecutive-fix-cycle count for
// projectPath; resetReviewFixCycles clears it (a poll that finds the PR clean, or
// the loop ending). See Daemon.reviewFixCycles.
func (d *Daemon) bumpReviewFixCycles(projectPath string) int {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	d.reviewFixCycles[projectPath]++
	return d.reviewFixCycles[projectPath]
}

func (d *Daemon) resetReviewFixCycles(projectPath string) {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	delete(d.reviewFixCycles, projectPath)
}

// bumpReviewErrors / resetReviewErrors track consecutive review-agent crashes
// (a non-nil session wait error — typically a sandbox that could not be
// created). resetReviewErrors is called on any successful review run so a single
// transient failure doesn't accumulate toward the block threshold.
func (d *Daemon) bumpReviewErrors(projectPath string) int {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	d.reviewErrors[projectPath]++
	return d.reviewErrors[projectPath]
}

func (d *Daemon) resetReviewErrors(projectPath string) {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	delete(d.reviewErrors, projectPath)
}

// reviewFingerprint returns the PR change-detection fingerprint, routing through
// the test seam when one is installed and otherwise the real host `gh` probe.
func (d *Daemon) reviewFingerprint(projectPath string, p *project.Project) (string, bool) {
	if d.reviewFingerprintFn != nil {
		return d.reviewFingerprintFn(projectPath, p)
	}
	return d.reviewPRFingerprint(projectPath, p)
}

// reviewPRFingerprint returns a cheap change-detection fingerprint for the
// project's pull request, computed on the host via `gh` (no sandbox, no claude).
// It hashes the fields that signal PR activity — head SHA (new commits),
// updatedAt (new comments/reviews), and the check-run rollup (Actions results) —
// so an unchanged fingerprint means nothing has happened since the last
// reconcile. ok is false when there is no PR to fingerprint yet (the review
// agent hasn't stamped one) or gh is unavailable; the caller treats that as
// "can't gate — run the agent" so the optimization never hides a real signal.
func (d *Daemon) reviewPRFingerprint(projectPath string, p *project.Project) (string, bool) {
	if len(p.PullRequests) == 0 {
		return "", false
	}
	base := d.ctx
	if base == nil {
		base = context.Background()
	}
	h := sha256.New()
	n := 0
	for _, pr := range p.PullRequests {
		if pr.Repo == "" || pr.Number == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(base, 20*time.Second)
		out, err := exec.CommandContext(ctx, "gh", "pr", "view",
			strconv.Itoa(pr.Number),
			"--repo", pr.Repo,
			"--json", "headRefOid,updatedAt,statusCheckRollup",
		).Output()
		cancel()
		if err != nil {
			// Can't gate reliably if any watched PR is unreadable — fail toward
			// running the agent so we never miss a signal.
			d.audit.Log("review_fingerprint_error", "path", projectPath, "repo", pr.Repo, "err", err.Error())
			return "", false
		}
		h.Write([]byte(pr.Repo))
		h.Write(out)
		n++
	}
	if n == 0 {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func (d *Daemon) lastReviewFinger(projectPath string) (string, bool) {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	fp, ok := d.reviewFinger[projectPath]
	return fp, ok
}

func (d *Daemon) storeReviewFinger(projectPath, fp string) {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	d.reviewFinger[projectPath] = fp
}

// growReviewBackoff advances the poll to the next slower cadence rung (capped at
// the last) and returns the spec to reschedule with. resetReviewBackoff returns
// it to the fastest rung.
func (d *Daemon) growReviewBackoff(projectPath string) string {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	idx := d.reviewBackoff[projectPath] + 1
	if idx >= len(reviewPollSpecs) {
		idx = len(reviewPollSpecs) - 1
	}
	d.reviewBackoff[projectPath] = idx
	return reviewPollSpecs[idx]
}

func (d *Daemon) resetReviewBackoff(projectPath string) {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	delete(d.reviewBackoff, projectPath)
}

// rescheduleReviewPoll re-registers the #review poll at spec. scheduler.Register
// no-ops when the spec is unchanged (so a quiet tick already at the cap doesn't
// churn the cron table) and otherwise replaces the entry, resetting the timer to
// the new interval.
func (d *Daemon) rescheduleReviewPoll(projectPath, spec string) {
	if d.scheduler == nil {
		return
	}
	key := reviewPollKey(projectPath)
	if err := d.scheduler.Register(key, spec, func() { d.onReviewPoll(projectPath) }); err != nil {
		d.audit.Log("review_poll_register_error", "path", projectPath, "spec", spec, "err", err.Error())
	}
}

// clearReviewPolling drops the poll-cadence state (fingerprint baseline + backoff
// rung) for a project leaving reviewing; a fresh baseline is taken when it
// returns. clearReviewState additionally clears the fix-cycle and crash counters
// — used when the loop ends entirely (done/blocked).
func (d *Daemon) clearReviewPolling(projectPath string) {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	delete(d.reviewFinger, projectPath)
	delete(d.reviewBackoff, projectPath)
}

func (d *Daemon) clearReviewState(projectPath string) {
	d.reviewMu.Lock()
	defer d.reviewMu.Unlock()
	delete(d.reviewFinger, projectPath)
	delete(d.reviewBackoff, projectPath)
	delete(d.reviewFixCycles, projectPath)
	delete(d.reviewErrors, projectPath)
}

// reviewPollKey is the scheduler key for a project's PR poll. The "#review"
// suffix keeps it distinct from the cron schedule (registered under the bare
// project path), so a project can carry both without one clobbering the other.
func reviewPollKey(projectPath string) string {
	return projectPath + "#review"
}

// reviewNetworkPolicies are the sandbox network allowances for the review agent.
// The github MCP reaches GitHub on the project's behalf; these allow rules make
// that reachability explicit (and cover the case where the MCP runs in-sandbox).
func reviewNetworkPolicies() []policy.Rule {
	return []policy.Rule{
		{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "api.github.com"},
		{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "api.githubcopilot.com"},
	}
}
