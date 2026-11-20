package daemon

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/taskgraph"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// maxIntegrationAttempts is how many times the daemon relaunches the commit
// agent for the same task after a transient integration failure
// (docs/parallel-task-worktrees.md I4): the shared tip moved outside the
// queue, a git lock, a sandbox that would not start, or a session that ended
// with the task still at success. It is tracked in integration.attempts, not
// attempts, because the task's own work is fine and is not re-run. Once it is
// spent the project blocks.
const maxIntegrationAttempts = 2

// Integration results the commit agent reports in the task file's
// integration.result (I4).
const (
	integrationConflict     = "conflict"
	integrationVerifyFailed = "verify_failed"
	integrationError        = "error"
	integrationPartial      = "partial"
)

// integrationLaunch is what launchCommitAgent knew about a worktree task's
// integration when it started the commit agent, handed to afterCommitSession
// so it can route and audit the outcome. Nil for a push task, a legacy task,
// or a daemon without worktrees.
type integrationLaunch struct {
	// onto is the shared tip of each repo just before the commit agent ran.
	// When every repo's tip still equals the task's base, the integration is
	// the I2 fast path: nothing to rebase, so no build check either.
	onto map[string]string
	// verify and docsOnly are the build check settings the prompt carried.
	verify   string
	docsOnly bool
	// attempts is integration.attempts at launch. The commit agent rewrites
	// the integration block when it reports a failure and may drop the
	// daemon's counter, so the routing takes the larger of this and the file.
	attempts int
}

// fastPath reports whether no repo's shared tip had moved past the task's
// base when the commit agent started.
func (il *integrationLaunch) fastPath(t *task.Task) bool {
	for _, b := range t.Base {
		if tip, ok := il.onto[b.Repo]; ok && tip != b.Hash {
			return false
		}
	}
	return true
}

// prepareIntegration is the host-side step before every commit agent launch
// for a worktree task (X7 part 4). It makes an interrupted integration safe to
// run again from the top: a stale index.lock (no session can hold it: the
// commit slot is free, and launches of the project's task agents, which run
// git in the same repositories, are serialized with it by worktreeLock) is
// removed, and a rebase left in progress in the task worktree
// is aborted, which restores the branch's pre-rebase tip. A dirty shared tree
// is not touched: it may hold a human's edits, so the caller blocks on the
// returned error. It returns each repo's shared tip.
func (d *Daemon) prepareIntegration(projectPath string, p *project.Project, t *task.Task, root string) (map[string]string, error) {
	shared, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	onto := map[string]string{}
	for _, repo := range repoDirNames(p) {
		sharedRepo := filepath.Join(shared, repo)
		if _, err := os.Stat(sharedRepo); err != nil {
			continue
		}
		dirs := []string{sharedRepo}
		wt := filepath.Join(root, repo)
		hasWT := false
		if _, err := os.Stat(wt); err == nil {
			dirs = append(dirs, wt)
			hasWT = true
		}
		for _, dir := range dirs {
			lock, err := d.worktrees.GitPath(d.ctx, dir, "index.lock")
			if err != nil {
				continue
			}
			if _, err := os.Stat(lock); err == nil {
				if err := os.Remove(lock); err != nil {
					return nil, fmt.Errorf("remove stale %s: %w", lock, err)
				}
				d.audit.Log("integration_lock_cleared", "path", projectPath, "task", t.Name, "repo", repo, "lock", lock)
			}
		}
		if hasWT {
			if busy, err := d.worktrees.RebaseInProgress(d.ctx, wt); err == nil && busy {
				if err := d.worktrees.AbortRebase(d.ctx, wt); err != nil {
					return nil, fmt.Errorf("abort the interrupted rebase in %s: %w", wt, err)
				}
				d.audit.Log("integration_rebase_aborted", "path", projectPath, "task", t.Name, "repo", repo)
			}
		}
		if dirty, err := d.worktrees.Dirty(d.ctx, sharedRepo); err == nil && dirty {
			return nil, fmt.Errorf("shared workspace %s has uncommitted changes (interrupted integration?)", repo)
		}
		if tip, err := d.worktrees.RevParse(d.ctx, sharedRepo, "refs/heads/"+p.Branch); err == nil {
			onto[repo] = tip
		}
	}
	return onto, nil
}

