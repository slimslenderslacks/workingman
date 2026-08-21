package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// gatedManager is a workspace.Manager whose Create call blocks on a
// per-branch gate until the test closes it (or the context is cancelled).
// Branches with no registered gate return immediately. It stands in for the
// "real wsp new that can block for real wall-clock seconds" scenario the
// cross-project-sandbox-parallelism task describes.
type gatedManager struct {
	mu    sync.Mutex
	gates map[string]chan struct{}
	calls []string
}

func newGatedManager() *gatedManager {
	return &gatedManager{gates: map[string]chan struct{}{}}
}

// gate registers a closeable gate for branch and returns the func that opens
// it. Must be called before the project that uses branch is dispatched.
func (m *gatedManager) gate(branch string) func() {
	ch := make(chan struct{})
	m.mu.Lock()
	m.gates[branch] = ch
	m.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func (m *gatedManager) callCount(branch string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range m.calls {
		if b == branch {
			n++
		}
	}
	return n
}

func (m *gatedManager) Create(ctx context.Context, branch string, _ []workspace.Repo) (string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, branch)
	gate := m.gates[branch]
	m.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "/tmp/ws/" + branch, nil
}

func (m *gatedManager) Path(branch string) (string, error) { return "/tmp/ws/" + branch, nil }

func (m *gatedManager) Remove(context.Context, string) error { return nil }

// seedWorkingProject writes a project at status:working with a single ready
// task, so the daemon's very next observation routes straight to
// dispatchNextTask → launchTaskAgent → runner.Start, the path that provisions
// a workspace synchronously inside Start().
func seedWorkingProject(t *testing.T, dir, branch, taskName string) string {
	t.Helper()
	if err := mkdirAll(filepath.Join(dir, "tasks")); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	if err := task.Save(filepath.Join(dir, "tasks", taskName+".yaml"),
		&task.Task{Name: taskName, Status: task.StatusReady}); err != nil {
		t.Fatalf("save task: %v", err)
	}
	projectPath := filepath.Join(dir, ".project.yaml")
	p := &project.Project{Branch: branch, Status: project.StatusWorking}
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	return projectPath
}

