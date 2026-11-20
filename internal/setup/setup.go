package setup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Context is the structured side of the agent handoff. Fields are tagged so
// the YAML on disk reads naturally; omitempty keeps role-irrelevant fields
// from cluttering the file.
type Context struct {
	Kind          string   `yaml:"kind"`
	Workspace     string   `yaml:"workspace"`
	Branch        string   `yaml:"branch,omitempty"`
	ProjectPath   string   `yaml:"project_path,omitempty"`
	TasksDir      string   `yaml:"tasks_dir,omitempty"`
	TaskPath      string   `yaml:"task_path,omitempty"`
	TaskName      string   `yaml:"task_name,omitempty"`
	FailedTasks   []string `yaml:"failed_tasks,omitempty"`
	BlockedReason string   `yaml:"blocked_reason,omitempty"`
	// InFlightTasks are the tasks still in flight when a wolf launched. See
	// prompts.Data.InFlightTasks.
	InFlightTasks []InFlightTask `yaml:"in_flight_tasks,omitempty"`

	// BlockedSessionPath/Summary/Attempted mirror project.BlockedSession, the
	// durable record the wolf agent reads/writes for a blocked project. See
	// prompts.Data for the field-by-field meaning.
	BlockedSessionPath      string   `yaml:"blocked_session_path,omitempty"`
	BlockedSessionSummary   string   `yaml:"blocked_session_summary,omitempty"`
	BlockedSessionAttempted []string `yaml:"blocked_session_attempted,omitempty"`

	Replan         bool     `yaml:"replan,omitempty"`
	IntakeFiles    []string `yaml:"intake_files,omitempty"`
	ProjectChanges []string `yaml:"project_changes,omitempty"`
	Worktree       string   `yaml:"worktree,omitempty"`
	// MaxParallelTasks and Repos tell the planner the project's effective
	// task concurrency limit and the repo dir names its touches globs start
	// with. See prompts.Data.
	MaxParallelTasks int      `yaml:"max_parallel_tasks,omitempty"`
	Repos            []string `yaml:"repos,omitempty"`

	// Touches/Areas are a task's declared parallelism hints
	// (docs/parallel-task-worktrees.md H1). Touches is a pointer, as on
	// task.Task, so an explicit `touches: []` is written rather than dropped.
	Touches *[]string `yaml:"touches,omitempty"`
	Areas   []string  `yaml:"areas,omitempty"`

	// TaskWorktree/TaskBranch/TaskBase/Attempt describe a task's own worktree
	// for the task and commit agents. See prompts.Data for the meanings.
	TaskWorktree string   `yaml:"task_worktree,omitempty"`
	TaskBranch   string   `yaml:"task_branch,omitempty"`
	TaskBase     []Commit `yaml:"task_base,omitempty"`
	Attempt      int      `yaml:"attempt,omitempty"`
	Resumed      bool     `yaml:"resumed,omitempty"`
	// Verify/DocsOnly are the commit agent's build check command and the
	// docs-only verdict that skips it. See prompts.Data.
	Verify   string `yaml:"verify,omitempty"`
	DocsOnly bool   `yaml:"docs_only,omitempty"`

	// PushBranch tells the commit agent to push after committing (review-fix
	// tasks) rather than leaving the commit local. See prompts.Data.PushBranch.
	PushBranch bool `yaml:"push_branch,omitempty"`

	// Sandboxed is true when a wolf or sleepy-head agent runs inside an sbx
	// sandbox rather than on the host. See prompts.Data.Sandboxed.
	Sandboxed bool `yaml:"sandboxed,omitempty"`

	// Roots/SnapshotFile/AuditLog/SessionsRoot describe the orch state a
	// workingman agent observes (the roots also writable). See prompts.Data.
	Roots        []string `yaml:"roots,omitempty"`
	SnapshotFile string   `yaml:"snapshot_file,omitempty"`
	AuditLog     string   `yaml:"audit_log,omitempty"`
	SessionsRoot string   `yaml:"sessions_root,omitempty"`
}

// InFlightTask is one entry of in_flight_tasks in .orch/context.yaml.
type InFlightTask struct {
	Name          string `yaml:"name"`
	Status        string `yaml:"status"`
	QueuePosition int    `yaml:"queue_position,omitempty"`
	Integrating   bool   `yaml:"integrating,omitempty"`
}

// Commit is one repo's commit in .orch/context.yaml (same shape as a task
// file's commits:).
type Commit struct {
	Repo string `yaml:"repo"`
	Hash string `yaml:"hash"`
}

// Skill is a directory of files copied verbatim into the workspace's
// .claude/skills/<Name>/ tree. We do not interpret the contents — that's
// claude's job once it boots.
type Skill struct {
	Name   string // destination subdirectory name
	Source string // absolute path to source dir
}

// Apply writes every artifact into workspace. Workspace must already exist.
// Apply is not transactional: a partial failure may leave some files in place.
// The orchestrator treats workspaces as disposable on error.
func Apply(workspace string, ctx Context, instructions string, skills []Skill) error {
	orchDir := filepath.Join(workspace, ".orch")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		return fmt.Errorf("setup: mkdir .orch: %w", err)
	}
	ctxBytes, err := yaml.Marshal(&ctx)
	if err != nil {
		return fmt.Errorf("setup: marshal context: %w", err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "context.yaml"), ctxBytes, 0o644); err != nil {
		return fmt.Errorf("setup: write context.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "instructions.md"), []byte(instructions), 0o644); err != nil {
		return fmt.Errorf("setup: write instructions.md: %w", err)
	}
	for _, sk := range skills {
		if err := installSkill(workspace, sk); err != nil {
			return err
		}
	}
	return nil
}

func installSkill(workspace string, sk Skill) error {
	if sk.Name == "" || sk.Source == "" {
		return fmt.Errorf("setup: skill must have Name and Source (got %+v)", sk)
	}
	dst := filepath.Join(workspace, ".claude", "skills", sk.Name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("setup: mkdir skill %s: %w", sk.Name, err)
	}
	return copyTree(sk.Source, dst)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
