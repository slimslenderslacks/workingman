package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
)

// reconcilePollInterval bounds how often a reconciledSession re-checks the
// on-disk record for the session it was adopted from. It only needs to be
// coarse enough to eventually notice the acp-wrapper removed the session
// directory on exit — the TUI's acpwatch package is what actually reconnects
// to the live ACP stream, on its own, much shorter interval.
const reconcilePollInterval = 3 * time.Second

// reconcileStartingGrace bounds how long an adopted session may sit in
// session.StatusStarting before its sandbox is probed and, if confirmed gone,
// reaped. It mirrors acpStartingGracePeriod in internal/tui/acpwatch.go: the
// same cold-sandbox-boot window that makes a fresh StatusStarting record
// unprobeable-by-design there applies here too, since this daemon may adopt a
// session an acp-wrapper process (which it did not itself launch, and so
// cannot distinguish from a genuinely stuck one) wrote only moments ago.
const reconcileStartingGrace = 2 * time.Minute

// reconcileSessions loads whatever ACP sessions are still on disk (written by
// acp-wrapper processes this daemon did not itself launch — typically because
// they belong to a prior orch process that has since restarted) and adopts
// the still-running ones into d.sessions. Without this, a fresh daemon has no
// idea those sessions exist until something else notices: it would happily
// dispatch a duplicate agent for the same project, and the TUI's sessions
// pane would show nothing until the agent's own session ended.
//
// Only sessions whose Kind and ProjectPath were recorded are adoptable — both
// fields are populated by every ACP launch (see runner.startACP), so their
// absence means either a pre-upgrade session.json or a non-ACP entry that
// isn't ours to track here.
func (d *Daemon) reconcileSessions() {
	if d.runner == nil {
		return
	}
	root, err := d.runner.ResolveSessionsRoot()
	if err != nil {
		d.audit.Log("session_reconcile_error", "err", err.Error())
		return
	}
	store, err := session.NewStore(root)
	if err != nil {
		d.audit.Log("session_reconcile_error", "err", err.Error())
		return
	}
	recs, err := store.List()
	if err != nil {
		d.audit.Log("session_reconcile_error", "err", err.Error())
		return
	}

	for _, rec := range recs {
		if rec.Status != session.StatusRunning && rec.Status != session.StatusStarting {
			continue
		}
		kind, ok := agent.ParseKind(rec.Kind)
		if !ok {
			continue
		}
		if kind == agent.WorkingmanAgent {
			// Project-independent: adopt it only if this daemon supervises one, and
			// under its own key. The supervisor is told when it ends, like a launch.
			if d.workingman == nil {
				continue
			}
			sess := newReconciledSession(rec.ID, store, nil)
			if d.trackConfiguredSession(workingmanSessionKey, sess, kind, "", d.reconciledConfig(rec, kind), func(error) { d.workingman.signalEnded() }) {
				d.audit.Log("session_reconciled", "key", workingmanSessionKey, "kind", rec.Kind, "id", rec.ID)
			}
			continue
		}
		if kind == agent.StepUpAgent {
			// Like the workingman agent: adopted only by a daemon that supervises
			// one, under its fixed key, with the supervisor told when it ends.
			if d.stepUp == nil {
				continue
			}
			sess := newReconciledSession(rec.ID, store, nil)
			if d.trackConfiguredSession(stepUpSessionKey, sess, kind, "", d.reconciledConfig(rec, kind), func(error) { d.stepUp.signalEnded() }) {
				d.audit.Log("session_reconciled", "key", stepUpSessionKey, "kind", rec.Kind, "id", rec.ID)
				poke(d.stepUp.started)
			}
			continue
		}
		if kind == agent.SleepyHeadAgent {
			// Project-independent and not detachable, so a live one is a leftover
			// from a daemon that died without closing it. Adopt it under its key
			// so launchSleepyHead doesn't start a second wrapper on the same
			// session id beside it.
			sess := newReconciledSession(rec.ID, store, nil)
			if d.trackConfiguredSession(sleepyHeadSessionKey, sess, kind, "", d.reconciledConfig(rec, kind), func(error) { d.audit.Log("sleepy_head_ended") }) {
				d.audit.Log("session_reconciled", "key", sleepyHeadSessionKey, "kind", rec.Kind, "id", rec.ID)
			}
			continue
		}
		if rec.ProjectPath == "" {
			continue
		}

		projectPath := project.CanonicalPath(rec.ProjectPath)
		sess := newReconciledSession(rec.ID, store, nil)
		onEnd := func(error) { d.revisitProject(projectPath) }
		// Every agent but the project and review agents lives in a slot of its
		// own (docs/parallel-task-worktrees.md S2, X7): each task agent in
		// #task:<name>, the commit agent in #commit, and the planner, the wolf
		// and the archive agent in theirs. Adopting one under the bare project
		// path would both occupy that slot and let a second one launch beside
		// it — and two adopted task sessions of one project would collide.
		key := projectPath
		taskPath := rec.TaskPath
		switch kind {
		case agent.TaskAgent:
			name := taskNameFor(taskPath)
			if name == "" {
				d.audit.Log("session_reconcile_error", "id", rec.ID, "kind", rec.Kind, "err", "task file unreadable: "+taskPath)
				continue
			}
			key = taskSessionKey(projectPath, name)
			// The adopted session ends the way a live one does
			// (afterTaskSession), routing on what its task file says; that
			// also releases the global token re-taken here.
			d.takeToken(taskPath)
			onEnd = func(error) {
				d.afterAdoptedTaskSession(projectPath, taskPath)
				d.resumeDrainedCleanup(projectPath)
			}
		case agent.CommitAgent:
			key = commitSessionKey(projectPath)
			if d.commitReserved(projectPath) {
				// A second live commit session for one project points at an
				// earlier crash bug; its task is left to integration recovery.
				d.audit.Log("session_reconcile_error", "id", rec.ID, "kind", rec.Kind, "path", projectPath, "err", "second commit session for the project")
				continue
			}
			d.beginCommit(projectPath, taskNameFor(taskPath))
			onEnd = func(error) {
				d.afterAdoptedCommitSession(projectPath, taskPath)
				d.resumeDrainedCleanup(projectPath)
			}
		case agent.PlanningAgent:
			// Planning has a slot of its own too (see launchPlanningAgent), and
			// its dedup is the planning reservation, which died with the previous
			// daemon. Re-take it — offering whatever intake is pending, since what
			// the adopted run was actually handed is unknown — and adopt the end
			// callback that releases it and runs the circuit breaker.
			if !d.beginPlanning(projectPath, pendingIntakeFiles(filepath.Dir(projectPath))) {
				continue
			}
			key = planningSessionKey(projectPath)
			onEnd = func(waitErr error) { d.afterPlanningSession(projectPath, waitErr) }
		case agent.WolfAgent:
			key = wolfSessionKey(projectPath)
		case agent.ArchiveAgent:
			// The archive agent's dedup is the in-flight reservation, not its
			// session key (see beginCleanup), and the reservation died with the
			// previous daemon. Re-take it, or the first project event after this
			// restart would read the still-set `cleanup: true` and dispatch a
			// second archive agent beside the adopted one. Adopt the end callback
			// too, so the flag is cleared when the adopted run finishes.
			if !d.beginCleanup(projectPath) {
				continue
			}
			key = archiveSessionKey(projectPath)
			onEnd = func(waitErr error) { d.afterArchiveSession(projectPath, waitErr) }
		}
		if d.trackConfiguredSession(key, sess, kind, taskNameFor(rec.TaskPath), d.reconciledConfig(rec, kind), onEnd) {
			d.audit.Log("session_reconciled", "key", key, "kind", rec.Kind, "id", rec.ID)
		} else if kind == agent.TaskAgent {
			// Already tracked: give back the token taken for it.
			d.releaseToken(taskPath)
		} else if kind == agent.CommitAgent {
			d.endCommit(projectPath)
		} else if kind == agent.ArchiveAgent {
			// Already tracked, so nothing will run afterArchiveSession for this
			// reservation — release the one we just took.
			d.endCleanup(projectPath)
		} else if kind == agent.PlanningAgent {
			// Same for the planning reservation.
			d.endPlanning(projectPath)
		}
	}
}

