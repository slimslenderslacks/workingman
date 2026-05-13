package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// StubManager is a minimal Manager that does not invoke wsp. It creates
// <Root>/<branch>/ on Create and removes it on Remove. The repos argument is
// recorded into <workspace>/.orch/stub-repos.txt purely as a breadcrumb for
// tests that want to assert what was requested — the stub never actually
// clones anything.
//
// Use this in tests and during early daemon development so the orchestrator
// can run without depending on a real wsp registry, mirrors, or git.
type StubManager struct {
	Root string
}

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
	return dir, nil
}

func (m *StubManager) Path(branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("workspace: branch is required")
	}
	return filepath.Join(m.Root, branch), nil
}

func (m *StubManager) Remove(_ context.Context, branch string) error {
	dir := filepath.Join(m.Root, branch)
	err := os.RemoveAll(dir)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
