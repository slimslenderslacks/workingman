package tui

import "time"

// SessionView is the read-only snapshot of one live agent session that the
// sessions pane renders. The daemon owns the real session state and adapts
// its SessionInfo into this shape; the tui package stays decoupled from
// daemon internals so it can be tested in isolation.
type SessionView struct {
	// ID is the stable key used to track selection across refreshes. The
	// daemon hands back the absolute path of the project's .project.yaml,
	// but the tui treats it as an opaque string.
	ID         string
	AgentName  string
	Project    string
	TmuxTarget string
	Status     string
	StartedAt  time.Time
	// TaskName is the name of the task the agent is operating on. Set for
	// task/commit agents; empty for project/planning/wolf.
	TaskName string
}

// reconcileSelection returns the session ID the pane should keep highlighted
// after a snapshot refresh. If the previously-selected ID still exists, it
// wins; otherwise selection falls back to the first row (or "" when empty).
// Stable selection across refreshes is what makes the live updates feel
// flicker-free — the highlight doesn't jump as the list re-renders.
func reconcileSelection(views []SessionView, prevID string) string {
	if len(views) == 0 {
		return ""
	}
	if prevID != "" {
		for _, v := range views {
			if v.ID == prevID {
				return prevID
			}
		}
	}
	return views[0].ID
}

// moveSelection shifts the current selection by delta rows, clamped to the
// list bounds. An unknown currentID is treated as "before the first row" so
// a single Down keypress lands on row 0.
func moveSelection(views []SessionView, currentID string, delta int) string {
	if len(views) == 0 {
		return ""
	}
	idx := -1
	for i, v := range views {
		if v.ID == currentID {
			idx = i
			break
		}
	}
	if idx < 0 {
		if delta >= 0 {
			return views[0].ID
		}
		return views[len(views)-1].ID
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(views) {
		idx = len(views) - 1
	}
	return views[idx].ID
}
