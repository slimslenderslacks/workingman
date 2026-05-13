package task

import (
	"fmt"
	"os"

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
	return &t, nil
}

func Save(path string, t *Task) error {
	data, err := yaml.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
