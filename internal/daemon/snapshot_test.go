package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/scheduler"
	"github.com/slimslenderslacks/work/internal/session"
)

const secretToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

// writeFixtureRoot lays out an orch root with two work streams:
//
//	alpha: working, branch alpha-branch, a failed task (with a secret in its
//	       failure_reason) and a dependent ready task, one open PR
//	bravo: blocked with a reason, no tasks
//	broken: unparseable .project.yaml
func writeFixtureRoot(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("alpha/.project.yaml", `description: build the thing
repos:
  - org: acme
    name: widgets
branch: alpha-branch
status: working
review: true
cron: "@every 1h"
cron_max_runs: 5
pull_requests:
  - repo: acme/widgets
    number: 7
    url: https://github.com/acme/widgets/pull/7
    head_sha: deadbeef
    state: open
updated_by: agent
`)
	mk("alpha/tasks/build.yaml", `name: build
description: build it
depends_on: []
status: failed
attempts: 3
failure_reason: "clone failed token=`+secretToken+`"
blocked_reason: ""
`)
	mk("alpha/tasks/ship.yaml", `name: ship
description: ship it
depends_on: [build]
status: ready
attempts: 0
`)
	mk("alpha/tasks/notes.txt", "ignored")
	mk("bravo/.project.yaml", `description: stuck
branch: bravo-branch
status: blocked
blocked_reason: "needs human: password=hunter2"
updated_by: daemon
`)
	mk("broken/.project.yaml", "status: [not, a, string\n")
	return root
}

func findProject(t *testing.T, s *Snapshot, ws string) ProjectSnapshot {
	t.Helper()
	for _, p := range s.Projects {
		if p.WorkStream == ws {
			return p
		}
	}
	t.Fatalf("project %q not in snapshot (have %d projects)", ws, len(s.Projects))
	return ProjectSnapshot{}
}

