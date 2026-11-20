package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/prompts"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/taskgraph"
)

// Session slots (docs/parallel-task-worktrees.md S2). The project and review
// agents keep the bare project path; each task agent has a slot of its own and
// the commit agent (the head of the commit queue) has one per project. The
// markers follow the #planning/#wolf/#archive convention, appended to the
// final path element so filepath.Dir(key) is still the control dir.
const (
	taskKeyMarker   = "#task:"
	commitKeyMarker = "#commit"
)

// taskSessionKey is the session-map key of the task agent working taskName.
func taskSessionKey(projectPath, taskName string) string {
	return projectPath + taskKeyMarker + taskName
}

// commitSessionKey is the session-map key of the project's commit agent.
func commitSessionKey(projectPath string) string {
	return projectPath + commitKeyMarker
}

// runsAlongsidePlanning reports whether the session in the slot named by
// marker (splitSessionKey's second result) may keep running while a planner
// works on the same project: task and commit agents work in worktrees and the
// shared workspace, not the control dir, and only ever touch their own task
// file there, which the planner is told to leave alone.
func runsAlongsidePlanning(marker string) bool {
	return marker == strings.TrimPrefix(commitKeyMarker, "#") ||
		strings.HasPrefix(marker, strings.TrimPrefix(taskKeyMarker, "#"))
}

// projectBusy reports whether any task or commit work of the project is in
// flight, or a project or review agent holds its bare slot: a live session in
// the bare, #task:* or #commit slot, a commit reservation (beginCommit), or a
// task that schedule has admitted but whose session is not tracked yet. The
// review agent, a re-plan and the archive agent wait for it to clear.
func (d *Daemon) projectBusy(projectPath string) bool {
	if d.commitReserved(projectPath) || len(d.admittingNames(projectPath)) > 0 {
		return true
	}
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	for key := range d.sessions {
		if key == projectPath || key == commitSessionKey(projectPath) || strings.HasPrefix(key, projectPath+taskKeyMarker) {
			return true
		}
	}
	return false
}

// liveTaskSessions counts the project's tracked #task:* sessions.
func (d *Daemon) liveTaskSessions(projectPath string) int {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	n := 0
	for key := range d.sessions {
		if strings.HasPrefix(key, projectPath+taskKeyMarker) {
			n++
		}
	}
	return n
}

// schedLock returns the project's scheduling mutex (schedMu in the doc).
func (d *Daemon) schedLock(projectPath string) *sync.Mutex {
	d.schedLocksMu.Lock()
	defer d.schedLocksMu.Unlock()
	mu := d.schedLocks[projectPath]
	if mu == nil {
		mu = &sync.Mutex{}
		d.schedLocks[projectPath] = mu
	}
	return mu
}

// worktreeLock returns the mutex that serializes one project's host-side git
// work at launch: provisioning the shared workspace, cutting or reusing a task
// worktree, and prepareIntegration. Several launches of one pass run at once,
// and concurrent `git worktree add`s in one repository race on its locks.
func (d *Daemon) worktreeLock(projectPath string) *sync.Mutex {
	d.schedLocksMu.Lock()
	defer d.schedLocksMu.Unlock()
	mu := d.worktreeLocks[projectPath]
	if mu == nil {
		mu = &sync.Mutex{}
		d.worktreeLocks[projectPath] = mu
	}
	return mu
}

// freeTokensLocked is the number of global task tokens not in use (S3), or
// math.MaxInt with no cap. tokenMu must be held.
func (d *Daemon) freeTokensLocked() int {
	if d.maxParallelTasks <= 0 {
		return math.MaxInt
	}
	return max(d.maxParallelTasks-len(d.taskTokens), 0)
}

// takeToken records a global token for the task at taskPath without checking
// the cap: reconcileSessions re-takes one per adopted task session, which is
// already running whatever the cap says.
func (d *Daemon) takeToken(taskPath string) {
	d.tokenMu.Lock()
	d.taskTokens[taskPath] = true
	d.tokenMu.Unlock()
}

