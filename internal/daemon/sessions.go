package daemon

import (
	"context"
	"path/filepath"
	"sort"
	"time"
)

// SessionStatus is the lifecycle state of a tracked session as the daemon
// sees it. Today every entry in Daemon.sessions is running — the daemon
// removes the entry the moment Wait returns — so this is effectively a
// single-value enum. It is typed so future states (e.g. draining) can be
// added without churning callers.
type SessionStatus string

const (
	SessionStatusRunning SessionStatus = "running"
)

// SessionInfo is the read-only snapshot of one live agent session that the
// TUI's sessions pane renders. Construction goes through ListSessions or
// WatchSessions; callers never build this struct directly.
type SessionInfo struct {
	// ID is the daemon's stable key for the session — the absolute path of
	// the .project.yaml driving it. Used by the TUI to diff snapshots and
	// route clicks.
	ID string
	// AgentName is the agent.Kind string ("project", "planning", "task",
	// "wolf", "commit"). Surfaced verbatim in the sessions pane.
	AgentName string
	// Project is the basename of the dir holding the .project.yaml — the
	// same identifier the projects gallery uses for that project, so the
	// two panes line up visually.
	Project string
	// TmuxTarget is the tmux session name. Clicking the row runs
	// `tmux attach -t <TmuxTarget>` in an external terminal.
	TmuxTarget string
	Status     SessionStatus
	StartedAt  time.Time
}

// ListSessions returns a snapshot of every session the daemon is currently
// tracking, sorted by StartedAt (oldest first) for stable display order.
// Ties are broken by ID so the order is fully deterministic.
func (d *Daemon) ListSessions() []SessionInfo {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	out := make([]SessionInfo, 0, len(d.sessions))
	for key, entry := range d.sessions {
		out = append(out, SessionInfo{
			ID:         key,
			AgentName:  entry.kind.String(),
			Project:    filepath.Base(filepath.Dir(key)),
			TmuxTarget: entry.sess.Name(),
			Status:     SessionStatusRunning,
			StartedAt:  entry.startedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// WatchSessions polls Daemon.sessions on interval and emits a snapshot
// whenever the result differs from the previous one. The first snapshot is
// emitted immediately. The channel closes when ctx is cancelled.
// interval <= 0 falls back to 200ms — short by default because session
// register/end transitions are user-visible state changes the TUI should
// reflect quickly.
//
// The channel is unbuffered: a slow consumer applies backpressure to the
// poller, matching WatchProjects.
func (d *Daemon) WatchSessions(ctx context.Context, interval time.Duration) <-chan []SessionInfo {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	out := make(chan []SessionInfo)
	go func() {
		defer close(out)
		snap := d.ListSessions()
		select {
		case out <- snap:
		case <-ctx.Done():
			return
		}
		prev := snap
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				snap := d.ListSessions()
				if sessionInfoSlicesEqual(prev, snap) {
					continue
				}
				prev = snap
				select {
				case out <- snap:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func sessionInfoSlicesEqual(a, b []SessionInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sessionInfoEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sessionInfoEqual(a, b SessionInfo) bool {
	return a.ID == b.ID &&
		a.AgentName == b.AgentName &&
		a.Project == b.Project &&
		a.TmuxTarget == b.TmuxTarget &&
		a.Status == b.Status &&
		a.StartedAt.Equal(b.StartedAt)
}