func TestOfflineSnapshotContent(t *testing.T) {
	root := writeFixtureRoot(t)
	auditLog := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(auditLog, []byte(
		"2026-10-02T10:00:00Z watch_root path=/x\n"+
			"2026-10-02T10:00:01Z task_failed name=build err=\"token="+secretToken+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := BuildOfflineSnapshot(OfflineOptions{Roots: []string{root}, AuditLog: auditLog, SessionsRoot: t.TempDir(), AuditEvents: 10})

	if s.Version != SnapshotVersion || s.Source != SnapshotSourceOffline || s.LiveStateAvailable {
		t.Fatalf("header = version %d source %q live %v", s.Version, s.Source, s.LiveStateAvailable)
	}
	if s.GeneratedAt.IsZero() || time.Since(s.GeneratedAt) > time.Minute {
		t.Errorf("generated_at = %v", s.GeneratedAt)
	}
	if len(s.Notes) == 0 || !strings.Contains(s.Notes[0], "unavailable") {
		t.Errorf("offline snapshot must annotate that live state is unavailable: %v", s.Notes)
	}
	if s.Daemon != nil {
		t.Errorf("offline snapshot has daemon info: %+v", s.Daemon)
	}
	if len(s.Projects) != 3 {
		t.Fatalf("projects = %d, want 3", len(s.Projects))
	}

	a := findProject(t, s, "alpha")
	if a.Path != filepath.Join(root, "alpha", ".project.yaml") || a.Status != "working" || a.Branch != "alpha-branch" {
		t.Errorf("alpha = %+v", a)
	}
	if !a.Review || !a.CronActive || a.Cron != "@every 1h" || a.WatchingPR {
		t.Errorf("alpha flags: review=%v cron_active=%v cron=%q watching=%v (watching only applies to idle projects)", a.Review, a.CronActive, a.Cron, a.WatchingPR)
	}
	if len(a.PullRequests) != 1 || a.PullRequests[0].Number != 7 || a.PullRequests[0].State != "open" || a.PullRequests[0].Repo != "acme/widgets" {
		t.Errorf("alpha PRs = %+v", a.PullRequests)
	}
	if a.TaskTotal != 2 || a.TaskCounts["failed"] != 1 || a.TaskCounts["ready"] != 1 || a.TaskCounts["committed"] != 0 {
		t.Errorf("alpha counts = %v total=%d", a.TaskCounts, a.TaskTotal)
	}
	for _, st := range []string{"ready", "running", "success", "failed", "blocked", "committed"} {
		if _, ok := a.TaskCounts[st]; !ok {
			t.Errorf("task_counts missing key %q", st)
		}
	}
	if len(a.Tasks) != 2 || a.Tasks[0].Name != "build" || a.Tasks[1].Name != "ship" {
		t.Fatalf("alpha tasks = %+v", a.Tasks)
	}
	if a.Tasks[0].Attempts != 3 || a.Tasks[0].Status != "failed" || !strings.Contains(a.Tasks[0].FailureReason, "clone failed") {
		t.Errorf("build task = %+v", a.Tasks[0])
	}
	if len(a.Tasks[1].DependsOn) != 1 || a.Tasks[1].DependsOn[0] != "build" {
		t.Errorf("ship deps = %v", a.Tasks[1].DependsOn)
	}
	if a.Live != nil {
		t.Error("offline project has live block")
	}

	b := findProject(t, s, "bravo")
	if b.Status != "blocked" || !strings.Contains(b.BlockedReason, "needs human") || b.TaskTotal != 0 || len(b.Tasks) != 0 {
		t.Errorf("bravo = %+v", b)
	}

	br := findProject(t, s, "broken")
	if br.LoadError == "" {
		t.Errorf("broken project should report load_error: %+v", br)
	}

	if len(s.AuditEvents) != 2 || s.AuditEvents[1].Event != "task_failed" || s.AuditEvents[0].Fields["path"] != "/x" {
		t.Errorf("audit events = %+v", s.AuditEvents)
	}
}

func TestSnapshotRedactsSecrets(t *testing.T) {
	root := writeFixtureRoot(t)
	auditLog := filepath.Join(t.TempDir(), "audit.log")
	_ = os.WriteFile(auditLog, []byte("2026-10-02T10:00:01Z task_failed name=build err=\"token="+secretToken+"\"\n"), 0o644)

	s := BuildOfflineSnapshot(OfflineOptions{Roots: []string{root}, AuditLog: auditLog, SessionsRoot: t.TempDir()})
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{secretToken, "hunter2"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("snapshot JSON leaks %q:\n%s", secret, data)
		}
	}
	if !strings.Contains(string(data), audit.Redacted) {
		t.Error("expected [REDACTED] markers in snapshot")
	}
}

func TestLiveSnapshotCarriesInMemoryState(t *testing.T) {
	root := writeFixtureRoot(t)
	auditBuf := &safeBuf{}
	sched := scheduler.New()
	d, err := New([]string{root}, audit.New(auditBuf),
		WithScheduler(sched),
		WithRuntimeInfo(RuntimeInfo{WorkspaceManager: "stub", AcpKit: "kit-ref", Headless: true, AuditLog: "/x/audit.log"}),
		WithStateFile(filepath.Join(t.TempDir(), "state", "snapshot.json"), 5),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.ctx = ctx
	t.Cleanup(func() { _ = d.watcher.Close() })

	alpha := filepath.Join(root, "alpha", ".project.yaml")
	task := newStubSession("orch-task-alpha")
	wolf := newStubSession("orch-wolf-alpha")
	d.trackSession(alpha, task, agent.TaskAgent, "build", nil)
	time.Sleep(2 * time.Millisecond)
	d.trackSession(wolfSessionKey(alpha), wolf, agent.WolfAgent, "", nil)

	d.planningMu.Lock()
	d.planningFailures[alpha] = 2
	d.projectFailures[alpha] = 1
	d.planningMu.Unlock()
	d.cleanupMu.Lock()
	d.cleanupInFlight[alpha] = true
	d.cleanupMu.Unlock()
	d.reviewMu.Lock()
	d.reviewFixCycles[alpha] = 4
	d.reviewErrors[alpha] = 1
	d.reviewBackoff[alpha] = 2
	d.reviewFinger[alpha] = "fp"
	d.reviewMu.Unlock()
	if err := sched.Register(reviewPollKey(alpha), "@every 15m", func() {}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Register(alpha, "@every 1h", func() {}); err != nil {
		t.Fatal(err)
	}
	d.audit.Log("custom_event", "n", "1")

	s := d.Snapshot()
	if s.Source != SnapshotSourceDaemon || !s.LiveStateAvailable {
		t.Fatalf("source=%q live=%v", s.Source, s.LiveStateAvailable)
	}
	di := s.Daemon
	if di == nil || di.State != DaemonStateRunning || di.PID != os.Getpid() || di.StartedAt.IsZero() ||
		di.WorkspaceManager != "stub" || di.AcpKit != "kit-ref" || !di.Headless || di.StateFile != d.StateFile() ||
		len(di.Roots) != 1 || di.Roots[0] != root {
		t.Errorf("daemon info = %+v", di)
	}

	if len(s.Sessions) != 2 {
		t.Fatalf("sessions = %+v", s.Sessions)
	}
	ts, ws := s.Sessions[0], s.Sessions[1]
	if ts.Key != alpha || ts.Kind != "task" || ts.Task != "build" || ts.WorkStream != "alpha" || ts.ProjectPath != alpha ||
		ts.TmuxTarget != "orch-task-alpha" || ts.Interactive || ts.Source != "daemon" || ts.StartedAt.IsZero() {
		t.Errorf("task session = %+v", ts)
	}
	if ws.Key != wolfSessionKey(alpha) || ws.Kind != "wolf" || ws.ProjectPath != alpha || !ws.Interactive {
		t.Errorf("wolf session = %+v", ws)
	}

	live := findProject(t, s, "alpha").Live
	if live == nil {
		t.Fatal("alpha has no live block")
	}
	if !live.WolfInFlight || !live.CleanupInFlight || live.ReviewAgentInFlight ||
		live.PlanningFailures != 2 || live.ProjectFailures != 1 || live.ReviewFixCycles != 4 || live.ReviewErrors != 1 ||
		live.CronSchedule != "@every 1h" {
		t.Errorf("live = %+v", live)
	}
	if strings.Join(live.Sessions, ",") != "task,wolf" {
		t.Errorf("live sessions = %v", live.Sessions)
	}
	if rp := live.ReviewPoll; rp == nil || rp.Schedule != "@every 15m" || rp.BackoffIndex != 2 || !rp.HasBaseline {
		t.Errorf("review poll = %+v", rp)
	}
	if bl := findProject(t, s, "bravo").Live; bl == nil || bl.WolfInFlight || len(bl.Sessions) != 0 || bl.ReviewPoll != nil {
		t.Errorf("bravo live = %+v", bl)
	}

	if n := len(s.AuditEvents); n == 0 || n > 5 {
		t.Errorf("audit events = %d, want 1..5 (configured last-N)", n)
	}
	if last := s.AuditEvents[len(s.AuditEvents)-1]; last.Event != "custom_event" || last.Fields["n"] != "1" {
		t.Errorf("last audit event = %+v", last)
	}
}

func TestLiveSnapshotACPSessionLocation(t *testing.T) {
	root := writeFixtureRoot(t)
	sessRoot := t.TempDir()
	r := &runner.Runner{AcpLauncher: stubLauncher{}, SessionsRoot: sessRoot}
	d, err := New([]string{root}, audit.New(&safeBuf{}), WithRunner(r))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.ctx = ctx
	t.Cleanup(func() { _ = d.watcher.Close() })

	alpha := filepath.Join(root, "alpha", ".project.yaml")
	store := session.Store{Root: sessRoot}
	if err := store.Write(session.Session{
		ID: "task-alpha-build-1", SandboxName: "alpha-build", Status: session.StatusRunning,
		CreatedAt: time.Now(), ProjectPath: alpha, Kind: "task",
	}); err != nil {
		t.Fatal(err)
	}
	d.trackSession(alpha, newStubSession("task-alpha-build-1"), agent.TaskAgent, "build", nil)

	s := d.Snapshot()
	if len(s.Sessions) != 1 {
		t.Fatalf("sessions = %+v", s.Sessions)
	}
	got := s.Sessions[0]
	if got.ACP == nil {
		t.Fatalf("ACP session has no acp block: %+v", got)
	}
	if got.ACP.SessionID != "task-alpha-build-1" || got.ACP.SessionDir != store.Dir("task-alpha-build-1") ||
		got.ACP.SocketPath != store.SocketPath("task-alpha-build-1") || got.ACP.Status != "running" {
		t.Errorf("acp = %+v", got.ACP)
	}
	if got.SandboxName != "alpha-build" || got.TmuxTarget != "" {
		t.Errorf("sandbox=%q tmux=%q", got.SandboxName, got.TmuxTarget)
	}
	if !strings.HasPrefix(got.ACP.SessionDir, sessRoot) {
		t.Errorf("session dir %q not under sessions root %q", got.ACP.SessionDir, sessRoot)
	}
}

func TestCollectorCachesUnchangedProjectsAndSeesChanges(t *testing.T) {
	root := writeFixtureRoot(t)
	c := newSnapshotCollector()
	first := c.projects([]string{root})
	if len(c.cache) != 3 {
		t.Fatalf("cache size = %d", len(c.cache))
	}
	// Poison the cached copy: if the second call re-parsed, the poison is gone.
	alphaPath := filepath.Join(root, "alpha", ".project.yaml")
	cp := c.cache[alphaPath]
	cp.snap.Description = "FROM-CACHE"
	c.cache[alphaPath] = cp
	second := c.projects([]string{root})
	if got := findInList(second, "alpha").Description; got != "FROM-CACHE" {
		t.Errorf("unchanged project was re-parsed (description %q)", got)
	}
	if findInList(first, "alpha").Description == "FROM-CACHE" {
		t.Error("cache poisoning leaked into an earlier result (results must be copies)")
	}

	// A changed task file invalidates only that project.
	taskPath := filepath.Join(root, "alpha", "tasks", "ship.yaml")
	if err := os.WriteFile(taskPath, []byte("name: ship\ndescription: ship it now\ndepends_on: [build]\nstatus: committed\nattempts: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third := c.projects([]string{root})
	a := findInList(third, "alpha")
	if a.Description == "FROM-CACHE" || a.TaskCounts["committed"] != 1 || a.TaskCounts["ready"] != 0 {
		t.Errorf("changed project not refreshed: desc=%q counts=%v", a.Description, a.TaskCounts)
	}

	// Removing a project drops it (and its cache entry).
	if err := os.RemoveAll(filepath.Join(root, "bravo")); err != nil {
		t.Fatal(err)
	}
	fourth := c.projects([]string{root})
	if len(fourth) != 2 || len(c.cache) != 2 {
		t.Errorf("after removal: %d projects, %d cached", len(fourth), len(c.cache))
	}
}

func findInList(ps []ProjectSnapshot, ws string) ProjectSnapshot {
	for _, p := range ps {
		if p.WorkStream == ws {
			return p
		}
	}
	return ProjectSnapshot{}
}

func TestWriteFileAtomicNoPartialReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "snapshot.json")
	mkDoc := func(i int) []byte {
		b, _ := json.Marshal(map[string]any{"i": i, "pad": strings.Repeat("x", 200000)})
		return b
	}
	if err := WriteFileAtomic(path, mkDoc(0)); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	var bad atomic.Int32
	var reads atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				data, err := os.ReadFile(path)
				if err != nil {
					bad.Add(1)
					continue
				}
				var v map[string]any
				if json.Unmarshal(data, &v) != nil {
					bad.Add(1)
				}
				reads.Add(1)
			}
		}()
	}
	for i := 1; i <= 100; i++ {
		if err := WriteFileAtomic(path, mkDoc(i)); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d of %d concurrent reads saw a missing or partial file", bad.Load(), reads.Load())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("temp files left behind: %v", names)
	}
}

