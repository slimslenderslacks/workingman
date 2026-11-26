package api

import (
	"context"
	"errors"
	"time"
)

// Errors a [Resources] method returns to pick the response status. Wrap them
// (fmt.Errorf("%w: ...", ErrNotFound)) to add the message the client sees.
var (
	// ErrNotFound is a 404: the project, task, session, ... does not exist.
	ErrNotFound = errors.New("not found")
	// ErrUnavailable is a 503: the data's source cannot be read right now
	// (for example `sbx ls` failing).
	ErrUnavailable = errors.New("unavailable")
)

// Resources is the read model behind the resource endpoints of §A.2. Every
// value it returns is marshalled to JSON as is, so it must already be
// redacted. List methods return the items of a list in order; the server
// wraps them in the [List] envelope with the cursor it read before calling
// the method, so the items are at least as new as the cursor.
type Resources interface {
	// Projects lists the project summaries that match q.
	Projects(ctx context.Context, q ProjectQuery) ([]any, error)
	// Project is one project's detail form; id is "p_<root>.<work-stream>".
	Project(ctx context.Context, id string, opts ProjectOptions) (any, error)
	// Tasks lists a project's tasks, ordered by name, keeping those whose
	// status is in status (all when empty).
	Tasks(ctx context.Context, project string, status []string) ([]any, error)
	// Task is one task of a project, by name.
	Task(ctx context.Context, project, name string) (any, error)
	// Graph is a project's task graph as the scheduler sees it.
	Graph(ctx context.Context, project string) (any, error)
	// Intake lists a project's intake requests in state ([IntakeStates];
	// "" or "all" for every one), without their bodies.
	Intake(ctx context.Context, project, state string) ([]any, error)
	// IntakeRequest is one intake request, with its body.
	IntakeRequest(ctx context.Context, project, name string) (any, error)
	// Pulls lists the pull requests a project records.
	Pulls(ctx context.Context, project string) ([]any, error)
	// Audit lists audit events, oldest first.
	Audit(ctx context.Context, q AuditQuery) ([]any, error)
	// Sessions lists the sessions the daemon tracks that match q.
	Sessions(ctx context.Context, q SessionQuery) ([]any, error)
	// Session is one session, by session id (or session-map key).
	Session(ctx context.Context, id string) (any, error)
	// Transcript reads a session's stream log from byte offset after.
	Transcript(ctx context.Context, id string, after int64, limit int) (Transcript, error)
	// Prompt is a session's input prompt files.
	Prompt(ctx context.Context, id string) (any, error)
	// Sandboxes lists the sandboxes `sbx ls` reports, with what is known
	// about each.
	Sandboxes(ctx context.Context, q SandboxQuery) ([]any, error)
	// Messages lists the post-office messages for the operator.
	Messages(ctx context.Context, q MessageQuery) ([]any, error)
	// Agents lists the daemon-owned agents (workingman, sleepy-head,
	// step-up) and their state.
	Agents(ctx context.Context) ([]any, error)
}

// Hidden filter values for [ProjectQuery.Hidden].
const (
	HiddenFalse = "false"
	HiddenTrue  = "true"
	HiddenAny   = "any"
)

// ProjectQuery is GET /projects' filter.
type ProjectQuery struct {
	// Status keeps projects whose status is listed (all when empty).
	Status []string
	// Hidden is HiddenFalse (the default, like the TUI gallery), HiddenTrue
	// or HiddenAny.
	Hidden string
	// Root keeps the projects of one root, by index; -1 for every root.
	Root int
	// Q keeps projects whose name or description contains it, ignoring case.
	Q string
}

// ProjectOptions are GET /projects/{id}'s options.
type ProjectOptions struct {
	// Fingerprint adds each pull request's review fingerprint
	// (?include=fingerprint).
	Fingerprint bool
}

// Intake request states.
const (
	IntakePending   = "pending"
	IntakeEmpty     = "empty"
	IntakeProcessed = "processed"
)

// IntakeStates are the values ?state= accepts on the intake list, besides
// "all".
var IntakeStates = []string{IntakePending, IntakeEmpty, IntakeProcessed}

// MaxAuditLimit caps ?limit= on the audit endpoints: the size of the
// daemon's in-memory audit ring.
const MaxAuditLimit = 500

// DefaultAuditLimit is the audit endpoints' ?limit= when none is given.
const DefaultAuditLimit = 100

// AuditQuery is the audit endpoints' filter.
type AuditQuery struct {
	// Since, when HasSince, keeps events with seq > Since, and the first
	// Limit of them are returned. Without it the last Limit are.
	Since    uint64
	HasSince bool
	// Limit is how many events to return, 1..MaxAuditLimit.
	Limit int
	// Events keeps the named events (all when empty).
	Events []string
	// Project keeps the events whose path or key field lies under the
	// project's directory; "" for every event.
	Project string
}

// SessionQuery is GET /sessions' filter.
type SessionQuery struct {
	// Project keeps a project's sessions ("" for all).
	Project string
	// Kind keeps sessions of the listed agent kinds (all when empty).
	Kind []string
	// Live, when set, keeps sessions whose sandbox `sbx ls` reports (true)
	// or does not (false).
	Live *bool
}

// SandboxQuery is GET /sandboxes' filter.
type SandboxQuery struct {
	// Retained keeps only sandboxes workingman made that no session uses.
	Retained bool
}

// Message states for [MessageQuery.State].
const (
	MessagesOpen   = "open"
	MessagesClosed = "closed"
	MessagesAll    = "all"
)

// MessageQuery is GET /messages' filter.
type MessageQuery struct {
	// State is MessagesOpen (the default), MessagesClosed or MessagesAll.
	State string
	// Since keeps closed messages resolved at or after it (zero: all that
	// are kept).
	Since time.Time
}

// MaxTranscriptLimit caps ?limit= on a transcript, in frames.
const MaxTranscriptLimit = 5000

// DefaultTranscriptLimit is a transcript's ?limit= when none is given.
const DefaultTranscriptLimit = 1000

// Transcript is a page of a session's stream log: whole frames, each one
// line of JSON without its newline, starting at byte offset After. Next is
// the offset to ask for to continue, and Size the log's size when it was
// read.
type Transcript struct {
	Frames [][]byte
	After  int64
	Next   int64
	Size   int64
}
