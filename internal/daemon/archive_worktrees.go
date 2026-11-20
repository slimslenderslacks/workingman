package daemon

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/prompts"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// Statuses prepareArchive reports for work with no task to take a status
// from.
const (
	unintegratedOrphaned = "orphaned" // a worktree or branch whose task file is gone
	unintegratedStale    = "stale"    // a directory W4 moved aside to .worktrees/.stale/
)

// prepareArchive is the pre-launch step of the archive agent
// (docs/parallel-task-worktrees.md W6, X6 step 4). It runs once nothing is in
// flight, and does two things:
//
//  1. Cleans up what is safe to drop: the W7 sweep, run inline, removes the
//     worktrees and branches of committed tasks whose work is on the shared
//     branch (and of deleted tasks that hold nothing), and prunes git's
//     worktree admin entries.
//  2. Returns everything left that holds work not on the shared branch —
//     each task worktree or task branch that is unintegrated or dirty, and
//     each .worktrees/.stale/ directory — for the archive agent's context.
//     `:archive` deletes all of it with the workspace, so the agent's report
//     has to name it.
//
// Without git-backed workspaces, or for a workspace that was never
// provisioned, it does nothing.
func (d *Daemon) prepareArchive(projectPath string, p *project.Project) []prompts.UnintegratedTask {
	if !d.worktreesEnabled() || p.Branch == "" {
		return nil
	}
	shared, err := d.runner.Workspaces.Path(p.Branch)
	if err != nil {
		return nil
	}
	if info, err := os.Stat(shared); err != nil || !info.IsDir() {
		return nil
	}
	d.sweepWorktrees(projectPath)

	mu := d.worktreeLock(projectPath)
	mu.Lock()
	defer mu.Unlock()

	tasks, _ := sweepTasks(filepath.Join(filepath.Dir(projectPath), "tasks"))
	sw := &sweep{d: d, projectPath: projectPath, p: p, shared: shared, repos: repoDirNames(p)}

	// Every task name with a worktree root or a task branch left.
	names := map[string]bool{}
	var stale []string
	entries, _ := os.ReadDir(filepath.Join(shared, worktree.DirName))
	for _, e := range entries {
		switch {
		case !e.IsDir():
		case e.Name() == worktree.StaleDirName:
			sub, _ := os.ReadDir(filepath.Join(shared, worktree.DirName, worktree.StaleDirName))
			for _, s := range sub {
				stale = append(stale, filepath.Join(shared, worktree.DirName, worktree.StaleDirName, s.Name()))
			}
		default:
			names[e.Name()] = true
		}
	}
	prefix := worktree.BranchPrefix + p.Branch + "/"
	for _, repo := range sw.repos {
		dir := filepath.Join(shared, repo)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		branches, err := d.worktrees.Branches(d.ctx, dir, prefix)
		if err != nil {
			continue
		}
		for _, b := range branches {
			if name := strings.TrimPrefix(b, prefix); !strings.Contains(name, "/") {
				names[name] = true
			}
		}
	}

	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	var out []prompts.UnintegratedTask
	for _, name := range sorted {
		t := tasks[name]
		branch, status := "", unintegratedOrphaned
		if t != nil {
			branch, status = t.TaskBranch, string(t.Status)
		}
		spec := sw.spec(name, branch)
		root := ""
		if _, err := os.Lstat(spec.Root()); err == nil {
			root = spec.Root()
		}
		// With a root on disk, uncommitted edits and stray files count too:
		// they are lost with the workspace just as unmerged commits are.
		why := sw.unintegrated(spec, root != "")
		if why == "" {
			continue
		}
		if t != nil && t.Status == task.StatusCommitted {
			status = string(task.StatusCommitted)
		}
		out = append(out, prompts.UnintegratedTask{Name: name, Status: status, Branch: spec.Branch, Worktree: root, Why: why})
	}
	for _, dir := range stale {
		out = append(out, prompts.UnintegratedTask{
			Name:     filepath.Base(dir),
			Status:   unintegratedStale,
			Worktree: dir,
			Why:      "moved aside to " + worktree.StaleDirName + "/ by the daemon; never swept",
		})
	}
	if len(out) > 0 {
		list := make([]string, len(out))
		for i, u := range out {
			list[i] = u.Name
		}
		d.audit.Log("archive_unintegrated", "path", projectPath,
			"count", strconv.Itoa(len(out)), "tasks", strings.Join(list, ","))
	}
	return out
}
