package daemon

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/slimslenderslacks/work/internal/project"
)

// enforceStopped is dispatchProject's response to observing status:stopped: it
// kills every agent session tracked for path — the bare project slot (the one
// project/planning/task/commit/review agents share) plus the wolf and archive
// slots, which run under keys of their own — and unregisters the project's
// cron and #review schedules. It does not touch the project file: the status
// write that got it here already happened (via requestStop), and there is
// nothing else to persist.
//
// Idempotent, and must stay that way: it runs on every observation of a
// stopped project, including a daemon restart onto one and repeat fsnotify
// events, most of which will find nothing left to stop.
func (d *Daemon) enforceStopped(path string) {
	d.killProjectSessions(path)
	if d.scheduler != nil {
		d.scheduler.Unregister(path)
		d.scheduler.Unregister(reviewPollKey(path))
	}
	d.clearReviewState(path)
}

// killProjectSessions closes every live session keyed under path — the bare
// key itself plus any "path#suffix" key (wolf, archive) — and logs each one.
// Closing is what actually terminates the underlying agent process (SIGTERM
// then SIGKILL for a host process, `tmux kill-window` for a tmux one); the
// session's own tracking goroutine (see trackSession) removes it from
// d.sessions and fires its onEnd callback once the process exits, exactly as
// on a normal completion. Those callbacks guard themselves against a project
// that has since been stopped (see isStopped) so a killed session cannot retry
// or escalate to blocked out from under this.
func (d *Daemon) killProjectSessions(path string) {
	d.sessionsMu.Lock()
	var victims []string
	for key := range d.sessions {
		if key == path || strings.HasPrefix(key, path+"#") {
			victims = append(victims, key)
		}
	}
	entries := make(map[string]sessionEntry, len(victims))
	for _, key := range victims {
		entries[key] = d.sessions[key]
	}
	d.sessionsMu.Unlock()

	for key, entry := range entries {
		if err := entry.sess.Close(); err != nil {
			d.audit.Log("session_close_error", "key", key, "err", err.Error())
			continue
		}
		d.audit.Log("session_stopped", "path", path, "key", key, "kind", entry.kind.String())
	}
}

// isStopped reports whether path's project is currently status:stopped,
// re-reading it from disk rather than trusting a caller's possibly-stale copy.
// A session-end callback (afterTaskSession, afterCommitSession,
// afterReviewSession) uses this to recognize a session enforceStopped just
// killed and bail out instead of treating the abnormal exit as a crash to
// retry or escalate — a project file that has vanished or fails to load is
// reported as not stopped so those callbacks fall through to their existing
// handling.
func (d *Daemon) isStopped(path string) bool {
	p, err := project.Load(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			d.audit.Log("project_load_error", "path", path, "err", err.Error())
		}
		return false
	}
	return p.Status == project.StatusStopped
}
