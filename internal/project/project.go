package project

import (
	"fmt"
	"os"

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
}

type Project struct {
	Description string `yaml:"description"`
	Repos       []Repo `yaml:"repos"`
	Branch      string `yaml:"branch"`
	Status      Status `yaml:"status"`
	Cron        string `yaml:"cron,omitempty"`
	UpdatedBy   Writer `yaml:"updated_by"`
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