// afterAdoptedTaskSession ends a task session adopted from a previous daemon
// the way a live one ends (X7 part 2): a task the agent left at the running
// the daemon wrote at dispatch is first put back to ready, the agent having
// died without writing anything — the pre-restart "re-run it, without
// spending an attempt" behaviour — and then afterTaskSession routes on the
// task file, as it would have live: success is stamped and queued, failed is
// retried or blocks. The callback's project copy is re-read from disk.
func (d *Daemon) afterAdoptedTaskSession(projectPath, taskPath string) {
	if taskPath == "" {
		d.releaseToken(taskPath)
		d.revisitProject(projectPath)
		return
	}
	d.releaseRunningTask(taskPath, "adopted_session_ended")
	p, err := project.Load(projectPath)
	if err != nil {
		d.releaseToken(taskPath)
		return
	}
	t, err := task.Load(taskPath)
	if err == nil && t.Status == task.StatusReady {
		// Released above: no outcome to judge; just give it its slot back.
		d.releaseToken(taskPath)
		d.revisitProject(projectPath)
		return
	}
	d.afterTaskSession(projectPath, taskPath, p)
}

// afterAdoptedCommitSession ends a commit session adopted from a previous
// daemon the way a live one ends: afterCommitSession verifies and cleans up a
// committed task and routes anything else on the integration report. The
// launch-time state it would have had (integrationLaunch) is gone, so it is
// rebuilt as "unknown": a fresh launch re-derives it.
func (d *Daemon) afterAdoptedCommitSession(projectPath, taskPath string) {
	var once sync.Once
	release := func() { once.Do(func() { d.endCommit(projectPath) }) }
	defer release()
	if taskPath == "" {
		release()
		d.revisitProject(projectPath)
		return
	}
	// A push task the daemon marked running is put back, as before.
	d.releaseRunningTask(taskPath, "adopted_session_ended")
	p, err := project.Load(projectPath)
	if err != nil {
		return
	}
	t, err := task.Load(taskPath)
	if err != nil {
		release()
		d.revisitProject(projectPath)
		return
	}
	var il *integrationLaunch
	if t.HasTaskBranch() && !t.IsPushTask() && d.worktreesEnabled() {
		il = &integrationLaunch{verify: p.Verify}
		if t.Integration != nil {
			il.attempts = t.Integration.Attempts
		}
	}
	if t.Status == task.StatusReady {
		release()
		d.revisitProject(projectPath)
		return
	}
	d.afterCommitSession(projectPath, taskPath, p, "", il, release)
}

