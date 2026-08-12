package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/setup"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
	"gopkg.in/yaml.v3"
)

// recordingLauncher is spawningLauncher with a memory: it keeps every Spec it
// was handed and every session it created, so a test can assert what was
// launched and then end a specific session on demand. Sessions stay "running"
// until the test closes them.
type recordingLauncher struct {
	mu       sync.Mutex
	specs    []agent.Spec
	sessions []*stubSession
}

func (l *recordingLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	s := newStubSession(spec.Name)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.specs = append(l.specs, spec)
	l.sessions = append(l.sessions, s)
	return s, nil
}

func (l *recordingLauncher) launched() []agent.Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]agent.Spec(nil), l.specs...)
}

// countKind returns how many launches so far were of kind k.
func (l *recordingLauncher) countKind(k agent.Kind) int {
	n := 0
	for _, s := range l.launched() {
		if s.Kind == k {
			n++
		}
	}
	return n
}

// closeKind ends the first still-open session launched for kind k, standing in
// for the agent process exiting. Fails the test if there is no such session.
func (l *recordingLauncher) closeKind(t *testing.T, k agent.Kind) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, spec := range l.specs {
		if spec.Kind == k {
			l.sessions[i].Close()
			return
		}
	}
	t.Fatalf("no %s session was launched", k)
}

func (l *recordingLauncher) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.sessions {
		s.Close()
	}
}

