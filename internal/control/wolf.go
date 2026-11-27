package control

import (
	"errors"
	"strings"

	"github.com/slimslenderslacks/work/internal/project"
)

// SummonReason is the blocked_reason written when the user summons the wolf
// manually and the project isn't already carrying a reason. It tells the wolf
// this block is human-initiated (not an automated task/commit failure) so it
// opens by asking the user what they need rather than hunting for a crash.
const SummonReason = "summoned manually by the user via the TUI (:wolf); no automated failure — ask the user what they need help with"

// SummonReasonGUI is [SummonReason] for a wolf summoned from the browser GUI.
const SummonReasonGUI = "summoned manually by the user via the GUI; no automated failure — ask the user what they need help with"

// SummonWolf flips the project identified by projectPath (the path of its
// project.yaml) to status:blocked so the daemon launches the wolf agent in
// that project's context. It returns the project's display name for the
// confirmation message.
//
// The wolf is the daemon's response to status:blocked, so blocking the project
// (written as `agent`, which the daemon acts on rather than ignoring as its
// own write) is how it is summoned. An existing blocked_reason is preserved:
// if the daemon already blocked this project for a real failure, re-summoning
// must not overwrite that diagnosis; SummonReason is supplied only when no
// reason is recorded yet ([WolfReason] replaces it). opts may also add a
// [Check].
func SummonWolf(projectPath string, opts ...Option) (string, error) {
	if projectPath == "" {
		return "", errors.New("no project selected")
	}
	o := collect(opts)
	reason := SummonReason
	if r := strings.TrimSpace(o.reason); r != "" {
		reason = r
	}
	err := o.update(projectPath, project.WriterAgent, func(p *project.Project) bool {
		p.Status = project.StatusBlocked
		if strings.TrimSpace(p.BlockedReason) == "" {
			p.BlockedReason = reason
		}
		return true
	})
	if err != nil {
		return "", err
	}
	return DisplayName(projectPath), nil
}