// taskNameFor loads the task's Name field from its on-disk file for display
// (SessionInfo.TaskName). Empty for planning/project sessions (taskPath is
// empty) and best-effort otherwise: a task file that no longer exists or
// fails to parse just leaves the reconciled entry's TaskName blank.
func taskNameFor(taskPath string) string {
	if taskPath == "" {
		return ""
	}
	t, err := task.Load(taskPath)
	if err != nil {
		return ""
	}
	return t.Name
}

// reconciledConfig rebuilds the launch config of an adopted ACP session from
// what is still on disk: the template and mounts its session.json recorded,
// and — for task and commit agents — the model and static_mcps of its task
// file. The MCP servers are re-derived the way the daemon picks them at
// launch (the review agent's github-only set, else the task's static_mcps,
// else the runner's defaults), since session.json does not record them.
func (d *Daemon) reconciledConfig(rec session.Session, kind agent.Kind) runner.SessionConfig {
	h, ok := d.runner.DefaultHarness()
	if !ok {
		h = runner.Harness{Name: rec.Harness}
	}
	plan := runner.Plan{Kind: kind}
	if kind == agent.ReviewAgent {
		plan.StaticMCPs = reviewStaticMCPs
	}
	if rec.TaskPath != "" {
		if t, err := task.Load(rec.TaskPath); err == nil {
			plan.Model = t.Model
			plan.StaticMCPs = t.StaticMCPs
		}
	}
	cfg := d.runner.ACPSessionConfig(h, kind, rec.Workspaces, d.runner.MCPServersFor(plan), d.runner.MCPGatewayDynamic(kind), plan.Model)
	if rec.Template != "" {
		cfg.Harness = rec.Template
	}
	return cfg
}

