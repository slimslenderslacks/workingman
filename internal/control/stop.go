package control

import (
	"errors"
	"fmt"

	"github.com/slimslenderslacks/work/internal/project"
)

// Stop pauses the work stream identified by projectPath: it records the
// project's current status in StoppedFrom (so [Start] knows what to restore)
// and flips Status to StatusStopped. It returns the project's display name for
// the confirmation message.
//
// The write is made as `agent`, which the daemon acts on rather than ignoring
// as its own write, and the daemon reacts by killing every agent session
// running for the project and unregistering its cron/#review schedules (see
// Daemon.enforceStopped).
//
// A project that is already stopped is refused: re-stopping would overwrite
// StoppedFrom with `stopped` itself, losing the status Start should restore.
func Stop(projectPath string) (string, error) {
	if projectPath == "" {
		return "", errors.New("no work stream selected")
	}
	name := DisplayName(projectPath)
	var refusal error
	err := project.Update(projectPath, project.WriterAgent, func(p *project.Project) bool {
		if p.Unpopulated() {
			refusal = fmt.Errorf("%s has no status yet to stop", name)
			return false
		}
		if p.Status == project.StatusStopped {
			refusal = fmt.Errorf("%s is already stopped", name)
			return false
		}
		p.StoppedFrom = p.Status
		p.Status = project.StatusStopped
		return true
	})
	if err != nil {
		return "", err
	}
	if refusal != nil {
		return "", refusal
	}
	return name, nil
}

// Start resumes a work stream [Stop] paused: it restores Status from
// StoppedFrom and clears the field, handing the project back to the daemon's
// ordinary fsnotify-driven routing — no separate "resume" logic exists on the
// daemon side, the restored status is all it takes to pick dispatch back up
// from wherever the project left off (planning if it was `ready`, the next
// task if `working`, the wolf if `blocked`, the PR poll if `reviewing`, or
// nothing further if `done`).
//
// Refused on anything but a stopped project — there is nothing to resume.
func Start(projectPath string) (string, error) {
	if projectPath == "" {
		return "", errors.New("no work stream selected")
	}
	name := DisplayName(projectPath)
	var refusal error
	err := project.Update(projectPath, project.WriterAgent, func(p *project.Project) bool {
		if p.Status != project.StatusStopped {
			refusal = fmt.Errorf("%s is not stopped (status %q)", name, p.Status)
			return false
		}
		// StoppedFrom should always be set by Stop, but a hand-edited file
		// could lack it; restoring to an empty status would read back as the
		// "unpopulated" placeholder and misroute to the project agent, so fall
		// back to idle (the ordinary resting state) instead.
		p.Status = p.StoppedFrom
		if p.Status == "" {
			p.Status = project.StatusIdle
		}
		p.StoppedFrom = ""
		return true
	})
	if err != nil {
		return "", err
	}
	if refusal != nil {
		return "", refusal
	}
	return name, nil
}
