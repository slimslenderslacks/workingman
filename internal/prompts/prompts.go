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
	TasksDir      string   // absolute path to the tasks/ directory
	Branch        string   // target branch (also the workspace name)
	TaskPath      string   // for TaskAgent: path to this task's yaml
	TaskName      string   // for TaskAgent: name field of the task
	FailedTasks   []string // for WolfAgent: paths to failed/blocked task yamls
	BlockedReason string   // for WolfAgent: why the project was blocked
	Worktree      string   // for PlanningAgent: absolute host path to the wsp worktree mounted as a second workspace (so source can be read without cloning); empty when no wsp is wired up
}

var tmpls = template.Must(template.ParseFS(templatesFS, "templates/*.tmpl"))

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
