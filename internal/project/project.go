package project

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Writer string

const (
	WriterAgent  Writer = "agent"
	WriterDaemon Writer = "daemon"
)

func (w Writer) Valid() bool {
	return w == WriterAgent || w == WriterDaemon
}

type Repo struct {
	Org  string `yaml:"org"`
	Name string `yaml:"name"`
	// BaseBranch is the branch the workspace's feature branch should start
	// from when the workspace is first created. Defaults to the repo's
	// default branch (typically `main`) when empty. Only applied on first
	// workspace creation — once the workspace exists, the agent's commits
	// own the branch's HEAD.
	BaseBranch string `yaml:"base_branch,omitempty"`
}

type Project struct {
	Description string `yaml:"description"`
	Repos       []Repo `yaml:"repos"`
	Branch      string `yaml:"branch"`
	Status      Status `yaml:"status"`
	Cron        string `yaml:"cron,omitempty"`
	// BlockedReason is set by the daemon when transitioning a project to
	// `status: blocked` so the cause survives a daemon restart and is
	// visible to both humans reading the file and the wolf agent. Left
	// empty for any non-blocked state. Cleared by whichever agent moves
	// the project back out of blocked (planning, wolf).
	BlockedReason string `yaml:"blocked_reason,omitempty"`
	// CreatedAt is stamped by the daemon the first time it observes a
	// populated .project.yaml (i.e. just after the project agent fills in
	// description/branch/status). Used by the TUI to order work streams
	// most-recent-first. Pointer + omitempty so the field stays out of
	// the YAML on disk until the daemon writes it.
	CreatedAt *time.Time `yaml:"created_at,omitempty"`
	UpdatedBy Writer     `yaml:"updated_by"`
}

// Empty reports whether the file is the unpopulated placeholder the project
// agent is meant to fill in. An empty file on disk is the canonical signal,
// but we also treat a parsed-but-fieldless document as empty.
func (p *Project) Empty() bool {
	return p.Description == "" && len(p.Repos) == 0 && p.Branch == "" && p.Status == ""
}

func Load(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return &Project{}, nil
	}
	var p Project
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &p, nil
}

// Save writes the project file with UpdatedBy forced to daemon. The daemon
// uses this marker to ignore fsnotify events triggered by its own writes.
// Agents that need to write the file should use SaveAs(WriterAgent).
func Save(path string, p *Project) error {
	return SaveAs(path, p, WriterDaemon)
}

func SaveAs(path string, p *Project, by Writer) error {
	if !by.Valid() {
		return fmt.Errorf("invalid writer %q", by)
	}
	out := *p
	out.UpdatedBy = by
	data, err := yaml.Marshal(&out)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
