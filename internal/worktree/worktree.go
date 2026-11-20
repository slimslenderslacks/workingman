package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DirName is the directory, at the root of a project's shared workspace, that
// holds every task worktree (W2). The daemon owns it; wsp never reads it.
const DirName = ".worktrees"

// StaleDirName is the directory under DirName that Ensure moves unrecognised
// directories into instead of deleting them (W4). Nothing sweeps it
// automatically.
const StaleDirName = ".stale"

// BranchPrefix is the namespace every task branch lives under (W3).
const BranchPrefix = "wm/"

// Root is the task worktree root for task in the shared workspace: the task
// agent's working directory, holding one worktree per repo and its .orch/.
func Root(shared, task string) string {
	return filepath.Join(shared, DirName, task)
}

// Path is the worktree of one repo (its workspace dir name) inside the task
// worktree root.
func Path(shared, task, repo string) string {
	return filepath.Join(Root(shared, task), repo)
}

// BranchName is the task branch for task in a project whose shared branch is
// projectBranch: wm/<project-branch>/<task>. It cannot be
// <project-branch>/task/<task>, because git cannot hold a ref
// refs/heads/<project-branch> and a directory of refs under the same name.
func BranchName(projectBranch, task string) string {
	return BranchPrefix + projectBranch + "/" + task
}

// RunFunc runs `git -C dir args...` and returns its stdout. A non-nil error
// should carry git's stderr, since that is what explains the failure.
type RunFunc func(ctx context.Context, dir string, args ...string) ([]byte, error)

// ExecGit is the production RunFunc: it runs the git binary on PATH.
func ExecGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}

// Manager wraps the git operations on task worktrees. The zero value runs the
// real git binary; tests may set Run (and Now) to fakes.
type Manager struct {
	Run RunFunc
	// Now stamps the names of directories moved aside to .stale/. Nil means
	// time.Now.
	Now func() time.Time
}

