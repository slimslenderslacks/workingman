package tui

import (
	"errors"
	"strings"

	"github.com/slimslenderslacks/work/internal/project"
)

// summonReason is the blocked_reason written when the user summons the wolf
// manually and the project isn't already carrying a reason. It tells the wolf
// this block is human-initiated (not an automated task/commit failure) so it
// opens by asking the user what they need rather than hunting for a crash.
const summonReason = "summoned manually by the user via the TUI (:wolf); no automated failure — ask the user what they need help with"

// summonWolf flips the project identified by projectPath (the path of its
// .project.yaml) to status:blocked so the daemon launches the wolf agent in
// that project's context. It returns the project's display name for the
// confirmation message.
//
// The wolf is the daemon's response to status:blocked, and the TUI has no
// direct handle on the runner — so blocking the project (written as `agent`,
// which the daemon acts on rather than ignoring as its own write) is how we
// summon it. An existing blocked_reason is preserved: if the daemon already
// blocked this project for a real failure, re-summoning must not overwrite
// that diagnosis; we only supply a reason when none is recorded yet.
func summonWolf(projectPath string) (string, error) {
	if projectPath == "" {
		return "", errors.New("no project selected")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return "", err
	}
	p.Status = project.StatusBlocked
	if strings.TrimSpace(p.BlockedReason) == "" {
		p.BlockedReason = summonReason
	}
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		return "", err
	}
	return projectDisplayName(projectPath), nil
}
