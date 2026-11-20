package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/workspace"
	"github.com/slimslenderslacks/work/internal/worktree"
)

// The tests in this file cover phase 4 of docs/parallel-task-worktrees.md:
// the slot split (S2), the limits (S3), the scheduling pass (S4) and the
// commit queue (S5). Unless a test needs agents that do real work, it drives
// the daemon directly (no Run) over a git-backed stub workspace, with a
// launcher whose sessions stay up until the test ends them, so which agents
// hold which slot is checked at each step.

// slotLauncher records every launch and keeps its session open until the test
// ends it (endTask / endCommit), standing in for the agent process.
type slotLauncher struct {
	mu       sync.Mutex
	specs    []agent.Spec
	sessions []*stubSession
}

func (l *slotLauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	s := newStubSession(spec.Name)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.specs = append(l.specs, spec)
	l.sessions = append(l.sessions, s)
	return s, nil
}

func (l *slotLauncher) count(kind agent.Kind) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.specs {
		if s.Kind == kind {
			n++
		}
	}
	return n
}

// close ends the open session the runner named name (runner.sessionName:
// "<kind>-<task>"), reporting whether there was one.
func (l *slotLauncher) close(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, s := range l.specs {
		if s.Name != name {
			continue
		}
		select {
		case <-l.sessions[i].done:
			continue
		default:
		}
		l.sessions[i].Close()
		return true
	}
	return false
}

func (l *slotLauncher) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.sessions {
		s.Close()
	}
}

// schedEnv is one project on a daemon under test.
type schedEnv struct {
	t      *testing.T
	d      *Daemon
	buf    *safeBuf
	lch    *slotLauncher
	stub   *workspace.StubManager
	cancel context.CancelFunc
	opts   []Option
	// sessionsRoot is where adopt writes session records for a restart.
	sessionsRoot string
	base         string // holds every project's control dir, the daemon's root
	root         string // control dir
	projectPath  string
	tasksDir     string
	shared       string
}

// newSchedEnv builds a daemon over a git-backed stub (so tasks get worktrees)
// with one project at limit, status working, and no tasks yet.
func newSchedEnv(t *testing.T, limit int, opts ...Option) *schedEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	buf := &safeBuf{}
	a := audit.New(buf)
	// Created before the cleanup below is registered, so they are removed
	// after it has ended every session (cleanups run last-in first-out).
	stubRoot := t.TempDir()
	base := t.TempDir()
	e := &schedEnv{t: t, buf: buf, stub: workspace.NewGitStub(stubRoot), base: base, sessionsRoot: t.TempDir(), opts: opts}
	e.newDaemon(a)
	e.addProject("proj", limit)
	return e
}

// newDaemon builds the env's daemon (again, for restart), watching base.
func (e *schedEnv) newDaemon(a *audit.Logger) {
	e.t.Helper()
	lch := &slotLauncher{}
	d, err := newDaemon(e.t, []string{e.base}, a, append([]Option{WithRunner(&runner.Runner{
		Workspaces:   e.stub,
		Launcher:     lch,
		Audit:        a,
		SessionsRoot: e.sessionsRoot,
		Command:      func(agent.Kind, string) []string { return []string{"true"} },
	})}, e.opts...)...)
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	e.t.Cleanup(func() { cancel(); lch.closeAll() })
	e.d, e.lch, e.cancel = d, lch, cancel
}

// restart stands in for a daemon restart: the old daemon stops without
// routing any session end (its context is cancelled first, as on shutdown),
// and a fresh one is built over the same files. The caller seeds session
// records (adopt) and runs the startup steps.
func (e *schedEnv) restart() {
	e.t.Helper()
	e.cancel()
	e.lch.closeAll()
	e.newDaemon(audit.New(e.buf))
}

