package project

import "fmt"

type Status string

const (
	StatusReady   Status = "ready"
	StatusWorking Status = "working"
	StatusBlocked Status = "blocked"
	StatusDone    Status = "done"
	// StatusReviewing is "done-but-watching": every task has committed, but the
	// project produced (or is expected to produce) a GitHub pull request whose
	// review comments and Actions runs are still live signals. The daemon polls
	// the PR while in this state (see the review agent and the #review schedule),
	// turning unresolved review threads and failed checks into new tasks
	// (→ working), escalating contested comments to the wolf (→ blocked), and
	// reaching done only once the PR is merged or closed.
	StatusReviewing Status = "reviewing"
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
	case StatusReady, StatusWorking, StatusBlocked, StatusDone, StatusReviewing, StatusStopped:
		return true
	}
	return false
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
		candidate := Status(raw)
		if !candidate.Valid() {
			return fmt.Errorf("invalid project status %q", raw)
		}
	}
	*s = Status(raw)
	return nil
}
