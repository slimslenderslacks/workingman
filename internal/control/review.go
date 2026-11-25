package control

import (
	"errors"
	"fmt"

	"github.com/slimslenderslacks/work/internal/project"
)

// Review drives the PR-resolution loop for the work stream identified by
// projectPath (the path of its project.yaml). On an idle project not yet
// watching a PR it starts the loop by setting the Review intent flag; on one
// already watching it requests an immediate reconcile. Either way the daemon
// finds the project's pull request and manages its review comments and Actions
// runs until the PR is merged or closed. The write is made as `agent`, which
// the daemon acts on rather than ignoring as its own write.
//
// The action adapts to where the project already is (status must be idle;
// see Project.WatchingPR for what distinguishes the two cases below):
//
//   - idle, not yet watching → start the loop by setting `review: true`. This
//     is the manual path for a project that finished before it had a
//     reviewable PR (or before this feature existed).
//   - idle, already watching → the loop is already running but the PR poll may
//     be sitting on a slow backoff rung (up to 1h). Set the one-shot
//     ReviewNow flag so the daemon runs the review agent immediately (see
//     Project.ReviewNow).
//
// Any other status is refused: projects still working enter the loop
// automatically when their last task commits, and forcing it on a
// working/blocked/ready project would interrupt live work. The second return
// value reports whether this was an immediate kick (true) or a loop start
// (false) so the caller can word its confirmation accordingly.
func Review(projectPath string) (name string, kicked bool, err error) {
	if projectPath == "" {
		return "", false, errors.New("no project selected")
	}
	var refusal error
	err = project.Update(projectPath, project.WriterAgent, func(p *project.Project) bool {
		if p.Status != project.StatusIdle {
			refusal = fmt.Errorf("review starts on an idle work stream or kicks one already watching a PR (this one is %q)", p.Status)
			return false
		}
		if p.WatchingPR() {
			p.ReviewNow = true
			kicked = true
		} else {
			p.Review = true
		}
		return true
	})
	if err != nil {
		return "", false, err
	}
	if refusal != nil {
		return "", false, refusal
	}
	return DisplayName(projectPath), kicked, nil
}
