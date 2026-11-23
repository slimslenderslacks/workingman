package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
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
	projectPath := filepath.Join(projectDir, "project.yaml")

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

// TestLaunchWolfAgentUnderACPIsPersistent verifies the wolf, when the runner has
// an AcpLauncher, launches as a persistent ACP session in its own sandbox: the
// wrapper gets --persistent (not the one-shot --exit-when-empty), the unblock
// grace that lets it end when the project leaves `blocked`, and the wolf's own
// sandbox/kind; the session is tracked under the wolf key, is detachable across
// a daemon restart, and its prompt tells it that it is sandboxed.
func TestLaunchWolfAgentUnderACPIsPersistent(t *testing.T) {
	d, _ := newTestDaemon(t)
	acp := &recordingLauncher{}
	d.runner = &runner.Runner{
		Launcher:     spawningLauncher{},
		AcpLauncher:  acp,
		Kit:          "kit",
		SessionsRoot: t.TempDir(),
	}

	root := t.TempDir()
	projectPath := filepath.Join(root, "myproj", "project.yaml")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &project.Project{Branch: "feat/x", Status: project.StatusBlocked}
	d.launchWolfAgent(projectPath, p, "task failed")

	if !d.hasSession(wolfSessionKey(projectPath)) {
		t.Fatal("wolf did not launch")
	}
	cmd := strings.Join(acp.launched()[0].Command, " ")
	for _, want := range []string{"--persistent", "--unblock-grace", "--idle-timeout", "--kind wolf", "--sandbox myproj-wolf", "--project-path " + projectPath} {
		if !strings.Contains(cmd, want) {
			t.Errorf("wrapper command missing %q: %s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--exit-when-empty") {
		t.Errorf("a conversational wolf must not be one-shot: %s", cmd)
	}
	if !d.detachable(agent.WolfAgent) {
		t.Error("ACP wolf should survive a daemon restart (detachable)")
	}
	infos := d.ListSessions()
	if len(infos) != 1 || infos[0].SandboxName != "myproj-wolf" {
		t.Errorf("ListSessions = %+v, want one session with sandbox myproj-wolf", infos)
	}

	instructions, err := os.ReadFile(filepath.Join(filepath.Dir(projectPath), ".orch", "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	if !strings.Contains(string(instructions), "you are sandboxed") {
		t.Errorf("ACP wolf instructions should say it is sandboxed:\n%s", instructions)
	}
}

// TestLaunchWolfAgentOnHostUsesTerminalTab verifies the --wolf-host escape hatch:
// even with an AcpLauncher configured, WolfOnHost keeps the wolf on the host
// terminal-tab path (no ACP wrapper, no sandbox, host-flavored prompt).
func TestLaunchWolfAgentOnHostUsesTerminalTab(t *testing.T) {
	d, _ := newTestDaemon(t)
	acp := &recordingLauncher{}
	tabs := &recordingLauncher{}
	d.runner = &runner.Runner{
		Launcher:     tabs,
		AcpLauncher:  acp,
		WolfOnHost:   true,
		Kit:          "kit",
		SessionsRoot: t.TempDir(),
		Command:      func(agent.Kind, string) []string { return []string{"claude", "hi"} },
	}

	projectPath := filepath.Join(t.TempDir(), "myproj", "project.yaml")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	d.launchWolfAgent(projectPath, &project.Project{Branch: "feat/x", Status: project.StatusBlocked}, "task failed")

	if got := acp.launched(); len(got) != 0 {
		t.Errorf("WolfOnHost must not use the ACP launcher, got %+v", got)
	}
	if got := tabs.launched(); len(got) != 1 || got[0].Command[0] != "claude" {
		t.Fatalf("wolf should launch once on the terminal-tab path, got %+v", got)
	}
	instructions, err := os.ReadFile(filepath.Join(filepath.Dir(projectPath), ".orch", "instructions.md"))
	if err != nil {
		t.Fatalf("read instructions.md: %v", err)
	}
	if strings.Contains(string(instructions), "you are sandboxed") || !strings.Contains(string(instructions), "macOS notification") {
		t.Errorf("host wolf instructions should keep the host guidance:\n%s", instructions)
	}
}

// TestWolfRunsInTandemWithParallelSlots is the tandem test with the slots
// split (X3): the wolf launches beside two task agents and an integration,
// leaves all three running, and a blocked project starts no new task or
// commit agent beside it.
func TestWolfRunsInTandemWithParallelSlots(t *testing.T) {
	e := newSchedEnv(t, 3)
	for _, n := range []string{"a", "b", "c"} {
		e.addTask(n, []string{"gateway/" + n + "/**"})
	}
	e.pass()
	e.finishTask("a", task.StatusSuccess)
	e.eventually("a integrating", func() bool { return e.integrating() == "a" })
	e.addTask("d", []string{"gateway/d/**"})

	p, _ := project.Load(e.projectPath)
	e.d.transitionProjectBlocked(e.projectPath, p, "summoned")
	if !e.d.hasSession(wolfSessionKey(e.projectPath)) {
		t.Fatal("wolf did not launch beside the task and commit slots")
	}
	for _, key := range []string{taskSessionKey(e.projectPath, "b"), taskSessionKey(e.projectPath, "c"), commitSessionKey(e.projectPath)} {
		if !e.d.hasSession(key) {
			t.Errorf("%s was disturbed by the wolf", key)
		}
	}
	e.pass()
	if tk := e.task("d"); tk.Status != task.StatusReady {
		t.Errorf("d = %s, want ready: a blocked project admits nothing", tk.Status)
	}
	// The integration already running finishes; nothing new starts after it.
	e.finishCommit("a")
	e.eventually("a committed", func() bool { return !e.d.commitReserved(e.projectPath) })
	e.finishTask("b", task.StatusSuccess)
	e.eventually("b's session ended", func() bool { return !e.d.hasSession(taskSessionKey(e.projectPath, "b")) })
	time.Sleep(100 * time.Millisecond)
	if n := e.lch.count(agent.CommitAgent); n != 1 {
		t.Errorf("commit agents started = %d, want 1 (the queue is frozen while blocked)", n)
	}
	if n := e.lch.count(agent.TaskAgent); n != 3 {
		t.Errorf("task agents started = %d, want 3", n)
	}
}
