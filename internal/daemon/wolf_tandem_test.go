package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
)

// spawningLauncher hands back a fresh, un-closed stubSession per launch so the
// tracked session stays "running" for the duration of the assertions. Unlike
// the sessions_test stubLauncher (which returns nil), this one exercises the
// full startSession → trackSession path.
type spawningLauncher struct{}

func (spawningLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	return newStubSession(spec.Name), nil
}

// TestWolfRunsInTandemWithMainSession verifies the wolf launches even while the
// project's main session slot is occupied — the exception summoning relies on.
// It also confirms the main session is left untouched and a second wolf launch
// dedups.
func TestWolfRunsInTandemWithMainSession(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.runner = &runner.Runner{
		Launcher: spawningLauncher{},
		Command:  func(agent.Kind, string) []string { return []string{"true"} },
	}

	root := t.TempDir()
	projectDir := filepath.Join(root, "myproj")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(projectDir, ".project.yaml")

	// Occupy the project's main slot with a running task session, exactly the
	// situation `:wolf` must not be blocked by.
	mainSess := newStubSession("task-myproj")
	if !d.trackSession(projectPath, mainSess, agent.TaskAgent, "scaffold", nil) {
		t.Fatal("could not track main session")
	}
	defer mainSess.Close()

	p := &project.Project{Branch: "feat/x", Status: project.StatusBlocked}
	d.launchWolfAgent(projectPath, p, "summoned")

	wolfKey := wolfSessionKey(projectPath)
	if !d.hasSession(wolfKey) {
		t.Fatalf("wolf did not launch while main slot was busy; hasSession(%q) = false", wolfKey)
	}
	if !d.hasSession(projectPath) {
		t.Errorf("main session slot should be untouched by the wolf launch")
	}

	// Both sessions surface in the pane, and both resolve to the same project
	// name despite the wolf's distinct key.
	infos := d.ListSessions()
	if len(infos) != 2 {
		t.Fatalf("ListSessions len = %d, want 2 (main + wolf)", len(infos))
	}
	for _, info := range infos {
		if info.Project != "myproj" {
			t.Errorf("session %q Project = %q, want myproj", info.ID, info.Project)
		}
	}

	// A second summon while the wolf is already running is a dedup no-op.
	d.launchWolfAgent(projectPath, p, "summoned again")
	if got := len(d.ListSessions()); got != 2 {
		t.Errorf("second wolf launch should dedup; ListSessions len = %d, want 2", got)
	}
}