// archiveTestDaemon builds a daemon whose runner really goes through
// runner.Start — stub wsp workspaces, recording launcher, no sandbox — so the
// Plan the daemon builds is observable in what lands on disk (the workspace the
// runner provisions and the .orch/context.yaml written into it).
func archiveTestDaemon(t *testing.T) (*Daemon, *recordingLauncher, *safeBuf, string) {
	t.Helper()
	buf := &safeBuf{}
	a := audit.New(buf)
	stubRoot := t.TempDir()
	lch := &recordingLauncher{}
	d, err := New([]string{t.TempDir()}, a, WithRunner(&runner.Runner{
		Workspaces: workspace.NewStub(stubRoot),
		Launcher:   lch,
		Audit:      a,
		Command:    func(agent.Kind, string) []string { return []string{"true"} },
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	t.Cleanup(func() {
		cancel()
		lch.closeAll()
		_ = d.watcher.Close()
	})
	return d, lch, buf, stubRoot
}

// seedCleanupProject writes a project that has a cleanup requested and is
// otherwise mid-work (status:working with a ready task), plus that task. The
// ready task is the control for "the cleanup request outranks status routing":
// if the daemon fell through to the status switch it would dispatch a task
// agent, which the assertions catch.
func seedCleanupProject(t *testing.T, dir string, p *project.Project) string {
	t.Helper()
	if err := mkdirAll(filepath.Join(dir, "tasks")); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	if err := task.Save(filepath.Join(dir, "tasks", "first.yaml"),
		&task.Task{Name: "first", Status: task.StatusReady}); err != nil {
		t.Fatalf("save task: %v", err)
	}
	projectPath := filepath.Join(dir, ".project.yaml")
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	return projectPath
}

func cleanupTestProject() *project.Project {
	stamp := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return &project.Project{
		Description: "cleanup me",
		Branch:      "feat/cleanup",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "slimslenderslacks", Name: "workingman"}},
		NewRepos:    []project.Repo{{Org: "slimslenderslacks", Name: "brandnew"}},
		Cleanup:     true,
		CreatedAt:   &stamp,
	}
}

// TestCleanupFlagDispatchesArchiveAgent covers the dispatch side: an observed
// `cleanup: true` launches the archive agent with the right Plan, ahead of the
// status routing, in tandem with the project's main slot — and a second request
// while it runs is a no-op.
func TestCleanupFlagDispatchesArchiveAgent(t *testing.T) {
	d, lch, buf, stubRoot := archiveTestDaemon(t)
	dir := t.TempDir()
	p := cleanupTestProject()
	projectPath := seedCleanupProject(t, dir, p)

	d.dispatchProject(projectPath, p)

	if !d.hasSession(archiveSessionKey(projectPath)) {
		t.Fatalf("archive session not tracked under %q.\naudit:\n%s",
			archiveSessionKey(projectPath), buf.String())
	}
	// The archive key is separate from the project's main slot, so a cleanup
	// can run alongside whatever else the project is doing.
	if d.hasSession(projectPath) {
		t.Errorf("archive agent occupied the project's main session slot")
	}
	// The flag is checked before the status switch: status:working with a ready
	// task must NOT also dispatch a task agent.
	if got := lch.countKind(agent.TaskAgent); got != 0 {
		t.Errorf("task agent launched %d times during a cleanup request, want 0", got)
	}
	specs := lch.launched()
	if len(specs) != 1 || specs[0].Kind != agent.ArchiveAgent {
		t.Fatalf("launched specs = %+v, want exactly one archive launch", specs)
	}
	if !strings.Contains(buf.String(), "archive_dispatch") {
		t.Errorf("no archive_dispatch audit entry:\n%s", buf.String())
	}

	// The Plan carried no WorkingDir, so the runner had to resolve the wsp
	// workspace from Branch + Repos — that is where the session runs.
	wantWorkspace := filepath.Join(stubRoot, p.Branch)
	if specs[0].Workspace != wantWorkspace {
		t.Errorf("archive workspace = %q, want the wsp workspace %q", specs[0].Workspace, wantWorkspace)
	}

	// The rest of the Plan is observable in the context file the runner wrote
	// into that workspace.
	var ctxFile setup.Context
	raw, err := os.ReadFile(filepath.Join(wantWorkspace, ".orch", "context.yaml"))
	if err != nil {
		t.Fatalf("read context.yaml: %v", err)
	}
	if err := yaml.Unmarshal(raw, &ctxFile); err != nil {
		t.Fatalf("parse context.yaml: %v", err)
	}
	if ctxFile.Kind != "archive" {
		t.Errorf("context kind = %q, want archive", ctxFile.Kind)
	}
	if ctxFile.Branch != p.Branch {
		t.Errorf("context branch = %q, want %q", ctxFile.Branch, p.Branch)
	}
	if ctxFile.ProjectPath != projectPath {
		t.Errorf("context project_path = %q, want %q", ctxFile.ProjectPath, projectPath)
	}
	if want := filepath.Join(dir, "tasks"); ctxFile.TasksDir != want {
		t.Errorf("context tasks_dir = %q, want %q", ctxFile.TasksDir, want)
	}

	// Repos: both the existing repo and the project's new_repos, as
	// workspaceReposFor builds them.
	repos, err := os.ReadFile(filepath.Join(wantWorkspace, ".orch", "stub-repos.txt"))
	if err != nil {
		t.Fatalf("read stub-repos.txt: %v", err)
	}
	for _, want := range []string{
		"github.com/slimslenderslacks/workingman",
		"github.com/slimslenderslacks/brandnew",
	} {
		if !strings.Contains(string(repos), want) {
			t.Errorf("workspace repos %q missing %q", string(repos), want)
		}
	}

	// A second `:cleanup` while the first is still running is a dedup no-op.
	d.dispatchProject(projectPath, p)
	if got := lch.countKind(agent.ArchiveAgent); got != 1 {
		t.Errorf("archive launches after a duplicate request = %d, want 1", got)
	}
	if !strings.Contains(buf.String(), "session_skip_duplicate") {
		t.Errorf("duplicate request should log session_skip_duplicate:\n%s", buf.String())
	}
}

// TestArchiveSessionEndClearsFlagAndResumesRouting covers the session-end side:
// the request flag is cleared exactly once, as the daemon, the agent's
// `archive: true` is left alone, and normal status routing picks up again.
func TestArchiveSessionEndClearsFlagAndResumesRouting(t *testing.T) {
	d, lch, buf, _ := archiveTestDaemon(t)
	dir := t.TempDir()
	p := cleanupTestProject()
	projectPath := seedCleanupProject(t, dir, p)

	d.dispatchProject(projectPath, p)
	if got := lch.countKind(agent.ArchiveAgent); got != 1 {
		t.Fatalf("archive launches = %d, want 1.\naudit:\n%s", got, buf.String())
	}

	// The agent's own writeback: success (archive: true), leaving the request
	// flag for the daemon to clear.
	done := *p
	done.Archive = true
	if err := project.SaveAs(projectPath, &done, project.WriterAgent); err != nil {
		t.Fatalf("agent SaveAs: %v", err)
	}

	lch.closeKind(t, agent.ArchiveAgent)

	if ok, snap := waitFor(t, buf, "cleanup_flag_cleared"); !ok {
		t.Fatalf("cleanup flag never cleared.\naudit:\n%s", snap)
	}
	reloaded, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Cleanup {
		t.Errorf("cleanup flag still set after the archive session ended")
	}
	if !reloaded.Archive {
		t.Errorf("daemon clobbered the agent's archive: true")
	}
	if reloaded.UpdatedBy != project.WriterDaemon {
		t.Errorf("clear written by %q, want daemon (so it can't retrigger dispatch)", reloaded.UpdatedBy)
	}
	if reloaded.Status != project.StatusWorking {
		t.Errorf("status = %q, want working — the cleanup must not change it", reloaded.Status)
	}
	// A successful cleanup is not an "incomplete" one.
	if strings.Contains(buf.String(), "archive_incomplete") {
		t.Errorf("archive_incomplete logged for a successful run:\n%s", buf.String())
	}

	// Routing resumed: with the request cleared, the revisit fell through to
	// status:working and dispatched the ready task.
	if ok, snap := waitForWithin(t, buf, "kind=task", 2*time.Second); !ok {
		t.Fatalf("normal routing did not resume after the cleanup.\naudit:\n%s", snap)
	}
	if got := lch.countKind(agent.TaskAgent); got != 1 {
		t.Errorf("task launches after the cleanup = %d, want 1", got)
	}
	// And the revisit did not loop back into another archive agent.
	if got := lch.countKind(agent.ArchiveAgent); got != 1 {
		t.Errorf("archive launches = %d, want exactly 1", got)
	}
	if got := strings.Count(buf.String(), "cleanup_flag_cleared"); got != 1 {
		t.Errorf("cleanup_flag_cleared logged %d times, want 1", got)
	}
}

// TestArchiveSessionEndWithoutArchiveFlagDoesNotBlock covers the agent giving
// up: the daemon records why, still clears the request, and leaves the project
// alone.
func TestArchiveSessionEndWithoutArchiveFlagDoesNotBlock(t *testing.T) {
	d, lch, buf, _ := archiveTestDaemon(t)
	dir := t.TempDir()
	p := cleanupTestProject()
	// status:blocked also proves the request is honoured from any status.
	p.Status = project.StatusBlocked
	p.BlockedReason = "waiting on a human"
	projectPath := seedCleanupProject(t, dir, p)

	d.dispatchProject(projectPath, p)
	if got := lch.countKind(agent.ArchiveAgent); got != 1 {
		t.Fatalf("archive launches = %d, want 1 (blocked project).\naudit:\n%s", got, buf.String())
	}
	// The wolf belongs to the blocked status, which the cleanup request
	// outranks; it should only appear after the request is cleared.
	if got := lch.countKind(agent.WolfAgent); got != 0 {
		t.Fatalf("wolf launched %d times while a cleanup was requested, want 0", got)
	}

	// Session ends with archive still unset — the agent could not finish.
	lch.closeKind(t, agent.ArchiveAgent)

	if ok, snap := waitFor(t, buf, "archive_incomplete"); !ok {
		t.Fatalf("no archive_incomplete entry for an unfinished run.\naudit:\n%s", snap)
	}
	if ok, snap := waitFor(t, buf, "cleanup_flag_cleared"); !ok {
		t.Fatalf("request flag not cleared after an unfinished run.\naudit:\n%s", snap)
	}
	reloaded, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Cleanup {
		t.Errorf("cleanup flag still set after an unfinished run — the daemon would relaunch in a loop")
	}
	if reloaded.Archive {
		t.Errorf("daemon set archive: true for a run that never reported success")
	}
	// Not finishing is not a reason to block: the project keeps the status and
	// reason it already had.
	if reloaded.Status != project.StatusBlocked || reloaded.BlockedReason != "waiting on a human" {
		t.Errorf("project state changed by the unfinished cleanup: %+v", reloaded)
	}
	if got := lch.countKind(agent.ArchiveAgent); got != 1 {
		t.Errorf("archive launches = %d, want exactly 1 (no relaunch)", got)
	}
}

// TestSingleCleanupProducesOneArchiveSession is the end-to-end loop guard, run
// against a live daemon with fsnotify wired up. Between the request landing and
// the daemon clearing it, several .project.yaml writes fly past — the agent's
// own `archive: true` writeback and the daemon's clear — and none of them may
// produce a second archive session.
func TestSingleCleanupProducesOneArchiveSession(t *testing.T) {
	root := t.TempDir()
	stubRoot := t.TempDir()
	buf := &safeBuf{}
	a := audit.New(buf)
	lch := &recordingLauncher{}
	d, err := New([]string{root}, a, WithRunner(&runner.Runner{
		Workspaces: workspace.NewStub(stubRoot),
		Launcher:   lch,
		Audit:      a,
		Command:    func(agent.Kind, string) []string { return []string{"true"} },
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() { cancel(); lch.closeAll(); <-done })

	if ok, snap := waitFor(t, buf, "watch_root"); !ok {
		t.Fatalf("daemon never ready: %s", snap)
	}

	// `:cleanup` writes the request as the agent so the daemon sees the event.
	p := cleanupTestProject()
	projectPath := seedCleanupProject(t, root, p)

	if ok, snap := waitFor(t, buf, "archive_dispatch"); !ok {
		t.Fatalf("cleanup request never dispatched an archive agent.\naudit:\n%s", snap)
	}

	// The agent reports success by rewriting the file — an agent-written event
	// the daemon does NOT filter, still carrying the request flag.
	succeeded := *p
	succeeded.Archive = true
	if err := project.SaveAs(projectPath, &succeeded, project.WriterAgent); err != nil {
		t.Fatalf("agent SaveAs: %v", err)
	}
	// Give that event time to be handled before the session ends.
	time.Sleep(200 * time.Millisecond)
	if got := lch.countKind(agent.ArchiveAgent); got != 1 {
		t.Fatalf("archive launches after the agent's writeback = %d, want 1.\naudit:\n%s", got, buf.String())
	}

	lch.closeKind(t, agent.ArchiveAgent)

	if ok, snap := waitFor(t, buf, "cleanup_flag_cleared"); !ok {
		t.Fatalf("request flag never cleared.\naudit:\n%s", snap)
	}
	// Settle: the daemon's own clear generates one more fsnotify event, and the
	// revisit that follows it re-dispatches the project. Neither may start a
	// second archive agent.
	time.Sleep(300 * time.Millisecond)

	if got := lch.countKind(agent.ArchiveAgent); got != 1 {
		t.Errorf("archive sessions for a single :cleanup = %d, want 1.\naudit:\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), "archive_dispatch"); got != 1 {
		t.Errorf("archive_dispatch entries = %d, want 1.\naudit:\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), "cleanup_flag_cleared"); got != 1 {
		t.Errorf("cleanup_flag_cleared entries = %d, want 1.\naudit:\n%s", got, buf.String())
	}
	reloaded, err := project.Load(projectPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Cleanup {
		t.Errorf("cleanup flag still set on disk")
	}
	if !reloaded.Archive {
		t.Errorf("archive flag lost: %+v", reloaded)
	}
}
