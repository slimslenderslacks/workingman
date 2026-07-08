package task

import (
	"fmt"
	"os"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
	"gopkg.in/yaml.v3"
)

type Status string

const (
	StatusReady     Status = "ready"
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailed    Status = "failed"
	StatusBlocked   Status = "blocked"
	StatusCommitted Status = "committed"
)

// ModelDefault is the placeholder value every task carries today. The
// planning agent writes this verbatim; Load() backfills it for tasks on
// disk that pre-date the field.
const ModelDefault = "default"

func (s Status) Valid() bool {
	switch s {
	case StatusReady, StatusRunning, StatusSuccess, StatusFailed, StatusBlocked, StatusCommitted:
		return true
	}
	return false
}

func (s *Status) UnmarshalYAML(unmarshal func(any) error) error {
	var raw string
	if err := unmarshal(&raw); err != nil {
		return err
	}
	candidate := Status(raw)
	if !candidate.Valid() {
		return fmt.Errorf("invalid task status %q", raw)
	}
	*s = candidate
	return nil
}

type Task struct {
	Name          string   `yaml:"name"`
	Description   string   `yaml:"description"`
	DependsOn     []string `yaml:"depends_on"`
	Status        Status   `yaml:"status"`
	Attempts      int      `yaml:"attempts"`
	FailureReason string   `yaml:"failure_reason"`
	BlockedReason string   `yaml:"blocked_reason"`

	// Summary is a short prose record of what was done, written by the
	// commit agent when it transitions the task to `committed`. Empty on a
	// task that hasn't been committed yet.
	Summary string `yaml:"summary,omitempty"`

	// Commits records the git commits the commit agent produced — one
	// entry per repo that had modifications. Empty when the work didn't
	// require any commits (e.g. the task was a no-op or its only output
	// landed in CreatedFiles).
	Commits []Commit `yaml:"commits,omitempty"`

	// CreatedFiles lists files the agent created outside of any commit:
	// untracked files left in repo working trees, scratch notes in the
	// workspace root, etc. Useful for surfacing artifacts that don't show
	// up in `git log`.
	CreatedFiles []string `yaml:"created_files,omitempty"`

	// CompletedAt is when the daemon observed this task reach `committed` —
	// i.e. when its work was fully landed. It is stamped by the daemon in its
	// commit-session callback, AFTER the commit agent has exited, so an agent
	// rewriting the task file can never clobber it. A pointer so it omits
	// cleanly from YAML until set; nil on tasks that haven't committed (or that
	// committed before this field existed). The TUI sorts the Tasks pane by it
	// to show tasks in the order they actually ran/completed.
	CompletedAt *time.Time `yaml:"completed_at,omitempty"`

	// StaticMCPs are the sbx static-MCP names this task's sandbox should be
	// created with: each entry becomes a `--static-mcp <name>` flag on
	// `sbx create`. Populated by the planning agent when a task needs an MCP
	// server inside its sandbox (web search, github, etc.). Empty means no
	// MCPs and a vanilla sandbox.
	StaticMCPs []string `yaml:"static_mcps,omitempty"`

	// Policies are per-task sandbox policy rules applied with `sbx policy`
	// after the sandbox is created and before any `sbx exec` runs in it.
	// Each rule is an allow/deny decision over a network or filesystem
	// resource pattern. The default sandbox policy is "balanced" — many
	// common resources are already permitted — so rules here typically
	// tighten the network surface (deny all + allow a known host) or
	// loosen filesystem access for a task that needs more.
	Policies []policy.Rule `yaml:"policies,omitempty"`

	// Model is the claude model the task agent should run under. For now
	// every task uses "default", which lets claude pick its own current
	// default; the field is reserved so a future planner can choose a
	// cheaper/heavier model per task (e.g. "haiku" for trivial tasks).
	// Load() backfills empty values to "default" so older tasks on disk
	// continue to work without a planner rewrite.
	Model string `yaml:"model"`

	// Path is the absolute file path the task was loaded from. It is set by
	// Load and excluded from YAML so it round-trips cleanly. Callers use it
	// when they need to read or write the task file again (e.g. the daemon
	// reloading after a session ends), so they never have to reconstruct a
	// path from Name — filenames may carry sort prefixes ("00-foo.yaml")
	// that don't match the task's identity.
	Path string `yaml:"-"`
}

// Commit captures a single git commit produced by the commit agent during a
// task's commit phase. Repo is the workspace-relative directory name of the
// repo that was committed in (matches workspace.Repo.DirName()); Hash is
// the full SHA from `git rev-parse HEAD`.
type Commit struct {
	Repo string `yaml:"repo"`
	Hash string `yaml:"hash"`
}

func Load(path string) (*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Task
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	t.Path = path
	if t.Model == "" {
		t.Model = ModelDefault
	}
	return &t, nil
}

func Save(path string, t *Task) error {
	data, err := yaml.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
