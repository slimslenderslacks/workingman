package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"path/filepath"
	"text/template"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/sbx"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// Data is the shared template input. Per-Kind templates use only the fields
// that apply to their role and ignore the rest.
type Data struct {
	Kind          agent.Kind
	Workspace     string   // absolute path to the workspace root
	ProjectPath   string   // absolute path to project.yaml
	ProjectName   string   // work-stream name: basename of the dir holding project.yaml
	TasksDir      string   // absolute path to the tasks/ directory
	Branch        string   // target branch (also the workspace name)
	TaskPath      string   // for TaskAgent: path to this task's yaml
	TaskName      string   // for TaskAgent: name field of the task
	FailedTasks   []string // for WolfAgent: paths to failed/blocked task yamls
	BlockedReason string   // for WolfAgent: why the project was blocked
	// InFlightTasks, for the WolfAgent, lists the tasks still in flight when
	// it launched: running tasks, then the commit queue head first, with the
	// one whose integration is running marked (docs/parallel-task-worktrees.md
	// X3). They finish on their own; the wolf must not edit them.
	InFlightTasks []InFlightTask

	// BlockedSessionPath is the absolute path to the project's durable
	// blocked-session record (project.BlockedSessionPath) — for WolfAgent,
	// the file it should write/update with its diagnosis.
	BlockedSessionPath string
	// BlockedSessionSummary is the step-by-step summary a prior wolf
	// invocation left in that record, if any — for WolfAgent.
	BlockedSessionSummary string
	// BlockedSessionAttempted is what a prior wolf invocation already tried
	// or ruled out, if any — for WolfAgent.
	BlockedSessionAttempted []string

	Replan         bool     // for PlanningAgent: re-plan the existing tasks (a cron cycle) instead of preserving them
	IntakeFiles    []string // for PlanningAgent: absolute paths to pending intake/*.md files awaiting tasks
	ProjectChanges []string // for PlanningAgent: human-readable project.yaml changes since the last successful plan
	Worktree       string   // for PlanningAgent: absolute host path to the wsp worktree mounted as a second workspace (so source can be read without cloning); empty when no wsp is wired up
	// MaxParallelTasks, for the PlanningAgent, is the project's effective task
	// concurrency limit (project.MaxParallel). Above 1 the prompt asks for the
	// touches/areas parallelism hints (docs/parallel-task-worktrees.md H4); at
	// 1 hints have no effect and the prompt leaves them out.
	MaxParallelTasks int
	// Repos are the project's repos, for the PlanningAgent: a touches glob
	// starts with a repo's DirName (H1).
	Repos []Repo

	// Touches and Areas, for the TaskAgent, are the task's declared
	// parallelism hints (H1). HasTouches tells an explicit `touches: []`
	// ("touches nothing") from no touches: at all.
	HasTouches bool
	Touches    []string
	Areas      []string

	// TaskWorktree, for the TaskAgent and CommitAgent, is the task's own
	// worktree root (<shared>/.worktrees/<task>, docs/parallel-task-worktrees.md
	// W2), holding one git worktree per repo on TaskBranch. Empty selects the
	// legacy wording: the task works, and the commit agent commits, in the
	// shared workspace on Branch.
	TaskWorktree string
	// TaskBranch is the task's branch, wm/<Branch>/<task> (W3).
	TaskBranch string
	// TaskBase is the per-repo commit TaskBranch started from (the task file's
	// base:), which the task agent diffs against to see what earlier attempts
	// left and the commit agent compares with to find repos with work.
	TaskBase []RepoCommit
	// Attempt, for the TaskAgent, is the task's attempts count: 0 on the first
	// run, n on the nth automatic retry. A retry with a TaskWorktree runs in the
	// worktree the previous attempt left, so the prompt says so.
	Attempt int
	// Resumed, for the TaskAgent, is true when the daemon reused an existing
	// worktree for this run (a stop/start, a restart, a wolf reset), so it may
	// hold an earlier run's changes even when Attempt is 0.
	Resumed bool
	// Integration, for the TaskAgent, is set when this run is a re-run after
	// the commit agent could not integrate the task's branch: a rebase
	// conflict or a failed build check (docs/parallel-task-worktrees.md I4).
	// The prompt tells the agent to rebase onto Branch and resolve it.
	Integration *IntegrationReport
	// Verify, for the CommitAgent, is the project's pre-integration build
	// check command (R1); empty means none. DocsOnly is the daemon's verdict
	// that the task changed documentation only, which skips the check.
	Verify   string
	DocsOnly bool

	// PushBranch, for the CommitAgent, tells it to push the branch after
	// committing instead of leaving the commit local. Set by the daemon for a
	// review-fix task (one carrying a `source:` — it addresses a live PR review
	// comment or check), because the remote PR branch must include the fix for
	// the external reviewer and CI to see it, and so the review agent's later
	// thread-resolve is truthful. False for ordinary tasks, whose commits stay
	// local until the archive agent publishes them at cleanup.
	PushBranch bool

	// Sandboxed, for the WolfAgent, is true when the wolf runs inside an sbx
	// sandbox (the ACP path) rather than directly on the host (a host terminal
	// tab: the --wolf-host escape hatch). A sandboxed wolf can only see the
	// control dir (and the project's wsp worktree, if any) and cannot reach host
	// tooling — sbx, the other agents' sandboxes, osascript — so its template
	// drops the host-only advice and tells it so. For the SleepyHeadAgent it
	// means the same (an ACP sandbox rather than a host terminal tab): the
	// conversation may reach it from a messaging channel as well as the TUI.
	Sandboxed bool

	// The fields below are for the WorkingmanAgent: the orch state it observes.
	// All of these are mounted into its sandbox at these same absolute paths:
	// Roots read-write (it edits project/task files and creates project.md and
	// intake files there, on the human's request), the rest read-only — the
	// directories of SnapshotFile and AuditLog, so their sibling files (the
	// daemon, acp-wrapper and channel logs) are visible too. Workspace is its
	// writable scratch directory. The WolfAgent gets SnapshotFile too, with
	// its directory mounted read-only, for the live integration state it
	// checks before committing on the shared branch.
	Roots        []string // orch roots (--root)
	SnapshotFile string   // the daemon's state snapshot (JSON); empty when publishing is off
	AuditLog     string   // the audit log file; empty when unknown
	SessionsRoot string   // ACP sessions root (<id>/session.json, <id>/stream.log)

	// The fields below are for the SleepyHeadAgent.
	BehindProjects []BehindProject // cron projects that missed ticks, in the order to report them
	// ReplanInstruction says how to trigger a ONE-TIME re-plan of a single
	// behind project (e.g. a command to run or a file edit to make). The
	// launcher decides the mechanism and supplies the text; the template
	// embeds it verbatim.
	ReplanInstruction string
}