func TestWriteFileAtomicFailureLeavesOldFileAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	if err := WriteFileAtomic(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	// Make the destination a directory so the final rename fails.
	bad := filepath.Join(dir, "adir")
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(bad, []byte("new")); err == nil {
		t.Fatal("expected error renaming over a non-empty directory")
	}
	if data, _ := os.ReadFile(path); string(data) != "old" {
		t.Errorf("old file clobbered: %q", data)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind after failed write", e.Name())
		}
	}
}

func TestNewRejectsStateFileInsideRoot(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		filepath.Join(root, "snapshot.json"),
		filepath.Join(root, "state", "snapshot.json"),
		root,
	} {
		if _, err := New([]string{root}, audit.New(&safeBuf{}), WithStateFile(p, 0)); err == nil {
			t.Errorf("New accepted state file %s inside root %s", p, root)
		} else if !strings.Contains(err.Error(), "outside") {
			t.Errorf("unhelpful error for %s: %v", p, err)
		}
	}
	sibling := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-state", "snapshot.json")
	d, err := New([]string{root}, audit.New(&safeBuf{}), WithStateFile(sibling, 0))
	if err != nil {
		t.Fatalf("sibling dir with shared name prefix wrongly rejected: %v", err)
	}
	_ = d.watcher.Close()
}

