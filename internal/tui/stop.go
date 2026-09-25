package tui

import (
	"errors"
	"fmt"

	"github.com/slimslenderslacks/work/internal/project"
)

// requestStop pauses the work stream identified by projectPath: it records the
// project's current status in StoppedFrom (so `:start` knows what to restore)
// and flips Status to StatusStopped. It returns the project's display name for
// the confirmation message.
//
// Like `:wolf`/`:cleanup`/`:review`, the TUI has no direct handle on the
// daemon, so it writes to the file (as `agent`, which the daemon acts on
// rather than ignoring as its own write) and lets the daemon react — here,
// by killing every agent session running for the project and unregistering
// its cron/#review schedules (see Daemon.enforceStopped).
//
// A project that is already stopped is refused: re-stopping would overwrite
// StoppedFrom with `stopped` itself, losing the status `:start` should restore.
func requestStop(projectPath string) (string, error) {
	if projectPath == "" {
		return "", errors.New("no work stream selected")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return "", err
	}
	name := projectDisplayName(projectPath)
	if p.Unpopulated() {
		return "", fmt.Errorf("%s has no status yet to stop", name)
	}
	if p.Status == project.StatusStopped {
		return "", fmt.Errorf("%s is already stopped", name)
	}
	p.StoppedFrom = p.Status
	p.Status = project.StatusStopped
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		return "", err
	}
	return name, nil
}

// requestStart resumes a work stream `:stop` paused: it restores Status from
// StoppedFrom and clears the field, handing the project back to the daemon's
// ordinary fsnotify-driven routing — no separate "resume" logic exists on the
// daemon side, the restored status is all it takes to pick dispatch back up
// from wherever the project left off (planning if it was `ready`, the next
// task if `working`, the wolf if `blocked`, the PR poll if `reviewing`, or
// nothing further if `done`).
//
// Refused on anything but a stopped project — there is nothing to resume.
func requestStart(projectPath string) (string, error) {
	if projectPath == "" {
		return "", errors.New("no work stream selected")
	}
	p, err := project.Load(projectPath)
	if err != nil {
		return "", err
	}
	name := projectDisplayName(projectPath)
	if p.Status != project.StatusStopped {
		return "", fmt.Errorf("%s is not stopped (status %q)", name, p.Status)
	}
	// StoppedFrom should always be set by requestStop, but a hand-edited file
	// could lack it; restoring to an empty status would read back as the
	// "unpopulated" placeholder and misroute to the project agent, so fall back
	// to done (an ordinary idle state) instead.
	p.Status = p.StoppedFrom
	if p.Status == "" {
		p.Status = project.StatusDone
	}
	p.StoppedFrom = ""
	if err := project.SaveAs(projectPath, p, project.WriterAgent); err != nil {
		return "", err
	}
	return name, nil
}
