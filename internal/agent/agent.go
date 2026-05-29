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

// Interactive reports whether this Kind expects a human in the loop. Every
// kind now runs claude without `--print`: the project agent interviews the
// user to fill in `.project.yaml`, the wolf agent asks for guidance when a
// project is blocked, and the planning / task / commit agents stream their
// output into the tmux window and stay at the prompt afterwards so a human
// can read what they did and respond if needed. The session ends only when
// the human closes the tmux window, at which point the daemon's onEnd
// callback advances the project's state machine to the next phase.
//
// The TUI uses this flag to highlight sessions waiting for input. With all
// kinds now interactive, every session row carries that highlight — the
// badge has become a "this session is alive" indicator rather than a
// distinguishing marker.
func (k Kind) Interactive() bool {
	return true
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
