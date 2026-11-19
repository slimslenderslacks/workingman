package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// worktreesEnabled reports whether this daemon gives tasks their own git
// worktrees (docs/parallel-task-worktrees.md W2–W6). It needs a workspace
// manager whose workspaces are real git repos: wsp always, the stub only when
// it was built with Git set. Without one every task runs in the shared tree,
// as it did before worktrees existed.
func (d *Daemon) worktreesEnabled() bool {
	return d.runner != nil && d.runner.Workspaces != nil && workspace.IsGitBacked(d.runner.Workspaces)
}

// repoDirNames are the workspace dir names of every repo in the project's
// workspace: the repos a task worktree holds one git worktree for (W2).
func repoDirNames(p *project.Project) []string {
	repos := workspaceReposFor(p)
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.DirName()
	}
	return out
}

// worktreeSpec describes t's task worktree in the shared workspace. The task
// branch is the one stored on the task when it has one (W3: stored, not
// recomputed), else the derived name.
func worktreeSpec(shared string, p *project.Project, t *task.Task) worktree.Spec {
	branch := t.TaskBranch
	if branch == "" {
		branch = worktree.BranchName(p.Branch, t.Name)
	}
	return worktree.Spec{
		Shared:       shared,
		Task:         t.Name,
		Branch:       branch,
		SharedBranch: p.Branch,
		Repos:        repoDirNames(p),
	}
}

// worktreeCreateError is a failure to provision the shared workspace or to
// cut or reuse a task worktree in it. launchTaskAgent releases the task and
// blocks the project on it (W4).
type worktreeCreateError struct {
	repo string // "" when the failure is not specific to one repo
	err  error
}

func (e *worktreeCreateError) Error() string { return e.err.Error() }
func (e *worktreeCreateError) Unwrap() error { return e.err }

// prepareTaskWorktree is the W4 step every dispatch of a non-push task goes
// through when worktrees are enabled: provision the shared workspace, then
// reuse, re-attach or cut t's worktree with worktree.Manager.Ensure, stamp
// worktree/task_branch/base on the task file, and point plan at it. It never
// resets a reused worktree: the retry or resumed run decides what to keep.
//
// A task that has never had a worktree stays on the legacy path (plan left
// untouched, so it runs in the shared tree) while the shared tree still holds
// legacy work: another task without a task branch at running or success, or
// uncommitted changes in a shared repo (D7). Those drain through the shared
// tree first; cutting a worktree beside them would strand their changes and
// make the commit agent's clean-tree check fail on files that are not this
// task's.
func (d *Daemon) prepareTaskWorktree(projectPath string, p *project.Project, t *task.Task, plan *runner.Plan) error {
	if !d.worktreesEnabled() || t.IsPushTask() {
		return nil
	}
	shared, err := d.runner.Workspaces.Create(d.ctx, p.Branch, workspaceReposFor(p))
	if err != nil {
		return &worktreeCreateError{err: fmt.Errorf("provision workspace: %w", err)}
	}
	spec := worktreeSpec(shared, p, t)
	if !t.HasTaskBranch() {
		if why := d.legacyInFlight(projectPath, t, shared, spec.Repos); why != "" {
			d.audit.Log("worktree_skipped_legacy", "path", projectPath, "task", t.Name, "why", why)
			return nil
		}
	}
	res, err := d.worktrees.Ensure(d.ctx, spec)
	for _, rr := range res.Repos {
		if rr.Stale != "" {
			d.audit.Log("worktree_stale_moved", "path", projectPath, "task", t.Name, "from", rr.Path, "to", rr.Stale)
		}
	}
	if err != nil {
		var re *worktree.RepoError
		if errors.As(err, &re) {
			return &worktreeCreateError{repo: re.Repo, err: err}
		}
		return &worktreeCreateError{err: err}
	}

	base := mergeBase(t.Base, res)
	if err := task.Update(t.Path, func(cur *task.Task) bool {
		cur.Worktree = res.Root
		cur.TaskBranch = res.Branch
		cur.Base = base
		return true
	}); err != nil {
		return &worktreeCreateError{err: fmt.Errorf("record worktree on task file: %w", err)}
	}
	t.Worktree, t.TaskBranch, t.Base = res.Root, res.Branch, base

	if res.Fresh() {
		d.audit.Log("worktree_created", "path", projectPath, "task", t.Name,
			"worktree", res.Root, "branch", res.Branch, "base", formatCommits(base))
	} else {
		d.audit.Log("worktree_reused", "path", projectPath, "task", t.Name,
			"worktree", res.Root, "attempt", strconv.Itoa(t.Attempts), "dirty", strconv.FormatBool(res.Dirty()))
	}

	mounts, err := d.worktreeMounts(shared, spec.Repos)
	if err != nil {
		return &worktreeCreateError{err: err}
	}
	plan.TaskWorktree = res.Root
	plan.TaskBranch = res.Branch
	plan.TaskBase = base
	plan.Attempt = t.Attempts
	plan.Resumed = !res.Fresh()
	plan.Integration = t.Integration
	plan.WorktreeMounts = mounts
	return nil
}