// TestSnapshotWritesNeverTouchTheWatchedRoots is the no-event-loop guarantee:
// publishing snapshots (and the final stopped snapshot) must produce no
// filesystem event under the roots — neither for an independent fsnotify
// watcher nor in the daemon's own audit trail (no project/task dispatch).
func TestSnapshotWritesNeverTouchTheWatchedRoots(t *testing.T) {
	root := writeFixtureRoot(t)
	stateFile := filepath.Join(t.TempDir(), "state", "snapshot.json")

	// Independent observer on every directory under the root.
	obs, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer obs.Close()
	if err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err == nil && e.IsDir() {
			return obs.Add(p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var events []string
	var evMu sync.Mutex
	go func() {
		for ev := range obs.Events {
			evMu.Lock()
			events = append(events, ev.String())
			evMu.Unlock()
		}
	}()

	auditBuf := &safeBuf{}
	d, err := New([]string{root}, audit.New(auditBuf), WithStateFile(stateFile, 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Let startup settle first: the startup scan legitimately dispatches the
	// fixture projects (e.g. stamping created_at). Only what happens AFTER that
	// is attributable to snapshot publishing.
	waitForFile(t, stateFile)
	if ok, got := waitFor(t, auditBuf, "startup_scan"); !ok {
		t.Fatalf("startup scan never finished:\n%s", got)
	}
	time.Sleep(500 * time.Millisecond)
	evMu.Lock()
	eventsBefore := len(events)
	evMu.Unlock()
	auditBefore := len(auditBuf.String())

	for i := 0; i < 5; i++ {
		d.markSnapshotDirty()
		if err := d.writeSnapshot(true); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(3 * snapshotTick / 2) // spans a ticker-driven write too

	evMu.Lock()
	newEvents := append([]string(nil), events[eventsBefore:]...)
	evMu.Unlock()
	if len(newEvents) != 0 {
		t.Errorf("snapshot writes produced %d filesystem event(s) under the watched root: %v", len(newEvents), newEvents)
	}
	if added := auditBuf.String()[auditBefore:]; added != "" {
		t.Errorf("snapshot activity caused audit/dispatch activity:\n%s", added)
	}

	// Positive control: the observer does see a write under the root, so the
	// silence above is meaningful.
	if err := os.WriteFile(filepath.Join(root, "alpha", "control.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evMu.Lock()
		n := len(events)
		evMu.Unlock()
		if n > eventsBefore {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let the control write's remaining events land
	evMu.Lock()
	sawControl := len(events) > eventsBefore
	eventsBefore = len(events)
	evMu.Unlock()
	if !sawControl {
		t.Fatal("observer watcher saw no event for a direct write under the root; test is not exercising the watcher")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The final (stopped) snapshot is written during shutdown: also silent.
	time.Sleep(3 * snapshotDebounce)
	evMu.Lock()
	after := append([]string(nil), events[eventsBefore:]...)
	evMu.Unlock()
	if len(after) != 0 {
		t.Errorf("shutdown snapshot produced filesystem event(s) under the root: %v", after)
	}

	// And nothing but the fixture's own files exists under the root.
	_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.Contains(e.Name(), "snapshot") {
			t.Errorf("snapshot artifact inside root: %s", p)
		}
		return nil
	})
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func readSnapshotFile(t *testing.T, path string) *Snapshot {
	t.Helper()
	s, err := ReadSnapshot(path)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	return s
}

func TestDaemonPublishesAndRefreshesOnSessionChange(t *testing.T) {
	root := writeFixtureRoot(t)
	stateFile := filepath.Join(t.TempDir(), "state", "snapshot.json")
	d, err := New([]string{root}, audit.New(&safeBuf{}), WithStateFile(stateFile, 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitForFile(t, stateFile)
	first := readSnapshotFile(t, stateFile)
	if first.Version != SnapshotVersion || first.Daemon == nil || first.Daemon.State != DaemonStateRunning || len(first.Sessions) != 0 {
		t.Fatalf("initial snapshot = %+v", first)
	}

	// A session start is reflected promptly (well inside the 2s tick).
	alpha := filepath.Join(root, "alpha", ".project.yaml")
	sess := newStubSession("orch-task-alpha")
	d.trackSession(alpha, sess, agent.TaskAgent, "build", nil)
	waitSnapshot(t, stateFile, "session start", func(s *Snapshot) bool { return len(s.Sessions) == 1 })

	// ...and its end.
	_ = sess.Close()
	waitSnapshot(t, stateFile, "session end", func(s *Snapshot) bool { return len(s.Sessions) == 0 })

	// A project transition is picked up too.
	p, err := project.Load(alpha)
	if err != nil {
		t.Fatal(err)
	}
	p.Status = project.StatusBlocked
	p.BlockedReason = "stuck"
	if err := project.SaveAs(alpha, p, project.WriterAgent); err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, stateFile, "project transition", func(s *Snapshot) bool {
		return findInList(s.Projects, "alpha").Status == "blocked"
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	final := readSnapshotFile(t, stateFile)
	if final.Daemon.State != DaemonStateStopped || final.LiveStateAvailable {
		t.Errorf("final snapshot daemon.state=%q live=%v, want stopped/false", final.Daemon.State, final.LiveStateAvailable)
	}
}

func waitSnapshot(t *testing.T, path, what string, pred func(*Snapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := ReadSnapshot(path); err == nil && pred(s) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("snapshot never reflected %s", what)
}

func TestPublishSkipsUnchangedContentButHeartbeats(t *testing.T) {
	root := writeFixtureRoot(t)
	stateFile := filepath.Join(t.TempDir(), "snapshot.json")
	d, err := New([]string{root}, audit.New(&safeBuf{}), WithStateFile(stateFile, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.watcher.Close() })

	if err := d.writeSnapshot(false); err != nil {
		t.Fatal(err)
	}
	st1, _ := os.Stat(stateFile)
	first := readSnapshotFile(t, stateFile)

	time.Sleep(20 * time.Millisecond)
	if err := d.writeSnapshot(false); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(stateFile)
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("unchanged content was rewritten before the heartbeat elapsed")
	}

	// Past the heartbeat the file is refreshed (generated_at advances).
	d.snapshot.mu.Lock()
	d.snapshot.lastAt = time.Now().Add(-2 * snapshotHeartbeat)
	d.snapshot.mu.Unlock()
	if err := d.writeSnapshot(false); err != nil {
		t.Fatal(err)
	}
	if again := readSnapshotFile(t, stateFile); !again.GeneratedAt.After(first.GeneratedAt) {
		t.Errorf("heartbeat did not advance generated_at: %v -> %v", first.GeneratedAt, again.GeneratedAt)
	}
}

func TestDefaultStateFileSitsBesideSessionsRoot(t *testing.T) {
	got, err := DefaultStateFile("/home/u/.workingman/sessions")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/home/u/.workingman/state/snapshot.json"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSnapshotTextRenders(t *testing.T) {
	root := writeFixtureRoot(t)
	s := BuildOfflineSnapshot(OfflineOptions{Roots: []string{root}, SessionsRoot: t.TempDir()})
	var sb strings.Builder
	if err := s.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"alpha", "[working]", "bravo", "[blocked]", "LOAD ERROR", "1 failed", "build", "attempts=3", "deps=build", "PR acme/widgets#7", "unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, secretToken) || strings.Contains(out, "hunter2") {
		t.Errorf("text output leaks a secret:\n%s", out)
	}
}
