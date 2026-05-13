// Package runner is the glue between the daemon and the agent process model.
// Given a Plan (which Kind of agent, where, on what), Runner:
//
//  1. Resolves the working directory: for ProjectAgent the plan supplies it
//     directly (the dir holding the empty .project.yaml); for every other
//     Kind it asks workspace.Manager to provision one.
//  2. Renders the kind-specific instructions text via the prompts package.
//  3. Hands off to setup.Apply to drop .orch/context.yaml,
//     .orch/instructions.md, and any skills into the working directory.
//  4. Builds an agent.Spec and asks the agent.Launcher to start the session.
//
// The CommandBuilder seam lets tests swap claude-code for `sleep` without
// changing any of the orchestration logic.
package runner

import (
	"context"
	"fmt"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/prompts"
	"github.com/slimslenderslacks/work/internal/setup"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// Plan is the request shape. Fields that don't apply to a Kind are ignored.
type Plan struct {
	Kind agent.Kind

	// WorkingDir, when set, is used directly — no wsp workspace is created.
	// Set this for agents that run in the project's control directory
	// (project, planning, wolf).
	//
	// Leave WorkingDir empty for agents that need a wsp workspace (task,
	// commit); in that case Branch + Repos are required so workspace.Manager
	// can provision one.
	WorkingDir string
	Branch     string
	Repos      []workspace.Repo

	ProjectPath string
	TasksDir    string
	TaskPath    string
	TaskName    string
	FailedTasks []string

	Skills []setup.Skill

	// SessionName is the tmux session name. If empty, Runner derives one
	// from Kind and Branch (or Kind and a short hash of WorkingDir).
	SessionName string
}

// CommandBuilder produces the argv that the launcher runs inside the
// workspace. Production builds a claude invocation; tests can return
// `sleep 1` or similar.
type CommandBuilder func(kind agent.Kind, workingDir string) []string

// DefaultCommandBuilder returns the production command: claude-code, told to
// read the instructions and context the orchestrator just wrote.
func DefaultCommandBuilder(_ agent.Kind, _ string) []string {
	return []string{
		"claude",
		"--dangerously-skip-permissions",
		"Read .orch/instructions.md and .orch/context.yaml, then follow the instructions.",
	}
}

type Runner struct {
	Workspaces workspace.Manager
	Launcher   agent.Launcher
	Audit      *audit.Logger
	Command    CommandBuilder // defaults to DefaultCommandBuilder when nil
}

// Start is non-blocking: it returns the Session once the launcher accepts it.
// The caller owns Wait/Close on the returned Session.
func (r *Runner) Start(ctx context.Context, p Plan) (agent.Session, error) {
	workingDir, err := r.resolveWorkingDir(ctx, p)
	if err != nil {
		return nil, err
	}

	data := prompts.Data{
		Kind:        p.Kind,
		Workspace:   workingDir,
		Branch:      p.Branch,
		ProjectPath: p.ProjectPath,
		TasksDir:    p.TasksDir,
		TaskPath:    p.TaskPath,
		TaskName:    p.TaskName,
		FailedTasks: p.FailedTasks,
	}
	instructions, err := prompts.Render(p.Kind, data)
	if err != nil {
		return nil, err
	}

	ctxFile := setup.Context{
		Kind:        p.Kind.String(),
		Workspace:   workingDir,
		Branch:      p.Branch,
		ProjectPath: p.ProjectPath,
		TasksDir:    p.TasksDir,
		TaskPath:    p.TaskPath,
		TaskName:    p.TaskName,
		FailedTasks: p.FailedTasks,
	}
	if err := setup.Apply(workingDir, ctxFile, instructions, p.Skills); err != nil {
		return nil, err
	}

	build := r.Command
	if build == nil {
		build = DefaultCommandBuilder
	}
	spec := agent.Spec{
		Kind:      p.Kind,
		Name:      sessionName(p),
		Workspace: workingDir,
		Command:   build(p.Kind, workingDir),
	}

	sess, err := r.Launcher.Launch(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("runner: launch: %w", err)
	}
	if r.Audit != nil {
		r.Audit.Log("session_started",
			"kind", p.Kind.String(),
			"name", spec.Name,
			"workspace", workingDir,
		)
	}
	return sess, nil
}

func (r *Runner) resolveWorkingDir(ctx context.Context, p Plan) (string, error) {
	if p.WorkingDir != "" {
		return p.WorkingDir, nil
	}
	if r.Workspaces == nil {
		return "", fmt.Errorf("runner: kind %s needs either WorkingDir or a workspace manager", p.Kind)
	}
	if p.Branch == "" {
		return "", fmt.Errorf("runner: kind %s requires Branch (or WorkingDir)", p.Kind)
	}
	return r.Workspaces.Create(ctx, p.Branch, p.Repos)
}

func sessionName(p Plan) string {
	if p.SessionName != "" {
		return p.SessionName
	}
	tail := p.Branch
	if tail == "" {
		tail = shortID(p.WorkingDir)
	}
	return fmt.Sprintf("orch-%s-%s", p.Kind, tail)
}

// shortID hashes a path to a short stable suffix. Used for session names when
// the project agent is launched (no Branch available yet).
func shortID(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}