// adopt writes a live session.json for an agent of kind working the named
// task, as an acp-wrapper the previous daemon launched would have left.
// StatusStarting within its grace keeps the adopted session up without a
// sandbox probe; end it with endAdopted.
func (e *schedEnv) adopt(kind agent.Kind, name string) string {
	e.t.Helper()
	store, err := session.NewStore(e.sessionsRoot)
	if err != nil {
		e.t.Fatal(err)
	}
	id := fmt.Sprintf("orch-%s-%s", kind, name)
	now := time.Now()
	if err := store.Write(session.Session{
		ID: id, Status: session.StatusStarting, CreatedAt: now, UpdatedAt: now,
		ProjectPath: e.projectPath, TaskPath: e.taskPath(name), Kind: kind.String(),
	}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// endAdopted removes an adopted session's record, which is how an
// acp-wrapper's exit shows (the adopted session notices within
// reconcilePollInterval).
func (e *schedEnv) endAdopted(id string) {
	e.t.Helper()
	store, err := session.NewStore(e.sessionsRoot)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := store.Remove(id); err != nil {
		e.t.Fatal(err)
	}
}

// addProject creates (and selects) another project on the same daemon.
func (e *schedEnv) addProject(name string, limit int) {
	e.t.Helper()
	e.root = filepath.Join(e.base, name)
	e.tasksDir = filepath.Join(e.root, "tasks")
	if err := os.MkdirAll(e.tasksDir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.projectPath = filepath.Join(e.root, "project.yaml")
	branch := "feat/" + name
	e.shared = filepath.Join(e.stub.Root, branch)
	if err := project.SaveAs(e.projectPath, &project.Project{
		Description:      name,
		Branch:           branch,
		Status:           project.StatusWorking,
		Repos:            []project.Repo{{Org: "docker", Name: "gateway"}},
		MaxParallelTasks: limit,
	}, project.WriterAgent); err != nil {
		e.t.Fatalf("SaveAs: %v", err)
	}
}

// addTask writes a ready task. A task with no touches/areas is exclusive (H1);
// pass touches to make it independent of other tasks touching elsewhere.
func (e *schedEnv) addTask(name string, touches []string, deps ...string) {
	e.t.Helper()
	tk := &task.Task{Name: name, Status: task.StatusReady, DependsOn: deps}
	if touches != nil {
		tk.Touches = &touches
	}
	mustSaveTask(e.t, filepath.Join(e.tasksDir, name+".yaml"), tk)
}

func (e *schedEnv) taskPath(name string) string { return filepath.Join(e.tasksDir, name+".yaml") }

func (e *schedEnv) task(name string) *task.Task {
	e.t.Helper()
	return mustLoadTask(e.t, e.taskPath(name))
}

func (e *schedEnv) setTask(name string, mutate func(*task.Task)) {
	e.t.Helper()
	if err := task.Update(e.taskPath(name), func(t *task.Task) bool { mutate(t); return true }); err != nil {
		e.t.Fatalf("update %s: %v", name, err)
	}
}

func (e *schedEnv) pass() {
	e.t.Helper()
	p, err := project.Load(e.projectPath)
	if err != nil {
		e.t.Fatal(err)
	}
	e.d.schedule(e.projectPath, p)
}

// running returns the names of the tasks holding a #task:<name> slot.
func (e *schedEnv) running() []string {
	var out []string
	for _, s := range e.d.ListSessions() {
		if s.AgentName == agent.TaskAgent.String() && strings.HasPrefix(s.ID, e.projectPath+"#") {
			out = append(out, s.TaskName)
		}
	}
	sortStrings(out)
	return out
}

// integrating returns the task holding #commit, or "".
func (e *schedEnv) integrating() string {
	e.d.sessionsMu.Lock()
	defer e.d.sessionsMu.Unlock()
	return e.d.sessions[commitSessionKey(e.projectPath)].taskName
}

// finishTask has the task's agent write status and exit.
func (e *schedEnv) finishTask(name string, status task.Status) {
	e.t.Helper()
	e.setTask(name, func(t *task.Task) { t.Status = status })
	if !e.lch.close("task-" + name) {
		e.t.Fatalf("no open task session for %s", name)
	}
}

// finishCommit has the commit agent integrate name's (empty) branch: the
// branch is fast-forwarded onto the shared branch, so the daemon's ancestor
// check passes, and the task is written committed.
func (e *schedEnv) finishCommit(name string) {
	e.t.Helper()
	sharedRepo := filepath.Join(e.shared, "gateway")
	tk := e.task(name)
	if tk.TaskBranch != "" {
		gitOK(e.t, sharedRepo, "merge", "-q", "--ff-only", tk.TaskBranch)
	}
	e.setTask(name, func(t *task.Task) { t.Status = task.StatusCommitted })
	if !e.lch.close("commit-" + name) {
		e.t.Fatalf("no open commit session for %s", name)
	}
}

func (e *schedEnv) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s.\naudit:\n%s", what, e.buf.String())
}

func (e *schedEnv) status() project.Status {
	p, err := project.Load(e.projectPath)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Status
}

func gitOK(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func eqStrings(a []string, b ...string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// With limit 2, two independent ready tasks both start, each in its own
// #task:<name> slot and worktree; with limit 1 only the first does, and the
// second starts only once the first is committed (S1, S3).
func TestScheduleLimitTwoVsOne(t *testing.T) {
	t.Run("limit 2", func(t *testing.T) {
		e := newSchedEnv(t, 2)
		e.addTask("api", []string{"gateway/api/**"})
		e.addTask("ui", []string{"gateway/ui/**"})
		e.pass()
		if got := e.running(); !eqStrings(got, "api", "ui") {
			t.Fatalf("running = %v, want both.\naudit:\n%s", got, e.buf.String())
		}
		for _, name := range []string{"api", "ui"} {
			if !e.d.hasSession(taskSessionKey(e.projectPath, name)) {
				t.Errorf("%s not tracked under its own task slot", name)
			}
			if tk := e.task(name); tk.Status != task.StatusRunning || tk.Worktree == "" {
				t.Errorf("%s = %s worktree %q, want running in a worktree", name, tk.Status, tk.Worktree)
			}
		}
		if e.d.hasSession(e.projectPath) {
			t.Error("a task agent took the bare project slot")
		}
	})
	t.Run("limit 1", func(t *testing.T) {
		e := newSchedEnv(t, 1)
		e.addTask("api", []string{"gateway/api/**"})
		e.addTask("ui", []string{"gateway/ui/**"})
		e.pass()
		if got := e.running(); !eqStrings(got, "api") {
			t.Fatalf("running = %v, want only api", got)
		}
		e.finishTask("api", task.StatusSuccess)
		e.eventually("api integrating", func() bool { return e.integrating() == "api" })
		// api at success still holds the one slot: ui waits for it to land.
		if got := e.running(); len(got) != 0 {
			t.Fatalf("running = %v while api integrates, want none", got)
		}
		if tk := e.task("ui"); tk.Status != task.StatusReady {
			t.Errorf("ui = %s while api is queued, want ready", tk.Status)
		}
		e.finishCommit("api")
		e.eventually("ui running", func() bool { return eqStrings(e.running(), "ui") })
	})
}

// Without max_parallel_tasks a project behaves exactly as before: tasks run
// one at a time even when their hints are disjoint (D7).
func TestScheduleNoLimitIsSerial(t *testing.T) {
	e := newSchedEnv(t, 0)
	e.addTask("a", []string{"gateway/a/**"})
	e.addTask("b", []string{"gateway/b/**"})
	e.pass()
	if got := e.running(); !eqStrings(got, "a") {
		t.Fatalf("running = %v, want only a", got)
	}
}

// Tasks without hints are exclusive (H1), so even at limit 2 they run one at
// a time, and a deferred task is logged once, not on every pass.
func TestScheduleExclusiveTasksSerialize(t *testing.T) {
	e := newSchedEnv(t, 2)
	e.addTask("a", nil)
	e.addTask("b", nil)
	e.pass()
	e.pass()
	if got := e.running(); !eqStrings(got, "a") {
		t.Fatalf("running = %v, want only a", got)
	}
	if n := strings.Count(e.buf.String(), "task_deferred_overlap"); n != 1 {
		t.Errorf("task_deferred_overlap logged %d times, want once:\n%s", n, e.buf.String())
	}
	if info := e.d.deferredInfo(e.projectPath); info == nil || !eqStrings(info.Overlap, "b") {
		t.Errorf("live.deferred = %+v, want overlap [b]", info)
	}
}

// A dependent does not start while its dependency is at success: it waits for
// the integration, and says so once (S5).
func TestScheduleDependentWaitsOnIntegration(t *testing.T) {
	e := newSchedEnv(t, 3)
	e.addTask("base", []string{"gateway/base/**"})
	e.addTask("child", []string{"gateway/child/**"}, "base")
	e.pass()
	e.finishTask("base", task.StatusSuccess)
	e.eventually("base integrating", func() bool { return e.integrating() == "base" })
	e.pass()
	if got := e.running(); len(got) != 0 {
		t.Fatalf("running = %v while base is integrating, want none", got)
	}
	if tk := e.task("child"); tk.Status != task.StatusReady {
		t.Fatalf("child = %s, want ready (waiting)", tk.Status)
	}
	if n := strings.Count(e.buf.String(), "task_waiting_integration"); n != 1 {
		t.Errorf("task_waiting_integration logged %d times, want once:\n%s", n, e.buf.String())
	}
	if tk := e.task("base"); tk.SucceededAt == nil {
		t.Error("succeeded_at not stamped when the task agent reported success")
	}
	e.finishCommit("base")
	e.eventually("child running", func() bool { return eqStrings(e.running(), "child") })
}

// The commit queue is FIFO by succeeded_at and integrates one task at a time:
// a task that finishes while another integrates waits its turn, and the end
// of each integration starts the next (S5).
func TestScheduleCommitQueueSerializesIntegrations(t *testing.T) {
	e := newSchedEnv(t, 2)
	e.addTask("a", []string{"gateway/a/**"})
	e.addTask("b", []string{"gateway/b/**"})
	e.pass()
	e.finishTask("b", task.StatusSuccess)
	e.eventually("b integrating", func() bool { return e.integrating() == "b" })
	e.finishTask("a", task.StatusSuccess)
	time.Sleep(100 * time.Millisecond)
	if got := e.integrating(); got != "b" {
		t.Fatalf("integrating = %q, want b still (one integration at a time)", got)
	}
	if n := e.lch.count(agent.CommitAgent); n != 1 {
		t.Fatalf("commit agents started = %d, want 1", n)
	}
	e.finishCommit("b")
	e.eventually("a integrating", func() bool { return e.integrating() == "a" })
	e.finishCommit("a")
	e.eventually("project idle", func() bool { return e.status() == project.StatusIdle })
}

// Session ends arriving at the same moment never admit more than the limit:
// each frees one slot, the passes they run are serialized by the scheduling
// lock, and the on-disk running marks are what the next pass counts (S4).
// Run with -race.
func TestScheduleSimultaneousSessionEndsRespectLimit(t *testing.T) {
	e := newSchedEnv(t, 2)
	for i := range 8 {
		e.addTask(fmt.Sprintf("t%d", i), []string{fmt.Sprintf("gateway/t%d/**", i)})
	}
	e.pass()
	if got := e.running(); len(got) != 2 {
		t.Fatalf("running = %v, want 2", got)
	}
	// Every task fails and is retried, then succeeds: lots of session ends,
	// in pairs, as close together as the test can make them.
	var wg sync.WaitGroup
	maxSeen := 0
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A task only enters flight under the scheduling lock, so a scan
			// holding it never counts one that joined mid-scan beside one
			// that left: the count is at most the real one at its start.
			mu := e.d.schedLock(e.projectPath)
			mu.Lock()
			n := 0
			for _, tk := range loadTasksLenient(e.tasksDir) {
				if tk.Status == task.StatusRunning || tk.Status == task.StatusSuccess {
					n++
				}
			}
			mu.Unlock()
			if n > maxSeen {
				maxSeen = n
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for round := 0; round < 12; round++ {
		names := e.running()
		if len(names) == 0 {
			break
		}
		for _, name := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.setTask(name, func(t *task.Task) { t.Status = task.StatusSuccess })
				e.lch.close("task-" + name)
			}()
		}
		wg.Wait()
		// Integrate whatever is queued, one at a time.
		for {
			time.Sleep(20 * time.Millisecond)
			head := e.integrating()
			if head == "" {
				break
			}
			e.finishCommit(head)
		}
	}
	close(stop)
	e.eventually("all committed", func() bool { return e.status() == project.StatusIdle })
	if maxSeen > 2 {
		t.Errorf("in-flight tasks peaked at %d, above the limit of 2", maxSeen)
	}
	if n := e.lch.count(agent.TaskAgent); n != 8 {
		t.Errorf("task agents started = %d, want 8 (no duplicates)", n)
	}
	if n := e.d.tokensInUse(); n != 0 {
		t.Errorf("tokens still held after the run: %d", n)
	}
}

// The global cap (--max-parallel-tasks) bounds task agents across projects:
// a project that wants a slot while none is free is deferred, logged once,
// and resumed when a token frees (S3).
func TestScheduleGlobalCapAcrossProjects(t *testing.T) {
	e := newSchedEnv(t, 2, WithMaxParallelTasks(2))
	first := e.projectPath
	e.addTask("a1", []string{"gateway/a1/**"})
	e.addTask("a2", []string{"gateway/a2/**"})
	e.pass()
	if got := e.running(); len(got) != 2 {
		t.Fatalf("project 1 running = %v, want 2", got)
	}

	e.addProject("other", 2)
	second := e.projectPath
	e.addTask("b1", []string{"gateway/b1/**"})
	e.pass()
	if got := e.running(); len(got) != 0 {
		t.Fatalf("project 2 running = %v with the cap spent, want none", got)
	}
	if tk := e.task("b1"); tk.Status != task.StatusReady {
		t.Fatalf("b1 = %s, want ready (deferred for the cap)", tk.Status)
	}
	if !strings.Contains(e.buf.String(), "task_deferred_cap") {
		t.Fatalf("no task_deferred_cap:\n%s", e.buf.String())
	}
	if info := e.d.deferredInfo(second); info == nil || !info.Cap {
		t.Errorf("live.deferred = %+v, want cap", info)
	}

	// A task of the first project ends (and fails for good): its token goes
	// to the deferred project.
	secondTasks := e.tasksDir
	e.projectPath, e.tasksDir = first, filepath.Join(filepath.Dir(first), "tasks")
	e.setTask("a1", func(t *task.Task) { t.Attempts = maxTaskAttempts })
	e.finishTask("a1", task.StatusFailed)
	e.projectPath, e.tasksDir = second, secondTasks
	e.eventually("b1 running", func() bool { return eqStrings(e.running(), "b1") })
	if n := e.d.tokensInUse(); n != 2 {
		t.Errorf("tokens in use = %d, want 2", n)
	}
}

// On battery a session end starts nothing: neither the commit agent for the
// task that just succeeded nor the next task (S6). Back on AC the resume pass
// starts both.
func TestScheduleOnBatterySessionEndStartsNothing(t *testing.T) {
	fp := &fakePower{}
	e := newSchedEnv(t, 2, WithPowerSource(fp, 0))
	e.addTask("a", []string{"gateway/a/**"})
	e.addTask("b", nil)
	e.pass()
	if got := e.running(); !eqStrings(got, "a") {
		t.Fatalf("running = %v, want a", got)
	}
	fp.set(true)
	e.d.checkPower(e.d.ctx)
	e.finishTask("a", task.StatusSuccess)
	time.Sleep(200 * time.Millisecond)
	if n := e.lch.count(agent.CommitAgent); n != 0 {
		t.Fatalf("commit agent started on battery")
	}
	if n := e.lch.count(agent.TaskAgent); n != 1 {
		t.Fatalf("task agents started = %d on battery, want 1", n)
	}
	if info := e.d.deferredInfo(e.projectPath); info == nil || !info.Power {
		t.Errorf("live.deferred = %+v, want power", info)
	}
	fp.set(false)
	e.d.checkPower(e.d.ctx)
	e.eventually("a integrating on AC", func() bool { return e.integrating() == "a" })
}

// A project with a project or review agent in its bare slot starts no task or
// commit agents beside it (S2, X1), and the review agent never starts while
// task or commit work is in flight.
func TestScheduleWaitsForBareSlotAndReviewWaitsForWork(t *testing.T) {
	e := newSchedEnv(t, 2)
	e.addTask("a", []string{"gateway/a/**"})
	review := newStubSession("review-x")
	if !e.d.trackSession(e.projectPath, review, agent.ReviewAgent, "", nil) {
		t.Fatal("track review")
	}
	e.pass()
	if got := e.running(); len(got) != 0 {
		t.Fatalf("running = %v beside a review agent, want none", got)
	}
	review.Close()
	e.eventually("review slot free", func() bool { return !e.d.hasSession(e.projectPath) })
	e.pass()
	if got := e.running(); !eqStrings(got, "a") {
		t.Fatalf("running = %v, want a", got)
	}
	if !e.d.projectBusy(e.projectPath) {
		t.Fatal("projectBusy false with a task agent running")
	}
	p, _ := project.Load(e.projectPath)
	e.d.dispatchReviewAgent(e.projectPath, p)
	if n := e.lch.count(agent.ReviewAgent); n != 0 {
		t.Errorf("review agent started beside a task agent")
	}
}

// A block does not kill siblings, and the queue is frozen while the project
// is blocked: a sibling's success stays queued and no commit agent starts. A
// second failure appends to the blocked reason instead of replacing it (X3).
func TestScheduleBlockFreezesQueueAndAppendsReasons(t *testing.T) {
	e := newSchedEnv(t, 3)
	e.addTask("a", []string{"gateway/a/**"})
	e.addTask("b", []string{"gateway/b/**"})
	e.addTask("c", []string{"gateway/c/**"})
	e.pass()
	if got := e.running(); len(got) != 3 {
		t.Fatalf("running = %v, want 3", got)
	}
	e.setTask("a", func(t *task.Task) { t.Attempts = maxTaskAttempts })
	e.finishTask("a", task.StatusFailed)
	e.eventually("blocked", func() bool { return e.status() == project.StatusBlocked })
	if got := e.running(); !eqStrings(got, "b", "c") {
		t.Fatalf("running = %v after the block, want b and c left running", got)
	}
	e.finishTask("b", task.StatusSuccess)
	e.setTask("c", func(t *task.Task) { t.Attempts = maxTaskAttempts })
	e.finishTask("c", task.StatusFailed)
	e.eventually("both sessions ended", func() bool { return len(e.running()) == 0 })
	time.Sleep(100 * time.Millisecond)
	if n := e.lch.count(agent.CommitAgent); n != 0 {
		t.Errorf("commit agent started on a blocked project")
	}
	if tk := e.task("b"); tk.Status != task.StatusSuccess || tk.SucceededAt == nil {
		t.Errorf("b = %s (succeeded_at %v), want queued", tk.Status, tk.SucceededAt)
	}
	p, _ := project.Load(e.projectPath)
	if !strings.Contains(p.BlockedReason, `task "a" failed`) || !strings.Contains(p.BlockedReason, `; also: task "c" failed`) {
		t.Errorf("blocked_reason = %q, want a's reason with c's appended", p.BlockedReason)
	}
	if !strings.Contains(e.buf.String(), "project_block_additional") {
		t.Errorf("no project_block_additional:\n%s", e.buf.String())
	}
}

// A failed task's retry takes the slot its run freed straight away, even with
// a ready task sorting ahead of it.
func TestScheduleRetryKeepsItsSlot(t *testing.T) {
	e := newSchedEnv(t, 1)
	e.addTask("b", []string{"gateway/b/**"})
	e.pass()
	e.addTask("a", []string{"gateway/a/**"})
	e.finishTask("b", task.StatusFailed)
	e.eventually("b retried", func() bool { return e.lch.count(agent.TaskAgent) == 2 && eqStrings(e.running(), "b") })
	if tk := e.task("a"); tk.Status != task.StatusReady {
		t.Errorf("a = %s, want ready behind b's retry", tk.Status)
	}
}

// The wolf launched for a block is told what is still in flight: running
// tasks, and the queue with the integrating task marked (X3).
func TestWolfToldInFlightTasks(t *testing.T) {
	e := newSchedEnv(t, 3)
	e.addTask("a", []string{"gateway/a/**"})
	e.addTask("b", []string{"gateway/b/**"})
	e.addTask("c", []string{"gateway/c/**"})
	e.pass()
	e.finishTask("b", task.StatusSuccess)
	e.eventually("b integrating", func() bool { return e.integrating() == "b" })
	got := e.d.inFlightTasks(e.projectPath)
	want := []string{"a:running:0:false", "c:running:0:false", "b:success:1:true"}
	var have []string
	for _, f := range got {
		have = append(have, fmt.Sprintf("%s:%s:%d:%t", f.Name, f.Status, f.QueuePosition, f.Integrating))
	}
	if !eqStrings(have, want...) {
		t.Errorf("inFlightTasks = %v, want %v", have, want)
	}
}

// A task's worktree is cut by its own launch, so tasks admitted together are
// not mistaken for legacy shared-tree work (D7), and each gets its own
// worktree on its own branch.
func TestScheduleAdmittedTasksGetOwnWorktrees(t *testing.T) {
	e := newSchedEnv(t, 3)
	for _, n := range []string{"a", "b", "c"} {
		e.addTask(n, []string{"gateway/" + n + "/**"})
	}
	e.pass()
	if strings.Contains(e.buf.String(), "worktree_skipped_legacy") {
		t.Fatalf("a task admitted beside others took the legacy path:\n%s", e.buf.String())
	}
	for _, n := range []string{"a", "b", "c"} {
		tk := e.task(n)
		if tk.Worktree != worktree.Root(e.shared, n) || tk.TaskBranch != worktree.BranchName("feat/proj", n) {
			t.Errorf("%s worktree %q branch %q", n, tk.Worktree, tk.TaskBranch)
		}
	}
}

// TestParallelEndToEnd runs a whole project at max_parallel_tasks: 2 through
// a real daemon, with agents that are shell scripts doing real git work on
// the stub's real repos: api and ui are independent (disjoint touches), docs
// depends on api. api and ui run at the same time, each in its own worktree;
// both integrate through the one commit queue (the second by a real rebase
// onto the first); docs starts only once api has landed; and the shared
// branch ends with all three tasks' work.
func TestParallelEndToEnd(t *testing.T) {
	e := newWtEnv(t)
	e.limit = 2
	mark := t.TempDir()
	for _, tk := range []struct {
		name    string
		touches []string
		deps    []string
	}{
		{"api", []string{"gateway/api/**"}, nil},
		{"ui", []string{"gateway/ui/**"}, nil},
		{"docs", []string{"gateway/docs/**"}, []string{"api"}},
	} {
		touches := tk.touches
		mustSaveTask(t, e.taskPath(tk.name), &task.Task{Name: tk.name, Status: task.StatusReady, Touches: &touches, DependsOn: tk.deps})
	}
	// Each task agent records that it started, waits until both independent
	// tasks are running (so the test fails, by timing out, if they were run
	// one after the other), then commits a file under its own directory.
	e.byTask = func(kind agent.Kind, name string) string {
		if kind == agent.CommitAgent {
			return e.integrateScript(name, integrateOpts{})
		}
		wait := ""
		if name != "docs" {
			wait = fmt.Sprintf("i=0; while [ ! -e %[1]q/api ] || [ ! -e %[1]q/ui ]; do i=$((i+1)); [ $i -lt 200 ] || exit 1; sleep 0.05; done\n", mark)
		}
		return fmt.Sprintf(`touch %[1]q/%[2]s
%[3]smkdir -p gateway/%[2]s && echo %[2]s > gateway/%[2]s/file.txt
git -C gateway add -A && git -C gateway %[4]s commit -q -m %[2]s
%[5]s`, mark, name, wait, gitID, setStatus(e.taskPath(name), "success"))
	}
	e.start()
	e.saveProject()
	snap := e.wait("project_idle")

	for _, name := range []string{"api", "ui", "docs"} {
		tk := e.loadTask(name)
		if tk.Status != task.StatusCommitted || tk.SucceededAt == nil || len(tk.Commits) != 1 {
			t.Errorf("%s = %s succeeded_at %v commits %v", name, tk.Status, tk.SucceededAt, tk.Commits)
		}
		if got := e.git(filepath.Join(e.shared, "gateway"), "show", "HEAD:"+name+"/file.txt"); got != name {
			t.Errorf("shared branch %s/file.txt = %q", name, got)
		}
	}
	// docs started only after api was integrated.
	apiDone := strings.Index(snap, "integration_succeeded path="+filepath.Join(e.root, "project.yaml")+" task=api")
	docsStart := strings.Index(snap, "task_running task=docs")
	if apiDone < 0 || docsStart < 0 || docsStart < apiDone {
		t.Errorf("docs started before api landed (api landed at %d, docs ran at %d):\n%s", apiDone, docsStart, snap)
	}
	// One of the two independent tasks integrated by rebasing onto the other.
	if !strings.Contains(snap, "fast_forward=false") {
		t.Errorf("no integration rebased onto a moved tip:\n%s", snap)
	}
	if n := e.count(agent.TaskAgent); n != 3 {
		t.Errorf("task agents started = %d, want 3", n)
	}
	if strings.Contains(snap, "project_blocked") {
		t.Errorf("project blocked:\n%s", snap)
	}
}

// A push task needs a free slot to join the commit queue from ready; while it
// has none, the next task already at success integrates first, so a full
// project cannot stall behind a push task (S3, S5).
func TestQueueHeadPassesOverPushTaskWithoutSlot(t *testing.T) {
	at := time.Now()
	push := &task.Task{Name: "a-push", Status: task.StatusReady, Source: &task.Source{Kind: task.SourceKindPush}}
	done := &task.Task{Name: "b", Status: task.StatusSuccess, SucceededAt: &at}
	q := []*task.Task{push, done}
	if got := queueHead(q, true); got != push {
		t.Errorf("with a free slot the head is %v, want the push task", got)
	}
	if got := queueHead(q, false); got != done {
		t.Errorf("without a free slot the head is %v, want b", got)
	}
	if got := queueHead([]*task.Task{push}, false); got != nil {
		t.Errorf("a lone push task without a slot = %v, want none", got)
	}
}

// Conflict feedback (H3): a re-run after an integration conflict also touches
// the files it conflicted in, so schedule holds it back while a task declaring
// one of them runs, though their declared hints are disjoint. It starts once
// that task has landed, ahead of a ready task sorting before it (re-runs
// first), which then waits on the re-run in turn.
func TestScheduleConflictReRunSerializedAgainstConflictingFile(t *testing.T) {
	e := newSchedEnv(t, 2)
	e.addTask("router", []string{"gateway/internal/router.go"})
	e.pass()
	if got := e.running(); !eqStrings(got, "router") {
		t.Fatalf("running = %v, want [router]", got)
	}
	e.addTask("api", []string{"gateway/internal/api/**"})
	e.setTask("api", func(t *task.Task) {
		t.Attempts = 1
		t.Integration = &task.Integration{Result: "conflict", Repo: "gateway", Files: []string{"internal/router.go"}}
	})
	e.addTask("aaa", []string{"gateway/internal/router.go"})
	e.pass()
	if got := e.running(); !eqStrings(got, "router") {
		t.Fatalf("running = %v, want only router: the re-run conflicted in its file", got)
	}
	if info := e.d.deferredInfo(e.projectPath); info == nil || !eqStrings(info.Overlap, "aaa", "api") {
		t.Errorf("live.deferred = %+v, want overlap [aaa api]", info)
	}
	if !strings.Contains(e.buf.String(), "task=api overlaps=router\n") {
		t.Errorf("no deferral naming router:\n%s", e.buf.String())
	}

	// router stays in flight (and api waits) until it has landed.
	e.finishTask("router", task.StatusSuccess)
	e.eventually("router integrating", func() bool { return e.integrating() == "router" })
	if got := e.running(); len(got) != 0 {
		t.Fatalf("running = %v while router integrates, want none", got)
	}
	e.finishCommit("router")
	e.eventually("api running", func() bool { return eqStrings(e.running(), "api") })
	if tk := e.task("aaa"); tk.Status != task.StatusReady {
		t.Errorf("aaa = %s, want ready behind the re-run", tk.Status)
	}
}