// docsOnly is the daemon's verdict that a task changed documentation only, so
// the commit agent skips the build check (R1). Both have to agree: the task
// declares touches, all of them documentation, and its actual diff against
// base (committed and not) touches documentation only. A task with no hints,
// or one that strayed outside them, is checked.
func (d *Daemon) docsOnly(t *task.Task, root string) bool {
	if t.Touches == nil || len(*t.Touches) == 0 || len(t.Areas) > 0 {
		return false
	}
	for _, g := range *t.Touches {
		if !isDocPath(g) {
			return false
		}
	}
	for _, b := range t.Base {
		paths, err := d.worktrees.ChangedPaths(d.ctx, filepath.Join(root, b.Repo), b.Hash)
		if err != nil {
			return false
		}
		for _, p := range paths {
			if !isDocPath(p) {
				return false
			}
		}
	}
	return true
}

// isDocPath reports whether a path (or a touches glob) is documentation: a
// markdown file, or anything under a docs/ directory.
func isDocPath(p string) bool {
	p = filepath.ToSlash(p)
	if strings.EqualFold(path.Ext(p), ".md") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "docs" {
			return true
		}
	}
	return false
}

// routeIntegrationFailure is afterCommitSession's I4 routing for a worktree
// task whose commit agent did not reach committed. It reads the commit agent's
// report from the task file:
//
//   - blocked: the agent found something it must not fix (a dirty or
//     wrong-branch shared checkout). Block.
//   - failed, integration.result conflict or verify_failed: re-run the task
//     in its worktree (conflictReRun).
//   - failed, integration.result partial: some repos are published and the
//     rest are not, which only a tip moved outside the queue causes. Block.
//   - otherwise (failed with result error or no report, still success, or a
//     commit agent that could not start): transient. Relaunch the commit
//     agent only, up to maxIntegrationAttempts, then block.
//
// done releases the commit reservation; a conflict re-run calls it before its
// scheduling pass, so the next queued task integrates straight away.
func (d *Daemon) routeIntegrationFailure(projectPath string, p *project.Project, t *task.Task, startFailure string, il *integrationLaunch, done func()) {
	result := ""
	if t.Integration != nil {
		result = t.Integration.Result
	}
	switch {
	case t.Status == task.StatusBlocked:
		d.retainSandbox(t.Path, t)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("commit agent blocked task %q: %s", t.Name, t.BlockedReason))
	case t.Status == task.StatusFailed && (result == integrationConflict || result == integrationVerifyFailed):
		if result == integrationVerifyFailed {
			d.countVerifyCheck(projectPath, verifyCheckFailed)
			d.audit.Log("integration_verify_check_failed", "path", projectPath, "task", t.Name,
				"repo", t.Integration.Repo, "onto", formatCommits(t.Integration.Onto), "attempts", strconv.Itoa(t.Attempts))
		}
		d.conflictReRun(projectPath, p, t, done)
	case t.Status == task.StatusFailed && result == integrationPartial:
		d.audit.Log("integration_partial", "path", projectPath, "task", t.Name,
			"published", strings.Join(t.Integration.Published, ","), "repo", t.Integration.Repo)
		d.retainSandbox(t.Path, t)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("task %q was partially integrated: %s published, %s not (%s); finish it by hand from its worktree",
				t.Name, strings.Join(t.Integration.Published, ", "), t.Integration.Repo, t.FailureReason))
	case t.Status == task.StatusFailed || t.Status == task.StatusSuccess:
		d.retryIntegration(projectPath, p, t, startFailure, il)
	default:
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("commit agent ended with task %q in status %q", t.Name, t.Status))
	}
}

