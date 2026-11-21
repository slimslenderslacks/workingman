package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitT runs git in dir with a fixed identity and signing off, so the tests
// don't depend on the host's git config.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newShared makes a shared workspace with one repo per name, each a git repo
// on branch with a single commit.
func newShared(t *testing.T, branch string, repos ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	shared := t.TempDir()
	for _, r := range repos {
		dir := filepath.Join(shared, r)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gitT(t, dir, "init", "-q", "-b", branch)
		writeT(t, filepath.Join(dir, "README"), r+"\n")
		gitT(t, dir, "add", "-A")
		gitT(t, dir, "commit", "-q", "-m", "init")
	}
	return shared
}

func writeT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func spec(shared, task string, repos ...string) Spec {
	return Spec{
		Shared:       shared,
		Task:         task,
		Branch:       BranchName("feat/x", task),
		SharedBranch: "feat/x",
		Repos:        repos,
	}
}

func TestNames(t *testing.T) {
	if got := BranchName("feat/x", "api"); got != "wm/feat/x/api" {
		t.Errorf("BranchName = %q", got)
	}
	if got := Root("/ws", "api"); got != "/ws/.worktrees/api" {
		t.Errorf("Root = %q", got)
	}
	if got := Path("/ws", "api", "repo"); got != "/ws/.worktrees/api/repo" {
		t.Errorf("Path = %q", got)
	}
}

