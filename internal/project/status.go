package project

import "fmt"

type Status string

const (
	StatusReady Status = "ready"
	// StatusWorking means the planning agent is running or the task graph has
	// dispatchable work in flight. Not a resting state — the daemon is
	// actively driving it forward.
	StatusWorking Status = "working"
	StatusBlocked Status = "blocked"
	// StatusIdle is the project's one resting state: nothing is currently
	// dispatchable. It covers what used to be two separate statuses:
	//
	//   - plain "done" — every task committed, nothing further expected.
	//   - "reviewing" — every task committed, but the project produced (or is
	//     expected to produce) a GitHub pull request whose review comments and
	//     Actions runs are still live signals.
	//
	// The two are no longer distinguished by status value at all: whether an
	// idle project is also being watched is entirely a function of its data
	// (see Project.WatchingPR — the Review flag and PullRequests' recorded
	// state), the same way "does it wake itself up on a timer" is answered by
	// whether Cron is set rather than by a separate status. The daemon polls a
	// watched PR (see the review agent and the #review schedule), turning
	// unresolved review threads and failed checks into new tasks (→ working),
	// escalating contested comments to the wolf (→ blocked), and settling back
	// to idle-and-not-watching once every PR is merged or closed.
	StatusIdle Status = "idle"
	// StatusStopped is a human-requested pause: `:stop` (see the TUI's
	// requestStop) kills every running agent session for the project and
	// unregisters its cron/#review schedules, then parks it here. Unlike every
	// other status this one is a dead end for the daemon's normal routing — no
	// case in dispatchProject's status switch launches anything for it, and
	// afterTaskSession/afterCommitSession/afterReviewSession refuse to act on a
	// session that ends while it's set (see Daemon.isStopped) so a session killed
	// out from under them can't retry or escalate to blocked. `:start` is the only
	// way out: it restores the status the project was in before the stop (see
	// Project.StoppedFrom) and lets the daemon's ordinary routing resume from
	// there.
	StatusStopped Status = "stopped"
)

func (s Status) Valid() bool {
	switch s {
	case StatusReady, StatusWorking, StatusBlocked, StatusIdle, StatusStopped:
		return true
	}
	return false
}

// legacyStatusAliases maps status values written by an older version of this
// code to their current equivalent, so a project file already on disk from
// before the "idle" collapse still loads instead of erroring out from under
// the user. Both "done" and "reviewing" collapsed into StatusIdle (see
// StatusIdle's doc comment) — which of the two applies to an old file no
// longer matters, since WatchingPR() re-derives it from the file's own
// review/pull_requests fields regardless of which legacy status wrote them.
var legacyStatusAliases = map[string]Status{
	"done":      StatusIdle,
	"reviewing": StatusIdle,
}

func (s *Status) UnmarshalYAML(unmarshal func(any) error) error {
	var raw string
	if err := unmarshal(&raw); err != nil {
		return err
	}
	// An empty status is the "unpopulated" signal — the seed the daemon
	// writes from a new project.md carries a description but no status yet,
	// and the daemon routes it (via Project.Unpopulated) to the project
	// agent. Accept it here so loading a seed doesn't error; only non-empty
	// values are enum-checked.
	if raw != "" {
		if alias, ok := legacyStatusAliases[raw]; ok {
			*s = alias
			return nil
		}
		candidate := Status(raw)
		if !candidate.Valid() {
			return fmt.Errorf("invalid project status %q", raw)
		}
	}
	*s = Status(raw)
	return nil
}