// retryIntegration handles a transient integration failure: the task goes
// back to success (its work is fine) with integration.attempts bumped, and the
// same task's commit agent is relaunched, so nothing overtakes it in the queue
// (S5). Once maxIntegrationAttempts relaunches are spent the project blocks.
func (d *Daemon) retryIntegration(projectPath string, p *project.Project, t *task.Task, startFailure string, il *integrationLaunch) {
	attempts := 0
	if t.Integration != nil {
		attempts = t.Integration.Attempts
	}
	if il != nil && il.attempts > attempts {
		attempts = il.attempts
	}
	why := startFailure
	if why == "" && t.FailureReason != "" {
		why = t.FailureReason
	}
	if why == "" {
		why = fmt.Sprintf("commit agent ended with the task at %s", t.Status)
	}
	if attempts >= maxIntegrationAttempts {
		d.retainSandbox(t.Path, t)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("integration of task %q failed after %d commit agent retries: %s", t.Name, attempts, why))
		return
	}
	attempts++
	if err := task.Update(t.Path, func(cur *task.Task) bool {
		cur.Status = task.StatusSuccess
		cur.FailureReason = ""
		if cur.Integration == nil {
			cur.Integration = &task.Integration{Result: integrationError}
		}
		cur.Integration.Attempts = attempts
		return true
	}); err != nil {
		d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
		d.transitionProjectBlocked(projectPath, p, "task save error: "+err.Error())
		return
	}
	d.audit.Log("integration_retry", "path", projectPath, "task", t.Name,
		"integration_attempts", strconv.Itoa(attempts), "err", why)
	if d.deferForPower(projectPath) {
		// On battery nothing new starts (S6). The task stays at the head of
		// the queue, and the resume pass relaunches its commit agent.
		d.audit.Log("power_deferred", "path", projectPath)
		return
	}
	cur, err := task.Load(t.Path)
	if err != nil {
		d.transitionProjectBlocked(projectPath, p, "task load error: "+err.Error())
		return
	}
	d.launchCommitAgent(projectPath, p, cur)
}

// conflictReRun handles a rebase conflict or a failed build check (I4): the
// task leaves the commit queue and re-runs in its worktree, which the commit agent's abort left at the
// pre-rebase tip, fully committed. The re-run's prompt (the integration block,
// kept for it) tells the agent to rebase onto the shared branch and resolve
// what is named. It costs an attempt, like any retry; once they are spent the
// project blocks with a reason naming the tasks that landed in between (I6),
// and the worktree is kept.
func (d *Daemon) conflictReRun(projectPath string, p *project.Project, t *task.Task, done func()) {
	if !d.requeueConflict(projectPath, p, t) {
		return
	}
	// The re-run competes for a task slot like any ready task (S5), first in
	// line for the one it held; the next queued task integrates meanwhile.
	if done != nil {
		done()
	}
	d.schedulePass(projectPath, p, passOpts{prefer: t.Name})
}

// requeueConflict is conflictReRun without the scheduling pass: it moves the
// task back to ready for its re-run and reports true, or blocks the project
// and reports false.
func (d *Daemon) requeueConflict(projectPath string, p *project.Project, t *task.Task) bool {
	in := t.Integration
	landed := d.landedSince(projectPath, p, t)
	cause := "conflict"
	if in.Result == integrationConflict {
		d.audit.Log("integration_conflicted", "path", projectPath, "task", t.Name, "repo", in.Repo,
			"files", strings.Join(in.Files, ","), "landed", strings.Join(landed, ","), "attempts", strconv.Itoa(t.Attempts))
	} else {
		cause = "verify"
	}

	if t.Attempts >= maxTaskAttempts {
		// retainSandbox saves t whole, so the in-memory copy carries landed too.
		t.Integration.Landed = landed
		if err := task.Update(t.Path, func(cur *task.Task) bool {
			if cur.Integration == nil {
				return false
			}
			cur.Integration.Landed = landed
			return true
		}); err != nil {
			d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
		}
		d.audit.Log("task_retry_exhausted", "path", projectPath, "task", t.Name, "attempts", strconv.Itoa(t.Attempts), "cause", cause)
		d.retainSandbox(t.Path, t)
		d.transitionProjectBlocked(projectPath, p, conflictBlockReason(p, t, landed))
		return false
	}

	var updated *task.Task
	if err := task.Update(t.Path, func(cur *task.Task) bool {
		cur.Attempts++
		cur.Status = task.StatusReady
		cur.FailureReason = ""
		cur.SucceededAt = nil
		if cur.Integration == nil {
			cur.Integration = &task.Integration{Result: in.Result, Repo: in.Repo, Files: in.Files, Onto: in.Onto, Output: in.Output}
		}
		cur.Integration.Landed = landed
		cur.Integration.Attempts = 0
		updated = cur
		return true
	}); err != nil {
		d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
		d.transitionProjectBlocked(projectPath, p, "task save error: "+err.Error())
		return false
	}
	d.audit.Log("task_retry", "path", projectPath, "task", updated.Name, "attempts", strconv.Itoa(updated.Attempts), "cause", cause)
	return true
}

