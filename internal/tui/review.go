package tui

import (
	"errors"
	"fmt"

	"github.com/slimslenderslacks/work/internal/project"
)

// requestReview starts the PR-resolution loop for a finished work stream by
// flipping the project identified by projectPath (the path of its
// .project.yaml) to status:reviewing. The daemon responds by arming the
// project's #review poll and dispatching the review agent, which finds the
// project's pull request and manages its review comments and Actions runs until
// the PR is merged or closed.
//
// Like `:wolf`/`:cleanup`, the TUI has no direct handle on the daemon, so it
// writes the target status (as `agent`, which the daemon acts on rather than
// ignoring as its own write) and lets the daemon react.
//
// It is gated to a `done` work stream on purpose: this is the manual path for a
// project that finished before it had a reviewable PR (or finished before this
// feature existed). Projects still working enter the loop automatically when
// their last task commits, and starting it on a working/blocked project would
// interrupt live work — so those are refused with an explanatory message rather
// than silently derailed.
func requestReview(projectPath string) (string, error) {
	if projectPath == "" {
		return "", errors.New("no project selected")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return "", err
	}
	if p.Status != project.StatusDone {
		return "", fmt.Errorf("review starts on a done work stream (this one is %q)", p.Status)
	}
	p.Status = project.StatusReviewing
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		return "", err
	}
	return projectDisplayName(projectPath), nil
}