// reconciledSandboxProbe reports whether the named sbx sandbox still exists.
// Production uses sbx.Alive; tests inject a fake so they never shell out to a
// real sbx binary. The nil-error contract matches sbx.Alive: alive=false with
// a nil error means authoritatively gone, a non-nil error means the query was
// inconclusive.
type reconciledSandboxProbe func(ctx context.Context, name string) (bool, error)

// reconciledSession is the agent.Session the daemon tracks for an ACP-backed
// agent it adopted from disk rather than launched itself. There is no host
// process handle to wait on or signal here — the acp-wrapper process (and its
// sandbox) belongs to whichever orch process actually launched it, which is
// the entire point: this daemon must leave it running unless its sandbox
// turns out to be gone (see Wait).
type reconciledSession struct {
	id    string
	store session.Store
	probe reconciledSandboxProbe
}

// newReconciledSession returns a reconciledSession for id. probe is the
// sandbox-liveness check Wait uses; a nil probe defaults to sbx.Alive (tests
// pass a fake instead).
func newReconciledSession(id string, store session.Store, probe reconciledSandboxProbe) *reconciledSession {
	if probe == nil {
		probe = sbx.Alive
	}
	return &reconciledSession{id: id, store: store, probe: probe}
}

func (s *reconciledSession) Name() string { return s.id }

