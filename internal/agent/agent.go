// Package agent owns the process model for the orchestrator's claude-code
// sessions: an Agent has a Kind, a Spec describes how to launch one, and a
// Session is the live handle the daemon holds until the work finishes.
package agent

import "context"

// Kind identifies which orchestrator role a session is playing. The Launcher
// itself doesn't dispatch on Kind — it just runs whatever Command the Spec
// carries — but the daemon uses Kind to pick a Spec and tests use it to log.
type Kind int

const (
	ProjectAgent Kind = iota
	PlanningAgent
	TaskAgent
	WolfAgent
	CommitAgent
)

func (k Kind) String() string {
	switch k {
	case ProjectAgent:
		return "project"
	case PlanningAgent:
		return "planning"
	case TaskAgent:
		return "task"
	case WolfAgent:
		return "wolf"
	case CommitAgent:
		return "commit"
	}
	return "unknown"
}

// Spec is the minimum a Launcher needs to start a session. Command is
// deliberately generic so step-3 tests can use `sleep` and later steps can
// pass the real `claude --dangerously-skip-permissions ...` invocation
// without changing the Launcher.
type Spec struct {
	Kind      Kind
	Name      string   // session/tmux name — must be unique per live session
	Workspace string   // working directory for the command
	Command   []string // command + args
}

// Session is the running-agent handle. Wait blocks until the underlying
// process exits (or ctx is done). Close terminates the session and is
// idempotent.
type Session interface {
	Name() string
	Wait(ctx context.Context) error
	Close() error
}

type Launcher interface {
	Launch(ctx context.Context, spec Spec) (Session, error)
}
