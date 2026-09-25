package tui

import (
	"errors"
	"fmt"

	"github.com/slimslenderslacks/work/internal/project"
)

// requestReview drives the PR-resolution loop for the work stream identified by
// projectPath (the path of its .project.yaml). On an idle project not yet
// watching a PR it starts the loop by setting the Review intent flag; on one
// already watching it requests an immediate reconcile. Either way the daemon
// finds the project's pull request and manages its review comments and Actions
// runs until the PR is merged or closed.
//
// Like `:wolf`/`:cleanup`, the TUI has no direct handle on the daemon, so it
// writes to the file (as `agent`, which the daemon acts on rather than ignoring
// as its own write) and lets the daemon react.
//
// The command adapts to where the project already is (status must be idle;
// see Project.WatchingPR for what distinguishes the two cases below):
//
//   - idle, not yet watching → start the loop by setting `review: true`. This
//     is the manual path for a project that finished before it had a
//     reviewable PR (or before this feature existed).
//   - idle, already watching → the loop is already running but the PR poll may
//     be sitting on a slow backoff rung (up to 30m). Set the one-shot
//     ReviewNow flag so the daemon runs the review agent immediately (see
//     Project.ReviewNow).
//
// Any other status is refused: projects still working enter the loop
// automatically when their last task commits, and forcing it on a
// working/blocked/ready project would interrupt live work. The second return
// value reports whether this was an immediate kick (true) or a loop start
// (false) so the caller can word the status line accordingly.
func requestReview(projectPath string) (name string, kicked bool, err error) {
	if projectPath == "" {
		return "", false, errors.New("no project selected")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return "", false, err
	}
	if p.Status != project.StatusIdle {
		return "", false, fmt.Errorf("review starts on an idle work stream or kicks one already watching a PR (this one is %q)", p.Status)
	}
	if p.WatchingPR() {
		p.ReviewNow = true
		if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
			return "", false, err
		}
		return projectDisplayName(projectPath), true, nil
	}
	p.Review = true
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		return "", false, err
	}
	return projectDisplayName(projectPath), false, nil
}