// concurrencyTestDaemon builds a daemon wired the same way archiveTestDaemon
// does (real runner.Start path, no sandbox/tmux) but with a caller-supplied
// workspace.Manager so tests can control provisioning delay.
func concurrencyTestDaemon(t *testing.T, mgr workspace.Manager) (*Daemon, *safeBuf) {
	t.Helper()
	buf := &safeBuf{}
	a := audit.New(buf)
	lch := &recordingLauncher{}
	d, err := New([]string{t.TempDir()}, a, WithRunner(&runner.Runner{
		Workspaces: mgr,
		Launcher:   lch,
		Audit:      a,
		Command:    func(agent.Kind, string) []string { return []string{"true"} },
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(lch.closeAll)
	return d, buf
}

func runDaemon(t *testing.T, d *Daemon, buf *safeBuf) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	if ok, snap := waitFor(t, buf, "watch_root"); !ok {
		t.Fatalf("daemon never logged watch_root: %q", snap)
	}
}

// TestSlowWorkspaceProvisioningDoesNotDelayOtherProject is the daemon-level
// regression test for the cross-project-sandbox-parallelism task: one
// project's workspace provisioning (inside runner.Runner.Start, called
// synchronously from d.handle) blocks indefinitely, and a second, unrelated
// project's task dispatch must still proceed — proving Run()'s event loop no
// longer serializes dispatch across projects.
func TestSlowWorkspaceProvisioningDoesNotDelayOtherProject(t *testing.T) {
	mgr := newGatedManager()
	open := mgr.gate("slow-branch") // never opened until the assertions below

	d, buf := concurrencyTestDaemon(t, mgr)
	runDaemon(t, d, buf)

	root := d.roots[0]
	slowDir := filepath.Join(root, "alpha")
	fastDir := filepath.Join(root, "bravo")

	// Each directory's watch must be confirmed installed before writing files
	// into it — otherwise the Create event for a file inside it can be missed
	// (see TestNewDirectoryIsPickedUp's comment on the same hazard).
	if err := mkdirAll(slowDir); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if ok, snap := waitFor(t, buf, "path="+slowDir); !ok {
		t.Fatalf("daemon never watched %s.\naudit:\n%s", slowDir, snap)
	}
	if err := mkdirAll(fastDir); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if ok, snap := waitFor(t, buf, "path="+fastDir); !ok {
		t.Fatalf("daemon never watched %s.\naudit:\n%s", fastDir, snap)
	}

	seedWorkingProject(t, slowDir, "slow-branch", "slow-task")
	seedWorkingProject(t, fastDir, "fast-branch", "fast-task")

	// The fast project's session must start well before the slow project's
	// gate is ever opened — if the daemon were still serializing dispatch on
	// a single goroutine, this would block for as long as the slow gate stays
	// closed (here, forever) and the test would time out.
	if ok, snap := waitForWithin(t, buf, "name=task-fast-task", 2*time.Second); !ok {
		t.Fatalf("fast project's task dispatch was blocked by the slow project's provisioning.\naudit:\n%s", snap)
	}
	if got := mgr.callCount("slow-branch"); got != 1 {
		t.Fatalf("slow-branch Create calls = %d, want 1 (it should have started, just not returned)", got)
	}

	// Now unblock the slow project and confirm it too eventually completes.
	open()
	if ok, snap := waitForWithin(t, buf, "name=task-slow-task", 2*time.Second); !ok {
		t.Fatalf("slow project's task dispatch never completed after unblocking.\naudit:\n%s", snap)
	}
}

// TestSameProjectEventsStillProcessInOrder proves the ordering guarantee
// dispatchEvent documents: two rapid-fire events for the SAME project must
// not run concurrently with each other, even though dispatch across
// different projects now does. It drives this through the daemon's
// deduplication seam (hasSession) — if the second event's dispatch could
// start before the first event's in-flight Start() call has registered its
// session, the daemon would launch two planning sessions for one project.
func TestSameProjectEventsStillProcessInOrder(t *testing.T) {
	mgr := newGatedManager()
	open := mgr.gate("dup-branch")

	d, buf := concurrencyTestDaemon(t, mgr)
	runDaemon(t, d, buf)

	root := d.roots[0]
	projectPath := filepath.Join(root, ".project.yaml")
	p := &project.Project{
		Branch: "dup-branch",
		Status: project.StatusReady,
		// A non-empty repo list is what makes runner.Start's
		// resolvePlanningWorktree call into the (gated) workspace manager for
		// a planning launch.
		Repos: []project.Repo{{Org: "acme", Name: "widgets"}},
	}
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	// Fire a second, identical fsnotify event for the same file immediately
	// behind the first, simulating a rapid re-save. handle() itself only
	// reacts to Write/Create ops.
	d.dispatchEvent(fsnotify.Event{Name: projectPath, Op: fsnotify.Write})

	// Give both events a chance to be picked up while the first is still
	// gated (Start() blocked inside resolvePlanningWorktree).
	time.Sleep(200 * time.Millisecond)
	open()

	if ok, snap := waitForWithin(t, buf, "name=planning-dup-branch", 2*time.Second); !ok {
		t.Fatalf("planning session never started.\naudit:\n%s", snap)
	}
	// Exactly one Start() should have reached the workspace manager: the
	// second event's dispatch, if correctly ordered, finds hasSession(...)
	// already true (or the project already off status:ready) and skips.
	if got := mgr.callCount("dup-branch"); got != 1 {
		t.Fatalf("dup-branch Create calls = %d, want 1 — two rapid events for the same project raced into duplicate dispatch", got)
	}
}

// TestStartupScanFansOutAcrossProjects is the N-projects analogue of
// TestSlowWorkspaceProvisioningDoesNotDelayOtherProject for the startup path:
// three projects already sit on disk when the daemon starts (as after a
// restart), one of which provisions forever until the test opens its gate.
// The other two must still get their first dispatch and start a session
// without waiting for it — proving startupScan fans revisitProject out
// across projects instead of walking them one at a time.
func TestStartupScanFansOutAcrossProjects(t *testing.T) {
	mgr := newGatedManager()
	open := mgr.gate("slow-branch")

	buf := &safeBuf{}
	a := audit.New(buf)
	lch := &recordingLauncher{}
	root := t.TempDir()
	d, err := New([]string{root}, a, WithRunner(&runner.Runner{
		Workspaces: mgr,
		Launcher:   lch,
		Audit:      a,
		Command:    func(agent.Kind, string) []string { return []string{"true"} },
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(lch.closeAll)

	// Seed all three projects on disk BEFORE the daemon starts, so their
	// first dispatch comes from startupScan rather than a live fsnotify event.
	seedWorkingProject(t, filepath.Join(root, "alpha"), "slow-branch", "slow-task")
	seedWorkingProject(t, filepath.Join(root, "bravo"), "fast-branch-1", "fast-task-1")
	seedWorkingProject(t, filepath.Join(root, "charlie"), "fast-branch-2", "fast-task-2")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Both fast projects must start well before the slow one's gate opens —
	// if startupScan still walked projects sequentially on one goroutine, one
	// of these would be stuck behind the still-blocked slow-branch Create.
	if ok, snap := waitForWithin(t, buf, "name=task-fast-task-1", 2*time.Second); !ok {
		t.Fatalf("fast-branch-1 dispatch was blocked by the slow project's provisioning.\naudit:\n%s", snap)
	}
	if ok, snap := waitForWithin(t, buf, "name=task-fast-task-2", 2*time.Second); !ok {
		t.Fatalf("fast-branch-2 dispatch was blocked by the slow project's provisioning.\naudit:\n%s", snap)
	}
	// startup_scan logs only after every fanned-out revisitProject call
	// (including the still-gated slow one) has returned, so it must not have
	// appeared yet.
	if strings.Contains(buf.String(), "startup_scan") {
		t.Fatalf("startup_scan logged before the slow project's provisioning finished — the scan isn't actually waiting on it")
	}

	open()
	if ok, snap := waitForWithin(t, buf, "name=task-slow-task", 2*time.Second); !ok {
		t.Fatalf("slow project's dispatch never completed after unblocking.\naudit:\n%s", snap)
	}
	if ok, snap := waitForWithin(t, buf, "startup_scan", 2*time.Second); !ok {
		t.Fatalf("startup_scan never logged after all projects finished.\naudit:\n%s", snap)
	}
}