// conflictBlockReason is the I6 blocked_reason for a task whose conflict
// re-runs are spent: the task, what landed since its base, where, and how many
// attempts it had.
func conflictBlockReason(p *project.Project, t *task.Task, landed []string) string {
	in := t.Integration
	with := "commits on " + p.Branch
	if len(landed) > 0 {
		with = fmt.Sprintf("%q", landed)
	}
	if in.Result == integrationVerifyFailed {
		return fmt.Sprintf("task %q failed its build check after rebasing onto %s (landed: %s) after %d attempts",
			t.Name, p.Branch, with, t.Attempts)
	}
	where := in.Repo
	if len(in.Files) > 0 {
		where += ": " + strings.Join(in.Files, ", ")
	}
	return fmt.Sprintf("task %q conflicted with %s in %s after %d attempts", t.Name, with, where, t.Attempts)
}

// landedSince lists the committed tasks whose commits landed on the shared
// branch between t's base and the tips its integration tried to rebase onto,
// in the repo that failed (every repo when none is named): the tasks it
// conflicted with (I4 `landed`). Best-effort; git errors leave a task out.
func (d *Daemon) landedSince(projectPath string, p *project.Project, t *task.Task) []string {
	shared, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		return nil
	}
	base := map[string]string{}
	for _, c := range t.Base {
		base[c.Repo] = c.Hash
	}
	onto := map[string]string{}
	if t.Integration != nil {
		for _, c := range t.Integration.Onto {
			onto[c.Repo] = c.Hash
		}
	}
	g, err := taskgraph.Load(filepath.Join(filepath.Dir(projectPath), "tasks"))
	if err != nil {
		return nil
	}
	var out []string
	for _, o := range g.Tasks() {
		if o.Name == t.Name || o.Status != task.StatusCommitted {
			continue
		}
		for _, c := range o.Commits {
			if t.Integration != nil && t.Integration.Repo != "" && c.Repo != t.Integration.Repo {
				continue
			}
			dir := filepath.Join(shared, c.Repo)
			tip := onto[c.Repo]
			if tip == "" {
				tip = "refs/heads/" + p.Branch
			}
			in, err := d.worktrees.IsAncestor(d.ctx, dir, c.Hash, tip)
			if err != nil || !in {
				continue
			}
			if b := base[c.Repo]; b != "" {
				if before, err := d.worktrees.IsAncestor(d.ctx, dir, c.Hash, b); err != nil || before {
					continue
				}
			}
			out = append(out, o.Name)
			break
		}
	}
	return out
}

// auditVerifyCheck records, for a worktree task that just integrated, whether
// the pre-integration build check ran (R1): passed, or skipped and why. A
// failed check is recorded where it is routed (routeIntegrationFailure).
func (d *Daemon) auditVerifyCheck(projectPath string, t *task.Task, il *integrationLaunch) {
	if il == nil {
		return
	}
	switch {
	case il.verify == "":
		d.audit.Log("integration_verify_check_skipped", "path", projectPath, "task", t.Name, "why", "not_configured")
	case il.fastPath(t):
		d.countVerifyCheck(projectPath, verifyCheckSkipped)
		d.audit.Log("integration_verify_check_skipped", "path", projectPath, "task", t.Name, "why", "fast_path")
	case il.docsOnly:
		d.countVerifyCheck(projectPath, verifyCheckSkipped)
		d.audit.Log("integration_verify_check_skipped", "path", projectPath, "task", t.Name, "why", "docs_only")
	default:
		d.countVerifyCheck(projectPath, verifyCheckRun)
		d.audit.Log("integration_verify_check_passed", "path", projectPath, "task", t.Name,
			"onto", formatOnto(il.onto))
	}
}

// auditTouches is the hint audit after a worktree task lands
// (docs/parallel-task-worktrees.md H3): the paths the integration changed on
// the shared branch that the task's touches and areas do not cover are logged
// as touches_exceeded and recorded in the task's touches_actual. Nothing
// blocks on it. Each repo's change is diffed from the shared tip the commit
// agent started from (onto) to the tip it published, not from base: base is
// where the worktree was cut, and the work of tasks that landed since would
// show up as this task's. A repo whose onto is unknown (an integration adopted
// across a restart) is left out. An exclusive task declared nothing and ran
// alone, so it is not audited.
func (d *Daemon) auditTouches(projectPath string, p *project.Project, t *task.Task, il *integrationLaunch) {
	if t.Exclusive() || len(t.Commits) == 0 {
		return
	}
	shared, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		return
	}
	var changed []string
	for _, c := range t.Commits {
		from := il.onto[c.Repo]
		if from == "" || from == c.Hash {
			continue
		}
		paths, err := d.worktrees.DiffNames(d.ctx, filepath.Join(shared, c.Repo), from, c.Hash)
		if err != nil {
			d.audit.Log("touches_audit_error", "path", projectPath, "task", t.Name, "repo", c.Repo, "err", err.Error())
			continue
		}
		for _, f := range paths {
			changed = append(changed, c.Repo+"/"+f)
		}
	}
	outside := taskgraph.Uncovered(t, changed)
	if len(outside) == 0 {
		return
	}
	d.audit.Log("touches_exceeded", "path", projectPath, "task", t.Name, "paths", strings.Join(outside, ","))
	t.TouchesActual = outside
	if err := task.Update(t.Path, func(cur *task.Task) bool {
		cur.TouchesActual = outside
		return true
	}); err != nil {
		d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
	}
}