// InFlightTask is one task in flight for the wolf's prompt.
type InFlightTask struct {
	Name   string
	Status string // running | success
	// QueuePosition is the task's 1-based place in the commit queue, 0 when
	// it is not queued (a running task).
	QueuePosition int
	// Integrating is true for the task whose commit agent is running.
	Integrating bool
}

// Repo is one of the project's repos as the planning prompt needs it.
type Repo struct {
	DirName string // workspace dir name (workspace.Repo.DirName)
}

// RepoCommit is one repo's commit, as in a task file's base: or commits:.
type RepoCommit struct {
	Repo string // workspace dir name of the repo
	Hash string
}

// IntegrationReport is a task file's integration: block as the task prompt
// needs it (task.Integration).
type IntegrationReport struct {
	Result string // conflict | verify_failed | error | partial
	Repo   string
	Files  []string
	Onto   []RepoCommit
	Output string
	Landed []string
}

// BehindProject is one cron project the SleepyHeadAgent must report on.
type BehindProject struct {
	Name        string // work-stream name
	ProjectPath string // absolute path to the project's project.yaml
	Cron        string // the project's cron spec
	LastFired   string // when the cron last fired (human-readable); empty if never
	MissedCount int    // how many ticks were missed
	MostRecent  string // the most recent missed tick (human-readable)
	Status      string // current project status (idle, working, blocked, ...)
}

// tmplFuncs are the helpers available to every template.
//
// `nameBudget` gives a template the exact per-task-name character budget for a
// work stream: the sandbox name is "<ProjectName>-<task-name>" and must fit
// sbx.MaxNameLen, so a task name may be at most that minus the work stream and
// its hyphen. Templates MUST call this rather than spelling the arithmetic out
// — any prompt that hardcodes the limit is one place for it to drift from what
// taskgraph.Load enforces, and a prompt that quotes a larger number than the
// loader accepts produces names that get silently repaired out from under the
// agent that chose them.
//
// `sub` remains for ordinary arithmetic in templates.
var tmplFuncs = template.FuncMap{
	"sub":        func(a, b int) int { return a - b },
	"nameBudget": sbx.NameBudget,
	"dir":        filepath.Dir,
	"add":        func(a, b int) int { return a + b },
	"join":       filepath.Join,
}

var tmpls = template.Must(template.New("").Funcs(tmplFuncs).ParseFS(templatesFS, "templates/*.tmpl"))

// Render returns the instruction text for the given Kind.
func Render(kind agent.Kind, data Data) (string, error) {
	name := kind.String() + ".tmpl"
	t := tmpls.Lookup(name)
	if t == nil {
		return "", fmt.Errorf("prompts: no template for kind %q (looked for %s)", kind, name)
	}
	data.Kind = kind
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("prompts: render %s: %w", name, err)
	}
	return buf.String(), nil
}
