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
)

func (s Status) Valid() bool {
	switch s {
	case StatusReady, StatusWorking, StatusBlocked, StatusDone, StatusReviewing:
		return true
	}
	return false
}

func (s *Status) UnmarshalYAML(unmarshal func(any) error) error {
	var raw string
	if err := unmarshal(&raw); err != nil {
		return err
	}
	// An empty status is the "unpopulated" signal — a `:new` seed carries a
	// description but no status yet, and the daemon routes it (via
	// Project.Unpopulated) to the project agent. Accept it here so loading a
	// seed doesn't error; only non-empty values are enum-checked.
	if raw != "" {
		candidate := Status(raw)
		if !candidate.Valid() {
			return fmt.Errorf("invalid project status %q", raw)
		}
	}
	*s = Status(raw)
	return nil
}
