package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
)

// TestLaunchWolfAgentReadsBackPriorBlockedSession verifies the "wolf starts
// up with prior context" deliverable: a blocked-session.yaml record left by
// an earlier wolf invocation is read back on the next launch and folded into
// both the rendered instructions and .orch/context.yaml, instead of the wolf
// re-deriving its diagnosis from scratch every time.
func TestLaunchWolfAgentReadsBackPriorBlockedSession(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.runner = &runner.Runner{
		Launcher: spawningLauncher{},
		Command:  func(agent.Kind, string) []string { return []string{"true"} },
	}

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")

	prior := &project.BlockedSession{
		BlockedReason: `task "add-healthz" failed after 3 attempts`,
		Summary:       "1. Fix the missing import in handler.go\n2. Re-run the task",
		Attempted:     []string{"reverted the last commit"},
	}
	blockedSessionPath := project.BlockedSessionPath(projectPath)
	if err := project.SaveBlockedSessionAs(blockedSessionPath, prior, project.WriterAgent); err != nil {
		t.Fatalf("SaveBlockedSessionAs: %v", err)
	}

	p := &project.Project{Branch: "feat/x", Status: project.StatusBlocked}
	d.launchWolfAgent(projectPath, p, "new failure")

	wolfKey := wolfSessionKey(projectPath)
	if !d.hasSession(wolfKey) {
		t.Fatalf("wolf did not launch; hasSession(%q) = false", wolfKey)
	}

	instructions, err := os.ReadFile(filepath.Join(root, ".orch", "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	out := string(instructions)
	for _, want := range []string{
		"Fix the missing import in handler.go",
		"reverted the last commit",
		blockedSessionPath,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions.md missing %q:\n%s", want, out)
		}
	}

	ctxBytes, err := os.ReadFile(filepath.Join(root, ".orch", "context.yaml"))
	if err != nil {
		t.Fatalf("read context.yaml: %v", err)
	}
	if !strings.Contains(string(ctxBytes), "Fix the missing import in handler.go") {
		t.Errorf("context.yaml missing prior summary:\n%s", ctxBytes)
	}
}

// TestLaunchWolfAgentWithNoPriorBlockedSession verifies a project blocked for
// the first time (no blocked-session.yaml on disk yet) launches the wolf
// normally rather than treating the missing file as an error.
func TestLaunchWolfAgentWithNoPriorBlockedSession(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.runner = &runner.Runner{
		Launcher: spawningLauncher{},
		Command:  func(agent.Kind, string) []string { return []string{"true"} },
	}

	root := t.TempDir()
	projectPath := filepath.Join(root, ".project.yaml")

	p := &project.Project{Branch: "feat/x", Status: project.StatusBlocked}
	d.launchWolfAgent(projectPath, p, "first failure")

	wolfKey := wolfSessionKey(projectPath)
	if !d.hasSession(wolfKey) {
		t.Fatalf("wolf did not launch; hasSession(%q) = false", wolfKey)
	}

	instructions, err := os.ReadFile(filepath.Join(root, ".orch", "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	if strings.Contains(string(instructions), "previous wolf investigation") {
		t.Errorf("should not claim a previous investigation with no prior record:\n%s", instructions)
	}
}
