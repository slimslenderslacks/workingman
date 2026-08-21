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
	// ArchiveAgent cleans a project up before it can be archived: it checks
	// the workspace for uncommitted work, proposes a `.gitignore` edit when
	// the leftovers are build artifacts (waiting for human approval), makes a
	// final commit and pushes — but only where there is actually something to
	// commit or push — and sets `archive: true` on the project file. New
	// kinds are appended so the iota values of the existing ones don't shift.
	ArchiveAgent
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
	case ArchiveAgent:
		return "archive"
	}
	return "unknown"
}

// ParseKind is the inverse of Kind.String(). It is used by the daemon to
// recover a session's Kind from the on-disk record it wrote (session.Session
// carries the string, not the iota) when reconciling session tracking after a
// restart. Returns false for an empty or unrecognized string.
func ParseKind(s string) (Kind, bool) {
	switch s {
	case "project":
		return ProjectAgent, true
	case "planning":
		return PlanningAgent, true
	case "task":
		return TaskAgent, true
	case "wolf":
		return WolfAgent, true
	case "commit":
		return CommitAgent, true
	case "archive":
		return ArchiveAgent, true
	}
	return 0, false
}

// Interactive reports whether this Kind expects a human in the loop. Two kinds
// do: the wolf agent asks for guidance when a project is blocked, and the
// archive agent may have to get a proposed `.gitignore` change approved before
// it commits. The project, planning, task, and commit agents are all
// autonomous — they run under `claude --print`, finish one turn, and exit
// without prompting. The project agent generates `.project.yaml` from the
// user's seed description and, when the description is insufficient, escalates
// by blocking the project (which summons the wolf) rather than interviewing the
// user itself.
//
// The runner uses this to pick the right claude flags; the TUI uses it to
// highlight sessions that won't make progress until someone attaches.
func (k Kind) Interactive() bool {
	return k == WolfAgent || k == ArchiveAgent
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
