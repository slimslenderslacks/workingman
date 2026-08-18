package daemon

import (
	"context"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
)

// reconcilePollInterval bounds how often a reconciledSession re-checks the
// on-disk record for the session it was adopted from. It only needs to be
// coarse enough to eventually notice the acp-wrapper removed the session
// directory on exit — the TUI's acpwatch package is what actually reconnects
// to the live ACP stream, on its own, much shorter interval.
const reconcilePollInterval = 3 * time.Second

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
		sess := newReconciledSession(rec.ID, store)
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

// reconciledSession is the agent.Session the daemon tracks for an ACP-backed
// agent it adopted from disk rather than launched itself. There is no host
// process handle to wait on or signal here — the acp-wrapper process (and its
// sandbox) belongs to whichever orch process actually launched it, which is
// the entire point: this daemon must leave it running.
type reconciledSession struct {
	id    string
	store session.Store
}

func newReconciledSession(id string, store session.Store) *reconciledSession {
	return &reconciledSession{id: id, store: store}
}

func (s *reconciledSession) Name() string { return s.id }

// Wait blocks until the session's on-disk record disappears (acp-wrapper
// removes it on exit; see internal/acpwrapper) or ctx is cancelled. It never
// closes the session on ctx cancellation — unlike processSession, adopting a
// session must not carry an implicit "tear it down on shutdown" behavior.
func (s *reconciledSession) Wait(ctx context.Context) error {
	t := time.NewTicker(reconcilePollInterval)
	defer t.Stop()
	for {
		if _, err := s.store.Read(s.id); err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Close cannot reach into a session this daemon never launched — there is no
// process handle to signal. It only exists to satisfy agent.Session; callers
// that want to stop an adopted session's local tracking should drop it from
// d.sessions directly, exactly like the shutdown/detach path does.
func (s *reconciledSession) Close() error { return nil }
