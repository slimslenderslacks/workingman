package project

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// BlockedSession is the durable "why blocked / how to get unblocked" record
// the wolf agent maintains for a project, stored at the path returned by
// BlockedSessionPath (a `blocked-session.yaml` file next to `.project.yaml`
// and `tasks/`).
//
// Project.BlockedReason is overwritten by the daemon on every new block, so
// on its own it only ever tells the wolf why the project is blocked *right
// now*, and nothing survives between wolf invocations. This file is
// wolf-owned instead: the wolf reads it in on startup (see
// launchWolfAgent) and rewrites it with an updated diagnosis before it hands
// control back, so the next invocation — of the same block or a later one —
// starts from what was already learned rather than re-deriving it from
// scratch.
type BlockedSession struct {
	// BlockedReason mirrors Project.BlockedReason as of this record's last
	// update, so the file reads coherently on its own even after the live
	// field has moved on to a different reason.
	BlockedReason string `yaml:"blocked_reason"`
	// Summary is the wolf's concise, step-by-step account of what needs to
	// happen to get the project back to a healthy state.
	Summary string `yaml:"summary"`
	// Attempted lists what the wolf has already tried or ruled out, so a
	// later invocation doesn't repeat dead ends.
	Attempted []string `yaml:"attempted,omitempty"`
	UpdatedBy Writer   `yaml:"updated_by"`
}

// BlockedSessionPath returns the path to the blocked-session record that
// belongs next to the .project.yaml at projectPath.
func BlockedSessionPath(projectPath string) string {
	return filepath.Join(filepath.Dir(projectPath), "blocked-session.yaml")
}

// LoadBlockedSession reads and parses the blocked-session record at path.
// As with Load, a missing file is reported as an error satisfying
// errors.Is(err, fs.ErrNotExist) — callers for which "no record yet" is
// unremarkable (e.g. launchWolfAgent, on a project's first block) check for
// that explicitly rather than treating it as a failure.
func LoadBlockedSession(path string) (*BlockedSession, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return &BlockedSession{}, nil
	}
	var bs BlockedSession
	if err := yaml.Unmarshal(data, &bs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &bs, nil
}

// SaveBlockedSessionAs writes the blocked-session record with UpdatedBy
// forced to by, mirroring project.SaveAs.
func SaveBlockedSessionAs(path string, bs *BlockedSession, by Writer) error {
	if !by.Valid() {
		return fmt.Errorf("invalid writer %q", by)
	}
	out := *bs
	out.UpdatedBy = by
	data, err := yaml.Marshal(&out)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