// legacyInFlight returns why t must still run in the shared tree, or "" when
// it may get a worktree (D7). t itself is skipped in the task scan: the
// caller has just marked it running.
func (d *Daemon) legacyInFlight(projectPath string, t *task.Task, shared string, repos []string) string {
	tasksDir := filepath.Join(filepath.Dir(projectPath), "tasks")
	if entries, err := os.ReadDir(tasksDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
				continue
			}
			o, err := task.Load(filepath.Join(tasksDir, e.Name()))
			if err != nil || o.Name == t.Name || o.HasTaskBranch() {
				continue
			}
			if o.Status == task.StatusRunning || o.Status == task.StatusSuccess {
				return fmt.Sprintf("legacy task %q is %s in the shared tree", o.Name, o.Status)
			}
		}
	}
	for _, repo := range repos {
		dirty, err := d.worktrees.Dirty(d.ctx, filepath.Join(shared, repo))
		if err != nil {
			// Not a repo we can read: let Ensure report it properly.
			continue
		}
		if dirty {
			return fmt.Sprintf("shared workspace %s has uncommitted changes", repo)
		}
	}
	return ""
}

// worktreeMounts are the extra sandbox mounts of a task agent in a worktree
// (W5): every repo's git common dir, where the worktree's objects, refs and
// admin dir live, and the shared workspace's .gomodcache when one is staged.
func (d *Daemon) worktreeMounts(shared string, repos []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, repo := range repos {
		dir, err := d.worktrees.GitCommonDir(d.ctx, filepath.Join(shared, repo))
		if err != nil {
			return nil, fmt.Errorf("git common dir of %s: %w", repo, err)
		}
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	cache := filepath.Join(shared, ".gomodcache")
	if info, err := os.Stat(cache); err == nil && info.IsDir() {
		out = append(out, cache)
	}
	return out, nil
}

// mergeBase folds what Ensure did into the task's recorded base (W4): a repo
// whose branch was just cut records the tip it was cut from, a re-attached
// branch records its merge base only if the task had none for that repo, and
// a reused worktree keeps the base already recorded.
func mergeBase(cur []task.Commit, res worktree.Result) []task.Commit {
	out := append([]task.Commit(nil), cur...)
	index := map[string]int{}
	for i, c := range out {
		index[c.Repo] = i
	}
	for _, rr := range res.Repos {
		if rr.Base == "" {
			continue
		}
		i, ok := index[rr.Repo]
		switch {
		case !ok:
			index[rr.Repo] = len(out)
			out = append(out, task.Commit{Repo: rr.Repo, Hash: rr.Base})
		case rr.Action == worktree.Created:
			out[i].Hash = rr.Base
		}
	}
	return out
}

func formatCommits(cs []task.Commit) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.Repo + "=" + c.Hash
	}
	return strings.Join(parts, ",")
}

// restampBase re-records a worktree task's base as the merge base of its
// branch with the shared branch, each time afterTaskSession observes success
// (W4): a re-run may have rebased the branch, and the commit agent uses the
// base to tell a repo with work from one without. Best-effort; a failure
// leaves the earlier base, which is only ever older.
func (d *Daemon) restampBase(p *project.Project, t *task.Task) {
	if !t.HasTaskBranch() || !d.worktreesEnabled() {
		return
	}
	shared, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		return
	}
	var base []task.Commit
	for _, repo := range repoDirNames(p) {
		dir := filepath.Join(shared, repo)
		if ok, err := d.worktrees.BranchExists(d.ctx, dir, t.TaskBranch); err != nil || !ok {
			continue
		}
		mb, err := d.worktrees.MergeBase(d.ctx, dir, t.TaskBranch, p.Branch)
		if err != nil {
			d.audit.Log("worktree_base_error", "path", d.projectPathOf(t), "task", t.Name, "repo", repo, "err", err.Error())
			return
		}
		base = append(base, task.Commit{Repo: repo, Hash: mb})
	}
	if formatCommits(base) == formatCommits(t.Base) {
		return
	}
	if err := task.Update(t.Path, func(cur *task.Task) bool {
		cur.Base = base
		return true
	}); err != nil {
		d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
		return
	}
	t.Base = base
}

