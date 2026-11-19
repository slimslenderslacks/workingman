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
