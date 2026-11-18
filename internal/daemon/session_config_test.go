package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
)

// sessionLauncher is an agent.Launcher whose sessions are stubSessions the
// test can end.
type sessionLauncher struct{ last *stubSession }

func (l *sessionLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	l.last = newStubSession(spec.Name)
	return l.last, nil
}

// A session launched through startSession carries the runner's launch config
// — harness, model, mounts, MCP servers and dynamic gateway — into
// ListSessions.
func TestListSessionsSurfacesLaunchConfig(t *testing.T) {
	d, _ := newTestDaemon(t)
	acp := &sessionLauncher{}
	d.runner = &runner.Runner{
		AcpLauncher:       acp,
		Kit:               "kit-ref",
		Image:             "img:2",
		SessionsRoot:      t.TempDir(),
		MCPGatewayURL:     "http://gw",
		MCPGatewayServers: []string{"github", "linear"},
	}
	orchDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(orchDir, "project.yaml")

	if err := d.startSession(projectPath, runner.Plan{
		Kind:        agent.ProjectAgent,
		WorkingDir:  orchDir,
		ProjectPath: projectPath,
		Model:       "claude-sonnet-5-5",
	}, nil); err != nil {
		t.Fatalf("startSession: %v", err)
	}
	defer acp.last.Close()

	got := d.ListSessions()
	if len(got) != 1 {
		t.Fatalf("ListSessions = %+v", got)
	}
	s := got[0]
	if s.Harness != "img:2" || s.Model != "claude-sonnet-5-5" {
		t.Errorf("Harness/Model = %q/%q, want img:2/claude-sonnet-5-5", s.Harness, s.Model)
	}
	if !reflect.DeepEqual(s.Mounts, []string{orchDir}) {
		t.Errorf("Mounts = %v, want [%s]", s.Mounts, orchDir)
	}
	if !reflect.DeepEqual(s.MCPs, []string{"github", "linear"}) || s.MCPDynamic {
		t.Errorf("MCPs = %v, MCPDynamic = %v; want [github linear], static", s.MCPs, s.MCPDynamic)
	}

	// The snapshot is a copy: mutating it must not reach the tracked entry.
	s.Mounts[0] = "mutated"
	if again := d.ListSessions(); again[0].Mounts[0] != orchDir {
		t.Errorf("ListSessions shares Mounts with the session entry: %v", again[0].Mounts)
	}
}

// A session tracked without a launch config (non-sandboxed) reports none.
func TestListSessionsEmptyConfigWhenUnknown(t *testing.T) {
	d, _ := newTestDaemon(t)
	key := filepath.Join(t.TempDir(), "alpha", "project.yaml")
	sess := newStubSession("orch-task-alpha")
	if !d.trackSession(key, sess, agent.TaskAgent, "t", nil) {
		t.Fatal("trackSession returned false")
	}
	defer sess.Close()
	s := d.ListSessions()[0]
	if s.Harness != "" || s.Model != "" || s.Mounts != nil || s.MCPs != nil || s.MCPDynamic {
		t.Errorf("unconfigured session = %+v, want empty config", s)
	}
}

// WatchSessions re-emits when only the launch config changed, so the TUI's
// agent box never shows a stale one.
func TestSessionInfoEqualComparesConfig(t *testing.T) {
	base := SessionInfo{ID: "a", Harness: "h", Model: "m", Mounts: []string{"/x"}, MCPs: []string{"github"}, MCPDynamic: true}
	if !sessionInfoEqual(base, base) {
		t.Fatal("identical infos compare unequal")
	}
	for name, mutate := range map[string]func(*SessionInfo){
		"harness":    func(s *SessionInfo) { s.Harness = "h2" },
		"model":      func(s *SessionInfo) { s.Model = "m2" },
		"mounts":     func(s *SessionInfo) { s.Mounts = []string{"/x", "/y"} },
		"mcps":       func(s *SessionInfo) { s.MCPs = []string{"linear"} },
		"mcpDynamic": func(s *SessionInfo) { s.MCPDynamic = false },
	} {
		other := base
		other.Mounts = append([]string(nil), base.Mounts...)
		other.MCPs = append([]string(nil), base.MCPs...)
		mutate(&other)
		if sessionInfoEqual(base, other) {
			t.Errorf("%s change not detected", name)
		}
	}
}

// A session adopted on restart gets its config back from what is on disk:
// session.json's template and mounts, the task file's model and static_mcps,
// and the runner's gateway rules for the kind.
func TestReconcileSessionsRestoresConfig(t *testing.T) {
	d, _ := newTestDaemon(t)
	sessionsRoot := t.TempDir()
	d.runner = &runner.Runner{
		AcpLauncher:       stubLauncher{},
		Kit:               "kit-ref",
		SessionsRoot:      sessionsRoot,
		MCPGatewayURL:     "http://gw",
		MCPGatewayServers: []string{"github"},
	}
	orchDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(filepath.Join(orchDir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(orchDir, "project.yaml")
	taskPath := filepath.Join(orchDir, "tasks", "build.yaml")
	if err := task.Save(taskPath, &task.Task{Name: "build", Status: task.StatusRunning, Model: "claude-haiku-5-5", StaticMCPs: []string{"linear"}}); err != nil {
		t.Fatalf("task.Save: %v", err)
	}
	store, err := session.NewStore(sessionsRoot)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mounts := []string{"/ws/alpha", orchDir}
	if err := store.Write(session.Session{
		ID:          "task-build",
		SandboxName: "alpha-build",
		Status:      session.StatusRunning,
		CreatedAt:   now,
		UpdatedAt:   now,
		Workspaces:  mounts,
		Template:    "img:3",
		ProjectPath: projectPath,
		TaskPath:    taskPath,
		Kind:        agent.TaskAgent.String(),
	}); err != nil {
		t.Fatal(err)
	}

	d.reconcileSessions()
	got := d.ListSessions()
	if len(got) != 1 {
		t.Fatalf("ListSessions after reconcile = %+v", got)
	}
	s := got[0]
	if s.Harness != "img:3" || s.Model != "claude-haiku-5-5" {
		t.Errorf("Harness/Model = %q/%q, want img:3/claude-haiku-5-5", s.Harness, s.Model)
	}
	if !reflect.DeepEqual(s.Mounts, mounts) {
		t.Errorf("Mounts = %v, want %v", s.Mounts, mounts)
	}
	if !reflect.DeepEqual(s.MCPs, []string{"linear"}) || !s.MCPDynamic {
		t.Errorf("MCPs = %v, MCPDynamic = %v; want [linear], dynamic", s.MCPs, s.MCPDynamic)
	}
}