func TestEnsureCreatesFromSharedTip(t *testing.T) {
	shared := newShared(t, "feat/x", "a", "b")
	tip := gitT(t, filepath.Join(shared, "a"), "rev-parse", "HEAD")
	ctx := context.Background()
	s := spec(shared, "api", "a", "b")

	res, err := Manager{}.Ensure(ctx, s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !res.Fresh() || len(res.Repos) != 2 {
		t.Fatalf("result = %+v, want two created repos", res)
	}
	if res.Repos[0].Base != tip {
		t.Errorf("base = %q, want shared tip %q", res.Repos[0].Base, tip)
	}
	for _, r := range []string{"a", "b"} {
		wt := Path(shared, "api", r)
		if got := gitT(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "wm/feat/x/api" {
			t.Errorf("%s worktree on %q, want the task branch", r, got)
		}
		if _, err := os.Stat(filepath.Join(wt, "README")); err != nil {
			t.Errorf("%s worktree missing checkout: %v", r, err)
		}
	}
	// The shared tree is untouched and still on its own branch.
	if got := gitT(t, filepath.Join(shared, "a"), "rev-parse", "--abbrev-ref", "HEAD"); got != "feat/x" {
		t.Errorf("shared repo moved to %q", got)
	}
}

func TestEnsureReusesDirtyWorktreeUntouched(t *testing.T) {
	shared := newShared(t, "feat/x", "a")
	ctx := context.Background()
	s := spec(shared, "api", "a")
	first, err := Manager{}.Ensure(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	wt := Path(shared, "api", "a")
	// A previous attempt: one commit plus uncommitted and untracked edits.
	writeT(t, filepath.Join(wt, "done.go"), "package a\n")
	gitT(t, wt, "add", "done.go")
	gitT(t, wt, "commit", "-q", "-m", "attempt 1")
	head := gitT(t, wt, "rev-parse", "HEAD")
	writeT(t, filepath.Join(wt, "README"), "edited\n")
	writeT(t, filepath.Join(wt, "scratch.txt"), "wip\n")

	res, err := Manager{}.Ensure(ctx, s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	rr := res.Repos[0]
	if rr.Action != Reused || !rr.Dirty || rr.Base != "" {
		t.Fatalf("result = %+v, want a dirty reuse with no new base", rr)
	}
	if res.Fresh() {
		t.Error("a reuse must not report Fresh")
	}
	if got := gitT(t, wt, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s, want %s", got, head)
	}
	for file, want := range map[string]string{"README": "edited\n", "scratch.txt": "wip\n"} {
		b, err := os.ReadFile(filepath.Join(wt, file))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q (%v), want %q kept", file, b, err, want)
		}
	}
	_ = first
}

func TestEnsureReattachesBranchWhoseDirectoryIsGone(t *testing.T) {
	shared := newShared(t, "feat/x", "a")
	ctx := context.Background()
	s := spec(shared, "api", "a")
	if _, err := (Manager{}).Ensure(ctx, s); err != nil {
		t.Fatal(err)
	}
	wt := Path(shared, "api", "a")
	base := gitT(t, wt, "rev-parse", "HEAD")
	writeT(t, filepath.Join(wt, "done.go"), "package a\n")
	gitT(t, wt, "add", "done.go")
	gitT(t, wt, "commit", "-q", "-m", "attempt 1")
	head := gitT(t, wt, "rev-parse", "HEAD")
	if err := os.RemoveAll(Root(shared, "api")); err != nil {
		t.Fatal(err)
	}

	res, err := Manager{}.Ensure(ctx, s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	rr := res.Repos[0]
	if rr.Action != Reattached {
		t.Fatalf("action = %s, want reattached", rr.Action)
	}
	if rr.Base != base {
		t.Errorf("base = %s, want merge base %s", rr.Base, base)
	}
	if got := gitT(t, wt, "rev-parse", "HEAD"); got != head {
		t.Errorf("re-attached HEAD = %s, want the branch's commit %s", got, head)
	}
}

func TestEnsureAddsMissingRepoToExistingRoot(t *testing.T) {
	shared := newShared(t, "feat/x", "a", "b")
	ctx := context.Background()
	if _, err := (Manager{}).Ensure(ctx, spec(shared, "api", "a")); err != nil {
		t.Fatal(err)
	}
	res, err := Manager{}.Ensure(ctx, spec(shared, "api", "a", "b"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Repos[0].Action != Reused || res.Repos[1].Action != Created {
		t.Fatalf("actions = %s,%s; want reused,created", res.Repos[0].Action, res.Repos[1].Action)
	}
	if res.Repos[1].Base == "" {
		t.Error("the added repo must report its base")
	}
}

func TestEnsureMovesStrayDirectoryAside(t *testing.T) {
	shared := newShared(t, "feat/x", "a")
	ctx := context.Background()
	stray := Path(shared, "api", "a")
	writeT(t, filepath.Join(stray, "notes.txt"), "keep me\n")
	m := Manager{Now: func() time.Time { return time.Unix(1700000000, 0) }}

	res, err := m.Ensure(ctx, spec(shared, "api", "a"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	rr := res.Repos[0]
	want := filepath.Join(shared, DirName, StaleDirName, "api-1700000000", "a")
	if rr.Action != Created || rr.Stale != want {
		t.Fatalf("result = %+v, want created with stale %s", rr, want)
	}
	if b, err := os.ReadFile(filepath.Join(want, "notes.txt")); err != nil || string(b) != "keep me\n" {
		t.Errorf("stray content not preserved: %q %v", b, err)
	}
}

func TestEnsureMovesWorktreeOnWrongBranchAside(t *testing.T) {
	shared := newShared(t, "feat/x", "a")
	ctx := context.Background()
	repo := filepath.Join(shared, "a")
	path := Path(shared, "api", "a")
	gitT(t, repo, "worktree", "add", "-q", "-b", "other", path)

	res, err := Manager{}.Ensure(ctx, spec(shared, "api", "a"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Repos[0].Action != Created || res.Repos[0].Stale == "" {
		t.Fatalf("result = %+v, want the other-branch worktree moved aside", res.Repos[0])
	}
	if got := gitT(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "wm/feat/x/api" {
		t.Errorf("new worktree on %q", got)
	}
	if got := gitT(t, res.Repos[0].Stale, "rev-parse", "--abbrev-ref", "HEAD"); got != "other" {
		t.Errorf("moved worktree on %q, want it still registered on other", got)
	}
}

func TestEnsureNestedBranchCollisionFails(t *testing.T) {
	shared := newShared(t, "feat/x", "a")
	repo := filepath.Join(shared, "a")
	// Another project on branch wm/feat/x/api/... would own the directory
	// refs/heads/wm/feat/x/api/, so the task branch wm/feat/x/api can't exist.
	gitT(t, repo, "branch", "wm/feat/x/api/child")

	_, err := Manager{}.Ensure(context.Background(), spec(shared, "api", "a"))
	var re *RepoError
	if !errors.As(err, &re) || re.Repo != "a" {
		t.Fatalf("err = %v, want a RepoError for repo a", err)
	}
}

func TestRemoveDeletesWorktreeAndBranch(t *testing.T) {
	shared := newShared(t, "feat/x", "a", "b")
	ctx := context.Background()
	s := spec(shared, "api", "a", "b")
	if _, err := (Manager{}).Ensure(ctx, s); err != nil {
		t.Fatal(err)
	}
	writeT(t, filepath.Join(Path(shared, "api", "a"), "leftover.txt"), "x\n")
	writeT(t, filepath.Join(Root(shared, "api"), ".orch", "context.yaml"), "kind: task\n")

	if err := (Manager{}).Remove(ctx, s); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(Root(shared, "api")); !os.IsNotExist(err) {
		t.Errorf("root still exists: %v", err)
	}
	for _, r := range []string{"a", "b"} {
		ok, err := Manager{}.BranchExists(ctx, filepath.Join(shared, r), s.Branch)
		if err != nil || ok {
			t.Errorf("%s: branch exists=%v err=%v, want deleted", r, ok, err)
		}
	}
	// Idempotent.
	if err := (Manager{}).Remove(ctx, s); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}

func TestIsAncestorAndGitCommonDir(t *testing.T) {
	shared := newShared(t, "feat/x", "a")
	ctx := context.Background()
	s := spec(shared, "api", "a")
	if _, err := (Manager{}).Ensure(ctx, s); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(shared, "a")
	wt := Path(shared, "api", "a")
	writeT(t, filepath.Join(wt, "x.go"), "package a\n")
	gitT(t, wt, "add", "x.go")
	gitT(t, wt, "commit", "-q", "-m", "work")

	m := Manager{}
	if ok, err := m.IsAncestor(ctx, repo, s.Branch, "feat/x"); err != nil || ok {
		t.Fatalf("before ff: ancestor=%v err=%v, want false", ok, err)
	}
	gitT(t, repo, "merge", "-q", "--ff-only", s.Branch)
	if ok, err := m.IsAncestor(ctx, repo, s.Branch, "feat/x"); err != nil || !ok {
		t.Fatalf("after ff: ancestor=%v err=%v, want true", ok, err)
	}

	for _, dir := range []string{repo, wt} {
		got, err := m.GitCommonDir(ctx, dir)
		if err != nil {
			t.Fatalf("GitCommonDir(%s): %v", dir, err)
		}
		if realPath(got) != realPath(filepath.Join(repo, ".git")) {
			t.Errorf("GitCommonDir(%s) = %s, want %s/.git", dir, got, repo)
		}
	}
}

func TestEnsureUsesInjectedRunner(t *testing.T) {
	shared := t.TempDir()
	var calls []string
	m := Manager{Run: func(_ context.Context, dir string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "for-each-ref":
			return nil, nil
		case "rev-parse":
			return []byte("abc1234def\n"), nil
		}
		return nil, nil
	}}
	res, err := m.Ensure(context.Background(), spec(shared, "api", "a"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Repos[0].Base != "abc1234def" {
		t.Errorf("base = %q", res.Repos[0].Base)
	}
	want := "worktree add -b wm/feat/x/api " + Path(shared, "api", "a") + " abc1234def"
	if calls[len(calls)-1] != want {
		t.Errorf("last call = %q, want %q", calls[len(calls)-1], want)
	}
}

func TestParseWorktreeList(t *testing.T) {
	out := "worktree /r\nHEAD 1\nbranch refs/heads/main\n\nworktree /r/.worktrees/t/r\nHEAD 2\ndetached\n"
	got := parseWorktreeList(out)
	if len(got) != 2 || got[0].branch != "refs/heads/main" || got[1].path != "/r/.worktrees/t/r" || got[1].branch != "" {
		t.Errorf("parse = %+v", got)
	}
}

// RebaseInProgress / AbortRebase / GitPath operate on the worktree's own git
// dir, and ChangedPaths sees committed, modified and untracked paths.
func TestRebaseRecoveryAndChangedPaths(t *testing.T) {
	shared := newShared(t, "feat/x", "a")
	ctx := context.Background()
	s := spec(shared, "api", "a")
	res, err := (Manager{}).Ensure(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	base := res.Repos[0].Base
	repo := filepath.Join(shared, "a")
	wt := Path(shared, "api", "a")
	m := Manager{}

	writeT(t, filepath.Join(wt, "README"), "task\n")
	gitT(t, wt, "commit", "-q", "-am", "task")
	saved := gitT(t, wt, "rev-parse", "HEAD")
	writeT(t, filepath.Join(repo, "README"), "human\n")
	gitT(t, repo, "commit", "-q", "-am", "human")

	if busy, err := m.RebaseInProgress(ctx, wt); err != nil || busy {
		t.Fatalf("before rebase: busy=%v err=%v", busy, err)
	}
	cmd := exec.Command("git", "-C", wt, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "rebase", "-q", "feat/x")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected a conflict: %s", out)
	}
	if busy, err := m.RebaseInProgress(ctx, wt); err != nil || !busy {
		t.Fatalf("mid-rebase: busy=%v err=%v", busy, err)
	}
	if busy, _ := m.RebaseInProgress(ctx, repo); busy {
		t.Error("the shared checkout must not see the worktree's rebase")
	}
	lock, err := m.GitPath(ctx, wt, "index.lock")
	if err != nil || !strings.Contains(lock, filepath.Join(".git", "worktrees")+string(filepath.Separator)) {
		t.Errorf("GitPath(index.lock) = %q, %v: want the worktree's admin dir", lock, err)
	}
	if err := m.AbortRebase(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, wt, "rev-parse", "HEAD"); got != saved {
		t.Errorf("after abort HEAD = %s, want %s", got, saved)
	}

	writeT(t, filepath.Join(wt, "new.txt"), "x\n")
	writeT(t, filepath.Join(wt, "docs", "a.md"), "x\n")
	got, err := m.ChangedPaths(ctx, wt, base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "README,docs/a.md,new.txt" {
		t.Errorf("ChangedPaths = %v", got)
	}

	// DiffNames sees committed changes only.
	got, err = m.DiffNames(ctx, wt, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "README" {
		t.Errorf("DiffNames = %v, want [README]", got)
	}
}

// Branches lists one namespace only, List/CheckedOut see registered
// worktrees, Prune drops a removed directory's entry, and DeleteBranch
// removes the ref.
func TestBranchesListPruneDelete(t *testing.T) {
	shared := newShared(t, "feat/x", "a")
	ctx := context.Background()
	m := Manager{}
	repo := filepath.Join(shared, "a")
	if _, err := m.Ensure(ctx, spec(shared, "api", "a")); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "branch", "wm/feat/x-other/z")
	gitT(t, repo, "branch", "wm/feat/x/loose")

	got, err := m.Branches(ctx, repo, BranchPrefix+"feat/x/")
	if err != nil || strings.Join(got, ",") != "wm/feat/x/api,wm/feat/x/loose" {
		t.Errorf("Branches = %v %v", got, err)
	}
	path := Path(shared, "api", "a")
	if b, ok, err := m.CheckedOut(ctx, repo, path); err != nil || !ok || b != "refs/heads/wm/feat/x/api" {
		t.Errorf("CheckedOut = %q %v %v", b, ok, err)
	}
	if err := os.RemoveAll(Root(shared, "api")); err != nil {
		t.Fatal(err)
	}
	if err := m.Prune(ctx, repo); err != nil {
		t.Fatal(err)
	}
	wts, err := m.List(ctx, repo)
	if err != nil || len(wts) != 1 {
		t.Errorf("List after prune = %+v %v, want the main tree only", wts, err)
	}
	if err := m.DeleteBranch(ctx, repo, "wm/feat/x/api"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.BranchExists(ctx, repo, "wm/feat/x/api"); ok {
		t.Error("branch still exists")
	}
}

func TestGitVersionCheck(t *testing.T) {
	m := Manager{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("git version 2.39.3 (Apple Git-146)\n"), nil
	}}
	if v, err := m.GitVersion(context.Background()); err != nil || v != "2.39.3" {
		t.Errorf("GitVersion = %q %v", v, err)
	}
	for v, ok := range map[string]bool{
		"2.39.3": true, "2.31.0": true, "3.0": true, "2.45.windows.1": true, "2.31.0.rc1": true,
		"2.30.9": false, "1.9.5": false, "": false, "garbage": false,
	} {
		if err := CheckGitVersion(v); (err == nil) != ok {
			t.Errorf("CheckGitVersion(%q) = %v, want ok=%v", v, err, ok)
		}
	}
	// The git running these tests is new enough.
	if v, err := (Manager{}).GitVersion(context.Background()); err != nil || CheckGitVersion(v) != nil {
		t.Errorf("host git %q: %v / %v", v, err, CheckGitVersion(v))
	}
}

// RemoveProject is :archive's teardown (X6): every task worktree, the stale
// ones included, and the project's refs under refs/heads/wm/<pb>/ and
// refs/wm/<pb>/ go, unintegrated work and all. Another project's refs, and a
// nested project's, stay.
func TestRemoveProjectDeletesWorktreesAndRefs(t *testing.T) {
	shared := newShared(t, "feat/x", "a", "b")
	ctx := context.Background()
	m := Manager{}
	if _, err := m.Ensure(ctx, spec(shared, "api", "a", "b")); err != nil {
		t.Fatal(err)
	}
	// Unintegrated, uncommitted work in one, committed work in the other.
	writeT(t, filepath.Join(Path(shared, "api", "a"), "wip"), "x\n")
	writeT(t, filepath.Join(Path(shared, "api", "b"), "done"), "x\n")
	gitT(t, Path(shared, "api", "b"), "add", "-A")
	gitT(t, Path(shared, "api", "b"), "commit", "-q", "-m", "work")
	// A worktree moved aside to .stale/ is still registered.
	if _, err := m.Ensure(ctx, spec(shared, "old", "a")); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(shared, DirName, StaleDirName, "old-1", "a")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	repoA := filepath.Join(shared, "a")
	gitT(t, repoA, "worktree", "move", Path(shared, "old", "a"), stale)
	gitT(t, repoA, "branch", "wm/feat/x/branchless")
	gitT(t, repoA, "update-ref", "refs/wm/feat/x/attempt", "HEAD")
	gitT(t, repoA, "branch", "wm/feat/x-other/keep")
	gitT(t, repoA, "update-ref", "refs/wm/feat/x-other/keep", "HEAD")

	refs, err := m.ProjectRefs(ctx, repoA, "feat/x")
	if err != nil {
		t.Fatal(err)
	}
	want := "refs/heads/wm/feat/x/api,refs/heads/wm/feat/x/branchless,refs/heads/wm/feat/x/old,refs/wm/feat/x/attempt"
	if strings.Join(refs, ",") != want {
		t.Errorf("ProjectRefs = %v, want %s", refs, want)
	}

	if err := m.RemoveProject(ctx, shared, "feat/x", []string{"a", "b", "missing"}); err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, DirName)); !os.IsNotExist(err) {
		t.Errorf(".worktrees/ still there: %v", err)
	}
	for _, repo := range []string{"a", "b"} {
		dir := filepath.Join(shared, repo)
		if refs, err := m.ProjectRefs(ctx, dir, "feat/x"); err != nil || len(refs) != 0 {
			t.Errorf("%s: refs left = %v %v", repo, refs, err)
		}
		if wts, err := m.List(ctx, dir); err != nil || len(wts) != 1 {
			t.Errorf("%s: worktrees left = %+v %v", repo, wts, err)
		}
	}
	if got := gitT(t, repoA, "for-each-ref", "--format=%(refname)", "refs/heads/wm/", "refs/wm/"); got != "refs/heads/wm/feat/x-other/keep\nrefs/wm/feat/x-other/keep" {
		t.Errorf("other projects' refs = %q", got)
	}
	// The shared branch and its tree are untouched.
	if got := gitT(t, repoA, "rev-parse", "--abbrev-ref", "HEAD"); got != "feat/x" {
		t.Errorf("shared HEAD = %q", got)
	}
}
