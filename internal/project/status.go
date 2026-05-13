package project

import "fmt"

type Status string

const (
	StatusReady   Status = "ready"
	StatusWorking Status = "working"
	StatusBlocked Status = "blocked"
	StatusDone    Status = "done"
)

func (s Status) Valid() bool {
	switch s {
	case StatusReady, StatusWorking, StatusBlocked, StatusDone:
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
		return fmt.Errorf("invalid project status %q", raw)
	}
	*s = candidate
	return nil
}