func formatOnto(onto map[string]string) string {
	var cs []task.Commit
	for repo, hash := range onto {
		cs = append(cs, task.Commit{Repo: repo, Hash: hash})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Repo < cs[j].Repo })
	return formatCommits(cs)
}

// worktreeRoot is t's worktree root: the path the task file records, else the
// derived one.
func worktreeRoot(shared string, t *task.Task) string {
	if t.Worktree != "" {
		return t.Worktree
	}
	return worktree.Root(shared, t.Name)
}

// beginCommit reserves the project's commit slot from launchCommitAgent until
// endCommit, after afterCommitSession has routed the outcome. A transient
// retry relaunches from inside afterCommitSession, so the count can briefly be
// 2; the project is commit-reserved while it is above 0.
func (d *Daemon) beginCommit(projectPath string) {
	d.commitMu.Lock()
	d.commitBusy[projectPath]++
	d.commitMu.Unlock()
	d.markSnapshotDirty()
}

func (d *Daemon) endCommit(projectPath string) {
	d.commitMu.Lock()
	if d.commitBusy[projectPath] <= 1 {
		delete(d.commitBusy, projectPath)
	} else {
		d.commitBusy[projectPath]--
	}
	d.commitMu.Unlock()
	d.markSnapshotDirty()
}

// commitReserved reports whether a commit agent of the project is between
// launch and the end of its afterCommitSession.
func (d *Daemon) commitReserved(projectPath string) bool {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()
	return d.commitBusy[projectPath] > 0
}

// Build check outcomes counted in verifyChecks (R1).
const (
	verifyCheckRun     = "run"
	verifyCheckFailed  = "failed"
	verifyCheckSkipped = "skipped"
)

// countVerifyCheck records one build check outcome for the project's
// verify_checks snapshot field. A failed check also ran, so it counts in both.
func (d *Daemon) countVerifyCheck(projectPath, outcome string) {
	d.commitMu.Lock()
	vc := d.verifyChecks[projectPath]
	if vc == nil {
		vc = &VerifyChecks{}
		d.verifyChecks[projectPath] = vc
	}
	switch outcome {
	case verifyCheckRun:
		vc.Run++
	case verifyCheckFailed:
		vc.Run++
		vc.Failed++
	case verifyCheckSkipped:
		vc.Skipped++
	}
	d.commitMu.Unlock()
}

// verifyCheckCounts returns a copy of the project's build check counters, or
// nil when none has been recorded.
func (d *Daemon) verifyCheckCounts(projectPath string) *VerifyChecks {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()
	if vc := d.verifyChecks[projectPath]; vc != nil {
		c := *vc
		return &c
	}
	return nil
}

// queueDepth is the length of the project's commit queue on disk (S5), for
// the integration_started event.
func (d *Daemon) queueDepth(projectPath string) int {
	return len(taskgraph.Queued(loadTasksLenient(filepath.Join(filepath.Dir(projectPath), "tasks"))))
}

// loadTasksLenient loads every task file in tasksDir that parses, skipping the
// rest, sorted by file name. An unnamed seed gets its filename stem as its
// name. It is for reports (the snapshot, audit fields), which must not fail on
// one bad file the way taskgraph.Load does.
func loadTasksLenient(tasksDir string) []*task.Task {
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return nil
	}
	var out []*task.Task
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		t, err := task.Load(filepath.Join(tasksDir, e.Name()))
		if err != nil {
			continue
		}
		if t.Name == "" {
			t.Name = strings.TrimSuffix(e.Name(), ".yaml")
		}
		out = append(out, t)
	}
	return out
}
