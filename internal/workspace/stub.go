package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// StubManager is a minimal Manager that does not invoke wsp. It creates
// <Root>/<branch>/ on Create and removes it on Remove. The repos argument is
// recorded into <workspace>/.orch/stub-repos.txt purely as a breadcrumb for
// tests that want to assert what was requested — the stub never actually
// clones anything.
//
// With Git set, it also creates each repo's directory (Repo.DirName) as a real
// git repository on the branch with one empty commit, so code that works on
// the checkout — the per-task worktrees in internal/worktree — can run against
// the stub. Nothing is ever fetched or pushed.
//
// Use this in tests and during early daemon development so the orchestrator
// can run without depending on a real wsp registry, mirrors, or git.
type StubManager struct {
	Root string
	// Git makes Create and AddRepos `git init` a repo per Repo.DirName (see
	// initRepo). Off by default: most tests only need the directory.
	Git bool
}

// NewStub returns a StubManager that creates workspaces under root.
func NewStub(root string) *StubManager {
	return &StubManager{Root: root}
}

func (m *StubManager) Create(_ context.Context, branch string, repos []Repo) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("workspace: branch is required")
	}
	dir := filepath.Join(m.Root, branch)
	if err := os.MkdirAll(filepath.Join(dir, ".orch"), 0o755); err != nil {
		return "", err
	}
	if len(repos) > 0 {
		var refs string
		for _, r := range repos {
			refs += r.Ref() + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, ".orch", "stub-repos.txt"), []byte(refs), 0o644); err != nil {
			return "", err
		}
	}
	if err := m.initRepos(dir, branch, repos); err != nil {
		return "", err
	}
	return dir, nil
}

// GitBacked reports whether the stub creates real git repos (Git).
func (m *StubManager) GitBacked() bool { return m.Git }

// NewGitStub returns a StubManager that creates real git repos (Git set).
func NewGitStub(root string) *StubManager {
	return &StubManager{Root: root, Git: true}
}

// initRepos creates each repo as a git repository when m.Git is set.
func (m *StubManager) initRepos(dir, branch string, repos []Repo) error {
	if !m.Git {
		return nil
	}
	for _, r := range repos {
		if err := initRepo(filepath.Join(dir, r.DirName()), branch); err != nil {
			return fmt.Errorf("workspace: stub git repo %s: %w", r.DirName(), err)
		}
	}
	return nil
}

// initRepo makes dir a git repository checked out on branch with one empty
// commit, unless it already is one (Create is idempotent). The identity and
// signing settings are passed per command so the host's git config can't
// make it prompt or fail.
func initRepo(dir, branch string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"-c", "user.name=orch-stub", "-c", "user.email=orch-stub@localhost", "-c", "commit.gpgsign=false",
			"commit", "-q", "--allow-empty", "-m", "stub workspace"},
	} {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (m *StubManager) Path(branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("workspace: branch is required")
	}
	return filepath.Join(m.Root, branch), nil
}

// AddRepos records the addition into the same breadcrumb file Create writes,
// prefixed so tests can distinguish an initial Create from a later add. The
// stub never actually clones anything, mirroring Create (with Git set it
// `git init`s the added repos).
func (m *StubManager) AddRepos(_ context.Context, branch string, repos []Repo) error {
	if len(repos) == 0 {
		return nil
	}
	if branch == "" {
		return fmt.Errorf("workspace: branch is required")
	}
	dir := filepath.Join(m.Root, branch)
	f, err := os.OpenFile(filepath.Join(dir, ".orch", "stub-repos.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, r := range repos {
		if _, err := f.WriteString("added:" + r.Ref() + "\n"); err != nil {
			return err
		}
	}
	return m.initRepos(dir, branch, repos)
}

func (m *StubManager) Remove(_ context.Context, branch string, _ bool) error {
	dir := filepath.Join(m.Root, branch)
	err := os.RemoveAll(dir)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
