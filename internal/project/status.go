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
	// An empty status is the "unpopulated" signal — a `:new` seed carries a
	// description but no status yet, and the daemon routes it (via
	// Project.Unpopulated) to the project agent. Accept it here so loading a
	// seed doesn't error; only non-empty values are enum-checked.
	if raw != "" {
		candidate := Status(raw)
		if !candidate.Valid() {
			return fmt.Errorf("invalid project status %q", raw)
		}
	}
	*s = Status(raw)
	return nil
}