func (m Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	run := m.Run
	if run == nil {
		run = ExecGit
	}
	out, err := run(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

func (m Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Spec identifies one task worktree.
type Spec struct {
	// Shared is the absolute path of the project's shared wsp workspace.
	Shared string
	// Task is the task's name: the worktree root's directory name.
	Task string
	// Branch is the task branch (BranchName). Stored on the task rather than
	// recomputed, so the caller passes what the task file says when it has one.
	Branch string
	// SharedBranch is the project's branch, checked out in every repo of the
	// shared workspace. New worktrees are cut from its local tip.
	SharedBranch string
	// Repos are the workspace dir names of the project's repos.
	Repos []string
}

// Root is the spec's task worktree root.
func (s Spec) Root() string { return Root(s.Shared, s.Task) }

// Action says what Ensure did for one repo.
type Action string

const (
	// Created: a new task branch was cut from the shared tip and checked out.
	Created Action = "created"
	// Reused: the worktree was already registered on the task branch and was
	// left exactly as it was, dirty or not.
	Reused Action = "reused"
	// Reattached: the task branch existed without a worktree (a directory
	// removed by hand, or a crash mid-add) and was checked out again, so its
	// committed work is kept.
	Reattached Action = "reattached"
)

// RepoResult is what Ensure did for one repo.
type RepoResult struct {
	Repo   string
	Path   string
	Action Action
	// Base is the commit the task branch starts from: the shared tip it was
	// cut from (Created), or its merge base with the shared branch
	// (Reattached). Empty for Reused — the base is whatever the task file
	// already records.
	Base string
	// Dirty is true when a reused worktree has uncommitted changes.
	Dirty bool
	// Stale is where an unrecognised directory at Path was moved to, if any.
	Stale string
}

// Result is what Ensure did for a whole task worktree.
type Result struct {
	Root   string
	Branch string
	Repos  []RepoResult
}

// Fresh reports whether every repo's worktree was newly cut — the task's first
// dispatch, as opposed to a reuse.
func (r Result) Fresh() bool {
	for _, rr := range r.Repos {
		if rr.Action != Created {
			return false
		}
	}
	return len(r.Repos) > 0
}

// Dirty reports whether any reused worktree has uncommitted changes.
func (r Result) Dirty() bool {
	for _, rr := range r.Repos {
		if rr.Dirty {
			return true
		}
	}
	return false
}

// RepoError is a git failure attributed to one repo of a task worktree.
type RepoError struct {
	Repo string
	Err  error
}

func (e *RepoError) Error() string { return fmt.Sprintf("worktree %s: %v", e.Repo, e.Err) }
func (e *RepoError) Unwrap() error { return e.Err }

// Ensure makes sure the task worktree exists, per repo, following the reuse
// rule of docs/parallel-task-worktrees.md W4:
//
//   - a worktree registered at the repo's path on the task branch is reused
//     as it is, dirty or not;
//   - no worktree, but the task branch exists: the branch is re-attached
//     (`git worktree add <path> <branch>`), keeping its commits;
//   - neither: a new branch is cut from the shared tip
//     (`git worktree add -b <branch> <path> refs/heads/<shared-branch>`);
//   - anything else at the path (a directory git does not know as a
//     worktree, or a worktree on another branch) is moved aside to
//     .worktrees/.stale/<task>-<unix-ts>/<repo> and then handled as "no
//     worktree". It is never deleted.
//
// Ensure never resets or cleans a worktree. It stops at the first repo that
// fails and returns a *RepoError, along with the results of the repos already
// handled.
func (m Manager) Ensure(ctx context.Context, s Spec) (Result, error) {
	res := Result{Root: s.Root(), Branch: s.Branch}
	if s.Shared == "" || s.Task == "" || s.Branch == "" || s.SharedBranch == "" {
		return res, fmt.Errorf("worktree: Shared, Task, Branch and SharedBranch are required")
	}
	if err := os.MkdirAll(res.Root, 0o755); err != nil {
		return res, fmt.Errorf("worktree: %w", err)
	}
	for _, repo := range s.Repos {
		rr, err := m.ensureRepo(ctx, s, repo)
		if err != nil {
			return res, &RepoError{Repo: repo, Err: err}
		}
		res.Repos = append(res.Repos, rr)
	}
	return res, nil
}

func (m Manager) ensureRepo(ctx context.Context, s Spec, repo string) (RepoResult, error) {
	repoDir := filepath.Join(s.Shared, repo)
	path := Path(s.Shared, s.Task, repo)
	rr := RepoResult{Repo: repo, Path: path}
	branchRef := "refs/heads/" + s.Branch

	if _, err := os.Lstat(path); err == nil {
		wts, err := m.list(ctx, repoDir)
		if err != nil {
			return rr, err
		}
		wt, registered := findWorktree(wts, path)
		if registered && wt.branch == branchRef {
			rr.Action = Reused
			dirty, err := m.Dirty(ctx, path)
			if err != nil {
				return rr, err
			}
			rr.Dirty = dirty
			return rr, nil
		}
		stale, err := m.moveAside(ctx, s, repo, repoDir, path, registered)
		if err != nil {
			return rr, err
		}
		rr.Stale = stale
	} else if !errors.Is(err, os.ErrNotExist) {
		return rr, err
	}

	// Drop admin entries whose directory is gone, so a worktree removed by
	// hand doesn't keep its branch "checked out" and block the re-attach.
	if _, err := m.git(ctx, repoDir, "worktree", "prune"); err != nil {
		return rr, err
	}
	exists, err := m.BranchExists(ctx, repoDir, s.Branch)
	if err != nil {
		return rr, err
	}
	if exists {
		if _, err := m.git(ctx, repoDir, "worktree", "add", path, s.Branch); err != nil {
			return rr, err
		}
		rr.Action = Reattached
		base, err := m.MergeBase(ctx, repoDir, s.Branch, s.SharedBranch)
		if err != nil {
			return rr, err
		}
		rr.Base = base
		return rr, nil
	}
	base, err := m.git(ctx, repoDir, "rev-parse", "--verify", "refs/heads/"+s.SharedBranch+"^{commit}")
	if err != nil {
		return rr, err
	}
	if _, err := m.git(ctx, repoDir, "worktree", "add", "-b", s.Branch, path, base); err != nil {
		return rr, err
	}
	rr.Action = Created
	rr.Base = base
	return rr, nil
}

// moveAside moves an unrecognised directory at path to
// .worktrees/.stale/<task>-<unix-ts>/<repo> and returns where it went. A
// registered worktree (on the wrong branch) is moved with `git worktree move`
// so git's bookkeeping follows it; anything else is renamed.
func (m Manager) moveAside(ctx context.Context, s Spec, repo, repoDir, path string, registered bool) (string, error) {
	dir := filepath.Join(s.Shared, DirName, StaleDirName, s.Task+"-"+strconv.FormatInt(m.now().Unix(), 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, repo)
	if registered {
		if _, err := m.git(ctx, repoDir, "worktree", "move", path, dest); err != nil {
			return "", err
		}
		return dest, nil
	}
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// Remove deletes the task worktree: per repo `git worktree remove --force`
// and `git branch -D <task-branch>`, then the root directory (with its
// .orch/). It is best-effort and keeps going after a failure, so as much as
// possible is cleaned up; it returns every per-repo failure joined. A repo
// whose worktree or branch is already gone is not an error.
func (m Manager) Remove(ctx context.Context, s Spec) error {
	var errs []error
	for _, repo := range s.Repos {
		repoDir := filepath.Join(s.Shared, repo)
		path := Path(s.Shared, s.Task, repo)
		if _, err := os.Lstat(path); err == nil {
			if _, err := m.git(ctx, repoDir, "worktree", "remove", "--force", path); err != nil {
				errs = append(errs, &RepoError{Repo: repo, Err: err})
				continue
			}
		}
		if _, err := m.git(ctx, repoDir, "worktree", "prune"); err != nil {
			errs = append(errs, &RepoError{Repo: repo, Err: err})
			continue
		}
		exists, err := m.BranchExists(ctx, repoDir, s.Branch)
		if err != nil {
			errs = append(errs, &RepoError{Repo: repo, Err: err})
			continue
		}
		if exists {
			if _, err := m.git(ctx, repoDir, "branch", "-D", s.Branch); err != nil {
				errs = append(errs, &RepoError{Repo: repo, Err: err})
			}
		}
	}
	if len(errs) == 0 {
		if err := os.RemoveAll(s.Root()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// GitCommonDir returns the absolute common git dir of the repo checked out at
// dir — where objects and refs live, and what a task sandbox must mount so
// commits made in a worktree land in the shared repo (W5). For an ordinary
// clone it is <dir>/.git; asking git rather than assuming that keeps working
// if the per-repo directory is itself a worktree.
func (m Manager) GitCommonDir(ctx context.Context, dir string) (string, error) {
	out, err := m.git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return filepath.Clean(out), nil
}

// BranchExists reports whether refs/heads/<branch> exists in the repo at dir.
func (m Manager) BranchExists(ctx context.Context, dir, branch string) (bool, error) {
	run := m.Run
	if run == nil {
		run = ExecGit
	}
	out, err := run(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "refs/heads/"+branch {
			return true, nil
		}
	}
	return false, nil
}

// Dirty reports whether the checkout at dir has uncommitted changes
// (tracked or untracked), per `git status --porcelain`.
func (m Manager) Dirty(ctx context.Context, dir string) (bool, error) {
	out, err := m.git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// RevParse resolves rev to a commit SHA in the repo at dir.
func (m Manager) RevParse(ctx context.Context, dir, rev string) (string, error) {
	return m.git(ctx, dir, "rev-parse", "--verify", rev+"^{commit}")
}

// MergeBase returns the merge base of two branches (or commits) in the repo
// at dir. Bare branch names are resolved under refs/heads/.
func (m Manager) MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	return m.git(ctx, dir, "merge-base", qualify(a), qualify(b))
}

// IsAncestor reports whether commit is an ancestor of (or equal to) rev in the
// repo at dir — the check that a task's work really landed on the shared
// branch (I3, W6). A bare branch name is resolved under refs/heads/.
func (m Manager) IsAncestor(ctx context.Context, dir, commit, rev string) (bool, error) {
	run := m.Run
	if run == nil {
		run = ExecGit
	}
	_, err := run(ctx, dir, "merge-base", "--is-ancestor", qualify(commit), qualify(rev))
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// GitPath resolves a path inside the git dir of the checkout at dir (`git
// rev-parse --git-path`), which for a worktree is its own admin dir: where its
// index.lock and an in-progress rebase's state live.
func (m Manager) GitPath(ctx context.Context, dir, name string) (string, error) {
	out, err := m.git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return filepath.Clean(out), nil
}

// RebaseInProgress reports whether the checkout at dir is in the middle of a
// rebase (a rebase-merge or rebase-apply dir exists in its git dir).
func (m Manager) RebaseInProgress(ctx context.Context, dir string) (bool, error) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		p, err := m.GitPath(ctx, dir, name)
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(p); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// AbortRebase runs `git rebase --abort` in the checkout at dir, restoring the
// branch to its pre-rebase tip.
func (m Manager) AbortRebase(ctx context.Context, dir string) error {
	_, err := m.git(ctx, dir, "rebase", "--abort")
	return err
}

// ChangedPaths lists the repo-relative paths that differ between base and the
// checkout at dir: committed (`git diff --name-only base..HEAD`) plus
// uncommitted and untracked ones (`git status --porcelain`). Sorted, no
// duplicates.
func (m Manager) ChangedPaths(ctx context.Context, dir, base string) ([]string, error) {
	seen := map[string]bool{}
	if base != "" {
		out, err := m.git(ctx, dir, "diff", "--name-only", base+"..HEAD")
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(out, "\n") {
			if l != "" {
				seen[l] = true
			}
		}
	}
	run := m.Run
	if run == nil {
		run = ExecGit
	}
	raw, err := run(ctx, dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if len(l) < 4 {
			continue
		}
		p := l[3:]
		if i := strings.Index(p, " -> "); i >= 0 {
			seen[p[:i]] = true
			p = p[i+4:]
		}
		seen[strings.Trim(p, `"`)] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// DiffNames lists the repo-relative paths that differ between two commits in
// the repo at dir (`git diff --name-only from..to`). Sorted, as git prints
// them.
func (m Manager) DiffNames(ctx context.Context, dir, from, to string) ([]string, error) {
	out, err := m.git(ctx, dir, "diff", "--name-only", from+".."+to)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			paths = append(paths, l)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// qualify turns a bare branch name into its full ref so it cannot be confused
// with a tag or a SHA prefix. Full refs, SHAs and rev expressions pass through.
func qualify(rev string) string {
	if strings.HasPrefix(rev, "refs/") || isHex(rev) || strings.ContainsAny(rev, "^~@:") {
		return rev
	}
	return "refs/heads/" + rev
}

func isHex(s string) bool {
	if len(s) < 7 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

type worktreeEntry struct {
	path   string
	branch string // full ref, "" when detached
}

func (m Manager) list(ctx context.Context, repoDir string) ([]worktreeEntry, error) {
	out, err := m.git(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeList(out), nil
}

// parseWorktreeList parses `git worktree list --porcelain`: blank-line
// separated records of "worktree <path>", "HEAD <sha>", "branch <ref>" (or
// "detached"), plus optional flags.
func parseWorktreeList(out string) []worktreeEntry {
	var list []worktreeEntry
	var cur *worktreeEntry
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			list = append(list, worktreeEntry{path: strings.TrimPrefix(line, "worktree ")})
			cur = &list[len(list)-1]
		case strings.HasPrefix(line, "branch ") && cur != nil:
			cur.branch = strings.TrimPrefix(line, "branch ")
		case line == "":
			cur = nil
		}
	}
	return list
}

// findWorktree returns the registered worktree at path, comparing resolved
// paths (git reports the real path, which may differ from ours through a
// symlink such as macOS's /var → /private/var).
func findWorktree(list []worktreeEntry, path string) (worktreeEntry, bool) {
	want := realPath(path)
	for _, wt := range list {
		if realPath(wt.path) == want {
			return wt, true
		}
	}
	return worktreeEntry{}, false
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	return filepath.Clean(p)
}

// Worktree is one entry of `git worktree list --porcelain`.
type Worktree struct {
	Path string
	// Branch is the checked-out branch's full ref, "" when detached.
	Branch string
}

// List returns the worktrees registered in the repo at repoDir, the main
// working tree included.
func (m Manager) List(ctx context.Context, repoDir string) ([]Worktree, error) {
	wts, err := m.list(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	out := make([]Worktree, len(wts))
	for i, wt := range wts {
		out[i] = Worktree{Path: wt.path, Branch: wt.branch}
	}
	return out, nil
}

// CheckedOut returns the branch (full ref) of the worktree registered at
// path in the repo at repoDir, and whether one is registered there at all.
func (m Manager) CheckedOut(ctx context.Context, repoDir, path string) (string, bool, error) {
	wts, err := m.list(ctx, repoDir)
	if err != nil {
		return "", false, err
	}
	wt, ok := findWorktree(wts, path)
	return wt.branch, ok, nil
}

// Prune runs `git worktree prune` in the repo at repoDir, dropping the admin
// entries of worktrees whose directory is gone (W7 step 1).
func (m Manager) Prune(ctx context.Context, repoDir string) error {
	_, err := m.git(ctx, repoDir, "worktree", "prune")
	return err
}

// Branches lists the branches under refs/heads/<prefix> in the repo at dir,
// as names without refs/heads/. A prefix ending in "/" matches a namespace
// (BranchPrefix + project branch + "/").
func (m Manager) Branches(ctx context.Context, dir, prefix string) ([]string, error) {
	out, err := m.git(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/heads/"+prefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(l, "refs/heads/"); ok && strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// DeleteBranch force-deletes refs/heads/<branch> in the repo at dir.
func (m Manager) DeleteBranch(ctx context.Context, dir, branch string) error {
	_, err := m.git(ctx, dir, "branch", "-D", branch)
	return err
}

// MinGitVersion is the oldest git the worktree code works with: `rev-parse
// --path-format=absolute` (GitCommonDir, GitPath) arrived in 2.31, after
// everything else used here (`worktree add/list --porcelain/prune/remove`,
// `--git-common-dir`).
var MinGitVersion = [2]int{2, 31}

// GitVersion returns the version `git version` reports, e.g. "2.39.3" from
// "git version 2.39.3 (Apple Git-146)".
func (m Manager) GitVersion(ctx context.Context) (string, error) {
	out, err := m.git(ctx, "", "version")
	if err != nil {
		return "", err
	}
	v, ok := strings.CutPrefix(out, "git version ")
	if !ok {
		return "", fmt.Errorf("unrecognised git version output %q", out)
	}
	if f := strings.Fields(v); len(f) > 0 {
		v = f[0]
	}
	return v, nil
}

// CheckGitVersion reports an error when version (as GitVersion returns it)
// is older than MinGitVersion or cannot be parsed.
func CheckGitVersion(version string) error {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("cannot parse git version %q", version)
	}
	var v [2]int
	for i := range v {
		digits := parts[i]
		if j := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); j >= 0 {
			digits = digits[:j]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return fmt.Errorf("cannot parse git version %q", version)
		}
		v[i] = n
	}
	if v[0] < MinGitVersion[0] || (v[0] == MinGitVersion[0] && v[1] < MinGitVersion[1]) {
		return fmt.Errorf("git %s is too old: per-task worktrees need git %d.%d or later", version, MinGitVersion[0], MinGitVersion[1])
	}
	return nil
}