// releaseToken gives back the task's global token, if it holds one, and
// revisits the projects that were deferred for the cap, oldest first, one
// after the other so they take the freed token in the order they asked for it.
// Idempotent: a second release for the same task is a no-op.
func (d *Daemon) releaseToken(taskPath string) {
	d.tokenMu.Lock()
	if !d.taskTokens[taskPath] {
		d.tokenMu.Unlock()
		return
	}
	delete(d.taskTokens, taskPath)
	resume := d.capDeferred
	d.capDeferred = nil
	d.tokenMu.Unlock()
	if len(resume) == 0 || (d.ctx != nil && d.ctx.Err() != nil) {
		return
	}
	d.audit.Log("cap_resumed", "deferred", strconv.Itoa(len(resume)))
	go func() {
		for _, p := range resume {
			d.revisitProject(p)
		}
	}()
}

// tokensInUse is the number of global task tokens held.
func (d *Daemon) tokensInUse() int {
	d.tokenMu.Lock()
	defer d.tokenMu.Unlock()
	return len(d.taskTokens)
}

// dropCapDeferred forgets a project's cap deferral (`:stop`, X4).
func (d *Daemon) dropCapDeferred(projectPath string) {
	d.tokenMu.Lock()
	defer d.tokenMu.Unlock()
	d.capDeferred = slices.DeleteFunc(d.capDeferred, func(p string) bool { return p == projectPath })
}

