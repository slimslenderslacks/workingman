// Package prompts renders the per-Kind instruction text the orchestrator
// gives a freshly launched claude session.
//
// Each agent receives two things in its workspace:
//
//  1. .orch/context.yaml — structured data (paths, branch, current task, ...)
//  2. .orch/instructions.md — rendered from the templates here
//
// The initial message piped to claude is short and just points it at those
// two files. The templates therefore carry the role-specific *behaviour*
// (what to do, what files to touch, when to exit) while context.yaml carries
// the role-specific *data*.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"

	"github.com/slimslenderslacks/work/internal/agent"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// Data is the shared template input. Per-Kind templates use only the fields
// that apply to their role and ignore the rest.
type Data struct {
	Kind          agent.Kind
	Workspace     string   // absolute path to the workspace root
	ProjectPath   string   // absolute path to .project.yaml
	ProjectName   string   // work-stream name: basename of the dir holding .project.yaml
	TasksDir      string   // absolute path to the tasks/ directory
	Branch        string   // target branch (also the workspace name)
	TaskPath      string   // for TaskAgent: path to this task's yaml
	TaskName      string   // for TaskAgent: name field of the task
	FailedTasks   []string // for WolfAgent: paths to failed/blocked task yamls
	BlockedReason string   // for WolfAgent: why the project was blocked

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

	Replan   bool   // for PlanningAgent: re-plan the existing tasks (a cron cycle) instead of preserving them
	Worktree string // for PlanningAgent: absolute host path to the wsp worktree mounted as a second workspace (so source can be read without cloning); empty when no wsp is wired up

	// PushBranch, for the CommitAgent, tells it to push the branch after
	// committing instead of leaving the commit local. Set by the daemon for a
	// review-fix task (one carrying a `source:` — it addresses a live PR review
	// comment or check), because the remote PR branch must include the fix for
	// the external reviewer and CI to see it, and so the review agent's later
	// thread-resolve is truthful. False for ordinary tasks, whose commits stay
	// local until the archive agent publishes them at cleanup.
	PushBranch bool
}

// tmplFuncs are the helpers available to every template. `sub` lets the
// planning template compute the exact per-task-name character budget from the
// work-stream name length (max task name = 62 - len(ProjectName), since the
// sandbox name is "<ProjectName>-<task-name>" and must fit the 63-char DNS
// label limit).
var tmplFuncs = template.FuncMap{
	"sub": func(a, b int) int { return a - b },
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
