package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
	"github.com/slimslenderslacks/work/internal/taskgraph"
)

const (
	// defaultSessionIdleTimeout is the default for Daemon.sessionIdleTimeout:
	// how long a tracked session may stream nothing before the reaper assumes it
	// has wedged mid-turn and terminates it. Generous on purpose — a single long
	// tool call (a multi-minute build or test run) legitimately emits no ACP
	// frames, so the bound must exceed the longest such gap to avoid killing
	// healthy work. It mirrors the TUI watcher's acpTurnIdleTimeout.
	defaultSessionIdleTimeout = 20 * time.Minute

	// sessionDoneGrace is how long after an agent has declared its stage complete
	// (its task/project file reached a status terminal for the agent's kind) the
	// reaper waits for the wrapper to exit on its own before terminating it.
	// Short: once the work is recorded there is no reason for the wrapper to
	// linger, but the normal exit-when-empty path deserves a moment to fire first.
	sessionDoneGrace = 90 * time.Second

	// reapInterval is how often the reaper scans the tracked sessions.
	reapInterval = 30 * time.Second
)

// reapLoop periodically reaps stranded sessions until ctx is cancelled. It runs
// as its own goroutine (started from Run) rather than inside the main select so
// a slow Close — Close blocks up to the agent's term grace — never stalls
// fsnotify event handling.
func (d *Daemon) reapLoop(ctx context.Context) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.reapStrandedSessions()
		}
	}
}

// reapStrandedSessions terminates tracked sessions whose agent has finished (or
// wedged) but whose acp-wrapper has not exited. It is the daemon's backstop for
// the wrapper's exit-when-empty path (and the TUI's turn-scoped idle watchdog)
// failing to bring a session down: a stranded session holds the project's single
// session slot indefinitely, silently stalling the pipeline — e.g. a task that
// wrote status:success never advances to its commit agent. Closing the session
// is, from the daemon's view, identical to a clean exit: it unblocks the
// trackSession wait goroutine, which logs session_ended and runs the onEnd
// callback that dispatches the next stage (commit-after-task, next task, planning
// retry, …).
func (d *Daemon) reapStrandedSessions() {
	type victim struct {
		key     string
		entry   sessionEntry
		verdict reapVerdict
	}
	var victims []victim
	d.sessionsMu.Lock()
	for key, e := range d.sessions {
		if v := d.strandedVerdict(key, e); v.reap {
			victims = append(victims, victim{key: key, entry: e, verdict: v})
		}
	}
	d.sessionsMu.Unlock()

	// Close outside the lock: Close blocks until the child exits (up to the
	// agent term grace) and the wait goroutine it unblocks re-acquires
	// sessionsMu to delete the entry — closing under the lock would deadlock.
	for _, v := range victims {
		// Record every reap with structured fields so the reasons can be
		// aggregated later to diagnose WHY sessions strand (cause=stage_complete
		// points at the wrapper's exit-when-empty path failing; cause=idle_timeout
		// points at a mid-turn agent/stream hang). The trackSession wait goroutine
		// also logs the subsequent session_ended.
		d.audit.Log("session_reaped",
			"key", v.key,
			"name", v.entry.sess.Name(),
			"kind", v.entry.kind.String(),
			"task", v.entry.taskName,
			"cause", v.verdict.cause,
			"idle", v.verdict.idle.Round(time.Second).String(),
			"age", time.Since(v.entry.startedAt).Round(time.Second).String(),
		)
		if err := v.entry.sess.Close(); err != nil {
			d.audit.Log("session_reap_error", "key", v.key, "name", v.entry.sess.Name(), "err", err.Error())
		}
	}
}

// reapVerdict is the outcome of evaluating one session for reaping. cause is a
// stable, greppable category (not free text) so reaps can be aggregated in the
// audit log to understand which failure mode is stranding sessions.
type reapVerdict struct {
	reap  bool
	cause string // "stage_complete" | "idle_timeout"
	idle  time.Duration
}

const (
	// reapCauseStageComplete: the agent recorded a terminal status for its stage
	// but its wrapper kept running past the done-grace — the exit-when-empty path
	// (or its TUI watchdog) failed to bring it down.
	reapCauseStageComplete = "stage_complete"
	// reapCauseIdleTimeout: the session produced no ACP stream activity for
	// longer than the idle bound — a mid-turn hang (e.g. a stalled model stream).
	reapCauseIdleTimeout = "idle_timeout"
)

// strandedVerdict decides whether the session for key should be reaped and why.
// A session is stranded when either its agent has declared its stage complete
// (so any further wrapper lifetime is pure zombie) and a short grace has elapsed,
// or it has produced no ACP stream activity for longer than the idle timeout.
func (d *Daemon) strandedVerdict(key string, e sessionEntry) reapVerdict {
	idle := d.sessionIdle(e)
	if d.stageComplete(key, e) {
		if idle >= sessionDoneGrace {
			return reapVerdict{reap: true, cause: reapCauseStageComplete, idle: idle}
		}
		return reapVerdict{}
	}
	if idle >= d.sessionIdleTimeout {
		return reapVerdict{reap: true, cause: reapCauseIdleTimeout, idle: idle}
	}
	return reapVerdict{}
}

// sessionIdle reports how long since the session last showed activity: the more
// recent of its start time and the last write to its ACP stream log. A session
// whose log does not exist yet (just launched, or never connected) is measured
// from startedAt, so a wrapper that dies before ever streaming is still reaped.
func (d *Daemon) sessionIdle(e sessionEntry) time.Duration {
	last := e.startedAt
	if d.runner != nil {
		if logPath, err := d.runner.SessionLogPath(e.sess.Name()); err == nil {
			if info, err := os.Stat(logPath); err == nil && info.ModTime().After(last) {
				last = info.ModTime()
			}
		}
	}
	return time.Since(last)
}

// stageComplete reports whether the agent for this session has declared its work
// done by advancing the on-disk state to a status terminal for its kind — after
// which a still-running wrapper is stranded, not working. Kinds without a single
// terminal-status signal (project, wolf) return false and are covered only by
// the idle timeout.
func (d *Daemon) stageComplete(key string, e sessionEntry) bool {
	switch e.kind {
	case agent.TaskAgent, agent.CommitAgent:
		if e.taskName == "" {
			return false
		}
		g, err := taskgraph.Load(filepath.Join(filepath.Dir(key), "tasks"))
		if err != nil {
			return false
		}
		t := g.Task(e.taskName)
		if t == nil {
			return false
		}
		if e.kind == agent.CommitAgent {
			// The commit agent's job is to turn success into committed.
			return t.Status == task.StatusCommitted
		}
		// The task agent's turn is over once it records a terminal outcome.
		return t.Status == task.StatusSuccess || t.Status == task.StatusFailed
	case agent.PlanningAgent:
		p, err := project.Load(key)
		if err != nil {
			return false
		}
		// Planning is done once it moves the project off ready (to working, or
		// blocked/done if it gave up).
		return p.Status != project.StatusReady
	default:
		return false
	}
}
