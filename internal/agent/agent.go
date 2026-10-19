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
	// the workspace for uncommitted work, makes a final commit and pushes —
	// but only where there is actually something to commit or push — and sets
	// `archive: true` on the project file. It is autonomous (runs under ACP in
	// a sandbox of its own, like the commit agent): leftovers it cannot
	// classify, including build artifacts that want a `.gitignore` rule, are
	// reported in its final message for a human to deal with rather than
	// waited on. New kinds are appended so the iota values of the existing
	// ones don't shift.
	ArchiveAgent
	// ReviewAgent drives the PR-resolution loop for a project in
	// status:reviewing: it polls the project's pull request via the github MCP,
	// turns each unresolved review thread and failed check into a ready task,
	// resolves threads whose fix has landed, escalates contested comments by
	// blocking the project (→ wolf), and flips the project to done once the PR is
	// merged or closed. It is autonomous (runs under `claude --print` via ACP,
	// like planning) so it can carry `--static-mcp github`. Appended so the iota
	// values of the existing kinds don't shift.
	ReviewAgent
	// WorkingmanAgent is the long-lived, always-on observer the daemon starts at
	// boot and keeps running in a sandbox of its own: it answers a human's
	// questions (relayed over messaging channels) about the orch state — which
	// projects are open, which tasks are running/blocked/failed and why, what the
	// wolf is doing, recent audit events. It is autonomous-ACP (no human drives
	// its prompt) but PERSISTENT (its wrapper outlives client disconnects), has no
	// project and no task, and is not part of any project's state machine; it
	// mounts the orch state read-only and cannot change it. Appended so the iota
	// values of the existing kinds don't shift.
	WorkingmanAgent
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
	case ReviewAgent:
		return "review"
	case WorkingmanAgent:
		return "workingman"
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
	case "review":
		return ReviewAgent, true
	case "workingman":
		return WorkingmanAgent, true
	}
	return 0, false
}

// Interactive reports whether this Kind expects a human in the loop. Exactly
// one does: the wolf agent, which asks for guidance when a project is blocked.
// Every other kind is autonomous — it runs one turn and exits without
// prompting. The project agent generates `.project.yaml` from the user's seed
// description and, when the description is insufficient, escalates by blocking
// the project (which summons the wolf) rather than interviewing the user
// itself. The archive agent used to wait for a human to approve a `.gitignore`
// change; it now reports such leftovers instead of blocking on an answer, so it
// is autonomous like the rest and runs in an ACP sandbox.
//
// The runner uses this to pick the right claude flags; the TUI uses it to
// highlight sessions that won't make progress until someone attaches. It does
// NOT decide the launch path on its own: the wolf is interactive but still runs
// as a persistent ACP session when an ACP launcher is configured, unless
// --wolf-host selects the host/tmux escape hatch (see runner.Runner.UsesACP).
func (k Kind) Interactive() bool {
	return k == WolfAgent
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
