package daemon

import (
	"context"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
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
		if rec.ProjectPath == "" {
			continue
		}
		kind, ok := agent.ParseKind(rec.Kind)
		if !ok {
			continue
		}

		projectPath := rec.ProjectPath
		sess := newReconciledSession(rec.ID, store, nil)
		onEnd := func(error) { d.revisitProject(projectPath) }
		if d.trackSession(projectPath, sess, kind, taskNameFor(rec.TaskPath), onEnd) {
			d.audit.Log("session_reconciled", "key", projectPath, "kind", rec.Kind, "id", rec.ID)
		}
	}
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