// admittingNames returns the project's tasks that schedule marked running and
// whose launch has not finished yet.
func (d *Daemon) admittingNames(projectPath string) []string {
	d.tokenMu.Lock()
	defer d.tokenMu.Unlock()
	var out []string
	for name := range d.admitting[projectPath] {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (d *Daemon) endAdmitting(projectPath, taskName string) {
	d.tokenMu.Lock()
	defer d.tokenMu.Unlock()
	delete(d.admitting[projectPath], taskName)
	if len(d.admitting[projectPath]) == 0 {
		delete(d.admitting, projectPath)
	}
}

// schedNote is what schedule remembers about a project between passes, so the
// deferral audit lines are written once per situation rather than on every
// pass, and so the snapshot can say why ready tasks did not start
// (live.deferred).
type schedNote struct {
	waiting map[string]bool   // "task>dep" pairs logged as task_waiting_integration
	overlap map[string]string // task → overlaps logged as task_deferred_overlap
	capped  bool              // task_deferred_cap logged for the current deferral
	// deferred is the last pass's overlap-deferred tasks, sorted.
	deferred []string
}

// DeferredInfo is why ready tasks were not started on the project's last
// scheduling pass (the snapshot's live.deferred, docs/parallel-task-worktrees.md D4).
type DeferredInfo struct {
	// Overlap names the ready tasks held back because they overlap a task in
	// flight (or one admitted or deferred ahead of them).
	Overlap []string `json:"overlap,omitempty"`
	// Cap is true while the project waits for a global task token.
	Cap bool `json:"cap,omitempty"`
	// Power is true while the project waits for AC power.
	Power bool `json:"power,omitempty"`
}

// deferredInfo returns the project's live.deferred, nil when nothing is
// deferred.
func (d *Daemon) deferredInfo(projectPath string) *DeferredInfo {
	info := &DeferredInfo{}
	d.tokenMu.Lock()
	if n := d.schedNotes[projectPath]; n != nil {
		info.Overlap = slices.Clone(n.deferred)
	}
	info.Cap = slices.Contains(d.capDeferred, projectPath)
	d.tokenMu.Unlock()
	d.powerMu.Lock()
	info.Power = d.powerPaused && d.powerDeferred[projectPath]
	d.powerMu.Unlock()
	if len(info.Overlap) == 0 && !info.Cap && !info.Power {
		return nil
	}
	return info
}

// noteLocked returns the project's schedNote. tokenMu must be held.
func (d *Daemon) noteLocked(projectPath string) *schedNote {
	n := d.schedNotes[projectPath]
	if n == nil {
		n = &schedNote{waiting: map[string]bool{}, overlap: map[string]string{}}
		d.schedNotes[projectPath] = n
	}
	return n
}

// legacyInFlightTask returns an in-flight task that works in the shared tree
// (no task branch, D7) and is not merely being admitted, or "". With
// runningOnly it considers only tasks at running, whose agent may be editing
// the shared tree right now. Push tasks never have a branch and are not
// counted.
func (d *Daemon) legacyInFlightTask(projectPath string, inflight []*task.Task, runningOnly bool) string {
	admitting := d.admittingNames(projectPath)
	for _, t := range inflight {
		if t.IsPushTask() || t.HasTaskBranch() || slices.Contains(admitting, t.Name) {
			continue
		}
		if runningOnly && t.Status != task.StatusRunning {
			continue
		}
		return t.Name
	}
	return ""
}

// passLaunches is what one scheduling pass decided to start, launched after
// the scheduling lock is released.
type passLaunches struct {
	p      *project.Project
	commit *task.Task
	tasks  []*task.Task
	// then is a graph-wide transition the pass decided on (completing or
	// blocking the project), run after the lock is released: both can launch
	// an agent (the review agent, the wolf).
	then func()
}

// schedule is one scheduling pass over a project (docs/parallel-task-worktrees.md
// S4), the one place that decides which agents start next: the head of the
// commit queue, if no integration is in flight, then as many runnable tasks
// as the project's limit and the global cap allow. Every entry point that
// picks "the next thing" calls it: the working and idle routing, the
// task and commit session ends, the startup scan, power and cap resume.
//
// It re-reads project.yaml (callers' copies can be stale, notably a session's
// launch-time copy) and the task graph. A project that is not working or idle
// starts nothing, so a blocked project's queue is frozen and its failed
// tasks wait at ready for the unblock; a project with `cleanup: true` drains
// its commit queue but admits no task. It reports whether it handled the
// project: started an agent, completed or blocked it, or deferred it for
// power. The idle routing then skips its PR-watch steps, as it always has
// when a manual task was dispatched.
//
// The decide-and-reserve step runs under the project's scheduling lock, so two
// passes on different session-end goroutines cannot both fill one free slot;
// the reservations are the on-disk `running` mark (markTaskRunning), the
// commit reservation (beginCommit) and the global tokens. The launches, which
// are slow (workspace, `git worktree add`, sandbox), run after it is released,
// concurrently, and the pass waits for them.
func (d *Daemon) schedule(projectPath string, p *project.Project) bool {
	return d.schedulePass(projectPath, p, passOpts{})
}

// passOpts tunes one scheduling pass.
type passOpts struct {
	// prefer names a task to give the first free slot when it is runnable:
	// the task whose failed or conflicted run just ended, so its retry starts
	// straight away as it always has, rather than queueing behind ready tasks
	// that sort before it.
	prefer string
	// complete makes the pass complete a fully committed graph, or block a
	// stuck one, on an idle project too, as a commit session's end always
	// has (an idle project's manual task landing re-arms its PR watch that
	// way). Otherwise only a working project is completed or blocked.
	complete bool
}

// schedulePass is schedule with options.
func (d *Daemon) schedulePass(projectPath string, p *project.Project, o passOpts) bool {
	mu := d.schedLock(projectPath)
	mu.Lock()
	l, handled := d.decidePass(projectPath, p, o)
	mu.Unlock()
	if l == nil {
		return handled
	}
	if l.then != nil {
		l.then()
		return true
	}
	var wg sync.WaitGroup
	launch := func(f func()) {
		if l.commit == nil && len(l.tasks) == 1 {
			f() // the common case: one launch, on this goroutine
			return
		}
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	if l.commit != nil {
		t := l.commit
		launch(func() { d.startCommitAgent(projectPath, l.p, t) })
	}
	for _, t := range l.tasks {
		launch(func() { d.launchTaskAgent(projectPath, l.p, t) })
	}
	wg.Wait()
	return true
}

// decidePass is the locked part of schedulePass. It returns the launches to
// make (nil when there are none) and whether the pass handled the project
// (see schedule).
func (d *Daemon) decidePass(projectPath string, fallback *project.Project, o passOpts) (*passLaunches, bool) {
	if d.deferForPower(projectPath) {
		d.audit.Log("power_deferred", "path", projectPath)
		return nil, true
	}
	p, err := project.Load(projectPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || fallback == nil {
			return nil, false
		}
		d.audit.Log("project_load_error", "path", projectPath, "err", err.Error())
		p = fallback
	}
	working := p.Status == project.StatusWorking
	if !working && p.Status != project.StatusIdle {
		return nil, false
	}
	if kind, busy := d.sessionKind(projectPath); busy {
		// A project or review agent holds the bare slot; task and commit work
		// never overlaps it (S2, X1). Its session end revisits the project.
		d.audit.Log("schedule_wait_main_slot", "path", projectPath, "holder", kind.String())
		return nil, true
	}
	g, ok := d.loadTaskGraph(projectPath, p)
	if !ok {
		return nil, true
	}
	if g.AllCommitted() {
		if (!working && !o.complete) || p.Cleanup {
			return nil, false
		}
		if d.deferToPlanner(projectPath, "complete") {
			return nil, true
		}
		return &passLaunches{then: func() { d.transitionProjectComplete(projectPath, p) }}, true
	}

	l := &passLaunches{p: p}
	inflight := g.InFlight()
	limit := p.MaxParallel()
	if limit > 1 {
		// Concurrency needs every task in a worktree of its own. Without
		// git-backed workspaces, or while a legacy task still works in the
		// shared tree, the project runs one task at a time (D7).
		if !d.worktreesEnabled() || d.legacyInFlightTask(projectPath, inflight, false) != "" ||
			(!d.commitReserved(projectPath) && d.sharedTreeDirty(p)) {
			limit = 1
		}
	}

	// 1. The commit queue: at most one integration per project (S5). A
	// legacy task still at work in the shared tree holds every integration
	// back: a commit agent would commit its half-done edits, or find the
	// shared tree dirty.
	// Nor does one start beside a running archive agent, which works in the
	// same shared tree (X6).
	if !d.commitReserved(projectPath) && d.legacyInFlightTask(projectPath, inflight, true) == "" && !(p.Cleanup && d.cleanupReserved(projectPath)) {
		if head := queueHead(g.Queued(), len(inflight) < limit); head != nil {
			l.commit = head
			if d.runner != nil {
				d.beginCommit(projectPath)
			}
			if head.Status == task.StatusReady {
				inflight = append(inflight, head)
			}
		}
	}

	// 2. Task slots.
	capped := false
	var adm taskgraph.Admission
	if !p.Cleanup {
		slots := limit - len(inflight)
		d.tokenMu.Lock()
		free := d.freeTokensLocked()
		var first []*task.Task
		if o.prefer != "" && slots > 0 && free > 0 {
			if t := g.Task(o.prefer); t != nil && !t.IsPushTask() && isReady(g, t.Name) && !overlapsAny(t, inflight) {
				first = []*task.Task{t}
			}
		}
		rest := slots - len(first)
		adm = g.Runnable(append(slices.Clone(inflight), first...), rest)
		if want := len(first) + len(adm.Admit); want > free && d.runner != nil {
			capped = true
			if len(first) > free {
				first = first[:free]
			}
			adm = g.Runnable(append(slices.Clone(inflight), first...), free-len(first))
		}
		adm.Admit = append(first, adm.Admit...)
		if capped && !slices.Contains(d.capDeferred, projectPath) {
			// Under the same lock as the token count, so a token released
			// right after this pass still finds the project to revisit.
			d.capDeferred = append(d.capDeferred, projectPath)
		}
		if d.runner != nil {
			for _, t := range adm.Admit {
				d.taskTokens[t.Path] = true
				if d.admitting[projectPath] == nil {
					d.admitting[projectPath] = map[string]bool{}
				}
				d.admitting[projectPath][t.Name] = true
			}
		}
		d.tokenMu.Unlock()
		if d.runner != nil {
			for _, t := range adm.Admit {
				if d.markTaskRunning(t) {
					l.tasks = append(l.tasks, t)
					continue
				}
				// Someone else moved it off ready since the graph was loaded.
				d.endAdmitting(projectPath, t.Name)
				d.releaseToken(t.Path)
			}
		} else {
			l.tasks = adm.Admit
		}
	}
	d.noteDeferrals(projectPath, g, adm, capped)

	if l.commit != nil || len(l.tasks) > 0 {
		if d.runner == nil {
			return nil, true // observation-only: nothing is launched
		}
		return l, true
	}

	// 3. Nothing started. Work still in flight makes "nothing ready right
	// now" not a dead end.
	queued := len(g.Queued())
	if len(inflight) > 0 || d.commitReserved(projectPath) || d.liveTaskSessions(projectPath) > 0 || len(d.admittingNames(projectPath)) > 0 || capped {
		d.audit.Log("scheduler_idle", "path", projectPath,
			"in_flight", strconv.Itoa(len(inflight)), "queued", strconv.Itoa(queued),
			"deferred", strconv.Itoa(len(adm.Deferred)))
		return nil, false
	}
	if (!working && !o.complete) || p.Cleanup {
		return nil, false
	}
	if g.Empty() || g.HasPendingSeed() {
		d.audit.Log("no_ready_tasks", "path", projectPath, "total", strconv.Itoa(len(g.Tasks())))
		return nil, false
	}
	// Stuck: nothing ready, nothing in flight, nothing integrating — a failed
	// task with no budget left, a task the agent blocked, or a dependency on
	// one of them. Unless a planner is still writing the graph.
	d.audit.Log("no_ready_tasks", "path", projectPath, "total", strconv.Itoa(len(g.Tasks())))
	if d.deferToPlanner(projectPath, "no_ready_tasks") {
		return nil, true
	}
	reason := stuckReason(g)
	return &passLaunches{then: func() { d.transitionProjectBlocked(projectPath, p, reason) }}, true
}

// queueHead is the task the next integration is for: the head of the commit
// queue q. A push task joins the queue straight from ready and is in flight
// from then on (it is marked running only at launch), so it needs a free task
// slot; without one (slotFree false) it is passed over for the first queued
// task already at success, which holds its slot and frees it by
// integrating.
func queueHead(q []*task.Task, slotFree bool) *task.Task {
	for _, t := range q {
		if t.Status == task.StatusSuccess || slotFree {
			return t
		}
	}
	return nil
}

// isReady reports whether the named task is in g.Ready().
func isReady(g *taskgraph.Graph, name string) bool {
	for _, t := range g.Ready() {
		if t.Name == name {
			return true
		}
	}
	return false
}

// overlapsAny reports whether t overlaps any task in set (H2).
func overlapsAny(t *task.Task, set []*task.Task) bool {
	for _, o := range set {
		if o.Name != t.Name && taskgraph.Overlap(t, o) {
			return true
		}
	}
	return false
}

// stuckReason is the blocked_reason of a graph with nothing ready and nothing
// in flight. It names the tasks that hold it up.
func stuckReason(g *taskgraph.Graph) string {
	var stuck []string
	for _, t := range g.Tasks() {
		if t.Status == task.StatusFailed || t.Status == task.StatusBlocked {
			stuck = append(stuck, fmt.Sprintf("%s (%s)", t.Name, t.Status))
		}
	}
	reason := "no ready tasks remain but project is not fully committed"
	if len(stuck) > 0 {
		reason += ": " + strings.Join(stuck, ", ")
	}
	return reason
}

// noteDeferrals writes the pass's scheduling audit lines, each once per
// situation: task_waiting_integration per (task, dependency at success),
// task_deferred_overlap per task and blocking set, task_deferred_cap once per
// cap deferral.
func (d *Daemon) noteDeferrals(projectPath string, g *taskgraph.Graph, adm taskgraph.Admission, capped bool) {
	tasks := g.Tasks()
	type line struct{ event, task, key, val string }
	var lines []line

	d.tokenMu.Lock()
	n := d.noteLocked(projectPath)
	waiting := map[string]bool{}
	for _, t := range tasks {
		if t.Status != task.StatusReady {
			continue
		}
		for _, dep := range taskgraph.WaitingOnIntegration(t, tasks) {
			k := t.Name + ">" + dep
			waiting[k] = true
			if !n.waiting[k] {
				lines = append(lines, line{"task_waiting_integration", t.Name, "waiting_on", dep})
			}
		}
	}
	n.waiting = waiting
	// A ready task the pass did not get to (its slots ran out first) keeps
	// its overlap note, so the same deferral is not logged again later.
	overlap := map[string]string{}
	for name, v := range n.overlap {
		if t := g.Task(name); t != nil && t.Status == task.StatusReady && !slices.Contains(adm.Admit, t) {
			overlap[name] = v
		}
	}
	n.deferred = nil
	for _, df := range adm.Deferred {
		v := strings.Join(df.Overlaps, ",")
		overlap[df.Task.Name] = v
		n.deferred = append(n.deferred, df.Task.Name)
		if n.overlap[df.Task.Name] != v {
			lines = append(lines, line{"task_deferred_overlap", df.Task.Name, "overlaps", v})
		}
	}
	sort.Strings(n.deferred)
	n.overlap = overlap
	inUse := len(d.taskTokens)
	if capped && !n.capped {
		lines = append(lines, line{"task_deferred_cap", "", "tokens_in_use", strconv.Itoa(inUse)})
	}
	n.capped = capped
	d.tokenMu.Unlock()

	for _, ln := range lines {
		fields := []string{"path", projectPath}
		if ln.task != "" {
			fields = append(fields, "task", ln.task)
		}
		d.audit.Log(ln.event, append(fields, ln.key, ln.val)...)
	}
}

// resumeDrainedCleanup runs at the very end of every task and commit session
// end, after its reservations are released: on a project draining for
// `:cleanup` with nothing left in flight it revisits the project, so the last
// session to finish is what launches the archive agent (X6), whatever it
// routed to (a block included).
func (d *Daemon) resumeDrainedCleanup(projectPath string) {
	if d.ctx != nil && d.ctx.Err() != nil {
		return
	}
	cur, err := project.Load(projectPath)
	if err != nil || !cur.Cleanup || d.projectBusy(projectPath) {
		return
	}
	d.revisitProject(projectPath)
}

// inFlightTasks is the wolf's in-flight list (X3): the tasks at running, by
// name, then the commit queue head first, with the task whose commit agent is
// running (or reserved) marked integrating.
func (d *Daemon) inFlightTasks(projectPath string) []prompts.InFlightTask {
	tasks := loadTasksLenient(filepath.Join(filepath.Dir(projectPath), "tasks"))
	integrating := ""
	d.sessionsMu.Lock()
	if e, ok := d.sessions[commitSessionKey(projectPath)]; ok {
		integrating = e.taskName
	}
	d.sessionsMu.Unlock()
	var out []prompts.InFlightTask
	for _, t := range tasks {
		if t.Status == task.StatusRunning {
			out = append(out, prompts.InFlightTask{Name: t.Name, Status: string(t.Status), Integrating: t.Name == integrating})
		}
	}
	for i, t := range taskgraph.Queued(tasks) {
		if t.Status == task.StatusRunning {
			continue // a push task being published, listed above
		}
		out = append(out, prompts.InFlightTask{Name: t.Name, Status: string(t.Status), QueuePosition: i + 1, Integrating: t.Name == integrating})
	}
	return out
}