// Wait blocks until the session's on-disk record disappears (acp-wrapper
// removes it on exit; see internal/acpwrapper), its backing sandbox is
// confirmed gone, or ctx is cancelled. It never closes the session on ctx
// cancellation — unlike processSession, adopting a session must not carry an
// implicit "tear it down on shutdown" behavior.
//
// A session adopted from disk has no live acp-wrapper process this daemon
// launched to notice its own sandbox died and remove the directory — that is
// exactly the process that owned it, and it may have crashed. Without this
// check an adopted session whose sandbox is gone would poll s.store.Read
// forever, since nothing else will ever remove the record. Each tick,
// sandboxDead re-reads the record fresh (status/sandbox name can change
// between ticks — notably StatusStarting flipping to StatusRunning) and
// decides based on the latest values.
func (s *reconciledSession) Wait(ctx context.Context) error {
	t := time.NewTicker(reconcilePollInterval)
	defer t.Stop()
	for {
		rec, err := s.store.Read(s.id)
		if err != nil {
			return nil
		}
		if s.sandboxDead(ctx, rec) {
			_ = s.store.Remove(s.id)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// sandboxDead reports whether rec's backing sandbox is confirmed gone.
//
// A StatusStarting record within reconcileStartingGrace of its CreatedAt is
// never treated as dead: the wrapper writes StatusStarting before it ever
// creates the sandbox (see acpwrapper.Run), so a brand-new adopted session
// legitimately has no sandbox yet. Past the grace period, a record with no
// SandboxName at all can never be confirmed alive and is stale by definition;
// otherwise the sandbox is probed, and only an authoritative "gone"
// (alive=false, err=nil) counts — an inconclusive probe (err != nil) must
// never reap a session that might still be legitimately running.
func (s *reconciledSession) sandboxDead(ctx context.Context, rec session.Session) bool {
	if rec.Status == session.StatusStarting && time.Since(rec.CreatedAt) <= reconcileStartingGrace {
		return false
	}
	if rec.SandboxName == "" {
		return true
	}
	alive, err := s.probe(ctx, rec.SandboxName)
	return err == nil && !alive
}

// Close cannot reach into a session this daemon never launched — there is no
// process handle to signal. It only exists to satisfy agent.Session; callers
// that want to stop an adopted session's local tracking should drop it from
// d.sessions directly, exactly like the shutdown/detach path does.
func (s *reconciledSession) Close() error { return nil }

// resumeOrphanedOutcomes replays, once per project at startup, what the
// session-end callbacks of sessions that ended while no daemon was watching
// would have done (docs/parallel-task-worktrees.md X7 part 3). It runs after
// releaseOrphanedTasks and only touches tasks no adopted session is working:
//
//   - success with no succeeded_at and a task branch: stamped from the task
//     file's mtime, so the queue roughly follows finish order. A legacy task
//     keeps the "sorts first" rule.
//   - failed with integration.result conflict or verify_failed: the conflict
//     re-run path (attempts++, back to ready, or block when spent).
//   - failed otherwise: handleTaskFailure's retry or block.
//   - blocked: the block the task agent asked for.
//   - committed with its task branch's worktree still set (the cleanup never
//     ran): the afterCommitSession verification and W6 cleanup, and
//     completed_at if missing.
//
// None of these starts an agent; the scheduling pass that follows does. A
// failed or blocked task on a project that is not working or idle (blocked
// already, stopped) is left for the wolf or `:start`.
func (d *Daemon) resumeOrphanedOutcomes(projectPath string) {
	p, err := project.Load(projectPath)
	if err != nil || (p.Status != project.StatusWorking && p.Status != project.StatusIdle) {
		return
	}
	for _, t := range loadTasksLenient(filepath.Join(filepath.Dir(projectPath), "tasks")) {
		if d.taskSessionLive(projectPath, t.Name) {
			continue
		}
		switch t.Status {
		case task.StatusSuccess:
			if t.SucceededAt != nil || !t.HasTaskBranch() || t.IsPushTask() {
				continue
			}
			at := d.clock()
			if info, err := os.Stat(t.Path); err == nil {
				at = info.ModTime()
			}
			if err := task.Update(t.Path, func(cur *task.Task) bool {
				if cur.Status != task.StatusSuccess || cur.SucceededAt != nil {
					return false
				}
				cur.SucceededAt = &at
				return true
			}); err != nil {
				d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
				continue
			}
			d.audit.Log("outcome_resumed", "path", projectPath, "task", t.Name, "status", string(t.Status), "action", "succeeded_at")
		case task.StatusFailed:
			if t.Integration != nil && (t.Integration.Result == integrationConflict || t.Integration.Result == integrationVerifyFailed) {
				d.audit.Log("outcome_resumed", "path", projectPath, "task", t.Name, "status", string(t.Status), "action", "conflict_rerun")
				if !d.requeueConflict(projectPath, p, t) {
					return // blocked
				}
				continue
			}
			d.audit.Log("outcome_resumed", "path", projectPath, "task", t.Name, "status", string(t.Status), "action", "retry")
			if !d.resetForRetry(projectPath, t.Path, p, t) {
				return
			}
		case task.StatusBlocked:
			d.audit.Log("outcome_resumed", "path", projectPath, "task", t.Name, "status", string(t.Status), "action", "block")
			d.retainSandbox(t.Path, t)
			d.transitionProjectBlocked(projectPath, p,
				fmt.Sprintf("task %q reported blocked: %s", t.Name, t.BlockedReason))
			return
		case task.StatusCommitted:
			if !t.HasTaskBranch() || t.Worktree == "" || !d.worktreesEnabled() {
				continue
			}
			d.audit.Log("outcome_resumed", "path", projectPath, "task", t.Name, "status", string(t.Status), "action", "finish")
			if !d.finishCommittedTask(projectPath, p, t) {
				return
			}
			if t.CompletedAt == nil {
				now := d.clock()
				if err := task.Update(t.Path, func(cur *task.Task) bool {
					if cur.CompletedAt != nil {
						return false
					}
					cur.CompletedAt = &now
					return true
				}); err != nil {
					d.audit.Log("task_save_error", "task", t.Path, "err", err.Error())
				}
			}
		}
	}
}