// finishWorktreeTask is the worktree branch of finishCommittedTask (W6): it
// checks that the task branch really landed on the shared branch in every
// repo, then removes the task worktree and branch, the only point in a task's
// life where they are removed. If a branch is not an ancestor of the shared
// tip the commit agent's `committed` is not true, so nothing is removed and
// the project blocks. A failed removal is logged and left behind; it never
// stops the pipeline.
func (d *Daemon) finishWorktreeTask(projectPath string, p *project.Project, t *task.Task) bool {
	shared, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("task %q committed but its workspace could not be resolved: %v", t.Name, err))
		return false
	}
	spec := worktreeSpec(shared, p, t)
	// Every recorded commit must be on the shared branch (I3): the agent's word
	// alone is not what makes `committed` mean "integrated".
	for _, c := range t.Commits {
		dir := filepath.Join(shared, c.Repo)
		ok, err := d.worktrees.IsAncestor(d.ctx, dir, c.Hash, p.Branch)
		if err == nil && ok {
			continue
		}
		why := "not an ancestor of " + p.Branch
		if err != nil {
			why = err.Error()
		}
		d.audit.Log("integration_verify_failed", "path", projectPath, "task", t.Name, "repo", c.Repo,
			"hash", c.Hash, "why", why)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("task %q is committed but its commit %s in %s is %s", t.Name, c.Hash, c.Repo, why))
		return false
	}
	for _, repo := range spec.Repos {
		dir := filepath.Join(shared, repo)
		exists, err := d.worktrees.BranchExists(d.ctx, dir, spec.Branch)
		if err != nil || !exists {
			continue
		}
		ok, err := d.worktrees.IsAncestor(d.ctx, dir, spec.Branch, p.Branch)
		if err == nil && ok {
			continue
		}
		why := "not an ancestor of " + p.Branch
		if err != nil {
			why = err.Error()
		}
		d.audit.Log("integration_verify_failed", "path", projectPath, "task", t.Name, "repo", repo,
			"hash", spec.Branch, "why", why)
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("task %q is committed but %s in %s is %s", t.Name, spec.Branch, repo, why))
		return false
	}

	// The task branch landed, but a repo whose branch moved past its base has
	// no commits: entry. The commit agent
	// claimed committed without recording what it published, so the record
	// the review agent and push tasks read would be wrong. Block (I4).
	recorded := map[string]bool{}
	for _, c := range t.Commits {
		recorded[c.Repo] = true
	}
	for _, b := range t.Base {
		if recorded[b.Repo] {
			continue
		}
		dir := filepath.Join(shared, b.Repo)
		tip, err := d.worktrees.RevParse(d.ctx, dir, "refs/heads/"+spec.Branch)
		if err != nil || tip == b.Hash {
			continue
		}
		d.audit.Log("integration_verify_failed", "path", projectPath, "task", t.Name, "repo", b.Repo,
			"hash", tip, "why", "missing from commits")
		d.transitionProjectBlocked(projectPath, p,
			fmt.Sprintf("task %q is committed but its work in %s (%s) is missing from its commits", t.Name, b.Repo, tip))
		return false
	}

	if t.Integration != nil {
		t.Integration = nil
		if err := task.Update(t.Path, func(cur *task.Task) bool {
			if cur.Integration == nil {
				return false
			}
			cur.Integration = nil
			return true
		}); err != nil {
			d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
		}
	}

	if err := d.worktrees.Remove(d.ctx, spec); err != nil {
		d.audit.Log("task_worktree_remove_error", "path", projectPath, "task", t.Name, "err", err.Error())
		return true
	}
	d.audit.Log("worktree_removed", "path", projectPath, "task", t.Name, "worktree", spec.Root(), "reason", "integrated")
	if t.Worktree != "" {
		t.Worktree = ""
		if err := task.Update(t.Path, func(cur *task.Task) bool {
			if cur.Worktree == "" {
				return false
			}
			cur.Worktree = ""
			return true
		}); err != nil {
			d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
		}
	}
	return true
}

// commitWorktree points a commit agent's plan at its task's worktree, which
// selects the worktree branch of the commit prompt (the I3 procedure), after
// prepareIntegration has cleaned up after any interrupted integration. It
// returns what afterCommitSession needs to route the outcome, or nil for a
// push task, a legacy task (no task branch) or a daemon without worktrees,
// which keep today's prompt, committing the shared tree.
func (d *Daemon) commitWorktree(projectPath string, p *project.Project, t *task.Task, plan *runner.Plan) (*integrationLaunch, error) {
	if !t.HasTaskBranch() || t.IsPushTask() || !d.worktreesEnabled() {
		return nil, nil
	}
	shared, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	root := worktreeRoot(shared, t)
	onto, err := d.prepareIntegration(projectPath, p, t, root)
	if err != nil {
		return nil, err
	}
	il := &integrationLaunch{onto: onto, verify: p.Verify}
	if t.Integration != nil {
		il.attempts = t.Integration.Attempts
	}
	if il.verify != "" {
		il.docsOnly = d.docsOnly(t, root)
	}
	plan.TaskWorktree = root
	plan.TaskBranch = t.TaskBranch
	plan.TaskBase = t.Base
	plan.Verify = il.verify
	plan.DocsOnly = il.docsOnly
	return il, nil
}

// projectPathOf is the project.yaml of the project t belongs to, derived from
// its file's place in <control-dir>/tasks/.
func (d *Daemon) projectPathOf(t *task.Task) string {
	return filepath.Join(filepath.Dir(filepath.Dir(t.Path)), project.FileName)
}
